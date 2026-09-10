package moderation

import (
	"fmt"
	"sync"
	"time"
)

const (
	// NoticeInterval is the shortest gap between two operator notices about
	// the same player. One notice says "look at this player"; a second
	// inside the same stretch of behaviour says nothing new, and the record
	// still holds every flag for whoever looks.
	NoticeInterval = 10 * time.Minute
	// WarningInterval is the shortest gap between two warnings whispered to
	// the same player. Someone repeating a term ten times does not need ten
	// identical whispers, and each one is a console command.
	WarningInterval = time.Minute

	// idleAfter is how long a player can go without a message before their
	// state is dropped. Equal to the longest interval above, which makes the
	// drop lossless: by then their flood window is empty and every throttle
	// has expired, so a fresh entry behaves exactly as the old one would
	// have.
	idleAfter = NoticeInterval
	// sweepEvery spaces the idle sweeps, so a busy chat pays for a pass over
	// the map once a minute rather than on every line.
	sweepEvery = time.Minute
	// defaultMaxPlayers caps the map outright. XUIDs come from the server's
	// own authentication and the idle sweep alone bounds this by who spoke
	// in the last ten minutes, so the cap is only there so that no input,
	// however strange, can grow memory without limit.
	defaultMaxPlayers = 1000
)

type playerState struct {
	// recent holds at most FloodMessages+1 send times, oldest first: that is
	// all "more than FloodMessages inside FloodWindow" ever needs to know.
	recent []time.Time
	// flooding is set by the message that starts a flood and cleared by the
	// first message after the window has drained, so a sustained burst is
	// one flag rather than one per line -- a spammer must not be able to
	// turn their own spam into a row per message.
	flooding  bool
	warnedAt  time.Time
	noticedAt time.Time
	lastSeen  time.Time
}

// Tracker is the per-player memory the rules and throttles need: recent send
// times, and when each player was last warned and last reported. It is safe
// for concurrent use, and bounded -- see idleAfter and defaultMaxPlayers.
type Tracker struct {
	mu         sync.Mutex
	players    map[string]*playerState
	lastSweep  time.Time
	maxPlayers int
}

// NewTracker builds an empty Tracker.
func NewTracker() *Tracker {
	return &Tracker{players: make(map[string]*playerState), maxPlayers: defaultMaxPlayers}
}

// Flood notes one message from xuid at now and reports whether it starts a
// flood.
//
// The window is half-open: a message exactly FloodWindow old has left it.
// Every message counts, whatever it says, because a burst of commands fills
// public chat as surely as a burst of anything else.
func (t *Tracker) Flood(xuid string, now time.Time) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.touch(xuid, now)

	cut := now.Add(-FloodWindow)
	kept := p.recent[:0]
	for _, at := range p.recent {
		if at.After(cut) {
			kept = append(kept, at)
		}
	}
	kept = append(kept, now)
	if over := len(kept) - (FloodMessages + 1); over > 0 {
		kept = append(kept[:0], kept[over:]...)
	}
	p.recent = kept

	if len(kept) <= FloodMessages {
		p.flooding = false
		return "", false
	}
	if p.flooding {
		return "", false
	}
	p.flooding = true
	return fmt.Sprintf("%d messages in %s", len(kept), FloodWindow), true
}

// ClaimWarning reports whether xuid may be warned at now, and if so records
// that they were. A claim is spent even if the whisper then fails: retrying
// a failing bridge on every flagged line is the load this exists to cap.
func (t *Tracker) ClaimWarning(xuid string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.touch(xuid, now)
	if !p.warnedAt.IsZero() && now.Sub(p.warnedAt) < WarningInterval {
		return false
	}
	p.warnedAt = now
	return true
}

// ClaimNotice reports whether operators may be told about xuid at now, and
// if so records that they were. Spent on failure, for ClaimWarning's reason.
func (t *Tracker) ClaimNotice(xuid string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.touch(xuid, now)
	if !p.noticedAt.IsZero() && now.Sub(p.noticedAt) < NoticeInterval {
		return false
	}
	p.noticedAt = now
	return true
}

// Len reports how many players are currently tracked.
func (t *Tracker) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.players)
}

// touch returns xuid's state, creating it if needed, and marks them seen.
// Callers hold t.mu.
//
// A claim counts as activity as well as a message: claims are made a moment
// after the message that caused them, and an entry dropped in that gap
// would forget a notice it had just sent.
func (t *Tracker) touch(xuid string, now time.Time) *playerState {
	if now.Sub(t.lastSweep) >= sweepEvery {
		t.sweep(now)
	}
	p, ok := t.players[xuid]
	if !ok {
		if len(t.players) >= t.maxPlayers {
			t.sweep(now)
		}
		if len(t.players) >= t.maxPlayers {
			t.evictOldest()
		}
		p = &playerState{}
		t.players[xuid] = p
	}
	if now.After(p.lastSeen) {
		p.lastSeen = now
	}
	return p
}

// sweep drops every player idle for idleAfter. Callers hold t.mu.
func (t *Tracker) sweep(now time.Time) {
	t.lastSweep = now
	for xuid, p := range t.players {
		if now.Sub(p.lastSeen) >= idleAfter {
			delete(t.players, xuid)
		}
	}
}

// evictOldest drops the least recently seen player. Unlike the idle sweep
// this can forget a live throttle, which is why it only runs when the map is
// full of players who all spoke inside idleAfter -- a state no real server
// reaches. Callers hold t.mu.
func (t *Tracker) evictOldest() {
	var oldest string
	var at time.Time
	for xuid, p := range t.players {
		if oldest == "" || p.lastSeen.Before(at) {
			oldest, at = xuid, p.lastSeen
		}
	}
	delete(t.players, oldest)
}
