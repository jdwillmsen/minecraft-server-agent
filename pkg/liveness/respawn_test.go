package liveness

import (
	"errors"
	"testing"

	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

type recorder struct {
	sent []packet.Packet
	err  error
}

func (r *recorder) WritePacket(pk packet.Packet) error {
	if r.err != nil {
		return r.err
	}
	r.sent = append(r.sent, pk)
	return nil
}

const runtimeID = 42

func TestDeathRequestsRespawn(t *testing.T) {
	r, w := New(runtimeID), &recorder{}
	handled, err := r.Handle(&packet.DeathInfo{Cause: "lava"}, w)
	if err != nil || !handled {
		t.Fatalf("Handle(DeathInfo) = (%v, %v), want (true, nil)", handled, err)
	}
	if !r.Dead() {
		t.Error("Dead() false immediately after a death")
	}
	action, ok := w.sent[0].(*packet.PlayerAction)
	if !ok {
		t.Fatalf("sent %T, want a PlayerAction", w.sent[0])
	}
	if action.ActionType != protocol.PlayerActionRespawn {
		t.Errorf("ActionType = %d, want PlayerActionRespawn", action.ActionType)
	}
	if action.EntityRuntimeID != runtimeID {
		t.Errorf("EntityRuntimeID = %d, want %d", action.EntityRuntimeID, runtimeID)
	}
}

// The server sends two Respawn packets. Confirming the first would claim
// readiness for a spawn position it has not chosen yet.
func TestOnlyTheReadyStateIsConfirmed(t *testing.T) {
	r, w := New(runtimeID), &recorder{}
	_, _ = r.Handle(&packet.DeathInfo{}, w)
	w.sent = nil

	handled, err := r.Handle(&packet.Respawn{State: packet.RespawnStateSearchingForSpawn}, w)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if handled || len(w.sent) != 0 {
		t.Errorf("answered the searching-for-spawn state; sent %d packets", len(w.sent))
	}
	if !r.Dead() {
		t.Error("Dead() cleared before the respawn completed")
	}
}

func TestRespawnCompletesTheHandshake(t *testing.T) {
	r, w := New(runtimeID), &recorder{}
	_, _ = r.Handle(&packet.DeathInfo{}, w)
	w.sent = nil

	handled, err := r.Handle(&packet.Respawn{State: packet.RespawnStateReadyToSpawn}, w)
	if err != nil || !handled {
		t.Fatalf("Handle(ReadyToSpawn) = (%v, %v), want (true, nil)", handled, err)
	}
	reply, ok := w.sent[0].(*packet.Respawn)
	if !ok {
		t.Fatalf("sent %T, want a Respawn", w.sent[0])
	}
	if reply.State != packet.RespawnStateClientReadyToSpawn {
		t.Errorf("State = %d, want ClientReadyToSpawn", reply.State)
	}
	if r.Dead() {
		t.Error("Dead() still true after completing the respawn")
	}
}

// A Respawn arriving outside a death -- on a dimension change, say -- must not
// make the agent confirm a respawn nobody asked for.
func TestRespawnOutsideADeathIsIgnored(t *testing.T) {
	r, w := New(runtimeID), &recorder{}
	handled, err := r.Handle(&packet.Respawn{State: packet.RespawnStateReadyToSpawn}, w)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if handled || len(w.sent) != 0 {
		t.Errorf("confirmed a respawn with no death; sent %d packets", len(w.sent))
	}
}

// Ordinary traffic must be untouched: this sits in the packet path of a live
// session, and a handler that reacts to the wrong packet is worse than none.
func TestUnrelatedPacketsAreLeftAlone(t *testing.T) {
	r, w := New(runtimeID), &recorder{}
	for _, pk := range []packet.Packet{&packet.Text{}, &packet.PlayerList{}, &packet.SetHealth{}} {
		if handled, err := r.Handle(pk, w); handled || err != nil {
			t.Errorf("Handle(%T) = (%v, %v), want (false, nil)", pk, handled, err)
		}
	}
	if len(w.sent) != 0 {
		t.Errorf("sent %d packets for unrelated traffic", len(w.sent))
	}
}

// A failed write must surface. Swallowing it would leave the agent dead with
// nothing recording why -- the exact silence this package exists to end.
func TestWriteFailureIsReported(t *testing.T) {
	r := New(runtimeID)
	w := &recorder{err: errors.New("connection closed")}
	if _, err := r.Handle(&packet.DeathInfo{}, w); err == nil {
		t.Fatal("want an error when the respawn request cannot be written")
	}
}
