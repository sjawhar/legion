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
// nothing retains the tree past this call to alias a later reader - a caller that reads tree past
// the call (updateChangesMarkdown's own onChanged, called before the lock is released) aliases
// none of it by doing so, since tree itself, not a copy of it, is what the caller walks.
func renderDocumentForUpdate(doc *crdt.Doc) (string, *pmdoc.Node, error) {
	if doc == nil {
		return "", nil, errDocUnloaded
	}
	tree, err := pmdoc.ReadForRendering(doc.GetXmlFragment(fragmentName))
	if err != nil {
		return "", nil, documentSchemaError(err)
	}
	markdown, err := documentMarkdown(tree)
	return markdown, tree, err
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
// the room committed, and every cleanup the room made is already among them (the room's own
// repairs and sweeps are local commits, which never clean up; a write's publish and a browser's
// update reach the room as remote applies, and the cleanup either makes is stored), so a copy
// that cleaned up would hold a state the room never held. A write's fork commits its own writes
// locally, which never clean up.
func newDocumentCopy(options ...crdt.DocOption) *crdt.Doc {
	return crdt.New(append(options, crdt.WithMaxPendingItems(maxUpdateItems), crdt.WithFormatCleanup(false))...)
}

// renderedReplica is the copy of a room's document its update observer renders and the room's
// reads walk (readLive). ygo fires the observer after the transaction has released the document's
// lock (sjawhar/ygo v1.51.3-sami.4, crdt/doc.go, transactInternal), and a walk of the live tree
// takes no lock (crdt/yxml.go), so a render or read of the live tree can walk it while another
// transaction writes it: a torn walk reads a healthy document as one outside the schema, logs a
// false WARN and counts the update as a content change. Only a holder of mu writes or walks the
// replica.
//
// The replica moves only through the room's commits, one at a time and in the order they committed
// (crdt.UpdateLog). ygo appends each committed transaction's update to the room's log under the
// document's lock, before any observer of that transaction runs, and the update observer that holds
// mu drains the log and classifies each update it applies against the replica as that update left
// it. Observers of two transactions run concurrently and in either order, so whichever runs first
// classifies both, each against the room as the commit before it left it. Catching the replica up
// to the live document instead took in every commit since the last one rendered: a published
// write's observer that ran first counted a browser's edit in its own change, which credits no
// one, and the browser's own observer then found nothing changed and credited no one either. The
// update an observer is handed cannot stand in for the log: each update carries the room's whole
// delete set, so the later update applied first deletes what the earlier one replaced while its
// own insertions wait for the earlier one's - a tree no transaction left.
//
// The observer renders the replica after an update only when it can neither know the update is a
// margin write's publish into the room its fork was warm-built from (liveWriteOrigin.marginOnly
// says why that changes no text), nor reuse the rendering the published live write that made the
// update made of it, nor know the update was already in the room (changedBy). All three are exact.
// The write's rendering (publishedRendering) is taken only on the replica, epoch and foreign
// count the operation's own fork read found, the replica's before that read's encode: a replica
// still at both has consumed, through that read, every commit the fork was read from, and since
// then only published live writes' commits, which under the writer slot are this write's earlier
// updates, which the fork holds too - so the replica after the update is the fork after the
// operation, and rendering is deterministic over one state. ygo puts the
// room's whole delete set in every update, so an update means that only on the room instance the
// fork was read from: a cold fork, read from the store, records no rendering, and a room reloaded
// since the read has a replica of its own at another epoch. An update already in the room
// (liveWriteOrigin.inRoom) has a version at or below the head its room instance's load folded
// (roomInstance.loadedHead), so the loaded document held it, and the replica, which starts from the
// room as of its first logged commit, holds the load: applying it changed nothing. Nothing makes
// that history other than what the load folded: RebuildDocument, the one writer that replaces a
// document's history, runs only on a history that cannot load (rebuild.go), and the write's own
// refuseUndecodable proved the history through its version decodes; PruneAfter has no production
// caller. A replica outside the schema (a nil markdown) renders the update, so such an update
// still logs and counts as changed.
//
// The replica starts from the state the log records as of just before the room's first transaction
// since its load, so a room that is only read holds no replica.
type renderedReplica struct {
	mu sync.Mutex
	// waiting counts the update observers waiting for mu (lockForUpdate).
	waiting atomic.Int32
	log     *crdt.UpdateLog
	doc     *crdt.Doc
	// markdown is the room's rendered markdown as the replica's latest update left it, or as the
	// log's base rendered before any; nil while the document is outside the schema.
	markdown *string
	// epoch names the replica's run from one state it started from: keepReplica draws it from
	// replicaEpochs when it makes the replica, and recover draws a fresh one whenever the replica
	// starts again, so it is unique in the process. No replica of another room instance - the one
	// a reload leaves behind has a replica of its own - and no run of this replica before a
	// recover ever stands at the epoch this one does. It is written under mu and read without it.
	epoch atomic.Uint64
	// foreign counts the logged updates the replica has consumed - applied, or handed to recover -
	// whose origin is not a published live write's (*liveWriteOrigin). observe adds to it while it
	// holds mu, update by update in commit order (consumed), and anyone may read it without mu. A
	// reader that reads epoch and foreign before it reads the room's state at some moment, and
	// later finds both unchanged on a replica that holds every logged commit (hold), knows that
	// every commit the replica consumed since that moment was a published live write's: under the
	// writer slot, only the reader's own write's earlier updates. A race goes the safe way: a
	// foreign commit the replica consumes after the read changes the count.
	foreign atomic.Uint64
	// paths is the service's count of the way each rendering was had (renderPathCounts), which
	// keepReplica hands over: the replica counts the paths its update observer takes.
	paths *renderPathCounts
}

// replicaEpochs is where every renderedReplica.epoch is drawn from, so no two replicas in the
// process, and no two runs of one replica either side of a recover, ever stand at one epoch.
var replicaEpochs atomic.Uint64

// renderPathCounts counts, one counter per path, how the renderings a room's update observer and a
// document write need were had, so a test can assert that the path it means to exercise ran rather
// than pass by rendering the replica, and a measurement build can report how often each one fires.
// The service keeps one (Service.renderPaths) and hands each replica a pointer to it (keepReplica).
// Nothing in the service reads them.
type renderPathCounts struct {
	// observerReused counts the logged updates the update observer classified with the rendering
	// a published live write made of the very state the update left, in place of one of its own.
	observerReused atomic.Uint64
	// observerInRoom counts the logged updates the update observer classified as unchanged without
	// rendering, since the room's load already held them (roomInstance.loadedHead).
	observerInRoom atomic.Uint64
	// observerRendered counts the logged updates the update observer applied and then rendered the
	// replica for (updateChangesMarkdown).
	observerRendered atomic.Uint64
	// observerMarginOnly counts the logged updates the update observer classified as unchanged
	// without rendering, since they were margin writes' publishes into the room instance their
	// fork was warm-built from (liveWriteOrigin.marginOnly).
	observerMarginOnly atomic.Uint64
	// beforeFromReplica counts the operations whose rendering of their fork before they applied was
	// the room replica's, which held the very state the fork was read from.
	beforeFromReplica atomic.Uint64
	// beforeFromWrite counts the operations whose rendering of the fork before them was the one the
	// write's previous operation made of the fork it left.
	beforeFromWrite atomic.Uint64
	// beforeRendered counts the operations that rendered their fork before applying.
	beforeRendered atomic.Uint64
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

// observedAsks is the ask blocks a logged update left in the room's document, as the update
// observer hands them to classify: walked from tree, the replica's rendering after the update,
// only on the first call to askBlocks, so a classify that never reads them - an identity repair's,
// which returns before it does - walks nothing; or, for an update a published live write's own
// rendering classified (changedBy), the ids that write walked from its tree of the same state
// (publishedRendering.askBlocks), with no tree. A classify handed nil has no ask blocks to record:
// the update changed no rendered markdown, was handed to recover, or left a document that does
// not render.
type observedAsks struct {
	// tree is the rendering the ids are walked from, valid only during classify's call.
	tree *pmdoc.Node
	// ids are the ask block ids, nil until askBlocks walks tree: askBlockIDs never returns nil, so
	// nil means not known yet.
	ids map[string]struct{}
}

// askBlocks is the id of every ask block the update left, walked from tree on the first call.
func (a *observedAsks) askBlocks() map[string]struct{} {
	if a.ids == nil {
		a.ids = askBlockIDs(a.tree)
	}
	return a.ids
}

// lockForUpdate takes mu for the update observer. A read only tries mu, and takes it only while
// no observer waits for it, so the observer waits at most for the one read already walking the
// replica: a read that took mu from under a waiting observer would make it wait for a second walk.
func (r *renderedReplica) lockForUpdate() {
	r.waiting.Add(1)
	r.mu.Lock()
	r.waiting.Add(-1)
}

// observe is the update observer's turn with the replica: it waits for mu (lockForUpdate), drains
// the room's update log, and moves the replica through each update the log hands over, in commit
// order, classifying each as it takes it (changedBy). It calls classify, when given, with each
// update, whether that update changed the room's rendered markdown, and the ask blocks it left
// (observedAsks) when it did - nil otherwise, and valid only during the call - still holding mu,
// so the room's own record of what each update introduced moves forward in commit order. The
// observer is the one holder of mu that waits for it, and the one that drains the log: every
// committed update is classified exactly once, by whichever observer reaches the log first.
func (r *renderedReplica) observe(room string, classify func(update crdt.LoggedUpdate, changed bool, asks *observedAsks)) {
	r.lockForUpdate()
	defer r.mu.Unlock()
	base, updates := r.log.Drain()
	if base != nil {
		r.start(room, base)
	}
	for i, update := range updates {
		if r.doc == nil {
			r.recover(room, updates[i:], classify)
			return
		}
		if err := crdt.ApplyUpdateV1(r.doc, update.Update, nil); err != nil {
			slog.Error("dispatch: apply a committed update to the document's rendered copy", "room", room, "error", err)
			r.recover(room, updates[i:], classify)
			return
		}
		published, rendering := r.consumed(update)
		changed, asks := r.changedBy(room, published, rendering)
		if classify != nil {
			classify(update, changed, asks)
		}
	}
}

// changedBy classifies the update the replica has just applied and counted (consumed), published
// when a published live write made it, nil otherwise, and rendering that write's operation's
// rendering of it: whether it changed the room's rendered markdown, and the ask blocks it left
// when it did. It renders the replica (updateChangesMarkdown) only when nothing else tells. A
// published live write's update can tell in three ways, tried in this order:
//
//   - it is a margin write's publish into the room instance its fork was warm-built from
//     (liveWriteOrigin.marginOnly, which says why that changes no text), so it changed nothing,
//     while the replica renders at all: one outside the schema renders the update, since the
//     render is what says what the document is now;
//   - its operation's rendering (publishedRendering), when that rendering was made on this
//     replica's epoch and foreign count and the replica still stands at both (standsAt): the
//     replica is then the fork the operation rendered, so the rendering is the replica's, and the
//     update changed the markdown exactly when it differs from the replica's before it. A change
//     the operation did not see as one carries no ask blocks, so it is rendered after all;
//   - its room instance's load already held it (liveWriteOrigin.inRoom), so applying it changed
//     nothing, while the replica renders at all: one outside the schema renders the update, so it
//     still logs and counts as changed.
//
// renderedReplica says why the last two are exact. The caller holds mu.
func (r *renderedReplica) changedBy(room string, published *liveWriteOrigin, rendering publishedRendering) (bool, *observedAsks) {
	if published != nil && published.marginOnly && r.markdown != nil {
		r.paths.observerMarginOnly.Add(1)
		return false, nil
	}
	if published != nil {
		if r.standsAt(rendering.forkReadMark) {
			if r.markdown != nil && *r.markdown == rendering.markdown {
				r.paths.observerReused.Add(1)
				return false, nil
			}
			if rendering.askBlocks != nil {
				// Its own string, not a pointer into rendering, which would keep the ask
				// block ids alive with the replica's markdown.
				markdown := rendering.markdown
				r.markdown = &markdown
				r.paths.observerReused.Add(1)
				return true, &observedAsks{ids: rendering.askBlocks}
			}
		} else if published.inRoom && r.markdown != nil {
			r.paths.observerInRoom.Add(1)
			return false, nil
		}
	}
	changed, tree := r.updateChangesMarkdown(room)
	r.paths.observerRendered.Add(1)
	var asks *observedAsks
	if tree != nil {
		asks = &observedAsks{tree: tree}
	}
	return changed, asks
}

// consumed counts update, which the replica has just applied or handed to recover, in foreign
// unless a published live write committed it. For such a write's update it returns the origin and
// takes the operation's rendering out of it (liveWriteOrigin.rendering), so no markdown outlives
// the update's classification; any other update gets nil and no rendering. The caller holds mu.
func (r *renderedReplica) consumed(update crdt.LoggedUpdate) (*liveWriteOrigin, publishedRendering) {
	published, _ := update.Origin.(*liveWriteOrigin)
	if published == nil {
		r.foreign.Add(1)
		return nil, publishedRendering{}
	}
	rendering := published.rendering
	published.rendering = publishedRendering{}
	return published, rendering
}

// mark is the replica's epoch and foreign count as of now, read without mu, epoch first: what a
// fork read records just before it encodes the room (warmFork).
func (r *renderedReplica) mark() forkReadMark {
	return forkReadMark{epoch: r.epoch.Load(), foreign: r.foreign.Load(), warm: true}
}

// standsAt reports whether mark is a warm fork read's and the replica still stands at its epoch
// and foreign count: since that read it has consumed only published live writes' commits.
func (r *renderedReplica) standsAt(mark forkReadMark) bool {
	return mark.warm && r.epoch.Load() == mark.epoch && r.foreign.Load() == mark.foreign
}

// start makes the replica from base, the room's state as of just before the first update its log
// hands over, and takes its rendering as the one that update is measured against. A base that does
// not apply leaves no replica (recover).
func (r *renderedReplica) start(room string, base []byte) {
	doc := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(doc, base, nil); err != nil {
		slog.Error("dispatch: make the document's rendered copy", "room", room, "error", err)
		r.doc, r.markdown = nil, nil
		return
	}
	r.doc = doc
	if markdown, _, err := renderDocumentForUpdate(doc); err == nil {
		r.markdown = &markdown
	} else {
		r.markdown = nil
	}
}

// recover answers a replica that could not take an update, which leaves it holding no state the
// room had: the log hands over every update it holds and records a fresh base at the room's next
// commit (crdt.UpdateLog.Reset), so the replica starts again exactly there, at a fresh epoch. Each
// update it could not place - the rest of this drain and what the reset handed over - is consumed
// all the same (consumed), and classified as a content change with no ask blocks, since nothing
// tells what it changed: crediting a connected browser with an edit that changed nothing loses
// less than an edit credited to nobody.
func (r *renderedReplica) recover(room string, unplaced []crdt.LoggedUpdate, classify func(update crdt.LoggedUpdate, changed bool, asks *observedAsks)) {
	unplaced = append(unplaced, r.log.Reset()...)
	r.doc, r.markdown = nil, nil
	r.epoch.Store(replicaEpochs.Add(1))
	for _, update := range unplaced {
		r.consumed(update)
	}
	slog.Error("dispatch: the document's rendered copy starts again at the room's next commit; updates it could not place count as content changes",
		"room", room, "updates", len(unplaced))
	if classify == nil {
		return
	}
	for _, update := range unplaced {
		classify(update, true, nil)
	}
}

// keepReplica makes the replica live's update observer keeps, moving through log, live's update
// log, and lists it for live's reads (readLive) while live is resident (listResident). The listing
// holds the replica weakly too, and the observer, which live holds, holds the replica, so the
// replica, a whole copy of live, is collected with live and in the same cycle. A reader holding an
// evicted instance of the room reaches that instance's replica or none, never its successor's. The
// replica takes a fresh epoch (replicaEpochs) and counts its paths on the service's renderPaths.
func (s *Service) keepReplica(live *crdt.Doc, log *crdt.UpdateLog) *renderedReplica {
	replica := &renderedReplica{log: log, paths: &s.renderPaths}
	replica.epoch.Store(replicaEpochs.Add(1))
	listResident(&s.replicas, live, weak.Make(replica))
	return replica
}

// listResident lists value under live, a room's resident document, in listing until live is
// collected: the key holds live weakly, so the listing never keeps an evicted room's document
// alive, and the entry goes once that document is collected.
func listResident(listing *sync.Map, live *crdt.Doc, value any) {
	key := weak.Make(live)
	listing.Store(key, value)
	runtime.AddCleanup(live, func(key weak.Pointer[crdt.Doc]) { listing.Delete(key) }, key)
}

// readLive runs read against live, a room's resident document, as of one moment: live's replica
// when it holds every update the room has committed (renderedReplica.hold), else a copy taken under
// live's document lock (snapshotDocument) - while live has no replica, since the room has had no
// transaction since it loaded, whenever another holds the replica's lock or the update observer
// waits for it, and in the moment between a commit and the observer that classifies it. A read
// never waits for that lock: the update observer takes it for each peer update before ygo
// broadcasts the update to the room's other browsers, so a read queued behind it would stall their
// keystrokes for its walk, and every read queued behind that one too. It opens no transaction on
// live, since ygo hands the room's persistence an update for every transaction it commits, even one
// that only reads. The caller must not hold live's document lock - be inside a Yjs transaction on
// it - since the copy encodes under that lock, and read must not keep doc past its return.
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
	if replica := s.replicaOf(live); replica != nil {
		if doc := replica.hold(); doc != nil {
			return doc, replica.mu.Unlock, nil
		}
	}
	copied, err := snapshotDocument(live)
	if err != nil {
		return nil, nil, err
	}
	return copied, func() {}, nil
}

// replicaOf is the replica live's update observer keeps (keepReplica, from the room's load), or nil
// for a document no room loaded or whose replica has been collected. live is a room's resident
// document, the key the replica is listed under.
func (s *Service) replicaOf(live *crdt.Doc) *renderedReplica {
	if listed, ok := s.replicas.Load(weak.Make(live)); ok {
		return listed.(weak.Pointer[renderedReplica]).Value()
	}
	return nil
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

// hold returns the replica holding mu, which the caller releases, when it holds every update the
// room has committed - its log holds none the observer has yet to classify - so it is the room as
// of the last commit. It returns nil, holding nothing, for a replica another holds mu on (the
// update observer, or another read) or the observer waits for (lockForUpdate), one the room's
// first transaction has not made yet, and one behind the room's latest commit: a read never moves
// the replica, since an update applied by a read would be rendered by no observer and credited to
// nobody, which is the loss the log exists to prevent (renderedReplica).
func (r *renderedReplica) hold() *crdt.Doc {
	if r.waiting.Load() > 0 || !r.mu.TryLock() {
		return nil
	}
	if r.doc == nil || r.log.Pending() > 0 {
		r.mu.Unlock()
		return nil
	}
	return r.doc
}

// updateChangesMarkdown reports whether the update the replica took last changed the room's
// rendered markdown, the only document content a version stores, and keeps the new rendering,
// with the tree it rendered when it changed. It renders the replica, the room's document as of
// that update, so its caller holds mu, through renderDocumentForUpdate: the tree it reads never
// outlives its caller's turn with the replica, so it carries none of pmdoc.Read's copy. An update
// that changes only what no rendering carries - an anchor mark, or a heading id or list item label
// the browser editor derives - is no content change. A document that does not render counts as
// changed, with no tree. It is the observer's measure of every update, which changedBy reaches
// without rendering only where it already knows the answer this would give: a published write's
// own rendering of the same state, or an update the room's load already held.
func (r *renderedReplica) updateChangesMarkdown(room string) (bool, *pmdoc.Node) {
	markdown, tree, err := renderDocumentForUpdate(r.doc)
	if err != nil {
		r.markdown = nil
		if errors.Is(err, ErrDocOutsideSchema) {
			slog.Warn("dispatch: updated document outside Proof schema", "room", room, "error", err)
		} else {
			slog.Error("dispatch: read updated document", "room", room, "error", err)
		}
		return true, nil
	}
	if r.markdown != nil && *r.markdown == markdown {
		return false, nil
	}
	r.markdown = &markdown
	return true, tree
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
