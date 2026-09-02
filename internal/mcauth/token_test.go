package mcauth

import (
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestSaveAndLoadToken_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, tokenFileName)

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
	path := filepath.Join(dir, tokenFileName)
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
	path := filepath.Join(dir, tokenFileName)

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
