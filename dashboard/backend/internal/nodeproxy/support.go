package nodeproxy

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
	"github.com/swenske/Janus/internal/support"
)

// supportTimeout bounds a diagnostic bundle: every call is 15 s at most,
// and there are a few dozen.
const supportTimeout = 3 * time.Minute

// registerSupportRoutes: the node's diagnostic bundle (internal/support),
// read with the account's own permissions - what it may not read is left
// out, never asked for (the node would keep each refusal in its audit) -
// and streamed as it's written.
func registerSupportRoutes(mux *http.ServeMux, node *store.Node) {
	mux.HandleFunc("GET /api/support", func(w http.ResponseWriter, r *http.Request) {
		if !sameOriginOrDirect(r) {
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		}
		conn, err := dialNode(node)
		if err != nil {
			http.Error(w, fmt.Sprintf("dial node: %v", err), http.StatusBadGateway)
			return
		}
		u := userOf(r.Context())
		col := &support.Collector{
			Conn: conn, Node: node.Name, Role: strings.Join(u.Roles(), ","), By: "Janus Controller " + ControllerVersion + " for " + u.Name,
			Allowed: func(method string) bool { return u.allows("/janus.v1alpha1." + method) },
		}
		ctx, cancel := context.WithTimeout(r.Context(), supportTimeout)
		defer cancel()
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="janus-support-%s-%s.tar.gz"`,
			unsafeFilenameChars.ReplaceAllString(node.Name, "_"), time.Now().UTC().Format("20060102T150405Z")))
		w.Header().Set("Cache-Control", "no-store")
		if _, err := col.Write(ctx, w); err != nil {
			log.Printf("nodeproxy: %s's diagnostic bundle: %v", node.Name, err)
		}
	})
}
