package pmdoc

import (
	"strings"
	"testing"
)

// The shapes below are the browser balancer's own, one Go test per shape
// packages/dispatch/web/src/features/refs/markdown-fragment.test.ts covers, so the two readings
// of a cut snippet can be compared line for line. They come from ts_headline with search.go's
// options (MaxWords=24, MinWords=12, MaxFragments=1) run on a real Postgres.

func balanced(t *testing.T, fragment, want string) {
	t.Helper()
	if got := BalanceCutMarkdown(fragment); got != want {
		t.Errorf("BalanceCutMarkdown(%q) = %q, want %q", fragment, got, want)
	}
}

// ts_headline on `… twenty **anchor** after the mark …` with a 24-word window starts the window
// after the opener and ends it after the closer, so only the `**` after `anchor` survives, and
// the balancer drops exactly that.
func TestASnippetCutAfterABoldSpanOpenedLosesOnlyTheDelimiterTheCutLeftOpen(t *testing.T) {
	balanced(t, "twenty anchor** after the mark twentyone", "twenty anchor after the mark twentyone")
	balanced(t,
		"stop the deploy before the cut and report back** eleven",
		"stop the deploy before the cut and report back eleven",
	)
	// The same for single emphasis, which CommonMark also leaves literal when unmatched.
	balanced(t, "check this thing* before you report back", "check this thing before you report back")
	balanced(t, "Please check *this thing before you report", "Please check this thing before you report")
}

// A delimiter the author typed on purpose stays: one CommonMark would read as literal. `** ` is
// not at either edge of a word and cannot open emphasis, so it is text, not a cut.
func TestADelimiterCommonMarkReadsAsLiteralStays(t *testing.T) {
	balanced(t, "Use ** for bold", "Use ** for bold")
	balanced(t, "press the ` key then", "press the ` key then")
	balanced(t, "2 * 3 is 6", "2 * 3 is 6")
	balanced(t, "snake_case and __init__", "snake_case and __init__")
}

// A tilde run pairs only with a closer its own length, as GFM's strikethrough does: remark-gfm
// reads `~~a~` as plain text, and leaving `~a~` would strike out text the author never struck.
func TestATildeRunPairsOnlyWithACloserItsOwnLength(t *testing.T) {
	balanced(t, "keep ~~a~ here", "keep a here")
	// A run of three or more is literal text to GFM (the third marker ends the sequence), so the
	// `~~` closer is the only delimiter here, and the cut is what took its opener.
	balanced(t, "x ~~~a~~ y", "x ~~~a y")
	// Runs that do pair are the author's, at either length.
	balanced(t, "a ~~struck~~ and ~one~ here", "a ~~struck~~ and ~one~ here")
	// A cut that took the closer leaves the opener to drop, as it does for `**`.
	balanced(t, "report ~~struck text the cut", "report struck text the cut")
}

// Delimiters inside a code span are code, never counted.
func TestDelimitersInsideACodeSpanAreNeverCounted(t *testing.T) {
	balanced(t, "`code with ** inside` and **bold**", "`code with ** inside` and **bold**")
	balanced(t, "call `f(**kwargs)` with", "call `f(**kwargs)` with")
	balanced(t, "``a ` tick`` stays", "``a ` tick`` stays")
}

// A snippet cut inside a link keeps the link's words.
func TestASnippetCutInsideALinkKeepsTheLinksWords(t *testing.T) {
	balanced(t, "read it and [link", "read it and link")
	balanced(t, "read [the docs](https://example.com/do", "read the docs")
	balanced(t, "docs](https://example.com/docs) and then", "docs and then")
}

// A fragment whose marks are balanced is left exactly as it is.
func TestABalancedFragmentIsLeftExactlyAsItIs(t *testing.T) {
	whole := "**Bold** then `code` and [a link](https://example.com) and ~~old~~ __u__ *i* _j_"
	balanced(t, whole, whole)
	balanced(t, "", "")
}

// A fragment cut inside a code span drops the open backtick.
func TestAFragmentCutInsideACodeSpanDropsTheOpenBacktick(t *testing.T) {
	balanced(t, "Before the cut `dispatch-deploy.y", "Before the cut dispatch-deploy.y")
}

// A search snippet is balanced only when the headline window cut it: exactly 24 words.
func TestASearchSnippetIsBalancedOnlyWhenTheHeadlineWindowCutIt(t *testing.T) {
	// ts_headline returns a short body whole (measured: `Use ** for bold and anchor here` ->
	// `bold and <<anchor>> here`, 4 words): the author's `**` is theirs.
	if got := SearchSnippetMarkdown("bold and ** anchor here"); got != "bold and ** anchor here" {
		t.Errorf("a four-word snippet was balanced: %q", got)
	}
	// At the window (measured: 24 words around the match, the mark's opener left outside it).
	window := strings.Repeat("word ", 21) + "anchor** after the"
	if words := len(strings.Split(window, " ")); words != 24 {
		t.Fatalf("the window holds %d words, want 24", words)
	}
	if got, want := SearchSnippetMarkdown(window), strings.Replace(window, "**", "", 1); got != want {
		t.Errorf("SearchSnippetMarkdown(window) = %q, want %q", got, want)
	}
}
