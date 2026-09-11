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

func TestTruncateEllipsisLeavesFittingStringsAlone(t *testing.T) {
	for _, limit := range []int{len("héllo"), 32} {
		if got := TruncateEllipsis("héllo", limit); got != "héllo" {
			t.Errorf("TruncateEllipsis(%d) = %q, want the input unchanged", limit, got)
		}
	}
}

func TestTruncateEllipsisMarksWhatItCut(t *testing.T) {
	got := TruncateEllipsis(strings.Repeat("a", 64), 20)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("TruncateEllipsis = %q, want it to end in an ellipsis", got)
	}
	if len(got) > 20 {
		t.Errorf("TruncateEllipsis = %d bytes, over the 20-byte limit", len(got))
	}
}

func TestTruncateEllipsisStaysWithinLimitAndValid(t *testing.T) {
	// Four bytes per rune, so most caps land inside one -- and the
	// ellipsis is three further bytes that must fit under the same limit.
	s := strings.Repeat("🪓", 8)
	for limit := 0; limit <= len(s)+4; limit++ {
		got := TruncateEllipsis(s, limit)
		if !utf8.ValidString(got) {
			t.Fatalf("TruncateEllipsis(%d) = %q, which is not valid UTF-8", limit, got)
		}
		if limit <= len(s) && len(got) > limit {
			t.Fatalf("TruncateEllipsis(%d) returned %d bytes", limit, len(got))
		}
		if limit >= len(s) && got != s {
			t.Fatalf("TruncateEllipsis(%d) = %q, want the input unchanged", limit, got)
		}
	}
}

func TestTruncateEllipsisBelowEllipsisWidthDropsIt(t *testing.T) {
	// The caller's limit is a hard bound, so under the ellipsis's own
	// three bytes the marker is what gives way, not the limit.
	for limit := 0; limit < len("…"); limit++ {
		got := TruncateEllipsis("abcdef", limit)
		if strings.Contains(got, "…") {
			t.Errorf("TruncateEllipsis(%d) = %q, want no ellipsis under its own width", limit, got)
		}
		if len(got) > limit {
			t.Errorf("TruncateEllipsis(%d) returned %d bytes", limit, len(got))
		}
	}
}
