package pmdoc

import (
	"slices"
	"testing"
)

// A replacement spliced into a typed block is fitted inside it, as ProseMirror fits one into a
// callout (the engine oracle's callout-paragraph-and-code case). The fit never goes on to the
// block's parent, where the only fit replaces the block and everything it holds: an ask given a
// code block keeps its id and its place, holding what it cannot parse, for the document service's
// ask-block check to refuse. ProseMirror would instead split the ask, closing it after the paragraph
// and putting the code block after it, which no oracle case pins because Dispatch refuses the write.
func TestSpliceKeepsAnAskItsReplacementDoesNotFit(t *testing.T) {
	doc, err := Parse("Intro.\n\n:::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich one?\n:::\n\nAfter.\n")
	if err != nil {
		t.Fatal(err)
	}
	r, err := FindQuote(doc, "Which one?", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	with, err := Parse("Which?\n\n```\ncode\n```\n")
	if err != nil {
		t.Fatal(err)
	}

	got, err := Splice(doc, r, with)

	if err != nil {
		t.Fatalf("Splice() error = %v", err)
	}
	ask := got.Children[1]
	var children []string
	for _, child := range ask.Children {
		children = append(children, child.Type)
	}
	if ask.Type != "ask" || ask.Attrs[BlockIDAttr] != "a1" || !slices.Equal(children, []string{"paragraph", "code_block"}) {
		t.Fatalf("Splice() second block = %s %v holding %v, want ask a1 holding the paragraph and the code block",
			ask.Type, ask.Attrs, children)
	}
}

// A replacement that is one typed block of the same type as the typed block it lands in is that
// block rewritten, the way the dispatch skill tells a writer to edit one (the same id, the new
// text), so the fit replaces the block rather than nesting the rewrite inside it: the document
// holds the id once, as main's server stores it. ProseMirror would leave the old ask emptied
// beside the new one, under the same id, which the write's block-id check would refuse.
func TestSpliceRewritesATypedBlockAReplacementOfItsTypeCovers(t *testing.T) {
	for _, test := range []struct{ name, markdown, quote, replacement, want string }{
		{name: "an ask's whole question",
			markdown:    "Intro.\n\n:::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich one?\n:::\n",
			quote:       "Which one?",
			replacement: ":::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich one, first?\n:::\n",
			want:        "Intro.\n\n:::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich one, first?\n:::\n"},
		{name: "a callout's only paragraph",
			markdown:    "Intro.\n\n:::callout{#c1 kind=\"note\" title=\"\"}\nA note.\n:::\n",
			quote:       "A note.",
			replacement: ":::callout{#c1 kind=\"warning\" title=\"\"}\nReworded.\n:::\n",
			want:        "Intro.\n\n:::callout{#c1 kind=\"warning\" title=\"\"}\nReworded.\n:::\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			doc, err := Parse(test.markdown)
			if err != nil {
				t.Fatal(err)
			}
			r, err := FindQuote(doc, test.quote, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			with, err := Parse(test.replacement)
			if err != nil {
				t.Fatal(err)
			}

			got, err := Splice(doc, r, with)

			if err != nil {
				t.Fatalf("Splice() error = %v", err)
			}
			if rendered, err := Render(got); err != nil || rendered != test.want {
				t.Fatalf("Splice() = %q (%v), want %q", rendered, err, test.want)
			}
		})
	}
}
