package pmdoc

import (
	"fmt"
	"strings"
	"testing"

	"github.com/yuin/goldmark/util"
)

// The guard's local-part table is goldmark's: a byte the guard reads as a local-part character is
// one util.FindEmailIndex reads as one, and no other.
func TestLinkifyGuardReadsGoldmarksLocalPartCharacters(t *testing.T) {
	for c := range 256 {
		goldmarks := util.FindEmailIndex([]byte{byte(c), '@', 'x', '.', 'y'}) > 0
		if goldmarks != emailLocal[c] {
			t.Errorf("byte %q: goldmark reads it as a local-part character: %v; the guard: %v", byte(c), goldmarks, emailLocal[c])
		}
	}
}

// The guard links exactly what goldmark's linkify links, and leaves text where goldmark did.
func TestLinkifyGuardLinksWhatGoldmarkLinks(t *testing.T) {
	for _, test := range []struct{ markdown, want string }{
		{"mail alice@example.com now", `text "mail " | text "alice@example.com" link alice@example.com | text " now"`},
		{"(bob@example.org)", `text "(" | text "bob@example.org" link bob@example.org | text ")"`},
		{"a_a_a@b.c", `text "a_a_a@b.c" link a_a_a@b.c`},
		{"a_a_a@b.c_", `text "a_a_a@b.c_"`},
		{"x@y and a@@b.c", `text "x@y and a@@b.c"`},
		{"see https://example.com/a_b and www.example.org/x", `text "see " | text "https://example.com/a_b" link https://example.com/a_b | text " and " | text "www.example.org/x" link http://www.example.org/x`},
		{"@start of a line", `text "@start of a line"`},
		{"end.@dot", `text "end.@dot"`},
	} {
		doc, err := Parse(test.markdown)
		if err != nil {
			t.Fatalf("%q: %v", test.markdown, err)
		}
		if got := inlineSummary(doc.Children[0]); got != test.want {
			t.Errorf("%q:\n got %s\nwant %s", test.markdown, got, test.want)
		}
	}
}

func inlineSummary(paragraph *Node) string {
	var parts []string
	for _, node := range paragraph.Children {
		part := fmt.Sprintf("%s %q", node.Type, node.Text)
		for _, mark := range node.Marks {
			if mark.Type == "link" {
				part += " link " + mark.Attrs["href"].(string)
			}
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, " | ")
}
