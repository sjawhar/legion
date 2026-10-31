package pmdoc

import (
	"errors"
	"os"
	"strings"
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
			// Goldmark ends every item around the empty one at the blank line, so a block the
			// browser editor keeps in an item further out is refused too.
			name:    "an empty list item followed by a blank quote line and the next item of a list further out",
			refused: "> 1. ---\n>    2. *\n>\n>    3. Setext\n",
		},
		{
			name:    "an empty list item in a quote in a list item, then blank quote lines the outer item holds",
			refused: "2. >\t10. # heading\n   >\t11. 1.\n   >\n   >\n",
		},
		{
			name:    "an empty list item in a quoted item, then that quote's next item in the list item around it",
			refused: "3. > - \t-\n   >\n   > - \t\n   >\n   >\n",
		},
		{
			name:    "an empty list item in a quote in a list item, then a quote that item holds after a blank line",
			refused: "1. > 2. Setext\n   > * *\n   >\n\n   >\n",
		},
		{
			name:    "an empty list item in a list item in a list item, then a footnote definition the outermost holds",
			refused: "* - 1.\n\n  [^m]:     indented code\n",
		},
		{
			// An item's content starts one column past its marker where its marker line holds
			// nothing, or indented code, however far in its first block stands.
			name:    "an empty list item in an item whose marker line holds nothing, then that item's next list item",
			refused: "2. \n     ===\n\n   - 1.\n\n   - *\n",
		},
		{
			name:    "an empty list item in a quote in an item opening with indented code, then a paragraph the quote holds",
			refused: "*     indented code\n  > ===\n\n  > 1.\n  >\n  > text a\n",
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
		{
			// The typed block's content is a document of its own to the browser editor's parser,
			// and the quote inside it ends where that content does.
			name:     "a blank line after a list in a quote in a typed block in a quote",
			refused:  "- > q\n\n> :::callout{#c1 kind=\"note\" title=\"T\"}\n> > - a\n> >\n>\n> :::\n",
			readable: "> :::callout{#c1 kind=\"note\" title=\"T\"}\n> > - a\n> >\n>\n> :::\n",
		},
		{
			name:     "a blank line after a list in a typed block in a quote in a typed block",
			refused:  "- > q\n\n:::callout{#c1 kind=\"note\" title=\"T\"}\n> :::callout{#c2 kind=\"note\" title=\"T\"}\n> - a\n>\n>\n> :::\n:::\n",
			readable: ":::callout{#c1 kind=\"note\" title=\"T\"}\n> :::callout{#c2 kind=\"note\" title=\"T\"}\n> - a\n>\n>\n> :::\n:::\n",
		},
		{
			// The list's quote stands in an item of a list in the typed block, which stands in a
			// quote; that item opening with a quote is itself a shape the browser editor alone reads.
			name:    "a blank line after a list in a quote in a list item in a typed block in a quote",
			refused: "> :::callout{#c1 kind=\"note\" title=\"T\"}\n> 1. > 1.\n>    >\n>\n> :::\n",
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

// The corpus-errors documents whose list item the browser editor spreads otherwise than this parser
// would store it are refused by every reader of caller markdown: the blank lines after a quoted item
// ending in a footnote definition whose typed block holds a fence no fence closed, which that
// parser reads as the code's, and a definition inside a typed block with a list item around it,
// whose lines and the blank lines next to it that parser counts otherwise.
func TestCorpusErrorsOfItemSpreadReadOtherwiseAreRefused(t *testing.T) {
	const (
		fenceBlanks     = "blank lines after a list item in a quote ending in a footnote definition whose typed block holds a fenced code block no fence closed"
		typedDefinition = "a footnote definition inside a typed block with a list item around it"
	)
	for name, refusal := range map[string]string{
		"quoted-star-item-definition-callout-unclosed-fence-then-blank-lines":                     fenceBlanks,
		"quoted-star-item-unspaced-definition-callout-unclosed-fence-then-blank-lines":            fenceBlanks,
		"quoted-star-item-definition-unspaced-callout-unclosed-fence-then-blank-lines":            fenceBlanks,
		"quoted-item-definition-callout-unclosed-fence-then-blank-lines":                          fenceBlanks,
		"quoted-item-definition-callout-unclosed-fence-then-blank-lines-and-text":                 fenceBlanks,
		"quoted-item-definition-callout-unclosed-tilde-fence-holding-code-then-blank-lines":       fenceBlanks,
		"ordered-item-callout-holding-a-definition-opening-on-a-later-line":                       typedDefinition,
		"item-callout-holding-a-definition-opening-on-a-later-line":                               typedDefinition,
		"item-text-then-callout-holding-a-definition-opening-on-a-later-line":                     typedDefinition,
		"referenced-item-callout-holding-a-definition-opening-on-a-later-line":                    typedDefinition,
		"item-unclosed-callout-holding-a-definition-opening-on-a-later-line":                      typedDefinition,
		"quoted-item-callout-holding-a-definition-opening-on-a-later-line":                        typedDefinition,
		"item-callout-opening-with-a-blank-line-before-a-definition":                              typedDefinition,
		"item-callout-opening-with-a-blank-line-before-an-empty-definition-at-the-end":            typedDefinition,
		"item-callout-definition-then-blank-line-and-text":                                        typedDefinition,
		"ordered-item-nested-callout-opening-with-a-blank-line-before-a-definition":               typedDefinition,
		"ordered-item-nested-callout-opening-with-a-whitespace-line-before-a-rule-and-definition": typedDefinition,
		"item-callout-quote-then-two-blank-lines-before-a-list-and-definition":                    typedDefinition,
		"quoted-item-callout-empty-definition-before-text":                                        typedDefinition,
		"quoted-callout-quoted-item-empty-definition-before-text":                                 typedDefinition,
		"quoted-ordered-item-callout-quoted-item-empty-definition-before-text":                    typedDefinition,
		"quoted-callout-item-definition-then-blank-line-and-text":                                 typedDefinition,
		"quoted-callout-item-empty-definition-before-next-item":                                   typedDefinition,
	} {
		markdown, err := os.ReadFile("testdata/corpus-errors/" + name + ".md")
		if err != nil {
			t.Fatalf("read %s corpus: %v", name, err)
		}
		for _, reader := range blockMarkdownReaders() {
			if _, err := reader.parse(string(markdown)); !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), refusal) {
				t.Errorf("%s of %s: %v, want ErrSchema saying %q", reader.name, name, err, refusal)
			}
		}
	}
}

// A list inside a footnote definition inside a typed block, with a blank line in its first item, is
// one the browser editor reads with that item spread, as it reads the same definition outside any
// typed block. A document write either refuses it or reads the item spread, so the blank line the
// writer put there is kept; storing the item tight drops it, which the writer before
// sjawhar/legion#2403 avoided by refusing every
// footnote definition in a typed block (INFRA-581).
func TestAListInADefinitionInATypedBlockIsReadSpreadAsTheBrowserReadsItOrRefused(t *testing.T) {
	for _, markdown := range []string{
		":::callout{}\n[^f]: - a\n\n    - b\n:::\n\nText[^f].\n",
		":::callout{}\n[^f]:\n    - x\n      \n      note\n:::\n\nText[^f].\n",
		":::callout{}\n[^g]:1. \n       ***\n    \n       ***\n:::\n\nText[^g].\n",
	} {
		doc, err := ParseForWrite(markdown, nil)
		if errors.Is(err, ErrSchema) {
			continue
		}
		if err != nil {
			t.Errorf("ParseForWrite(%q): %v", markdown, err)
			continue
		}
		item := firstNodeOfType(doc, "list_item")
		if item == nil {
			t.Errorf("ParseForWrite(%q) read no list item", markdown)
			continue
		}
		if spread, _ := item.Attrs["spread"].(bool); !spread {
			t.Errorf("ParseForWrite(%q) stores its first list item tight; the browser editor reads it spread, so a write must read it spread or refuse it", markdown)
		}
	}
}
