package main

import (
	"sync"
	"time"
)

// latencySource is the one method of *minecraft.Conn the meter reads.
type latencySource interface {
	Latency() time.Duration
}

// linkMeter exposes the current session's Bedrock round trip to !ping.
//
// The connection belongs to one session and the pinger to the process, so
// the meter is told when a session begins and ends rather than keeping a
// connection a reconnect has already closed.
type linkMeter struct {
	mu   sync.RWMutex
	conn latencySource
}

func (m *linkMeter) beginSession(conn latencySource) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.conn = conn
}

func (m *linkMeter) endSession() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.conn = nil
}

// roundTrip doubles what the connection reports: gophertunnel's Latency is
// half the round trip, and "ping" means the whole of it.
func (m *linkMeter) roundTrip() (time.Duration, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.conn == nil {
		return 0, false
	}
	return 2 * m.conn.Latency(), true
}
