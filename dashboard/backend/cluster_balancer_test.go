package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
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

// fakeRouterOS is a RouterOS router's REST API for its routes - IPv4,
// and IPv6 for a route whose "_family" is "ipv6": the routes it holds,
// and every call it got with the node calls made by then.
type fakeRouterOS struct {
	mu     sync.Mutex
	routes map[string]map[string]string // .id: the route
	next   int
	calls  []string
	answer int // a status to answer every call with; 0: as RouterOS would
	f      *clusterFake
	mark   int
}

func (r *fakeRouterOS) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	user, pass, ok := req.BasicAuth()
	if !ok || user != "janus-hook" || pass != "router-pw" {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":401,"message":"Unauthorized"}`))
		return
	}
	r.calls = append(r.calls, req.Method+" "+req.URL.Path+" nodes:"+strings.Join(r.f.callsSince(r.mark), ","))
	if r.answer != 0 {
		w.WriteHeader(r.answer)
		_, _ = w.Write([]byte(`{"error":503,"message":"Service Unavailable","detail":"busy"}`))
		return
	}
	family := func(rt map[string]string) string {
		if rt["_family"] == "ipv6" {
			return "ipv6"
		}
		return "ip"
	}
	switch {
	case req.Method == "GET" && (req.URL.Path == "/rest/ip/route" || req.URL.Path == "/rest/ipv6/route"):
		out := []map[string]string{}
		for _, rt := range r.routes {
			q := req.URL.Query()
			if "/rest/"+family(rt)+"/route" == req.URL.Path && (q.Get("comment") == "" || rt["comment"] == q.Get("comment")) &&
				(q.Get("dst-address") == "" || rt["dst-address"] == q.Get("dst-address")) {
				out = append(out, rt)
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	case req.Method == "PUT" && req.URL.Path == "/rest/ip/route":
		var rt map[string]string
		if json.NewDecoder(req.Body).Decode(&rt) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		r.next++
		rt[".id"] = fmt.Sprintf("*%X", r.next)
		r.routes[rt[".id"]] = rt
		_ = json.NewEncoder(w).Encode(rt)
	case req.Method == "DELETE" && (strings.HasPrefix(req.URL.Path, "/rest/ip/route/") || strings.HasPrefix(req.URL.Path, "/rest/ipv6/route/")):
		id := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
		if _, ok := r.routes[id]; !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":404,"message":"Not Found"}`))
			return
		}
		delete(r.routes, id)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

// gateways are the gateways of the routes to dst with comment, sorted.
func (r *fakeRouterOS) gateways(dst, comment string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, rt := range r.routes {
		if rt["dst-address"] == dst && rt["comment"] == comment {
			out = append(out, rt["gateway"])
		}
	}
	slices.Sort(out)
	return out
}

func (r *fakeRouterOS) since(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls[n:])
}

func (r *fakeRouterOS) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// A RouterOS router as the balancer: a route to the service prefix per
// member in service, kept by the engine - removed before a drain, put
// back after an undrain and when changed on the router; another route
// never touched.
func TestClusterBalancerRouterOS(t *testing.T) {
	a, f, nodes := clusterApp(t, "lb-1", "lb-2", "lb-3")
	c := newCluster(t, a, nodes, "lb-1", "lb-2", "lb-3")
	ctx := context.Background()
	if err := a.clusters.Create(ctx, c.ID, "lb-1", "admin"); err != nil {
		t.Fatal(err)
	}
	prevRetries := clusters.BalancerRetries
	clusters.BalancerRetries = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { clusters.BalancerRetries = prevRetries })
	router := &fakeRouterOS{f: f, routes: map[string]map[string]string{
		"*99": {".id": "*99", "dst-address": "0.0.0.0/0", "gateway": "192.0.2.1", "distance": "1", "routing-table": "main", "comment": "uplink"},
	}}
	srv := httptest.NewTLSServer(router)
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(srv.Certificate().Raw)
	fp := strings.ToUpper(hex.EncodeToString(sum[:]))
	comment := clusters.RouterOSComment(c.ID)
	put := func(body map[string]any) *httpResult {
		rec := call(t, a.handleClusterBalancer, "PUT", "/api/clusters/"+c.ID+"/balancer", "PUT /api/clusters/{id}/balancer", body)
		return &httpResult{rec.Code, rec.Body.String()}
	}
	ros := func(extra map[string]any) map[string]any {
		b := map[string]any{"kind": "routeros", "url": srv.URL, "fingerprint": fp, "user": "janus-hook", "prefixes": []string{"203.0.113.10/32"}}
		for k, v := range extra {
			b[k] = v
		}
		return b
	}

	// Refused as it stands.
	for _, tc := range []struct {
		body map[string]any
		want string
	}{
		{ros(map[string]any{"url": strings.Replace(srv.URL, "https", "http", 1), "fingerprint": ""}), "over https"},
		{ros(map[string]any{"prefixes": []string{"203.0.113.10/24"}}), "no host bits"},
		{ros(map[string]any{"user": ""}), "balancer user"},
		{ros(map[string]any{"kind": "bgp"}), "webhook or routeros"},
		{map[string]any{"url": "https://hook.example.net", "prefixes": []string{"203.0.113.10/32"}}, "for a routeros balancer"},
		{ros(nil), "the router user's password"},
	} {
		if r := put(tc.body); r.code == http.StatusOK || !strings.Contains(r.body, tc.want) {
			t.Errorf("%v: %d %s", tc.body, r.code, r.body)
		}
	}

	// Set: a route per member, nothing else touched.
	if r := put(ros(map[string]any{"secret": "router-pw"})); r.code != http.StatusOK || strings.Contains(r.body, "router-pw") || !strings.Contains(r.body, `"has_secret":true`) {
		t.Fatalf("set: %d %s", r.code, r.body)
	}
	if got := router.gateways("203.0.113.10/32", comment); !slices.Equal(got, []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}) {
		t.Fatalf("the routes: %v", got)
	}
	if !journalSays(t, a, c.ID, "the router was told the members (routes: 3 added, 0 removed)") {
		t.Errorf("the journal: %+v", a.clusters.Store.Entries(c.ID, 5))
	}

	// Drain: lb-1's route gone before the drain; undrain: back after.
	n, mark := router.count(), f.mark()
	router.mu.Lock()
	router.mark = mark
	router.mu.Unlock()
	if err := a.clusters.Drain(ctx, c.ID, "lb-1", "admin"); err != nil {
		t.Fatal(err)
	}
	if got := router.gateways("203.0.113.10/32", comment); !slices.Equal(got, []string{"10.0.0.2", "10.0.0.3"}) {
		t.Errorf("after lb-1's drain: %v", got)
	}
	calls := router.since(n)
	del := slices.IndexFunc(calls, func(s string) bool { return strings.HasPrefix(s, "DELETE ") })
	if del < 0 || strings.Contains(calls[del], "lb-1 ClusterDrain") || !slices.Contains(f.callsOf("lb-1", mark), "ClusterDrain") {
		t.Errorf("lb-1's route removed before its drain: %v", calls)
	}
	if err := a.clusters.Undrain(ctx, c.ID, "lb-1", "admin"); err != nil {
		t.Fatal(err)
	}
	if got := router.gateways("203.0.113.10/32", comment); !slices.Equal(got, []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}) {
		t.Errorf("after lb-1's undrain: %v", got)
	}

	// Changed on the router: a route deleted, another's distance changed -
	// the next check puts them back, the foreign route untouched.
	router.mu.Lock()
	for id, rt := range router.routes {
		switch rt["gateway"] {
		case "10.0.0.2":
			delete(router.routes, id)
		case "10.0.0.3":
			rt["distance"] = "5"
		}
	}
	router.mu.Unlock()
	cur, _ := a.clusters.Store.Get(c.ID)
	a.clusters.Reconcile(ctx, cur)
	if got := router.gateways("203.0.113.10/32", comment); !slices.Equal(got, []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}) {
		t.Errorf("after the check: %v", got)
	}
	if !journalSays(t, a, c.ID, "the router's routes put back (2 added, 1 removed)") {
		t.Errorf("the journal: %+v", a.clusters.Store.Entries(c.ID, 5))
	}
	n = router.count()
	cur, _ = a.clusters.Store.Get(c.ID)
	a.clusters.Reconcile(ctx, cur)
	if calls := router.since(n); len(calls) != 1 || !strings.HasPrefix(calls[0], "GET ") {
		t.Errorf("a check with nothing changed: %v", calls)
	}
	if got := router.gateways("0.0.0.0/0", "uplink"); !slices.Equal(got, []string{"192.0.2.1"}) {
		t.Errorf("the foreign route: %v", got)
	}

	// check_gateway: the routes made again with check-gateway=ping.
	if r := put(ros(map[string]any{"check_gateway": true})); r.code != http.StatusOK || strings.Contains(r.body, "wasn't told") {
		t.Fatalf("check_gateway: %d %s", r.code, r.body)
	}
	router.mu.Lock()
	pinged := 0
	for _, rt := range router.routes {
		if rt["comment"] == comment && rt["check-gateway"] == "ping" {
			pinged++
		}
	}
	router.mu.Unlock()
	if pinged != 3 || !journalSays(t, a, c.ID, "(routes: 3 added, 3 removed)") {
		t.Errorf("check_gateway: %d routes pinging", pinged)
	}

	// The router busy: retried, then the drain refused, lb-1 serving on.
	router.mu.Lock()
	router.answer = http.StatusServiceUnavailable
	router.mu.Unlock()
	n, mark = router.count(), f.mark()
	err := a.clusters.Drain(ctx, c.ID, "lb-1", "admin")
	if err == nil || !strings.Contains(err.Error(), "the router wasn't told lb-1 going out of service") || !strings.Contains(err.Error(), "fix the router's access") {
		t.Errorf("a router answering 503: %v", err)
	}
	if router.count()-n != 3 || slices.Contains(f.callsOf("lb-1", mark), "ClusterDrain") {
		t.Errorf("tried %d times, lb-1's calls %v", router.count()-n, f.callsOf("lb-1", mark))
	}
	router.mu.Lock()
	router.answer = 0
	router.mu.Unlock()

	// A wrong password: once, no retry; a wrong certificate: refused.
	if r := put(ros(map[string]any{"secret": "wrong"})); r.code != http.StatusOK || !strings.Contains(r.body, "401") {
		t.Errorf("a wrong password: %d %s", r.code, r.body)
	}
	if r := put(ros(map[string]any{"secret": "router-pw", "fingerprint": strings.Repeat("ab", 32)})); r.code != http.StatusOK || !strings.Contains(r.body, "not the one pinned") {
		t.Errorf("a wrong fingerprint: %d %s", r.code, r.body)
	}
	// Back to a webhook: the router's password isn't kept as its secret.
	if r := put(map[string]any{"url": "https://hook.example.net/janus"}); r.code != http.StatusOK || strings.Contains(r.body, `"has_secret":true`) {
		t.Errorf("a webhook again: %d %s", r.code, r.body)
	}
}

// TestRouterOSLive keeps a real RouterOS router's routes - run by
// hack/routeros-hook-test.sh against a CHR under QEMU, skipped
// otherwise: JANUS_ROUTEROS_URL (https), _FINGERPRINT, _USER, _PASSWORD
// for the engine; JANUS_ROUTEROS_CHECK_URL, an administrator's REST
// over HTTP, to read what the router holds; _GATEWAY_NET, the first
// three octets of a network the router is on, for the members' routes.
func TestRouterOSLive(t *testing.T) {
	u := os.Getenv("JANUS_ROUTEROS_URL")
	if u == "" {
		t.Skip("no RouterOS router (hack/routeros-hook-test.sh)")
	}
	check, gwNet := os.Getenv("JANUS_ROUTEROS_CHECK_URL"), os.Getenv("JANUS_ROUTEROS_GATEWAY_NET")
	a, _, nodes := clusterApp(t, "lb-1", "lb-2", "lb-3")
	c := newCluster(t, a, nodes, "lb-1", "lb-2", "lb-3")
	ctx := context.Background()
	if err := a.clusters.Create(ctx, c.ID, "lb-1", "admin"); err != nil {
		t.Fatal(err)
	}
	// The members' routes go to addresses on the router's network: the
	// balancer_gateway variable.
	c, _ = a.clusters.Store.Update(c.ID, func(c *clusters.Cluster) error {
		for i := range c.Members {
			c.Members[i].Vars = map[string]string{"balancer_gateway": fmt.Sprintf("%s.%d", gwNet, 21+i)}
		}
		return nil
	})
	comment := clusters.RouterOSComment(c.ID)
	type route struct {
		ID, Dst, Gateway, Distance, Comment, Active, ECMP string
	}
	routes := func() []route {
		t.Helper()
		resp, err := http.Get(check + "/rest/ip/route")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var raw []map[string]string
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			t.Fatal(err)
		}
		var out []route
		for _, r := range raw {
			out = append(out, route{r[".id"], r["dst-address"], r["gateway"], r["distance"], r["comment"], r["active"], r["ecmp"]})
		}
		return out
	}
	// ours are the gateways of the cluster's routes - each active, and
	// in ECMP with the others when there are several.
	ours := func() []string {
		t.Helper()
		var mine []route
		for _, r := range routes() {
			if r.Comment == comment {
				mine = append(mine, r)
			}
		}
		var out []string
		for _, r := range mine {
			if r.Dst != "203.0.113.10/32" || r.Distance != "1" || r.Active != "true" || (len(mine) > 1 && r.ECMP != "true") {
				t.Errorf("a route: %+v", r)
			}
			out = append(out, r.Gateway)
		}
		slices.Sort(out)
		return out
	}
	gw := func(i ...int) []string {
		var out []string
		for _, n := range i {
			out = append(out, fmt.Sprintf("%s.%d", gwNet, 20+n))
		}
		return out
	}
	foreign := func() int {
		n := 0
		for _, r := range routes() {
			if r.Comment == "foreign" {
				n++
			}
		}
		return n
	}
	put := func(body map[string]any) *httpResult {
		rec := call(t, a.handleClusterBalancer, "PUT", "/api/clusters/"+c.ID+"/balancer", "PUT /api/clusters/{id}/balancer", body)
		return &httpResult{rec.Code, rec.Body.String()}
	}
	body := func(fp string) map[string]any {
		return map[string]any{"kind": "routeros", "url": u, "fingerprint": fp, "user": os.Getenv("JANUS_ROUTEROS_USER"),
			"secret": os.Getenv("JANUS_ROUTEROS_PASSWORD"), "prefixes": []string{"203.0.113.10/32"}}
	}
	before := foreign()

	// A wrong certificate: nothing written.
	if r := put(body(strings.Repeat("0", 64))); r.code != http.StatusOK || !strings.Contains(r.body, "not the one pinned") || len(ours()) != 0 {
		t.Fatalf("a wrong fingerprint: %d %s", r.code, r.body)
	}
	// Set: a route per member, active - the router spreads over them.
	if r := put(body(os.Getenv("JANUS_ROUTEROS_FINGERPRINT"))); r.code != http.StatusOK || strings.Contains(r.body, "wasn't told") {
		t.Fatalf("set: %d %s", r.code, r.body)
	}
	if got := ours(); !slices.Equal(got, gw(1, 2, 3)) {
		t.Fatalf("the routes: %v", got)
	}
	t.Logf("set: three routes to 203.0.113.10/32, active, in ECMP")

	// Drain: lb-1's route goes first; undrain: back.
	if err := a.clusters.Drain(ctx, c.ID, "lb-1", "admin"); err != nil {
		t.Fatal(err)
	}
	if got := ours(); !slices.Equal(got, gw(2, 3)) {
		t.Errorf("lb-1 drained: %v", got)
	}
	entries := a.clusters.Store.Entries(c.ID, 20)
	told := slices.IndexFunc(entries, func(e clusters.Entry) bool {
		return strings.HasPrefix(e.Message, "the router was told lb-1 going out of service (routes: 0 added, 1 removed)")
	})
	drained := slices.IndexFunc(entries, func(e clusters.Entry) bool { return strings.HasPrefix(e.Message, "lb-1 drained") })
	if told < 0 || drained < 0 || told < drained {
		t.Errorf("the router told before the drain (journal newest first: told %d, drained %d)", told, drained)
	}
	if err := a.clusters.Undrain(ctx, c.ID, "lb-1", "admin"); err != nil {
		t.Fatal(err)
	}
	if got := ours(); !slices.Equal(got, gw(1, 2, 3)) {
		t.Errorf("lb-1 undrained: %v", got)
	}
	t.Logf("drain: lb-1's route removed before the drain; undrain: back")

	// lb-2's route removed on the router by hand: the check puts it back.
	for _, r := range routes() {
		if r.Comment == comment && r.Gateway == gw(2)[0] {
			req, _ := http.NewRequest(http.MethodDelete, check+"/rest/ip/route/"+r.ID, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
		}
	}
	if got := ours(); !slices.Equal(got, gw(1, 3)) {
		t.Fatalf("lb-2's route deleted by hand: %v", got)
	}
	cur, _ := a.clusters.Store.Get(c.ID)
	a.clusters.Reconcile(ctx, cur)
	if got := ours(); !slices.Equal(got, gw(1, 2, 3)) || !journalSays(t, a, c.ID, "the router's routes put back (1 added, 0 removed)") {
		t.Errorf("after the check: %v", got)
	}
	t.Logf("a route deleted on the router: put back by the check")

	// lb-3 removed from the cluster: its route goes.
	if err := a.clusters.RemoveMember(ctx, c.ID, "lb-3", false, "admin"); err != nil {
		t.Fatal(err)
	}
	if got := ours(); !slices.Equal(got, gw(1, 2)) {
		t.Errorf("lb-3 removed: %v", got)
	}
	if foreign() != before || before == 0 {
		t.Errorf("the foreign routes: %d, were %d", foreign(), before)
	}
	t.Logf("lb-3 removed: its route gone; the router's other routes untouched")

	// check_gateway: the routes made again with check-gateway=ping.
	b := body(os.Getenv("JANUS_ROUTEROS_FINGERPRINT"))
	b["check_gateway"] = true
	delete(b, "secret")
	if r := put(b); r.code != http.StatusOK || strings.Contains(r.body, "wasn't told") {
		t.Fatalf("check_gateway: %d %s", r.code, r.body)
	}
	resp, err := http.Get(check + "/rest/ip/route?comment=" + url.QueryEscape(comment))
	if err != nil {
		t.Fatal(err)
	}
	var raw []map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&raw)
	_ = resp.Body.Close()
	for _, r := range raw {
		if r["check-gateway"] != "ping" {
			t.Errorf("a route without check-gateway=ping: %v", r)
		}
	}
	if len(raw) != 2 {
		t.Errorf("check_gateway: %d routes", len(raw))
	}
	t.Logf("check_gateway: the routes made again with check-gateway=ping")
}

// A read-only RouterOS hook - a cluster whose members announce the
// service prefixes over BGP: it writes no route and removes those it
// wrote before; it reads the routes BGP brought - each member's role per
// prefix, an IPv6 route's link-local gateway matched with the member's
// addresses -, on the cluster's page and on each member's.
func TestClusterRouterReadOnly(t *testing.T) {
	a, f, nodes := clusterApp(t, "lb-1", "lb-2", "lb-3")
	c := newCluster(t, a, nodes, "lb-1", "lb-2", "lb-3")
	ctx := context.Background()
	if err := a.clusters.Create(ctx, c.ID, "lb-1", "admin"); err != nil {
		t.Fatal(err)
	}
	prevRetries := clusters.BalancerRetries
	clusters.BalancerRetries = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { clusters.BalancerRetries = prevRetries })
	f.mu.Lock()
	f.nodes["lb-1"].addrs = []netip.Addr{netip.MustParseAddr("fe80::1")}
	f.nodes["lb-2"].addrs = []netip.Addr{netip.MustParseAddr("fe80::2")}
	f.mu.Unlock()
	comment := clusters.RouterOSComment(c.ID)
	route := func(id, family, dst, gw, active, ecmp, comment string) map[string]string {
		return map[string]string{".id": id, "_family": family, "dst-address": dst, "gateway": gw, "immediate-gw": gw, "routing-table": "main",
			"distance": "20", "active": active, "ecmp": ecmp, "comment": comment}
	}
	router := &fakeRouterOS{f: f, routes: map[string]map[string]string{
		// The hook's own, from before it was read-only.
		"*1": route("*1", "ip", "203.0.113.10/32", "10.0.0.3", "false", "", comment),
		// BGP's: lb-1 and lb-2 share the IPv4 prefix, lb-1 alone carries
		// the IPv6 one (lb-2's route there inactive), lb-3 announces none.
		"*2": route("*2", "ip", "203.0.113.10/32", "10.0.0.1", "true", "true", ""),
		"*3": route("*3", "ip", "203.0.113.10/32", "10.0.0.2%ether1", "true", "true", ""),
		"*4": route("*4", "ipv6", "2001:db8::10/128", "fe80::1%ether1", "true", "", ""),
		"*5": route("*5", "ipv6", "2001:db8::10/128", "fe80::2%ether1", "false", "", ""),
		"*6": route("*6", "ip", "198.51.100.0/24", "10.0.0.9", "true", "", ""),
	}}
	srv := httptest.NewTLSServer(router)
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(srv.Certificate().Raw)
	fp := strings.ToUpper(hex.EncodeToString(sum[:]))
	put := func(body map[string]any) *httpResult {
		rec := call(t, a.handleClusterBalancer, "PUT", "/api/clusters/"+c.ID+"/balancer", "PUT /api/clusters/{id}/balancer", body)
		return &httpResult{rec.Code, rec.Body.String()}
	}
	ros := func(extra map[string]any) map[string]any {
		b := map[string]any{"kind": "routeros", "url": srv.URL, "fingerprint": fp, "user": "janus-hook", "secret": "router-pw",
			"prefixes": []string{"203.0.113.10/32", "2001:db8::10/128"}, "read_only": true}
		for k, v := range extra {
			b[k] = v
		}
		return b
	}
	for _, tc := range []struct {
		body map[string]any
		want string
	}{
		{ros(map[string]any{"distance": 5}), "read_only reads the routes BGP brings"},
		{ros(map[string]any{"check_gateway": true}), "read_only reads the routes BGP brings"},
		{map[string]any{"url": "https://hook.example.net", "read_only": true}, "for a routeros balancer"},
	} {
		if r := put(tc.body); r.code == http.StatusOK || !strings.Contains(r.body, tc.want) {
			t.Errorf("%v: %d %s", tc.body, r.code, r.body)
		}
	}
	if r := put(ros(nil)); r.code != http.StatusOK || !strings.Contains(r.body, `"read_only":true`) {
		t.Fatalf("set: %d %s", r.code, r.body)
	}
	router.mu.Lock()
	_, kept := router.routes["*1"]
	left := len(router.routes)
	router.mu.Unlock()
	if kept || left != 5 {
		t.Errorf("read-only: the hook's former route removed, BGP's untouched - %d routes, *1 kept %v", left, kept)
	}

	rec := call(t, a.handleClusterRouter, "GET", "/api/clusters/"+c.ID+"/router", "GET /api/clusters/{id}/router", nil)
	var view struct {
		Available bool                             `json:"available"`
		ReadOnly  bool                             `json:"read_only"`
		Members   map[string][]clusters.RouterRole `json:"members"`
		Unknown   int                              `json:"unknown"`
		Error     string                           `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil || !view.Available || !view.ReadOnly || view.Error != "" {
		t.Fatalf("the router's view: %d %s", rec.Code, rec.Body.String())
	}
	want := map[string][]clusters.RouterRole{
		"lb-1": {{Prefix: "203.0.113.10/32", Role: "multipath", Active: 2, Total: 2}, {Prefix: "2001:db8::10/128", Role: "master", Active: 1, Total: 2}},
		"lb-2": {{Prefix: "203.0.113.10/32", Role: "multipath", Active: 2, Total: 2}, {Prefix: "2001:db8::10/128", Role: "backup", Active: 1, Total: 2}},
		"lb-3": {{Prefix: "203.0.113.10/32", Role: "absent", Active: 2, Total: 2}, {Prefix: "2001:db8::10/128", Role: "absent", Active: 1, Total: 2}},
	}
	for name, roles := range want {
		if !slices.Equal(view.Members[name], roles) {
			t.Errorf("%s: %+v, want %+v", name, view.Members[name], roles)
		}
	}

	// lb-2's page: its own roles, read from the cache.
	n := router.count()
	got := a.memberRouterView(ctx, nodes["lb-2"])
	if !got.Available || got.ReadAt == nil || !slices.Equal(got.Roles, want["lb-2"]) || router.count() != n {
		t.Errorf("lb-2's page: %+v (router calls %d -> %d)", got, n, router.count())
	}
	// A node in no cluster: nothing.
	if got := a.memberRouterView(ctx, &store.Node{ID: "elsewhere"}); got.Available {
		t.Errorf("a node in no cluster: %+v", got)
	}

	// A drain writes nothing either.
	n = router.count()
	if err := a.clusters.Drain(ctx, c.ID, "lb-1", "admin"); err != nil {
		t.Fatal(err)
	}
	for _, s := range router.since(n) {
		if !strings.HasPrefix(s, "GET ") {
			t.Errorf("a read-only hook wrote at a drain: %s", s)
		}
	}
}
