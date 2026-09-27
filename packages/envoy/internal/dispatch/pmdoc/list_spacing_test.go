package pmdoc

import (
	"errors"
	"testing"
)

// A document holding a shape only the browser editor's parser and this one read alike is refused
// where this parser cannot read its lists or blocks as that parser does, and the same text
// without such a shape still parses, since goldmark's looseness is how Dispatch has read it. The
// spacing it reads, in quotes and footnote definitions included, is the list-spacing-in-containers
// and footnote-placement corpus documents', which the fixtures compare with the engine's reading.
func TestBrowserListSpacingRefusesWhatItCannotRead(t *testing.T) {
	for _, test := range []struct {
		name string
		// refused holds a shape only the browser editor and this parser read alike; readable, when
		// set, is the same text without one.
		refused, readable string
	}{
		{
			name:     "a typed block holding a blank line in a list item",
			refused:  "-\n\n- a\n  :::callout{#c1 kind=\"note\" title=\"T\"}\n  one\n\n  two\n  :::\n",
			readable: "- a\n  :::callout{#c1 kind=\"note\" title=\"T\"}\n  one\n\n  two\n  :::\n",
		},
		{
			// A definition nothing refers to gets no backlink from goldmark, and is the same shape.
			name:     "a typed block holding a blank line in a list item, beside a definition nothing refers to that ends in a list",
			refused:  "- a\n  :::callout{#c1 kind=\"note\" title=\"T\"}\n  one\n\n  two\n  :::\n\n[^u]: - one\n",
			readable: "- a\n  :::callout{#c1 kind=\"note\" title=\"T\"}\n  one\n\n  two\n  :::\n\n[^u]: one\n",
		},
		{
			// A footnote definition's lines are the lines of the item that holds it.
			name:     "a typed block holding a blank line in a footnote definition a list item holds",
			refused:  "ref[^n] here.\n\n- [^n]: :::callout{#c1 kind=\"note\" title=\"T\"}\n\n      :::\n",
			readable: "ref[^n] here.\n\n- [^n]: :::callout{#c1 kind=\"note\" title=\"T\"}\n      :::\n",
		},
		{
			// The typed block's rule holds with a quote inside it: the browser editor reads a blank
			// line after the list as spacing its last item by what follows.
			name:     "a blank line after a list in a quote in a typed block in a footnote definition",
			refused:  "x[^n]\n\n[^n]: :::callout{#c1 kind=\"note\" title=\"T\"}\n    > - c\n    >\n    > p\n    :::\n",
			readable: "x[^n]\n\n[^n]: :::callout{#c1 kind=\"note\" title=\"T\"}\n    > - c\n    > p\n    :::\n",
		},
		{
			// Goldmark ends the quote and the items around the empty item there, and re-opens the
			// quote for the next line, which the browser editor keeps in the inner list.
			name:    "an empty list item followed by a blank line in a list in a quote in a list item",
			refused: "- > 1. - a\n  >\n  >    -\n  >\n  >    - c\n",
		},
		{
			// Every blank line goes on with the quote around the outer item, so the browser editor
			// keeps the next block in that item, whether its text or a quote opening at the item's
			// content column.
			name:    "an empty list item followed by a blank quote line in a list item in two quotes",
			refused: "> > - a\n> >\n> >   -\n> >\n> >   x\n",
		},
		{
			// The empty item's content starts on the next line, so the ordered list is the outer
			// item's, and so is the paragraph after the blank line.
			name:    "an empty list item holding a list on its next line, then a blank line and a paragraph",
			refused: "-\n  1.\n\n  para\n",
		},
		{
			name:    "an empty list item followed by a blank quote line and a quote in the list item around it",
			refused: "> - a\n>\n>   -\n>\n>   > x\n",
		},
		{
			// A quote standing between the definition and the typed block keeps the typed block's
			// refusal: the browser editor reads the blank line as spacing the list.
			name:    "a blank line after a list in a typed block in a quote in a footnote definition",
			refused: "x[^n]\n\n[^n]: > :::callout{#c1 kind=\"note\" title=\"T\"}\n    > - b\n    >\n    > a\n    > :::\n",
		},
		{
			// With no shape the browser editor alone reads, goldmark's looseness spreads the item
			// holding two blocks and leaves "b", which a blank line follows, unspread; the browser
			// editor spreads "b" by what follows it.
			name:    "a blank line after a list in a typed block in a footnote definition, in a document holding no browser-only shape",
			refused: "x[^n]\n\n[^n]: :::callout{#c1 kind=\"note\" title=\"T\"}\n    - a\n\n      > q\n    - b\n\n    a\n    :::\n    tail\n",
		},
		{
			name:    "an empty list item before a paragraph its outer item holds",
			refused: "- t\n\n  -\n\n  para\n",
		},
		{
			// A definition's later lines start four columns past its quote's content, not past its
			// marker, so "- c" stands inside the item "a" as both parsers measure it.
			name:    "an empty list item in an item opening a definition indented in its quote",
			refused: ">   [^n]: - a\n>\n>       -\n>\n>       - c\n",
		},
		{
			name:    "the same, indented by a tab after the quote's marker",
			refused: ">\t[^n]: - a\n>\n>\t    -\n>\n>\t    - c\n",
		},
		{
			name:    "a blank line at the end of a quote holding a footnote definition, after a list",
			refused: "x[^1]\n\n> [^1]: t\n>\n>     - a\n>\n\nAfter.\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Parse(test.refused); !errors.Is(err, ErrSchema) {
				t.Fatalf("Parse(%q) = %v, want a schema refusal", test.refused, err)
			}
			if test.readable == "" {
				return
			}
			if _, err := Parse(test.readable); err != nil {
				t.Fatalf("Parse(%q): %v", test.readable, err)
			}
		})
	}
}
