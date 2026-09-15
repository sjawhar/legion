package pmdoc

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// sequentialBlockIDs makes Parse stamp b1, b2, … in document order so tests can name
// untyped blocks.
func sequentialBlockIDs(t *testing.T) {
	t.Helper()
	next := 0
	SetBlockIDGenerator(func() string {
		next++
		return fmt.Sprintf("b%d", next)
	})
	t.Cleanup(func() { SetBlockIDGenerator(nil) })
}

func parseFixture(t *testing.T, markdown string) *Node {
	t.Helper()
	doc, err := Parse(markdown)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestDeleteBlockRemovesAnyBlockByID(t *testing.T) {
	sequentialBlockIDs(t)
	// b1 heading, b2 ask body paragraph, b3 paragraph, b4 ordered list, b5 item, b6 item paragraph.
	doc := parseFixture(t, "# Title\n\n:::ask{#decision urgency=\"high\" multiple=\"false\" state=\"open\"}\nWhich?\n:::\n\nBody.\n\n1. one\n")
	for _, test := range []struct {
		name, id, want string
	}{
		{name: "typed block", id: "decision", want: "# Title\n\nBody.\n\n1. one\n"},
		{name: "heading", id: "b1", want: ":::ask{#decision urgency=\"high\" multiple=\"false\" state=\"open\"}\nWhich?\n:::\n\nBody.\n\n1. one\n"},
		{name: "list", id: "b4", want: "# Title\n\n:::ask{#decision urgency=\"high\" multiple=\"false\" state=\"open\"}\nWhich?\n:::\n\nBody.\n"},
		{name: "last list item removes the list", id: "b5", want: "# Title\n\n:::ask{#decision urgency=\"high\" multiple=\"false\" state=\"open\"}\nWhich?\n:::\n\nBody.\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			out, err := DeleteBlock(doc, test.id)
			if err != nil {
				t.Fatal(err)
			}
			assertRenderedMarkdown(t, out, test.want)
		})
	}
}

func TestDeleteBlockLeavesAnEmptyParagraphWhenTheDocumentEmpties(t *testing.T) {
	sequentialBlockIDs(t)
	doc := parseFixture(t, "Only.\n")
	out, err := DeleteBlock(doc, "b1")
	if err != nil {
		t.Fatal(err)
	}
	if !emptyDocument(out) {
		t.Fatalf("emptied document = %#v, want one empty paragraph", out.Children)
	}
}

func TestDeleteBlockReportsUnknownAndUnremovableBlocks(t *testing.T) {
	sequentialBlockIDs(t)
	doc := parseFixture(t, ":::ask{#decision urgency=\"high\" multiple=\"false\" state=\"open\"}\nWhich?\n:::\n")
	if _, err := DeleteBlock(doc, "missing"); !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("unknown block error = %v, want ErrTargetNotFound", err)
	}
	// b1 is the ask's only paragraph; removing it leaves an ask with no body.
	if _, err := DeleteBlock(doc, "b1"); !errors.Is(err, ErrSchema) {
		t.Fatalf("emptied typed block error = %v, want ErrSchema", err)
	}
}

func TestDeleteTextblockRemovesTheBlockWhoseWholeTextIsDeleted(t *testing.T) {
	sequentialBlockIDs(t)
	const fixture = "- first item\n- second item\n\nA paragraph.\n\n| Key | Value |\n| --- | --- |\n| A10 | old |\n"
	const table = "| Key | Value |\n| :--- | :--- |\n| A10 | old |\n"
	for _, test := range []struct {
		name, quote string
		removed     bool
		want        string
	}{
		{name: "list item text removes the item", quote: "first item", removed: true, want: "- second item\n\nA paragraph.\n\n" + table},
		{name: "paragraph text removes the paragraph", quote: "A paragraph.", removed: true, want: "- first item\n- second item\n\n" + table},
		{name: "partial text keeps the block", quote: "paragraph"},
		{name: "table cell text keeps the cell", quote: "old"},
	} {
		t.Run(test.name, func(t *testing.T) {
			doc := parseFixture(t, fixture)
			r, err := FindQuote(doc, test.quote, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			out, removed, err := DeleteTextblock(doc, r)
			if err != nil || removed != test.removed {
				t.Fatalf("DeleteTextblock(%q) removed=%t err=%v, want removed=%t", test.quote, removed, err, test.removed)
			}
			if removed {
				assertRenderedMarkdown(t, out, test.want)
			}
		})
	}
}

// Deleting a parent bullet's text hoists its nested list's items into its place, the way an
// outliner does; the hoisted items keep their ids.
func TestDeleteTextblockHoistsANestedListIntoTheParentBulletsPlace(t *testing.T) {
	for _, test := range []struct {
		name, markdown, want string
		hoisted              []string
	}{
		{
			name:     "bullet list",
			markdown: "- Parent\n  - child one\n  - child two\n- Sibling\n",
			want:     "- child one\n- child two\n- Sibling\n",
			// b1 list, b2 Parent item, b3 paragraph, b4 nested list, b5 child one, b6, b7 child two, b8, b9 Sibling.
			hoisted: []string{"b5", "b7", "b9"},
		},
		{
			name:     "ordered list with a nested bullet list",
			markdown: "1. Parent\n   - child one\n   - child two\n2. Sibling\n",
			want:     "1. child one\n2. child two\n3. Sibling\n",
			hoisted:  []string{"b5", "b7", "b9"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			sequentialBlockIDs(t)
			doc := parseFixture(t, test.markdown)
			r, err := FindQuote(doc, "Parent", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			out, removed, err := DeleteTextblock(doc, r)
			if err != nil || !removed {
				t.Fatalf("DeleteTextblock(Parent) removed=%t err=%v", removed, err)
			}
			assertRenderedMarkdown(t, out, test.want)
			for index, want := range test.hoisted {
				if got := out.Children[0].Children[index].Attrs[BlockIDAttr]; got != want {
					t.Fatalf("hoisted item %d id = %v, want %s", index, got, want)
				}
			}
		})
	}
}

func TestDeleteTextblockRefusesABulletWithOtherContentAndNamesTheItem(t *testing.T) {
	sequentialBlockIDs(t)
	// b1 list, b2 Parent item, b3 paragraph, b4 extra paragraph, b5 Sibling item.
	doc := parseFixture(t, "- Parent\n\n  Extra paragraph.\n\n- Sibling\n")
	r, err := FindQuote(doc, "Parent", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = DeleteTextblock(doc, r)
	if !errors.Is(err, ErrListItemContent) || !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), `delete {block:"b2"}`) {
		t.Fatalf("delete of a bullet with other content = %v, want ErrListItemContent naming delete {block:\"b2\"}", err)
	}
}

func TestBlockRangeCoversTheBlock(t *testing.T) {
	sequentialBlockIDs(t)
	doc := parseFixture(t, "Intro.\n\n- item\n")
	r, err := BlockRange(doc, "b2")
	if err != nil {
		t.Fatal(err)
	}
	// "Intro." paragraph is 8 positions; the list wraps the item.
	if r != (Range{From: 8, To: 8 + nodeSize(doc.Children[1])}) {
		t.Fatalf("list range = %#v", r)
	}
	if _, err := BlockRange(doc, "missing"); !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("unknown block error = %v, want ErrTargetNotFound", err)
	}
}

func TestMoveBlockRelocatesTheBlockWithItsIdentity(t *testing.T) {
	sequentialBlockIDs(t)
	const ask = ":::ask{#decision urgency=\"high\" multiple=\"false\" state=\"open\"}\nWhich?\n:::\n"
	doc := parseFixture(t, ask+"\n# Context\n\nThe context ends with no drift.\n\n## Next\n")
	anchor, err := FindQuote(doc, "no drift.", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := MoveBlock(doc, "decision", anchor, true)
	if err != nil {
		t.Fatal(err)
	}
	assertRenderedMarkdown(t, out, "# Context\n\nThe context ends with no drift.\n\n"+ask+"\n## Next\n")
	if got := out.Children[2].Attrs[BlockIDAttr]; got != "decision" {
		t.Fatalf("moved block id = %v, want decision", got)
	}

	heading, err := FindHeading(doc, "Next", nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err = MoveBlock(doc, "b2", heading, false)
	if err != nil {
		t.Fatal(err)
	}
	assertRenderedMarkdown(t, out, ask+"\nThe context ends with no drift.\n\n# Context\n\n## Next\n")

	out, err = MoveBlock(doc, "b4", Range{}, false)
	if err != nil {
		t.Fatal(err)
	}
	assertRenderedMarkdown(t, out, "## Next\n\n"+ask+"\n# Context\n\nThe context ends with no drift.\n")
}

func TestMoveBlockRejectsAnAnchorInsideTheMovedBlock(t *testing.T) {
	sequentialBlockIDs(t)
	doc := parseFixture(t, ":::ask{#decision urgency=\"high\" multiple=\"false\" state=\"open\"}\nWhich transport?\n:::\n\nAfter.\n")
	inside, err := FindQuote(doc, "transport", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MoveBlock(doc, "decision", inside, true); !errors.Is(err, ErrMoveInsideItself) {
		t.Fatalf("quote inside moved block error = %v, want ErrMoveInsideItself", err)
	}
	self, err := BlockRange(doc, "decision")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MoveBlock(doc, "decision", self, false); !errors.Is(err, ErrMoveInsideItself) {
		t.Fatalf("block anchored to itself error = %v, want ErrMoveInsideItself", err)
	}
	if _, err := MoveBlock(doc, "missing", Range{}, false); !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("unknown block error = %v, want ErrTargetNotFound", err)
	}
}

func TestMoveBlockRejectsABlockThatCannotStandAtDocumentLevel(t *testing.T) {
	sequentialBlockIDs(t)
	// b1 list, b2 item, b3 item paragraph, b4 paragraph.
	doc := parseFixture(t, "- item\n\nAfter.\n")
	anchor, err := FindQuote(doc, "After.", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MoveBlock(doc, "b2", anchor, true); !errors.Is(err, ErrSchema) {
		t.Fatalf("moving a list item to document level error = %v, want ErrSchema", err)
	}
}

func TestRetypeBlockRetypesATypedBlock(t *testing.T) {
	sequentialBlockIDs(t)
	doc := parseFixture(t, ":::ask{#decision urgency=\"high\" multiple=\"false\" state=\"open\"}\nWhich?\n\n- A: first\n:::\n")
	out, err := RetypeBlock(doc, "decision", "callout", Attrs{"kind": "note"})
	if err != nil {
		t.Fatalf("retype ask block: %v", err)
	}
	assertRenderedMarkdown(t, out, ":::callout{#decision kind=\"note\" title=\"\"}\nWhich?\n\n- A: first\n:::\n")
}

func TestRetypeBlockRejectsBlocksThatAreNeitherParagraphsNorTyped(t *testing.T) {
	sequentialBlockIDs(t)
	doc := parseFixture(t, "# Heading\n")
	_, err := RetypeBlock(doc, "b1", "callout", nil)
	if !errors.Is(err, ErrSchema) || errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("retype heading error = %v, want a schema error, not target not found", err)
	}
}
