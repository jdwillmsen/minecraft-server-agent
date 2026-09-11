// Package roster tracks the server's live player roster from PlayerList
// packets, the authoritative source for who is actually connected —
// unlike chat, which is only ever present when someone types. It serves
// four needs: join/leave detection (for the welcome plugin), XUID-to-gamertag
// resolution (for targeting a tellraw reply at a specific player, since
// Voice.Tell only carries the XUID), gamertag-to-XUID resolution (the
// reverse, for turning a typed "@PlayerName" reference back into the
// identity everything else keys on), and reporting who is online at all
// (for an announcement deciding who actually hears it).
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
	// xuidByUUID exists because Bedrock's removal record carries only the
	// player's UUID: gophertunnel decodes nothing past it for a remove, so
	// the XUID the rest of this type keys on is blank on exactly the record
	// that says someone left. Learned from each add record, which carries
	// both. Without it every departure was dropped as an unusable entry, and
	// the agent never saw a single player leave.
	xuidByUUID map[string]string
	// snapshotSeen is false until this session's opening PlayerList has
	// been absorbed. The server sends every already-connected player as an
	// add record immediately after login, so without this the whole
	// existing population would look like a burst of arrivals.
	snapshotSeen bool
}

// New builds an empty Roster awaiting its first session snapshot.
func New() *Roster {
	return &Roster{players: make(map[string]string), xuidByUUID: make(map[string]string)}
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
	r.xuidByUUID = make(map[string]string)
	r.snapshotSeen = false
}

// Apply updates the roster from one PlayerList packet's entries and
// reports every player who is newly present — an add whose XUID this
// Roster had not already recorded. A player already known who reappears
// (e.g. a duplicate add, which the protocol permits) is not reported
// again. An add with a blank XUID is ignored outright: it carries no usable
// identity and must never be treated as a join.
//
// A removal is resolved by its UUID when it carries no XUID, which on the
// wire is always -- see xuidByUUID. A removal that resolves to nobody this
// session recorded is dropped: there is no one to report as leaving.
//
// The first packet of a session carrying at least one usable add is the
// server's roster snapshot: those players were already connected before
// this process was watching, so they are recorded but never reported as
// joins. Neither an empty packet nor a removal consumes it, so neither can
// cause the real snapshot behind it to be mistaken for arrivals.
func (r *Roster) Apply(entries []PlayerListEntry) (joins, leaves []Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()

	snapshot := !r.snapshotSeen

	for _, e := range entries {
		if e.Remove {
			xuid := e.XUID
			if xuid == "" {
				xuid = r.xuidByUUID[e.UUID]
			}
			if e.UUID != "" {
				delete(r.xuidByUUID, e.UUID)
			}
			if xuid == "" {
				continue
			}
			// Reported, not just forgotten. A departure is half of a play
			// session, and the roster is the only place the agent learns of
			// one -- the server sends a removal record, and nothing else does.
			if name, known := r.players[xuid]; known {
				leaves = append(leaves, Entry{XUID: xuid, Username: name})
			}
			delete(r.players, xuid)
			continue
		}

		if e.XUID == "" {
			continue
		}
		r.snapshotSeen = true
		if e.UUID != "" {
			r.xuidByUUID[e.UUID] = e.XUID
		}
		if _, known := r.players[e.XUID]; !known && !snapshot {
			joins = append(joins, Entry{XUID: e.XUID, Username: e.Username})
		}
		r.players[e.XUID] = e.Username
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

// Online returns the XUIDs currently on the roster, so a caller deciding
// who actually hears a broadcast doesn't have to re-derive "connected" from
// join/leave events itself. The load-bearing property is that this builds
// a fresh slice under the lock rather than returning anything that aliases
// the internal map: the map is mutated by Apply from the packet-read
// goroutine, so a caller iterating a returned reference to it — instead of
// a copy — would be racing that goroutine. The order is unspecified;
// callers that need a deterministic order sort it themselves.
func (r *Roster) Online() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.players))
	for xuid := range r.players {
		out = append(out, xuid)
	}
	return out
}

// XUIDFor is NameFor's reverse: it resolves a gamertag back to the XUID
// currently on record for it, so a caller that only has a display name
// (e.g. a "@PlayerName" reference typed into a command) can turn it into
// the identity Voice.Tell and the announcement store actually key on. ok is
// false if name isn't the current username of anyone on the roster.
//
// This assumes gamertags are unique per server, same as NameFor's map
// already assumes in the other direction. If that were ever violated —
// two entries on the roster sharing a name — whichever is encountered
// first during the scan wins, with no significance attached to which that
// is; this is a documented tie-break for a case that should not occur, not
// a guarantee callers should rely on.
//
// This is a linear scan rather than a second index: the roster is sized to
// a Bedrock server's concurrent player count, not a lookup table, and the
// map already gives O(1) resolution the other direction (NameFor), which is
// the hot path (every Tell). A reverse index would double the bookkeeping
// Apply has to keep consistent for a lookup that isn't on that hot path.
func (r *Roster) XUIDFor(name string) (xuid string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for x, n := range r.players {
		if n == name {
			return x, true
		}
	}
	return "", false
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
	// UUID is the only identity a removal record carries on the wire.
	UUID string
	// Remove is true for a removal record, false for an add. Named for
	// what it means here rather than mirroring the protocol's numeric
	// ActionType, which callers translate at the boundary.
	Remove bool
}
