package presence

import "sync"

// Gate is whether this process's own actor should be in the world, as the
// leader's loop last decided. It satisfies the agent's sessionGate: the
// session lifecycle reads Wanted and waits on the channel.
type Gate struct {
	mu      sync.Mutex
	present bool
	changed chan struct{}
}

func NewGate(present bool) *Gate {
	return &Gate{present: present, changed: make(chan struct{})}
}

// Set records the answer, waking every waiter only when it differs: a
// session is torn down on a wake, so a spurious one every tick would be
// read again for nothing.
func (g *Gate) Set(present bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.present == present {
		return
	}
	g.present = present
	close(g.changed)
	g.changed = make(chan struct{})
}

// Wanted reports the answer and a channel closed when it next changes.
func (g *Gate) Wanted() (bool, <-chan struct{}) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.present, g.changed
}
