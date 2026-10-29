package consul

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/swenske/Janus/internal/haproxystat"
)

// Registrar keeps the node's own services registered in its agent, as
// the managed layer's toggles choose them (services.go), with the
// layer's token: what's missing or changed is registered, the janus:*
// services no longer wanted deregistered - never another ID, the
// operator's services {} blocks are the agent's own. While the node is
// out of service (drained, its HAProxy stopped, going down) they're in
// maintenance.
//
// It syncs when the agent starts, when HAProxy runs another
// configuration or a map changes, when the node is drained or
// undrained, when the layer changes - and every minute, the anti-entropy
// that puts back what someone took off. The frontends' TTL checks are
// fed from HAProxy's show stat after each sync and every TTLEvery.
type Registrar struct {
	Manager *Manager
	// Sources reads what the services come from, now.
	Sources func() (Sources, error)
	// Maintenance says why the node's services are out of service ("":
	// they're not).
	Maintenance func() string
	// Wake returns channels closed when it's worth syncing again (taken
	// before each sync).
	Wake func() []<-chan struct{}
	// ShowStat reads HAProxy's show stat (its CSV): the frontends' TTL
	// checks are fed from their FRONTEND rows.
	ShowStat func() ([]byte, error)
	// Every is the anti-entropy's period, Retry the wait after a failure,
	// TTLEvery the TTL checks' (FrontendTTL lets one expire after a few
	// missed).
	Every, Retry, TTLEvery time.Duration

	// addr is the agent's HTTP API, HTTPAddr if empty (tests).
	addr string

	poke chan struct{}

	mu        sync.Mutex
	defs      map[string]string // what was registered under each ID, hashed
	ttl       map[string]string // each TTL check: the proxy whose state feeds it
	token     string            // the token they were registered with
	feedErr   string
	down      string // going down: why
	status    SyncStatus
	pokeOnce  sync.Once
	lastWarns string
}

// SyncStatus is how the registration stands.
type SyncStatus struct {
	At          time.Time // the last sync that went through
	Err         string    // why the last one didn't
	Registered  int
	Maintenance bool
}

func (r *Registrar) init() {
	r.pokeOnce.Do(func() { r.poke = make(chan struct{}, 1) })
}

// Poke has the services synced now.
func (r *Registrar) Poke() {
	if r == nil {
		return
	}
	r.init()
	select {
	case r.poke <- struct{}{}:
	default:
	}
}

// Status is how the registration stands.
func (r *Registrar) Status() SyncStatus {
	if r == nil {
		return SyncStatus{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

// Run syncs until ctx ends.
func (r *Registrar) Run(ctx context.Context) {
	r.init()
	every, retry := r.Every, r.Retry
	if every == 0 {
		every = time.Minute
	}
	if retry == 0 {
		retry = 5 * time.Second
	}
	go r.feedLoop(ctx, cmp.Or(r.TTLEvery, 15*time.Second))
	for {
		var wake []<-chan struct{}
		if r.Wake != nil {
			wake = r.Wake()
		}
		sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := r.Sync(sctx)
		cancel()
		wait := every
		if err != nil {
			wait = retry
		}
		if !r.wait(ctx, wake, wait) {
			return
		}
	}
}

// wait waits for d, a poke or one of wake; false: ctx ended. A wake
// waits a little more, for the changes that come together (a map's
// entries one by one).
func (r *Registrar) wait(ctx context.Context, wake []<-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	woken := make(chan struct{})
	stop := make(chan struct{})
	defer close(stop)
	for _, ch := range wake {
		go func(ch <-chan struct{}) {
			select {
			case <-ch:
				select {
				case woken <- struct{}{}:
				case <-stop:
				}
			case <-stop:
			}
		}(ch)
	}
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	case <-r.poke:
	case <-woken:
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(500 * time.Millisecond):
		return true
	}
}

// Sync registers what's wanted, deregisters what isn't, and puts the
// services in or out of maintenance.
func (r *Registrar) Sync(ctx context.Context) error {
	err := r.sync(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		if err.Error() != r.status.Err {
			log.Printf("consul: the node's services: %v - trying again", err)
		}
		r.status.Err = err.Error()
	} else {
		if r.status.Err != "" {
			log.Printf("consul: the node's services are registered again")
		}
		r.status.Err, r.status.At = "", time.Now()
	}
	return err
}

func (r *Registrar) sync(ctx context.Context) error {
	managed, err := LoadManaged()
	if err != nil {
		return err
	}
	if managed == nil {
		r.mu.Lock()
		r.defs, r.ttl, r.status = nil, nil, SyncStatus{}
		r.mu.Unlock()
		return nil
	}
	c := &agentClient{token: managed.AgentToken, addr: r.addr}
	maint := r.maintenance()
	src, err := r.Sources()
	var desired []Service
	truncated := false
	if err == nil {
		desired, truncated, err = Desired(managed.Services, src)
	}
	if err != nil {
		if maint == "" {
			return err
		}
		// Out of service, and what it serves can't be read now: HAProxy
		// stopped, its maps with it (they're read from its stats socket).
		// The services registered go in maintenance all the same - the
		// sync once it serves again reads them.
		n, merr := r.maintainRegistered(ctx, c, maint)
		r.mu.Lock()
		r.status.Registered, r.status.Maintenance = n, merr == nil
		r.mu.Unlock()
		return merr
	}
	warn := ""
	if truncated {
		warn = fmt.Sprintf("over %d URLs: the first %d registered", MaxURLs, MaxURLs)
	}
	r.mu.Lock()
	if warn != r.lastWarns && warn != "" {
		log.Printf("consul: %s", warn)
	}
	r.lastWarns = warn
	r.mu.Unlock()
	state := r.frontendStates()
	n, err := r.reconcile(ctx, c, desired, maint, state)
	r.mu.Lock()
	r.status.Registered, r.status.Maintenance = n, maint != ""
	r.mu.Unlock()
	if ferr := r.feed(ctx, state); err == nil {
		err = ferr
	}
	return err
}

// frontendStates reads HAProxy's show stat once: each frontend's TTL
// check status and output (frontendState) - critical, saying why, when
// it can't be read.
func (r *Registrar) frontendStates() func(proxy string) (string, string) {
	var raw []byte
	err := errors.New("no statistics to read")
	if r.ShowStat != nil {
		raw, err = r.ShowStat()
	}
	var t *haproxystat.Table
	if err == nil {
		t, err = haproxystat.Parse(raw)
	}
	if err != nil {
		out := "HAProxy's statistics: " + err.Error()
		return func(string) (string, string) { return "critical", out }
	}
	return func(proxy string) (string, string) { return frontendState(t, proxy) }
}

// feed gives each TTL check registered its frontend's state (state,
// read now when nil) - which keeps it from expiring.
func (r *Registrar) feed(ctx context.Context, state func(string) (string, string)) error {
	r.mu.Lock()
	ttl, token := maps.Clone(r.ttl), r.token
	r.mu.Unlock()
	if len(ttl) == 0 {
		return nil
	}
	if state == nil {
		state = r.frontendStates()
	}
	c := &agentClient{token: token, addr: r.addr}
	var errs []string
	for _, id := range slices.Sorted(maps.Keys(ttl)) {
		status, out := state(ttl[id])
		body := map[string]string{"Status": status, "Output": out}
		if err := c.do(ctx, http.MethodPut, "/v1/agent/check/update/"+url.PathEscape(id), body, nil); err != nil {
			errs = append(errs, fmt.Sprintf("check %s: %v", id, err))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// feedLoop feeds the TTL checks every period until ctx ends.
func (r *Registrar) feedLoop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		fctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := r.feed(fctx, nil)
		cancel()
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		r.mu.Lock()
		if msg != r.feedErr {
			if msg != "" {
				log.Printf("consul: the frontends' checks: %s - trying again", msg)
			} else {
				log.Printf("consul: the frontends' checks are fed again")
			}
		}
		r.feedErr = msg
		r.mu.Unlock()
	}
}

func (r *Registrar) maintenance() string {
	r.mu.Lock()
	down := r.down
	r.mu.Unlock()
	if down != "" {
		return down
	}
	if r.Maintenance != nil {
		return r.Maintenance()
	}
	return ""
}

func hashOf(s Service, token string) string {
	b, _ := json.Marshal(s)
	sum := sha256.Sum256(append(b, token...))
	return hex.EncodeToString(sum[:])
}

// reconcile makes the agent's janus:* services desired, in maintenance
// (maint, the reason) or not; it returns how many are registered.
func (r *Registrar) reconcile(ctx context.Context, c *agentClient, desired []Service, maint string, state func(string) (string, string)) (int, error) {
	have, err := c.services(ctx)
	if err != nil {
		return 0, err
	}
	r.mu.Lock()
	if r.defs == nil || r.token != c.token {
		r.defs = map[string]string{} // another token: everything again, with it
	}
	r.token = c.token
	defs := r.defs
	r.mu.Unlock()
	want := map[string]bool{}
	ttl := map[string]string{}
	var errs []string
	registered := 0
	changed := false
	for _, d := range desired {
		want[d.ID] = true
		if d.Check != nil && d.Check.TTL != "" {
			ttl[d.Check.CheckID] = d.Check.proxy
		}
		h := hashOf(d, c.token)
		cur, ok := have[d.ID]
		if ok && defs[d.ID] == h && sameService(cur, d) {
			registered++
			continue
		}
		reg := d
		if d.Check != nil && d.Check.TTL != "" {
			// Registered with its frontend's state: never critical for
			// the moment until it's fed.
			ck := *d.Check
			ck.Status, _ = state(ck.proxy)
			reg.Check = &ck
		}
		if err := c.do(ctx, http.MethodPut, "/v1/agent/service/register?replace-existing-checks=true", reg, nil); err != nil {
			errs = append(errs, fmt.Sprintf("register %s: %v", d.ID, err))
			r.mu.Lock()
			delete(defs, d.ID)
			r.mu.Unlock()
			if d.Check != nil && !ok {
				delete(ttl, d.Check.CheckID) // nothing to feed
			}
			continue
		}
		r.mu.Lock()
		defs[d.ID] = h
		r.mu.Unlock()
		registered++
		changed = true
	}
	for id := range have {
		if !strings.HasPrefix(id, IDPrefix) || want[id] {
			continue
		}
		if err := c.do(ctx, http.MethodPut, "/v1/agent/service/deregister/"+url.PathEscape(id), nil, nil); err != nil {
			errs = append(errs, fmt.Sprintf("deregister %s: %v", id, err))
			continue
		}
		r.mu.Lock()
		delete(defs, id)
		r.mu.Unlock()
	}
	r.mu.Lock()
	r.ttl = ttl
	r.mu.Unlock()
	if changed && len(errs) == 0 {
		log.Printf("consul: %d services registered", registered)
	}
	if err := r.setMaintenance(ctx, c, desired, maint); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return registered, errors.New(strings.Join(errs, "; "))
	}
	return registered, nil
}

// setMaintenance puts services in maintenance (maint, the reason) or
// takes them out.
func (r *Registrar) setMaintenance(ctx context.Context, c *agentClient, services []Service, maint string) error {
	checks, err := c.checks(ctx)
	if err != nil {
		return err
	}
	var errs []string
	for _, s := range services {
		_, in := checks[maintenanceCheck(s.ID)]
		if in == (maint != "") {
			continue
		}
		q := url.Values{"enable": {fmt.Sprint(maint != "")}}
		if maint != "" {
			q.Set("reason", maint)
		}
		if err := c.do(ctx, http.MethodPut, "/v1/agent/service/maintenance/"+url.PathEscape(s.ID)+"?"+q.Encode(), nil, nil); err != nil {
			errs = append(errs, fmt.Sprintf("maintenance %s: %v", s.ID, err))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// sameService: what the agent has of s is s.
func sameService(have agentService, s Service) bool {
	return have.Service == s.Name && have.Address == s.Address && have.Port == s.Port &&
		slices.Equal(orNone(have.Tags), orNone(s.Tags)) && sameMeta(have.Meta, s.Meta)
}

func orNone(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

func sameMeta(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// GoingDown puts the services in maintenance now, for good: the node is
// about to stop (a shutdown hook) - why says so.
func (r *Registrar) GoingDown(why string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.down = why
	r.mu.Unlock()
	managed, err := LoadManaged()
	if err != nil || managed == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	n, err := r.maintainRegistered(ctx, &agentClient{token: managed.AgentToken, addr: r.addr}, why)
	if err != nil {
		log.Printf("consul: services not all put in maintenance: %v", err)
		return
	}
	if n > 0 {
		log.Printf("consul: %d services in maintenance (%s)", n, why)
	}
}

// maintainRegistered puts every janus:* service the agent has in
// maintenance (why) - without reading what the node serves; it returns
// how many there are.
func (r *Registrar) maintainRegistered(ctx context.Context, c *agentClient, why string) (int, error) {
	have, err := c.services(ctx)
	if err != nil {
		return 0, err
	}
	var mine []Service
	for id := range have {
		if strings.HasPrefix(id, IDPrefix) {
			mine = append(mine, Service{ID: id})
		}
	}
	return len(mine), r.setMaintenance(ctx, c, mine, why)
}

// Forget deregisters every janus:* service, with the layer's token -
// before the layer goes.
func (r *Registrar) Forget(ctx context.Context) error {
	if r == nil {
		return nil
	}
	managed, err := LoadManaged()
	if err != nil || managed == nil {
		return err
	}
	c := &agentClient{token: managed.AgentToken, addr: r.addr}
	have, err := c.services(ctx)
	if err != nil {
		return err
	}
	var errs []string
	for id := range have {
		if !strings.HasPrefix(id, IDPrefix) {
			continue
		}
		if err := c.do(ctx, http.MethodPut, "/v1/agent/service/deregister/"+url.PathEscape(id), nil, nil); err != nil {
			errs = append(errs, fmt.Sprintf("deregister %s: %v", id, err))
		}
	}
	r.mu.Lock()
	r.defs, r.ttl = nil, nil
	r.mu.Unlock()
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}
