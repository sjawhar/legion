package docs

import (
	"errors"
	"fmt"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

const fragmentName = "prosemirror"
const marksMapName = "marks"

var ErrInvalidMarkdown = errors.New("markdown is not a Proof document")
var ErrDocSchema = errors.New("document is outside the Proof schema")

// parseInput parses markdown a caller writes that replaces no live document: a fragment an edit or
// an accept places, or a new document's first text.
func parseInput(markdown string) (*pmdoc.Node, error) {
	return parseUpload(nil, markdown)
}

// parseUpload parses markdown that replaces live whole, refusing a repeated block id live does not
// already carry (pmdoc.ParseForWrite).
func parseUpload(live *pmdoc.Node, markdown string) (*pmdoc.Node, error) {
	tree, err := pmdoc.ParseForWrite(markdown, live)
	if err != nil {
		if errors.Is(err, pmdoc.ErrSchema) {
			return nil, fmt.Errorf("%w: %v", ErrInvalidMarkdown, err)
		}
		return nil, err
	}
	return asUploaded(tree), nil
}

// asUploaded is a copy of tree as an upload is taken: without anchor marks, and with every
// server-owned typed-block attribute at its default, since the server owns those.
func asUploaded(tree *pmdoc.Node) *pmdoc.Node {
	out := pmdoc.StripAnchorMarks(tree)
	pmdoc.StripServerOwnedAttrs(out)
	return out
}

// errDocUnloaded is returned for a room whose live document is not resident (evicted, or never
// warmed). Callers that can reload do so; nothing dereferences a nil document.
var errDocUnloaded = errors.New("document is not loaded")

func treeOf(doc *crdt.Doc) (*pmdoc.Node, error) {
	if doc == nil {
		return nil, errDocUnloaded
	}
	tree, err := pmdoc.Read(doc.GetXmlFragment(fragmentName))
	if err != nil {
		if errors.Is(err, pmdoc.ErrSchema) {
			return nil, fmt.Errorf("%w: %v", ErrDocSchema, err)
		}
		return nil, err
	}
	return tree, nil
}

func treeOfTransaction(txn *crdt.Transaction, fragment *crdt.YXmlFragment) (*pmdoc.Node, error) {
	if fragment == nil {
		return nil, errDocUnloaded
	}
	tree, err := pmdoc.ReadInTransaction(txn, fragment)
	if err != nil {
		if errors.Is(err, pmdoc.ErrSchema) {
			return nil, fmt.Errorf("%w: %v", ErrDocSchema, err)
		}
		return nil, err
	}
	return tree, nil
}

func renderTree(tree *pmdoc.Node) (string, error) {
	markdown, err := pmdoc.Render(tree)
	if err != nil {
		if errors.Is(err, pmdoc.ErrSchema) {
			return "", fmt.Errorf("%w: %v", ErrDocSchema, err)
		}
		return "", err
	}
	return markdown, nil
}
