package nodeproxy

import (
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	pkcs12 "software.sslmate.com/src/go-pkcs12"

	"github.com/swenske/Janus/internal/pki"
)

func TestParseStatCSV(t *testing.T) {
	raw, err := os.ReadFile("../../../../internal/api/testdata/show-stat.csv")
	if err != nil {
		t.Fatal(err)
	}
	table, err := parseStatCSV(raw)
	if err != nil {
		t.Fatal(err)
	}
	if table.Columns[0] != "pxname" || table.Columns[1] != "svname" {
		t.Errorf("columns start %v", table.Columns[:2])
	}
	if len(table.Rows) != 5 || table.Rows[1][0] != "web" || table.Rows[1][1] != "web1" {
		t.Errorf("rows = %d, second row starts %v", len(table.Rows), table.Rows[1][:2])
	}
	empty, err := parseStatCSV(nil)
	if err != nil || len(empty.Rows) != 0 {
		t.Errorf("empty CSV = %+v, %v", empty, err)
	}
}

func TestEncodePFXRoundTrip(t *testing.T) {
	ca, err := pki.NewCA("test CA")
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := ca.Issue(pki.IssueOptions{CommonName: "operator", Roles: []string{"os:reader"}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	if err != nil {
		t.Fatal(err)
	}
	pfx, err := encodePFX(ca.CertPEM, certPEM, keyPEM, "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	key, cert, caCerts, err := pkcs12.DecodeChain(pfx, "s3cret")
	if err != nil {
		t.Fatalf("the .pfx doesn't decode with its password: %v", err)
	}
	if key == nil || cert.Subject.CommonName != "operator" || len(caCerts) != 1 || caCerts[0].Subject.CommonName != "test CA" {
		t.Errorf("decoded %v / %v / %v", key != nil, cert.Subject, caCerts)
	}
	block, _ := pem.Decode(certPEM)
	orig, _ := x509.ParseCertificate(block.Bytes)
	if !orig.Equal(cert) {
		t.Error("certificate changed through the .pfx")
	}
	if _, _, _, err := pkcs12.DecodeChain(pfx, "wrong"); err == nil {
		t.Error("wrong password accepted")
	}
}

type flushRecorder struct{ *httptest.ResponseRecorder }

func (f flushRecorder) Flush() {}

func TestSSESendFraming(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	s, ok := newSSE(flushRecorder{rec}, req)
	if !ok {
		t.Fatal("newSSE failed")
	}
	_ = s.send("", "line one\nline two")
	_ = s.send("failure", "boom")
	got := rec.Body.String()
	if want := "data: line one\ndata: line two\n\nevent: failure\ndata: boom\n\n"; got != want {
		t.Errorf("SSE body =\n%q\nwant\n%q", got, want)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestGRPCHTTPStatus(t *testing.T) {
	for code, want := range map[codes.Code]int{
		codes.InvalidArgument: 400, codes.FailedPrecondition: 400, codes.NotFound: 404,
		codes.PermissionDenied: 403, codes.Unimplemented: 501, codes.DeadlineExceeded: 504, codes.Unknown: 502,
	} {
		if got := grpcHTTPStatus(status.Error(code, "x")); got != want {
			t.Errorf("%v -> %d, want %d", code, got, want)
		}
	}
}
