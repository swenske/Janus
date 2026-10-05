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
// JANUS_TEST_S3 = "https://ACCESS:SECRET@host:port/bucket" (path-style),
// JANUS_TEST_S3_REGION (us-east-1) and JANUS_TEST_S3_PREFIX ("live/") -
// hack/qemu-dashboard-test.sh runs versitygw for it; Backblaze B2 with a
// key limited to a prefix, without the right to delete (the deletes are
// then skipped). Skipped without.
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
	region, prefix := os.Getenv("JANUS_TEST_S3_REGION"), os.Getenv("JANUS_TEST_S3_PREFIX")
	if region == "" {
		region = "us-east-1"
	}
	if prefix == "" {
		prefix = "live/"
	}
	c := &Client{Endpoint: &url.URL{Scheme: u.Scheme, Host: u.Host}, Region: region, Bucket: strings.Trim(u.Path, "/"), AccessKey: u.User.Username(), SecretKey: secret, PathStyle: true}
	ctx := context.Background()
	body := bytes.Repeat([]byte("janus backup "), 1000)
	keys := []string{prefix + "2026-10-05T03:00:00Z a+b.age", prefix + "2026-10-05T03:00:00Z a+b.manifest.json"}
	for _, k := range keys {
		if err := c.Put(ctx, k, body, "application/octet-stream"); err != nil {
			t.Fatalf("put %s: %v", k, err)
		}
	}
	got, err := c.Get(ctx, keys[0])
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("get: %v (%d bytes)", err, len(got))
	}
	list, err := c.List(ctx, prefix)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := 0
	for _, o := range list {
		if (o.Key == keys[0] || o.Key == keys[1]) && o.Size == int64(len(body)) {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("list: %+v", list)
	}
	for i, k := range keys {
		err := c.Delete(ctx, k)
		if i == 0 && IsAccessDenied(err) {
			t.Logf("deleting refused (%v): a key without the right to delete - the deletes are skipped", err)
			break
		}
		if err != nil {
			t.Fatalf("delete %s: %v", k, err)
		}
		if i == 0 {
			if _, err := c.Get(ctx, k); !IsNotFound(err) {
				t.Errorf("a deleted object: %v", err)
			}
		}
	}
	bad := *c
	bad.SecretKey = "wrong"
	var e *Error
	// Refused: 403 - or, from Backblaze B2 for an upload, IncompleteBody
	// (400), once it stopped reading it.
	if err := bad.Put(ctx, prefix+"x", body, "text/plain"); !errors.As(err, &e) || (e.Status != 403 && e.Code != "IncompleteBody") {
		t.Errorf("a wrong secret: %v", err)
	}
	if _, err := bad.Get(ctx, keys[1]); !errors.As(err, &e) || e.Status != 403 {
		t.Errorf("a wrong secret, reading: %v", err)
	}
}
