package pmdoc

import (
	"fmt"
	"reflect"
	"testing"
)

// Six operations an edit makes over a tree it has already validated are methods on ValidDocument,
// each with a plain-tree function of the same name that validates and then calls the method:
// MoveBlock, FindHeading, DeleteTableRow, DeleteTableColumn, TableRowMarks and TableColumnMarks.
// The tests below drive both forms over every corpus document, over every block id, table and
// heading that document holds, and hold the two to one answer: the same tree, by
// reflect.DeepEqual, or the same error text. What each operation does is held by its own tests
// (blocks_test.go, find_test.go, table_test.go); what these hold is that the form an edit batch
// uses cannot drift from the one a caller holding only a tree still uses.

// corpusTrees is every testdata/corpus document parsed and stamped, under the name its failures
// give it (namedTree), since the operations below name their blocks by id.
func corpusTrees(t testing.TB) []namedTree {
	t.Helper()
	documents := corpusMarkdown(t)
	trees := make([]namedTree, 0, len(documents))
	for _, document := range documents {
		tree, err := Parse(document.markdown)
		if err != nil {
			t.Fatalf("parse %s: %v", document.name, err)
		}
		EnsureBlockIDs(tree)
		trees = append(trees, namedTree{name: document.name, tree: tree})
	}
	return trees
}

// blockIDsIn is every block id the tree carries, in document order.
func blockIDsIn(tree *Node) []string {
	var ids []string
	Walk(tree, func(node *Node) bool {
		if node.Type != "doc" && !isInlineNodeType(node.Type) {
			if id, _ := node.Attrs[BlockIDAttr].(string); id != "" {
				ids = append(ids, id)
			}
		}
		return true
	})
	return ids
}

// tableIDsIn is the id of every table in the tree, in document order.
func tableIDsIn(tree *Node) []string {
	var ids []string
	Walk(tree, func(node *Node) bool {
		if node.Type == "table" {
			if id, _ := node.Attrs[BlockIDAttr].(string); id != "" {
				ids = append(ids, id)
			}
		}
		return true
	})
	return ids
}

// headingTitlesIn is the text of every document-level heading, as FindHeading matches it.
func headingTitlesIn(tree *Node) []string {
	var titles []string
	for _, child := range tree.Children {
		if child.Type == "heading" {
			titles = append(titles, TextContent(child))
		}
	}
	return titles
}

// tableProbeIDs are the block ids each table operation is driven over for one document: its
// tables, a block that is no table, and one the document does not carry, so the refusals of both
// forms are compared as well as their answers.
func tableProbeIDs(tree *Node) []string {
	ids := tableIDsIn(tree)
	if blocks := blockIDsIn(tree); len(blocks) > 0 {
		ids = append(ids, blocks[0])
	}
	return append(ids, "no-such-block")
}

// tableIndexProbes are the row or column indexes each table operation is driven over: the two the
// edit path takes, one below the first and one past the last.
var tableIndexProbes = []int{-1, 0, 1, 99}

// errorDiff reports how two errors differ, or "" when they do not, by their text: every caller
// serves an error's own words.
func errorDiff(wantErr, gotErr error) string {
	switch {
	case (wantErr == nil) != (gotErr == nil):
		return fmt.Sprintf("errors: want %v, got %v", wantErr, gotErr)
	case wantErr != nil && wantErr.Error() != gotErr.Error():
		return fmt.Sprintf("errors: want %q, got %q", wantErr.Error(), gotErr.Error())
	}
	return ""
}

// sameTreeAnswer reports how two answers of an operation that returns a tree differ, or "" when
// they do not: the errors by their text and the trees by reflect.DeepEqual, printing the markdown
// each renders as when they differ.
func sameTreeAnswer(wantTree, gotTree *Node, wantErr, gotErr error) string {
	if diff := errorDiff(wantErr, gotErr); diff != "" {
		return diff
	}
	if !reflect.DeepEqual(wantTree, gotTree) {
		return fmt.Sprintf("trees: want %q, got %q", renderedOrNil(wantTree), renderedOrNil(gotTree))
	}
	return ""
}

// renderedOrNil is the tree's markdown, or what stands in its place when there is no tree or it
// cannot be rendered: a failure message, never a panic.
func renderedOrNil(tree *Node) string {
	if tree == nil {
		return "<nil>"
	}
	markdown, err := Render(tree)
	if err != nil {
		return fmt.Sprintf("<render: %v>", err)
	}
	return markdown
}

// A move over a validated tree answers what MoveBlock answers for the same tree: every block of
// every corpus document, moved to the boundary before and after the document's first block.
func TestValidDocumentMoveBlockAnswersMoveBlock(t *testing.T) {
	documents, compared := corpusTrees(t), 0
	for _, document := range documents {
		tree := document.tree
		if len(tree.Children) == 0 {
			continue
		}
		valid := validOf(t, tree)
		target := Range{From: 0, To: nodeSize(tree.Children[0])}
		for _, blockID := range append(blockIDsIn(tree), "no-such-block") {
			for _, after := range []bool{false, true} {
				wantTree, wantErr := MoveBlock(tree, blockID, target, after)
				gotTree, gotErr := valid.MoveBlock(blockID, target, after)
				if diff := sameTreeAnswer(wantTree, gotTree, wantErr, gotErr); diff != "" {
					t.Fatalf("%s: MoveBlock(%q, after=%v) %s", document.name, blockID, after, diff)
				}
				compared++
			}
		}
	}
	t.Logf("%d documents, %d moves compared", len(documents), compared)
}

// A heading search over a validated tree answers what FindHeading answers for the same tree:
// every heading of every corpus document, without an occurrence and at the first two, and a title
// no document holds.
func TestValidDocumentFindHeadingAnswersFindHeading(t *testing.T) {
	documents, compared := corpusTrees(t), 0
	zero, one := 0, 1
	for _, document := range documents {
		tree := document.tree
		valid := validOf(t, tree)
		titles := append(headingTitlesIn(tree), "no document holds this heading \u2014 zzqq")
		for _, title := range titles {
			for _, occurrence := range []*int{nil, &zero, &one} {
				wantRange, wantErr := FindHeading(tree, title, occurrence)
				gotRange, gotErr := valid.FindHeading(title, occurrence)
				if diff := errorDiff(wantErr, gotErr); diff != "" {
					t.Fatalf("%s: FindHeading(%q, %v) %s", document.name, title, occurrence, diff)
				}
				if wantRange != gotRange {
					t.Fatalf("%s: FindHeading(%q, %v) ranges: want %#v, got %#v",
						document.name, title, occurrence, wantRange, gotRange)
				}
				compared++
			}
		}
	}
	t.Logf("%d documents, %d heading searches compared", len(documents), compared)
}

// A row delete over a validated tree answers what DeleteTableRow answers for the same tree: every
// table of every corpus document, at the index the edit path takes and at the edges of the range.
func TestValidDocumentDeleteTableRowAnswersDeleteTableRow(t *testing.T) {
	documents, compared, tables := corpusTrees(t), 0, 0
	for _, document := range documents {
		tree := document.tree
		valid := validOf(t, tree)
		tables += len(tableIDsIn(tree))
		for _, blockID := range tableProbeIDs(tree) {
			for _, index := range tableIndexProbes {
				wantTree, wantErr := DeleteTableRow(tree, blockID, index)
				gotTree, gotErr := valid.DeleteTableRow(blockID, index)
				if diff := sameTreeAnswer(wantTree, gotTree, wantErr, gotErr); diff != "" {
					t.Fatalf("%s: DeleteTableRow(%q, %d) %s", document.name, blockID, index, diff)
				}
				compared++
			}
		}
	}
	t.Logf("%d documents, %d tables, %d row deletes compared", len(documents), tables, compared)
}

// A column delete over a validated tree answers what DeleteTableColumn answers for the same tree.
func TestValidDocumentDeleteTableColumnAnswersDeleteTableColumn(t *testing.T) {
	documents, compared, tables := corpusTrees(t), 0, 0
	for _, document := range documents {
		tree := document.tree
		valid := validOf(t, tree)
		tables += len(tableIDsIn(tree))
		for _, blockID := range tableProbeIDs(tree) {
			for _, index := range tableIndexProbes {
				wantTree, wantErr := DeleteTableColumn(tree, blockID, index)
				gotTree, gotErr := valid.DeleteTableColumn(blockID, index)
				if diff := sameTreeAnswer(wantTree, gotTree, wantErr, gotErr); diff != "" {
					t.Fatalf("%s: DeleteTableColumn(%q, %d) %s", document.name, blockID, index, diff)
				}
				compared++
			}
		}
	}
	t.Logf("%d documents, %d tables, %d column deletes compared", len(documents), tables, compared)
}

// The marks a row delete would remove, read over a validated tree, are the marks TableRowMarks
// reads for the same tree, and a refused index is refused with the same words.
func TestValidDocumentTableRowMarksAnswersTableRowMarks(t *testing.T) {
	documents, compared, tables := corpusTrees(t), 0, 0
	for _, document := range documents {
		tree := document.tree
		valid := validOf(t, tree)
		tables += len(tableIDsIn(tree))
		for _, blockID := range tableProbeIDs(tree) {
			for _, index := range tableIndexProbes {
				wantMarks, wantErr := TableRowMarks(tree, blockID, index)
				gotMarks, gotErr := valid.TableRowMarks(blockID, index)
				if diff := errorDiff(wantErr, gotErr); diff != "" {
					t.Fatalf("%s: TableRowMarks(%q, %d) %s", document.name, blockID, index, diff)
				}
				if !reflect.DeepEqual(wantMarks, gotMarks) {
					t.Fatalf("%s: TableRowMarks(%q, %d) marks: want %#v, got %#v",
						document.name, blockID, index, wantMarks, gotMarks)
				}
				compared++
			}
		}
	}
	t.Logf("%d documents, %d tables, %d row mark reads compared", len(documents), tables, compared)
}

// The marks a column delete would remove, read over a validated tree, are the marks
// TableColumnMarks reads for the same tree.
func TestValidDocumentTableColumnMarksAnswersTableColumnMarks(t *testing.T) {
	documents, compared, tables := corpusTrees(t), 0, 0
	for _, document := range documents {
		tree := document.tree
		valid := validOf(t, tree)
		tables += len(tableIDsIn(tree))
		for _, blockID := range tableProbeIDs(tree) {
			for _, index := range tableIndexProbes {
				wantMarks, wantErr := TableColumnMarks(tree, blockID, index)
				gotMarks, gotErr := valid.TableColumnMarks(blockID, index)
				if diff := errorDiff(wantErr, gotErr); diff != "" {
					t.Fatalf("%s: TableColumnMarks(%q, %d) %s", document.name, blockID, index, diff)
				}
				if !reflect.DeepEqual(wantMarks, gotMarks) {
					t.Fatalf("%s: TableColumnMarks(%q, %d) marks: want %#v, got %#v",
						document.name, blockID, index, wantMarks, gotMarks)
				}
				compared++
			}
		}
	}
	t.Logf("%d documents, %d tables, %d column mark reads compared", len(documents), tables, compared)
}

// The zero ValidDocument refuses each of the six with the words its plain-tree function gives a
// nil tree, and reaches no nil dereference.
func TestTheZeroValidDocumentRefusesTheBlockAndTableOperations(t *testing.T) {
	zero := ValidDocument{}
	for _, test := range []struct {
		name string
		want func() error
		got  func() error
	}{
		{
			name: "MoveBlock",
			want: func() error { _, err := MoveBlock(nil, "b", Range{}, true); return err },
			got:  func() error { _, err := zero.MoveBlock("b", Range{}, true); return err },
		},
		{
			name: "FindHeading",
			want: func() error { _, err := FindHeading(nil, "title", nil); return err },
			got:  func() error { _, err := zero.FindHeading("title", nil); return err },
		},
		{
			name: "DeleteTableRow",
			want: func() error { _, err := DeleteTableRow(nil, "b", 0); return err },
			got:  func() error { _, err := zero.DeleteTableRow("b", 0); return err },
		},
		{
			name: "DeleteTableColumn",
			want: func() error { _, err := DeleteTableColumn(nil, "b", 0); return err },
			got:  func() error { _, err := zero.DeleteTableColumn("b", 0); return err },
		},
		{
			name: "TableRowMarks",
			want: func() error { _, err := TableRowMarks(nil, "b", 0); return err },
			got:  func() error { _, err := zero.TableRowMarks("b", 0); return err },
		},
		{
			name: "TableColumnMarks",
			want: func() error { _, err := TableColumnMarks(nil, "b", 0); return err },
			got:  func() error { _, err := zero.TableColumnMarks("b", 0); return err },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			want, got := test.want(), test.got()
			if !isSchemaRefusal(got, test.name+" wants a document") {
				t.Fatalf("the zero ValidDocument's %s = %v, want the wants-a-document refusal", test.name, got)
			}
			if diff := errorDiff(want, got); diff != "" {
				t.Fatalf("the zero ValidDocument's %s %s", test.name, diff)
			}
		})
	}
}
