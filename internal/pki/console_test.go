package pki

import (
	"crypto/x509"
	"encoding/pem"
	"testing"
)

// TestConsoleMark: a certificate issued with Console carries the mark
// once parsed; any other doesn't - a scoped one included.
func TestConsoleMark(t *testing.T) {
	ca, err := NewCA("node")
	if err != nil {
		t.Fatal(err)
	}
	parse := func(opts IssueOptions) *x509.Certificate {
		t.Helper()
		certPEM, _, err := ca.Issue(opts)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := pem.Decode(certPEM)
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	client := []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	if c := parse(IssueOptions{CommonName: "rescue:bob", Roles: []string{RoleAdmin}, ExtKeyUsage: client, Console: true}); !IsConsole(c) {
		t.Error("a console certificate without the mark")
	}
	if c := parse(IssueOptions{CommonName: "api", Roles: []string{RoleAdmin}, ExtKeyUsage: client}); IsConsole(c) {
		t.Error("an API certificate with the mark")
	}
	scope := &Scope{Any: []ScopePerm{{Role: RoleReader}}}
	if c := parse(IssueOptions{CommonName: "s", Scope: scope, ExtKeyUsage: client, Console: true}); !IsConsole(c) || len(c.Extensions) < 2 {
		t.Error("the mark and a scope together")
	}
	if IsConsole(nil) {
		t.Error("nil")
	}
}
