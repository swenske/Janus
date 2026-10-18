package nodeproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"google.golang.org/grpc/status"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// probeTimeout covers a whole probe (60 s on the node) and the way there.
const probeTimeout = 75 * time.Second

// The node tests the network (SystemService.Probe): each event a line
// of NDJSON - protojson, every field -, the last "done", or "error"
// when the stream broke after the first. A request the node refuses
// (an argument, the role, 4 probes running) is an HTTP error: the
// first event is awaited before answering. POST: a probe is no read,
// and the page reads the stream with fetch - an EventSource would run
// it again on its own once it ends.
func registerProbeRoutes(mux *http.ServeMux, node *store.Node) {
	mux.HandleFunc("POST /api/system/probe", func(w http.ResponseWriter, r *http.Request) {
		req := &janusv1alpha1.ProbeRequest{}
		if !decodeProto(w, r, req) {
			return
		}
		conn, err := dialNode(node)
		if err != nil {
			http.Error(w, fmt.Sprintf("dial node: %v", err), http.StatusBadGateway)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
		defer cancel()
		stream, err := janusv1alpha1.NewSystemServiceClient(conn).Probe(ctx, req)
		var first *janusv1alpha1.ProbeEvent
		if err == nil {
			first, err = stream.Recv()
		}
		if err != nil {
			http.Error(w, status.Convert(err).Message(), grpcHTTPStatus(err))
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		line := func(data []byte) {
			_, _ = w.Write(append(data, '\n'))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		for ev := first; ; {
			data, err := protoOut.Marshal(ev)
			if err != nil {
				break
			}
			line(data)
			if ev, err = stream.Recv(); err == io.EOF {
				return
			} else if err != nil {
				data, _ := json.Marshal(map[string]string{"stage": "error", "message": status.Convert(err).Message()})
				line(data)
				return
			}
		}
	})
}
