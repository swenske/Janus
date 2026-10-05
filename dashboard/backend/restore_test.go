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
	"testing/fstest"

	"github.com/swenske/Janus/dashboard/backend/internal/audit"
	"github.com/swenske/Janus/dashboard/backend/internal/auth"
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
	return &authApp{app: a, h: a.audited(a.routes(fstest.MapFS{}))}
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
