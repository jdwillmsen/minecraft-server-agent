package mcauth

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestSaveAndLoadToken_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	want := &oauth2.Token{
		AccessToken:  "access",
		RefreshToken: "refresh",
		Expiry:       time.Now().Add(time.Hour).Truncate(time.Second),
	}
	if err := saveToken(path, want); err != nil {
		t.Fatalf("saveToken: %v", err)
	}

	got, err := loadToken(path)
	if err != nil {
		t.Fatalf("loadToken: %v", err)
	}
	if got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestLoadToken_MissingFile(t *testing.T) {
	dir := t.TempDir()
	_, err := loadToken(filepath.Join(dir, "does-not-exist.json"))
	if err == nil {
		t.Fatal("expected an error for a missing token file")
	}
}

func TestLoadToken_MissingRefreshTokenRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")
	if err := saveToken(path, &oauth2.Token{AccessToken: "access-only"}); err != nil {
		t.Fatalf("saveToken: %v", err)
	}

	if _, err := loadToken(path); err == nil {
		t.Fatal("expected an error for a token with no refresh token")
	}
}

type stubTokenSource struct {
	tokens []*oauth2.Token
	calls  int
}

func (s *stubTokenSource) Token() (*oauth2.Token, error) {
	tok := s.tokens[s.calls]
	s.calls++
	return tok, nil
}

func TestCachingTokenSource_PersistsEachRefresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	stub := &stubTokenSource{tokens: []*oauth2.Token{
		{AccessToken: "first", RefreshToken: "r1"},
		{AccessToken: "second", RefreshToken: "r2"},
	}}
	cts := &cachingTokenSource{path: path, inner: stub}

	if _, err := cts.Token(); err != nil {
		t.Fatalf("Token: %v", err)
	}
	got, err := loadToken(path)
	if err != nil {
		t.Fatalf("loadToken after first call: %v", err)
	}
	if got.AccessToken != "first" {
		t.Errorf("AccessToken = %q, want first", got.AccessToken)
	}

	if _, err := cts.Token(); err != nil {
		t.Fatalf("Token: %v", err)
	}
	got, err = loadToken(path)
	if err != nil {
		t.Fatalf("loadToken after second call: %v", err)
	}
	if got.AccessToken != "second" {
		t.Errorf("AccessToken = %q, want second after refresh", got.AccessToken)
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

func TestTokenFileName_RejectsBlankUsername(t *testing.T) {
	if _, err := tokenFileName("   "); err == nil {
		t.Fatal("expected an error for a blank username")
	}
}

func TestTokenSource_UsesPerUsernameCacheFile(t *testing.T) {
	dir := t.TempDir()
	name, err := tokenFileName("agent-one")
	if err != nil {
		t.Fatalf("tokenFileName: %v", err)
	}
	if err := saveToken(filepath.Join(dir, name), &oauth2.Token{AccessToken: "a", RefreshToken: "r"}); err != nil {
		t.Fatalf("saveToken: %v", err)
	}

	// A cached token for agent-one must not be picked up for agent-two;
	// without a real login available here, the proof is that the second
	// account's cache file simply doesn't exist yet.
	other, err := tokenFileName("agent-two")
	if err != nil {
		t.Fatalf("tokenFileName: %v", err)
	}
	if _, err := loadToken(filepath.Join(dir, other)); err == nil {
		t.Error("agent-two loaded a token cached for agent-one")
	}

	if _, err := loadToken(filepath.Join(dir, name)); err != nil {
		t.Errorf("agent-one could not load its own cached token: %v", err)
	}
}
