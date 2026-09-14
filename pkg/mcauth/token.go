// Package mcauth handles Xbox Live device-code authentication and caches
// the resulting token so the agent doesn't need an interactive login on
// every restart - the same problem minecraft-afk-bot solves with
// prismarine-auth's profilesFolder, adapted to gophertunnel's auth package.
//
// Where the token is cached is a Store, not a path: two agent processes
// coexist during a release, and only a cache both of them can read lets the
// standby finish its login before the handover. See store.go.
package mcauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/sandertv/gophertunnel/minecraft/auth"
	"golang.org/x/oauth2"

	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// saveTimeout bounds one persistence attempt. Generous for a single row or a
// single file, and short relative to the hour an access token lasts, so a
// store that has gone away costs a refresh rather than the connection.
const saveTimeout = 5 * time.Second

// requestLiveToken is the interactive device-code login. A package-level
// var, not a direct call, so tests can substitute a stub and prove
// TokenSource only reaches it on a genuinely empty store - never on one that
// is merely unreadable (see the Load error handling below).
var requestLiveToken = auth.RequestLiveTokenContext

// Option configures a TokenSource.
type Option func(*cachingTokenSource)

// WithWriteGate makes persistence conditional on allowed returning true at
// the moment a token is refreshed.
//
// This is how a warm standby stays harmless. Microsoft rotates the refresh
// token on every refresh, so two processes refreshing the same cached token
// invalidate each other's copy; the gate lets the caller say that only the
// live agent may write one back. The gate is consulted per refresh rather
// than once at construction because a standby becomes the live agent without
// rebuilding anything, and the token it refreshed while waiting is the token
// its first write must persist.
func WithWriteGate(allowed func() bool) Option {
	return func(c *cachingTokenSource) {
		if allowed != nil {
			c.allowed = allowed
		}
	}
}

// WithLogger reports each refresh, and each refresh deliberately not
// persisted, so a reader can tell from two pods' logs that both hold a valid
// token and only one of them is writing.
func WithLogger(l *logging.Logger) Option {
	return func(c *cachingTokenSource) { c.log = l }
}

// TokenSource returns an oauth2.TokenSource backed by the token cached in
// store for one account.
//
// If the store holds no token yet, it performs an interactive device-code
// login, writing the code and URL to out - which in a container is stdout,
// so the instructions land in the pod's logs exactly like
// minecraft-afk-bot's device_code_required event does today. Any other load
// failure (a corrupt entry, a store that cannot be reached, a transient I/O
// error) is a hard error instead: falling through to an interactive login on
// those would silently block reconnect attempts for up to ~15 minutes
// waiting on a device code nobody is watching for, every time.
//
// Every subsequent refresh is persisted back to the store, subject to
// WithWriteGate, so a later restart resumes without a fresh login as long as
// the refresh token is still valid.
func TokenSource(ctx context.Context, store Store, out io.Writer, opts ...Option) (oauth2.TokenSource, error) {
	if store == nil {
		return nil, errors.New("mcauth: nil token store")
	}

	tok, err := store.Load(ctx)
	if err != nil {
		if !errors.Is(err, ErrNoToken) {
			return nil, fmt.Errorf("mcauth: load cached token: %w", err)
		}
		tok, err = requestLiveToken(ctx, out)
		if err != nil {
			return nil, fmt.Errorf("mcauth: device-code login: %w", err)
		}
		if err := store.Save(ctx, tok); err != nil {
			return nil, fmt.Errorf("mcauth: save token: %w", err)
		}
	}

	c := &cachingTokenSource{
		store:   store,
		inner:   auth.RefreshTokenSourceWriter(tok, out),
		allowed: func() bool { return true },
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// cachingTokenSource wraps another oauth2.TokenSource and persists every
// token it returns, so a background refresh doesn't get lost on restart.
//
// gophertunnel may call Token() concurrently with its own background refresh
// goroutine, so access to inner and to the store is serialised by mu rather
// than relying on inner's own thread-safety for the write side.
type cachingTokenSource struct {
	mu      sync.Mutex
	store   Store
	inner   oauth2.TokenSource
	allowed func() bool
	log     *logging.Logger
	// saved is the refresh token this process has written, and starts empty
	// even though the store was just read: what was loaded is not
	// necessarily what the store the agent writes to holds. A token read
	// through a Fallback came from the file the cluster is moving away from,
	// and a token a standby refreshed while waiting was never written at
	// all. Starting empty costs one redundant write per process and makes
	// both of those land the moment this process is allowed to write.
	saved string
}

func (c *cachingTokenSource) Token() (*oauth2.Token, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	tok, err := c.inner.Token()
	if err != nil {
		return nil, err
	}
	if tok.RefreshToken == c.saved {
		return tok, nil
	}
	if !c.allowed() {
		c.note(func() { c.log.Info("auth_token_write_skipped", logging.Fields{"reason": "not the live agent"}) })
		return tok, nil
	}
	// Bounded and detached: oauth2 gives Token() no context to inherit, and
	// an unbounded write against an unreachable database would hold the
	// mutex that every dial and every background refresh waits on.
	saveCtx, cancel := context.WithTimeout(context.Background(), saveTimeout)
	defer cancel()
	// Best-effort: a failed cache write shouldn't fail the connection, but
	// it does mean the next restart re-authenticates.
	if err := c.store.Save(saveCtx, tok); err != nil {
		c.note(func() { c.log.Error("auth_token_write_failed", logging.Fields{"error": err.Error()}) })
		return tok, nil
	}
	c.saved = tok.RefreshToken
	c.note(func() { c.log.Info("auth_token_written", nil) })
	return tok, nil
}

// note runs emit only when a logger was configured. A closure rather than a
// nil-checked logger at each call site because every one of them builds
// fields that are pure waste when nothing is listening.
func (c *cachingTokenSource) note(emit func()) {
	if c.log != nil {
		emit()
	}
}
