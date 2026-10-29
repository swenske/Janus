package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"slices"
	"sync"
)

// priority is the order a release's builds go in (app.prebuild): the
// schematics nodes run, the most run first, as janus-insights - the
// Controllers' opt-in usage reports, docs/guide/telemetry.md - says at
// -prebuild-priority-url. Asked at each pass; when it can't be, the last
// order it gave stays.
type priority struct {
	url, token string
	client     *http.Client

	mu   sync.Mutex
	rank map[string]int // schematic ID -> place, 0 first
}

func (p *priority) refresh(ctx context.Context) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	resp, err := p.client.Do(req)
	if err != nil {
		log.Printf("prebuild priority: %v", err)
		return
	}
	defer resp.Body.Close()
	var body struct {
		Schematics []struct {
			ID string `json:"id"`
		} `json:"schematics"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body) != nil {
		log.Printf("prebuild priority: %s", resp.Status)
		return
	}
	rank := map[string]int{}
	for i, s := range body.Schematics {
		if _, ok := rank[s.ID]; !ok {
			rank[s.ID] = i
		}
	}
	p.mu.Lock()
	p.rank = rank
	p.mu.Unlock()
}

// order sorts ids by their place, those it doesn't know last (in their
// order).
func (p *priority) order(ctx context.Context, ids []string) []string {
	if p == nil {
		return ids
	}
	p.refresh(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	out := slices.Clone(ids)
	slices.SortStableFunc(out, func(a, b string) int {
		ra, oka := p.rank[a]
		rb, okb := p.rank[b]
		switch {
		case oka && okb:
			return ra - rb
		case oka:
			return -1
		case okb:
			return 1
		}
		return 0
	})
	return out
}
