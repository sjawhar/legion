package pmdoc

import (
	"errors"
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
