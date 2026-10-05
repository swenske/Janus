package main

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/fleetkit"
	"github.com/swenske/Janus/internal/pki"
)

// janusctl fleet: a fleet without a Controller. Its root's key stays in
// a recovery kit, offline - the Controller's kit format, so either opens
// the other's. Each machine (or CI) that manages the nodes has its own
// issuing CA, signed by the root and listed in the bundle the nodes
// apply; janusctl signs itself 12-hour certificates with it, on the spot,
// and reaches the nodes directly, each pinned by its own CA. Taking a
// machine out of the fleet is a bundle without its issuing CA, synced to
// the nodes.
//
// A fleet context's directory holds root.crt, issuing.crt, issuing.key
// (0600), bundle.json, and key.pem/cert.pem - the short certificate.

const (
	fleetRootFile       = "root.crt"
	fleetIssuingCert    = "issuing.crt"
	fleetIssuingKey     = "issuing.key"
	fleetBundleFile     = "bundle.json"
	kitPassphraseEnv    = "JANUS_KIT_PASSPHRASE"
	issuerRequestForm   = "janus-issuer-request/1"
	issuerGrantForm     = "janus-issuer-grant/1"
	issuingNamePrefix   = "Janus fleet issuing CA - "
	fleetCertValidity   = 12 * time.Hour
	kitCertValidity     = time.Hour
	fleetRootValidity   = 20 * 365 * 24 * time.Hour
	fleetIssuerValidity = 2 * 365 * 24 * time.Hour
)

// ctxFleet is a context's fleet, when janusctl keeps it itself.
type ctxFleet struct {
	Name string `json:"name"`
	// Issuer is this machine's issuing CA's name.
	Issuer string `json:"issuer"`
	// Pending: the issuing CA waits for its grant (fleet issuer accept).
	Pending bool `json:"pending,omitempty"`
}

// localFleet is a fleet context's files, read.
type localFleet struct {
	dir     string
	root    *x509.Certificate
	rootPEM []byte
	issuing *pki.CA
	signed  []byte
	bundle  *pki.Bundle
	cas     []*x509.Certificate
}

func openLocalFleet(name string) (*localFleet, error) {
	f := &localFleet{dir: contextDir(name)}
	read := func(file string) ([]byte, error) { return os.ReadFile(filepath.Join(f.dir, file)) }
	var err error
	if f.rootPEM, err = read(fleetRootFile); err != nil {
		return nil, err
	}
	b, _ := pem.Decode(f.rootPEM)
	if b == nil {
		return nil, errors.New("root.crt: no PEM certificate")
	}
	if f.root, err = x509.ParseCertificate(b.Bytes); err != nil {
		return nil, err
	}
	certPEM, err := read(fleetIssuingCert)
	if err != nil {
		return nil, err
	}
	keyPEM, err := read(fleetIssuingKey)
	if err != nil {
		return nil, err
	}
	if f.issuing, err = pki.LoadCA(certPEM, keyPEM); err != nil {
		return nil, err
	}
	if f.signed, err = read(fleetBundleFile); err != nil {
		return nil, err
	}
	if f.bundle, f.cas, err = pki.VerifyBundle(f.root, f.signed); err != nil {
		return nil, fmt.Errorf("bundle.json: %w", err)
	}
	return f, nil
}

func (f *localFleet) saveBundle(signed []byte) error {
	b, cas, err := pki.VerifyBundle(f.root, signed)
	if err != nil {
		return err
	}
	if signed, err = pki.CanonicalBundle(signed); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(f.dir, fleetBundleFile), signed, 0o644); err != nil {
		return err
	}
	f.signed, f.bundle, f.cas = signed, b, cas
	return nil
}

// nextVersion is a bundle version newer than prev: the time, in
// milliseconds - newer than any other machine's too, without knowing it
// -, or prev+1.
func nextVersion(prev uint64) uint64 {
	if v := uint64(time.Now().UnixMilli()); v > prev {
		return v
	}
	return prev + 1
}

// sameBundle reports whether two signed bundles are the same one.
func sameBundle(a, b []byte) bool {
	ca, err1 := pki.CanonicalBundle(a)
	cb, err2 := pki.CanonicalBundle(b)
	return err1 == nil && err2 == nil && string(ca) == string(cb)
}

// resign signs f's issuing CAs again into a bundle newer than above - the
// kit at hand: this machine's view of the fleet wins (fleet recover).
func resign(f *localFleet, root *pki.CA, above uint64) error {
	signed, err := signBundle(root, max(above, f.bundle.Version), f.cas)
	if err != nil {
		return err
	}
	return f.saveBundle(signed)
}

// refusedHint says why every node refuses this machine, when they do.
func refusedHint(errs []error) string {
	for _, err := range errs {
		if err == nil || !strings.Contains(err.Error(), "bad certificate") {
			return ""
		}
	}
	return "\nEvery node refuses this machine's certificate: its issuing CA left the fleet's bundle (revoked - janusctl fleet issuer list on another machine), or the nodes never got it (fleet sync where it was signed)."
}

func issuerName(c *x509.Certificate) string {
	return strings.TrimPrefix(c.Subject.CommonName, issuingNamePrefix)
}

// signBundle is a new bundle of root listing cas, newer than prev.
func signBundle(root *pki.CA, prev uint64, cas []*x509.Certificate) ([]byte, error) {
	var pems []string
	for _, c := range cas {
		pems = append(pems, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})))
	}
	return pki.SignBundle(root, pki.Bundle{Version: nextVersion(prev), Issued: time.Now().UTC(), IssuingCAs: pems})
}

// contextKey is the context's key - made the first time, kept in
// key.pem.
func contextKey(dir string) (crypto.Signer, error) {
	path := filepath.Join(dir, "key.pem")
	if data, err := os.ReadFile(path); err == nil {
		b, _ := pem.Decode(data)
		if b == nil {
			return nil, errors.New("key.pem: no PEM key")
		}
		k, err := x509.ParsePKCS8PrivateKey(b.Bytes)
		if err != nil {
			return nil, err
		}
		s, ok := k.(crypto.Signer)
		if !ok {
			return nil, errors.New("key.pem: not a signing key")
		}
		return s, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return key, writeFileAtomic(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600)
}

// localSignIn signs the context's certificate with this machine's
// issuing CA: 12 hours, the context's user and role.
func localSignIn(name string, c *cliContext) error {
	f, err := openLocalFleet(name)
	if err != nil {
		return fmt.Errorf("the fleet's files: %w", err)
	}
	if time.Until(f.issuing.Cert.NotAfter) < fleetCertValidity {
		return fmt.Errorf("this machine's issuing CA ends %s: ask for a new one (janusctl fleet issuer request)", f.issuing.Cert.NotAfter.Local().Format("2006-01-02"))
	}
	key, err := contextKey(f.dir)
	if err != nil {
		return err
	}
	leaf, err := f.issuing.IssueFor(key.Public(), pki.IssueOptions{CommonName: c.User, Roles: []string{c.Role}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, Validity: fleetCertValidity})
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(f.dir, "cert.pem"), append(leaf, f.issuing.CertPEM...), 0o600); err != nil {
		return err
	}
	b, _ := pem.Decode(leaf)
	cert, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return err
	}
	c.Expires = cert.NotAfter
	return nil
}

// fleetClientCert is what janusctl presents to the fleet's nodes: the
// context's certificate, or with root (the kit opened) one the root signs
// for an hour - for nodes whose bundle doesn't list this machine yet.
func fleetClientCert(name string, c *cliContext, root *pki.CA) (tls.Certificate, error) {
	dir := contextDir(name)
	if root == nil {
		if time.Until(c.Expires) <= renewBefore {
			if err := localSignIn(name, c); err != nil {
				return tls.Certificate{}, err
			}
		}
		return tls.LoadX509KeyPair(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	}
	key, err := contextKey(dir)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := root.IssueFor(key.Public(), pki.IssueOptions{CommonName: c.User, Roles: []string{c.Role}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, Validity: kitCertValidity})
	if err != nil {
		return tls.Certificate{}, err
	}
	b, _ := pem.Decode(leaf)
	return tls.Certificate{Certificate: [][]byte{b.Bytes}, PrivateKey: key}, nil
}

// openKitFile opens a recovery kit, its passphrase from
// JANUS_KIT_PASSPHRASE or asked on the terminal.
func openKitFile(path string) (*fleetkit.Kit, *pki.CA, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	pass := os.Getenv(kitPassphraseEnv)
	if pass == "" {
		p, err := askPassphrase("The recovery kit's passphrase: ")
		if err != nil {
			return nil, nil, fmt.Errorf("the kit's passphrase (or %s): %w", kitPassphraseEnv, err)
		}
		pass = string(p)
	}
	kit, err := fleetkit.Open(data, pass)
	if err != nil {
		return nil, nil, err
	}
	root, err := kit.Root()
	return kit, root, err
}

// kitRootFor opens the kit at path, which must be f's root's.
func kitRootFor(path string, f *localFleet) (*pki.CA, error) {
	_, root, err := openKitFile(path)
	if err != nil {
		return nil, err
	}
	if !root.Cert.Equal(f.root) {
		return nil, errors.New("this kit is another fleet's")
	}
	return root, nil
}

// fleetContext is the fleet context the command is for.
func fleetContext(cfg *cliConfig, flagName string) (string, *cliContext) {
	name, c := currentContext(cfg, flagName)
	if c == nil || c.Fleet == nil {
		log.Fatalf("janusctl fleet: %q isn't a fleet context (janusctl fleet init, or -context NAME)", name)
	}
	if c.Fleet.Pending {
		log.Fatalf("janusctl fleet: context %q waits for its issuing CA: janusctl fleet issuer accept GRANT", name)
	}
	return name, c
}

func defaultUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "admin"
}

func defaultIssuer() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "janusctl"
	}
	return defaultUser() + "@" + host
}

func runFleet(globalCtx string, args []string) {
	if len(args) == 0 {
		fleetUsage()
		os.Exit(2)
	}
	switch args[0] {
	case "init":
		fleetInit(globalCtx, args[1:])
	case "recover":
		fleetRecover(globalCtx, args[1:])
	case "adopt":
		fleetAdopt(globalCtx, args[1:])
	case "sync":
		fleetSync(globalCtx, args[1:])
	case "status":
		fleetStatus(globalCtx, args[1:])
	case "export":
		fleetExport(globalCtx, args[1:])
	case "forget":
		fleetForget(globalCtx, args[1:])
	case "issuer":
		if len(args) < 2 {
			fleetUsage()
			os.Exit(2)
		}
		switch args[1] {
		case "list":
			issuerList(globalCtx)
		case "request":
			issuerRequest(globalCtx, args[2:])
		case "sign":
			issuerSign(globalCtx, args[2:])
		case "accept":
			issuerAccept(globalCtx, args[2:])
		case "revoke":
			issuerRevoke(globalCtx, args[2:])
		default:
			fleetUsage()
			os.Exit(2)
		}
	default:
		fleetUsage()
		os.Exit(2)
	}
}

func fleetUsage() {
	for _, l := range []string{
		"usage: janusctl [-context NAME] fleet <command> - a fleet without a Controller",
		"  init [-name FLEET] [-issuer NAME] [-user NAME] [-role ROLE] [-yes] KIT   a new fleet: its root's key in KIT (a recovery kit, its passphrase shown once), this machine's issuing CA here",
		"  adopt NAME -endpoint HOST:PORT (-ca FILE -cert FILE -key FILE | -ca-fingerprint SHA256) [-kit KIT]   a node into the fleet: with its first boot's admin credential, or - already in the fleet (provisioned) - checked on its CA's fingerprint (its console)",
		"  sync [-kit KIT]                the newest bundle - here or on a node - everywhere",
		"  status                         each node's bundle, this machine's certificate and issuing CA",
		"  export DIR                     root.crt, bundle.json and user-data.json (NoCloud) to provision nodes with the fleet",
		"  forget NAME                    a node out of this context (the node keeps trusting the fleet)",
		"  issuer list                    the issuing CAs of the bundle",
		"  issuer request [-issuer NAME] [-user NAME] [-role ROLE] REQUEST   a new machine: its issuing CA's key here, REQUEST to sign where the kit is",
		"  issuer sign -kit KIT [-replace] REQUEST GRANT   sign a machine's issuing CA into the bundle (then fleet sync); GRANT goes back to it",
		"  issuer accept GRANT            the machine's issuing CA, the fleet and its nodes",
		"  issuer revoke -kit KIT NAME    a bundle without NAME's issuing CA (then fleet sync)",
		"  recover -kit KIT [-name FLEET] [-issuer NAME] [-user NAME]   a fleet from its kit - a Controller's too -: this machine's issuing CA alone in a new bundle (then adopt -kit each node)",
		"the kit's passphrase is asked, or read from " + kitPassphraseEnv,
	} {
		fmt.Fprintln(os.Stderr, l)
	}
}

func checkRole(role string) {
	if role != pki.RoleAdmin && role != pki.RoleOperator && role != pki.RoleReader {
		log.Fatalf("janusctl fleet: -role %q: os:admin, os:operator or os:reader", role)
	}
}

// newFleetContext writes a fleet context's files and saves it as the
// current one, its first certificate signed.
func newFleetContext(cfg *cliConfig, name string, c *cliContext, rootPEM []byte, issuing *pki.CA, signed []byte) {
	dir := contextDir(name)
	keyPEM, err := issuing.KeyPEM()
	if err != nil {
		log.Fatal(err)
	}
	for _, f := range []struct {
		file string
		data []byte
		mode os.FileMode
	}{
		{fleetRootFile, rootPEM, 0o644},
		{fleetIssuingCert, issuing.CertPEM, 0o644},
		{fleetIssuingKey, keyPEM, 0o600},
		{fleetBundleFile, signed, 0o644},
	} {
		if err := writeFileAtomic(filepath.Join(dir, f.file), f.data, f.mode); err != nil {
			log.Fatal(err)
		}
	}
	if err := localSignIn(name, c); err != nil {
		log.Fatal(err)
	}
	cfg.Contexts[name] = c
	cfg.Current = name
	if err := cfg.save(); err != nil {
		log.Fatal(err)
	}
}

func fleetContextName(cfg *cliConfig, globalCtx, fallback string) string {
	name := globalCtx
	if name == "" {
		name = fallback
	}
	if cfg.Contexts[name] != nil {
		log.Fatalf("janusctl fleet: a context %q exists already - -context NAME for another (janusctl context delete %s to start over)", name, name)
	}
	return name
}

func fleetInit(globalCtx string, args []string) {
	fs := flag.NewFlagSet("fleet init", flag.ExitOnError)
	fleetName := fs.String("name", "fleet", "the fleet's name - also the context's, unless -context")
	issuer := fs.String("issuer", defaultIssuer(), "this machine's issuing CA's name")
	userName := fs.String("user", defaultUser(), "who this machine's certificates are for - the nodes log it")
	role := fs.String("role", pki.RoleAdmin, "the role of this machine's certificates")
	yes := fs.Bool("yes", false, "don't ask the passphrase back (you stored it)")
	parseAnywhere(fs, args)
	if fs.NArg() != 1 {
		fleetUsage()
		os.Exit(2)
	}
	checkRole(*role)
	kitPath := fs.Arg(0)
	if _, err := os.Stat(kitPath); err == nil {
		log.Fatalf("janusctl fleet init: %s exists already", kitPath)
	}
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	name := fleetContextName(cfg, globalCtx, *fleetName)

	root, err := pki.NewCAFor("Janus fleet root - "+*fleetName, fleetRootValidity)
	if err != nil {
		log.Fatal(err)
	}
	issuing, err := root.IssueCA(issuingNamePrefix+*issuer, fleetIssuerValidity)
	if err != nil {
		log.Fatal(err)
	}
	signed, err := signBundle(root, 0, []*x509.Certificate{issuing.Cert})
	if err != nil {
		log.Fatal(err)
	}
	rootKey, err := root.KeyPEM()
	if err != nil {
		log.Fatal(err)
	}
	pass, err := fleetkit.NewPassphrase()
	if err != nil {
		log.Fatal(err)
	}
	kit, err := fleetkit.Make(fleetkit.Kit{Name: *fleetName, Created: time.Now().UTC(), RootCert: string(root.CertPEM), RootKey: string(rootKey)}, pass)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(kitPath, kit, 0o600); err != nil {
		log.Fatal(err)
	}
	fmt.Fprintf(os.Stderr, "The fleet's recovery kit is %s - its root's key, encrypted. Its passphrase, shown once:\n\n", kitPath)
	fmt.Println(pass)
	fmt.Fprintln(os.Stderr, "\nStore both together, apart from this machine (a password manager's note): a new machine or a lost one is signed with them.")
	if !*yes {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			_ = os.Remove(kitPath)
			log.Fatal("janusctl fleet init: no terminal to ask the passphrase back on: -yes once it's stored")
		}
		typed, err := askPassphrase("Type the passphrase back: ")
		if err != nil {
			_ = os.Remove(kitPath)
			log.Fatal(err)
		}
		if _, err := fleetkit.Open(kit, string(typed)); err != nil {
			_ = os.Remove(kitPath)
			log.Fatal("janusctl fleet init: that's not the passphrase - nothing kept, start again")
		}
	}
	newFleetContext(cfg, name, &cliContext{User: *userName, Role: *role, Fleet: &ctxFleet{Name: *fleetName, Issuer: *issuer}}, root.CertPEM, issuing, signed)
	fmt.Fprintf(os.Stderr, "Fleet %q: context %q, this machine's issuing CA %q, %s %s. Next: janusctl fleet export DIR to provision nodes with it, or janusctl fleet adopt NAME -endpoint ... for one running.\n", *fleetName, name, *issuer, *userName, *role)
}

func fleetRecover(globalCtx string, args []string) {
	fs := flag.NewFlagSet("fleet recover", flag.ExitOnError)
	kitPath := fs.String("kit", "", "the fleet's recovery kit - a Controller's, or janusctl fleet init's (required)")
	fleetName := fs.String("name", "", "the fleet's name (default: the kit's, or \"fleet\")")
	issuer := fs.String("issuer", defaultIssuer(), "this machine's issuing CA's name")
	userName := fs.String("user", defaultUser(), "who this machine's certificates are for")
	role := fs.String("role", pki.RoleAdmin, "the role of this machine's certificates")
	parseAnywhere(fs, args)
	if *kitPath == "" || fs.NArg() != 0 {
		fleetUsage()
		os.Exit(2)
	}
	checkRole(*role)
	kit, root, err := openKitFile(*kitPath)
	if err != nil {
		log.Fatalf("janusctl fleet recover: %v", err)
	}
	if *fleetName == "" {
		*fleetName = kit.Name
	}
	if *fleetName == "" {
		*fleetName = "fleet"
	}
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	name := fleetContextName(cfg, globalCtx, *fleetName)
	issuing, err := root.IssueCA(issuingNamePrefix+*issuer, fleetIssuerValidity)
	if err != nil {
		log.Fatal(err)
	}
	signed, err := signBundle(root, 0, []*x509.Certificate{issuing.Cert})
	if err != nil {
		log.Fatal(err)
	}
	newFleetContext(cfg, name, &cliContext{User: *userName, Role: *role, Fleet: &ctxFleet{Name: *fleetName, Issuer: *issuer}}, root.CertPEM, issuing, signed)
	fmt.Fprintf(os.Stderr, "Fleet %q recovered: context %q, a bundle listing only this machine's issuing CA %q - the issuing CAs before it (a Controller's) leave the fleet on each node it reaches. Bring the nodes in: janusctl fleet adopt NAME -endpoint HOST:PORT -ca-fingerprint SHA256 -kit %s\n", *fleetName, name, *issuer, *kitPath)
}

// nodeConn reaches a node of the fleet - pinned on its CA - with cert.
func nodeConn(n ctxNode, cert tls.Certificate) (janusv1alpha1.AccessServiceClient, func(), error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(n.CAPEM)) {
		return nil, nil, fmt.Errorf("%s: no CA in the context", n.Name)
	}
	conn, err := dialTLS(n.Address, &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: pool, MinVersion: tls.VersionTLS13})
	if err != nil {
		return nil, nil, err
	}
	return janusv1alpha1.NewAccessServiceClient(conn), func() { conn.Close() }, nil
}

// adoptBundle takes the node's bundle when it's newer than f's and its
// root's: the fleet changed on another machine.
func adoptBundle(f *localFleet, st *janusv1alpha1.TrustState) (bool, error) {
	if len(st.GetBundle()) == 0 || st.GetBundleVersion() <= f.bundle.Version {
		return false, nil
	}
	if _, _, err := pki.VerifyBundle(f.root, st.GetBundle()); err != nil {
		return false, fmt.Errorf("its bundle: %w", err)
	}
	return true, f.saveBundle(st.GetBundle())
}

func fleetAdopt(globalCtx string, args []string) {
	fs := flag.NewFlagSet("fleet adopt", flag.ExitOnError)
	endpoint := fs.String("endpoint", "", "the node's API, HOST[:9505] (required)")
	caFile := fs.String("ca", "", "with its first boot's admin credential: the node's CA (ca.crt)")
	certFile := fs.String("cert", "", "with its first boot's admin credential: admin.crt")
	keyFile := fs.String("key", "", "with its first boot's admin credential: admin.key")
	fingerprint := fs.String("ca-fingerprint", "", "a node already in the fleet: its CA's SHA-256, as its console shows it (\"ca sha256\")")
	kitPath := fs.String("kit", "", "with -ca-fingerprint: reach the node with a certificate the root signs - its bundle doesn't list this machine yet (janusctl fleet recover) - and make it take this machine's bundle, signed again above its own")
	parseAnywhere(fs, args)
	if fs.NArg() != 1 || *endpoint == "" || (*fingerprint == "") == (*caFile == "") {
		fleetUsage()
		os.Exit(2)
	}
	nodeName := fs.Arg(0)
	addr := *endpoint
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(strings.Trim(addr, "[]"), "9505")
	}
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	name, c := fleetContext(cfg, globalCtx)
	f, err := openLocalFleet(name)
	if err != nil {
		log.Fatal(err)
	}
	c2, cancel := ctx()
	defer cancel()

	var client janusv1alpha1.AccessServiceClient
	var caPEM []byte
	var kitRoot *pki.CA
	if *kitPath != "" {
		if kitRoot, err = kitRootFor(*kitPath, f); err != nil {
			log.Fatalf("janusctl fleet adopt: %v", err)
		}
	}
	if *caFile != "" {
		// Its first boot's admin credential: the node's own CA lets it in,
		// verified against the CA given.
		conn, err := dial(addr, *caFile, *certFile, *keyFile)
		if err != nil {
			log.Fatal(err)
		}
		defer conn.Close()
		client = janusv1alpha1.NewAccessServiceClient(conn)
		if caPEM, err = os.ReadFile(*caFile); err != nil {
			log.Fatal(err)
		}
	} else {
		cert, err := fleetClientCert(name, c, kitRoot)
		if err != nil {
			log.Fatal(err)
		}
		// The node isn't pinned yet: its chain is kept, then checked
		// against its CA - whose fingerprint was given - once TrustGet
		// returned it, before anything else is sent.
		var mu sync.Mutex
		var chain [][]byte
		conn, err := dialTLS(addr, &tls.Config{
			Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13,
			InsecureSkipVerify: true, //nolint:gosec // checked against the given fingerprint below, before anything is sent
			VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
				mu.Lock()
				defer mu.Unlock()
				chain = raw
				return nil
			},
		})
		if err != nil {
			log.Fatal(err)
		}
		defer conn.Close()
		client = janusv1alpha1.NewAccessServiceClient(conn)
		st, err := client.TrustGet(c2, &emptypb.Empty{})
		if err != nil {
			log.Fatalf("janusctl fleet adopt: %s doesn't let this fleet in (%v) - give it its first boot's admin credential instead (-ca -cert -key)%s", addr, err, map[bool]string{true: "", false: ", or -kit when its bundle doesn't list this machine yet"}[*kitPath != ""])
		}
		caPEM = st.GetLocalCaCert()
		mu.Lock()
		got := chain
		mu.Unlock()
		if err := checkNodeCA(caPEM, got, *fingerprint); err != nil {
			log.Fatalf("janusctl fleet adopt: %s: %v - not adopted", addr, err)
		}
	}

	st, err := client.TrustGet(c2, &emptypb.Empty{})
	check("TrustGet", err)
	if root := kitRoot; root != nil && len(st.GetRootCert()) > 0 && st.GetBundleVersion() >= f.bundle.Version && !sameBundle(st.GetBundle(), f.signed) {
		if err := resign(f, root, st.GetBundleVersion()); err != nil {
			log.Fatal(err)
		}
	}
	switch {
	case len(st.GetRootCert()) > 0 && !samePEM(st.GetRootCert(), f.rootPEM):
		log.Fatalf("janusctl fleet adopt: %s trusts another fleet's root - janusctl access trust-reset with its own CA's certificate first", addr)
	case st.GetBundleVersion() == f.bundle.Version && !sameBundle(st.GetBundle(), f.signed):
		log.Fatalf("janusctl fleet adopt: %s has another bundle of the same version (%d) - adopt it from the machine whose bundle should win, with -kit", addr, st.GetBundleVersion())
	case st.GetBundleVersion() > f.bundle.Version:
		if _, err := adoptBundle(f, st); err != nil {
			log.Fatalf("janusctl fleet adopt: %s: %v", addr, err)
		}
		fmt.Fprintf(os.Stderr, "%s has a newer bundle (version %d): taken here.\n", nodeName, st.GetBundleVersion())
	case st.GetBundleVersion() < f.bundle.Version || len(st.GetRootCert()) == 0:
		if st, err = client.TrustSet(c2, &janusv1alpha1.TrustSetRequest{RootCert: f.rootPEM, Bundle: f.signed}); err != nil {
			log.Fatalf("janusctl fleet adopt: TrustSet: %v", err)
		}
	}
	if len(caPEM) == 0 {
		log.Fatal("janusctl fleet adopt: the node didn't give its CA - update it")
	}
	caCert, err := parsePEMCert(caPEM)
	if err != nil {
		log.Fatal(err)
	}
	n := ctxNode{ID: pki.Fingerprint(caCert.Raw)[:16], Name: nodeName, Address: addr, CAPEM: string(caPEM), Fleet: true}
	c.Nodes = slices.DeleteFunc(c.Nodes, func(o ctxNode) bool { return o.Name == nodeName || o.ID == n.ID })
	c.Nodes = append(c.Nodes, n)
	if err := cfg.save(); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s (%s) is in fleet %q - bundle version %d. janusctl -n %s version\n", nodeName, addr, c.Fleet.Name, st.GetBundleVersion(), nodeName)
}

func samePEM(a, b []byte) bool {
	ca, err1 := parsePEMCert(a)
	cb, err2 := parsePEMCert(b)
	return err1 == nil && err2 == nil && ca.Equal(cb)
}

func parsePEMCert(p []byte) (*x509.Certificate, error) {
	b, _ := pem.Decode(p)
	if b == nil {
		return nil, errors.New("no PEM certificate")
	}
	return x509.ParseCertificate(b.Bytes)
}

// checkNodeCA checks caPEM is the CA the fingerprint names, and that the
// node's server certificate (chain, as presented) is that CA's.
func checkNodeCA(caPEM []byte, chain [][]byte, fingerprint string) error {
	ca, err := parsePEMCert(caPEM)
	if err != nil {
		return fmt.Errorf("its CA: %w", err)
	}
	if got := pki.Fingerprint(ca.Raw); got != normalizeFingerprint(fingerprint) {
		return fmt.Errorf("its CA's SHA-256 is %s, not %s", got, normalizeFingerprint(fingerprint))
	}
	if len(chain) == 0 {
		return errors.New("no server certificate")
	}
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return err
	}
	inter := x509.NewCertPool()
	for _, r := range chain[1:] {
		if c, err := x509.ParseCertificate(r); err == nil {
			inter.AddCert(c)
		}
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return fmt.Errorf("its server certificate isn't its CA's: %w", err)
	}
	return nil
}

// eachNode runs fn on every node of the context, at once, with cert.
func eachNode(c *cliContext, cert tls.Certificate, fn func(n ctxNode, client janusv1alpha1.AccessServiceClient) string) []string {
	out := make([]string, len(c.Nodes))
	var wg sync.WaitGroup
	for i, n := range c.Nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client, closeConn, err := nodeConn(n, cert)
			if err != nil {
				out[i] = err.Error()
				return
			}
			defer closeConn()
			out[i] = fn(n, client)
		}()
	}
	wg.Wait()
	return out
}

func fleetSync(globalCtx string, args []string) {
	fs := flag.NewFlagSet("fleet sync", flag.ExitOnError)
	kitPath := fs.String("kit", "", "reach the nodes with a certificate the root signs - those whose bundle doesn't list this machine - and make them take this machine's bundle, signed again above theirs")
	parseAnywhere(fs, args)
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	name, c := fleetContext(cfg, globalCtx)
	f, err := openLocalFleet(name)
	if err != nil {
		log.Fatal(err)
	}
	var root *pki.CA
	if *kitPath != "" {
		if root, err = kitRootFor(*kitPath, f); err != nil {
			log.Fatalf("janusctl fleet sync: %v", err)
		}
	}
	cert, err := fleetClientCert(name, c, root)
	if err != nil {
		log.Fatal(err)
	}
	if len(c.Nodes) == 0 {
		fmt.Println("No node in this fleet yet: janusctl fleet adopt")
		return
	}

	// What each node has.
	states := make([]*janusv1alpha1.TrustState, len(c.Nodes))
	errs := make([]error, len(c.Nodes))
	eachNode(c, cert, func(n ctxNode, client janusv1alpha1.AccessServiceClient) string {
		i := slices.IndexFunc(c.Nodes, func(o ctxNode) bool { return o.ID == n.ID })
		c2, cancel := ctx()
		defer cancel()
		states[i], errs[i] = client.TrustGet(c2, &emptypb.Empty{})
		return ""
	})
	for i, err := range errs {
		if states[i] == nil && err == nil {
			errs[i] = errors.New("unreachable")
		}
	}
	before := f.bundle.Version
	if root != nil {
		// The kit at hand: this machine's bundle wins, signed above any.
		above, differ := uint64(0), false
		for _, st := range states {
			if st != nil && len(st.GetRootCert()) > 0 {
				above = max(above, st.GetBundleVersion())
				differ = differ || (st.GetBundleVersion() >= f.bundle.Version && !sameBundle(st.GetBundle(), f.signed))
			}
		}
		if differ {
			if err := resign(f, root, above); err != nil {
				log.Fatal(err)
			}
		}
	} else {
		// Else the newest - one node may hold a bundle signed elsewhere.
		for i, st := range states {
			if st == nil {
				continue
			}
			if taken, err := adoptBundle(f, st); err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", c.Nodes[i].Name, err)
			} else if taken {
				fmt.Fprintf(os.Stderr, "%s had a newer bundle (version %d): taken here.\n", c.Nodes[i].Name, st.GetBundleVersion())
			}
		}
		if f.bundle.Version != before && !slices.ContainsFunc(f.cas, func(ca *x509.Certificate) bool { return ca.Equal(f.issuing.Cert) }) {
			fmt.Fprintf(os.Stderr, "This machine's issuing CA isn't in the fleet's bundle any more (version %d): it was revoked.\n", f.bundle.Version)
		}
	}

	// Then the bundle to every node that hasn't it.
	lines := eachNode(c, cert, func(n ctxNode, client janusv1alpha1.AccessServiceClient) string {
		i := slices.IndexFunc(c.Nodes, func(o ctxNode) bool { return o.ID == n.ID })
		st := states[i]
		switch {
		case st == nil:
			return "unreachable: " + errs[i].Error()
		case sameBundle(st.GetBundle(), f.signed):
			return fmt.Sprintf("bundle version %d, up to date", st.GetBundleVersion())
		case st.GetBundleVersion() >= f.bundle.Version:
			return fmt.Sprintf("another bundle, version %d - janusctl fleet sync -kit on the machine whose bundle should win", st.GetBundleVersion())
		}
		c2, cancel := ctx()
		defer cancel()
		if _, err := client.TrustSet(c2, &janusv1alpha1.TrustSetRequest{RootCert: f.rootPEM, Bundle: f.signed}); err != nil {
			return "refused: " + err.Error()
		}
		return fmt.Sprintf("bundle version %d -> %d", st.GetBundleVersion(), f.bundle.Version)
	})
	if err := cfg.save(); err != nil {
		log.Fatal(err)
	}
	for i, n := range c.Nodes {
		fmt.Printf("%-20s %s\n", n.Name, lines[i])
	}
	if hint := refusedHint(errs); hint != "" {
		fmt.Fprintln(os.Stderr, strings.TrimPrefix(hint, "\n"))
		os.Exit(1)
	}
}

func fleetStatus(globalCtx string, args []string) {
	if len(args) != 0 {
		fleetUsage()
		os.Exit(2)
	}
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	name, c := fleetContext(cfg, globalCtx)
	f, err := openLocalFleet(name)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Fleet %q (context %q), root %s…\n", c.Fleet.Name, name, pki.Fingerprint(f.root.Raw)[:16])
	fmt.Printf("This machine: issuing CA %q until %s; %s (%s)\n", c.Fleet.Issuer, f.issuing.Cert.NotAfter.Local().Format("2006-01-02"), c.User, c.Role)
	fmt.Printf("Bundle version %d, %d issuing CA(s)\n", f.bundle.Version, len(f.cas))
	if len(c.Nodes) == 0 {
		return
	}
	cert, err := fleetClientCert(name, c, nil)
	if err != nil {
		log.Fatal(err)
	}
	_ = cfg.save()
	lines := eachNode(c, cert, func(n ctxNode, client janusv1alpha1.AccessServiceClient) string {
		c2, cancel := ctx()
		defer cancel()
		st, err := client.TrustGet(c2, &emptypb.Empty{})
		if err != nil {
			return "unreachable: " + err.Error()
		}
		switch v := st.GetBundleVersion(); {
		case v == f.bundle.Version:
			return fmt.Sprintf("bundle version %d", v)
		case v < f.bundle.Version:
			return fmt.Sprintf("bundle version %d, older: janusctl fleet sync", v)
		default:
			return fmt.Sprintf("bundle version %d, newer: janusctl fleet sync", v)
		}
	})
	fmt.Printf("\n%-20s %-24s %s\n", "NODE", "ADDRESS", "TRUST")
	for i, n := range c.Nodes {
		fmt.Printf("%-20s %-24s %s\n", n.Name, n.Address, lines[i])
	}
}

func fleetExport(globalCtx string, args []string) {
	if len(args) != 1 {
		fleetUsage()
		os.Exit(2)
	}
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	name, _ := fleetContext(cfg, globalCtx)
	f, err := openLocalFleet(name)
	if err != nil {
		log.Fatal(err)
	}
	dir := args[0]
	userData, err := json.MarshalIndent(map[string]any{"fleet_root_cert": string(f.rootPEM), "fleet_bundle": json.RawMessage(f.signed)}, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	for file, data := range map[string][]byte{"root.crt": f.rootPEM, "bundle.json": f.signed, "user-data.json": append(userData, '\n')} {
		if err := writeFileAtomic(filepath.Join(dir, file), data, 0o644); err != nil {
			log.Fatal(err)
		}
	}
	fmt.Printf("Wrote %s/{root.crt,bundle.json,user-data.json} (bundle version %d):\n", dir, f.bundle.Version)
	fmt.Printf("  janusctl image seed-fleet -fleet-root %s/root.crt -fleet-bundle %s/bundle.json DISK\n", dir, dir)
	fmt.Printf("  janusctl lifecycle install -fleet-root %s/root.crt -fleet-bundle %s/bundle.json DISK BUNDLE_DIR\n", dir, dir)
	fmt.Println("  a NoCloud volume (cidata) whose user-data is user-data.json - merged with a network or a Controller's if any")
}

func fleetForget(globalCtx string, args []string) {
	if len(args) != 1 {
		fleetUsage()
		os.Exit(2)
	}
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	_, c := fleetContext(cfg, globalCtx)
	n := len(c.Nodes)
	c.Nodes = slices.DeleteFunc(c.Nodes, func(o ctxNode) bool { return o.Name == args[0] || o.ID == args[0] })
	if len(c.Nodes) == n {
		log.Fatalf("janusctl fleet forget: no node %q: %s", args[0], nodeNames(c.Nodes))
	}
	if err := cfg.save(); err != nil {
		log.Fatal(err)
	}
}

// parseAnywhere parses fs's flags before, between and after its
// positional arguments ("adopt NAME -endpoint ..."), which flag.Parse
// stops at.
func parseAnywhere(fs *flag.FlagSet, args []string) {
	var pos []string
	for {
		_ = fs.Parse(args)
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	_ = fs.Parse(append([]string{"--"}, pos...))
}

// fleetFiles is a fleet to provision a node with: the files given, or
// the fleet context's.
func fleetFiles(ctxName, rootFile, bundleFile string) (root, bundle []byte) {
	if rootFile != "" || bundleFile != "" {
		if rootFile == "" || bundleFile == "" {
			log.Fatal("janusctl: -fleet-root and -fleet-bundle go together")
		}
		var err error
		if root, err = os.ReadFile(rootFile); err != nil {
			log.Fatal(err)
		}
		if bundle, err = os.ReadFile(bundleFile); err != nil {
			log.Fatal(err)
		}
		return root, bundle
	}
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	name, _ := fleetContext(cfg, ctxName)
	f, err := openLocalFleet(name)
	if err != nil {
		log.Fatal(err)
	}
	return f.rootPEM, f.signed
}

// --- issuers: the machines of the fleet ---

type issuerRequestFile struct {
	Format string `json:"format"`
	Name   string `json:"name"`
	CSR    string `json:"csr"`
}

type issuerGrantFile struct {
	Format      string    `json:"format"`
	Fleet       string    `json:"fleet"`
	Issuer      string    `json:"issuer"`
	RootCert    string    `json:"root_cert"`
	IssuingCert string    `json:"issuing_cert"`
	Bundle      string    `json:"bundle"`
	Nodes       []ctxNode `json:"nodes"`
}

func issuerList(globalCtx string) {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	name, _ := fleetContext(cfg, globalCtx)
	f, err := openLocalFleet(name)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Bundle version %d:\n", f.bundle.Version)
	for _, ca := range f.cas {
		mark := ""
		if ca.Equal(f.issuing.Cert) {
			mark = "  (this machine)"
		}
		fmt.Printf("  %-32s until %s  %s…%s\n", issuerName(ca), ca.NotAfter.Local().Format("2006-01-02"), pki.Fingerprint(ca.Raw)[:16], mark)
	}
}

func issuerRequest(globalCtx string, args []string) {
	fs := flag.NewFlagSet("fleet issuer request", flag.ExitOnError)
	issuer := fs.String("issuer", defaultIssuer(), "this machine's issuing CA's name")
	userName := fs.String("user", defaultUser(), "who this machine's certificates are for")
	role := fs.String("role", pki.RoleAdmin, "the role of this machine's certificates")
	parseAnywhere(fs, args)
	if fs.NArg() != 1 {
		fleetUsage()
		os.Exit(2)
	}
	checkRole(*role)
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	name := fleetContextName(cfg, globalCtx, "fleet")
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		log.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		log.Fatal(err)
	}
	if err := writeFileAtomic(filepath.Join(contextDir(name), fleetIssuingKey), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		log.Fatal(err)
	}
	req, _ := json.MarshalIndent(issuerRequestFile{Format: issuerRequestForm, Name: *issuer, CSR: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}))}, "", "  ")
	if err := os.WriteFile(fs.Arg(0), append(req, '\n'), 0o644); err != nil {
		log.Fatal(err)
	}
	cfg.Contexts[name] = &cliContext{User: *userName, Role: *role, Fleet: &ctxFleet{Issuer: *issuer, Pending: true}}
	cfg.Current = name
	if err := cfg.save(); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Context %q waits for its issuing CA %q: give %s to whoever holds the fleet's kit (janusctl fleet issuer sign -kit KIT %s GRANT), then janusctl fleet issuer accept GRANT. It holds no secret.\n", name, *issuer, fs.Arg(0), fs.Arg(0))
}

func issuerSign(globalCtx string, args []string) {
	fs := flag.NewFlagSet("fleet issuer sign", flag.ExitOnError)
	kitPath := fs.String("kit", "", "the fleet's recovery kit (required)")
	replace := fs.Bool("replace", false, "an issuing CA of that name is in the bundle already: replace it (a machine's lost key)")
	parseAnywhere(fs, args)
	if *kitPath == "" || fs.NArg() != 2 {
		fleetUsage()
		os.Exit(2)
	}
	data, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		log.Fatal(err)
	}
	var req issuerRequestFile
	if err := json.Unmarshal(data, &req); err != nil || req.Format != issuerRequestForm || req.Name == "" {
		log.Fatalf("janusctl fleet issuer sign: %s isn't an issuer request", fs.Arg(0))
	}
	b, _ := pem.Decode([]byte(req.CSR))
	if b == nil {
		log.Fatal("janusctl fleet issuer sign: the request has no CSR")
	}
	csr, err := x509.ParseCertificateRequest(b.Bytes)
	if err != nil || csr.CheckSignature() != nil {
		log.Fatal("janusctl fleet issuer sign: the request's CSR doesn't check")
	}
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	name, c := fleetContext(cfg, globalCtx)
	f, err := openLocalFleet(name)
	if err != nil {
		log.Fatal(err)
	}
	cas := slices.Clone(f.cas)
	if i := slices.IndexFunc(cas, func(ca *x509.Certificate) bool { return issuerName(ca) == req.Name }); i >= 0 {
		if !*replace {
			log.Fatalf("janusctl fleet issuer sign: the bundle has an issuing CA %q already - -replace to replace it", req.Name)
		}
		cas = slices.Delete(cas, i, i+1)
	}
	root, err := kitRootFor(*kitPath, f)
	if err != nil {
		log.Fatalf("janusctl fleet issuer sign: %v", err)
	}
	certPEM, err := root.IssueCAFor(csr.PublicKey, issuingNamePrefix+req.Name, fleetIssuerValidity)
	if err != nil {
		log.Fatal(err)
	}
	cert, err := parsePEMCert(certPEM)
	if err != nil {
		log.Fatal(err)
	}
	signed, err := signBundle(root, f.bundle.Version, append(cas, cert))
	if err != nil {
		log.Fatal(err)
	}
	if err := f.saveBundle(signed); err != nil {
		log.Fatal(err)
	}
	grant, _ := json.MarshalIndent(issuerGrantFile{Format: issuerGrantForm, Fleet: c.Fleet.Name, Issuer: req.Name, RootCert: string(f.rootPEM), IssuingCert: string(certPEM), Bundle: string(f.signed), Nodes: c.Nodes}, "", "  ")
	if err := os.WriteFile(fs.Arg(1), append(grant, '\n'), 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Issuing CA %q signed into bundle version %d. Now: janusctl fleet sync - the nodes take it -, and give %s back (janusctl fleet issuer accept %s).\n", req.Name, f.bundle.Version, fs.Arg(1), fs.Arg(1))
}

func issuerAccept(globalCtx string, args []string) {
	if len(args) != 1 {
		fleetUsage()
		os.Exit(2)
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		log.Fatal(err)
	}
	var g issuerGrantFile
	if err := json.Unmarshal(data, &g); err != nil || g.Format != issuerGrantForm {
		log.Fatalf("janusctl fleet issuer accept: %s isn't a grant", args[0])
	}
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	name, c := currentContext(cfg, globalCtx)
	if c == nil || c.Fleet == nil || !c.Fleet.Pending {
		log.Fatalf("janusctl fleet issuer accept: context %q doesn't wait for an issuing CA (janusctl fleet issuer request)", name)
	}
	dir := contextDir(name)
	keyPEM, err := os.ReadFile(filepath.Join(dir, fleetIssuingKey))
	if err != nil {
		log.Fatal(err)
	}
	issuing, err := pki.LoadCA([]byte(g.IssuingCert), keyPEM)
	if err != nil {
		log.Fatalf("janusctl fleet issuer accept: the grant isn't for this machine's key: %v", err)
	}
	root, err := parsePEMCert([]byte(g.RootCert))
	if err != nil {
		log.Fatal(err)
	}
	if issuing.Cert.CheckSignatureFrom(root) != nil {
		log.Fatal("janusctl fleet issuer accept: the issuing CA isn't the root's")
	}
	_, cas, err := pki.VerifyBundle(root, []byte(g.Bundle))
	if err != nil {
		log.Fatalf("janusctl fleet issuer accept: the bundle: %v", err)
	}
	if !slices.ContainsFunc(cas, func(ca *x509.Certificate) bool { return ca.Equal(issuing.Cert) }) {
		log.Fatal("janusctl fleet issuer accept: the bundle doesn't list this machine's issuing CA")
	}
	c.Fleet = &ctxFleet{Name: g.Fleet, Issuer: g.Issuer}
	c.Nodes = g.Nodes
	newFleetContext(cfg, name, c, []byte(g.RootCert), issuing, []byte(g.Bundle))
	fmt.Printf("Context %q is in fleet %q as %q, %d node(s). Once the bundle is synced to them: janusctl fleet status\n", name, g.Fleet, g.Issuer, len(c.Nodes))
}

func issuerRevoke(globalCtx string, args []string) {
	fs := flag.NewFlagSet("fleet issuer revoke", flag.ExitOnError)
	kitPath := fs.String("kit", "", "the fleet's recovery kit (required)")
	parseAnywhere(fs, args)
	if *kitPath == "" || fs.NArg() != 1 {
		fleetUsage()
		os.Exit(2)
	}
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	name, _ := fleetContext(cfg, globalCtx)
	f, err := openLocalFleet(name)
	if err != nil {
		log.Fatal(err)
	}
	cas := slices.DeleteFunc(slices.Clone(f.cas), func(ca *x509.Certificate) bool { return issuerName(ca) == fs.Arg(0) })
	if len(cas) == len(f.cas) {
		log.Fatalf("janusctl fleet issuer revoke: no issuing CA %q in the bundle (janusctl fleet issuer list)", fs.Arg(0))
	}
	if !slices.ContainsFunc(cas, func(ca *x509.Certificate) bool { return ca.Equal(f.issuing.Cert) }) {
		log.Fatal("janusctl fleet issuer revoke: that's this machine's issuing CA - revoke it from another machine of the fleet")
	}
	root, err := kitRootFor(*kitPath, f)
	if err != nil {
		log.Fatalf("janusctl fleet issuer revoke: %v", err)
	}
	signed, err := signBundle(root, f.bundle.Version, cas)
	if err != nil {
		log.Fatal(err)
	}
	if err := f.saveBundle(signed); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Bundle version %d, without %q. Now: janusctl fleet sync - each node lets it in until it has this bundle.\n", f.bundle.Version, fs.Arg(0))
}
