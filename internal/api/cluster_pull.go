package api

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/events"
	"github.com/swenske/Janus/internal/haproxy"
)

// The secrets members carry to one another (docs/cluster.md "Secrets:
// member to member"): a tar ClusterExport streams to a member of the
// same cluster, ClusterPull fetches from one. Its entries, by item:
//
//	runtime-certs/index.json, runtime-certs/<file>   the certificates CertificateUpload loaded
//	files/<name>                                      HAProxy's files (FilePut)
//	acme/<name>.pem                                   the letsencrypt extension's certificates, with their keys
//	tls-ticket-keys                                   the TLS ticket key ring
//
// Nothing of their content reaches a log, an event or the pull's
// progress: names only.

const (
	exportRuntimeCerts = "runtime-certs/"
	exportFiles        = "files/"
	exportACME         = "acme/"
	exportTicketKeys   = "tls-ticket-keys"
)

var allItems = []janusv1alpha1.ClusterItem{
	janusv1alpha1.ClusterItem_CLUSTER_ITEM_RUNTIME_CERTS, janusv1alpha1.ClusterItem_CLUSTER_ITEM_FILES,
	janusv1alpha1.ClusterItem_CLUSTER_ITEM_ACME_CERTS, janusv1alpha1.ClusterItem_CLUSTER_ITEM_TLS_TICKET_KEYS,
}

func itemsOf(req []janusv1alpha1.ClusterItem) ([]janusv1alpha1.ClusterItem, error) {
	if len(req) == 0 {
		return allItems, nil
	}
	for _, it := range req {
		if !slices.Contains(allItems, it) {
			return nil, fmt.Errorf("unknown item %s", it)
		}
	}
	return req, nil
}

// ClusterExport streams the items to a member of this node's cluster.
func (c *Cluster) ClusterExport(req *janusv1alpha1.ClusterExportRequest, stream janusv1alpha1.ClusterService_ClusterExportServer) error {
	caller, _ := CallerFrom(stream.Context())
	items, err := itemsOf(req.GetItems())
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	w := &dataWriter{send: func(b []byte) error { return stream.Send(&janusv1alpha1.Data{Bytes: b}) }}
	tw := tar.NewWriter(w)
	add := func(name string, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: time.Now()}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	addDir := func(prefix, dir string, keep func(string) bool) error {
		if dir == "" {
			return nil
		}
		return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil || !d.Type().IsRegular() {
				return err
			}
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if strings.HasPrefix(path.Base(rel), ".") || (keep != nil && !keep(rel)) {
				return nil // a temporary file, or not asked for
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			return add(prefix+rel, data)
		})
	}
	for _, it := range items {
		switch it {
		case janusv1alpha1.ClusterItem_CLUSTER_ITEM_RUNTIME_CERTS:
			err = addDir(exportRuntimeCerts, c.HAProxy.CertStoreDir, nil)
		case janusv1alpha1.ClusterItem_CLUSTER_ITEM_FILES:
			err = addDir(exportFiles, c.HAProxy.FilesDir, nil)
		case janusv1alpha1.ClusterItem_CLUSTER_ITEM_ACME_CERTS:
			err = addDir(exportACME, c.ACMECertDir, func(rel string) bool { return strings.HasSuffix(rel, ".pem") && !strings.Contains(rel, "/") })
		case janusv1alpha1.ClusterItem_CLUSTER_ITEM_TLS_TICKET_KEYS:
			data, rerr := os.ReadFile(c.Store.TicketKeysPath())
			if rerr == nil {
				err = add(exportTicketKeys, data)
			} else if !errors.Is(rerr, os.ErrNotExist) {
				err = rerr
			}
		}
		if err != nil {
			return status.Errorf(codes.Internal, "export %s: %v", it, err)
		}
	}
	if err := tw.Close(); err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	if err := w.flush(); err != nil {
		return err
	}
	log.Printf("cluster: secrets exported to member %s (%s)", caller.Name, itemNames(items))
	return nil
}

func itemNames(items []janusv1alpha1.ClusterItem) string {
	var out []string
	for _, it := range items {
		out = append(out, strings.ToLower(strings.TrimPrefix(it.String(), "CLUSTER_ITEM_")))
	}
	return strings.Join(out, ", ")
}

// ClusterPull fetches the items from another member and installs them.
func (c *Cluster) ClusterPull(req *janusv1alpha1.ClusterPullRequest, stream janusv1alpha1.ClusterService_ClusterPullServer) error {
	caller, _ := CallerFrom(stream.Context())
	items, err := itemsOf(req.GetItems())
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	m := c.Store.Membership()
	if m == nil {
		return status.Error(codes.FailedPrecondition, "this node is in no cluster")
	}
	from := m.Find(req.GetFrom())
	if from == nil || from.Name == m.Member {
		return status.Errorf(codes.InvalidArgument, "%q isn't another member of the cluster", req.GetFrom())
	}
	cert := c.Store.MemberTLSCertificate()
	if cert == nil {
		return status.Error(codes.FailedPrecondition, "this node has no member certificate")
	}
	addr := from.API
	if addr == "" {
		addr = net.JoinHostPort(from.Address, "9505")
	}
	pool := x509.NewCertPool()
	for _, ca := range c.Store.CAs() {
		pool.AddCert(ca)
	}
	// The source presents its member certificate for its own name: the
	// cluster's CA vouches for both ends.
	creds := credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{*cert}, RootCAs: pool, ServerName: from.Name, MinVersion: tls.VersionTLS13})
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		return status.Errorf(codes.Internal, "dial %s: %v", from.Name, err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(stream.Context(), 5*time.Minute)
	defer cancel()
	exp, err := janusv1alpha1.NewClusterServiceClient(conn).ClusterExport(ctx, &janusv1alpha1.ClusterExportRequest{Items: items})
	if err != nil {
		return status.Errorf(codes.Unavailable, "member %s: %v", from.Name, err)
	}
	// The whole tar first: a member that stops answering halfway leaves
	// nothing half installed.
	var archive bytes.Buffer
	for {
		chunk, err := exp.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return status.Errorf(codes.Unavailable, "member %s: %v", from.Name, err)
		}
		archive.Write(chunk.GetBytes())
	}
	send := func(item janusv1alpha1.ClusterItem, name, msg string) error {
		return stream.Send(&janusv1alpha1.ClusterPullProgress{Item: item, Name: name, Message: msg})
	}
	var certIndex []byte
	certFiles := map[string][]byte{}
	tr := tar.NewReader(&archive)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return status.Errorf(codes.Internal, "the export from %s: %v", from.Name, err)
		}
		data, err := io.ReadAll(io.LimitReader(tr, 4<<20))
		if err != nil {
			return status.Errorf(codes.Internal, "the export from %s: %v", from.Name, err)
		}
		name := path.Clean(h.Name)
		switch {
		case strings.HasPrefix(name, exportRuntimeCerts):
			if rest := strings.TrimPrefix(name, exportRuntimeCerts); rest == "index.json" {
				certIndex = data
			} else {
				certFiles[rest] = data
			}
		case strings.HasPrefix(name, exportFiles):
			file := strings.TrimPrefix(name, exportFiles)
			if errs, err := c.HAProxy.FilePut(file, data); err != nil {
				if err := send(janusv1alpha1.ClusterItem_CLUSTER_ITEM_FILES, file, "refused: "+err.Error()+" "+joinLines(errs)); err != nil {
					return err
				}
			} else if err := send(janusv1alpha1.ClusterItem_CLUSTER_ITEM_FILES, file, "written"); err != nil {
				return err
			}
		case strings.HasPrefix(name, exportACME):
			cn := strings.TrimSuffix(strings.TrimPrefix(name, exportACME), ".pem")
			msg := "installed"
			if c.ACME == nil || !c.ACME.Available() {
				msg = "skipped: no letsencrypt extension on this node"
			} else if err := c.ACME.InstallPulled(cn, data); err != nil {
				msg = "refused: " + err.Error()
			}
			if err := send(janusv1alpha1.ClusterItem_CLUSTER_ITEM_ACME_CERTS, cn, msg); err != nil {
				return err
			}
		case name == exportTicketKeys:
			var msg string
			keys, err := c.Store.ReplaceTicketKeys(data)
			if err != nil {
				msg = "refused: " + err.Error()
			} else if loaded, err := c.pushTicketKeys(keys); err != nil {
				msg = "installed, but HAProxy didn't take it: " + err.Error()
			} else if !loaded {
				msg = "installed; the configuration doesn't use it yet"
			} else {
				msg = "installed and handed to HAProxy"
			}
			if err := send(janusv1alpha1.ClusterItem_CLUSTER_ITEM_TLS_TICKET_KEYS, "", msg); err != nil {
				return err
			}
		}
	}
	if certIndex != nil {
		var index []struct {
			Name    string   `json:"name"`
			File    string   `json:"file"`
			CrtList string   `json:"crt_list"`
			SNI     []string `json:"sni"`
		}
		if err := json.Unmarshal(certIndex, &index); err != nil {
			return status.Errorf(codes.Internal, "the certificate store from %s: %v", from.Name, err)
		}
		for _, e := range index {
			msg := "loaded"
			data, ok := certFiles[e.File]
			if !ok {
				msg = "refused: its file isn't in the export"
			} else if err := c.HAProxy.CertificateUpload(e.Name, data, e.CrtList, e.SNI); err != nil {
				msg = "refused: " + err.Error()
			}
			if err := send(janusv1alpha1.ClusterItem_CLUSTER_ITEM_RUNTIME_CERTS, e.Name, msg); err != nil {
				return err
			}
		}
	}
	log.Printf("cluster: secrets pulled from member %s (%s) - by %s", from.Name, itemNames(items), caller)
	events.Publish("cluster.secrets_pulled", map[string]any{"from": from.Name, "items": itemNames(items), "by": caller.String()})
	return stream.Send(&janusv1alpha1.ClusterPullProgress{Message: "pulled from " + from.Name, Done: true})
}

// pushTicketKeys hands HAProxy the ring, oldest key first: its own ring
// ends up the file's. loaded false: the configuration names no ring.
func (c *Cluster) pushTicketKeys(keys []string) (loaded bool, err error) {
	if c.HAProxy == nil || c.PeersFile == "" {
		return false, nil
	}
	for _, k := range keys {
		err := c.HAProxy.SetTLSKey(c.Store.TicketKeysPath(), k)
		if errors.Is(err, haproxy.ErrTLSKeysNotLoaded) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
	return true, nil
}

// TLSTicketKeysRotate adds a key to the ring and hands it to HAProxy.
func (c *Cluster) TLSTicketKeysRotate(ctx context.Context, _ *emptypb.Empty) (*janusv1alpha1.TLSTicketKeysState, error) {
	caller, _ := CallerFrom(ctx)
	if _, err := c.Store.EnsureTicketKeys(); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	key, err := c.Store.RotateTicketKeys()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	loaded, err := c.pushTicketKeys([]string{key})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "the ring is rotated, but HAProxy didn't take the key: %v", err)
	}
	log.Printf("cluster: TLS ticket key ring rotated (HAProxy loaded it: %v) - by %s", loaded, caller)
	events.Publish("cluster.tls_ticket_keys_rotated", map[string]any{"loaded": loaded, "by": caller.String()})
	st := c.ticketKeysState()
	st.Loaded = loaded
	return st, nil
}

func (c *Cluster) ticketKeysState() *janusv1alpha1.TLSTicketKeysState {
	st := c.Store.TicketKeysState()
	if st.Keys == 0 {
		return nil
	}
	return &janusv1alpha1.TLSTicketKeysState{Keys: uint32(st.Keys), RotatedUnix: st.Changed.Unix(), Sha256: st.SHA256}
}
