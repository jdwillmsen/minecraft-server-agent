package presence

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jdwillmsen/minecraft-server-agent/internal/audit"
	"github.com/jdwillmsen/minecraft-server-agent/internal/metrics"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

var (
	ErrNotFound    = errors.New("presence: no such actor or group")
	ErrInvalid     = errors.New("presence: invalid request")
	ErrUnavailable = errors.New("presence: store unavailable")
)

// ConflictError is a write refused because the override changed since the
// caller read it. Current is what it is now, so the caller can decide again
// without a second read.
type ConflictError struct {
	Current presenceapi.Presence
}

func (e *ConflictError) Error() string {
	var v int64
	if e.Current.Override != nil {
		v = e.Current.Override.Version
	}
	return fmt.Sprintf("%s changed since it was read; it is now at version %d", e.Current.ActorID, v)
}

func (e *ConflictError) Unwrap() error { return ErrConflict }

// Request is one override as a caller asks for it, before the service
// stamps who and when.
type Request struct {
	State   presenceapi.State
	Until   *time.Time
	WakeOn  *presenceapi.WakeOn
	Reason  string
	Version int64
}

// Source is who asked for a change, as set_by and the audit trail record it.
type Source struct {
	SetBy      string
	XUID       string
	Gamertag   string
	Permission string
}

// APISource is a write through the HTTP API. The audit columns are
// player-shaped, and an API client has no XUID, so the set_by string stands
// in for one.
func APISource(tokenName string) Source {
	by := "api:" + tokenName
	return Source{SetBy: by, XUID: by, Gamertag: tokenName, Permission: "api"}
}

// ChatSource is a write from a chat command.
func ChatSource(xuid, gamertag, permission string) Source {
	return Source{SetBy: "chat:" + gamertag, XUID: xuid, Gamertag: gamertag, Permission: permission}
}

// loopSource is the leader's loop removing an override that has ended.
var loopSource = Source{SetBy: "presence-loop", XUID: "presence-loop", Gamertag: "presence-loop", Permission: "system"}

const (
	// maxReasonChars bounds a reason, which is printed into chat replies and
	// audit rows.
	maxReasonChars = 280
	// maxStatusVersion bounds a reported process version, which is stored
	// and served back to every API reader.
	maxStatusVersion = 64
	// auditTimeout bounds the audit write, which runs after the change has
	// landed and must never be what makes a caller wait.
	auditTimeout = 2 * time.Second
)

// Service is the one path every read and write takes, whether from the API,
// chat or the loop, so validation, audit and notification cannot differ
// between them.
type Service struct {
	reg   *Registry
	store Store
	audit audit.Store
	log   *logging.Logger
	now   func() time.Time

	mu     sync.Mutex
	notify func()
}

func NewService(reg *Registry, store Store, auditor audit.Store, log *logging.Logger) *Service {
	return &Service{reg: reg, store: store, audit: auditor, log: log, now: time.Now, notify: func() {}}
}

func (s *Service) Registry() *Registry { return s.reg }

// Enabled reports whether any actor is configured.
func (s *Service) Enabled() bool { return s.reg.Enabled() }

// OnChange sets what runs after every write that landed. The loop registers
// itself here, so a change made on the leader acts at once instead of on the
// next tick.
func (s *Service) OnChange(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notify = fn
}

func (s *Service) changed() {
	s.mu.Lock()
	fn := s.notify
	s.mu.Unlock()
	fn()
}

func (s *Service) Presence(ctx context.Context, id string) (presenceapi.Presence, error) {
	a, ok := s.reg.Actor(id)
	if !ok {
		return presenceapi.Presence{}, ErrNotFound
	}
	ovs, err := s.store.Overrides(ctx)
	if err != nil {
		return presenceapi.Presence{}, unavailable(err)
	}
	return viewFrom(a, ovs), nil
}

func (s *Service) List(ctx context.Context) ([]presenceapi.ActorView, error) {
	ovs, err := s.store.Overrides(ctx)
	if err != nil {
		return nil, unavailable(err)
	}
	statuses, err := s.store.Statuses(ctx)
	if err != nil {
		return nil, unavailable(err)
	}
	views := make([]presenceapi.ActorView, 0, len(s.reg.actors))
	for _, a := range s.reg.Actors() {
		v := presenceapi.ActorView{ID: a.ID, Gamertag: a.Gamertag, Kind: a.Kind, Groups: a.Groups, Presence: viewFrom(a, ovs)}
		if st, ok := statuses[a.ID]; ok {
			v.Status = &st
		}
		views = append(views, v)
	}
	return views, nil
}

func (s *Service) Set(ctx context.Context, id string, req Request, by Source) (presenceapi.Presence, error) {
	a, ok := s.reg.Actor(id)
	if !ok {
		return presenceapi.Presence{}, ErrNotFound
	}
	ov, err := s.override(req, by)
	if err != nil {
		return presenceapi.Presence{}, err
	}
	ch, err := s.store.Set(ctx, id, ov, req.Version)
	if errors.Is(err, ErrConflict) {
		return presenceapi.Presence{}, &ConflictError{Current: View(a, ch.Prev)}
	}
	if err != nil {
		return presenceapi.Presence{}, unavailable(err)
	}
	s.record(ctx, a, ch, CauseSet, by)
	s.changed()
	return View(a, ch.Now), nil
}

// SetGroup applies one request to every member of group in one transaction.
// A group has no single version to compare, so Version is ignored.
func (s *Service) SetGroup(ctx context.Context, group string, req Request, by Source) ([]presenceapi.Presence, error) {
	actors, ok := s.reg.Group(group)
	if !ok {
		return nil, ErrNotFound
	}
	return s.SetEach(ctx, actors, func(Actor) Request { return req }, by)
}

// SetEach writes a request built per actor, for every actor, in one
// transaction and regardless of versions. Chat needs the per-actor build:
// "!park all" gives the agent a way back that it does not give a bot.
func (s *Service) SetEach(ctx context.Context, actors []Actor, build func(Actor) Request, by Source) ([]presenceapi.Presence, error) {
	ovs := make(map[string]presenceapi.Override, len(actors))
	for _, a := range actors {
		ov, err := s.override(build(a), by)
		if err != nil {
			return nil, err
		}
		ovs[a.ID] = ov
	}
	changes, err := s.store.SetMany(ctx, ovs)
	if err != nil {
		return nil, unavailable(err)
	}
	return s.landed(ctx, actors, changes, CauseSet, by), nil
}

// Clear removes the overrides of actors, returning each to its default.
func (s *Service) Clear(ctx context.Context, actors []Actor, by Source) ([]presenceapi.Presence, error) {
	ids := make([]string, 0, len(actors))
	for _, a := range actors {
		ids = append(ids, a.ID)
	}
	changes, err := s.store.Clear(ctx, ids)
	if err != nil {
		return nil, unavailable(err)
	}
	return s.landed(ctx, actors, changes, CauseCleared, by), nil
}

// landed audits every change and reports every actor's presence afterwards,
// in the caller's order. An actor with no change is reported as it is.
func (s *Service) landed(ctx context.Context, actors []Actor, changes []Change, cause Cause, by Source) []presenceapi.Presence {
	after := make(map[string]*presenceapi.Override, len(changes))
	touched := make(map[string]bool, len(changes))
	for _, ch := range changes {
		after[ch.ActorID] = ch.Now
		touched[ch.ActorID] = true
		if a, ok := s.reg.Actor(ch.ActorID); ok {
			s.record(ctx, a, ch, cause, by)
		}
	}
	out := make([]presenceapi.Presence, 0, len(actors))
	for _, a := range actors {
		out = append(out, View(a, after[a.ID]))
	}
	if len(changes) > 0 {
		s.changed()
	}
	return out
}

// Expire removes an override the policy says has ended, only if it is still
// the version the policy read. A newer write means an operator decided again
// in the meantime, and that decision stands until the next tick weighs it.
func (s *Service) Expire(ctx context.Context, r Removal) (bool, error) {
	a, ok := s.reg.Actor(r.ActorID)
	if !ok {
		return false, ErrNotFound
	}
	removed, err := s.store.Remove(ctx, r.ActorID, r.Override.Version)
	if err != nil {
		return false, unavailable(err)
	}
	if removed {
		prev := r.Override
		s.record(ctx, a, Change{ActorID: r.ActorID, Prev: &prev}, r.Cause, loopSource)
	}
	return removed, nil
}

// ReportStatus stores what an actor says it is doing.
func (s *Service) ReportStatus(ctx context.Context, id string, st presenceapi.Status) error {
	if _, ok := s.reg.Actor(id); !ok {
		return ErrNotFound
	}
	if !st.ObservedState.Valid() {
		return invalid("observed_state must be present or parked")
	}
	if len(st.ProcessVersion) > maxStatusVersion {
		return invalid(fmt.Sprintf("process_version is capped at %d bytes", maxStatusVersion))
	}
	// Stamped here rather than taken from the reporter: staleness is judged
	// against this clock, and a bot whose clock drifted would otherwise look
	// fresh long after it stopped reporting.
	st.LastSeen = s.now().UTC()
	if err := s.store.PutStatus(ctx, id, st); err != nil {
		return unavailable(err)
	}
	return nil
}

func (s *Service) override(req Request, by Source) (presenceapi.Override, error) {
	now := s.now().UTC()
	reason := strings.TrimSpace(req.Reason)
	switch {
	case !req.State.Valid():
		return presenceapi.Override{}, invalid("state must be present or parked")
	case reason == "":
		return presenceapi.Override{}, invalid("reason is required")
	case utf8.RuneCountInString(reason) > maxReasonChars:
		return presenceapi.Override{}, invalid(fmt.Sprintf("reason is capped at %d characters", maxReasonChars))
	case req.Until != nil && !req.Until.After(now):
		return presenceapi.Override{}, invalid("until must be in the future")
	}
	if w := req.WakeOn; w != nil {
		if req.State != presenceapi.StateParked {
			return presenceapi.Override{}, invalid("wake_on only ends a parked override")
		}
		if w.AnyPlayerJoin == (len(w.Players) > 0) {
			return presenceapi.Override{}, invalid("wake_on takes exactly one of any_player_join and players")
		}
		for _, p := range w.Players {
			if strings.TrimSpace(p) == "" {
				return presenceapi.Override{}, invalid("wake_on.players holds a blank gamertag")
			}
		}
	}
	ov := presenceapi.Override{State: req.State, WakeOn: req.WakeOn, Reason: reason, SetBy: by.SetBy, SetAt: now}
	if req.Until != nil {
		u := req.Until.UTC()
		ov.Until = &u
	}
	return ov, nil
}

// record logs and audits one change. The audit write is bounded and
// detached from the caller: the change has already landed, and a caller who
// gave up must not leave it unrecorded.
func (s *Service) record(ctx context.Context, a Actor, ch Change, cause Cause, by Source) {
	from, to := stateOf(a, ch.Prev), stateOf(a, ch.Now)
	reason := ""
	switch {
	case ch.Now != nil:
		reason = ch.Now.Reason
	case ch.Prev != nil:
		reason = ch.Prev.Reason
	}
	s.log.Info("presence_changed", logging.Fields{"actor": a.ID, "from": string(from), "to": string(to), "cause": string(cause), "source": by.SetBy})
	if s.audit == nil || !s.audit.Enabled() {
		return
	}
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditTimeout)
	defer cancel()
	err := s.audit.Write(auditCtx, audit.Record{
		XUID:       by.XUID,
		Gamertag:   by.Gamertag,
		Permission: by.Permission,
		Command:    "presence",
		Args:       fmt.Sprintf("actor=%s from=%s to=%s cause=%s source=%s reason=%q", a.ID, from, to, cause, by.SetBy, reason),
		Outcome:    audit.OutcomeOK,
		At:         s.now(),
	})
	if err != nil {
		metrics.AuditWriteFailure()
		s.log.Error("presence_audit_failed", logging.Fields{"actor": a.ID, "cause": string(cause), "error": err.Error()})
	}
}

func stateOf(a Actor, ov *presenceapi.Override) presenceapi.State {
	if ov == nil {
		return a.Default
	}
	return ov.State
}

func viewFrom(a Actor, ovs map[string]presenceapi.Override) presenceapi.Presence {
	if ov, ok := ovs[a.ID]; ok {
		return View(a, &ov)
	}
	return View(a, nil)
}

func invalid(msg string) error { return fmt.Errorf("%w: %s", ErrInvalid, msg) }

func unavailable(err error) error { return fmt.Errorf("%w: %w", ErrUnavailable, err) }
