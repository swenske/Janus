package consul

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
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
// that puts back what someone took off.
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
	// Every is the anti-entropy's period, Retry the wait after a failure.
	Every, Retry time.Duration

	// addr is the agent's HTTP API, HTTPAddr if empty (tests).
	addr string

	poke chan struct{}

	mu        sync.Mutex
	defs      map[string]string // what was registered under each ID, hashed
	token     string            // the token they were registered with
	down      string            // going down: why
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
		r.defs, r.status = nil, SyncStatus{}
		r.mu.Unlock()
		return nil
	}
	src, err := r.Sources()
	if err != nil {
		return err
	}
	desired, truncated, err := Desired(managed.Services, src)
	if err != nil {
		return err
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
	maint := r.maintenance()
	n, err := r.reconcile(ctx, &agentClient{token: managed.AgentToken, addr: r.addr}, desired, maint)
	r.mu.Lock()
	r.status.Registered, r.status.Maintenance = n, maint != ""
	r.mu.Unlock()
	return err
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
func (r *Registrar) reconcile(ctx context.Context, c *agentClient, desired []Service, maint string) (int, error) {
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
	var errs []string
	registered := 0
	changed := false
	for _, d := range desired {
		want[d.ID] = true
		h := hashOf(d, c.token)
		cur, ok := have[d.ID]
		if ok && defs[d.ID] == h && sameService(cur, d) {
			registered++
			continue
		}
		if err := c.do(ctx, http.MethodPut, "/v1/agent/service/register?replace-existing-checks=true", d, nil); err != nil {
			errs = append(errs, fmt.Sprintf("register %s: %v", d.ID, err))
			r.mu.Lock()
			delete(defs, d.ID)
			r.mu.Unlock()
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
	c := &agentClient{token: managed.AgentToken, addr: r.addr}
	have, err := c.services(ctx)
	if err != nil {
		log.Printf("consul: services not put in maintenance: %v", err)
		return
	}
	var mine []Service
	for id := range have {
		if strings.HasPrefix(id, IDPrefix) {
			mine = append(mine, Service{ID: id})
		}
	}
	if err := r.setMaintenance(ctx, c, mine, why); err != nil {
		log.Printf("consul: services not all put in maintenance: %v", err)
		return
	}
	if len(mine) > 0 {
		log.Printf("consul: %d services in maintenance (%s)", len(mine), why)
	}
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
	r.defs = nil
	r.mu.Unlock()
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}
