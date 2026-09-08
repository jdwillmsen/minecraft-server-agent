package mcproto

import (
	"testing"

	"github.com/sandertv/gophertunnel/minecraft"
)

// Captured from the production server on 2026-09-08 rather than written by
// hand, so the field positions this package depends on are the real ones.
const realPong = "MCPE;FWB Server;2169;1.26.45;4;20;13467591190557326198;FWB;Survival;1;31132;19133;0;1;0;"

func TestParsePongReadsTheLiveServersAdvertisement(t *testing.T) {
	ad, err := ParsePong(realPong)
	if err != nil {
		t.Fatalf("parsing a real pong failed: %v", err)
	}
	if ad.Protocol != 2169 {
		t.Errorf("protocol = %d, want 2169", ad.Protocol)
	}
	if ad.Version != "1.26.45" {
		t.Errorf("version = %q, want \"1.26.45\"", ad.Version)
	}
}

// A MOTD may contain an escaped semicolon. Splitting naively would shift
// every later field by one and read the MOTD's tail as the protocol number.
func TestParsePongHandlesEscapedSemicolonsInTheMotd(t *testing.T) {
	ad, err := ParsePong(`MCPE;Server\;s Home;2169;1.26.45;0;10;1;x;Survival;`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ad.Protocol != 2169 {
		t.Errorf("protocol = %d, want 2169 -- the escaped semicolon shifted the fields", ad.Protocol)
	}
}

func TestParsePongRejectsUnusablePongs(t *testing.T) {
	for name, pong := range map[string]string{
		"truncated":      "MCPE;FWB Server;2169",
		"empty":          "",
		"not a number":   "MCPE;FWB Server;latest;1.26.45;0;10;",
		"zero":           "MCPE;FWB Server;0;1.26.45;0;10;",
		"negative":       "MCPE;FWB Server;-2169;1.26.45;0;10;",
		"empty protocol": "MCPE;FWB Server;;1.26.45;0;10;",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParsePong(pong); err == nil {
				t.Errorf("ParsePong(%q) succeeded; a bad pong must fall back to the compiled-in protocol", pong)
			}
		})
	}
}

// The case that costs nothing must also change nothing: when the server is on
// the version this binary was built for, the caller gets the default back and
// nothing is logged.
func TestMatchingAdvertisementUsesTheCompiledProtocolUntouched(t *testing.T) {
	baked := minecraft.DefaultProtocol
	spoofs := 0
	got := protocolFor(Advertisement{Protocol: baked.ID(), Version: baked.Ver()}, baked, func(Advertisement) { spoofs++ })
	if got != minecraft.Protocol(baked) {
		t.Error("a server on the compiled-in protocol should dial with the compiled-in protocol")
	}
	if spoofs != 0 {
		t.Errorf("logged %d spoofs for a server that needed none", spoofs)
	}
}

func TestDifferentAdvertisementAnnouncesTheServersNumber(t *testing.T) {
	baked := minecraft.DefaultProtocol
	advertised := baked.ID() + 7

	var seen Advertisement
	got := protocolFor(Advertisement{Protocol: advertised, Version: "1.27.0"}, baked, func(ad Advertisement) { seen = ad })

	if got.ID() != advertised {
		t.Errorf("announced protocol %d, want the server's %d", got.ID(), advertised)
	}
	if seen.Protocol != advertised || seen.Version != "1.27.0" {
		t.Errorf("spoof callback got %+v, want the advertised values", seen)
	}
}

// The reason this is an embed and not a reimplementation: announcing a
// different number must not change the packet schema the client speaks. If
// Ver or the packet pool moved with the number, the client would be claiming
// a schema it does not have.
func TestSpoofingChangesOnlyTheAnnouncedNumber(t *testing.T) {
	baked := minecraft.DefaultProtocol
	got := protocolFor(Advertisement{Protocol: baked.ID() + 1}, baked, nil)

	if got.Ver() != baked.Ver() {
		t.Errorf("version became %q, want the compiled-in %q", got.Ver(), baked.Ver())
	}
	if len(got.Packets(false)) != len(baked.Packets(false)) {
		t.Error("packet pool changed; spoofing must only change the announced number")
	}
}

// A server that rolls back -- a restore, a pinned downgrade -- must clear the
// spoof rather than leave the client announcing a number that no longer
// exists. Negotiate is stateless precisely so this needs no unwinding.
func TestRollbackClearsTheSpoof(t *testing.T) {
	baked := minecraft.DefaultProtocol
	if p := protocolFor(Advertisement{Protocol: baked.ID() + 1}, baked, nil); p.ID() == baked.ID() {
		t.Fatal("setup: expected a spoof")
	}
	if p := protocolFor(Advertisement{Protocol: baked.ID()}, baked, nil); p.ID() != baked.ID() {
		t.Error("a server back on the compiled-in protocol still got a spoofed number")
	}
}
