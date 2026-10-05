package main

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/swenske/Janus/internal/sshsig"
)

// Contexts: janusctl signed in to a Controller (janusctl login). The
// Controller gives it a short certificate of its fleet for the account -
// for a key janusctl makes and keeps - and the nodes it can use it with;
// janusctl then talks to those nodes directly, each checking the
// certificate's role itself. The configuration is $JANUSCONFIG, else
// ~/.config/janus/janusctl.json, and each context's key and certificate
// sit next to it, in a directory named after the context.
//
// -endpoint/-ca/-cert/-key still reach one node with a certificate of
// its own - the way in when nothing else works - and take precedence.

const (
	configEnv = "JANUSCONFIG"
	tokenEnv  = "JANUS_TOKEN"
	// renewBefore: a certificate this close to its end is renewed first.
	renewBefore = 5 * time.Minute
)

type cliConfig struct {
	Current  string                 `json:"current"`
	Contexts map[string]*cliContext `json:"contexts"`
}

type cliContext struct {
	// Controller is the Controller's host:port.
	Controller string `json:"controller"`
	// ControllerCA is what the Controller's certificate is checked
	// against (PEM): its own certificate, pinned, or its CA.
	ControllerCA string    `json:"controller_ca"`
	User         string    `json:"user,omitempty"`
	Role         string    `json:"role,omitempty"`
	Expires      time.Time `json:"expires,omitzero"`
	Nodes        []ctxNode `json:"nodes,omitempty"`
	// SSHKey is the SSH key the context signs in with ("agent:<SHA256
	// fingerprint>" or "file:<path>") - the certificate's key too; empty
	// for an API token's context (a key made here, key.pem).
	SSHKey string `json:"ssh_key,omitempty"`

	// seen is the SHA-256 of the certificate the Controller presented on
	// the last call - what an SSH sign-in's signature names.
	seen string
}

type ctxNode struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Address string `json:"address"`
	CAPEM   string `json:"ca_pem"`
	Fleet   bool   `json:"fleet"`
}

func configPath() string {
	if p := os.Getenv(configEnv); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "janus", "janusctl.json")
}

// contextDir holds a context's key and certificate.
func contextDir(name string) string {
	return filepath.Join(filepath.Dir(configPath()), name)
}

func loadConfig() (*cliConfig, error) {
	cfg := &cliConfig{Contexts: map[string]*cliContext{}}
	data, err := os.ReadFile(configPath())
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", configPath(), err)
	}
	if cfg.Contexts == nil {
		cfg.Contexts = map[string]*cliContext{}
	}
	return cfg, nil
}

func (cfg *cliConfig) save() error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(configPath(), append(data, '\n'), 0o600)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// --- janusctl login ---

func runLogin(args []string) {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	name := fs.String("context", "", "the context to sign in (default: the current one, or \"default\" for the first)")
	controller := fs.String("controller", "", "the Controller's address, host[:port] (port 443 by default) - required the first time")
	caFile := fs.String("controller-ca", "", "check the Controller's certificate against this PEM file (its CA, or the certificate itself)")
	fingerprint := fs.String("controller-fingerprint", "", "trust the Controller's certificate with this SHA-256 fingerprint (hex, colons allowed)")
	user := fs.String("user", "", "your account on the Controller (with an SSH key; kept in the context)")
	keyFlag := fs.String("ssh-key", "", "the SSH key to sign in with: its private key file, or its .pub to use it from ssh-agent (default: ssh-agent's Ed25519 key)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: janusctl login [-context NAME] [-controller HOST[:PORT]] [-controller-ca FILE | -controller-fingerprint SHA256] -user NAME [-ssh-key FILE]")
		fmt.Fprintln(fs.Output(), "       JANUS_TOKEN=janus_... janusctl login [-context NAME] [-controller ...]   with an API token instead (CI)")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	token := os.Getenv(tokenEnv)
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	ctxName := *name
	if ctxName == "" {
		ctxName = cfg.Current
	}
	if ctxName == "" {
		ctxName = "default"
	}
	ctx := cfg.Contexts[ctxName]
	if ctx == nil {
		ctx = &cliContext{}
	}
	if *controller != "" {
		addr := normalizeController(*controller)
		if addr != ctx.Controller {
			ctx = &cliContext{Controller: addr}
		}
	}
	if ctx.Controller == "" {
		log.Fatalf("janusctl login: -controller is required for a new context")
	}
	switch {
	case *caFile != "":
		pemBytes, err := os.ReadFile(*caFile)
		if err != nil {
			log.Fatal(err)
		}
		ctx.ControllerCA = string(pemBytes)
	case *fingerprint != "":
		leaf, err := fetchLeaf(ctx.Controller)
		if err != nil {
			log.Fatalf("janusctl login: %v", err)
		}
		if got := certFingerprint(leaf); got != normalizeFingerprint(*fingerprint) {
			log.Fatalf("janusctl login: the Controller at %s presents a certificate with SHA-256 %s, not %s - not trusted", ctx.Controller, colonHex(got), *fingerprint)
		}
		ctx.ControllerCA = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw}))
	case ctx.ControllerCA == "":
		leaf, err := fetchLeaf(ctx.Controller)
		if err != nil {
			log.Fatalf("janusctl login: %v", err)
		}
		log.Fatalf("janusctl login: the Controller at %s presents a certificate with SHA-256\n  %s\nCheck it (on the Controller's host: openssl x509 -in <data-dir>/dashboard-identity.crt -noout -fingerprint -sha256), then pass it as -controller-fingerprint, or give -controller-ca", ctx.Controller, colonHex(certFingerprint(leaf)))
	}
	if token != "" {
		if err := ctx.signIn(ctxName, token); err != nil {
			log.Fatalf("janusctl login: %v", err)
		}
	} else {
		if *user != "" {
			ctx.User = *user
		}
		if ctx.User == "" {
			log.Fatalf("janusctl login: -user NAME - your account on the Controller (or %s with an API token)", tokenEnv)
		}
		spec := *keyFlag
		if spec == "" {
			spec = ctx.SSHKey
		}
		key, err := openSSHKey(spec)
		if err != nil {
			log.Fatalf("janusctl login: %v", err)
		}
		defer key.Close()
		if err := ctx.sshSignIn(ctxName, key); err != nil {
			log.Fatalf("janusctl login: %v", err)
		}
	}
	cfg.Contexts[ctxName] = ctx
	cfg.Current = ctxName
	if err := cfg.save(); err != nil {
		log.Fatal(err)
	}
	fleet := 0
	for _, n := range ctx.Nodes {
		if n.Fleet {
			fleet++
		}
	}
	fmt.Printf("Signed in to %s as %s (%s) until %s - context %q, %d node(s), %d reachable with it.\n", ctx.Controller, ctx.User, ctx.Role, ctx.Expires.Local().Format("2006-01-02 15:04"), ctxName, len(ctx.Nodes), fleet)
}

func normalizeController(s string) string {
	s = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://"), "/")
	if _, _, err := net.SplitHostPort(s); err != nil {
		return net.JoinHostPort(strings.Trim(s, "[]"), "443")
	}
	return s
}

func normalizeFingerprint(s string) string {
	s = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(s), "SHA256:"), "sha256:")
	return strings.ToLower(strings.NewReplacer(":", "", " ", "").Replace(s))
}

func certFingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	return hex.EncodeToString(sum[:])
}

func colonHex(h string) string {
	var parts []string
	for i := 0; i+2 <= len(h); i += 2 {
		parts = append(parts, strings.ToUpper(h[i:i+2]))
	}
	return strings.Join(parts, ":")
}

// fetchLeaf is the certificate the Controller presents - only to show
// its fingerprint or compare it with one given; nothing is sent.
func fetchLeaf(addr string) (*x509.Certificate, error) {
	host, _, _ := net.SplitHostPort(addr)
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", addr, &tls.Config{ServerName: host, InsecureSkipVerify: true}) //nolint:gosec // only to read the certificate, then compared with what the user gave
	if err != nil {
		return nil, fmt.Errorf("reach the Controller at %s: %w", addr, err)
	}
	defer conn.Close()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return nil, fmt.Errorf("the Controller at %s presented no certificate", addr)
	}
	return certs[0], nil
}

// controllerTLS checks the Controller: its certificate pinned exactly,
// or a chain to the given CA for the name dialed.
func (c *cliContext) controllerTLS() (*tls.Config, error) {
	var pinned []*x509.Certificate
	for rest := []byte(c.ControllerCA); ; {
		var b *pem.Block
		if b, rest = pem.Decode(rest); b == nil {
			break
		}
		cert, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return nil, err
		}
		pinned = append(pinned, cert)
	}
	if len(pinned) == 0 {
		return nil, errors.New("no Controller certificate to check it against: janusctl login -controller-fingerprint")
	}
	host, _, _ := net.SplitHostPort(c.Controller)
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, //nolint:gosec // verified in VerifyConnection below
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("no certificate from the Controller")
			}
			leaf := cs.PeerCertificates[0]
			c.seen = certFingerprint(leaf)
			for _, p := range pinned {
				if p.Equal(leaf) {
					return nil
				}
			}
			roots, inter := x509.NewCertPool(), x509.NewCertPool()
			for _, p := range pinned {
				roots.AddCert(p)
			}
			for _, ic := range cs.PeerCertificates[1:] {
				inter.AddCert(ic)
			}
			_, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, DNSName: host})
			if err != nil {
				return fmt.Errorf("the Controller's certificate isn't the one trusted: %w", err)
			}
			return nil
		},
	}, nil
}

// call is a request to the Controller's API with token, its JSON answer
// decoded into out.
func (c *cliContext) call(method, path, token string, body, out any) error {
	tlsConfig, err := c.controllerTLS()
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "https://"+c.Controller+path, rd)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsConfig}}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(data))
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return fmt.Errorf("%s %s: %s", method, path, msg)
	}
	return json.Unmarshal(data, out)
}

// signIn gets a new certificate - for a key made here - and the nodes,
// with token.
func (c *cliContext) signIn(name, token string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		return err
	}
	var cert struct {
		CertificatePEM string    `json:"certificate_pem"`
		ExpiresAt      time.Time `json:"expires_at"`
		User           string    `json:"user"`
		Role           string    `json:"role"`
	}
	if err := c.call("POST", "/api/cli/certificate", token, map[string]string{"csr_pem": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}))}, &cert); err != nil {
		return err
	}
	var inv struct {
		Nodes []ctxNode `json:"nodes"`
	}
	if err := c.call("GET", "/api/cli/inventory", token, nil, &inv); err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	dir := contextDir(name)
	if err := writeFileAtomic(filepath.Join(dir, "key.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(dir, "cert.pem"), []byte(cert.CertificatePEM), 0o600); err != nil {
		return err
	}
	c.User, c.Role, c.Expires, c.Nodes = cert.User, cert.Role, cert.ExpiresAt, inv.Nodes
	return nil
}

// sshSignIn gets a new certificate - for the SSH key itself - and the
// nodes, signing the Controller's challenge with key, for the certificate
// janusctl saw the Controller present.
func (c *cliContext) sshSignIn(name string, key *sshKey) error {
	fp := key.fingerprint()
	var ch struct {
		Challenge string `json:"challenge"`
	}
	if err := c.call("POST", "/api/cli/challenge", "", map[string]string{"user": c.User, "fingerprint": fp}, &ch); err != nil {
		return err
	}
	sig, err := sshsig.Sign(randReader, key.ssh, "janus-login", []byte("janus-login\n"+c.seen+"\n"+ch.Challenge))
	if err != nil {
		return fmt.Errorf("sign with the SSH key: %w", err)
	}
	var out struct {
		CertificatePEM string    `json:"certificate_pem"`
		ExpiresAt      time.Time `json:"expires_at"`
		User           string    `json:"user"`
		Role           string    `json:"role"`
		Nodes          []ctxNode `json:"nodes"`
	}
	err = c.call("POST", "/api/cli/ssh-login", "", map[string]string{"user": c.User, "fingerprint": fp, "challenge": ch.Challenge, "signature": string(sig)}, &out)
	if err != nil {
		return fmt.Errorf("%w - is the key %s added to %s's account (the Controller's page, your account)?", err, fp, c.User)
	}
	dir := contextDir(name)
	if err := writeFileAtomic(filepath.Join(dir, "cert.pem"), []byte(out.CertificatePEM), 0o600); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dir, "key.pem")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	c.User, c.Role, c.Expires, c.Nodes, c.SSHKey = out.User, out.Role, out.ExpiresAt, out.Nodes, key.spec
	return nil
}

// --- using a context ---

// currentContext is the context named, or the current one; nil without
// any.
func currentContext(cfg *cliConfig, name string) (string, *cliContext) {
	if name == "" {
		name = cfg.Current
	}
	return name, cfg.Contexts[name]
}

// selectNodes are the nodes -n names (names or IDs, comma-separated), or
// every node reachable with the certificate for -all, or the only one.
func selectNodes(ctx *cliContext, names string, all bool) ([]ctxNode, error) {
	var reachable []ctxNode
	for _, n := range ctx.Nodes {
		if n.Fleet {
			reachable = append(reachable, n)
		}
	}
	switch {
	case all:
		if len(reachable) == 0 {
			return nil, errors.New("no node trusts the fleet yet")
		}
		return reachable, nil
	case names == "":
		if len(reachable) == 1 {
			return reachable, nil
		}
		return nil, fmt.Errorf("which node? -n NAME (or -all): %s", nodeNames(ctx.Nodes))
	}
	var out []ctxNode
	for _, want := range strings.Split(names, ",") {
		want = strings.TrimSpace(want)
		i := slices.IndexFunc(ctx.Nodes, func(n ctxNode) bool { return n.Name == want || n.ID == want })
		if i < 0 {
			return nil, fmt.Errorf("no node %q in this context: %s (janusctl login refreshes the list)", want, nodeNames(ctx.Nodes))
		}
		if !ctx.Nodes[i].Fleet {
			return nil, fmt.Errorf("%s doesn't trust the fleet yet: update it, or use its own certificate (-endpoint -ca -cert -key)", want)
		}
		out = append(out, ctx.Nodes[i])
	}
	return out, nil
}

func nodeNames(nodes []ctxNode) string {
	var names []string
	for _, n := range nodes {
		s := n.Name
		if !n.Fleet {
			s += " (not in the fleet)"
		}
		names = append(names, s)
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// fresh renews the context's certificate when it's ending - with its SSH
// key, or JANUS_TOKEN; else it says to sign in again.
func fresh(cfg *cliConfig, name string, ctx *cliContext) error {
	if time.Until(ctx.Expires) > renewBefore {
		return nil
	}
	if token := os.Getenv(tokenEnv); token != "" && ctx.SSHKey == "" {
		if err := ctx.signIn(name, token); err != nil {
			return fmt.Errorf("renew the certificate: %w", err)
		}
		return cfg.save()
	}
	if ctx.SSHKey == "" {
		return fmt.Errorf("the certificate of context %q ended at %s: janusctl login", name, ctx.Expires.Local().Format("2006-01-02 15:04"))
	}
	key, err := openSSHKey(ctx.SSHKey)
	if err != nil {
		return fmt.Errorf("renew the certificate: %w", err)
	}
	defer key.Close()
	if err := ctx.sshSignIn(name, key); err != nil {
		return fmt.Errorf("renew the certificate: %w", err)
	}
	return cfg.save()
}

// nodeTLS is janusctl's TLS to node: the context's certificate - with its
// SSH key, or the key made for it -, the node's own CA.
func nodeTLS(name string, ctx *cliContext, n ctxNode) (*tls.Config, error) {
	dir := contextDir(name)
	var cert tls.Certificate
	var err error
	if ctx.SSHKey != "" {
		cert, err = sshKeyPair(filepath.Join(dir, "cert.pem"), ctx.SSHKey)
	} else {
		cert, err = tls.LoadX509KeyPair(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	}
	if err != nil {
		return nil, fmt.Errorf("the context's certificate: %w (janusctl login)", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(n.CAPEM)) {
		return nil, fmt.Errorf("%s: no CA certificate in the context (janusctl login refreshes it)", n.Name)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: pool, MinVersion: tls.VersionTLS13}, nil
}

// sshKeyPair is the certificate at certFile with the SSH key spec names -
// its signer stays open (an agent's connection) for the process's life.
func sshKeyPair(certFile, spec string) (tls.Certificate, error) {
	data, err := os.ReadFile(certFile)
	if err != nil {
		return tls.Certificate{}, err
	}
	var cert tls.Certificate
	for rest := data; ; {
		var b *pem.Block
		if b, rest = pem.Decode(rest); b == nil {
			break
		}
		cert.Certificate = append(cert.Certificate, b.Bytes)
	}
	if len(cert.Certificate) == 0 {
		return tls.Certificate{}, errors.New("no certificate")
	}
	if cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
		return tls.Certificate{}, err
	}
	key, err := openSSHKey(spec)
	if err != nil {
		return tls.Certificate{}, err
	}
	cert.PrivateKey = key.tls
	return cert, nil
}

// runOnEach runs the command on each node: janusctl again, once per
// node, at once - each line of their output prefixed with the node's
// name. Exits with the first failure's status.
func runOnEach(ctxName string, ctx *cliContext, nodes []ctxNode, cmdArgs []string) {
	if len(cmdArgs) >= 2 && cmdArgs[0] == "system" && cmdArgs[1] == "pcap" {
		log.Fatal("janusctl: a packet capture is one node at a time (-n NAME)")
	}
	if encryptedKeyFile(ctx.SSHKey) {
		log.Fatal("janusctl: the context's SSH key file has a passphrase - each node would ask it: add the key to ssh-agent (ssh-add) to run on several nodes")
	}
	self, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}
	width := 0
	for _, n := range nodes {
		width = max(width, len(n.Name))
	}
	var (
		wg     sync.WaitGroup
		outMu  sync.Mutex
		status int
	)
	for _, n := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(self, append([]string{"-context", ctxName, "-n", n.ID}, cmdArgs...)...)
			cmd.Env = os.Environ()
			stdout, _ := cmd.StdoutPipe()
			stderr, _ := cmd.StderrPipe()
			if err := cmd.Start(); err != nil {
				outMu.Lock()
				fmt.Fprintf(os.Stderr, "%-*s  %v\n", width, n.Name, err)
				status = max(status, 1)
				outMu.Unlock()
				return
			}
			var copies sync.WaitGroup
			for _, p := range []struct {
				r io.Reader
				w io.Writer
			}{{stdout, os.Stdout}, {stderr, os.Stderr}} {
				copies.Add(1)
				go func() {
					defer copies.Done()
					sc := bufio.NewScanner(p.r)
					sc.Buffer(make([]byte, 64<<10), 4<<20)
					for sc.Scan() {
						outMu.Lock()
						fmt.Fprintf(p.w, "%-*s  %s\n", width, n.Name, sc.Text())
						outMu.Unlock()
					}
				}()
			}
			copies.Wait()
			if err := cmd.Wait(); err != nil {
				outMu.Lock()
				code := 1
				var ee *exec.ExitError
				if errors.As(err, &ee) {
					code = ee.ExitCode()
				}
				if status == 0 {
					status = code
				}
				outMu.Unlock()
			}
		}()
	}
	wg.Wait()
	os.Exit(status)
}

// --- janusctl context / nodes ---

func runContext(args []string) {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	if len(args) == 0 || args[0] == "list" {
		names := make([]string, 0, len(cfg.Contexts))
		for name := range cfg.Contexts {
			names = append(names, name)
		}
		sort.Strings(names)
		if len(names) == 0 {
			fmt.Println("No context: janusctl login -controller HOST")
			return
		}
		for _, name := range names {
			c := cfg.Contexts[name]
			mark := " "
			if name == cfg.Current {
				mark = "*"
			}
			state := "until " + c.Expires.Local().Format("2006-01-02 15:04")
			if time.Now().After(c.Expires) {
				state = "ended - janusctl login"
			}
			fmt.Printf("%s %-12s %s  %s (%s), %s\n", mark, name, c.Controller, c.User, c.Role, state)
		}
		return
	}
	if len(args) != 2 || (args[0] != "use" && args[0] != "delete") {
		log.Fatal("usage: janusctl context [list | use NAME | delete NAME]")
	}
	name := args[1]
	if cfg.Contexts[name] == nil {
		log.Fatalf("no context %q", name)
	}
	if args[0] == "use" {
		cfg.Current = name
	} else {
		delete(cfg.Contexts, name)
		if cfg.Current == name {
			cfg.Current = ""
		}
		if err := os.RemoveAll(contextDir(name)); err != nil {
			log.Fatal(err)
		}
	}
	if err := cfg.save(); err != nil {
		log.Fatal(err)
	}
}

func runNodes(ctxFlag string) {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	_, ctx := currentContext(cfg, ctxFlag)
	if ctx == nil {
		log.Fatal("no context: janusctl login -controller HOST")
	}
	if token := os.Getenv(tokenEnv); token != "" {
		var inv struct {
			Nodes []ctxNode `json:"nodes"`
		}
		if err := ctx.call("GET", "/api/cli/inventory", token, nil, &inv); err != nil {
			log.Fatal(err)
		}
		ctx.Nodes = inv.Nodes
		if err := cfg.save(); err != nil {
			log.Fatal(err)
		}
	}
	fmt.Printf("%-20s %-18s %-24s %s\n", "NAME", "ID", "ADDRESS", "FLEET")
	for _, n := range ctx.Nodes {
		fleet := "yes"
		if !n.Fleet {
			fleet = "no - its own certificate only"
		}
		fmt.Printf("%-20s %-18s %-24s %s\n", n.Name, n.ID, n.Address, fleet)
	}
}
