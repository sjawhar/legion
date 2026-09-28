package pmdoc

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// A document a caller writes is stored as its rendering, so one whose rendering does not read back
// as the document is refused rather than stored: the next read of what was stored would give
// another document, or refuse it. Each document here is read as the browser editor's engine reads
// it, and its rendering read back otherwise at bc74741f, where it was stored.
func TestParseForWriteRefusesADocumentItsRenderingReadsBackOtherwise(t *testing.T) {
	for _, markdown := range []string{
		"> [^n]: -\n> -\n",
		"> [^n]: *\n> 10. text x[^n]\n",
		"1. [^a1]: :::callout{#c1 kind=\"note\" title=\"\"}\n       ---\n       1\n",
		"2. [^N]:\n       12. *\n       --\n",
	} {
		if _, err := Parse(markdown); err != nil {
			t.Fatalf("Parse(%q) = %v, want it read", markdown, err)
		}
		_, err := ParseForWrite(markdown, nil)
		if !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), "reads back otherwise") {
			t.Errorf("ParseForWrite(%q) = %v, want a schema refusal of a document that reads back otherwise", markdown, err)
		}
	}
}

// A short table row is padded to the header's width, and a padded cell takes its column's
// alignment: the rendering writes it as a cell of that column, which reads back with the column's
// alignment, so a padded cell holding another would store a table that reads back otherwise.
func TestParsePadsAShortRowWithItsColumnsAlignment(t *testing.T) {
	markdown := "| a | b | c | d |\n| :--- | :---: | ---: | --- |\n| x |\n"
	doc, err := Parse(markdown)
	if err != nil {
		t.Fatal(err)
	}
	row := doc.Children[0].Children[1]
	var got []any
	for _, cell := range row.Children {
		got = append(got, cell.Attrs["alignment"])
	}
	if want := []any{"left", "center", "right", nil}; !slices.Equal(got, want) {
		t.Errorf("Parse(%q) pads the short row with alignments %v, want %v", markdown, got, want)
	}
	if _, err := ParseForWrite(markdown, nil); err != nil {
		t.Errorf("ParseForWrite(%q) = %v, want the table stored", markdown, err)
	}
}

// A bare URL ends where linkify stops, and a backslash escape stops it: `https://x.com/a\_b` reads
// as the link `https://x.com/a` and the text `_b`. The renderer keeps that escape wherever the
// character would otherwise continue the URL, so the document is stored as written and reads back
// as it was read, rather than the link growing over the text on every write.
func TestRenderKeepsTheEscapeThatEndsABareURL(t *testing.T) {
	for _, char := range "!\"#$%&'()+,-./:;=>?@[]^_`{}~" {
		markdown := "see https://x.com/a\\" + string(char) + "b c\n"
		doc, err := Parse(markdown)
		if err != nil {
			t.Fatalf("Parse(%q) = %v", markdown, err)
		}
		rendered, err := Render(doc)
		if err != nil {
			t.Fatalf("Render(Parse(%q)) = %v", markdown, err)
		}
		if rendered != markdown {
			t.Errorf("Render(Parse(%q)) = %q, want it as written", markdown, rendered)
		}
		if _, err := ParseForWrite(markdown, nil); err != nil {
			t.Errorf("ParseForWrite(%q) = %v, want it stored", markdown, err)
		}
	}
}

// A fused delimiter run that reads back is written as main writes it: the parser splits it as
// written, and the browser editor's parser reads the same bytes as the same tree.
func TestRenderKeepsAFusedRunThatReadsBack(t *testing.T) {
	assertRenders(t, []struct{ markdown, want string }{
		{"*x**b***\n", "*x****b***\n"},
		{"_p x**b**_ q\n", "*p x****b*** q\n"},
		{"> *x**b***\n", "> *x****b***\n"},
		{"- *x**b***\n", "- *x****b***\n"},
		{"*a **b** c*\n", "*a&#32;****b****&#32;c*\n"},
		{"*a **b** c **d** e*\n", "*a&#32;****b****&#32;c&#32;****d****&#32;e*\n"},
		{"| *a **b** c* |\n| --- |\n", "| *a&#32;****b****&#32;c* |\n| --- |\n"},
		{"> _&#42; ~ it's **`` x ``**_\n", "> *\\* ~ it's&#32;****`x`***\n"},
	})
}

// A fused run that does not read back is written with no mark the next text still carries closed
// and opened again between them: open marks stay open, and a text opens first the marks the text
// after it carries.
func TestRenderKeepsAMarkOpenWhereAFusedRunDoesNotReadBack(t *testing.T) {
	assertRenders(t, []struct{ markdown, want string }{
		{"*a **b [l](https://e.com/u) c** d*\n", "*a **b [l](https://e.com/u) c** d*\n"},
		{"*a **b***\n", "*a **b***\n"},
		{"***a** b*\n", "***a** b*\n"},
		{"_**Note:** see below_\n", "***Note:** see below*\n"},
		{"x ***a** b* y\n", "x ***a** b* y\n"},
		{"[***a** b*](https://e.com/u)\n", "[***a** b*](https://e.com/u)\n"},
	})
}

// assertRenders requires each markdown to render as want and to be stored by ParseForWrite.
func assertRenders(t *testing.T, cases []struct{ markdown, want string }) {
	t.Helper()
	for _, c := range cases {
		doc, err := Parse(c.markdown)
		if err != nil {
			t.Fatalf("Parse(%q) = %v", c.markdown, err)
		}
		rendered, err := Render(doc)
		if err != nil {
			t.Fatalf("Render(Parse(%q)) = %v", c.markdown, err)
		}
		if rendered != c.want {
			t.Errorf("Render(Parse(%q)) = %q, want %q", c.markdown, rendered, c.want)
		}
		if _, err := ParseForWrite(c.markdown, nil); err != nil {
			t.Errorf("ParseForWrite(%q) = %v, want it stored", c.markdown, err)
		}
	}
}
