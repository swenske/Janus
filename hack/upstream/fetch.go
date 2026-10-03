package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// fetcher is the tool's only way out to the network, so tests can serve
// recorded upstream answers instead.
type fetcher interface {
	// get returns a URL's body; an HTTP status other than 200 is an error.
	get(ctx context.Context, url string) ([]byte, error)
	// post sends a JSON body and returns the answer's body.
	post(ctx context.Context, url string, body []byte) ([]byte, error)
	// download stores a URL's body in a file and returns its path - a
	// release artifact, possibly hundreds of megabytes, cached by URL.
	download(ctx context.Context, url string) (string, error)
}

type httpFetcher struct {
	client   *http.Client
	cacheDir string
	// githubToken authenticates api.github.com requests (GITHUB_TOKEN):
	// 5000 requests an hour instead of 60.
	githubToken string
}

func newHTTPFetcher() *httpFetcher {
	cache := os.Getenv("UPSTREAM_CACHE")
	if cache == "" {
		cache = filepath.Join("build", "upstream-cache")
	}
	return &httpFetcher{
		client:      &http.Client{Timeout: 10 * time.Minute},
		cacheDir:    cache,
		githubToken: os.Getenv("GITHUB_TOKEN"),
	}
}

// request sends a request, retried up to three times when the network or
// the server fails (not when it answers 4xx).
func (f *httpFetcher) request(ctx context.Context, method, url string, body []byte) (*http.Response, error) {
	var err error
	for attempt := range 3 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt*attempt) * 2 * time.Second):
			}
		}
		var resp *http.Response
		resp, err = f.try(ctx, method, url, body)
		if he, ok := err.(*httpError); err == nil || (ok && he.status < 500) {
			return resp, err
		}
	}
	return nil, err
}

func (f *httpFetcher) try(ctx context.Context, method, url string, body []byte) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "janus-upstream (https://github.com/swenske/Janus)")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if strings.HasPrefix(url, "https://api.github.com/") {
		req.Header.Set("Accept", "application/vnd.github+json")
		if f.githubToken != "" {
			req.Header.Set("Authorization", "Bearer "+f.githubToken)
		}
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, &httpError{url: url, status: resp.StatusCode, body: strings.TrimSpace(string(msg))}
	}
	return resp, nil
}

type httpError struct {
	url    string
	status int
	body   string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("GET %s: HTTP %d %s", e.url, e.status, e.body)
}

func (f *httpFetcher) get(ctx context.Context, url string) ([]byte, error) {
	resp, err := f.request(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 256<<20))
}

func (f *httpFetcher) post(ctx context.Context, url string, body []byte) ([]byte, error) {
	resp, err := f.request(ctx, http.MethodPost, url, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

func (f *httpFetcher) download(ctx context.Context, url string) (string, error) {
	sum := sha256.Sum256([]byte(url))
	path := filepath.Join(f.cacheDir, hex.EncodeToString(sum[:8])+"-"+filepath.Base(url))
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if err := os.MkdirAll(f.cacheDir, 0o755); err != nil {
		return "", err
	}
	resp, err := f.request(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	tmp, err := os.CreateTemp(f.cacheDir, ".download-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return "", fmt.Errorf("GET %s: %w", url, err)
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return path, os.Rename(tmp.Name(), path)
}

func getJSON(ctx context.Context, f fetcher, url string, v any) error {
	data, err := f.get(ctx, url)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", url, err)
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
