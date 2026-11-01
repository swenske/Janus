package main

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/swenske/Janus/internal/nocloud"
	"github.com/swenske/Janus/internal/pairing"
)

// rescuePath is -rescue-file resolved: "auto" is where init leaves this
// boot's rescue volume (nocloud.RescueRunFile) on a node (-manage-host),
// nothing elsewhere.
func rescuePath(flagValue string, manageHost bool) string {
	if flagValue != "auto" {
		return flagValue
	}
	if manageHost {
		return nocloud.RescueRunFile
	}
	return ""
}

// rescueState is this boot's rescue window, kept beside the rescue file
// (a tmpfs): a janusd restarted in the same boot opens it again until
// its time is up - never once its certificate was issued.
type rescueState struct {
	Until time.Time `json:"until"`
	Used  bool      `json:"used,omitempty"`
}

func rescueStatePath(path string) string {
	return filepath.Join(filepath.Dir(path), "rescue-window.json")
}

// bootRescue is what this boot's rescue volume says (init's file at
// path) and until when its window is open: nil without a volume, or once
// the window was used or is over this boot.
func bootRescue(path string, now time.Time) (*nocloud.Rescue, time.Time) {
	if path == "" {
		return nil, time.Time{}
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, time.Time{}
	}
	if err != nil {
		log.Printf("rescue: %v - no rescue window", err)
		return nil, time.Time{}
	}
	r, err := nocloud.ParseRescue(raw)
	if err != nil {
		log.Printf("rescue: the rescue volume's %v - no rescue window", err)
		return nil, time.Time{}
	}
	var st rescueState
	if b, err := os.ReadFile(rescueStatePath(path)); err == nil && json.Unmarshal(b, &st) == nil && !st.Until.IsZero() {
		switch {
		case st.Used:
			log.Printf("rescue: this boot's rescue certificate was issued already - another needs the volume and another boot")
			return nil, time.Time{}
		case !now.Before(st.Until):
			log.Printf("rescue: this boot's rescue window ended at %s - another needs another boot", st.Until.Format(time.RFC3339))
			return nil, time.Time{}
		}
		return r, st.Until
	}
	st.Until = now.Add(pairing.RescueLifetime)
	if err := writeRescueState(path, st); err != nil {
		log.Printf("rescue: %v - no rescue window", err)
		return nil, time.Time{}
	}
	return r, st.Until
}

// markRescued records that this boot's rescue window gave its
// certificate.
func markRescued(path string) {
	var st rescueState
	if b, err := os.ReadFile(rescueStatePath(path)); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	st.Used = true
	if err := writeRescueState(path, st); err != nil {
		log.Printf("rescue: %v", err)
	}
}

func writeRescueState(path string, st rescueState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := rescueStatePath(path) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, rescueStatePath(path))
}
