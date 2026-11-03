package pmdoc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The wire encoding of a display tree leaves out only what the reader puts back. Every document
// of both corpora is read in both modes, encoded as a response carries it, decoded, and filled
// as the browser's schema fills a node it is handed no attribute for (ProseMirror's
// NodeType.create, which documentFromJSON calls): the result must be the tree the read made,
// attribute for attribute.
func TestTheDisplayEncodingLeavesOutOnlyWhatTheSchemaFillsBack(t *testing.T) {
	for _, path := range displayEncodingCorpus(t) {
		markdown, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		name := strings.TrimSuffix(filepath.Base(path), ".md")
		for _, mode := range []struct {
			name string
			mode DisplayMode
		}{{"latest", DisplayLatest}, {"version", DisplayVersion}} {
			t.Run(name+"/"+mode.name, func(t *testing.T) {
				want := ReadForDisplay(string(markdown), mode.mode)
				encoded, err := json.Marshal(want.toDisplayJSON())
				if err != nil {
					t.Fatalf("encode the display tree: %v", err)
				}
				got, err := FromJSON(encoded)
				if err != nil {
					t.Fatalf("decode the display tree: %v", err)
				}
				fillSchemaDefaults(got)
				if !got.EqualWithBlockIDs(want) {
					wanted, _ := want.JSON()
					t.Errorf("the encoding filled back is\n\t%s\nwant the display read's\n\t%s", encoded, wanted)
				}
			})
		}
	}
}

// The encoding really does leave those attributes out: a plain bullet list carries no minted
// block id, no list marker the schema already knows and no spread the schema already knows,
// and keeps the one attribute whose value is not its default (a tight item's `spread`).
func TestTheDisplayEncodingWritesNoDefaultAttribute(t *testing.T) {
	encoded, err := json.Marshal(ReadForDisplay("# Title\n\n- one\n- two\n", DisplayLatest).toDisplayJSON())
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{`"blockId"`, `"label"`, `"listType"`, `"checked"`, `"id":""`, `"level":1`} {
		if strings.Contains(string(encoded), gone) {
			t.Errorf("the encoding still writes %s:\n\t%s", gone, encoded)
		}
	}
	if !strings.Contains(string(encoded), `"spread":false`) {
		t.Errorf("the encoding dropped a tight list item's spread, which the schema defaults to true:\n\t%s", encoded)
	}
}

// displayEncodingCorpus is every document both corpora hold: the writer's and the display
// read's own.
func displayEncodingCorpus(t *testing.T) []string {
	t.Helper()
	var paths []string
	for _, corpus := range []string{"corpus", "corpus-display"} {
		found, err := filepath.Glob(filepath.Join("testdata", corpus, "*.md"))
		if err != nil || len(found) == 0 {
			t.Fatalf("testdata/%s: %d documents, %v", corpus, len(found), err)
		}
		paths = append(paths, found...)
	}
	return paths
}

// fillSchemaDefaults gives every node and mark of tree the attributes the encoding left out, as
// the browser's schema gives them to a node built with none: the declared default of each
// attribute the object does not name.
func fillSchemaDefaults(tree *Node) {
	Walk(tree, func(node *Node) bool {
		node.Attrs = filledAttrs(node.Attrs, displayDefaultsFor(node.Type))
		for index := range node.Marks {
			node.Marks[index].Attrs = filledAttrs(node.Marks[index].Attrs, displayMarkDefaults[node.Marks[index].Type])
		}
		return true
	})
}

// displayDefaultsFor is the defaults declared for one node type, a typed block's own among them.
func displayDefaultsFor(nodeType string) Attrs {
	if defaults, typed := typedDefaults[nodeType]; typed {
		return defaults
	}
	return displayNodeDefaults[nodeType]
}

// filledAttrs is attrs with every default it does not name written into it.
func filledAttrs(attrs, defaults Attrs) Attrs {
	for name, value := range defaults {
		if _, named := attrs[name]; named {
			continue
		}
		if attrs == nil {
			attrs = make(Attrs, len(defaults))
		}
		attrs[name] = value
	}
	return attrs
}
