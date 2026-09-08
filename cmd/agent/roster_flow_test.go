package main

import (
	"context"
	"testing"

	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"

	"github.com/jdwillmsen/minecraft-server-agent/internal/bus"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
	"github.com/jdwillmsen/minecraft-server-agent/internal/store"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

func addEntry(xuid, username string) protocol.PlayerListEntry {
	return protocol.PlayerListEntry{ActionType: protocol.PlayerListActionAdd, XUID: xuid, Username: username}
}

func removeEntry(xuid string) protocol.PlayerListEntry {
	return protocol.PlayerListEntry{ActionType: protocol.PlayerListActionRemove, XUID: xuid}
}

// drainJoins collects every join already delivered to events. Publish is
// synchronous into a buffered channel, so everything handlePlayerList
// emitted is readable by the time it returns.
func drainJoins(t *testing.T, events <-chan bus.Event) []roster.JoinEvent {
	t.Helper()
	var joins []roster.JoinEvent
	for {
		select {
		case ev := <-events:
			join, ok := ev.(roster.JoinEvent)
			if !ok {
				t.Fatalf("got event %T on the join channel, want roster.JoinEvent", ev)
			}
			joins = append(joins, join)
		default:
			return joins
		}
	}
}

// TestPlayerListFlow drives the same function the live packet loop calls,
// so each case is the end-user behaviour: player list packets in, who the
// welcome plugin would end up greeting out.
func TestPlayerListFlow(t *testing.T) {
	log := logging.New("info")
	siblings := map[string]struct{}{siblingBot: {}}

	// The server reports every already-connected player as an add record
	// immediately after login. Greeting those is a welcome storm at every
	// agent restart, to players who did not just arrive.
	t.Run("opening snapshot greets nobody", func(t *testing.T) {
		eventBus := bus.New()
		events, _ := eventBus.Subscribe(roster.JoinKind, 8)
		playerRoster := roster.New()

		handlePlayerList(context.Background(), &packet.PlayerList{Entries: []protocol.PlayerListEntry{
			addEntry(selfXUID, "Agent"),
			addEntry(playerXUID, "Steve"),
			addEntry("2535411111111111", "Alex"),
		}}, selfXUID, siblings, log, eventBus, playerRoster, store.Nop{})

		if joins := drainJoins(t, events); len(joins) != 0 {
			t.Errorf("got %d joins from the opening snapshot, want 0: %+v", len(joins), joins)
		}
		if name, ok := playerRoster.NameFor(playerXUID); !ok || name != "Steve" {
			t.Errorf("NameFor(player) = (%q, %v), want (Steve, true) — the snapshot must still populate the roster", name, ok)
		}
	})

	t.Run("arrival after the snapshot is published as a join", func(t *testing.T) {
		eventBus := bus.New()
		events, _ := eventBus.Subscribe(roster.JoinKind, 8)
		playerRoster := roster.New()

		handlePlayerList(context.Background(), &packet.PlayerList{Entries: []protocol.PlayerListEntry{
			addEntry(selfXUID, "Agent"),
		}}, selfXUID, siblings, log, eventBus, playerRoster, store.Nop{})
		handlePlayerList(context.Background(), &packet.PlayerList{Entries: []protocol.PlayerListEntry{
			addEntry(playerXUID, "Steve"),
		}}, selfXUID, siblings, log, eventBus, playerRoster, store.Nop{})

		joins := drainJoins(t, events)
		if len(joins) != 1 {
			t.Fatalf("got %d joins, want 1: %+v", len(joins), joins)
		}
		if joins[0].XUID != playerXUID || joins[0].Username != "Steve" {
			t.Errorf("join = %+v, want Steve's arrival", joins[0])
		}
	})

	t.Run("self and sibling arrivals are never published", func(t *testing.T) {
		eventBus := bus.New()
		events, _ := eventBus.Subscribe(roster.JoinKind, 8)
		playerRoster := roster.New()

		handlePlayerList(context.Background(), &packet.PlayerList{Entries: []protocol.PlayerListEntry{
			addEntry(playerXUID, "Steve"),
		}}, selfXUID, siblings, log, eventBus, playerRoster, store.Nop{})
		handlePlayerList(context.Background(), &packet.PlayerList{Entries: []protocol.PlayerListEntry{
			addEntry(selfXUID, "Agent"),
			addEntry(siblingBot, "AfkBot"),
		}}, selfXUID, siblings, log, eventBus, playerRoster, store.Nop{})

		if joins := drainJoins(t, events); len(joins) != 0 {
			t.Errorf("got %d joins, want 0 — the agent and its sibling bots must never be greeted: %+v", len(joins), joins)
		}
	})

	t.Run("leave then rejoin is greeted once", func(t *testing.T) {
		eventBus := bus.New()
		events, _ := eventBus.Subscribe(roster.JoinKind, 8)
		playerRoster := roster.New()

		handlePlayerList(context.Background(), &packet.PlayerList{Entries: []protocol.PlayerListEntry{
			addEntry(selfXUID, "Agent"),
			addEntry(playerXUID, "Steve"),
		}}, selfXUID, siblings, log, eventBus, playerRoster, store.Nop{})
		handlePlayerList(context.Background(), &packet.PlayerList{Entries: []protocol.PlayerListEntry{
			removeEntry(playerXUID),
		}}, selfXUID, siblings, log, eventBus, playerRoster, store.Nop{})
		handlePlayerList(context.Background(), &packet.PlayerList{Entries: []protocol.PlayerListEntry{
			addEntry(playerXUID, "Steve"),
		}}, selfXUID, siblings, log, eventBus, playerRoster, store.Nop{})

		if joins := drainJoins(t, events); len(joins) != 1 {
			t.Errorf("got %d joins across leave+rejoin, want 1: %+v", len(joins), joins)
		}
	})

	// A reconnect must not replay the population as arrivals, and must not
	// keep claiming a player who left while the agent was disconnected.
	t.Run("reconnect greets nobody and forgets who left in the gap", func(t *testing.T) {
		eventBus := bus.New()
		events, _ := eventBus.Subscribe(roster.JoinKind, 8)
		playerRoster := roster.New()

		handlePlayerList(context.Background(), &packet.PlayerList{Entries: []protocol.PlayerListEntry{
			addEntry(selfXUID, "Agent"),
			addEntry(playerXUID, "Steve"),
		}}, selfXUID, siblings, log, eventBus, playerRoster, store.Nop{})

		playerRoster.BeginSession()
		handlePlayerList(context.Background(), &packet.PlayerList{Entries: []protocol.PlayerListEntry{
			addEntry(selfXUID, "Agent"),
			addEntry("2535411111111111", "Alex"),
		}}, selfXUID, siblings, log, eventBus, playerRoster, store.Nop{})

		if joins := drainJoins(t, events); len(joins) != 0 {
			t.Errorf("got %d joins after reconnecting, want 0: %+v", len(joins), joins)
		}
		if _, ok := playerRoster.NameFor(playerXUID); ok {
			t.Error("NameFor for a player absent from the reconnect snapshot = ok, want not-ok — a tellraw aimed at them would reach nobody")
		}
	})
}
