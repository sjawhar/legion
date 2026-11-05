package pmdoc

import "testing"

// The six operations an edit makes over a tree it has already validated - MoveBlock, FindHeading,
// the two table deletes and the two mark reads - are methods on ValidDocument beside the
// plain-tree functions that validate the tree for themselves. Each benchmark below runs the two
// forms over one generated document of a real spec's size (realSizeMarkdown, the smaller of the
// two the tests read), so what separates Function from Method is the one validation of that
// document the method does not pay for: a walk of the whole tree, about a millisecond on this
// document, which allocates nothing. The two forms therefore allocate alike, and what the method
// saves is that walk's time - most of a read that does no other walk (FindHeading, the mark
// reads), and a sixth of one that copies the tree (the deletes, MoveBlock).

// benchmarkDocument is the document those benchmarks read, stamped with block ids because the
// operations name their blocks by id, beside itself as a ValidDocument.
func benchmarkDocument(b *testing.B) (*Node, ValidDocument) {
	b.Helper()
	size := realSizeDocuments[0]
	doc := parseRealSize(b, size.bytes, size.seed)
	EnsureBlockIDs(doc)
	valid, err := ValidateDocument(doc, "benchmark")
	if err != nil {
		b.Fatalf("validate the %s document: %v", size.name, err)
	}
	return doc, valid
}

// benchmarkTableID is the id of a table the deletes and the mark reads can act on: one with a
// body row and a column to spare, so neither operation is refused.
func benchmarkTableID(b *testing.B, doc *Node) string {
	b.Helper()
	var found string
	Walk(doc, func(node *Node) bool {
		if node.Type != "table" || len(node.Children) < 3 {
			return true
		}
		for _, row := range node.Children {
			if len(row.Children) < 2 {
				return true
			}
		}
		found, _ = node.Attrs[BlockIDAttr].(string)
		return found == ""
	})
	if found == "" {
		b.Fatal("the generated document holds no table with a row and a column to spare")
	}
	return found
}

// benchmarkMovedBlockID is the id of the document's last top-level block, which stands at
// document level and so can be moved.
func benchmarkMovedBlockID(b *testing.B, doc *Node) string {
	b.Helper()
	for index := len(doc.Children) - 1; index >= 0; index-- {
		if id, _ := doc.Children[index].Attrs[BlockIDAttr].(string); id != "" {
			return id
		}
	}
	b.Fatal("the generated document holds no top-level block with an id")
	return ""
}

func BenchmarkMoveBlock(b *testing.B) {
	doc, valid := benchmarkDocument(b)
	blockID := benchmarkMovedBlockID(b, doc)
	target := Range{From: 0, To: nodeSize(doc.Children[0])}
	b.Run("Function", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := MoveBlock(doc, blockID, target, true); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Method", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := valid.MoveBlock(blockID, target, true); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkFindHeading(b *testing.B) {
	doc, valid := benchmarkDocument(b)
	titles := headingTitlesIn(doc)
	if len(titles) == 0 {
		b.Fatal("the generated document holds no heading")
	}
	title := titles[len(titles)-1]
	b.Run("Function", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := FindHeading(doc, title, nil); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Method", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := valid.FindHeading(title, nil); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkDeleteTableRow(b *testing.B) {
	doc, valid := benchmarkDocument(b)
	tableID := benchmarkTableID(b, doc)
	b.Run("Function", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := DeleteTableRow(doc, tableID, 1); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Method", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := valid.DeleteTableRow(tableID, 1); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkDeleteTableColumn(b *testing.B) {
	doc, valid := benchmarkDocument(b)
	tableID := benchmarkTableID(b, doc)
	b.Run("Function", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := DeleteTableColumn(doc, tableID, 0); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Method", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := valid.DeleteTableColumn(tableID, 0); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkTableRowMarks(b *testing.B) {
	doc, valid := benchmarkDocument(b)
	tableID := benchmarkTableID(b, doc)
	b.Run("Function", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := TableRowMarks(doc, tableID, 1); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Method", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := valid.TableRowMarks(tableID, 1); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkTableColumnMarks(b *testing.B) {
	doc, valid := benchmarkDocument(b)
	tableID := benchmarkTableID(b, doc)
	b.Run("Function", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := TableColumnMarks(doc, tableID, 0); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Method", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := valid.TableColumnMarks(tableID, 0); err != nil {
				b.Fatal(err)
			}
		}
	})
}
