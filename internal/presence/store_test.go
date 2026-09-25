package presence

import (
	"errors"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

// With no database there is nothing to read an override from, so every call
// says so rather than answering "no overrides" -- which a caller would take
// for the truth and report every actor at its default.
func TestNopRefusesEveryCall(t *testing.T) {
	ctx := t.Context()
	var s Store = Nop{}
	if s.Enabled() {
		t.Error("Nop reports enabled")
	}
	checks := map[string]error{}
	_, checks["Overrides"] = s.Overrides(ctx)
	_, checks["Set"] = s.Set(ctx, "a", presenceapi.Override{}, 0)
	_, checks["SetMany"] = s.SetMany(ctx, map[string]presenceapi.Override{"a": {}})
	_, checks["Clear"] = s.Clear(ctx, []string{"a"})
	_, checks["Remove"] = s.Remove(ctx, "a", 1, time.Time{})
	_, checks["Statuses"] = s.Statuses(ctx)
	checks["PutStatus"] = s.PutStatus(ctx, "a", presenceapi.Status{})
	for name, err := range checks {
		if !errors.Is(err, ErrDisabled) {
			t.Errorf("%s = %v, want ErrDisabled", name, err)
		}
	}
}
