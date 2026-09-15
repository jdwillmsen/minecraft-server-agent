package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jdwillmsen/minecraft-server-agent/internal/announce"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

const testToken = "s3cret-token"

type fakePublisher struct {
	got []announce.Announcement
	// reached is what the immediate send counted. uncounted overrides it
	// with the answer a blind broadcast gives: said, audience unknown.
	reached   int
	uncounted bool
	queued    bool
	err       error
	id        int64
}

func (f *fakePublisher) Publish(_ context.Context, a announce.Announcement) (int64, announce.Reach, error) {
	f.got = append(f.got, a)
	if f.err != nil {
		return f.id, announce.Reach{Counted: true}, f.err
	}
	if f.uncounted {
		return 42, announce.Reach{}, nil
	}
	sent := announce.Reach{Players: f.reached, Counted: true}
	if f.queued {
		sent.Outcome = announce.OutcomeQueued
	}
	return 42, sent, nil
}

type fakePlayers struct {
	known map[string]string
	err   error
}

func (f fakePlayers) XUIDFor(_ context.Context, name string) (string, bool, error) {
	if f.err != nil {
		return "", false, f.err
	}
	x, ok := f.known[name]
	return x, ok, nil
}

func mounted(t *testing.T, token string, pub *fakePublisher, players fakePlayers) *Server {
	t.Helper()
	srv, err := New("127.0.0.1:0")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { srv.ln.Close() })
	// The live agent: what every case below is about. A process that is not
	// has its own test.
	srv.SetRole(RoleLive)
	srv.MountAnnouncements(token, pub, players, logging.New("error"))
	return srv
}

func post(srv *Server, auth, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/announcements", strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)
	return rec
}

const okBody = `{"body":"deploying v2","target":{"kind":"everyone"}}`

// With no token the route does not exist. Indistinguishable from an absent
// path, with or without credentials, and never open.
func TestAnnouncementsAreNotMountedWithoutAToken(t *testing.T) {
	pub := &fakePublisher{}
	srv := mounted(t, "", pub, fakePlayers{})
	for _, auth := range []string{"", "Bearer ", "Bearer anything"} {
		if rec := post(srv, auth, okBody); rec.Code != http.StatusNotFound {
			t.Errorf("auth %q: status = %d, want 404", auth, rec.Code)
		}
	}
	if len(pub.got) != 0 {
		t.Error("a disabled API published something")
	}
}

func TestAnnouncementsRequireTheBearerToken(t *testing.T) {
	cases := []struct {
		name string
		auth string
		want int
	}{
		{"missing", "", http.StatusUnauthorized},
		{"scheme only", "Bearer ", http.StatusUnauthorized},
		{"wrong token", "Bearer nope", http.StatusUnauthorized},
		{"right token plus a suffix", "Bearer " + testToken + "x", http.StatusUnauthorized},
		{"right token, wrong scheme", "Basic " + testToken, http.StatusUnauthorized},
		{"bare token", testToken, http.StatusUnauthorized},
		{"right token", "Bearer " + testToken, http.StatusCreated},
		{"scheme is case-insensitive", "bearer " + testToken, http.StatusCreated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pub := &fakePublisher{}
			rec := post(mounted(t, testToken, pub, fakePlayers{}), tc.auth, okBody)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.want, rec.Body)
			}
			if tc.want == http.StatusUnauthorized {
				if len(pub.got) != 0 {
					t.Error("an unauthenticated request was published")
				}
				if rec.Header().Get("WWW-Authenticate") != "Bearer" {
					t.Error("a 401 should say which scheme it wants")
				}
			}
		})
	}
}

// Authentication comes first: a stranger's malformed body is a 401, not a
// 400 that teaches them what a valid one looks like.
func TestAnnouncementsAuthenticateBeforeReadingTheBody(t *testing.T) {
	rec := post(mounted(t, testToken, &fakePublisher{}, fakePlayers{}), "Bearer wrong", "{not json")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestAnnouncementsOnlyAcceptPost(t *testing.T) {
	srv := mounted(t, testToken, &fakePublisher{}, fakePlayers{})
	req := httptest.NewRequest(http.MethodGet, "/announcements", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
		t.Errorf("GET = %d (Allow %q), want 405 naming POST", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestAnnouncementsCreateAndReportTheCountReached(t *testing.T) {
	pub := &fakePublisher{reached: 3}
	before := time.Now()
	rec := post(mounted(t, testToken, pub, fakePlayers{}), "Bearer "+testToken,
		`{"body":"  deploying v2  ","target":{"kind":"everyone"},"priority":"expedited"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", rec.Code, rec.Body)
	}
	var resp announcementResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != 42 || resp.Reached == nil || *resp.Reached != 3 {
		t.Errorf("response = %+v, want id 42 reached 3", resp)
	}
	a := pub.got[0]
	if a.Source != announce.SourceAPI || a.Body != "deploying v2" || a.Priority != announce.PriorityExpedited || a.AuthorXUID != "" {
		t.Errorf("announcement = %+v, want api-sourced, trimmed, expedited, no author", a)
	}
	if a.ExpiresAt == nil || a.ExpiresAt.Sub(before) < 23*time.Hour || a.ExpiresAt.Sub(before) > 25*time.Hour {
		t.Errorf("expiry = %v, want the default day for everyone", a.ExpiresAt)
	}
}

func TestAnnouncementsHonourAChosenExpiry(t *testing.T) {
	pub := &fakePublisher{}
	before := time.Now()
	rec := post(mounted(t, testToken, pub, fakePlayers{}), "Bearer "+testToken,
		`{"body":"x","target":{"kind":"permission","value":"operator"},"expires_in_seconds":3600}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}
	a := pub.got[0]
	if d := a.ExpiresAt.Sub(before); d < time.Hour-time.Second || d > time.Hour+time.Minute {
		t.Errorf("expiry %v out, want an hour", d)
	}
	if a.Delivery != announce.DeliveryWhisper || a.TargetValue != "operator" {
		t.Errorf("announcement = %s/%s, want a whisper to operators", a.Delivery, a.TargetValue)
	}
}

func TestAnnouncementsRefuseInvalidRequests(t *testing.T) {
	long := strings.Repeat("a", announce.MaxBodyChars+1)
	cases := map[string]string{
		"malformed JSON":             `{"body":`,
		"not an object":              `"hello"`,
		"trailing data":              okBody + ` {}`,
		"unknown field":              `{"body":"x","target":{"kind":"everyone"},"expires_in":60}`,
		"key in another case":        `{"Body":"x","target":{"kind":"everyone"}}`,
		"nested key in another case": `{"body":"x","target":{"Kind":"everyone"}}`,
		"duplicate key":              `{"body":"x","body":"y","target":{"kind":"everyone"}}`,
		"duplicate nested key":       `{"body":"x","target":{"kind":"player","kind":"everyone"}}`,
		"trailing brace":             okBody + `}`,
		"empty body":                 `{"body":"   ","target":{"kind":"everyone"}}`,
		"over-long body":             fmt.Sprintf(`{"body":%q,"target":{"kind":"everyone"}}`, long),
		"missing target":             `{"body":"x"}`,
		"unknown target":             `{"body":"x","target":{"kind":"team"}}`,
		"everyone with a value":      `{"body":"x","target":{"kind":"everyone","value":"all"}}`,
		"unknown permission":         `{"body":"x","target":{"kind":"permission","value":"admin"}}`,
		"player with no name":        `{"body":"x","target":{"kind":"player"}}`,
		"unknown priority":           `{"body":"x","target":{"kind":"everyone"},"priority":"high"}`,
		"zero expiry":                `{"body":"x","target":{"kind":"everyone"},"expires_in_seconds":0}`,
		"negative expiry":            `{"body":"x","target":{"kind":"everyone"},"expires_in_seconds":-5}`,
		"expiry past the cap":        `{"body":"x","target":{"kind":"everyone"},"expires_in_seconds":2592001}`,
		"expiry on online_only":      `{"body":"x","target":{"kind":"online_only"},"expires_in_seconds":60}`,
		"body of the wrong type":     `{"body":7,"target":{"kind":"everyone"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			pub := &fakePublisher{}
			rec := post(mounted(t, testToken, pub, fakePlayers{}), "Bearer "+testToken, body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (%s)", rec.Code, rec.Body)
			}
			if len(pub.got) != 0 {
				t.Error("an invalid request was published")
			}
			if !strings.Contains(rec.Body.String(), `"error"`) {
				t.Errorf("body %q should explain the refusal", rec.Body)
			}
		})
	}
}

// The request cap is on bytes read, independent of what the JSON says.
func TestAnnouncementsCapTheRequestBody(t *testing.T) {
	pub := &fakePublisher{}
	padding := strings.Repeat(" ", maxAnnouncementRequest)
	rec := post(mounted(t, testToken, pub, fakePlayers{}), "Bearer "+testToken, okBody+padding)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
	if len(pub.got) != 0 {
		t.Error("an oversized request was published")
	}

	// Just under the cap still parses: the cap is not a smaller limit in
	// disguise.
	under := okBody + strings.Repeat(" ", maxAnnouncementRequest-len(okBody)-1)
	if rec := post(mounted(t, testToken, &fakePublisher{}, fakePlayers{}), "Bearer "+testToken, under); rec.Code != http.StatusCreated {
		t.Errorf("a request just under the cap = %d, want 201", rec.Code)
	}
}

func TestAnnouncementsResolveAPlayerByGamertag(t *testing.T) {
	players := fakePlayers{known: map[string]string{"Dotablaze": "2535400000000001"}}

	pub := &fakePublisher{}
	rec := post(mounted(t, testToken, pub, players), "Bearer "+testToken, `{"body":"hi","target":{"kind":"player","value":"Dotablaze"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}
	if a := pub.got[0]; a.TargetValue != "2535400000000001" || a.Delivery != announce.DeliveryWhisper {
		t.Errorf("announcement = %s/%s, want a whisper to the resolved xuid", a.TargetValue, a.Delivery)
	}

	pub = &fakePublisher{}
	rec = post(mounted(t, testToken, pub, players), "Bearer "+testToken, `{"body":"hi","target":{"kind":"player","value":"Nobody"}}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("unknown player = %d, want 422", rec.Code)
	}
	if len(pub.got) != 0 {
		t.Error("a message for an unknown player was published")
	}

	pub = &fakePublisher{}
	rec = post(mounted(t, testToken, pub, fakePlayers{err: errors.New("db down")}), "Bearer "+testToken, `{"body":"hi","target":{"kind":"player","value":"Dotablaze"}}`)
	if rec.Code != http.StatusServiceUnavailable || len(pub.got) != 0 {
		t.Errorf("a failed lookup = %d with %d published, want 503 and nothing", rec.Code, len(pub.got))
	}
}

func TestAnnouncementsReportTheStoreState(t *testing.T) {
	cases := []struct {
		name string
		err  error
		id   int64
		want int
	}{
		{"no store", announce.ErrDisabled, 0, http.StatusServiceUnavailable},
		{"not migrated", fmt.Errorf("x: %w", &pgconn.PgError{Code: "42P01"}), 0, http.StatusServiceUnavailable},
		{"not granted", fmt.Errorf("x: %w", &pgconn.PgError{Code: "42501"}), 0, http.StatusServiceUnavailable},
		{"broken", errors.New("connection refused"), 0, http.StatusInternalServerError},
		// Stored, send failed part way: the queue owns it now.
		{"stored but the send failed", errors.New("bridge timeout"), 9, http.StatusCreated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := post(mounted(t, testToken, &fakePublisher{err: tc.err, id: tc.id}, fakePlayers{}), "Bearer "+testToken, okBody)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (%s)", rec.Code, tc.want, rec.Body)
			}
		})
	}
}

// This route is mounted for the whole process and a standby answers /readyz,
// so it sits in the Service endpoints like any other pod. It matters most
// during the live agent's own reconnect gap: readiness drops for the pod
// whose session is down, so the standby is the only one left answering.
//
// A target that queues is still worth taking there. The row is stored, this
// process says nothing, and whoever holds the lock delivers it on the next
// join -- refusing would make the API unusable for the whole reconnect
// backoff.
func TestAQueueingPublishIsStoredByAProcessThatIsNotTheLiveAgent(t *testing.T) {
	for _, role := range []struct {
		name string
		role Role
	}{
		{"starting", RoleStarting},
		{"standby", RoleStandby},
	} {
		t.Run(role.name, func(t *testing.T) {
			pub := &fakePublisher{}
			srv := mounted(t, testToken, pub, fakePlayers{})
			srv.SetRole(role.role)

			rec := post(srv, "Bearer "+testToken, okBody)

			if rec.Code != http.StatusCreated {
				t.Errorf("status = %d, want 201 — everyone queues, so the leader delivers it on the next join", rec.Code)
			}
			if len(pub.got) != 1 {
				t.Errorf("published %+v, want the announcement stored for the leader to deliver", pub.got)
			}
		})
	}
}

// Online-only is the one target that cannot be taken here: it never queues,
// so a row stored by a process that will not speak would be picked up by
// nothing while the caller had been told it was created.
func TestAnOnlineOnlyPublishIsRefusedByAProcessThatIsNotTheLiveAgent(t *testing.T) {
	pub := &fakePublisher{}
	srv := mounted(t, testToken, pub, fakePlayers{})
	srv.SetRole(RoleStandby)

	rec := post(srv, "Bearer "+testToken, `{"body":"restarting now","target":{"kind":"online_only"}}`)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 so the caller retries and reaches the live agent", rec.Code)
	}
	if len(pub.got) != 0 {
		t.Errorf("published %+v, want nothing stored: nothing would ever pick it up", pub.got)
	}
	if !strings.Contains(rec.Body.String(), "not the live agent") {
		t.Errorf("body = %q, want it to say why", rec.Body.String())
	}
}

// The live agent takes online_only like any other target.
func TestAnOnlineOnlyPublishIsServedByTheLiveAgent(t *testing.T) {
	pub := &fakePublisher{}
	srv := mounted(t, testToken, pub, fakePlayers{})

	rec := post(srv, "Bearer "+testToken, `{"body":"restarting now","target":{"kind":"online_only"}}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", rec.Code, rec.Body)
	}
	if len(pub.got) != 1 {
		t.Errorf("published %+v, want exactly one announcement", pub.got)
	}
}

// A broadcast said while the agent cannot see who is on the server reaches
// whoever is there and can name none of them. Reporting 0 would read as
// "nobody heard it" -- and a caller retrying on that would broadcast twice.
func TestAnUncountedBroadcastReportsNoRecipientCount(t *testing.T) {
	pub := &fakePublisher{uncounted: true}
	srv := mounted(t, testToken, pub, fakePlayers{})

	rec := post(srv, "Bearer "+testToken, okBody)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", rec.Code, rec.Body)
	}
	var resp announcementResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Reached != nil {
		t.Errorf("reached = %d, want null — it was said, and the audience could not be counted", *resp.Reached)
	}
	if !strings.Contains(rec.Body.String(), `"reached":null`) {
		t.Errorf("body = %q, want reached rendered as null", rec.Body.String())
	}
}

// The same request on the process that is playing is served as before.
func TestAPublishIsServedByTheLiveAgent(t *testing.T) {
	pub := &fakePublisher{reached: 3}
	srv := mounted(t, testToken, pub, fakePlayers{})

	rec := post(srv, "Bearer "+testToken, okBody)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	if len(pub.got) != 1 {
		t.Errorf("published %+v, want exactly one announcement", pub.got)
	}
}

// Authentication comes first: an unauthenticated caller learns nothing about
// this pod, not even which role it is in.
func TestAStandbyStillRefusesAnUnauthenticatedPublishAsUnauthorized(t *testing.T) {
	pub := &fakePublisher{}
	srv := mounted(t, testToken, pub, fakePlayers{})
	srv.SetRole(RoleStandby)

	if rec := post(srv, "Bearer wrong", okBody); rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if len(pub.got) != 0 {
		t.Error("an unauthenticated request published something")
	}
}

// A standby stores the announcement and says nothing, which is a different
// outcome from the live agent finding nobody on the server. Both report zero
// recipients, so the response has to tell them apart: a caller that retried
// the standby's zero would store a second copy and the next player to join
// would be whispered the same line twice.
func TestAStandbysStoredPublishIsReportedAsQueued(t *testing.T) {
	pub := &fakePublisher{queued: true}
	srv := mounted(t, testToken, pub, fakePlayers{})
	srv.SetRole(RoleStandby)

	rec := post(srv, "Bearer "+testToken, okBody)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", rec.Code, rec.Body)
	}
	var resp announcementResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Queued {
		t.Errorf("response = %+v, want queued true — this pod said nothing and the live agent owes the delivery", resp)
	}
	if resp.Reached == nil || *resp.Reached != 0 {
		t.Errorf("reached = %v, want 0: nothing was spoken here", resp.Reached)
	}
}

// The live agent's own zero carries no queued flag, so the two are
// distinguishable on the wire and not merely in the count.
func TestTheLiveAgentsEmptyServerIsNotReportedAsQueued(t *testing.T) {
	pub := &fakePublisher{}
	srv := mounted(t, testToken, pub, fakePlayers{})

	rec := post(srv, "Bearer "+testToken, okBody)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "queued") {
		t.Errorf("body = %q, want no queued flag from the process that does the speaking", rec.Body.String())
	}
}
