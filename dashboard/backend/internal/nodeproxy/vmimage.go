// VM disk images for the machines the Controller creates on hypervisors
// itself: the release's own image for the default schematic (GitHub
// Releases, checked against the digest GitHub computed), or one the
// image factory builds for a schematic with extensions.
package nodeproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/swenske/Janus/internal/schematic"
)

// VMImage is where to download a disk image, and how to check it.
type VMImage struct {
	Version   string `json:"version"`
	Schematic string `json:"schematic"`
	// State is "ready" (URL, SHA256 and Size set), "building" (the
	// factory is building it - ask again later), or "failed"/
	// "unavailable" (Message says why).
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
	URL     string `json:"url,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
	Size    int64  `json:"size,omitempty"`
}

// ResolveVMImage finds the disk image file (e.g. "janus-kvm.qcow2") of
// version (empty: the newest release) built from schematic sc, asking
// the image factory to build it when it doesn't exist yet - call it
// again until it's ready.
func ResolveVMImage(ctx context.Context, version string, sc *schematic.Schematic, file string) (*VMImage, error) {
	sc = sc.Clone()
	if err := sc.Normalize(); err != nil {
		return nil, err
	}
	if version == "" {
		rel, err := getLatestRelease(ctx)
		if err != nil {
			return nil, fmt.Errorf("newest release: %w", err)
		}
		version = rel.TagName
	}
	img := &VMImage{Version: version, Schematic: sc.ID()}
	if img.Schematic == schematic.DefaultID() {
		return img, releaseAsset(ctx, img, file)
	}
	return img, factoryImage(ctx, img, sc, file)
}

func releaseAsset(ctx context.Context, img *VMImage, file string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(ReleasesURL, "/")+"/tags/"+url.PathEscape(img.Version), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("no release %s", img.Version)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub API returned %s", resp.Status)
	}
	var rel struct {
		Assets []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
			Size               int64  `json:"size"`
			Digest             string `json:"digest"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return fmt.Errorf("decode GitHub API response: %w", err)
	}
	for _, a := range rel.Assets {
		if a.Name != file {
			continue
		}
		sum, ok := strings.CutPrefix(a.Digest, "sha256:")
		if !ok || len(sum) != 64 {
			// Never an image nobody can check.
			img.State, img.Message = "unavailable", fmt.Sprintf("release %s doesn't give a SHA-256 for %s - give the image's URL and SHA-256 instead", img.Version, file)
			return nil
		}
		img.State, img.URL, img.SHA256, img.Size = "ready", a.BrowserDownloadURL, sum, a.Size
		return nil
	}
	img.State, img.Message = "unavailable", fmt.Sprintf("release %s has no %s", img.Version, file)
	return nil
}

func factoryImage(ctx context.Context, img *VMImage, sc *schematic.Schematic, file string) error {
	base := strings.TrimRight(ImageFactoryURL, "/")
	if base == "" {
		return fmt.Errorf("extensions, a HAProxy branch or a kernel track need an image factory, and none is configured (dashboardd -image-factory)")
	}
	ask := func(ctx context.Context, base string, mirror bool) error {
		return askFactoryImage(ctx, base, mirror, img, sc, file)
	}
	if !slices.Contains(bundleFiles, file) {
		// The mirror has update bundles, no disk images.
		return ask(ctx, base, false)
	}
	_, err := withMirror(ctx, base, ask)
	return err
}

// bundleFiles are an update bundle's files: what the image factory's
// mirror serves.
var bundleFiles = []string{"rootfs.squashfs", "rootfs.squashfs.sha256", "rootfs.verity", "uki-a.efi", "uki-b.efi"}

// askFactoryImage asks the image factory at base for img's file, and to
// build it when it isn't yet - not its mirror (read only), which only
// has what was built.
func askFactoryImage(ctx context.Context, base string, mirror bool, img *VMImage, sc *schematic.Schematic, file string) error {
	who := factoryName(mirror)
	if !mirror {
		var created struct {
			ID string `json:"id"`
		}
		if err := factoryCallAs(ctx, who, http.MethodPost, base+"/api/v1/schematics", sc.Canonical(), &created); err != nil {
			return err
		}
		if created.ID != img.Schematic {
			return fmt.Errorf("the image factory gives the schematic the ID %s, not %s", short(created.ID), short(img.Schematic))
		}
	}
	statusURL := base + "/api/v1/images/" + img.Schematic + "/" + url.PathEscape(img.Version) + "/amd64"
	var st struct {
		State   string `json:"state"`
		Message string `json:"message"`
		Files   []struct {
			Name   string `json:"name"`
			URL    string `json:"url"`
			Size   int64  `json:"size"`
			SHA256 string `json:"sha256"`
		} `json:"files"`
	}
	if err := factoryCallAs(ctx, who, http.MethodGet, statusURL, nil, &st); err != nil {
		return err
	}
	if st.State == "none" && !mirror {
		if err := factoryCallAs(ctx, who, http.MethodPost, statusURL, []byte("{}"), &st); err != nil {
			return err
		}
	}
	img.State, img.Message = st.State, st.Message
	if st.State != "ready" {
		return nil
	}
	for _, f := range st.Files {
		if f.Name == file {
			if len(f.SHA256) != 64 {
				img.State, img.Message = "unavailable", "the image factory gives no SHA-256 for "+file
				return nil
			}
			img.URL, img.SHA256, img.Size = f.URL, f.SHA256, f.Size
			return nil
		}
	}
	img.State, img.Message = "unavailable", "the image factory built no "+file
	return nil
}
