package pmdoc

import "fmt"

// ValidDocument is a document tree that has been validated once (Validate), so that several reads
// of it pay for one validation rather than one each. Only ValidateDocument and ReadUnshared make
// one, after the validation passes. The contract is the holder's: nothing changes the tree after
// it was validated, and every method reads the tree without validating it again, so a method's
// answer is the answer its exported counterpart (BlockPathsAndTableRows, TableRows,
// TableDescendantIDs, RenderWithBlockOffsets, EncodeTokenJSON) gives the same tree.
//
// The zero ValidDocument holds no tree and is not valid: each method returns the error its
// exported counterpart returns for a nil tree.
type ValidDocument struct {
	doc *Node
}

// ValidateDocument is doc as a ValidDocument once doc is a document (its type is doc) and
// Validate accepts it. A nil or non-document tree is ErrSchema naming operation, the function
// that wants the document ("BlockRange wants a document"), and a tree Validate refuses is
// Validate's error, so every caller refuses a tree with the message it always gave.
func ValidateDocument(doc *Node, operation string) (ValidDocument, error) {
	if doc == nil || doc.Type != "doc" {
		return ValidDocument{}, fmt.Errorf("%w: %s wants a document", ErrSchema, operation)
	}
	if err := doc.Validate(); err != nil {
		return ValidDocument{}, err
	}
	return ValidDocument{doc: doc}, nil
}

// Node is the validated tree, nil for the zero ValidDocument. The caller must not change it.
func (d ValidDocument) Node() *Node {
	return d.doc
}
