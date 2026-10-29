// The node's own update check: the newest release built for the node's
// image schematic (docs/image-factory.md). A node with the default
// schematic updates from the GitHub Releases (release.go); a node with
// extensions needs an update built from its schematic, which the image
// factory - janus.sw-servers.net, or ImageFactoryURL - builds and serves.
package nodeproxy

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
	"github.com/swenske/Janus/dashboard/updater/updaterapi"
	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/schematic"
)

// ImageFactoryURL is the image factory nodes with extensions get their
// updates from; empty disables it (those nodes then have no update
// source). Set once at startup (dashboardd's -image-factory).
var ImageFactoryURL = DefaultImageFactory

// ControllerVersion is dashboardd's, shown on the node pages - set at
// startup.
var ControllerVersion = "dev"

// updateCheck is GET /api/update-check.
type updateCheck struct {
	// The node.
	Version     string   `json:"version"`
	Arch        string   `json:"arch"`
	SchematicID string   `json:"schematic_id"`
	Extensions  []string `json:"extensions"`
	// HAProxy and Kernel: the HAProxy branch and kernel track its
	// schematic pins, "" for each release's default.
	HAProxy string `json:"haproxy,omitempty"`
	Kernel  string `json:"kernel,omitempty"`
	Default bool   `json:"default_schematic"`
	// schematic is the node's whole schematic (NodeSchematic).
	schematic *schematic.Schematic

	// Its newest update. Source is "github" or "image-factory" - Via
	// "mirror" when the factory couldn't be reached and its mirror
	// answered (mirror.go); State is "ready" (BundleURL and SHA256 set),
	// "building", "failed" or "unavailable" (Message says why).
	Source          string `json:"source"`
	Via             string `json:"via,omitempty"`
	Latest          string `json:"latest,omitempty"`
	ReleaseURL      string `json:"release_url,omitempty"`
	PublishedAt     string `json:"published_at,omitempty"`
	BundleURL       string `json:"bundle_base_url,omitempty"`
	SHA256          string `json:"sha256,omitempty"`
	State           string `json:"state"`
	Message         string `json:"message,omitempty"`
	UpdateAvailable bool   `json:"update_available"`
	// SecurityUpdate is the worst vulnerability the releases after the
	// node's fix, SecurityRelease the newest of those releases
	// (ReleaseInfo.SecurityUpdate) - whatever the source of its updates.
	SecurityUpdate  string `json:"security_update,omitempty"`
	SecurityRelease string `json:"security_release,omitempty"`
	// Support is the end of upstream support of the HAProxy branch the
	// node's schematic pins (EndOfSupport), when the image factory's
	// newest release says.
	Support *Support `json:"support,omitempty"`

	// Renamed is set when the newest release offers some of the
	// extensions under a new name (old -> new): the update is built from
	// the migrated schematic, TargetSchematicID with TargetExtensions,
	// and installing it takes allow_schematic_change.
	Renamed           map[string]string `json:"renamed,omitempty"`
	TargetSchematicID string            `json:"target_schematic_id,omitempty"`
	TargetExtensions  []string          `json:"target_extensions,omitempty"`

	// Prerelease is the newest pre-release, when newer than the node and
	// the newest release (ReleaseInfo.PrereleaseFor): for testing, shown on
	// the Update page only - never in UpdateAvailable or SecurityUpdate.
	// RunningPrerelease: the node runs a pre-release.
	Prerelease        *prereleaseOffer `json:"prerelease,omitempty"`
	RunningPrerelease bool             `json:"running_prerelease,omitempty"`
}

// prereleaseOffer is a pre-release for a node. State is "ready" (BundleURL
// and SHA256 set) for the default schematic; for another one "prepare" -
// the image factory builds it only when asked (POST /api/factory/prerelease),
// so viewing an Update page never starts a build - then "building", "ready",
// "failed" or "unavailable" (Message says why).
type prereleaseOffer struct {
	Version     string `json:"version"`
	ReleaseURL  string `json:"release_url,omitempty"`
	PublishedAt string `json:"published_at,omitempty"`
	BundleURL   string `json:"bundle_base_url,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
	State       string `json:"state"`
	Message     string `json:"message,omitempty"`
}

func registerUpdateRoutes(mux *http.ServeMux, node *store.Node) {
	mux.HandleFunc("GET /api/update-check", func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(RefreshAsked(r)) // ?refresh: "Refresh now", a release just published
		unary(w, r, node, unaryTimeout, func(ctx context.Context, c *grpc.ClientConn) (any, error) {
			v, err := janusv1alpha1.NewSystemServiceClient(c).Version(ctx, &emptypb.Empty{})
			if err != nil {
				return nil, err
			}
			return checkUpdate(ctx, v), nil
		})
	})
	mux.HandleFunc("GET /api/factory/catalog", func(w http.ResponseWriter, r *http.Request) {
		// The catalog of the version an image change builds for this
		// node (imageCatalog): its version decides.
		unary(w, r, node, unaryTimeout, func(ctx context.Context, c *grpc.ClientConn) (any, error) {
			v, err := janusv1alpha1.NewSystemServiceClient(c).Version(ctx, &emptypb.Empty{})
			if err != nil {
				return nil, err
			}
			return imageCatalog(ctx, v.GetVersion())
		})
	})
	mux.HandleFunc("POST /api/factory/update", func(w http.ResponseWriter, r *http.Request) {
		// The image to change to: these extensions, and the HAProxy branch
		// and kernel track given ("" for the release's default) - those
		// left out stay what the node's schematic picks.
		var req struct {
			Extensions []string `json:"extensions"`
			HAProxy    *string  `json:"haproxy"`
			Kernel     *string  `json:"kernel"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		if !decodeJSON(w, r, &req) {
			return
		}
		probe := &schematic.Schematic{Customization: schematic.Customization{Extensions: req.Extensions,
			HAProxy: deref(req.HAProxy), Kernel: deref(req.Kernel)}}
		if err := probe.Normalize(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		unary(w, r, node, unaryTimeout, func(ctx context.Context, c *grpc.ClientConn) (any, error) {
			v, err := janusv1alpha1.NewSystemServiceClient(c).Version(ctx, &emptypb.Empty{})
			if err != nil {
				return nil, err
			}
			// By the catalog of the version it builds, before the factory
			// is asked to build anything.
			if err := requirementsIn(imageCatalog(ctx, v.GetVersion())).CheckRequirements(probe.Customization.Extensions); err != nil {
				return nil, status.Error(codes.InvalidArgument, err.Error())
			}
			sc := NodeSchematic(v).Clone()
			sc.Customization.Extensions = probe.Customization.Extensions
			if req.HAProxy != nil {
				sc.Customization.HAProxy = *req.HAProxy
			}
			if req.Kernel != nil {
				sc.Customization.Kernel = *req.Kernel
			}
			return updateFor(ctx, v, sc), nil
		})
	})
	mux.HandleFunc("POST /api/factory/prerelease", func(w http.ResponseWriter, r *http.Request) {
		// The pre-release to prepare: the one the update check offers, no
		// other (a stale page gets a conflict, not some older build).
		var req struct {
			Version string `json:"version"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		if !decodeJSON(w, r, &req) {
			return
		}
		unary(w, r, node, unaryTimeout, func(ctx context.Context, c *grpc.ClientConn) (any, error) {
			v, err := janusv1alpha1.NewSystemServiceClient(c).Version(ctx, &emptypb.Empty{})
			if err != nil {
				return nil, err
			}
			return preparePrerelease(ctx, v, req.Version)
		})
	})
}

// preparePrerelease finds the bundle of the pre-release version built for
// the node running v - from GitHub for the default schematic, else asking
// the image factory to build it when it doesn't exist yet.
func preparePrerelease(ctx context.Context, v *janusv1alpha1.VersionResponse, version string) (*prereleaseOffer, error) {
	rel, err := getLatestRelease(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "newest release: %v", err)
	}
	pre := rel.PrereleaseFor(v.GetVersion())
	if pre == nil || pre.TagName != version {
		return nil, status.Errorf(codes.FailedPrecondition, "%s isn't the pre-release offered to this node - reload the page", version)
	}
	offer := &prereleaseOffer{Version: pre.TagName, ReleaseURL: pre.HTMLURL, PublishedAt: pre.PublishedAt}
	b, state, err := ResolveBundle(ctx, pre.TagName, NodeSchematic(v))
	switch {
	case err == nil:
		offer.State, offer.BundleURL, offer.SHA256 = "ready", b.BaseURL, b.SHA256
	case state != "":
		offer.State, offer.Message = state, err.Error()
	default:
		offer.State, offer.Message = "unavailable", err.Error()
	}
	return offer, nil
}

func checkUpdate(ctx context.Context, v *janusv1alpha1.VersionResponse) *updateCheck {
	uc := nodeUpdateCheck(v)
	resolveUpdate(ctx, uc)
	uc.Support = EndOfSupport(ctx, uc.HAProxy)
	if rel, err := getLatestRelease(ctx); err == nil {
		uc.SecurityUpdate, uc.SecurityRelease = rel.SecurityUpdate(uc.Version, "node", NodeImage{
			Extensions: uc.Extensions, HAProxy: uc.HAProxy, Kernel: uc.Kernel})
		uc.RunningPrerelease = rel.IsPrerelease(uc.Version)
		if pre := rel.PrereleaseFor(uc.Version); pre != nil {
			uc.Prerelease = &prereleaseOffer{Version: pre.TagName, ReleaseURL: pre.HTMLURL, PublishedAt: pre.PublishedAt, State: "prepare"}
			if uc.Default {
				uc.Prerelease.State, uc.Prerelease.BundleURL, uc.Prerelease.SHA256 = "ready", pre.BundleBaseURL, pre.SHA256
			}
		}
	}
	return uc
}

// nodeUpdateCheck is the node's part of an update check: its version,
// architecture and schematic.
func nodeUpdateCheck(v *janusv1alpha1.VersionResponse) *updateCheck {
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
	uc.schematic = NodeSchematic(v)
	uc.HAProxy, uc.Kernel = uc.schematic.HAProxyBranch(), uc.schematic.KernelTrack()
	return uc
}

// NodeSchematic is the schematic a node's image is built from: the one
// its image reports (VersionResponse.schematic), checked against the ID
// its signed command line names; a node whose image doesn't report it -
// built before images did - only ever had extensions.
func NodeSchematic(v *janusv1alpha1.VersionResponse) *schematic.Schematic {
	if doc := v.GetSchematic(); doc != "" {
		if sc, err := schematic.Parse([]byte(doc)); err == nil && (v.GetSchematicId() == "" || sc.ID() == v.GetSchematicId()) {
			return sc
		}
	}
	sc := &schematic.Schematic{}
	for _, e := range v.GetExtensions() {
		sc.Customization.Extensions = append(sc.Customization.Extensions, e.GetName())
	}
	_ = sc.Normalize() // names a node's extension manifests passed: valid
	return sc
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// resolveUpdate fills in the newest update built from uc's schematic for
// a node running uc.Version on uc.Arch.
func resolveUpdate(ctx context.Context, uc *updateCheck) {
	if uc.Default {
		uc.Source = "github"
		rel, err := getLatestRelease(ctx)
		if err != nil {
			uc.State, uc.Message = "unavailable", err.Error()
			return
		}
		uc.State = "ready"
		uc.Latest, uc.ReleaseURL, uc.PublishedAt = rel.TagName, rel.HTMLURL, rel.PublishedAt
		uc.BundleURL, uc.SHA256 = rel.BundleBaseURL, rel.SHA256
		uc.UpdateAvailable = updateAvailable(uc.Latest, uc.Version)
		return
	}

	uc.Source = "image-factory"
	base := strings.TrimRight(ImageFactoryURL, "/")
	if base == "" {
		uc.State, uc.Message = "unavailable", "an image with extensions gets its updates from an image factory, and none is configured (dashboardd -image-factory)"
		return
	}
	newest := ""
	if rel, err := getLatestRelease(ctx); err == nil {
		newest = rel.TagName
	}
	up, err := factoryUpdate(ctx, base, uc, newest)
	if err != nil {
		uc.State, uc.Message = "unavailable", err.Error()
		return
	}
	uc.Latest, uc.ReleaseURL, uc.Via = up.Version, up.ReleaseURL, up.Via
	uc.BundleURL, uc.SHA256 = up.BundleURL, up.SHA256
	uc.State, uc.Message = up.State, up.Message
	uc.UpdateAvailable = updateAvailable(up.Version, uc.Version)
	if len(up.Renamed) > 0 && up.Schematic != "" && up.Schematic != uc.SchematicID {
		uc.Renamed, uc.TargetSchematicID = up.Renamed, up.Schematic
		uc.TargetExtensions = []string{}
		for _, e := range uc.Extensions {
			uc.TargetExtensions = append(uc.TargetExtensions, cmp.Or(up.Renamed[e], e))
		}
		slices.Sort(uc.TargetExtensions)
		uc.UpdateAvailable = up.Version != ""
	}
}

// updateAvailable reports whether latest is an update for a node
// running running: newer by CalVer - a node that just installed a
// release the cached latest predates isn't offered the older one, nor a
// build after a release that release again. A version that isn't
// CalVer ("dev") only compares as different.
func updateAvailable(latest, running string) bool {
	if latest == "" {
		return false
	}
	if newer, known := updaterapi.Newer(latest, running); known {
		return newer
	}
	return latest != running
}

// extensionsUpdate is POST /api/factory/update: the newest update built
// for the node with another set of extensions. The embedded check's
// Version and Arch are the node's, its schematic and extensions the
// target's.
type extensionsUpdate struct {
	updateCheck
	NodeSchematicID string   `json:"node_schematic_id"`
	NodeExtensions  []string `json:"node_extensions"`
	// SchematicChange says installing it needs allow_schematic_change.
	SchematicChange bool `json:"schematic_change"`
	// TargetPrerelease: Latest is a pre-release - the node runs one, and
	// stays on that track (ReleaseInfo.PrereleaseTrack).
	TargetPrerelease bool `json:"target_prerelease,omitempty"`
}

// updateFor finds the update that would give the node running v the
// image of sc (normalized): its extensions, HAProxy branch and kernel
// track - of the newest release, or for a node running a pre-release, of
// its pre-release track (prereleaseTrack): changing a lab node's
// extensions doesn't take it back to the release. An image factory
// builds it if it doesn't exist yet - State says so.
func updateFor(ctx context.Context, v *janusv1alpha1.VersionResponse, sc *schematic.Schematic) *extensionsUpdate {
	node := nodeUpdateCheck(v)
	exts := sc.Extensions()
	if exts == nil {
		exts = []string{}
	}
	eu := &extensionsUpdate{
		updateCheck: updateCheck{
			Version:     node.Version,
			Arch:        node.Arch,
			SchematicID: sc.ID(),
			Extensions:  exts,
			HAProxy:     sc.HAProxyBranch(),
			Kernel:      sc.KernelTrack(),
			Default:     sc.ID() == schematic.DefaultID(),
			schematic:   sc,
		},
		NodeSchematicID: node.SchematicID,
		NodeExtensions:  node.Extensions,
	}
	if pre := prereleaseTrack(ctx, node.Version); pre != nil {
		eu.TargetPrerelease = true
		resolvePrerelease(ctx, &eu.updateCheck, pre)
	} else {
		resolveUpdate(ctx, &eu.updateCheck)
	}
	eu.SchematicChange = cmp.Or(eu.TargetSchematicID, eu.SchematicID) != node.SchematicID
	return eu
}

// prereleaseTrack is the pre-release an image change keeps a node running
// version on (ReleaseInfo.PrereleaseTrack); nil when it takes the release -
// or when the release list can't be read, as before.
func prereleaseTrack(ctx context.Context, version string) *ReleaseInfo {
	rel, err := getLatestRelease(ctx)
	if err != nil {
		return nil
	}
	return rel.PrereleaseTrack(version)
}

// resolvePrerelease fills in uc's update from the pre-release pre - the
// site's updates never name one (GET /api/v1/updates skips them on
// purpose): its GitHub release for the default schematic, as the update
// check's offer has it; for another, the image factory's build of that
// version, started when it doesn't exist yet (ResolveBundle, as
// preparePrerelease).
func resolvePrerelease(ctx context.Context, uc *updateCheck, pre *ReleaseInfo) {
	uc.Latest, uc.ReleaseURL, uc.PublishedAt = pre.TagName, pre.HTMLURL, pre.PublishedAt
	uc.UpdateAvailable = updateAvailable(pre.TagName, uc.Version)
	if uc.Default {
		uc.Source, uc.State = "github", "ready"
		uc.BundleURL, uc.SHA256 = pre.BundleBaseURL, pre.SHA256
		return
	}
	uc.Source = "image-factory"
	if uc.Arch != "amd64" {
		// As the site's updates say: ResolveBundle would build amd64's.
		uc.State, uc.Message = "unavailable", "updates are published for amd64 only"
		return
	}
	b, state, err := ResolveBundle(ctx, pre.TagName, uc.schematic)
	switch {
	case err == nil:
		uc.State, uc.BundleURL, uc.SHA256 = "ready", b.BaseURL, b.SHA256
	case state != "":
		uc.State, uc.Message = state, err.Error()
	default:
		uc.State, uc.Message = "unavailable", err.Error()
	}
}

// CheckRequirements refuses extensions naming one without another it
// requires (schematic.RequirementError), by the image factory's newest
// catalog: the message the factory gives for such a schematic, before
// a node or a machine is touched. Nothing is refused when the factory
// can't be asked - it refuses the build itself, with the same message.
func CheckRequirements(ctx context.Context, exts []string) error {
	return requirementsIn(factoryCatalog(ctx)).CheckRequirements(exts)
}

// requirementsIn is a catalog's extensions, to check requirements
// against: none when the factory couldn't be asked.
func requirementsIn(view *factoryCatalogView, err error) *schematic.Catalog {
	if err != nil || view == nil {
		return &schematic.Catalog{}
	}
	return &schematic.Catalog{Version: view.Version, Extensions: view.Extensions}
}

// factoryCatalogView is GET /api/factory/catalog: the extensions an image
// factory builds, from the newest release that publishes them - or from
// the version asked for (factoryCatalogFor).
type factoryCatalogView struct {
	Factory    string                   `json:"factory"`
	Version    string                   `json:"version"`
	Extensions []schematic.CatalogEntry `json:"extensions"`
	// HAProxy and Kernel are the branches and tracks the release offers
	// (one default each), Retired those it no longer does; empty for a
	// release from before images could choose them.
	HAProxy []schematic.Variant `json:"haproxy"`
	Kernel  []schematic.Variant `json:"kernel"`
	Retired []schematic.Retired `json:"retired,omitempty"`
	// Prerelease: Version is a pre-release, the one a node running a
	// pre-release changes its image to (imageCatalog).
	Prerelease bool `json:"prerelease,omitempty"`
}

// catalogCache keeps the catalogs asked for, by factory and version (""
// the newest release's).
var catalogCache struct {
	mu      sync.Mutex
	entries map[string]catalogCacheEntry
}

type catalogCacheEntry struct {
	view    *factoryCatalogView
	err     error
	expires time.Time
}

// factoryCatalog asks the image factory for the catalog of its newest
// release - never a pre-release's: what a machine or an image change is
// built from, unless a version is asked for (factoryCatalogFor).
func factoryCatalog(ctx context.Context) (*factoryCatalogView, error) {
	return factoryCatalogFor(ctx, "")
}

// factoryCatalogFor asks the image factory for the catalog of version,
// "" for its newest release's; the answer is cached like the release
// check, an error briefly (errorCacheTTL).
func factoryCatalogFor(ctx context.Context, version string) (*factoryCatalogView, error) {
	base := strings.TrimRight(ImageFactoryURL, "/")
	if base == "" {
		return nil, fmt.Errorf("no image factory is configured (dashboardd -image-factory)")
	}
	key := base + " " + version
	catalogCache.mu.Lock()
	defer catalogCache.mu.Unlock()
	if e, ok := catalogCache.entries[key]; ok && time.Now().Before(e.expires) {
		return e.view, e.err
	}
	fctx, cancel := sharedFetchContext(ctx)
	defer cancel()
	view, err := fetchFactoryCatalog(fctx, base, version)
	ttl := releaseCacheTTL
	if err != nil {
		ttl = errorCacheTTL
	}
	if catalogCache.entries == nil {
		catalogCache.entries = map[string]catalogCacheEntry{}
	}
	catalogCache.entries[key] = catalogCacheEntry{view: view, err: err, expires: time.Now().Add(ttl)}
	return view, err
}

// imageCatalog is the catalog of the version an image change builds for
// a node running version (updateFor): its pre-release track's on a node
// running a pre-release, the newest release's otherwise - what the page
// offers is what the factory builds.
func imageCatalog(ctx context.Context, version string) (*factoryCatalogView, error) {
	pre := prereleaseTrack(ctx, version)
	if pre == nil {
		return factoryCatalog(ctx)
	}
	view, err := factoryCatalogFor(ctx, pre.TagName)
	if err != nil {
		return nil, err
	}
	out := *view // the cached one stays as it is
	out.Prerelease = true
	return &out, nil
}

// fetchFactoryCatalog fetches the catalog of version - "": of the newest
// release that publishes one, pre-releases left out.
func fetchFactoryCatalog(ctx context.Context, base, version string) (*factoryCatalogView, error) {
	if version == "" {
		var versions []struct {
			Version    string `json:"version"`
			Schematics bool   `json:"schematics"`
			Prerelease bool   `json:"prerelease"`
		}
		if err := factoryCall(ctx, http.MethodGet, base+"/api/v1/versions", nil, &versions); err != nil {
			return nil, err
		}
		for _, v := range versions { // newest first
			if v.Schematics && !v.Prerelease {
				version = v.Version
				break
			}
		}
		if version == "" {
			return nil, fmt.Errorf("image factory: no release offers extensions")
		}
	}
	var c schematic.Catalog
	if err := factoryCall(ctx, http.MethodGet, base+"/api/v1/versions/"+url.PathEscape(version)+"/extensions", nil, &c); err != nil {
		return nil, err
	}
	if c.Extensions == nil {
		c.Extensions = []schematic.CatalogEntry{}
	}
	view := &factoryCatalogView{Factory: base, Version: version, Extensions: c.Extensions,
		HAProxy: c.HAProxy, Kernel: c.Kernel, Retired: c.Retired}
	if view.HAProxy == nil {
		view.HAProxy = []schematic.Variant{}
	}
	if view.Kernel == nil {
		view.Kernel = []schematic.Variant{}
	}
	return view, nil
}

// factoryUpdate is the image factory's answer, as site/backend's
// GET /api/v1/updates/{id} gives it.
type factoryUpdateResponse struct {
	Schematic  string `json:"schematic"`
	Version    string `json:"version"`
	ReleaseURL string `json:"release_url"`
	BundleURL  string `json:"bundle_url"`
	SHA256     string `json:"sha256"`
	State      string `json:"state"`
	Message    string `json:"message"`
	// Renamed: the update is built from another schematic, Schematic,
	// with these extensions renamed (old -> new).
	Renamed map[string]string `json:"renamed"`
	// Via is "mirror" when the factory's mirror answered.
	Via string `json:"-"`
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
//
// newest, the newest release the Controller knows (getLatestRelease), is
// part of the cache key: a release it learns of asks the factory again at
// once, which starts that release's build. Without it, a ready answer
// kept offering the previous release for up to releaseCacheTTL while
// SecurityUpdate, read from the new list, already named the new release's
// fixes - "security update → v2026.10.27-3" on a node running it, right
// after v2026.10.28-2's promotion.
func factoryUpdate(ctx context.Context, base string, uc *updateCheck, newest string) (*factoryUpdateResponse, error) {
	key := uc.SchematicID + " " + uc.Arch + " " + uc.Version + " " + newest
	factoryCache.mu.Lock()
	defer factoryCache.mu.Unlock()
	if e, ok := factoryCache.entries[key]; ok && time.Now().Before(e.expires) {
		return e.up, e.err
	}
	fctx, cancel := sharedFetchContext(ctx)
	defer cancel()
	up, err := fetchFactoryUpdate(fctx, base, uc)
	ttl := time.Minute // not ready yet, or another answer
	switch {
	case err != nil:
		ttl = errorCacheTTL
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
	sc := uc.schematic
	if sc == nil || sc.ID() != uc.SchematicID {
		return nil, fmt.Errorf("the node's extensions (%s) don't make up its schematic %s, and its image doesn't say what it is", strings.Join(uc.Extensions, ", "), short(uc.SchematicID))
	}
	var up factoryUpdateResponse
	via, err := withMirror(ctx, base, func(ctx context.Context, base string, mirror bool) error {
		up = factoryUpdateResponse{}
		return askFactoryUpdate(ctx, base, mirror, uc, &up)
	})
	if err != nil {
		return nil, err
	}
	up.Via = via
	return &up, nil
}

// askFactoryUpdate asks the image factory at base for uc's update - its
// mirror (read only) gets no schematic to register.
func askFactoryUpdate(ctx context.Context, base string, mirror bool, uc *updateCheck, up *factoryUpdateResponse) error {
	who := factoryName(mirror)
	if !mirror {
		var created struct {
			ID string `json:"id"`
		}
		if err := factoryCallAs(ctx, who, http.MethodPost, base+"/api/v1/schematics", uc.schematic.Canonical(), &created); err != nil {
			return err
		}
		if created.ID != uc.SchematicID {
			return fmt.Errorf("the image factory gives the node's schematic the ID %s, not %s", short(created.ID), short(uc.SchematicID))
		}
	}
	q := url.Values{"arch": {uc.Arch}, "from": {uc.Version}}
	return factoryCallAs(ctx, who, http.MethodGet, base+"/api/v1/updates/"+uc.SchematicID+"?"+q.Encode(), nil, up)
}

func factoryName(mirror bool) string {
	if mirror {
		return "the image factory's mirror"
	}
	return "image factory"
}

func factoryCall(ctx context.Context, method, u string, body []byte, out any) error {
	return factoryCallAs(ctx, "image factory", method, u, body, out)
}

// factoryCallAs calls the image factory (or its mirror: who, in errors).
// An error that isn't the factory's answer - no connection, a timeout, a
// 5xx - is an *unreachableError.
func factoryCallAs(ctx context.Context, who, method, u string, body []byte, out any) error {
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
		return &unreachableError{fmt.Errorf("%s: %w", who, err)}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return &unreachableError{fmt.Errorf("%s: %w", who, err)}
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			err = fmt.Errorf("%s: %s", who, e.Error)
		} else {
			err = fmt.Errorf("%s: %s", who, resp.Status)
		}
		if resp.StatusCode >= 500 {
			return &unreachableError{err}
		}
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s: %w", who, err)
	}
	return nil
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// Support is how long the HAProxy branch an image pins stays offered.
type Support struct {
	Component string `json:"component"` // "haproxy"
	Variant   string `json:"variant"`   // "3.0"
	// EOL is the end of upstream support (YYYY-MM-DD); Soon: within six
	// months.
	EOL  string `json:"eol,omitempty"`
	Soon bool   `json:"soon,omitempty"`
	// Retired: the newest release no longer offers it - LastRelease is
	// the last one that does, and the image gets no newer update.
	Retired     bool   `json:"retired,omitempty"`
	LastRelease string `json:"last_release,omitempty"`
}

// EndOfSupport says when the HAProxy branch a node's schematic pins
// leaves the image factory's offer: nil for a node that takes the
// default (it moves to the next branch with the releases), or when the
// factory's newest release doesn't say.
func EndOfSupport(ctx context.Context, branch string) *Support {
	if branch == "" {
		return nil
	}
	c, err := factoryCatalog(ctx)
	if err != nil || len(c.HAProxy) == 0 {
		return nil
	}
	s := &Support{Component: schematic.ComponentHAProxy, Variant: branch}
	for _, r := range c.Retired {
		if r.Component == schematic.ComponentHAProxy && r.Name == branch {
			s.Retired, s.LastRelease = true, r.LastRelease
			return s
		}
	}
	for _, v := range c.HAProxy {
		if v.Name != branch {
			continue
		}
		s.EOL = v.EOL
		if t, err := time.Parse(time.DateOnly, v.EOL); err == nil {
			s.Soon = time.Until(t) < 183*24*time.Hour
		}
		return s
	}
	return nil
}
