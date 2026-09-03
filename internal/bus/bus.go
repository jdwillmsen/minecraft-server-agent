// Package bus is a small typed publish/subscribe event bus. It exists so
// the protocol client (the "ear") and the plugin host stay decoupled: the
// client only ever publishes events, plugins only ever subscribe.
package bus

import "sync"

// Event is anything the bus can carry. Concrete event types (chat messages,
// player join/leave, server log events, ...) live in their owning packages
// and satisfy this marker interface by having a Kind().
type Event interface {
	Kind() string
}

// Bus fans out published events to every subscriber. It is safe for
// concurrent use.
type Bus struct {
	mu   sync.RWMutex
	subs map[string][]chan Event
}

// New builds an empty Bus.
func New() *Bus {
	return &Bus{subs: make(map[string][]chan Event)}
}

// Subscribe returns a channel that receives every future event of the given
// kind, and an unsubscribe function that removes it. The channel is buffered
// so a slow subscriber cannot block Publish; events are dropped for that
// subscriber if its buffer fills rather than stalling the publisher.
//
// Callers that subscribe outside of process-lifetime setup (e.g. a
// per-request subscription while waiting for a correlated event) must call
// the returned unsubscribe function when done, or the channel and its map
// entry leak for the life of the process.
func (b *Bus) Subscribe(kind string, buffer int) (<-chan Event, func()) {
	if buffer <= 0 {
		buffer = 16
	}
	ch := make(chan Event, buffer)

	b.mu.Lock()
	b.subs[kind] = append(b.subs[kind], ch)
	b.mu.Unlock()

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			subs := b.subs[kind]
			for i, c := range subs {
				if c == ch {
					b.subs[kind] = append(subs[:i], subs[i+1:]...)
					break
				}
			}
		})
	}
	return ch, unsubscribe
}

// Publish delivers ev to every subscriber registered for ev.Kind(). It never
// blocks: a subscriber whose buffer is full simply misses the event.
func (b *Bus) Publish(ev Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	for _, ch := range b.subs[ev.Kind()] {
		select {
		case ch <- ev:
		default:
			// Subscriber's buffer is full; drop rather than block the
			// publisher. Plugins that need every event should size their
			// buffer accordingly.
		}
	}
}

// SubscriberCount reports how many subscribers are registered for kind.
// Primarily useful in tests.
func (b *Bus) SubscriberCount(kind string) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs[kind])
}
