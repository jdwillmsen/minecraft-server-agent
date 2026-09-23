# Bridge `kick` for Actors Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** mc-console-bridge's `POST /command` accepts `kick <gamertag>` only when the gamertag is in `BRIDGE_KICKABLE`, so the presence loop can force an actor's session closed while a leaked bridge token still cannot kick a real player.

**Architecture:** A new `Kickable` value (its own file, `kickable.go`) holds the parsed `BRIDGE_KICKABLE` set and answers an ASCII-case-insensitive membership check. `CheckAllowlist` gains a `kickable Kickable` parameter and one new rule, `kick`, whose regex admits exactly one bare or double-quoted name and whose `validate` step checks it against the set; everything else about the allowlist (newline refusal, anchored templates, 403 on refusal) is unchanged. `LoadConfig` parses the variable and fails startup on an entry that could not be passed to the console safely. Execute on a new branch in a fresh worktree created off `origin/main` of `github.com/jdwillmsen/mc-console-bridge` (this plan does not create it); the last tag on `main` is `v0.2.1`.

**Tech Stack:** Go 1.27 (`go.mod`: `go 1.27`, `toolchain go1.27.1`), standard library `regexp`/`strings`/`unicode`, `log/slog`, existing `github.com/coder/websocket` test fake. No new dependencies. Releases: semantic-release 25 via `.github/workflows/semantic-release.yml` → `.github/workflows/release.yml` (GHCR publish + Docker Hub mirror).

**Spec:** `docs/superpowers/specs/2026-09-23-actor-presence-design.md` in the minecraft-server-agent repo (spec worktree ``), section "Bridge: `kick`, limited to actors", rollout step 4, and the testing bullet "Bridge tests: `kick` accepted for an actor, rejected for anyone else". Cross-repo names come from `docs/superpowers/plans/2026-09-23-actor-presence-00-contract.md` beside this plan.

## Global Constraints

- Variable name is exactly `BRIDGE_KICKABLE`: comma-separated gamertags; empty or unset = `kick` refused for everyone (contract, "Environment variables").
- The bridge never kicks a gamertag that is not in `BRIDGE_KICKABLE`, whatever the caller sends ("A leaked bridge token cannot kick a real player").
- `kick` takes exactly one target and no reason argument. A gamertag containing spaces must be sent double-quoted: `kick "Name With Space"`.
- Selectors (`@a`, `@p`, `@r`, `@s`, `@e`, with or without `[...]`) are never accepted as a kick target, quoted or not.
- Name matching is exact apart from ASCII letter case (`AfkBotOne` = `afkbotone`); no trimming, no prefix match, no Unicode case folding.
- Every existing allowlist behaviour stays: newline/carriage-return refusal, anchored templates, `403` with the refusal reason, `mc_console_bridge_commands_refused_total` on refusal.
- `go.mod` and `go.sum` are untouched by this plan.
- `gofmt -l .` prints nothing; `go vet ./...`, `CGO_ENABLED=0 go build ./...` and `go test -race ./...` are clean (these are exactly the CI steps in `.github/workflows/ci.yml`).
- Comments explain why, never what; match the surrounding density. No ticket IDs in code, comments, docs or commit messages.
- Conventional commits. The release is driven by commit type: at least one `feat` commit must land so semantic-release cuts `v0.3.0`. Every commit is signed (the repo sets `commit.gpgsign=true` and `verify-pr-signatures.yml` checks the PR) — do not override the signing identity. End each commit message with:
  ```
  Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
  ```

## Review Focus

- A gamertag with a space sent unquoted (`kick Afk Bot Two`): Bedrock reads it as target `Afk` plus a reason. Expected: refused, never "the closest actor". Pinned in Task 2 (`TestCheckAllowlist_KickRefused`).
- A name that differs from an actor only by a Unicode case-fold lookalike (Kelvin sign `K`, U+212A): `strings.EqualFold` would accept it. Expected: refused. Pinned in Task 1 (`TestKickableContains_IgnoresOnlyASCIICase`) and Task 2.
- A deployment that never sets `BRIDGE_KICKABLE` (every bridge running today): expected every `kick` refused with 403 and nothing sent to the console. Pinned in Task 2 (`TestCommand_KickRefusedWithoutKickableList`) and Task 3 (`TestLoadConfig_KickableDefaultsToNobody`).
- A chart that renders a malformed `BRIDGE_KICKABLE` (a stray quote, a selector): expected a startup error naming the variable, not a bridge that silently drops the entry and leaves the actor unkickable. Pinned in Task 1 (`TestParseKickable_UnsafeEntryIsAnError`) and Task 3 (`TestLoadConfig_KickableUnsafeEntryIsAnError`).
- A refused kick reaching the server anyway (the check running after the console write, or on a different string than the one sent): expected nothing on the console's stdin. Pinned in Task 2 (`TestCommand_KickReachesConsoleOnlyForActors` asserts the fake console received exactly the one accepted line).

---

## File Structure

- `kickable.go` (create) — `Kickable` type, `ParseKickable`, `Len`, `Contains`, and the private `foldASCII`. One responsibility: what `BRIDGE_KICKABLE` means. Kept out of `allowlist.go` so the template table stays a table.
- `kickable_test.go` (create) — parsing and membership tests.
- `allowlist.go` (modify, lines 21-97) — `validate` gains the `Kickable` argument, the `kick` rule is appended to `rules`, `CheckAllowlist` gains the `kickable Kickable` parameter.
- `allowlist_test.go` (modify lines 21 and 49; append) — existing calls pass `Kickable{}`; kick acceptance and refusal tables.
- `config.go` (modify lines 23-46 and 95-124) — `Config.Kickable`, parsed in `LoadConfig`.
- `config_test.go` (append) — env loading tests.
- `http.go` (modify line 125) — `handleCommand` passes `s.cfg.Kickable`.
- `http_test.go` (modify imports lines 3-11; append) — end-to-end `POST /command` tests against the existing fake console from `console_test.go` (`startFakeConsole`, `writeTestMsg`, `readTestMsg`, `waitConnected`, `testLogger`, `testOrigin`, `testServer`, `testConsole`).
- `main.go` (modify line 52) — startup log reports how many actors are kickable.
- `README.md` (modify lines 44-60 and 80-87) — allowlist table row and env var row.

---

### Task 1: `Kickable` set

**Files:**
- Create: `kickable.go`
- Test: `kickable_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `type Kickable struct { /* unexported */ }` (zero value refuses everyone); `func ParseKickable(raw string) (Kickable, error)`; `func (k Kickable) Len() int`; `func (k Kickable) Contains(name string) bool`.

- [ ] **Step 1: Write the failing test**

Create `kickable_test.go`:

```go
package main

import "testing"

func TestParseKickable(t *testing.T) {
	k, err := ParseKickable(" AfkBotOne, Afk Bot Two ,ServerAgent,")
	if err != nil {
		t.Fatalf("ParseKickable: %v", err)
	}
	if k.Len() != 3 {
		t.Errorf("Len = %d, want 3", k.Len())
	}
	for _, name := range []string{"AfkBotOne", "afkbotone", "AFKBOTONE", "Afk Bot Two", "afk bot two", "ServerAgent"} {
		if !k.Contains(name) {
			t.Errorf("Contains(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", "Steve", "AfkBot", "AfkBotOne2", " AfkBotOne", "Afk  Bot Two", "AfkBotTwo"} {
		if k.Contains(name) {
			t.Errorf("Contains(%q) = true, want false", name)
		}
	}
}

// Unicode case folding maps the Kelvin sign to "k", so a folding match would
// accept a name the operator never listed. Only ASCII case is ignored.
func TestKickableContains_IgnoresOnlyASCIICase(t *testing.T) {
	k, err := ParseKickable("AfkBotOne")
	if err != nil {
		t.Fatalf("ParseKickable: %v", err)
	}
	if k.Contains("AfKBotOne") {
		t.Error(`Contains("AfKBotOne") = true, want false: the Kelvin sign is not the letter k`)
	}
}

func TestParseKickable_EmptyRefusesEveryone(t *testing.T) {
	for _, raw := range []string{"", " ", ",", " , ,"} {
		k, err := ParseKickable(raw)
		if err != nil {
			t.Fatalf("ParseKickable(%q): %v", raw, err)
		}
		if k.Len() != 0 {
			t.Errorf("ParseKickable(%q).Len = %d, want 0", raw, k.Len())
		}
		if k.Contains("AfkBotOne") {
			t.Errorf("ParseKickable(%q) made AfkBotOne kickable", raw)
		}
	}
	if (Kickable{}).Contains("AfkBotOne") {
		t.Error("the zero Kickable made AfkBotOne kickable")
	}
}

func TestParseKickable_DuplicatesCollapse(t *testing.T) {
	k, err := ParseKickable("AfkBotOne,afkbotone")
	if err != nil {
		t.Fatalf("ParseKickable: %v", err)
	}
	if k.Len() != 1 {
		t.Errorf("Len = %d, want 1", k.Len())
	}
}

// An entry that could change how the console parses `kick "<name>"` is a
// startup error, not a silently dropped name: a quote or backslash could end
// the quoted name early, a leading @ is a selector, and a control character
// has no place in a gamertag.
func TestParseKickable_UnsafeEntryIsAnError(t *testing.T) {
	for _, raw := range []string{`Afk"Bot`, `Afk\Bot`, "@a", "@p[name=Steve]", "Afk\tBot", "AfkBot\x00"} {
		if _, err := ParseKickable("AfkBotOne," + raw); err == nil {
			t.Errorf("ParseKickable with entry %q returned no error", raw)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -race -run Kickable ./...`
Expected: build failure, `./kickable_test.go:6:12: undefined: ParseKickable`.

- [ ] **Step 3: Write minimal implementation**

Create `kickable.go`:

```go
package main

import (
	"fmt"
	"strings"
	"unicode"
)

// Kickable is the set of gamertags `kick` may name: the server's own actors
// (the agent and the AFK bots), never a real player, so a leaked bridge token
// still cannot remove one. The zero value refuses every kick.
type Kickable struct {
	names map[string]struct{}
}

// ParseKickable reads BRIDGE_KICKABLE's comma-separated gamertags. Blank
// entries are skipped, so an empty value or a trailing comma is harmless.
// An entry that could change how the console parses the command is an error
// rather than a dropped name: a quote or backslash could end a quoted name
// early, and a leading @ is a selector, not a player.
func ParseKickable(raw string) (Kickable, error) {
	k := Kickable{names: make(map[string]struct{})}
	for entry := range strings.SplitSeq(raw, ",") {
		name := strings.TrimSpace(entry)
		if name == "" {
			continue
		}
		if strings.HasPrefix(name, "@") || strings.ContainsAny(name, `"\`) || strings.ContainsFunc(name, unicode.IsControl) {
			return Kickable{}, fmt.Errorf("gamertag %q cannot be passed to kick safely", name)
		}
		k.names[foldASCII(name)] = struct{}{}
	}
	return k, nil
}

// Len reports how many distinct gamertags are kickable.
func (k Kickable) Len() int {
	return len(k.names)
}

// Contains reports whether name is kickable, ignoring ASCII case only. Full
// Unicode folding would accept names the operator never listed (the Kelvin
// sign folds to "k"), and whom such a name reaches is the console's call,
// not this check's.
func (k Kickable) Contains(name string) bool {
	_, ok := k.names[foldASCII(name)]
	return ok
}

func foldASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -race -run Kickable -v ./... 2>&1 | grep -E '^(--- |ok|FAIL)'`
Expected:
```
--- PASS: TestParseKickable (0.00s)
--- PASS: TestKickableContains_IgnoresOnlyASCIICase (0.00s)
--- PASS: TestParseKickable_EmptyRefusesEveryone (0.00s)
--- PASS: TestParseKickable_DuplicatesCollapse (0.00s)
--- PASS: TestParseKickable_UnsafeEntryIsAnError (0.00s)
ok  	github.com/jdwillmsen/mc-console-bridge	...
```
Then `gofmt -l . && go vet ./...` prints nothing.

- [ ] **Step 5: Commit**

```bash
git add kickable.go kickable_test.go
git commit -m "feat(kick): parse the set of gamertags the bridge may kick

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 2: `kick` allowlist rule, wired into `POST /command`

**Files:**
- Modify: `allowlist.go:21-27` (rule struct `validate` field), `allowlist.go:46` (tellraw `validate` signature), `allowlist.go:65-71` (append the rule after `gamerule_query`), `allowlist.go:73-97` (`CheckAllowlist`)
- Modify: `config.go:43-46` (`Config.Kickable` field only; loading is Task 3)
- Modify: `http.go:125`
- Test: `allowlist_test.go:21`, `allowlist_test.go:49`, append; `http_test.go:3-11` imports, append

**Interfaces:**
- Consumes: `Kickable`, `ParseKickable`, `Kickable.Contains` from Task 1. From `console_test.go` (same package): `startFakeConsole(t, handler) (addr string, connections *atomic.Int64)`, `writeTestMsg`, `readTestMsg`, `waitConnected(t, c)`, `testLogger()`, `testOrigin`, `testServer(t, c) http.Handler`, `testConsole()`. From `http.go`: `commandRequest`, `commandResponse`, `newMux`, `server`.
- Produces: `func CheckAllowlist(cmd string, kickable Kickable) (string, error)` returning rule name `"kick"` for an accepted kick; `Config.Kickable Kickable`. Callers (the agent's presence loop, plan 3) send exactly `kick <name>` for a name without spaces or `kick "<name>"` for any name; the quoted form is always safe to send.

- [ ] **Step 1: Write the failing tests**

In `allowlist_test.go`, change line 21 from `rule, err := CheckAllowlist(c.cmd)` to:

```go
		rule, err := CheckAllowlist(c.cmd, Kickable{})
```

and line 49 from `if _, err := CheckAllowlist(cmd); err == nil {` to:

```go
		if _, err := CheckAllowlist(cmd, Kickable{}); err == nil {
```

(The existing `"kick Steve"` refusal case at line 36 stays: with no kickable list it must still be refused.)

Append to `allowlist_test.go`:

```go

// testKickable is the actor list the kick tests share: a bare gamertag, one
// with spaces that must travel quoted, and a third so a prefix of one actor
// cannot pass as another.
func testKickable(t *testing.T) Kickable {
	t.Helper()
	k, err := ParseKickable("AfkBotOne,Afk Bot Two,ServerAgent")
	if err != nil {
		t.Fatalf("ParseKickable: %v", err)
	}
	return k
}

func TestCheckAllowlist_KickAllowedForActors(t *testing.T) {
	k := testKickable(t)
	for _, cmd := range []string{
		"kick AfkBotOne",
		"kick afkbotone",
		"kick AFKBOTONE",
		`kick "AfkBotOne"`,
		`kick "Afk Bot Two"`,
		`kick "afk bot two"`,
		"kick ServerAgent",
	} {
		rule, err := CheckAllowlist(cmd, k)
		if err != nil {
			t.Errorf("CheckAllowlist(%q) unexpected error: %v", cmd, err)
			continue
		}
		if rule != "kick" {
			t.Errorf("CheckAllowlist(%q) rule = %q, want kick", cmd, rule)
		}
	}
}

func TestCheckAllowlist_KickRefused(t *testing.T) {
	k := testKickable(t)
	cases := []struct{ cmd, why string }{
		{"kick Steve", "a real player"},
		{`kick "Steve"`, "a real player, quoted"},
		{"kick AfkBot", "a prefix of an actor is not that actor"},
		{"kick AfkBotOne2", "an actor's name plus a suffix is someone else"},
		{"kick AfKBotOne", "the Kelvin sign only Unicode-folds to k"},
		{"kick Afk Bot Two", "unquoted, this is the name Afk plus a reason"},
		{`kick "Afk Bot Two`, "unterminated quote"},
		{`kick " Afk Bot Two"`, "padding inside the quotes changes the name"},
		{`kick "Afk  Bot Two"`, "a doubled inner space changes the name"},
		{"kick AfkBotOne reason text", "a reason argument"},
		{`kick "AfkBotOne" reason`, "a reason after a quoted name"},
		{`kick "AfkBotOne""Steve"`, "a second quoted name"},
		{`kick "AfkBotOne\" Steve"`, "an escaped quote inside the name"},
		{"kick AfkBotOne;stop", "a separator is part of an unknown name"},
		{"kick AfkBotOne ", "trailing space"},
		{"kick  AfkBotOne", "doubled separator"},
		{"kick\tAfkBotOne", "tab separator"},
		{"kick AfkBotOne\nstop", "embedded newline"},
		{"kick AfkBotOne\rstop", "embedded carriage return"},
		{"kick @a", "selector: everyone"},
		{"kick @s", "selector: self"},
		{"kick @r", "selector: random player"},
		{"kick @e[type=player]", "selector with arguments"},
		{"kick @p[name=AfkBotOne]", "a selector naming an actor is still a selector"},
		{`kick "@a"`, "quoted selector"},
		{"kick", "no target"},
		{"kick ", "empty target"},
		{`kick ""`, "empty quoted target"},
		{"Kick AfkBotOne", "templates are case-sensitive"},
	}
	for _, c := range cases {
		if _, err := CheckAllowlist(c.cmd, k); err == nil {
			t.Errorf("CheckAllowlist(%q) = nil error, want refusal (%s)", c.cmd, c.why)
		}
	}
}

func TestCheckAllowlist_KickRefusedWhenNothingIsKickable(t *testing.T) {
	empty, err := ParseKickable("")
	if err != nil {
		t.Fatalf("ParseKickable: %v", err)
	}
	for _, k := range []Kickable{{}, empty} {
		for _, cmd := range []string{"kick AfkBotOne", `kick "Afk Bot Two"`, "kick @a"} {
			if _, err := CheckAllowlist(cmd, k); err == nil {
				t.Errorf("CheckAllowlist(%q) with an empty kickable list = nil error, want refusal", cmd)
			}
		}
	}
}
```

In `http_test.go`, replace the import block (lines 3-11) with:

```go
import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
)
```

Append to `http_test.go`:

```go

func postCommand(t *testing.T, mux http.Handler, cmd string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(commandRequest{Command: cmd})
	if err != nil {
		t.Fatalf("marshal command: %v", err)
	}
	req := httptest.NewRequest("POST", "/command", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// kickServer wires the HTTP API to a fake console that records every stdin
// line it receives, so a test can assert what did and did not reach the
// server.
func kickServer(t *testing.T, kickable string) (http.Handler, <-chan string) {
	t.Helper()
	received := make(chan string, 16)
	addr, _ := startFakeConsole(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
		writeTestMsg(t, ctx, conn, wsMessage{Type: "logHistory"})
		for {
			msg, err := readTestMsg(ctx, conn)
			if err != nil {
				return
			}
			if msg.Type == "stdin" {
				received <- msg.Data
			}
		}
	})

	k, err := ParseKickable(kickable)
	if err != nil {
		t.Fatalf("ParseKickable: %v", err)
	}
	c := NewConsole(addr, "pw", testOrigin, 2*time.Second, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	waitConnected(t, c)

	return newMux(&server{
		cfg:     Config{BridgeToken: "tok", Kickable: k},
		console: c,
		logger:  testLogger(),
	}), received
}

func TestCommand_KickReachesConsoleOnlyForActors(t *testing.T) {
	mux, received := kickServer(t, "AfkBotOne,Afk Bot Two")

	rec := postCommand(t, mux, `kick "Afk Bot Two"`)
	if rec.Code != http.StatusOK {
		t.Fatalf("kick of an actor = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	var resp commandResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Rule != "kick" {
		t.Errorf("rule = %q, want kick", resp.Rule)
	}
	select {
	case got := <-received:
		if want := "kick \"Afk Bot Two\"\n"; got != want {
			t.Errorf("console received %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an accepted kick never reached the console")
	}

	for _, cmd := range []string{"kick Steve", "kick @a", "kick AfkBotOne reason", "kick AfkBotOne\nstop"} {
		if rec := postCommand(t, mux, cmd); rec.Code != http.StatusForbidden {
			t.Errorf("POST /command %q = %d, want 403", cmd, rec.Code)
		}
	}
	// Refusal happens before the console is touched, so anything received
	// now came from a refused command.
	select {
	case got := <-received:
		t.Errorf("a refused kick reached the console as %q", got)
	default:
	}
}

// A bridge deployed without BRIDGE_KICKABLE must refuse every kick, including
// one naming a real actor's gamertag.
func TestCommand_KickRefusedWithoutKickableList(t *testing.T) {
	mux := testServer(t, testConsole())

	if rec := postCommand(t, mux, "kick AfkBotOne"); rec.Code != http.StatusForbidden {
		t.Errorf("kick with no kickable list = %d, want 403", rec.Code)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -race ./...`
Expected: build failure starting `./allowlist_test.go:21:38: too many arguments in call to CheckAllowlist` (the compiler stops after ten errors, so the missing `Config.Kickable` field used by `http_test.go` may not be listed).

- [ ] **Step 3: Implement the rule**

In `allowlist.go`, change the `validate` field comment and type (lines 24-26) to:

```go
	// validate runs after a regex match for templates that need more than
	// syntax checking (e.g. tellraw's JSON payload, kick's actor list).
	validate func(matches []string, kickable Kickable) error
```

Change the tellraw rule's `validate: func(m []string) error {` (line 46) to:

```go
		validate: func(m []string, _ Kickable) error {
```

Append this rule as the last element of `rules`, after the `gamerule_query` entry (before the closing `}` on line 71):

```go
	{
		name: "kick",
		// Exactly one player and no reason. A gamertag with spaces must be
		// quoted, as Bedrock requires; a bare name cannot start with @, so
		// no selector matches.
		re: regexp.MustCompile(`^kick (?:"([^"\\]+)"|([^\s"\\@][^\s"\\]*))$`),
		validate: func(m []string, kickable Kickable) error {
			name := m[1] + m[2]
			if !kickable.Contains(name) {
				return fmt.Errorf("%q is not a kickable actor", name)
			}
			return nil
		},
	},
```

(Exactly one of the two capture groups is non-empty, so `m[1] + m[2]` is the name without its quotes. A quoted `"@a"` matches the regex but no `Kickable` entry can start with `@`, so `Contains` refuses it.)

Replace the `CheckAllowlist` doc comment and signature (lines 73-76) with:

```go
// CheckAllowlist reports whether cmd matches an allowlisted template. It
// returns the matched rule name on success, or ErrCommandNotAllowed (wrapped
// with a reason) on refusal. kick is accepted only for a gamertag in
// kickable.
func CheckAllowlist(cmd string, kickable Kickable) (string, error) {
```

and the call on line 90 `if err := r.validate(m); err != nil {` with:

```go
			if err := r.validate(m, kickable); err != nil {
```

In `config.go`, append to the `Config` struct after `DataDir string` (line 45):

```go

	// Kickable is the set of gamertags POST /command may kick.
	Kickable Kickable
```

In `http.go`, change line 125 `rule, err := CheckAllowlist(req.Command)` to:

```go
	rule, err := CheckAllowlist(req.Command, s.cfg.Kickable)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race -run 'CheckAllowlist|Command_' -v ./... 2>&1 | grep -E '^(--- |ok|FAIL)'`
Expected:
```
--- PASS: TestCheckAllowlist_Allowed (0.00s)
--- PASS: TestCheckAllowlist_Refused (0.00s)
--- PASS: TestCheckAllowlist_KickAllowedForActors (0.00s)
--- PASS: TestCheckAllowlist_KickRefused (0.00s)
--- PASS: TestCheckAllowlist_KickRefusedWhenNothingIsKickable (0.00s)
--- PASS: TestCommand_KickReachesConsoleOnlyForActors (...)
--- PASS: TestCommand_KickRefusedWithoutKickableList (0.00s)
ok  	github.com/jdwillmsen/mc-console-bridge	...
```
Then run the whole suite: `go test -race ./...` → `ok  	github.com/jdwillmsen/mc-console-bridge`. And `gofmt -l . && go vet ./...` prints nothing.

- [ ] **Step 5: Commit**

```bash
git add allowlist.go allowlist_test.go config.go http.go http_test.go
git commit -m "feat(allowlist): accept kick for gamertags in the kickable set

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: Load `BRIDGE_KICKABLE`, log it, document it

**Files:**
- Modify: `config.go:110-113` (parse after `CONSOLE_ORIGIN`), `config.go:115-123` (return literal)
- Modify: `main.go:52`
- Modify: `README.md:44-60` (Allowlist section), `README.md:80-87` (env var table)
- Test: `config_test.go` (append)

**Interfaces:**
- Consumes: `ParseKickable`, `Kickable.Len`, `Kickable.Contains` (Task 1); `Config.Kickable` (Task 2); `setRequiredEnv(t)` (`config_test.go:8-12`).
- Produces: `LoadConfig` fills `Config.Kickable` from `BRIDGE_KICKABLE`, and returns an error prefixed `BRIDGE_KICKABLE: ` for an unsafe entry. The chart (plan 6) renders the actors' gamertags into this variable.

- [ ] **Step 1: Write the failing test**

Append to `config_test.go`:

```go

func TestLoadConfig_KickableDefaultsToNobody(t *testing.T) {
	setRequiredEnv(t)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Kickable.Len() != 0 {
		t.Errorf("Kickable.Len = %d, want 0 when BRIDGE_KICKABLE is unset", cfg.Kickable.Len())
	}
}

func TestLoadConfig_KickableFromEnv(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("BRIDGE_KICKABLE", "AfkBotOne,Afk Bot Two")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	for _, name := range []string{"AfkBotOne", "Afk Bot Two"} {
		if !cfg.Kickable.Contains(name) {
			t.Errorf("Kickable.Contains(%q) = false, want true", name)
		}
	}
}

func TestLoadConfig_KickableUnsafeEntryIsAnError(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("BRIDGE_KICKABLE", "AfkBotOne,@a")

	if _, err := LoadConfig(); err == nil {
		t.Fatal("LoadConfig with a selector in BRIDGE_KICKABLE returned no error — the operator would believe it was applied")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -race -run Kickable ./... 2>&1 | grep -E '^(--- |ok|FAIL)'`
Expected:
```
--- FAIL: TestLoadConfig_KickableFromEnv (0.00s)
--- FAIL: TestLoadConfig_KickableUnsafeEntryIsAnError (0.00s)
FAIL
FAIL	github.com/jdwillmsen/mc-console-bridge	...
```
(`TestLoadConfig_KickableDefaultsToNobody` already passes, because the zero `Kickable` is empty. That is the behaviour it pins.)

- [ ] **Step 3: Implement loading**

In `config.go`, after the `consoleOrigin` block (lines 110-113), add:

```go
	kickable, err := ParseKickable(os.Getenv("BRIDGE_KICKABLE"))
	if err != nil {
		return Config{}, fmt.Errorf("BRIDGE_KICKABLE: %w", err)
	}
```

and add the field to the returned literal after `DataDir:` (line 122):

```go
		Kickable:        kickable,
```

In `main.go`, replace the startup log on line 52 with:

```go
	logger.Info("starting", "http_addr", cfg.HTTPAddr, "console_addr", cfg.ConsoleAddr, "console_origin", cfg.ConsoleOrigin, "kickable_actors", cfg.Kickable.Len())
```

In `README.md`, add this row to the Allowlist table after the `gamerule <name>` row (line 57):

```markdown
| `kick <gamertag>` or `kick "<gamertag>"`, only for a gamertag in `BRIDGE_KICKABLE` | `kick "Afk Bot Two"` |
```

and after the paragraph ending "one HTTP request cannot smuggle a second console line." (line 60) add:

```markdown
`kick` exists so the agent can force one of its own actors (itself or an AFK
bot) off the server; it is never a moderation tool. It takes exactly one
target and no reason. A gamertag with spaces has to be quoted, since unquoted
Bedrock would read everything after the first space as the reason. The name
must equal an entry in `BRIDGE_KICKABLE` apart from ASCII letter case.
Selectors are always refused, and with `BRIDGE_KICKABLE` empty every kick is,
so a leaked bridge token cannot remove a real player.
```

Add this row to the Environment variables table after `DATA_DIR` (line 87):

```markdown
| `BRIDGE_KICKABLE` | no | empty | Comma-separated gamertags `kick` may name (the server's own actors). Empty refuses every kick. An entry containing `"`, `\` or a control character, or starting with `@`, fails startup |
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./...`
Expected: `ok  	github.com/jdwillmsen/mc-console-bridge`.
Run: `gofmt -l . && go vet ./... && CGO_ENABLED=0 go build ./...`
Expected: no output.

- [ ] **Step 5: Commit**

```bash
git add config.go config_test.go main.go README.md
git commit -m "feat(config): read the kickable actor list from BRIDGE_KICKABLE

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: Verify, ship and confirm the release

The release path already exists and was confirmed while writing this plan: `semantic-release.yml` runs on every green CI `push` to `main`, tags `v<version>` from the Conventional Commits since the last tag (`.releaserc.json`: `feat` → minor), then calls `release.yml` via `workflow_call`, which re-runs CI on the tag, publishes `ghcr.io/jdwillmsen/mc-console-bridge:<version>` and `:sha-<commit>` (linux/amd64, OCI labels and annotations, provenance `mode=max`, SBOM, never `latest`), then the `mirror-dockerhub` job copies both tags to `docker.io/jdwillmsen/mc-console-bridge` with `imagetools create` (same digest) and pushes `README.docker.md` as the Overview. The `DOCKERHUB_USERNAME` variable is set (`jdwillmsen`), and the `v0.2.1` Release run finished `mirror-dockerhub: success`. No workflow changes are needed. This will be the first version semantic-release itself publishes (its only earlier run released nothing and skipped `publish`), so check the chain end to end below.

**Files:** none changed.

**Interfaces:**
- Consumes: Tasks 1-3 committed on the branch.
- Produces: image `ghcr.io/jdwillmsen/mc-console-bridge:0.3.0` (and the identical `docker.io/jdwillmsen/mc-console-bridge:0.3.0`) that plan 6 pins in the `minecraft-fwb` chart.

- [ ] **Step 1: Full local verification**

Run:
```bash
gofmt -l .
go vet ./...
CGO_ENABLED=0 go build ./...
go test -race -count=1 ./...
golangci-lint run --new-from-rev=origin/main
golangci-lint run
docker build -t mc-console-bridge:kick-test .
```
Expected: `gofmt`, `vet` and `build` print nothing; `go test` prints `ok  	github.com/jdwillmsen/mc-console-bridge`; `golangci-lint run --new-from-rev=origin/main` prints `0 issues.`; plain `golangci-lint run` prints exactly the 7 `errcheck` findings that already exist on `origin/main` (`console.go:197`, `console_test.go:141`, `console_test.go:320`, `console_test.go:412`, `http.go:151`, `http.go:168`, `main.go:49`) and nothing in `kickable.go`, `allowlist.go`, `config.go` or the new tests; `docker build` succeeds.

- [ ] **Step 2: Check the commit types that drive the release**

Run: `git log --format='%s' origin/main..HEAD`
Expected (three lines, all `feat`, so semantic-release cuts a minor, `v0.3.0`):
```
feat(config): read the kickable actor list from BRIDGE_KICKABLE
feat(allowlist): accept kick for gamertags in the kickable set
feat(kick): parse the set of gamertags the bridge may kick
```
Run: `git log --format='%G? %s' origin/main..HEAD` — every line starts with `G` (signed).

- [ ] **Step 3: Push and open the PR**

Use the `/no-mistakes` pipeline if available; otherwise:
```bash
git push -u origin HEAD
gh pr create --title "feat: allow kick for the server's own actors only" --body "$(cat <<'BODY'
Adds `kick <gamertag>` to the POST /command allowlist, accepted only for gamertags listed in the new `BRIDGE_KICKABLE` variable (comma-separated; empty refuses every kick). The agent's presence loop uses it to close a parked actor's lingering session; a leaked bridge token still cannot kick a real player.

- One target, no reason; names with spaces must be quoted.
- Selectors refused, quoted or not.
- Match ignores ASCII case only, so a Unicode lookalike cannot pass for an actor.
- An unsafe entry in BRIDGE_KICKABLE fails startup instead of being dropped.

Verification: go test -race ./..., go vet, gofmt, golangci-lint --new-from-rev=origin/main (0 issues), docker build.

🤖 Generated with [Claude Code](https://claude.com/claude-code)
BODY
)"
```
Expected: a PR URL. Then `gh pr checks --watch` ends with every check passing (CI `test` and `docker`, CodeQL, Security Scan, Verify PR Signatures).

- [ ] **Step 4: Merge only after review**

Per the repo owner's rules: read the diff line by line, resolve every review thread and code-scanning alert (`gh api repos/jdwillmsen/mc-console-bridge/code-scanning/alerts?state=open&ref=refs/pull/<n>/merge` returns `[]`), then rebase-merge (the only method this repo allows):
```bash
gh pr merge --rebase --delete-branch
```

- [ ] **Step 5: Confirm the release chain**

Run after the merge:
```bash
gh run list --workflow=ci.yml --branch=main -L 1
gh run list --workflow=semantic-release.yml -L 1
gh run view <semantic-release run id> --json jobs -q '[.jobs[]|.name+":"+.conclusion]|join(", ")'
gh release view v0.3.0 --json tagName,name -q .tagName
```
Expected: CI on `main` succeeds; every job in the semantic-release run concludes `success`: `release`, then the called `release.yml` jobs (`version`, `ci / test`, `ci / docker`, `publish`, `mirror-dockerhub`), each shown with a `publish / ` prefix. None is `skipped`. `gh release view` prints `v0.3.0`.

Then:
```bash
docker buildx imagetools inspect ghcr.io/jdwillmsen/mc-console-bridge:0.3.0 | grep '^Digest'
docker buildx imagetools inspect docker.io/jdwillmsen/mc-console-bridge:0.3.0 | grep '^Digest'
```
Expected: two identical `Digest: sha256:...` lines. If `publish` was skipped, semantic-release found no releasable commit: re-check Step 2 on `main` (`git log --format=%s v0.2.1..origin/main`). If `mirror-dockerhub` failed while `publish` succeeded, GHCR is still correct (deployments pull from GHCR); re-run only that job with `gh run rerun <id> --failed` and report the Docker Hub error, since its token is set by a human.

---

## Self-review

- Spec coverage: `POST /command` gains `kick <gamertag>` (Task 2); accepted only for `BRIDGE_KICKABLE` gamertags (Tasks 1-3); a leaked token cannot kick a real player (Task 2 HTTP test: refused commands never reach the console); "Bridge tests: kick accepted for an actor, rejected for anyone else" (Task 2); rollout step 4 is its own PR and safe alone, because an unset variable refuses every kick, which is today's behaviour (Tasks 3-4).
- Placeholder scan: none; every code step has complete code, every run step an expected result. `<n>` and `<semantic-release run id>` in Task 4 are values printed by the previous command.
- Type consistency: `Kickable`, `ParseKickable(string) (Kickable, error)`, `Len() int`, `Contains(string) bool`, `CheckAllowlist(string, Kickable) (string, error)`, `Config.Kickable` are used with these exact signatures in every task. All test code was compiled and run against a copy of `origin/main` while writing this plan.
- Review Focus: each of the five lines has a named test in its owning task.

## Deviations from spec/contract

- **Case-insensitive means ASCII case only.** The brief asks for an exact case-insensitive match. Full Unicode folding (`strings.EqualFold`) would accept names the operator never listed, such as the Kelvin sign for `k`, so only ASCII letters are folded. Xbox gamertags an operator would list for the bots are unaffected.
- **`BRIDGE_KICKABLE` gains a validity rule the contract does not state.** An entry containing `"`, `\` or a control character, or starting with `@`, fails startup, because it could not be passed to the console as a single quoted name. Plan 6's chart must render plain gamertags, which real gamertags always are. Empty or unset still means "refuse every kick", as the contract says.
- **`kick` accepts no reason argument.** The spec says only `kick <gamertag>`; Bedrock allows an optional reason. Refusing it keeps the template to one token. Plan 3's loop must send `kick <name>` or `kick "<name>"` with nothing after it. Always quoting is safe.
- **`golangci-lint run` is not clean on `origin/main`.** The repo has no golangci config and CI does not run the linter, and the default linters already report 7 `errcheck` findings in code this plan does not touch. The final verification uses `golangci-lint run --new-from-rev=origin/main` (must be `0 issues.`) and checks that the full run reports only those 7. Fixing them is out of scope.
- **No logging.Fields.** The bridge logs with `log/slog` key/value pairs, so the new startup field follows that (`"kickable_actors", n`). No kick metric is added to the bridge: refusals already count in `mc_console_bridge_commands_refused_total`, and the contract's `mc_presence_kicks_total` belongs to the agent.
