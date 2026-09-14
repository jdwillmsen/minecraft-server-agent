package mcauth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// memStore is a Store two roles can share, which is the property the whole
// change exists for: a file on a ReadWriteOnce volume cannot be one.
//
// failLoad and failSave are set by the tests that need a store which is
// present but cannot answer, since that is a different reaction from a store
// which is simply empty.
type memStore struct {
	mu       sync.Mutex
	token    []byte
	loads    int
	saves    int
	failLoad error
	failSave error
}

func (m *memStore) Load(context.Context) (*oauth2.Token, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loads++
	if m.failLoad != nil {
		return nil, m.failLoad
	}
	if m.token == nil {
		return nil, ErrNoToken
	}
	return DecodeToken(m.token)
}

func (m *memStore) Save(_ context.Context, tok *oauth2.Token) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saves++
	if m.failSave != nil {
		return m.failSave
	}
	data, err := EncodeToken(tok)
	if err != nil {
		return err
	}
	m.token = data
	return nil
}

func (m *memStore) seed(t *testing.T, tok *oauth2.Token) {
	t.Helper()
	if err := m.Save(context.Background(), tok); err != nil {
		t.Fatalf("seed: %v", err)
	}
	m.mu.Lock()
	m.saves = 0
	m.mu.Unlock()
}

func (m *memStore) counts() (loads, saves int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loads, m.saves
}

func fileStore(t *testing.T, dir, username string) *FileStore {
	t.Helper()
	fs, err := NewFileStore(dir, username)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	return fs
}

func TestFileStore_RoundTrip(t *testing.T) {
	ctx := context.Background()
	fs := fileStore(t, t.TempDir(), "agent-one")

	want := &oauth2.Token{
		AccessToken:  "access",
		RefreshToken: "refresh",
		Expiry:       time.Now().Add(time.Hour).Truncate(time.Second),
	}
	if err := fs.Save(ctx, want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := fs.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestFileStore_MissingFileIsErrNoToken(t *testing.T) {
	fs := fileStore(t, t.TempDir(), "agent-one")
	_, err := fs.Load(context.Background())
	if !errors.Is(err, ErrNoToken) {
		t.Fatalf("Load of an empty cache = %v, want ErrNoToken", err)
	}
}

func TestFileStore_MissingRefreshTokenIsNotErrNoToken(t *testing.T) {
	ctx := context.Background()
	fs := fileStore(t, t.TempDir(), "agent-one")
	if err := fs.Save(ctx, &oauth2.Token{AccessToken: "access-only"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	_, err := fs.Load(ctx)
	if err == nil {
		t.Fatal("expected an error for a token with no refresh token")
	}
	// The distinction is the point: ErrNoToken is the one load failure that
	// may print a device code, and a cache file that is present but useless
	// is not it.
	if errors.Is(err, ErrNoToken) {
		t.Error("a refresh-token-less cache file reported itself as an empty store")
	}
}

func TestFileStore_SaveIsAtomic_NoTempFileLeftOnSuccess(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	fs := fileStore(t, dir, "agent-one")

	if err := fs.Save(ctx, &oauth2.Token{AccessToken: "a", RefreshToken: "r"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || filepath.Join(dir, entries[0].Name()) != fs.Path() {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("cache dir after a successful save = %v, want only the token file", names)
	}
	got, err := fs.Load(ctx)
	if err != nil || got.AccessToken != "a" {
		t.Errorf("Load after Save = (%+v, %v), want AccessToken=a, nil", got, err)
	}
}

func TestFileStore_ConcurrentSaversNeverExposeAPartialFile(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	fs := fileStore(t, dir, "agent-one")

	if err := fs.Save(ctx, &oauth2.Token{AccessToken: "seed", RefreshToken: "r"}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	const savers = 4
	const savesEach = 10

	stop := make(chan struct{})
	readErrCh := make(chan error, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := fs.Load(ctx); err != nil {
				readErrCh <- err
				return
			}
		}
	}()

	var wg sync.WaitGroup
	errCh := make(chan error, savers)
	for i := 0; i < savers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < savesEach; j++ {
				tok := &oauth2.Token{AccessToken: fmt.Sprintf("a-%d-%d", i, j), RefreshToken: "r"}
				if err := fs.Save(ctx, tok); err != nil {
					errCh <- err
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(stop)
	<-readerDone
	close(errCh)

	for err := range errCh {
		t.Fatalf("Save during concurrent saves: %v", err)
	}
	select {
	case err := <-readErrCh:
		t.Fatalf("Load saw a partially-written file during concurrent saves: %v", err)
	default:
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("cache dir holds %d entries after concurrent saves, want only the token file", len(entries))
	}
}

func TestFileStore_UsesPerUsernameCacheFile(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	one := fileStore(t, dir, "agent-one")
	two := fileStore(t, dir, "agent-two")
	if err := one.Save(ctx, &oauth2.Token{AccessToken: "a", RefreshToken: "r"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if _, err := two.Load(ctx); !errors.Is(err, ErrNoToken) {
		t.Errorf("agent-two loaded a token cached for agent-one: %v", err)
	}
	if _, err := one.Load(ctx); err != nil {
		t.Errorf("agent-one could not load its own cached token: %v", err)
	}
}

func TestNewFileStore_RejectsBlankUsername(t *testing.T) {
	if _, err := NewFileStore(t.TempDir(), "   "); err == nil {
		t.Fatal("expected an error for a blank username")
	}
}

func TestTokenFileName_IsDistinctPerUsername(t *testing.T) {
	a, err := tokenFileName("AgentOne")
	if err != nil {
		t.Fatalf("tokenFileName: %v", err)
	}
	b, err := tokenFileName("AgentTwo")
	if err != nil {
		t.Fatalf("tokenFileName: %v", err)
	}
	if a == b {
		t.Errorf("two usernames share cache file %q", a)
	}
}

func TestTokenFileName_SlugCollisionsStayDistinct(t *testing.T) {
	a, err := tokenFileName("agent.one")
	if err != nil {
		t.Fatalf("tokenFileName: %v", err)
	}
	b, err := tokenFileName("agent/one")
	if err != nil {
		t.Fatalf("tokenFileName: %v", err)
	}
	if a == b {
		t.Errorf("usernames that slug identically share cache file %q", a)
	}
}

func TestTokenFileName_StaysInsideCacheDir(t *testing.T) {
	dir := t.TempDir()
	for _, hostile := range []string{"../../etc/hosts", "a/b/c", `..\..\win`, strings.Repeat("x", 200)} {
		name, err := tokenFileName(hostile)
		if err != nil {
			t.Fatalf("tokenFileName(%q): %v", hostile, err)
		}
		path := filepath.Join(dir, name)
		if filepath.Dir(path) != dir {
			t.Errorf("tokenFileName(%q) = %q escapes cache dir: %q", hostile, name, path)
		}
	}
}

func TestEncodeDecodeToken_IsPortableBetweenStores(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	fs := fileStore(t, dir, "agent-one")
	want := &oauth2.Token{AccessToken: "a", RefreshToken: "r", Expiry: time.Now().Add(time.Hour).Truncate(time.Second)}
	if err := fs.Save(ctx, want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// The bytes on disk are what another Store implementation would hold, so
	// a token written by one has to be readable by the other -- that is what
	// makes moving the cache a copy rather than a re-authentication.
	raw, err := os.ReadFile(fs.Path())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	got, err := DecodeToken(raw)
	if err != nil {
		t.Fatalf("DecodeToken: %v", err)
	}
	if got.RefreshToken != want.RefreshToken || !got.Expiry.Equal(want.Expiry) {
		t.Errorf("decoded %+v, want %+v", got, want)
	}
}

func TestFallback_ReadsThroughWhenPrimaryIsEmpty(t *testing.T) {
	ctx := context.Background()
	primary := &memStore{}
	secondary := &memStore{}
	secondary.seed(t, &oauth2.Token{AccessToken: "a", RefreshToken: "on-the-volume"})

	got, err := NewFallback(primary, secondary).Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.RefreshToken != "on-the-volume" {
		t.Errorf("RefreshToken = %q, want the secondary's", got.RefreshToken)
	}
}

func TestFallback_ReadsThroughWhenPrimaryIsUnavailable(t *testing.T) {
	ctx := context.Background()
	// The state on the first release after this change and before the
	// migration that gives the database its table.
	primary := &memStore{failLoad: fmt.Errorf("%w: relation does not exist", ErrStoreUnavailable)}
	secondary := &memStore{}
	secondary.seed(t, &oauth2.Token{AccessToken: "a", RefreshToken: "on-the-volume"})

	got, err := NewFallback(primary, secondary).Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.RefreshToken != "on-the-volume" {
		t.Errorf("RefreshToken = %q, want the secondary's", got.RefreshToken)
	}
}

func TestFallback_CorruptPrimaryIsNotMaskedBySecondary(t *testing.T) {
	ctx := context.Background()
	primary := &memStore{failLoad: errors.New("cached token has no refresh token")}
	secondary := &memStore{}
	secondary.seed(t, &oauth2.Token{AccessToken: "a", RefreshToken: "on-the-volume"})

	if _, err := NewFallback(primary, secondary).Load(ctx); err == nil {
		t.Fatal("a corrupt primary was answered from the secondary instead of reported")
	}
	if _, saves := secondary.counts(); saves != 0 {
		t.Errorf("secondary saves = %d, want 0", saves)
	}
}

func TestFallback_EmptyEverywhereIsErrNoToken(t *testing.T) {
	_, err := NewFallback(&memStore{}, &memStore{}).Load(context.Background())
	if !errors.Is(err, ErrNoToken) {
		t.Fatalf("Load with nothing cached anywhere = %v, want ErrNoToken", err)
	}
}

func TestFallback_WritesOnlyToThePrimary(t *testing.T) {
	ctx := context.Background()
	primary := &memStore{}
	secondary := &memStore{}
	secondary.seed(t, &oauth2.Token{AccessToken: "a", RefreshToken: "on-the-volume"})

	fb := NewFallback(primary, secondary)
	if err := fb.Save(ctx, &oauth2.Token{AccessToken: "b", RefreshToken: "rotated"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, saves := secondary.counts(); saves != 0 {
		t.Errorf("secondary saves = %d, want 0: two copies of a rotating token are one too many", saves)
	}

	// And once the primary holds a token, the secondary stops being read at
	// all -- which is what lets the volume go away.
	before, _ := secondary.counts()
	got, err := fb.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.RefreshToken != "rotated" {
		t.Errorf("RefreshToken = %q, want the primary's", got.RefreshToken)
	}
	if after, _ := secondary.counts(); after != before {
		t.Errorf("secondary was read %d more times after the primary was populated", after-before)
	}
}

func TestFallback_SaveFailureIsReportedNotRedirected(t *testing.T) {
	primary := &memStore{failSave: fmt.Errorf("%w: relation does not exist", ErrStoreUnavailable)}
	secondary := &memStore{}

	err := NewFallback(primary, secondary).Save(context.Background(), &oauth2.Token{AccessToken: "a", RefreshToken: "r"})
	if !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("Save = %v, want the primary's failure", err)
	}
	if _, saves := secondary.counts(); saves != 0 {
		t.Errorf("secondary saves = %d, want 0", saves)
	}
}
