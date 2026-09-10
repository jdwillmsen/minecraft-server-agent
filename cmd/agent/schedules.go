package main

import (
	"context"

	"github.com/jdwillmsen/minecraft-server-agent/internal/announce"
	"github.com/jdwillmsen/minecraft-server-agent/internal/chat"
	"github.com/jdwillmsen/minecraft-server-agent/internal/sources"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// scheduleBook is the schedule store as this binary uses it: a schedule
// names its author, and announcement_schedules.author_xuid is a foreign key
// into minecraft.players, so the author is made writable first -- the same
// operator-already-online case the outbox exists for.
type scheduleBook struct {
	announce.ScheduleStore
	outbox *outbox
}

func (b scheduleBook) AddSchedule(ctx context.Context, s announce.Schedule) (int64, error) {
	if s.AuthorXUID == chat.ServerOrigin {
		s.AuthorXUID = ""
	}
	b.outbox.ensure(ctx, s.AuthorXUID)
	return b.ScheduleStore.AddSchedule(ctx, s)
}

// runScheduler fires due schedules until ctx ends. With no schedule store
// there is nothing to fire, and the loop is not started at all.
func runScheduler(ctx context.Context, schedules announce.ScheduleStore, send sources.Sender, log *logging.Logger) {
	if !schedules.Enabled() {
		return
	}
	sources.NewScheduler(schedules, send, log).Run(ctx)
}
