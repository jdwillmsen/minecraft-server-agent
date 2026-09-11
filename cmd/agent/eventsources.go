package main

import (
	"context"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/adapters"
	"github.com/jdwillmsen/minecraft-server-agent/internal/announce"
	"github.com/jdwillmsen/minecraft-server-agent/internal/config"
	"github.com/jdwillmsen/minecraft-server-agent/internal/sources"
	"github.com/jdwillmsen/minecraft-server-agent/internal/store"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// announcingProfiles is the profile store as this binary uses it: every
// arrival and departure it records is also offered to the player event
// source.
//
// A wrapper around the store rather than a plugin on the join event, because
// the two facts these events need exist only as return values of the writes
// that record them. RecordJoin is the one call that knows an arrival is a
// player's first -- it reads the profile as it stood before counting this
// one -- and RecordLeave the one that knows what the session added. A plugin
// asking again afterwards would read the state those calls just overwrote;
// calling them a second time would count the visit twice.
type announcingProfiles struct {
	store.Store
	events *sources.Events
}

func withPlayerEvents(s store.Store, events *sources.Events) store.Store {
	return announcingProfiles{Store: s, events: events}
}

// RecordJoin satisfies store.Store. A disabled store remembers nobody and so
// reports every arrival as new; it is asked first so that is never
// announced as a stream of first-ever joins. A failed write says nothing
// true about the player either.
func (p announcingProfiles) RecordJoin(ctx context.Context, xuid, gamertag string, at time.Time) (store.Profile, error) {
	profile, err := p.Store.RecordJoin(ctx, xuid, gamertag, at)
	if err == nil && p.Store.Enabled() {
		p.events.Joined(gamertag, profile)
	}
	return profile, err
}

// EnsurePlayer satisfies store.Store. A row it creates is the agent's first
// sight of a player it never watched arrive, so that is when operators hear
// of them; their later join finds the row and says nothing.
func (p announcingProfiles) EnsurePlayer(ctx context.Context, xuid, gamertag string, at time.Time) (bool, error) {
	created, err := p.Store.EnsurePlayer(ctx, xuid, gamertag, at)
	p.firstSeen(gamertag, created, err)
	return created, err
}

// ResumeSession satisfies store.Store, announcing a first sight exactly as
// EnsurePlayer does.
func (p announcingProfiles) ResumeSession(ctx context.Context, xuid, gamertag string, at time.Time) (bool, error) {
	created, err := p.Store.ResumeSession(ctx, xuid, gamertag, at)
	p.firstSeen(gamertag, created, err)
	return created, err
}

func (p announcingProfiles) firstSeen(gamertag string, created bool, err error) {
	if err == nil && created && p.Store.Enabled() {
		p.events.FirstSeenOnline(gamertag)
	}
}

// RecordLeave satisfies store.Store.
func (p announcingProfiles) RecordLeave(ctx context.Context, xuid string, since, at time.Time) (store.Playtime, error) {
	pt, err := p.Store.RecordLeave(ctx, xuid, since, at)
	if err == nil {
		p.events.Left(pt)
	}
	return pt, err
}

// runServerWatcher announces server version changes and stale backups until
// ctx ends, when there is both an outbox to write to and an exporter to
// read. Neither missing is an error: each is a supported deployment, and a
// watcher with nowhere to write or nothing to read would only spend scrapes.
//
// Its own MetricsFacts rather than the plugin context's: the value is
// stateless over the same two URLs and timeout, and reaching into the
// context for a concrete type its interface deliberately hides would be the
// less honest dependency.
func runServerWatcher(ctx context.Context, cfg config.Config, timeout time.Duration, outbox announce.Store, pub sources.Publisher, log *logging.Logger) {
	if !outbox.Enabled() {
		return
	}
	facts := adapters.NewMetricsFacts(adapters.NewMetricsClient(cfg.MCMonitorURL, cfg.BackupExporterURL, timeout))
	if !facts.StatusEnabled() && !facts.BackupEnabled() {
		return
	}
	sources.NewWatcher(facts, pub, log).Run(ctx)
}
