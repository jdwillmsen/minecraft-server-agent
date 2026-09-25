package presence

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

const (
	ScopeRead   = "presence:read"
	ScopeWrite  = "presence:write"
	ScopeReport = "presence:report"
)

const (
	// maxPresenceRequest caps a request body. A valid one is a few short
	// fields and a reason of at most maxReasonChars.
	maxPresenceRequest = 8 << 10
	// presenceRequestTimeout keeps each request inside the server's
	// WriteTimeout, so a slow database produces a 503 rather than a dropped
	// connection.
	presenceRequestTimeout = 5 * time.Second
)

// Token is one PRESENCE_TOKENS entry.
type Token struct {
	Name   string
	Secret string
	Scopes []string
	// Actor, when set, is the only actor this token may speak for.
	Actor string
}

type credential struct {
	name   string
	digest [sha256.Size]byte
	scopes map[string]bool
	actor  string
}

// mayActFor reports whether the token may speak for actor id.
func (c credential) mayActFor(id string) bool { return c.actor == "" || c.actor == id }

// API serves the /v1 presence routes. It answers from Postgres on any
// replica, so a standby serves it as well as the leader.
type API struct {
	svc   *Service
	creds []credential
	log   *logging.Logger
	mux   *http.ServeMux
	now   func() time.Time
}

func NewAPI(svc *Service, tokens []Token, log *logging.Logger) *API {
	a := &API{svc: svc, log: log, now: time.Now}
	for _, t := range tokens {
		c := credential{name: t.Name, digest: sha256.Sum256([]byte(t.Secret)), scopes: make(map[string]bool, len(t.Scopes)), actor: t.Actor}
		for _, s := range t.Scopes {
			c.scopes[s] = true
		}
		a.creds = append(a.creds, c)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/actors", a.guard(ScopeRead, a.list))
	mux.HandleFunc("GET /v1/actors/{id}/presence", a.guard(ScopeRead, a.get))
	mux.HandleFunc("PUT /v1/actors/{id}/presence", a.guard(ScopeWrite, a.put))
	mux.HandleFunc("DELETE /v1/actors/{id}/presence", a.guard(ScopeWrite, a.del))
	mux.HandleFunc("PUT /v1/groups/{group}/presence", a.guard(ScopeWrite, a.putGroup))
	mux.HandleFunc("POST /v1/actors/{id}/status", a.guard(ScopeReport, a.status))
	a.mux = mux
	return a
}

// Enabled reports whether there is anything to serve: at least one token,
// and at least one actor for it to act on.
func (a *API) Enabled() bool { return len(a.creds) > 0 && a.svc.Enabled() }

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.mux.ServeHTTP(w, r) }

type handler func(w http.ResponseWriter, r *http.Request, c credential)

// guard authenticates before anything else is read, so an unauthenticated
// caller learns nothing about what a valid request looks like.
func (a *API) guard(scope string, h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := a.authenticate(r.Header.Get("Authorization"))
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, presenceapi.Error{Code: presenceapi.CodeUnauthorized, Message: "a valid bearer token is required"})
			return
		}
		if !c.scopes[scope] {
			writeError(w, http.StatusForbidden, presenceapi.Error{Code: presenceapi.CodeForbidden, Message: "this token lacks " + scope})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), presenceRequestTimeout)
		defer cancel()
		h(w, r.WithContext(ctx), c)
	}
}

// authenticate compares the presented token against every configured one,
// as digests over equal lengths, and never stops at the first match: the
// time taken says nothing about which token matched or how long any is.
func (a *API) authenticate(header string) (credential, bool) {
	const scheme = "bearer "
	if len(header) <= len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return credential{}, false
	}
	got := sha256.Sum256([]byte(header[len(scheme):]))
	var found credential
	ok := false
	for _, c := range a.creds {
		if subtle.ConstantTimeCompare(got[:], c.digest[:]) == 1 {
			found, ok = c, true
		}
	}
	return found, ok
}

func (a *API) list(w http.ResponseWriter, r *http.Request, _ credential) {
	views, err := a.svc.List(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, views)
}

func (a *API) get(w http.ResponseWriter, r *http.Request, _ credential) {
	p, err := a.svc.Presence(r.Context(), r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	tag := ETag(p)
	w.Header().Set("ETag", tag)
	if etagMatches(r.Header.Get("If-None-Match"), tag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *API) put(w http.ResponseWriter, r *http.Request, c credential) {
	id := r.PathValue("id")
	if !c.mayActFor(id) {
		forbidden(w, "this token may only change "+c.actor)
		return
	}
	req, err := a.decodeSet(w, r)
	if err != nil {
		a.fail(w, err)
		return
	}
	p, err := a.svc.Set(r.Context(), id, req, APISource(c.name))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *API) del(w http.ResponseWriter, r *http.Request, c credential) {
	id := r.PathValue("id")
	if !c.mayActFor(id) {
		forbidden(w, "this token may only change "+c.actor)
		return
	}
	actor, ok := a.svc.Registry().Actor(id)
	if !ok {
		a.fail(w, ErrNotFound)
		return
	}
	ps, err := a.svc.Clear(r.Context(), []Actor{actor}, APISource(c.name))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ps[0])
}

// putGroup ignores the request's version: a group has no single version to
// compare, so the write lands over whatever each member holds.
func (a *API) putGroup(w http.ResponseWriter, r *http.Request, c credential) {
	if c.actor != "" {
		forbidden(w, "a token bound to one actor cannot change a group")
		return
	}
	req, err := a.decodeSet(w, r)
	if err != nil {
		a.fail(w, err)
		return
	}
	ps, err := a.svc.SetGroup(r.Context(), r.PathValue("group"), req, APISource(c.name))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ps)
}

// status takes a report only from a token bound to the actor it reports
// for: an unbound token belongs to an operator, and an operator's word on
// what a bot is doing would hide what the bot itself says.
func (a *API) status(w http.ResponseWriter, r *http.Request, c credential) {
	id := r.PathValue("id")
	if c.actor != id {
		forbidden(w, "only a token bound to this actor may report its status")
		return
	}
	var st presenceapi.Status
	if err := decodeStrict(w, r, &st); err != nil {
		a.fail(w, err)
		return
	}
	if err := a.svc.ReportStatus(r.Context(), id, st); err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// decodeSet reads a SetRequest and turns a duration into an expiry. The
// duration is the CLI's natural input and is resolved here, on the agent's
// clock, so the CLI's clock never decides when a park ends.
func (a *API) decodeSet(w http.ResponseWriter, r *http.Request) (Request, error) {
	var body presenceapi.SetRequest
	if err := decodeStrict(w, r, &body); err != nil {
		return Request{}, err
	}
	req := Request{State: body.State, Until: body.Until, WakeOn: body.WakeOn, Reason: body.Reason, Version: body.Version}
	if body.Duration != "" {
		if body.Until != nil {
			return Request{}, invalid("until and duration are exclusive")
		}
		d, err := time.ParseDuration(body.Duration)
		if err != nil || d <= 0 {
			return Request{}, invalid("duration must be a positive Go duration such as 30m or 2h")
		}
		until := a.now().Add(d)
		req.Until = &until
	}
	return req, nil
}

// decodeStrict reads exactly one JSON object of bounded size. Unknown fields
// are refused: a caller who wrote "expires" meant something by it.
func decodeStrict(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPresenceRequest))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return invalid("invalid JSON: " + err.Error())
	}
	if dec.More() {
		return invalid("trailing data after the JSON object")
	}
	return nil
}

// ETag is the validator GET /v1/actors/{id}/presence serves: the override's
// version (0 without one) and the effective state. A bot polling with it
// gets a 304 until either changes.
func ETag(p presenceapi.Presence) string {
	var v int64
	if p.Override != nil {
		v = p.Override.Version
	}
	return `"` + strconv.FormatInt(v, 10) + "-" + string(p.Effective) + `"`
}

func etagMatches(header, tag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		c := strings.TrimPrefix(strings.TrimSpace(candidate), "W/")
		if c == tag || c == "*" {
			return true
		}
	}
	return false
}

func (a *API) fail(w http.ResponseWriter, err error) {
	var conflict *ConflictError
	switch {
	case errors.As(err, &conflict):
		current := conflict.Current
		writeError(w, http.StatusConflict, presenceapi.Error{Code: presenceapi.CodeConflict, Message: err.Error(), Current: &current})
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, presenceapi.Error{Code: presenceapi.CodeNotFound, Message: "no such actor or group"})
	case errors.Is(err, ErrInvalid):
		writeError(w, http.StatusBadRequest, presenceapi.Error{Code: presenceapi.CodeInvalid, Message: strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": ")})
	default:
		// ErrUnavailable, and anything the service did not classify: either
		// way the store could not answer, and a retry is the caller's move.
		a.log.Error("presence_api_unavailable", logging.Fields{"error": err.Error()})
		writeError(w, http.StatusServiceUnavailable, presenceapi.Error{Code: presenceapi.CodeUnavailable, Message: "the presence store is unavailable; retry"})
	}
}

func forbidden(w http.ResponseWriter, msg string) {
	writeError(w, http.StatusForbidden, presenceapi.Error{Code: presenceapi.CodeForbidden, Message: msg})
}

func writeError(w http.ResponseWriter, status int, e presenceapi.Error) { writeJSON(w, status, e) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
