//go:build livedb

package store

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// A brand-new player can reach RecordJoin (a dispatch goroutine) and
// ResumeSession (the read loop) at the same moment. Each decides "first time
// seen" by looking for an earlier session, so without serializing them both
// can find none and operators are told twice. Exactly one must win.
func TestFirstSightIsDecidedOnceUnderConcurrency(t *testing.T) {
	pg := liveStore(t)
	ctx := t.Context()
	base := time.Now().UnixNano() % 1_000_000_000

	for i := range 40 {
		xuid := fmt.Sprintf("25354%010d", base+int64(i))
		at := time.Now().UTC()

		var (
			wg      sync.WaitGroup
			start   = make(chan struct{})
			joined  Profile
			resumed bool
			errJoin error
			errRes  error
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			joined, errJoin = pg.RecordJoin(ctx, xuid, "Racer", at)
		}()
		go func() {
			defer wg.Done()
			<-start
			resumed, errRes = pg.ResumeSession(ctx, xuid, "Racer", at)
		}()
		close(start)
		wg.Wait()

		if errJoin != nil || errRes != nil {
			t.Fatalf("%s: join err %v, resume err %v", xuid, errJoin, errRes)
		}
		firsts := 0
		if joined.Sessions == 0 {
			firsts++
		}
		if resumed {
			firsts++
		}
		if firsts != 1 {
			t.Fatalf("%s: %d first sightings (join saw %d prior sessions, resume first=%v), want exactly 1", xuid, firsts, joined.Sessions, resumed)
		}
	}
}
