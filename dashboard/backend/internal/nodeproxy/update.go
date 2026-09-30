// The node's own update check: the newest release built for the node's
// image schematic (docs/image-factory.md). A node with the default
// schematic updates from the GitHub Releases (release.go); a node with
// extensions needs an update built from its schematic, which the image
// factory - janus.sw-servers.net, or ImageFactoryURL - builds and serves.
package nodeproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
	"github.com/swenske/Janus/internal/schematic"
)

// ImageFactoryURL is the image factory nodes with extensions get their
// updates from; empty disables it (those nodes then have no update
// source). Set once at startup (dashboardd's -image-factory).
var ImageFactoryURL = "https://janus.sw-servers.net"

// updateCheck is GET /api/update-check.
type updateCheck struct {
	// The node.
	Version     string   `json:"version"`
	Arch        string   `json:"arch"`
	SchematicID string   `json:"schematic_id"`
	Extensions  []string `json:"extensions"`
	Default     bool     `json:"default_schematic"`

	// Its newest update. Source is "github" or "image-factory"; State is
	// "ready" (BundleURL and SHA256 set), "building", "failed" or
	// "unavailable" (Message says why).
	Source          string `json:"source"`
	Latest          string `json:"latest,omitempty"`
	ReleaseURL      string `json:"release_url,omitempty"`
	PublishedAt     string `json:"published_at,omitempty"`
	BundleURL       string `json:"bundle_base_url,omitempty"`
	SHA256          string `json:"sha256,omitempty"`
	State           string `json:"state"`
	Message         string `json:"message,omitempty"`
	UpdateAvailable bool   `json:"update_available"`
}

func registerUpdateRoutes(mux *http.ServeMux, node *store.Node) {
	mux.HandleFunc("GET /api/update-check", func(w http.ResponseWriter, r *http.Request) {
		unary(w, r, node, unaryTimeout, func(ctx context.Context, c *grpc.ClientConn) (any, error) {
			v, err := janusv1alpha1.NewSystemServiceClient(c).Version(ctx, &emptypb.Empty{})
			if err != nil {
				return nil, err
			}
			return checkUpdate(ctx, v), nil
		})
	})
}

func checkUpdate(ctx context.Context, v *janusv1alpha1.VersionResponse) *updateCheck {
	uc := &updateCheck{Version: v.GetVersion(), Arch: v.GetArch(), SchematicID: v.GetSchematicId(), Extensions: []string{}}
	for _, e := range v.GetExtensions() {
		uc.Extensions = append(uc.Extensions, e.GetName())
	}
	if uc.Arch == "" {
		uc.Arch = "amd64" // a node older than VersionResponse.arch: only amd64 had updates
	}
	if uc.SchematicID == "" {
		uc.SchematicID = schematic.DefaultID() // older than schematics
	}
	uc.Default = uc.SchematicID == schematic.DefaultID()

	if uc.Default {
		uc.Source = "github"
		rel, err := getLatestRelease(ctx)
		if err != nil {
			uc.State, uc.Message = "unavailable", err.Error()
			return uc
		}
		uc.State = "ready"
		uc.Latest, uc.ReleaseURL, uc.PublishedAt = rel.TagName, rel.HTMLURL, rel.PublishedAt
		uc.BundleURL, uc.SHA256 = rel.BundleBaseURL, rel.SHA256
		uc.UpdateAvailable = uc.Latest != uc.Version
		return uc
	}

	uc.Source = "image-factory"
	base := strings.TrimRight(ImageFactoryURL, "/")
	if base == "" {
		uc.State, uc.Message = "unavailable", "this node's image has extensions, and no image factory is configured to build its updates (dashboardd -image-factory)"
		return uc
	}
	up, err := factoryUpdate(ctx, base, uc)
	if err != nil {
		uc.State, uc.Message = "unavailable", err.Error()
		return uc
	}
	uc.Latest, uc.ReleaseURL = up.Version, up.ReleaseURL
	uc.BundleURL, uc.SHA256 = up.BundleURL, up.SHA256
	uc.State, uc.Message = up.State, up.Message
	uc.UpdateAvailable = up.Version != "" && up.Version != uc.Version
	return uc
}

// factoryUpdate is the image factory's answer, as site/backend's
// GET /api/v1/updates/{id} gives it.
type factoryUpdateResponse struct {
	Version    string `json:"version"`
	ReleaseURL string `json:"release_url"`
	BundleURL  string `json:"bundle_url"`
	SHA256     string `json:"sha256"`
	State      string `json:"state"`
	Message    string `json:"message"`
}

var factoryCache struct {
	mu      sync.Mutex
	entries map[string]factoryCacheEntry
}

type factoryCacheEntry struct {
	up      *factoryUpdateResponse
	err     error
	expires time.Time
}

// factoryUpdate registers the node's schematic with the factory (it's
// content-addressed: registering it again changes nothing, and it's how a
// factory learns a schematic whose image was built elsewhere) and asks
// for its newest update. The factory starts building one that doesn't
// exist yet; its answer is cached briefly, less briefly once ready.
func factoryUpdate(ctx context.Context, base string, uc *updateCheck) (*factoryUpdateResponse, error) {
	key := uc.SchematicID + " " + uc.Arch + " " + uc.Version
	factoryCache.mu.Lock()
	defer factoryCache.mu.Unlock()
	if e, ok := factoryCache.entries[key]; ok && time.Now().Before(e.expires) {
		return e.up, e.err
	}
	up, err := fetchFactoryUpdate(ctx, base, uc)
	ttl := time.Minute
	switch {
	case err == nil && up.State == "ready":
		ttl = releaseCacheTTL
	case err == nil && up.State == "building":
		ttl = 20 * time.Second
	}
	if factoryCache.entries == nil {
		factoryCache.entries = map[string]factoryCacheEntry{}
	}
	factoryCache.entries[key] = factoryCacheEntry{up: up, err: err, expires: time.Now().Add(ttl)}
	return up, err
}

func fetchFactoryUpdate(ctx context.Context, base string, uc *updateCheck) (*factoryUpdateResponse, error) {
	sc := &schematic.Schematic{Customization: schematic.Customization{Extensions: append([]string(nil), uc.Extensions...)}}
	if err := sc.Normalize(); err != nil {
		return nil, fmt.Errorf("the node's extensions: %w", err)
	}
	if sc.ID() != uc.SchematicID {
		return nil, fmt.Errorf("the node's extensions (%s) don't make up its schematic %s", strings.Join(uc.Extensions, ", "), short(uc.SchematicID))
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := factoryCall(ctx, http.MethodPost, base+"/api/v1/schematics", sc.Canonical(), &created); err != nil {
		return nil, err
	}
	if created.ID != uc.SchematicID {
		return nil, fmt.Errorf("the image factory gives the node's schematic the ID %s, not %s", short(created.ID), short(uc.SchematicID))
	}
	q := url.Values{"arch": {uc.Arch}, "from": {uc.Version}}
	var up factoryUpdateResponse
	if err := factoryCall(ctx, http.MethodGet, base+"/api/v1/updates/"+uc.SchematicID+"?"+q.Encode(), nil, &up); err != nil {
		return nil, err
	}
	return &up, nil
}

func factoryCall(ctx context.Context, method, u string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("image factory: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return fmt.Errorf("image factory: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return fmt.Errorf("image factory: %s", e.Error)
		}
		return fmt.Errorf("image factory: %s", resp.Status)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("image factory: %w", err)
	}
	return nil
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
