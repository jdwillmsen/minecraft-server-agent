package waypoints

import (
	"context"
	"errors"
	"testing"
)

func TestNormalizeDimension(t *testing.T) {
	ok := map[string]string{
		"":           "overworld",
		"Overworld":  "overworld",
		"NETHER":     "nether",
		"end":        "end",
		"the_nether": "nether",
		"the end":    "end",
	}
	for in, want := range ok {
		got, err := NormalizeDimension(in)
		if err != nil {
			t.Errorf("NormalizeDimension(%q) errored: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("NormalizeDimension(%q) = %q, want %q", in, got, want)
		}
	}

	if _, err := NormalizeDimension("moon"); !errors.Is(err, ErrUnknownDimension) {
		t.Errorf("NormalizeDimension(\"moon\") err = %v, want ErrUnknownDimension", err)
	}
}

func TestNopStoresNothing(t *testing.T) {
	var s Store = Nop{}
	if s.Enabled() {
		t.Fatal("Nop reports itself enabled")
	}
	if err := s.Set(context.Background(), "xuid", Waypoint{Name: "base"}); err != nil {
		t.Fatalf("Set on Nop must not error: %v", err)
	}
	if _, found, err := s.Get(context.Background(), "xuid", "base"); err != nil || found {
		t.Fatalf("Get = (found %v, err %v), want (false, nil)", found, err)
	}
}
