package wiki

import (
	"context"
	"errors"
	"testing"
)

// Shaped exactly like plugin.Wiki, checked locally so this package need not
// import internal/plugin just to prove Nop satisfies it.
var _ interface {
	Lookup(ctx context.Context, topic, aspect string) (string, error)
	Enabled() bool
} = Nop{}

func TestNopIsDisabledAndUnavailable(t *testing.T) {
	var n Nop
	if n.Enabled() {
		t.Error("Nop reported enabled")
	}
	if _, err := n.Lookup(t.Context(), "torch", ""); !errors.Is(err, ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
}
