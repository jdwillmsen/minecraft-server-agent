# AFK Bot Desired-State Reconciler Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let `minecraft-afk-bot` park (disconnect and stay out) and resume on the agent's say-so, by polling `GET /v1/actors/{id}/presence` and reporting `POST /v1/actors/{id}/status`, while an unset `PRESENCE_URL` leaves the bot behaving exactly as it does today.

**Architecture:** A new `internal/presence` package holds an HTTP client for the two routes and a `Reconciler` that keeps the last desired state it was told (or `PRESENCE_DEFAULT` until the first answer) and exposes it as a `Gate`. `runConnectLoop` in `cmd/bot/main.go` asks the gate for admission before every connect; admission blocks while parked and hands back a session context the gate cancels on park. With the feature off the gate is `presence.AlwaysPresent`, which admits at once and never cancels. Work happens on a feature branch in a fresh worktree off `origin/main` of `github.com/jdwillmsen/minecraft-afk-bot` (not created by this plan).

**Tech Stack:** Go 1.27, standard library (`net/http`, `encoding/json`, `net/http/httptest`), plus one new module: `github.com/jdwillmsen/minecraft-server-agent/presenceapi` `v0.1.0` (standard library only).

**Spec:** `docs/superpowers/specs/2026-09-23-actor-presence-design.md` in `minecraft-server-agent` (sections "AFK bot: desired-state reconciler" and "Shared contract"; rollout step 5), plus the shared contract `docs/superpowers/plans/2026-09-23-actor-presence-00-contract.md` beside this plan.

**HARD PREREQUISITE:** plan 3 must have merged and tagged `presenceapi/v0.1.0` in `minecraft-server-agent`, including `presenceapi/testdata/*.json`. Check before starting:

```bash
GOFLAGS=-mod=mod go list -m -versions github.com/jdwillmsen/minecraft-server-agent/presenceapi
```

Expected: a line ending in `v0.1.0`. If it prints `no matching versions` or a 404, stop: this plan cannot start.

## Global Constraints

- Go 1.27 as pinned in `go.mod`. The only new requirement is `github.com/jdwillmsen/minecraft-server-agent/presenceapi v0.1.0`; `go get` must change **no other** version in `go.mod` (that is the reason the contract is its own module).
- Never rename anything from the contract: routes `GET /v1/actors/{id}/presence`, `POST /v1/actors/{id}/status`; header `Authorization: Bearer <token>`; `ETag` / `If-None-Match` with 304; states `present` / `parked` only; env vars `PRESENCE_URL`, `PRESENCE_TOKEN`, `PRESENCE_ACTOR_ID`, `PRESENCE_DEFAULT`, `PRESENCE_POLL_MS` (default `10000`); actor id regex `^[a-z0-9][a-z0-9-]{0,62}$`.
- `PRESENCE_URL` unset = feature off = exactly today's behaviour. No other `PRESENCE_*` variable is read or validated when it is unset, and no HTTP call is made.
- Agent unreachable, or answering with an error: keep acting on the last answer. Never answered: act on `PRESENCE_DEFAULT`. An agent outage must never make the bot connect or disconnect.
- 401/403/404 from the agent are logged, never fatal; the process keeps running and keeps polling.
- The bearer token never appears in a log line or an error message.
- Repeated identical failures log once per streak, not once per poll (a 10 s poll against a dead agent would otherwise write 360 lines an hour).
- The shared packages copied from the agent (`internal/liveness`, `internal/logging`, `internal/mcproto`, `internal/skin`, `internal/mcauth`) are **not** modified.
- Comments explain why, never what, at the density of the surrounding file. No ticket IDs in code, comments, docs or commit messages.
- `gofmt -l .` empty, `go vet ./...`, `go test -race ./...` and `golangci-lint run` clean at the end.
- Conventional commits; every commit message ends with:
  ```
  Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
  ```

## Review Focus

- **Agent hangs instead of refusing.** A GET that never returns must not stall parking or resuming beyond one poll: the client has a 5 s timeout and a timed-out fetch keeps the last answer. Test: `TestHungAgentKeepsLastAnswer` (Task 3).
- **Agent answers 200 with a state this bot does not know** (a future third state, an empty body). The bot must keep its last answer and log an error, not treat it as parked or present. Test: `TestInvalidStateKeepsLastAnswer` (Task 3).
- **SIGTERM while parked.** A parked bot blocked in admission must still exit promptly on pod termination. Test: `TestConnectLoopExitsWhileParked` (Task 4).
- **Park arriving mid-dial or during the reconnect wait.** The bot must not complete a connection it was told to drop, and a resume must reconnect immediately rather than after an accumulated backoff. Tests: `TestConnectLoopParkEndsSessionAndResumesWithoutBackoff` (Task 4), `TestParkCancelsAdmittedSession` (Task 3).
- **Token leakage.** The token is a secret mounted into the pod; neither config errors nor the `presence_enabled` line may carry it. Tests: `TestLoadPresenceErrorsNeverEchoTheToken` (Task 1), `TestPresenceGateLogsNoToken` (Task 4).

---

## File Structure

- `internal/config/config.go` — gains `Presence` (the five `PRESENCE_*` settings) and `loadPresence`, beside the existing `Load`, `required`, `positiveInt`. Settings live together; this file stays the one place env vars are read.
- `internal/config/config_test.go` — presence cases.
- `internal/presence/client.go` — **new.** HTTP client for the two routes: `Client`, `Fetched`, `HTTPError`, `ErrInvalidResponse`.
- `internal/presence/client_test.go` — **new.** Wire-level client tests against `httptest`.
- `internal/presence/contract_test.go` — **new.** Decodes the agent's golden JSON from the `presenceapi` module's `testdata/`.
- `internal/presence/gate.go` — **new.** `Gate` interface and `AlwaysPresent`, the feature-off gate.
- `internal/presence/reconciler.go` — **new.** `Reconciler`: poll loop, fail-static state, status reports, admission, streak logging.
- `internal/presence/reconciler_test.go` — **new.** Behaviour tests against a fake agent: park, resume, unreachable, never-answered, 304, 401/403, invalid state, hung agent, status report.
- `cmd/bot/main.go` — `runConnectLoop` (lines 60-99) takes a `presence.Gate` and a `connectFunc`; `session` (lines 101-199) closes the connection when its context ends and reports spawn; `main` (lines 37-58) builds the gate; new `version` variable stamped at build.
- `cmd/bot/loop_test.go` — **new.** Connect-loop tests with fake gates and fake sessions.
- `Dockerfile` — comment at lines 5-7 corrected (the build now fetches one module from the agent repo); `ARG VERSION` stamped into `main.version`.
- `.github/workflows/release.yml` — passes `VERSION` as a build arg (build step lines 111-122).
- `docs/decisions.md`, `docs/operations.md`, `README.md` — the import decision, the runbook, config and event tables.

---

### Task 1: `PRESENCE_*` configuration and the `presenceapi` dependency

**Files:**
- Modify: `go.mod`, `go.sum` (via `go get`)
- Modify: `internal/config/config.go` (imports lines 8-13, constants lines 23-37, `Config` lines 40-68, `Load` lines 73-111; append `loadPresence` after `positiveInt`)
- Test: `internal/config/config_test.go` (append)

**Interfaces:**
- Consumes: `required(name string) (string, error)` and `positiveInt(name string, def, max int) (int, error)` from `internal/config/config.go`; `presenceapi.State`, `presenceapi.StatePresent`, `presenceapi.StateParked` from the contract module.
- Produces: `type config.Presence struct { URL, Token, ActorID string; Default presenceapi.State; PollMs int }`, `func (p config.Presence) Enabled() bool`, and the field `Config.Presence Presence`.

- [ ] **Step 1: Add the contract module**

```bash
go get github.com/jdwillmsen/minecraft-server-agent/presenceapi@v0.1.0
git diff go.mod
```

Expected: the diff adds exactly one line, `github.com/jdwillmsen/minecraft-server-agent/presenceapi v0.1.0` (marked `// indirect` until Step 5 imports it), and changes no other version. If any other line changes, stop: the module has grown a dependency and the decision recorded in Task 5 no longer holds.

- [ ] **Step 2: Write the failing tests**

Append to `internal/config/config_test.go`, and add `"github.com/jdwillmsen/minecraft-server-agent/presenceapi"` to its imports:

```go
// setPresence supplies a complete, valid presence configuration so each test
// can break exactly one variable.
func setPresence(t *testing.T) {
	t.Helper()
	t.Setenv("PRESENCE_URL", "http://fwb-server-agent:8080/")
	t.Setenv("PRESENCE_TOKEN", "ffffffffffffffff")
	t.Setenv("PRESENCE_ACTOR_ID", "afk-bot-1")
	t.Setenv("PRESENCE_DEFAULT", "present")
}

// Feature off must mean today's bot. A deployment without the agent may still
// carry a stray PRESENCE_* variable, and that must not stop the bot starting.
func TestLoadPresenceOffWhenURLUnset(t *testing.T) {
	setRequired(t)
	t.Setenv("PRESENCE_DEFAULT", "bogus")
	t.Setenv("PRESENCE_POLL_MS", "not-a-number")
	t.Setenv("PRESENCE_ACTOR_ID", "Not Valid")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load with PRESENCE_URL unset refused other PRESENCE_* variables: %v", err)
	}
	if cfg.Presence.Enabled() {
		t.Error("presence enabled with PRESENCE_URL unset")
	}
	if cfg.Presence != (Presence{}) {
		t.Errorf("Presence = %+v, want the zero value", cfg.Presence)
	}
}

func TestLoadPresenceEnabled(t *testing.T) {
	setRequired(t)
	setPresence(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Presence{
		URL:     "http://fwb-server-agent:8080",
		Token:   "ffffffffffffffff",
		ActorID: "afk-bot-1",
		Default: presenceapi.StatePresent,
		PollMs:  10000,
	}
	if cfg.Presence != want {
		t.Errorf("Presence = %+v, want %+v", cfg.Presence, want)
	}
	if !cfg.Presence.Enabled() {
		t.Error("presence not enabled with PRESENCE_URL set")
	}
}

// With the feature on, a missing or malformed setting would leave the bot
// guessing whether it may connect, so each one fails startup instead.
func TestLoadPresenceRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		env   string
		value string
	}{
		{"token missing", "PRESENCE_TOKEN", ""},
		{"actor id missing", "PRESENCE_ACTOR_ID", ""},
		{"actor id not a slug", "PRESENCE_ACTOR_ID", "AFK_Bot"},
		{"actor id too long", "PRESENCE_ACTOR_ID", "a" + strings.Repeat("b", 63)},
		{"default missing", "PRESENCE_DEFAULT", ""},
		{"default not a state", "PRESENCE_DEFAULT", "absent"},
		{"url not http", "PRESENCE_URL", "ftp://fwb-server-agent"},
		{"url without host", "PRESENCE_URL", "http://"},
		{"poll zero", "PRESENCE_POLL_MS", "0"},
		{"poll above the ceiling", "PRESENCE_POLL_MS", "600001"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setRequired(t)
			setPresence(t)
			t.Setenv(tc.env, tc.value)

			_, err := Load()
			if err == nil {
				t.Fatalf("%s=%q was accepted", tc.env, tc.value)
			}
			if !strings.Contains(err.Error(), tc.env) {
				t.Errorf("error should name %s, got %q", tc.env, err)
			}
		})
	}
}

func TestLoadPresencePollAtTheCeiling(t *testing.T) {
	setRequired(t)
	setPresence(t)
	t.Setenv("PRESENCE_POLL_MS", "600000")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("PRESENCE_POLL_MS=600000 was refused: %v", err)
	}
	if cfg.Presence.PollMs != 600000 {
		t.Errorf("PollMs = %d, want 600000", cfg.Presence.PollMs)
	}
}

// Startup errors land in the pod log; the token must not.
func TestLoadPresenceErrorsNeverEchoTheToken(t *testing.T) {
	for _, env := range []string{"PRESENCE_ACTOR_ID", "PRESENCE_DEFAULT"} {
		t.Run(env, func(t *testing.T) {
			setRequired(t)
			setPresence(t)
			t.Setenv(env, "ffffffffffffffff")

			_, err := Load()
			if err == nil {
				t.Fatalf("%s set to the token was accepted", env)
			}
			if strings.Contains(err.Error(), "ffffffffffffffff") {
				t.Errorf("error echoes a value that was set to the token: %q", err)
			}
		})
	}
}
```

(These are the two variables most likely to be swapped with the token in a chart edit. `PRESENCE_URL` errors do quote the URL, which is not a secret.)

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/config/ -run 'TestLoadPresence' -v`
Expected: FAIL to compile with `cfg.Presence undefined (type Config has no field or method Presence)` and `undefined: Presence`.

- [ ] **Step 4: Implement**

In `internal/config/config.go`, extend the imports (lines 8-13):

```go
import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)
```

Add to the ceiling constants block (after `maxBackoffMs`, line 36):

```go

	// Ten minutes. The poll interval is how long a park or resume takes to
	// reach the bot, and parking is meant to work mid-game.
	maxPollMs = 600_000
```

Below the constants block add:

```go
// The shared contract's actor id shape, checked here so a typo fails at
// startup rather than as a 404 on every poll.
var actorIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
```

Add a field at the end of `Config` (after `ReconnectMaxMs`, line 67):

```go

	// Presence is the zero value, and the reconciler is off, unless
	// PRESENCE_URL is set.
	Presence Presence
```

After the `Config` type add:

```go
// Presence configures the desired-state reconciler that lets the agent park
// this bot.
type Presence struct {
	URL     string
	Token   string
	ActorID string
	// Default is acted on until the agent first answers, so a bot that
	// starts during an agent outage still does what git says.
	Default presenceapi.State
	PollMs  int
}

// Enabled reports whether the bot should ask the agent before connecting.
func (p Presence) Enabled() bool { return p.URL != "" }
```

In `Load`, after the `reconnectMax < reconnectMin` check (line 100), add:

```go
	presence, err := loadPresence()
	if err != nil {
		return Config{}, err
	}
```

and add `Presence: presence,` as the last field of the returned `Config` literal (lines 102-110).

Append after `positiveInt`:

```go
// loadPresence reads nothing beyond PRESENCE_URL while it is unset, so a
// stray variable on a bot deployed without the agent cannot stop it starting.
//
// No error here quotes PRESENCE_TOKEN: startup errors go to the pod log.
func loadPresence() (Presence, error) {
	raw := strings.TrimSpace(os.Getenv("PRESENCE_URL"))
	if raw == "" {
		return Presence{}, nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Presence{}, fmt.Errorf("environment variable PRESENCE_URL must be an http or https URL, got %q", raw)
	}
	token, err := required("PRESENCE_TOKEN")
	if err != nil {
		return Presence{}, err
	}
	actorID, err := required("PRESENCE_ACTOR_ID")
	if err != nil {
		return Presence{}, err
	}
	if !actorIDPattern.MatchString(actorID) {
		return Presence{}, fmt.Errorf("environment variable PRESENCE_ACTOR_ID must match %s", actorIDPattern)
	}
	def, err := required("PRESENCE_DEFAULT")
	if err != nil {
		return Presence{}, err
	}
	state := presenceapi.State(def)
	if state != presenceapi.StatePresent && state != presenceapi.StateParked {
		return Presence{}, fmt.Errorf("environment variable PRESENCE_DEFAULT must be %q or %q", presenceapi.StatePresent, presenceapi.StateParked)
	}
	pollMs, err := positiveInt("PRESENCE_POLL_MS", 10000, maxPollMs)
	if err != nil {
		return Presence{}, err
	}
	return Presence{
		URL:     strings.TrimRight(raw, "/"),
		Token:   token,
		ActorID: actorID,
		Default: state,
		PollMs:  pollMs,
	}, nil
}
```

(The actor id and default errors deliberately do not quote the offending value: it is the only way to make "never echo" hold without special-casing which variable a mistake came from.)

- [ ] **Step 5: Run the tests to verify they pass, and tidy**

```bash
go mod tidy
go test ./internal/config/ -v
git diff go.mod
```

Expected: every test PASS, including the pre-existing `TestLoadBounds` and `TestLoadDefaults`; the `go.mod` diff is still the single added require, now without `// indirect`.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): read PRESENCE_* settings, off unless PRESENCE_URL is set

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 2: Presence API client and the golden-JSON contract test

**Files:**
- Create: `internal/presence/client.go`
- Test: `internal/presence/client_test.go`, `internal/presence/contract_test.go`

**Interfaces:**
- Consumes: `presenceapi.Presence`, `presenceapi.Status`, `presenceapi.Error`, `presenceapi.State*` from the contract module (Task 1 added it to `go.mod`).
- Produces:
  - `func NewClient(baseURL, actorID, token string, hc *http.Client) *Client`
  - `func (c *Client) Fetch(ctx context.Context, etag string) (Fetched, error)`
  - `func (c *Client) Report(ctx context.Context, st presenceapi.Status) error`
  - `type Fetched struct { Presence presenceapi.Presence; ETag string; NotModified bool }`
  - `type HTTPError struct { StatusCode int; Body presenceapi.Error }` implementing `error`
  - `var ErrInvalidResponse error`

- [ ] **Step 1: Write the failing client tests**

Create `internal/presence/client_test.go`:

```go
package presence

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

// seen is what a test handler observed. Handlers run on the server's
// goroutines, so they hand it over on a channel rather than through shared
// variables the race detector would flag.
type seen struct {
	method, path, auth, ifNoneMatch string
	hasIfNoneMatch                  bool
	body                            []byte
}

func record(r *http.Request, out chan<- seen) {
	body, _ := io.ReadAll(r.Body)
	_, has := r.Header["If-None-Match"]
	out <- seen{
		method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"),
		ifNoneMatch: r.Header.Get("If-None-Match"), hasIfNoneMatch: has, body: body,
	}
}

func TestFetchSendsTokenAndETag(t *testing.T) {
	reqs := make(chan seen, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record(r, reqs)
		w.Header().Set("ETag", `"3-parked"`)
		_ = json.NewEncoder(w).Encode(presenceapi.Presence{ActorID: "afk-bot-1", Effective: presenceapi.StateParked, Default: presenceapi.StatePresent})
	}))
	defer srv.Close()

	got, err := NewClient(srv.URL, "afk-bot-1", "tok", srv.Client()).Fetch(context.Background(), `"2-present"`)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	req := <-reqs
	if req.auth != "Bearer tok" {
		t.Errorf("Authorization = %q, want %q", req.auth, "Bearer tok")
	}
	if req.ifNoneMatch != `"2-present"` {
		t.Errorf("If-None-Match = %q, want the ETag passed in", req.ifNoneMatch)
	}
	if req.path != "/v1/actors/afk-bot-1/presence" {
		t.Errorf("path = %q", req.path)
	}
	if got.NotModified || got.Presence.Effective != presenceapi.StateParked || got.ETag != `"3-parked"` {
		t.Errorf("Fetched = %+v, want parked with the new ETag", got)
	}
}

// The first poll has nothing to compare against; an empty If-None-Match
// header would be a malformed conditional request.
func TestFetchOmitsIfNoneMatchWithoutAnETag(t *testing.T) {
	reqs := make(chan seen, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record(r, reqs)
		_ = json.NewEncoder(w).Encode(presenceapi.Presence{Effective: presenceapi.StatePresent})
	}))
	defer srv.Close()

	if _, err := NewClient(srv.URL, "afk-bot-1", "tok", srv.Client()).Fetch(context.Background(), ""); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if (<-reqs).hasIfNoneMatch {
		t.Error("If-None-Match sent on a request with no ETag")
	}
}

func TestFetchNotModified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	got, err := NewClient(srv.URL, "afk-bot-1", "tok", srv.Client()).Fetch(context.Background(), `"1-parked"`)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !got.NotModified || got.ETag != `"1-parked"` {
		t.Errorf("Fetched = %+v, want NotModified keeping the ETag", got)
	}
}

func TestFetchErrorCarriesStatusAndBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(presenceapi.Error{Code: "forbidden", Message: "token bound to afk-bot-2"})
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, "afk-bot-1", "tok", srv.Client()).Fetch(context.Background(), "")
	var he *HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("err = %v, want *HTTPError", err)
	}
	if he.StatusCode != http.StatusForbidden || he.Body.Code != "forbidden" {
		t.Errorf("HTTPError = %+v", he)
	}
}

// A state outside the contract must not be read as either state: the
// reconciler keeps its last answer instead.
func TestFetchRejectsUnknownState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"actor_id":"afk-bot-1","effective":"hibernating","default":"present"}`))
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, "afk-bot-1", "tok", srv.Client()).Fetch(context.Background(), "")
	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("err = %v, want ErrInvalidResponse", err)
	}
}

func TestReportPostsStatus(t *testing.T) {
	reqs := make(chan seen, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record(r, reqs)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	want := presenceapi.Status{Connected: true, ObservedState: presenceapi.StatePresent, LastSeen: time.Date(2026, 9, 23, 6, 0, 0, 0, time.UTC), ProcessVersion: "1.2.3"}
	if err := NewClient(srv.URL, "afk-bot-1", "tok", srv.Client()).Report(context.Background(), want); err != nil {
		t.Fatalf("Report: %v", err)
	}
	req := <-reqs
	if req.method != http.MethodPost || req.auth != "Bearer tok" || req.path != "/v1/actors/afk-bot-1/status" {
		t.Errorf("request = %s %s auth %q", req.method, req.path, req.auth)
	}
	var got presenceapi.Status
	if err := json.Unmarshal(req.body, &got); err != nil {
		t.Fatalf("decode posted body: %v", err)
	}
	if !got.LastSeen.Equal(want.LastSeen) || got.Connected != want.Connected || got.ObservedState != want.ObservedState || got.ProcessVersion != want.ProcessVersion {
		t.Errorf("posted %+v, want %+v", got, want)
	}
}

func TestReportErrorCarriesStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	err := NewClient(srv.URL, "afk-bot-1", "tok", srv.Client()).Report(context.Background(), presenceapi.Status{ObservedState: presenceapi.StateParked})
	var he *HTTPError
	if !errors.As(err, &he) || he.StatusCode != http.StatusUnauthorized {
		t.Fatalf("err = %v, want *HTTPError 401", err)
	}
}
```

- [ ] **Step 2: Write the failing contract test**

Create `internal/presence/contract_test.go`:

```go
package presence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

const contractModule = "github.com/jdwillmsen/minecraft-server-agent/presenceapi"

// goldenDir finds the agent's golden files inside the pinned module, so this
// test reads the very bytes the agent's own contract test reads at that
// version rather than a copy that could drift.
func goldenDir(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", contractModule).Output()
	if err != nil {
		t.Fatalf("go list %s: %v", contractModule, err)
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		t.Fatalf("%s is not downloaded; run go mod download", contractModule)
	}
	return filepath.Join(dir, "testdata")
}

func golden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(goldenDir(t), name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return b
}

// Strict decoding proves the pinned types and the pinned goldens agree field
// for field; the round trip proves nothing is lost on the way back out.
func TestGoldenFilesRoundTrip(t *testing.T) {
	for name, target := range map[string]any{
		"presence_parked.json":  &presenceapi.Presence{},
		"presence_default.json": &presenceapi.Presence{},
		"status.json":           &presenceapi.Status{},
		"error_conflict.json":   &presenceapi.Error{},
	} {
		t.Run(name, func(t *testing.T) {
			raw := golden(t, name)
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(target); err != nil {
				t.Fatalf("decode: %v", err)
			}
			again, err := json.Marshal(target)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			var want, got any
			_ = json.Unmarshal(raw, &want)
			_ = json.Unmarshal(again, &got)
			if !reflect.DeepEqual(want, got) {
				t.Errorf("round trip changed the document:\nwant %s\ngot  %s", raw, again)
			}
		})
	}
}

func serveGolden(t *testing.T, status int, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"1-golden"`)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestGoldenParkedDecodesThroughTheClient(t *testing.T) {
	srv := serveGolden(t, http.StatusOK, golden(t, "presence_parked.json"))

	got, err := NewClient(srv.URL, "afk-bot-1", "tok", srv.Client()).Fetch(context.Background(), "")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got.Presence.Effective != presenceapi.StateParked {
		t.Errorf("Effective = %q, want parked", got.Presence.Effective)
	}
	if got.Presence.Override == nil || got.Presence.Override.State != presenceapi.StateParked {
		t.Errorf("Override = %+v, want a parked override", got.Presence.Override)
	}
}

func TestGoldenDefaultDecodesThroughTheClient(t *testing.T) {
	srv := serveGolden(t, http.StatusOK, golden(t, "presence_default.json"))

	got, err := NewClient(srv.URL, "afk-bot-1", "tok", srv.Client()).Fetch(context.Background(), "")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got.Presence.Override != nil {
		t.Errorf("Override = %+v, want none", got.Presence.Override)
	}
	if got.Presence.Effective != got.Presence.Default {
		t.Errorf("Effective = %q, want it to equal Default %q", got.Presence.Effective, got.Presence.Default)
	}
}

func TestGoldenConflictDecodesIntoHTTPError(t *testing.T) {
	srv := serveGolden(t, http.StatusConflict, golden(t, "error_conflict.json"))

	_, err := NewClient(srv.URL, "afk-bot-1", "tok", srv.Client()).Fetch(context.Background(), "")
	var he *HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("err = %v, want *HTTPError", err)
	}
	if he.Body.Code != "conflict" || he.Body.Current == nil {
		t.Errorf("HTTPError body = %+v, want code conflict with the current presence", he.Body)
	}
}

// The agent validates what the bot posts against this shape; a key the bot
// omits or renames would be rejected there, not here.
func TestReportSendsTheGoldenStatusShape(t *testing.T) {
	reqs := make(chan seen, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record(r, reqs)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	var st presenceapi.Status
	if err := json.Unmarshal(golden(t, "status.json"), &st); err != nil {
		t.Fatalf("decode golden: %v", err)
	}
	if err := NewClient(srv.URL, "afk-bot-1", "tok", srv.Client()).Report(context.Background(), st); err != nil {
		t.Fatalf("Report: %v", err)
	}
	var posted, want map[string]any
	if err := json.Unmarshal((<-reqs).body, &posted); err != nil {
		t.Fatalf("decode posted body: %v", err)
	}
	_ = json.Unmarshal(golden(t, "status.json"), &want)
	if !reflect.DeepEqual(posted, want) {
		t.Errorf("posted %v, want the golden %v", posted, want)
	}
}
```

If a golden's content differs from what an assertion above assumes (for example `presence_parked.json` carries no override), change the assertion to what the golden says and note it in the commit message: the goldens are the agent's, and this repo follows them.

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/presence/ -v`
Expected: FAIL to compile with `undefined: NewClient`, `undefined: HTTPError`, `undefined: ErrInvalidResponse`.

- [ ] **Step 4: Implement the client**

Create `internal/presence/client.go`:

```go
// Package presence asks minecraft-server-agent whether this bot should be in
// the world, and holds the connect loop out of it while the answer is parked.
package presence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

// Presence and error bodies are a few hundred bytes. The cap stops a
// misbehaving endpoint from making the bot buffer whatever it sends.
const maxBody = 64 << 10

// ErrInvalidResponse marks a 2xx answer this bot cannot act on. It is kept
// apart from transport errors because it means the agent is up and wrong,
// which needs a person rather than patience.
var ErrInvalidResponse = errors.New("presence api: invalid response")

// HTTPError is any status the client does not treat as success.
type HTTPError struct {
	StatusCode int
	Body       presenceapi.Error
}

func (e *HTTPError) Error() string {
	if e.Body.Code != "" {
		return fmt.Sprintf("presence api: %d %s: %s", e.StatusCode, e.Body.Code, e.Body.Message)
	}
	return fmt.Sprintf("presence api: %d", e.StatusCode)
}

// Fetched is one answer to GET /v1/actors/{id}/presence.
type Fetched struct {
	Presence    presenceapi.Presence
	ETag        string
	NotModified bool
}

// Client speaks the two routes a bot is allowed: reading its own presence
// and reporting its own status.
type Client struct {
	base    string
	actorID string
	token   string
	http    *http.Client
}

func NewClient(baseURL, actorID, token string, hc *http.Client) *Client {
	return &Client{base: strings.TrimRight(baseURL, "/"), actorID: actorID, token: token, http: hc}
}

func (c *Client) endpoint(suffix string) string {
	return c.base + "/v1/actors/" + url.PathEscape(c.actorID) + "/" + suffix
}

// Fetch returns the actor's presence, or NotModified when etag still matches.
func (c *Client) Fetch(ctx context.Context, etag string) (Fetched, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint("presence"), nil)
	if err != nil {
		return Fetched{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Fetched{}, fmt.Errorf("fetch presence: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusNotModified:
		return Fetched{ETag: etag, NotModified: true}, nil
	case http.StatusOK:
	default:
		return Fetched{}, readHTTPError(resp)
	}

	var p presenceapi.Presence
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&p); err != nil {
		return Fetched{}, fmt.Errorf("%w: decode presence: %v", ErrInvalidResponse, err)
	}
	if p.Effective != presenceapi.StatePresent && p.Effective != presenceapi.StateParked {
		return Fetched{}, fmt.Errorf("%w: effective state %q", ErrInvalidResponse, p.Effective)
	}
	return Fetched{Presence: p, ETag: resp.Header.Get("ETag")}, nil
}

// Report posts what the bot is doing. Any 2xx is success.
func (c *Client) Report(ctx context.Context, st presenceapi.Status) error {
	body, err := json.Marshal(st)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("status"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("report status: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return readHTTPError(resp)
	}
	return nil
}

// readHTTPError keeps the status even when the body is not the contract's
// Error shape, since a proxy in front of the agent answers with its own.
func readHTTPError(resp *http.Response) error {
	he := &HTTPError{StatusCode: resp.StatusCode}
	_ = json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&he.Body)
	return he
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -race ./internal/presence/ -v`
Expected: PASS for all seven client tests and the four contract tests (with `TestGoldenFilesRoundTrip` reporting four subtests).

- [ ] **Step 6: Commit**

```bash
git add internal/presence/client.go internal/presence/client_test.go internal/presence/contract_test.go
git commit -m "feat(presence): add a client for the agent's presence API

Decodes the agent's golden JSON from the pinned presenceapi module, so a
contract change shows up here as a failing test on the Renovate bump.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: Reconciler — fail-static desired state, admission gate, status reports

**Files:**
- Create: `internal/presence/gate.go`, `internal/presence/reconciler.go`
- Test: `internal/presence/reconciler_test.go`

**Interfaces:**
- Consumes: `NewClient`, `(*Client).Fetch`, `(*Client).Report`, `Fetched`, `HTTPError`, `ErrInvalidResponse` (Task 2); `logging.Fields` from `internal/logging/log.go:64`.
- Produces:
  - `type Gate interface { Admit(ctx context.Context) (context.Context, context.CancelFunc, error); Connected(bool) }`
  - `type AlwaysPresent struct{}` implementing `Gate`
  - `type Logger interface { Info(string, logging.Fields); Warn(string, logging.Fields); Error(string, logging.Fields) }` — `*logging.Logger` satisfies it
  - `func NewReconciler(c *Client, initial presenceapi.State, interval time.Duration, version string, log Logger) *Reconciler`
  - `func (r *Reconciler) Run(ctx context.Context)` — polls until `ctx` ends
  - `func (r *Reconciler) Desired() presenceapi.State`
  - `*Reconciler` implements `Gate`
  - Log events: `presence_changed` (info, `from`, `to`); `presence_fetch_unreachable` (warn), `presence_fetch_rejected` (error, `status`), `presence_fetch_invalid` (error), `presence_fetch_recovered` (info); the same four with `presence_report_` for status reports. Every failure line carries `error` and `acting_on`.

A `Logger` interface rather than `*logging.Logger` directly: `internal/logging` is a copy of the agent's package kept identical to upstream (`docs/decisions.md`, "The other four are copies of finished code"), and its only buffer-backed constructor, `newWithWriters`, is unexported. Tests need to assert on log lines, and adding an export to the copy would be a divergence to port back.

- [ ] **Step 1: Write the failing tests**

Create `internal/presence/reconciler_test.go`:

```go
package presence

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-afk-bot/internal/logging"
	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

// fakeAgent serves the two bot routes for afk-bot-1 the way the contract
// says the agent does: ETag "<version>-<effective>", 304 on a match.
type fakeAgent struct {
	mu          sync.Mutex
	state       presenceapi.State
	version     int
	fail        int           // non-zero: answer every request with this status
	hang        time.Duration // non-zero: sleep before answering a GET
	rawBody     string        // non-empty: serve this body on GET instead
	ifNoneMatch []string
	notModified int
	statuses    []presenceapi.Status
}

func (f *fakeAgent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	hang := f.hang
	f.mu.Unlock()
	if hang > 0 && r.Method == http.MethodGet {
		time.Sleep(hang)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != 0 {
		w.WriteHeader(f.fail)
		_ = json.NewEncoder(w).Encode(presenceapi.Error{Code: "forbidden", Message: "refused"})
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/actors/afk-bot-1/presence":
		etag := fmt.Sprintf(`"%d-%s"`, f.version, f.state)
		f.ifNoneMatch = append(f.ifNoneMatch, r.Header.Get("If-None-Match"))
		if r.Header.Get("If-None-Match") == etag {
			f.notModified++
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		if f.rawBody != "" {
			_, _ = w.Write([]byte(f.rawBody))
			return
		}
		_ = json.NewEncoder(w).Encode(presenceapi.Presence{ActorID: "afk-bot-1", Effective: f.state, Default: presenceapi.StatePresent})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/actors/afk-bot-1/status":
		var st presenceapi.Status
		_ = json.NewDecoder(r.Body).Decode(&st)
		f.statuses = append(f.statuses, st)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeAgent) set(state presenceapi.State) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = state
	f.version++
}

func (f *fakeAgent) with(fn func(f *fakeAgent)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

type logLine struct {
	level, event string
	fields       logging.Fields
}

type recorder struct {
	mu    sync.Mutex
	lines []logLine
}

func (r *recorder) add(level, event string, f logging.Fields) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, logLine{level, event, f})
}
func (r *recorder) Info(e string, f logging.Fields)  { r.add("info", e, f) }
func (r *recorder) Warn(e string, f logging.Fields)  { r.add("warn", e, f) }
func (r *recorder) Error(e string, f logging.Fields) { r.add("error", e, f) }

func (r *recorder) all(event string) []logLine {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []logLine
	for _, l := range r.lines {
		if l.event == event {
			out = append(out, l)
		}
	}
	return out
}

func newTestReconciler(t *testing.T, agent *fakeAgent, initial presenceapi.State) (*Reconciler, *httptest.Server, *recorder) {
	t.Helper()
	srv := httptest.NewServer(agent)
	t.Cleanup(srv.Close)
	log := &recorder{}
	hc := srv.Client()
	hc.Timeout = 200 * time.Millisecond
	r := NewReconciler(NewClient(srv.URL, "afk-bot-1", "tok", hc), initial, time.Hour, "test", log)
	r.now = func() time.Time { return time.Date(2026, 9, 23, 6, 0, 0, 0, time.UTC) }
	return r, srv, log
}

// admitWithin reports whether Admit returns within d. The admitted session
// lives until the test ends, so a test can watch it being cancelled by a park.
func admitWithin(t *testing.T, r *Reconciler, d time.Duration) (context.Context, bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	got := make(chan context.Context, 1)
	go func() {
		if sess, _, err := r.Admit(ctx); err == nil {
			got <- sess
		}
	}()
	select {
	case sess := <-got:
		return sess, true
	case <-time.After(d):
		cancel()
		return nil, false
	}
}

func TestNeverAnsweredStartUsesDefault(t *testing.T) {
	for _, initial := range []presenceapi.State{presenceapi.StatePresent, presenceapi.StateParked} {
		t.Run(string(initial), func(t *testing.T) {
			r, srv, _ := newTestReconciler(t, &fakeAgent{state: presenceapi.StatePresent}, initial)
			srv.Close()
			r.tick(context.Background())

			if got := r.Desired(); got != initial {
				t.Errorf("Desired = %q after an unanswered poll, want the default %q", got, initial)
			}
			if _, admitted := admitWithin(t, r, 50*time.Millisecond); admitted != (initial == presenceapi.StatePresent) {
				t.Errorf("admitted = %v with default %q", admitted, initial)
			}
		})
	}
}

func TestParkCancelsAdmittedSession(t *testing.T) {
	agent := &fakeAgent{state: presenceapi.StatePresent}
	r, _, log := newTestReconciler(t, agent, presenceapi.StatePresent)

	sess, admitted := admitWithin(t, r, time.Second)
	if !admitted {
		t.Fatal("present bot was not admitted")
	}
	agent.set(presenceapi.StateParked)
	r.tick(context.Background())

	select {
	case <-sess.Done():
	case <-time.After(time.Second):
		t.Fatal("session context survived a park")
	}
	if got := log.all("presence_changed"); len(got) != 1 || got[0].fields["to"] != "parked" {
		t.Errorf("presence_changed lines = %+v, want one to parked", got)
	}
}

func TestResumeAdmitsAfterPark(t *testing.T) {
	agent := &fakeAgent{state: presenceapi.StateParked}
	r, _, _ := newTestReconciler(t, agent, presenceapi.StateParked)

	admitted := make(chan struct{})
	go func() {
		if _, _, err := r.Admit(context.Background()); err == nil {
			close(admitted)
		}
	}()
	r.tick(context.Background())
	select {
	case <-admitted:
		t.Fatal("admitted while parked")
	case <-time.After(50 * time.Millisecond):
	}

	agent.set(presenceapi.StatePresent)
	r.tick(context.Background())
	select {
	case <-admitted:
	case <-time.After(time.Second):
		t.Fatal("not admitted after resume")
	}
}

func TestUnreachableAgentKeepsLastAnswer(t *testing.T) {
	agent := &fakeAgent{state: presenceapi.StateParked}
	r, srv, log := newTestReconciler(t, agent, presenceapi.StatePresent)
	r.tick(context.Background())
	if r.Desired() != presenceapi.StateParked {
		t.Fatalf("Desired = %q, want parked from the agent", r.Desired())
	}

	srv.Close()
	r.tick(context.Background())
	r.tick(context.Background())

	if r.Desired() != presenceapi.StateParked {
		t.Errorf("Desired = %q after the agent went away, want the last answer parked", r.Desired())
	}
	lines := log.all("presence_fetch_unreachable")
	if len(lines) != 1 {
		t.Fatalf("presence_fetch_unreachable logged %d times over two failed polls, want once", len(lines))
	}
	if lines[0].level != "warn" || lines[0].fields["acting_on"] != "parked" {
		t.Errorf("line = %+v, want warn acting_on parked", lines[0])
	}
}

func TestHungAgentKeepsLastAnswer(t *testing.T) {
	agent := &fakeAgent{state: presenceapi.StateParked}
	r, _, log := newTestReconciler(t, agent, presenceapi.StatePresent)
	r.tick(context.Background())

	agent.with(func(f *fakeAgent) { f.hang = time.Second; f.state = presenceapi.StatePresent; f.version++ })
	started := time.Now()
	r.poll(context.Background())

	if took := time.Since(started); took > 900*time.Millisecond {
		t.Errorf("poll took %v against a hung agent, want it bounded by the client timeout", took)
	}
	if r.Desired() != presenceapi.StateParked {
		t.Errorf("Desired = %q, want the last answer parked", r.Desired())
	}
	if len(log.all("presence_fetch_unreachable")) != 1 {
		t.Error("a timed-out poll was not logged as unreachable")
	}
}

func TestNotModifiedKeepsState(t *testing.T) {
	agent := &fakeAgent{state: presenceapi.StateParked, version: 4}
	r, _, log := newTestReconciler(t, agent, presenceapi.StatePresent)
	r.tick(context.Background())
	r.tick(context.Background())

	agent.with(func(f *fakeAgent) {
		if len(f.ifNoneMatch) != 2 || f.ifNoneMatch[0] != "" || f.ifNoneMatch[1] != `"4-parked"` {
			t.Errorf("If-None-Match sent = %q, want none then the ETag from the first answer", f.ifNoneMatch)
		}
		if f.notModified != 1 {
			t.Errorf("304s served = %d, want 1", f.notModified)
		}
	})
	if r.Desired() != presenceapi.StateParked {
		t.Errorf("Desired = %q after a 304, want parked kept", r.Desired())
	}
	if n := len(log.all("presence_changed")); n != 1 {
		t.Errorf("presence_changed logged %d times, want once (the 304 changes nothing)", n)
	}
}

func TestRejectedTokenIsLoggedNotFatal(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			agent := &fakeAgent{state: presenceapi.StateParked, fail: status}
			r, _, log := newTestReconciler(t, agent, presenceapi.StatePresent)

			ctx, cancel := context.WithCancel(context.Background())
			r.interval = 10 * time.Millisecond
			done := make(chan struct{})
			go func() { r.Run(ctx); close(done) }()
			time.Sleep(100 * time.Millisecond)

			select {
			case <-done:
				t.Fatal("Run returned on a rejected token")
			default:
			}
			if r.Desired() != presenceapi.StatePresent {
				t.Errorf("Desired = %q, want the default kept", r.Desired())
			}
			lines := log.all("presence_fetch_rejected")
			if len(lines) != 1 {
				t.Fatalf("presence_fetch_rejected logged %d times over repeated polls, want once", len(lines))
			}
			if lines[0].level != "error" || lines[0].fields["status"] != status {
				t.Errorf("line = %+v, want error with status %d", lines[0], status)
			}

			agent.with(func(f *fakeAgent) { f.fail = 0 })
			time.Sleep(100 * time.Millisecond)
			cancel()
			<-done
			if r.Desired() != presenceapi.StateParked {
				t.Errorf("Desired = %q after the token was fixed, want parked", r.Desired())
			}
			if len(log.all("presence_fetch_recovered")) != 1 {
				t.Error("recovery was not logged once")
			}
		})
	}
}

func TestInvalidStateKeepsLastAnswer(t *testing.T) {
	agent := &fakeAgent{state: presenceapi.StateParked}
	r, _, log := newTestReconciler(t, agent, presenceapi.StatePresent)
	r.tick(context.Background())

	agent.with(func(f *fakeAgent) { f.version++; f.rawBody = `{"actor_id":"afk-bot-1","effective":"hibernating"}` })
	r.tick(context.Background())

	if r.Desired() != presenceapi.StateParked {
		t.Errorf("Desired = %q, want parked kept over an unknown state", r.Desired())
	}
	if lines := log.all("presence_fetch_invalid"); len(lines) != 1 || lines[0].level != "error" {
		t.Errorf("presence_fetch_invalid = %+v, want one error line", lines)
	}
}

func TestReportsObservedStatus(t *testing.T) {
	agent := &fakeAgent{state: presenceapi.StatePresent}
	r, _, _ := newTestReconciler(t, agent, presenceapi.StatePresent)
	r.Connected(true)
	r.tick(context.Background())
	r.Connected(false)
	agent.set(presenceapi.StateParked)
	r.tick(context.Background())

	agent.with(func(f *fakeAgent) {
		if len(f.statuses) != 2 {
			t.Fatalf("statuses posted = %d, want one per tick", len(f.statuses))
		}
		first, second := f.statuses[0], f.statuses[1]
		if !first.Connected || first.ObservedState != presenceapi.StatePresent || first.ProcessVersion != "test" {
			t.Errorf("first status = %+v, want connected present from version test", first)
		}
		if !first.LastSeen.Equal(time.Date(2026, 9, 23, 6, 0, 0, 0, time.UTC)) {
			t.Errorf("LastSeen = %v, want the reconciler's clock", first.LastSeen)
		}
		if second.Connected || second.ObservedState != presenceapi.StateParked {
			t.Errorf("second status = %+v, want disconnected parked", second)
		}
	})
}

func TestAlwaysPresentAdmitsAtOnceAndNeverCancels(t *testing.T) {
	sess, release, err := AlwaysPresent{}.Admit(context.Background())
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	defer release()
	select {
	case <-sess.Done():
		t.Fatal("AlwaysPresent session context is already done")
	case <-time.After(20 * time.Millisecond):
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := (AlwaysPresent{}).Admit(ctx); err == nil {
		t.Error("AlwaysPresent admitted on a cancelled context")
	}
}

func TestAdmitReturnsWhenContextEndsWhileParked(t *testing.T) {
	r, _, _ := newTestReconciler(t, &fakeAgent{state: presenceapi.StateParked}, presenceapi.StateParked)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, _, err := r.Admit(ctx); errc <- err }()
	cancel()

	select {
	case err := <-errc:
		if err == nil {
			t.Error("Admit admitted a parked bot on shutdown")
		}
	case <-time.After(time.Second):
		t.Fatal("Admit did not return when its context ended")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/presence/ -v`
Expected: FAIL to compile with `undefined: NewReconciler`, `undefined: AlwaysPresent`, `undefined: Reconciler`.

- [ ] **Step 3: Implement the gate**

Create `internal/presence/gate.go`:

```go
package presence

import "context"

// Gate decides when the connect loop may hold a session.
type Gate interface {
	// Admit blocks until the bot should be in the world, then returns a
	// context the gate cancels when it no longer should. It fails only when
	// ctx ends.
	Admit(ctx context.Context) (context.Context, context.CancelFunc, error)
	// Connected records whether a session is spawned, for status reports.
	Connected(bool)
}

// AlwaysPresent is the gate with the feature off: it admits at once and
// never ends a session, which is the bot as it was before presence existed.
type AlwaysPresent struct{}

func (AlwaysPresent) Admit(ctx context.Context) (context.Context, context.CancelFunc, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	sess, cancel := context.WithCancel(ctx)
	return sess, cancel, nil
}

func (AlwaysPresent) Connected(bool) {}
```

- [ ] **Step 4: Implement the reconciler**

Create `internal/presence/reconciler.go`:

```go
package presence

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jdwillmsen/minecraft-afk-bot/internal/logging"
	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

// Logger is the subset of *logging.Logger the reconciler writes to.
type Logger interface {
	Info(event string, fields logging.Fields)
	Warn(event string, fields logging.Fields)
	Error(event string, fields logging.Fields)
}

// Reconciler acts on the last desired state the agent gave it.
//
// It is fail-static on purpose: an agent outage, a bad token or a malformed
// answer leaves the bot doing whatever it was last told, because a bot that
// flapped with the agent's health would drop the farms every time the agent
// restarted.
type Reconciler struct {
	client   *Client
	log      Logger
	interval time.Duration
	version  string
	now      func() time.Time

	mu        sync.Mutex
	desired   presenceapi.State
	etag      string
	connected bool
	// changed is closed and replaced on every change of desired, which is
	// how any number of waiters hear about it without missing one.
	changed chan struct{}

	// Touched only from Run's goroutine.
	fetchStreak  streak
	reportStreak streak
}

func NewReconciler(c *Client, initial presenceapi.State, interval time.Duration, version string, log Logger) *Reconciler {
	return &Reconciler{
		client:       c,
		log:          log,
		interval:     interval,
		version:      version,
		now:          time.Now,
		desired:      initial,
		changed:      make(chan struct{}),
		fetchStreak:  streak{event: "presence_fetch"},
		reportStreak: streak{event: "presence_report"},
	}
}

// Run polls immediately and then every interval until ctx ends.
func (r *Reconciler) Run(ctx context.Context) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		r.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (r *Reconciler) tick(ctx context.Context) {
	r.poll(ctx)
	r.report(ctx)
}

func (r *Reconciler) poll(ctx context.Context) {
	r.mu.Lock()
	etag := r.etag
	r.mu.Unlock()

	res, err := r.client.Fetch(ctx, etag)
	if err != nil {
		if ctx.Err() == nil {
			r.fetchStreak.failed(r.log, err, r.Desired())
		}
		return
	}
	r.fetchStreak.succeeded(r.log)
	if !res.NotModified {
		r.set(res.Presence.Effective, res.ETag)
	}
}

func (r *Reconciler) report(ctx context.Context) {
	r.mu.Lock()
	st := presenceapi.Status{
		Connected:      r.connected,
		ObservedState:  r.desired,
		LastSeen:       r.now().UTC(),
		ProcessVersion: r.version,
	}
	r.mu.Unlock()

	if err := r.client.Report(ctx, st); err != nil {
		if ctx.Err() == nil {
			r.reportStreak.failed(r.log, err, st.ObservedState)
		}
		return
	}
	r.reportStreak.succeeded(r.log)
}

func (r *Reconciler) set(state presenceapi.State, etag string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.etag = etag
	if state == r.desired {
		return
	}
	r.log.Info("presence_changed", logging.Fields{"from": string(r.desired), "to": string(state)})
	r.desired = state
	close(r.changed)
	r.changed = make(chan struct{})
}

// Desired is the state the bot is acting on.
func (r *Reconciler) Desired() presenceapi.State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.desired
}

func (r *Reconciler) Connected(c bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.connected = c
}

func (r *Reconciler) Admit(ctx context.Context) (context.Context, context.CancelFunc, error) {
	for {
		r.mu.Lock()
		state, changed := r.desired, r.changed
		r.mu.Unlock()

		if state == presenceapi.StatePresent {
			sess, cancel := context.WithCancel(ctx)
			go r.cancelOnPark(sess, cancel, changed)
			return sess, cancel, nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
}

// cancelOnPark starts from the change channel read together with the state
// that admitted the session, so a park landing between admission and this
// goroutine starting is still seen.
func (r *Reconciler) cancelOnPark(sess context.Context, cancel context.CancelFunc, changed <-chan struct{}) {
	for {
		select {
		case <-sess.Done():
			return
		case <-changed:
		}
		r.mu.Lock()
		state, next := r.desired, r.changed
		r.mu.Unlock()
		if state == presenceapi.StateParked {
			cancel()
			return
		}
		changed = next
	}
}

// streak logs a failure when it starts or changes kind, and once when it
// ends, so a poll every ten seconds against a dead agent writes two lines
// rather than one per poll.
type streak struct {
	event string
	kind  string
}

func (s *streak) failed(log Logger, err error, actingOn presenceapi.State) {
	kind, status := classify(err)
	if kind == s.kind {
		return
	}
	s.kind = kind
	fields := logging.Fields{"error": err.Error(), "acting_on": string(actingOn)}
	if status != 0 {
		fields["status"] = status
	}
	if kind == "unreachable" {
		log.Warn(s.event+"_unreachable", fields)
		return
	}
	// Rejected and invalid answers mean a wrong token, an unregistered actor
	// or an agent bug: waiting will not fix them, so they are errors.
	log.Error(s.event+"_"+kind, fields)
}

func (s *streak) succeeded(log Logger) {
	if s.kind == "" {
		return
	}
	s.kind = ""
	log.Info(s.event+"_recovered", nil)
}

func classify(err error) (kind string, status int) {
	var he *HTTPError
	switch {
	case errors.Is(err, ErrInvalidResponse):
		return "invalid", 0
	case errors.As(err, &he) && he.StatusCode >= 400 && he.StatusCode < 500:
		return "rejected", he.StatusCode
	case errors.As(err, &he):
		return "unreachable", he.StatusCode
	default:
		return "unreachable", 0
	}
}

var _ Gate = (*Reconciler)(nil)
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -race ./internal/presence/ -v`
Expected: PASS for every test in the package, with no `DATA RACE` report.

- [ ] **Step 6: Commit**

```bash
git add internal/presence/gate.go internal/presence/reconciler.go internal/presence/reconciler_test.go
git commit -m "feat(presence): reconcile the bot's desired state with the agent

Acts on the last answer when the agent is unreachable or refuses, and on
PRESENCE_DEFAULT until the first answer, so an agent outage never makes the
bot connect or disconnect.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: Gate the connect loop, and stamp the process version

**Files:**
- Modify: `cmd/bot/main.go` (imports lines 9-30; `main` lines 37-58; `runConnectLoop` lines 60-99 replaced; `session` signature line 102, after line 139, after line 147, lines 166-169)
- Modify: `Dockerfile` (comment lines 5-7; build line 11)
- Modify: `.github/workflows/release.yml` (build step lines 111-122)
- Test: `cmd/bot/loop_test.go` (new; `cmd/bot/backoff_test.go` is unchanged and must still pass)

**Interfaces:**
- Consumes: `config.Config.Presence`, `config.Presence.Enabled()` (Task 1); `presence.NewClient` (Task 2); `presence.Gate`, `presence.AlwaysPresent`, `presence.Logger`, `presence.NewReconciler`, `(*presence.Reconciler).Run` (Task 3); existing `backoff`, `jitter` (`cmd/bot/main.go:201-220`).
- Produces:
  - `type connectFunc func(ctx context.Context, spawned func()) error`
  - `func runConnectLoop(ctx context.Context, cfg config.Config, gate presence.Gate, connect connectFunc, log *logging.Logger)`
  - `func presenceGate(cfg config.Config, log presence.Logger) (presence.Gate, func(context.Context))`
  - `var version = "dev"`, set with `-ldflags "-X main.version=<v>"`
  - Log events: `presence_enabled` (info: `actor_id`, `url`, `default`, `poll_ms`), `session_parked` (info: `session_lasted_ms`).

- [ ] **Step 1: Write the failing tests**

Create `cmd/bot/loop_test.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-afk-bot/internal/config"
	"github.com/jdwillmsen/minecraft-afk-bot/internal/logging"
	"github.com/jdwillmsen/minecraft-afk-bot/internal/presence"
	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

// fakeGate admits once per value sent on allow, and parks the current
// session when park is called.
type fakeGate struct {
	allow     chan struct{}
	mu        sync.Mutex
	cancel    context.CancelFunc
	connected []bool
}

func newFakeGate() *fakeGate { return &fakeGate{allow: make(chan struct{})} }

func (g *fakeGate) Admit(ctx context.Context) (context.Context, context.CancelFunc, error) {
	select {
	case <-g.allow:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	sess, cancel := context.WithCancel(ctx)
	g.mu.Lock()
	g.cancel = cancel
	g.mu.Unlock()
	return sess, cancel, nil
}

func (g *fakeGate) Connected(c bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.connected = append(g.connected, c)
}

func (g *fakeGate) park() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cancel()
}

func quietLog() *logging.Logger { return logging.New("info") }

// With PRESENCE_URL unset the loop must be the loop it was: connect at once,
// and back off between failed sessions.
func TestConnectLoopWithoutPresenceReconnectsAsBefore(t *testing.T) {
	cfg := config.Config{ReconnectMinMs: 200, ReconnectMaxMs: 400}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var starts []time.Time
	began := time.Now()
	runConnectLoop(ctx, cfg, presence.AlwaysPresent{}, func(sess context.Context, spawned func()) error {
		if sess.Err() != nil {
			t.Error("session started with a cancelled context")
		}
		starts = append(starts, time.Now())
		if len(starts) == 2 {
			cancel()
		}
		return errors.New("dial: connection refused")
	}, quietLog())

	if len(starts) != 2 {
		t.Fatalf("sessions started = %d, want 2", len(starts))
	}
	if first := starts[0].Sub(began); first > 50*time.Millisecond {
		t.Errorf("first session waited %v, want it immediate", first)
	}
	if gap := starts[1].Sub(starts[0]); gap < 100*time.Millisecond {
		t.Errorf("reconnected after %v, want the backoff (at least half of 200ms)", gap)
	}
}

func TestConnectLoopParkEndsSessionAndResumesWithoutBackoff(t *testing.T) {
	// A 10s floor makes any backoff after the resume impossible to miss.
	cfg := config.Config{ReconnectMinMs: 10_000, ReconnectMaxMs: 20_000}
	gate := newFakeGate()
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan int, 4)
	done := make(chan struct{})

	n := 0
	go func() {
		defer close(done)
		runConnectLoop(ctx, cfg, gate, func(sess context.Context, spawned func()) error {
			n++
			started <- n
			spawned()
			<-sess.Done()
			return nil
		}, quietLog())
	}()

	gate.allow <- struct{}{}
	<-started
	gate.park()

	select {
	case <-started:
		t.Fatal("reconnected while parked")
	case <-time.After(100 * time.Millisecond):
	}

	resumed := time.Now()
	gate.allow <- struct{}{}
	select {
	case <-started:
		if took := time.Since(resumed); took > time.Second {
			t.Errorf("resume took %v, want it immediate", took)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no session after resume")
	}

	cancel()
	<-done
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if want := []bool{true, false, true, false}; !reflect.DeepEqual(gate.connected, want) {
		t.Errorf("Connected calls = %v, want %v", gate.connected, want)
	}
}

func TestConnectLoopExitsWhileParked(t *testing.T) {
	cfg := config.Config{ReconnectMinMs: 10_000, ReconnectMaxMs: 20_000}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runConnectLoop(ctx, cfg, newFakeGate(), func(context.Context, func()) error {
			t.Error("connected while parked")
			return nil
		}, quietLog())
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("loop did not exit on shutdown while parked")
	}
}

type recordingLog struct {
	mu    sync.Mutex
	lines []string
}

func (r *recordingLog) add(event string, f logging.Fields) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, fmt.Sprintf("%s %v", event, f))
}
func (r *recordingLog) Info(e string, f logging.Fields)  { r.add(e, f) }
func (r *recordingLog) Warn(e string, f logging.Fields)  { r.add(e, f) }
func (r *recordingLog) Error(e string, f logging.Fields) { r.add(e, f) }

func TestPresenceGateOffIsAlwaysPresent(t *testing.T) {
	log := &recordingLog{}
	gate, run := presenceGate(config.Config{}, log)

	if _, ok := gate.(presence.AlwaysPresent); !ok {
		t.Errorf("gate = %T with presence off, want presence.AlwaysPresent", gate)
	}
	if run != nil {
		t.Error("a poller was returned with presence off")
	}
	if len(log.lines) != 0 {
		t.Errorf("logged %v with presence off, want nothing", log.lines)
	}
}

func TestPresenceGateLogsNoToken(t *testing.T) {
	log := &recordingLog{}
	cfg := config.Config{Presence: config.Presence{
		URL: "http://fwb-server-agent:8080", Token: "ffffffffffffffff", ActorID: "afk-bot-1",
		Default: presenceapi.StateParked, PollMs: 10000,
	}}
	gate, run := presenceGate(cfg, log)

	if _, ok := gate.(*presence.Reconciler); !ok || run == nil {
		t.Fatalf("gate = %T, run nil = %v; want a reconciler and its poller", gate, run == nil)
	}
	if len(log.lines) != 1 || !strings.HasPrefix(log.lines[0], "presence_enabled ") {
		t.Fatalf("lines = %v, want one presence_enabled", log.lines)
	}
	if strings.Contains(log.lines[0], "ffffffffffffffff") {
		t.Errorf("presence_enabled carries the token: %s", log.lines[0])
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./cmd/bot/ -v`
Expected: FAIL to compile with `too many arguments in call to runConnectLoop` and `undefined: presenceGate`.

- [ ] **Step 3: Implement the loop and session changes**

In `cmd/bot/main.go`, add to the imports (lines 9-30) `"net/http"` in the standard-library group and `"github.com/jdwillmsen/minecraft-afk-bot/internal/presence"` in the module group.

After `const stableSession` (line 35) add:

```go

// Bounds one poll of the agent, so a hung agent delays the next answer
// rather than stopping the poller.
const presenceTimeout = 5 * time.Second

// version is what the bot reports to the agent. The image build stamps the
// release into it; a plain go build reports "dev".
var version = "dev"
```

Replace the last two lines of `main` (line 57, `runConnectLoop(ctx, cfg, ts, log)`, and its closing brace, line 58) with:

```go
	gate, runPresence := presenceGate(cfg, log)
	if runPresence != nil {
		go runPresence(ctx)
	}

	runConnectLoop(ctx, cfg, gate, func(ctx context.Context, spawned func()) error {
		return session(ctx, cfg, ts, log, spawned)
	}, log)
}

// presenceGate returns what the connect loop waits on, and the poller to run
// beside it when PRESENCE_URL is set.
func presenceGate(cfg config.Config, log presence.Logger) (presence.Gate, func(context.Context)) {
	p := cfg.Presence
	if !p.Enabled() {
		return presence.AlwaysPresent{}, nil
	}
	client := presence.NewClient(p.URL, p.ActorID, p.Token, &http.Client{Timeout: presenceTimeout})
	r := presence.NewReconciler(client, p.Default, time.Duration(p.PollMs)*time.Millisecond, version, log)
	log.Info("presence_enabled", logging.Fields{
		"actor_id": p.ActorID,
		"url":      p.URL,
		"default":  string(p.Default),
		"poll_ms":  p.PollMs,
	})
	return r, r.Run
}
```

Replace `runConnectLoop` (lines 60-99) with:

```go
// connectFunc runs one session until it ends, calling spawned once the bot
// is in the world.
type connectFunc func(ctx context.Context, spawned func()) error

// runConnectLoop keeps the bot connected for the life of the process, except
// while the gate holds it out.
//
// It reconnects rather than exiting, because exiting moves the retry loop into
// Kubernetes: a coarser backoff, a fresh pod, and a token reload on every
// attempt.
func runConnectLoop(ctx context.Context, cfg config.Config, gate presence.Gate, connect connectFunc, log *logging.Logger) {
	minDelay := time.Duration(cfg.ReconnectMinMs) * time.Millisecond
	maxDelay := time.Duration(cfg.ReconnectMaxMs) * time.Millisecond
	delay := minDelay
	first := true

	for {
		if ctx.Err() != nil {
			return
		}
		if !first {
			wait := jitter(delay)
			log.Info("reconnecting", logging.Fields{"in_ms": wait.Milliseconds()})
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return
			}
		}
		first = false

		sess, release, err := gate.Admit(ctx)
		if err != nil {
			return
		}
		started := time.Now()
		err = connect(sess, func() { gate.Connected(true) })
		lasted := time.Since(started)
		gate.Connected(false)
		parked := sess.Err() != nil
		release()
		if ctx.Err() != nil {
			return
		}
		if parked {
			// A deliberate disconnect is not a failure, so it neither grows
			// the backoff nor delays the return once the gate admits again.
			log.Info("session_parked", logging.Fields{"session_lasted_ms": lasted.Milliseconds()})
			delay = minDelay
			first = true
			continue
		}
		if err != nil {
			log.Error("session_error", logging.Fields{"error": err.Error(), "session_lasted_ms": lasted.Milliseconds()})
		} else {
			log.Info("session_ended", logging.Fields{"session_lasted_ms": lasted.Milliseconds()})
		}
		delay = backoff(delay, lasted, minDelay, maxDelay)
	}
}
```

In `session`:

1. Change the signature (line 102) to:

```go
func session(ctx context.Context, cfg config.Config, ts oauth2.TokenSource, log *logging.Logger, spawned func()) error {
```

2. After `defer func() { _ = conn.Close() }()` (line 139) add:

```go

	// ReadPacket does not watch ctx, so without this a park would take effect
	// only when the server next sent something, and the player would stay
	// listed until then.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
```

3. After the `log.Info("spawned", ...)` call (lines 144-147) add:

```go
	spawned()
```

4. Replace the read error branch (lines 166-169):

```go
		pk, err := conn.ReadPacket()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("read: %w", err)
		}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./cmd/bot/ -v`
Expected: PASS for the five new tests and the five existing ones in `backoff_test.go`.

- [ ] **Step 5: Stamp the version into the image**

In `Dockerfile`, replace the comment at lines 5-7 with:

```dockerfile
# The packages shared with minecraft-server-agent are copies under internal/.
# The one module this fetches from that repository is its standard-library-only
# presence contract; docs/decisions.md has why each is handled the way it is.
```

and replace the build line (line 11) with:

```dockerfile
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/bot ./cmd/bot
```

(`ARG` sits after `COPY . .` so a new version never invalidates the cached `go mod download` layer.)

In `.github/workflows/release.yml`, add to the `docker/build-push-action` step's `with:` block (after `tags:`, line 116):

```yaml
          build-args: VERSION=${{ steps.version.outputs.value }}
```

- [ ] **Step 6: Verify the stamp and the image build**

```bash
out="$(mktemp -d)/bot"
go build -ldflags="-X main.version=9.9.9-check" -o "$out" ./cmd/bot && grep -c 9.9.9-check "$out"
docker build --build-arg VERSION=9.9.9-check -t minecraft-afk-bot:presence-check .
```

Expected: `grep` prints a count of at least `1`; the image builds to `naming to docker.io/library/minecraft-afk-bot:presence-check`.

- [ ] **Step 7: Commit**

```bash
git add cmd/bot/main.go cmd/bot/loop_test.go Dockerfile .github/workflows/release.yml
git commit -m "feat(bot): park and resume on the agent's desired state

The connect loop waits on a presence gate before every session. With
PRESENCE_URL unset the gate admits at once and never ends a session, so
the loop behaves as before. A parked session closes its connection straight
away instead of waiting for the server's next packet.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: Record the import decision, document operations, verify the branch

**Files:**
- Modify: `docs/decisions.md` (insert a dated section after the 2026-09-17 section ending line 64, before `## Shared packages are copied, not imported` at line 66; add one paragraph to that section after line 75)
- Modify: `docs/operations.md` (append a section after line 48)
- Modify: `README.md` (configuration table lines 20-29, event table lines 50-62, documentation list line 126)

**Interfaces:**
- Consumes: every name produced by Tasks 1-4 (env vars, log events, the `go.mod` diff measured in Task 1 Step 1).
- Produces: documentation only.

- [ ] **Step 1: Add the decision**

Insert into `docs/decisions.md` before `## Shared packages are copied, not imported`:

```markdown
## 2026-09-23: the presence contract is imported, not copied

The bot polls the agent for whether it should be in the world and reports
what it is doing. The request and response types come from
`github.com/jdwillmsen/minecraft-server-agent/presenceapi`, imported rather
than copied — the opposite of the packages below, for reasons that do not
apply to them.

- **It is its own module with no dependencies beyond the standard library.**
  Importing the agent's root module raised 20 of this bot's module versions
  through minimum version selection. `presenceapi` has its own `go.mod`, so
  adding it changed exactly one line of this bot's `go.mod` and no other
  version.
- **It is versioned on its own.** It is tagged `presenceapi/vX.Y.Z`, separate
  from the agent's `vX.Y.Z` releases, so Renovate proposes a bump here only
  when the contract changes, not for every agent release.
- **A copy of a wire contract is a bug waiting.** The copied packages are
  finished code that only this process runs; a drifted copy costs nothing
  until someone ports a fix. Two copies of the types both ends of an HTTP call
  decode would drift into a request the other side rejects, found in
  production. `internal/presence/contract_test.go` decodes the agent's own
  golden files from the pinned module, so a contract change fails CI on the
  bump that brings it.

The feature is off unless `PRESENCE_URL` is set, and the bot then makes no
call to the agent at all. When it is on and the agent is unreachable or
refuses, the bot keeps acting on the last answer — or on `PRESENCE_DEFAULT`
before the first — because a bot that followed the agent's health would drop
the farms every time the agent restarted.
```

In the same file, after the paragraph ending `All five stay copies, for different reasons.` (line 75), add:

```markdown

`presenceapi` is imported, not copied; the 2026-09-23 section above says why
that reasoning does not carry over to it.
```

- [ ] **Step 2: Add the runbook section**

Append to `docs/operations.md`:

```markdown

## Parking

The agent can take the bot out of the world and put it back, so the chunks
around it stop ticking without touching `replicas`. Parking is decided on the
agent — `!park`, `!unpark`, the HTTP API or `tools/mc presence` — and the bot
only follows. The chart sets these variables:

| Variable | Meaning |
|---|---|
| `PRESENCE_URL` | The agent's HTTP address. Unset turns the feature off: no call to the agent, and the bot behaves as it always has |
| `PRESENCE_TOKEN` | Bearer token with `presence:read` and `presence:report`, bound to this bot's actor |
| `PRESENCE_ACTOR_ID` | This bot's actor id, e.g. `afk-bot-1` |
| `PRESENCE_DEFAULT` | `present` or `parked`: what the bot does until the agent first answers |
| `PRESENCE_POLL_MS` | How often it asks, default `10000`; a park or resume reaches the bot within one interval |

Parked means disconnected: the reconnect loop waits instead of retrying, and
reconnects as soon as the state returns to `present`, without a backoff.

**The agent being down never moves the bot.** Unreachable, erroring or
refusing, the bot keeps doing what it was last told, and a bot started during
an agent outage does what `PRESENCE_DEFAULT` says. It keeps polling, so it
picks up the agent's answer when the agent returns.

What to look for in the logs:

| Event | Means |
|---|---|
| `presence_enabled` | Feature on at startup, with actor, URL, default and interval |
| `presence_changed` | The desired state moved, `from` → `to` |
| `session_parked` | The session was closed because the bot was parked |
| `presence_fetch_unreachable` / `presence_report_unreachable` | Agent down, timing out or answering 5xx; the bot is holding `acting_on` |
| `presence_fetch_rejected` / `presence_report_rejected` | 401: the token is wrong. 403: it is bound to another actor or lacks a scope. 404: `PRESENCE_ACTOR_ID` is not in the agent's `PRESENCE_ACTORS` |
| `presence_fetch_invalid` | The agent answered with something this bot cannot act on — usually a contract change this image predates |
| `presence_fetch_recovered` / `presence_report_recovered` | The failure above has ended |

Each failure is logged once when it starts and once when it ends, not on
every poll. A bot that stays parked when it should not is almost always
holding an answer from before a `rejected` line: fix the token or actor, and
it acts on the next poll.
```

- [ ] **Step 3: Update the README tables**

In `README.md`, add to the configuration table after the `LOG_LEVEL` row (line 29):

```markdown
| `PRESENCE_URL` | no | — | http(s) URL | The agent's address. Unset: parking is off and no other `PRESENCE_*` variable is read ([parking][parking]) |
| `PRESENCE_TOKEN` | with `PRESENCE_URL` | — | non-empty | Bearer token for the agent's presence API |
| `PRESENCE_ACTOR_ID` | with `PRESENCE_URL` | — | `^[a-z0-9][a-z0-9-]{0,62}$` | This bot's actor id |
| `PRESENCE_DEFAULT` | with `PRESENCE_URL` | — | `present`, `parked` | Acted on until the agent first answers |
| `PRESENCE_POLL_MS` | no | `10000` | 1–600000 | How often the agent is asked |
```

and after the `[radius]:` link definition (line 34):

```markdown
[parking]: docs/operations.md#parking
```

Add to the event table after the `session_error` row (line 59):

```markdown
| `session_parked` | info | The agent parked this bot; the session was closed and the loop is waiting |
| `presence_enabled` | info | Parking is on; names the actor, URL, default and poll interval |
| `presence_changed` | info | The desired state moved (`from`, `to`) |
| `presence_fetch_unreachable` | warn | The agent could not be asked; the bot holds its last answer |
| `presence_fetch_rejected` | error | The agent refused the token or does not know the actor |
| `presence_fetch_invalid` | error | The agent's answer could not be acted on |
| `presence_fetch_recovered` | info | Asking the agent works again |
| `presence_report_unreachable`, `presence_report_rejected`, `presence_report_recovered` | warn, error, info | The same, for the status report |
```

In the Documentation list (line 126) change the operations entry to:

```markdown
- [docs/operations.md](docs/operations.md) — first run, allowlist, auth cache, parking
```

- [ ] **Step 4: Full verification**

```bash
gofmt -l .
go vet ./...
CGO_ENABLED=0 go build ./...
go test -race ./...
golangci-lint run
git diff origin/main -- go.mod
```

Expected: `gofmt -l .` prints nothing; `go vet` and `go build` print nothing; `go test -race` reports `ok` for `cmd/bot`, `cmd/protocolcheck`, `internal/config`, `internal/liveness`, `internal/logging`, `internal/mcauth`, `internal/mcproto`, `internal/presence`, `internal/skin`, and no `DATA RACE`; `golangci-lint run` prints `0 issues.`; the `go.mod` diff adds only the `presenceapi v0.1.0` require.

- [ ] **Step 5: Commit**

```bash
git add docs/decisions.md docs/operations.md README.md
git commit -m "docs: record the presence contract import and document parking

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

## Deviations from spec/contract

1. **Shutdown with the feature off is faster than today.** The spec says an unset `PRESENCE_URL` leaves the bot "exactly as today". Parking needs the session to end within seconds, but `conn.ReadPacket` (`cmd/bot/main.go:166`) does not watch its context; today a cancelled session ends only when the server next sends a packet. Task 4 closes the connection when the session context ends (`context.AfterFunc`), and that code runs on SIGTERM whether or not presence is on. The only observable difference with the feature off is that the process exits on SIGTERM without waiting for a server packet. Everything else is pinned by `TestConnectLoopWithoutPresenceReconnectsAsBefore` and `TestPresenceGateOffIsAlwaysPresent`: connect at once, the same backoff, no HTTP call, no `presence_*` line.
2. **The gate is checked before every session, not once before the loop.** The spec says the reconciler "gates entry to" `runConnectLoop`. In the code, the loop owns the backoff and every reconnect (`cmd/bot/main.go:71-98`), so a park that arrives between sessions has to be checked on each pass. `gate.Admit` is called at the top of every iteration, and a parked session skips the backoff so a resume reconnects at once.
3. **`process_version` needed a source, so the build now stamps one.** The contract's `Status.ProcessVersion` exists, but the bot has no version today: the image is built without `-ldflags -X`, and `.dockerignore` excludes `.git`, so `debug.ReadBuildInfo` returns `(devel)`. Task 4 adds `ARG VERSION` to the `Dockerfile` and a `build-args` line to `release.yml`. The spec does not list these files.
4. **The reconciler logs through an interface, not `*logging.Logger`.** `internal/logging` is kept identical to the agent's copy (`docs/decisions.md:93-97`), and its only buffer-backed constructor is unexported. The reconciler takes a three-method `presence.Logger`, which `*logging.Logger` already satisfies, so the copied package stays unchanged.
5. **`PRESENCE_DEFAULT` is required when `PRESENCE_URL` is set.** The contract lists no default for it. A bot with the feature on and no default would have to guess whether to connect during an agent outage, so it refuses to start instead. The chart sets the variable anyway (spec: "which the chart sets to the actor's default").

## Self-review notes

- Spec coverage: 10 s poll with `If-None-Match` (Tasks 2, 3); status POST every tick (Task 3); park closes the session and pauses reconnects, resume reconnects (Tasks 3, 4); fail-static on an unreachable agent and `PRESENCE_DEFAULT` before the first answer (Task 3); feature off when `PRESENCE_URL` is unset (Tasks 1, 4); the contract as its own module, with golden files (Tasks 1, 2); `decisions.md` and `operations.md` (Task 5). Required tests, all against an `httptest` fake: park, resume, unreachable, never answered, 304, 401/403 logged and not fatal (Task 3).
- Out of scope, owned by other plans: the agent's routes and golden files (plan 3), the chart variables and tokens (plan 6), alerts on `mc_presence_observed` (plan 7).
- The plan's code was checked while writing it. It was applied to a copy of `minecraft-afk-bot` at `cf7cd65`, with `presenceapi` stubbed from the contract's type block and sample goldens. On that copy, `go vet`, `go test -race -count=5` and `golangci-lint run` (`0 issues.`) all passed. The real goldens from plan 3 may differ from the samples; Task 2 says how to adjust the assertions if they do.
