package s3

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
)

// TestLive puts, lists, gets and deletes against a real S3 service:
// JANUS_TEST_S3 = "https://ACCESS:SECRET@host:port/bucket" (path-style,
// region us-east-1) - hack/controller-backup-test.sh runs versitygw for
// it. Skipped without.
func TestLive(t *testing.T) {
	raw := os.Getenv("JANUS_TEST_S3")
	if raw == "" {
		t.Skip("JANUS_TEST_S3 unset")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := u.User.Password()
	c := &Client{Endpoint: &url.URL{Scheme: u.Scheme, Host: u.Host}, Region: "us-east-1", Bucket: strings.Trim(u.Path, "/"), AccessKey: u.User.Username(), SecretKey: secret, PathStyle: true}
	ctx := context.Background()
	body := bytes.Repeat([]byte("janus backup "), 1000)
	keys := []string{"live/2026-10-05T03:00:00Z a+b.age", "live/2026-10-05T03:00:00Z a+b.manifest.json"}
	for _, k := range keys {
		if err := c.Put(ctx, k, body, "application/octet-stream"); err != nil {
			t.Fatalf("put %s: %v", k, err)
		}
	}
	got, err := c.Get(ctx, keys[0])
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("get: %v (%d bytes)", err, len(got))
	}
	list, err := c.List(ctx, "live/")
	if err != nil || len(list) != 2 || list[0].Key != keys[0] || list[0].Size != int64(len(body)) {
		t.Fatalf("list: %+v %v", list, err)
	}
	for _, k := range keys {
		if err := c.Delete(ctx, k); err != nil {
			t.Fatalf("delete %s: %v", k, err)
		}
	}
	if _, err := c.Get(ctx, keys[0]); !IsNotFound(err) {
		t.Errorf("a deleted object: %v", err)
	}
	bad := *c
	bad.SecretKey = "wrong"
	var e *Error
	if err := bad.Put(ctx, "live/x", body, "text/plain"); !errors.As(err, &e) || e.Status != 403 {
		t.Errorf("a wrong secret: %v", err)
	}
}
