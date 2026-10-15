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
// answers 409 for the copies, taken as stored.
type elasticSink struct {
	cfg    ElasticsearchTarget
	env    *env
	client *http.Client
	auth   string

	mu         sync.Mutex
	lastReject time.Time
}

func newElasticSink(t Target, e *env) (sink, error) {
	es := *t.Elasticsearch
	u, err := url.Parse(es.URL)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
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
	s := &elasticSink{cfg: es, env: e, client: &http.Client{Transport: tr, Timeout: time.Minute}}
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

// Send stores recs: an error when any of them may be stored on a later
// try (the cluster busy, unreachable, refusing the key) - the batch is
// sent again whole, the copies answered 409. A document the cluster
// refuses for good (its mapping, a field too long) is dropped, said
// once a minute in janusd's log.
func (s *elasticSink) Send(ctx context.Context, recs []Record) error {
	var body bytes.Buffer
	var w io.Writer = &body
	var zw *gzip.Writer
	if s.cfg.Gzip {
		zw = gzip.NewWriter(&body)
		w = zw
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	host, nodeID, labels := s.env.meta.hostname(), s.env.nodeID(), s.env.labels()
	for _, r := range recs {
		action := map[string]any{"_index": dataStream(r.Source, s.cfg.Namespace)}
		if r.ID != "" {
			action["_id"] = docID(nodeID, r.ID)
		}
		if err := enc.Encode(map[string]any{"create": action}); err != nil {
			return err
		}
		if err := enc.Encode(s.document(r, host, nodeID, labels)); err != nil {
			return err
		}
	}
	if zw != nil {
		if err := zw.Close(); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.URL+"/_bulk", &body)
	if err != nil {
		return err
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
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("elasticsearch: %s: %s", resp.Status, errorReason(data))
	}
	return s.bulkResult(data)
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

// document is r as an ECS document.
func (s *elasticSink) document(r Record, host, nodeID string, labels map[string]string) map[string]any {
	sev := clampSeverity(r.Severity)
	doc := map[string]any{
		"@timestamp": r.Time.UTC().Format(time.RFC3339Nano),
		"message":    r.Message,
		"log": map[string]any{
			"level":  SeverityName(sev),
			"syslog": map[string]any{"severity": map[string]any{"code": sev, "name": SeverityName(sev)}},
		},
		"host":        map[string]any{"name": host, "hostname": host},
		"service":     map[string]any{"name": r.Service},
		"event":       map[string]any{"dataset": dataset(r.Source), "module": "janus"},
		"data_stream": map[string]any{"type": "logs", "dataset": dataset(r.Source), "namespace": s.cfg.Namespace},
		"agent":       map[string]any{"type": s.env.meta.Agent, "version": s.env.meta.AgentVersion},
	}
	janus := map[string]any{"source": r.Source}
	if nodeID != "" {
		janus["node_id"] = nodeID
		doc["host"].(map[string]any)["id"] = nodeID
	}
	if s.env.meta.Slot != "" {
		janus["slot"] = s.env.meta.Slot
	}
	if s.env.meta.Schematic != "" {
		janus["schematic"] = s.env.meta.Schematic
	}
	doc["janus"] = janus
	if len(labels) > 0 {
		doc["labels"] = labels
	}
	for k, v := range r.Fields {
		switch k {
		case "event":
			// The event log's type and ID: ECS's event.action and
			// event.sequence.
			if m, ok := v.(map[string]any); ok {
				ev := doc["event"].(map[string]any)
				if t, ok := m["type"]; ok {
					ev["action"] = t
				}
				if id, ok := m["id"]; ok {
					ev["sequence"] = id
				}
			}
		case "audit":
			// Who, from where, how it ended: ECS's user, source and
			// event.outcome - and the whole entry under janus.audit.
			if m, ok := v.(map[string]any); ok {
				user := map[string]any{"name": m["caller"]}
				if roles, ok := m["roles"]; ok {
					user["roles"] = roles
				}
				doc["user"] = user
				if p, ok := m["peer"].(string); ok && p != "" {
					src := map[string]any{"address": p}
					if host, _, err := net.SplitHostPort(p); err == nil {
						src["ip"] = host
					}
					doc["source"] = src
				}
				ev := doc["event"].(map[string]any)
				ev["outcome"] = "success"
				if m["code"] != "OK" {
					ev["outcome"] = "failure"
				}
				if ms, ok := m["duration_ms"].(float64); ok {
					ev["duration"] = int64(ms * 1e6)
				}
				ev["category"] = []string{"configuration"}
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
	if r.Source == SourceAccess && strings.HasPrefix(r.Message, "{") {
		// An access log HAProxy formats as JSON (%{+json}o): its fields.
		var fields map[string]any
		if json.Unmarshal([]byte(r.Message), &fields) == nil {
			doc["haproxy"] = fields
		}
	}
	return doc
}
