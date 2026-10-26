package nodeproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// applyStream is ApplyConfig's progress stream as the node sends it:
// msgs, then err (io.EOF: the end).
type applyStream struct {
	msgs []*janusv1alpha1.ApplyConfigResponse
	err  error
}

func (s *applyStream) Recv() (*janusv1alpha1.ApplyConfigResponse, error) {
	if len(s.msgs) > 0 {
		m := s.msgs[0]
		s.msgs = s.msgs[1:]
		return m, nil
	}
	return nil, s.err
}

// A configuration the node refuses - its cluster's lock (v2026.10.26's
// lab) - is answered as a refused file is: 400 with the node's words,
// not a 502 carrying gRPC's.
func TestApplyConfigRefusal(t *testing.T) {
	const lock = "HAProxy's configuration is managed by cluster edge: change it on the cluster, or release this node's lock first (janusctl cluster release haproxy) - the cluster then reports this node as drifted"
	for _, tc := range []struct {
		name   string
		stream *applyStream
		code   int
		body   string
	}{
		{"applied", &applyStream{msgs: []*janusv1alpha1.ApplyConfigResponse{{Stage: "validating"}, {Stage: "done", Accepted: true}}, err: io.EOF}, http.StatusOK, `"accepted":true`},
		{"the cluster's lock", &applyStream{err: status.Error(codes.FailedPrecondition, lock)}, http.StatusBadRequest, lock},
		{"not allowed", &applyStream{err: status.Error(codes.PermissionDenied, "ApplyConfig requires role [os:admin os:operator]")}, http.StatusForbidden, "requires role"},
		{"the node gone", &applyStream{msgs: []*janusv1alpha1.ApplyConfigResponse{{Stage: "validating"}}, err: status.Error(codes.Unavailable, "connection reset")}, http.StatusBadGateway, "connection reset"},
		{"nothing", &applyStream{err: io.EOF}, http.StatusBadGateway, "no progress messages"},
	} {
		rec := httptest.NewRecorder()
		writeApplyResult(rec, tc.stream)
		if rec.Code != tc.code || !strings.Contains(rec.Body.String(), tc.body) || strings.Contains(rec.Body.String(), "rpc error") {
			t.Errorf("%s: %d %q, want %d with %q", tc.name, rec.Code, rec.Body.String(), tc.code, tc.body)
		}
	}
}
