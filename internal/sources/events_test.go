package sources

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jdwillmsen/minecraft-server-agent/internal/adapters"
	"github.com/jdwillmsen/minecraft-server-agent/internal/announce"
	"github.com/jdwillmsen/minecraft-server-agent/internal/store"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// recordingPublisher keeps every announcement it was handed, so a test
// asserts on what a source described rather than on how many rows exist.
type recordingPublisher struct {
	mu   sync.Mutex
	got  []announce.Announcement
	err  error
	sent chan struct{}
}

func (p *recordingPublisher) Publish(_ context.Context, a announce.Announcement) (int64, announce.Reach, error) {
	p.mu.Lock()
	p.got = append(p.got, a)
	n := len(p.got)
	p.mu.Unlock()
	if p.sent != nil {
		p.sent <- struct{}{}
	}
	return int64(n), announce.Reach{Counted: true}, p.err
}

func (p *recordingPublisher) all() []announce.Announcement {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]announce.Announcement(nil), p.got...)
}

func inlineEvents(pub Publisher) *Events {
	e := NewEvents(context.Background(), pub, logging.New("error"))
	e.spawn = func(f func()) { f() }
	return e
}

func TestCrossedMilestone(t *testing.T) {
	h := time.Hour
	cases := []struct {
		name          string
		before, after time.Duration
		want          time.Duration
	}{
		{"short of the first", 0, 9*h + 59*time.Minute, 0},
		{"exactly at a milestone counts", 9 * h, 10 * h, 10 * h},
		{"leaving from exactly a milestone does not re-fire", 10 * h, 11 * h, 0},
		{"crossing one", 20 * h, 30 * h, 24 * h},
		{"crossing two at once reports only the highest", 5 * h, 30 * h, 24 * h},
		{"crossing every milestone at once", 0, 600 * h, 500 * h},
		{"past the last", 500 * h, 900 * h, 0},
		{"nothing added", 50 * h, 50 * h, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := CrossedMilestone(tc.before, tc.after)
			if got != tc.want || ok != (tc.want > 0) {
				t.Errorf("CrossedMilestone(%v, %v) = (%v, %v), want %v", tc.before, tc.after, got, ok, tc.want)
			}
		})
	}
}

func TestLeftAnnouncesAMilestoneToEveryone(t *testing.T) {
	pub := &recordingPublisher{}
	before := time.Now()
	inlineEvents(pub).Left(store.Playtime{Gamertag: "Steve", Before: 5 * time.Hour, After: 30 * time.Hour})

	got := pub.all()
	if len(got) != 1 {
		t.Fatalf("published %d announcements, want exactly 1 for two milestones crossed at once", len(got))
	}
	a := got[0]
	if a.Source != announce.SourceEvent || a.TargetKind != announce.TargetEveryone || a.Delivery != announce.DeliveryBroadcast {
		t.Errorf("announcement = %s/%s/%s, want event/everyone/broadcast", a.Source, a.TargetKind, a.Delivery)
	}
	if !strings.Contains(a.Body, "Steve") || !strings.Contains(a.Body, "24 hours") {
		t.Errorf("body %q should name the player and the highest milestone", a.Body)
	}
	if a.ExpiresAt == nil || a.ExpiresAt.Sub(before) < 23*time.Hour || a.ExpiresAt.Sub(before) > 25*time.Hour {
		t.Errorf("expiry = %v, want about 24h out", a.ExpiresAt)
	}
}

func TestLeftWithoutACrossingSaysNothing(t *testing.T) {
	pub := &recordingPublisher{}
	e := inlineEvents(pub)
	e.Left(store.Playtime{Gamertag: "Steve", Before: 11 * time.Hour, After: 12 * time.Hour})
	// A leave that closed no session carries no name, whatever the totals.
	e.Left(store.Playtime{Before: 0, After: 10 * time.Hour})
	if got := pub.all(); len(got) != 0 {
		t.Errorf("published %+v, want nothing", got)
	}
}

func TestJoinedWhispersOperatorsOnAFirstEverArrivalOnly(t *testing.T) {
	pub := &recordingPublisher{}
	e := inlineEvents(pub)
	now := time.Now()

	e.Joined("Returner", store.Profile{JoinCount: 2, FirstSeen: now.Add(-time.Hour), Sessions: 1})
	if got := pub.all(); len(got) != 0 {
		t.Fatalf("a returning player was announced: %+v", got)
	}
	// A session but no counted join: first met already online, and
	// announced then. Their first watched arrival is not news a second time.
	e.Joined("SeenOnline", store.Profile{JoinCount: 1, FirstSeen: now.Add(-time.Hour), Sessions: 1})
	if got := pub.all(); len(got) != 0 {
		t.Fatalf("a player with a recorded session was announced as new: %+v", got)
	}

	// A bare row written for an announcement, with no session: the join is
	// still the first sight of them.
	e.Joined("Newcomer", store.Profile{JoinCount: 1, FirstSeen: now.Add(-time.Minute)})
	got := pub.all()
	if len(got) != 1 {
		t.Fatalf("published %d announcements, want 1", len(got))
	}
	a := got[0]
	if a.TargetKind != announce.TargetPermission || a.TargetValue != "operator" || a.Delivery != announce.DeliveryWhisper {
		t.Errorf("target = %s/%s/%s, want a whisper to the operator permission", a.TargetKind, a.TargetValue, a.Delivery)
	}
	if !strings.Contains(a.Body, "Newcomer") {
		t.Errorf("body %q should name the new player", a.Body)
	}
}

func TestFirstSeenOnlineWhispersOperatorsWithoutClaimingAJoin(t *testing.T) {
	pub := &recordingPublisher{}
	before := time.Now()
	inlineEvents(pub).FirstSeenOnline("Stranger")

	got := pub.all()
	if len(got) != 1 {
		t.Fatalf("published %d announcements, want 1", len(got))
	}
	a := got[0]
	if a.Source != announce.SourceEvent || a.TargetKind != announce.TargetPermission || a.TargetValue != "operator" || a.Delivery != announce.DeliveryWhisper {
		t.Errorf("announcement = %s/%s/%s/%s, want an event whispered to the operator permission", a.Source, a.TargetKind, a.TargetValue, a.Delivery)
	}
	if !strings.Contains(a.Body, "Stranger") || !strings.Contains(a.Body, "already online") || strings.Contains(a.Body, "joined") {
		t.Errorf("body %q should name the player as first seen already online, not as having joined", a.Body)
	}
	if a.ExpiresAt == nil || a.ExpiresAt.Sub(before) < 23*time.Hour || a.ExpiresAt.Sub(before) > 25*time.Hour {
		t.Errorf("expiry = %v, want about 24h out, as a first-join notice has", a.ExpiresAt)
	}
}

// The production spawn is a goroutine; this is the one test that proves the
// publish still happens off the caller.
func TestEventsPublishOffTheCallersGoroutine(t *testing.T) {
	pub := &recordingPublisher{sent: make(chan struct{}, 1)}
	NewEvents(context.Background(), pub, logging.New("error")).
		Joined("Newcomer", store.Profile{JoinCount: 1})
	select {
	case <-pub.sent:
	case <-time.After(5 * time.Second):
		t.Fatal("the first-join announcement was never published")
	}
}

// fakeState is a scripted exporter: each field is what the next reading
// returns.
type fakeState struct {
	statusOn, backupOn bool
	version            string
	versionErr         error
	backup             adapters.BackupState
	backupErr          error
}

func (f *fakeState) StatusEnabled() bool { return f.statusOn }
func (f *fakeState) BackupEnabled() bool { return f.backupOn }
func (f *fakeState) CurrentVersion(context.Context) (string, error) {
	return f.version, f.versionErr
}
func (f *fakeState) BackupState(context.Context) (adapters.BackupState, error) {
	return f.backup, f.backupErr
}

func freshBackup() adapters.BackupState {
	return adapters.BackupState{Completed: true, Age: time.Hour, MaxAge: 26 * time.Hour}
}

func staleBackup() adapters.BackupState {
	return adapters.BackupState{Completed: true, Age: 30 * time.Hour, MaxAge: 26 * time.Hour}
}

func TestWatcherAnnouncesAVersionChangeOncePerChange(t *testing.T) {
	state := &fakeState{statusOn: true, version: "1.21.100"}
	pub := &recordingPublisher{}
	w := NewWatcher(state, pub, logging.New("error"))

	steps := []struct {
		version string
		err     error
		want    int // total published after this poll
	}{
		{"1.21.100", nil, 0}, // baseline
		{"1.21.100", nil, 0},
		{"1.21.101", nil, 1},
		{"1.21.101", nil, 1},                 // same news is not news twice
		{"", errors.New("scrape failed"), 1}, // a failed read is not a change
		{"1.21.101", nil, 1},
		{"1.21.102", nil, 2},
	}
	for i, s := range steps {
		state.version, state.versionErr = s.version, s.err
		w.Poll(context.Background())
		if got := len(pub.all()); got != s.want {
			t.Fatalf("poll %d (%q): %d published, want %d", i, s.version, got, s.want)
		}
	}
	last := pub.all()[1]
	if last.TargetKind != announce.TargetEveryone || !strings.Contains(last.Body, "1.21.102") {
		t.Errorf("announcement = %s %q, want a broadcast naming the new version", last.TargetKind, last.Body)
	}
}

// A restart builds a new watcher. Whatever the server looks like at that
// moment -- a version the old process already announced, a backup already
// stale -- is the baseline, not news.
func TestWatcherTakesABaselineAfterARestart(t *testing.T) {
	state := &fakeState{statusOn: true, backupOn: true, version: "1.21.101", backup: staleBackup()}
	pub := &recordingPublisher{}
	restarted := NewWatcher(state, pub, logging.New("error"))

	restarted.Poll(context.Background())
	restarted.Poll(context.Background())
	if got := pub.all(); len(got) != 0 {
		t.Errorf("a restart re-announced old news: %+v", got)
	}
}

func TestWatcherAnnouncesAStaleBackupOnceUntilItRecovers(t *testing.T) {
	state := &fakeState{backupOn: true, backup: freshBackup()}
	pub := &recordingPublisher{}
	w := NewWatcher(state, pub, logging.New("error"))

	steps := []struct {
		name   string
		backup adapters.BackupState
		err    error
		want   int
	}{
		{"baseline fresh", freshBackup(), nil, 0},
		{"goes stale", staleBackup(), nil, 1},
		{"stays stale", staleBackup(), nil, 1},
		{"exporter blip is not a recovery", adapters.BackupState{}, errors.New("timeout"), 1},
		{"no policy published is not a recovery", adapters.BackupState{Completed: true, Age: time.Hour}, nil, 1},
		{"still stale", staleBackup(), nil, 1},
		{"recovers", freshBackup(), nil, 1},
		{"stale again", staleBackup(), nil, 2},
	}
	for _, s := range steps {
		state.backup, state.backupErr = s.backup, s.err
		w.Poll(context.Background())
		if got := len(pub.all()); got != s.want {
			t.Fatalf("%s: %d published, want %d", s.name, got, s.want)
		}
	}
	a := pub.all()[0]
	if a.TargetKind != announce.TargetPermission || a.TargetValue != "operator" {
		t.Errorf("target = %s/%s, want the operator permission", a.TargetKind, a.TargetValue)
	}
}

// An exporter that is not configured is never asked, so a deployment
// without one spends nothing polling it.
func TestWatcherSkipsUnconfiguredExporters(t *testing.T) {
	state := &fakeState{versionErr: errors.New("must not be asked"), backupErr: errors.New("must not be asked")}
	pub := &recordingPublisher{}
	w := NewWatcher(state, pub, logging.New("error"))
	for i := 0; i < 3; i++ {
		w.Poll(context.Background())
	}
	if got := pub.all(); len(got) != 0 {
		t.Errorf("published %+v with no exporter configured", got)
	}
}

// A publish into a database that cannot take it yet must not stop the
// watcher: the state still advances, so the change is not re-announced on
// every later poll either.
func TestWatcherSurvivesAnUnreadyStore(t *testing.T) {
	state := &fakeState{statusOn: true, version: "1"}
	pub := &recordingPublisher{err: fmt.Errorf("announce: insert: %w", &pgconn.PgError{Code: "42P01"})}
	w := NewWatcher(state, pub, logging.New("error"))

	w.Poll(context.Background())
	state.version = "2"
	w.Poll(context.Background())
	w.Poll(context.Background())
	if got := len(pub.all()); got != 1 {
		t.Errorf("attempted %d publishes, want 1: a failed one is not retried as if it were news", got)
	}
}
