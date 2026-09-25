package presence

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jdwillmsen/minecraft-server-agent/internal/metrics"
	"github.com/jdwillmsen/minecraft-server-agent/internal/metrics/metricstest"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

var _ Roster = (*roster.Roster)(nil)

type fakeConsole struct {
	mu     sync.Mutex
	online []string
	err    error
	lists  int
	kicked []string
}

func (c *fakeConsole) OnlinePlayers(context.Context) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lists++
	return append([]string(nil), c.online...), c.err
}

func (c *fakeConsole) Kick(_ context.Context, gamertag string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.kicked = append(c.kicked, gamertag)
	return nil
}

func (c *fakeConsole) commands() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lists + len(c.kicked)
}

// fakeRoster is keyed by XUID like the real one, and here the XUID is simply
// the gamertag.
type fakeRoster struct {
	mu    sync.Mutex
	knows bool
	names []string
}

func (r *fakeRoster) show(knows bool, names ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.knows, r.names = knows, names
}

func (r *fakeRoster) Knows() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.knows
}

func (r *fakeRoster) Online() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.names...)
}

func (r *fakeRoster) NameFor(xuid string) (string, bool) { return xuid, true }

type loopRig struct {
	clock   time.Time
	store   *fakeStore
	svc     *Service
	audit   *fakeAudit
	console *fakeConsole
	roster  *fakeRoster
	joins   *JoinLog
	gate    *Gate
	up      bool
	loop    *Loop
}

func newLoopRig(t *testing.T) *loopRig {
	t.Helper()
	return newLoopRigLogging(t, logging.New("error"))
}

func newLoopRigLogging(t *testing.T, log *logging.Logger) *loopRig {
	t.Helper()
	t.Cleanup(metrics.ResetPresence)
	r := &loopRig{clock: t0, store: newFakeStore(), console: &fakeConsole{}, roster: &fakeRoster{knows: true}, joins: NewJoinLog(), gate: NewGate(true), up: true}
	r.svc, r.audit = newTestService(t, r.store, &r.clock)
	r.joins.now = func() time.Time { return r.clock }
	r.loop = NewLoop(LoopConfig{
		Service: r.svc, Console: r.console, Roster: r.roster, Joins: r.joins, Gate: r.gate,
		SessionUp: func() bool { return r.up }, Version: "test", Log: log,
	})
	return r
}

func (r *loopRig) set(t *testing.T, id string, req Request) {
	t.Helper()
	if _, err := r.svc.Set(t.Context(), id, req, ChatSource("x", "Op", "operator")); err != nil {
		t.Fatalf("Set %s: %v", id, err)
	}
}

func (r *loopRig) present() bool { p, _ := r.gate.Wanted(); return p }

// captureStdout runs fn with the process's stdout redirected, for a logger
// built inside fn: *logging.Logger resolves os.Stdout at construction.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	rd, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()
	fn()
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, rd); err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	return buf.String()
}

func TestLoopParksAndRestoresItsOwnActor(t *testing.T) {
	r := newLoopRig(t)
	until := t0.Add(time.Hour)
	r.set(t, "agent", Request{State: presenceapi.StateParked, Until: &until, WakeOn: &presenceapi.WakeOn{AnyPlayerJoin: true}, Reason: "r"})
	r.loop.tick(t.Context())
	if r.present() {
		t.Fatal("gate present after the agent was parked")
	}
	r.clock = until
	r.loop.tick(t.Context())
	if !r.present() {
		t.Error("gate still parked after the override expired")
	}
	if _, ok := r.store.row("agent"); ok {
		t.Error("expired override still stored")
	}
	if last := r.audit.all()[len(r.audit.all())-1]; !strings.Contains(last.Args, "cause=expired") {
		t.Errorf("last audit = %+v, want the expiry", last)
	}
}

func TestLoopWakesOnAPlayerWhoArrivesAfterThePark(t *testing.T) {
	r := newLoopRig(t)
	r.joins.Record("Steve")
	r.clock = t0.Add(time.Second)
	until := r.clock.Add(time.Hour)
	r.set(t, "agent", Request{State: presenceapi.StateParked, Until: &until, WakeOn: &presenceapi.WakeOn{AnyPlayerJoin: true}, Reason: "r"})
	r.loop.tick(t.Context())
	if r.present() {
		t.Fatal("a join from before the park woke the agent")
	}
	r.clock = r.clock.Add(time.Minute)
	r.joins.Record("Alex")
	r.loop.tick(t.Context())
	if !r.present() {
		t.Error("a later join did not wake the agent")
	}
}

func TestLoopDoesNotRemoveANewerOverride(t *testing.T) {
	r := newLoopRig(t)
	until := t0.Add(time.Minute)
	r.set(t, "afk-bot-1", Request{State: presenceapi.StateParked, Until: &until, Reason: "r"})
	r.clock = until
	r.store.bumpOnRemove = true
	r.loop.tick(t.Context())
	if _, ok := r.store.row("afk-bot-1"); !ok {
		t.Error("the loop removed a row that changed after it was read")
	}
}

func TestLoopSkipsATickItCannotRead(t *testing.T) {
	r := newLoopRig(t)
	r.set(t, "agent", Request{State: presenceapi.StateParked, Reason: "r"})
	r.loop.tick(t.Context())
	r.store.fail(errors.New("connection refused"))
	r.roster.show(true, "JdwAgent")
	r.console.online = []string{"JdwAgent"}
	r.clock = t0.Add(time.Hour)
	r.loop.tick(t.Context())
	if r.present() {
		t.Error("a failed read changed the gate; it must keep its last answer")
	}
	if n := r.console.commands(); n != 0 {
		t.Errorf("sent %d console commands on a tick that could not read the overrides", n)
	}
}

// A release that outran its migration reads a table that is not there on
// every tick. Said once, at INFO, and each such tick is skipped outright.
func TestLoopReportsAMissingTableOnceAndSkipsTheTick(t *testing.T) {
	var r *loopRig
	out := captureStdout(t, func() {
		r = newLoopRigLogging(t, logging.New("info"))
		r.store.fail(fmt.Errorf("presence: read overrides: %w", &pgconn.PgError{
			Code: "42P01", Message: `relation "minecraft.presence_override" does not exist`,
		}))
		r.gate.Set(false)
		r.roster.show(true, "JdwAfk2")
		r.console.online = []string{"JdwAfk2"}
		for range 3 {
			r.loop.tick(t.Context())
		}
	})
	if got := strings.Count(out, `"event":"presence_store_unready"`); got != 1 {
		t.Errorf("stdout carried %d presence_store_unready events, want exactly 1:\n%s", got, out)
	}
	if !strings.Contains(out, `"level":"info"`) {
		t.Errorf("unready not logged at info:\n%s", out)
	}
	if strings.Contains(out, "presence_tick_skipped") {
		t.Errorf("a missing table was also reported as a failed read:\n%s", out)
	}
	if r.present() {
		t.Error("the gate moved on a tick that could not read the table")
	}
	if n := r.console.commands(); n != 0 {
		t.Errorf("sent %d console commands on ticks that could not read the table", n)
	}
	if _, ok := r.store.status["agent"]; ok {
		t.Error("reported its own status on a skipped tick")
	}
	if metricstest.Exists(t, "mc_presence_desired", "actor", "agent") {
		t.Error("exported metrics on a skipped tick")
	}
}

func TestLoopKicksAParkedActorStillOnTheServerAfterTheGrace(t *testing.T) {
	r := newLoopRig(t)
	r.set(t, "afk-bot-1", Request{State: presenceapi.StateParked, Reason: "r"})
	r.roster.show(true, "JdwAfk1", "Steve")
	r.console.online = []string{"jdwafk1", "Steve"}

	// afk-bot-2 is parked by default and due at once, but the roster has it
	// gone; afk-bot-1 is on the roster but still inside its grace.
	r.clock = t0.Add(KickGrace - time.Second)
	r.loop.tick(t.Context())
	if r.console.lists != 0 || len(r.console.kicked) != 0 {
		t.Fatalf("inside the grace: lists=%d kicked=%v, want no console command", r.console.lists, r.console.kicked)
	}

	r.clock = t0.Add(KickGrace)
	d := metricstest.Delta(t, func() { r.loop.tick(t.Context()) }, "mc_presence_kicks_total", "actor", "afk-bot-1")
	if d != 1 {
		t.Errorf("kick counter moved by %v, want 1", d)
	}
	if r.console.lists != 1 {
		t.Errorf("lists = %d, want one to confirm the roster before kicking", r.console.lists)
	}
	if got := r.console.kicked; len(got) != 1 || got[0] != "JdwAfk1" {
		t.Errorf("kicked %v, want JdwAfk1 by its configured name", got)
	}
}

func TestLoopDoesNotKickWhenTheServerListDisagreesWithTheRoster(t *testing.T) {
	r := newLoopRig(t)
	r.roster.show(true, "JdwAfk2")
	r.console.online = []string{"Steve"}
	r.loop.tick(t.Context())
	if r.console.lists != 1 || len(r.console.kicked) != 0 {
		t.Errorf("lists=%d kicked=%v, want one list and no kick", r.console.lists, r.console.kicked)
	}
}

func TestLoopSendsNoCommandForAParkedActorTheRosterHasGone(t *testing.T) {
	r := newLoopRig(t)
	r.roster.show(true, "Steve")
	r.console.online = []string{"JdwAfk2", "Steve"}
	for range 3 {
		r.loop.tick(t.Context())
		r.clock = r.clock.Add(TickInterval)
	}
	if n := r.console.commands(); n != 0 {
		t.Errorf("sent %d console commands for a parked actor the roster shows gone", n)
	}
}

func TestLoopSendsNoCommandWhileTheRosterKnowsNobody(t *testing.T) {
	r := newLoopRig(t)
	r.roster.show(false)
	r.console.online = []string{"JdwAfk2"}
	r.loop.tick(t.Context())
	if n := r.console.commands(); n != 0 {
		t.Errorf("sent %d console commands on a roster that cannot answer", n)
	}
}

func TestLoopDoesNotAskTheConsoleWhenNobodyIsDue(t *testing.T) {
	r := newLoopRig(t)
	r.roster.show(true, "JdwAgent", "JdwAfk1", "JdwAfk2")
	r.set(t, "afk-bot-2", Request{State: presenceapi.StatePresent, Reason: "r"})
	r.loop.tick(t.Context())
	if r.console.lists != 0 {
		t.Errorf("ran list %d times with every actor present", r.console.lists)
	}
}

func TestLoopTreatsAStaleReportAsNotConnected(t *testing.T) {
	r := newLoopRig(t)
	r.store.status["afk-bot-1"] = presenceapi.Status{Connected: true, ObservedState: presenceapi.StatePresent, LastSeen: t0.Add(-statusStale - time.Second)}
	r.store.status["afk-bot-2"] = presenceapi.Status{Connected: true, ObservedState: presenceapi.StatePresent, LastSeen: t0.Add(-time.Second)}
	r.loop.tick(t.Context())
	if got := metricstest.Value(t, "mc_presence_observed", "actor", "afk-bot-1"); got != 0 {
		t.Errorf("stale bot observed = %v, want 0", got)
	}
	if got := metricstest.Value(t, "mc_presence_observed", "actor", "afk-bot-2"); got != 1 {
		t.Errorf("fresh bot observed = %v, want 1", got)
	}
	if got := metricstest.Value(t, "mc_presence_desired", "actor", "afk-bot-2"); got != 0 {
		t.Errorf("afk-bot-2 desired = %v, want 0 from its parked default", got)
	}
}

func TestLoopReportsItsOwnStatusAndOverrideAge(t *testing.T) {
	r := newLoopRig(t)
	r.set(t, "afk-bot-1", Request{State: presenceapi.StateParked, Reason: "r"})
	r.clock = t0.Add(time.Hour)
	r.up = false
	r.loop.tick(t.Context())
	st := r.store.status["agent"]
	if st.Connected || st.ObservedState != presenceapi.StatePresent || st.ProcessVersion != "test" || !st.LastSeen.Equal(r.clock) {
		t.Errorf("own status = %+v", st)
	}
	if got := metricstest.Value(t, "mc_presence_override_age_seconds", "actor", "afk-bot-1"); got != 3600 {
		t.Errorf("override age = %v, want 3600", got)
	}
	if got := metricstest.Value(t, "mc_presence_observed", "actor", "agent"); got != 0 {
		t.Errorf("agent observed = %v, want 0 while its session is down", got)
	}
}

func TestPrimeDecidesTheGateBeforeRunStarts(t *testing.T) {
	r := newLoopRig(t)
	r.set(t, "agent", Request{State: presenceapi.StateParked, Reason: "r"})
	r.loop.Prime(t.Context())
	if r.present() {
		t.Error("Prime returned with the gate still present; the agent would join before its first tick")
	}
}

func (r *loopRig) waitFor(want bool) bool {
	deadline := time.Now().Add(2 * time.Second)
	for r.present() != want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	return r.present() == want
}

func (r *loopRig) runUntilCancelled(t *testing.T) (stop func()) {
	t.Helper()
	r.loop.interval = time.Hour
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { r.loop.Run(ctx); close(done) }()
	return func() { cancel(); <-done }
}

func TestRunActsOnANudgeAndWithdrawsMetricsOnExit(t *testing.T) {
	r := newLoopRig(t)
	stop := r.runUntilCancelled(t)

	r.set(t, "agent", Request{State: presenceapi.StateParked, Reason: "r"})
	r.loop.Nudge()
	if !r.waitFor(false) {
		t.Fatal("a nudge did not tick the loop")
	}
	stop()
	if metricstest.Exists(t, "mc_presence_desired", "actor", "agent") {
		t.Error("desired still exported after the leader's loop ended")
	}
}

// A write on the leader moves its own gate at once, not on the next tick.
func TestAWriteMovesTheGateWithoutWaitingForATick(t *testing.T) {
	r := newLoopRig(t)
	stop := r.runUntilCancelled(t)
	defer stop()

	r.set(t, "agent", Request{State: presenceapi.StateParked, Reason: "r"})
	if !r.waitFor(false) {
		t.Fatal("parking the agent did not close its gate")
	}
	agent, _ := r.svc.Registry().Actor("agent")
	if _, err := r.svc.Clear(t.Context(), []Actor{agent}, ChatSource("x", "Op", "operator")); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if !r.waitFor(true) {
		t.Error("clearing the park did not reopen the gate")
	}
}

func TestLoopFollowsTheDefaultWithNoDatabase(t *testing.T) {
	reg, _ := NewRegistry([]Actor{{ID: "agent", Gamertag: "A", Kind: KindAgent, Default: presenceapi.StateParked}}, "agent")
	gate := NewGate(true)
	l := NewLoop(LoopConfig{Service: NewService(reg, Nop{}, &fakeAudit{}, logging.New("error")), Console: &fakeConsole{}, Joins: NewJoinLog(), Gate: gate, SessionUp: func() bool { return false }, Log: logging.New("error")})
	l.Prime(t.Context())
	if p, _ := gate.Wanted(); p {
		t.Error("with no database the gate must follow the configured default")
	}
}
