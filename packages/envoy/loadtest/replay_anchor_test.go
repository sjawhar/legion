package main

import (
	"slices"
	"strings"
	"testing"
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
		if q := nextPad(p, n, 3); q == p || len(q) != n {
			t.Fatalf("nextPad must change the bytes, not the size: %q %q", p, q)
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
	// Answers: the answer's version keeps its place, and one past the end takes the last version.
	p, err = planHistory(text, real, 5, []int{0, 1}, []int{3, 0})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.AnswerAt[3], []int{0}) || !slices.Equal(p.AnswerAt[5], []int{1}) || p.Restore != 4 {
		t.Fatalf("answers %v restore %d", p.AnswerAt, p.Restore)
	}
	// A missing size counts and is taken as unchanged.
	if p, _ := planHistory(text, []int{300, -1, len(text)}, 3, nil, nil); p.Missing != 1 {
		t.Fatalf("missing %d", p.Missing)
	}
}
