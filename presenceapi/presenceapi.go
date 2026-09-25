// Package presenceapi is the wire contract of the agent's presence API: the
// bodies its /v1 routes accept and return.
//
// A module of its own with nothing beyond the standard library, because the
// AFK bot imports it. Importing the agent's root module once raised twenty of
// the bot's module versions and made every agent release a dependency bump
// there; this module moves only when the contract does.
package presenceapi

import "time"

type State string

const (
	StatePresent State = "present"
	StateParked  State = "parked"
)

// Valid reports whether s is one of the two states the API accepts.
func (s State) Valid() bool { return s == StatePresent || s == StateParked }

type WakeOn struct {
	AnyPlayerJoin bool     `json:"any_player_join,omitempty"`
	Players       []string `json:"players,omitempty"`
}

// Override is a runtime deviation from an actor's default.
type Override struct {
	State   State      `json:"state"`
	Until   *time.Time `json:"until,omitempty"`
	WakeOn  *WakeOn    `json:"wake_on,omitempty"`
	Reason  string     `json:"reason"`
	SetBy   string     `json:"set_by"`
	SetAt   time.Time  `json:"set_at"`
	Version int64      `json:"version"`
}

// Presence is GET /v1/actors/{id}/presence.
type Presence struct {
	ActorID   string    `json:"actor_id"`
	Effective State     `json:"effective"`
	Default   State     `json:"default"`
	Override  *Override `json:"override,omitempty"`
}

// SetRequest is the body of PUT /v1/actors/{id}/presence and
// PUT /v1/groups/{group}/presence. Exactly one of Until and Duration may be
// set. Version is required for a single actor (0 = "expect no override"),
// ignored for a group.
type SetRequest struct {
	State    State      `json:"state"`
	Until    *time.Time `json:"until,omitempty"`
	Duration string     `json:"duration,omitempty"` // Go duration, e.g. "2h"
	WakeOn   *WakeOn    `json:"wake_on,omitempty"`
	Reason   string     `json:"reason"`
	Version  int64      `json:"version"`
}

// Status is the body of POST /v1/actors/{id}/status and part of ActorView.
type Status struct {
	Connected      bool      `json:"connected"`
	ObservedState  State     `json:"observed_state"`
	LastSeen       time.Time `json:"last_seen"`
	ProcessVersion string    `json:"process_version"`
}

// ActorView is one element of GET /v1/actors.
type ActorView struct {
	ID       string   `json:"id"`
	Gamertag string   `json:"gamertag"`
	Kind     string   `json:"kind"`
	Groups   []string `json:"groups"`
	Presence
	Status *Status `json:"status,omitempty"`
}

// Error is every non-2xx body.
type Error struct {
	Code    string    `json:"code"`
	Message string    `json:"message"`
	Current *Presence `json:"current,omitempty"` // set on 409
}

// The values Error.Code takes.
const (
	CodeNotFound     = "not_found"
	CodeConflict     = "conflict"
	CodeForbidden    = "forbidden"
	CodeUnauthorized = "unauthorized"
	CodeInvalid      = "invalid"
	CodeUnavailable  = "unavailable"
)
