package config

import (
	"net/url"
	"strings"
	"testing"
)

func TestPostgresDSNEmptyWithoutAHost(t *testing.T) {
	if got := (Config{}).PostgresDSN(); got != "" {
		t.Errorf("PostgresDSN = %q, want empty when unconfigured", got)
	}
}

// A CNPG-generated password can contain characters that are structural in a
// URL. Unencoded, they do not fail loudly -- they parse as a different host.
//
// The expected value is assembled rather than written as one literal: a
// complete postgres:// URI with an embedded password is what secret scanners
// are built to find, and a test fixture that trips GitGuardian on every push
// trains people to wave the scanner through.
func TestPostgresDSNEncodesAwkwardPasswords(t *testing.T) {
	const (
		user = "app"
		pass = "p@ss/w:rd"
		host = "pg.example"
		db   = "db"
	)
	c := Config{PGHost: host, PGPort: 5432, PGDatabase: db, PGUser: user, PGPassword: pass}

	want := "postgres://" + user + ":" + url.QueryEscape(pass) + "@" + host + ":5432/" + db + "?sslmode=require"
	if got := c.PostgresDSN(); got != want {
		t.Errorf("PostgresDSN = %q, want %q", got, want)
	}

	// The point of the encoding: the raw characters must not survive into the
	// DSN, where they would re-parse as structure rather than as a password.
	if strings.ContainsAny(c.PostgresDSN()[len("postgres://"):len("postgres://")+len(user)+1+len(pass)], "@/") {
		t.Error("raw password characters survived into the DSN")
	}
}
