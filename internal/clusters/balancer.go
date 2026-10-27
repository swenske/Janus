package clusters

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// A balancer in front (docs/cluster.md section 6, "An external L4
// balancer"): a cloud load balancer, a router's ECMP routes, anything
// that spreads the traffic over the members and must know which ones
// take it. The cluster tells it through a hook - an HTTPS (or HTTP) POST
// of every member and whether it is in service - when a member comes
// into service (it joined, was undrained, took a replaced member's
// place) and **before** a member goes out of it (drained, removed,
// replaced), the operation waiting for the balancer's 2xx - and its
// drain_delay - before the member is touched. Each call carries the
// whole list: a receiver makes its targets the members in service, and
// a call it missed is made good by the next. Reconcile calls again when
// the list differs from the last one delivered, or that call failed.
// Signed with HMAC-SHA256 when a secret is set (X-Janus-Signature-256,
// as GitHub's webhooks). Or a RouterOS router kept up to date by the
// engine itself (routeros.go): ECMP routes to the members in service.

// The balancer kinds.
const (
	BalancerWebhook  = "webhook"
	BalancerRouterOS = "routeros"
)

// Balancer is a cluster's hook.
type Balancer struct {
	// Kind: BalancerWebhook ("" too) or BalancerRouterOS.
	Kind string `json:"kind,omitempty"`
	// URL: the webhook; a RouterOS router's own (https://router).
	URL string `json:"url"`
	// CACert, PEM: the only CA the hook's HTTPS certificate may chain
	// to; empty: the system's.
	CACert string `json:"ca_cert,omitempty"`
	// Fingerprint pins the HTTPS certificate itself, its SHA-256 (hex,
	// colons allowed) - a router's own self-signed one.
	Fingerprint string `json:"fingerprint,omitempty"`
	// RouterOS: the router's user the engine signs in as (its password
	// is the secret), the service prefixes the members carry (a /32 each,
	// as for BGP anycast), the routes' distance (1) and routing table
	// (main); AllowHTTP lets a lab's router answer over plain HTTP.
	User         string   `json:"user,omitempty"`
	Prefixes     []string `json:"prefixes,omitempty"`
	Distance     int      `json:"distance,omitempty"`
	RoutingTable string   `json:"routing_table,omitempty"`
	AllowHTTP    bool     `json:"allow_http,omitempty"`
	// CheckGateway: the routes check their member with a ping
	// (check-gateway=ping) - one that stops answering is left out by
	// the router itself, before any drain or check.
	CheckGateway bool `json:"check_gateway,omitempty"`
	// ReadOnly (RouterOS): the routes to the prefixes come from elsewhere
	// - the members' BGP announcements -, the engine writes none: it reads
	// them, each member's role on the router (RouterRoles), shown on the
	// members' pages. Distance and check_gateway are for routes it writes.
	ReadOnly bool `json:"read_only,omitempty"`
	// DrainDelaySeconds: how long a member stays in service after the
	// balancer was told it goes - its connections finishing - before it
	// is drained or removed.
	DrainDelaySeconds int `json:"drain_delay_seconds,omitempty"`
	// HasSecret: calls are signed; the secret is kept apart, sealed.
	HasSecret bool `json:"has_secret,omitempty"`
}

// BalancerState is what the cluster knows of its last call.
type BalancerState struct {
	// Sent is the members list (BalancerList's hash) last delivered.
	Sent   string    `json:"sent,omitempty"`
	SentAt time.Time `json:"sent_at,omitzero"`
	// Error is the last call's failure, empty once one succeeds.
	Error string `json:"error,omitempty"`
}

// IsRouterOS reports whether b is a RouterOS router the engine keeps.
func (b *Balancer) IsRouterOS() bool { return b != nil && b.Kind == BalancerRouterOS }

// noun is what b is, in the journal's words.
func (b *Balancer) noun() string {
	if b.IsRouterOS() {
		return "router"
	}
	return "balancer"
}

// Validate checks b.
func (b *Balancer) Validate() error {
	u, err := url.Parse(b.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("balancer url %q: an http(s) URL", b.URL)
	}
	if u.User != nil {
		return errors.New("balancer url: no credentials in the URL - set a secret, the calls are signed with it")
	}
	switch b.Kind {
	case "", BalancerWebhook:
		if b.User != "" || len(b.Prefixes) > 0 || b.Distance != 0 || b.RoutingTable != "" || b.AllowHTTP || b.CheckGateway || b.ReadOnly {
			return errors.New("balancer user, prefixes, distance, routing_table, allow_http, check_gateway, read_only: for a routeros balancer")
		}
	case BalancerRouterOS:
		if err := b.validateRouterOS(u); err != nil {
			return err
		}
	default:
		return fmt.Errorf("balancer kind %q: webhook or routeros", b.Kind)
	}
	if b.Fingerprint != "" {
		if u.Scheme != "https" {
			return errors.New("balancer fingerprint: for an https URL")
		}
		if len(normalFingerprint(b.Fingerprint)) != 64 {
			return errors.New("balancer fingerprint: the certificate's SHA-256, 64 hexadecimal digits (colons allowed)")
		}
	}
	if b.CACert != "" {
		if u.Scheme != "https" {
			return errors.New("balancer ca_cert: for an https URL")
		}
		if !x509.NewCertPool().AppendCertsFromPEM([]byte(b.CACert)) {
			return errors.New("balancer ca_cert: no PEM certificate in it")
		}
	}
	if b.DrainDelaySeconds < 0 || b.DrainDelaySeconds > 3600 {
		return errors.New("balancer drain_delay_seconds: 0 to 3600")
	}
	return nil
}

// BalancerCall is what the hook receives.
type BalancerCall struct {
	ClusterID string `json:"cluster_id"`
	Cluster   string `json:"cluster"`
	// Event: "in" (Member came into service), "out" (Member is about to
	// go out of it - drain it from the balancer now), "sync" (the list
	// again: settings saved, a call missed, a member drained or
	// undrained elsewhere).
	Event   string           `json:"event"`
	Member  string           `json:"member,omitempty"`
	Members []BalancerMember `json:"members"`
	At      time.Time        `json:"at"`
}

// BalancerMember is a member as the balancer sees it: Address is the
// one its peers reach it on; Vars its template variables - a
// public_address of your own, a weight.
type BalancerMember struct {
	Name      string            `json:"name"`
	Address   string            `json:"address"`
	InService bool              `json:"in_service"`
	Vars      map[string]string `json:"vars,omitempty"`
}

// BalancerList is cluster c's members for its balancer, out: those
// out of service whatever the state says (one about to be drained).
func (e *Engine) BalancerList(c *Cluster, out ...string) []BalancerMember {
	st := e.Store.State(c.ID)
	list := []BalancerMember{}
	for _, m := range c.Members {
		in := !st.Members[m.Name].Drained
		for _, o := range out {
			if o == m.Name {
				in = false
			}
		}
		list = append(list, BalancerMember{Name: m.Name, Address: m.Address, InService: in, Vars: cloneVars(m.Vars)})
	}
	return list
}

func listHash(list []BalancerMember) string {
	b, _ := json.Marshal(list)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// setDrained records member name as drained or not - what the engine
// just did, before Reconcile reads it from the node.
func (e *Engine) setDrained(id, name string, drained bool) {
	_ = e.Store.UpdateState(id, func(s *State) {
		ms := s.Members[name]
		ms.Drained = drained
		s.Members[name] = ms
	})
}

// announce tells cluster id's balancer, when it has one, event about
// member (out: the members to show out of service). It journals the
// call, records its outcome, and returns its failure.
func (e *Engine) announce(ctx context.Context, id, event, member, actor string, out ...string) error {
	c, ok := e.Store.Get(id)
	if !ok || c.Balancer == nil {
		return nil
	}
	list := e.BalancerList(c, out...)
	detail, err := e.callBalancer(ctx, c, BalancerCall{ClusterID: c.ID, Cluster: c.Name, Event: event, Member: member, Members: list, At: time.Now().UTC()})
	_ = e.Store.UpdateState(id, func(s *State) {
		if err != nil {
			s.Balancer.Error = err.Error()
			return
		}
		s.Balancer = BalancerState{Sent: listHash(list), SentAt: time.Now().UTC()}
	})
	what := map[string]string{"in": member + " in service", "out": member + " going out of service", "sync": "the members"}[event]
	in := 0
	for _, m := range list {
		if m.InService {
			in++
		}
	}
	noun := c.Balancer.noun()
	if err != nil {
		e.Journal(id, actor, "balancer", member, 0, "the %s wasn't told %s: %v", noun, what, err)
		return fmt.Errorf("the %s wasn't told %s: %w", noun, what, err)
	}
	e.Journal(id, actor, "balancer", member, 0, "the %s was told %s%s - %d of %d member(s) in service", noun, what, detail, in, len(list))
	return nil
}

// leaving tells the balancer member goes out of service, then waits its
// drain delay; force: a failure doesn't stop the operation (a node gone
// for good), it is journaled.
func (e *Engine) leaving(ctx context.Context, id, member string, force bool, actor string) error {
	c, ok := e.Store.Get(id)
	if !ok || c.Balancer == nil {
		return nil
	}
	if err := e.announce(ctx, id, "out", member, actor, member); err != nil {
		if force {
			return nil
		}
		fix := "the hook"
		if c.Balancer.IsRouterOS() {
			fix = "the router's access"
		}
		return fmt.Errorf("%v - %s left in service: fix %s, or remove it with force", err, member, fix)
	}
	if d := time.Duration(c.Balancer.DrainDelaySeconds) * time.Second; d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// SyncBalancer calls cluster id's balancer with its members now.
func (e *Engine) SyncBalancer(ctx context.Context, id, actor string) error {
	return e.announce(ctx, id, "sync", "", actor)
}

// reconcileBalancer calls the balancer again when the list changed
// since the last call delivered, or that call failed - a router, every
// time: its routes are read and put back if changed there.
func (e *Engine) reconcileBalancer(ctx context.Context, c *Cluster) error {
	if c.Balancer == nil {
		return nil
	}
	if c.Balancer.IsRouterOS() {
		return e.reconcileRouterOS(ctx, c)
	}
	st := e.Store.State(c.ID)
	if st.Balancer.Error == "" && st.Balancer.Sent == listHash(e.BalancerList(c)) {
		return nil
	}
	return e.announce(ctx, c.ID, "sync", "", Actor)
}

// normalFingerprint is a SHA-256 fingerprint as lowercase hex, its
// colons and spaces gone.
func normalFingerprint(s string) string {
	s = strings.ToLower(strings.NewReplacer(":", "", " ", "").Replace(strings.TrimSpace(s)))
	if _, err := hex.DecodeString(s); err != nil {
		return ""
	}
	return s
}

// BalancerClient makes the hook's HTTP calls; tests replace it.
var BalancerClient = func(b *Balancer) (*http.Client, error) {
	t := http.DefaultTransport.(*http.Transport).Clone()
	if b.CACert != "" {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM([]byte(b.CACert))
		t.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	if want := normalFingerprint(b.Fingerprint); want != "" {
		t.TLSClientConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			// The chain isn't checked: the certificate itself is, by its
			// pinned fingerprint, below.
			InsecureSkipVerify: true, //nolint:gosec // G402: VerifyConnection checks the pinned SHA-256
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 {
					return errors.New("no certificate")
				}
				sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
				if got := hex.EncodeToString(sum[:]); got != want {
					return fmt.Errorf("its certificate is SHA-256 %s, not the one pinned (%s)", got, want)
				}
				return nil
			},
		}
	}
	return &http.Client{Transport: t, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse // a hook answers, it doesn't send elsewhere
	}}, nil
}

// BalancerRetries are the waits between a failed call's attempts.
var BalancerRetries = []time.Duration{time.Second, 3 * time.Second}

// callBalancer delivers call: a POST to the webhook, or the router's
// routes made to match its members; detail says what changed there.
func (e *Engine) callBalancer(ctx context.Context, c *Cluster, call BalancerCall) (string, error) {
	if c.Balancer.IsRouterOS() {
		added, removed, err := e.routerOSApply(ctx, c, call.Members)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf(" (routes: %d added, %d removed)", added, removed), nil
	}
	return "", e.postBalancer(ctx, c, call)
}

func (e *Engine) postBalancer(ctx context.Context, c *Cluster, call BalancerCall) error {
	body, err := json.Marshal(call)
	if err != nil {
		return err
	}
	secret, err := e.Store.BalancerSecret(c.ID)
	if err != nil {
		return err
	}
	client, err := BalancerClient(c.Balancer)
	if err != nil {
		return err
	}
	var last error
	for attempt := 0; ; attempt++ {
		last = postHook(ctx, client, c.Balancer.URL, call.Event, body, secret)
		var final *finalError
		if last == nil || errors.As(last, &final) || attempt >= len(BalancerRetries) {
			break
		}
		select {
		case <-time.After(BalancerRetries[attempt]):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	var final *finalError
	if errors.As(last, &final) {
		return final.err
	}
	return last
}

// finalError: the hook refused the call (4xx) - no point trying again.
type finalError struct{ err error }

func (f *finalError) Error() string { return f.err.Error() }

func postHook(ctx context.Context, client *http.Client, u, event string, body, secret []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return &finalError{err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Janus-cluster-balancer/1")
	req.Header.Set("X-Janus-Event", event)
	if len(secret) > 0 {
		req.Header.Set("X-Janus-Signature-256", Signature(secret, body))
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode/100 == 2 {
		return nil
	}
	err = fmt.Errorf("%s answered %s", u, resp.Status)
	if s := strings.TrimSpace(string(answer)); s != "" {
		err = fmt.Errorf("%v: %s", err, s)
	}
	if resp.StatusCode/100 == 4 && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusRequestTimeout {
		return &finalError{err}
	}
	return err
}

// Signature is the X-Janus-Signature-256 header of body under secret:
// "sha256=" and the hex HMAC-SHA256 - a receiver computes the same over
// the raw body and compares in constant time.
func Signature(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// --- the secret, kept as the CA's key is ---

const (
	balancerSecretFile      = "balancer.secret.sealed"
	balancerSecretPlainFile = "balancer.secret"
)

func balancerPurpose(id string) string { return "cluster/" + id + "/balancer" }

// BalancerSecret is cluster id's hook secret; none: nil.
func (s *Store) BalancerSecret(id string) ([]byte, error) {
	dir := s.clusterDir(id)
	if s.key == nil {
		b, err := os.ReadFile(filepath.Join(dir, balancerSecretPlainFile))
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return b, err
	}
	sealed, err := os.ReadFile(filepath.Join(dir, balancerSecretFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b, err := s.key.Open(sealed, balancerPurpose(id))
	if err != nil {
		return nil, fmt.Errorf("the balancer's secret: %w", err)
	}
	return b, nil
}

// setBalancerSecret keeps secret for cluster id; empty removes it.
func (s *Store) setBalancerSecret(id string, secret []byte) error {
	dir := s.clusterDir(id)
	if len(secret) == 0 {
		for _, f := range []string{balancerSecretFile, balancerSecretPlainFile} {
			if err := os.Remove(filepath.Join(dir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		return nil
	}
	if s.key == nil {
		return writeDurable(filepath.Join(dir, balancerSecretPlainFile), secret)
	}
	sealed, err := s.key.Seal(secret, balancerPurpose(id))
	if err != nil {
		return fmt.Errorf("seal the balancer's secret: %w", err)
	}
	return writeDurable(filepath.Join(dir, balancerSecretFile), sealed)
}

// SetBalancer gives cluster id the hook b (nil: none) - secret nil
// keeps the secret it has, empty removes it - and calls it with the
// members at once; the settings are kept even when that call fails, its
// error returned.
func (e *Engine) SetBalancer(ctx context.Context, id string, b *Balancer, secret *string, actor string) error {
	if b != nil {
		if err := b.Validate(); err != nil {
			return err
		}
	}
	if b == nil {
		empty := ""
		secret = &empty
	}
	var prev *Balancer
	if cur, ok := e.Store.Get(id); ok {
		prev = cur.Balancer
	}
	// Another kind: the secret it had was the other's (a signing secret,
	// a router's password) - given again, or gone.
	if prev != nil && b != nil && prev.IsRouterOS() != b.IsRouterOS() && secret == nil {
		empty := ""
		secret = &empty
	}
	if b.IsRouterOS() {
		has, err := e.Store.BalancerSecret(id)
		if err != nil {
			return err
		}
		if (secret != nil && *secret == "") || (secret == nil && len(has) == 0) {
			return errors.New("a routeros balancer signs in with the router user's password: give it as the secret")
		}
	}
	if secret != nil {
		if err := e.Store.setBalancerSecret(id, []byte(*secret)); err != nil {
			return err
		}
	}
	has, err := e.Store.BalancerSecret(id)
	if err != nil {
		return err
	}
	if _, err := e.Store.Update(id, func(c *Cluster) error {
		if b != nil {
			nb := *b
			nb.HasSecret = len(has) > 0
			c.Balancer = &nb
		} else {
			c.Balancer = nil
		}
		return nil
	}); err != nil {
		return err
	}
	_ = e.Store.UpdateState(id, func(s *State) { s.Balancer = BalancerState{} })
	if b.IsRouterOS() && prev.IsRouterOS() && prev.URL == b.URL {
		// The same router: the families no prefix is in any more lose
		// their routes now.
		var gone []string
		for _, f := range prev.families() {
			if !slices.Contains(b.families(), f) {
				gone = append(gone, f)
			}
		}
		if len(gone) > 0 {
			c, _ := e.Store.Get(id)
			if _, _, err := e.routerOSApply(ctx, c, e.BalancerList(c), gone...); err != nil {
				e.Journal(id, actor, "balancer", "", 0, "the router's routes of the prefixes taken out weren't removed: %v", err)
			}
		}
	}
	if b == nil {
		e.Journal(id, actor, "balancer", "", 0, "no balancer hook any more")
		return nil
	}
	if b.IsRouterOS() {
		e.Journal(id, actor, "balancer", "", 0, "RouterOS router %s, as %s: routes to %s", b.URL, b.User, strings.Join(b.Prefixes, ", "))
	} else {
		e.Journal(id, actor, "balancer", "", 0, "balancer hook %s", b.URL)
	}
	return e.SyncBalancer(ctx, id, actor)
}

// Drain takes member name of cluster id out of service: the balancer
// told first (and its drain delay waited), then its virtual IPs and
// routes moved to the others; HAProxy keeps serving.
func (e *Engine) Drain(ctx context.Context, id, name, actor string) error {
	c, ok := e.Store.Get(id)
	if !ok {
		return ErrNotFound
	}
	m := c.Member(name)
	if m == nil {
		return fmt.Errorf("%s isn't a member", name)
	}
	if err := e.leaving(ctx, id, name, false, actor); err != nil {
		return err
	}
	msg, err := e.Nodes.ClusterDrain(ctx, *m, time.Minute)
	if err != nil {
		_ = e.announce(ctx, id, "in", name, actor) // it serves on
		return err
	}
	e.setDrained(id, name, true)
	e.Journal(id, actor, "drain", name, 0, "%s drained: %s", name, msg)
	return nil
}

// Undrain puts member name of cluster id back in service, then tells
// the balancer - a failure journaled, Reconcile calling again.
func (e *Engine) Undrain(ctx context.Context, id, name, actor string) error {
	c, ok := e.Store.Get(id)
	if !ok {
		return ErrNotFound
	}
	m := c.Member(name)
	if m == nil {
		return fmt.Errorf("%s isn't a member", name)
	}
	if err := e.Nodes.ClusterUndrain(ctx, *m); err != nil {
		return err
	}
	e.setDrained(id, name, false)
	e.Journal(id, actor, "undrain", name, 0, "%s back in service", name)
	_ = e.announce(ctx, id, "in", name, actor)
	return nil
}
