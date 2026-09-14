package mcauth

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/oauth2"
)

const tokenFileMode = 0o600

// tokenSlugLimit bounds the readable part of a cache filename so a long
// username can't push the path past a filesystem's name limit.
const tokenSlugLimit = 32

// ErrNoToken means the store is working and holds nothing for this account
// yet. It is the one load failure that may lead to an interactive
// device-code login; every other one is a hard error, because a container
// that prints a device code nobody is watching for blocks for as long as the
// code lasts.
var ErrNoToken = errors.New("mcauth: no cached token")

// ErrStoreUnavailable means the store itself cannot answer right now -- the
// table is not migrated, the role lacks the grant, the database is
// unreachable. Distinct from ErrNoToken because the two demand opposite
// reactions: nothing cached is a reason to log in, a store that cannot be
// read is a reason to stop and say so.
var ErrStoreUnavailable = errors.New("mcauth: token store unavailable")

// Store is where a refresh token lives between runs.
//
// The seam exists because the agent runs as two pods during a release, and a
// file on a ReadWriteOnce volume can only be mounted by one of them: the
// standby cannot pay its Xbox Live login before taking over if it cannot
// read the token at all. A store both pods reach is what makes a handover
// cost seconds instead of a fresh startup.
//
// Each Store is bound to one account when it is built, so Load and Save take
// no key: two accounts are two stores, which is what keeps a caller from
// reading one account's token with another's identity.
type Store interface {
	// Load returns the cached token, ErrNoToken if the store holds none for
	// this account, or ErrStoreUnavailable if it cannot say either way.
	Load(ctx context.Context) (*oauth2.Token, error)
	// Save replaces whatever the store holds for this account.
	Save(ctx context.Context, tok *oauth2.Token) error
}

// EncodeToken and DecodeToken are the on-the-wire form of a cached token,
// exported so every Store implementation agrees on it byte for byte. A token
// written by one store has to be readable by another: that is what makes
// moving the cache from a file to a database a copy rather than a
// re-authentication.
func EncodeToken(tok *oauth2.Token) ([]byte, error) {
	data, err := json.Marshal(tok)
	if err != nil {
		return nil, fmt.Errorf("mcauth: encode token: %w", err)
	}
	return data, nil
}

// DecodeToken parses an encoded token and rejects one that cannot be
// refreshed. A token with no refresh token would dial successfully until its
// access token expired and then fail forever, which is a worse failure than
// refusing it here.
func DecodeToken(data []byte) (*oauth2.Token, error) {
	var tok oauth2.Token
	if err := json.Unmarshal(data, &tok); err != nil {
		return nil, fmt.Errorf("mcauth: cached token is not valid JSON: %w", err)
	}
	if tok.RefreshToken == "" {
		return nil, errors.New("mcauth: cached token has no refresh token")
	}
	return &tok, nil
}

// FileStore caches the token in a file, one per account, under a directory.
//
// Kept for local development and for the first-run login, where a database
// is not necessarily configured at all. In the cluster it is the source side
// of the move off the volume rather than the store the agent writes to -- see
// Fallback.
type FileStore struct {
	path string
}

var _ Store = (*FileStore)(nil)

// NewFileStore binds a store to username's cache file under dir, creating
// dir if it does not exist.
//
// The directory is created here rather than at the first Save so that a
// cache directory the process cannot write to is a startup error, not a
// token silently lost on the first refresh hours later.
func NewFileStore(dir, username string) (*FileStore, error) {
	name, err := tokenFileName(username)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mcauth: create cache dir: %w", err)
	}
	return &FileStore{path: filepath.Join(dir, name)}, nil
}

// Path is the file this store reads and writes. Exported for the operator
// instructions in the README, which have to name a real path for someone
// recovering from a corrupt cache.
func (f *FileStore) Path() string { return f.path }

func (f *FileStore) Load(context.Context) (*oauth2.Token, error) {
	data, err := os.ReadFile(f.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNoToken
		}
		return nil, fmt.Errorf("mcauth: read %s: %w", f.path, err)
	}
	tok, err := DecodeToken(data)
	if err != nil {
		return nil, fmt.Errorf("%w (at %s)", err, f.path)
	}
	return tok, nil
}

// Save writes atomically: to a uniquely-named temporary file in the same
// directory, flushed to stable storage, then renamed over the target.
// Readers therefore see either the previous token or the new one, never a
// partially-written and so unparseable file, even when several processes
// share one cache directory.
func (f *FileStore) Save(_ context.Context, tok *oauth2.Token) error {
	data, err := EncodeToken(tok)
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(f.path), ".token-*")
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
	return os.Rename(tmpName, f.path)
}

// Fallback reads through to a second store when the first has nothing to
// give, and writes only to the first.
//
// It exists for the window in which the token is moving from the pod's
// volume into the database. The primary starts empty, so the first load
// comes from the file; the first refresh the live agent persists lands in the
// primary, and every load after that is answered there. Nobody has to read a
// refresh token out of a running pod and paste it somewhere to migrate it,
// which is a credential printed into a terminal that this avoids entirely.
//
// Writes deliberately never fall back. A token written to the secondary
// while the primary is merely unreachable would be the newer one, and the
// next load -- which prefers the primary -- would take the older, already
// rotated one and fail to refresh it. One writer, one truth: a primary that
// cannot be written to is a logged failure, not a second copy.
type Fallback struct {
	primary   Store
	secondary Store
}

var _ Store = (*Fallback)(nil)

func NewFallback(primary, secondary Store) *Fallback {
	return &Fallback{primary: primary, secondary: secondary}
}

func (f *Fallback) Load(ctx context.Context) (*oauth2.Token, error) {
	tok, err := f.primary.Load(ctx)
	switch {
	case err == nil:
		return tok, nil
	case errors.Is(err, ErrNoToken), errors.Is(err, ErrStoreUnavailable):
		// Both are "the primary has no answer": nothing stored yet, or a
		// database released ahead of the migration that gives it the table.
		// The second is the ordinary state during a rollout and must not
		// cost the agent a login it already has a token for.
		return f.secondary.Load(ctx)
	default:
		// A corrupt or refresh-token-less row is not a reason to reach past
		// it. Whatever is in the primary is what the agent would write back
		// to, and answering from the secondary would hide that.
		return nil, err
	}
}

func (f *Fallback) Save(ctx context.Context, tok *oauth2.Token) error {
	return f.primary.Save(ctx, tok)
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
