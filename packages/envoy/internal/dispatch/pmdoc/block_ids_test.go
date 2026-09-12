package pmdoc

import (
	"fmt"
	"testing"
)

func TestEnsureBlockIDsStampsMissingBlocksInPreorder(t *testing.T) {
	installCounterBlockIDs(t)
	tree := &Node{Type: "doc", Children: []*Node{
		{Type: "heading", Children: []*Node{{Type: "text", Text: "Title"}}},
		{Type: "paragraph", Attrs: Attrs{"blockId": "kept"}, Children: []*Node{{Type: "text", Text: "Body"}}},
		{Type: "hr"},
	}}

	changed := EnsureBlockIDs(tree)
	if !changed {
		t.Fatal("EnsureBlockIDs() changed = false, want true")
	}
	if got, want := blockIDs(tree), []string{"b-000001", "kept", "b-000002"}; !equalStrings(got, want) {
		t.Fatalf("block ids = %q, want %q", got, want)
	}
}

func TestEnsureBlockIDsRemintsDuplicateAfterFirstOccurrence(t *testing.T) {
	installCounterBlockIDs(t)
	tree := &Node{Type: "doc", Children: []*Node{
		{Type: "paragraph", Attrs: Attrs{"blockId": "same"}},
		{Type: "paragraph", Attrs: Attrs{"blockId": "same"}},
		{Type: "paragraph", Attrs: Attrs{"blockId": "other"}},
	}}

	changed := EnsureBlockIDs(tree)
	if !changed {
		t.Fatal("EnsureBlockIDs() changed = false, want true")
	}
	if got, want := blockIDs(tree), []string{"same", "b-000001", "other"}; !equalStrings(got, want) {
		t.Fatalf("block ids = %q, want %q", got, want)
	}
}

func TestEnsureBlockIDsStampsNestedStructuralBlocks(t *testing.T) {
	tree := &Node{Type: "doc", Children: []*Node{{
		Type: "bullet_list", Children: []*Node{{
			Type: "list_item", Children: []*Node{{
				Type: "paragraph", Children: []*Node{{Type: "text", Text: "Nested"}},
			}},
		}},
	}}}
	installCounterBlockIDs(t)
	EnsureBlockIDs(tree)
	if got, want := blockIDs(tree), []string{"b-000001", "b-000002", "b-000003"}; !equalStrings(got, want) {
		t.Fatalf("block ids = %q, want %q", got, want)
	}
}

func TestParseStampsBlocksWithInjectedGenerator(t *testing.T) {
	installCounterBlockIDs(t)

	tree, err := Parse("# Title\n\n- Nested\n")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := blockIDs(tree), []string{"b-000001", "b-000002", "b-000003", "b-000004"}; !equalStrings(got, want) {
		t.Fatalf("parsed block ids = %q, want %q", got, want)
	}
}

func installCounterBlockIDs(t *testing.T) {
	t.Helper()
	SetBlockIDGenerator(counterBlockIDs())
	t.Cleanup(func() { SetBlockIDGenerator(nil) })
}

func counterBlockIDs() func() string {
	n := 0
	return func() string {
		n++
		return fmt.Sprintf("b-%06d", n)
	}
}
func blockIDs(tree *Node) []string {
	var ids []string
	walk(tree, func(node *Node, _ []int, _, _ int) bool {
		if node.Type != "doc" && !isInlineNodeType(node.Type) {
			ids = append(ids, node.Attrs["blockId"].(string))
		}
		return true
	})
	return ids
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
