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

import (
	"sync"
	"time"
)

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
	// Generation is which of the agent's connections was live when this was
	// published. A handler that waits before acting compares it against the
	// current one: a connection that has since been replaced no longer
	// speaks for this player, and the new one has reported them again.
	Generation uint64
}

// Kind satisfies the bus's Event interface.
func (JoinEvent) Kind() string { return JoinKind }

// PresentKind is the bus event kind published for a player the opening
// snapshot reports as already online.
const PresentKind = "roster.present"

// PresentEvent is published once per player in a session's opening
// snapshot. Deliberately not a JoinEvent: these players did not just
// arrive and must never be greeted for reappearing. What they may be is
// mid-load — the agent cannot tell a builder of an hour from someone who
// reconnected a second before it did — so a handler that owes them
// something delayed has to hear about them.
type PresentEvent struct {
	Entry
	// Generation is the connection whose snapshot reported them, read the
	// same way JoinEvent's is.
	Generation uint64
}

// Kind satisfies the bus's Event interface.
func (PresentEvent) Kind() string { return PresentKind }

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
	// agentXUID is this process's own entry in the player list, learned at
	// login and given to BeginSession. It is what marks the opening burst:
	// see Apply. Empty until a session supplies it.
	agentXUID string
	// snapshotStarted is false until this session has absorbed a packet
	// carrying at least one usable add. snapshotEnded is false until the
	// server has finished describing the world to this client. Between the
	// two, every add is a player who was already online -- see Apply.
	snapshotStarted bool
	snapshotEnded   bool
	// answered is false until the server has said who is on it in a way this
	// Roster can report, which is what Knows() reads.
	//
	// Not snapshotStarted, because the packet that starts the snapshot names
	// this client alone and the roster behind it is still one packet away:
	// answering from it would report an empty server nobody has described,
	// swallowing a broadcast that should have been spoken. Set instead by a
	// packet adding somebody other than the agent, or by a second
	// add-carrying packet -- an empty server sends the agent's entry twice
	// and nothing else, and that repeat is the whole answer, which is that
	// nobody is here.
	//
	// A removal says who left, an empty packet says nothing at all, and an
	// add with no XUID carries nobody this Roster can key on; none of the
	// three answers who is here.
	answered bool
	// names is the last gamertag each XUID was seen under. Kept apart from
	// players because presence and identity stop being true at different
	// moments: presence ends with the connection that reported it, a name
	// does not. Voice.Tell carries only an XUID and reaches a player through
	// the console bridge, a separate process that stays up across a
	// reconnect, so a reply whose model call outlived the connection still
	// has to be addressable. Replaced whenever a later session reports the
	// same XUID under a different name, and never dropped: the set is
	// bounded by the distinct players this process has ever seen.
	names map[string]string
	// since is when the current session began watching; zero before the
	// first one.
	since time.Time
}

// New builds an empty Roster awaiting its first session snapshot.
func New() *Roster {
	return &Roster{players: make(map[string]string), xuidByUUID: make(map[string]string), names: make(map[string]string)}
}

// BeginSession puts the Roster back into its pre-snapshot state, holding
// nobody as present. Leave records only arrive while connected, so across a
// disconnect gap the retained presence is not merely incomplete but wrong:
// it would claim players who have since left are still online (and suppress
// a genuine rejoin as already-known). Call this at the start of every
// session, before any packet from it is applied, with the moment it began
// watching and the XUID this connection logged in as. Names outlive it --
// see names.
//
// The agent's own XUID is session state rather than a constructor argument
// because it is not known until the connection has logged in, which is
// after the process (and this Roster) already exist. Left empty, Apply
// cannot tell the agent's own entry from a player's and falls back to
// treating the first add of the session as the whole snapshot.
func (r *Roster) BeginSession(at time.Time, agentXUID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.since = at
	r.agentXUID = agentXUID
	r.clearPresence()
}

// EndSession drops the presence a dead connection was reporting, at the
// moment it dies rather than at the moment the next one opens. Everything
// BeginSession says about retained presence being wrong applies from then:
// in between, anyone reading Online() -- an announcement published by a
// schedule, an event source or the HTTP API -- would be handed players who
// may already have left, and recording a delivery against one of them loses
// that message for good.
//
// Names are deliberately kept, because what the gap makes unknowable is who
// is here, not who an XUID belongs to: a reply that goes out over the
// console bridge after the connection died still needs a gamertag to target,
// and the next session teaches new names as it reports them. The next
// session's BeginSession still runs and still sets when it began watching.
func (r *Roster) EndSession() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clearPresence()
}

// clearPresence drops everything a connection reported about who is here,
// leaving the Roster pre-snapshot. Shared by the two ends of a session so
// that a presence field added to the Roster cannot be cleared at one of them
// and retained at the other, which is the stale-state class both calls exist
// to prevent. Names are not presence and are left alone. Callers hold r.mu.
func (r *Roster) clearPresence() {
	r.players = make(map[string]string)
	r.xuidByUUID = make(map[string]string)
	r.snapshotStarted = false
	r.snapshotEnded = false
	r.answered = false
}

// Since reports when the current session began watching, as given to
// BeginSession. A presence that started before it was not watched through:
// whatever happened across the gap -- a departure, a return -- was never
// seen. Zero before any session, which makes everything count as watched.
func (r *Roster) Since() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.since
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
// A session opens with the server describing the world to this client in a
// burst of PlayerList packets, and it names this client in every one of
// them: the agent's own entry arrives alone first, then again at the head of
// the full roster. Everyone that burst adds was already connected before
// this process was watching, so they are recorded and reported as present
// rather than as joins.
//
// The burst ends at the first packet that adds somebody without naming the
// agent, because nothing after it ever does -- an arrival or a departure
// carries only the player it concerns. That boundary is the agent's own
// entry rather than simply the first add, because the first add *is* the
// agent, alone: ending the burst there left the entire already-online
// population to arrive in the packet behind it, each one greeted as a fresh
// arrival with their join counted, on every connect. Nor is it enough to
// refuse the agent's entry the boundary while giving it to the next add,
// since an empty server sends that entry twice and nothing else, which would
// hand the boundary to the day's first genuine arrival and swallow their
// greeting instead.
//
// Neither an empty packet nor a removal carries an add, so neither can end
// the burst or start it, and neither can cause the roster behind it to be
// mistaken for arrivals.
func (r *Roster) Apply(entries []PlayerListEntry) (joins, leaves, present []Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var namesAgent, namesOthers, adds bool
	for _, e := range entries {
		if e.Remove || e.XUID == "" {
			continue
		}
		adds = true
		// An unset agentXUID matches nobody, since a usable add always
		// carries one: a Roster never told who it is falls back to treating
		// the first add of the session as the whole burst, and to reading
		// that first add as the server's answer about who is here.
		if e.XUID == r.agentXUID {
			namesAgent = true
		} else {
			namesOthers = true
		}
	}
	if r.snapshotStarted && adds && !namesAgent {
		r.snapshotEnded = true
	}
	snapshot := !r.snapshotEnded
	if adds {
		if namesOthers || r.snapshotStarted {
			r.answered = true
		}
		r.snapshotStarted = true
	}

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
		if e.UUID != "" {
			r.xuidByUUID[e.UUID] = e.XUID
		}
		if _, known := r.players[e.XUID]; !known {
			entry := Entry{XUID: e.XUID, Username: e.Username}
			if snapshot {
				present = append(present, entry)
			} else {
				joins = append(joins, entry)
			}
		}
		r.players[e.XUID] = e.Username
		r.names[e.XUID] = e.Username
	}
	return joins, leaves, present
}

// NameFor returns the last username recorded for xuid, so a caller that
// only has an XUID (as Voice.Tell does) can build a tellraw target. It still
// answers for a player the current connection is not watching -- one who
// left, or one whose session ended with the connection itself -- because a
// tellraw goes out over the console bridge, which does not end with the
// Bedrock connection, and a reply with nobody to address is simply thrown
// away. Ask IsOnline for whether they are here now.
//
// ok is false only for an XUID this process has never seen named.
func (r *Roster) NameFor(xuid string) (name string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name, ok = r.names[xuid]
	return name, ok
}

// Knows reports whether this Roster can currently answer who is on the
// server. False in the gap between connections, and false again in the
// moments after one opens until the live connection has described who is
// here -- in both, an empty Online() means "not known yet" rather than
// "nobody is here", and a caller that cannot tell those apart will act on
// the wrong one.
//
// False for the opening packet too, which names this client and nobody else
// -- the population arrives in the packet behind it, and a roster holding
// only the agent is not an empty server. See answered for what does settle
// it, and for why a second entry for the agent alone does.
func (r *Roster) Knows() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.answered
}

// IsOnline reports whether xuid is on the roster the current connection is
// watching. Separate from NameFor because the two stop being true at
// different moments -- see names.
func (r *Roster) IsOnline(xuid string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.players[xuid]
	return ok
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

// XUIDFor resolves a gamertag back to the XUID currently on record for it,
// so a caller that only has a display name (e.g. a "@PlayerName" reference
// typed into a command) can turn it into the identity Voice.Tell and the
// announcement store actually key on. ok is false if name isn't the current
// username of anyone on the roster.
//
// Not NameFor's reverse, and the asymmetry is deliberate: this scans the
// presence map, which a session boundary empties, while NameFor answers from
// the retained names one. After a connection dies NameFor still resolves an
// XUID to "Steve" -- a reply already in flight has to stay addressable --
// but XUIDFor("Steve") finds nobody, because nobody is on the server. A
// caller that needs that round trip for an offline player wants the durable
// record instead; see cmd/agent's playerLookup.
//
// This assumes gamertags are unique per server, same as the names map
// already assumes in the other direction. If that were ever violated —
// two entries on the roster sharing a name — whichever is encountered
// first during the scan wins, with no significance attached to which that
// is; this is a documented tie-break for a case that should not occur, not
// a guarantee callers should rely on.
//
// This is a linear scan rather than a second index: the roster is sized to
// a Bedrock server's concurrent player count, not a lookup table, and the
// maps already give O(1) resolution the other direction (NameFor, IsOnline),
// which is the hot path (every Tell, every recipient). A reverse index would
// double the bookkeeping Apply has to keep consistent for a lookup that
// isn't on that hot path.
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
