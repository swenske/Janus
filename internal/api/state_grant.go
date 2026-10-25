package api

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"slices"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/events"
	"github.com/swenske/Janus/internal/pki"
	"github.com/swenske/Janus/internal/statexfer"
)

// A new node pulling an old node's state itself (docs/guide/
// replacing-a-node.md "Node to node"): the new node makes a key for it
// (CertificateRequest STATE_TRANSFER, kept in memory), the old node
// signs its request with its own CA - ten minutes, one export, the
// identity only if granted (StateExportGrant, the grant kept in memory) -
// and the new node dials the old with it (StateImportFrom). No key
// leaves the node that made it: the operator's janusctl carries a
// signing request and a certificate, and the state goes from node to
// node.

// transferValidity is how long a state transfer certificate is good for.
const transferValidity = 10 * time.Minute

// stateGrant is one StateExportGrant: an export the certificate it
// signed may make.
type stateGrant struct {
	identity bool
	expires  time.Time
}

// stateGrants are the live grants, by their certificate's SHA-256 - in
// memory: janusd's restart ends them.
var stateGrants = struct {
	sync.Mutex
	m map[string]stateGrant
}{m: map[string]stateGrant{}}

func addGrant(fp string, g stateGrant) {
	stateGrants.Lock()
	defer stateGrants.Unlock()
	now := time.Now()
	for k, x := range stateGrants.m {
		if now.After(x.expires) {
			delete(stateGrants.m, k)
		}
	}
	stateGrants.m[fp] = g
}

// grantFor is the live grant of the certificate fp.
func grantFor(fp string) (stateGrant, bool) {
	stateGrants.Lock()
	defer stateGrants.Unlock()
	g, ok := stateGrants.m[fp]
	return g, ok && time.Now().Before(g.expires)
}

// takeGrant uses the grant of the certificate fp up: false when it was
// already, or ended.
func takeGrant(fp string) (stateGrant, bool) {
	stateGrants.Lock()
	defer stateGrants.Unlock()
	g, ok := stateGrants.m[fp]
	delete(stateGrants.m, fp)
	return g, ok && time.Now().Before(g.expires)
}

func certSHA256(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// authorizeTransfer checks a state transfer certificate's call: only the
// RPCs taking pki.RoleStateTransfer (StateExport), only signed by this
// node's own CA - a fleet's CA can't make one -, only while its grant
// lives.
func authorizeTransfer(leaf *x509.Certificate, chains [][]*x509.Certificate, fullMethod string) (Caller, error) {
	if !slices.Contains(rolesFor(fullMethod), pki.RoleStateTransfer) {
		return Caller{}, status.Errorf(codes.PermissionDenied, "%s isn't for a state transfer certificate: it opens StateExport, once", fullMethod)
	}
	if len(leaf.Subject.Organization) != 1 {
		return Caller{}, status.Error(codes.PermissionDenied, "a state transfer certificate carries no role of its own")
	}
	var ca *x509.Certificate
	if LocalCA != nil {
		ca = LocalCA()
	}
	if ca == nil || len(chains) == 0 || len(chains[0]) < 2 || !bytes.Equal(pki.CAKey(chains[0][1]), pki.CAKey(ca)) {
		return Caller{}, status.Error(codes.PermissionDenied, "a state transfer certificate this node's own CA didn't sign")
	}
	fp := certSHA256(leaf.Raw)
	if _, ok := grantFor(fp); !ok {
		return Caller{}, status.Errorf(codes.PermissionDenied, "%s's state transfer certificate: no grant for it - used already, ended, or janusd restarted since (grant another: janusctl system state import -from)", leaf.Subject.CommonName)
	}
	return Caller{Name: leaf.Subject.CommonName, Roles: []string{pki.RoleStateTransfer}, Transfer: fp}, nil
}

// transferKey is the key a new node pulls another's state with: made by
// CertificateRequest STATE_TRANSFER, used once by StateImportFrom.
type transferKey struct {
	mu  sync.Mutex
	key *ecdsa.PrivateKey
}

// newCSR replaces the key with a new one: its signing request, PEM.
func (t *transferKey) newCSR() ([]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	name, _ := os.Hostname()
	if !clientNamePattern.MatchString(name) {
		name = "replacement"
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: name}}, key)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	t.key = key
	t.mu.Unlock()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}

// take is the key, gone from memory: used once.
func (t *transferKey) take() *ecdsa.PrivateKey {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := t.key
	t.key = nil
	return k
}

// StateExportGrant signs another node's request for one export of this
// node's state.
func (s *System) StateExportGrant(ctx context.Context, req *janusv1alpha1.StateExportGrantRequest) (*janusv1alpha1.StateExportGrantResponse, error) {
	caller, _ := CallerFrom(ctx)
	if err := s.stateTransferAllowed(caller); err != nil {
		return nil, err
	}
	block, _ := pem.Decode(req.GetCsr())
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, status.Error(codes.InvalidArgument, "csr: a PEM signing request (CertificateRequest STATE_TRANSFER on the new node)")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err == nil {
		err = csr.CheckSignature()
	}
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "csr: %v", err)
	}
	if k, ok := csr.PublicKey.(*ecdsa.PublicKey); !ok || k.Curve != elliptic.P256() {
		return nil, status.Error(codes.InvalidArgument, "csr: an ECDSA P-256 key, as CertificateRequest makes")
	}
	name := csr.Subject.CommonName
	if !clientNamePattern.MatchString(name) {
		return nil, status.Errorf(codes.InvalidArgument, "csr: the name %q - up to 64 letters, digits, spaces and ._@:+-", name)
	}
	ca := s.LocalCA()
	certPEM, err := ca.IssueFor(csr.PublicKey, pki.IssueOptions{
		CommonName:  name,
		Roles:       []string{pki.RoleStateTransfer},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		Validity:    transferValidity,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "sign: %v", err)
	}
	b, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "sign: %v", err)
	}
	addGrant(certSHA256(cert.Raw), stateGrant{identity: req.GetIncludeIdentity(), expires: cert.NotAfter})
	log.Printf("state: %s may pull this node's state once, until %s (identity %v) - granted by %s", name, cert.NotAfter.UTC().Format(time.RFC3339), req.GetIncludeIdentity(), caller)
	events.Publish("system.state_export_granted", map[string]any{"to": name, "identity": req.GetIncludeIdentity(), "until": cert.NotAfter.UTC().Format(time.RFC3339), "by": caller.String()})
	return &janusv1alpha1.StateExportGrantResponse{Certificate: certPEM, Ca: ca.CertPEM, ExpiresUnix: cert.NotAfter.Unix()}, nil
}

// StateImportFrom pulls another node's state and stages it.
func (s *System) StateImportFrom(ctx context.Context, req *janusv1alpha1.StateImportFromRequest) (*janusv1alpha1.StateImportResponse, error) {
	caller, _ := CallerFrom(ctx)
	if err := s.stateTransferAllowed(caller); err != nil {
		return nil, err
	}
	opts := req.GetOptions()
	if opts == nil {
		opts = &janusv1alpha1.StateImportOptions{}
	}
	if why := statexfer.InUse(s.StateRoot); why != "" {
		return nil, status.Errorf(codes.FailedPrecondition, "this node is in use (%s): Reset it first (janusctl system reset -state), then import", why)
	}
	addr := req.GetAddress()
	if addr == "" {
		return nil, status.Error(codes.InvalidArgument, "address: where this node reaches the other node's API")
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, "9505")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(req.GetCa()) {
		return nil, status.Error(codes.InvalidArgument, "ca: the other node's CA, PEM (StateExportGrant's)")
	}
	block, _ := pem.Decode(req.GetCertificate())
	if block == nil {
		return nil, status.Error(codes.InvalidArgument, "certificate: the one the other node granted, PEM")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "certificate: %v", err)
	}
	// One key, one attempt: a failure means a new request and a new grant.
	key := s.transfer.take()
	if key == nil {
		return nil, status.Error(codes.FailedPrecondition, "no key made for a state transfer, or used already: CertificateRequest STATE_TRANSFER first (janusctl system state import -from does it all)")
	}
	if pub, ok := leaf.PublicKey.(*ecdsa.PublicKey); !ok || !pub.Equal(key.Public()) {
		return nil, status.Error(codes.InvalidArgument, "certificate: not for the key this node made - grant this node's latest request")
	}
	// The other node is told by its CA, pinned: its server certificate
	// names its own addresses, never the one a NAT or a name may give it -
	// and always "localhost".
	creds := credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}},
		RootCAs:      pool,
		ServerName:   "localhost",
		MinVersion:   tls.VersionTLS13,
	})
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "dial %s: %v", addr, err)
	}
	defer conn.Close()
	pctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	exp, err := janusv1alpha1.NewSystemServiceClient(conn).StateExport(pctx, &janusv1alpha1.StateExportRequest{IncludeIdentity: opts.GetReuseIdentity()})
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "%s: %v", addr, err)
	}
	pr, pw := io.Pipe()
	// The source's own error, when it's what stopped the staging - not
	// one that follows it (the pull cancelled once staging is over).
	var mu sync.Mutex
	var srcErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			d, err := exp.Recv()
			if err == io.EOF {
				pw.Close()
				return
			}
			if err != nil {
				mu.Lock()
				srcErr = err
				mu.Unlock()
				pw.CloseWithError(err)
				return
			}
			if _, err := pw.Write(d.GetBytes()); err != nil {
				return // staging stopped reading
			}
		}
	}()
	resp, err := s.stageState(caller, opts, pr, "pulled from "+addr)
	mu.Lock()
	serr := srcErr
	mu.Unlock()
	pr.CloseWithError(errors.New("the import is over"))
	cancel()
	<-done
	if err != nil {
		if serr != nil {
			if st, ok := status.FromError(serr); ok {
				return nil, status.Errorf(st.Code(), "%s: %s", addr, st.Message())
			}
			return nil, status.Errorf(codes.Unavailable, "%s: %v", addr, serr)
		}
		return nil, err
	}
	if resp.Rebooting {
		s.schedulePower(syscall.LINUX_REBOOT_CMD_RESTART)
	}
	return resp, nil
}

// transferExport: a state transfer certificate's export uses its grant
// up - the identity only if granted.
func transferExport(caller Caller, identity bool) error {
	if caller.Transfer == "" {
		return nil
	}
	g, ok := grantFor(caller.Transfer)
	if ok && identity && !g.identity {
		return status.Error(codes.PermissionDenied, "the grant doesn't include this node's identity: grant it with the identity")
	}
	if _, ok := takeGrant(caller.Transfer); !ok {
		return status.Error(codes.PermissionDenied, "this state transfer certificate's grant was used already, or ended")
	}
	return nil
}
