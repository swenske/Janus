package clusters

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// What a balancer says of a member (docs/guide/clusters.md "Waiting for
// the balancer"): a member's node says it announces its routes, or that
// it is undrained - the balancer in front is the one that decides where
// the traffic goes, and it can take a moment to act on it. An update that
// waits for it only moves on - takes the next member out - once the
// balancer sends the member that came back its traffic again. A RouterOS
// router is read (its routes to the service prefixes, RouterRoles); a
// webhook with confirm is asked (a "check" call).

// BalancerTimeout bounds a wait for the balancer to take a member in or
// out.
var BalancerTimeout = 2 * time.Minute

// CanConfirm reports whether cluster c's balancer can say whether it
// sends a member traffic, and how - or why not.
func (c *Cluster) CanConfirm() (how string, ok bool) {
	b := c.Balancer
	switch {
	case b == nil:
		return "the cluster has no balancer hook to ask", false
	case b.IsRouterOS():
		return "the router's routes to " + strings.Join(b.Prefixes, ", "), true
	case b.Confirm:
		return "the webhook's answers to check calls", true
	}
	return "its webhook doesn't answer check calls (confirm off)", false
}

// BalancerSees reports whether cluster c's balancer sends member name
// traffic now, and what it said (a router: the member's role for each
// service prefix).
func (e *Engine) BalancerSees(ctx context.Context, c *Cluster, name string) (bool, string, error) {
	if why, ok := c.CanConfirm(); !ok {
		return false, "", errors.New(why)
	}
	if c.Balancer.IsRouterOS() {
		view, err := e.RouterRoles(ctx, c)
		if err != nil {
			return false, "", err
		}
		roles := view.Members[name]
		if len(roles) == 0 {
			return false, "no service prefix", nil
		}
		in := true
		var parts []string
		for _, r := range roles {
			if r.Role != "multipath" && r.Role != "master" {
				in = false
			}
			parts = append(parts, r.Role+" for "+r.Prefix)
		}
		return in, strings.Join(parts, ", "), nil
	}
	in, err := e.checkHook(ctx, c, name)
	if err != nil {
		return false, "", err
	}
	if in {
		return true, "in service, it says", nil
	}
	return false, "out of service, it says", nil
}

// checkHook asks cluster c's webhook whether it sends member name
// traffic: one "check" call, never retried here - the wait asks again.
func (e *Engine) checkHook(ctx context.Context, c *Cluster, name string) (bool, error) {
	call := BalancerCall{ClusterID: c.ID, Cluster: c.Name, Event: "check", Member: name, Members: e.BalancerList(c), At: time.Now().UTC()}
	body, err := json.Marshal(call)
	if err != nil {
		return false, err
	}
	secret, err := e.Store.BalancerSecret(c.ID)
	if err != nil {
		return false, err
	}
	client, err := BalancerClient(c.Balancer)
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Balancer.URL, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Janus-cluster-balancer/1")
	req.Header.Set("X-Janus-Event", "check")
	if len(secret) > 0 {
		req.Header.Set("X-Janus-Signature-256", Signature(secret, body))
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("%s answered %s to a check call", c.Balancer.URL, resp.Status)
		if s := strings.TrimSpace(string(answer)); s != "" && len(s) < 512 {
			err = fmt.Errorf("%v: %s", err, s)
		}
		return false, err
	}
	var said struct {
		InService *bool `json:"in_service"`
	}
	if err := json.Unmarshal(answer, &said); err != nil || said.InService == nil {
		return false, fmt.Errorf("%s answered a check call without {\"in_service\": true|false}", c.Balancer.URL)
	}
	return *said.InService, nil
}

// waitBalancer waits until cluster c's balancer sends member name
// traffic (in) or none (!in), BalancerTimeout at most, journaling what it
// saw once it does.
func (e *Engine) waitBalancer(ctx context.Context, c *Cluster, name string, in bool, poll time.Duration, actor string) error {
	if poll <= 0 {
		poll = 2 * time.Second
	}
	noun := c.Balancer.noun()
	want := "in service"
	if !in {
		want = "out of service"
	}
	deadline := time.Now().Add(BalancerTimeout)
	since := time.Now()
	last := ""
	for {
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		sees, said, err := e.BalancerSees(cctx, c, name)
		cancel()
		switch {
		case err != nil:
			last = err.Error()
		case sees == in:
			e.Journal(c.ID, actor, "balancer", name, 0, "the %s has %s %s after %s: %s", noun, name, want, time.Since(since).Round(time.Second), said)
			return nil
		default:
			last = said
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the %s doesn't have %s %s after %s (%s)", noun, name, want, BalancerTimeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// balancerHasOthers checks that cluster c's balancer sends traffic to
// every member but name - before name goes out, its traffic has somewhere
// to go.
func (e *Engine) balancerHasOthers(ctx context.Context, c *Cluster, name string) error {
	for _, o := range c.Members {
		if o.Name == name {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		in, said, err := e.BalancerSees(cctx, c, o.Name)
		cancel()
		if err != nil {
			return fmt.Errorf("asking the %s about %s: %v", c.Balancer.noun(), o.Name, err)
		}
		if !in {
			return fmt.Errorf("the %s doesn't send %s traffic (%s) - %s's would have nowhere to go", c.Balancer.noun(), o.Name, said, name)
		}
	}
	return nil
}
