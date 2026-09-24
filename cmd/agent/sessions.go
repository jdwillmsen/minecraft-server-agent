package main

import (
	"context"

	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// sessionGate decides whether the live agent should be in the world. It is
// a separate question from whether this process is the live agent: the lock
// decides that, and the monitoring that comes with the lock keeps running
// either way.
type sessionGate interface {
	// Wanted reports whether a session should run now, and a channel closed
	// the next time that may change. A nil channel never closes. A close
	// with no change is allowed, and the answer is read again before anything
	// acts on it.
	Wanted() (present bool, changed <-chan struct{})
}

// alwaysPresent is the gate for a deployment with nothing deciding presence:
// the live agent is always in the world, as every release before the gate
// was.
type alwaysPresent struct{}

func (alwaysPresent) Wanted() (bool, <-chan struct{}) { return true, nil }

// sessionModes is what runSessions switches between.
type sessionModes struct {
	// present runs the connect loop until its context ends.
	present func(context.Context)
	// left settles what ending a session on purpose owes, once present has
	// returned. It is not called when the turn itself ends, because the
	// turn's handover settles that and settling twice would close the same
	// playtime twice.
	left func()
	// absent keeps the roster answering from outside the world until its
	// context ends.
	absent func(context.Context)
}

// runSessions is the session lifecycle inside one turn as the live agent:
// in the world while the gate says so, following it from the console bridge
// while it does not, until ctx ends.
//
// A mode always returns before the next one starts. The two write the same
// roster, and a follower still applying events after the connection's
// BeginSession would put players back that the server's snapshot never
// named.
func runSessions(ctx context.Context, gate sessionGate, modes sessionModes, log *logging.Logger) {
	for ctx.Err() == nil {
		present, changed := gate.Wanted()
		modeCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			if present {
				modes.present(modeCtx)
			} else {
				modes.absent(modeCtx)
			}
		}()
		awaitGateChange(ctx, gate, present, changed)
		cancel()
		<-done
		if ctx.Err() != nil {
			return
		}
		if present {
			modes.left()
		}
		log.Info("session_gate_changed", logging.Fields{"present": !present})
	}
}

// awaitGateChange returns when ctx ends or the gate's answer is no longer
// present.
func awaitGateChange(ctx context.Context, gate sessionGate, present bool, changed <-chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-changed:
			now, next := gate.Wanted()
			if now != present {
				return
			}
			changed = next
		}
	}
}
