package pmdoc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The browser editor's parser ends a typed block at a line of colons in code it holds, measured on
// the written line: starting at most three columns past the typed block's content column, list
// markers and a blockquote's `> ` adding their width, a tab advancing to the next multiple of four
// from the column it stands at, and never in a blockquote inside the typed block. The cases put the
// callout at the top, and inside a blockquote, list items and a footnote definition.
// testdata/typed-fence-lines.json holds the engine's own verdict for each case (the fixture
// generator writes it), and TypedFenceLineInCode must agree with every one; each case's markdown is
// what this renderer writes for its tree, so the verdicts are about this renderer's output.
func TestTypedFenceLineInCodeAgreesWithTheEngine(t *testing.T) {
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
			line, closes := "", false
			for _, block := range doc.Children {
				if line, closes = TypedFenceLineInCode(block); closes {
					break
				}
			}
			if closes != test.EngineCloses {
				t.Fatalf("TypedFenceLineInCode = %q, %v; the engine closes: %v", line, closes, test.EngineCloses)
			}
		})
	}
}
