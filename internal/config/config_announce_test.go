package config

import "testing"

// Optional, and off by default: the API must never be open because a
// variable was forgotten. Whitespace around a token pasted into a Secret
// is not part of it.
func TestAnnounceAPIToken(t *testing.T) {
	cases := map[string]string{"": "", "  ": "", " s3cret\n": "s3cret"}
	for raw, want := range cases {
		setRequired(t)
		t.Setenv("ANNOUNCE_API_TOKEN", raw)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.AnnounceAPIToken != want {
			t.Errorf("ANNOUNCE_API_TOKEN=%q loaded as %q, want %q", raw, cfg.AnnounceAPIToken, want)
		}
	}
}
