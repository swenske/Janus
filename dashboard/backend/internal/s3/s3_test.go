package s3

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestSigV4Vectors: AWS's own examples (S3 API reference, "Signature
// Calculations for the Authorization Header", with
// AKIAIOSFODNN7EXAMPLE).
func TestSigV4Vectors(t *testing.T) {
	endpoint, _ := url.Parse("https://s3.amazonaws.com")
	c := &Client{Endpoint: endpoint, Region: "us-east-1", Bucket: "examplebucket", AccessKey: "AKIAIOSFODNN7EXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		now: func() time.Time { return time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC) }}
	for name, tc := range map[string]struct {
		key, query string
		headers    map[string]string
		want       string
	}{
		"GET Object":   {"test.txt", "", map[string]string{"Range": "bytes=0-9"}, "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"},
		"List Objects": {"", "max-keys=2&prefix=J", nil, "34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7"},
	} {
		host, path := c.target(tc.key)
		q, _ := url.ParseQuery(tc.query)
		req, _ := http.NewRequest("GET", "https://"+host+path+"?"+tc.query, nil)
		req.Host = host
		req.URL.RawQuery = canonicalQuery(q)
		for k, v := range tc.headers {
			req.Header.Set(k, v)
		}
		c.sign(req, path, emptySHA256)
		if got := req.Header.Get("Authorization"); !strings.HasSuffix(got, "Signature="+tc.want) {
			t.Errorf("%s: %s", name, got)
		}
	}
}

func TestTarget(t *testing.T) {
	endpoint, _ := url.Parse("https://minio.example:9000/")
	c := &Client{Endpoint: endpoint, Bucket: "backups", PathStyle: true}
	if host, path := c.target("janus/2026-10-05 a+b.age"); host != "minio.example:9000" || path != "/backups/janus/2026-10-05%20a%2Bb.age" {
		t.Errorf("path-style: %s %s", host, path)
	}
	c.PathStyle = false
	if host, path := c.target("k"); host != "backups.minio.example:9000" || path != "/k" {
		t.Errorf("virtual-host: %s %s", host, path)
	}
}
