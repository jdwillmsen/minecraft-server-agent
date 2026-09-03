// Package ratelimit bounds how often a single chat actor may trigger a
// command, so chat spam can't turn into unbounded downstream calls once a
// command's Run does real work (an HTTP call to mc-console-bridge in Stage
// 2, an LLM call in Stage 4). Nothing in this repo rate-limited commands
// before this package existed.
package ratelimit

import (
	"sync"
	"time"
)

// PerActor is a sliding-window limiter keyed by actor (XUID). now is
// supplied by the caller rather than read internally, so tests don't need
// real delays to exercise window expiry.
type PerActor struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	events map[string][]time.Time
}

// NewPerActor builds a limiter allowing at most max calls per actor in any
// rolling window-sized interval.
func NewPerActor(max int, window time.Duration) *PerActor {
	return &PerActor{max: max, window: window, events: make(map[string][]time.Time)}
}

// Allow reports whether actor may act at now, and records the attempt if
// so. Denied attempts are not recorded, so an actor stuck at the limit
// doesn't need to wait out a full window once traffic actually stops.
func (r *PerActor) Allow(actor string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	cutoff := now.Add(-r.window)
	kept := r.events[actor][:0]
	for _, t := range r.events[actor] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}

	if len(kept) >= r.max {
		r.events[actor] = kept
		return false
	}
	r.events[actor] = append(kept, now)
	return true
}
