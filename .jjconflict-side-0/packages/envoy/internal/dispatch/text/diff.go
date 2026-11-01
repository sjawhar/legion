package text

import (
	"strings"
	"unicode/utf8"

	"github.com/pmezard/go-difflib/difflib"
)

const (
	maxUnifiedDiffBytes = 4 << 10
	truncatedDiffLine   = "\n… (truncated)\n"
)

// UnifiedDiff returns a context diff from previous markdown to current markdown.
func UnifiedDiff(previous, current string) string {
	diff := wholeUnifiedDiff(previous, current)
	if len(diff) <= maxUnifiedDiffBytes {
		return diff
	}
	prefix := diff[:maxUnifiedDiffBytes-len(truncatedDiffLine)]
	for !utf8.ValidString(prefix) {
		prefix = prefix[:len(prefix)-1]
	}
	return prefix + truncatedDiffLine
}

// UnifiedDiffCut returns the same context diff as UnifiedDiff, cut at the last line end at or
// under limit bytes rather than marked inside it, and whether it was cut. A cut diff is whole
// lines of the full diff, so a reader can still apply it as far as it goes; a diff whose first line
// is already longer than limit cuts to nothing.
func UnifiedDiffCut(previous, current string, limit int) (string, bool) {
	diff := wholeUnifiedDiff(previous, current)
	if len(diff) <= limit {
		return diff, false
	}
	return diff[:strings.LastIndexByte(diff[:limit], '\n')+1], true
}

func wholeUnifiedDiff(previous, current string) string {
	previousLines := []string{}
	if previous != "" {
		previousLines = difflib.SplitLines(strings.TrimSuffix(previous, "\n"))
	}
	currentLines := []string{}
	if current != "" {
		currentLines = difflib.SplitLines(strings.TrimSuffix(current, "\n"))
	}
	diff, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A:        previousLines,
		B:        currentLines,
		Context:  3,
		FromFile: "previous",
		ToFile:   "current",
	})
	if err != nil {
		panic(err)
	}
	return diff
}
