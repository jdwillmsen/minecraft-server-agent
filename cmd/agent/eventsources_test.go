package main

import (
	"context"
	"errors"
	"strings"
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
func (s scriptedProfiles) RecordLeave(context.Context, string, time.Time, time.Time) (store.Playtime, error) {
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

// rememberedProfiles keeps player rows the way Postgres does: a join counts
// and reports the row as it stood before, while a resume or an ensure only
// creates a missing row, with no join counted.
type rememberedProfiles struct {
	store.Nop
	rows map[string]store.Profile
}

func newRememberedProfiles() *rememberedProfiles {
	return &rememberedProfiles{rows: map[string]store.Profile{}}
}

func (r *rememberedProfiles) Enabled() bool { return true }

func (r *rememberedProfiles) RecordJoin(_ context.Context, xuid, gamertag string, at time.Time) (store.Profile, error) {
	prior := r.rows[xuid]
	row := prior
	if row.FirstSeen.IsZero() {
		row.FirstSeen = at
	}
	row.XUID, row.Gamertag, row.LastSeen = xuid, gamertag, at
	row.JoinCount++
	r.rows[xuid] = row
	prior.XUID, prior.Gamertag = xuid, gamertag
	prior.JoinCount++
	return prior, nil
}

func (r *rememberedProfiles) EnsurePlayer(_ context.Context, xuid, gamertag string, at time.Time) (bool, error) {
	if _, ok := r.rows[xuid]; ok {
		return false, nil
	}
	r.rows[xuid] = store.Profile{XUID: xuid, Gamertag: gamertag, FirstSeen: at, LastSeen: at}
	return true, nil
}

func (r *rememberedProfiles) ResumeSession(ctx context.Context, xuid, gamertag string, at time.Time) (bool, error) {
	return r.EnsurePlayer(ctx, xuid, gamertag, at)
}

func wrapRemembered() (store.Store, signallingPublisher) {
	pub := signallingPublisher{got: make(chan announce.Announcement, 4)}
	return withPlayerEvents(newRememberedProfiles(), sources.NewEvents(context.Background(), pub, logging.New("error"))), pub
}

// A player who arrived while the agent was away is first seen in a
// reconnect's snapshot. Operators hear of them once, then, worded for what
// was seen; their later real join finds the row and is not a first time.
func TestPlayerEventsAnnounceASnapshotFirstPlayerOnceAtResume(t *testing.T) {
	s, pub := wrapRemembered()
	now := time.Now()

	if _, err := s.ResumeSession(t.Context(), playerXUID, "Newcomer", now); err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	a := expectPublished(t, pub)
	if a.TargetKind != announce.TargetPermission || a.TargetValue != "operator" || !strings.Contains(a.Body, "already online") {
		t.Errorf("announcement = %s/%s %q, want an operator notice of a player first seen already online", a.TargetKind, a.TargetValue, a.Body)
	}
	if _, err := s.RecordLeave(t.Context(), playerXUID, now, now.Add(time.Hour)); err != nil {
		t.Fatalf("RecordLeave: %v", err)
	}
	if _, err := s.RecordJoin(t.Context(), playerXUID, "Newcomer", now.Add(48*time.Hour)); err != nil {
		t.Fatalf("RecordJoin: %v", err)
	}
	if _, err := s.ResumeSession(t.Context(), playerXUID, "Newcomer", now.Add(72*time.Hour)); err != nil {
		t.Fatalf("second ResumeSession: %v", err)
	}
	expectSilence(t, pub)
}

func TestPlayerEventsAnnounceANormalFirstJoinExactlyOnce(t *testing.T) {
	s, pub := wrapRemembered()
	now := time.Now()

	if _, err := s.RecordJoin(t.Context(), playerXUID, "Newcomer", now); err != nil {
		t.Fatalf("RecordJoin: %v", err)
	}
	if a := expectPublished(t, pub); !strings.Contains(a.Body, "first time") {
		t.Errorf("body %q, want the first-join notice", a.Body)
	}
	if _, err := s.ResumeSession(t.Context(), playerXUID, "Newcomer", now.Add(time.Hour)); err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	if _, err := s.RecordJoin(t.Context(), playerXUID, "Newcomer", now.Add(2*time.Hour)); err != nil {
		t.Fatalf("second RecordJoin: %v", err)
	}
	expectSilence(t, pub)
}

// The outbox ensures a row for an operator already online when an
// announcement names them. That is the agent's first sight of them, and
// their next join must not be mistaken for a first-ever one.
func TestPlayerEventsGiveAnEnsuredPlayerNoFalseFirstJoin(t *testing.T) {
	s, pub := wrapRemembered()
	now := time.Now()

	if _, err := s.EnsurePlayer(t.Context(), playerXUID, "Operator", now); err != nil {
		t.Fatalf("EnsurePlayer: %v", err)
	}
	if a := expectPublished(t, pub); !strings.Contains(a.Body, "already online") {
		t.Errorf("body %q, want the first-seen notice", a.Body)
	}
	if _, err := s.EnsurePlayer(t.Context(), playerXUID, "Operator", now); err != nil {
		t.Fatalf("second EnsurePlayer: %v", err)
	}
	if _, err := s.RecordJoin(t.Context(), playerXUID, "Operator", now.Add(time.Hour)); err != nil {
		t.Fatalf("RecordJoin: %v", err)
	}
	expectSilence(t, pub)
}

func TestPlayerEventsAnnounceAFirstEverJoinToOperators(t *testing.T) {
	s, pub := wrapScripted(scriptedProfiles{enabled: true, profile: store.Profile{JoinCount: 1}})
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
	if _, err := s.RecordLeave(t.Context(), playerXUID, time.Time{}, time.Now()); err != nil {
		t.Fatalf("RecordLeave: %v", err)
	}
	if a := expectPublished(t, pub); a.TargetKind != announce.TargetEveryone {
		t.Errorf("target = %s, want everyone", a.TargetKind)
	}
}
