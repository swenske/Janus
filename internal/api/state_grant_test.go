package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/pki"
)

// stateNode is a node serving SystemService over real mTLS from root,
// its CA ca - the old node of a transfer.
func stateNode(t *testing.T, ca *pki.CA, root string) (*System, string) {
	t.Helper()
	srvPEM, srvKey, err := ca.Issue(pki.IssueOptions{CommonName: "old", DNSNames: []string{"old", "localhost"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	if err != nil {
		t.Fatal(err)
	}
	srvCert, err := tls.X509KeyPair(srvPEM, srvKey)
	if err != nil {
		t.Fatal(err)
	}
	s := &System{BuildVersion: "test", LocalCA: func() *pki.CA { return ca }, StateRoot: root}
	srv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(ca.ServerTLSConfig(srvCert))),
		grpc.UnaryInterceptor(UnaryAuthInterceptor),
		grpc.StreamInterceptor(StreamAuthInterceptor),
	)
	janusv1alpha1.RegisterSystemServiceServer(srv, s)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return s, lis.Addr().String()
}

func writeState(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestStateImportFrom: a new node pulls an old node's state itself, over
// real mTLS - the new node's key made in its memory, the old node's
// grant opening one export, the identity only when granted, nothing
// else; never through the Controller, never onto a node in use.
func TestStateImportFrom(t *testing.T) {
	caA, err := pki.NewCA("old node")
	if err != nil {
		t.Fatal(err)
	}
	caB, err := pki.NewCA("new node")
	if err != nil {
		t.Fatal(err)
	}
	defer func(prev func() *x509.Certificate) { LocalCA = prev }(LocalCA)
	LocalCA = func() *x509.Certificate { return caA.Cert } // what the old node's authz reads

	rootA, rootB := t.TempDir(), t.TempDir()
	writeState(t, rootA, map[string]string{
		"haproxy/haproxy.cfg":            "global\n",
		"haproxy/files/maps/hosts.map":   "example.test be\n",
		"pki/ca.key":                     "the old node's CA key\n",
		"config/sysctl.d/90-tuning.conf": "net.core.somaxconn = 8192\n",
	})
	sysA, addrA := stateNode(t, caA, rootA)
	sysB := &System{BuildVersion: "test", LocalCA: func() *pki.CA { return caB }, StateRoot: rootB}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin := context.WithValue(ctx, callerKey{}, Caller{Name: "alice", Roles: []string{pki.RoleAdmin}})
	purpose := &janusv1alpha1.CertificateRequestRequest{Purpose: janusv1alpha1.CertificatePurpose_CERTIFICATE_PURPOSE_STATE_TRANSFER}

	// Through the Controller - a cluster seeded from a node -: the steps,
	// but never the identity; the state itself never (StateExport and
	// StateImport stay refused).
	viaCtl := context.WithValue(ctx, callerKey{}, Caller{Name: "alice", Roles: []string{pki.RoleAdmin}, Via: "janus-controller"})
	if _, err := sysA.StateExportGrant(viaCtl, &janusv1alpha1.StateExportGrantRequest{IncludeIdentity: true}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("a grant with the identity through the Controller: %v", err)
	}
	if _, err := sysB.StateImportFrom(viaCtl, &janusv1alpha1.StateImportFromRequest{Options: &janusv1alpha1.StateImportOptions{ReuseIdentity: true}}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("a pull of the identity through the Controller: %v", err)
	}
	if err := sysA.StateExport(&janusv1alpha1.StateExportRequest{}, &discardExport{ctx: viaCtl}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("an export through the Controller: %v", err)
	}
	if _, err := sysB.CertificateRequest(viaCtl, purpose); err != nil {
		t.Errorf("a transfer key's request through the Controller: %v", err)
	}
	sysB.transfer.take() // that key: not the one the steps below make
	if _, err := sysA.StateExportGrant(viaCtl, &janusv1alpha1.StateExportGrantRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a grant without the identity through the Controller (an empty request): %v", err)
	}
	// No key made: nothing to pull with.
	if _, err := sysB.StateImportFrom(admin, &janusv1alpha1.StateImportFromRequest{Address: addrA, Ca: caA.CertPEM, Certificate: caA.CertPEM}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("no key made: %v", err)
	}

	// The whole way, without the identity.
	csr, err := sysB.CertificateRequest(admin, purpose)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := sysA.StateExportGrant(admin, &janusv1alpha1.StateExportGrantRequest{Csr: csr.GetCsr()})
	if err != nil {
		t.Fatal(err)
	}
	if left := time.Until(time.Unix(grant.GetExpiresUnix(), 0)); left < 9*time.Minute || left > 11*time.Minute {
		t.Errorf("the grant lasts %v", left)
	}
	// Asking the identity a grant doesn't give: refused, and the key
	// gone with it - a new request, a new grant.
	if _, err := sysB.StateImportFrom(admin, &janusv1alpha1.StateImportFromRequest{Address: addrA, Ca: grant.GetCa(), Certificate: grant.GetCertificate(), Options: &janusv1alpha1.StateImportOptions{ReuseIdentity: true, NoReboot: true}}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("the identity without a grant for it: %v", err)
	}
	if _, err := sysB.StateImportFrom(admin, &janusv1alpha1.StateImportFromRequest{Address: addrA, Ca: grant.GetCa(), Certificate: grant.GetCertificate(), Options: &janusv1alpha1.StateImportOptions{NoReboot: true}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("the key used again: %v", err)
	}
	if _, err := sysB.CertificateRequest(admin, purpose); err != nil {
		t.Fatal(err)
	}
	// A certificate for another request: refused before dialing.
	if _, err := sysB.StateImportFrom(admin, &janusv1alpha1.StateImportFromRequest{Address: addrA, Ca: grant.GetCa(), Certificate: grant.GetCertificate(), Options: &janusv1alpha1.StateImportOptions{NoReboot: true}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("another request's certificate: %v", err)
	}
	csr, err = sysB.CertificateRequest(admin, purpose)
	if err != nil {
		t.Fatal(err)
	}
	grant, err = sysA.StateExportGrant(admin, &janusv1alpha1.StateExportGrantRequest{Csr: csr.GetCsr()})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := sysB.StateImportFrom(admin, &janusv1alpha1.StateImportFromRequest{Address: addrA, Ca: grant.GetCa(), Certificate: grant.GetCertificate(), Options: &janusv1alpha1.StateImportOptions{NoReboot: true}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(resp.GetFiles(), "haproxy/files/maps/hosts.map") || slices.Contains(resp.GetFiles(), "pki/ca.key") || resp.GetRebooting() {
		t.Errorf("pulled: %+v", resp)
	}
	if _, err := os.Stat(filepath.Join(rootB, "import", "ready")); err != nil {
		t.Errorf("nothing staged: %v", err)
	}

	// The node in use now (it has the import staged; an applied
	// configuration makes it so): no key for another pull.
	writeState(t, rootB, map[string]string{"haproxy/history/1.cfg": "global\n"})
	if _, err := sysB.CertificateRequest(admin, purpose); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a key on a node in use: %v", err)
	}
	if _, err := sysB.StateImportFrom(admin, &janusv1alpha1.StateImportFromRequest{Address: addrA, Ca: grant.GetCa(), Certificate: grant.GetCertificate()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a pull onto a node in use: %v", err)
	}

	// The grant itself, from a client holding the key: one export, the
	// identity only when granted, no other RPC.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "new"}}, key)
	if err != nil {
		t.Fatal(err)
	}
	grant, err = sysA.StateExportGrant(admin, &janusv1alpha1.StateExportGrantRequest{Csr: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})})
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(grant.GetCertificate())
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(grant.GetCa())
	conn, err := grpc.NewClient(addrA, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.Raw}, PrivateKey: key}}, RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS13,
	})))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := janusv1alpha1.NewSystemServiceClient(conn)
	export := func(identity bool) error {
		st, err := c.StateExport(ctx, &janusv1alpha1.StateExportRequest{IncludeIdentity: identity})
		if err != nil {
			return err
		}
		for {
			if _, err := st.Recv(); err == io.EOF {
				return nil
			} else if err != nil {
				return err
			}
		}
	}
	if _, err := c.Version(ctx, nil); status.Code(err) != codes.PermissionDenied {
		t.Errorf("a transfer certificate asking the version: %v", err)
	}
	if err := export(true); status.Code(err) != codes.PermissionDenied {
		t.Errorf("the identity, not granted: %v", err)
	}
	if err := export(false); err != nil {
		t.Errorf("the export granted: %v", err)
	}
	if err := export(false); status.Code(err) != codes.PermissionDenied {
		t.Errorf("a second export: %v", err)
	}
}

// TestStateTransferRole: a state transfer certificate opens StateExport
// alone, signed by this node's own CA, while its grant lives - whatever
// else it carries.
func TestStateTransferRole(t *testing.T) {
	own, err := pki.NewCA("own")
	if err != nil {
		t.Fatal(err)
	}
	other, err := pki.NewCA("a fleet's")
	if err != nil {
		t.Fatal(err)
	}
	defer func(prev func() *x509.Certificate) { LocalCA = prev }(LocalCA)
	LocalCA = func() *x509.Certificate { return own.Cert }
	leaf := &x509.Certificate{Raw: []byte("a transfer certificate"), Subject: pkix.Name{CommonName: "new", Organization: []string{pki.RoleStateTransfer}}}
	with := func(leaf, ca *x509.Certificate) context.Context {
		return peer.NewContext(context.Background(), &peer.Peer{Addr: &net.TCPAddr{}, AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{
			PeerCertificates: []*x509.Certificate{leaf},
			VerifiedChains:   [][]*x509.Certificate{{leaf, ca}},
		}}})
	}
	const export = "/janus.v1alpha1.SystemService/StateExport"
	if got := status.Code(checkRole(with(leaf, own.Cert), export)); got != codes.PermissionDenied {
		t.Errorf("no grant: %v", got)
	}
	addGrant(certSHA256(leaf.Raw), stateGrant{expires: time.Now().Add(time.Minute)})
	caller, err := authorize(with(leaf, own.Cert), export)
	if err != nil || caller.Transfer == "" || caller.Name != "new" {
		t.Errorf("granted: %v %+v", err, caller)
	}
	if got := status.Code(checkRole(with(leaf, other.Cert), export)); got != codes.PermissionDenied {
		t.Errorf("signed by another CA: %v", got)
	}
	for _, method := range []string{"/janus.v1alpha1.SystemService/Version", "/janus.v1alpha1.SystemService/StateImport", "/janus.v1alpha1.SystemService/StateExportGrant", "/janus.v1alpha1.HAProxyService/ApplyConfig"} {
		if got := status.Code(checkRole(with(leaf, own.Cert), method)); got != codes.PermissionDenied {
			t.Errorf("a transfer certificate calling %s: %v", method, got)
		}
	}
	admin := &x509.Certificate{Raw: leaf.Raw, Subject: pkix.Name{CommonName: "new", Organization: []string{pki.RoleStateTransfer, pki.RoleAdmin}}}
	if got := status.Code(checkRole(with(admin, own.Cert), "/janus.v1alpha1.SystemService/Reboot")); got != codes.PermissionDenied {
		t.Errorf("a transfer certificate claiming admin too: %v", got)
	}
	addGrant(certSHA256(leaf.Raw), stateGrant{expires: time.Now().Add(-time.Second)})
	if got := status.Code(checkRole(with(leaf, own.Cert), export)); got != codes.PermissionDenied {
		t.Errorf("an ended grant: %v", got)
	}
}

// discardExport is a StateExport stream that keeps nothing.
type discardExport struct {
	grpc.ServerStream
	ctx context.Context
}

func (d *discardExport) Context() context.Context       { return d.ctx }
func (d *discardExport) Send(*janusv1alpha1.Data) error { return nil }
