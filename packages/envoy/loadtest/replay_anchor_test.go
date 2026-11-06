package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

const anchorDoc = "# Title line\n\nThe first paragraph sentence is here. It has another sentence too.\n\n" +
	"The second paragraph sentence lives here. With a second unique sentence.\n\n" +
	"The third paragraph sentence is the last. Plus one more closing sentence.\n"

func anchorMap() *replayBlockMap {
	return &replayBlockMap{Version: 3, Blocks: []replayBlock{
		{ID: "h", Type: "heading", Text: "# Title line"},
		{ID: "p1", Type: "paragraph", Text: "The first paragraph sentence is here. It has another sentence too."},
		{ID: "p2", Type: "paragraph", Text: "The second paragraph sentence lives here. With a second unique sentence."},
		{ID: "gone", Type: "paragraph", Text: "A paragraph only the current version holds, not the text at At."},
		{ID: "p3", Type: "paragraph", Text: "The third paragraph sentence is the last. Plus one more closing sentence."},
	}}
}

func inSpan(t *testing.T, c anchorChoice, want string) {
	t.Helper()
	span := anchorDoc[c.Start:c.End]
	if span != want {
		t.Fatalf("branch %s chose span %q, want %q", c.Branch, span, want)
	}
	for _, q := range c.Candidates {
		if !strings.Contains(span, q) || strings.Count(anchorDoc, q) != 1 {
			t.Fatalf("candidate %q is not unique inside the chosen span", q)
		}
	}
}

func TestChooseAnchorBranches(t *testing.T) {
	bm := anchorMap()
	c := chooseAnchor(anchorDoc, bm, "p2", "second paragraph sentence", false, false, 1, "d", "c1")
	if c.Branch != ruleLoggedQuote || !slices.Equal(c.Candidates, []string{"second paragraph sentence"}) {
		t.Fatalf("logged quote inside its block: %+v", c)
	}
	inSpan(t, c, bm.Blocks[2].Text)

	// The quote lies outside its logged block but once in the text: still the logged quote, its
	// span the quote itself; a logged block that is gone does not stop it either.
	c = chooseAnchor(anchorDoc, bm, "p2", "third paragraph sentence", false, false, 1, "d", "c2")
	if c.Branch != ruleLoggedQuote || anchorDoc[c.Start:c.End] != "third paragraph sentence" {
		t.Fatalf("quote outside its block: %s", c.Branch)
	}
	if c := chooseAnchor(anchorDoc, bm, "unknown", "third paragraph sentence", false, false, 1, "d", "c2b"); c.Branch != ruleLoggedQuote {
		t.Fatalf("absent block, usable quote: %s", c.Branch)
	}
	// A repeated logged quote, once inside its located block: that occurrence (the second of
	// three), under in_logged_block.
	c = chooseAnchor(anchorDoc, bm, "p2", "paragraph sentence", false, false, 1, "d", "c2c")
	if c.Branch != ruleInBlock || c.Occurrence == nil || *c.Occurrence != 1 || anchorDoc[c.QuoteAt:c.QuoteAt+len("paragraph sentence")] != "paragraph sentence" ||
		c.QuoteAt < c.Start || !slices.Equal(c.Candidates, []string{"paragraph sentence"}) || quoteAt(c) != c.QuoteAt {
		t.Fatalf("repeated quote in its block: %+v", c)
	}
	// Orphaned, or refused (skipLogged): in the block too.
	for _, c := range []anchorChoice{
		chooseAnchor(anchorDoc, bm, "p2", "second paragraph sentence", true, false, 1, "d", "c3"),
		chooseAnchor(anchorDoc, bm, "p2", "second paragraph sentence", false, true, 1, "d", "c3"),
	} {
		if c.Branch != ruleInBlock {
			t.Fatalf("orphaned or refused: %s", c.Branch)
		}
		inSpan(t, c, bm.Blocks[2].Text)
	}

	// The block's text is not in the text at At: the nearest surviving block, the earlier on a tie.
	c = chooseAnchor(anchorDoc, bm, "gone", "", false, false, 1, "d", "c4")
	if c.Branch != ruleNearest || c.BlockID != "p2" {
		t.Fatalf("missing block text: %s %s", c.Branch, c.BlockID)
	}
	inSpan(t, c, bm.Blocks[2].Text)

	// The block id is absent and the quote orphaned: from the block holding the quote in the
	// current text.
	c = chooseAnchor(anchorDoc, bm, "unknown", "third paragraph sentence", true, false, 1, "d", "c5")
	if c.Branch != ruleNearest || c.BlockID != "p3" {
		t.Fatalf("absent id with quote: %s %s", c.Branch, c.BlockID)
	}
	// Absent id, quote nowhere: the block at the middle.
	c = chooseAnchor(anchorDoc, bm, "unknown", "nowhere at all", false, false, 1, "d", "c6")
	if c.Branch != ruleNearest || c.BlockID != "p2" {
		t.Fatalf("absent id without quote: %s %s", c.Branch, c.BlockID)
	}
	// Deterministic.
	if a, b := chooseAnchor(anchorDoc, bm, "gone", "", false, false, 7, "d", "x"), chooseAnchor(anchorDoc, bm, "gone", "", false, false, 7, "d", "x"); !slices.Equal(a.Candidates, b.Candidates) {
		t.Fatal("the choice must be deterministic")
	}
	if c := chooseAnchor(anchorDoc, nil, "p2", "second paragraph sentence", false, false, 1, "d", "c7"); c.Branch != ruleNoBlockMap {
		t.Fatalf("no block map: %s", c.Branch)
	}
	q, branch, err := substituteAnchor(anchorDoc, bm, "gone", "", 1, "d", "w1")
	if err != nil || branch != ruleNearest || !strings.Contains(bm.Blocks[2].Text, q) {
		t.Fatalf("substituteAnchor: %q %s %v", q, branch, err)
	}
}

func TestStripAnswers(t *testing.T) {
	md := ":::ask{#b1 urgency=\"high\" multiple=\"false\" state=\"answered\" answered_by=\"u\" answered_at=\"2026-10-26T22:00:00Z\" selected=\"[]\" answer=\"x\"}\nQ\n:::\n" +
		":::ask{#b2 urgency=\"high\" multiple=\"false\" state=\"answered\" answered_by=\"u\" answered_at=\"t\" selected=\"[]\"}\nQ\n:::\n"
	got := stripAnswers(md, map[string]bool{"b1": true})
	want := ":::ask{#b1 urgency=\"high\" multiple=\"false\" state=\"open\"}\nQ\n:::\n" + md[strings.Index(md, ":::ask{#b2"):]
	if got != want {
		t.Fatalf("stripped\n%q\nwant\n%q", got, want)
	}
}

func TestPadText(t *testing.T) {
	for _, n := range []int{8, 9, 40, 333} {
		p := padText(n, 3)
		if len(p) != n || strings.HasSuffix(p, " ") || !strings.HasPrefix(p, "zqpad") || strings.ContainsAny(p, fallbackSyntax) {
			t.Fatalf("padText(%d) = %q", n, p)
		}
	}
}

// TestPadEdits walks padding paragraphs through growths, shrinks and unchanged sizes and holds
// each edit to padEdit's promise: applied to the paragraph its find occurs in once, it gives the
// next paragraph, and it carries the change and under 40 bytes besides.
func TestPadEdits(t *testing.T) {
	base := "Some text.\n\n## Heading\n\nMore text.\n"
	sizes := []int{8, 8, 9, 9, 10, 19, 20, 21, 21, 29, 30, 31, 500, 500, 501, 499, 12, 12, 8, 4000, 3990, 3990, 3981, 3981, 9, 9, 8}
	pad := padAt(sizes[0])
	for i, n := range sizes[1:] {
		next := padNext(pad, n)
		if len(next) != n || next == pad || strings.HasSuffix(next, " ") || !strings.HasPrefix(next, padToken) || strings.ContainsAny(next, fallbackSyntax) {
			t.Fatalf("step %d: padding %q after %q", i, next, pad)
		}
		find, with := padEdit(pad, next)
		doc := base + "\n" + pad + "\n"
		if strings.Count(doc, find) != 1 || !strings.HasSuffix(pad, find) {
			t.Fatalf("step %d: find %q is not the padding's unique tail in %q", i, find, pad)
		}
		if got := strings.Replace(doc, find, with, 1); got != base+"\n"+next+"\n" {
			t.Fatalf("step %d: replace gives %q, want padding %q", i, got, next)
		}
		if change := abs(len(next) - len(pad)); len(find)+len(with) > change+40 {
			t.Fatalf("step %d: %d to %d bytes sent find %d and with %d bytes", i, len(pad), len(next), len(find), len(with))
		}
		pad = next
	}
	if w := padWord(26*26*26*26 - 1); w != padToken+"zzzz" || padWord(27) != padToken+"aabb" {
		t.Fatalf("pad words %q %q", w, padWord(27))
	}
}

func TestJSONPrefix(t *testing.T) {
	for _, s := range []string{"", "plain words", "q\"uote\\ <a href=\"x\">&amp;</a>\n\t\r\b\f\x01\x1f", "caf\u00e9 \u2028\u2029 \U0001F600 \xff\xfe end", strings.Repeat("ab<\n", 50)} {
		enc := jsonPrefix(s)
		b, _ := json.Marshal(s)
		if enc[len(s)] != len(b)-2 {
			t.Fatalf("%q: jsonPrefix %d, json.Marshal %d", s, enc[len(s)], len(b)-2)
		}
		for k := range len(s) {
			if !utf8.RuneStart(s[k]) {
				continue
			}
			b, _ := json.Marshal(s[:k])
			if enc[k] != len(b)-2 {
				t.Fatalf("%q[:%d]: jsonPrefix %d, json.Marshal %d", s, k, enc[k], len(b)-2)
			}
		}
	}
}

func TestPlanHistorySizes(t *testing.T) {
	text := "Intro paragraph text.\n\n## One\n\n" + strings.Repeat("first section words. ", 20) + "\n\n## Two\n\n" + strings.Repeat("second section words. ", 20) + "\n"
	// Production: grows, stays (a named settlement), shrinks a lot, then grows to the text's size.
	real := []int{300, 300, 900, 250, len(text) + 50}
	p, err := planHistory(text, real, 5, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Sizes[4] != len(text) || p.Restore != 5 || p.Flipped != 0 || p.Unreachable != 0 {
		t.Fatalf("plan %+v", p)
	}
	for n := 2; n <= 5; n++ {
		if p.Sizes[n-1]-p.Sizes[n-2] != real[n-1]-real[n-2] {
			t.Fatalf("version %d changes %d, production %d", n, p.Sizes[n-1]-p.Sizes[n-2], real[n-1]-real[n-2])
		}
	}
	base := baseText(text, p.RunStart, p.RunEnd)
	if p.RunEnd <= p.RunStart || len(base)+padOverhead(len(base))+padMin > p.Sizes[3] {
		t.Fatalf("the run [%d, %d) leaves %d bytes, too many for the smallest version %d", p.RunStart, p.RunEnd, len(base), p.Sizes[3])
	}
	if !strings.HasPrefix(text[p.RunStart:], "## ") && p.RunStart != 0 {
		t.Fatal("a run starts at a section")
	}
	// Ask steps: an answer's or resolve's version keeps its place, and one past the end takes the
	// last free version.
	p, err = planHistory(text, real, 5, []askStep{{Ask: 0}, {Ask: 1, Resolve: true}}, []int{3, 0})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.AskAt[3], []askStep{{Ask: 0}}) || !slices.Equal(p.AskAt[5], []askStep{{Ask: 1, Resolve: true}}) || p.Restore != 4 {
		t.Fatalf("ask steps %v restore %d", p.AskAt, p.Restore)
	}
	// A missing size counts and is taken as unchanged.
	if p, _ := planHistory(text, []int{300, -1, len(text)}, 3, nil, nil); p.Missing != 1 {
		t.Fatalf("missing %d", p.Missing)
	}
}

// TestChooseRunFitsTheRestoreBody: the restore edit carries its run whole, so a run whose encoded
// markdown is over the bound is never chosen, however much the history needs removed; the longest
// run that fits stands in.
func TestChooseRunFitsTheRestoreBody(t *testing.T) {
	one := "## One\n\n" + strings.Repeat("plain words. ", 10) + "\n\n"
	two := "## Two\n\n" + strings.Repeat("\"quoted\" <tag> & words.\n", 10) + "\n"
	text := "Intro.\n\n" + one + two
	start, end := chooseRun(text, len(one)+len(two), 1<<20)
	if text[start:end] != one+two {
		t.Fatalf("unbounded: run %q", text[start:end])
	}
	enc := jsonPrefix(two)[len(two)]
	if enc <= len(one) || len(two) >= enc {
		t.Fatalf("the escaped section must encode longer than the plain one: %d, %d", enc, len(one))
	}
	start, end = chooseRun(text, len(one)+len(two), enc-1)
	if text[start:end] != "Intro.\n\n"+one {
		t.Fatalf("bounded under the escaped section's %d encoded bytes: run %q", enc, text[start:end])
	}
}
