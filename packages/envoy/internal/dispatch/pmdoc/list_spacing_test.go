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
			name:    "an empty list item before a paragraph its outer item holds",
			refused: "- t\n\n  -\n\n  para\n",
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
