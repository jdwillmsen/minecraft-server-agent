// Package mcauth handles Xbox Live device-code authentication and caches
// the resulting token to disk so the agent doesn't need an interactive
// login on every restart - the same problem minecraft-afk-bot solves with
// prismarine-auth's profilesFolder, adapted to gophertunnel's auth package.
package mcauth

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/sandertv/gophertunnel/minecraft/auth"
	"golang.org/x/oauth2"
)

const tokenFileMode = 0o600

// tokenSlugLimit bounds the readable part of a cache filename so a long
// username can't push the path past a filesystem's name limit.
const tokenSlugLimit = 32

// requestLiveToken is the interactive device-code login. A package-level
// var, not a direct call, so tests can substitute a stub and prove
// TokenSource only reaches it on a genuinely absent cache file - never on a
// corrupt one (see the loadToken error handling below).
var requestLiveToken = auth.RequestLiveTokenContext

// TokenSource returns an oauth2.TokenSource backed by a token cached under
// cacheDir for the account named by username.
//
// If no cached token file exists yet, it performs an interactive
// device-code login, writing the code and URL to out - which in a container
// is stdout, so the instructions land in the pod's logs exactly like
// minecraft-afk-bot's device_code_required event does today. Any other load
// failure (a corrupt file, a permissions problem, a transient I/O error) is
// a hard error instead: falling through to an interactive login on those
// would silently block reconnect attempts for up to ~15 minutes waiting on
// a device code nobody is watching for, every time.
//
// Every subsequent refresh is persisted back to cacheDir, so a later
// restart resumes without a fresh login as long as the refresh token is
// still valid. Each username gets its own cache file, so several accounts
// can share one cacheDir volume without clobbering each other's tokens.
func TokenSource(ctx context.Context, cacheDir, username string, out io.Writer) (oauth2.TokenSource, error) {
	name, err := tokenFileName(username)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("mcauth: create cache dir: %w", err)
	}
	path := filepath.Join(cacheDir, name)

	tok, err := loadToken(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("mcauth: load cached token: %w", err)
		}
		tok, err = requestLiveToken(ctx, out)
		if err != nil {
			return nil, fmt.Errorf("mcauth: device-code login: %w", err)
		}
		if err := saveToken(path, tok); err != nil {
			return nil, fmt.Errorf("mcauth: save token: %w", err)
		}
	}

	base := auth.RefreshTokenSourceWriter(tok, out)
	return &cachingTokenSource{path: path, inner: base}, nil
}

// cachingTokenSource wraps another oauth2.TokenSource and persists every
// token it returns, so a background refresh doesn't get lost on restart.
//
// gophertunnel may call Token() concurrently with its own background
// refresh goroutine, so access to inner and the cache file is serialised by
// mu rather than relying on inner's own thread-safety for the write side.
type cachingTokenSource struct {
	mu    sync.Mutex
	path  string
	inner oauth2.TokenSource
}

func (c *cachingTokenSource) Token() (*oauth2.Token, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	tok, err := c.inner.Token()
	if err != nil {
		return nil, err
	}
	// Best-effort: a failed cache write shouldn't fail the connection, but
	// it does mean the next restart re-authenticates.
	_ = saveToken(c.path, tok)
	return tok, nil
}

// tokenFileName derives the per-account cache filename for username. The
// username reaches this function from the environment and ends up in a
// filesystem path, so only a conservative slug of it is used; a hash of the
// full value is appended so two usernames that slug identically still get
// distinct cache files.
func tokenFileName(username string) (string, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return "", errors.New("mcauth: username must not be empty")
	}

	slug := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, username)
	if len(slug) > tokenSlugLimit {
		slug = slug[:tokenSlugLimit]
	}

	sum := sha256.Sum256([]byte(username))
	return fmt.Sprintf("token-%s-%x.json", slug, sum[:4]), nil
}

// loadToken reads and validates the cached token at path. A missing file
// returns an error satisfying errors.Is(err, fs.ErrNotExist); every other
// failure (malformed JSON, no refresh token) is a distinct error, so callers
// can tell "nothing cached yet" apart from "something is wrong with what's
// cached" and react differently (see TokenSource above).
func loadToken(path string) (*oauth2.Token, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tok oauth2.Token
	if err := json.Unmarshal(data, &tok); err != nil {
		return nil, fmt.Errorf("mcauth: cached token at %s is not valid JSON: %w", path, err)
	}
	if tok.RefreshToken == "" {
		return nil, fmt.Errorf("mcauth: cached token at %s has no refresh token", path)
	}
	return &tok, nil
}

// saveToken writes tok to path atomically: it writes to a uniquely-named
// temporary file in the same directory, flushes it to stable storage, then
// renames it over path. Readers of path therefore see either the previous
// token or the new one, never a partially-written (and so unparseable)
// file, even when several processes share one cache directory.
func saveToken(path string, tok *oauth2.Token) error {
	data, err := json.Marshal(tok)
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".token-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	if err := tmp.Chmod(tokenFileMode); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
