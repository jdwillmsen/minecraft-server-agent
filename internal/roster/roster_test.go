package roster

import (
	"testing"
	"time"
)

// agentEntry stands in for this process's own player list record, which is
// always part of the server's opening snapshot.
var agentEntry = PlayerListEntry{XUID: "999", Username: "Agent"}

// absorbSnapshot consumes the session's opening PlayerList, which reports
// the already-connected population and must never be read as arrivals, so
// the test can go on to exercise genuine joins.
func absorbSnapshot(t *testing.T, r *Roster, entries ...PlayerListEntry) {
	t.Helper()
	if joins, _, _ := r.Apply(entries); len(joins) != 0 {
		t.Fatalf("session snapshot reported %d joins, want 0", len(joins))
	}
}

func TestApply_NewPlayerReportedAsJoin(t *testing.T) {
	r := New()
	absorbSnapshot(t, r, agentEntry)

	joins, _, _ := r.Apply([]PlayerListEntry{{XUID: "111", Username: "Steve"}})
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

	joins, _, _ := r.Apply([]PlayerListEntry{{XUID: "111", Username: "Steve"}})
	if len(joins) != 0 {
		t.Errorf("got %d joins on a repeated add, want 0", len(joins))
	}
}

func TestApply_RemoveThenReAddReportsJoinAgain(t *testing.T) {
	r := New()
	absorbSnapshot(t, r, agentEntry, PlayerListEntry{XUID: "111", Username: "Steve"})
	r.Apply([]PlayerListEntry{{XUID: "111", Remove: true}})

	joins, _, _ := r.Apply([]PlayerListEntry{{XUID: "111", Username: "Steve"}})
	if len(joins) != 1 {
		t.Errorf("got %d joins after leave+rejoin, want 1", len(joins))
	}
}

// The shape the server actually sends: the add carries XUID and UUID, the
// removal carries only the UUID. Every departure used to be dropped here.
func TestApply_RemovalCarryingOnlyAUUIDIsALeave(t *testing.T) {
	r := New()
	absorbSnapshot(t, r, agentEntry, PlayerListEntry{XUID: "111", Username: "Steve", UUID: "u-111"})

	_, leaves, _ := r.Apply([]PlayerListEntry{{UUID: "u-111", Remove: true}})
	if len(leaves) != 1 || leaves[0].XUID != "111" || leaves[0].Username != "Steve" {
		t.Fatalf("leaves = %+v, want Steve (111)", leaves)
	}
	if _, ok := r.NameFor("111"); ok {
		t.Error("Steve still on the roster after a UUID-only removal")
	}

	joins, _, _ := r.Apply([]PlayerListEntry{{XUID: "111", Username: "Steve", UUID: "u-111"}})
	if len(joins) != 1 {
		t.Errorf("got %d joins for a rejoin after a UUID-only removal, want 1: a rejoin is what triggers the welcome and the announcement drain", len(joins))
	}
}

func TestApply_RemovalOfAnUnknownUUIDIsIgnored(t *testing.T) {
	r := New()
	absorbSnapshot(t, r, agentEntry, PlayerListEntry{XUID: "111", Username: "Steve", UUID: "u-111"})

	if _, leaves, _ := r.Apply([]PlayerListEntry{{UUID: "u-unknown", Remove: true}}); len(leaves) != 0 {
		t.Errorf("leaves = %+v for a UUID nobody was recorded under, want none", leaves)
	}
	if _, ok := r.NameFor("111"); !ok {
		t.Error("an unrelated removal took Steve off the roster")
	}
}

// A removal before the snapshot is not the snapshot. If it consumed it, the
// real snapshot behind it would be read as a burst of arrivals.
func TestApply_RemovalDoesNotConsumeTheSnapshot(t *testing.T) {
	r := New()
	r.Apply([]PlayerListEntry{{UUID: "u-111", Remove: true}})

	if joins, _, _ := r.Apply([]PlayerListEntry{agentEntry, {XUID: "111", Username: "Steve", UUID: "u-111"}}); len(joins) != 0 {
		t.Errorf("got %d joins from the snapshot after a stray removal, want 0", len(joins))
	}
}

func TestBeginSession_ForgetsUUIDs(t *testing.T) {
	r := New()
	absorbSnapshot(t, r, agentEntry, PlayerListEntry{XUID: "111", Username: "Steve", UUID: "u-111"})
	r.BeginSession(time.Now(), agentEntry.XUID)
	absorbSnapshot(t, r, agentEntry)

	if _, leaves, _ := r.Apply([]PlayerListEntry{{UUID: "u-111", Remove: true}}); len(leaves) != 0 {
		t.Errorf("leaves = %+v resolved through a previous session's UUID, want none", leaves)
	}
}

func TestApply_BlankXUIDIgnored(t *testing.T) {
	r := New()
	absorbSnapshot(t, r, agentEntry)

	joins, _, _ := r.Apply([]PlayerListEntry{{XUID: "", Username: "Nobody"}})
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

	joins, _, _ := r.Apply([]PlayerListEntry{
		{XUID: "111", Username: "Steve"},
		{XUID: "222", Username: "Alex"},
	})
	if len(joins) != 2 {
		t.Fatalf("got %d joins, want 2", len(joins))
	}
}

func TestApply_SessionSnapshotRecordsPlayersWithoutReportingJoins(t *testing.T) {
	r := New()

	joins, _, _ := r.Apply([]PlayerListEntry{
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

	if joins, _, _ := r.Apply(nil); len(joins) != 0 {
		t.Fatalf("got %d joins from an empty packet, want 0", len(joins))
	}

	joins, _, _ := r.Apply([]PlayerListEntry{agentEntry, {XUID: "111", Username: "Steve"}})
	if len(joins) != 0 {
		t.Errorf("got %d joins, want 0 — the empty packet must not have consumed the snapshot", len(joins))
	}
}

func TestBeginSession_ForgetsThePreviousSessionsPlayers(t *testing.T) {
	r := New()
	absorbSnapshot(t, r, agentEntry, PlayerListEntry{XUID: "111", Username: "Steve"})

	r.BeginSession(time.Now(), agentEntry.XUID)

	if _, ok := r.NameFor("111"); ok {
		t.Error("NameFor after BeginSession = ok, want not-ok — a player who may have left while disconnected must not still resolve")
	}
}

func TestBeginSession_NextSnapshotIsNotReportedAsJoins(t *testing.T) {
	r := New()
	absorbSnapshot(t, r, agentEntry, PlayerListEntry{XUID: "111", Username: "Steve"})
	r.BeginSession(time.Now(), agentEntry.XUID)

	joins, _, _ := r.Apply([]PlayerListEntry{agentEntry, {XUID: "111", Username: "Steve"}})
	if len(joins) != 0 {
		t.Fatalf("got %d joins from the reconnect snapshot, want 0", len(joins))
	}

	joins, _, _ = r.Apply([]PlayerListEntry{{XUID: "222", Username: "Alex"}})
	if len(joins) != 1 {
		t.Errorf("got %d joins for an arrival after the reconnect snapshot, want 1", len(joins))
	}
}

// Whoever the snapshot shows is still playing, and their session has to be
// reopened -- so they are reported, once, as present rather than dropped.
func TestApply_SnapshotReportsPresentPlayersOnce(t *testing.T) {
	r := New()

	_, _, present := r.Apply([]PlayerListEntry{agentEntry, {XUID: "111", Username: "Steve"}, {XUID: "111", Username: "Steve"}})
	want := []Entry{{XUID: agentEntry.XUID, Username: agentEntry.Username}, {XUID: "111", Username: "Steve"}}
	if len(present) != len(want) || present[0] != want[0] || present[1] != want[1] {
		t.Errorf("present = %+v, want %+v", present, want)
	}

	joins, _, present := r.Apply([]PlayerListEntry{{XUID: "222", Username: "Alex"}})
	if len(present) != 0 || len(joins) != 1 {
		t.Errorf("after the snapshot: joins = %+v, present = %+v; want one join and nobody merely present", joins, present)
	}

	r.BeginSession(time.Now(), agentEntry.XUID)
	if _, _, present := r.Apply([]PlayerListEntry{{XUID: "111", Username: "Steve"}}); len(present) != 1 {
		t.Errorf("reconnect snapshot present = %+v, want Steve", present)
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

func containsXUID(xuids []string, want string) bool {
	for _, x := range xuids {
		if x == want {
			return true
		}
	}
	return false
}

func TestOnline_ReportsEveryPresentXUID(t *testing.T) {
	r := New()
	absorbSnapshot(t, r, agentEntry, PlayerListEntry{XUID: "111", Username: "Steve"})
	r.Apply([]PlayerListEntry{{XUID: "222", Username: "Alex"}})

	online := r.Online()
	if len(online) != 3 {
		t.Fatalf("Online() = %v, want 3 entries", online)
	}
	for _, want := range []string{agentEntry.XUID, "111", "222"} {
		if !containsXUID(online, want) {
			t.Errorf("Online() = %v, missing %q", online, want)
		}
	}
}

func TestOnline_OmitsPlayersWhoLeft(t *testing.T) {
	r := New()
	absorbSnapshot(t, r, agentEntry, PlayerListEntry{XUID: "111", Username: "Steve"})
	r.Apply([]PlayerListEntry{{XUID: "111", Remove: true}})

	if online := r.Online(); containsXUID(online, "111") {
		t.Errorf("Online() = %v, want 111 absent after leaving", online)
	}
}

func TestXUIDFor_ResolvesTheReverseOfNameFor(t *testing.T) {
	r := New()
	absorbSnapshot(t, r, agentEntry, PlayerListEntry{XUID: "111", Username: "Steve"})

	xuid, ok := r.XUIDFor("Steve")
	if !ok || xuid != "111" {
		t.Errorf("XUIDFor(Steve) = (%q, %v), want (111, true)", xuid, ok)
	}
}

func TestXUIDFor_UnknownNameNotOK(t *testing.T) {
	r := New()
	absorbSnapshot(t, r, agentEntry)

	if _, ok := r.XUIDFor("NoSuchPlayer"); ok {
		t.Error("XUIDFor on an unknown name = ok, want not-ok")
	}
}

func TestXUIDFor_StaleAfterRemove(t *testing.T) {
	r := New()
	absorbSnapshot(t, r, agentEntry, PlayerListEntry{XUID: "111", Username: "Steve"})
	r.Apply([]PlayerListEntry{{XUID: "111", Remove: true}})

	if _, ok := r.XUIDFor("Steve"); ok {
		t.Error("XUIDFor after removal = ok, want not-ok")
	}
}

// The shape a Bedrock server actually sends a joining client, captured off
// the wire: the client's own entry alone, then the whole roster with that
// same entry at its head. Reading the first of those as the entire snapshot
// left everyone already online to arrive in the second, where each was
// counted as a fresh arrival on every single connect.
func TestApply_AgentsOwnEntryDoesNotEndTheOpeningSnapshot(t *testing.T) {
	r := New()
	r.BeginSession(time.Now(), agentEntry.XUID)

	if joins, _, _ := r.Apply([]PlayerListEntry{agentEntry}); len(joins) != 0 {
		t.Fatalf("got %d joins from the agent's own entry arriving alone, want 0", len(joins))
	}

	joins, _, present := r.Apply([]PlayerListEntry{
		agentEntry,
		{XUID: "111", Username: "Steve"},
		{XUID: "222", Username: "Alex"},
	})
	if len(joins) != 0 {
		t.Fatalf("got %d joins from the roster behind the agent's own entry, want 0 — those players were already online: %+v", len(joins), joins)
	}
	if len(present) != 2 || present[0].XUID != "111" || present[1].XUID != "222" {
		t.Fatalf("present = %+v, want Steve and Alex", present)
	}

	joins, _, present = r.Apply([]PlayerListEntry{{XUID: "333", Username: "Notch"}})
	if len(joins) != 1 || joins[0].XUID != "333" {
		t.Fatalf("joins = %+v, want the arrival after the burst reported as one", joins)
	}
	if len(present) != 0 {
		t.Errorf("present = %+v, want none: the opening burst is over", present)
	}
}

// An empty server sends the agent its own entry twice and nothing else, so
// the agent's entry is the only thing that can mark where the burst ends.
// The first player of the day arrives long afterwards and is a genuine join:
// suppressing that greeting would trade this bug for its mirror image.
func TestApply_FirstArrivalOnAnEmptyServerIsAJoin(t *testing.T) {
	r := New()
	r.BeginSession(time.Now(), agentEntry.XUID)
	r.Apply([]PlayerListEntry{agentEntry})
	r.Apply([]PlayerListEntry{agentEntry})

	joins, _, present := r.Apply([]PlayerListEntry{{XUID: "111", Username: "Steve"}})
	if len(joins) != 1 || joins[0].XUID != "111" {
		t.Fatalf("joins = %+v, want Steve greeted as the arrival he is", joins)
	}
	if len(present) != 0 {
		t.Errorf("present = %+v, want none: Steve was not already online", present)
	}
}

// A departure carries no adds, so it can neither end the opening burst nor
// be mistaken for the traffic that does.
func TestApply_DepartureDoesNotEndTheOpeningSnapshot(t *testing.T) {
	r := New()
	r.BeginSession(time.Now(), agentEntry.XUID)
	r.Apply([]PlayerListEntry{agentEntry, {XUID: "111", Username: "Steve", UUID: "u-111"}})
	r.Apply([]PlayerListEntry{{UUID: "u-111", Remove: true}})

	_, _, present := r.Apply([]PlayerListEntry{agentEntry, {XUID: "222", Username: "Alex"}})
	if len(present) != 1 || present[0].XUID != "222" {
		t.Errorf("present = %+v, want Alex: a removal must not end the burst behind it", present)
	}
}
