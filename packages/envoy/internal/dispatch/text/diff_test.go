package text

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestUnifiedDiff(t *testing.T) {
	got := UnifiedDiff("before\nsame\n", "after\nsame\n")
	want := "--- previous\n+++ current\n@@ -1,2 +1,2 @@\n-before\n+after\n same\n"
	if got != want {
		t.Fatalf("unified diff = %q, want %q", got, want)
	}
}

func TestUnifiedDiffTruncatesAtFourKilobytes(t *testing.T) {
	got := UnifiedDiff(strings.Repeat("before\n", 1_000), strings.Repeat("after\n", 1_000))
	if len(got) > 4<<10 {
		t.Fatalf("truncated diff has %d bytes, want at most %d", len(got), 4<<10)
	}
	if !strings.HasSuffix(got, "\n… (truncated)\n") {
		t.Fatalf("truncated diff = %q, want trailing truncation line", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("truncated diff is not valid UTF-8: %q", got)
	}
}

// A diff within the limit is whole and not cut.
func TestUnifiedDiffCutKeepsADiffWithinTheLimit(t *testing.T) {
	got, cut := UnifiedDiffCut("before\nsame\n", "after\nsame\n", 4096)
	if want := UnifiedDiff("before\nsame\n", "after\nsame\n"); got != want || cut {
		t.Fatalf("UnifiedDiffCut = %q, %t; want %q, false", got, cut, want)
	}
}

// A diff over the limit is cut at its last line end at or under the limit: it is a prefix of the
// whole diff, ends with a line end, and the next line of the whole diff would not have fitted.
func TestUnifiedDiffCutCutsAtTheLastLineEndUnderTheLimit(t *testing.T) {
	previous := strings.Repeat("before ünïcode line\n", 400)
	current := strings.Repeat("after ünïcode line\n", 400)
	const limit = 4096
	got, cut := UnifiedDiffCut(previous, current, limit)
	whole, _ := UnifiedDiffCut(previous, current, 1<<30)
	if !cut || len(whole) <= limit {
		t.Fatalf("cut = %t with a whole diff of %d bytes; want a cut of a diff over %d bytes", cut, len(whole), limit)
	}
	if len(got) > limit || !strings.HasSuffix(got, "\n") || !strings.HasPrefix(whole, got) {
		t.Fatalf("cut diff of %d bytes (ends %q) is not a line-ended prefix of the whole diff within %d bytes", len(got), got[max(0, len(got)-8):], limit)
	}
	next := strings.IndexByte(whole[len(got):], '\n')
	if next < 0 || len(got)+next+1 <= limit {
		t.Fatalf("the cut at %d bytes leaves the next line (ending at %d) out though it fits within %d", len(got), len(got)+next+1, limit)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("cut diff is not valid UTF-8")
	}
}
