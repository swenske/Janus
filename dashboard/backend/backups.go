package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/dashboard/backend/internal/backup"
	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	"github.com/swenske/Janus/dashboard/backend/internal/s3"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// Backups (internal/backup): everything the Controller keeps - its data
// directory and its master key - and its nodes' configurations, read
// through their API (never their private keys: the API refuses them),
// encrypted to the backup kit and admins' keys, signed, to an S3 bucket
// on a schedule, or downloaded.

// maxBackupFile is the largest file a backup takes from the data
// directory: what's larger is left out (and said so).
const maxBackupFile = 64 << 20

// backupSkipped are what the data directory holds that a backup leaves
// out: images downloaded on their way to a hypervisor, temporary files.
func backupSkipped(rel string, d fs.DirEntry) bool {
	base := d.Name()
	return rel == "images" || strings.HasPrefix(base, "cidata-") || strings.HasSuffix(base, ".tmp") || strings.HasSuffix(base, ".download")
}

type archiveWriter struct {
	tw *tar.Writer
}

func (w archiveWriter) add(name string, data []byte, mtime time.Time) error {
	if err := w.tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: mtime, Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err := w.tw.Write(data)
	return err
}

// buildArchive is a backup's content: controller/data/... (the data
// directory), controller/master.key, nodes/<name>/... (their
// configurations).
func (a *app) buildArchive(ctx context.Context) ([]byte, backup.Manifest, error) {
	m := backup.Manifest{Version: version, Created: time.Now().UTC(), Nodes: map[string]string{}}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	w := archiveWriter{tar.NewWriter(gz)}
	err := filepath.WalkDir(a.dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(a.dataDir, path)
		if rel == "." {
			return nil
		}
		if backupSkipped(rel, d) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > maxBackupFile {
			m.Skipped = append(m.Skipped, rel)
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return w.add("controller/data/"+filepath.ToSlash(rel), data, info.ModTime())
	})
	if err != nil {
		return nil, m, fmt.Errorf("the data directory: %w", err)
	}
	if a.masterKeyPath != "" {
		key, err := os.ReadFile(a.masterKeyPath)
		if err != nil {
			return nil, m, fmt.Errorf("the master key: %w", err)
		}
		if err := w.add("controller/master.key", key, time.Now()); err != nil {
			return nil, m, err
		}
	}
	for _, n := range a.store.List() {
		if err := a.archiveNode(ctx, w, n); err != nil {
			m.Nodes[n.Name] = err.Error()
		} else {
			m.Nodes[n.Name] = ""
		}
	}
	if err := w.tw.Close(); err != nil {
		return nil, m, err
	}
	if err := gz.Close(); err != nil {
		return nil, m, err
	}
	return buf.Bytes(), m, nil
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// archiveNode adds node's configurations, each as its API answers it
// (JSON), with haproxy.cfg, the maps and HAProxy's files as they are. A
// module the node's image hasn't is left out.
func (a *app) archiveNode(ctx context.Context, w archiveWriter, n *store.Node) error {
	conn, err := nodeproxy.Conn(n)
	if err != nil {
		return err
	}
	dir := "nodes/" + unsafeName.ReplaceAllString(n.Name, "_") + "-" + n.ID + "/"
	now := time.Now()
	var failures []string
	call := func(name string, fn func(context.Context, *grpc.ClientConn) (proto.Message, error)) proto.Message {
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		msg, err := fn(cctx, conn)
		if status.Code(err) == codes.FailedPrecondition || status.Code(err) == codes.Unimplemented {
			return nil // not in this node's image
		}
		if err != nil {
			failures = append(failures, name+": "+status.Convert(err).Message())
			return nil
		}
		data, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(msg)
		if err == nil {
			err = w.add(dir+name+".json", data, now)
		}
		if err != nil {
			failures = append(failures, name+": "+err.Error())
		}
		return msg
	}
	hap := func(c *grpc.ClientConn) janusv1alpha1.HAProxyServiceClient {
		return janusv1alpha1.NewHAProxyServiceClient(c)
	}
	net := func(c *grpc.ClientConn) janusv1alpha1.NetworkServiceClient {
		return janusv1alpha1.NewNetworkServiceClient(c)
	}
	sys := func(c *grpc.ClientConn) janusv1alpha1.SystemServiceClient {
		return janusv1alpha1.NewSystemServiceClient(c)
	}
	empty := &emptypb.Empty{}

	if cfg, ok := call("haproxy-config", func(c context.Context, cc *grpc.ClientConn) (proto.Message, error) {
		return hap(cc).GetConfig(c, empty)
	}).(*janusv1alpha1.GetConfigResponse); ok {
		_ = w.add(dir+"haproxy.cfg", cfg.GetConfig(), now)
	}
	if maps, ok := call("haproxy-maps", func(c context.Context, cc *grpc.ClientConn) (proto.Message, error) {
		return hap(cc).MapList(c, empty)
	}).(*janusv1alpha1.MapListResponse); ok {
		for _, name := range maps.GetMaps() {
			call("haproxy-map-"+unsafeName.ReplaceAllString(strings.Trim(name, "/"), "_"), func(c context.Context, cc *grpc.ClientConn) (proto.Message, error) {
				return hap(cc).MapGet(c, &janusv1alpha1.MapGetRequest{Map: name})
			})
		}
	}
	if files, ok := call("haproxy-files", func(c context.Context, cc *grpc.ClientConn) (proto.Message, error) {
		return hap(cc).FileList(c, empty)
	}).(*janusv1alpha1.FileListResponse); ok {
		for _, f := range files.GetFiles() {
			if f.GetSecret() {
				continue // a private key: never read back
			}
			cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			got, err := hap(conn).FileGet(cctx, &janusv1alpha1.FileGetRequest{Name: f.GetName()})
			cancel()
			if err != nil {
				failures = append(failures, "haproxy file "+f.GetName()+": "+status.Convert(err).Message())
				continue
			}
			_ = w.add(dir+"haproxy-files/"+f.GetName(), got.GetContent(), now)
		}
	}
	call("letsencrypt", func(c context.Context, cc *grpc.ClientConn) (proto.Message, error) {
		return hap(cc).ACMEGetConfig(c, empty)
	})
	call("network", func(c context.Context, cc *grpc.ClientConn) (proto.Message, error) {
		return net(cc).NetworkConfigGet(c, empty)
	})
	call("firewall", func(c context.Context, cc *grpc.ClientConn) (proto.Message, error) {
		return net(cc).FirewallGetRuleset(c, empty)
	})
	call("vrrp", func(c context.Context, cc *grpc.ClientConn) (proto.Message, error) {
		return net(cc).VRRPGetConfig(c, empty)
	})
	call("bgp", func(c context.Context, cc *grpc.ClientConn) (proto.Message, error) {
		return net(cc).BGPGetConfig(c, empty)
	})
	call("consul", func(c context.Context, cc *grpc.ClientConn) (proto.Message, error) {
		return net(cc).ConsulGetConfig(c, empty)
	})
	call("metrics", func(c context.Context, cc *grpc.ClientConn) (proto.Message, error) {
		return sys(cc).MetricsConfigGet(c, empty)
	})
	call("node-exporter", func(c context.Context, cc *grpc.ClientConn) (proto.Message, error) {
		return sys(cc).NodeExporterConfigGet(c, empty)
	})
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

// backupName is a backup's object name: sortable by time.
func (a *app) backupName(t time.Time) string {
	return "janus-controller-" + t.UTC().Format("20060102T150405.000Z") + "-" + a.controllerID[:8] + ".janusbackup"
}

// makeBackup is a backup file, sealed.
func (a *app) makeBackup(ctx context.Context) ([]byte, backup.Manifest, error) {
	archive, m, err := a.buildArchive(ctx)
	if err != nil {
		return nil, m, err
	}
	file, err := a.backups.Seal(archive, m)
	return file, m, err
}

// runBackup makes a backup and puts it in the bucket, then keeps the last
// Keep (deleting needs the right to; else the bucket's lifecycle rules
// keep them).
func (a *app) runBackup(ctx context.Context) backup.Run {
	start := time.Now()
	run := backup.Run{Time: start.UTC()}
	defer func() {
		run.Seconds = time.Since(start).Seconds()
		a.backups.Record(run)
		if run.Error != "" {
			log.Printf("backup: %s", run.Error)
		} else {
			log.Printf("backup: %s (%d bytes)%s", run.Key, run.Size, map[bool]string{true: " - " + run.Note, false: ""}[run.Note != ""])
		}
	}()
	client, prefix, err := a.backups.Client()
	if err != nil {
		run.Error = err.Error()
		return run
	}
	file, m, err := a.makeBackup(ctx)
	if err != nil {
		run.Error = err.Error()
		return run
	}
	key := prefix + a.backupName(start)
	if err := client.Put(ctx, key, file, "application/octet-stream"); err != nil {
		run.Error = "put " + key + ": " + err.Error()
		return run
	}
	run.Key, run.Size, run.Manifest = key, int64(len(file)), &m
	var failed []string
	for name, why := range m.Nodes {
		if why != "" {
			failed = append(failed, name)
		}
	}
	if len(failed) > 0 {
		sort.Strings(failed)
		run.Note = "without all of " + strings.Join(failed, ", ") + "'s configuration"
	}
	if keep := a.backups.Keep(); keep > 0 {
		deleted, err := a.pruneBackups(ctx, client, prefix, keep)
		run.Deleted = deleted
		switch {
		case s3.IsAccessDenied(err):
			run.Note = strings.TrimPrefix(run.Note+"; older backups left to the bucket's own rules (no right to delete)", "; ")
		case err != nil:
			run.Note = strings.TrimPrefix(run.Note+"; pruning: "+err.Error(), "; ")
		}
	}
	return run
}

// pruneBackups deletes this Controller's backups past the newest keep.
func (a *app) pruneBackups(ctx context.Context, client *s3.Client, prefix string, keep int) (int, error) {
	objects, err := client.List(ctx, prefix)
	if err != nil {
		return 0, err
	}
	var mine []string
	for _, o := range objects {
		if strings.HasSuffix(o.Key, "-"+a.controllerID[:8]+".janusbackup") {
			mine = append(mine, o.Key)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(mine)))
	deleted := 0
	for i := keep; i < len(mine); i++ {
		if err := client.Delete(ctx, mine[i]); err != nil {
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}

// backupLoop runs the backups when they're due.
func (a *app) backupLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		if a.backups.Due(time.Now()) {
			bctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
			a.runBackup(bctx)
			cancel()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// --- /api/backups (admin) ---

func (a *app) registerBackupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/backups", a.gate(auth.Admin, auth.Admin, a.handleBackupStatus))
	mux.HandleFunc("PUT /api/backups/settings", a.sessionGate(auth.Admin, auth.Admin, a.handleBackupSettings))
	mux.HandleFunc("POST /api/backups/kit", a.sessionGate(auth.Admin, auth.Admin, a.handleBackupKitStart))
	mux.HandleFunc("GET /api/backups/kit", a.sessionGate(auth.Admin, auth.Admin, a.handleBackupKitDownload))
	mux.HandleFunc("POST /api/backups/kit/confirm", a.sessionGate(auth.Admin, auth.Admin, a.handleBackupKitConfirm))
	mux.HandleFunc("POST /api/backups/run", a.gate(auth.Admin, auth.Admin, a.handleBackupRun))
	mux.HandleFunc("GET /api/backups/download", a.sessionGate(auth.Admin, auth.Admin, a.handleBackupDownload))
	mux.HandleFunc("GET /api/backups/list", a.gate(auth.Admin, auth.Admin, a.handleBackupList))
}

func (a *app) handleBackupStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.backups.Status())
}

func (a *app) handleBackupSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		backup.Settings
		// SecretKey is write-only: given to change it, never answered.
		SecretKey *string `json:"secret_key"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.SecretKey != nil && *req.SecretKey == "" {
		req.SecretKey = nil
	}
	if err := a.backups.SetSettings(req.Settings, req.SecretKey); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.backups.Status())
}

func (a *app) handleBackupKitStart(w http.ResponseWriter, _ *http.Request) {
	pass, err := a.backups.StartKit()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"passphrase": pass})
}

func (a *app) handleBackupKitDownload(w http.ResponseWriter, _ *http.Request) {
	kit, err := a.backups.PendingKit()
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="janus-backup-kit-`+a.controllerID[:8]+`.age"`)
	_, _ = w.Write(kit)
}

func (a *app) handleBackupKitConfirm(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kit        string `json:"kit"`
		Passphrase string `json:"passphrase"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if err := a.backups.ConfirmKit([]byte(req.Kit), req.Passphrase); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.backups.Status())
}

// handleBackupRun backs up now, to the bucket.
func (a *app) handleBackupRun(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Minute)
	defer cancel()
	run := a.runBackup(ctx)
	if run.Error != "" {
		writeError(w, http.StatusBadGateway, run.Error)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// handleBackupDownload makes a backup and hands it over - no bucket
// needed; sealed all the same.
func (a *app) handleBackupDownload(w http.ResponseWriter, r *http.Request) {
	file, _, err := a.makeBackup(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+a.backupName(time.Now())+`"`)
	_, _ = w.Write(file)
}

// handleBackupList lists the bucket's backups (needs the right to list).
func (a *app) handleBackupList(w http.ResponseWriter, r *http.Request) {
	client, prefix, err := a.backups.Client()
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	objects, err := client.List(r.Context(), prefix)
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
