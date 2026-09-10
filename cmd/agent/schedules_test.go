package main

import (
	"context"
	"testing"

	"github.com/jdwillmsen/minecraft-server-agent/internal/announce"
	"github.com/jdwillmsen/minecraft-server-agent/internal/chat"
)

type addRecorder struct {
	announce.Nop
	added []announce.Schedule
}

func (a *addRecorder) AddSchedule(_ context.Context, s announce.Schedule) (int64, error) {
	a.added = append(a.added, s)
	return int64(len(a.added)), nil
}

// An operator connected before the agent logged in has no players row, and
// the schedule's author column is a foreign key into it.
func TestScheduleBookMakesTheAuthorWritableFirst(t *testing.T) {
	players := &recordingPlayers{}
	inner := &addRecorder{}
	book := scheduleBook{ScheduleStore: inner, outbox: testOutbox(&recordingAnnounceStore{}, players, onlineRoster(present(playerXUID, "Steve")))}

	if _, err := book.AddSchedule(t.Context(), announce.Schedule{AuthorXUID: playerXUID}); err != nil {
		t.Fatalf("AddSchedule: %v", err)
	}
	if got := players.all(); len(got) != 1 || got[0] != playerXUID+" as Steve" {
		t.Errorf("ensured %v, want the author", got)
	}

	if _, err := book.AddSchedule(t.Context(), announce.Schedule{AuthorXUID: chat.ServerOrigin}); err != nil {
		t.Fatalf("AddSchedule: %v", err)
	}
	if got := inner.added[1].AuthorXUID; got != "" {
		t.Errorf("console author reached the store as %q, want empty", got)
	}
}
