package presence

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

const (
	opsSecret    = "ops-secret-0123456789"
	readSecret   = "read-secret-0123456789"
	botSecret    = "bot1-secret-0123456789"
	writerSecret = "writer1-secret-0123456789"
)

type apiRig struct {
	clock time.Time
	store *fakeStore
	api   *API
}

func newAPIRig(t *testing.T) *apiRig {
	t.Helper()
	r := &apiRig{clock: t0, store: newFakeStore()}
	svc, _ := newTestService(t, r.store, &r.clock)
	r.api = NewAPI(svc, []Token{
		{Name: "ops", Secret: opsSecret, Scopes: []string{ScopeRead, ScopeWrite}},
		{Name: "reader", Secret: readSecret, Scopes: []string{ScopeRead}},
		{Name: "afk-bot-1", Secret: botSecret, Scopes: []string{ScopeRead, ScopeReport}, Actor: "afk-bot-1"},
		{Name: "writer-1", Secret: writerSecret, Scopes: []string{ScopeWrite}, Actor: "afk-bot-1"},
	}, logging.New("error"))
	r.api.now = func() time.Time { return r.clock }
	return r
}

func (r *apiRig) do(method, path, secret, body string, header ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	r.api.ServeHTTP(rec, req)
	return rec
}

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

func wantError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) presenceapi.Error {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d (%s), want %d", rec.Code, rec.Body.String(), status)
	}
	e := decodeBody[presenceapi.Error](t, rec)
	if e.Code != code {
		t.Errorf("code = %q, want %q", e.Code, code)
	}
	return e
}

const parkBody = `{"state":"parked","reason":"chunk budget","version":0}`

func TestAPIRequiresAKnownToken(t *testing.T) {
	r := newAPIRig(t)
	for _, secret := range []string{"", "not-a-token-at-all"} {
		rec := r.do(http.MethodGet, "/v1/actors", secret, "")
		wantError(t, rec, http.StatusUnauthorized, presenceapi.CodeUnauthorized)
		if rec.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Error("401 without WWW-Authenticate: Bearer")
		}
	}
}

func TestAPIEnforcesScopes(t *testing.T) {
	r := newAPIRig(t)
	wantError(t, r.do(http.MethodPut, "/v1/actors/afk-bot-1/presence", readSecret, parkBody), http.StatusForbidden, presenceapi.CodeForbidden)
	wantError(t, r.do(http.MethodPost, "/v1/actors/afk-bot-1/status", opsSecret, `{"connected":true,"observed_state":"present"}`), http.StatusForbidden, presenceapi.CodeForbidden)
	if _, ok := r.store.row("afk-bot-1"); ok {
		t.Error("a refused write was stored")
	}
}

func TestAPIPresenceETag(t *testing.T) {
	r := newAPIRig(t)
	rec := r.do(http.MethodGet, "/v1/actors/afk-bot-1/presence", botSecret, "")
	if rec.Code != http.StatusOK || rec.Header().Get("ETag") != `"0-present"` {
		t.Fatalf("GET = %d with ETag %q, want 200 and \"0-present\"", rec.Code, rec.Header().Get("ETag"))
	}
	again := r.do(http.MethodGet, "/v1/actors/afk-bot-1/presence", botSecret, "", "If-None-Match", `"0-present"`)
	if again.Code != http.StatusNotModified || again.Body.Len() != 0 {
		t.Errorf("matching If-None-Match = %d with %d body bytes, want an empty 304", again.Code, again.Body.Len())
	}
	if rec := r.do(http.MethodPut, "/v1/actors/afk-bot-1/presence", opsSecret, parkBody); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
	}
	changed := r.do(http.MethodGet, "/v1/actors/afk-bot-1/presence", botSecret, "", "If-None-Match", `"0-present"`)
	if changed.Code != http.StatusOK || changed.Header().Get("ETag") != `"1-parked"` {
		t.Errorf("after a park = %d with ETag %q, want 200 and \"1-parked\"", changed.Code, changed.Header().Get("ETag"))
	}
	if p := decodeBody[presenceapi.Presence](t, changed); p.Effective != presenceapi.StateParked || p.Override.SetBy != "api:ops" {
		t.Errorf("body = %+v", p)
	}
}

func TestAPIConflictCarriesTheCurrentRow(t *testing.T) {
	r := newAPIRig(t)
	r.do(http.MethodPut, "/v1/actors/afk-bot-1/presence", opsSecret, parkBody)
	e := wantError(t, r.do(http.MethodPut, "/v1/actors/afk-bot-1/presence", opsSecret, parkBody), http.StatusConflict, presenceapi.CodeConflict)
	if e.Current == nil || e.Current.Override == nil || e.Current.Override.Version != 1 {
		t.Errorf("current = %+v, want the version 1 row", e.Current)
	}
}

func TestAPIRefusesMalformedRequests(t *testing.T) {
	r := newAPIRig(t)
	for name, body := range map[string]string{
		"not JSON":           `{`,
		"unknown field":      `{"state":"parked","reason":"r","version":0,"expires":"2h"}`,
		"trailing data":      parkBody + `{}`,
		"until and duration": `{"state":"parked","reason":"r","version":0,"until":"2026-09-23T20:00:00Z","duration":"2h"}`,
		"bad duration":       `{"state":"parked","reason":"r","version":0,"duration":"soon"}`,
		"negative duration":  `{"state":"parked","reason":"r","version":0,"duration":"-1h"}`,
		"no reason":          `{"state":"parked","version":0}`,
	} {
		t.Run(name, func(t *testing.T) {
			wantError(t, r.do(http.MethodPut, "/v1/actors/afk-bot-1/presence", opsSecret, body), http.StatusBadRequest, presenceapi.CodeInvalid)
		})
	}
	wantError(t, r.do(http.MethodPut, "/v1/actors/afk-bot-9/presence", opsSecret, parkBody), http.StatusNotFound, presenceapi.CodeNotFound)
	wantError(t, r.do(http.MethodGet, "/v1/actors/afk-bot-9/presence", opsSecret, ""), http.StatusNotFound, presenceapi.CodeNotFound)
}

func TestAPIDurationBecomesAnExpiry(t *testing.T) {
	r := newAPIRig(t)
	rec := r.do(http.MethodPut, "/v1/actors/afk-bot-1/presence", opsSecret, `{"state":"parked","duration":"2h","reason":"r","version":0}`)
	p := decodeBody[presenceapi.Presence](t, rec)
	if p.Override == nil || p.Override.Until == nil || !p.Override.Until.Equal(t0.Add(2*time.Hour)) {
		t.Errorf("override = %+v, want until two hours on", p.Override)
	}
}

func TestAPIDeleteReturnsTheDefault(t *testing.T) {
	r := newAPIRig(t)
	r.do(http.MethodPut, "/v1/actors/afk-bot-2/presence", opsSecret, `{"state":"present","reason":"r","version":0}`)
	rec := r.do(http.MethodDelete, "/v1/actors/afk-bot-2/presence", opsSecret, "")
	if p := decodeBody[presenceapi.Presence](t, rec); rec.Code != http.StatusOK || p.Effective != presenceapi.StateParked || p.Override != nil {
		t.Errorf("DELETE = %d %+v, want 200 and the parked default", rec.Code, p)
	}
}

// A group has no single version to compare, so the version a group PUT
// carries is ignored rather than refused.
func TestAPIGroupWrite(t *testing.T) {
	r := newAPIRig(t)
	rec := r.do(http.MethodPut, "/v1/groups/bots/presence", opsSecret, `{"state":"parked","reason":"r","version":42}`)
	if ps := decodeBody[[]presenceapi.Presence](t, rec); rec.Code != http.StatusOK || len(ps) != 2 {
		t.Errorf("group PUT = %d %+v, want both bots", rec.Code, ps)
	}
	wantError(t, r.do(http.MethodPut, "/v1/groups/miners/presence", opsSecret, parkBody), http.StatusNotFound, presenceapi.CodeNotFound)
}

func TestBoundTokenCannotSpeakForAnotherActor(t *testing.T) {
	r := newAPIRig(t)
	status := `{"connected":true,"observed_state":"present","last_seen":"2026-09-23T18:00:00Z","process_version":"1.4.0"}`
	if rec := r.do(http.MethodPost, "/v1/actors/afk-bot-1/status", botSecret, status); rec.Code != http.StatusNoContent {
		t.Fatalf("own status = %d: %s", rec.Code, rec.Body.String())
	}
	wantError(t, r.do(http.MethodPost, "/v1/actors/afk-bot-2/status", botSecret, status), http.StatusForbidden, presenceapi.CodeForbidden)
	wantError(t, r.do(http.MethodPut, "/v1/actors/afk-bot-2/presence", writerSecret, parkBody), http.StatusForbidden, presenceapi.CodeForbidden)
	wantError(t, r.do(http.MethodPut, "/v1/groups/bots/presence", writerSecret, parkBody), http.StatusForbidden, presenceapi.CodeForbidden)
	if _, ok := r.store.row("afk-bot-2"); ok {
		t.Error("a forbidden write was stored")
	}
	if rec := r.do(http.MethodPut, "/v1/actors/afk-bot-1/presence", writerSecret, parkBody); rec.Code != http.StatusOK {
		t.Errorf("bound token writing its own actor = %d", rec.Code)
	}
}

func TestAPIStoreOutageIsUnavailable(t *testing.T) {
	r := newAPIRig(t)
	r.store.fail(errors.New("connection refused"))
	wantError(t, r.do(http.MethodGet, "/v1/actors", opsSecret, ""), http.StatusServiceUnavailable, presenceapi.CodeUnavailable)
	wantError(t, r.do(http.MethodPut, "/v1/actors/afk-bot-1/presence", opsSecret, parkBody), http.StatusServiceUnavailable, presenceapi.CodeUnavailable)
}

// The bot and CLI decode groups as a list; null would be a different answer.
func TestAPIListEncodesEmptyGroupsAsAList(t *testing.T) {
	r := newAPIRig(t)
	rec := r.do(http.MethodGet, "/v1/actors", readSecret, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"groups":[]`) {
		t.Errorf("GET /v1/actors = %d %s, want the agent's groups as []", rec.Code, rec.Body.String())
	}
}

// A status report is a bot speaking for itself, so even a token with every
// scope may not report unless it is bound to the actor it reports for.
func TestAPIStatusNeedsATokenBoundToTheActor(t *testing.T) {
	r := &apiRig{clock: t0, store: newFakeStore()}
	svc, _ := newTestService(t, r.store, &r.clock)
	r.api = NewAPI(svc, []Token{{Name: "root", Secret: opsSecret, Scopes: []string{ScopeRead, ScopeWrite, ScopeReport}}}, logging.New("error"))
	wantError(t, r.do(http.MethodPost, "/v1/actors/afk-bot-1/status", opsSecret, `{"connected":true,"observed_state":"present"}`), http.StatusForbidden, presenceapi.CodeForbidden)
	if st, _ := r.store.Statuses(t.Context()); len(st) != 0 {
		t.Errorf("a refused report was stored: %+v", st)
	}
}

func TestAPINeverEchoesAToken(t *testing.T) {
	r := newAPIRig(t)
	for _, rec := range []*httptest.ResponseRecorder{
		r.do(http.MethodGet, "/v1/actors", "wrong-"+opsSecret, ""),
		r.do(http.MethodPut, "/v1/actors/afk-bot-1/presence", readSecret, parkBody),
		r.do(http.MethodPut, "/v1/actors/afk-bot-2/presence", writerSecret, parkBody),
		r.do(http.MethodPost, "/v1/actors/afk-bot-2/status", botSecret, `{}`),
	} {
		for _, secret := range []string{opsSecret, readSecret, writerSecret, botSecret} {
			if strings.Contains(rec.Body.String(), secret) {
				t.Errorf("a %d response carries a token", rec.Code)
			}
		}
	}
}
