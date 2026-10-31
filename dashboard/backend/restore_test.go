package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swenske/Janus/dashboard/backend/internal/audit"
	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/dashboard/backend/internal/identity"
	"github.com/swenske/Janus/dashboard/backend/internal/secrets"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// freshApp is a new Controller: no account yet.
func freshApp(t *testing.T) *authApp {
	t.Helper()
	dir := t.TempDir()
	st, err := auth.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	tokens, _ := auth.OpenTokens(dir)
	log, _ := audit.Open(dir)
	nodes, _ := store.Open(dir)
	a := &app{auth: st, tokens: tokens, loginLimiter: auth.NewLoginLimiter(), audit: log, store: nodes, dataDir: dir, masterKeyPath: filepath.Join(t.TempDir(), "master.key")}
	return &authApp{app: a, h: a.audited(a.routes(http.NotFoundHandler()))}
}

func restoreRequest(t *testing.T, a *authApp, kit []byte, pass string, file []byte) (int, string) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	kw, _ := mw.CreateFormFile("kit", "kit.age")
	_, _ = kw.Write(kit)
	_ = mw.WriteField("passphrase", pass)
	bw, _ := mw.CreateFormFile("backup", "x.janusbackup")
	_, _ = bw.Write(file)
	_ = mw.Close()
	r := httptest.NewRequest("POST", "/api/restore", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.RemoteAddr = "192.0.2.9:1"
	rec := httptest.NewRecorder()
	a.h.ServeHTTP(rec, r)
	return rec.Code, rec.Body.String()
}

// TestRestore: a new Controller takes a backup with its kit - the data
// directory and the master key back - once; another Controller's backup,
// a wrong passphrase, a Controller set up already don't.
func TestRestore(t *testing.T) {
	prev := restartSelf
	restartSelf = func() {}
	t.Cleanup(func() { restartSelf = prev })

	src, f, kit, pass := backupApp(t)
	run := src.runBackup(context.Background())
	if run.Error != "" {
		t.Fatal(run.Error)
	}
	file := f.objects[run.Key]
	origKey, _ := os.ReadFile(src.masterKeyPath)

	dst := freshApp(t)
	if code, body := restoreRequest(t, dst, kit, "AAAA-BBBB-CCCC-DDDD-EEEE-FFFF", file); code != http.StatusBadRequest || !strings.Contains(body, "passphrase") {
		t.Errorf("a wrong passphrase: %d %s", code, body)
	}
	other, of, okit, opass := backupApp(t)
	orun := other.runBackup(context.Background())
	if code, body := restoreRequest(t, dst, kit, pass, of.objects[orun.Key]); code != http.StatusBadRequest || !strings.Contains(body, "signature") {
		t.Errorf("another Controller's backup with this kit: %d %s", code, body)
	}
	_, _ = okit, opass

	code, body := restoreRequest(t, dst, kit, pass, file)
	if code != http.StatusOK || !strings.Contains(body, src.controllerID) {
		t.Fatalf("restore: %d %s", code, body)
	}
	dst.h = dst.unlessRestored(dst.h)
	if code, _ := dst.req(t, "GET", "/api/auth/status", "", nil); code != http.StatusServiceUnavailable {
		t.Errorf("after the restore, before starting again: %d", code)
	}
	dst.restored.Store(false)
	if data, _ := os.ReadFile(filepath.Join(dst.dataDir, "users.json")); string(data) != `{"users":[]}` {
		t.Errorf("users.json: %q", data)
	}
	if key, _ := os.ReadFile(dst.masterKeyPath); !bytes.Equal(key, origKey) {
		t.Error("the master key wasn't restored")
	}
	if _, err := os.Stat(filepath.Join(dst.dataDir, "backup", "state.json")); err != nil {
		t.Errorf("the backup settings: %v", err)
	}

	// Set up: no restore from its page.
	if _, err := dst.auth.Setup("root", "root-password", ""); err != nil {
		t.Fatal(err)
	}
	if code, _ := restoreRequest(t, dst, kit, pass, file); code != http.StatusConflict {
		t.Errorf("restoring over a set-up Controller: %d", code)
	}
}

// TestRestoreFilesStaysInside: an archive naming a path out of the data
// directory writes nothing there.
func TestRestoreFilesStaysInside(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "controller/data/../../evil", Mode: 0o600, Size: 4, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("evil"))
	_ = tw.Close()
	_ = gz.Close()
	dir := t.TempDir()
	if n, err := restoreFiles(buf.Bytes(), filepath.Join(dir, "data"), filepath.Join(dir, "master.key")); err != nil || n != 0 {
		t.Errorf("a path out of the data directory: %d files, %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "evil")); err == nil {
		t.Error("written outside")
	}
}

// tarGz is an archive of files (name -> content), as a backup's.
func tarGz(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(data)
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// TestRestoreIdentity: restored on a new Controller - which made its own
// identity at its first start, sealed with its own master key -, a backup
// made before Controllers kept the identity's CA starts with a CA of its
// own; one that has it brings it back.
func TestRestoreIdentity(t *testing.T) {
	// The Controller backed up: its master key, its data directory.
	src := t.TempDir()
	srcKeyPath := filepath.Join(t.TempDir(), "master.key")
	srcKey, err := secrets.LoadOrCreate(srcKeyPath, src)
	if err != nil {
		t.Fatal(err)
	}
	srcID, err := identity.Open(src, srcKey, "")
	if err != nil {
		t.Fatal(err)
	}
	read := func(dir, name string) []byte {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	masterKey := read(filepath.Dir(srcKeyPath), "master.key")

	for _, withCA := range []bool{false, true} {
		files := map[string][]byte{
			"controller/master.key":                masterKey,
			"controller/data/" + identity.CertFile: read(src, identity.CertFile),
			"controller/data/" + identity.KeyFile:  read(src, identity.KeyFile),
			"controller/data/users.json":           []byte(`{"users":[]}`),
		}
		if withCA {
			files["controller/data/"+identity.CAFile] = read(src, identity.CAFile)
			files["controller/data/"+identity.CAKeyFile] = read(src, identity.CAKeyFile)
		}
		// The new Controller, started once.
		dst := t.TempDir()
		dstKeyPath := filepath.Join(t.TempDir(), "master.key")
		dstKey, _ := secrets.LoadOrCreate(dstKeyPath, dst)
		dstID, err := identity.Open(dst, dstKey, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := restoreFiles(tarGz(t, files), dst, dstKeyPath); err != nil {
			t.Fatal(err)
		}
		// Started again on what was restored.
		key, err := secrets.LoadOrCreate(dstKeyPath, dst)
		if err != nil {
			t.Fatal(err)
		}
		got, err := identity.Open(dst, key, "")
		if err != nil {
			t.Fatalf("with the CA %v: the restored Controller doesn't start: %v", withCA, err)
		}
		if withCA != got.CA().Equal(srcID.CA()) || got.CA().Equal(dstID.CA()) {
			t.Errorf("with the CA %v: the CA is the backed-up one: %v", withCA, got.CA().Equal(srcID.CA()))
		}
	}
}
