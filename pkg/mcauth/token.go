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

// storeTimeout bounds one attempt to read or write the store from inside
// Token, which has no context of its own. Generous for a single row or a
// single file, and short relative to the hour an access token lasts, so a
// store that has gone away costs a refresh rather than the connection.
const storeTimeout = 5 * time.Second

// ErrStandbyUnwarmed means this process is not the live agent, what it
// loaded has expired, and the store holds nothing newer -- so it holds no
// usable token and may not refresh one into existence.
//
// The ordinary state of a standby that started more than an access token's
// lifetime after the live agent last rotated, not a failure: the live agent
// writes when the credential rotates, which a stable connection can go hours
// without doing. It costs the handover the one refresh the warm-up hoped to
// save, and it is reported so a reader can tell it from a store that broke.
var ErrStandbyUnwarmed = errors.New("mcauth: standby holds no unexpired token and may not refresh one")

// requestLiveToken is the interactive device-code login. A package-level
// var, not a direct call, so tests can substitute a stub and prove it is
// reached only on a genuinely empty store - never on one that is merely
// unreadable - and only by a process the gate says holds the login.
var requestLiveToken = auth.RequestLiveTokenContext

// Option configures a TokenSource.
type Option func(*cachingTokenSource)

// WithLiveGate makes every use of the shared login conditional on live
// returning true: refreshing the cached token, persisting the result, and the
// first-run device-code login.
//
// This is how a warm standby stays harmless. Microsoft retires a refresh
// token the moment it issues the replacement, so the damage a second process
// does is done by the refresh itself and not by storing it -- a gate on the
// write alone would suppress the copy and leave the live agent holding a
// credential that has already been revoked. A standby therefore rotates
// nothing and reads the live agent's work out of the store instead; see
// Token.
//
// The gate is consulted per call rather than once at construction because a
// standby becomes the live agent without rebuilding anything.
func WithLiveGate(live func() bool) Option {
	return func(c *cachingTokenSource) {
		if live != nil {
			c.live = live
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
// A store that holds nothing for this account yet is not an error here: the
// device-code login it calls for is deferred to the first Token call made by
// a process the gate says may hold the login, because a standby that printed
// a code would print it into a pod log nobody is watching and block for as
// long as the code lasts. Any other load failure (a corrupt entry, a store
// that cannot be reached, a transient I/O error) is a hard error instead,
// for the same reason stated the other way round: those must never be
// mistaken for an empty store and answered with a prompt.
//
// ctx bounds the initial load and, later, that deferred login, so a process
// asked to shut down while it waits on a device code stops waiting.
//
// Every refresh is persisted back to the store, subject to WithLiveGate, so
// a later restart resumes without a fresh login as long as the refresh token
// is still valid.
func TokenSource(ctx context.Context, store Store, out io.Writer, opts ...Option) (oauth2.TokenSource, error) {
	if store == nil {
		return nil, errors.New("mcauth: nil token store")
	}

	tok, err := store.Load(ctx)
	if err != nil && !errors.Is(err, ErrNoToken) {
		return nil, fmt.Errorf("mcauth: load cached token: %w", err)
	}

	c := &cachingTokenSource{
		store:    store,
		out:      out,
		loginCtx: ctx,
		live:     func() bool { return true },
	}
	if tok != nil {
		c.adopt(tok)
	}
	for _, opt := range opts {
		opt(c)
	}
	// A process that starts as a standby reads the store again before it
	// rotates anything, however long it stands by first: what was loaded
	// here is the live agent's token, and the live agent goes on rotating it.
	c.mustReload = !c.live()
	return c, nil
}

// cachingTokenSource holds one account's token for one process and persists
// every rotation of it, so a background refresh doesn't get lost on restart.
//
// gophertunnel may call Token() concurrently with its own background refresh
// goroutine, so access to inner, to held and to the store is serialised by mu
// rather than relying on inner's own thread-safety for the write side.
type cachingTokenSource struct {
	mu    sync.Mutex
	store Store
	out   io.Writer
	// loginCtx bounds the deferred device-code login, the one call here that
	// blocks for minutes rather than milliseconds. Held on the struct
	// because oauth2.TokenSource gives Token() no context to inherit and
	// the login no longer happens at construction, where ctx was in scope.
	loginCtx context.Context
	// inner refreshes held, and is rebuilt whenever held is replaced by a
	// token this process did not derive from the previous one. nil until
	// there is a token at all, which is the cold start.
	inner oauth2.TokenSource
	held  *oauth2.Token
	live  func() bool
	log   *logging.Logger
	// saved is the refresh token this process has written, and starts empty
	// even though the store was just read: what was loaded is not
	// necessarily what the store the agent writes to holds. A token read
	// through a Fallback came from the file the cluster is moving away from,
	// and the one write that starting empty costs is what copies it into the
	// row. It is only ever that copy: a process that stood by first seeds
	// this from the store before it rotates anything, so an empty saved can
	// no longer flush a superseded token over a newer one -- see reload.
	saved string
	// mustReload records that the store may hold a token newer than the one
	// this process is holding: it stood by while another process was live,
	// or a write of its own lost to one. Cleared by the reload the next live
	// call performs.
	mustReload bool
}

// Token returns a usable token, doing only what this process is entitled to
// do to get one.
//
// The live agent holds the account's login and so may rotate it: it
// refreshes, or on a cold start logs in, and persists the result. A standby
// holds nothing and may rotate nothing -- see standbyToken.
func (c *cachingTokenSource) Token() (*oauth2.Token, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.live() {
		// Another process is rotating the account's token while this one
		// stands by, so anything this one holds may be superseded before it
		// is allowed to use it.
		c.mustReload = true
		return c.standbyToken()
	}
	if c.mustReload {
		if err := c.reload(); err != nil {
			// A token that may have been superseded is still one this
			// process can dial with; what it may not do is rotate from it,
			// since refreshing a token Microsoft has already retired is what
			// costs the account its login. The connect loop calls Token per
			// dial, so the reload retries within seconds.
			if c.held.Valid() {
				return c.held, nil
			}
			return nil, err
		}
	}

	tok, err := c.liveToken()
	if err != nil {
		return nil, err
	}
	c.held = tok
	if tok.RefreshToken == c.saved {
		return tok, nil
	}
	// Bounded and detached: oauth2 gives Token() no context to inherit, and
	// an unbounded write against an unreachable database would hold the
	// mutex that every dial and every background refresh waits on.
	saveCtx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	// Best-effort: a failed cache write shouldn't fail the connection, but
	// it does mean the next restart re-authenticates.
	if err := c.store.Save(saveCtx, tok); err != nil {
		if errors.Is(err, ErrSavedToFallback) {
			// Durable, but not where the next load prefers to look. Leaving
			// saved untouched is what retries the primary: the connect loop
			// calls Token per dial, so the row catches up in seconds rather
			// than at the end of this access token's life.
			c.note(func() { c.log.Info("auth_token_written_to_fallback", nil) })
			return tok, nil
		}
		if errors.Is(err, ErrStoreConflict) {
			// Another process wrote the account's row, so it holds the
			// login this one was rotating. Taking its token back is the
			// next call's job -- see reload.
			c.mustReload = true
			c.note(func() { c.log.Info("auth_token_write_superseded", nil) })
			return tok, nil
		}
		c.note(func() { c.log.Error("auth_token_write_failed", logging.Fields{"error": err.Error()}) })
		return tok, nil
	}
	c.saved = tok.RefreshToken
	c.note(func() { c.log.Info("auth_token_written", nil) })
	return tok, nil
}

// reload takes what the store holds before this process rotates anything.
//
// The gate opens on a process that has been standing by, holding the token it
// last read. The agent that was live has rotated the account since, and
// Microsoft retired that copy as it issued the replacement: refreshing from
// it fails, and writing it back leaves the account's only stored credential
// dead and the next restart loading it too.
//
// An empty store is not a failure here. That is the cold start, and the
// migration window where the row is empty and the load was answered by the
// file behind it -- leaving saved empty is what copies that file into the row
// on the first write.
func (c *cachingTokenSource) reload() error {
	loadCtx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()

	tok, err := c.store.Load(loadCtx)
	switch {
	case errors.Is(err, ErrNoToken):
	case err != nil:
		return fmt.Errorf("mcauth: reload before rotating: %w", err)
	default:
		if c.held == nil || tok.RefreshToken != c.held.RefreshToken {
			c.adopt(tok)
			c.note(func() { c.log.Info("auth_token_adopted_from_store", nil) })
		}
		c.saved = tok.RefreshToken
	}
	c.mustReload = false
	return nil
}

// liveToken refreshes the held token, or performs the first-run device-code
// login when there is none to refresh.
//
// The login lands here rather than at construction because this is the first
// moment the process is known to be the one entitled to it, and the account
// allows one login at a time: two pods prompting independently produce two
// grants, of which the stored one is not necessarily the one either process
// is using.
func (c *cachingTokenSource) liveToken() (*oauth2.Token, error) {
	if c.inner != nil {
		tok, err := c.inner.Token()
		if err == nil {
			return tok, nil
		}
		return c.reloadAfterFailedRefresh(err)
	}
	c.note(func() { c.log.Info("auth_device_code_login", nil) })
	tok, err := requestLiveToken(c.loginCtx, c.out)
	if err != nil {
		return nil, fmt.Errorf("mcauth: device-code login: %w", err)
	}
	c.adopt(tok)
	return tok, nil
}

// standbyToken answers without rotating anything.
//
// The token this process loaded stays usable until its access token expires.
// Past that, refreshing is not an option a standby has: Microsoft retires the
// refresh token as it issues the replacement, so a standby that refreshed
// would revoke the credential the live agent is holding the game with and
// leave neither process able to reconnect. What it does instead is re-read
// the store, because the live agent persists every rotation -- staying warm
// on the other process's work rather than on work of its own.
//
// A store that has nothing newer leaves this process unwarmed, which is
// reported rather than worked around. It costs the handover one refresh; the
// alternative costs the account its login.
func (c *cachingTokenSource) standbyToken() (*oauth2.Token, error) {
	if c.held.Valid() {
		return c.held, nil
	}
	loadCtx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	tok, err := c.store.Load(loadCtx)
	switch {
	case errors.Is(err, ErrNoToken):
		return nil, ErrStandbyUnwarmed
	case err != nil:
		return nil, fmt.Errorf("mcauth: standby reload: %w", err)
	case !tok.Valid():
		// Adopting would point the refresher at this token, and a standby
		// that already holds one reaches here only because what it holds is
		// no fresher -- so taking the reload over would leave the process
		// rotating from whichever of the two is older.
		//
		// A process holding nothing is the exception: an expired token is
		// still a refresh token, and having one to rotate from on promotion
		// beats having none at all.
		if c.held == nil {
			c.adopt(tok)
		}
		return nil, ErrStandbyUnwarmed
	}
	// Adopting rebuilds the refresher, so on this path -- reached only once
	// what is held has expired -- the single thing it can still change is
	// which credential a later rotation starts from. Only an unexpired
	// reload earns that, which the switch above has already established: a
	// store answering at all does not make its answer the newer one, and a
	// fallback reaching past an unreachable database returns the copy the
	// migration left behind.
	c.adopt(tok)
	c.note(func() { c.log.Info("auth_token_standby_reloaded", nil) })
	return tok, nil
}

// reloadAfterFailedRefresh reads the store again, once, when a refresh has
// just been rejected.
//
// The refresh token this process holds is not always the account's current
// one. A load that fell through to the file cache because the database could
// not be reached answers with whatever the volume still holds, and that copy
// stopped being current the first time the live agent rotated it: Microsoft
// rejects it, and goes on rejecting it for as long as this process lives,
// while the database that has since come back holds one that works. A
// rejected refresh is therefore the moment to look at the store rather than
// the moment to give up -- the connect loop's backoff would otherwise retry
// the same dead credential forever.
//
// A store that cannot answer, or that answers with the refresh token that
// was just rejected, has nothing to add, and the original failure stands.
func (c *cachingTokenSource) reloadAfterFailedRefresh(refreshErr error) (*oauth2.Token, error) {
	rejected := ""
	if c.held != nil {
		rejected = c.held.RefreshToken
	}
	loadCtx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	tok, err := c.store.Load(loadCtx)
	if err != nil || tok.RefreshToken == rejected {
		return nil, refreshErr
	}
	c.note(func() { c.log.Info("auth_token_reloaded_after_failed_refresh", nil) })
	c.adopt(tok)
	if tok.Valid() {
		return tok, nil
	}
	return c.inner.Token()
}

// adopt makes tok the token this process holds, rebuilding the refresher
// around it: inner keeps its own copy, so replacing held without this would
// leave the next refresh working from the token it superseded.
func (c *cachingTokenSource) adopt(tok *oauth2.Token) {
	c.held = tok
	c.inner = auth.RefreshTokenSourceWriter(tok, c.out)
}

// note runs emit only when a logger was configured. A closure rather than a
// nil-checked logger at each call site because every one of them builds
// fields that are pure waste when nothing is listening.
func (c *cachingTokenSource) note(emit func()) {
	if c.log != nil {
		emit()
	}
}
