package haproxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// History keeps every configuration ApplyConfig accepted: who applied
// it, when, and its text - on STATE next to haproxy.cfg, so a version
// survives reboots and updates like the configuration itself. It is
// what "what changed since yesterday?" and going back to the last good
// configuration read.
//
// One file per version (v<id>.cfg) and an index (index.json) naming
// them, oldest first. A version's file is written before the index that
// names it, each atomically: after a power cut the index names only
// complete files, and a file it doesn't name is ignored, then removed.
type History struct {
	Dir string
	// The newest versions kept, and their total size: STATE (128 MiB)
	// also holds the PKI, the events, the certificates and the files.
	// The running configuration's version is kept whatever its size.
	MaxVersions int   // 0: DefaultHistoryVersions
	MaxBytes    int64 // 0: DefaultHistoryBytes

	mu sync.Mutex
}

// The history's default bounds.
const (
	DefaultHistoryVersions = 50
	DefaultHistoryBytes    = 8 << 20
)

// ErrNoVersion is returned for a version the history doesn't hold.
var ErrNoVersion = errors.New("no such version in the history")

// Author is who applied a version: a certificate's name and roles, and
// the Controller that acted for it.
type Author struct {
	Name  string   `json:"name"`
	Roles []string `json:"roles,omitempty"`
	Via   string   `json:"via,omitempty"`
}

// Version is one configuration of the history.
type Version struct {
	ID     uint64    `json:"id"`
	Time   time.Time `json:"time,omitzero"` // zero: not known (BeforeHistory)
	Author Author    `json:"author"`
	SHA256 string    `json:"sha256"`
	Size   int       `json:"size"`
	// RevertedFrom: applied again from this version (going back).
	RevertedFrom uint64 `json:"reverted_from,omitempty"`
	// BeforeHistory: the configuration found when the history began -
	// applied before this node kept one, by someone it can't name.
	BeforeHistory bool `json:"before_history,omitempty"`
}

const historyIndex = "index.json"

func (h *History) maxVersions() int {
	if h.MaxVersions > 0 {
		return h.MaxVersions
	}
	return DefaultHistoryVersions
}

func (h *History) maxBytes() int64 {
	if h.MaxBytes > 0 {
		return h.MaxBytes
	}
	return DefaultHistoryBytes
}

func versionFile(id uint64) string { return fmt.Sprintf("v%d.cfg", id) }

func shaHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Record keeps cfg, just applied by author, as a new version - unless it
// is the newest version already (a reload of the same file). prev is the
// configuration it replaced: when the history is empty, prev is kept
// first, as the configuration found before it began. revertedFrom names
// the version cfg was taken from, 0 for none. It returns the version cfg
// is, new or not.
func (h *History) Record(prev, cfg []byte, author Author, revertedFrom uint64) (Version, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := os.MkdirAll(h.Dir, 0o700); err != nil {
		return Version{}, err
	}
	index, err := h.readIndex()
	if err != nil {
		return Version{}, err
	}
	sum := shaHex(cfg)
	if n := len(index); n > 0 && index[n-1].SHA256 == sum {
		return index[n-1], nil
	}
	now := time.Now().UTC()
	if len(index) == 0 && len(prev) > 0 && shaHex(prev) != sum {
		// When it was applied isn't known either: no time.
		v := Version{ID: 1, SHA256: shaHex(prev), Size: len(prev), BeforeHistory: true}
		if err := writeFileAtomic(filepath.Join(h.Dir, versionFile(v.ID)), prev); err != nil {
			return Version{}, err
		}
		index = append(index, v)
	}
	var id uint64 = 1
	if n := len(index); n > 0 {
		id = index[n-1].ID + 1
	}
	v := Version{ID: id, Time: now, Author: author, SHA256: sum, Size: len(cfg), RevertedFrom: revertedFrom}
	if err := writeFileAtomic(filepath.Join(h.Dir, versionFile(id)), cfg); err != nil {
		return Version{}, err
	}
	index = h.trim(append(index, v))
	if err := h.writeIndex(index); err != nil {
		return Version{}, err
	}
	h.removeUnlisted(index)
	return v, nil
}

// trim drops the oldest versions beyond the bounds, always keeping the
// newest.
func (h *History) trim(index []Version) []Version {
	if n := len(index) - h.maxVersions(); n > 0 {
		index = index[n:]
	}
	var total int64
	for i := len(index) - 1; i >= 0; i-- {
		total += int64(index[i].Size)
		if total > h.maxBytes() && i < len(index)-1 {
			return index[i+1:]
		}
	}
	return index
}

// removeUnlisted deletes the version files the index doesn't name:
// trimmed ones, and any a power cut left before its index was written.
func (h *History) removeUnlisted(index []Version) {
	keep := map[string]bool{historyIndex: true}
	for _, v := range index {
		keep[versionFile(v.ID)] = true
	}
	entries, err := os.ReadDir(h.Dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !keep[e.Name()] && !strings.HasPrefix(e.Name(), ".") {
			_ = os.Remove(filepath.Join(h.Dir, e.Name()))
		}
	}
}

// List returns the versions, newest first.
func (h *History) List() ([]Version, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	index, err := h.readIndex()
	if err != nil {
		return nil, err
	}
	slices.Reverse(index)
	return index, nil
}

// Get returns one version and its text.
func (h *History) Get(id uint64) (Version, []byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	index, err := h.readIndex()
	if err != nil {
		return Version{}, nil, err
	}
	for _, v := range index {
		if v.ID == id {
			cfg, err := os.ReadFile(filepath.Join(h.Dir, versionFile(id)))
			if err != nil {
				return Version{}, nil, err
			}
			if shaHex(cfg) != v.SHA256 {
				return Version{}, nil, fmt.Errorf("version %d's file doesn't match its SHA-256", id)
			}
			return v, cfg, nil
		}
	}
	return Version{}, nil, fmt.Errorf("%w: %d", ErrNoVersion, id)
}

func (h *History) readIndex() ([]Version, error) {
	data, err := os.ReadFile(filepath.Join(h.Dir, historyIndex))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var index []Version
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("read the configuration history's index: %w", err)
	}
	return index, nil
}

func (h *History) writeIndex(index []Version) error {
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(h.Dir, historyIndex), append(data, '\n'))
}

// writeFileAtomic writes path through a temporary file, fsynced and
// renamed, then fsyncs the directory: a power cut leaves the old file or
// the new one.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
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
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// LineChanges counts the lines b adds to a and removes from it, as a
// line diff would (Myers' shortest edit script). Past maxEdits it falls
// back to counting lines by content, order aside.
func LineChanges(a, b []byte) (added, removed int) {
	x, y := splitLines(a), splitLines(b)
	const maxEdits = 4000
	if d, ok := editDistance(x, y, maxEdits); ok {
		lcs := (len(x) + len(y) - d) / 2
		return len(y) - lcs, len(x) - lcs
	}
	count := map[string]int{}
	for _, l := range x {
		count[l]++
	}
	for _, l := range y {
		if count[l] > 0 {
			count[l]--
		} else {
			added++
		}
	}
	for _, n := range count {
		removed += n
	}
	return added, removed
}

func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	return strings.Split(string(bytes.TrimSuffix(b, []byte("\n"))), "\n")
}

// editDistance is the number of lines inserted and deleted turning x
// into y (Myers, O((N+M)D)), or false past max.
func editDistance(x, y []string, max int) (int, bool) {
	n, m := len(x), len(y)
	limit := min(n+m, max)
	off := limit + 1
	v := make([]int, 2*limit+3)
	for d := 0; d <= limit; d++ {
		for k := -d; k <= d; k += 2 {
			var i int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				i = v[off+k+1]
			} else {
				i = v[off+k-1] + 1
			}
			j := i - k
			for i < n && j < m && x[i] == y[j] {
				i++
				j++
			}
			v[off+k] = i
			if i >= n && j >= m {
				return d, true
			}
		}
	}
	return 0, false
}

// MaxVersionsOrDefault and MaxBytesOrDefault are the bounds in effect.
func (h *History) MaxVersionsOrDefault() int { return h.maxVersions() }
func (h *History) MaxBytesOrDefault() int64  { return h.maxBytes() }
