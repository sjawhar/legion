package pmdoc

import (
	"errors"
	"testing"
)

// A document holding a shape only the browser editor's parser and this one read alike is refused
// where this parser cannot read its lists or blocks as that parser does, and the same text
// without such a shape still parses, since goldmark's looseness is how Dispatch has read it.
func TestBrowserListSpacingRefusesWhatItCannotRead(t *testing.T) {
	for _, test := range []struct {
		name string
		// refused holds a shape only the browser editor and this parser read alike; readable, when
		// set, is the same text without one.
		refused, readable string
	}{
		{
			name:     "a blank line between a quote's list items",
			refused:  "-\n\n> - a\n>\n> - b\n",
			readable: "> - a\n>\n> - b\n",
		},
		{
			name:     "two blank lines in a quote after a list",
			refused:  "-\n\n> - a\n>\n>\n> After.\n",
			readable: "> - a\n>\n>\n> After.\n",
		},
		{
			name:     "a blank line in a quote before a quote after a list",
			refused:  "-\n\n> - a\n>\n> > q\n",
			readable: "> - a\n>\n> > q\n",
		},
		{
			name:     "a blank line between a footnote definition's list items",
			refused:  "x[^1]\n\n[^1]: t\n\n    - a\n\n    - b\n",
			readable: "x[^1]\n\n[^1]: t\n\n    - a\n\n    - b\n\n    End.\n",
		},
		{
			name:     "a blank line before a quote after a footnote definition's list",
			refused:  "x[^1]\n\n[^1]: t\n\n    - a\n\n    > q\n",
			readable: "x[^1]\n\n[^1]: t\n\n    - a\n\n    > q\n\n    End.\n",
		},
		{
			name:     "a typed block holding a blank line in a list item",
			refused:  "-\n\n- a\n  :::callout{#c1 kind=\"note\" title=\"T\"}\n  one\n\n  two\n  :::\n",
			readable: "- a\n  :::callout{#c1 kind=\"note\" title=\"T\"}\n  one\n\n  two\n  :::\n",
		},
		{
			name:    "an empty list item before a paragraph its outer item holds",
			refused: "- t\n\n  -\n\n  para\n",
		},
		{
			name:     "a footnote definition inside a quote",
			refused:  "> x[^1]\n>\n> [^1]: t\n>\n>     - a\n",
			readable: "> x[^1]\n>\n> [^1]: t\n",
		},
		{
			name:     "a block after a footnote definition",
			refused:  "x[^1]\n\n[^1]: t\n\n    - a\n\nAfter.\n",
			readable: "x[^1]\n\n[^1]: t\n\nAfter.\n",
		},
		{
			name:     "a footnote definition nothing refers to",
			refused:  "-\n\n[^q]: t\n",
			readable: "Text.\n\n[^q]: t\n",
		},
		{
			name:     "footnote definitions out of the order of their references",
			refused:  "x[^2] y[^1]\n\n[^1]: a\n\n    - l\n\n[^2]: b\n",
			readable: "x[^2] y[^1]\n\n[^1]: a\n\n[^2]: b\n",
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
