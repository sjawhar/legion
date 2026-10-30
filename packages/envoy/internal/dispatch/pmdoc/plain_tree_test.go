package pmdoc

// The package's own tests address a plain tree, where every caller of this package now holds a
// validated one (ValidDocument): an edit validates each tree it touches once and reads it through
// that value, so the operations below have one form, the method. These helpers are that validation
// in front of the method, for a test that has only a tree - the shape the exported functions had
// before the callers moved. They are test-only on purpose: a second production entry point per
// operation is what this package was cut down to remove.

func splice(doc *Node, r Range, with *Node) (*Node, error) {
	if err := spliceShape(doc, r, with); err != nil {
		return nil, err
	}
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	return ValidDocument{doc: doc}.spliceValidated(r, with)
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
	valid, err := ValidateDocument(doc, "BlockIndex")
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

func blockDescendantIDs(doc *Node, blockID string) ([]string, error) {
	valid, err := ValidateDocument(doc, "BlockDescendantIDs")
	if err != nil {
		return nil, err
	}
	return valid.BlockDescendantIDs(blockID)
}

func deleteTextblock(doc *Node, r Range) (*Node, bool, error) {
	valid, err := ValidateDocument(doc, "DeleteTextblock")
	if err != nil {
		return nil, false, err
	}
	return valid.DeleteTextblock(r)
}

func insertTableRows(doc *Node, target Range, markdown string, after bool, budget *WriteBudget) (*Node, bool, error) {
	if doc == nil || doc.Type != "doc" {
		return nil, false, wantsDocument("InsertTableRows")
	}
	if err := doc.Validate(); err != nil {
		return nil, false, err
	}
	return ValidDocument{doc: doc}.InsertTableRows(target, markdown, after, budget)
}
