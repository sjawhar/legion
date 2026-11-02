package pmdoc

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
)

// An upload writes the whole document's markdown over the live one, and that markdown names no
// untyped block's id (GET /text renders none). The blocks it leaves unchanged keep the ids live
// gave them, so the write tombstones no blockId attribute it did not change, and an anchor or a
// reference keyed on such a block's id stays on it. Only a changed block, and every block holding
// it, takes a fresh id; an id never moves onto other content (a swapped, retyped or replaced
// block), a typed block's id is the one its markdown names, and a repeat live carries is kept for
// its first holder alone.
func TestAnUploadKeepsTheIDsOfTheBlocksItLeavesUnchanged(t *testing.T) {
	spec, _, _ := docstest.LoadSpec(rand.New(rand.NewPCG(715, 1)), "LOAD-1", 25_000)

	oneToken := func(t *testing.T, old, replacement string, wantTypes []string) {
		t.Helper()
		parsed, err := Parse(spec)
		if err != nil {
			t.Fatal(err)
		}
		doc, frag, live := writeLiveDocument(t, parsed)
		markdown, err := Render(live)
		if err != nil {
			t.Fatal(err)
		}
		if count := strings.Count(markdown, old); count != 1 {
			t.Fatalf("the rendered spec holds %q %d times, want once", old, count)
		}
		after, diff := uploadOver(t, doc, frag, live, strings.Replace(markdown, old, replacement, 1))
		t.Logf("upload of %d markdown bytes changing %q to %q: diff update %d bytes", len(markdown), old, replacement, diff)
		requireOnlyTheChangedBlockAndItsAncestorsChangeID(t, live, after, wantTypes)
	}
	t.Run("a token in a top-level paragraph", func(t *testing.T) {
		oneToken(t, "Revision token r0", "Revision token r1", []string{"paragraph"})
	})
	t.Run("a token in a table cell", func(t *testing.T) {
		oneToken(t, "Requirement LOAD-1-R5 holds", "Requirement LOAD-1-R5 fails", []string{"table", "table_row", "table_cell", "paragraph"})
	})

	t.Run("an ask replaced by one naming no id", func(t *testing.T) {
		const ask = ":::ask{#ask-1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich transport should we expose?\n\n- REST: Matches the existing platform\n- gRPC: Adds streaming\n:::\n"
		for _, test := range []struct {
			name string
			edit func(string) string
		}{
			{name: "with the same content", edit: func(markdown string) string { return markdown }},
			{name: "with another question", edit: func(markdown string) string {
				return strings.Replace(markdown, "Which transport should we expose?", "Which transport should we drop?", 1)
			}},
		} {
			t.Run(test.name, func(t *testing.T) {
				parsed, err := Parse("Opening paragraph.\n\n" + ask + "\nClosing paragraph.\n")
				if err != nil {
					t.Fatal(err)
				}
				doc, frag, live := writeLiveDocument(t, parsed)
				markdown, err := Render(live)
				if err != nil {
					t.Fatal(err)
				}
				unnamed := strings.Replace(markdown, "{#ask-1 ", "{", 1)
				if unnamed == markdown {
					t.Fatalf("the rendered document names no ask-1:\n%s", markdown)
				}
				after, diff := uploadOver(t, doc, frag, live, test.edit(unnamed))
				t.Logf("diff update %d bytes", diff)
				oldAsk, newAsk := live.Children[1], after.Children[1]
				if newAsk.Type != "ask" {
					t.Fatalf("block 1 after the upload is a %s, want the ask", newAsk.Type)
				}
				if id, _ := storedBlockID(newAsk); id == "ask-1" || slices.Contains(blockIDsOf(live), id) {
					t.Fatalf("the ask naming no id has id %q, a live block's; want a fresh one", id)
				}
				oldInner := blockIDsOf(&Node{Type: "doc", Children: oldAsk.Children})
				for _, id := range blockIDsOf(&Node{Type: "doc", Children: newAsk.Children}) {
					if slices.Contains(oldInner, id) {
						t.Fatalf("a block inside the new ask took id %q from inside the replaced ask (%q)", id, oldInner)
					}
				}
			})
		}
	})

	t.Run("markdown naming a held typed block's id on another block is refused", func(t *testing.T) {
		parsed, err := Parse(":::callout{#note}\nA note.\n:::\n\nFirst paragraph.\n")
		if err != nil {
			t.Fatal(err)
		}
		_, _, live := writeLiveDocument(t, parsed)
		markdown, err := Render(live)
		if err != nil {
			t.Fatal(err)
		}
		_, err = ParseForWrite(markdown+"\n:::callout{#note}\nAnother note.\n:::\n", live)
		if !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), `block id "note" would name two blocks`) {
			t.Fatalf("ParseForWrite = %v, want ErrSchema naming note twice", err)
		}
	})

	t.Run("a typed block naming an untyped live block's id keeps it", func(t *testing.T) {
		parsed, err := Parse("First paragraph.\n\nSecond paragraph.\n")
		if err != nil {
			t.Fatal(err)
		}
		doc, frag, live := writeLiveDocument(t, parsed)
		held, _ := storedBlockID(live.Children[0])
		markdown, err := Render(live)
		if err != nil {
			t.Fatal(err)
		}
		after, diff := uploadOver(t, doc, frag, live, fmt.Sprintf(":::callout{#%s}\nA note.\n:::\n\n", held)+markdown)
		t.Logf("diff update %d bytes", diff)
		if id, _ := storedBlockID(after.Children[0]); after.Children[0].Type != "callout" || id != held {
			t.Fatalf("block 0 after the upload is a %s with id %q, want the callout naming %q", after.Children[0].Type, id, held)
		}
		if id, _ := storedBlockID(after.Children[1]); id == held {
			t.Fatalf("the unchanged paragraph kept id %q, which the callout names", held)
		}
		if err := RepeatedBlockID(nil, after, after); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("two swapped paragraphs", func(t *testing.T) {
		parsed, err := Parse("Opening paragraph.\n\nAlpha paragraph.\n\nBeta paragraph.\n\nClosing paragraph.\n")
		if err != nil {
			t.Fatal(err)
		}
		doc, frag, live := writeLiveDocument(t, parsed)
		markdown, err := Render(live)
		if err != nil {
			t.Fatal(err)
		}
		swapped := strings.Replace(markdown, "Alpha paragraph.\n\nBeta paragraph.", "Beta paragraph.\n\nAlpha paragraph.", 1)
		if swapped == markdown {
			t.Fatalf("the rendered document does not hold the two paragraphs in order:\n%s", markdown)
		}
		after, diff := uploadOver(t, doc, frag, live, swapped)
		t.Logf("diff update %d bytes", diff)
		liveIDs := blockIDsOf(live)
		for _, index := range []int{1, 2} {
			if id, _ := storedBlockID(after.Children[index]); slices.Contains(liveIDs, id) {
				t.Errorf("swapped paragraph %d (%q) has live id %q, want a fresh one", index, TextContent(after.Children[index]), id)
			}
		}
		for _, index := range []int{0, 3} {
			got, _ := storedBlockID(after.Children[index])
			if want, _ := storedBlockID(live.Children[index]); got != want {
				t.Errorf("unchanged paragraph %d (%q) has id %q, want live's %q", index, TextContent(after.Children[index]), got, want)
			}
		}
	})

	t.Run("a repeat live carries", func(t *testing.T) {
		parsed, err := Parse("First paragraph.\n\nSecond paragraph.\n\nThird paragraph.\n\nFourth paragraph r0.\n")
		if err != nil {
			t.Fatal(err)
		}
		repeated, _ := storedBlockID(parsed.Children[0])
		parsed.Children[1].Attrs[BlockIDAttr] = repeated
		doc, frag, live := writeLiveDocument(t, parsed)
		if id, _ := storedBlockID(live.Children[1]); id != repeated {
			t.Fatalf("live's second paragraph has id %q, want the repeat %q", id, repeated)
		}
		markdown, err := Render(live)
		if err != nil {
			t.Fatal(err)
		}
		after, diff := uploadOver(t, doc, frag, live, strings.Replace(markdown, "Fourth paragraph r0.", "Fourth paragraph r1.", 1))
		t.Logf("diff update %d bytes", diff)
		if id, _ := storedBlockID(after.Children[0]); id != repeated {
			t.Errorf("the repeat's first holder has id %q, want %q", id, repeated)
		}
		if id, _ := storedBlockID(after.Children[1]); slices.Contains(blockIDsOf(live), id) {
			t.Errorf("the repeat's second holder has live id %q, want a fresh one", id)
		}
		third, _ := storedBlockID(after.Children[2])
		if want, _ := storedBlockID(live.Children[2]); third != want {
			t.Errorf("the unchanged third paragraph has id %q, want live's %q", third, want)
		}
		if err := RepeatedBlockID(nil, after, after); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a paragraph retyped as a heading", func(t *testing.T) {
		parsed, err := Parse("Opening paragraph.\n\nRetyped text\n\nClosing paragraph.\n")
		if err != nil {
			t.Fatal(err)
		}
		doc, frag, live := writeLiveDocument(t, parsed)
		markdown, err := Render(live)
		if err != nil {
			t.Fatal(err)
		}
		retyped := strings.Replace(markdown, "Retyped text", "## Retyped text", 1)
		after, diff := uploadOver(t, doc, frag, live, retyped)
		t.Logf("diff update %d bytes", diff)
		if after.Children[1].Type != "heading" || TextContent(after.Children[1]) != "Retyped text" {
			t.Fatalf("block 1 after the upload is a %s holding %q, want the heading", after.Children[1].Type, TextContent(after.Children[1]))
		}
		if id, _ := storedBlockID(after.Children[1]); slices.Contains(blockIDsOf(live), id) {
			t.Fatalf("the retyped heading has live id %q, want a fresh one", id)
		}
	})
}

// writeLiveDocument writes tree into a fresh document's fragment and returns the document, the
// fragment and the tree read back from it, as a room holds it.
func writeLiveDocument(t *testing.T, tree *Node) (*crdt.Doc, *crdt.YXmlFragment, *Node) {
	t.Helper()
	doc := crdt.New()
	frag := doc.GetXmlFragment("prosemirror")
	writeUpdate(t, doc, frag, tree)
	live, err := Read(frag)
	if err != nil {
		t.Fatal(err)
	}
	return doc, frag, live
}

// uploadOver writes markdown over live as an upload does (ParseForWrite, then Update into the
// document holding live) and returns the tree read back and the size of the update the write
// added.
func uploadOver(t *testing.T, doc *crdt.Doc, frag *crdt.YXmlFragment, live *Node, markdown string) (*Node, int) {
	t.Helper()
	next, err := ParseForWrite(markdown, live)
	if err != nil {
		t.Fatalf("ParseForWrite: %v", err)
	}
	before := doc.StateVector()
	writeUpdate(t, doc, frag, next)
	after, err := Read(frag)
	if err != nil {
		t.Fatal(err)
	}
	return after, len(crdt.EncodeStateAsUpdateV1(doc, before))
}

// requireOnlyTheChangedBlockAndItsAncestorsChangeID walks before and after in parallel, which
// must have one shape, and requires exactly one text node to differ, every block holding it (of
// wantTypes, outermost first) to carry an id before did not, and every other block to keep its id.
func requireOnlyTheChangedBlockAndItsAncestorsChangeID(t *testing.T, before, after *Node, wantTypes []string) {
	t.Helper()
	var changedText [][]int
	var changedIDs []string
	blocks := 0
	var walk func(a, b *Node, path []int)
	walk = func(a, b *Node, path []int) {
		if a.Type != b.Type || len(a.Children) != len(b.Children) {
			t.Fatalf("at %v the upload changed the shape: %s with %d children became %s with %d", path, a.Type, len(a.Children), b.Type, len(b.Children))
		}
		if a.Type == "text" && (a.Text != b.Text || fmt.Sprint(a.Marks) != fmt.Sprint(b.Marks)) {
			changedText = append(changedText, slices.Clone(path))
		}
		if a.Type != "doc" && !isInlineNodeType(a.Type) {
			blocks++
			if idA, _ := storedBlockID(a); idA != fmt.Sprint(b.Attrs[BlockIDAttr]) {
				changedIDs = append(changedIDs, fmt.Sprint(path))
			}
		}
		for index := range a.Children {
			walk(a.Children[index], b.Children[index], append(path, index))
		}
	}
	walk(before, after, nil)
	if len(changedText) != 1 {
		t.Fatalf("the upload changed %d text nodes (%v), want 1", len(changedText), changedText)
	}
	changed := changedText[0]
	var wantIDs, gotTypes []string
	node := before
	for depth := 1; depth < len(changed); depth++ {
		node = node.Children[changed[depth-1]]
		wantIDs = append(wantIDs, fmt.Sprint(changed[:depth]))
		gotTypes = append(gotTypes, node.Type)
	}
	if !slices.Equal(gotTypes, wantTypes) {
		t.Fatalf("the changed text sits in %q, want %q", gotTypes, wantTypes)
	}
	if !slices.Equal(changedIDs, wantIDs) {
		shown := changedIDs[:min(len(changedIDs), 8)]
		t.Fatalf("%d of %d blocks changed id, want %d (the changed block and its ancestors, at %q); first changed: %q", len(changedIDs), blocks, len(wantIDs), wantIDs, shown)
	}
	liveIDs := blockIDsOf(before)
	node = after
	for depth := 1; depth < len(changed); depth++ {
		node = node.Children[changed[depth-1]]
		if id, ok := storedBlockID(node); !ok || slices.Contains(liveIDs, id) {
			t.Fatalf("the changed %s at %v has id %q, want a fresh one", node.Type, changed[:depth], id)
		}
	}
}

// blockIDsOf is the id of every block in tree that carries one, in document order.
func blockIDsOf(tree *Node) []string {
	var ids []string
	walkBlockIDs(tree, func(id string) bool {
		ids = append(ids, id)
		return true
	})
	return ids
}
