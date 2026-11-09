package nodeproxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"
)

// ReleaseCacheFile keeps what was read of each release's files - its
// security.json (securityByTag), its checksum and Controller image
// (assetTexts) - across a restart: a published release's files don't
// change, and a Controller starting while GitHub is slow read all of
// them again (#553: ~60 security.json, minutes). dashboardd sets it in
// its data directory; "" keeps them in memory only.
var ReleaseCacheFile string

// releaseCacheDirty: something was read since the file was written.
var releaseCacheDirty atomic.Bool

type releaseCacheDisk struct {
	Version  int                         `json:"version"`
	Security map[string][]releaseFixDisk `json:"security"`
	Assets   map[string]string           `json:"assets"`
}

type releaseFixDisk struct {
	ID        string   `json:"id"`
	Target    string   `json:"target,omitempty"`
	Extension string   `json:"extension,omitempty"`
	Severity  string   `json:"severity,omitempty"`
	Component string   `json:"component,omitempty"`
	Variant   string   `json:"variant,omitempty"`
	Audience  []string `json:"audience,omitempty"`
}

// LoadReleaseCache reads ReleaseCacheFile into the caches - none yet is
// no error. What the caches hold already stays.
func LoadReleaseCache() error {
	if ReleaseCacheFile == "" {
		return nil
	}
	data, err := os.ReadFile(ReleaseCacheFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var disk releaseCacheDisk
	if err := json.Unmarshal(data, &disk); err != nil {
		return fmt.Errorf("%s: %w", ReleaseCacheFile, err)
	}
	if disk.Version != 1 {
		return nil // another format: read again from GitHub
	}
	securityByTag.Lock()
	for tag, fixes := range disk.Security {
		if _, ok := securityByTag.m[tag]; ok {
			continue
		}
		out := make([]releaseFix, 0, len(fixes))
		for _, f := range fixes {
			out = append(out, releaseFix{id: f.ID, target: f.Target, extension: f.Extension, severity: f.Severity,
				component: f.Component, variant: f.Variant, audience: f.Audience})
		}
		securityByTag.m[tag] = out
	}
	securityByTag.Unlock()
	assetTexts.Lock()
	for u, text := range disk.Assets {
		if _, ok := assetTexts.m[u]; !ok {
			assetTexts.m[u] = text
		}
	}
	assetTexts.Unlock()
	return nil
}

// saveReleaseCache writes the caches to ReleaseCacheFile when something
// was read since - tmp, fsync, rename; a failure is said in the log (the
// caches stay in memory).
func saveReleaseCache() {
	if ReleaseCacheFile == "" || !releaseCacheDirty.Swap(false) {
		return
	}
	disk := releaseCacheDisk{Version: 1, Security: map[string][]releaseFixDisk{}, Assets: map[string]string{}}
	securityByTag.Lock()
	for tag, fixes := range securityByTag.m {
		out := make([]releaseFixDisk, 0, len(fixes))
		for _, f := range fixes {
			out = append(out, releaseFixDisk{ID: f.id, Target: f.target, Extension: f.extension, Severity: f.severity,
				Component: f.component, Variant: f.variant, Audience: f.audience})
		}
		disk.Security[tag] = out
	}
	securityByTag.Unlock()
	assetTexts.Lock()
	for u, text := range assetTexts.m {
		disk.Assets[u] = text
	}
	assetTexts.Unlock()
	if err := writeFileAtomic(ReleaseCacheFile, disk); err != nil {
		releaseCacheDirty.Store(true) // tried again next time
		log.Printf("releases: keeping what was read: %v", err)
	}
}

func writeFileAtomic(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".release-cache-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
