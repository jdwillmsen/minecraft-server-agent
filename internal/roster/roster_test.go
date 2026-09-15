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
	if r.IsOnline("111") {
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
	if !r.IsOnline("111") {
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

	if r.IsOnline("111") {
		t.Error("IsOnline after BeginSession = true, want false — a player who may have left while disconnected must not still count as present")
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

// Presence and identity stop being true at different moments: a departure
// ends the first and leaves the second alone, because a tellraw goes out over
// the console bridge and still needs a gamertag to aim at.
func TestNameForOutlivesPresenceAfterRemove(t *testing.T) {
	r := New()
	absorbSnapshot(t, r, agentEntry, PlayerListEntry{XUID: "111", Username: "Steve"})
	r.Apply([]PlayerListEntry{{XUID: "111", Remove: true}})

	if r.IsOnline("111") {
		t.Error("IsOnline after removal = true, want false")
	}
	if name, ok := r.NameFor("111"); !ok || name != "Steve" {
		t.Errorf("NameFor after removal = (%q, %v), want (Steve, true)", name, ok)
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

// A connection that dies takes the roster's knowledge with it. Kept, it
// would answer Online() with whoever was here when the connection died, and
// an announcement published in the gap would be recorded as delivered to a
// player who may already have left -- which nothing retries.
func TestEndSessionEmptiesTheRoster(t *testing.T) {
	r := New()
	absorbSnapshot(t, r, agentEntry, PlayerListEntry{XUID: "111", Username: "Steve", UUID: "u-111"})
	if len(r.Online()) == 0 {
		t.Fatal("nobody online after the snapshot, so this test proves nothing")
	}

	r.EndSession()

	if online := r.Online(); len(online) != 0 {
		t.Errorf("Online() = %v after the connection ended, want nobody", online)
	}
	if r.IsOnline("111") {
		t.Error("IsOnline = true after the connection ended, want false")
	}
}

// The other half of EndSession: presence is what the gap makes unknowable,
// not who an XUID belongs to. An @server answer whose model call outlives the
// connection is still whispered over the console bridge, which is a separate
// process, and a tellraw needs a gamertag to target -- so a name the dead
// connection taught must still resolve.
func TestEndSessionKeepsNamesSoALateReplyCanStillBeAddressed(t *testing.T) {
	r := New()
	absorbSnapshot(t, r, agentEntry, PlayerListEntry{XUID: "111", Username: "Steve", UUID: "u-111"})

	r.EndSession()

	name, ok := r.NameFor("111")
	if !ok || name != "Steve" {
		t.Errorf("NameFor(111) = (%q, %v) after the connection ended, want (Steve, true) — the answer would be logged and thrown away", name, ok)
	}
	if r.IsOnline("111") {
		t.Error("IsOnline(111) = true after the connection ended, want false — nobody is being watched in the gap")
	}
}

// A name is the last one seen, not the first: the next session teaches it
// whatever the server reports now, so a rename between connections wins.
func TestNameForTakesTheNextSessionsName(t *testing.T) {
	r := New()
	absorbSnapshot(t, r, agentEntry, PlayerListEntry{XUID: "111", Username: "OldName"})
	r.EndSession()
	r.BeginSession(time.Now(), agentEntry.XUID)
	absorbSnapshot(t, r, agentEntry, PlayerListEntry{XUID: "111", Username: "NewName"})

	if name, _ := r.NameFor("111"); name != "NewName" {
		t.Errorf("NameFor(111) = %q, want NewName", name)
	}
}

// The next connection's snapshot is still a snapshot: those players were
// already here, so none of them is an arrival.
func TestEndSessionThenSnapshotReportsNobodyAsJoining(t *testing.T) {
	r := New()
	absorbSnapshot(t, r, agentEntry, PlayerListEntry{XUID: "111", Username: "Steve", UUID: "u-111"})
	r.EndSession()

	joins, _, present := r.Apply([]PlayerListEntry{agentEntry, {XUID: "111", Username: "Steve", UUID: "u-111"}})
	if len(joins) != 0 {
		t.Errorf("joins = %+v after reconnecting, want none: the snapshot is not a burst of arrivals", joins)
	}
	if len(present) == 0 {
		t.Error("nobody reported as present, so their backlog would never be scheduled")
	}
}

// An add with no XUID carries no identity this Roster can key on, so it is
// ignored outright -- and an ignored entry cannot be the server's answer to
// who is here either. Reading one as "told, and the answer is nobody"
// suppresses a broadcast published in that window, and an online-only one
// never queues, so it would be gone.
func TestABlankXUIDOpeningPacketLeavesTheRosterUntold(t *testing.T) {
	r := New()
	r.BeginSession(time.Now(), agentEntry.XUID)

	r.Apply([]PlayerListEntry{{UUID: "u-111", Username: "Steve"}})

	if r.Knows() {
		t.Error("Knows() after a blank-XUID add = true, want false — nothing usable has said who is on the server")
	}
	if len(r.Online()) != 0 {
		t.Errorf("Online() = %+v, want nobody: a blank-XUID add is ignored outright", r.Online())
	}

	joins, _, present := r.Apply([]PlayerListEntry{agentEntry, {XUID: "111", Username: "Steve"}})
	if !r.Knows() {
		t.Error("Knows() after the opening list = false, want true")
	}
	if len(joins) != 0 || len(present) != 2 {
		t.Errorf("joins = %+v, present = %+v; want nobody joining and both present", joins, present)
	}
}

// A removal says who left, not who is here. If the first packet of a session
// carries only one -- a player who quit in that instant -- the roster is as
// unanswered as it was before, and the server behind it may be full. Reading
// it as "told, and the answer is nobody" suppresses a broadcast published in
// that window, and an online-only one never queues, so it would be gone.
func TestARemovalOnlyOpeningPacketLeavesTheRosterUntold(t *testing.T) {
	r := New()
	r.BeginSession(time.Now(), agentEntry.XUID)
	if r.Knows() {
		t.Fatal("Knows() before any packet = true, want false")
	}

	r.Apply([]PlayerListEntry{{UUID: "u-111", Remove: true}})

	if r.Knows() {
		t.Error("Knows() after a removal-only packet = true, want false — nothing has said who is on the server")
	}

	// The real snapshot behind it still answers, and still reports its
	// players as present rather than as arrivals.
	joins, _, present := r.Apply([]PlayerListEntry{agentEntry, {XUID: "111", Username: "Steve"}})
	if !r.Knows() {
		t.Error("Knows() after the opening list = false, want true")
	}
	if len(joins) != 0 || len(present) != 2 {
		t.Errorf("joins = %+v, present = %+v; want nobody joining and both present", joins, present)
	}
}

// The two flags answer different questions and must not be collapsed: being
// told is not the same as having accounted for the players who were already
// here, and the snapshot-versus-join classification depends on the second.
func TestAnOpeningPacketWithNoAddStillLetsTheRealSnapshotBePresent(t *testing.T) {
	r := New()
	r.BeginSession(time.Now(), agentEntry.XUID)
	r.Apply([]PlayerListEntry{{UUID: "u-111", Remove: true}})

	joins, _, present := r.Apply([]PlayerListEntry{agentEntry, {XUID: "111", Username: "Steve"}})

	if len(joins) != 0 {
		t.Errorf("joins = %+v, want none: the roster behind an empty packet is still a snapshot", joins)
	}
	if len(present) != 2 {
		t.Errorf("present = %+v, want both, so their backlogs are still scheduled", present)
	}
}

// A session boundary takes it back: in the gap nobody has told this roster
// anything about the connection that follows.
func TestKnowsIsFalseAgainAfterTheConnectionEnds(t *testing.T) {
	r := New()
	absorbSnapshot(t, r, agentEntry, PlayerListEntry{XUID: "111", Username: "Steve"})
	if !r.Knows() {
		t.Fatal("Knows() while watching = false, want true")
	}

	r.EndSession()

	if r.Knows() {
		t.Error("Knows() in the gap = true, want false — an empty roster there means nothing is known, not that nobody is on")
	}
}

// A packet with no entries says nothing about who is here, so the roster is
// still untold. Reading it as knowledge would have a broadcast published in
// that instant suppressed as "nobody is on" -- and an online-only one, which
// never queues, would be gone.
func TestAZeroEntryPacketDoesNotMakeTheRosterKnow(t *testing.T) {
	r := New()
	r.BeginSession(time.Now(), agentEntry.XUID)

	r.Apply(nil)

	if r.Knows() {
		t.Error("Knows() after an empty packet = true, want false — nothing has been said about who is here")
	}
}

// The opening packet names this client alone and the roster follows it, so
// a list holding nobody but the agent is one packet short of an answer.
// Reading it as "told, and the answer is nobody" suppresses a broadcast
// published in that window on a server that may be full, and an online-only
// one never queues, so it would be gone.
func TestTheOpeningPacketNamingOnlyTheAgentLeavesTheRosterUntold(t *testing.T) {
	r := New()
	r.BeginSession(time.Now(), agentEntry.XUID)

	r.Apply([]PlayerListEntry{agentEntry})

	if r.Knows() {
		t.Error("Knows() after the agent's own entry alone = true, want false — the population arrives in the packet behind it")
	}

	r.Apply([]PlayerListEntry{agentEntry, {XUID: "111", Username: "Steve"}})

	if !r.Knows() {
		t.Error("Knows() after the roster behind it = false, want true")
	}
}

// An empty server sends the agent its own entry twice and nothing else, so
// the repeat is the whole answer: nobody else is here. Left unanswered,
// every broadcast to an idle server would be spoken into an empty world and
// reported as a reach nobody could count.
func TestTheAgentsEntryArrivingTwiceMakesTheRosterKnow(t *testing.T) {
	r := New()
	r.BeginSession(time.Now(), agentEntry.XUID)

	r.Apply([]PlayerListEntry{agentEntry})
	r.Apply([]PlayerListEntry{agentEntry})

	if !r.Knows() {
		t.Error("Knows() after the agent's entry twice = false, want true — the server has said who is here, and it is nobody but the agent")
	}
	if len(r.Online()) != 1 {
		t.Errorf("Online() = %+v, want the agent alone", r.Online())
	}
}

// KnownOffline is asked by everything that decides whether to send a player
// something, so the window before the first roster packet has to answer
// "nothing known" rather than "gone": a player chatting in it is standing in
// the world, and writing them off withholds what they are owed.
func TestKnownOfflineNeedsARosterThatHasBeenTold(t *testing.T) {
	r := New()
	r.BeginSession(time.Now(), agentEntry.XUID)

	if KnownOffline(r, "111") {
		t.Error("KnownOffline before the first roster packet = true, want false — the roster has not looked yet")
	}

	r.Apply([]PlayerListEntry{agentEntry, {XUID: "111", Username: "Steve"}})

	if KnownOffline(r, "111") {
		t.Error("KnownOffline for a player the roster names = true, want false")
	}
	if !KnownOffline(r, "222") {
		t.Error("KnownOffline for a player a watching roster does not name = false, want true")
	}

	r.EndSession()

	if KnownOffline(r, "111") {
		t.Error("KnownOffline once the connection ended = true, want false — the roster stopped knowing, not the player leaving")
	}
}

// A caller with no presence wired keeps doing what it did before rather than
// silently withholding everything it would otherwise send.
func TestKnownOfflineOfNothingKnowsNothing(t *testing.T) {
	if KnownOffline(nil, "111") {
		t.Error("KnownOffline(nil) = true, want false")
	}
}
