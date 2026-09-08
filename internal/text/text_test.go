package text

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateNeverSplitsARune(t *testing.T) {
	// Four bytes per rune, so every cap that is not a multiple of four
	// falls inside one.
	s := strings.Repeat("🪓", 8)
	for limit := 0; limit <= len(s)+4; limit++ {
		got := Truncate(s, limit)
		if !utf8.ValidString(got) {
			t.Fatalf("Truncate(%d) = %q, which is not valid UTF-8", limit, got)
		}
		if len(got) > limit && limit <= len(s) {
			t.Fatalf("Truncate(%d) returned %d bytes", limit, len(got))
		}
		if !strings.HasPrefix(s, got) {
			t.Fatalf("Truncate(%d) = %q, not a prefix of the input", limit, got)
		}
	}
}

func TestTruncateLeavesShortStringsAlone(t *testing.T) {
	if got := Truncate("héllo", 32); got != "héllo" {
		t.Errorf("Truncate = %q, want the input unchanged", got)
	}
}
