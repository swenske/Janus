package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/backup"
	"github.com/swenske/Janus/dashboard/backend/internal/consulctl"
	"github.com/swenske/Janus/dashboard/backend/internal/ha"
	"github.com/swenske/Janus/dashboard/backend/internal/secrets"
)

// The warm standby's witness (internal/ha, witness.go): where it lives
// for a data directory, the active's look at it, and what a standby
// being promoted reads it from - the copy it restores.

// errNoWitness: neither Consul nor an S3 bucket is set up.
var errNoWitness = errors.New("no witness: neither Consul (Settings, Consul) nor the backups' S3 bucket is set up")

// witnessOf is the witness a data directory's settings name - file reads
// one of its files by its path in it, key is its master key: the Consul
// KV when its Consul settings are on, else its backups' bucket. The
// settings choose, never which of the two answers: two witnesses would
// be two truths.
func witnessOf(file func(rel string) ([]byte, error), key ha.Sealer) (ha.Witness, error) {
	raw, err := file("controller-id")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errNoWitness
	}
	if err != nil {
		return nil, err
	}
	id := strings.TrimSpace(string(raw))
	if data, err := file("consul/state.json"); err == nil {
		set, err := consulctl.SettingsFrom(data, key)
		if err != nil {
			return nil, err
		}
		if set.Enabled && set.Address != "" && set.Token != "" {
			c, err := consulctl.New(set)
			if err != nil {
				return nil, fmt.Errorf("the Consul settings: %w", err)
			}
			return ha.ConsulWitness{KV: c, Key: ha.ConsulWitnessKey(id), Server: set.Address, NoKey: consulctl.ErrNoKey}, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	data, err := file("backup/state.json")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errNoWitness
	}
	if err != nil {
		return nil, err
	}
	client, prefix, err := backup.ClientFrom(data, key)
	if errors.Is(err, backup.ErrNoBucket) {
		return nil, errNoWitness
	}
	if err != nil {
		return nil, fmt.Errorf("the backups' bucket: %w", err)
	}
	return ha.S3Witness{Bucket: client, Key: ha.WitnessKey(prefix, id), Name: client.Bucket}, nil
}

// dirFiles reads a data directory's files.
func dirFiles(dir string) func(string) ([]byte, error) {
	return func(rel string) ([]byte, error) { return os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel))) }
}

// witnessOfCopy is the witness a copy of the active (its archive) names,
// with the master key it carries: what a standby being promoted claims.
func witnessOfCopy(archive []byte) (ha.Witness, error) {
	want := map[string]bool{"controller/master.key": true, "controller/data/controller-id": true, "controller/data/consul/state.json": true, "controller/data/backup/state.json": true}
	files := map[string][]byte{}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("the copy: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("the copy: %w", err)
		}
		if name := path.Clean(h.Name); want[name] && h.Typeflag == tar.TypeReg {
			if files[name], err = io.ReadAll(io.LimitReader(tr, 1<<20)); err != nil {
				return nil, fmt.Errorf("the copy: %w", err)
			}
		}
	}
	raw, ok := files["controller/master.key"]
	if !ok {
		return nil, errors.New("the copy has no master key")
	}
	key, err := secrets.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("the copy's master key: %w", err)
	}
	return witnessOf(func(rel string) ([]byte, error) {
		if data, ok := files["controller/data/"+rel]; ok {
			return data, nil
		}
		return nil, fs.ErrNotExist
	}, key)
}

// haWitnessView is the active's last look at the witness.
type haWitnessView struct {
	mu            sync.Mutex
	checked       time.Time
	where         string
	rec           *ha.WitnessRecord
	holder        string
	err           string
	none          bool
	unconditional bool
	logged        string // the last problem logged: once each
}

func (v *haWitnessView) set(where string, rec *ha.WitnessRecord, holder string, unconditional bool, err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.checked, v.where, v.rec, v.holder, v.unconditional = time.Now(), where, rec, holder, unconditional
	v.none, v.err = errors.Is(err, errNoWitness), ""
	if err != nil {
		v.err = err.Error()
	}
	problem := v.err
	if unconditional {
		problem = "unconditional"
	}
	if problem != v.logged {
		switch {
		case err != nil && !v.none:
			log.Printf("ha: the witness %s: %v - a promotion can't stop this Controller through it", where, err)
		case unconditional:
			log.Printf("ha: the witness %s takes no conditional write: written plainly", where)
		case v.logged != "" && !v.none:
			log.Printf("ha: the witness %s answers again", where)
		}
		v.logged = problem
	}
}

func (v *haWitnessView) view() map[string]any {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.checked.IsZero() {
		return nil
	}
	out := map[string]any{"checked": v.checked}
	switch {
	case v.none:
		out["state"] = "none"
	case v.err != "":
		out["state"], out["error"], out["where"] = "error", v.err, v.where
	default:
		out["state"], out["where"] = "ok", v.where
	}
	if v.rec != nil {
		out["epoch"], out["holder"], out["name"], out["at"] = v.rec.Epoch, v.holder, v.rec.Name, v.rec.At
	}
	if v.unconditional {
		out["unconditional"] = true
	}
	return out
}

// checkWitness is the active's look at the witness: the peer holding an
// epoch above this Controller's, or nil. A witness that's none of this
// pair's, absent or behind gets this Controller's epoch.
func (a *app) checkWitness(ctx context.Context) *ha.PeerState {
	w, err := witnessOf(dirFiles(a.dataDir), a.haKey)
	if err != nil {
		a.haWitness.set("", nil, "", false, err)
		return nil
	}
	rec, version, err := w.Read(ctx)
	if err != nil {
		a.haWitness.set(w.Where(), nil, "", false, err)
		return nil
	}
	holder := ""
	var whose error
	if rec != nil {
		holder, whose = a.ha.CheckWitness(*rec)
	}
	own := a.ha.State()
	switch ha.JudgeWitness(own, rec, holder) {
	case ha.WitnessStepDown:
		a.haWitness.set(w.Where(), rec, holder, false, nil)
		return &ha.PeerState{Role: ha.Active, Epoch: rec.Epoch, Name: rec.Name}
	case ha.WitnessWrite:
		if whose != nil {
			log.Printf("ha: the witness %s: %v - written over with this Controller's epoch %d", w.Where(), whose, own.Epoch)
		}
		signed, err := a.ha.SignWitness(own.Epoch, haName(), time.Now())
		if err != nil {
			a.haWitness.set(w.Where(), nil, "", false, err)
			return nil
		}
		unconditional, err := w.Write(ctx, signed, version)
		if err != nil {
			a.haWitness.set(w.Where(), rec, holder, false, err)
			return nil
		}
		a.haWitness.set(w.Where(), &signed, ha.HolderSelf, unconditional, nil)
	default:
		a.haWitness.set(w.Where(), rec, holder, false, nil)
	}
	return nil
}

// witnessFenced reads the witness for a Controller starting as the
// active whose peer doesn't answer: the peer it says holds an epoch above
// this one's, or nil.
func witnessFenced(ctx context.Context, s *ha.Store, w ha.Witness) (*ha.PeerState, error) {
	rec, _, err := w.Read(ctx)
	if err != nil {
		return nil, err
	}
	holder := ""
	if rec != nil {
		holder, _ = s.CheckWitness(*rec)
	}
	if ha.JudgeWitness(s.State(), rec, holder) != ha.WitnessStepDown {
		return nil, nil
	}
	return &ha.PeerState{Role: ha.Active, Epoch: rec.Epoch, Name: rec.Name}, nil
}
