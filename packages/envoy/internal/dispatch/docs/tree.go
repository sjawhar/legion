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

// retiredMapName is the root under which a rebuilt history lists the clients and clocks of the
// history it replaced, so a browser still holding that history cannot hand it back (rebuiltSeed,
// weighPeerUpdate). Only a rebuild writes it, and no rendering reads it.
const retiredMapName = "retired"

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
	tree, _, err := parseWrittenReplacing(live, markdown)
	return tree, err
}

// parseWrittenReplacing is parseReplacing with the read-back of the markdown the parse already
// made (pmdoc.ParseWritten), which a write that stores that markdown and reads it again shares
// rather than parsing it a second time (Ledger.ReadBackOf). The tree it returns is the parse's
// as an upload is taken (asUploaded), so the read-back stands for what this write stores only
// where the two render alike, which its caller compares markdown for.
func parseWrittenReplacing(live *pmdoc.Node, markdown string) (*pmdoc.Node, pmdoc.WrittenReadBack, error) {
	parsed, read, err := pmdoc.ParseWritten(markdown, live)
	tree, err := uploadedInput(parsed, err)
	if err != nil {
		return nil, pmdoc.WrittenReadBack{}, err
	}
	return tree, read, nil
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

// treeOfUnshared is treeOf for a document nothing else reads or writes while the call runs - a
// write transaction's fork, read on the request goroutine that holds it (readForkTree, startTree),
// and a room's replica, read by a holder of its mu, the only holders of which read or write it
// (Service.treePoint) - which it reads through pmdoc.ReadUnshared, without ygo's ToDelta, and hands
// back validated once (pmdoc.ValidDocument), so the renders and reads that follow do not validate
// it again.
func treeOfUnshared(doc *crdt.Doc) (pmdoc.ValidDocument, error) {
	if doc == nil {
		return pmdoc.ValidDocument{}, errDocUnloaded
	}
	valid, err := pmdoc.ReadUnshared(doc.GetXmlFragment(fragmentName))
	if err != nil {
		return pmdoc.ValidDocument{}, documentSchemaError(err)
	}
	return valid, nil
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
// (LEGION-479). It returns the tree as the transaction left it; whether the transaction changed the
// document is the update ygo emits for it, or its absence.
func rewriteLive(doc *crdt.Doc, origin any, edit func(live *pmdoc.Node) bool) (*pmdoc.Node, error) {
	fragment := doc.GetXmlFragment(fragmentName)
	var live *pmdoc.Node
	err := doc.TransactE(func(transaction *crdt.Transaction) error {
		var readErr error
		if live, readErr = treeOfTransaction(transaction, fragment); readErr != nil {
			return readErr
		}
		if !edit(live) {
			return nil
		}
		return pmdoc.Update(transaction, fragment, live)
	}, origin)
	if err != nil {
		return nil, err
	}
	return live, nil
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

// documentRenderingValidWithLayout is documentRenderingWithLayout of a tree readForkTree already
// validated, which the render then does not validate again (pmdoc.ValidDocument.RenderWithLayout).
func documentRenderingValidWithLayout(tree pmdoc.ValidDocument) (string, []pmdoc.BlockOffset, *pmdoc.Layout, error) {
	markdown, offsets, layout, err := tree.RenderWithLayout()
	return markdown, offsets, layout, documentSchemaError(err)
}

// documentMarkdownValid is documentMarkdown of a tree readForkTree already validated, which the
// render then does not validate again (pmdoc.ValidDocument.RenderWithBlockOffsets).
func documentMarkdownValid(tree pmdoc.ValidDocument) (string, error) {
	markdown, _, err := documentMarkdownValidWithOffsets(tree)
	return markdown, err
}

// documentMarkdownValidWithOffsets is documentMarkdownWithOffsets of a validated tree.
func documentMarkdownValidWithOffsets(tree pmdoc.ValidDocument) (string, []pmdoc.BlockOffset, error) {
	markdown, offsets, err := tree.RenderWithBlockOffsets()
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

// readFragmentForRendering is the tree fragment holds, read without pmdoc.Read's deep copy
// (pmdoc.ReadForRendering), for the two renders that read a room's live fragment:
// renderDocumentForUpdate and renderFragmentMarkdown. A nil fragment is a document that is not
// loaded, and a tree the reader refuses is that document's schema error (documentSchemaError).
func readFragmentForRendering(fragment *crdt.YXmlFragment) (*pmdoc.Node, error) {
	if fragment == nil {
		return nil, errDocUnloaded
	}
	tree, err := pmdoc.ReadForRendering(fragment)
	if err != nil {
		return nil, documentSchemaError(err)
	}
	return tree, nil
}

// renderDocumentForUpdate is renderDocument of the document whose fragment is fragment, read
// without pmdoc.Read's deep copy (readFragmentForRendering), recording the rendering's layout from
// the same render: for the update observer's render, which runs the render and discards the tree
// before releasing the replica's lock (renderedReplica.updateChangesMarkdown), so nothing retains
// the tree past this call to alias a later reader - a caller that reads tree past the call (the
// ask blocks updateChangesMarkdown hands classify, walked before the lock is released) aliases
// none of it by doing so, since tree itself, not a copy of it, is what the caller walks.
func renderDocumentForUpdate(fragment *crdt.YXmlFragment) (string, *pmdoc.Node, *pmdoc.Layout, error) {
	tree, err := readFragmentForRendering(fragment)
	if err != nil {
		return "", nil, nil, err
	}
	markdown, _, layout, err := documentRenderingWithLayout(tree)
	return markdown, tree, layout, err
}

// renderFragmentMarkdown is renderDocumentForUpdate's read and render without its layout and
// without its tree: what fragment renders now, for the measures that compare one rendering with
// another and keep neither's blocks - a live write's shadow (roomShadow.renderLocked) and the
// fold of a document's stored updates (renderStoredFold) - which pmdoc.RenderWithLayout would
// hand a pmdoc.Layout each has nothing to do with.
func renderFragmentMarkdown(fragment *crdt.YXmlFragment) (string, error) {
	tree, err := readFragmentForRendering(fragment)
	if err != nil {
		return "", err
	}
	return documentMarkdown(tree)
}

// snapshotDocument copies doc's state as of one moment. Encoding it takes the document's lock, which
// every peer update and service write holds while it applies, so the copy is never a tree half
// way through a write - which a direct walk of a resident room's live tree can read, since the
// walk takes no lock (sjawhar/ygo v1.55.1-sami.1, crdt/yxml.go:351-366) - and nothing writes the
// copy.
func snapshotDocument(doc *crdt.Doc) (*crdt.Doc, error) {
	snapshot := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(snapshot, crdt.EncodeStateAsUpdateV1(doc, nil), nil); err != nil {
		return nil, fmt.Errorf("copy live document: %w", err)
	}
	return snapshot, nil
}

// newDocumentCopy is newCappedDocumentCopy at maxUpdateItems, for a decode that runs for no
// instance with a cap of its own below it: a snapshot or a write's fork of a room this server
// holds, a room load's copy of its stored history, which becomes the room's replica
// (decodeStoredCopy), or one update the store decodes alone - a browser's, which no document here
// encoded (appendUpdate), and one whose header names no single client's item count
// (refuseUpdateItemCap).
func newDocumentCopy(options ...crdt.DocOption) *crdt.Doc {
	return newCappedDocumentCopy(maxUpdateItems, options...)
}

// newCappedDocumentCopy builds a fresh document for replay at the instance's pending-queue cap:
// a copy of its room decodes what that instance decodes and refuses what it would refuse. The
// cap follows the instance (newRoomServer, loadTree, replica); other callers use newDocumentCopy.
// See maxUpdateItems for the production and protocol caps.
//
// Every copy replays without ygo's format cleanup (crdt.WithFormatCleanup), which follows each
// remote apply that changes formatted text with a commit of its own: every update a copy applies
// is one the room committed, and every cleanup the room made is already among them (the room's own
// repairs and sweeps are local commits, which never clean up; a write's publish and a browser's
// update reach the room as remote applies, and the cleanup either makes is stored), so a copy that
// cleaned up would hold a state the room never held - and a compaction that folded such a copy
// would store it. A write's fork commits its own writes locally, which never clean up. The room's
// load decodes its stored state the same way (bootstrapLoadedState), so the room and the copy its
// load decoded hold the same state.
func newCappedDocumentCopy(maxPending int, options ...crdt.DocOption) *crdt.Doc {
	return crdt.New(documentCopyOptions(maxPending, options...)...)
}

// documentCopyOptions is what newCappedDocumentCopy builds a copy with, for a caller that needs
// the options rather than a document built from them: a write's fork, which is built from them
// both by decoding a state into a new document (buildFork) and by cloning the room's replica
// (Service.forkPoint), and the two must be the same document (forkOptions).
func documentCopyOptions(maxPending int, options ...crdt.DocOption) []crdt.DocOption {
	return append(options, crdt.WithMaxPendingItems(maxPending), crdt.WithFormatCleanup(false))
}

// roomWeighing is what the peer update hook reads of a resident room to weigh a browser's update
// (weighPeerUpdate): the room's charge in the room budget, the clients its history retires, the
// allowance of each apply in flight, its scan total, the bytes of the rendering its replica last
// published and the bytes of state it is tracked at. The room's document lists it strongly
// (roomListing, listResident), so the hook finds it with one map load, and it holds no reference
// to that document, so listing it strongly keeps no evicted room's document alive.
//
// It is a record of its own, and not the replica's own fields, because a keystroke must make no
// weak pointer strong. The collector blocks every weak-to-strong conversion from the moment mark
// termination begins (runtime/mgc.go:1020, runtime/mheap.go:2406-2420), and a termination that
// finds more marking to do returns with that block still up, so the conversions it parked wait out
// the rest of the mark phase: two to four seconds on this pod's two processors at the 3.4 GB heap
// goal a full room budget leaves. A keystroke that read the replica through the listing's weak
// pointer waited exactly that: 2.96 s and 3.23 s in one run of cmd/dispatch's
// TestTheHeaviestApplyBesideAFullCopyBoundStaysUnderThePodsMemory, where the same step's other
// keystrokes were answered in 14 to 55 ms.
type roomWeighing struct {
	// retired is the clients the room's history lists as retired by the rebuild that seeded it,
	// each with the clock it had reached (retiredClientsOf), read once at the room's load: only
	// a rebuild writes them, a rebuild replaces a history no room holds (RebuildDocument), and
	// the roots a browser's update may name refuse any change to them (peerAllowedRoots). Nil
	// for a room never rebuilt.
	retired map[crdt.ClientID]uint64
	// budgets is the allowance of each of the hook's applies in flight and the roots they may
	// name (roomApplies); scans is the room's scan total and the budget of each remote apply's
	// scans (roomScans). Both are made by the room's load (keepReplica).
	budgets *roomApplies
	scans   *roomScans
	// bytes is the size of the replica's latest published rendering, published beside it, for
	// the peer update hook to load (Service.weighPeerUpdate): the byte bound's measure of the
	// document. The hook runs on the peer's read loop, where taking the replica's mu would put
	// every browser in the room behind its load (AGENTS.md:559-562), so the measurement travels
	// as an atomic rather than under the lock the rendering itself is kept under. It is
	// sizeUnmeasured while the room has no rendering to measure, which is a document outside the
	// schema.
	bytes atomic.Int64
	// state is the bytes of encoded state the room is tracked at: what the stored state its load
	// decoded came to, plus the bytes of each update its observer has drained since, re-based by
	// a compaction that folds the room's whole stored history into one row (roomWeight,
	// renderedReplica.chargeAdded, Service.rebaseRoomState). The budget charges the room by it,
	// and the hook bounds what each apply may add by what is left of maxRoomStateBytes above it,
	// both without ever encoding the document.
	state atomic.Int64
	// charge is the room's charge in the room budget, which the room's load stores once it has
	// made it (chargeLoadedRoom) and nothing replaces; nil until then, and for a room no load
	// charged, a test's.
	charge atomic.Pointer[roomCharge]
}

// roomCharge is the room's charge in the room budget, nil for a document no room's load listed and
// for a room no load charged.
func (w *roomWeighing) roomCharge() *roomCharge {
	if w == nil {
		return nil
	}
	return w.charge.Load()
}

// retiredClients is the clients the room's history lists as retired, nil for a room never
// rebuilt and for a document no room's load listed.
func (w *roomWeighing) retiredClients() map[crdt.ClientID]uint64 {
	if w == nil {
		return nil
	}
	return w.retired
}

// applies is the allowance of each of the hook's applies in flight in the room, nil for a
// document no room's load listed, which bounds nothing and runs each apply as it is.
func (w *roomWeighing) applies() *roomApplies {
	if w == nil {
		return nil
	}
	return w.budgets
}

// roomScans is the room's scan total, nil for a document no room's load listed.
func (w *roomWeighing) roomScans() *roomScans {
	if w == nil {
		return nil
	}
	return w.scans
}

// roomStateBytes is the bytes of encoded state the room is tracked at (state), and zero for a
// document no room's load listed, which is tracked at nothing: the hook then bounds its applies
// by the whole of maxRoomStateBytes, as it weighs such a room against the byte bound on the
// update's own bytes alone.
func (w *roomWeighing) roomStateBytes() int64 {
	if w == nil {
		return 0
	}
	return w.state.Load()
}

// size is the room's last published measurement - the bytes of its rendering - and whether it has
// one; no measurement at all for a document no room's load listed. It takes no lock, so a caller
// on the peer's read loop holds up nobody for it.
//
// It is never stale: the update observer publishes it with the rendering itself, so it is as
// recent as the room's last update observer. It does not make the document bound a ceiling
// (weighPeerUpdate).
func (w *roomWeighing) size() (bytes int64, measured bool) {
	if w == nil {
		return 0, false
	}
	bytes = w.bytes.Load()
	return bytes, bytes != sizeUnmeasured
}

// roomListing is what a room's document is listed under while it is resident (listResident,
// keepReplica): the weighing the peer update hook reads, held strongly, and the replica the room's
// reads walk (readLive), held weakly, so that the replica - a whole copy of the document - is
// collected with the document and in the same cycle. A read does make that weak pointer strong
// (replicaOf), and waits where the collector blocks the conversion; it then holds the replica's
// lock and walks the whole document, which is not a keystroke.
type roomListing struct {
	weighing *roomWeighing
	replica  weak.Pointer[renderedReplica]
}

// sizeUnmeasured is what bytes holds for a room whose document renders no markdown to measure: a
// document outside the Proof schema, which the socket path refuses to open in the first place
// (refuseOutsideSchema) and which a live document can still fall into between two renders. The
// hook admits an update on such a room; the room's next render settles it.
const sizeUnmeasured = -1

// emptyUpdateV1 is the V1 encoding of no structs and no delete ranges. ygo logs it beside Parked
// for a commit that kept only parked bytes, and calls no OnUpdate for that commit
// (crdt.LoggedUpdate).
var emptyUpdateV1 = crdt.EncodeStateAsUpdateV1(crdt.New(), nil)

// forkTreeCounts counts the trees read off a write transaction's fork, and the reads that taking
// the write's rendering in place of one avoided, so a test can assert that a write reads its fork
// no more often than it means to, and a measurement build can report how often the read is
// avoided. The service keeps one (Service.forkTrees). Nothing in the service reads them.
type forkTreeCounts struct {
	// read counts the trees read off a write transaction's fork (readForkTree): an operation's
	// reads before and after its write (applyLive), a margin write's read when the write holds no
	// rendering of its fork (applyMargin), and the reads joined to the transaction (docTree,
	// heldTree, captureWrite).
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
