package moderation

import (
	"fmt"
	"testing"
	"time"
)

var epoch = time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

func at(d time.Duration) time.Time { return epoch.Add(d) }

func TestFloodIsTheSixthMessageNotTheFifth(t *testing.T) {
	tr := NewTracker()
	for i := 0; i < FloodMessages; i++ {
		if _, ok := tr.Flood("p", at(time.Duration(i)*time.Second)); ok {
			t.Fatalf("message %d flagged; five in ten seconds is allowed", i+1)
		}
	}
	detail, ok := tr.Flood("p", at(5*time.Second))
	if !ok {
		t.Fatal("the sixth message inside ten seconds was not flagged")
	}
	if detail != "6 messages in 10s" {
		t.Errorf("detail = %q", detail)
	}
}

// The window is half-open: a message exactly ten seconds old has left it.
func TestFloodWindowIsTenSecondsExactly(t *testing.T) {
	tr := NewTracker()
	for i := 0; i < FloodMessages; i++ {
		tr.Flood("p", at(time.Duration(i)*2*time.Second))
	}
	if _, ok := tr.Flood("p", at(FloodWindow)); ok {
		t.Error("six messages spanning exactly ten seconds were flagged")
	}

	tr = NewTracker()
	for i := 0; i < FloodMessages; i++ {
		tr.Flood("p", at(time.Duration(i)*2*time.Second))
	}
	if _, ok := tr.Flood("p", at(FloodWindow-time.Millisecond)); !ok {
		t.Error("six messages inside ten seconds were not flagged")
	}
}

// A sustained burst is one flag, and a burst after the window has drained is
// another: a spammer must not be able to buy a database row per line.
func TestFloodIsOneFlagPerBurst(t *testing.T) {
	tr := NewTracker()
	flags := 0
	for i := 0; i < 30; i++ {
		if _, ok := tr.Flood("p", at(time.Duration(i)*500*time.Millisecond)); ok {
			flags++
		}
	}
	if flags != 1 {
		t.Fatalf("a 30-message burst produced %d flags, want 1", flags)
	}

	quiet := at(time.Minute)
	for i := 0; i < FloodMessages; i++ {
		if _, ok := tr.Flood("p", quiet.Add(time.Duration(i)*time.Second)); ok {
			t.Fatalf("message %d of the second burst flagged too early", i+1)
		}
	}
	if _, ok := tr.Flood("p", quiet.Add(5*time.Second)); !ok {
		t.Error("a second burst after the window drained was not flagged")
	}
}

func TestFloodIsCountedPerPlayer(t *testing.T) {
	tr := NewTracker()
	for i := 0; i < 3; i++ {
		tr.Flood("a", at(time.Duration(i)*time.Second))
		if _, ok := tr.Flood("b", at(time.Duration(i)*time.Second)); ok {
			t.Fatal("two players' messages were counted together")
		}
	}
}

func TestNoticeIsAtMostOncePerTenMinutes(t *testing.T) {
	tr := NewTracker()
	if !tr.ClaimNotice("p", at(0)) {
		t.Fatal("the first notice was refused")
	}
	if tr.ClaimNotice("p", at(NoticeInterval-time.Second)) {
		t.Error("a second notice inside ten minutes was allowed")
	}
	if !tr.ClaimNotice("other", at(time.Second)) {
		t.Error("one player's notice throttled another's")
	}
	if !tr.ClaimNotice("p", at(NoticeInterval)) {
		t.Error("a notice ten minutes later was refused")
	}
}

func TestWarningIsAtMostOncePerMinute(t *testing.T) {
	tr := NewTracker()
	if !tr.ClaimWarning("p", at(0)) {
		t.Fatal("the first warning was refused")
	}
	if tr.ClaimWarning("p", at(WarningInterval-time.Second)) {
		t.Error("a second warning inside a minute was allowed")
	}
	if !tr.ClaimWarning("p", at(WarningInterval)) {
		t.Error("a warning a minute later was refused")
	}
}

func TestIdlePlayersAreForgotten(t *testing.T) {
	tr := NewTracker()
	tr.Flood("gone", at(0))
	tr.ClaimNotice("gone", at(0))
	tr.Flood("recent", at(idleAfter-time.Minute))

	tr.Flood("speaker", at(idleAfter))
	if n := tr.Len(); n != 2 {
		t.Fatalf("Len = %d after ten idle minutes, want 2 (recent and speaker)", n)
	}
	tr.mu.Lock()
	_, stillThere := tr.players["gone"]
	_, recentThere := tr.players["recent"]
	tr.mu.Unlock()
	if stillThere {
		t.Error("a player idle for ten minutes was kept")
	}
	if !recentThere {
		t.Error("a player seen a minute before the sweep was dropped")
	}
}

// Dropping an idle player must not change behaviour: their throttle has
// expired by the time they can be dropped.
func TestForgettingAnIdlePlayerLosesNoLiveThrottle(t *testing.T) {
	tr := NewTracker()
	tr.ClaimNotice("p", at(0))
	// p stays silent while someone else's messages run a sweep every minute.
	for d := time.Minute; d < idleAfter; d += time.Minute {
		tr.Flood("someone-else", at(d))
	}
	if tr.ClaimNotice("p", at(idleAfter-time.Second)) {
		t.Error("a sweep forgot p's notice before its ten minutes were up")
	}
}

func TestTrackerIsBoundedOutright(t *testing.T) {
	tr := NewTracker()
	tr.maxPlayers = 3
	for i := 0; i < 10; i++ {
		tr.Flood(fmt.Sprintf("p%d", i), at(time.Duration(i)*time.Second))
		if n := tr.Len(); n > 3 {
			t.Fatalf("Len = %d, want at most 3", n)
		}
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for _, want := range []string{"p7", "p8", "p9"} {
		if _, ok := tr.players[want]; !ok {
			t.Errorf("%s was evicted; the least recently seen should go first", want)
		}
	}
}
