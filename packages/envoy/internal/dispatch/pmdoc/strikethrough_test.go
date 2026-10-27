package pmdoc

import (
	"fmt"
	"strings"
	"testing"
)

// A comment is drawn by the browser's headless engine, whose strikethrough
// (micromark-extension-gfm-strikethrough) pairs a `~` run only with a closer of the same length,
// 1 or 2, lets a run directly after an escaped `~` open or close, and pairs `~` runs and `*`/`_`
// runs in separate passes: the kind whose run was tokenized first goes first at a textblock's top
// level, and `~` goes first inside a link's label. A document is read by Go, so each row is
// markdown and the reading the engine gives it, each textblock's text and marks (readingSummary),
// asserted for every reader a document is read with - so a run without the engine still checks
// them - and Go's Parse is then compared with the engine's own reading. A row marked blocks is not
// one paragraph (a table, a heading, a quote, a footnote definition), which ParseInline refuses, so
// only the block readers read it. A `~` written `\&#126;` reads otherwise for a reason of its own
// (dispatch://INFRA-536) and is left out.
func TestTildeRunsReadAsTheEngineReadsThem(t *testing.T) {
	tests := []struct {
		markdown, want string
		blocks         bool
	}{
		// The cases dispatch://INFRA-534 was filed for, and a pair of each length.
		{`Plain ~a~~ text.`, `paragraph: "Plain ~a~~ text."`, false},
		{`Plain ~~a~ text.`, `paragraph: "Plain ~~a~ text."`, false},
		{`Plain ~\~~ end.`, `paragraph: "Plain " | "~" strike_through | " end."`, false},
		{`Plain ~~a~~ text.`, `paragraph: "Plain " | "a" strike_through | " text."`, false},
		{`Plain ~a~ text.`, `paragraph: "Plain " | "a" strike_through | " text."`, false},
		// A closer pairs with the nearest opener of its own length, passing runs of the other; a run
		// of three is text, and so is a `~~~` opening no fence.
		{`~~a ~b~~ c~`, `paragraph: "a ~b" strike_through | " c~"`, false},
		{`~a ~~b~~ c~`, `paragraph: "a b" strike_through | " c"`, false},
		{`~a~~b~`, `paragraph: "a~~b" strike_through`, false},
		{`~a~~ ~~b~`, `paragraph: "a~~ ~~b" strike_through`, false},
		{`~~a~~~`, `paragraph: "~~a~~~"`, false},
		{`b ~~~a~~~`, `paragraph: "b ~~~a~~~"`, false},
		{"| h |\n| --- |\n| ~a~~ |", `paragraph: "h" / paragraph: "~a~~"`, true},
		{`# ~a~~`, `heading: "~a~~"`, true},
		{`[~a~~](u)`, `paragraph: "~a~~" link(u)`, false},
		{`![~a~~](u)`, `paragraph: image "~a~~"`, false},
		{`[~\~~](u)`, `paragraph: "~" link(u) strike_through`, false},
		// An escaped `~` is text, and a run directly after it may open or close; a `\` that is
		// itself escaped escapes nothing.
		{`\~~a~~`, `paragraph: "~~a~~"`, false},
		{`a\~~~b~~`, `paragraph: "a~" | "b" strike_through`, false},
		{`~\~~~`, `paragraph: "~~~~"`, false},
		{`x ~\~~ y ~\~~`, `paragraph: "x " | "~" strike_through | " y " | "~" strike_through`, false},
		{`\\~~a~~`, `paragraph: "\\" | "a" strike_through`, false},
		{`\\\~~a~`, `paragraph: "\\~" | "a" strike_through`, false},
		{`\\\~~a~~`, `paragraph: "\\~~a~~"`, false},
		{`\\\\~~a~~`, `paragraph: "\\\\" | "a" strike_through`, false},
		{`\~\~~a~~`, `paragraph: "~~~a~~"`, false},
		{`\~~~a~~ b`, `paragraph: "~" | "a" strike_through | " b"`, false},
		{"| h |\n| --- |\n| ~\\~~ |", `paragraph: "h" / paragraph: "~" strike_through`, true},
		// A `~` written as a character reference is text and never a run.
		{`&#126;~a~`, `paragraph: "~" | "a" strike_through`, false},
		{`~&#126;~`, `paragraph: "~" strike_through`, false},
		{`~a~&#126;`, `paragraph: "a" strike_through | "~"`, false},
		// The order the two kinds pair in: the kind whose run was tokenized first, where a run that
		// can neither open nor close, one in a link's label and one after an escaped `~` count, and
		// one inside a www URL or a defined footnote call's label does not.
		{`*a ~b* c~`, `paragraph: "a ~b" emphasis | " c~"`, false},
		{`~a *b~ c*`, `paragraph: "a *b" strike_through | " c*"`, false},
		{`x* ~~a*b~~c*`, `paragraph: "x* ~~a" | "b~~c" emphasis`, false},
		{`~ x* ~~a*b~~c*`, `paragraph: "~ x* " | "a*b" strike_through | "c*"`, false},
		{`[*q*](u) ~~a*b~~c*`, `paragraph: "q" emphasis link(u) | " ~~a" | "b~~c" emphasis`, false},
		{`see www.a.com/~x* ~~a*b~~c*`, `paragraph: "see " | "www.a.com/~x" link(http://www.a.com/~x) | "* ~~a" | "b~~c" emphasis`, false},
		{`[^a_b] ~~a*b~~c*`, `paragraph: "[^a_b] ~~a" | "b~~c" emphasis`, false},
		{"[^a_b] ~~a*b~~c*\n\n[^a_b]: n", `paragraph: footnote "a_b" | " " | "a*b" strike_through | "c*" / paragraph: "n"`, true},
		{`\~~ x* ~~a*b~~c*`, `paragraph: "~~ x* " | "a*b" strike_through | "c*"`, false},
		{`> **\~~](u)![_![~_`, `paragraph: "**~~](u)![" | "![~" emphasis`, true},
		{`[**~~**d**~~**](u)`, `paragraph: "d" link(u) strike_through strong`, false},
		// A `**` run directly after a `~~` opener, inside a `*` pair: main read these as the engine
		// does (the `**` stays text, struck inside the emphasis), so pairing in separate passes must
		// not pair the `**` with the outer closer.
		{`*~~**b~~*`, `paragraph: "**b" emphasis strike_through`, false},
		{`p *~~**a~~* q`, `paragraph: "p " | "**a" emphasis strike_through | " q"`, false},
		{`*x ~~**b~~ y*`, `paragraph: "x " emphasis | "**b" emphasis strike_through | " y" emphasis`, false},
		{`*[~~**é~~*`, `paragraph: "[" emphasis | "**é" emphasis strike_through`, false},
		// A `*` run beside a `~` opens or closes toward it, as beside another `*` (micromark's
		// attentionMarkers list `~`), so the rule of three keeps a `**` that can both open and
		// close from a single `*`.
		{`Plain b*~* text.`, `paragraph: "Plain b" | "~" emphasis | " text."`, false},
		{`Plain ~**b<* end.`, `paragraph: "Plain ~**b<* end."`, false},
		{`Plain *b**~~ end.`, `paragraph: "Plain *b**~~ end."`, false},
	}
	readers := []struct {
		name  string
		parse func(string) (*Node, error)
	}{
		{"Parse", Parse},
		{"ParseRendering", ParseRendering},
		{"ParseFragment", func(markdown string) (*Node, error) { return ParseFragment(markdown, false, NewWriteBudget()) }},
	}
	for _, test := range tests {
		t.Run(test.markdown, func(t *testing.T) {
			for _, reader := range readers {
				doc, err := reader.parse(test.markdown)
				if err != nil {
					t.Errorf("%s(%q) = %v", reader.name, test.markdown, err)
					continue
				}
				if got := readingSummary(doc); got != test.want {
					t.Errorf("%s(%q) reads\n\t%s\nwant\n\t%s", reader.name, test.markdown, got, test.want)
				}
			}
			if test.blocks {
				return
			}
			nodes, err := ParseInline(test.markdown, NewWriteBudget())
			if err != nil {
				t.Errorf("ParseInline(%q) = %v", test.markdown, err)
				return
			}
			if got := "paragraph: " + markedSummary(nodes); got != test.want {
				t.Errorf("ParseInline(%q) reads\n\t%s\nwant\n\t%s", test.markdown, got, test.want)
			}
		})
	}
	t.Run("the engine", func(t *testing.T) {
		markdowns := make([]string, 0, len(tests))
		for _, test := range tests {
			markdowns = append(markdowns, test.markdown)
		}
		readings := engineReadings(t, markdowns)
		for index, markdown := range markdowns {
			doc, err := Parse(markdown)
			if err != nil {
				t.Errorf("Parse(%q) = %v", markdown, err)
				continue
			}
			if readings[index] == nil {
				t.Errorf("the engine does not read %q", markdown)
				continue
			}
			if !doc.Equal(readings[index]) {
				t.Errorf("%q: Go reads\n\t%s\nthe engine\n\t%s", markdown, readingSummary(doc), readingSummary(readings[index]))
			}
		}
	})
}

// readingSummary is a document's reading, each textblock's inline nodes in document order
// (markedSummary), separated by " / ".
func readingSummary(doc *Node) string {
	if doc == nil {
		return "no reading"
	}
	var blocks []string
	var walk func(*Node)
	walk = func(node *Node) {
		if len(node.Children) > 0 && isInlineNodeType(node.Children[0].Type) {
			blocks = append(blocks, node.Type+": "+markedSummary(node.Children))
			return
		}
		for _, child := range node.Children {
			walk(child)
		}
	}
	walk(doc)
	return strings.Join(blocks, " / ")
}

// markedSummary is inline nodes as inlineSummary gives them, with every mark: a text's text, an
// image's alt, a footnote call's label, each followed by its marks and a link's href.
func markedSummary(nodes []*Node) string {
	var parts []string
	for _, node := range nodes {
		var part string
		switch node.Type {
		case "text":
			part = fmt.Sprintf("%q", node.Text)
		case "image":
			part = fmt.Sprintf("image %q", node.Attrs["alt"])
		case "footnote_reference":
			part = fmt.Sprintf("footnote %q", node.Attrs["label"])
		default:
			part = node.Type
		}
		for _, mark := range node.Marks {
			if mark.Type == "link" {
				part += fmt.Sprintf(" link(%s)", mark.Attrs["href"])
				continue
			}
			part += " " + mark.Type
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, " | ")
}
