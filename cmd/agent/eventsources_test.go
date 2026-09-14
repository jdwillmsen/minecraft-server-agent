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

func (p signallingPublisher) Publish(_ context.Context, a announce.Announcement) (int64, announce.Reach, error) {
	p.got <- a
	return 1, announce.Reach{Counted: true}, nil
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

// rememberedProfiles keeps players the way Postgres does: a join and a
// resume each record a session, and a join reports how many came before
// it, while an ensure writes a bare row and records no session.
type rememberedProfiles struct {
	store.Nop
	rows     map[string]time.Time
	sessions map[string]int
}

func (r *rememberedProfiles) Enabled() bool { return true }

func (r *rememberedProfiles) row(xuid string, at time.Time) time.Time {
	if _, ok := r.rows[xuid]; !ok {
		r.rows[xuid] = at
	}
	return r.rows[xuid]
}

func (r *rememberedProfiles) RecordJoin(_ context.Context, xuid, gamertag string, at time.Time) (store.Profile, error) {
	_, existed := r.rows[xuid]
	prior := store.Profile{XUID: xuid, Gamertag: gamertag, JoinCount: 1, Sessions: r.sessions[xuid]}
	if existed {
		prior.FirstSeen = r.rows[xuid]
	}
	r.row(xuid, at)
	r.sessions[xuid]++
	return prior, nil
}

func (r *rememberedProfiles) EnsurePlayer(_ context.Context, xuid, _ string, at time.Time) error {
	r.row(xuid, at)
	return nil
}

func (r *rememberedProfiles) ResumeSession(_ context.Context, xuid, _ string, at time.Time) (bool, error) {
	r.row(xuid, at)
	firstSeen := r.sessions[xuid] == 0
	r.sessions[xuid]++
	return firstSeen, nil
}

func wrapRemembered() (*rememberedProfiles, store.Store, signallingPublisher) {
	r := &rememberedProfiles{rows: map[string]time.Time{}, sessions: map[string]int{}}
	pub := signallingPublisher{got: make(chan announce.Announcement, 4)}
	return r, withPlayerEvents(r, sources.NewEvents(context.Background(), pub, logging.New("error"))), pub
}

// A player who arrived while the agent was away is first seen in a
// reconnect's snapshot. Operators hear of them once, then, worded for what
// was seen; their later real join finds that session and is not a first time.
func TestPlayerEventsAnnounceASnapshotFirstPlayerOnceAtResume(t *testing.T) {
	_, s, pub := wrapRemembered()
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
	_, s, pub := wrapRemembered()
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

// The drain's delivery and the welcome's RecordJoin answer the same join on
// different goroutines, so the outbox can write a brand-new joiner's bare
// row first. That row is bookkeeping, not a sighting: the join is still
// announced, once, as the join it was.
func TestPlayerEventsAnnounceAJoinThatLostTheRowRaceAsAJoin(t *testing.T) {
	_, s, pub := wrapRemembered()
	now := time.Now()

	if err := s.EnsurePlayer(t.Context(), playerXUID, "Newcomer", now); err != nil {
		t.Fatalf("EnsurePlayer: %v", err)
	}
	expectSilence(t, pub)
	if _, err := s.RecordJoin(t.Context(), playerXUID, "Newcomer", now); err != nil {
		t.Fatalf("RecordJoin: %v", err)
	}
	if a := expectPublished(t, pub); !strings.Contains(a.Body, "joined the server for the first time") || strings.Contains(a.Body, "already online") {
		t.Errorf("body %q, want the first-join notice", a.Body)
	}
	if err := s.EnsurePlayer(t.Context(), playerXUID, "Newcomer", now); err != nil {
		t.Fatalf("second EnsurePlayer: %v", err)
	}
	expectSilence(t, pub)
}

func TestPlayerEventsSayNothingForAReturningPlayer(t *testing.T) {
	r, s, pub := wrapRemembered()
	now := time.Now()
	r.rows[playerXUID], r.sessions[playerXUID] = now.Add(-100*time.Hour), 3

	if _, err := s.RecordJoin(t.Context(), playerXUID, "Regular", now); err != nil {
		t.Fatalf("RecordJoin: %v", err)
	}
	if _, err := s.ResumeSession(t.Context(), playerXUID, "Regular", now.Add(time.Hour)); err != nil {
		t.Fatalf("ResumeSession: %v", err)
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
