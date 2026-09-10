package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/announce"
	"github.com/jdwillmsen/minecraft-server-agent/internal/sources"
	"github.com/jdwillmsen/minecraft-server-agent/internal/store"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// scriptedProfiles answers RecordJoin and RecordLeave with fixed values.
type scriptedProfiles struct {
	store.Nop
	enabled bool
	profile store.Profile
	leave   store.Playtime
	err     error
}

func (s scriptedProfiles) Enabled() bool { return s.enabled }
func (s scriptedProfiles) RecordJoin(context.Context, string, string, time.Time) (store.Profile, error) {
	return s.profile, s.err
}
func (s scriptedProfiles) RecordLeave(context.Context, string, time.Time) (store.Playtime, error) {
	return s.leave, s.err
}

// signallingPublisher reports each publish on a channel, since the event
// source publishes off the caller's goroutine.
type signallingPublisher struct{ got chan announce.Announcement }

func (p signallingPublisher) Publish(_ context.Context, a announce.Announcement) (int64, int, error) {
	p.got <- a
	return 1, 0, nil
}

func wrapScripted(s scriptedProfiles) (store.Store, signallingPublisher) {
	pub := signallingPublisher{got: make(chan announce.Announcement, 4)}
	return withPlayerEvents(s, sources.NewEvents(context.Background(), pub, logging.New("error"))), pub
}

func expectPublished(t *testing.T, pub signallingPublisher) announce.Announcement {
	t.Helper()
	select {
	case a := <-pub.got:
		return a
	case <-time.After(5 * time.Second):
		t.Fatal("nothing was published")
		return announce.Announcement{}
	}
}

func expectSilence(t *testing.T, pub signallingPublisher) {
	t.Helper()
	select {
	case a := <-pub.got:
		t.Fatalf("published %+v, want nothing", a)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestPlayerEventsAnnounceAFirstEverJoinToOperators(t *testing.T) {
	s, pub := wrapScripted(scriptedProfiles{enabled: true, profile: store.Profile{JoinCount: 1, FirstSeen: time.Now()}})
	if _, err := s.RecordJoin(t.Context(), playerXUID, "Steve", time.Now()); err != nil {
		t.Fatalf("RecordJoin: %v", err)
	}
	if a := expectPublished(t, pub); a.TargetKind != announce.TargetPermission || a.TargetValue != "operator" {
		t.Errorf("target = %s/%s, want the operator permission", a.TargetKind, a.TargetValue)
	}
}

// With no database, every arrival looks new -- the store remembers nobody.
// Announcing on that would tell operators about a first join every time
// anyone at all logged in.
func TestPlayerEventsIgnoreADisabledStore(t *testing.T) {
	s, pub := wrapScripted(scriptedProfiles{enabled: false})
	if _, err := s.RecordJoin(t.Context(), playerXUID, "Steve", time.Now()); err != nil {
		t.Fatalf("RecordJoin: %v", err)
	}
	expectSilence(t, pub)
}

// A failed write says nothing true about the player: its zero profile would
// otherwise read as a first-ever arrival.
func TestPlayerEventsIgnoreAFailedWrite(t *testing.T) {
	s, pub := wrapScripted(scriptedProfiles{enabled: true, err: errors.New("connection refused")})
	if _, err := s.RecordJoin(t.Context(), playerXUID, "Steve", time.Now()); err == nil {
		t.Fatal("the store's error was swallowed")
	}
	expectSilence(t, pub)
}

func TestPlayerEventsAnnounceAMilestoneOnLeave(t *testing.T) {
	s, pub := wrapScripted(scriptedProfiles{enabled: true, leave: store.Playtime{Gamertag: "Steve", Before: 9 * time.Hour, After: 10 * time.Hour}})
	if _, err := s.RecordLeave(t.Context(), playerXUID, time.Now()); err != nil {
		t.Fatalf("RecordLeave: %v", err)
	}
	if a := expectPublished(t, pub); a.TargetKind != announce.TargetEveryone {
		t.Errorf("target = %s, want everyone", a.TargetKind)
	}
}
