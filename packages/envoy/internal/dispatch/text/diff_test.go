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
