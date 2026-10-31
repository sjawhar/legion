package main

import (
	"reflect"
	"strings"
	"testing"
)

// applyAsDispatch is a pure model of POST /api/v1/artifacts/{id}/edits at Dispatch 35efd326
// (replay_edit.go names the code it reads) over the ops the replay plans: it applies ops to text
// in order and returns the text Dispatch then holds, or the status and code it answers the batch
// with. A find or an insert's anchor is matched against the rendered text (mdDoc.rendered): none is
// 404 TARGET_NOT_FOUND, more than one 409 TARGET_AMBIGUOUS, one reaching across a line
// 400 TARGET_SPANS_BLOCKS; the match must be a plain run of one line, the only run the markdown
// writes it as, else Dispatch's match is not the client's (404 TARGET_NOT_FOUND here). A replace's
// with must be one line of plain text that reads back as itself where it lands and opens a block's
// text only with a letter or a digit that starts no list item (else 400 INVALID_OP: Dispatch
// refuses a with of two paragraphs or one opening with the block's own marker, and writes other
// openings escaped). An insert lands after the one-line top-level paragraph or heading holding its
// anchor, a blank line apart; any other landing is not modelled ("unmodelled").
func applyAsDispatch(text string, ops []replayEditOp) (string, string) {
	for _, op := range ops {
		d := scanMarkdown(text)
		switch op.Op {
		case "replace":
			at, code := resolveAsDispatch(d, op.Find)
			if code != "" {
				return "", code
			}
			if op.With == nil || *op.With == "" || strings.Contains(*op.With, "\n") {
				return "", "400 INVALID_OP"
			}
			with := *op.With
			next := text[:at] + with + text[at+len(op.Find):]
			nd := scanMarkdown(next)
			li := lineOf(nd, at)
			if !nd.plainRun(at, at+len(with)) || (at-nd.lines[li].start == blockStart(nd.lines[li].text) && !opensText(with)) {
				return "", "400 INVALID_OP"
			}
			text = next
		case "insert":
			at, code := resolveAsDispatch(d, op.After)
			if code != "" {
				return "", code
			}
			li := lineOf(d, at)
			l := d.lines[li].text
			blank := func(i int) bool { return i < 0 || i >= len(d.lines) || d.lines[i].text == "" }
			if !blank(li-1) || !blank(li+1) || strings.HasPrefix(l, "|") || (blockStart(l) > 0 && !strings.HasPrefix(l, "#")) {
				return "", "unmodelled"
			}
			md := op.Markdown
			if md == "" || strings.TrimSpace(md) != md || strings.Contains(md, ":::") {
				return "", "unmodelled"
			}
			end := d.lines[li].start + len(l)
			text = text[:end] + "\n\n" + md + text[end:]
		default:
			return "", "unmodelled"
		}
	}
	return text, ""
}

func resolveAsDispatch(d *mdDoc, quote string) (int, string) {
	switch n := strings.Count(d.rendered, quote); {
	case strings.TrimSpace(quote) == "":
		return 0, "400 INVALID_OP"
	case strings.Contains(quote, "\n"):
		return 0, "400 TARGET_SPANS_BLOCKS"
	case n == 0:
		return 0, "404 TARGET_NOT_FOUND"
	case n > 1 || strings.Count(d.text, quote) > 1:
		return 0, "409 TARGET_AMBIGUOUS"
	}
	at := strings.Index(d.text, quote)
	if !d.plainRun(at, at+len(quote)) {
		return 0, "404 TARGET_NOT_FOUND"
	}
	return at, ""
}

// lineOf is the index of d's line holding the byte at.
func lineOf(d *mdDoc, at int) int {
	for i := len(d.lines) - 1; i >= 0; i-- {
		if d.lines[i].start <= at {
			return i
		}
	}
	return 0
}

// TestApplyAsDispatchRefusals holds the model to the refusals the first replay met at 35efd326:
// a find holding a list marker and a line break (400 TARGET_SPANS_BLOCKS), one holding escapes
// (404 TARGET_NOT_FOUND), a with forming two paragraphs and one opening with the block's own
// marker (400 INVALID_OP), and a repeated find (409 TARGET_AMBIGUOUS).
func TestApplyAsDispatchRefusals(t *testing.T) {
	doc := "# Plan\n\n- first item here\n- second item here\n\nThe word \\_\\_kmem stays escaped.\n\nAlpha beta. Alpha beta.\n"
	for _, c := range []struct {
		name string
		op   replayEditOp
		code string
	}{
		{"marker and line break", replayEditOp{Op: "replace", Find: "- first item here\n- second", With: new("x")}, "400 TARGET_SPANS_BLOCKS"},
		{"escapes", replayEditOp{Op: "replace", Find: "word \\_\\_kmem", With: new("x")}, "404 TARGET_NOT_FOUND"},
		{"two paragraphs", replayEditOp{Op: "replace", Find: "first item", With: new("one\n\ntwo")}, "400 INVALID_OP"},
		{"the block's marker", replayEditOp{Op: "replace", Find: "first item here", With: new("- other")}, "400 INVALID_OP"},
		{"repeated", replayEditOp{Op: "replace", Find: "Alpha beta", With: new("x")}, "409 TARGET_AMBIGUOUS"},
		{"anchor in a list", replayEditOp{Op: "insert", After: "second item here", Markdown: "x"}, "unmodelled"},
	} {
		if _, code := applyAsDispatch(doc, []replayEditOp{c.op}); code != c.code {
			t.Errorf("%s: %q, want %q", c.name, code, c.code)
		}
	}
	next, code := applyAsDispatch(doc, []replayEditOp{{Op: "replace", Find: "second", With: new("2nd")}, {Op: "insert", After: "stays", Markdown: "New paragraph."}})
	if code != "" || next != "# Plan\n\n- first item here\n- 2nd item here\n\nThe word \\_\\_kmem stays escaped.\n\nNew paragraph.\n\nAlpha beta. Alpha beta.\n" {
		t.Fatalf("applied: %q %q", code, next)
	}
}

func TestPlainBytes(t *testing.T) {
	plainOf := func(line string) string {
		_, p := plainBytes(line)
		var b strings.Builder
		for i := range p {
			if p[i] {
				b.WriteByte(line[i])
			} else {
				b.WriteByte('.')
			}
		}
		return b.String()
	}
	for line, want := range map[string]string{
		"- item with `code x` and *em y* end":    "..item with ........ and ...... end",
		"1. see [the link](http://x) or www.a.b": "...see .................... or .......",
		"| cell one | two |":                     "..cell one...two..",
		"## Heading: a=b (c)":                    "...Heading: a=b (c)",
		"Escaped \\_x and a@b.c and ![i](s)":     "Escaped ..x and ..... and .......",
		"|---|:--:|":                             "",
		"    indented code":                      "",
		"[ref]: http://x":                        "",
	} {
		if got := plainOf(line); got != want {
			t.Errorf("%q:\n got %q\nwant %q", line, got, want)
		}
	}
}

func TestPlanEditDiffAndSubstitute(t *testing.T) {
	// A complete diff changing a line: one replace of the changed word, unique in the text.
	ops, next, fromDiff, ok := planEdit(rpDocText, rpDiff, 0, 1, "D1", "e1")
	if !ok || !fromDiff || len(ops) != 1 || ops[0].Find != "dog" || *ops[0].With != "cat" || next != strings.Replace(rpDocText, "lazy dog", "lazy cat", 1) {
		t.Fatalf("diff edit: ok %v diff %v ops %+v", ok, fromDiff, ops)
	}
	// A repeated word widens until unique; two changed lines are two replaces; a paragraph added
	// between two is an insert after the one before it.
	two := "--- a\n+++ b\n@@ -3,3 +3,5 @@\n-Alpha bravo charlie delta echo foxtrot golf. Hotel india juliet kilo lima mike november.\n+Alpha bravo charlie delta echo foxtrot golf. Hotel india juliet kilo lima mike october.\n \n+Inserted paragraph here.\n+\n-Oscar papa quebec romeo sierra tango uniform. Victor whiskey xray yankee zulu again here.\n+Oscar papa quebec romeo sierra tango uniform. Victor whiskey xray yankee zulu again there.\n"
	ops, next, fromDiff, ok = planEdit(rpDocText, two, 0, 1, "D1", "e2")
	want := strings.NewReplacer("november.", "october.", "again here.", "again there.", "\n\nOscar", "\n\nInserted paragraph here.\n\nOscar").Replace(rpDocText)
	if !ok || !fromDiff || len(ops) != 3 || ops[1].Op != "insert" || next != want {
		t.Fatalf("lines and insert: ok %v diff %v ops %+v\n%q", ok, fromDiff, ops, next)
	}
	if got, code := applyAsDispatch(rpDocText, ops); code != "" || got != next {
		t.Fatalf("Dispatch would answer %q", code)
	}
	// A cut diff, a removed line, a diff the text is not the base of, a change inside a code span:
	// substitute replaces whose length change is exactly the logged size delta, as many sentences
	// as a shrink takes.
	cut := rpDiff + "… (truncated)\n"
	removed := "--- a\n+++ b\n@@ -6,2 +6,1 @@\n \n-The quick brown fox jumps over the lazy dog today.\n"
	missing := "--- a\n+++ b\n@@ -1,1 +1,1 @@\n-No such line in the text.\n+Another.\n"
	code := "--- a\n+++ b\n@@ -1,1 +1,1 @@\n-# Plan `x`\n+# Plan `y`\n"
	for _, d := range []string{cut, removed, missing, ""} {
		for _, delta := range []int{0, 37, -10, -150} {
			ops, next, fromDiff, ok := planEdit(rpDocText, d, delta, 1, "D1", "e3")
			if !ok || fromDiff || len(ops) == 0 || (delta > -40 && len(ops) != 1) {
				t.Fatalf("delta %d: ok %v diff %v ops %+v", delta, ok, fromDiff, ops)
			}
			if got := len(next) - len(rpDocText); got != delta {
				t.Fatalf("delta %d: the text changed by %d bytes", delta, got)
			}
			if got, code := applyAsDispatch(rpDocText, ops); code != "" || got != next {
				t.Fatalf("delta %d: Dispatch would answer %q", delta, code)
			}
		}
	}
	if _, _, fromDiff, _ := planEdit(strings.Replace(rpDocText, "# Plan", "# Plan `x`", 1), code, 0, 1, "D1", "e5"); fromDiff {
		t.Fatal("a change inside a code span came from the diff")
	}
	// One seed, one substitute.
	a, _, _, _ := planEdit(rpDocText, "", 5, 1, "D1", "e4")
	b, _, _, _ := planEdit(rpDocText, "", 5, 1, "D1", "e4")
	if !reflect.DeepEqual(a, b) {
		t.Fatal("the substitute is not deterministic")
	}
	// A diff of exactly the cap is cut.
	if !diffCut(strings.Repeat("x", replayDiffCap)) || diffCut(rpDiff) {
		t.Fatal("diffCut")
	}
}

func TestNATSKey(t *testing.T) {
	for id, want := range map[string]bool{
		"01a179a2-7029-707e-943c-e192020b043b": true, "ses-replay-0001": true,
		"legion-daemon:LINUX": false, "": false, ".a": false, "a.": false, "a b": false,
	} {
		if natsKey(id) != want {
			t.Errorf("natsKey(%q) = %v", id, !want)
		}
	}
}

// TestSubstituteAnchorMatchesOnce: a sentence the markdown holds once but the rendered text twice
// (another copy split by markup) is no anchor quote, since Dispatch would match it twice.
func TestSubstituteAnchorMatchesOnce(t *testing.T) {
	doc := "Alpha bravo charlie delta echo foxtrot.\n\nAlpha **bravo** charlie delta echo foxtrot.\n\nGolf hotel india juliet kilo lima.\n"
	q, _, err := substituteAnchor(doc, nil, "", "", 1, "d", "c1")
	if err != nil || q != "Golf hotel india juliet kilo lima." {
		t.Fatalf("substituteAnchor: %q %v", q, err)
	}
	if q, sub := anchorQuote(doc, nil, "", "Alpha bravo charlie", 1, "d", "c2"); !sub || q != "Golf hotel india juliet kilo lima." {
		t.Fatalf("anchorQuote kept a logged quote matching twice: %q %v", q, sub)
	}
}
