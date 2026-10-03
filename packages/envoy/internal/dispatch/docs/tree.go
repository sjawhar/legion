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

// ErrDocumentTooLarge is a document whose tree encodes to more items than one document update can
// store (ygo's cap of 1,048,576, maxUpdateItems): more formatted spans than any real document
// has, such as 1 MiB of `)_`, which reads as 524,288 italic spans.
var ErrDocumentTooLarge = errors.New("document holds more formatted spans than can be stored")

// parseInput parses markdown a caller writes that replaces no live document: a new document's
// first text, or the empty text a delete splices. Text an insert or an accept writes into a
// document is parseFragmentInput's.
func parseInput(markdown string) (*pmdoc.Node, error) {
	return parseReplacing(nil, markdown)
}

// parseFragmentInput parses text written into a document - an insert's, or an accepted
// suggestion's - where a leading `---` is a rule rather than front matter, except that a closed
// front-matter block is front matter where the text lands at the document's start
// (pmdoc.ParseFragment). A block id the text repeats is refused, as parseInput refuses it. Its
// tables' short rows are padded on budget, which the write's other markdown shares.
func parseFragmentInput(markdown string, opensDocument bool, budget *pmdoc.TablePaddingBudget) (*pmdoc.Node, error) {
	return uploadedInput(pmdoc.ParseFragment(markdown, opensDocument, budget))
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

// parseReplacing parses markdown a caller writes over live, the document it replaces whole (nil
// for none), refusing a block id the markdown repeats that live does not already carry
// (pmdoc.ParseForWrite).
func parseReplacing(live *pmdoc.Node, markdown string) (*pmdoc.Node, error) {
	return uploadedInput(pmdoc.ParseForWrite(markdown, live))
}

func uploadedInput(tree *pmdoc.Node, err error) (*pmdoc.Node, error) {
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
		return nil, docSchema(err)
	}
	return tree, nil
}

func treeOfTransaction(txn *crdt.Transaction, fragment *crdt.YXmlFragment) (*pmdoc.Node, error) {
	if fragment == nil {
		return nil, errDocUnloaded
	}
	tree, err := pmdoc.ReadInTransaction(txn, fragment)
	if err != nil {
		return nil, docSchema(err)
	}
	return tree, nil
}

func renderTree(tree *pmdoc.Node) (string, error) {
	markdown, err := pmdoc.Render(tree)
	if err != nil {
		return "", docSchema(err)
	}
	return markdown, nil
}

// docSchema is err with pmdoc's refusal of what a live document holds (pmdoc.ErrSchema) told as
// that document outside the schema (ErrDocSchema), which the API serves as DOC_SCHEMA: neither the
// caller's input nor an internal fault. Every read and walk of a live tree through pmdoc tells its
// refusal this way, including a walk inside a write's transaction, which a peer can have deepened
// past the schema's depth bound since the write read the tree.
func docSchema(err error) error {
	if errors.Is(err, pmdoc.ErrSchema) {
		return fmt.Errorf("%w: %v", ErrDocSchema, err)
	}
	return err
}

// renderDocument is what doc renders now, or the error that stopped it being read or rendered.
func renderDocument(doc *crdt.Doc) (string, error) {
	tree, err := treeOf(doc)
	if err != nil {
		return "", err
	}
	return renderTree(tree)
}

// closureChangedMarkdown reports whether a document closure that produced after changed the
// canonical markdown a version stores, given what the document rendered before it ran (before,
// beforeErr). It is the measure `content_changed` records for every other write, applied to
// settlement's own: stamping a block id or restoring an ask block's server-owned attributes
// moves the stored Proof state while the rendered document stays as it was.
//
// A document that did not render before the closure counts as changed - the closure is what made
// it renderable - and so does one that does not render after it, since the text a version stores
// has moved out of reach either way.
func closureChangedMarkdown(before string, beforeErr error, after *pmdoc.Node) bool {
	if beforeErr != nil {
		return true
	}
	rendered, err := renderTree(after)
	return err != nil || rendered != before
}
