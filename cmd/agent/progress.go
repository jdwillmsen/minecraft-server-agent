package main

import "time"

// progressText is whispered, never broadcast: only the asker is waiting, and
// a line in open chat for every slow answer would be noise to everyone else.
const progressText = "Looking that up…"

// progressDelay is how long a question can go unanswered before its asker is
// told it is coming. Most answers land well inside it, and for those the
// message would only be clutter.
const progressDelay = 3 * time.Second

// newProgressHook returns a tool-round hook that calls send at most once,
// on the first round finishing after delay has passed. Called on the
// answering goroutine, so no locking.
func newProgressHook(started time.Time, delay time.Duration, now func() time.Time, send func()) func(round int) {
	done := false
	return func(int) {
		if done || now().Sub(started) < delay {
			return
		}
		done = true
		send()
	}
}
