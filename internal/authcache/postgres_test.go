package authcache

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"

	"github.com/jdwillmsen/minecraft-server-agent/pkg/mcauth"
)

// deadPool is a pool aimed at a port nothing is listening on, which is what a
// database that is down looks like from inside this process: pgxpool connects
// lazily, so the failure lands on the first statement rather than here.
func deadPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := listener.Addr().(*net.TCPAddr)
	if err := listener.Close(); err != nil {
		t.Fatalf("release the port: %v", err)
	}

	dsn := fmt.Sprintf("postgres://agent@127.0.0.1:%d/agent?sslmode=disable&connect_timeout=2", addr.Port)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// The agent used to boot from the file cache when the database was down and
// play through the outage. Reporting an unreachable database as an ordinary
// error instead of as an unavailable store takes that away: mcauth.Fallback
// refuses to read past anything else, and the process exits.
func TestLoadAgainstADatabaseThatIsDownIsUnavailableNotAPlainError(t *testing.T) {
	s := NewPostgres(deadPool(t), "agent-one")

	_, err := s.Load(context.Background())
	if err == nil {
		t.Fatal("Load against a dead database succeeded")
	}
	if !errors.Is(err, mcauth.ErrStoreUnavailable) {
		t.Errorf("Load = %v, want it to carry ErrStoreUnavailable", err)
	}
	// And never as an empty store, which is the one load failure that
	// licenses a device-code login.
	if errors.Is(err, mcauth.ErrNoToken) {
		t.Error("a database that is down was reported as an empty store")
	}
}

func TestSaveAgainstADatabaseThatIsDownIsUnavailableNotAPlainError(t *testing.T) {
	s := NewPostgres(deadPool(t), "agent-one")

	err := s.Save(context.Background(), &oauth2.Token{AccessToken: "a", RefreshToken: "r"})
	if err == nil {
		t.Fatal("Save against a dead database succeeded")
	}
	// Fallback only writes through to the file for this error, so a rotated
	// token is persisted nowhere unless the classification is right.
	if !errors.Is(err, mcauth.ErrStoreUnavailable) {
		t.Errorf("Save = %v, want it to carry ErrStoreUnavailable", err)
	}
}

// The whole agent behind the seam: a database that is down leaves the file
// cache answering, which is what keeps the agent in the game through a blip.
func TestADatabaseThatIsDownFallsThroughToTheFileCache(t *testing.T) {
	ctx := context.Background()
	file, err := mcauth.NewFileStore(t.TempDir(), "agent-one")
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := file.Save(ctx, &oauth2.Token{AccessToken: "a", RefreshToken: "on-the-volume"}); err != nil {
		t.Fatalf("file Save: %v", err)
	}

	got, err := mcauth.NewFallback(NewPostgres(deadPool(t), "agent-one"), file).Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.RefreshToken != "on-the-volume" {
		t.Errorf("RefreshToken = %q, want the file's", got.RefreshToken)
	}
}
