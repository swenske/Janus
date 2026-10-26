package auth

import (
	"errors"
	"fmt"
	"slices"
)

// Preferences are how an account's pages behave - the Controller's
// Settings tab -, kept with the account so they follow it from browser to
// browser: what it does to the nodes and hypervisors it watches (how
// often they're asked, whether their figures are sampled) is the
// account's choice, not a browser's. Zero values take the defaults
// (Effective).
type Preferences struct {
	// NodesRefreshSeconds: how often the Nodes page asks the nodes'
	// status - one of RefreshChoices["nodes"].
	NodesRefreshSeconds int `json:"nodes_refresh_seconds,omitempty"`
	// HypervisorsRefreshSeconds: how often the Hypervisors page asks
	// each hypervisor's - one of RefreshChoices["hypervisors"].
	HypervisorsRefreshSeconds int `json:"hypervisors_refresh_seconds,omitempty"`
	// NodeStats: the nodes' figures on the Nodes page (CPU, requests,
	// traffic) - off, the page stops asking for them, and the Controller
	// stops sampling the nodes for it.
	NodeStats *bool `json:"node_stats,omitempty"`
	// NodeCharts: their curves - off, the figures alone.
	NodeCharts *bool `json:"node_charts,omitempty"`
	// TimeZone: the pages' times in the browser's zone ("local") or in
	// "utc" - as Elasticsearch and Prometheus keep them.
	TimeZone string `json:"time_zone,omitempty"`
	// RateUnit: traffic in "bytes" a second (KiB/s, HAProxy's counters) or
	// "bits" (Mbit/s, a link's or a router's).
	RateUnit string `json:"rate_unit,omitempty"`
}

// RefreshChoices are the intervals offered, in seconds: a node's status
// is an RPC to each node, a hypervisor's a round trip to its API or over
// SSH - nothing faster than this, for every page open.
var RefreshChoices = map[string][]int{
	"nodes":       {5, 10, 15, 30, 60},
	"hypervisors": {10, 15, 30, 60, 120},
}

// DefaultPreferences are what an account that chose nothing gets: the
// pages as they were.
var DefaultPreferences = Preferences{NodesRefreshSeconds: 15, HypervisorsRefreshSeconds: 15, NodeStats: boolPtr(true), NodeCharts: boolPtr(true), TimeZone: "local", RateUnit: "bytes"}

func boolPtr(v bool) *bool { return &v }

// Effective is p with the defaults filled in.
func (p Preferences) Effective() Preferences {
	d := DefaultPreferences
	if p.NodesRefreshSeconds == 0 {
		p.NodesRefreshSeconds = d.NodesRefreshSeconds
	}
	if p.HypervisorsRefreshSeconds == 0 {
		p.HypervisorsRefreshSeconds = d.HypervisorsRefreshSeconds
	}
	if p.NodeStats == nil {
		p.NodeStats = boolPtr(*d.NodeStats)
	}
	if p.NodeCharts == nil {
		p.NodeCharts = boolPtr(*d.NodeCharts)
	}
	if p.TimeZone == "" {
		p.TimeZone = d.TimeZone
	}
	if p.RateUnit == "" {
		p.RateUnit = d.RateUnit
	}
	return p
}

// Floored is p (effective) with its refreshes raised to the Controller's
// floors (Settings): an account's choice made before an admin raised
// them follows them.
func (p Preferences) Floored(s Settings) Preferences {
	p.NodesRefreshSeconds = max(p.NodesRefreshSeconds, s.NodesRefreshFloorSeconds)
	p.HypervisorsRefreshSeconds = max(p.HypervisorsRefreshSeconds, s.HypervisorsRefreshFloorSeconds)
	return p
}

// Choices are the refreshes an account may choose under s's floors.
func Choices(s Settings) map[string][]int {
	floors := map[string]int{"nodes": s.NodesRefreshFloorSeconds, "hypervisors": s.HypervisorsRefreshFloorSeconds}
	out := map[string][]int{}
	for k, all := range RefreshChoices {
		out[k] = []int{}
		for _, v := range all {
			if v >= floors[k] {
				out[k] = append(out[k], v)
			}
		}
	}
	return out
}

// CheckFloors refuses a refresh shorter than the Controller's floors.
func (p Preferences) CheckFloors(s Settings) error {
	if p.NodesRefreshSeconds != 0 && p.NodesRefreshSeconds < s.NodesRefreshFloorSeconds {
		return fmt.Errorf("nodes_refresh_seconds: this Controller's admins ask for %d s at least", s.NodesRefreshFloorSeconds)
	}
	if p.HypervisorsRefreshSeconds != 0 && p.HypervisorsRefreshSeconds < s.HypervisorsRefreshFloorSeconds {
		return fmt.Errorf("hypervisors_refresh_seconds: this Controller's admins ask for %d s at least", s.HypervisorsRefreshFloorSeconds)
	}
	return nil
}

func (p Preferences) clone() Preferences {
	if p.NodeStats != nil {
		p.NodeStats = boolPtr(*p.NodeStats)
	}
	if p.NodeCharts != nil {
		p.NodeCharts = boolPtr(*p.NodeCharts)
	}
	return p
}

// Check refuses an interval that isn't offered.
func (p Preferences) Check() error {
	if p.NodesRefreshSeconds != 0 && !slices.Contains(RefreshChoices["nodes"], p.NodesRefreshSeconds) {
		return fmt.Errorf("nodes_refresh_seconds: one of %v", RefreshChoices["nodes"])
	}
	if p.HypervisorsRefreshSeconds != 0 && !slices.Contains(RefreshChoices["hypervisors"], p.HypervisorsRefreshSeconds) {
		return fmt.Errorf("hypervisors_refresh_seconds: one of %v", RefreshChoices["hypervisors"])
	}
	switch p.TimeZone {
	case "", "local", "utc":
	default:
		return errors.New(`time_zone: "local" or "utc"`)
	}
	switch p.RateUnit {
	case "", "bytes", "bits":
	default:
		return errors.New(`rate_unit: "bytes" or "bits"`)
	}
	return nil
}

// SetPreferences replaces account name's preferences.
func (s *Store) SetPreferences(name string, p Preferences) (Preferences, error) {
	if err := p.Check(); err != nil {
		return Preferences{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	u, ok := s.users[name]
	if !ok {
		return Preferences{}, ErrNoUser
	}
	next := *u
	next.Preferences = p.clone()
	s.users[name] = &next
	if err := s.save(); err != nil {
		s.users[name] = u
		return Preferences{}, err
	}
	return p, nil
}
