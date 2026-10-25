package main

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swenske/Janus/internal/clusters"
)

// hookCall is a call the test balancer received, with the calls its
// nodes had had when it came.
type hookCall struct {
	clusters.BalancerCall
	signed     bool
	nodeCalls  []string
	statusCode int
}

type testBalancer struct {
	mu     sync.Mutex
	calls  []hookCall
	answer int // the status it answers; 0: 200
	f      *clusterFake
	mark   int
}

func (b *testBalancer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var c clusters.BalancerCall
	_ = json.Unmarshal(body, &c)
	b.mu.Lock()
	defer b.mu.Unlock()
	code := b.answer
	if code == 0 {
		code = http.StatusOK
	}
	signed := hmac.Equal([]byte(r.Header.Get("X-Janus-Signature-256")), []byte(clusters.Signature([]byte("s3cret"), body)))
	b.calls = append(b.calls, hookCall{BalancerCall: c, signed: signed && r.Header.Get("X-Janus-Event") == c.Event, nodeCalls: b.f.callsSince(b.mark), statusCode: code})
	w.WriteHeader(code)
}

func (b *testBalancer) since(n int) []hookCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.calls[n:])
}

func (b *testBalancer) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.calls)
}

// inService is the members a call shows in service.
func inService(c hookCall) []string {
	var out []string
	for _, m := range c.Members {
		if m.InService {
			out = append(out, m.Name)
		}
	}
	return out
}

// The balancer in front is told a member is in service once it is, and
// that one goes out before it is drained or removed - the operation
// waiting for its answer, refused when it doesn't come.
func TestClusterBalancer(t *testing.T) {
	a, f, nodes := clusterApp(t, "lb-1", "lb-2", "lb-3", "lb-4")
	c := newCluster(t, a, nodes, "lb-1", "lb-2", "lb-3")
	ctx := context.Background()
	if err := a.clusters.Create(ctx, c.ID, "lb-1", "admin"); err != nil {
		t.Fatal(err)
	}
	prevRetries := clusters.BalancerRetries
	clusters.BalancerRetries = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { clusters.BalancerRetries = prevRetries })
	hook := &testBalancer{f: f}
	srv := httptest.NewTLSServer(hook)
	t.Cleanup(srv.Close)
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))

	// Set: called at once, every member in service, signed.
	put := func(body map[string]any) *httpResult {
		rec := call(t, a.handleClusterBalancer, "PUT", "/api/clusters/"+c.ID+"/balancer", "PUT /api/clusters/{id}/balancer", body)
		return &httpResult{rec.Code, rec.Body.String()}
	}
	if r := put(map[string]any{"url": "ftp://lb.example.net"}); r.code != http.StatusBadRequest {
		t.Errorf("an ftp URL: %d %s", r.code, r.body)
	}
	if r := put(map[string]any{"url": srv.URL + "/hook", "ca_cert": ca, "secret": "s3cret"}); r.code != http.StatusOK || !strings.Contains(r.body, `"has_secret":true`) || strings.Contains(r.body, "s3cret") {
		t.Fatalf("set: %d %s", r.code, r.body)
	}
	got := hook.since(0)
	if len(got) != 1 || got[0].Event != "sync" || !got[0].signed || !slices.Equal(inService(got[0]), []string{"lb-1", "lb-2", "lb-3"}) || got[0].Members[0].Address != "10.0.0.1" {
		t.Fatalf("the first call: %+v", got)
	}

	// Drain: out first, then the drain; undrain: back, then in.
	n, mark := hook.count(), f.mark()
	hook.mu.Lock()
	hook.mark = mark
	hook.mu.Unlock()
	if err := a.clusters.Drain(ctx, c.ID, "lb-1", "admin"); err != nil {
		t.Fatal(err)
	}
	got = hook.since(n)
	if len(got) != 1 || got[0].Event != "out" || got[0].Member != "lb-1" || !slices.Equal(inService(got[0]), []string{"lb-2", "lb-3"}) || slices.Contains(got[0].nodeCalls, "lb-1 ClusterDrain") {
		t.Errorf("draining lb-1: %+v", got)
	}
	if !slices.Contains(f.callsOf("lb-1", mark), "ClusterDrain") {
		t.Error("lb-1 not drained")
	}
	n, mark = hook.count(), f.mark()
	hook.mu.Lock()
	hook.mark = mark
	hook.mu.Unlock()
	if err := a.clusters.Undrain(ctx, c.ID, "lb-1", "admin"); err != nil {
		t.Fatal(err)
	}
	got = hook.since(n)
	if len(got) != 1 || got[0].Event != "in" || !slices.Equal(inService(got[0]), []string{"lb-1", "lb-2", "lb-3"}) || !slices.Contains(got[0].nodeCalls, "lb-1 ClusterUndrain") {
		t.Errorf("undraining lb-1: %+v", got)
	}

	// The hook refusing: no drain, lb-1 serves on, the cluster says so.
	hook.mu.Lock()
	hook.answer = http.StatusServiceUnavailable
	hook.mu.Unlock()
	n, mark = hook.count(), f.mark()
	err := a.clusters.Drain(ctx, c.ID, "lb-1", "admin")
	if err == nil || !strings.Contains(err.Error(), "the balancer wasn't told lb-1 going out of service") || !strings.Contains(err.Error(), "503") {
		t.Errorf("a hook answering 503: %v", err)
	}
	if hook.count()-n < 3 || slices.Contains(f.callsOf("lb-1", mark), "ClusterDrain") {
		t.Errorf("tried %d times, lb-1's calls %v", hook.count()-n, f.callsOf("lb-1", mark))
	}
	cur, _ := a.clusters.Store.Get(c.ID)
	if v := a.clusterView(cur); !slices.ContainsFunc(v.Problems, func(p string) bool { return strings.Contains(p, "balancer in front wasn't told") }) {
		t.Errorf("the problems: %v", v.Problems)
	}
	// Reconcile calls again once it answers.
	hook.mu.Lock()
	hook.answer = 0
	hook.mu.Unlock()
	n = hook.count()
	a.clusters.Reconcile(ctx, cur)
	if got = hook.since(n); len(got) != 1 || got[0].Event != "sync" || a.clusters.Store.State(c.ID).Balancer.Error != "" {
		t.Errorf("the check after: %+v, state %+v", got, a.clusters.Store.State(c.ID).Balancer)
	}
	// Nothing changed since: the next check calls nothing.
	n = hook.count()
	cur, _ = a.clusters.Store.Get(c.ID)
	a.clusters.Reconcile(ctx, cur)
	if hook.count() != n {
		t.Errorf("a check with nothing changed called %d times", hook.count()-n)
	}

	// A replacement: lb-4 in once in service, lb-1 out before its drain,
	// then the list without lb-1.
	n, mark = hook.count(), f.mark()
	hook.mu.Lock()
	hook.mark = mark
	hook.mu.Unlock()
	lb4 := clusters.Member{NodeID: nodes["lb-4"].ID, Name: "lb-4", Address: "10.0.0.4"}
	if err := a.clusters.Replace(ctx, c.ID, "lb-1", lb4, clusters.ReplaceOptions{}, "admin"); err != nil {
		t.Fatal(err)
	}
	got = hook.since(n)
	var events []string
	for _, h := range got {
		events = append(events, h.Event+" "+h.Member)
	}
	if !slices.Equal(events, []string{"in lb-4", "out lb-1", "sync "}) {
		t.Fatalf("the replacement's calls: %v", events)
	}
	if !slices.Contains(got[0].nodeCalls, "lb-4 ClusterUndrain") || !slices.Equal(inService(got[0]), []string{"lb-1", "lb-2", "lb-3", "lb-4"}) {
		t.Errorf("lb-4 in: %v, %v", inService(got[0]), got[0].nodeCalls)
	}
	if slices.Contains(got[1].nodeCalls, "lb-1 ClusterDrain") || !slices.Equal(inService(got[1]), []string{"lb-2", "lb-3", "lb-4"}) {
		t.Errorf("lb-1 out: %v, %v", inService(got[1]), got[1].nodeCalls)
	}
	if len(got[2].Members) != 3 || !slices.Equal(inService(got[2]), []string{"lb-2", "lb-3", "lb-4"}) {
		t.Errorf("after: %+v", got[2].Members)
	}

	// A removal with force goes on when the hook fails (a node gone).
	hook.mu.Lock()
	hook.answer = http.StatusBadRequest
	hook.mu.Unlock()
	n = hook.count()
	if err := a.clusters.RemoveMember(ctx, c.ID, "lb-3", false, "admin"); err == nil {
		t.Error("removing lb-3 without the balancer told went through")
	} else if hook.count()-n != 1 {
		t.Errorf("a 400 tried %d times: no point trying again", hook.count()-n)
	}
	if cur, _ := a.clusters.Store.Get(c.ID); cur.Member("lb-3") == nil {
		t.Fatal("lb-3 removed")
	}
	if err := a.clusters.RemoveMember(ctx, c.ID, "lb-3", true, "admin"); err != nil {
		t.Fatal(err)
	}
	if cur, _ := a.clusters.Store.Get(c.ID); cur.Member("lb-3") != nil {
		t.Error("lb-3 not removed with force")
	}

	// The drain delay: the member serves that long after the balancer
	// was told, then is drained.
	hook.mu.Lock()
	hook.answer = 0
	hook.mu.Unlock()
	if r := put(map[string]any{"url": srv.URL + "/hook", "ca_cert": ca, "drain_delay_seconds": 1}); r.code != http.StatusOK || !strings.Contains(r.body, `"has_secret":true`) {
		t.Fatalf("the secret kept when not given: %d %s", r.code, r.body)
	}
	start := time.Now()
	if err := a.clusters.Drain(ctx, c.ID, "lb-4", "admin"); err != nil || time.Since(start) < time.Second {
		t.Errorf("drain with a delay: %v after %v", err, time.Since(start))
	}
	_ = a.clusters.Undrain(ctx, c.ID, "lb-4", "admin")

	// Removed: no call any more.
	rec := call(t, a.handleClusterBalancerDelete, "DELETE", "/api/clusters/"+c.ID+"/balancer", "DELETE /api/clusters/{id}/balancer", nil)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"balancer"`) {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	n = hook.count()
	if err := a.clusters.Drain(ctx, c.ID, "lb-2", "admin"); err != nil || hook.count() != n {
		t.Errorf("without a hook: %v, %d calls", err, hook.count()-n)
	}
}
