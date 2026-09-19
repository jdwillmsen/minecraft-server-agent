package joinprobe

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"

	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

func TestRunProbesImmediatelyAndRepeats(t *testing.T) {
	var probes atomic.Int32
	s := newFakeServer(t, samplePong, func(int32) []packet.Packet {
		probes.Add(1)
		return []packet.Packet{&packet.NetworkSettings{}}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		Run(ctx, s.addr(), 0, 50*time.Millisecond, logging.New("error"))
		close(done)
	}()

	// The first probe must not wait for the first tick: a process that has
	// just started and published nothing looks exactly like one that cannot
	// reach the server.
	deadline := time.After(3 * time.Second)
	for probes.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("only %d probes in 3s, want the immediate one plus repeats", probes.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}

	if got := testutil.ToFloat64(joinable); got != 1 {
		t.Errorf("joinable = %v after probing a healthy server, want 1", got)
	}
}
