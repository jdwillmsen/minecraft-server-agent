//go:build livedb

// Exercises the real SQL against a real PostgreSQL, as the runtime role.
//
// Everything this store does is one row, so there is little logic to unit
// test and a great deal that only a real server can answer: whether the
// table exists, whether the runtime role was granted it, and whether two
// connections reading and writing the same row behave the way a live agent
// and its standby need them to. The last of those is the whole point of
// moving the cache off the pod's volume, and it is untestable anywhere else.
//
// Behind a build tag because it needs a database. Run it with:
//
//	MC_TEST_DSN=postgres://app:...@127.0.0.1:55432/jdwillmsen_prd go test -tags livedb ./internal/authcache/
package authcache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"

	"github.com/jdwillmsen/minecraft-server-agent/pkg/mcauth"
)

func livePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("MC_TEST_DSN")
	if dsn == "" {
		t.Skip("MC_TEST_DSN unset")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// testAccount keys every row this file writes to the test that wrote it, so
// a run never touches the real agent's cached token in a database the other
// live suites share.
func testAccount(t *testing.T) string {
	t.Helper()
	return "__test-" + t.Name()
}

func liveStore(t *testing.T, pool *pgxpool.Pool) *Postgres {
	t.Helper()
	account := testAccount(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM `+Table+` WHERE account = $1`, account)
	})
	return NewPostgres(pool, account)
}

func TestLoadWithNothingStoredIsErrNoToken(t *testing.T) {
	s := liveStore(t, livePool(t))
	if _, err := s.Load(context.Background()); !errors.Is(err, mcauth.ErrNoToken) {
		t.Fatalf("Load of an unknown account = %v, want ErrNoToken", err)
	}
}

func TestSaveThenLoadRoundTripsEveryFieldThatMatters(t *testing.T) {
	ctx := context.Background()
	s := liveStore(t, livePool(t))

	want := liveToken("refresh")
	if err := s.Save(ctx, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken || got.TokenType != want.TokenType {
		t.Errorf("round trip gave %+v, want %+v", got, want)
	}
}

func TestSaveReplacesRatherThanAccumulating(t *testing.T) {
	ctx := context.Background()
	pool := livePool(t)
	s := liveStore(t, pool)

	for _, refresh := range []string{"r1", "r2", "r3"} {
		if err := s.Save(ctx, liveToken(refresh)); err != nil {
			t.Fatalf("Save %s: %v", refresh, err)
		}
	}

	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+Table+` WHERE account = $1`, testAccount(t)).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Errorf("account holds %d rows, want 1: a rotating token must not accumulate copies", rows)
	}
	got, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.RefreshToken != "r3" {
		t.Errorf("RefreshToken = %q, want the last one written", got.RefreshToken)
	}
}

// The reason this store exists: two processes, on two nodes, reading and
// writing the same account's token at the same time. A file on a
// ReadWriteOnce volume cannot do this at all -- the second pod never even
// mounts it.
func TestLiveAgentWritesAndStandbyReadsTheSameRow(t *testing.T) {
	ctx := context.Background()
	pool := livePool(t)
	account := testAccount(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM `+Table+` WHERE account = $1`, account)
	})

	liveAgent := NewPostgres(pool, account)
	standby := NewPostgres(pool, account)

	if err := liveAgent.Save(ctx, liveToken("rotated-by-the-leader")); err != nil {
		t.Fatalf("live agent Save: %v", err)
	}
	got, err := standby.Load(ctx)
	if err != nil {
		t.Fatalf("standby Load: %v", err)
	}
	if got.RefreshToken != "rotated-by-the-leader" {
		t.Errorf("standby read %q, want what the live agent wrote", got.RefreshToken)
	}
}

func TestConcurrentReadersAndWriterNeverSeeAPartialToken(t *testing.T) {
	ctx := context.Background()
	pool := livePool(t)
	account := testAccount(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM `+Table+` WHERE account = $1`, account)
	})

	writer := NewPostgres(pool, account)
	if err := writer.Save(ctx, liveToken("r0")); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	const rounds = 20
	var wg sync.WaitGroup
	errCh := make(chan error, 2*rounds)

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			tok := liveToken(fmt.Sprintf("r-%d", i))
			if err := writer.Save(ctx, tok); err != nil {
				errCh <- err
				return
			}
		}
	}()

	reader := NewPostgres(pool, account)
	for i := 0; i < rounds; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Every read must produce a decodable token with a refresh
			// token in it. A torn write would surface here as a decode
			// failure, which is exactly what DecodeToken refuses.
			if _, err := reader.Load(ctx); err != nil {
				errCh <- err
			}
		}()
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent access: %v", err)
	}
}

// A token written by the file store has to load out of this one unchanged,
// or moving the cache off the volume would cost an interactive login.
func TestAcceptsATokenEncodedByAnotherStore(t *testing.T) {
	ctx := context.Background()
	pool := livePool(t)
	account := testAccount(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM `+Table+` WHERE account = $1`, account)
	})

	file, err := mcauth.NewFileStore(t.TempDir(), "agent-one")
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	want := liveToken("r1")
	if err := file.Save(ctx, want); err != nil {
		t.Fatalf("file Save: %v", err)
	}
	onDisk, err := file.Load(ctx)
	if err != nil {
		t.Fatalf("file Load: %v", err)
	}

	pg := NewPostgres(pool, account)
	if err := pg.Save(ctx, onDisk); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := pg.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken {
		t.Errorf("token crossed stores as %+v, want %+v", got, want)
	}
}

// Released ahead of the migration is a real state, and it has to report
// itself as the store being unavailable rather than as the store being
// empty -- an empty store is licence to print a device code.
func TestAMissingTableReportsItselfUnavailableNotEmpty(t *testing.T) {
	ctx := context.Background()
	pool := livePool(t)
	missing := &Postgres{pool: pool, account: "__test-missing"}
	// Aimed at a relation nothing creates, which is what the agent's own
	// statement hits before its migration lands.
	missing.table = "minecraft.auth_tokens_not_migrated"

	_, err := missing.Load(ctx)
	if !errors.Is(err, mcauth.ErrStoreUnavailable) {
		t.Fatalf("Load against a missing table = %v, want ErrStoreUnavailable", err)
	}
	if errors.Is(err, mcauth.ErrNoToken) {
		t.Error("a missing table reported itself as an empty store")
	}

	err = missing.Save(ctx, liveToken("r"))
	if !errors.Is(err, mcauth.ErrStoreUnavailable) {
		t.Fatalf("Save against a missing table = %v, want ErrStoreUnavailable", err)
	}
}

// Nothing in the schema stopped an older token replacing a newer one, and two
// processes writing this row is a designed state rather than a Kubernetes
// fault: leadership can be forced when the lock holder is gone without having
// released it. A write that would replace a row this process never read is
// refused, so the writer can take what is there instead of retiring the
// credential the other process is playing on.
func TestSaveRefusesToReplaceARowThisProcessNeverRead(t *testing.T) {
	ctx := context.Background()
	pool := livePool(t)
	account := testAccount(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM `+Table+` WHERE account = $1`, account)
	})

	leader := NewPostgres(pool, account)
	forced := NewPostgres(pool, account)

	if err := leader.Save(ctx, liveToken("r1")); err != nil {
		t.Fatalf("leader Save: %v", err)
	}
	if _, err := forced.Load(ctx); err != nil {
		t.Fatalf("forced Load: %v", err)
	}
	if err := leader.Save(ctx, liveToken("r2")); err != nil {
		t.Fatalf("leader rotation: %v", err)
	}

	if err := forced.Save(ctx, liveToken("r3")); !errors.Is(err, mcauth.ErrStoreConflict) {
		t.Fatalf("Save over a row written since = %v, want ErrStoreConflict", err)
	}
	got, err := leader.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.RefreshToken != "r2" {
		t.Errorf("the row holds %q, want the newer r2 left alone", got.RefreshToken)
	}

	// And re-reading is what entitles it to write: the writer reacts rather
	// than being locked out of its own store.
	if _, err := forced.Load(ctx); err != nil {
		t.Fatalf("forced reload: %v", err)
	}
	if err := forced.Save(ctx, liveToken("r3")); err != nil {
		t.Fatalf("Save after re-reading: %v", err)
	}
}

// The same guard on the row that is not there yet: a process whose load
// answered "nothing stored" -- the cold start, and the migration window --
// must not overwrite a row that appeared since.
func TestSaveIntoARowAnotherProcessCreatedIsAConflict(t *testing.T) {
	ctx := context.Background()
	pool := livePool(t)
	account := testAccount(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM `+Table+` WHERE account = $1`, account)
	})

	first := NewPostgres(pool, account)
	second := NewPostgres(pool, account)
	if _, err := second.Load(ctx); !errors.Is(err, mcauth.ErrNoToken) {
		t.Fatalf("Load of an empty row = %v, want ErrNoToken", err)
	}

	if err := first.Save(ctx, liveToken("r1")); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if err := second.Save(ctx, liveToken("r2")); !errors.Is(err, mcauth.ErrStoreConflict) {
		t.Fatalf("Save into a row created since = %v, want ErrStoreConflict", err)
	}
	got, err := first.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.RefreshToken != "r1" {
		t.Errorf("the row holds %q, want the first writer's r1", got.RefreshToken)
	}
}

// liveToken is a token with the fields this store round-trips and an access
// half that is still good, since a stored token with no expiry would never be
// refreshed by either role.
func liveToken(refresh string) *oauth2.Token {
	return &oauth2.Token{
		AccessToken:  "access",
		RefreshToken: refresh,
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(time.Hour),
	}
}
