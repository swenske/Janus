// Package s3 is the little of S3's API the Controller's backups need -
// put, get, list and delete an object - signed with AWS Signature
// Version 4: AWS, MinIO, Garage, Ceph, Backblaze B2, Cloudflare R2,
// versitygw... alike.
package s3

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Client reaches one bucket.
type Client struct {
	// Endpoint is the service's base URL (https://s3.eu-west-3.amazonaws.com,
	// https://minio.example:9000).
	Endpoint *url.URL
	// Region is the bucket's region (us-east-1 for most non-AWS services).
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	// PathStyle addresses the bucket as <endpoint>/<bucket>/ rather than
	// <bucket>.<endpoint host>/ - what most self-hosted services want.
	PathStyle bool
	HTTP      *http.Client
	now       func() time.Time
}

// MaxObject bounds what Get reads.
const MaxObject = 1 << 30

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// Object is one listed object.
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// Error is the service's answer to a request that failed.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("S3: %s (%d): %s", e.Code, e.Status, e.Message)
	}
	return fmt.Sprintf("S3: HTTP %d", e.Status)
}

// IsNotFound reports whether err is a missing object or bucket.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

// IsAccessDenied reports whether err is a refusal - write-only
// credentials deleting or listing, say.
func IsAccessDenied(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusForbidden
}

func (c *Client) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// target is a request's host and canonical path for key.
func (c *Client) target(key string) (host, path string) {
	host = c.Endpoint.Host
	base := strings.TrimSuffix(c.Endpoint.EscapedPath(), "/")
	if c.PathStyle {
		path = base + "/" + uriEncode(c.Bucket, false)
	} else {
		host = c.Bucket + "." + host
		path = base
	}
	return host, path + "/" + uriEncode(key, false)
}

// uriEncode is SigV4's: everything but unreserved characters, and '/' too
// unless path.
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vals := append([]string{}, q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	return strings.Join(parts, "&")
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// sign adds SigV4's headers to req, its body hashing to payloadHash:
// every header set on req is signed.
func (c *Client) sign(req *http.Request, path string, payloadHash string) {
	now := c.clock().UTC()
	amzDate := now.Format("20060102T150405Z")
	day := now.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	headers := map[string]string{"host": req.Host}
	for k, v := range req.Header {
		headers[strings.ToLower(k)] = strings.Join(v, ",")
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + strings.TrimSpace(headers[k]) + "\n")
	}
	signed := strings.Join(names, ";")
	canonical := strings.Join([]string{req.Method, path, canonicalQuery(req.URL.Query()), canonHeaders.String(), signed, payloadHash}, "\n")
	scope := day + "/" + c.Region + "/s3/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + sha256Hex([]byte(canonical))
	key := hmacSHA256([]byte("AWS4"+c.SecretKey), day)
	key = hmacSHA256(key, c.Region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(key, toSign))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", c.AccessKey, scope, signed, sig))
}

func (c *Client) do(ctx context.Context, method, key string, query url.Values, body []byte, headers map[string]string) (*http.Response, error) {
	host, path := c.target(key)
	u := url.URL{Scheme: c.Endpoint.Scheme, Host: host, Opaque: "//" + host + path, RawQuery: canonicalQuery(query)}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return nil, err
	}
	req.URL = &u
	req.Host = host
	req.ContentLength = int64(len(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	hash := emptySHA256
	if body != nil {
		hash = sha256Hex(body)
	}
	c.sign(req, path, hash)
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Minute}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		e := &Error{Status: resp.StatusCode}
		var x struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		}
		if xml.Unmarshal(data, &x) == nil {
			e.Code, e.Message = x.Code, x.Message
		}
		return nil, e
	}
	return resp, nil
}

// Put writes key.
func (c *Client) Put(ctx context.Context, key string, body []byte, contentType string) error {
	resp, err := c.do(ctx, http.MethodPut, key, nil, body, map[string]string{"Content-Type": contentType})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Get reads key, MaxObject at most.
func (c *Client) Get(ctx context.Context, key string) ([]byte, error) {
	resp, err := c.do(ctx, http.MethodGet, key, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxObject+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxObject {
		return nil, fmt.Errorf("%s is larger than %d bytes", key, MaxObject)
	}
	return data, nil
}

// Delete removes key.
func (c *Client) Delete(ctx context.Context, key string) error {
	resp, err := c.do(ctx, http.MethodDelete, key, nil, nil, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// List returns the objects under prefix (ListObjectsV2, every page).
func (c *Client) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := c.do(ctx, http.MethodGet, "", q, nil, nil)
		if err != nil {
			return nil, err
		}
		var page struct {
			Contents []struct {
				Key          string    `xml:"Key"`
				Size         int64     `xml:"Size"`
				LastModified time.Time `xml:"LastModified"`
			} `xml:"Contents"`
			IsTruncated           bool   `xml:"IsTruncated"`
			NextContinuationToken string `xml:"NextContinuationToken"`
		}
		err = xml.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("S3 list: %w", err)
		}
		for _, o := range page.Contents {
			out = append(out, Object{Key: o.Key, Size: o.Size, LastModified: o.LastModified})
		}
		if !page.IsTruncated || page.NextContinuationToken == "" {
			return out, nil
		}
		token = page.NextContinuationToken
	}
}
