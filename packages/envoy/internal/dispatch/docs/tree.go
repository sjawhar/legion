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
	tree, err := pmdoc.Parse(markdown)
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

func renderTree(tree *pmdoc.Node) (string, error) {
	markdown, _, err := pmdoc.Render(tree)
	if err != nil {
		if errors.Is(err, pmdoc.ErrSchema) {
			return "", fmt.Errorf("%w: %v", ErrDocSchema, err)
		}
		return "", err
	}
	return markdown, nil
}
