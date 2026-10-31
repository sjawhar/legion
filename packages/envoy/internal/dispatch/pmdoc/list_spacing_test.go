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

// The blank lines that end a typed block in a quoted list item are no blank lines of the item to
// the browser editor's parser where a list the block ends with takes them - it ends that list past
// the quotes' markers on the line after it - or where the block, closed by no fence, ends before
// the last of them, which ends the item (typedEndBlankInItem). Each document below carries the
// outer item's spread that parser was measured reading for it, in round 2 of PR #2507, which has
// this reader store them again: the reader before that round refused every one of them, where the
// merged base stored them and read them as that parser does. The corpus holds them as well, in
// quoted-item-typed-blocks-ending-in-blank-lines and
// quoted-item-typed-block-code-ending-in-a-blank-line, which lock the tree that parser reads;
// this test names the spread those documents turn on.
func TestQuotedItemsAreTightWhereTheirTypedBlocksEndInBlankLines(t *testing.T) {
	for _, test := range []struct {
		name     string
		markdown string
	}{
		{"a list the block ends with, before its closing fence", "> - a\n>   :::callout{#c1 kind=\"note\" title=\"T\"}\n>   - x\n>\n>   :::\n"},
		{"the same, with nothing in the document past the block", "> - a\n>   :::callout{#c1 kind=\"note\" title=\"T\"}\n>   - x\n>\n"},
		{"indented code, with nothing in the document past the block", "> - a\n>   :::callout{#c1 kind=\"note\" title=\"T\"}\n>       code\n>\n"},
		{"a list the block ends with, before a line of markers and spaces", "> - a\n>   :::callout{#c1 kind=\"note\" title=\"T\"}\n>   - x\n>   \n>   :::\n"},
		{"a list the block ends with, before two blank lines", "> - a\n>   :::callout{#c1 kind=\"note\" title=\"T\"}\n>   - x\n>\n>\n>   :::\n"},
		{"a list holding a list", "> - a\n>   :::callout{#c1 kind=\"note\" title=\"T\"}\n>   - x\n>     - y\n>\n>   :::\n"},
		{"a list holding a lazy line of its own", "> - a\n>   :::callout{#c1 kind=\"note\" title=\"T\"}\n>   - x\n>         code\n>\n>   :::\n"},
		{"a list in a typed block no fence closes inside the block", "> - a\n>   ::::callout{#c1 kind=\"note\" title=\"T\"}\n>   :::callout{#c2 kind=\"note\" title=\"T\"}\n>   - x\n>\n>\n>   ::::\n"},
		{"a list the block ends with, in two quotes", "> > - a\n> >   :::callout{#c1 kind=\"note\" title=\"T\"}\n> >   - x\n> >\n> >   :::\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			doc, err := ParseForWrite(test.markdown, nil)
			if err != nil {
				t.Fatalf("ParseForWrite(%q): %v", test.markdown, err)
			}
			item := firstNodeOfType(doc, "list_item")
			if item == nil {
				t.Fatalf("ParseForWrite(%q) read no list item", test.markdown)
			}
			if spread, _ := item.Attrs["spread"].(bool); spread {
				t.Errorf("ParseForWrite(%q) reads its outer item spread, want tight, as the browser editor's parser reads it", test.markdown)
			}
		})
	}
}

// A list in a quote is spread by where the browser editor's parser puts the list's exit event and
// how many line endings it crosses walking back from there: an exit after a quote marker leaves
// the blank line's ending and the item's last line ending behind it, so one blank line spreads the
// list, and an exit at the start of a line needs two. Each document below carries the spread that
// parser was measured reading for it (PR #2507), and the list under test is each document's first.
func TestQuotedListsAreSpreadWhereTheBrowserEditorsExitLands(t *testing.T) {
	for _, test := range []struct {
		name     string
		markdown string
		spread   bool
	}{
		{
			// The list's quote ends where the typed block's content starts on the line "> a", past
			// the marker of the quote around the typed block, so one blank line spreads the list.
			name:     "a list in a quote in a typed block in a quote, then a blank line and a line of the typed block",
			markdown: "> :::callout{}\n> > - x\n> >\n> a\n",
			spread:   true,
		},
		{
			name:     "the same, the list's quote three deep and no space after the markers",
			markdown: ">:::callout{}\n>>>- x\n>>>\n>a\n",
			spread:   true,
		},
		{
			// The typed block's fence is no line of its content, so the list ends before it and
			// one blank line only ends the list's quote.
			name:     "the same, one blank line before the typed block's fence",
			markdown: "> :::callout{}\n> > - x\n> >\n> :::\n",
			spread:   false,
		},
		{
			name:     "the same, two blank lines before the typed block's fence",
			markdown: "> :::callout{}\n> > - x\n> >\n> >\n> :::\n",
			spread:   true,
		},
		{
			// The quote the item ends in holds the first blank line's ending, so the list needs
			// one more.
			name:     "the same, one blank line after an item ending in a quote",
			markdown: "> :::callout{}\n> > - x\n> >   > q\n> >\n> a\n",
			spread:   false,
		},
		{
			name:     "the same, two blank lines after an item ending in a quote",
			markdown: "> :::callout{}\n> > - x\n> >   > q\n> >\n> >\n> a\n",
			spread:   true,
		},
		{
			// A footnote definition opening in the quote around the list's ends the list past that
			// quote's marker on its own line, so one blank line spreads it.
			name:     "a blank line, then a footnote definition opening in the quote around the list's",
			markdown: "> > - a\n> >\n> [^f]: b\n",
			spread:   true,
		},
		{
			name:     "a blank line, then a list opening in the quote around the list's",
			markdown: "> > - a\n> >\n> - b\n",
			spread:   true,
		},
		{
			// Text opens no container, so the list ends at the start of its line and two blank
			// lines are needed.
			name:     "a blank line, then text in the quote around the list's",
			markdown: "> > - a\n> >\n> a\n",
			spread:   false,
		},
		{
			name:     "a blank line of the quote around the list's, then a list opening there",
			markdown: "> > - a\n> >\n>\n> - b\n",
			spread:   false,
		},
		{
			// A line holding a space past the quote's marker is a line of the typed block no fence
			// closes, and ends the list on it, so two blank lines spread the list.
			name:     "a typed block in a quote no fence closes, ending in a line of spaces past the quote's marker",
			markdown: "> :::callout{}\n> - a\n>\n>   \n",
			spread:   true,
		},
		{
			// A line holding nothing past the marker is no line of the typed block, which ends
			// before it, so a third blank line is needed.
			name:     "the same, ending in a line holding nothing past the quote's marker",
			markdown: "> :::callout{}\n> - a\n>\n> \n",
			spread:   false,
		},
		{
			name:     "the same, with a third blank line",
			markdown: "> :::callout{}\n> - a\n>\n>\n> \n",
			spread:   true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			doc, err := ParseForWrite(test.markdown, nil)
			if err != nil {
				t.Fatalf("ParseForWrite(%q): %v", test.markdown, err)
			}
			list := firstNodeOfType(doc, "bullet_list")
			if list == nil {
				t.Fatalf("ParseForWrite(%q) read no list", test.markdown)
			}
			if spread, _ := list.Attrs["spread"].(bool); spread != test.spread {
				t.Errorf("ParseForWrite(%q) reads its list spread = %v, want %v, as the browser editor's parser reads it", test.markdown, spread, test.spread)
			}
		})
	}
}

// The corpus-errors documents whose list item the browser editor spreads otherwise than this parser
// would store it are refused by every reader of caller markdown: the blank lines after a quoted item
// ending in a footnote definition whose typed block holds a fence no fence closed, which that
// parser reads as the code's; a definition inside a typed block with a list item around it,
// whose lines and the blank lines next to it that parser counts otherwise; and the blank lines
// that end a typed block in a list item, which that parser reads as the item's and goldmark as no
// block's (measured for PR #2507, at the fork point and at this head).
func TestCorpusErrorsOfItemSpreadReadOtherwiseAreRefused(t *testing.T) {
	for name, refusal := range map[string]string{
		"item-callout-code-ending-in-a-short-whitespace-line":                                     typedEndBlankInItem,
		"item-callout-list-ending-in-a-short-whitespace-line":                                     typedEndBlankInItem,
		"item-unclosed-callout-code-ending-in-a-short-whitespace-line":                            typedEndBlankInItem,
		"item-callout-paragraph-ending-in-a-blank-line":                                           typedEndBlankInItem,
		"item-callout-quote-ending-in-a-blank-line":                                               typedEndBlankInItem,
		"item-unclosed-callout-paragraph-ending-in-a-blank-line":                                  typedEndBlankInItem,
		"item-nested-callouts-ending-in-a-blank-line":                                             typedEndBlankInItem,
		"quoted-item-callout-paragraph-ending-in-a-blank-line":                                    typedEndBlankInItem,
		"quoted-item-unclosed-callout-paragraph-ending-in-two-blank-lines":                        typedEndBlankInItem,
		"quoted-item-nested-callout-list-ending-in-a-blank-line":                                  typedEndBlankInItem,
		"twice-quoted-item-callout-fence-ending-in-a-blank-line":                                  typedEndBlankInItem,
		"quoted-star-item-definition-callout-unclosed-fence-then-blank-lines":                     quotedDefinitionFenceBlank,
		"quoted-star-item-unspaced-definition-callout-unclosed-fence-then-blank-lines":            quotedDefinitionFenceBlank,
		"quoted-star-item-definition-unspaced-callout-unclosed-fence-then-blank-lines":            quotedDefinitionFenceBlank,
		"quoted-item-definition-callout-unclosed-fence-then-blank-lines":                          quotedDefinitionFenceBlank,
		"quoted-item-definition-callout-unclosed-fence-then-blank-lines-and-text":                 quotedDefinitionFenceBlank,
		"quoted-item-definition-callout-unclosed-tilde-fence-holding-code-then-blank-lines":       quotedDefinitionFenceBlank,
		"ordered-item-callout-holding-a-definition-opening-on-a-later-line":                       typedDefinitionInItem,
		"item-callout-holding-a-definition-opening-on-a-later-line":                               typedDefinitionInItem,
		"item-text-then-callout-holding-a-definition-opening-on-a-later-line":                     typedDefinitionInItem,
		"referenced-item-callout-holding-a-definition-opening-on-a-later-line":                    typedDefinitionInItem,
		"item-unclosed-callout-holding-a-definition-opening-on-a-later-line":                      typedDefinitionInItem,
		"quoted-item-callout-holding-a-definition-opening-on-a-later-line":                        typedDefinitionInItem,
		"item-callout-opening-with-a-blank-line-before-a-definition":                              typedDefinitionInItem,
		"item-callout-opening-with-a-blank-line-before-an-empty-definition-at-the-end":            typedDefinitionInItem,
		"item-callout-definition-then-blank-line-and-text":                                        typedDefinitionInItem,
		"ordered-item-nested-callout-opening-with-a-blank-line-before-a-definition":               typedDefinitionInItem,
		"ordered-item-nested-callout-opening-with-a-whitespace-line-before-a-rule-and-definition": typedDefinitionInItem,
		"item-callout-quote-then-two-blank-lines-before-a-list-and-definition":                    typedDefinitionInItem,
		"quoted-item-callout-empty-definition-before-text":                                        typedDefinitionInItem,
		"quoted-callout-quoted-item-empty-definition-before-text":                                 typedDefinitionInItem,
		"quoted-ordered-item-callout-quoted-item-empty-definition-before-text":                    typedDefinitionInItem,
		"quoted-callout-item-definition-then-blank-line-and-text":                                 typedDefinitionInItem,
		"quoted-callout-item-empty-definition-before-next-item":                                   typedDefinitionInItem,
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
// All three are refused today, each as a definition holding a block other than a paragraph
// (typedDefinitionHoldingBlock, measured over them for PR #2507), so the refusal is pinned by its
// reason: a reader that lifted it for another reason would still pass. The spread branch stands for
// the reader that reads one of them instead.
func TestAListInADefinitionInATypedBlockIsReadSpreadAsTheBrowserReadsItOrRefused(t *testing.T) {
	for _, markdown := range []string{
		":::callout{}\n[^f]: - a\n\n    - b\n:::\n\nText[^f].\n",
		":::callout{}\n[^f]:\n    - x\n      \n      note\n:::\n\nText[^f].\n",
		":::callout{}\n[^g]:1. \n       ***\n    \n       ***\n:::\n\nText[^g].\n",
	} {
		doc, err := ParseForWrite(markdown, nil)
		if errors.Is(err, ErrSchema) {
			if !strings.Contains(err.Error(), typedDefinitionHoldingBlock) {
				t.Errorf("ParseForWrite(%q) = %v, want it refused as %q", markdown, err, typedDefinitionHoldingBlock)
			}
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
