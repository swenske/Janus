package haproxy

import "testing"

// Fixture captured verbatim from `show ssl cert <name>` against a real
// haproxy 3.4.0 instance during development.
const showSSLCertFixture = `Filename: /tmp/newcert.pem
Option: ocsp-update off
Option: jwt off
Status: Unused
Serial: 3A83DFCAD09C3E78CD85094EFF488F11EF10CF3B
notBefore: Sep 22 16:12:14 2026 GMT
notAfter: Sep 23 16:12:14 2026 GMT
Subject Alternative Name:
Algorithm: OCSP Response Key:
`

func TestParseNotAfter(t *testing.T) {
	got := parseNotAfter([]byte(showSSLCertFixture))
	want := "2026-09-23T16:12:14Z"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestParseNotAfter_SingleDigitDay(t *testing.T) {
	// OpenSSL's ASN1_TIME print format space-pads (not zero-pads) the
	// day of month - "Sep  3" not "Sep 03" - notAfterLayout's "_2" must
	// handle that.
	got := parseNotAfter([]byte("notAfter: Sep  3 01:02:03 2027 GMT\n"))
	want := "2027-09-03T01:02:03Z"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestParseNotAfter_Missing(t *testing.T) {
	if got := parseNotAfter([]byte("Filename: x\nStatus: Unused\n")); got != "" {
		t.Fatalf("expected empty string when notAfter is absent, got %q", got)
	}
}

func TestParseCertField_Status(t *testing.T) {
	if got := parseCertField([]byte(showSSLCertFixture), "Status"); got != "Unused" {
		t.Fatalf("Status = %q, want %q", got, "Unused")
	}
}

func TestParseCertField_Missing(t *testing.T) {
	if got := parseCertField([]byte(showSSLCertFixture), "NoSuchField"); got != "" {
		t.Fatalf("expected empty string for a missing field, got %q", got)
	}
}

// Captured from `show ssl cert <name>` against build/haproxy (3.4.0) with
// a certificate that has a subject, which the fixture above lacks.
const showSSLCertWithSubject = `Filename: /tmp/site.pem
Status: Used
Serial: 3A1DE05526CB45289DD666D020F5AB23BB76F825
notBefore: Sep 30 23:45:47 2026 GMT
notAfter: Oct 30 23:45:47 2026 GMT
Subject Alternative Name: DNS:www.example.com, DNS:example.com
Algorithm: EC256
SHA1 FingerPrint: 0DAA8AAF1C4B58546F3304EB8E8B38E230DEFDAE
Subject: /O=Example Org/CN=www.example.com
Issuer: /O=Example Org/CN=www.example.com
OCSP Response Key:
`

func TestParseCertSubject(t *testing.T) {
	if got := parseCertField([]byte(showSSLCertWithSubject), "Subject"); got != "/O=Example Org/CN=www.example.com" {
		t.Fatalf("Subject = %q", got)
	}
	if got := parseNotAfter([]byte(showSSLCertWithSubject)); got != "2026-10-30T23:45:47Z" {
		t.Fatalf("notAfter = %q", got)
	}
}
