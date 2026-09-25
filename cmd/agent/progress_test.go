package main

import (
	"testing"
	"time"
)

func TestProgressHookWaitsThenSendsOnce(t *testing.T) {
	start := time.Unix(0, 0)
	now := start
	sent := 0
	hook := newProgressHook(start, 3*time.Second, func() time.Time { return now }, func() { sent++ })

	now = start.Add(time.Second)
	hook(0)
	if sent != 0 {
		t.Fatalf("sent after 1s, want nothing before 3s")
	}
	now = start.Add(4 * time.Second)
	hook(1)
	hook(2)
	if sent != 1 {
		t.Errorf("sent %d times, want exactly once", sent)
	}
}
