package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/backup"
	"github.com/swenske/Janus/dashboard/backend/internal/secrets"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// fakeS3 is a bucket in memory, path-style: put, get, list (V2),
// delete - refused when noDelete, like write-only credentials.
type fakeS3 struct {
	mu       sync.Mutex
	objects  map[string][]byte
	noDelete bool
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/bucket"), "/")
	switch {
	case r.Method == http.MethodPut:
		data, _ := io.ReadAll(r.Body)
		f.objects[key] = data
	case r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
		type content struct {
			Key  string
			Size int64
		}
		var out struct {
			XMLName  xml.Name `xml:"ListBucketResult"`
			Contents []content
		}
		for k, v := range f.objects {
			if strings.HasPrefix(k, r.URL.Query().Get("prefix")) {
				out.Contents = append(out.Contents, content{k, int64(len(v))})
			}
		}
		sort.Slice(out.Contents, func(i, j int) bool { return out.Contents[i].Key < out.Contents[j].Key })
		_ = xml.NewEncoder(w).Encode(out)
	case r.Method == http.MethodGet:
		data, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(data)
	case r.Method == http.MethodDelete:
		if f.noDelete {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("<Error><Code>AccessDenied</Code><Message>no</Message></Error>"))
			return
		}
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	}
}

// backupApp is an app with backups to fakeS3, its kit confirmed: the kit
// and its passphrase.
func backupApp(t *testing.T) (*authApp, *fakeS3, []byte, string) {
	t.Helper()
	a := newAuthApp(t)
	a.controllerID = "0123456789abcdef0123456789abcdef"
	a.dataDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(a.dataDir, "users.json"), []byte(`{"users":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(a.dataDir, "images"), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(a.dataDir, "images", "big.qcow2.download"), []byte("an image on its way"), 0o600)
	mk, _ := secrets.LoadOrCreate(filepath.Join(t.TempDir(), "master.key"), a.dataDir)
	a.masterKeyPath = mk.Path
	st, err := backup.OpenStore(filepath.Join(a.dataDir, "backup"), mk, a.controllerID)
	if err != nil {
		t.Fatal(err)
	}
	a.backups = st
	pass, _ := st.StartKit()
	kit, _ := st.PendingKit()
	if err := st.ConfirmKit(kit, pass); err != nil {
		t.Fatal(err)
	}
	f := &fakeS3{objects: map[string][]byte{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	set := backup.DefaultSettings
	set.Enabled, set.Endpoint, set.Bucket, set.AccessKey, set.Keep = true, srv.URL, "bucket", "ak", 2
	secret := "sk"
	if err := st.SetSettings(set, &secret); err != nil {
		t.Fatal(err)
	}
	return a, f, kit, pass
}

func untar(t *testing.T, archive []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		out[h.Name] = string(data)
	}
}

// TestBackupRun: a backup in the bucket - signed, encrypted to the kit,
// the data directory and the master key in it, not the images on their
// way; the last two kept; a node it couldn't read said so.
func TestBackupRun(t *testing.T) {
	a, f, kit, pass := backupApp(t)
	if err := a.store.Add(&store.Node{Name: "edge-1", Address: "127.0.0.1:1", CACertPEM: []byte("ca"), Fleet: true}); err != nil {
		t.Fatal(err)
	}
	var runs []backup.Run
	for range 3 {
		runs = append(runs, a.runBackup(context.Background()))
		time.Sleep(5 * time.Millisecond)
	}
	last := runs[2]
	if last.Error != "" || last.Key == "" || !strings.Contains(last.Note, "edge-1") {
		t.Fatalf("run: %+v", last)
	}
	if len(f.objects) != 2 {
		t.Errorf("%d backups kept, want 2: %v", len(f.objects), runs)
	}
	opened, err := backup.OpenKit(kit, pass)
	if err != nil {
		t.Fatal(err)
	}
	id, signer, _ := opened.Keys()
	m, enc, err := backup.Open(f.objects[last.Key], signer)
	if err != nil || m.Nodes["edge-1"] == "" {
		t.Fatalf("open: %+v %v", m, err)
	}
	archive, err := backup.Decrypt(enc, id)
	if err != nil {
		t.Fatal(err)
	}
	files := untar(t, archive)
	if files["controller/data/users.json"] != `{"users":[]}` || len(files["controller/master.key"]) == 0 || files["controller/data/backup/state.json"] == "" {
		t.Errorf("the archive holds: %v", keys(files))
	}
	for name := range files {
		if strings.Contains(name, "images/") {
			t.Errorf("an image on its way: %s", name)
		}
	}

	// Credentials that may only write: older backups left to the bucket.
	f.noDelete = true
	run := a.runBackup(context.Background())
	if run.Error != "" || !strings.Contains(run.Note, "bucket's own rules") || len(f.objects) != 3 {
		t.Errorf("without the right to delete: %+v (%d objects)", run, len(f.objects))
	}
	if st := a.backups.Status(); len(st.History) != 4 || st.History[0].Key != run.Key {
		t.Errorf("history: %+v", st.History)
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
