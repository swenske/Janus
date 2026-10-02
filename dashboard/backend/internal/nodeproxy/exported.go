package nodeproxy

import (
	"context"
	"net/http"

	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/schematic"
)

// Shutdown asks node to power off cleanly (it stops HAProxy first) -
// before the Controller destroys the virtual machine it runs in.
func Shutdown(ctx context.Context, node *store.Node) error {
	conn, err := dialNode(node)
	if err != nil {
		return err
	}
	_, err = janusv1alpha1.NewSystemServiceClient(conn).Shutdown(ctx, &emptypb.Empty{})
	return err
}

// SSE is the per-node pages' Server-Sent Events writer (heartbeat
// included), for dashboardd's own streams.
type SSE struct{ s *sseWriter }

func NewSSE(w http.ResponseWriter, r *http.Request) (*SSE, bool) {
	s, ok := newSSE(w, r)
	if !ok {
		return nil, false
	}
	return &SSE{s: s}, true
}

// Send writes one event ("" for the default "message" event). An error
// for the browser goes out as "failure", never "error" (EventSource's
// own connection-error event).
func (s *SSE) Send(event, payload string) error { return s.s.send(event, payload) }

// FactoryExtensions is the extensions the image factory builds, from
// its newest release that offers them (cached).
func FactoryExtensions(ctx context.Context) ([]schematic.CatalogEntry, error) {
	c, err := factoryCatalog(ctx)
	if err != nil {
		return nil, err
	}
	return c.Extensions, nil
}
