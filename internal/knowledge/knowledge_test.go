package knowledge

import (
	"context"
	"testing"
)

func TestNormalizeTopic(t *testing.T) {
	cases := map[string]string{
		"Gold Farm":   "gold farm",
		"  RULES  ":   "rules",
		"Nether\tHub": "nether hub",
		"":            "",
	}
	for in, want := range cases {
		if got := NormalizeTopic(in); got != want {
			t.Errorf("NormalizeTopic(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNopAnswersNothingKnown(t *testing.T) {
	var s Store = Nop{}
	if s.Enabled() {
		t.Fatal("Nop reports itself enabled")
	}
	entries, err := s.Lookup(context.Background(), "anything", 3)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("Lookup returned %d entries, want 0", len(entries))
	}
	if _, found, err := s.Get(context.Background(), "rules"); err != nil || found {
		t.Fatalf("Get = (found %v, err %v), want (false, nil)", found, err)
	}
	if err := s.Upsert(context.Background(), "rules", "be nice", "xuid"); err != nil {
		t.Fatalf("Upsert on Nop must not error: %v", err)
	}
}
