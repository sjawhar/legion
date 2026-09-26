package pmdoc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The browser editor's parser ends a typed block at a line of colons in code it holds, measured on
// the written line: indented at most three columns from the typed block's prefix, list markers
// adding their width, a tab counting four, and never inside a blockquote or a nested list's code.
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
			if line, closes := TypedFenceLineInCode(doc.Children[1]); closes != test.EngineCloses {
				t.Fatalf("TypedFenceLineInCode = %q, %v; the engine closes: %v", line, closes, test.EngineCloses)
			}
		})
	}
}
