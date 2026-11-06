package pmdoc

// The operations below have one production form, the method on ValidDocument: their callers hold
// a tree validated once (ValidateDocument, ReadUnshared) and read it through that value, so an
// edit pays for one validation of a tree rather than one for each read of it. These helpers are
// that validation in front of the method, for a test that has only a tree - the shape those
// operations' exported functions had before their callers moved. They are test-only on purpose: a
// second production entry point for an operation already cut over is what that cutover removed.

func splice(doc *Node, r Range, with *Node) (*Node, error) {
	// The shape is read before the document is validated, as Splice reads it, so a splice whose
	// shape cannot be read is refused with that error rather than the document's. Splice reads the
	// shape again, which costs this test-only helper one more read of three fields.
	if err := spliceShape(doc, r, with); err != nil {
		return nil, err
	}
	valid, err := ValidateDocument(doc, "Splice")
	if err != nil {
		return nil, err
	}
	return valid.Splice(r, with)
}

func blockBoundary(doc *Node, target Range, after bool) (int, error) {
	valid, err := ValidateDocument(doc, "BlockBoundary")
	if err != nil {
		return 0, err
	}
	return valid.BlockBoundary(target, after)
}

// blockIndex is the index of the document-level block holding target, the answer
// ValidDocument.BlockIndexes gives for a range whose endpoints lie in one block, found the way
// documentBlock finds it. The tests that hold BlockIndexes to its rule read it from here.
func blockIndex(doc *Node, target Range) (int, error) {
	valid, err := ValidateDocument(doc, "BlockIndexes")
	if err != nil {
		return 0, err
	}
	index, _, _, err := documentBlock(valid.Node(), target)
	return index, err
}

func blockRange(doc *Node, blockID string) (Range, error) {
	valid, err := ValidateDocument(doc, "BlockRange")
	if err != nil {
		return Range{}, err
	}
	return valid.BlockRange(blockID)
}

func deleteTextblock(doc *Node, r Range) (*Node, bool, error) {
	valid, err := ValidateDocument(doc, "DeleteTextblock")
	if err != nil {
		return nil, false, err
	}
	return valid.DeleteTextblock(r)
}

func insertTableRows(doc *Node, target Range, markdown string, after bool, budget *WriteBudget) (*Node, bool, error) {
	valid, err := ValidateDocument(doc, "InsertTableRows")
	if err != nil {
		return nil, false, err
	}
	return valid.InsertTableRows(target, markdown, after, budget)
}
