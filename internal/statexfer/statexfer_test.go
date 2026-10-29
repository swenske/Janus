package statexfer

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// nodeState is a node's STATE as janusd leaves it: configuration,
// secrets, identity, and what isn't carried.
var nodeState = map[string]string{
	"haproxy/haproxy.cfg":              "global\n    chroot /var/empty\n",
	"haproxy/history/index.json":       "[]\n",
	"haproxy/history/v1.cfg":           "global\n",
	"haproxy/files/maps/hosts.map":     "example.test be_x\n",
	"haproxy/runtime-certs/index.json": "[]\n",
	"haproxy/runtime-certs/ab.pem":     "-----BEGIN PRIVATE KEY-----\n",
	"haproxy/acme/www.pem":             "-----BEGIN CERTIFICATE-----\n",
	"haproxy/cluster/tls-ticket-keys":  "a\nb\nc\n",
	"haproxy/cluster/member.pem":       "a member's key - never carried\n",
	"haproxy/janus-peers.cfg":          "generated\n",
	"config/letsencrypt.json":          "{}\n",
	"config/acme/account.key":          "-----BEGIN PRIVATE KEY-----\n",
	"config/console.json":              "{}\n",
	"config/consul-node-id":            "0000\n",
	"config/consul-managed.json":       "{\"agent_token\": \"the node's own\"}\n",
	"config/sysctl-observations.json":  "{}\n",
	"config/logforward/config.json":    "{}\n",
	"config/logforward/cursors.json":   "{}\n",
	"config/.console.json.tmp":         "half written\n",
	"network/config.json":              `{"hostname":"old-lb"}` + "\n",
	"controller/address":               "ctl:8443\n",
	"controller/ca.crt":                "ca\n",
	"controller/registered":            "yes\n",
	"controller/token":                 "one-time\n",
	"pki/ca.crt":                       "ca\n",
	"pki/ca.key":                       "-----BEGIN PRIVATE KEY-----\n",
	"pki/server.crt":                   "srv\n",
	"pki/server.key":                   "-----BEGIN PRIVATE KEY-----\n",
	"pki/admin.crt":                    "adm\n",
	"pki/admin.key":                    "-----BEGIN PRIVATE KEY-----\n",
	"pki/fleet/root.crt":               "root\n",
	"pki/fleet/bundle.json":            "{}\n",
	"boot/pending.json":                "{}\n",
	"events/events.jsonl":              "{}\n",
	"upgrade-incoming/rootfs.squashfs": "big\n",
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func export(t *testing.T, identity bool) (*Manifest, []byte) {
	t.Helper()
	src := t.TempDir()
	writeTree(t, src, nodeState)
	var buf bytes.Buffer
	m, err := Export(src, ExportOptions{Identity: identity, Hostname: "old-lb", Version: "v1", Schematic: "s"}, &buf)
	if err != nil {
		t.Fatal(err)
	}
	return m, buf.Bytes()
}

func paths(m *Manifest) []string {
	var out []string
	for _, f := range m.Files {
		out = append(out, f.Path)
	}
	return out
}

func TestExportCarries(t *testing.T) {
	m, _ := export(t, false)
	got := paths(m)
	for _, want := range []string{"haproxy/haproxy.cfg", "haproxy/history/v1.cfg", "haproxy/files/maps/hosts.map", "haproxy/runtime-certs/ab.pem", "haproxy/acme/www.pem", "haproxy/cluster/tls-ticket-keys", "config/letsencrypt.json", "config/acme/account.key", "config/logforward/config.json", "network/config.json", "controller/address", "pki/fleet/root.crt"} {
		if !slices.Contains(got, want) {
			t.Errorf("%s isn't carried", want)
		}
	}
	for _, never := range []string{"haproxy/cluster/member.pem", "haproxy/janus-peers.cfg", "config/sysctl-observations.json", "config/logforward/cursors.json", "config/.console.json.tmp", "controller/token", "boot/pending.json", "events/events.jsonl", "upgrade-incoming/rootfs.squashfs",
		// the identity, not asked for
		"pki/ca.key", "pki/server.key", "pki/admin.key", "controller/registered", "config/consul-node-id", "config/consul-managed.json"} {
		if slices.Contains(got, never) {
			t.Errorf("%s is carried", never)
		}
	}
	if m.Identity || m.Hostname != "old-lb" || m.Format != Format {
		t.Errorf("manifest: %+v", m)
	}
	m, _ = export(t, true)
	for _, want := range []string{"pki/ca.key", "pki/server.key", "pki/admin.key", "controller/registered", "config/consul-node-id", "config/consul-managed.json"} {
		if !slices.Contains(paths(m), want) {
			t.Errorf("with the identity, %s isn't carried", want)
		}
	}
	if slices.Contains(paths(m), "controller/token") {
		t.Error("the one-time token is carried with the identity")
	}
}

func TestExportRefusesARotation(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, map[string]string{"pki/ca.crt": "ca\n", "pki/rotation/ready": "\n"})
	if _, err := Export(src, ExportOptions{Identity: true}, &bytes.Buffer{}); err != ErrRotating {
		t.Errorf("got %v", err)
	}
	if _, err := Export(src, ExportOptions{}, &bytes.Buffer{}); err != nil {
		t.Errorf("without the identity: %v", err)
	}
}

func TestStageApply(t *testing.T) {
	_, stream := export(t, true)
	dst := t.TempDir()
	writeTree(t, dst, map[string]string{"pki/ca.crt": "the new node's\n", "network/config.json": "the new node's\n"})
	st, err := Stage(dst, ImportOptions{Identity: true}, bytes.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if string(st.HeldNetwork) != nodeState["network/config.json"] {
		t.Errorf("held network: %q", st.HeldNetwork)
	}
	if slices.Contains(st.Files, NetworkConfig) || !slices.Contains(st.Files, "pki/ca.key") {
		t.Errorf("staged: %v", st.Files)
	}
	// Nothing in place before janusd's next start.
	if b, _ := os.ReadFile(filepath.Join(dst, "pki/ca.crt")); string(b) != "the new node's\n" {
		t.Fatalf("staging changed pki/ca.crt: %q", b)
	}
	a, err := ApplyStaged(dst)
	if err != nil {
		t.Fatal(err)
	}
	if a == nil || a.Manifest == nil || a.Manifest.Hostname != "old-lb" || len(a.Files) != len(st.Files) {
		t.Fatalf("applied: %+v", a)
	}
	for _, rel := range st.Files {
		b, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(rel)))
		if err != nil || string(b) != nodeState[rel] {
			t.Errorf("%s: %q %v", rel, b, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dst, NetworkConfig)); string(b) != "the new node's\n" {
		t.Errorf("the network configuration was applied: %q", b)
	}
	if _, err := os.Stat(filepath.Join(dst, StagingDir)); !os.IsNotExist(err) {
		t.Error("the stage stayed")
	}
	// Again: nothing staged, nothing applied.
	if a, err := ApplyStaged(dst); a != nil || err != nil {
		t.Errorf("a second apply: %+v %v", a, err)
	}
}

func TestStageOptions(t *testing.T) {
	_, withID := export(t, true)
	// The identity not reused: skipped, the node keeps its own.
	dst := t.TempDir()
	st, err := Stage(dst, ImportOptions{ApplyNetwork: true}, bytes.NewReader(withID))
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(st.Files, "pki/ca.key") || !slices.Contains(st.Files, NetworkConfig) || st.HeldNetwork != nil {
		t.Errorf("staged %v, held %q", st.Files, st.HeldNetwork)
	}
	if !slices.ContainsFunc(st.Skipped, func(s string) bool { return strings.HasPrefix(s, "pki/ca.key:") }) {
		t.Errorf("skipped: %v", st.Skipped)
	}
	// The identity asked of an export without it: refused.
	_, without := export(t, false)
	if _, err := Stage(t.TempDir(), ImportOptions{Identity: true}, bytes.NewReader(without)); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Errorf("got %v", err)
	}
}

// tamper rewrites a stream entry by entry.
func tamper(t *testing.T, stream []byte, edit func(h *tar.Header, body []byte) (*tar.Header, []byte)) []byte {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(stream))
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		h, body = edit(h, body)
		if h == nil {
			continue
		}
		h.Size = int64(len(body))
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestStageRefuses(t *testing.T) {
	_, stream := export(t, false)
	for _, tc := range []struct {
		name string
		edit func(h *tar.Header, body []byte) (*tar.Header, []byte)
		want string
	}{
		{"a changed file", func(h *tar.Header, b []byte) (*tar.Header, []byte) {
			if h.Name == "haproxy/haproxy.cfg" {
				b = append([]byte(nil), b...)
				b[0] = 'G'
			}
			return h, b
		}, "isn't what the manifest says"},
		{"a file left out", func(h *tar.Header, b []byte) (*tar.Header, []byte) {
			if h.Name == "config/console.json" {
				return nil, nil
			}
			return h, b
		}, "not in the stream"},
		{"a file not in the manifest", func(h *tar.Header, b []byte) (*tar.Header, []byte) {
			if h.Name == "config/console.json" {
				h.Name = "config/other.json"
			}
			return h, b
		}, "isn't in the manifest"},
		{"a path outside what's carried", func(h *tar.Header, b []byte) (*tar.Header, []byte) {
			if h.Name == ManifestName {
				var m Manifest
				_ = json.Unmarshal(b, &m)
				m.Files = append(m.Files, File{Path: "boot/pending.json", Size: 3})
				b, _ = json.Marshal(m)
			}
			return h, b
		}, "not something a node's state carries"},
		{"a path climbing out", func(h *tar.Header, b []byte) (*tar.Header, []byte) {
			if h.Name == ManifestName {
				var m Manifest
				_ = json.Unmarshal(b, &m)
				m.Files = append(m.Files, File{Path: "config/../../etc/shadow", Size: 3})
				b, _ = json.Marshal(m)
			}
			return h, b
		}, "not a path under STATE"},
		{"another format", func(h *tar.Header, b []byte) (*tar.Header, []byte) {
			if h.Name == ManifestName {
				b = bytes.Replace(b, []byte(Format), []byte("janus-state-9"), 1)
			}
			return h, b
		}, "format"},
		{"a link", func(h *tar.Header, b []byte) (*tar.Header, []byte) {
			if h.Name == "config/console.json" {
				h.Typeflag, h.Linkname, b = tar.TypeSymlink, "/etc/shadow", nil
			}
			return h, b
		}, "not a regular file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst := t.TempDir()
			_, err := Stage(dst, ImportOptions{}, bytes.NewReader(tamper(t, stream, tc.edit)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			if _, err := os.Stat(filepath.Join(dst, StagingDir)); !os.IsNotExist(err) {
				t.Error("a refused import left a stage")
			}
		})
	}
	if _, err := Stage(t.TempDir(), ImportOptions{}, strings.NewReader("not a tar")); err == nil {
		t.Error("garbage staged")
	}
}

func TestApplyLeftover(t *testing.T) {
	dst := t.TempDir()
	// A stage without its ready marker: an import cut short.
	writeTree(t, dst, map[string]string{StagingDir + "/files/config/console.json": "half\n"})
	if _, err := ApplyStaged(dst); err == nil || !strings.Contains(err.Error(), "didn't finish") {
		t.Errorf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "config/console.json")); !os.IsNotExist(err) {
		t.Error("an unfinished import was applied")
	}
	if _, err := os.Stat(filepath.Join(dst, StagingDir)); !os.IsNotExist(err) {
		t.Error("the unfinished stage stayed")
	}
}

func TestInUse(t *testing.T) {
	dst := t.TempDir()
	writeTree(t, dst, map[string]string{"haproxy/haproxy.cfg": "boot\n", "haproxy/history/.index.json.tmp": "x", "pki/ca.crt": "ca\n"})
	if why := InUse(dst); why != "" {
		t.Errorf("a fresh node is in use: %s", why)
	}
	writeTree(t, dst, map[string]string{"haproxy/files/maps/x.map": "x"})
	if why := InUse(dst); !strings.Contains(why, "file") {
		t.Errorf("got %q", why)
	}
}
