package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"filippo.io/age"
	"filippo.io/age/agessh"
	"golang.org/x/term"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/dashboard/backend/internal/backup"
	"github.com/swenske/Janus/dashboard/backend/internal/s3"
)

// Restoring a backup: on a new Controller - before its first account, from
// its first page -, or `dashboardd restore` on the host. The backup's
// signature is checked with the key the backup kit holds, it's decrypted
// with the kit's identity (or an admin's SSH or age key), and the data
// directory and the master key are put back; the Controller then starts
// again on them - the same Controller, its accounts, its fleet: the
// nodes keep trusting it.

// restoreFiles puts a decrypted archive's Controller back: its data
// directory into dataDir, its master key at masterKeyPath. Only what a
// backup holds, under those two places.
func restoreFiles(archive []byte, dataDir, masterKeyPath string) (int, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return 0, fmt.Errorf("the archive: %w", err)
	}
	tr := tar.NewReader(gz)
	n := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, fmt.Errorf("the archive: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		var dest string
		switch name := path.Clean(h.Name); {
		case name == "controller/master.key":
			dest = masterKeyPath
		case strings.HasPrefix(name, "controller/data/"):
			rel := strings.TrimPrefix(name, "controller/data/")
			if rel == "" || strings.HasPrefix(rel, "../") || path.IsAbs(rel) {
				return n, fmt.Errorf("the archive names %q: refused", h.Name)
			}
			dest = filepath.Join(dataDir, filepath.FromSlash(rel))
		default:
			continue // the nodes' configurations: for the operator, not restored
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxBackupFile+1))
		if err != nil {
			return n, err
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return n, err
		}
		if err := os.WriteFile(dest, data, 0o600); err != nil {
			return n, err
		}
		n++
	}
}

// openBackup checks and decrypts a backup file.
func openBackup(file []byte, signer ed25519.PublicKey, ids ...age.Identity) (*backup.Manifest, []byte, error) {
	m, enc, err := backup.Open(file, signer)
	if err != nil {
		return nil, nil, err
	}
	archive, err := backup.Decrypt(enc, ids...)
	if err != nil {
		return nil, nil, err
	}
	return m, archive, nil
}

// --- on a new Controller's first page ---

func (a *app) registerRestoreRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/restore/list", a.handleRestoreList)
	mux.HandleFunc("POST /api/restore", a.handleRestore)
}

// unlessRestored answers 503 once a backup is restored, until the
// Controller starts again on it.
func (a *app) unlessRestored(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.restored.Load() {
			w.Header().Set("Retry-After", "2")
			http.Error(w, "a backup was restored: the Controller is starting again", http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// restoreAllowed: only a Controller that has no account yet takes a
// backup from its page - the same first come as its setup.
func (a *app) restoreAllowed(w http.ResponseWriter) bool {
	if !a.auth.SetupRequired() {
		writeError(w, http.StatusConflict, "this Controller is set up: a backup is restored on a new one (or with dashboardd restore on its host)")
		return false
	}
	return true
}

type restoreS3 struct {
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
	PathStyle bool   `json:"path_style"`
}

func (r restoreS3) client() (*s3.Client, error) {
	u, err := url.Parse(r.Endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("endpoint: the S3 service's URL, https://host[:port]")
	}
	region := r.Region
	if region == "" {
		region = "us-east-1"
	}
	return &s3.Client{Endpoint: u, Region: region, Bucket: r.Bucket, AccessKey: r.AccessKey, SecretKey: r.SecretKey, PathStyle: r.PathStyle}, nil
}

// handleRestoreList lists a bucket's backups, newest first.
func (a *app) handleRestoreList(w http.ResponseWriter, r *http.Request) {
	if !a.restoreAllowed(w) {
		return
	}
	var req restoreS3
	if !decodeBody(w, r, &req) {
		return
	}
	c, err := req.client()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	objects, err := c.List(r.Context(), req.Prefix)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	out := []s3.Object{}
	for _, o := range objects {
		if strings.HasSuffix(o.Key, ".janusbackup") {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key > out[j].Key })
	writeJSON(w, http.StatusOK, out)
}

// handleRestore restores a backup - uploaded, or from a bucket - with the
// backup kit and its passphrase, then starts the Controller again on it.
func (a *app) handleRestore(w http.ResponseWriter, r *http.Request) {
	if !a.restoreAllowed(w) || a.throttledCLI(w, r) {
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "a multipart form: kit, passphrase, and backup or s3 + key")
		return
	}
	read := func(field string) ([]byte, error) {
		f, _, err := r.FormFile(field)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", field, err)
		}
		defer f.Close()
		return io.ReadAll(io.LimitReader(f, s3.MaxObject))
	}
	kitFile, err := read("kit")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	kit, err := backup.OpenKit(kitFile, r.FormValue("passphrase"))
	if err != nil {
		a.loginLimiter.Fail(clientAddr(r))
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, signer, err := kit.Keys()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var file []byte
	if raw := r.FormValue("s3"); raw != "" {
		var src restoreS3
		if err := json.Unmarshal([]byte(raw), &src); err != nil {
			writeError(w, http.StatusBadRequest, "s3: "+err.Error())
			return
		}
		c, err := src.client()
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if file, err = c.Get(r.Context(), r.FormValue("key")); err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
	} else if file, err = read("backup"); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	m, archive, err := openBackup(file, signer, id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	n, err := restoreFiles(archive, a.dataDir, a.masterKeyPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "restoring: "+err.Error()+" - the data directory may be half restored: empty it before trying again")
		return
	}
	noteAudit(r, "", "restore")
	log.Printf("restored the backup of %s (Controller %s, %s, version %s): %d files - starting again", m.Created.Format(time.RFC3339), m.Controller, m.Created.Format("2006-01-02"), m.Version, n)
	// Until it starts again, this process still holds the stores it read
	// at start: it answers nothing more.
	a.restored.Store(true)
	writeJSON(w, http.StatusOK, map[string]any{"created": m.Created, "controller": m.Controller, "version": m.Version, "files": n})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go restartSelf()
}

// restartSelf starts the Controller again on what was restored: its
// stores are read at start. A variable for the tests.
var restartSelf = func() {
	time.Sleep(time.Second)
	exe, err := os.Executable()
	if err != nil {
		log.Fatalf("restart after the restore: %v - start the Controller again", err)
	}
	if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil {
		log.Fatalf("restart after the restore: %v - start the Controller again", err)
	}
}

// --- dashboardd restore, on the host ---

// restoreCommand is `dashboardd restore [-data-dir DIR] [-master-key-file
// FILE] -kit KIT BACKUP` - the passphrase from JANUS_BACKUP_PASSPHRASE or
// asked - or, with an admin's key instead of the kit, `-identity
// ~/.ssh/id_ed25519 -signing-key BASE64` (the key the Controller's backup
// page shows). The Controller must not be running on that data directory.
func restoreCommand(args []string) {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	dataDir := fs.String("data-dir", "/data", "the Controller's data directory")
	masterKeyFile := fs.String("master-key-file", envOr("JANUS_CONTROLLER_MASTER_KEY_FILE", ""), "where the master key goes (default: <data-dir>/master.key)")
	kitFile := fs.String("kit", "", "the backup kit (its passphrase from JANUS_BACKUP_PASSPHRASE, or asked)")
	identity := fs.String("identity", "", "instead of the kit: an admin's SSH or age private key the backups are encrypted to")
	signingKey := fs.String("signing-key", "", "with -identity: the Controller's backup signing key (base64, on its backup page)")
	force := fs.Bool("force", false, "restore over a data directory that has accounts")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: dashboardd restore [-data-dir DIR] [-master-key-file FILE] (-kit KIT | -identity KEY -signing-key BASE64) BACKUP")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if fs.NArg() != 1 || (*kitFile == "") == (*identity == "") {
		fs.Usage()
		os.Exit(2)
	}
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, "restore:", err)
		os.Exit(1)
	}
	file, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		fail(err)
	}
	var ids []age.Identity
	var signer ed25519.PublicKey
	if *kitFile != "" {
		data, err := os.ReadFile(*kitFile)
		if err != nil {
			fail(err)
		}
		pass := os.Getenv("JANUS_BACKUP_PASSPHRASE")
		if pass == "" {
			fmt.Fprint(os.Stderr, "The backup kit's passphrase: ")
			p, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(os.Stderr)
			if err != nil {
				fail(errors.New("no terminal to ask the passphrase on: set JANUS_BACKUP_PASSPHRASE"))
			}
			pass = string(p)
		}
		kit, err := backup.OpenKit(data, pass)
		if err != nil {
			fail(err)
		}
		id, s, err := kit.Keys()
		if err != nil {
			fail(err)
		}
		ids, signer = append(ids, id), s
	} else {
		data, err := os.ReadFile(*identity)
		if err != nil {
			fail(err)
		}
		if strings.HasPrefix(strings.TrimSpace(string(data)), "AGE-SECRET-KEY-") {
			parsed, err := age.ParseIdentities(bytes.NewReader(data))
			if err != nil {
				fail(err)
			}
			ids = parsed
		} else {
			id, err := agessh.ParseIdentity(data)
			if err != nil {
				fail(fmt.Errorf("%s: %w (a passphrase-protected SSH key: decrypt a copy first)", *identity, err))
			}
			ids = append(ids, id)
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(*signingKey))
		if err != nil || len(raw) != ed25519.PublicKeySize {
			fail(errors.New("-signing-key: the base64 key the Controller's backup page shows"))
		}
		signer = raw
	}
	st, err := auth.Open(*dataDir)
	if err != nil {
		fail(err)
	}
	if !st.SetupRequired() && !*force {
		fail(fmt.Errorf("%s has accounts already: a backup is restored onto a new data directory (-force to restore over it)", *dataDir))
	}
	m, archive, err := openBackup(file, signer, ids...)
	if err != nil {
		fail(err)
	}
	keyPath := *masterKeyFile
	if keyPath == "" {
		keyPath = filepath.Join(*dataDir, "master.key")
	}
	n, err := restoreFiles(archive, *dataDir, keyPath)
	if err != nil {
		fail(err)
	}
	fmt.Printf("Restored the backup of %s (Controller %s, version %s): %d files in %s, the master key at %s. Start the Controller.\n", m.Created.Format(time.RFC3339), m.Controller, m.Version, n, *dataDir, keyPath)
}
