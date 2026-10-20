package logship

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// bulkServer is a stand-in _bulk endpoint: it keeps what it was sent
// and answers each document with the next status of answer (201 when
// none is left).
type bulkServer struct {
	t      *testing.T
	mu     sync.Mutex
	auth   []string
	docs   []map[string]any
	ids    []string
	index  []string
	answer []int
	gzip   bool
	length []int64 // each request's body, as it came
}

func (b *bulkServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/_bulk" || r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-ndjson" {
		http.Error(w, `{"error":{"type":"bad","reason":"not a bulk"}}`, http.StatusBadRequest)
		return
	}
	var body io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			b.t.Error(err)
			return
		}
		body = zr
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.auth = append(b.auth, r.Header.Get("Authorization"))
	b.gzip = b.gzip || r.Header.Get("Content-Encoding") == "gzip"
	b.length = append(b.length, r.ContentLength)
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var items []map[string]any
	anyError := false
	for sc.Scan() {
		var action map[string]map[string]any
		if err := json.Unmarshal(sc.Bytes(), &action); err != nil {
			b.t.Errorf("action line %q: %v", sc.Text(), err)
			return
		}
		if !sc.Scan() {
			b.t.Error("an action without its document")
			return
		}
		var doc map[string]any
		if err := json.Unmarshal(sc.Bytes(), &doc); err != nil {
			b.t.Errorf("document %q: %v", sc.Text(), err)
			return
		}
		status := 201
		if len(b.answer) > 0 {
			status, b.answer = b.answer[0], b.answer[1:]
		}
		item := map[string]any{"status": status}
		switch status {
		case 201:
			b.docs = append(b.docs, doc)
			id, _ := action["create"]["_id"].(string)
			b.ids = append(b.ids, id)
			b.index = append(b.index, action["create"]["_index"].(string))
		case 409:
			item["error"] = map[string]any{"type": "version_conflict_engine_exception", "reason": "document already exists"}
		case 429:
			item["error"] = map[string]any{"type": "es_rejected_execution_exception", "reason": "rejected execution"}
		default:
			item["error"] = map[string]any{"type": "document_parsing_exception", "reason": "failed to parse"}
		}
		if status >= 300 {
			anyError = true
		}
		items = append(items, map[string]any{"create": item})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": anyError, "items": items})
}

func esTarget(url string, es ElasticsearchTarget) Target {
	es.URL = url
	return Target{Name: "es", Elasticsearch: &es}
}

func newESSink(t *testing.T, tg Target, labels map[string]string) *elasticSink {
	t.Helper()
	cfg := Config{Targets: []Target{tg}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	e := &env{
		meta:   &Meta{Hostname: func() string { return "lb1" }, Agent: "janusd", AgentVersion: "v2026.10.16", Slot: "A", Schematic: "a055fbb6"},
		labels: func() map[string]string { return labels },
		nodeID: func() string { return "f55632ea6473b530ee44c09feb798d3b672273202eaee28994ed8cd943c4347e" },
	}
	sk, err := newElasticSink(cfg.Targets[0], e)
	if err != nil {
		t.Fatal(err)
	}
	return sk.(*elasticSink)
}

func TestElasticDocuments(t *testing.T) {
	srv := &bulkServer{t: t}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	sk := newESSink(t, esTarget(ts.URL, ElasticsearchTarget{APIKey: "VuaCfGcBCdbkQm-e5aOx:ui2lp2axTNmsyakw9tvNnw", Gzip: true}), map[string]string{"env": "prod"})
	at := time.Date(2026, 10, 16, 8, 0, 0, 123456789, time.UTC)
	n, err := sk.Send(context.Background(), []Record{
		{Time: at, Source: SourceJanusd, Service: "janusd", Severity: SevNotice, Message: "api: SystemService/Reboot: admin (os:admin)", ID: "0011223344556677-42"},
		EventRecord(7, at, "haproxy.config.applied", []byte(`{"bytes":512,"by":"admin"}`), "janusd"),
		{Time: at, Source: SourceAccess, Service: "haproxy", Severity: SevInfo, Message: `{"client_ip":"10.0.2.2","status":200}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !srv.gzip {
		t.Error("not gzipped")
	}
	// The bytes said are the body as sent, compressed; the addresses the
	// connection's.
	if len(srv.length) != 1 || int64(n) != srv.length[0] || n == 0 {
		t.Errorf("Send said %d bytes, the server got %v", n, srv.length)
	}
	if local, remote := sk.Peer(); remote != ts.Listener.Addr().String() || !strings.HasPrefix(local, "127.0.0.1:") {
		t.Errorf("peer %q -> %q, want -> %s", local, remote, ts.Listener.Addr())
	}
	if want := "ApiKey " + base64.StdEncoding.EncodeToString([]byte("VuaCfGcBCdbkQm-e5aOx:ui2lp2axTNmsyakw9tvNnw")); srv.auth[0] != want {
		t.Errorf("Authorization = %q", srv.auth[0])
	}
	if strings.Join(srv.index, " ") != "logs-janus.janusd-default logs-janus.events-default logs-janus.access-default" {
		t.Errorf("indices = %q", srv.index)
	}
	if srv.ids[0] != "f55632ea6473b530-0011223344556677-42" || srv.ids[2] != "" {
		t.Errorf("ids = %q", srv.ids)
	}
	d := srv.docs[0]
	get := func(doc map[string]any, path string) any {
		var v any = doc
		for _, k := range strings.Split(path, ".") {
			m, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			v = m[k]
		}
		return v
	}
	for path, want := range map[string]any{
		"@timestamp": "2026-10-16T08:00:00.123456789Z", "message": "api: SystemService/Reboot: admin (os:admin)",
		"log.level": "notice", "log.syslog.severity.code": 5.0, "host.name": "lb1", "host.id": "f55632ea6473b530ee44c09feb798d3b672273202eaee28994ed8cd943c4347e",
		"service.name": "janusd", "event.dataset": "janus.janusd", "data_stream.namespace": "default", "agent.version": "v2026.10.16",
		"janus.slot": "A", "janus.schematic": "a055fbb6", "labels.env": "prod",
	} {
		if got := get(d, path); got != want {
			t.Errorf("janusd document %s = %v, want %v", path, got, want)
		}
	}
	ev := srv.docs[1]
	if get(ev, "event.action") != "haproxy.config.applied" || get(ev, "event.sequence") != 7.0 || get(ev, "janus.payload") != `{"by":"admin","bytes":512}` {
		t.Errorf("event document: %v", ev)
	}
	if get(srv.docs[2], "haproxy.status") != 200.0 {
		t.Errorf("an access log in JSON isn't parsed: %v", srv.docs[2])
	}
}

func TestElasticRetriesAndRefusals(t *testing.T) {
	srv := &bulkServer{t: t}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	sk := newESSink(t, esTarget(ts.URL, ElasticsearchTarget{Username: "janus", Password: "pw"}), nil)
	recs := []Record{{Time: time.Now(), Source: SourceJanusd, Message: "one", ID: "1"}, {Time: time.Now(), Source: SourceJanusd, Message: "two", ID: "2"}}

	// The cluster busy with one of them: an error, the batch sent again;
	// the one stored the first time answers 409 - stored, not an error.
	srv.answer = []int{201, 429}
	if _, err := sk.Send(context.Background(), recs); err == nil || !strings.Contains(err.Error(), "rejected execution") {
		t.Fatalf("a 429 = %v", err)
	}
	srv.answer = []int{409, 201}
	if _, err := sk.Send(context.Background(), recs); err != nil {
		t.Fatalf("the batch sent again: %v", err)
	}
	if len(srv.docs) != 2 || srv.auth[0] != "Basic "+base64.StdEncoding.EncodeToString([]byte("janus:pw")) {
		t.Fatalf("stored %d, auth %q", len(srv.docs), srv.auth[0])
	}
	// A document refused for good is dropped: no error, no retry.
	srv.answer = []int{400}
	if _, err := sk.Send(context.Background(), recs[:1]); err != nil {
		t.Fatalf("a 400 = %v", err)
	}
}

func TestElasticErrors(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"type":"security_exception","reason":"unable to authenticate with provided credentials"}}`)
	}))
	defer ts.Close()
	sk := newESSink(t, esTarget(ts.URL, ElasticsearchTarget{APIKey: "bad"}), nil)
	_, err := sk.Send(context.Background(), []Record{{Time: time.Now(), Source: SourceJanusd, Message: "x"}})
	if err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "unable to authenticate") {
		t.Fatalf("a 401 = %v", err)
	}
}

// Through the shipper: records offered from memory get an ID, so a
// batch sent again is stored once.
func TestShipperToElastic(t *testing.T) {
	srv := &bulkServer{t: t}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	cfg := Config{Targets: []Target{esTarget(ts.URL, ElasticsearchTarget{})}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	sh := New(Options{Meta: Meta{Agent: "janusd"}})
	defer sh.Close()
	sh.SetNodeID("abcdef0123456789abcdef")
	sh.Apply(cfg)
	sh.Offer(Record{Time: time.Now(), Source: SourceJanusd, Service: "janusd", Message: "hello"})
	deadline := time.Now().Add(5 * time.Second)
	for {
		srv.mu.Lock()
		n := len(srv.docs)
		var id string
		if n > 0 {
			id = srv.ids[0]
		}
		srv.mu.Unlock()
		if n == 1 {
			if !strings.HasPrefix(id, "abcdef0123456789-m") {
				t.Fatalf("id %q", id)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("nothing stored")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestElasticAuditDocument(t *testing.T) {
	srv := &bulkServer{t: t}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	sk := newESSink(t, esTarget(ts.URL, ElasticsearchTarget{}), nil)
	r := AuditRecord(3, time.Now(), "SystemService/Reboot", []byte(`{"caller":"rita","roles":["os:reader"],"peer":"10.0.2.2:41234","code":"PermissionDenied","error":"requires os:admin","denied":true,"duration_ms":0}`))
	if r.Severity != SevWarning || !strings.Contains(r.Message, "SystemService/Reboot by rita (os:reader) from 10.0.2.2:41234: refused: requires os:admin") {
		t.Fatalf("record = %+v", r)
	}
	if _, err := sk.Send(context.Background(), []Record{r}); err != nil {
		t.Fatal(err)
	}
	d := srv.docs[0]
	user, _ := d["user"].(map[string]any)
	src, _ := d["source"].(map[string]any)
	ev, _ := d["event"].(map[string]any)
	if user["name"] != "rita" || src["ip"] != "10.0.2.2" || ev["outcome"] != "failure" || ev["action"] != "SystemService/Reboot" || srv.index[0] != "logs-janus.audit-default" {
		t.Fatalf("audit document: %v in %s", d, srv.index[0])
	}
}

// A batch of 500 records of the three kinds a node sends most - janusd
// lines, access logs HAProxy formats as JSON, events - built and sent
// to a cluster that stores them all.
func BenchmarkElasticSend(b *testing.B) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte(`{"errors":false,"items":[]}`))
	}))
	defer ts.Close()
	cfg := Config{Targets: []Target{esTarget(ts.URL, ElasticsearchTarget{APIKey: "VuaCfGcBCdbkQm-e5aOx:ui2lp2axTNmsyakw9tvNnw"})}}
	if err := cfg.Validate(); err != nil {
		b.Fatal(err)
	}
	e := &env{
		meta:   &Meta{Hostname: func() string { return "lb1" }, Agent: "janusd", AgentVersion: "v2026.10.20", Slot: "A", Schematic: "a055fbb6"},
		labels: func() map[string]string { return map[string]string{"env": "prod", "site": "par"} },
		nodeID: func() string { return "f55632ea6473b530ee44c09feb798d3b672273202eaee28994ed8cd943c4347e" },
	}
	sk, err := newElasticSink(cfg.Targets[0], e)
	if err != nil {
		b.Fatal(err)
	}
	at := time.Date(2026, 10, 20, 8, 0, 0, 123456789, time.UTC)
	recs := make([]Record, 0, 500)
	for i := 0; len(recs) < 500; i++ {
		recs = append(recs,
			Record{Time: at, Source: SourceJanusd, Service: "janusd", Severity: SevNotice, Message: "api: HAProxyService/ShowInfo: admin (os:admin)", ID: fmt.Sprintf("0011223344556677-%d", i)},
			Record{Time: at, Source: SourceAccess, Service: "haproxy", Severity: SevInfo, Message: `{"client_ip":"10.200.40.7","client_port":40000,"request_date":"15/Oct/2026:21:30:01.123","frontend_name":"web","backend_name":"web","server_name":"srv1","status_code":200,"bytes_read":1234,"termination_state":"----","http_method":"GET","http_uri":"/api/x","http_version":"HTTP/1.1"}`, ID: fmt.Sprintf("0011223344556677-a%d", i)},
			EventRecord(uint64(i), at, "haproxy.config.applied", []byte(`{"bytes":512,"by":"admin"}`), "janusd"),
		)
	}
	recs = recs[:500]
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := sk.Send(context.Background(), recs); err != nil {
			b.Fatal(err)
		}
	}
}
