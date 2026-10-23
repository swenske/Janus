package main

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/swenske/Janus/internal/exporter"
	"github.com/swenske/Janus/internal/nodeexporter"
)

// listenDefaultMarker, in the config directory, says the exporters'
// default address was settled on this node.
const listenDefaultMarker = "listen-default"

// settleListenDefault keeps a node set up while the exporters listened
// on every address by default as it was: once, at the first boot of a
// janusd whose default is the main interface, an exporter left to its
// default gets "every address" saved. initialized says the node booted
// before (its PKI was bootstrapped); a node new since writes the marker
// at its first boot, before its PKI - never taken for an old one. A
// save that fails leaves the marker out: the next boot tries again.
func settleListenDefault(dir string, initialized, hasNodeExporter bool) error {
	marker := filepath.Join(dir, listenDefaultMarker)
	if _, err := os.Stat(marker); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if initialized {
		var kept []string
		if _, isDefault, err := exporter.Load(); err == nil && isDefault {
			if err := exporter.Save(exporter.Config{Enabled: true, Port: exporter.DefaultPort}); err != nil {
				return err
			}
			kept = append(kept, "the Janus exporter")
		}
		if hasNodeExporter {
			if cfg, isDefault, err := nodeexporter.Load(); err == nil && isDefault {
				cfg.Address = ""
				if err := nodeexporter.Save(cfg); err != nil {
					return err
				}
				kept = append(kept, "node_exporter")
			}
		}
		if len(kept) > 0 {
			log.Printf("exporters: set up before they listened on the main interface by default - %s kept on every address", strings.Join(kept, " and "))
		}
	}
	return writeDurably(marker, []byte(exporter.Auto+"\n"))
}

// writeDurably writes path so that a power cut leaves it whole or
// absent: a temporary file synced, renamed, the directory synced.
func writeDurably(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+filepath.Base(path)+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
