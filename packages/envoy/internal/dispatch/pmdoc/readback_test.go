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
