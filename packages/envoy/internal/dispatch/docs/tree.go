package docs

import (
	"errors"
	"fmt"
	"log/slog"
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
	markdown, err := pmdoc.Render(tree)
	if err != nil {
		if errors.Is(err, pmdoc.ErrSchema) {
			return "", fmt.Errorf("%w: %v", ErrDocSchema, err)
		}
		return "", err
	}
	return markdown, nil
}

// documentMarkdown renders a tree treeOf read from a document, so a tree only the renderer refuses
// is that document's OutsideSchemaError, as treeOf makes one the reader refuses.
func documentMarkdown(tree *pmdoc.Node) (string, error) {
	markdown, err := pmdoc.Render(tree)
	return markdown, documentSchemaError(err)
}

// renderDocument is what doc renders now, or the error that stopped it being read or rendered.
func renderDocument(doc *crdt.Doc) (string, error) {
	tree, err := treeOf(doc)
	if err != nil {
		return "", err
	}
	return documentMarkdown(tree)
}

// renderDocumentForUpdate is renderDocument without pmdoc.Read's deep copy
// (pmdoc.ReadForRendering): for the update observer's render, which runs the render and discards
// the tree before releasing the replica's lock (renderedReplica.updateChangesMarkdown), so
// nothing retains the tree past this call to alias a later reader.
func renderDocumentForUpdate(doc *crdt.Doc) (string, error) {
	if doc == nil {
		return "", errDocUnloaded
	}
	tree, err := pmdoc.ReadForRendering(doc.GetXmlFragment(fragmentName))
	if err != nil {
		return "", documentSchemaError(err)
	}
	return documentMarkdown(tree)
}

// snapshotDocument copies doc's state as of one moment. Encoding it takes the document's lock, which
// every peer update and service write holds while it applies, so the copy is never a tree half
// way through a write - which a direct walk of a resident room's live tree can read, since the
// walk takes no lock (sjawhar/ygo v1.50.1-sami.2, crdt/yxml.go:301-317) - and nothing writes the
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
func newDocumentCopy(options ...crdt.DocOption) *crdt.Doc {
	return crdt.New(append(options, crdt.WithMaxPendingItems(maxUpdateItems))...)
}

// renderedReplica is the copy of a room's document its update observer renders and the room's
// reads walk (readLive). ygo fires the observer after the transaction has released the document's
// lock (sjawhar/ygo v1.50.1-sami.2, crdt/doc.go:640-645), and a walk of the live tree takes no
// lock (crdt/yxml.go:301-317), so a render or read of the live tree can walk it while another
// transaction writes it: a torn walk reads a healthy document as one outside the schema, logs a
// false WARN and counts the update as a content change. Only a holder of mu writes or walks the
// replica, and it brings the replica up to date under the live document's lock first, so each walk
// is of the room as of one moment.
//
// The update the observer is handed cannot stand in for that: observers of two transactions run
// concurrently and in either order, and each update carries the room's whole delete set, so the
// later update applied first deletes what the earlier one replaced while its own insertions wait
// for the earlier one's - a tree no transaction left. Copying the whole room for every update would
// encode and decode the whole document per keystroke. The room's first update copies it once, so a
// room that is only read holds no replica.
type renderedReplica struct {
	mu sync.Mutex
	// waiting counts the update observers waiting for mu (lockForUpdate).
	waiting atomic.Int32
	doc     *crdt.Doc
	// markdown is the room's rendered markdown when the update observer last saw it change, or at
	// the room's load before any update has; nil while the document is outside the schema.
	markdown *string
}

// lockForUpdate takes mu for the update observer. A read only tries mu, and takes it only while
// no observer waits for it, so the observer waits at most for the one read already walking the
// replica: a read that took mu from under a waiting observer would make it wait for a second walk.
func (r *renderedReplica) lockForUpdate() {
	r.waiting.Add(1)
	r.mu.Lock()
	r.waiting.Add(-1)
}

// observe is the update observer's turn with the replica: it waits for mu (lockForUpdate), brings
// the replica up to date with live, and reports whether the update changed the room's rendered
// markdown (updateChangesMarkdown). The observer is the one holder of mu that waits for it.
func (r *renderedReplica) observe(room string, live *crdt.Doc) bool {
	r.lockForUpdate()
	defer r.mu.Unlock()
	r.catchUp(room, live)
	return r.updateChangesMarkdown(room)
}

// keepReplica makes the replica live's update observer keeps, starting from markdown, live's
// rendering at its load, and lists it for live's reads (readLive) while live is resident. The
// listing holds live and the replica weakly, and the observer, which live holds, holds the
// replica, so the replica, a whole copy of live, is collected with live and in the same cycle; the
// listing goes once live is collected. A reader holding an evicted instance of the room reaches that
// instance's replica or none, never its successor's.
func (s *Service) keepReplica(live *crdt.Doc, markdown *string) *renderedReplica {
	replica := &renderedReplica{markdown: markdown}
	key := weak.Make(live)
	s.replicas.Store(key, weak.Make(replica))
	runtime.AddCleanup(live, func(key weak.Pointer[crdt.Doc]) { s.replicas.Delete(key) }, key)
	return replica
}

// readLive runs read against live, a room's resident document, as of one moment: live's replica
// brought up to date under live's document lock (renderedReplica), else a copy taken under that
// lock (snapshotDocument) - while live has no replica, since the room has had no update since it
// loaded, and whenever another holds the replica's lock or the update observer waits for it. A
// read never waits for that lock: the update observer takes it for each peer update before ygo
// broadcasts the update to the room's other browsers, so a read queued behind it would stall their
// keystrokes for its walk, and every read queued behind that one too. It opens no transaction on
// live, since ygo hands the room's persistence an update for every transaction it commits, even one
// that only reads. The caller must not hold live's document lock - be inside a Yjs transaction on
// it - since the catch-up and the copy encode under that lock, and read must not keep doc past its
// return.
func (s *Service) readLive(room string, live *crdt.Doc, read func(doc *crdt.Doc)) error {
	doc, release, err := s.holdLive(room, live)
	if err != nil {
		return err
	}
	defer release()
	read(doc)
	return nil
}

// holdLive is live as of one moment, as readLive reads it, for the caller to walk until it calls
// release: the replica, holding its lock, or a copy. A caller that must take the room as of one
// moment under a lock of its own takes it here under that lock and walks it once it has released
// that lock (captureLiveTextAndAuthors).
func (s *Service) holdLive(room string, live *crdt.Doc) (doc *crdt.Doc, release func(), err error) {
	if live == nil {
		return nil, nil, errDocUnloaded
	}
	if listed, ok := s.replicas.Load(weak.Make(live)); ok {
		if replica := listed.(weak.Pointer[renderedReplica]).Value(); replica != nil {
			if doc := replica.hold(room, live); doc != nil {
				return doc, replica.mu.Unlock, nil
			}
		}
	}
	copied, err := snapshotDocument(live)
	if err != nil {
		return nil, nil, err
	}
	return copied, func() {}, nil
}

// liveTree is the tree of live, the document resident under room, as of one moment (readLive).
func (s *Service) liveTree(room string, live *crdt.Doc) (*pmdoc.Node, error) {
	var tree *pmdoc.Node
	var treeErr error
	if err := s.readLive(room, live, func(doc *crdt.Doc) { tree, treeErr = treeOf(doc) }); err != nil {
		return nil, err
	}
	return tree, treeErr
}

// hold brings the replica up to date with live and returns it holding mu, which the caller
// releases. It returns nil, holding nothing, for a replica another holds mu on (the update
// observer, or another read) or the observer waits for (lockForUpdate), one the room's first
// update has not made yet, or one whose copy failed.
func (r *renderedReplica) hold(room string, live *crdt.Doc) (doc *crdt.Doc) {
	if r.waiting.Load() > 0 || !r.mu.TryLock() {
		return nil
	}
	defer func() {
		if doc == nil {
			r.mu.Unlock()
		}
	}()
	if r.doc == nil {
		return nil
	}
	r.catchUp(room, live)
	return r.doc
}

// catchUp brings the replica up to date with live: what live gained since the replica's state
// vector, encoded under live's lock, as forkLive brings a transaction's fork up to date. Without a
// replica - the room's first update, or one after an update the replica could not take, which
// leaves it in an unknown state - it copies live whole; a copy that fails leaves no replica, which
// the next update copies again.
func (r *renderedReplica) catchUp(room string, live *crdt.Doc) {
	if r.doc != nil {
		err := crdt.ApplyUpdateV1(r.doc, crdt.EncodeStateAsUpdateV1(live, r.doc.StateVector()), nil)
		if err == nil {
			return
		}
		slog.Error("dispatch: bring the document's rendered copy up to date; copying it again", "room", room, "error", err)
	}
	copied, err := snapshotDocument(live)
	if err != nil {
		slog.Error("dispatch: copy updated document for its update observer", "room", room, "error", err)
	}
	r.doc = copied
}

// updateChangesMarkdown reports whether the room's latest update changed its rendered markdown,
// the only document content a version stores, and keeps the new rendering. It renders the
// replica, the room's document as of that update, so its caller holds mu, through
// renderDocumentForUpdate: the tree it reads never outlives this call, so it carries none of
// pmdoc.Read's copy. An update that changes only what no rendering carries - an anchor mark, or a
// heading id or list item label the browser editor derives - is no content change.
func (r *renderedReplica) updateChangesMarkdown(room string) bool {
	markdown, err := renderDocumentForUpdate(r.doc)
	if err != nil {
		r.markdown = nil
		if errors.Is(err, ErrDocOutsideSchema) {
			slog.Warn("dispatch: updated document outside Proof schema", "room", room, "error", err)
		} else {
			slog.Error("dispatch: read updated document", "room", room, "error", err)
		}
		return true
	}
	if r.markdown != nil && *r.markdown == markdown {
		return false
	}
	r.markdown = &markdown
	return true
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
