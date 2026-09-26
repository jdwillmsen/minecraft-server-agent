package main

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/chat"
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

// The console has no chat to whisper into: a question typed at the server
// console would otherwise send a /tell to the console's pseudo-XUID.
func TestProgressWhisperSkipsTheConsole(t *testing.T) {
	_, pctx, voice, _, _, _, _ := newHarness(t)
	ans := testAnswering()

	// The console first, so a whisper it wrongly sent is already on its way
	// by the time the player's arrives.
	sendProgress(context.Background(), chat.ServerOrigin, pctx, ans, quietLog)()
	sendProgress(context.Background(), playerXUID, pctx, ans, quietLog)()

	want := "tell " + playerXUID + ": " + progressText
	deadline := time.Now().Add(5 * time.Second)
	for !slices.Contains(voice.output(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("the player was never told %q; voice output = %q", progressText, voice.output())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := voice.output(); len(got) != 1 {
		t.Errorf("voice output = %q, want only %q -- the console must get no whisper", got, want)
	}
}
