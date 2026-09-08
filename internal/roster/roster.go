// Package roster tracks the server's live player roster from PlayerList
// packets, the authoritative source for who is actually connected —
// unlike chat, which is only ever present when someone types. It serves
// two needs: join detection (for the welcome plugin) and XUID-to-gamertag
// resolution (for targeting a tellraw reply at a specific player, since
// Voice.Tell only carries the XUID).
package roster

import "sync"

// Entry is one player's identity as reported by the server's own roster,
// not the spoofable chat SourceName field.
type Entry struct {
	XUID     string
	Username string
}

// JoinKind is the bus event kind published for a genuinely new arrival.
const JoinKind = "roster.join"

// JoinEvent is published once per player who genuinely arrives while this
// session is watching — an add record for an XUID not already on the
// roster, and not part of the session's opening snapshot.
type JoinEvent struct {
	Entry
}

// Kind satisfies the bus's Event interface.
func (JoinEvent) Kind() string { return JoinKind }

// LeaveKind is the bus event kind published when a known player departs.
const LeaveKind = "roster.leave"

// LeaveEvent is published for each departure the roster observes.
type LeaveEvent struct {
	XUID     string
	Username string
}

// Kind satisfies the bus's Event interface.
func (LeaveEvent) Kind() string { return LeaveKind }

// Roster holds the current XUID-to-username mapping. Safe for concurrent
// use.
type Roster struct {
	mu      sync.Mutex
	players map[string]string
	// snapshotSeen is false until this session's opening PlayerList has
	// been absorbed. The server sends every already-connected player as an
	// add record immediately after login, so without this the whole
	// existing population would look like a burst of arrivals.
	snapshotSeen bool
}

// New builds an empty Roster awaiting its first session snapshot.
func New() *Roster {
	return &Roster{players: make(map[string]string)}
}

// BeginSession discards everything the Roster knows and puts it back into
// its pre-snapshot state. Leave records only arrive while connected, so
// across a disconnect gap the retained map is not merely incomplete but
// wrong: it would claim players who have since left are still online (and
// suppress a genuine rejoin as already-known). Call this at the start of
// every session, before any packet from it is applied.
func (r *Roster) BeginSession() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.players = make(map[string]string)
	r.snapshotSeen = false
}

// Apply updates the roster from one PlayerList packet's entries and
// reports every player who is newly present — an add whose XUID this
// Roster had not already recorded. A player already known who reappears
// (e.g. a duplicate add, which the protocol permits) is not reported
// again. A blank XUID is ignored outright: it carries no usable identity
// and must never be treated as a join.
//
// The first packet of a session carrying at least one usable entry is the
// server's roster snapshot: those players were already connected before
// this process was watching, so they are recorded but never reported as
// joins. A packet with no usable entry leaves the snapshot unconsumed, so
// an empty update cannot cause the real snapshot behind it to be mistaken
// for arrivals.
func (r *Roster) Apply(entries []PlayerListEntry) (joins, leaves []Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()

	snapshot := !r.snapshotSeen

	for _, e := range entries {
		if e.XUID == "" {
			continue
		}
		r.snapshotSeen = true
		switch {
		case e.Remove:
			// Reported, not just forgotten. A departure is half of a play
			// session, and the roster is the only place the agent learns of
			// one -- the server sends a removal record, and nothing else does.
			if name, known := r.players[e.XUID]; known {
				leaves = append(leaves, Entry{XUID: e.XUID, Username: name})
			}
			delete(r.players, e.XUID)
		default:
			if _, known := r.players[e.XUID]; !known && !snapshot {
				joins = append(joins, Entry{XUID: e.XUID, Username: e.Username})
			}
			r.players[e.XUID] = e.Username
		}
	}
	return joins, leaves
}

// NameFor returns the username currently on record for xuid, so a caller
// that only has an XUID (as Voice.Tell does) can build a tellraw target.
// ok is false if xuid is not in the current roster (never seen, or left).
func (r *Roster) NameFor(xuid string) (name string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name, ok = r.players[xuid]
	return name, ok
}

// PlayerListEntry is the subset of protocol.PlayerListEntry this package
// needs. Defined here rather than importing the protocol package directly,
// so Roster's own tests don't need to construct a full gophertunnel
// PlayerListEntry (which carries skin data and other fields irrelevant to
// roster tracking) — the caller (cmd/agent) maps the real packet type into
// this one at the single point they meet.
type PlayerListEntry struct {
	XUID     string
	Username string
	// Remove is true for a removal record, false for an add. Named for
	// what it means here rather than mirroring the protocol's numeric
	// ActionType, which callers translate at the boundary.
	Remove bool
}
