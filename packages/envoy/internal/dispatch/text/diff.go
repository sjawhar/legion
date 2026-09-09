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
	if len(diff) <= maxUnifiedDiffBytes {
		return diff
	}
	prefix := diff[:maxUnifiedDiffBytes-len(truncatedDiffLine)]
	for !utf8.ValidString(prefix) {
		prefix = prefix[:len(prefix)-1]
	}
	return prefix + truncatedDiffLine
}
