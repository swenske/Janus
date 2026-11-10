package nodeproxy

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// TestClusterOfAnOldNode: a node too old to have ClusterService is in no
// cluster - a 200 the page reads as none, not a 501 the browser logs as
// an error on each of its pages (v2026.11.09's lab campaign, a
// v2026.10.21 node).
func TestClusterOfAnOldNode(t *testing.T) {
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer() // no ClusterService: Unimplemented
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	node := &store.Node{ID: "old-cluster-test"}
	conns.Store(node.ID, conn)
	t.Cleanup(func() { closeNodeConn(node.ID) })

	mux := http.NewServeMux()
	registerClusterRoutes(mux, node)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/cluster", nil))
	var got clusterMembership
	if err := json.Unmarshal(rec.Body.Bytes(), &got); rec.Code != http.StatusOK || err != nil || got.Member {
		t.Fatalf("%d %s (%v)", rec.Code, rec.Body.String(), err)
	}
}
