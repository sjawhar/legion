package docs

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// An insert's refusal of a typed block opening that continues a paragraph names the line of the
// insert's markdown the caller wrote, wherever the insert lands: at a table-cell quote the fragment
// is first read as rows under a header the server writes itself, and those two lines are not the
// caller's (LEGION-416).
func TestInsertRefusalNamesTheCallersLineAtATableCell(t *testing.T) {
	const opening = `:::ask{urgency="med" title="x | y"}`
	const table = "| K | V |\n| --- | --- |\n| a | c |\n"
	const paragraph = "Context c here.\n"
	for _, test := range []struct {
		name, document, markdown string
		line                     int
	}{
		{"list item at a table cell", table, "- a | b\n        " + opening + "\n", 2},
		{"quote at a table cell", table, "> a | b\n>     " + opening + "\n", 2},
		{"list item after a blank line at a table cell", table, "\n- a | b\n        " + opening + "\n", 3},
		{"list item at a paragraph", paragraph, "- a | b\n        " + opening + "\n", 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput(test.document)
			if err != nil {
				t.Fatal(err)
			}
			_, err = applyOperations(tree, []model.EditOp{{Op: "insert", Markdown: test.markdown, After: "c"}})
			want := fmt.Sprintf("line %d, %q, continues a paragraph", test.line, opening)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("insert = %v, want a refusal naming %s", err, want)
			}
		})
	}
}
