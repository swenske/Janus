package nodeproxy

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/pki"
)

// certView is a certificate as the Access page shows it.
type certView struct {
	Subject     string    `json:"subject"`
	Fingerprint string    `json:"fingerprint"`
	NotAfter    time.Time `json:"not_after"`
}

func viewCert(p []byte) *certView {
	block, _ := pem.Decode(p)
	if block == nil {
		return nil
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil
	}
	return &certView{Subject: c.Subject.CommonName, Fingerprint: pki.Fingerprint(c.Raw), NotAfter: c.NotAfter}
}

// registerAccessRoutes: who the node lets in - its fleet's trust, its own
// CA - and replacing that CA.
func registerAccessRoutes(mux *http.ServeMux, node *store.Node) {
	mux.HandleFunc("GET /api/access/trust", func(w http.ResponseWriter, r *http.Request) {
		unary(w, r, node, unaryTimeout, func(ctx context.Context, conn *grpc.ClientConn) (any, error) {
			st, err := janusv1alpha1.NewAccessServiceClient(conn).TrustGet(ctx, &emptypb.Empty{})
			if err != nil {
				return nil, err
			}
			out := map[string]any{
				"controller_on_fleet": node.TrustsFleet(),
				"local_ca":            viewCert(node.CA()),
				"bundle_version":      st.GetBundleVersion(),
				"issuing_cas":         []*certView{},
			}
			if root := viewCert(st.GetRootCert()); root != nil {
				out["root"] = root
			}
			if t := st.GetBundleIssuedUnix(); t > 0 {
				out["bundle_issued"] = time.Unix(t, 0).UTC()
			}
			var cas []*certView
			for _, p := range st.GetIssuingCas() {
				if c := viewCert(p); c != nil {
					cas = append(cas, c)
				}
			}
			if cas != nil {
				out["issuing_cas"] = cas
			}
			return out, nil
		})
	})

	// Replacing the node's own CA: every certificate it issued stops
	// working - the first boot's admin credential, those issued on this
	// page, and a credential the Controller holds from before the fleet:
	// refused until the Controller reaches the node through the fleet.
	// The new admin credential is for the key the browser made (its
	// private half never leaves the browser), or printed on the node's
	// console.
	mux.HandleFunc("POST /api/access/rotate-ca", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			AdminPublicKey string `json:"admin_public_key"`
			Console        bool   `json:"console"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		if !node.TrustsFleet() {
			http.Error(w, "the Controller reaches this node with a credential its own CA issued: replacing the CA would cut it off - secure the fleet first", http.StatusConflict)
			return
		}
		if req.Console == (strings.TrimSpace(req.AdminPublicKey) != "") {
			http.Error(w, "either a public key for the new admin credential, or the node's console", http.StatusBadRequest)
			return
		}
		unary(w, r, node, unaryTimeout, func(ctx context.Context, conn *grpc.ClientConn) (any, error) {
			resp, err := janusv1alpha1.NewAccessServiceClient(conn).LocalCARotate(ctx, &janusv1alpha1.LocalCARotateRequest{AdminPublicKey: []byte(req.AdminPublicKey)})
			if err != nil {
				return nil, err
			}
			return map[string]any{"ca_cert": string(resp.GetCaCert()), "admin_cert": string(resp.GetAdminCert()), "ca": viewCert(resp.GetCaCert())}, nil
		})
	})
}
