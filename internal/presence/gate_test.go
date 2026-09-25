package presence

import "testing"

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestGateClosesItsChannelOnlyOnAChange(t *testing.T) {
	g := NewGate(true)
	present, ch := g.Wanted()
	if !present || ch == nil {
		t.Fatalf("Wanted() = %v, %v; want present and a channel that can close", present, ch)
	}
	g.Set(true)
	if closed(ch) {
		t.Error("setting the same answer closed the channel")
	}
	g.Set(false)
	if !closed(ch) {
		t.Fatal("a change did not close the channel")
	}
	present, next := g.Wanted()
	if present || closed(next) {
		t.Errorf("after the change Wanted() = %v with a closed channel %v; want parked and a fresh channel", present, closed(next))
	}
}
