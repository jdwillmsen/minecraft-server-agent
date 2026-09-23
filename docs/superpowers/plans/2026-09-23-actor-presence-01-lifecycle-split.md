# Actor Presence 1: Split the Agent's Monitoring From Its In-World Session — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the agent two lifecycles: one that follows the leader lock and runs the monitoring work, and one for the in-world session that sits behind a gate. While the gate keeps the agent out of the world, a roster built from the console bridge stands in for the session's roster. Nothing visible changes, because in this phase the gate always says "present".

**Architecture:** Once a turn has started `startLiveWork` (already per turn, `cmd/agent/leading.go:270-286`, `cmd/agent/main.go:331`), the turn body stops being `runConnectLoop` directly. It becomes `runSessions`, which asks a `sessionGate` whether a session is wanted. If yes, it runs the connect loop. If no, it runs a bridge follower that seeds the existing `*roster.Roster` from `list` and then follows `GET /events`, and it drives the existing `joinTimes`, so the announcement deliverer's `Online` and `SinceConnect` keep answering. In this phase the gate is `alwaysPresent{}`. Plan 3 swaps in the presence-backed gate. Execute on a new branch in a fresh worktree of `minecraft-server-agent` cut from `origin/main` (this plan does not create it).

**Tech Stack:** Go 1.27 (`go.mod`), standard library, the existing `internal/adapters.BridgeClient`, `internal/roster`, `pkg/logging`. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-09-23-actor-presence-design.md` (sections "Agent: split monitoring from presence" and Rollout step 1). Shared contract: `docs/superpowers/plans/2026-09-23-actor-presence-00-contract.md`. This phase adds no contract names. Both files sit beside this plan in the agent repo.

## Global Constraints

- No visible behavior change. With `alwaysPresent{}` wired, the process must connect, reconnect, hand over and shut down exactly as it does on `origin/main`: same log events, same metrics, same `/readyz`.
- `go.mod` and `go.sum` stay untouched. No new dependencies.
- The console bridge's `GET /events` wire format is fixed by `mc-console-bridge/events.go:22-32`: a JSON array of `{"id":int64,"type":"connect"|"disconnect"|"content_error"|"crash","time":RFC3339,"player":string,"raw":string}`, `since` as a query integer. IDs start again at 1 whenever the bridge process restarts, and the bridge keeps at most 2000 events.
- `list` is already allowlisted by the bridge (`mc-console-bridge/allowlist.go:29-30`). Use no other console command.
- Comments explain why, never what, at the density of the code around them. No ticket IDs anywhere: code, comments, docs or commit messages.
- Conventional commits. Every file you touch is `gofmt`ed. CI runs `gofmt -l .`, `go vet ./...`, `CGO_ENABLED=0 go build ./...` and `go test -race ./...` (`.github/workflows/ci.yml`), and `golangci-lint run` must also be clean.
- Log with `logging.Fields` and snake_case event names, the way `cmd/agent/leading.go` does.

## Review Focus

- **The bridge restarts between two polls.** Its event IDs begin again at 1, so a follower that keeps asking `since=<old cursor>` hears nothing forever. Expected: the follower notices the reset, clears the roster and seeds again. Pinned in Task 3 (`TestBridgeRosterReseedsWhenTheBridgeRestarts`).
- **`list` output is truncated, or other console lines are mixed into it.** The bridge collects output in a fixed window with no request correlation (`internal/adapters/facts.go:27-31`). Expected: the seed is refused and retried, and the roster is never seeded with a wrong population. Pinned in Task 1 (`TestParseListRefusesACountItCannotAccountFor`) and Task 3 (`TestBridgeRosterRetriesASeedTheBridgeCouldNotAnswer`).
- **An online player whose XUID nobody recorded.** `list` names gamertags only. Expected: the player is left out of `Online()` and the seed still succeeds. `Knows()` is true, so the deliverer holds that player's copy pending instead of recording it (`internal/announce/deliver.go:158-172`). Pinned in Task 3 (`TestBridgeRosterSeedsFromListResolvingXUIDs`).
- **The gate signals a change that didn't happen** (plan 3's store re-read returns the same state). Expected: the live session is not torn down. Pinned in Task 4 (`TestASpuriousGateSignalDoesNotRestartTheSession`).
- **Shutdown or lock loss while the agent is out of the world.** Expected: the turn ends. The leave hook for deliberately ending a session (the playtime close) is not run a second time. The roster is left not knowing who is online, the same state `connectionEnded` leaves. Pinned in Task 3 (`TestBridgeRosterLeavesTheRosterUnknowingWhenItStops`) and Task 4 (`TestShutdownWhileAbsentDoesNotSettleALeave`).

---

## File Structure

| File | Change | Responsibility |
|---|---|---|
| `internal/adapters/bridgeroster.go` | Create | Two transport calls on `BridgeClient`: `Events` (`GET /events`) and `OnlinePlayers` (`list`, parsed). Also `ParseList` and `BridgeEvent.XUID`. |
| `internal/adapters/bridgeroster_test.go` | Create | httptest tests for both calls and table tests for the parser. |
| `internal/roster/roster.go` | Modify (insert after `EndSession`, lines 154-171) | `Seed`: replace presence with a population that a source other than a Bedrock connection reported. |
| `internal/roster/roster_test.go` | Modify (append) | `Seed` semantics. |
| `cmd/agent/bridgeroster.go` | Create | `bridgeRoster`: seed, follow, detect a bridge reset, drive `*roster.Roster` and `*joinTimes`. |
| `cmd/agent/bridgeroster_test.go` | Create | Follower tests against a fake bridge feed. |
| `cmd/agent/sessions.go` | Create | `sessionGate`, `alwaysPresent`, `sessionModes`, `runSessions`. |
| `cmd/agent/sessions_test.go` | Create | Gate-driven switching tests. |
| `cmd/agent/main.go` | Modify (lines 121-125 comment, 300-341 turn loop) | Wire `runSessions` into the turn in place of the direct `runConnectLoop` call. |

---

### Task 1: Bridge transport for the roster (`Events`, `OnlinePlayers`)

**Files:**
- Create: `internal/adapters/bridgeroster.go`
- Test: `internal/adapters/bridgeroster_test.go`

**Interfaces:**
- Consumes: `(*BridgeClient).do(ctx, method, path string, body io.Reader, out any) error` and `(*BridgeClient).runCommand(ctx, cmd string) (commandResponse, error)` (`internal/adapters/bridgeclient.go:51-113`).
- Produces:
  - `type BridgeEvent struct { ID int64; Type string; Time time.Time; Player string; Raw string }` (JSON tags `id,type,time,player,raw`)
  - `const BridgeEventConnect = "connect"`, `const BridgeEventDisconnect = "disconnect"`
  - `func (e BridgeEvent) XUID() string` (empty when the line carries none)
  - `func (c *BridgeClient) Events(ctx context.Context, since int64) ([]BridgeEvent, error)`
  - `func (c *BridgeClient) OnlinePlayers(ctx context.Context) ([]string, error)` (non-nil empty slice for an empty server)
  - `func ParseList(output string) ([]string, error)`

- [ ] **Step 1: Write the failing tests**

Create `internal/adapters/bridgeroster_test.go`:

```go
package adapters

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestBridgeClient_EventsAsksForEverythingAfterSince(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/events" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("since"); got != "41" {
			t.Errorf("since = %q, want 41", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q, want Bearer tok", got)
		}
		_, _ = w.Write([]byte(`[{"id":42,"type":"connect","time":"2026-09-23T10:00:00Z","player":"Steve","raw":"[2026-09-23 10:00:00:000 INFO] Player connected: Steve, xuid: 2535457893448396"}]`))
	}))
	defer srv.Close()

	got, err := NewBridgeClient(srv.URL, "tok", time.Second).Events(context.Background(), 41)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(got) != 1 || got[0].ID != 42 || got[0].Type != BridgeEventConnect || got[0].Player != "Steve" {
		t.Fatalf("Events = %+v, want the one connect event", got)
	}
	if x := got[0].XUID(); x != "2535457893448396" {
		t.Errorf("XUID = %q, want 2535457893448396", x)
	}
}

func TestBridgeClient_EventsFailureIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	if _, err := NewBridgeClient(srv.URL, "tok", time.Second).Events(context.Background(), 0); err == nil {
		t.Fatal("Events returned no error for a 401")
	}
}

func TestBridgeEventXUIDIsEmptyWhenTheLineCarriesNone(t *testing.T) {
	if x := (BridgeEvent{Raw: "Player connected: Steve"}).XUID(); x != "" {
		t.Errorf("XUID = %q, want empty", x)
	}
}

func TestBridgeClient_OnlinePlayersRunsListAndParsesIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req commandRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Command != "list" {
			t.Errorf("command = %q, want list", req.Command)
		}
		_ = json.NewEncoder(w).Encode(commandResponse{Rule: "list", Output: "There are 2/20 players online:\nSteve, Alex\n"})
	}))
	defer srv.Close()

	got, err := NewBridgeClient(srv.URL, "tok", time.Second).OnlinePlayers(context.Background())
	if err != nil {
		t.Fatalf("OnlinePlayers: %v", err)
	}
	if want := []string{"Steve", "Alex"}; !reflect.DeepEqual(got, want) {
		t.Errorf("OnlinePlayers = %q, want %q", got, want)
	}
}

func TestParseList(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
		want   []string
	}{
		{"names on the next line", "There are 2/20 players online:\nSteve, Alex\n", []string{"Steve", "Alex"}},
		{"names on the same line", "There are 2/20 players online: Steve, Alex", []string{"Steve", "Alex"}},
		{"log-prefixed header and CRLF", "[2026-09-23 10:00:00:000 INFO] There are 1/10 players online:\r\nSteve\r\n", []string{"Steve"}},
		{"gamertag with a space", "There are 1/10 players online:\nBig Steve\n", []string{"Big Steve"}},
		{"empty server", "There are 0/10 players online:\n", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseList(tc.output)
			if err != nil {
				t.Fatalf("ParseList: %v", err)
			}
			if got == nil || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseList = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// The bridge collects whatever the console prints inside its window, so a
// join landing at the same moment can sit where the names should be. A
// count the header cannot account for is a seed that would be wrong.
func TestParseListRefusesACountItCannotAccountFor(t *testing.T) {
	for _, output := range []string{
		"",
		"Unknown command: list",
		"There are 2/20 players online:\n",
		"There are 2/20 players online:\n[2026-09-23 10:00:00:000 INFO] Player connected: Sam, xuid: 3\n",
		"There are 1/20 players online:\nSteve, Alex\n",
	} {
		if got, err := ParseList(output); err == nil {
			t.Errorf("ParseList(%q) = %q, nil; want an error", output, got)
		}
	}
}
```

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/adapters/ -run 'Events|OnlinePlayers|ParseList|BridgeEventXUID'`
Expected: FAIL, build errors `undefined: BridgeEvent`, `c.Events undefined`, `undefined: ParseList`.

- [ ] **Step 3: Implement**

Create `internal/adapters/bridgeroster.go`:

```go
package adapters

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Event types the roster follows. The bridge also reports content errors and
// crashes, which say nothing about who is online.
const (
	BridgeEventConnect    = "connect"
	BridgeEventDisconnect = "disconnect"
)

// BridgeEvent is one entry of mc-console-bridge's GET /events. IDs are only
// unique within one bridge process: they start again at 1 when it restarts.
type BridgeEvent struct {
	ID     int64     `json:"id"`
	Type   string    `json:"type"`
	Time   time.Time `json:"time"`
	Player string    `json:"player,omitempty"`
	Raw    string    `json:"raw"`
}

var eventXUID = regexp.MustCompile(`xuid: (\d+)`)

// XUID is the account the server logged the line for. The bridge parses only
// the gamertag out of it, and the roster keys on the XUID, so it is read from
// the raw line here.
func (e BridgeEvent) XUID() string {
	m := eventXUID.FindStringSubmatch(e.Raw)
	if m == nil {
		return ""
	}
	return m[1]
}

// Events returns every event the bridge still holds with an ID above since,
// oldest first.
func (c *BridgeClient) Events(ctx context.Context, since int64) ([]BridgeEvent, error) {
	var out []BridgeEvent
	if err := c.do(ctx, http.MethodGet, "/events?since="+strconv.FormatInt(since, 10), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// OnlinePlayers runs `list` through the bridge and returns the gamertags it
// names.
func (c *BridgeClient) OnlinePlayers(ctx context.Context) ([]string, error) {
	resp, err := c.runCommand(ctx, "list")
	if err != nil {
		return nil, fmt.Errorf("bridge: online players: %w", err)
	}
	names, err := ParseList(resp.Output)
	if err != nil {
		return nil, fmt.Errorf("bridge: online players: %w", err)
	}
	return names, nil
}

var listHeader = regexp.MustCompile(`There are (\d+)/\d+ players online:`)

var (
	errNoListHeader = errors.New("no \"There are N/M players online:\" line in the console output")
	errListCount    = errors.New("the names do not match the count the server gave")
)

// ParseList reads Bedrock's answer to `list`. The server prints the count on
// one line and the names, comma separated, on the same line or the next one.
//
// Strict on purpose. The bridge returns whatever the console printed inside a
// fixed window, so another log line can sit where the names belong. A roster
// seeded from that would be wrong about everyone until the next seed, which is
// worse than one that waits a poll for a clean answer.
func ParseList(output string) ([]string, error) {
	lines := strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n")
	for i, line := range lines {
		loc := listHeader.FindStringSubmatchIndex(line)
		if loc == nil {
			continue
		}
		want, err := strconv.Atoi(line[loc[2]:loc[3]])
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errNoListHeader, err)
		}
		names := []string{}
		if want == 0 {
			return names, nil
		}
		rest := strings.TrimSpace(line[loc[1]:])
		if rest == "" {
			for _, next := range lines[i+1:] {
				if rest = strings.TrimSpace(next); rest != "" {
					break
				}
			}
		}
		for _, n := range strings.Split(rest, ",") {
			n = strings.TrimSpace(n)
			if n == "" {
				continue
			}
			// A log line in the names' place splits into as many "names" as
			// it has commas, which can match the count by accident; its
			// stamp and fields cannot be a gamertag.
			if strings.ContainsAny(n, ":[") {
				return nil, fmt.Errorf("%w: %q is not a gamertag", errListCount, n)
			}
			names = append(names, n)
		}
		if len(names) != want {
			return nil, fmt.Errorf("%w: expected %d, found %d in %q", errListCount, want, len(names), rest)
		}
		return names, nil
	}
	return nil, errNoListHeader
}
```

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/adapters/ -run 'Events|OnlinePlayers|ParseList|BridgeEventXUID' -v`
Expected: PASS for all six tests, including every `TestParseList` subtest.

- [ ] **Step 5: Commit**

```bash
gofmt -l internal/adapters
git add internal/adapters/bridgeroster.go internal/adapters/bridgeroster_test.go
git commit -m "feat(adapters): read the online roster and join events from the console bridge"
```
Expected: `gofmt -l` prints nothing, and the commit succeeds.

---

### Task 2: `Roster.Seed`: a population reported by something other than a connection

**Files:**
- Modify: `internal/roster/roster.go` (insert the new method directly after `EndSession`, which ends at line 171)
- Test: `internal/roster/roster_test.go` (append)

**Interfaces:**
- Consumes: `Roster.clearPresence()` (`roster.go:178-184`) and the existing fields `players`, `names`, `agentXUID`, `snapshotStarted`, `snapshotEnded`, `answered`.
- Produces: `func (r *Roster) Seed(players []Entry)`. After it returns, `Knows()` is true, `Online()` is exactly the seeded XUIDs, and `Apply` reports every later add as a join (never as present). `Since()` is unchanged, and so are names learned before the seed.

- [ ] **Step 1: Write the failing tests**

Append to `internal/roster/roster_test.go`:

```go
func TestSeedIsWhoIsHereAndSaysSo(t *testing.T) {
	r := New()
	r.Seed([]Entry{{XUID: "111", Username: "Steve"}, {XUID: "222", Username: "Alex"}})

	if !r.Knows() {
		t.Fatal("Knows() = false after a seed: an empty Online() would read as not known yet")
	}
	if got := len(r.Online()); got != 2 {
		t.Errorf("Online() holds %d players, want 2", got)
	}
	if !r.IsOnline("111") || !r.IsOnline("222") {
		t.Error("a seeded player is not online")
	}
	if name, ok := r.NameFor("222"); !ok || name != "Alex" {
		t.Errorf("NameFor(222) = %q, %v; want Alex, true", name, ok)
	}
}

// Nobody on the server is still an answer, and the one a seed from an empty
// `list` gives.
func TestSeedWithNobodyKnowsTheServerIsEmpty(t *testing.T) {
	r := New()
	r.Seed(nil)
	if !r.Knows() || len(r.Online()) != 0 {
		t.Errorf("Knows() = %v, Online() = %v; want true and empty", r.Knows(), r.Online())
	}
}

// A seed is not an opening snapshot: there is no burst to wait out, so the
// very next add is somebody arriving.
func TestAnAddAfterASeedIsAJoin(t *testing.T) {
	r := New()
	r.Seed([]Entry{{XUID: "111", Username: "Steve"}})

	joins, _, present := r.Apply([]PlayerListEntry{{XUID: "222", Username: "Alex"}})
	if len(joins) != 1 || joins[0].XUID != "222" || len(present) != 0 {
		t.Errorf("joins = %+v, present = %+v; want Alex as the one join", joins, present)
	}
}

func TestARemovalAfterASeedIsALeave(t *testing.T) {
	r := New()
	r.Seed([]Entry{{XUID: "111", Username: "Steve"}})

	_, leaves, _ := r.Apply([]PlayerListEntry{{XUID: "111", Remove: true}})
	if len(leaves) != 1 || leaves[0] != (Entry{XUID: "111", Username: "Steve"}) {
		t.Errorf("leaves = %+v, want Steve", leaves)
	}
	if r.IsOnline("111") {
		t.Error("Steve still online after leaving")
	}
}

// A seed replaces what the roster held, including a dead connection's
// presence, and forgets which entry was the agent's own: the agent is not on
// the server while something else is reporting who is.
func TestSeedReplacesEarlierPresence(t *testing.T) {
	r := New()
	r.BeginSession(time.Unix(100, 0), agentEntry.XUID)
	absorbSnapshot(t, r, agentEntry, PlayerListEntry{XUID: "111", Username: "Steve"})

	r.Seed([]Entry{{XUID: "222", Username: "Alex"}})

	if r.IsOnline("111") || r.IsOnline(agentEntry.XUID) {
		t.Error("presence from before the seed survived it")
	}
	if !r.IsOnline("222") {
		t.Error("the seeded player is not online")
	}
	if name, ok := r.NameFor("111"); !ok || name != "Steve" {
		t.Errorf("NameFor(111) = %q, %v; names outlive presence and must survive a seed", name, ok)
	}
	joins, _, _ := r.Apply([]PlayerListEntry{{XUID: agentEntry.XUID, Username: agentEntry.Username}})
	if len(joins) != 1 {
		t.Errorf("the old agent XUID arriving after a seed gave %d joins, want 1", len(joins))
	}
}

// Since is what the profile store watched through. A seed writes nothing to
// the store, so it must not move it.
func TestSeedLeavesSinceAlone(t *testing.T) {
	r := New()
	began := time.Unix(100, 0)
	r.BeginSession(began, agentEntry.XUID)
	r.Seed(nil)
	if got := r.Since(); !got.Equal(began) {
		t.Errorf("Since() = %v after a seed, want %v", got, began)
	}
}

func TestSeedSkipsAnEntryWithNoXUID(t *testing.T) {
	r := New()
	r.Seed([]Entry{{XUID: "", Username: "Ghost"}})
	if len(r.Online()) != 0 {
		t.Errorf("Online() = %v, want empty: an entry with no XUID has no identity", r.Online())
	}
}

func TestEndSessionAfterASeedForgetsWhoIsHere(t *testing.T) {
	r := New()
	r.Seed([]Entry{{XUID: "111", Username: "Steve"}})
	r.EndSession()
	if r.Knows() || r.IsOnline("111") {
		t.Error("the roster still claims to know who is here after EndSession")
	}
}
```

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/roster/ -run 'Seed|AfterASeed'`
Expected: FAIL, build error `r.Seed undefined (type *Roster has no field or method Seed)`.

- [ ] **Step 3: Implement**

Insert into `internal/roster/roster.go` right after `EndSession` (after line 171):

```go
// Seed replaces who is present with players, as reported by something other
// than this process's own Bedrock connection: the console bridge, while the
// agent is deliberately out of the world. The roster then knows who is here,
// and every later add Apply sees is an arrival, because nothing is describing
// the world to a client that just logged in.
//
// The agent's own XUID is forgotten, since the agent is not on the server
// while it is being described from outside. Since is left alone: it dates
// what the profile store watched through, and nothing a seed feeds writes to
// the store. Names learned earlier are kept, as EndSession keeps them.
func (r *Roster) Seed(players []Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clearPresence()
	r.agentXUID = ""
	for _, p := range players {
		if p.XUID == "" {
			continue
		}
		r.players[p.XUID] = p.Username
		r.names[p.XUID] = p.Username
	}
	r.snapshotStarted = true
	r.snapshotEnded = true
	r.answered = true
}
```

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test -race ./internal/roster/`
Expected: `ok  	github.com/jdwillmsen/minecraft-server-agent/internal/roster`. The new tests pass and so does every existing one.

- [ ] **Step 5: Commit**

```bash
gofmt -l internal/roster
git add internal/roster/roster.go internal/roster/roster_test.go
git commit -m "feat(roster): seed presence from a source other than the game connection"
```
Expected: `gofmt -l` prints nothing, and the commit succeeds.

---

### Task 3: `bridgeRoster`: follow the server from the console bridge

**Files:**
- Create: `cmd/agent/bridgeroster.go`
- Test: `cmd/agent/bridgeroster_test.go`

**Interfaces:**
- Consumes:
  - Task 1: `adapters.BridgeEvent` (with `.XUID()`), `adapters.BridgeEventConnect`, `adapters.BridgeEventDisconnect`, `(*adapters.BridgeClient).Events`, `(*adapters.BridgeClient).OnlinePlayers`
  - Task 2: `(*roster.Roster).Seed([]roster.Entry)`
  - Existing: `(*roster.Roster).Apply` (`internal/roster/roster.go:229`); `joinTimes.connected/joined/left` (`cmd/agent/joins.go:58-100`); `connectionEnded(*roster.Roster, *joinTimes)` (`cmd/agent/main.go:471-474`); `waitOrShutdown(ctx, time.Duration) bool` (`main.go:545-552`); `nameArchive` (`cmd/agent/announcing.go:122-124`, which `store.Store` satisfies)
  - Existing test helpers: `waitUntil` (`cmd/agent/leading_test.go:463`), `quiet()` (`leading_test.go:161`), `recordedNames` (`cmd/agent/announcing_test.go:395-408`)
- Produces:
  - `type bridgeFeed interface { Events(ctx context.Context, since int64) ([]adapters.BridgeEvent, error); OnlinePlayers(ctx context.Context) ([]string, error) }`
  - `func newBridgeRoster(feed bridgeFeed, r *roster.Roster, joins *joinTimes, archive nameArchive, log *logging.Logger) *bridgeRoster`
  - `func (b *bridgeRoster) run(ctx context.Context)`: blocks until ctx ends. It then leaves the roster not knowing who is online and ends the join clock's connection.

- [ ] **Step 1: Write the failing tests**

Create `cmd/agent/bridgeroster_test.go`:

```go
package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/adapters"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
)

// fakeBridgeFeed is the console bridge as the roster follower reads it: a
// log of numbered events that starts again when the bridge restarts, and
// whatever `list` currently answers.
type fakeBridgeFeed struct {
	mu      sync.Mutex
	nextID  int64
	events  []adapters.BridgeEvent
	online  []string
	listErr error
	lists   int
}

var _ bridgeFeed = (*fakeBridgeFeed)(nil)

func (f *fakeBridgeFeed) Events(_ context.Context, since int64) ([]adapters.BridgeEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []adapters.BridgeEvent
	for _, e := range f.events {
		if e.ID > since {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeBridgeFeed) OnlinePlayers(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]string{}, f.online...), nil
}

// logged appends a connect or disconnect line the way Bedrock prints it.
func (f *fakeBridgeFeed) logged(kind, name, xuid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	verb := "connected"
	if kind == adapters.BridgeEventDisconnect {
		verb = "disconnected"
	}
	f.nextID++
	f.events = append(f.events, adapters.BridgeEvent{
		ID:     f.nextID,
		Type:   kind,
		Player: name,
		Raw:    "[2026-09-23 10:00:00:000 INFO] Player " + verb + ": " + name + ", xuid: " + xuid,
	})
}

func (f *fakeBridgeFeed) listAnswers(names []string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.online, f.listErr = names, err
}

// restart is the bridge process coming back: its event IDs begin again.
func (f *fakeBridgeFeed) restart(online ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID, f.events, f.online = 0, nil, online
}

func (f *fakeBridgeFeed) listCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lists
}

type runningBridgeRoster struct {
	roster *roster.Roster
	joins  *joinTimes
	stop   func()
}

// startBridgeRoster runs a follower on fast timings until the test stops it
// or ends.
func startBridgeRoster(t *testing.T, feed bridgeFeed, archive nameArchive, reseed time.Duration) runningBridgeRoster {
	t.Helper()
	r := roster.New()
	j := newJoinTimes()
	b := newBridgeRoster(feed, r, j, archive, quiet())
	b.poll = time.Millisecond
	b.reseed = reseed
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.run(ctx)
	}()
	stop := func() {
		cancel()
		<-done
	}
	t.Cleanup(stop)
	return runningBridgeRoster{roster: r, joins: j, stop: stop}
}

func TestBridgeRosterSeedsFromListResolvingXUIDs(t *testing.T) {
	feed := &fakeBridgeFeed{}
	// Steve's XUID is in the bridge's own backlog, Alex's only in the
	// profile store, and Sam's nowhere.
	feed.logged(adapters.BridgeEventConnect, "Steve", "111")
	feed.listAnswers([]string{"Steve", "Alex", "Sam"}, nil)
	got := startBridgeRoster(t, feed, recordedNames{byName: map[string]string{"Alex": "222"}}, time.Hour)

	waitUntil(t, got.roster.Knows, "the roster never learned who is online")
	if !got.roster.IsOnline("111") || !got.roster.IsOnline("222") {
		t.Errorf("Online() = %v, want Steve (111) and Alex (222)", got.roster.Online())
	}
	if n := len(got.roster.Online()); n != 2 {
		t.Errorf("Online() holds %d players, want 2: a player with no known XUID cannot be keyed", n)
	}
	if _, ok := got.joins.SinceConnect(); !ok {
		t.Error("SinceConnect() reports no connection: the seed must stand in for everyone it found")
	}
}

func TestBridgeRosterFollowsJoinsAndLeaves(t *testing.T) {
	feed := &fakeBridgeFeed{}
	feed.listAnswers(nil, nil)
	got := startBridgeRoster(t, feed, recordedNames{}, time.Hour)
	waitUntil(t, got.roster.Knows, "the roster never learned who is online")

	feed.logged(adapters.BridgeEventConnect, "Alex", "222")
	waitUntil(t, func() bool { return got.roster.IsOnline("222") }, "a connect line never put Alex on the roster")
	if _, ok := got.joins.SinceJoin("222"); !ok {
		t.Error("no arrival recorded for Alex: an announcement could be recorded against a client still loading")
	}

	feed.logged(adapters.BridgeEventDisconnect, "Alex", "222")
	waitUntil(t, func() bool { return !got.roster.IsOnline("222") }, "a disconnect line never took Alex off the roster")
	if _, ok := got.joins.SinceJoin("222"); ok {
		t.Error("Alex's arrival is still remembered after they left")
	}
}

func TestBridgeRosterReseedsWhenTheBridgeRestarts(t *testing.T) {
	feed := &fakeBridgeFeed{}
	feed.logged(adapters.BridgeEventConnect, "Steve", "111")
	feed.listAnswers([]string{"Steve"}, nil)
	got := startBridgeRoster(t, feed, recordedNames{byName: map[string]string{"Alex": "222"}}, time.Hour)
	waitUntil(t, func() bool { return got.roster.IsOnline("111") }, "the first seed never landed")

	feed.restart("Alex")

	waitUntil(t, func() bool {
		return got.roster.IsOnline("222") && !got.roster.IsOnline("111")
	}, "a restarted bridge was never noticed: the roster still holds the old population")
}

func TestBridgeRosterRetriesASeedTheBridgeCouldNotAnswer(t *testing.T) {
	feed := &fakeBridgeFeed{}
	feed.logged(adapters.BridgeEventConnect, "Steve", "111")
	feed.listAnswers(nil, errors.New("the names do not match the count the server gave"))
	got := startBridgeRoster(t, feed, recordedNames{}, time.Hour)

	waitUntil(t, func() bool { return feed.listCalls() >= 2 }, "a failed seed was never retried")
	if got.roster.Knows() {
		t.Fatal("Knows() = true with no seed ever landing")
	}

	feed.listAnswers([]string{"Steve"}, nil)
	waitUntil(t, func() bool { return got.roster.IsOnline("111") }, "the seed never landed once list answered")
}

// Anything the bridge missed, or a player resolved only later, is corrected
// by a periodic seed rather than waiting for a bridge restart.
func TestBridgeRosterReseedsPeriodically(t *testing.T) {
	feed := &fakeBridgeFeed{}
	feed.listAnswers([]string{"Steve"}, nil)
	archive := recordedNames{byName: map[string]string{"Steve": "111", "Alex": "222"}}
	got := startBridgeRoster(t, feed, archive, 20*time.Millisecond)
	waitUntil(t, func() bool { return got.roster.IsOnline("111") }, "the first seed never landed")

	feed.listAnswers([]string{"Steve", "Alex"}, nil)
	waitUntil(t, func() bool { return got.roster.IsOnline("222") }, "no later seed picked up a player the events never mentioned")
}

func TestBridgeRosterLeavesTheRosterUnknowingWhenItStops(t *testing.T) {
	feed := &fakeBridgeFeed{}
	feed.logged(adapters.BridgeEventConnect, "Steve", "111")
	feed.listAnswers([]string{"Steve"}, nil)
	got := startBridgeRoster(t, feed, recordedNames{}, time.Hour)
	waitUntil(t, got.roster.Knows, "the roster never learned who is online")
	before := got.joins.Generation()

	got.stop()

	if got.roster.Knows() || got.roster.IsOnline("111") {
		t.Error("the roster still claims to know who is online after the follower stopped")
	}
	if got.joins.Generation() == before {
		t.Error("the join clock's connection did not end: a delivery scheduled under the follower would still speak")
	}
}
```

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./cmd/agent/ -run BridgeRoster`
Expected: FAIL, build errors `undefined: bridgeFeed`, `undefined: newBridgeRoster`.

- [ ] **Step 3: Implement**

Create `cmd/agent/bridgeroster.go`:

```go
package main

import (
	"context"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/adapters"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// bridgeRosterPoll spaces reads of the bridge's event log. The log is held in
// the bridge's memory and costs no console command, so the poll can be short
// enough that a join is on the roster before anything is sent to that player.
const bridgeRosterPoll = 2 * time.Second

// bridgeRosterReseed is how often the population is read again from `list`.
// The event log only reports changes. A player whose XUID could not be
// resolved at the last seed, or a line the bridge never parsed, stays wrong
// until the next seed corrects it.
const bridgeRosterReseed = 5 * time.Minute

// bridgeFeed is the console bridge as the roster follower reads it.
type bridgeFeed interface {
	Events(ctx context.Context, since int64) ([]adapters.BridgeEvent, error)
	OnlinePlayers(ctx context.Context) ([]string, error)
}

var _ bridgeFeed = (*adapters.BridgeClient)(nil)

// bridgeRoster keeps the live roster answering while the agent is
// deliberately out of the world. It writes the roster and the join clock that
// a session would otherwise feed, so the announcement deliverer reads the
// same two objects either way.
//
// It publishes no bus events and writes nothing to the profile store.
// Greetings, join drains and playtime all belong to a session that is
// watching, and running them off console lines would change what players get
// while the agent is away.
type bridgeRoster struct {
	feed    bridgeFeed
	roster  *roster.Roster
	joins   *joinTimes
	archive nameArchive
	poll    time.Duration
	reseed  time.Duration
	log     *logging.Logger
}

func newBridgeRoster(feed bridgeFeed, r *roster.Roster, joins *joinTimes, archive nameArchive, log *logging.Logger) *bridgeRoster {
	return &bridgeRoster{feed: feed, roster: r, joins: joins, archive: archive, poll: bridgeRosterPoll, reseed: bridgeRosterReseed, log: log}
}

// run follows the server until ctx ends. It never returns early, because the
// caller treats its return as the end of the agent's absence.
func (b *bridgeRoster) run(ctx context.Context) {
	// Once nothing is following the bridge, what this learned stops being
	// true, just as a dead connection's roster does.
	defer connectionEnded(b.roster, b.joins)
	for {
		if cursor, ok := b.seed(ctx); ok {
			b.follow(ctx, cursor)
		}
		if !waitOrShutdown(ctx, b.poll) {
			return
		}
	}
}

// seed replaces the roster with who `list` says is online and reports the
// newest event ID it has already taken into account.
//
// The backlog is read before `list`, not after. Every event is applied again
// from that cursor onward, and applying a join or a leave the list already
// reflects changes nothing. Reading in the other order would lose any change
// that landed between the two reads.
func (b *bridgeRoster) seed(ctx context.Context) (int64, bool) {
	backlog, err := b.feed.Events(ctx, 0)
	if err != nil {
		b.seedFailed(err)
		return 0, false
	}
	var cursor int64
	logged := make(map[string]string)
	for _, e := range backlog {
		cursor = e.ID
		if e.Type == adapters.BridgeEventConnect {
			if xuid := e.XUID(); xuid != "" {
				logged[e.Player] = xuid
			}
		}
	}

	names, err := b.feed.OnlinePlayers(ctx)
	if err != nil {
		b.seedFailed(err)
		return 0, false
	}
	players := make([]roster.Entry, 0, len(names))
	for _, name := range names {
		xuid, ok := b.resolve(ctx, name, logged)
		if !ok {
			// Left out rather than guessed. The roster then knows the server
			// and not this player, which the deliverer reads as departed, so
			// their copy of an announcement stays pending and is not recorded.
			b.log.Info("bridge_roster_unresolved", logging.Fields{"gamertag": name})
			continue
		}
		players = append(players, roster.Entry{XUID: xuid, Username: name})
	}
	b.roster.Seed(players)
	b.joins.connected()
	b.log.Info("bridge_roster_seeded", logging.Fields{"players": len(players), "unresolved": len(names) - len(players)})
	return cursor, true
}

// seedFailed drops what an earlier seed reported. A roster that cannot be
// refreshed must say it does not know, not repeat a population that may have
// left. Debug for the same reason as tps_sample_failed: a bridge outage would
// otherwise add a line every poll.
func (b *bridgeRoster) seedFailed(err error) {
	connectionEnded(b.roster, b.joins)
	b.log.Debug("bridge_roster_seed_failed", logging.Fields{"error": err.Error()})
}

// follow applies the bridge's events after cursor until ctx ends, the bridge
// restarts, or it is time to seed again.
//
// The bridge's IDs restart with its process and it has no epoch to compare,
// so each poll asks again for the event at the cursor. If that event is
// missing, the log this cursor points into is gone.
func (b *bridgeRoster) follow(ctx context.Context, cursor int64) {
	reseedAt := time.Now().Add(b.reseed)
	for waitOrShutdown(ctx, b.poll) {
		if time.Now().After(reseedAt) {
			return
		}
		events, err := b.feed.Events(ctx, max(cursor-1, 0))
		if err != nil {
			// Kept, not cleared: the bridge holds its log across its own
			// outage, and the next poll picks up where this one stopped.
			b.log.Debug("bridge_roster_poll_failed", logging.Fields{"error": err.Error()})
			continue
		}
		if cursor > 0 && (len(events) == 0 || events[0].ID != cursor) {
			b.log.Info("bridge_roster_reset", logging.Fields{"cursor": cursor})
			connectionEnded(b.roster, b.joins)
			return
		}
		for _, e := range events {
			if e.ID <= cursor {
				continue
			}
			b.apply(ctx, e)
			cursor = e.ID
		}
	}
}

// apply puts one connect or disconnect on the roster and the join clock, as
// handlePlayerList does for a session's packets.
func (b *bridgeRoster) apply(ctx context.Context, e adapters.BridgeEvent) {
	var remove bool
	switch e.Type {
	case adapters.BridgeEventConnect:
	case adapters.BridgeEventDisconnect:
		remove = true
	default:
		return
	}
	xuid := e.XUID()
	if xuid == "" {
		var ok bool
		if xuid, ok = b.resolve(ctx, e.Player, nil); !ok {
			b.log.Info("bridge_roster_unresolved", logging.Fields{"gamertag": e.Player})
			return
		}
	}
	joins, leaves, _ := b.roster.Apply([]roster.PlayerListEntry{{XUID: xuid, Username: e.Player, Remove: remove}})
	for _, j := range joins {
		b.joins.joined(j.XUID)
		b.log.Info("bridge_player_joined", logging.Fields{"xuid": j.XUID, "username": j.Username})
	}
	for _, l := range leaves {
		b.joins.left(l.XUID)
		b.log.Info("bridge_player_left", logging.Fields{"xuid": l.XUID, "username": l.Username})
	}
}

// resolve turns a gamertag into the XUID everything else keys on: first from
// the bridge's own connect lines, then from the profile store.
func (b *bridgeRoster) resolve(ctx context.Context, name string, logged map[string]string) (string, bool) {
	if xuid, ok := logged[name]; ok {
		return xuid, true
	}
	xuid, ok, err := b.archive.XUIDForName(ctx, name)
	if err != nil {
		b.log.Debug("bridge_roster_lookup_failed", logging.Fields{"gamertag": name, "error": err.Error()})
		return "", false
	}
	return xuid, ok
}
```

The event names are `bridge_player_joined` and `bridge_player_left`, not `player_joined` and `player_left`. That keeps anything already counting the session's lines from counting a second source.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test -race ./cmd/agent/ -run BridgeRoster -count=3 -v`
Expected: PASS for all six `TestBridgeRoster*` tests on each of the 3 runs, with no race reports.

- [ ] **Step 5: Commit**

```bash
gofmt -l cmd/agent
git add cmd/agent/bridgeroster.go cmd/agent/bridgeroster_test.go
git commit -m "feat(agent): follow the online roster from the console bridge"
```
Expected: `gofmt -l` prints nothing, and the commit succeeds.

---

### Task 4: `sessionGate` and `runSessions`: the session lifecycle inside a leader turn

**Files:**
- Create: `cmd/agent/sessions.go`
- Test: `cmd/agent/sessions_test.go`

**Interfaces:**
- Consumes: `steps` (`cmd/agent/leading_test.go:74-89`), `waitUntil`, `quiet()` (test only).
- Produces (plan 3 plugs presence in through the first one. It implements the method structurally from `internal/presence` and passes the value where `main` now passes `alwaysPresent{}`):
  - `type sessionGate interface { Wanted() (present bool, changed <-chan struct{}) }`. `present` says whether this leader should be in the world now. `changed` is closed the next time that answer *may* have changed, and a nil channel means it never will. A spurious close is allowed, because `runSessions` re-reads the answer before acting on it.
  - `type alwaysPresent struct{}`, whose `Wanted()` returns `(true, nil)`.
  - `type sessionModes struct { present func(context.Context); left func(); absent func(context.Context) }`
  - `func runSessions(ctx context.Context, gate sessionGate, modes sessionModes, log *logging.Logger)`: blocks until ctx ends. It runs `present` or `absent` on a child context and switches when the gate's answer changes. It calls `left()` only after `present` has returned because the gate closed, never when ctx ended.

- [ ] **Step 1: Write the failing tests**

Create `cmd/agent/sessions_test.go`:

```go
package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeGate is a presence decision a test flips by hand.
type fakeGate struct {
	mu      sync.Mutex
	present bool
	changed chan struct{}
}

func newFakeGate(present bool) *fakeGate {
	return &fakeGate{present: present, changed: make(chan struct{})}
}

func (g *fakeGate) Wanted() (bool, <-chan struct{}) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.present, g.changed
}

// set records a new answer and wakes whoever is waiting, whether or not the
// answer differs, as a store re-read would.
func (g *fakeGate) set(present bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.present = present
	close(g.changed)
	g.changed = make(chan struct{})
}

// recordedModes logs each mode starting and ending, and each settled leave,
// into one ordered list.
func recordedModes(order *steps) sessionModes {
	return sessionModes{
		present: func(ctx context.Context) {
			order.add("present")
			<-ctx.Done()
			order.add("present_ended")
		},
		left: func() { order.add("left") },
		absent: func(ctx context.Context) {
			order.add("absent")
			<-ctx.Done()
			order.add("absent_ended")
		},
	}
}

func startSessions(t *testing.T, gate sessionGate, modes sessionModes) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runSessions(ctx, gate, modes, quiet())
	}()
	stop = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("runSessions did not return after its context ended")
		}
	}
	t.Cleanup(func() { cancel(); <-done })
	return stop
}

func equalSteps(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// Today's behavior, which this phase must keep: the leader is always in the
// world, and a shutdown is not a deliberate leave.
func TestAlwaysPresentRunsTheSessionForTheWholeTurn(t *testing.T) {
	order := &steps{}
	stop := startSessions(t, alwaysPresent{}, recordedModes(order))
	waitUntil(t, func() bool { return len(order.list()) == 1 }, "the session never started")

	stop()

	if got, want := order.list(), []string{"present", "present_ended"}; !equalSteps(got, want) {
		t.Errorf("steps = %v, want %v", got, want)
	}
}

func TestClosingTheGateEndsTheSessionThenFollowsFromOutside(t *testing.T) {
	order := &steps{}
	gate := newFakeGate(true)
	startSessions(t, gate, recordedModes(order))
	waitUntil(t, func() bool { return len(order.list()) == 1 }, "the session never started")

	gate.set(false)

	want := []string{"present", "present_ended", "left", "absent"}
	waitUntil(t, func() bool { return equalSteps(order.list(), want) }, "closing the gate did not end the session, settle it and start following")
}

func TestReopeningTheGateStopsFollowingAndRejoins(t *testing.T) {
	order := &steps{}
	gate := newFakeGate(false)
	startSessions(t, gate, recordedModes(order))
	waitUntil(t, func() bool { return len(order.list()) == 1 }, "following never started")

	gate.set(true)

	want := []string{"absent", "absent_ended", "present"}
	waitUntil(t, func() bool { return equalSteps(order.list(), want) }, "reopening the gate did not stop following and rejoin")
}

func TestASpuriousGateSignalDoesNotRestartTheSession(t *testing.T) {
	order := &steps{}
	gate := newFakeGate(true)
	startSessions(t, gate, recordedModes(order))
	waitUntil(t, func() bool { return len(order.list()) == 1 }, "the session never started")

	gate.set(true)
	gate.set(true)
	// Nothing to wait for: the assertion is that nothing happens. The
	// sleep only gives a wrong implementation time to act.
	time.Sleep(20 * time.Millisecond)

	if got := order.list(); !equalSteps(got, []string{"present"}) {
		t.Errorf("steps = %v, want the one session still running", got)
	}
}

func TestShutdownWhileAbsentDoesNotSettleALeave(t *testing.T) {
	order := &steps{}
	gate := newFakeGate(true)
	stop := startSessions(t, gate, recordedModes(order))
	waitUntil(t, func() bool { return len(order.list()) == 1 }, "the session never started")
	gate.set(false)
	waitUntil(t, func() bool { return len(order.list()) == 4 }, "following never started")

	stop()

	want := []string{"present", "present_ended", "left", "absent", "absent_ended"}
	if got := order.list(); !equalSteps(got, want) {
		t.Errorf("steps = %v, want %v: the leave was already settled once", got, want)
	}
}

// The turn's own handover settles a session that ends with the turn, so
// settling it here as well would close the same playtime twice.
func TestShutdownWhilePresentDoesNotSettleALeave(t *testing.T) {
	order := &steps{}
	stop := startSessions(t, newFakeGate(true), recordedModes(order))
	waitUntil(t, func() bool { return len(order.list()) == 1 }, "the session never started")

	stop()

	for _, s := range order.list() {
		if s == "left" {
			t.Fatalf("steps = %v: a session ended by the turn was settled as a deliberate leave", order.list())
		}
	}
}
```

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./cmd/agent/ -run 'Gate|Present|Absent|Spurious'`
Expected: FAIL, build errors `undefined: sessionModes`, `undefined: runSessions`, `undefined: alwaysPresent`, `undefined: sessionGate`.

- [ ] **Step 3: Implement**

Create `cmd/agent/sessions.go`:

```go
package main

import (
	"context"

	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// sessionGate decides whether the live agent should be in the world. It is
// a separate question from whether this process is the live agent: the lock
// decides that, and the monitoring that comes with the lock keeps running
// either way.
type sessionGate interface {
	// Wanted reports whether a session should run now, and a channel closed
	// the next time that may change. A nil channel never closes. A close
	// with no change is allowed, and the answer is read again before anything
	// acts on it.
	Wanted() (present bool, changed <-chan struct{})
}

// alwaysPresent is the gate for a deployment with nothing deciding presence:
// the live agent is always in the world, as every release before the gate
// was.
type alwaysPresent struct{}

func (alwaysPresent) Wanted() (bool, <-chan struct{}) { return true, nil }

// sessionModes is what runSessions switches between.
type sessionModes struct {
	// present runs the connect loop until its context ends.
	present func(context.Context)
	// left settles what ending a session on purpose owes, once present has
	// returned. It is not called when the turn itself ends, because the
	// turn's handover settles that and settling twice would close the same
	// playtime twice.
	left func()
	// absent keeps the roster answering from outside the world until its
	// context ends.
	absent func(context.Context)
}

// runSessions is the session lifecycle inside one turn as the live agent:
// in the world while the gate says so, following it from the console bridge
// while it does not, until ctx ends.
//
// A mode always returns before the next one starts. The two write the same
// roster, and a follower still applying events after the connection's
// BeginSession would put players back that the server's snapshot never
// named.
func runSessions(ctx context.Context, gate sessionGate, modes sessionModes, log *logging.Logger) {
	for ctx.Err() == nil {
		present, changed := gate.Wanted()
		modeCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			if present {
				modes.present(modeCtx)
			} else {
				modes.absent(modeCtx)
			}
		}()
		awaitGateChange(ctx, gate, present, changed)
		cancel()
		<-done
		if ctx.Err() != nil {
			return
		}
		if present {
			modes.left()
		}
		log.Info("session_gate_changed", logging.Fields{"present": !present})
	}
}

// awaitGateChange returns when ctx ends or the gate's answer is no longer
// present.
func awaitGateChange(ctx context.Context, gate sessionGate, present bool, changed <-chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-changed:
			now, next := gate.Wanted()
			if now != present {
				return
			}
			changed = next
		}
	}
}
```

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test -race ./cmd/agent/ -run 'Gate|Present|Absent|Spurious' -count=3 -v`
Expected: PASS. The six tests from this task are among those run, on each of the 3 runs, with no race reports.

- [ ] **Step 5: Commit**

```bash
gofmt -l cmd/agent
git add cmd/agent/sessions.go cmd/agent/sessions_test.go
git commit -m "feat(agent): gate the in-world session separately from the leader turn"
```
Expected: `gofmt -l` prints nothing, and the commit succeeds.

---

### Task 5: Wire the two lifecycles into `main`, and verify nothing visible changed

**Files:**
- Modify: `cmd/agent/main.go:121-125` (the roster comment)
- Modify: `cmd/agent/main.go:300-341` (the turn loop)

**Interfaces:**
- Consumes: Task 3 `newBridgeRoster(feed bridgeFeed, r *roster.Roster, joins *joinTimes, archive nameArchive, log *logging.Logger) *bridgeRoster` and `(*bridgeRoster).run`. Task 4 `sessionGate`, `alwaysPresent{}`, `sessionModes{present, left, absent}` and `runSessions(ctx, gate, modes, log)`. Existing `handover(ctx, term leadership, profiles store.Store, since time.Time, log)` (`cmd/agent/leading.go:213-236`), called with a nil term as `recycleSession` does (`main.go:824-826`).
- Produces: the production wiring. The one line plan 3 changes is `var gate sessionGate = alwaysPresent{}`.

The existing flow tests in `cmd/agent` (`leading_test.go`, `roster_flow_test.go`, the `announcement_*_flow_test.go` files) are the regression net. `main()` itself has no test, and this task adds none, because the logic it wires is covered by Tasks 3 and 4.

- [ ] **Step 1: Confirm the baseline is green**

Run: `go build ./... && go test -race ./cmd/agent/`
Expected: `ok  	github.com/jdwillmsen/minecraft-server-agent/cmd/agent`.

- [ ] **Step 2: Update the roster comment**

In `cmd/agent/main.go`, replace lines 121-124:

```go
	// roster is the live XUID<->gamertag mapping, fed from PlayerList
	// packets (see handlePlayerList). It serves two needs: join detection
	// for the welcome plugin, and gamertag resolution for BridgeVoice.Tell
	// (which only ever receives an XUID).
```

with:

```go
	// roster is the live XUID<->gamertag mapping, fed from PlayerList
	// packets while the agent is in the world (see handlePlayerList) and from
	// the console bridge while it is deliberately out of it (see
	// bridgeRoster). It serves two needs: join detection for the welcome
	// plugin, and gamertag resolution for BridgeVoice.Tell (which only ever
	// receives an XUID).
```

- [ ] **Step 3: Replace the turn loop**

In `cmd/agent/main.go`, replace everything from the comment `// One pass of this loop is one turn as the live agent:` (line 302) through the loop's closing `}` (line 341) with:

```go
	// Nothing decides presence yet, so the live agent is always in the world.
	var gate sessionGate = alwaysPresent{}
	// Built after withPlayerEvents has wrapped playerStore, so a name the
	// follower resolves comes from the same store the session writes.
	bridgeFollower := newBridgeRoster(bridgeClient, playerRoster, joins, playerStore, log)

	// One pass of this loop is one turn as the live agent: wait for the lock,
	// do the live agent's work until the process is shutting down or the lock
	// is gone, then hand over. A process that loses the lock becomes a
	// standby again rather than exiting -- the database blinking must not
	// cost the server its agent, which is exactly what it cost before there
	// was a lock at all.
	//
	// The turn and the session are separate lifecycles. The monitoring the
	// lock entitles this process to runs for the whole turn. The session in
	// the world runs only while the gate wants it, and while it does not,
	// the roster is followed from the console bridge instead.
	for ctx.Err() == nil {
		term, live := awaitLeadership(ctx, election, httpServer.SetRole, log)
		if !live {
			break
		}
		// liveCtx ends with this turn, not with the process: the sessions
		// and every live-only writer below run under it, so losing the lock
		// takes the agent out of the game without taking the process down.
		// The claim on the Xbox Live login is opened and closed with it --
		// see beginTurn.
		//
		// Standing down demotes the role ahead of both, so nothing that
		// reads it acts on a game this process no longer has a claim to
		// while the connect loop is still unwinding.
		liveCtx, endClaim := beginTurn(ctx, tokenGate)
		endTurn := func() {
			httpServer.SetRole(httpapi.RoleStandby)
			endClaim()
		}
		go endTermOnLockLoss(liveCtx, term, endTurn, log)
		// A turn that began without the lock -- because whoever holds it is
		// gone without having released it -- is a turn worth flagging for as
		// long as it lasts.
		go watchForcedLeadership(liveCtx, term, log)
		startLiveWork(liveCtx, cfg, bridgeTimeout, pinger, link, announceStore, deliverer, scheduleStore, moderationStore, log)

		runSessions(liveCtx, gate, sessionModes{
			present: func(sessionCtx context.Context) {
				runConnectLoop(sessionCtx, cfg, ts, log, registry, pctx, eventBus, limiter, httpServer, playerRoster, audience, siblings, permResolver, ans, playerStore, auditor, link, joins)
			},
			// Leaving on purpose owes what a recycle owes: everyone still
			// here keeps the time they were watched for, instead of the
			// next connection's CloseOrphans rewriting it as unknown. No
			// term, because the lock is not being passed on.
			left: func() {
				handover(liveCtx, nil, playerStore, playerRoster.Since(), log)
			},
			absent: bridgeFollower.run,
		}, log)

		endTurn()
		// The agent is out of the game by now -- the connect loop waits for
		// its own disconnect to reach the server -- so the sessions it was
		// watching can be closed at the moment it stopped watching, and only
		// then is the lock passed on.
		handover(ctx, term, playerStore, playerRoster.Since(), log)
	}
```

With `alwaysPresent{}`, `runSessions` calls `present` once per turn and returns when `liveCtx` ends. That is the exact lifetime `runConnectLoop(liveCtx, ...)` had before. `left` and `absent` are unreachable until plan 3 supplies a real gate.

- [ ] **Step 4: Build and run the package tests**

Run: `gofmt -l cmd/agent && go vet ./cmd/agent/ && go test -race ./cmd/agent/`
Expected: `gofmt -l` prints nothing, `go vet` prints nothing, and the test run prints `ok  	github.com/jdwillmsen/minecraft-server-agent/cmd/agent`.

- [ ] **Step 5: Check the diff for behavior changes**

Run: `git diff origin/main --stat -- cmd/agent/main.go && git diff origin/main -- cmd/agent/main.go`
Expected: the only changes are the roster comment, the new `gate` and `bridgeFollower` lines, the rewritten loop comment, and `runConnectLoop(liveCtx, ...)` replaced by `runSessions(liveCtx, gate, sessionModes{...}, log)`. `runConnectLoop`, `session`, `connectionEnded`, `handover`, `startLiveWork` and every log event name are unchanged.

- [ ] **Step 6: Full verification**

Run each command in turn:

```bash
gofmt -l .
go vet ./...
CGO_ENABLED=0 go build ./...
go test -race ./...
golangci-lint run
```

Expected: `gofmt -l .` prints nothing. `go vet` prints nothing. The build succeeds. `go test -race ./...` reports `ok` for every package, including `internal/adapters`, `internal/roster` and `cmd/agent`, with no `FAIL` and no race reports; the Postgres live tests skip as they do on `main` when no database is configured. `golangci-lint run` reports `0 issues.`

- [ ] **Step 7: Commit**

```bash
git add cmd/agent/main.go
git commit -m "refactor(agent): run the session under a presence gate inside the leader turn"
```
Expected: the commit succeeds.

---

## Notes for plan 3 (not work for this plan)

- **Plugging in presence:** implement `Wanted() (bool, <-chan struct{})` on the presence loop's view of this actor's (`PRESENCE_SELF_ID`) effective state. Close the channel whenever a tick or a write may have changed it. Then replace `var gate sessionGate = alwaysPresent{}` in `main`.
- **Readiness while parked:** `runConnectLoop` calls `httpServer.SetReady(false)` after every session (`main.go:502`), so a parked leader answers `/readyz` "not ready" (`internal/httpapi/http.go:80-90`), and the Service stops routing to it. The standby still serves, but plan 3 decides whether a parked leader should report ready.
- **Joins for `wake_on`:** the bridge follower publishes nothing to the bus by design. The policy's "recent joins" need a source that works in both modes: session `roster.JoinEvent`s while present, and `bridgeRoster.apply`'s joins while absent (for example an `onJoin` hook added then).
- **Announcing the rejoin through `say`, and confirming before leaving,** belong to plan 3's chat plugin, not to the lifecycle.

## Deviations from spec/contract

1. **The monitoring already follows the lock, not the session.** The spec says monitoring "only runs while it holds a live in-world session". In the code, `startLiveWork` already starts once per leader turn on `liveCtx` (`cmd/agent/main.go:321-331`, `leading.go:270-286`) and never restarts with a reconnect. The real coupling is that the turn body *is* `runConnectLoop` (`main.go:333`), so there is no way to be leader without trying to be in the world. The plan leaves `startLiveWork` untouched and makes the turn body `runSessions`.
2. **The bridge roster runs only when the gate says absent, not in the reconnect backoff between sessions.** The spec says "while the session is down". In the code, the gap between reconnects is a deliberately "not known" roster (`connectionEnded`, `main.go:456-474`), and several flow tests pin that. Filling it from the bridge would change delivery behavior now, which rollout step 1 rules out ("no visible change").
3. **XUIDs for the seed must be resolved.** The spec says to seed from `list`, but `list` prints gamertags only, while the roster and the deliverer key on XUIDs. The plan resolves them from the bridge's own connect lines (`xuid:` in `raw`) and then `store.XUIDForName`. An unresolved player is left out, is logged, and is corrected at the next seed (every 5 minutes).
4. **The bridge's event IDs are not stable.** The spec says to follow `/events`, but the IDs restart with the bridge process and there is no epoch (`mc-console-bridge/events.go:84-113`). The follower detects a reset by asking again for the event at its cursor, and it reseeds on a timer. No bridge change is needed.
5. **The presence policy loop is not started in this plan.** The spec's leader lifecycle includes "the presence policy loop". It does not exist until plan 3, so this plan only makes room for it: it would start next to `startLiveWork` on `liveCtx`.
6. **The gate type lives in `package main`,** as unexported `sessionGate`, because `runSessions` is its only consumer and this repo's pattern is consumer-side interfaces (`leadership`, `campaigner` in `leading.go`). Plan 3 satisfies it structurally, so no exported name crosses a package boundary.
