package presenceapi

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// roundTrip decodes a golden file strictly and encodes it again. Byte
// equality pins every JSON name and every omitempty: the AFK bot decodes
// these same files, so a tag changed here without changing the files fails
// in both repositories rather than in production.
func roundTrip[T any](t *testing.T, name string) T {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var v T
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	got, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("encode %s: %v", name, err)
	}
	got = append(got, '\n')
	if !bytes.Equal(got, raw) {
		t.Errorf("%s does not round-trip:\n--- got\n%s--- want\n%s", name, got, raw)
	}
	return v
}

func TestGoldenFilesRoundTrip(t *testing.T) {
	t.Run("presence_parked", func(t *testing.T) { roundTrip[Presence](t, "presence_parked.json") })
	t.Run("presence_default", func(t *testing.T) { roundTrip[Presence](t, "presence_default.json") })
	t.Run("set_request", func(t *testing.T) { roundTrip[SetRequest](t, "set_request.json") })
	t.Run("status", func(t *testing.T) { roundTrip[Status](t, "status.json") })
	t.Run("error_conflict", func(t *testing.T) { roundTrip[Error](t, "error_conflict.json") })
	t.Run("actors", func(t *testing.T) { roundTrip[[]ActorView](t, "actors.json") })
}

func TestParkedPresenceDecodesToItsMeaning(t *testing.T) {
	p := roundTrip[Presence](t, "presence_parked.json")
	if p.Effective != StateParked || p.Default != StatePresent {
		t.Errorf("effective/default = %q/%q, want parked/present", p.Effective, p.Default)
	}
	if p.Override == nil || p.Override.Until == nil || p.Override.WakeOn == nil {
		t.Fatalf("override = %+v, want until and wake_on set", p.Override)
	}
	if want := time.Date(2026, 9, 23, 20, 0, 0, 0, time.UTC); !p.Override.Until.Equal(want) {
		t.Errorf("until = %v, want %v", p.Override.Until, want)
	}
	if !p.Override.WakeOn.AnyPlayerJoin || p.Override.Version != 3 {
		t.Errorf("wake_on/version = %+v/%d, want any_player_join and 3", p.Override.WakeOn, p.Override.Version)
	}
}

// Version 0 is "I expect no override", so it must be sent rather than
// omitted: a request without it would be a different request.
func TestSetRequestAlwaysCarriesItsVersion(t *testing.T) {
	b, err := json.Marshal(SetRequest{State: StateParked, Reason: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"version":0`)) {
		t.Errorf("encoded %s, want an explicit version 0", b)
	}
}

func TestStateValid(t *testing.T) {
	for s, want := range map[State]bool{StatePresent: true, StateParked: true, "": false, "Parked": false, "gone": false} {
		if got := s.Valid(); got != want {
			t.Errorf("State(%q).Valid() = %v, want %v", s, got, want)
		}
	}
}
