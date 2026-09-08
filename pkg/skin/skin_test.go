package skin

import (
	"encoding/base64"
	"testing"
)

// The server rejects a skin whose buffer does not match its declared
// dimensions, and the failure is a login rejection rather than a bad-looking
// player -- worth asserting rather than eyeballing in game.
func TestSkinDataMatchesItsDeclaredSize(t *testing.T) {
	s := For("fwb-server-agent")
	raw, err := base64.StdEncoding.DecodeString(s.Data)
	if err != nil {
		t.Fatalf("skin data is not valid base64: %v", err)
	}
	if want := s.Width * s.Height * 4; len(raw) != want {
		t.Errorf("skin data is %d bytes, want %d for %dx%d RGBA", len(raw), want, s.Width, s.Height)
	}
	if s.Width != 64 || s.Height != 64 {
		t.Errorf("skin is %dx%d, want the modern 64x64 format", s.Width, s.Height)
	}
}

// A skin that changed on redeploy would be worse than none: the agent would
// look like a different player every time it rolled.
func TestSkinIsDeterministic(t *testing.T) {
	a, b := For("fwb-server-agent"), For("fwb-server-agent")
	if a.ID != b.ID || a.Data != b.Data {
		t.Error("same name produced a different skin; it must be stable across restarts")
	}
}

// The whole point: two bots on one server must be visually distinguishable.
func TestDifferentNamesGetDifferentSkins(t *testing.T) {
	seen := map[string]string{}
	for _, name := range []string{"fwb-server-agent", "fwb-afk-bot", "fwb-afk-bot-2"} {
		s := For(name)
		if other, clash := seen[s.Data]; clash {
			t.Errorf("%s and %s produced identical skins", name, other)
		}
		seen[s.Data] = name
	}
}

// Every pixel must be opaque. A transparent one renders as a hole in the
// model, which reads as a rendering fault rather than a design choice.
func TestEveryPixelIsOpaque(t *testing.T) {
	raw, _ := base64.StdEncoding.DecodeString(For("fwb-server-agent").Data)
	for i := 3; i < len(raw); i += 4 {
		if raw[i] != 255 {
			t.Fatalf("pixel %d has alpha %d, want 255", i/4, raw[i])
		}
	}
}

// An unset skin is solid black. If the generated one were too, the fix would
// be indistinguishable from the bug it replaces.
func TestSkinIsNotTheBlackPlaceholder(t *testing.T) {
	raw, _ := base64.StdEncoding.DecodeString(For("fwb-server-agent").Data)
	for i := 0; i < len(raw); i += 4 {
		if raw[i] != 0 || raw[i+1] != 0 || raw[i+2] != 0 {
			return
		}
	}
	t.Error("generated skin is entirely black, which is what an unset skin already looks like")
}

// The eyes exist to make the head readable at a glance; a body colour that
// happened to match them would undo that.
func TestFaceIsVisibleAgainstTheBody(t *testing.T) {
	for _, name := range []string{"fwb-server-agent", "fwb-afk-bot", "fwb-afk-bot-2", "someone-else"} {
		raw, _ := base64.StdEncoding.DecodeString(For(name).Data)
		at := func(x, y int) (byte, byte, byte) {
			i := (y*Width + x) * 4
			return raw[i], raw[i+1], raw[i+2]
		}
		er, eg, eb := at(10, 11) // eye
		br, bg, bb := at(8, 8)   // forehead, plain body colour
		if er == br && eg == bg && eb == bb {
			t.Errorf("%s: eye colour matches the body, so the face is invisible", name)
		}
	}
}
