package logship

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// elasticSink sends to an Elasticsearch cluster's _bulk API: each record
// a document of the data stream logs-janus.<source>-<namespace>, with
// ECS fields - the cluster's built-in logs-*-* template makes the data
// stream, nothing to set up there but an API key that may create
// documents in logs-janus*.
//
// A record with an ID gets a document ID made of it and the node's: a
// batch sent again (a timeout, a 429) is stored once - Elasticsearch
// answers 409 for the copies, taken as stored. Nothing else of the
// document enters it: the same record sent again after the node joined
// or left a cluster is still the same document (the copy stored first
// stays).
type elasticSink struct {
	cfg    ElasticsearchTarget
	env    *env
	client *http.Client
	auth   string

	mu         sync.Mutex
	lastReject time.Time
	peer       peer
}

func newElasticSink(t Target, e *env) (sink, error) {
	es := *t.Elasticsearch
	u, err := url.Parse(es.URL)
	if err != nil {
		return nil, err
	}
	s := &elasticSink{cfg: es, env: e}
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		Proxy: nil,
		// The connection the client opens last: where the batches go.
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := d.DialContext(ctx, network, addr)
			if err == nil {
				s.peer.set(c)
			}
			return c, err
		},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       90 * time.Second,
	}
	if u.Scheme == "https" {
		cfg, err := t.TLS.Config(u.Hostname())
		if err != nil {
			return nil, err
		}
		tr.TLSClientConfig = cfg
	}
	s.client = &http.Client{Transport: tr, Timeout: time.Minute}
	switch {
	case es.APIKey != "":
		key := es.APIKey
		if strings.Contains(key, ":") {
			key = base64.StdEncoding.EncodeToString([]byte(key)) // "id:key", as the API returns them apart
		}
		s.auth = "ApiKey " + key
	case es.Username != "":
		s.auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(es.Username+":"+es.Password))
	}
	return s, nil
}

func (s *elasticSink) Close() { s.client.CloseIdleConnections() }

func (s *elasticSink) Peer() (local, remote string) { return s.peer.get() }

// Send stores recs: an error when any of them may be stored on a later
// try (the cluster busy, unreachable, refusing the key) - the batch is
// sent again whole, the copies answered 409. A document the cluster
// refuses for good (its mapping, a field too long) is dropped, said
// once a minute in janusd's log. The bytes it says are the request's
// body as sent - compressed with gzip.
func (s *elasticSink) Send(ctx context.Context, recs []Record) (int, error) {
	var body bytes.Buffer
	var w io.Writer = &body
	var zw *gzip.Writer
	if s.cfg.Gzip {
		zw = gzip.NewWriter(&body)
		w = zw
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	host, nodeID, labels, cl := s.env.meta.hostname(), s.env.nodeID(), s.env.labels(), s.env.membership()
	for _, r := range recs {
		var action esAction
		action.Create.Index = dataStream(r.Source, s.cfg.Namespace)
		if r.ID != "" {
			action.Create.ID = docID(nodeID, r.ID)
		}
		if err := enc.Encode(&action); err != nil {
			return 0, err
		}
		if err := enc.Encode(s.document(r, host, nodeID, labels, cl)); err != nil {
			return 0, err
		}
	}
	if zw != nil {
		if err := zw.Close(); err != nil {
			return 0, err
		}
	}
	size := body.Len()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.URL+"/_bulk", &body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	if zw != nil {
		req.Header.Set("Content-Encoding", "gzip")
	}
	if s.auth != "" {
		req.Header.Set("Authorization", s.auth)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("elasticsearch: %s: %s", resp.Status, errorReason(data))
	}
	if err := s.bulkResult(data); err != nil {
		return 0, err
	}
	return size, nil
}

// bulkResult reads a _bulk answer: nil when every document is stored
// (or was already), an error when some may be on another try.
func (s *elasticSink) bulkResult(data []byte) error {
	var br struct {
		Errors bool `json:"errors"`
		Items  []map[string]struct {
			Status int `json:"status"`
			Error  *struct {
				Type   string `json:"type"`
				Reason string `json:"reason"`
			} `json:"error"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &br); err != nil {
		return fmt.Errorf("elasticsearch: an answer that isn't a _bulk one: %.200s", data)
	}
	if !br.Errors {
		return nil
	}
	var retry, refused string
	for _, item := range br.Items {
		for _, r := range item {
			switch {
			case r.Status < 300, r.Status == http.StatusConflict: // stored, or stored before
			case r.Status == http.StatusTooManyRequests || r.Status >= 500:
				if retry == "" && r.Error != nil {
					retry = r.Error.Type + ": " + r.Error.Reason
				}
				if retry == "" {
					retry = http.StatusText(r.Status)
				}
			default:
				if refused == "" && r.Error != nil {
					refused = fmt.Sprintf("%d %s: %s", r.Status, r.Error.Type, r.Error.Reason)
				}
			}
		}
	}
	if refused != "" {
		s.mu.Lock()
		say := time.Since(s.lastReject) >= time.Minute
		if say {
			s.lastReject = time.Now()
		}
		s.mu.Unlock()
		if say {
			log.Printf("logship: elasticsearch refused a document for good (dropped): %s", refused)
		}
	}
	if retry != "" {
		return errors.New("elasticsearch: " + retry)
	}
	return nil
}

// errorReason is what an Elasticsearch error answer says.
func errorReason(data []byte) string {
	var e struct {
		Error struct {
			Type   string `json:"type"`
			Reason string `json:"reason"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &e) == nil && e.Error.Reason != "" {
		return e.Error.Type + ": " + e.Error.Reason
	}
	return strings.TrimSpace(string(data[:min(len(data), 200)]))
}

// dataStream is a source's data stream: logs-janus.<source>-<namespace>.
func dataStream(source, namespace string) string {
	return "logs-" + dataset(source) + "-" + namespace
}

// dataset is a source's: janus.<source> (no '-': it separates a data
// stream's parts).
func dataset(source string) string {
	return "janus." + strings.ReplaceAll(source, "-", "_")
}

// docID is a record's document ID: the node's and the record's.
func docID(nodeID, id string) string {
	if len(nodeID) > 16 {
		nodeID = nodeID[:16]
	}
	return nodeID + "-" + id
}

// The _bulk lines, typed: a map of maps per record was built and
// encoded by reflection, its keys sorted, for every access log line. The
// fields keep the order encoding/json gave the maps - sorted - so the
// documents are byte for byte what they were (TestElasticDocumentsAsBefore).
type esAction struct {
	Create struct {
		ID    string `json:"_id,omitempty"`
		Index string `json:"_index"`
	} `json:"create"`
}

type esDocument struct {
	Timestamp    string            `json:"@timestamp"`
	Agent        esAgent           `json:"agent"`
	DataStream   esDataStream      `json:"data_stream"`
	Event        esEvent           `json:"event"`
	HAProxy      json.RawMessage   `json:"haproxy,omitempty"`
	Host         esHost            `json:"host"`
	Janus        map[string]any    `json:"janus"`
	Labels       map[string]string `json:"labels,omitempty"`
	Log          esLog             `json:"log"`
	Message      string            `json:"message"`
	Orchestrator *esOrchestrator   `json:"orchestrator,omitempty"` // a cluster member's
	Service      esService         `json:"service"`
	Source       *esSource         `json:"source,omitempty"`
	User         *esUser           `json:"user,omitempty"`
}

// esOrchestrator is ECS's orchestrator: the cluster a member belongs to,
// "janus" its type (what tells it from a Kubernetes cluster of the same
// name in a data view over both).
type esOrchestrator struct {
	Cluster esCluster `json:"cluster"`
	Type    string    `json:"type"`
}

type esCluster struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

type esAgent struct {
	Type    string `json:"type"`
	Version string `json:"version"`
}

type esDataStream struct {
	Dataset   string `json:"dataset"`
	Namespace string `json:"namespace"`
	Type      string `json:"type"`
}

type esEvent struct {
	Action   any      `json:"action,omitempty"`
	Category []string `json:"category,omitempty"`
	Dataset  string   `json:"dataset"`
	Duration *int64   `json:"duration,omitempty"`
	Module   string   `json:"module"`
	Outcome  string   `json:"outcome,omitempty"`
	Sequence any      `json:"sequence,omitempty"`
}

type esHost struct {
	Hostname string `json:"hostname"`
	ID       string `json:"id,omitempty"`
	Name     string `json:"name"`
}

type esLog struct {
	Level  string   `json:"level"`
	Syslog esSyslog `json:"syslog"`
}

type esSyslog struct {
	Severity esSeverity `json:"severity"`
}

type esSeverity struct {
	Code int    `json:"code"`
	Name string `json:"name"`
}

type esService struct {
	Name string `json:"name"`
}

type esSource struct {
	Address string `json:"address"`
	IP      string `json:"ip,omitempty"`
}

type esUser struct {
	Name  any `json:"name"`
	Roles any `json:"roles,omitempty"`
}

// document is r as an ECS document; cl the node's cluster, the zero
// value for none.
func (s *elasticSink) document(r Record, host, nodeID string, labels map[string]string, cl Cluster) *esDocument {
	sev := clampSeverity(r.Severity)
	ds := dataset(r.Source)
	doc := &esDocument{
		Timestamp:  r.Time.UTC().Format(time.RFC3339Nano),
		Message:    r.Message,
		Log:        esLog{Level: SeverityName(sev), Syslog: esSyslog{Severity: esSeverity{Code: sev, Name: SeverityName(sev)}}},
		Host:       esHost{Name: host, Hostname: host, ID: nodeID},
		Service:    esService{Name: r.Service},
		Event:      esEvent{Dataset: ds, Module: "janus"},
		DataStream: esDataStream{Type: "logs", Dataset: ds, Namespace: s.cfg.Namespace},
		Agent:      esAgent{Type: s.env.meta.Agent, Version: s.env.meta.AgentVersion},
	}
	janus := make(map[string]any, 4+len(r.Fields))
	janus["source"] = r.Source
	if nodeID != "" {
		janus["node_id"] = nodeID
	}
	if s.env.meta.Slot != "" {
		janus["slot"] = s.env.meta.Slot
	}
	if s.env.meta.Schematic != "" {
		janus["schematic"] = s.env.meta.Schematic
	}
	if cl != (Cluster{}) {
		// The cluster in ECS's orchestrator.cluster - id and name -, the
		// member name, which ECS has no field for, under janus.cluster.
		doc.Orchestrator = &esOrchestrator{Type: "janus", Cluster: esCluster{ID: cl.ID, Name: cl.Name}}
		if cl.Member != "" {
			janus["cluster"] = map[string]any{"member": cl.Member}
		}
	}
	doc.Janus = janus
	if len(labels) > 0 {
		doc.Labels = labels
	}
	for k, v := range r.Fields {
		switch k {
		case "event":
			// The event log's type and ID: ECS's event.action and
			// event.sequence.
			if m, ok := v.(map[string]any); ok {
				if t, ok := m["type"]; ok {
					doc.Event.Action = t
				}
				if id, ok := m["id"]; ok {
					doc.Event.Sequence = id
				}
			}
		case "audit":
			// Who, from where, how it ended: ECS's user, source and
			// event.outcome - and the whole entry under janus.audit.
			if m, ok := v.(map[string]any); ok {
				user := &esUser{Name: m["caller"]}
				if roles, ok := m["roles"]; ok {
					user.Roles = roles
				}
				doc.User = user
				if p, ok := m["peer"].(string); ok && p != "" {
					src := &esSource{Address: p}
					if host, _, err := net.SplitHostPort(p); err == nil {
						src.IP = host
					}
					doc.Source = src
				}
				doc.Event.Outcome = "success"
				if m["code"] != "OK" {
					doc.Event.Outcome = "failure"
				}
				if ms, ok := m["duration_ms"].(float64); ok {
					d := int64(ms * 1e6)
					doc.Event.Duration = &d
				}
				doc.Event.Category = []string{"configuration"}
				janus["audit"] = m
			}
		case "payload":
			// As text: payloads differ by event type, a field's type
			// across them would clash in the data stream's mapping.
			if b, err := json.Marshal(v); err == nil {
				janus["payload"] = string(b)
			}
		default:
			janus[k] = v
		}
	}
	if r.Source == SourceAccess && strings.HasPrefix(r.Message, "{") && json.Valid([]byte(r.Message)) {
		// An access log HAProxy formats as JSON (%{+json}o): its
		// fields, as HAProxy wrote them - not decoded into a map and
		// encoded again (the keys came out sorted then; the cluster
		// doesn't mind the order).
		doc.HAProxy = json.RawMessage(r.Message)
	}
	return doc
}
