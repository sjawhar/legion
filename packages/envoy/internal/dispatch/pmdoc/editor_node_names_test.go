package pmdoc

import (
	_ "embed"
	"encoding/json"
	"slices"
	"testing"
)

// editorNodeNamesJSON is the browser editor's own node and mark names, written from its schema by
// gen/gen.ts (`bun run gen` there). The two sides are held to each other below, so a node or mark
// the editor gains or loses is a change this package sees.
//
//go:embed schema/editor-node-names.json
var editorNodeNamesJSON []byte

// editorNodeDifferences are the node names the editor's schema and nodeTypes do not share, each
// with why. A typed block is declared by schema/blocks.json, so the editor carries a node for it
// while nodeTypes does not: validateNode accepts it through IsTypedBlock instead.
var editorNodeDifferences = map[string]string{
	"ask":         "a typed block of schema/blocks.json: IsTypedBlock accepts it, nodeTypes does not list it",
	"callout":     "a typed block of schema/blocks.json: IsTypedBlock accepts it, nodeTypes does not list it",
	"check":       "a typed block of schema/blocks.json: IsTypedBlock accepts it, nodeTypes does not list it",
	"plan_task":   "a typed block of schema/blocks.json: IsTypedBlock accepts it, nodeTypes does not list it",
	"requirement": "a typed block of schema/blocks.json: IsTypedBlock accepts it, nodeTypes does not list it",
}

// editorMarkDifferences are the mark names the editor's schema and markTypes do not share. There
// are none: every mark the editor carries is one a stored document may hold.
var editorMarkDifferences = map[string]string{}

// The editor's nodes and marks are the ones this package accepts: a name on one side only would
// be a document the editor writes and the server refuses, or the other way round. Every
// deliberate difference is listed above with its reason, so dropping a name from either side
// fails here, and so does listing a name that is no longer a difference.
func TestEditorNodeAndMarkNamesMatchTheGoLists(t *testing.T) {
	var editor struct {
		Nodes []string `json:"nodes"`
		Marks []string `json:"marks"`
	}
	if err := json.Unmarshal(editorNodeNamesJSON, &editor); err != nil {
		t.Fatalf("decode schema/editor-node-names.json: %v", err)
	}
	if len(editor.Nodes) == 0 || len(editor.Marks) == 0 {
		t.Fatal("schema/editor-node-names.json lists no nodes or no marks: run `bun run gen` in gen/")
	}
	requireSameNames(t, "node", editor.Nodes, nodeTypes, editorNodeDifferences)
	requireSameNames(t, "mark", editor.Marks, markTypes, editorMarkDifferences)
}

// requireSameNames fails t for a name editor or own holds alone that declared does not explain,
// and for a declared name that both sides hold or neither does.
func requireSameNames(t *testing.T, what string, editor []string, own map[string]bool, declared map[string]string) {
	t.Helper()
	held := make(map[string]bool, len(editor))
	for _, name := range editor {
		held[name] = true
	}
	var different []string
	for _, name := range editor {
		if !own[name] {
			different = append(different, name)
		}
	}
	for name := range own {
		if !held[name] {
			different = append(different, name)
		}
	}
	slices.Sort(different)
	for _, name := range different {
		if _, why := declared[name]; !why {
			t.Errorf("the browser editor and pmdoc disagree on %s %q: one carries it and the other does not. Declare why in this file, or put it back", what, name)
		}
	}
	for name := range declared {
		if !slices.Contains(different, name) {
			t.Errorf("%s %q is listed as a deliberate difference, but both sides carry it or neither does: drop the entry", what, name)
		}
	}
}
