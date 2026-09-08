// Package liveness keeps the agent alive in the world, not merely connected
// to it.
//
// Death and disconnection are different things, and only the second was ever
// handled. A killed player keeps its session: the connection stays open, the
// server keeps listing it, /readyz keeps reporting ready, and the agent sits
// on a death screen indefinitely. Measured on the live server on 2026-09-08 by
// killing the agent from the console -- pod 1/1 Running, zero restarts, still
// in the player list, and not one log line. Nothing anywhere would have said
// so.
//
// The same hazard applies more expensively to the AFK bots, whose entire
// purpose is holding chunks loaded: a dead one stops doing that while still
// counting as an available replica.
package liveness

import (
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// Writer is the part of a Bedrock connection this package needs.
//
// Narrowed to one method so the respawn handshake can be tested against a
// recording fake rather than a live server -- there is no way to die on
// demand in a unit test.
type Writer interface {
	WritePacket(pk packet.Packet) error
}

// Respawner completes the respawn handshake on the agent's behalf.
//
// Bedrock respawn is a three-step exchange, not a single packet: the client
// asks with a PlayerAction, the server answers with Respawn states, and the
// client confirms with ClientReadyToSpawn. Sending only the last step does
// nothing, which is why this is a small state machine rather than one write.
type Respawner struct {
	runtimeID uint64
	// dead tracks whether a death has been seen but not yet completed, so a
	// stray Respawn packet outside a death cannot make the agent confirm a
	// respawn nobody asked for.
	dead bool
}

// New builds a Respawner for the player with the given entity runtime ID.
func New(runtimeID uint64) *Respawner {
	return &Respawner{runtimeID: runtimeID}
}

// Dead reports whether the agent is currently between dying and respawning.
// Readiness reads this: an agent on a death screen is connected but not doing
// its job, and reporting ready would be a lie the whole point of this package
// is to stop telling.
func (r *Respawner) Dead() bool { return r.dead }

// Handle inspects one incoming packet and drives the respawn exchange.
//
// Returns true when the packet was part of a death or respawn, so the caller
// can log it; every other packet is left entirely alone.
func (r *Respawner) Handle(pk packet.Packet, w Writer) (handled bool, err error) {
	switch p := pk.(type) {
	case *packet.DeathInfo:
		// The death screen has appeared. Ask to respawn immediately: there is
		// nobody at a keyboard to press the button.
		r.dead = true
		return true, w.WritePacket(&packet.PlayerAction{
			EntityRuntimeID: r.runtimeID,
			ActionType:      protocol.PlayerActionRespawn,
		})

	case *packet.Respawn:
		// The server sends two of these while placing the player. Only the
		// second expects an answer, and answering the first would confirm a
		// spawn that has not been found yet.
		if !r.dead || p.State != packet.RespawnStateReadyToSpawn {
			return false, nil
		}
		r.dead = false
		return true, w.WritePacket(&packet.Respawn{
			Position:        p.Position,
			State:           packet.RespawnStateClientReadyToSpawn,
			EntityRuntimeID: r.runtimeID,
		})
	}
	return false, nil
}
