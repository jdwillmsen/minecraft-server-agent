package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jdwillmsen/minecraft-server-agent/internal/announce"
	"github.com/jdwillmsen/minecraft-server-agent/internal/pgerr"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// AnnouncementPublisher stores an announcement and sends it to whoever is
// online. Satisfied by announce.Deliverer.
type AnnouncementPublisher interface {
	Publish(ctx context.Context, a announce.Announcement) (id int64, sent int, err error)
}

// PlayerResolver turns a gamertag into an XUID, through the same two-tier
// lookup !announce @player uses. Satisfied by plugin.Roster.
type PlayerResolver interface {
	XUIDFor(ctx context.Context, name string) (xuid string, ok bool, err error)
}

const (
	// maxAnnouncementRequest caps a request body. A valid one is a body of
	// at most announce.MaxBodyChars characters and a few short fields; this
	// leaves room for multi-byte text and JSON escaping, and nothing more.
	maxAnnouncementRequest = 16 << 10
	// maxExpiresIn caps a caller-chosen lifetime. The default is a day or a
	// week by target; a month is past any notice a deploy hook has, and an
	// announcement that outlives that is a nag the queue exists to prevent.
	maxExpiresIn = 30 * 24 * time.Hour
	// announceRequestTimeout keeps the store write and the immediate send
	// inside the server's WriteTimeout, so a slow bridge produces a reply
	// rather than a dropped connection. A send cut short here is not lost:
	// the row is already stored and the queue owns it.
	announceRequestTimeout = 8 * time.Second
)

// MountAnnouncements serves POST /announcements when token is non-empty,
// and reports whether it did.
//
// With no token nothing is mounted, so the path answers the mux's own 404
// like any path that was never there. A disabled API is indistinguishable
// from an absent one, and there is no configuration in which it is open.
func (s *Server) MountAnnouncements(token string, pub AnnouncementPublisher, players PlayerResolver, log *logging.Logger) bool {
	if token == "" {
		return false
	}
	s.mux.Handle("/announcements", announcementsHandler(token, pub, players, log, time.Now))
	return true
}

type announcementRequest struct {
	Body   string `json:"body"`
	Target struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
	} `json:"target"`
	Priority string `json:"priority"`
	// A pointer so "absent" -- the default lifetime -- is distinguishable
	// from an explicit zero, which is a mistake to refuse.
	ExpiresInSeconds *int64 `json:"expires_in_seconds"`
}

type announcementResponse struct {
	ID      int64 `json:"id"`
	Reached int   `json:"reached"`
}

func announcementsHandler(token string, pub AnnouncementPublisher, players PlayerResolver, log *logging.Logger, now func() time.Time) http.Handler {
	// Compared as digests so the comparison runs over equal lengths:
	// ConstantTimeCompare returns early on a length mismatch, which would
	// tell a caller how long the token is.
	want := sha256.Sum256([]byte(token))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeError(w, http.StatusMethodNotAllowed, "only POST is supported")
			return
		}
		// Before the body is read: an unauthenticated caller learns nothing
		// about what a valid request looks like.
		if !bearerMatches(r.Header.Get("Authorization"), want) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "a valid bearer token is required")
			return
		}

		req, status, msg := decodeAnnouncement(w, r)
		if status != 0 {
			writeError(w, status, msg)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), announceRequestTimeout)
		defer cancel()

		a, status, msg := describeAnnouncement(ctx, req, players, now())
		if status != 0 {
			writeError(w, status, msg)
			return
		}

		id, reached, err := pub.Publish(ctx, a)
		switch {
		case errors.Is(err, announce.ErrDisabled), pgerr.NotMigrated(err):
			writeError(w, http.StatusServiceUnavailable, "announcements are not configured")
			return
		case pgerr.NotGranted(err):
			writeError(w, http.StatusServiceUnavailable, "the announcement store is refusing access")
			return
		case err != nil && id == 0:
			log.Error("announce_api_failed", logging.Fields{"error": err.Error()})
			writeError(w, http.StatusInternalServerError, "the announcement could not be stored")
			return
		case err != nil:
			// Stored, and the immediate send failed part way: the queue
			// delivers it from here, so this is still a created announcement.
			log.Error("announce_api_send_failed", logging.Fields{"announcement_id": id, "error": err.Error()})
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(announcementResponse{ID: id, Reached: reached})
	})
}

func bearerMatches(header string, want [sha256.Size]byte) bool {
	const scheme = "bearer "
	if len(header) <= len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return false
	}
	got := sha256.Sum256([]byte(header[len(scheme):]))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

// decodeAnnouncement reads exactly one JSON object of at most
// maxAnnouncementRequest bytes. Unknown fields are refused rather than
// ignored: a caller who wrote "expires_in" meant something by it, and
// dropping it silently would give their announcement a lifetime they did not
// choose. So are keys that encoding/json would otherwise accept -- one in
// another case, or one given twice, where the last would silently win.
func decodeAnnouncement(w http.ResponseWriter, r *http.Request) (announcementRequest, int, string) {
	var req announcementRequest
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAnnouncementRequest))
	if err == nil {
		err = checkKeys(json.NewDecoder(bytes.NewReader(raw)), announcementKeys)
	}
	if err == nil {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		err = dec.Decode(&req)
		if err == nil {
			switch extra := dec.Decode(&struct{}{}); {
			case extra == io.EOF:
			case extra != nil:
				err = extra
			default:
				err = errors.New("trailing data after the JSON object")
			}
		}
	}
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		return req, http.StatusRequestEntityTooLarge, fmt.Sprintf("request bodies are capped at %d bytes", maxAnnouncementRequest)
	case err != nil:
		return req, http.StatusBadRequest, "invalid JSON: " + err.Error()
	}
	return req, 0, ""
}

// jsonKeys is the exact set of keys an object may carry, each mapped to the
// keys of the object it holds, or nil for any other value.
type jsonKeys map[string]jsonKeys

var announcementKeys = jsonKeys{
	"body":               nil,
	"target":             {"kind": nil, "value": nil},
	"priority":           nil,
	"expires_in_seconds": nil,
}

// checkKeys reads one value from dec and refuses any object key keys does
// not name exactly, and any key given twice. A value of the wrong type is
// left for the decode that follows to report.
func checkKeys(dec *json.Decoder, keys jsonKeys) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok != json.Delim('{') || keys == nil {
		return skipValue(dec, tok)
	}
	seen := make(map[string]bool, len(keys))
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := tok.(string)
		inner, known := keys[key]
		switch {
		case !known:
			return fmt.Errorf("unknown field %q", key)
		case seen[key]:
			return fmt.Errorf("duplicate field %q", key)
		}
		seen[key] = true
		if err := checkKeys(dec, inner); err != nil {
			return err
		}
	}
	_, err = dec.Token()
	return err
}

// skipValue consumes the rest of a value whose first token was tok.
func skipValue(dec *json.Decoder, tok json.Token) error {
	if tok != json.Delim('{') && tok != json.Delim('[') {
		return nil
	}
	for depth := 1; depth > 0; {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch tok {
		case json.Delim('{'), json.Delim('['):
			depth++
		case json.Delim('}'), json.Delim(']'):
			depth--
		}
	}
	return nil
}

var permissionLevels = map[string]bool{
	plugin.PermissionVisitor.String():  true,
	plugin.PermissionMember.String():   true,
	plugin.PermissionOperator.String(): true,
}

// describeAnnouncement validates req and turns it into an api-sourced
// announcement. A non-zero status is the refusal to send.
func describeAnnouncement(ctx context.Context, req announcementRequest, players PlayerResolver, now time.Time) (announce.Announcement, int, string) {
	var a announce.Announcement
	body := strings.TrimSpace(req.Body)
	if body == "" {
		return a, http.StatusBadRequest, "body is required"
	}
	if n := utf8.RuneCountInString(body); n > announce.MaxBodyChars {
		return a, http.StatusBadRequest, fmt.Sprintf("body is %d characters; the limit is %d", n, announce.MaxBodyChars)
	}

	target := announce.Target(req.Target.Kind)
	value := strings.TrimSpace(req.Target.Value)
	switch target {
	case announce.TargetEveryone, announce.TargetOnlineOnly:
		if value != "" {
			return a, http.StatusBadRequest, fmt.Sprintf("target %s takes no value", target)
		}
	case announce.TargetPermission:
		if !permissionLevels[value] {
			return a, http.StatusBadRequest, "a permission target's value is visitor, member or operator"
		}
	case announce.TargetPlayer:
		if value == "" {
			return a, http.StatusBadRequest, "a player target's value is the player's gamertag"
		}
	default:
		return a, http.StatusBadRequest, "target.kind is everyone, player, permission or online_only"
	}

	priority := announce.Priority(req.Priority)
	switch priority {
	case "":
		priority = announce.PriorityNormal
	case announce.PriorityNormal, announce.PriorityExpedited:
	default:
		return a, http.StatusBadRequest, "priority is normal or expedited"
	}

	expires := announce.DefaultExpiry(announce.SourceAPI, target, now)
	if req.ExpiresInSeconds != nil {
		secs := *req.ExpiresInSeconds
		switch {
		case !announce.Queues(target):
			return a, http.StatusBadRequest, "online_only never queues, so it takes no expiry"
		case secs <= 0 || secs > int64(maxExpiresIn/time.Second):
			return a, http.StatusBadRequest, fmt.Sprintf("expires_in_seconds must be between 1 and %d", int64(maxExpiresIn/time.Second))
		}
		at := now.Add(time.Duration(secs) * time.Second)
		expires = &at
	}

	if target == announce.TargetPlayer {
		xuid, ok, err := players.XUIDFor(ctx, value)
		if err != nil {
			// Not an answer of "no": storing on a guess would aim a private
			// message at nobody, and refusing would deny a player who exists.
			return a, http.StatusServiceUnavailable, "the player lookup failed; try again"
		}
		if !ok {
			// Refused rather than stored: an XUID nobody holds can never be
			// delivered, so the row would sit in the outbox looking in flight.
			return a, http.StatusUnprocessableEntity, fmt.Sprintf("no player named %q has ever been seen on this server", value)
		}
		value = xuid
	}

	return announce.Announcement{
		Body:         body,
		Source:       announce.SourceAPI,
		TargetKind:   target,
		TargetValue:  value,
		Priority:     priority,
		Delivery:     announce.DeliveryFor(target),
		CreatedAt:    now,
		DeliverAfter: now,
		ExpiresAt:    expires,
	}, 0, ""
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
