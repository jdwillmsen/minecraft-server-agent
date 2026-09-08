package roster

import "testing"

// agentEntry stands in for this process's own player list record, which is
// always part of the server's opening snapshot.
var agentEntry = PlayerListEntry{XUID: "999", Username: "Agent"}

// absorbSnapshot consumes the session's opening PlayerList, which reports
// the already-connected population and must never be read as arrivals, so
// the test can go on to exercise genuine joins.
func absorbSnapshot(t *testing.T, r *Roster, entries ...PlayerListEntry) {
	t.Helper()
	if joins, _ := r.Apply(entries); len(joins) != 0 {
		t.Fatalf("session snapshot reported %d joins, want 0", len(joins))
	}
}

func TestApply_NewPlayerReportedAsJoin(t *testing.T) {
	r := New()
	absorbSnapshot(t, r, agentEntry)

	joins, _ := r.Apply([]PlayerListEntry{{XUID: "111", Username: "Steve"}})
	if len(joins) != 1 {
		t.Fatalf("got %d joins, want 1", len(joins))
	}
	if joins[0] != (Entry{XUID: "111", Username: "Steve"}) {
		t.Errorf("join = %+v, want {111 Steve}", joins[0])
	}
}

func TestApply_KnownPlayerNotReportedAgain(t *testing.T) {
	r := New()
	absorbSnapshot(t, r, agentEntry)
	r.Apply([]PlayerListEntry{{XUID: "111", Username: "Steve"}})

	joins, _ := r.Apply([]PlayerListEntry{{XUID: "111", Username: "Steve"}})
	if len(joins) != 0 {
		t.Errorf("got %d joins on a repeated add, want 0", len(joins))
	}
}

func TestApply_RemoveThenReAddReportsJoinAgain(t *testing.T) {
	r := New()
	absorbSnapshot(t, r, agentEntry, PlayerListEntry{XUID: "111", Username: "Steve"})
	r.Apply([]PlayerListEntry{{XUID: "111", Remove: true}})

	joins, _ := r.Apply([]PlayerListEntry{{XUID: "111", Username: "Steve"}})
	if len(joins) != 1 {
		t.Errorf("got %d joins after leave+rejoin, want 1", len(joins))
	}
}

func TestApply_BlankXUIDIgnored(t *testing.T) {
	r := New()
	absorbSnapshot(t, r, agentEntry)

	joins, _ := r.Apply([]PlayerListEntry{{XUID: "", Username: "Nobody"}})
	if len(joins) != 0 {
		t.Errorf("got %d joins for a blank XUID, want 0", len(joins))
	}
	if _, ok := r.NameFor(""); ok {
		t.Error("NameFor(\"\") = ok, want not-ok — a blank XUID must never resolve")
	}
}

func TestApply_MultipleNewPlayersInOneUpdate(t *testing.T) {
	r := New()
	absorbSnapshot(t, r, agentEntry)

	joins, _ := r.Apply([]PlayerListEntry{
		{XUID: "111", Username: "Steve"},
		{XUID: "222", Username: "Alex"},
	})
	if len(joins) != 2 {
		t.Fatalf("got %d joins, want 2", len(joins))
	}
}

func TestApply_SessionSnapshotRecordsPlayersWithoutReportingJoins(t *testing.T) {
	r := New()

	joins, _ := r.Apply([]PlayerListEntry{
		agentEntry,
		{XUID: "111", Username: "Steve"},
		{XUID: "222", Username: "Alex"},
	})
	if len(joins) != 0 {
		t.Fatalf("got %d joins from the opening snapshot, want 0 — those players were already online", len(joins))
	}

	for xuid, want := range map[string]string{"111": "Steve", "222": "Alex"} {
		if name, ok := r.NameFor(xuid); !ok || name != want {
			t.Errorf("NameFor(%s) = (%q, %v), want (%q, true)", xuid, name, ok, want)
		}
	}
}

func TestApply_EmptyPacketDoesNotConsumeTheSnapshot(t *testing.T) {
	r := New()

	if joins, _ := r.Apply(nil); len(joins) != 0 {
		t.Fatalf("got %d joins from an empty packet, want 0", len(joins))
	}

	joins, _ := r.Apply([]PlayerListEntry{agentEntry, {XUID: "111", Username: "Steve"}})
	if len(joins) != 0 {
		t.Errorf("got %d joins, want 0 — the empty packet must not have consumed the snapshot", len(joins))
	}
}

func TestBeginSession_ForgetsThePreviousSessionsPlayers(t *testing.T) {
	r := New()
	absorbSnapshot(t, r, agentEntry, PlayerListEntry{XUID: "111", Username: "Steve"})

	r.BeginSession()

	if _, ok := r.NameFor("111"); ok {
		t.Error("NameFor after BeginSession = ok, want not-ok — a player who may have left while disconnected must not still resolve")
	}
}

func TestBeginSession_NextSnapshotIsNotReportedAsJoins(t *testing.T) {
	r := New()
	absorbSnapshot(t, r, agentEntry, PlayerListEntry{XUID: "111", Username: "Steve"})
	r.BeginSession()

	joins, _ := r.Apply([]PlayerListEntry{agentEntry, {XUID: "111", Username: "Steve"}})
	if len(joins) != 0 {
		t.Fatalf("got %d joins from the reconnect snapshot, want 0", len(joins))
	}

	joins, _ = r.Apply([]PlayerListEntry{{XUID: "222", Username: "Alex"}})
	if len(joins) != 1 {
		t.Errorf("got %d joins for an arrival after the reconnect snapshot, want 1", len(joins))
	}
}

func TestNameFor_UnknownXUID(t *testing.T) {
	r := New()
	if _, ok := r.NameFor("does-not-exist"); ok {
		t.Error("NameFor on an unknown XUID = ok, want not-ok")
	}
}

func TestNameFor_KnownXUIDAfterRemove(t *testing.T) {
	r := New()
	absorbSnapshot(t, r, agentEntry, PlayerListEntry{XUID: "111", Username: "Steve"})
	r.Apply([]PlayerListEntry{{XUID: "111", Remove: true}})

	if _, ok := r.NameFor("111"); ok {
		t.Error("NameFor after removal = ok, want not-ok")
	}
}

func TestApply_UsernameUpdatesOnReAdd(t *testing.T) {
	r := New()
	absorbSnapshot(t, r, agentEntry, PlayerListEntry{XUID: "111", Username: "OldName"})
	r.Apply([]PlayerListEntry{{XUID: "111", Username: "NewName"}})

	name, ok := r.NameFor("111")
	if !ok || name != "NewName" {
		t.Errorf("NameFor(111) = (%q, %v), want (NewName, true)", name, ok)
	}
}
