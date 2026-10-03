package pmdoc

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Parse and Render finish in time linear in the text on the shapes that cost quadratic time
// before LEGION-465. Each bound is at least ten times the time measured after the fix on a
// development machine (parse: `a_` 0.3 s, `a_b*` 2 s, `a~b_` 1 s per MiB; render of 4 MiB: `[a`
// 1.4 s, `a_b&` 0.4 s, `<a` 1.3 s, `[^a` 1.0 s) and well under the time before it (parse of 1 MiB
// of `a_`: about 4 minutes; of `a_b*`: 12 minutes; render of 4 MiB of `[a`: minutes). A caller's
// write of a mebibyte of these is refused for the elements it makes (maxWriteElements), so the
// parse here is a read-back's, which counts none and reads all of it.
func TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText(t *testing.T) {
	for _, shape := range []string{"a_", "a_b*", "a~b_"} {
		markdown := strings.Repeat(shape, (1<<20)/len(shape))
		started := time.Now()
		if _, err := ParseRendering(markdown); err != nil {
			t.Fatalf("parse 1 MiB of %q: %v", shape, err)
		}
		if elapsed := time.Since(started); elapsed > 30*time.Second {
			t.Errorf("parse of 1 MiB of %q took %s, want under 30 s", shape, elapsed)
		}
	}
	for _, shape := range []string{"[a", "a_b&", "<a", "[^a"} {
		doc := &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{{Type: "text", Text: strings.Repeat(shape, (4<<20)/len(shape))}}}}}
		started := time.Now()
		if _, err := Render(doc); err != nil {
			t.Fatalf("render 4 MiB of %q: %v", shape, err)
		}
		if elapsed := time.Since(started); elapsed > 30*time.Second {
			t.Errorf("render of 4 MiB of %q took %s, want under 30 s", shape, elapsed)
		}
	}
}

// The shapes a sweep found after the ones above, each quadratic before LEGION-465 finished: a
// text of one delimiter character, whose every character was escaped by a scan of its whole run;
// many `[^` before one `]`, whose every label was case-folded to look it up among the footnote
// labels; a text of many line feeds, whose every line joined the rest of the text to ask whether
// only whitespace followed; and a run of `~` in a paragraph, which goldmark's strikethrough parser
// scanned again from every `~` in it. Before the fix 128 KiB of each text took 18-115 s to render
// and 32 KiB of `[^a` 7.7 s; each bound is at least ten times the time measured after it.
func TestRenderIsLinearOnRunsFootnoteLabelsAndLineFeeds(t *testing.T) {
	paragraph := func(text string) *Node {
		return &Node{Type: "paragraph", Children: []*Node{{Type: "text", Text: text}}}
	}
	definition := func(label string) *Node {
		return &Node{Type: "footnote_definition", Attrs: Attrs{"label": label}, Children: []*Node{paragraph("x")}}
	}
	labels := strings.Repeat("[^a", (4<<20)/3) + "]"
	for _, test := range []struct {
		name string
		doc  *Node
	}{
		{"a run of *", &Node{Type: "doc", Children: []*Node{paragraph(strings.Repeat("*", 4<<20))}}},
		{"a run of _", &Node{Type: "doc", Children: []*Node{paragraph(strings.Repeat("_", 4<<20))}}},
		{"a run of ~", &Node{Type: "doc", Children: []*Node{paragraph(strings.Repeat("~", 4<<20))}}},
		{"[^a before one ] with no footnote", &Node{Type: "doc", Children: []*Node{paragraph(labels)}}},
		{"[^a before one ] with a short label defined", &Node{Type: "doc", Children: []*Node{paragraph(labels), definition("a")}}},
		{"[^a before one ] with a long label defined", &Node{Type: "doc", Children: []*Node{paragraph(labels), definition(strings.Repeat("b", 1000))}}},
		{"line feeds", &Node{Type: "doc", Children: []*Node{paragraph(strings.Repeat("a\n", 2<<20))}}},
	} {
		started := time.Now()
		if _, err := Render(test.doc); err != nil {
			t.Fatalf("render 4 MiB of %s: %v", test.name, err)
		}
		if elapsed := time.Since(started); elapsed > 30*time.Second {
			t.Errorf("render of 4 MiB of %s took %s, want under 30 s", test.name, elapsed)
		}
	}
	started := time.Now()
	if _, err := Parse("a" + strings.Repeat("~", 1<<20)); err != nil {
		t.Fatalf("parse a run of 1 MiB of ~: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Errorf("parse of a run of 1 MiB of ~ took %s, want under 30 s", elapsed)
	}
}

// A table whose every row holds an escaped pipe in a code span parses in time linear in its size,
// up to the 1 MiB a document may be. goldmark's table transformer checked every code span's text
// against every escaped pipe in the document: on one machine at a load of about 300, 16 KiB of such
// rows parsed in 35 ms and 256 KiB in 6.8 s, and 1 MiB took 2 min 9 s. unescapeTablePipes,
// which replaced it, parsed 16 KiB in 19 ms, 256 KiB in 0.6-0.8 s and 1 MiB in 2.0-2.4 s there. No
// wall-clock bound holds on every runner, so the parse is timed at growing sizes and its growth
// bounded (growsLinearly). A caller's write of a table that size passes the element limit and is
// refused, so it is read as a rendering is, whose parse no element limit stops.
func TestParseIsLinearInATableOfEscapedPipes(t *testing.T) {
	const header, row = "| a |\n| --- |\n", "`\\|`\n"
	growsLinearly(t, "parsing a table of `\\|` cells", []int{16 << 10, 256 << 10, 1 << 20}, func(size int) time.Duration {
		markdown := header + strings.Repeat(row, (size-len(header))/len(row))
		started := time.Now()
		if _, err := ParseRendering(markdown); err != nil {
			t.Fatalf("parse %d bytes of `\\|` rows: %v", size, err)
		}
		return time.Since(started)
	})
}

// growthAllowance is how many times faster than its input a parse's time may grow from one size
// to the next. Linear time grows at most as fast as the input, less while a fixed cost counts, and
// quadratic time as the input's square. Four times the input's growth lets a runner's load
// quadruple between two parses and still refuses a quadratic one at any step over four.
const growthAllowance = 4

// growsLinearly times what at each of sizes in turn and fails t at the first size whose time grew
// more than growthAllowance times faster than the size did. The first size is timed three times
// before the second and three times after it, and the fastest of the six kept, so neither a first
// run's warm-up nor a load spike shorter than the second size's run reads as growth. A step over
// the bound times the larger size again and keeps the faster time, and after the first step also
// times the smaller size again and keeps the slower, so load that rose between two runs does not
// read as growth either.
func growsLinearly(t *testing.T, what string, sizes []int, timed func(size int) time.Duration) {
	t.Helper()
	first := func() time.Duration { return min(timed(sizes[0]), timed(sizes[0]), timed(sizes[0])) }
	before := first()
	for i := 1; i < len(sizes); i++ {
		smaller, size := sizes[i-1], sizes[i]
		growth := size / smaller
		took := timed(size)
		if i == 1 {
			before = min(before, first())
		}
		if took > time.Duration(growthAllowance*growth)*before {
			took = min(took, timed(size))
			if i > 1 {
				before = max(before, timed(smaller))
			}
		}
		ratio := float64(took) / float64(before)
		if took > time.Duration(growthAllowance*growth)*before {
			t.Fatalf("%s took %s at %s, %.0f times the %s it took at %s: the input grew %d times, and a parse linear in it may grow %d times at most (growthAllowance)",
				what, took.Round(time.Millisecond), sizeText(size), ratio, before.Round(time.Millisecond), sizeText(smaller), growth, growthAllowance*growth)
		}
		t.Logf("%s: %s at %s, %s at %s, %.1f times for an input %d times larger (at most %d)", what, before.Round(time.Millisecond), sizeText(smaller), took.Round(time.Millisecond), sizeText(size), ratio, growth, growthAllowance*growth)
		before = took
	}
}

func sizeText(size int) string {
	if size >= 1<<20 && size%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", size>>20)
	}
	return fmt.Sprintf("%d KiB", size>>10)
}
