package pmdoc

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

func TestBlockIDForRangeUsesTheLowestContainingBlock(t *testing.T) {
	tests := []struct {
		name string
		tree *Node
		r    Range
		want string
	}{
		{
			name: "within one block",
			tree: &Node{Type: "doc", Children: []*Node{{
				Type: "paragraph", Attrs: Attrs{BlockIDAttr: "paragraph"},
				Children: []*Node{{Type: "text", Text: "Anchored text"}},
			}}},
			r:    Range{From: 1, To: 9},
			want: "paragraph",
		},
		{
			name: "siblings under one list item",
			tree: &Node{Type: "doc", Children: []*Node{{
				Type: "bullet_list", Attrs: Attrs{BlockIDAttr: "list"}, Children: []*Node{{
					Type: "list_item", Attrs: Attrs{BlockIDAttr: "item"}, Children: []*Node{
						{Type: "paragraph", Attrs: Attrs{BlockIDAttr: "first"}, Children: []*Node{{Type: "text", Text: "One"}}},
						{Type: "paragraph", Attrs: Attrs{BlockIDAttr: "second"}, Children: []*Node{{Type: "text", Text: "Two"}}},
					},
				}},
			}}},
			r:    Range{From: 3, To: 11},
			want: "item",
		},
		{
			name: "siblings under document root",
			tree: &Node{Type: "doc", Children: []*Node{
				{Type: "paragraph", Attrs: Attrs{BlockIDAttr: "first"}, Children: []*Node{{Type: "text", Text: "One"}}},
				{Type: "paragraph", Attrs: Attrs{BlockIDAttr: "second"}, Children: []*Node{{Type: "text", Text: "Two"}}},
			}},
			r:    Range{From: 1, To: 9},
			want: "",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			blockID, err := BlockIDForRange(test.tree, test.r)
			if err != nil {
				t.Fatalf("BlockIDForRange: %v", err)
			}
			if blockID != test.want {
				t.Fatalf("BlockIDForRange = %q, want %q", blockID, test.want)
			}
		})
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

// A document that names no block id twice parses under ParseForWrite exactly as under Parse: the
// same tree, the same ids and the same rendering, for every corpus document, typed blocks with and
// without an explicit id included.
func TestParseForWriteParsesADocumentWithoutRepeatsAsParseDoes(t *testing.T) {
	paths, err := filepath.Glob("testdata/corpus/*.md")
	if err != nil || len(paths) == 0 {
		t.Fatalf("corpus: %v (%d documents)", err, len(paths))
	}
	t.Cleanup(func() { SetBlockIDGenerator(nil) })
	for _, path := range paths {
		markdown, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		SetBlockIDGenerator(counterBlockIDs())
		parsed, err := Parse(string(markdown))
		if err != nil {
			t.Fatalf("%s: Parse: %v", path, err)
		}
		SetBlockIDGenerator(counterBlockIDs())
		written, err := ParseForWrite(string(markdown), nil)
		if err != nil {
			t.Fatalf("%s: ParseForWrite: %v", path, err)
		}
		if !reflect.DeepEqual(written, parsed) {
			t.Fatalf("%s: ParseForWrite and Parse differ:\n%#v\n%#v", path, written, parsed)
		}
	}
}

// A write is judged by what it adds to the live document, never by a repeat the live document
// already carries: a browser write can leave one until settlement repairs it, and an insert, an
// accept or an upload beside it is not the one repeating anything. Only an id the stored document
// would carry on more blocks than the live one does is refused, and the refusal names it.
func TestRepeatedBlockIDJudgesOnlyWhatTheWriteAdds(t *testing.T) {
	parse := func(markdown string, ids ...string) *Node {
		t.Helper()
		tree, err := Parse(markdown)
		if err != nil {
			t.Fatal(err)
		}
		for index, id := range ids {
			if id != "" {
				tree.Children[index].Attrs[BlockIDAttr] = id
			}
		}
		return tree
	}
	const two = "First.\n\nSecond.\n"
	const note = ":::callout{#note}\nA note.\n:::\n"
	repeated := parse(two, "p1", "p1")
	for _, test := range []struct {
		name              string
		live, next, named *Node
		want              string
	}{
		{name: "a fresh typed block beside a live repeat", live: repeated,
			next: parse(two+"\n"+":::callout{#fresh}\nA note.\n:::\n", "p1", "p1")},
		{name: "the live repeat carried back whole", live: repeated, next: parse(two, "p1", "p1")},
		{name: "a third block under the live repeat's id", live: repeated,
			next: parse(two+"\nThird.\n", "p1", "p1", "p1"), want: "p1"},
		{name: "a new block under a held id", live: parse(two, "p1", "p2"),
			next: parse("Zero.\n\n"+two, "p1", "p1", "p2"), want: "p1"},
		{name: "a new block under a held typed block's id", live: parse(note+"\n"+two, "", "p1", "p2"),
			next: parse(note+"\n"+note+"\n"+two, "note", "note", "p1", "p2"), want: "note"},
		{name: "a held block replaced by one naming its id", live: parse(note+"\n"+two, "", "p1", "p2"),
			next: parse(":::callout{#note kind=\"warning\"}\nReworded.\n:::\n\n"+two, "", "p1", "p2")},
		{name: "markdown naming one id twice, with no live document",
			next: parse(note+"\n"+note, "note", "note"), want: "note"},
		{name: "a split block's id on both halves, which the write never named", live: parse(two, "p1", "p2"),
			next: parse("Fir\n\n"+note+"\nst.\n\nSecond.\n", "p1", "", "p1", "p2"), named: parse(note)},
		{name: "a named id beside a split block's halves", live: parse(note+"\n"+two, "", "p1", "p2"),
			next:  parse(note+"\n"+"Fir\n\n"+note+"\nst.\n\nSecond.\n", "note", "p1", "note", "p1", "p2"),
			named: parse(note, "note"), want: "note"},
	} {
		t.Run(test.name, func(t *testing.T) {
			named := test.named
			if named == nil {
				named = test.next
			}
			err := RepeatedBlockID(test.live, test.next, named)
			if test.want == "" {
				if err != nil {
					t.Fatalf("RepeatedBlockID = %v, want nil", err)
				}
				return
			}
			if want := fmt.Sprintf("block id %q would name two blocks", test.want); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("RepeatedBlockID = %v, want %q", err, want)
			}
		})
	}
}
