package acme

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// How the node reaches the CA (ACMEPath, docs/letsencrypt.md "How the
// node reaches Let's Encrypt"): directly, or through an HTTP(S) proxy.
// Saved apart from letsencrypt.json - acme/path.json, beside the account
// key: the proxy's password is a secret - because a janusd older than it
// refuses a letsencrypt.json with a field it doesn't know, and would lose
// its whole configuration after a rollback.

const pathFile = "acme/path.json"

// pathMode is p's mode: unspecified is direct.
func pathMode(p *janusv1alpha1.ACMEPath) janusv1alpha1.ACMEPathMode {
	if p == nil || p.Mode == janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_UNSPECIFIED {
		return janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_DIRECT
	}
	return p.Mode
}

// normalizePath trims p's fields and drops those its mode doesn't use; a
// direct path is nil - what's saved when nothing is.
func normalizePath(p *janusv1alpha1.ACMEPath) *janusv1alpha1.ACMEPath {
	if pathMode(p) == janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_DIRECT {
		return nil
	}
	p = proto.Clone(p).(*janusv1alpha1.ACMEPath)
	p.ProxyUrl = strings.TrimSpace(p.ProxyUrl)
	p.ProxyUsername = strings.TrimSpace(p.ProxyUsername)
	p.ProxyCa = strings.TrimSpace(p.ProxyCa)
	return p
}

// validatePath checks p (normalized).
func validatePath(p *janusv1alpha1.ACMEPath) []string {
	switch pathMode(p) {
	case janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_DIRECT:
		return nil
	case janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_PROXY:
	default:
		return []string{fmt.Sprintf("path: mode %s isn't one this node knows", p.Mode)}
	}
	var errs []string
	u, err := url.Parse(p.ProxyUrl)
	switch {
	case p.ProxyUrl == "":
		errs = append(errs, "path: a proxy needs its URL")
	case err != nil:
		errs = append(errs, fmt.Sprintf("path: proxy URL: %v", err))
	case u.Scheme != "http" && u.Scheme != "https":
		errs = append(errs, fmt.Sprintf("path: proxy URL %q: http:// or https://", p.ProxyUrl))
	case u.Host == "" || u.Hostname() == "":
		errs = append(errs, fmt.Sprintf("path: proxy URL %q has no host", p.ProxyUrl))
	case u.User != nil:
		errs = append(errs, "path: the proxy's credentials go in proxy_username and proxy_password, not in its URL")
	case (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "":
		errs = append(errs, fmt.Sprintf("path: proxy URL %q: a scheme, a host and a port only", p.ProxyUrl))
	}
	if p.ProxyCa != "" {
		if err == nil && u.Scheme != "https" {
			errs = append(errs, "path: proxy_ca is for an https:// proxy")
		}
		if !x509.NewCertPool().AppendCertsFromPEM([]byte(p.ProxyCa)) {
			errs = append(errs, "path: proxy_ca: no PEM certificate in it")
		}
	}
	if p.ProxyPassword != "" && p.ProxyUsername == "" {
		errs = append(errs, "path: a proxy password needs its username")
	}
	return errs
}

// mergePathSecret keeps the saved proxy password where p leaves it empty,
// for the same proxy and username.
func mergePathSecret(p, saved *janusv1alpha1.ACMEPath) {
	if p == nil || saved == nil || p.ProxyPassword != "" {
		return
	}
	if p.ProxyUrl == saved.ProxyUrl && p.ProxyUsername == saved.ProxyUsername {
		p.ProxyPassword = saved.ProxyPassword
	}
}

// redactPath is p without its password.
func redactPath(p *janusv1alpha1.ACMEPath) *janusv1alpha1.ACMEPath {
	if p == nil {
		return nil
	}
	p = proto.Clone(p).(*janusv1alpha1.ACMEPath)
	p.ProxyPassword = ""
	return p
}

// proxyURL is the proxy janus-acme goes through, its credentials in it;
// "" when it reaches the CA directly.
func proxyURL(p *janusv1alpha1.ACMEPath) string {
	if pathMode(p) != janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_PROXY {
		return ""
	}
	u, err := url.Parse(p.ProxyUrl)
	if err != nil {
		return ""
	}
	if p.ProxyUsername != "" {
		u.User = url.UserPassword(p.ProxyUsername, p.ProxyPassword)
	}
	return u.String()
}

// via says the path in words: "directly", "through the proxy URL".
func via(p *janusv1alpha1.ACMEPath) string {
	if pathMode(p) == janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_PROXY {
		return "through the proxy " + p.ProxyUrl
	}
	return "directly"
}

// loadPath reads the saved path; nil (direct) if there's none.
func loadPath() (*janusv1alpha1.ACMEPath, error) {
	data, err := os.ReadFile(filepath.Join(Dir, pathFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p := &janusv1alpha1.ACMEPath{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(data, p); err != nil {
		return nil, fmt.Errorf("%s: %w", pathFile, err)
	}
	return normalizePath(p), nil
}

// savePath writes p durably, or removes the file for a direct path.
func savePath(p *janusv1alpha1.ACMEPath) error {
	path := filepath.Join(Dir, pathFile)
	if p == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	data, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(p)
	if err != nil {
		return err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, data, "", "  "); err != nil { // protojson's own multiline output isn't stable
		return err
	}
	out.WriteByte('\n')
	return writeDurable(filepath.Dir(path), filepath.Base(path), out.Bytes(), 0o600)
}
