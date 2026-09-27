package pmdoc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The browser editor's parser ends a typed block at a line of at least its fence's colons in code
// it holds, measured on the written line: starting at most three columns past the typed block's
// own content column, list markers adding their width, a tab advancing to the next multiple of
// four from the column it stands at, and never inside a blockquote within the typed block.
// testdata/typed-fence-lines.json holds, for code lines across layouts - a callout at the top,
// and inside a blockquote, list items, a footnote definition and another callout - a tree, the
// markdown this renderer writes for it, and the engine's own reading of that markdown (the
// fixture generator writes it). The renderer writes each typed block's fence longer than every
// line that could close it, so the engine keeps every block whole with its code, and so does this
// parser.
func TestTypedFencesReadTheSameInTheEngineAndHere(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "typed-fence-lines.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name         string          `json:"name"`
		Tree         json.RawMessage `json:"tree"`
		Markdown     string          `json:"markdown"`
		EngineCloses bool            `json:"engine_closes"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, test := range cases {
		t.Run(test.Name, func(t *testing.T) {
			doc, err := FromJSON(test.Tree)
			if err != nil {
				t.Fatal(err)
			}
			if markdown, err := Render(doc); err != nil || markdown != test.Markdown {
				t.Fatalf("the case's markdown is not what this renderer writes for its tree: %q (%v)", markdown, err)
			}
			if test.EngineCloses {
				t.Fatalf("the engine ends a typed block early in %q", test.Markdown)
			}
			back, err := Parse(test.Markdown)
			if err != nil {
				t.Fatalf("Parse(%q): %v", test.Markdown, err)
			}
			if want, got := shapeDifference(doc, back); want != "" || codeText(back) != codeText(doc) {
				t.Fatalf("Parse(%q) reads %s as %s, code %q, want the tree's blocks holding %q", test.Markdown, want, got, codeText(back), codeText(doc))
			}
		})
	}
}

func codeText(doc *Node) string {
	text := ""
	Walk(doc, func(node *Node) bool {
		if node.Type == "code_block" && text == "" {
			text = node.Children[0].Text
		}
		return true
	})
	return text
}
