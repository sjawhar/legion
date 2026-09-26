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

func parseInput(markdown string) (*pmdoc.Node, error) {
	return parsedInput(pmdoc.Parse(markdown))
}

// parseFragmentInput parses text written into a document - an insert's, or an accepted
// suggestion's - where a leading `---` is a rule rather than front matter, except that a closed
// front-matter block is front matter where the text lands at the document's start
// (pmdoc.ParseFragment).
func parseFragmentInput(markdown string, opensDocument bool) (*pmdoc.Node, error) {
	return parsedInput(pmdoc.ParseFragment(markdown, opensDocument))
}

// opensDocument reports whether text written at position lands where the document begins: before
// its first block, or at the start of the text of a first block that is a top-level textblock.
func opensDocument(tree *pmdoc.Node, position int) bool {
	if position == 0 {
		return true
	}
	at, ok := pmdoc.ContainingTextblock(tree, position)
	return ok && position == 1 && len(at.Ancestors) == 1
}

func parsedInput(tree *pmdoc.Node, err error) (*pmdoc.Node, error) {
	if err != nil {
		if errors.Is(err, pmdoc.ErrSchema) {
			return nil, fmt.Errorf("%w: %v", ErrInvalidMarkdown, err)
		}
		return nil, err
	}
	pmdoc.StripServerOwnedAttrs(tree)
	return pmdoc.StripAnchorMarks(tree), nil
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
