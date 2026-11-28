package haproxy

import (
	"log"
	"time"
)

// DefaultLearnWait bounds how long a reload waits for the running process
// to learn its stick tables: HAProxy's resync timeout is 5 s for the
// lesson from the process it replaced, then 5 s for one from a remote
// peer (PEER_RESYNC_TIMEOUT) - a process whose remote peers never answer
// has finished both after 10 s.
const DefaultLearnWait = 11 * time.Second

// learnStep is how often waitLearnt reads the section.
var learnStep = 100 * time.Millisecond

// waitLearnt waits, before a reload, until the running HAProxy has learnt
// the stick tables of PeersSection - both of its resyncs finished, the
// flags ClusterStatus.tables_learnt reads -, or LearnWait has passed.
//
// A soft-stopped HAProxy teaches the process that replaces it only when
// its own resync is finished (src/peers.c __process_stopping_peer_sync,
// 3.4.7 and 3.4.9 alike); otherwise it exits at once. Reloaded earlier -
// within ~5 s of its start, or of a reload whose lesson didn't come -, the
// new process starts with empty tables, counts from zero under traffic,
// and its updates overwrite the other members' counters before a remote
// peer teaches it the same small values 5 s later: a key's count reset
// for good on every member. Reloads closer than 5 s apart kept doing it
// once one lesson was missed (#701, measured on the build box: 4 reloads
// 3 s apart from 4 s after a start reset both keys on both members at
// each one; the same reloads of a process that had learnt kept every
// count). Called with mu held; an unanswering stats socket or no section
// doesn't wait.
func (m *Manager) waitLearnt() {
	if m.PeersSection == "" {
		return
	}
	limit := m.LearnWait
	if limit <= 0 {
		limit = DefaultLearnWait
	}
	start := time.Now()
	for {
		s, err := m.ShowPeersSection(m.PeersSection)
		if err != nil || s.Learnt {
			if waited := time.Since(start); s != nil && s.Learnt && waited >= learnStep {
				log.Printf("haproxy: reloading once the stick tables were learnt (%s)", waited.Round(100*time.Millisecond))
			}
			return
		}
		if time.Since(start) >= limit {
			log.Printf("haproxy: reloading before the running process learnt its stick tables (%s): the new one may start without them", limit)
			return
		}
		time.Sleep(learnStep)
	}
}
