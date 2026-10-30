package pmdoc

import "fmt"

// ValidDocument is a document tree that has been validated once (Validate), so that several reads
// of it pay for one validation rather than one each. ValidateDocument and ReadUnshared make one,
// after the validation passes, and so does a read inside this package that has just validated the
// tree it holds. The contract is the holder's: nothing changes the tree after it was validated,
// and every method reads the tree without validating it again.
//
// Most of what this type answers has no other form: a caller holding a plain tree validates it
// first (ValidateDocument) and reads it through the value that gives. Where an exported function
// still takes a plain tree (BlockPathsAndTableRows, TableRows, TableDescendantIDs,
// RenderWithBlockOffsets, EncodeTokenJSON, DeleteBlock, Update), it is that validation in front of
// the same core, so the two answer alike.
//
// The zero ValidDocument holds no tree and is not valid: each method refuses with the words
// ValidateDocument refuses a tree that is no document with (wantsDocument).
type ValidDocument struct {
	doc *Node
}

// ValidateDocument is doc as a ValidDocument once doc is a document (its type is doc) and
// Validate accepts it. A nil or non-document tree is ErrSchema naming operation, the function
// that wants the document ("BlockRange wants a document"), and a tree Validate refuses is
// Validate's error, so every caller refuses a tree with the message it always gave.
func ValidateDocument(doc *Node, operation string) (ValidDocument, error) {
	if doc == nil || doc.Type != "doc" {
		return ValidDocument{}, wantsDocument(operation)
	}
	if err := doc.Validate(); err != nil {
		return ValidDocument{}, err
	}
	return ValidDocument{doc: doc}, nil
}

// wantsDocument refuses a read of a tree that is no document, naming the read that wanted one.
func wantsDocument(operation string) error {
	return fmt.Errorf("%w: %s wants a document", ErrSchema, operation)
}

// Node is the validated tree, nil for the zero ValidDocument. The caller must not change it.
func (d ValidDocument) Node() *Node {
	return d.doc
}
