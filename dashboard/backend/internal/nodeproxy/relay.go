// The Controller-delivered update: the Controller downloads a release
// bundle itself and pushes it to the node (UploadReleaseFile), then has
// the node install it from there (Upgrade) - for nodes that can't reach
// the bundle's server, when the Controller can. The node still checks
// the bundle's signature; the Controller only carries it.
package nodeproxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// relayFiles are a release bundle's files, in the order they're pushed.
var relayFiles = []string{"rootfs.squashfs", "rootfs.verity", "uki-a.efi", "uki-b.efi"}

// relayProgress is one line of POST /api/lifecycle/upgrade-relay's
// NDJSON answer.
type relayProgress struct {
	// "download" (Controller -> node, per file), then the node's own
	// Upgrade stages; "done" or "error" last.
	Stage   string `json:"stage"`
	Message string `json:"message,omitempty"`
	File    string `json:"file,omitempty"`
	Bytes   int64  `json:"bytes,omitempty"`
	Total   int64  `json:"total,omitempty"`
	Percent uint32 `json:"percent,omitempty"`
}

// relayHTTPClient fetches bundles; a var for tests.
var relayHTTPClient = http.DefaultClient

// relayRequest is POST /api/lifecycle/upgrade-relay's body - the same as
// upgrade-url's.
type relayRequest struct {
	Reference                  string `json:"reference"`
	SHA256                     string `json:"sha256"`
	WaitForHealth              bool   `json:"wait_for_health"`
	HealthTimeoutSeconds       uint32 `json:"health_timeout_seconds"`
	InsecureSkipSignatureCheck bool   `json:"insecure_skip_signature_check"`
	AllowSchematicChange       bool   `json:"allow_schematic_change"`
}

func handleUpgradeRelay(w http.ResponseWriter, r *http.Request, node *store.Node) {
	var req relayRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	base, err := url.Parse(strings.TrimRight(req.Reference, "/"))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		http.Error(w, "reference must be the bundle's http(s):// base URL", http.StatusBadRequest)
		return
	}
	conn, err := dialNode(node)
	if err != nil {
		http.Error(w, fmt.Sprintf("dial node: %v", err), http.StatusBadGateway)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), upgradeUploadTimeout)
	defer cancel()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	relayUpgrade(ctx, janusv1alpha1.NewLifecycleServiceClient(conn), base.String(), req, newProgressWriter(w))
}

// relayUpgrade pushes the bundle under base to the node, then installs
// it, reporting every step to out - an "error" or a "done" line last.
func relayUpgrade(ctx context.Context, client janusv1alpha1.LifecycleServiceClient, base string, req relayRequest, out *progressWriter) {
	fail := func(format string, args ...any) {
		out.send(relayProgress{Stage: "error", Message: fmt.Sprintf(format, args...)})
	}

	var stagingDir, squashfsSHA string
	for _, name := range relayFiles {
		dir, sum, err := pushFile(ctx, client, base+"/"+name, name, name == "rootfs.squashfs", out)
		if err != nil {
			fail("%s: %v", name, err)
			return
		}
		stagingDir = dir
		if sum != "" {
			squashfsSHA = sum
		}
	}
	if want := strings.ToLower(strings.TrimSpace(req.SHA256)); want != "" && want != squashfsSHA {
		fail("rootfs.squashfs's sha256 is %s, not %s - the bundle isn't the one expected", squashfsSHA, want)
		return
	}

	stream, err := client.Upgrade(ctx, &janusv1alpha1.UpgradeRequest{
		Source:               &janusv1alpha1.ImageSource{Reference: stagingDir, Sha256: squashfsSHA, InsecureSkipSignatureCheck: req.InsecureSkipSignatureCheck, AllowSchematicChange: req.AllowSchematicChange},
		WaitForHealth:        req.WaitForHealth,
		HealthTimeoutSeconds: req.HealthTimeoutSeconds,
	})
	if err != nil {
		fail("Upgrade: %v", err)
		return
	}
	var last *janusv1alpha1.UpgradeResponse
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			fail("Upgrade: %v", err)
			return
		}
		last = msg
		out.send(relayProgress{Stage: msg.GetStage(), Message: msg.GetMessage(), Percent: uint32(msg.GetProgress()*100 + 0.5)})
	}
	if last == nil {
		fail("Upgrade returned no progress")
		return
	}
	out.send(relayProgress{Stage: "done", Message: last.GetMessage()})
}

// pushFile downloads one bundle file and streams it to the node as it
// arrives, reporting progress; with sum, it also returns the file's
// sha256.
func pushFile(ctx context.Context, client janusv1alpha1.LifecycleServiceClient, src, name string, sum bool, out *progressWriter) (stagingDir, sha string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return "", "", err
	}
	resp, err := relayHTTPClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("download %s: %s", src, resp.Status)
	}
	total := max(resp.ContentLength, 0) // 0: not announced
	counter := &progressReader{r: resp.Body, total: total, file: name, out: out}
	var h hash.Hash
	var body io.Reader = counter
	if sum {
		h = sha256.New()
		body = io.TeeReader(counter, h)
	}
	out.send(relayProgress{Stage: "download", File: name, Total: total, Message: "the Controller downloads it and pushes it to the node"})
	dir, err := relayReleaseFile(ctx, client, name, body)
	if err != nil {
		return "", "", fmt.Errorf("push to the node: %w", err)
	}
	counter.report(true)
	if h != nil {
		sha = hex.EncodeToString(h.Sum(nil))
	}
	return dir, sha, nil
}

// progressReader counts what's read and reports it, at most every 250ms.
type progressReader struct {
	r     io.Reader
	n     int64
	total int64
	file  string
	out   *progressWriter
	last  time.Time
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.n += int64(n)
	p.report(false)
	return n, err
}

func (p *progressReader) report(final bool) {
	if !final && time.Since(p.last) < 250*time.Millisecond {
		return
	}
	p.last = time.Now()
	if final && p.total == 0 {
		p.total = p.n // known at last
	}
	p.out.send(relayProgress{Stage: "download", File: p.file, Bytes: p.n, Total: p.total})
}

// progressWriter writes NDJSON lines, flushed at once.
type progressWriter struct {
	mu sync.Mutex
	w  http.ResponseWriter
}

func newProgressWriter(w http.ResponseWriter) *progressWriter { return &progressWriter{w: w} }

func (p *progressWriter) send(v relayProgress) {
	p.mu.Lock()
	defer p.mu.Unlock()
	data, _ := json.Marshal(v)
	_, _ = p.w.Write(append(data, '\n'))
	if f, ok := p.w.(http.Flusher); ok {
		f.Flush()
	}
}
