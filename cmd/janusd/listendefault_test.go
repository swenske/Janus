package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/swenske/Janus/internal/exporter"
	"github.com/swenske/Janus/internal/nodeexporter"
)

func TestSettleListenDefault(t *testing.T) {
	fresh := func(t *testing.T) string {
		dir := t.TempDir()
		exporter.Dir, nodeexporter.Dir = dir, dir
		return dir
	}
	marker := func(dir string) bool {
		_, err := os.Stat(filepath.Join(dir, listenDefaultMarker))
		return err == nil
	}

	// A node set up before: both exporters kept on every address, once.
	dir := fresh(t)
	if err := settleListenDefault(dir, true, true); err != nil {
		t.Fatal(err)
	}
	if cfg, isDefault, err := exporter.Load(); err != nil || isDefault || cfg.Address != "" || !cfg.Enabled || cfg.Port != exporter.DefaultPort {
		t.Errorf("exporter: %+v %v %v", cfg, isDefault, err)
	}
	if cfg, isDefault, err := nodeexporter.Load(); err != nil || isDefault || cfg.Address != "" || !cfg.Enabled {
		t.Errorf("node_exporter: %+v %v %v", cfg, isDefault, err)
	}
	if !marker(dir) {
		t.Fatal("no marker")
	}
	_ = os.Remove(filepath.Join(dir, "metrics.json"))
	if err := settleListenDefault(dir, true, true); err != nil {
		t.Fatal(err)
	}
	if _, isDefault, _ := exporter.Load(); !isDefault {
		t.Error("settled twice")
	}

	// A new node: the marker only - its exporters on the main interface.
	dir = fresh(t)
	if err := settleListenDefault(dir, false, true); err != nil || !marker(dir) {
		t.Fatalf("new node: %v, marker %v", err, marker(dir))
	}
	if cfg, isDefault, _ := exporter.Load(); !isDefault || cfg.Address != exporter.Auto {
		t.Errorf("new node's exporter: %+v", cfg)
	}

	// Settings of its own, or unreadable ones, are left alone.
	dir = fresh(t)
	if err := exporter.Save(exporter.Config{Enabled: true, Port: 10056, Address: "10.0.0.5"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node-exporter.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := settleListenDefault(dir, true, true); err != nil {
		t.Fatal(err)
	}
	if cfg, _, _ := exporter.Load(); cfg.Address != "10.0.0.5" {
		t.Errorf("an address of its own: %+v", cfg)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "node-exporter.json")); string(data) != "{not json" {
		t.Errorf("an unreadable file rewritten: %q", data)
	}
	// Without the extension, node_exporter's settings aren't written.
	dir = fresh(t)
	if err := settleListenDefault(dir, true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "node-exporter.json")); err == nil {
		t.Error("node_exporter's settings written without the extension")
	}
}
