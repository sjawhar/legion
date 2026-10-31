package docs

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"weak"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

const fragmentName = "prosemirror"
const marksMapName = "marks"

var (
	ErrInvalidMarkdown = errors.New("markdown is not a Proof document")
	ErrDocSchema       = errors.New("document is outside the Proof schema")
	// ErrDocOutsideSchema is the tree a document holds - not one an operation just produced -
	// refused by the Proof schema's reader or renderer. A replacement from markdown repairs it, so
	// every route answers it alike (OutsideSchemaError).
	ErrDocOutsideSchema = errors.New("document is outside the Proof schema; replace the document from markdown to repair it")
	// ErrDocumentUnloadable is a document whose stored history does not decode, so nothing can read
	// or open it. A rebuild from its latest saved version restores it (RebuildDocument).
	ErrDocumentUnloadable = errors.New("the document's stored history cannot load; rebuild it from its latest saved version")
	ErrDocumentLive       = errors.New("document is live")
	ErrDocumentLoads      = errors.New("this document loads; nothing to rebuild — if it is outside the schema, replace it from markdown to repair it")
)

// OutsideSchemaError is ErrDocOutsideSchema with the refusal that names what is wrong. treeOf and
// documentMarkdown classify a document's tree by it where they read or render it, so an operation
// that starts from that tree - a read, an edit, a version - fails with it whatever route called
// the operation and whatever the route wrapped it in, and its message is the same on every route.
type OutsideSchemaError struct{ Cause error }

func (e *OutsideSchemaError) Error() string {
	return ErrDocOutsideSchema.Error() + ": " + e.Cause.Error()
}

// Is makes it ErrDocOutsideSchema and ErrDocSchema both: a check that a tree is outside the
// schema holds whoever classified it.
func (e *OutsideSchemaError) Is(target error) bool {
	return target == ErrDocOutsideSchema || target == ErrDocSchema
}

func (e *OutsideSchemaError) Unwrap() error { return e.Cause }

// documentSchemaError classifies err from reading, rendering or walking the tree a document holds -
// a walk inside a write's transaction included, since a peer can have deepened that tree past the
// schema's depth bound after the write read it: a schema refusal is an OutsideSchemaError, and any
// other error is returned as it is.
func documentSchemaError(err error) error {
	if err == nil || errors.Is(err, ErrDocOutsideSchema) {
		return err
	}
	if errors.Is(err, ErrDocSchema) || errors.Is(err, pmdoc.ErrSchema) {
		return &OutsideSchemaError{Cause: err}
	}
	return err
}

// producedSchemaError is err from reading or rendering a tree an operation has just written: a
// schema refusal there is the operation's fault, not the stored document's, so it is a plain
// ErrDocSchema that no route answers as a repair.
func producedSchemaError(err error) error {
	var outside *OutsideSchemaError
	if errors.As(err, &outside) {
		return fmt.Errorf("%w: the write left it outside: %v", ErrDocSchema, outside.Cause)
	}
	return err
}

// ErrDocumentTooLarge is the refusal of a write that would leave a document larger than the server
// stores: past what one upload of it may hold (refuseGrowth), with a live state ygo could not decode
// (refuseUndecodable), or holding more items than one document update can store (ygo's cap of
// 1,048,576, maxUpdateItems). It is served as 413 CAP_EXCEEDED, as markdown making more elements
// than one write may (pmdoc.ErrTooManyElements) is.
var ErrDocumentTooLarge = errors.New("document too large to store")

// IsTooLarge reports a refusal of a write too large to store: markdown making more elements than
// one write may (pmdoc.ErrTooManyElements), or a document larger than the server stores
// (ErrDocumentTooLarge). Every route serves it as 413 CAP_EXCEEDED in its own words, which say
// what to shorten.
func IsTooLarge(err error) bool {
	return errors.Is(err, ErrDocumentTooLarge) || errors.Is(err, pmdoc.ErrTooManyElements)
}

// wrapUnlessTooLarge is err behind context, which names the write that failed, unless err refuses a
// write too large to store (IsTooLarge), which reaches its route in the bound's own words.
func wrapUnlessTooLarge(err error, context string) error {
	if IsTooLarge(err) {
		return err
	}
	return fmt.Errorf("%s: %w", context, err)
}

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
func parseFragmentInput(markdown string, opensDocument bool, budget *pmdoc.WriteBudget) (*pmdoc.Node, error) {
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
// for none), refusing a block id the markdown repeats that live does not already repeat
// (pmdoc.ParseForWrite).
func parseReplacing(live *pmdoc.Node, markdown string) (*pmdoc.Node, error) {
	return uploadedInput(pmdoc.ParseForWrite(markdown, live))
}

// uploadedInput is a tree parsed from caller markdown as an upload is taken (asUploaded). A
// refusal the schema makes of the markdown is ErrInvalidMarkdown, the caller's to fix.
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

// encodeDocumentTree writes tree into a fresh document and returns the first durable update for
// callers that create or rebuild a document outside a live room.
func encodeDocumentTree(tree *pmdoc.Node) ([]byte, error) {
	doc := crdt.New()
	fragment := doc.GetXmlFragment(fragmentName)
	doc.GetMap(marksMapName)
	if err := doc.TransactE(func(transaction *crdt.Transaction) error {
		return pmdoc.Update(transaction, fragment, tree)
	}); err != nil {
		return nil, err
	}
	return crdt.EncodeStateAsUpdateV1(doc, nil), nil
}

// errDocUnloaded is returned for a room whose live document is not resident (evicted, or never
// warmed). Callers that can reload do so; nothing dereferences a nil document.
var errDocUnloaded = errors.New("document is not loaded")

// treeOf reads the tree doc holds. A tree outside the Proof schema is an OutsideSchemaError: an
// operation that reads a tree it has just written takes that back with producedSchemaError.
func treeOf(doc *crdt.Doc) (*pmdoc.Node, error) {
	if doc == nil {
		return nil, errDocUnloaded
	}
	tree, err := pmdoc.Read(doc.GetXmlFragment(fragmentName))
	if err != nil {
		return nil, documentSchemaError(err)
	}
	return tree, nil
}

func treeOfTransaction(txn *crdt.Transaction, fragment *crdt.YXmlFragment) (*pmdoc.Node, error) {
	if fragment == nil {
		return nil, errDocUnloaded
	}
	tree, err := pmdoc.ReadInTransaction(txn, fragment)
	if err != nil {
		return nil, documentSchemaError(err)
	}
	return tree, nil
}

// rewriteLive rewrites doc's tree in one transaction tagged with origin. edit is handed the tree as
// it stands, read inside that transaction, which holds the document's lock, and reports whether it
// changed the tree; only a changed tree is written. A repair computed on a tree read before the
// transaction and written back would revert whatever another writer wrote after that read
// (LEGION-479). It returns the tree as the transaction left it and whether edit changed it.
func rewriteLive(doc *crdt.Doc, origin any, edit func(live *pmdoc.Node) bool) (*pmdoc.Node, bool, error) {
	fragment := doc.GetXmlFragment(fragmentName)
	var live *pmdoc.Node
	changed := false
	err := doc.TransactE(func(transaction *crdt.Transaction) error {
		var readErr error
		if live, readErr = treeOfTransaction(transaction, fragment); readErr != nil {
			return readErr
		}
		if changed = edit(live); !changed {
			return nil
		}
		return pmdoc.Update(transaction, fragment, live)
	}, origin)
	if err != nil {
		return nil, false, err
	}
	return live, changed, nil
}

func renderTree(tree *pmdoc.Node) (string, error) {
	markdown, _, err := renderTreeWithOffsets(tree)
	return markdown, err
}

// renderTreeWithOffsets is renderTree with each identified block's byte range in the markdown,
// which the same render records, so a write that stores the rendering indexes its blocks and cuts
// its mentions' excerpts (indexDocument) from it rather than rendering the tree again for them.
func renderTreeWithOffsets(tree *pmdoc.Node) (string, []pmdoc.BlockOffset, error) {
	markdown, offsets, err := pmdoc.RenderWithBlockOffsets(tree)
	if err != nil {
		if errors.Is(err, pmdoc.ErrSchema) {
			return "", nil, fmt.Errorf("%w: %v", ErrDocSchema, err)
		}
		return "", nil, err
	}
	return markdown, offsets, nil
}

// documentMarkdown renders a tree treeOf read from a document, so a tree only the renderer refuses
// is that document's OutsideSchemaError, as treeOf makes one the reader refuses.
func documentMarkdown(tree *pmdoc.Node) (string, error) {
	markdown, _, err := documentMarkdownWithOffsets(tree)
	return markdown, err
}

// documentMarkdownWithOffsets is documentMarkdown with each identified block's byte range, from the
// same render.
func documentMarkdownWithOffsets(tree *pmdoc.Node) (string, []pmdoc.BlockOffset, error) {
	markdown, offsets, err := pmdoc.RenderWithBlockOffsets(tree)
	return markdown, offsets, documentSchemaError(err)
}

// documentRenderingWithLayout is documentMarkdownWithOffsets that records the rendering's
// pmdoc.Layout as well, from the same render, for a rendering the room's replica may keep and
// splice a changed block into later (renderedReplica.updateChangesMarkdown).
func documentRenderingWithLayout(tree *pmdoc.Node) (string, []pmdoc.BlockOffset, *pmdoc.Layout, error) {
	markdown, offsets, layout, err := pmdoc.RenderWithLayout(tree)
	return markdown, offsets, layout, documentSchemaError(err)
}

// renderDocument is what doc renders now, or the error that stopped it being read or rendered.
func renderDocument(doc *crdt.Doc) (string, error) {
	tree, err := treeOf(doc)
	if err != nil {
		return "", err
	}
	return documentMarkdown(tree)
}

// renderDocumentForUpdate is renderDocument of the document whose fragment is fragment, without
// pmdoc.Read's deep copy (pmdoc.ReadForRendering), recording the rendering's layout from the same
// render: for the update observer's render, which runs the render and discards the tree before
// releasing the replica's lock (renderedReplica.updateChangesMarkdown), so nothing retains the
// tree past this call to alias a later reader - a caller that reads tree past the call (the ask
// blocks updateChangesMarkdown hands classify, walked before the lock is released) aliases none of
// it by doing so, since tree itself, not a copy of it, is what the caller walks.
func renderDocumentForUpdate(fragment *crdt.YXmlFragment) (string, *pmdoc.Node, *pmdoc.Layout, error) {
	if fragment == nil {
		return "", nil, nil, errDocUnloaded
	}
	tree, err := pmdoc.ReadForRendering(fragment)
	if err != nil {
		return "", nil, nil, documentSchemaError(err)
	}
	markdown, _, layout, err := documentRenderingWithLayout(tree)
	return markdown, tree, layout, err
}

// snapshotDocument copies doc's state as of one moment. Encoding it takes the document's lock, which
// every peer update and service write holds while it applies, so the copy is never a tree half
// way through a write - which a direct walk of a resident room's live tree can read, since the
// walk takes no lock (sjawhar/ygo v1.51.3-sami.1, crdt/yxml.go:353-369) - and nothing writes the
// copy.
func snapshotDocument(doc *crdt.Doc) (*crdt.Doc, error) {
	snapshot := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(snapshot, crdt.EncodeStateAsUpdateV1(doc, nil), nil); err != nil {
		return nil, fmt.Errorf("copy live document: %w", err)
	}
	return snapshot, nil
}

// newDocumentCopy is a document to decode a document's state into: a snapshot or a write's fork of
// a room this server holds, the document's stored history (loadTree, validateUpdate, and the
// fold of its stored updates and its read-back, stateThrough), or one update the store appends
// (appendUpdate, AppendUpdateTx), decoded alone. Its pending queue is maxUpdateItems, the queue
// every decode of document bytes takes (see maxUpdateItems).
//
// It replays without ygo's format cleanup (crdt.WithFormatCleanup), which follows each remote
// apply that changes formatted text with a commit of its own: every update a copy applies is one
// the room committed, and the room's own cleanups are already among them, so a copy that cleaned
// up would hold a tree the room never held. A write's fork commits its own writes locally, which
// never clean up, as the room's own writes do not.
func newDocumentCopy(options ...crdt.DocOption) *crdt.Doc {
	return crdt.New(append(options, crdt.WithMaxPendingItems(maxUpdateItems), crdt.WithFormatCleanup(false))...)
}

// forkTreeCounts counts the trees read off a write transaction's fork, and the reads that taking
// the write's rendering in place of one avoided, so a test can assert that a write reads its fork
// no more often than it means to, and a measurement build can report how often the read is
// avoided. The service keeps one (Service.forkTrees). Nothing in the service reads them.
type forkTreeCounts struct {
	// read counts the trees read off a write transaction's fork (readFork): an operation's reads
	// before and after its write (applyLive), a margin write's read when the write holds no
	// rendering of its fork (applyMargin), and the reads joined to the transaction (docTree,
	// heldTree, captureLiveTextAndAuthors).
	read atomic.Uint64
	// renderingReused counts the margin writes (applyMargin) and the joined reads that took the
	// write's rendering in place of reading the fork (reuseRendering), each a fork read avoided.
	renderingReused atomic.Uint64
}

// listResident lists value under live, a room's resident document, in listing until live is
// collected: the key holds live weakly, so the listing never keeps an evicted room's document
// alive, and the entry goes once that document is collected.
func listResident(listing *sync.Map, live *crdt.Doc, value any) {
	key := weak.Make(live)
	listing.Store(key, value)
	runtime.AddCleanup(live, func(key weak.Pointer[crdt.Doc]) { listing.Delete(key) }, key)
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
