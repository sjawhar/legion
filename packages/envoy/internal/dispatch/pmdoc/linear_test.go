package pmdoc

import (
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
