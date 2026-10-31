package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"runtime/debug"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"weak"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

type versionWrite struct {
	named            bool
	summary          *string
	authors          []model.Actor
	capture          *authorCapture
	docUpdateVersion *int64
}

// mergeInto copies src into *dst, allocating *dst if it is nil and src holds anything; a nil or
// empty src leaves *dst exactly as it was, nil included.
func mergeInto[K comparable, V any](dst *map[K]V, src map[K]V) {
	if len(src) == 0 {
		return
	}
	if *dst == nil {
		*dst = make(map[K]V, len(src))
	}
	maps.Copy(*dst, src)
}

// recoverMutation, deferred around a document mutation, turns its panic into *err and logs it, and
// reports through *panicked that the mutation panicked.
func recoverMutation(room string, err *error, panicked *bool) {
	if recovered := recover(); recovered != nil {
		*panicked = true
		*err = fmt.Errorf("document mutation panicked: %v\n%s", recovered, debug.Stack())
		slog.Error("dispatch: document mutation panicked", "room", room, "error", *err)
	}
}

// liveOperation is one operation of a joined transaction's write to a document: the transaction it
// appends in, the write it belongs to (liveWrite), the fork of the room it changes, the updates
// that change made, and whether it recorded them. applyLive and applyMargin each open one
// (openLiveOperation), change its fork (change) and record what it leaves (recordLiveOperation),
// so the refusals both meet run in one order, and the append and the record exist once.
type liveOperation struct {
	tx    pgx.Tx
	write *liveWrite
	fork  *crdt.Doc
	// read is the room replica's epoch and foreign count as this operation's own fork read found
	// them (liveWrite.forkRead), copied when the fork is read: a later fork read in the same
	// operation (a joined read's) must not pair this operation's rendering with another read's
	// counters (publishedRendering).
	read     forkReadMark
	updates  [][]byte
	recorded bool
	// panicked is whether the operation's change panicked (change), which abandon releases the
	// fork for whatever updates holds.
	panicked bool
}

// openLiveOperation opens an operation on artifactID's document inside the transaction the
// context's ledger joined. It refuses a document the service is shutting down
// (ErrServiceUnavailable) and a context Join did not return (errUnjoined), then takes the
// transaction's write to the document (joinLiveWrite) and brings that write's fork up to date with
// the room (forkLive). The caller defers the operation's abandon once it is open.
func (s *Service) openLiveOperation(ctx context.Context, artifactID string) (liveOperation, error) {
	if s.shuttingDown(artifactID) {
		return liveOperation{}, ErrServiceUnavailable
	}
	tx, joined := txFromContext(ctx)
	if !joined {
		return liveOperation{}, errUnjoined
	}
	ledger := ledgerFrom(ctx)
	write, err := s.joinLiveWrite(ctx, ledger, artifactID)
	if err != nil {
		return liveOperation{}, err
	}
	fork, err := s.forkLive(ctx, write)
	if err != nil {
		return liveOperation{}, err
	}
	return liveOperation{tx: tx, write: write, fork: fork, read: write.forkRead}, nil
}

// abandon, deferred once the operation is open, undoes what an operation that changed the fork but
// does not reach its record leaves on its write: the fork holds changes its transaction never
// recorded, so it goes, and the next operation rebuilds it. One that changed nothing leaves the
// fork as it was, so the next operation keeps it. One whose change panicked goes whatever updates
// holds: ygo emits a panicking transaction's update, except when its phase 2 fails before handing
// the update on (buildPhase2, or a type observer it fires first), which leaves the fork holding a
// change no update carries, so neither the fork nor the rendering a joined read takes of it
// (liveWrite.rendering) describes what the transaction records.
func (op *liveOperation) abandon() {
	if !op.panicked && (op.recorded || len(op.updates) == 0) {
		return
	}
	// The rendering describes that fork, so it goes with it (releaseFork). forkLive's rebuild
	// drops it too; abandoning both in one call is what covers an operation that recorded the
	// rendering and then failed to version.
	op.write.releaseFork()
	// A stamp this failed operation ran (stampAskBlockIDs, registerStampedAskBlocks) staged its
	// renamed ids and any carried-forward authors on these before refusal - a later operation of
	// the same transaction must not inherit them as its own.
	op.write.addedAskBlockIDs, op.write.renamedAskBlockIDs, op.write.carriedAskAuthors = nil, nil, nil
}

// change runs mutate, which writes the operation's fork, and captures every update it makes, for
// the record to append, and the margin records it changes (watchMargin), for the growth weigh. A
// panic in mutate is its error (recoverMutation), and marks the operation panicked, so abandon
// drops the fork.
func (op *liveOperation) change(mutate func() error) (*marginWatch, error) {
	unsubscribe := op.fork.OnUpdate(func(update []byte, _ any) {
		op.updates = append(op.updates, append([]byte(nil), update...))
	})
	margin, stopMargin := watchMargin(op.fork)
	err := func() (err error) {
		defer recoverMutation(op.write.artifactID, &err, &op.panicked)
		return mutate()
	}()
	stopMargin()
	unsubscribe()
	return margin, err
}

// recordLiveOperation records op, a change its caller has weighed, inside its transaction:
// rendered is the fork's tree as the change left it with that tree's rendering, and contentChanged
// whether the change moved the rendered markdown, the class its update is appended with. It
// refuses a live state ygo could not decode (refuseUndecodable), appends the update, refuses a
// write whose room failed before that append, keeps the rendering on the transaction's write for a
// version it writes later, and refreshes the document's anchors against the tree in the same
// transaction (writeVersionTx). Only once all of that is done is op recorded, so a refusal anywhere
// in it leaves abandon to drop the fork and the rendering. rendering and marginRoom are recorded
// beside the update, index for index (liveWrite.renderings, liveWrite.marginRooms), so none of the
// three can fall out of step with the others: rendering is what the room's update observer may take
// in place of rendering the room after the update (publishedRendering), the zero value for a fork
// read cold; marginRoom is the room a margin write's fork was warm-built from, which its publish
// tags the update against, and the zero pointer for every other write.
func (s *Service) recordLiveOperation(ctx context.Context, op *liveOperation, actor model.Actor, rendered renderedTree, contentChanged bool, rendering publishedRendering, marginRoom weak.Pointer[crdt.Doc]) error {
	// Every write is refused when the live state it leaves could not be decoded, the one check
	// that decodes a whole second copy of the document. The caller has let go of whatever tree it
	// held from before the change, so the decode runs beside the fork and the tree the change left,
	// not a third tree (LEGION-504).
	if err := refuseUndecodable(op.fork); err != nil {
		return err
	}
	update, err := mergeUpdates(op.updates)
	if err != nil {
		return err
	}
	version, err := s.persistence.AppendUpdateTx(ctx, op.tx, op.write.artifactID, update, contentChanged)
	if err != nil {
		return fmt.Errorf("append transactional live document update: %w", err)
	}
	// The append holds the document's advisory lock until the transaction ends, so no eviction
	// of the room can finish before then, and a later reload holds the write. A room that failed
	// before it may already have reloaded without the write, beyond the writer slot, which is on
	// the failed room: the write cannot reach it coherently, so it fails - before it records the
	// rendering below, which a refused write must not leave behind.
	if err := op.write.state.failure(); err != nil {
		// The 503 this becomes is the only other trace of a room that stays failed, so the
		// refusal names the room and the cause here too (see awaitRoomRecovery).
		slog.Warn("dispatch: refuse a joined write on a failed document room",
			"room", op.write.artifactID, "error", err)
		return err
	}
	// The rendering is the document as the transaction now sees it, so a version it writes later
	// snapshots this tree rather than walking and rendering the document again
	// (captureLiveTextAndAuthors), and a later margin write records it again (applyMargin).
	// forkLive drops both the moment the fork moves.
	op.write.tree, op.write.markdown, op.write.offsets = rendered.tree, rendered.markdown, rendered.offsets
	if _, err := s.writeVersionTx(ctx, op.tx, op.write.artifactID, rendered, actor, nil); err != nil {
		return fmt.Errorf("refresh transactional document anchors: %w", err)
	}
	op.write.anchorsTree = rendered.tree
	op.write.updates = append(op.write.updates, update)
	op.write.versions = append(op.write.versions, version)
	op.write.renderings = append(op.write.renderings, rendering)
	op.write.marginRooms = append(op.write.marginRooms, marginRoom)
	op.recorded = true
	return nil
}

// applyLive runs mutate against artifactID's live document inside the transaction the context's
// ledger joined, and credits actor with the content it changes. Every document write a caller
// makes runs here - an upload's, an edit's, an accepted or rejected suggestion's, an ask's text
// and its answer, a comment's anchor - except a comment's or suggestion's margin record, which
// runs in applyMargin. Both open and record their operation through the same steps
// (openLiveOperation, recordLiveOperation), so two rules hold for every write by structure rather
// than by its caller remembering them:
//
//   - It refuses a context Join did not return (errUnjoined), as SeedText and the version writes
//     do: an unjoined write would reach no transaction to be weighed and appended in.
//   - It weighs what the write leaves, and refuses one that would grow the document past what one
//     upload may hold (refuseGrowth) or that leaves a live state ygo could not decode
//     (refuseUndecodable), before anything is appended. applyLive takes every measure of the
//     growth weigh; a margin record, which leaves the rendering and the anchor marks as they
//     were, is weighed by the margin bound alone (applyMargin).
//
// It writes the transaction's fork of the room (see liveWrite), appends the update inside the
// transaction, and leaves the room and the credit to its ledger's Commit, so a transaction that
// does not commit never reaches the room, a browser or a version's authors. It returns
// websocket.ErrNoChanges, as the room's Apply does for a write that wrote nothing, when mutate
// left the fork as it was, even through a transaction: ygo hands the fork's observers an update
// for every transaction, one that changes nothing included. It returns ErrServiceUnavailable when
// the room its slot is on failed before its append.
//
// mutate is handed the fork and current, the fork's tree as the operation starts, which applyLive
// reads anyway to weigh the write against: every mutate starts from it (startTree, editTree) rather
// than reading the fork again, and so holds no third copy of a heavy document beside the fork and
// the tree it writes (LEGION-504). current is applyLive's to compare with what the write leaves, so
// a mutate never changes it: every pmdoc edit returns a copy. It is nil when the fork's tree is
// outside the schema, where startTree reads the fork again for the refusal.
//
// The rendering of the fork before the operation, which only a write that changed the fork, or a
// failure to classify, needs, is often known without rendering the fork again, and is then taken
// as it is: forkLive's (liveWrite.forkMarkdown), the room replica's rendering of the very state
// the fork was read from (warmFork says why that is exact), or else the transaction's previous
// operation's (liveWrite.markdown), which is the fork's as it stands for as long as the write
// keeps its tree. A fork moves only through forkLive, which drops that rendering whenever it does
// (buildFork, releaseFork), and through an operation's mutate, after which that operation records
// the rendering of what it left or abandons the fork with its rendering (releaseFork). Rendering
// is deterministic over one state - pmdoc.Read and ReadForRendering differ only in copying
// attributes, and both trees render through pmdoc.RenderWithBlockOffsets - so either is the very
// string the render would make. A fork no rendering is known for is rendered here.
func (s *Service) applyLive(ctx context.Context, artifactID string, actor model.Actor, mutate func(doc *crdt.Doc, current *pmdoc.Node, transact func(func(*crdt.Transaction))) error) error {
	op, err := s.openLiveOperation(ctx, artifactID)
	if err != nil {
		return err
	}
	defer op.abandon()
	fork, write := op.fork, op.write
	// The tree the operation starts from. One outside the Proof schema - the reader refuses it, or
	// only the renderer does - is the document's own refusal (OutsideSchemaError), which every
	// schema refusal this operation meets is then answered with, unless the operation repairs it.
	// Its rendering is taken only when something needs it: a failure to classify, or the content
	// comparison of a write that changed the fork. It may be known without rendering the fork
	// (above), and is then picked up here, before mutate moves the fork it describes; only beside
	// a readable tree, since one the reader refuses is the refusal, whatever else is known. The
	// path it took is counted once, when it is first needed (renderPathCounts).
	before, startErr := s.readFork(fork)
	if startErr != nil && !errors.Is(startErr, ErrDocOutsideSchema) {
		return startErr
	}
	var beforeMarkdown string
	var known *atomic.Uint64
	if startErr == nil {
		switch {
		case write.forkMarkdown != nil:
			beforeMarkdown, known = *write.forkMarkdown, &s.renderPaths.beforeFromReplica
		case write.tree != nil:
			beforeMarkdown, known = write.markdown, &s.renderPaths.beforeFromWrite
		}
	}
	write.forkMarkdown = nil
	rendered := startErr != nil
	start := func() error {
		if !rendered {
			if known != nil {
				known.Add(1)
			} else {
				beforeMarkdown, startErr = documentMarkdown(before)
				s.renderPaths.beforeRendered.Add(1)
			}
			rendered = true
		}
		return startErr
	}
	// What says the write changed the fork is the fork itself, read on either side of mutate: its
	// state vector and its delete set, since a deletion advances no clock (forkHeld). The updates
	// the fork emits cannot say it, for they include one for a transaction that changes nothing.
	// applyMargin reads neither: a marks-map Set always adds an item, so its write always moves the
	// fork, and the two whole-store walks would buy it nothing.
	wasClocks, wasDeletes := fork.StateVector(), crdt.DeleteSetFromDoc(fork)
	margin, mutateErr := op.change(func() error {
		return mutate(fork, before, func(inner func(*crdt.Transaction)) { fork.Transact(inner) })
	})
	if mutateErr != nil {
		return joinedSchemaError(mutateErr, start)
	}
	// A write whose transactions changed nothing writes nothing. Appended, its empty update would
	// refresh the anchors and mark the document owing a settlement again (markUpdateOwed), which
	// every repair pass over a closed issue would then sweep. Its updates are dropped, so the fork,
	// which it left as it was, is kept.
	if len(op.updates) > 0 && forkHeld(wasClocks, wasDeletes, fork) {
		op.updates = nil
	}
	if len(op.updates) == 0 {
		return websocket.ErrNoChanges
	}
	tree, err := s.readFork(fork)
	var markdown string
	var offsets []pmdoc.BlockOffset
	if err == nil {
		markdown, offsets, err = documentMarkdownWithOffsets(tree)
	}
	if err != nil {
		return joinedSchemaError(err, start)
	}
	// A write over a tree outside the Proof schema that leaves it readable is a repair, so it
	// necessarily changes the content a version stores, and what it leaves is weighed as a new
	// document's rendering is (SeedText): against nothing, with no anchor marks before it and no
	// typed-block state to be the only change. Other writes compare the markdown before and after,
	// the measure the room's update observer classifies a live update by (updateChangesMarkdown): a
	// write that leaves the rendered markdown alone - an anchor mark, an attribute no rendering
	// carries - changes nothing a version stores.
	contentChanged, weighedFrom := true, (*pmdoc.Node)(nil)
	err = start()
	if err != nil && !errors.Is(err, ErrDocOutsideSchema) {
		return err
	}
	unchanged := beforeMarkdown == markdown
	if unchanged {
		// One string retained, not two equal ones: the write's rendering and the one it was
		// compared with.
		markdown = beforeMarkdown
	}
	if err == nil {
		contentChanged, weighedFrom = !unchanged, before
	}
	// The ask blocks the write added are read off the tree before it now, so that tree is done
	// with ahead of the decode check the record runs.
	var added map[string]struct{}
	if contentChanged {
		added = addedAskBlockIDs(before, tree)
	}
	if err := refuseGrowth(growth{before: beforeMarkdown, after: markdown, unchanged: unchanged, margin: margin, anchors: anchorWatch{before: weighedFrom, after: tree}, serverState: func() bool {
		return weighedFrom != nil && tree.EqualOutsideServerState(weighedFrom)
	}}); err != nil {
		return err
	}
	// The tree before the write has served every use it has, so it is let go before the record's
	// decode check (LEGION-504).
	before, weighedFrom = nil, nil
	// What the room's update observer can take in place of rendering the room after this update
	// (publishedRendering): this operation's rendering, when its fork was read warm.
	var rendering publishedRendering
	if op.read.warm {
		rendering = publishedRendering{forkReadMark: op.read, markdown: markdown}
		if contentChanged {
			rendering.askBlocks = askBlockIDs(tree)
		}
	}
	if err := s.recordLiveOperation(ctx, &op, actor, renderedTree{tree: tree, markdown: markdown, offsets: offsets}, contentChanged, rendering, weak.Pointer[crdt.Doc]{}); err != nil {
		return err
	}
	// A server lifecycle write (withLifecycleWrite) is no edit of the document: it credits no
	// pending author and leaves the document's latest edit source as it was.
	if contentChanged && !isLifecycleWrite(ctx) {
		s.creditLiveWrite(op.write, actor)
		maps.DeleteFunc(added, func(id string, _ struct{}) bool {
			_, excluded := op.write.renamedAskBlockIDs[id]
			return excluded
		})
		mergeInto(&op.write.addedAskBlockIDs, added)
	}
	return nil
}

// applyMargin writes record as markID's margin record - a comment's or suggestion's, which a
// browser shows beside the document (ProjectMark) - inside the transaction the context's ledger
// joined. It is applyLive for the one write that changes the marks map alone and never the
// document's XML fragment, so the document's tree and rendering are the same on either side of it,
// and it skips the work applyLive does to tell what a write changed.
//
// It opens and records its operation through applyLive's own steps (openLiveOperation,
// recordLiveOperation), so it refuses a shutting-down document and an unjoined context
// (errUnjoined), refuses a live state ygo could not decode (refuseUndecodable) and appends its
// update at the same points, and refreshes the document's anchors against the tree in its own
// transaction (writeVersionTx) as every applyLive operation does - on an anchored comment's root
// that refresh is the first to see the comment's new row. Between those steps:
//
//   - It reads no tree before the write and renders none. applyLive reads and renders it to
//     compare with what the write leaves - the content class (updateChangesMarkdown's measure),
//     the ask blocks added (addedAskBlockIDs), the anchor marks gained - and here both are the
//     same fragment: the update changes no content, adds no ask block, and credits actor with
//     nothing (creditLiveWrite).
//   - The tree and rendering it records are the ones the transaction's last operation left on its
//     write (liveWrite.rendering, taken through reuseRendering), when this operation's own forkLive
//     kept them: forkLive drops them whenever the fork moves (buildFork, releaseFork), and so does
//     a refused operation (abandon), so kept, they describe the fork's fragment exactly, and this
//     write does not move it. Otherwise it reads and renders the fork once (readFork). A tree the
//     reader or only the renderer refuses is then the document's OutsideSchemaError (treeOf,
//     documentMarkdownWithOffsets), the one applyLive answers from the tree it started from
//     (joinedSchemaError), which is this same fragment, and it comes before any growth refusal, as
//     there.
//   - Of the growth weigh (refuseGrowth) only the margin bound applies (marginWatch). The anchor
//     marks the weigh counts (anchorWatch) are on the unchanged tree, so they carry what they
//     carried and their weigh never refuses - it walks every mark of the tree to say so
//     (anchorText) - and an unchanged rendering is never weighed.
//   - It records beside its update the room its fork was warm-built from (liveWrite.marginRooms),
//     so the room's update observer, which would render the publish only to find the same
//     markdown, keeps its rendering instead when the update reaches that same room
//     (liveWriteOrigin.marginOnly says why that is exact). For a warm fork it also records, as
//     applyLive does, its rendering of the fork with the fork read's replica counters
//     (publishedRendering): the rendering it records, which the margin write leaves as it was, with
//     no ask blocks, since it changes no rendered markdown. A cold fork records neither.
//
// A marks-map Set always adds an item, so the write always changes the fork: unlike applyLive it
// never answers websocket.ErrNoChanges. A panic in it is its error (recoverMutation), as there.
func (s *Service) applyMargin(ctx context.Context, artifactID string, actor model.Actor, markID string, record map[string]any) error {
	op, err := s.openLiveOperation(ctx, artifactID)
	if err != nil {
		return err
	}
	defer op.abandon()
	margin, err := op.change(func() error {
		marks := op.fork.GetMap(marksMapName)
		op.fork.Transact(func(txn *crdt.Transaction) {
			marks.Set(txn, markID, record)
		})
		return nil
	})
	if err != nil {
		return err
	}
	rendered, reused := s.reuseRendering(op.write, op.fork, "margin")
	if !reused {
		if rendered.tree, err = s.readFork(op.fork); err != nil {
			return err
		}
		if rendered.markdown, rendered.offsets, err = documentMarkdownWithOffsets(rendered.tree); err != nil {
			return err
		}
	}
	if err := margin.refusal(); err != nil {
		return err
	}
	// The room the fork was warm-built from, which the publish tags the update against
	// (liveWriteOrigin.marginOnly), and the rendering the observer may take in its place
	// (publishedRendering). A cold fork was built from the stored document, with no room behind
	// it, so it names neither; write.cold says so directly rather than leaving it to coldFork to
	// have cleared forkedFrom and forkLive the fork read's counters.
	var room weak.Pointer[crdt.Doc]
	var rendering publishedRendering
	if !op.write.cold {
		room = op.write.forkedFrom
		if op.read.warm {
			rendering = publishedRendering{forkReadMark: op.read, markdown: rendered.markdown}
		}
	}
	return s.recordLiveOperation(ctx, &op, actor, rendered, false, rendering, room)
}

// joinedSchemaError is the error a joined operation answers for err, a failure it met after it
// read the tree it started from (start). A schema refusal is that tree's own when the tree is
// outside the schema - start's OutsideSchemaError, the same one every route answers - and
// otherwise the operation's fault, since the tree it started from was readable. A refusal of the
// caller's own ask block keeps its reason, which writeHandlerError answers before any schema
// refusal.
func joinedSchemaError(err error, start func() error) error {
	var invalidAsk *ErrInvalidAskBlock
	if !errors.Is(err, ErrDocSchema) || errors.As(err, &invalidAsk) {
		return err
	}
	if startErr := start(); errors.Is(startErr, ErrDocOutsideSchema) {
		return startErr
	}
	return producedSchemaError(err)
}

func mergeUpdates(updates [][]byte) ([]byte, error) {
	switch len(updates) {
	case 0:
		return nil, websocket.ErrNoChanges
	case 1:
		return updates[0], nil
	default:
		update, err := crdt.MergeUpdatesV1(updates...)
		if err != nil {
			return nil, fmt.Errorf("merge live document updates: %w", err)
		}
		return update, nil
	}
}

// SeedText writes a new room's first Yjs update inside the caller's artifact
// creation transaction. A new room is loaded from this update on first use.
func (s *Service) SeedText(ctx context.Context, artifactID, markdown string, actor model.Actor) (string, error) {
	tx, joined := txFromContext(ctx)
	if !joined {
		return "", errUnjoined
	}
	tree, err := parseInput(markdown)
	if err != nil {
		return "", err
	}
	if err := pmdoc.AskContentError(tree); err != nil {
		return "", &ErrInvalidAskBlock{Reason: err}
	}
	canonical, offsets, err := renderTreeWithOffsets(tree)
	if err != nil {
		return "", err
	}
	if err := weighRendering("", canonical); err != nil {
		return "", err
	}
	update, err := encodeDocumentTree(tree)
	if err != nil {
		return "", fmt.Errorf("seed live document tree: %w", err)
	}
	if _, err := s.persistence.AppendUpdateTx(ctx, tx, artifactID, update, true); err != nil {
		return "", fmt.Errorf("seed live document: %w", err)
	}
	// The seeding actor is the caller's own first version author (written directly by the
	// caller, never through writeVersionTx), so it must not join `pending` - only the
	// settlement that indexes the seeded ask blocks needs to know who wrote them. The ledger
	// records it once the transaction commits (Ledger.creditRooms), so a seed that never commits
	// leaves the document no state.
	ledgerFrom(ctx).seeded(artifactID, renderedTree{tree: tree, markdown: canonical, offsets: offsets}, actor)
	return canonical, nil
}

// ReplaceText replaces the entire live document tree so connected clients
// receive document uploads as a regular server-side transaction.
func (s *Service) ReplaceText(ctx context.Context, artifactID, markdown string, actor model.Actor) (string, error) {
	tx, joined := txFromContext(ctx)
	if !joined {
		return "", errUnjoined
	}
	anchors, err := s.openAnchoredMarks(ctx, s.queryFrom(ctx), artifactID)
	if err != nil {
		return "", err
	}
	// The live document's locks, in their order (see liveWrite), before the write reads the
	// document: its owner row and writer slot, then its advisory lock. The upload's version clears
	// every pending author and each in-flight credit its read held (Ledger.WroteVersion), so no
	// edit may land between that read and its commit. A document no room holds is read from the
	// store under that lock (coldFork), which loads no room for the write.
	if _, err := s.joinLiveWrite(ctx, ledgerFrom(ctx), artifactID); err != nil {
		return "", err
	}
	if err := lockDocumentRoom(ctx, tx, artifactID); err != nil {
		return "", err
	}
	var canonical string
	var unchanged bool
	err = s.applyLive(ctx, artifactID, actor, func(doc *crdt.Doc, current *pmdoc.Node, transact func(func(*crdt.Transaction))) error {
		fragment := doc.GetXmlFragment(fragmentName)
		current, err := startTree(current, doc)
		var currentMarkdown string
		if err == nil {
			currentMarkdown, err = documentMarkdown(current)
		}
		// A tree outside the Proof schema - one treeOf refuses, or one it reads that does not
		// render - has no readable text to compare or reanchor against: this replacement repairs it.
		repairing := errors.Is(err, ErrDocOutsideSchema)
		if err != nil && !repairing {
			return err
		}
		if repairing {
			current = nil
		}
		target, err := parseReplacing(current, markdown)
		if err != nil {
			return err
		}
		// A repair is a whole new document, held to the ask content rule for every ask, as
		// RebuildDocument holds supplied markdown, and it keeps every open ask block.
		if repairing {
			if err := pmdoc.AskContentError(target); err != nil {
				return &ErrInvalidAskBlock{Reason: err}
			}
			if err := s.refuseDroppedAskBlocks(ctx, artifactID, target); err != nil {
				return &ErrInvalidAskBlock{Reason: err}
			}
		} else if err := refuseChangedAsks(current, target, pmdoc.AskContentError, newAskMarkdown()); err != nil {
			return &ErrInvalidAskBlock{Reason: err}
		}
		if canonical, err = renderTree(target); err != nil {
			return err
		}
		if !repairing && currentMarkdown == canonical {
			unchanged = true
			return nil
		}
		type reanchor struct {
			mark   anchoredMark
			range_ pmdoc.Range
			attrs  pmdoc.Attrs
		}
		reanchors := make([]reanchor, 0, len(anchors))
		if !repairing {
			// One walk reads every row's mark, where one per row would make an upload cost the
			// document once per open anchor.
			keys := make([]pmdoc.MarkRef, len(anchors))
			for index, mark := range anchors {
				keys[index] = mark.markRef()
			}
			marks := pmdoc.FindMarks(current, keys)
			for index, mark := range anchors {
				found, ok := marks[keys[index]]
				if !ok {
					continue
				}
				reanchors = append(reanchors, reanchor{mark: mark, range_: found.Range, attrs: found.Attrs})
			}
		}
		// An upload over open anchors writes the room the target with its anchors already on it,
		// built first in a scratch document no one else sees: the target written there, each open
		// anchor marked again where its quote now stands, and the tree read back. Writing the
		// target to the room without its anchors and marking them again re-formats every anchored
		// run twice, stripping each anchor and adding it back: a tombstone per anchor per upload
		// that the room keeps for the document's life, even where the upload left the anchored text
		// unchanged. pmdoc.Update leaves the text it finds unchanged as it is, so writing the tree
		// with its anchors leaves the anchored text the upload did not touch alone.
		write := target
		if len(reanchors) > 0 {
			scratch := newDocumentCopy()
			scratchFragment := scratch.GetXmlFragment(fragmentName)
			if err := scratch.TransactE(func(txn *crdt.Transaction) error {
				if err := pmdoc.Update(txn, scratchFragment, target); err != nil {
					return err
				}
				// One finder over target and one writer over the scratch fragment serve every row, so
				// the upload flattens target and reads the fragment's text ranges once, not once a row.
				// Both read nothing until the first row needs them. A row whose quote no longer
				// resolves keeps no mark, so its miss is never described. Each row is marked at the
				// occurrence of its quote nearest where it stood, and where that occurrence stands
				// somewhere the schema allows no mark of its type (pmdoc.ErrMarkNotAllowed: an ask's
				// quote in a code block, any anchor's in the front matter), at the next nearest that
				// allows it: an upload that copies anchored text into a code block or the front matter
				// and keeps the original leaves the anchor on the original. Only when no occurrence
				// allows the mark is the upload refused, as its markdown's to fix (reanchorError).
				finder := pmdoc.NewQuoteFinder(target)
				writer := pmdoc.NewMarkWriter(scratchFragment)
			reanchors:
				for _, reanchor := range reanchors {
					occurrences, err := finder.NearestFirst(reanchor.mark.anchor.Quote, reanchor.range_.From)
					if errors.Is(err, pmdoc.ErrTargetNotFound) {
						continue
					}
					if err != nil {
						return reanchorError(reanchor.mark, err)
					}
					mark := MarkSpec{
						Kind:  MarkKind(reanchor.mark.markType),
						ID:    reanchor.mark.anchor.MarkID,
						Attrs: reanchor.attrs,
					}.pmMark()
					// Every occurrence the schema refuses the mark on is kept, so a quote with
					// nowhere left to stand names each place it stands, and how many places of each
					// kind, rather than the last place tried, and one fix of the markdown resolves
					// the upload.
					var notAllowed []error
					for _, occurrence := range occurrences {
						refused := writer.Mark(txn, occurrence, mark)
						if refused == nil || errors.Is(refused, pmdoc.ErrTargetNotFound) {
							continue reanchors
						}
						if !errors.Is(refused, pmdoc.ErrMarkNotAllowed) {
							return reanchorError(reanchor.mark, refused)
						}
						notAllowed = append(notAllowed, refused)
					}
					return reanchorError(reanchor.mark, notAllowed...)
				}
				var err error
				write, err = pmdoc.ReadInTransaction(txn, scratchFragment)
				return err
			}); err != nil {
				return err
			}
		}
		var updateErr error
		transact(func(transaction *crdt.Transaction) {
			// A repair keeps nothing of the unreadable tree, which pmdoc.Update would otherwise
			// diff against node by node, reading text content - an embed a crafted client wrote
			// into a paragraph, say - that it refuses.
			if length := fragment.Len(); repairing && length > 0 {
				fragment.Delete(transaction, 0, length)
			}
			updateErr = pmdoc.Update(transaction, fragment, write)
		})
		return updateErr
	})
	if unchanged && errors.Is(err, websocket.ErrNoChanges) {
		return canonical, nil
	}
	if err != nil {
		return "", wrapUnlessTooLarge(err, "replace live document")
	}
	return canonical, nil
}

// anchorKinds names, for a person, each mark type an open anchor carries (openAnchoredMarks).
var anchorKinds = map[string]string{
	string(MarkAsk):        "ask",
	string(MarkComment):    "comment",
	string(MarkSuggestion): "suggestion",
}

// reanchorError is an upload's refusal to put an open anchor back, causes being the finder's or the
// mark writer's refusal, and, where every occurrence of the quote refuses the mark, one refusal an
// occurrence, nearest first. A mark the schema allows nowhere the anchor's quote now stands
// (pmdoc.MarkNotAllowedError) is the caller's markdown to fix: ErrInvalidMarkdown, naming the
// anchor's kind and quote, each kind of place its text landed and how many such places, the kinds
// in the order of their first occurrence, so the nearest comes first, and the two ways
// out, holding no cause it wraps, so no route reads it as a document outside its schema. The count
// is what keeps one pass over the markdown enough: a quote left in two code blocks says so, rather
// than reading as one code block and refusing the upload again once its author has moved the first.
// Any other cause, or a mark type no open anchor carries, is a fault of the stored anchor or of
// the server: a plain ErrDocSchema, which the routes answer 500 and log, and never a stored
// document to repair, since the scratch document no peer can reach holds nothing but the caller's
// target.
func reanchorError(mark anchoredMark, causes ...error) error {
	kind, named := anchorKinds[mark.markType]
	places := make([]refusedPlace, 0, len(causes))
	for _, cause := range causes {
		var notAllowed *pmdoc.MarkNotAllowedError
		if !named || !errors.As(cause, &notAllowed) {
			return fmt.Errorf("%w: re-anchor %s %s: %v", ErrDocSchema, mark.markType, mark.anchor.MarkID, errors.Join(causes...))
		}
		place := notAllowed.Place()
		if index := slices.IndexFunc(places, func(seen refusedPlace) bool { return seen.singular == place }); index >= 0 {
			if !slices.Contains(places[index].at, notAllowed.At) {
				places[index].at = append(places[index].at, notAllowed.At)
			}
			continue
		}
		places = append(places, refusedPlace{singular: place, plural: notAllowed.PlacePlural(), at: []int{notAllowed.At}})
	}
	them, distinct := "it", 0
	for _, place := range places {
		distinct += len(place.at)
	}
	if distinct > 1 {
		them = "them"
	}
	counted := make([]string, len(places))
	for index, place := range places {
		counted[index] = place.String()
	}
	return fmt.Errorf("%w: the open %s anchored on %q would land in %s, where its mark is not allowed, and the upload leaves its quote nowhere it may stand; move that text out of %s, or resolve the %s first",
		ErrInvalidMarkdown, kind, mark.anchor.Quote, placeList(counted), them, kind)
}

// refusedPlace is one kind of place an upload left an anchor's quote where its mark is not allowed,
// with where each such place begins (pmdoc.MarkNotAllowedError.At): a quote left in two code blocks
// is one refusedPlace holding two, so the refusal says "two code blocks" and its author moves both
// out in one pass over the markdown, while two occurrences in one code block are one place. singular
// and plural are pmdoc.MarkNotAllowedError's words for the place, the plural empty where the schema
// holds one place of the kind at most.
type refusedPlace struct {
	singular string
	plural   string
	at       []int
}

// String names the place as a person counts it: "a code block" for one, "two code blocks" for two,
// and "the front matter" always, since a document inside the schema holds one.
func (p refusedPlace) String() string {
	count := len(p.at)
	if count < 2 || p.plural == "" {
		return p.singular
	}
	if count < len(spelledCounts) {
		return spelledCounts[count] + " " + p.plural
	}
	return strconv.Itoa(count) + " " + p.plural
}

// spelledCounts are the counts a person reads as a word before a place ("two code blocks"); past
// them the digits read better, and a quote standing in that many places is past counting at a
// glance anyway.
var spelledCounts = [...]string{2: "two", 3: "three", 4: "four", 5: "five", 6: "six", 7: "seven", 8: "eight", 9: "nine", 10: "ten"}

// placeList reads the places a refused anchor's quote stands as a person says them: "a code
// block", "the front matter and two code blocks".
func placeList(places []string) string {
	if len(places) < 2 {
		return strings.Join(places, "")
	}
	return strings.Join(places[:len(places)-1], ", ") + " and " + places[len(places)-1]
}

func (s *Service) refuseDroppedAskBlocks(ctx context.Context, artifactID string, target *pmdoc.Node) error {
	rows, err := s.queryFrom(ctx).Query(ctx, `
		select block_id from asks
		where block_artifact_id = $1 and state = 'open' and block_id is not null
	`, artifactID)
	if err != nil {
		return fmt.Errorf("read open ask blocks: %w", err)
	}
	defer rows.Close()
	open := map[string]struct{}{}
	for rows.Next() {
		var blockID string
		if err := rows.Scan(&blockID); err != nil {
			return fmt.Errorf("scan open ask block: %w", err)
		}
		open[blockID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read open ask blocks: %w", err)
	}
	pmdoc.Walk(target, func(node *pmdoc.Node) bool {
		if node.Type == "ask" {
			if blockID, ok := node.Attrs[pmdoc.BlockIDAttr].(string); ok {
				delete(open, blockID)
			}
		}
		return true
	})
	if len(open) == 0 {
		return nil
	}
	blockIDs := make([]string, 0, len(open))
	for blockID := range open {
		blockIDs = append(blockIDs, blockID)
	}
	sort.Strings(blockIDs)
	return fmt.Errorf("replacement removes open ask blocks %s", strings.Join(blockIDs, ", "))
}

// Text returns the rendered document the caller sees (readDocument).
func (s *Service) Text(ctx context.Context, artifactID string) (string, error) {
	read, tree, err := s.readDocument(ctx, artifactID)
	switch {
	case err != nil:
		return "", err
	case read != nil:
		return read.markdown, nil
	case tree == nil:
		return "", nil
	}
	return documentMarkdown(tree)
}

// TextWithToken returns canonical markdown and a token over its full Proof tree,
// including inline marks that canonical Markdown does not render.
func (s *Service) TextWithToken(ctx context.Context, artifactID string) (string, string, error) {
	read, tree, err := s.readDocument(ctx, artifactID)
	switch {
	case err != nil:
		return "", "", err
	case read != nil:
		return read.markdown, read.token, nil
	case tree == nil:
		return "", "", nil
	}
	return renderTokenTree(tree)
}

// readDocument is the document the caller sees, read without loading or writing its room, so it
// also reads a closed issue's document: the tree of the calling transaction's fork or of the
// resident room (heldTree), else the rendering of the stored document (coldRead), or the stored
// document's tree where the rendering does not take it. A document with no stored state has
// neither, and no error.
func (s *Service) readDocument(ctx context.Context, artifactID string) (*documentRead, *pmdoc.Node, error) {
	tree, held, err := s.heldTree(ctx, artifactID)
	if err != nil || held {
		return nil, tree, err
	}
	return s.coldRead(ctx, artifactID)
}

// heldTree is the tree of the document where this server holds it, once a failed room's recovery
// is done: the calling transaction's fork's when it has one (joinRead), else the resident room's,
// as of one moment (liveTree). held is false where neither holds it.
func (s *Service) heldTree(ctx context.Context, artifactID string) (tree *pmdoc.Node, held bool, err error) {
	if err := s.awaitRoomRecovery(ctx, artifactID); err != nil {
		return nil, false, err
	}
	fork, err := s.joinRead(ctx, artifactID)
	if err != nil {
		return nil, false, err
	}
	if fork != nil {
		tree, err := s.readFork(fork)
		return tree, true, err
	}
	if live := s.srv.GetDoc(artifactID); live != nil {
		tree, err := s.liveTree(artifactID, live)
		return tree, true, err
	}
	return nil, false, nil
}

// loadTree is the tree of the document a socket's admission check reads (heldTree, else the stored
// document decoded), with the durable state it decoded the document from, nil when this server
// holds the document. A durable history that does not decode is ErrDocumentUnloadable and fails
// the room, as ygo's own load of it would.
func (s *Service) loadTree(ctx context.Context, artifactID string) (*pmdoc.Node, *persistence.LoadResult, error) {
	tree, held, err := s.heldTree(ctx, artifactID)
	if err != nil || held {
		return tree, nil, err
	}
	loaded, err := s.persistence.Load(ctx, artifactID)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, nil, err
		}
		s.failRoom(artifactID, err)
		return nil, nil, fmt.Errorf("%w: %w", ErrServiceUnavailable, err)
	}
	if len(loaded.Update) == 0 {
		return nil, &loaded, nil
	}
	doc := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(doc, loaded.Update, nil); err != nil {
		// The room carries the decode failure as its cause, which a reader that meets the
		// failed room answers as DOC_SERVICE_UNAVAILABLE; this read met the history itself.
		s.failRoom(artifactID, fmt.Errorf("decode live document: %w", err))
		return nil, nil, fmt.Errorf("%w: decode live document: %w", ErrDocumentUnloadable, err)
	}
	tree, err = treeOf(doc)
	return tree, &loaded, err
}

func renderTokenTree(tree *pmdoc.Node) (string, string, error) {
	markdown, err := documentMarkdown(tree)
	if err != nil {
		return "", "", err
	}
	token, err := nodeToken(tree)
	if err != nil {
		return "", "", err
	}
	return markdown, token, nil
}

// Blocks returns each stamped block and its byte range in canonical markdown.
func (s *Service) Blocks(ctx context.Context, artifactID string) ([]model.ArtifactBlock, error) {
	_, blocks, err := s.TextWithBlocks(ctx, artifactID)
	return blocks, err
}

// TextWithBlocks renders the document the caller sees (readDocument) once and returns its
// canonical markdown beside the blocks whose byte ranges index into it. The blocks are the
// caller's own: it may write into them (References).
func (s *Service) TextWithBlocks(ctx context.Context, artifactID string) (string, []model.ArtifactBlock, error) {
	read, tree, err := s.readDocument(ctx, artifactID)
	switch {
	case err != nil:
		return "", nil, err
	case read != nil:
		return read.markdown, slices.Clone(read.blocks), nil
	case tree == nil:
		return "", nil, nil
	}
	return documentBlocks(tree)
}

// documentBlocks renders tree once into its canonical markdown and the blocks whose byte ranges
// index into it, each with its token and, for a table, the blocks it holds and its rows. It leaves
// out the document's token, which none of its callers reads. It validates tree once, refusing it
// as pmdoc.TableRows does, and reads it thereafter without validating it again.
func documentBlocks(tree *pmdoc.Node) (string, []model.ArtifactBlock, error) {
	valid, err := pmdoc.ValidateDocument(tree, "TableRows")
	if err != nil {
		return "", nil, documentSchemaError(err)
	}
	rows, err := valid.TableRows()
	if err != nil {
		return "", nil, documentSchemaError(err)
	}
	rendered, err := renderedBlocks(valid, rows)
	if err != nil {
		return "", nil, err
	}
	return rendered.markdown, rendered.blocks, nil
}

// renderedDocument is a tree rendered (renderedBlocks): its canonical markdown, its blocks, and
// its document token beside the error that refused that token. A tree holding a value no token can
// encode outside every block still renders, so the document token's error is the caller's to
// weigh rather than the rendering's.
type renderedDocument struct {
	markdown string
	blocks   []model.ArtifactBlock
	token    string
	tokenErr error
}

// renderedBlocks is documentBlocks with each table's rows already listed (pmdoc.TableRows), and the
// document's token, from the same one encoding of the tree as its blocks' tokens (documentTokens).
// It reads tree, validated once by its caller, without validating it again: its descendants, its
// tokens and its rendering (pmdoc.ValidDocument). A table block carries the rows of the table its
// id addresses: where two tables carry one id, the first in document order, the one an edit naming
// that id changes. Its error is a block token's or the rendering's, never the document token's,
// which is tokenErr.
func renderedBlocks(tree pmdoc.ValidDocument, rows map[string][]pmdoc.TableRow) (renderedDocument, error) {
	tableDescendants, err := tree.TableDescendantIDs()
	if err != nil {
		return renderedDocument{}, documentSchemaError(err)
	}
	tokens := documentTokens(tree)
	if tokens.blocksErr != nil {
		return renderedDocument{}, documentSchemaError(tokens.blocksErr)
	}
	markdown, offsets, err := tree.RenderWithBlockOffsets()
	if err != nil {
		return renderedDocument{}, documentSchemaError(err)
	}
	tables := make(map[string]*model.BlockTable, len(rows))
	blocks := make([]model.ArtifactBlock, len(offsets))
	for index, offset := range offsets {
		blocks[index] = model.ArtifactBlock{
			ID:            offset.ID,
			Type:          offset.Type,
			From:          offset.From,
			To:            offset.To,
			Token:         tokens.blocks[offset.ID],
			DescendantIDs: tableDescendants[offset.ID],
		}
		if offset.Type != "table" {
			continue
		}
		table, listed := tables[offset.ID]
		if !listed {
			table = blockTable(rows[offset.ID])
			tables[offset.ID] = table
		}
		blocks[index].Table = table
	}
	return renderedDocument{markdown: markdown, blocks: blocks, token: tokens.document, tokenErr: tokens.documentErr}, nil
}

// blockTable is a table's rows as the API carries them.
func blockTable(rows []pmdoc.TableRow) *model.BlockTable {
	table := &model.BlockTable{Rows: make([]model.BlockTableRow, len(rows))}
	for index, row := range rows {
		cells := make([]model.BlockTableCell, len(row.Cells))
		for column, cell := range row.Cells {
			cells[column] = model.BlockTableCell(cell)
		}
		table.Rows[index] = model.BlockTableRow{ID: row.ID, Index: row.Index, Cells: cells}
	}
	return table
}

// BlockPath is where the block carrying blockID stands in the document the caller sees
// (readDocument): pmdoc.ErrTargetNotFound when no block carries it, a document with no state
// included.
func (s *Service) BlockPath(ctx context.Context, artifactID, blockID string) (model.BlockPath, error) {
	read, tree, err := s.readDocument(ctx, artifactID)
	if err != nil {
		return model.BlockPath{}, err
	}
	if read != nil {
		path, ok := read.paths[blockID]
		if !ok {
			return model.BlockPath{}, fmt.Errorf("%w: block %q", pmdoc.ErrTargetNotFound, blockID)
		}
		return modelBlockPath(path), nil
	}
	if tree == nil {
		return model.BlockPath{}, fmt.Errorf("%w: block %q", pmdoc.ErrTargetNotFound, blockID)
	}
	path, err := pmdoc.BlockPathOf(tree, blockID)
	if err != nil {
		return model.BlockPath{}, err
	}
	return modelBlockPath(path), nil
}

// modelBlockPath is path as the API carries it, in entries of its own. Its table position's row,
// column, header and cells are shared with path, which pmdoc.BlockPathsAndTableRows shares across a table's paths and
// the read cache across every read of one stored head (documentRead); nothing writes them.
func modelBlockPath(path pmdoc.BlockPath) model.BlockPath {
	out := model.BlockPath{ID: path.ID, Type: path.Type, Path: make([]model.BlockPathEntry, len(path.Path))}
	for index, entry := range path.Path {
		out.Path[index] = model.BlockPathEntry(entry)
	}
	if path.Table != nil {
		table := model.TablePosition(*path.Table)
		out.Table = &table
	}
	return out
}

// SnapshotVersion returns the current immutable version, adding an unnamed
// version only when the live text has diverged since the previous one.
func (s *Service) SnapshotVersion(ctx context.Context, artifactID string, actor model.Actor) (VersionResult, error) {
	tx, joined := txFromContext(ctx)
	if !joined {
		return VersionResult{}, errUnjoined
	}
	rendered, capture, err := s.captureLiveTextAndAuthors(ctx, artifactID, nil)
	if err != nil {
		return VersionResult{}, err
	}
	latest, err := latestVersion(ctx, tx, artifactID)
	if err != nil {
		return VersionResult{}, err
	}
	if latest.markdown == rendered.markdown {
		return VersionResult{Version: latest.Version}, nil
	}
	capture.authors[actorKey(actor)] = actor
	_, open, err := lockArtifactOwner(ctx, tx, artifactID)
	if err != nil {
		return VersionResult{}, err
	}
	if !open {
		return VersionResult{}, ErrIssueClosed
	}
	result, err := s.writeVersionTx(ctx, tx, artifactID, rendered, actor, &versionWrite{
		authors: actorSlice(capture.authors),
		capture: &capture,
	})
	if err != nil {
		return VersionResult{}, err
	}
	if err := s.recordEditNotice(ctx, tx, artifactID, result.version.Number, actorSlice(capture.authors), capture.latest); err != nil {
		return VersionResult{}, err
	}
	return VersionResult{Version: result.version, Wrote: true, Changes: result.changes}, nil
}

// prevalidateLiveOperations performs database-backed table-anchor checks
// before the Yjs transaction, retaining the table-mark snapshots the
// transaction re-derives before it writes.
func (s *Service) prevalidateLiveOperations(ctx context.Context, artifactID string, ops []model.EditOp) ([]tableAnchorSnapshot, error) {
	tree, err := s.docTree(ctx, artifactID)
	if err != nil {
		return nil, err
	}
	return s.prevalidateOperations(ctx, artifactID, tree, ops)
}

const maxQueuedConditionalEdits = 32

type conditionalEditGate struct {
	turn   chan struct{}
	mu     sync.Mutex
	queued int
}

// AcquireConditionalEdit queues one conditional edit in memory before it can
// acquire a database connection, keeping stale retries from exhausting pgx.
func (s *Service) AcquireConditionalEdit(ctx context.Context, artifactID string) (func(), error) {
	for {
		created := &conditionalEditGate{turn: make(chan struct{}, 1)}
		created.turn <- struct{}{}
		value, _ := s.conditionalGates.LoadOrStore(artifactID, created)
		gate := value.(*conditionalEditGate)
		gate.mu.Lock()
		current, present := s.conditionalGates.Load(artifactID)
		if !present || current != gate {
			gate.mu.Unlock()
			continue
		}
		if gate.queued >= maxQueuedConditionalEdits {
			gate.mu.Unlock()
			return nil, ErrPreconditionBusy
		}
		gate.queued++
		gate.mu.Unlock()

		select {
		case <-gate.turn:
			return func() {
				gate.mu.Lock()
				gate.queued--
				last := gate.queued == 0
				if last {
					s.conditionalGates.CompareAndDelete(artifactID, gate)
				}
				gate.mu.Unlock()
				if !last {
					gate.turn <- struct{}{}
				}
			}, nil
		case <-ctx.Done():
			gate.mu.Lock()
			gate.queued--
			last := gate.queued == 0
			if last {
				s.conditionalGates.CompareAndDelete(artifactID, gate)
			}
			gate.mu.Unlock()
			return nil, ctx.Err()
		}
	}
}

// lockTableAskAnchorsQuery and lockTableCommentAnchorsQuery take a share lock on each row of
// their table that is anchored on the document bound to $1 and records one of the marks bound
// to $2, so no writer resolves or answers one of them while the edit that would remove its
// table row runs. Each finds its rows through its table's anchor index
// (anchorTarget.anchoredOn), so the lock reads the document's anchors rather than the table;
// the text of each is built once, as the edit validation's statements are.
var (
	lockTableAskAnchorsQuery = `
		select id::text, anchor->>'mark_id', state
		from asks
		where ` + asksAnchorTarget.anchoredOn() + ` and anchor->>'mark_id' = any($2::text[])
		for share
	`
	lockTableCommentAnchorsQuery = `
		select id::text, anchor->>'mark_id', resolved
		from comments
		where ` + commentsAnchorTarget.anchoredOn() + ` and anchor->>'mark_id' = any($2::text[])
		for share
	`
)

func lockTableAnchorRows(ctx context.Context, tx pgx.Tx, artifactID string, snapshots []tableAnchorSnapshot) error {
	byMark := make(map[string]tableAnchorSnapshot)
	for _, snapshot := range snapshots {
		for _, markID := range snapshot.markIDs {
			byMark[markID] = snapshot
		}
	}
	if len(byMark) == 0 {
		return nil
	}
	markIDs := make([]string, 0, len(byMark))
	for markID := range byMark {
		markIDs = append(markIDs, markID)
	}
	sort.Strings(markIDs)
	seen := make(map[string]struct{}, len(markIDs))
	var active []string
	rows, err := tx.Query(ctx, lockTableAskAnchorsQuery, artifactID, markIDs)
	if err != nil {
		return fmt.Errorf("lock table ask anchors: %w", err)
	}
	for rows.Next() {
		var id, markID, state string
		if err := rows.Scan(&id, &markID, &state); err != nil {
			rows.Close()
			return fmt.Errorf("scan table ask anchor: %w", err)
		}
		seen[markID] = struct{}{}
		if state == "open" {
			active = append(active, "ask "+id+" (anchor "+markID+")")
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate table ask anchors: %w", err)
	}
	rows.Close()

	rows, err = tx.Query(ctx, lockTableCommentAnchorsQuery, artifactID, markIDs)
	if err != nil {
		return fmt.Errorf("lock table comment anchors: %w", err)
	}
	for rows.Next() {
		var id, markID string
		var resolved bool
		if err := rows.Scan(&id, &markID, &resolved); err != nil {
			rows.Close()
			return fmt.Errorf("scan table comment anchor: %w", err)
		}
		seen[markID] = struct{}{}
		if !resolved {
			active = append(active, "comment "+id+" (anchor "+markID+")")
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate table comment anchors: %w", err)
	}
	rows.Close()
	for _, markID := range markIDs {
		if _, found := seen[markID]; !found {
			snapshot := byMark[markID]
			return &ErrInvalidOp{Field: "index", Reason: fmt.Sprintf("%s index %d has unindexed anchor mark: %s", snapshot.axis, snapshot.index, markID)}
		}
	}
	if len(active) == 0 {
		return nil
	}
	sort.Strings(active)
	first := byMark[markIDs[0]]
	return &ErrInvalidOp{Field: "index", Reason: fmt.Sprintf("%s index %d would remove active anchors: %s", first.axis, first.index, strings.Join(active, ", "))}
}

func (s *Service) warmLiveDocument(ctx context.Context, artifactID string) error {
	err := s.srv.Apply(ctx, artifactID, func(_ *crdt.Doc, _ func(func(*crdt.Transaction))) {})
	if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
		return err
	}
	return nil
}

// currentToken is the whole-document token of the document the caller sees, for an edit that
// applies no operation and so writes no tree of its own to take one from.
func (s *Service) currentToken(ctx context.Context, artifactID string) (string, error) {
	tree, err := s.docTree(ctx, artifactID)
	if err != nil {
		return "", err
	}
	return nodeToken(tree)
}

// ApplyOps resolves every requested operation against the document's one Yjs
// transaction. A conditional edit checks its precondition, resolves the batch,
// and writes the plan inside that transaction, so no live writer can enter the
// check-to-apply window.
func (s *Service) ApplyOps(ctx context.Context, artifactID string, ops []model.EditOp, actor model.Actor, precondition *model.EditPrecondition) (EditOutcome, error) {
	tx, joined := txFromContext(ctx)
	if !joined {
		return EditOutcome{}, errUnjoined
	}
	if precondition == nil {
		return s.applyOpsUnconditional(ctx, artifactID, ops, actor)
	}
	// The live document's locks, in their order (see liveWrite): its owner row and, unless the
	// room has failed, its writer slot; then its advisory lock, held from before the precondition
	// is read. A document no room holds is forked from the store under that lock (coldFork), which
	// loads no room while the lock is held.
	if _, err := s.joinLiveWrite(ctx, ledgerFrom(ctx), artifactID); err != nil {
		return EditOutcome{}, err
	}
	if err := lockDocumentRoom(ctx, tx, artifactID); err != nil {
		return EditOutcome{}, err
	}
	if len(ops) == 0 {
		// No operation to apply, so the document this check read is the document the caller's
		// next edit meets: its token is that edit's precondition.
		tree, err := s.docTree(ctx, artifactID)
		if err != nil {
			return EditOutcome{}, err
		}
		if err := checkEditPrecondition(tree, *precondition); err != nil {
			return EditOutcome{}, err
		}
		token, err := nodeToken(tree)
		if err != nil {
			return EditOutcome{}, err
		}
		return EditOutcome{Token: token}, nil
	}
	var (
		err       error
		outcome   EditOutcome
		snapshots []tableAnchorSnapshot
	)
	if hasTableAnchorMutation(ops) {
		snapshots, err = s.prevalidateLiveOperations(ctx, artifactID, ops)
		if err != nil {
			return EditOutcome{}, fmt.Errorf("prevalidate live document operations: %w", err)
		}
	}
	if err := lockTableAnchorRows(ctx, tx, artifactID, snapshots); err != nil {
		return EditOutcome{}, err
	}
	err = s.applyLive(ctx, artifactID, actor, func(doc *crdt.Doc, current *pmdoc.Node, transact func(func(*crdt.Transaction))) error {
		fragment := doc.GetXmlFragment(fragmentName)
		// Read before the Yjs transaction opens: the state vector takes the document lock.
		since := authoredClock(ctx, artifactID, doc)
		var mutationErr error
		transact(func(transaction *crdt.Transaction) {
			tree, err := editTree(current, func() (*pmdoc.Node, error) { return treeOfTransaction(transaction, fragment) })
			if err != nil {
				mutationErr = err
				return
			}
			if err := checkEditPrecondition(tree, *precondition); err != nil {
				mutationErr = err
				return
			}
			if len(snapshots) > 0 {
				if err := verifyTableAnchorSnapshots(tree, ops, snapshots); err != nil {
					mutationErr = err
					return
				}
			}
			// Block addressing (delete/move by id, whole-text delete) needs every block
			// identified; a browser-authored block the closer has not yet stamped gets its id
			// here, and the same ids persist through the update below.
			stamped := stampAskBlockIDs(tree)
			batch, err := applyOperations(tree, ops)
			if err != nil {
				mutationErr = err
				return
			}
			if err := requirePreconditionCoverage(tree, ops, *precondition); err != nil {
				mutationErr = err
				return
			}
			next := batch.tree
			pmdoc.EnsureBlockIDs(next)
			if err := validateEditedAskBlocks(tree, next); err != nil {
				mutationErr = &ErrInvalidAskBlock{Reason: err}
				return
			}
			if outcome, mutationErr = batch.outcome(len(ops)); mutationErr != nil {
				return
			}
			mutationErr = recordInsertedText(ctx, artifactID, "", fragment, since, batch.writes, func() error {
				if err := pmdoc.Update(transaction, fragment, next); err != nil {
					return err
				}
				s.registerStampedAskBlocks(ctx, artifactID, stamped)
				return nil
			})
		})
		if mutationErr != nil {
			return mutationErr
		}
		return nil
	})
	if err != nil {
		// A batch that resolved and wrote nothing is not a failure: it is the verdict the route
		// mints no version for.
		if errors.Is(err, websocket.ErrNoChanges) {
			return outcome, nil
		}
		if isEditRefusal(err) {
			return EditOutcome{}, err
		}
		return EditOutcome{}, fmt.Errorf("apply live document operations: %w", err)
	}
	return outcome, nil
}

// applyOpsUnconditional preserves the precondition-free edit path's existing
// validation and live-mutation behavior. Its mutate callback below reads and writes applyLive's
// own fork, never the live room directly: applyLive refuses an unjoined context (errUnjoined) and
// always hands mutate a fork, a structural guarantee rather than a rule this function keeps.
func (s *Service) applyOpsUnconditional(ctx context.Context, artifactID string, ops []model.EditOp, actor model.Actor) (EditOutcome, error) {
	if len(ops) == 0 {
		// Nothing to apply: the caller still gets the token of the document as it stands, the
		// precondition its next edit passes.
		token, err := s.currentToken(ctx, artifactID)
		if err != nil {
			return EditOutcome{}, err
		}
		return EditOutcome{Token: token}, nil
	}
	var outcome EditOutcome
	err := s.applyLive(ctx, artifactID, actor, func(doc *crdt.Doc, current *pmdoc.Node, transact func(func(*crdt.Transaction))) error {
		fragment := doc.GetXmlFragment(fragmentName)
		since := authoredClock(ctx, artifactID, doc)
		tree, err := editTree(current, func() (*pmdoc.Node, error) { return treeOf(doc) })
		if err != nil {
			return err
		}
		stamped := stampAskBlockIDs(tree)
		batch, err := s.applyOperations(ctx, artifactID, tree, ops)
		if err != nil {
			return err
		}
		next := batch.tree
		pmdoc.EnsureBlockIDs(next)
		if err := validateEditedAskBlocks(tree, next); err != nil {
			return &ErrInvalidAskBlock{Reason: err}
		}
		if outcome, err = batch.outcome(len(ops)); err != nil {
			return err
		}
		return recordInsertedText(ctx, artifactID, "", fragment, since, batch.writes, func() error {
			var updateErr error
			transact(func(transaction *crdt.Transaction) {
				if updateErr = pmdoc.Update(transaction, fragment, next); updateErr != nil {
					return
				}
				s.registerStampedAskBlocks(ctx, artifactID, stamped)
			})
			return updateErr
		})
	})
	if err != nil {
		if errors.Is(err, websocket.ErrNoChanges) {
			return outcome, nil
		}
		if isEditRefusal(err) {
			return EditOutcome{}, err
		}
		return EditOutcome{}, fmt.Errorf("apply live document operations: %w", err)
	}
	return outcome, nil
}

// startTree is the tree a mutate starts from: current, applyLive's own read of the fork, or, when
// that tree is outside the schema (nil), the fork read again, for the refusal its caller answers
// or the repair it makes.
func startTree(current *pmdoc.Node, doc *crdt.Doc) (*pmdoc.Node, error) {
	if current != nil {
		return current, nil
	}
	return treeOf(doc)
}

// editTree is the tree an edit batch starts from: current (startTree), which the batch's
// operations copy rather than change, unless a block lacks its id. The ids an edit stamps
// (stampAskBlockIDs) go on a tree read for the edit, since current is applyLive's to compare with.
func editTree(current *pmdoc.Node, read func() (*pmdoc.Node, error)) (*pmdoc.Node, error) {
	if current != nil && pmdoc.BlockIDRepairCount(current) == 0 {
		return current, nil
	}
	return read()
}

// SetBlockAttributes applies server-owned typed-block state through the
// transactional live-document mutation path. An ask's answer the document has no room for - its
// words or the options it selects (pmdoc.IsAnswerAttribute), which are caller text - is left out of
// the block: the block is written without either, saying who answered and when, and the caller's
// ask keeps the answer, as settlement leaves a returning block's answer out (withholdAnswers).
// attributes' names are the server's own, never taken from a request (pmdoc.SetBlockAttributes).
func (s *Service) SetBlockAttributes(
	ctx context.Context,
	artifactID, blockID string,
	attributes map[string]any,
	actor model.Actor,
) error {
	err := s.setBlockAttributes(ctx, artifactID, blockID, attributes, actor)
	if IsTooLarge(err) {
		if withheld, withholds := withoutAnswer(attributes); withholds {
			slog.Warn("dispatch: an answer is left out of its block", "room", artifactID, "block", blockID, "reason", err)
			// A refused write appends nothing, and the transaction's next write starts from its
			// fork as it was (applyLive).
			err = s.setBlockAttributes(ctx, artifactID, blockID, withheld, actor)
		}
	}
	// Setting attributes a block already carries writes nothing, which the live path reports as
	// ErrNoChanges; the block holds what the caller asked for, so that is success.
	if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
		return wrapUnlessTooLarge(err, "set live block attributes")
	}
	return nil
}

// withoutAnswer is attributes with each answer attribute they name (pmdoc.IsAnswerAttribute) taken
// off the block instead, and whether they name one with a value to leave out.
func withoutAnswer(attributes map[string]any) (withheld map[string]any, withholds bool) {
	withheld = make(map[string]any, len(attributes))
	for name, value := range attributes {
		if pmdoc.IsAnswerAttribute(name) {
			withholds = withholds || value != nil
			value = nil
		}
		withheld[name] = value
	}
	return withheld, withholds
}

func (s *Service) setBlockAttributes(ctx context.Context, artifactID, blockID string, attributes map[string]any, actor model.Actor) error {
	return s.applyLive(ctx, artifactID, actor, func(doc *crdt.Doc, current *pmdoc.Node, transact func(func(*crdt.Transaction))) error {
		fragment := doc.GetXmlFragment(fragmentName)
		tree, err := startTree(current, doc)
		if err != nil {
			return err
		}
		next, err := pmdoc.SetBlockAttributes(tree, blockID, pmdoc.Attrs(pmdoc.LineFeedAttrs(attributes)))
		if err != nil {
			return err
		}
		if next.EqualWithBlockIDs(tree) {
			return nil
		}
		var updateErr error
		transact(func(transaction *crdt.Transaction) {
			updateErr = pmdoc.Update(transaction, fragment, next)
		})
		if updateErr != nil {
			return updateErr
		}
		return nil
	})
}

// NamedVersion records the live document as a deliberately named immutable version. Like
// SeedText and SnapshotVersion it runs inside the caller's joined transaction, which credits
// its authors and publishes its events when it commits (Ledger.Commit).
func (s *Service) NamedVersion(ctx context.Context, artifactID, summary string, actor model.Actor) (VersionResult, error) {
	tx, joined := txFromContext(ctx)
	if !joined {
		return VersionResult{}, errUnjoined
	}
	rendered, capture, err := s.captureLiveTextAndAuthors(ctx, artifactID, &actor)
	if err != nil {
		return VersionResult{}, err
	}
	_, open, err := lockArtifactOwner(ctx, tx, artifactID)
	if err != nil {
		return VersionResult{}, err
	}
	if !open {
		return VersionResult{}, ErrIssueClosed
	}
	result, err := s.writeVersionTx(ctx, tx, artifactID, rendered, actor, &versionWrite{
		named:   true,
		summary: new(summary),
		authors: actorSlice(capture.authors),
		capture: &capture,
	})
	if err != nil {
		return VersionResult{}, err
	}
	// A person's credit in this version opens the edit notice: the owner lock this transaction
	// holds keeps the sweep's lock order (sendEditNotice).
	if err := s.recordEditNotice(ctx, tx, artifactID, result.version.Number, actorSlice(capture.authors), capture.latest); err != nil {
		return VersionResult{}, err
	}
	return VersionResult{Version: result.version, Wrote: true, Changes: result.changes}, nil
}

// serviceTransact wraps Server.Apply's transact so the room's update observer can tell the
// service's own repairs - the sweep of marks no row records - from browser peers' edits, and
// credits their changes to no one: the Apply call's origin is registered in serviceOrigins on the
// first transaction and forgotten by release. Observers fire before a transaction returns, so
// release is safe once the Apply callback is done with transact.
func (s *Service) serviceTransact(transact func(func(*crdt.Transaction))) (wrapped func(func(*crdt.Transaction)), release func()) {
	var origin any
	wrapped = func(inner func(*crdt.Transaction)) {
		transact(func(txn *crdt.Transaction) {
			if origin == nil {
				origin = txn.Origin
				s.serviceOrigins.Store(origin, struct{}{})
			}
			if s.inServiceTransaction != nil {
				s.inServiceTransaction(txn)
			}
			inner(txn)
		})
	}
	release = func() {
		if origin != nil {
			s.serviceOrigins.Delete(origin)
		}
	}
	return wrapped, release
}

// stampAskBlockIDs stamps tree's block ids (EnsureBlockIDs) and returns each ask whose id it
// renamed, pairing the tree's ask-block order before and after. It runs eagerly, since the stamp
// must happen before the edit's own operations read tree's ids, but registering the rename into
// the room's bookkeeping waits until the write that ran it is certain to land
// (registerStampedAskBlocks): an earlier-looking success here does not mean the edit's later
// operations, validation or outcome will not still fail before the stamp's rename ever reaches
// the document.
func stampAskBlockIDs(tree *pmdoc.Node) []stampedAsk {
	before := askBlockOrder(tree)
	pmdoc.EnsureBlockIDs(tree)
	if len(before) == 0 {
		return nil
	}
	return stampedAskBlocks(before, askBlockOrder(tree))
}

// registerStampedAskBlocks stages, on the write's own liveWrite (joinedLiveWrite - every caller
// is joined, applyLive refuses otherwise before the mutate closure this always runs inside ever
// starts), every id the stamp (stampAskBlockIDs) renamed - for applyLive's own before/after diff
// to exclude, via write.renamedAskBlockIDs - and the author carried forward for each one that has
// a recorded author to carry (carriedAskAuthors, the same rule a settlement's or the backfill's
// own suppressed stamp applies), via write.carriedAskAuthors. Neither is written into the room's
// own bookkeeping here: Ledger.creditRooms registers both only once the write they belong to
// actually commits, the same point it already registers write.addedAskBlockIDs, since a write
// refused after this call returns (growth, a later validation failure) must not leave the room
// holding a trace of it (LEGION-503). Call once stampAskBlockIDs has run.
func (s *Service) registerStampedAskBlocks(ctx context.Context, room string, stamped []stampedAsk) {
	if len(stamped) == 0 {
		return
	}
	write := joinedLiveWrite(ctx, room)
	renamed := make(map[string]struct{}, len(stamped))
	for _, ask := range stamped {
		renamed[ask.minted] = struct{}{}
	}
	mergeInto(&write.renamedAskBlockIDs, renamed)

	state := s.lockState(room)
	carried := state.carriedAskAuthors(stamped)
	s.unlockState(room, state)
	mergeInto(&write.carriedAskAuthors, carried)
}

// captureLiveTextAndAuthors is the tree a version records and whom it lists, its authors taken
// under the document's advisory lock and no later than it reads that tree. It takes the
// document's locks in their order (see liveWrite) before anything else: the owner row and the
// writer slot, the room loaded - unless the calling transaction has a write of its own, whose fork
// holds the document already - then the advisory lock, which every write to the pending authors
// and every in-flight credit's landing hold, so the authors it reads stay where it read them until
// the version's transaction ends. A transaction with a write of its own versions its fork, which
// joinRead brings up to date with the room: that is where a browser change made while the write
// was in flight merges with it - and where a write whose text that merge annihilated is refused
// rather than versioned as applied (refuseLostWrite, LEGION-269). Its authors are taken before
// that read, so each in-flight credit taken is for a change the room held and the fork takes in.
// Without a write of its own, a version reads the room as of one moment under its document lock
// (holdLive), after taking its authors under the room's state lock. Either way a credit observed
// after the take, for a change the version holds or not, stays pending for the next version
// (LEGION-503). The room is taken inside the Apply that loads and as docTree reads it: a room
// looked up again once that Apply returned can have been evicted in between.
//
// A version is never rendered from a room behind the durable head (refuseRoomBehindHead): with two
// envoy-dispatch processes on one Postgres, that room lacks what the other process appended, and
// the version would record that update's cursor over text missing it, so no later settlement
// would version those words (INFRA-218). A room found behind the head inside the Apply, under the
// document's advisory lock this transaction holds, is versioned from the stored document instead,
// as a write behind the head forks from it (coldFork); so is a write's fork that came from the
// store. Neither holds a change still in flight in the room, so the version lists none of the
// room's in-flight credits (withoutInflight): a credit whose append lands later is owed then, and
// one whose append the gate refuses is credited again when its browser resends the edit.
func (s *Service) captureLiveTextAndAuthors(ctx context.Context, room string, actor *model.Actor) (renderedTree, authorCapture, error) {
	tx, _ := txFromContext(ctx)
	write := joinedLiveWrite(ctx, room)
	if write == nil {
		fork, err := s.joinRead(ctx, room)
		if err != nil {
			return renderedTree{}, authorCapture{}, err
		}
		if err := s.refuseLostWrite(ctx, room, fork, actor); err != nil {
			return renderedTree{}, authorCapture{}, err
		}
	}
	// A write of the transaction's own holds the document already, in its fork; loading a room for
	// it would hold a second copy beside that fork until the write commits (LEGION-504).
	if write == nil {
		if err := s.warmLiveDocument(ctx, room); err != nil {
			return renderedTree{}, authorCapture{}, err
		}
	}
	// The warmed-read hook deliberately evicts a loaded room in tests. It runs before this
	// transaction takes the document lock: CloseRoom flushes a room's persistence, which may
	// need that lock, so invoking it from the tree read while we hold the lock would be a cycle.
	// The capture below takes the lock only after the hook returns, then reads R and F before
	// it holds the current room's tree.
	if write == nil && s.afterReadWarm != nil {
		s.afterReadWarm(room)
	}
	if err := lockDocumentRoom(ctx, tx, room); err != nil {
		return renderedTree{}, authorCapture{}, err
	}
	var tree *pmdoc.Node
	var capture authorCapture
	var err error
	if write != nil {
		if capture, err = s.capturePendingAuthors(ctx, tx, room, write, actor); err != nil {
			return renderedTree{}, authorCapture{}, err
		}
		fork, err := s.joinRead(ctx, room)
		if err != nil {
			return renderedTree{}, authorCapture{}, err
		}
		if err := s.refuseLostWrite(ctx, room, fork, actor); err != nil {
			return renderedTree{}, authorCapture{}, err
		}
		if s.afterCaptureFork != nil {
			s.afterCaptureFork(room)
		}
		if write.cold {
			capture = capture.withoutInflight(write, actor)
		}
		if rendering, reused := s.reuseRendering(write, fork, "capture"); reused {
			return rendering, capture, nil
		}
		if tree, err = s.readFork(fork); err != nil {
			return renderedTree{}, authorCapture{}, err
		}
	} else {
		// The head under the document's advisory lock, which this transaction holds from here to
		// its end: every append, this process's own included, waits for it, so no room's cursor
		// moves and the head stays where it is read.
		head, err := currentUpdateCursor(ctx, tx, room)
		if err != nil {
			return renderedTree{}, authorCapture{}, err
		}
		if capture, err = s.capturePendingAuthors(ctx, tx, room, nil, actor); err != nil {
			return renderedTree{}, authorCapture{}, err
		}
		if s.afterCaptureAuthorsTake != nil {
			s.afterCaptureAuthorsTake(room)
		}
		var readErr error
		var behind bool
		err = s.srv.Apply(ctx, room, func(live *crdt.Doc, _ func(func(*crdt.Transaction))) {
			if cursor := s.cursorOf(live); cursor != nil && refuseRoomBehindHead(room, cursor, head) != nil {
				behind = true
				return
			}
			var doc *crdt.Doc
			var release func()
			doc, release, readErr = s.holdLive(room, live)
			if readErr != nil {
				return
			}
			defer release()
			tree, readErr = treeOf(doc)
		})
		if readErr != nil {
			return renderedTree{}, authorCapture{}, readErr
		}
		if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
			return renderedTree{}, authorCapture{}, fmt.Errorf("warm live document: %w", err)
		}
		if behind {
			capture = capture.withoutInflight(nil, actor)
			if tree, err = s.storedTree(ctx, room); err != nil {
				return renderedTree{}, authorCapture{}, err
			}
		}
	}
	markdown, offsets, err := documentMarkdownWithOffsets(tree)
	if err != nil {
		return renderedTree{}, authorCapture{}, err
	}
	return renderedTree{tree: tree, markdown: markdown, offsets: offsets}, capture, nil
}

// storedTree is the tree of artifactID's stored document at its head, for a version whose room
// stands behind that head (captureLiveTextAndAuthors). It is read on the pool that owns loads
// (LoadDocument), as a cold write's fork reads the stored document (coldFork), and the caller
// holds the document's advisory lock, so no append lands between the head it compared against
// and this read. A load that fails does not fail the room, where coldFork's does: coldFork reads
// the store in place of a room, so its failure is the document's, while here the room is sound
// and only behind, and its settlement or the stale-room evictor reloads it. The version's
// transaction answers ErrServiceUnavailable instead, and its caller retries.
func (s *Service) storedTree(ctx context.Context, artifactID string) (*pmdoc.Node, error) {
	loaded, err := s.persistence.LoadDocument(ctx, artifactID)
	if err != nil {
		if errors.Is(err, ErrDocumentUnloadable) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %w", ErrServiceUnavailable, err)
	}
	doc := loaded.Doc
	if doc == nil {
		doc = newDocumentCopy()
	}
	return treeOf(doc)
}

func latestVersion(ctx context.Context, tx pgx.Tx, artifactID string) (struct {
	model.Version
	markdown         string
	docUpdateVersion int64
}, error) {
	var version struct {
		model.Version
		markdown         string
		docUpdateVersion int64
	}
	var authors []byte
	if err := tx.QueryRow(ctx, `
		select v.number, v.named, v.summary, v.authors, v.created_at, `+model.VersionSize+`, v.markdown, v.doc_update_version
		from artifact_versions v where v.artifact_id = $1
		order by v.number desc limit 1
	`, artifactID).Scan(&version.Number, &version.Named, &version.Summary, &authors, &version.CreatedAt, &version.Size, &version.markdown, &version.docUpdateVersion); err != nil {
		return version, fmt.Errorf("read latest document version: %w", err)
	}
	if err := json.Unmarshal(authors, &version.Authors); err != nil {
		return version, fmt.Errorf("decode latest document version authors: %w", err)
	}
	return version, nil
}

func contentChangedSinceVersion(ctx context.Context, tx pgx.Tx, artifactID string, cursor int64) (bool, error) {
	var changed bool
	if err := tx.QueryRow(ctx, `
		select exists(
			select 1 from doc_updates
			where artifact_id = $1 and version > $2 and content_changed
		)
	`, artifactID, cursor).Scan(&changed); err != nil {
		return false, fmt.Errorf("check content updates since version: %w", err)
	}
	return changed, nil
}

// versionWriteResult is a written version together with the counted reference targets its body
// moved: the caller that appends the version event names them, so a reader holding those
// nodes' batched backlink counts refreshes exactly their rows.
type versionWriteResult struct {
	version model.Version
	changes model.ReferenceChanges
}

// renderedTree is a document tree with its canonical markdown and each identified block's byte
// range in that markdown, all from one render (pmdoc.RenderWithBlockOffsets), so a version write
// stores the markdown and indexes its blocks and cuts its mentions' excerpts from that render
// rather than rendering the tree again for them. An upload that changes the document still renders
// it twice: ReplaceText renders the replacement to tell whether it changes anything, and applyLive
// renders the fork it leaves; the blocks and excerpts are cut from applyLive's render, which
// IndexDocumentReferences checks is the version's markdown.
type renderedTree struct {
	tree     *pmdoc.Node
	markdown string
	offsets  []pmdoc.BlockOffset
}

// writeVersionTx is the only path that changes the durable version protocol:
// version writes index references and blocks, move open approval asks to the new version, carry a
// human's approval to the version his answer to a decision block writes (carryApproval), refresh
// anchors, and retain their author capture until the enclosing transaction commits. A nil write
// records no version but keeps a transactional tree mutation's anchors in the same path.
func (s *Service) writeVersionTx(ctx context.Context, tx pgx.Tx, artifactID string, rendered renderedTree, actor model.Actor, write *versionWrite) (versionWriteResult, error) {
	markdown, tree := rendered.markdown, rendered.tree
	if write == nil {
		return versionWriteResult{}, s.refreshAnchors(ctx, tx, artifactID, tree, actor)
	}

	encodedAuthors, err := json.Marshal(write.authors)
	if err != nil {
		return versionWriteResult{}, fmt.Errorf("encode document version authors: %w", err)
	}
	var version model.Version
	var authorsRaw []byte
	if err := tx.QueryRow(ctx, `
		insert into artifact_versions as v (artifact_id, number, markdown, authors, named, summary, doc_update_version)
		select $1, coalesce(max(number), 0) + 1, $2, $3, $4, $5,
			coalesce($6, (select max(version) from doc_updates where artifact_id = $1), 0)
		from artifact_versions where artifact_id = $1
		returning number, named, summary, authors, created_at, `+model.VersionSize+`
	`, artifactID, markdown, encodedAuthors, write.named, write.summary, write.docUpdateVersion).Scan(
		&version.Number, &version.Named, &version.Summary, &authorsRaw, &version.CreatedAt, &version.Size,
	); err != nil {
		return versionWriteResult{}, fmt.Errorf("write document version: %w", err)
	}
	// The version's tree is the one the issue's task counts are read from, so they are written
	// with it (LEGION-542).
	if err := RecordTaskProgress(ctx, tx, artifactID, tree); err != nil {
		return versionWriteResult{}, err
	}
	if err := json.Unmarshal(authorsRaw, &version.Authors); err != nil {
		return versionWriteResult{}, fmt.Errorf("decode document version authors: %w", err)
	}
	// The version's references, its mentions' excerpts and its blocks are indexed from the render
	// this version's markdown came from, in its own transaction, so GET /api/v1/references serves
	// them without rendering the document.
	changes, err := s.indexDocument(ctx, tx, artifactID, version.Number, version.CreatedAt, markdown, rendered)
	if err != nil {
		return versionWriteResult{}, err
	}
	// The open approval request follows the document version without leaving its thread or opening
	// another Inbox row. The request remains waiting on its agent until it is handed back.
	moved, err := MoveApprovalAsk(ctx, tx, s.events, artifactID, version, actor, s.serverURL)
	if err != nil {
		return versionWriteResult{}, err
	}
	for _, event := range moved {
		collectEvent(ctx, event)
	}
	// A human's answer to a decision block carries his approval to the version it writes, when
	// that answer is the whole change since the version he approved. It runs after the move, so
	// an open request already names this version when the carry answers it in his name.
	carried, err := s.carryApproval(ctx, tx, artifactID, version, rendered)
	if err != nil {
		return versionWriteResult{}, err
	}
	for _, event := range carried {
		collectEvent(ctx, event)
	}
	// The transaction's own live operation already refreshed this tree's anchors
	// (recordLiveOperation); refreshing the same tree twice reads and re-derives every open anchor
	// for no change.
	// Neither call goes: this is the only refresh a version written without a live write of its
	// own gets - settlement and a standalone named version - and the write == nil arm above is
	// the only one a mark-only write, which writes no version, gets at all.
	if live := joinedLiveWrite(ctx, artifactID); live == nil || live.anchorsTree != tree {
		if err := s.refreshAnchors(ctx, tx, artifactID, tree, actor); err != nil {
			return versionWriteResult{}, err
		}
	}
	if write.capture != nil {
		if ledger := ledgerFrom(ctx); ledger != nil && ledger.tx == tx {
			ledger.recordVersion(artifactID, version, *write.capture)
		}
	}
	return versionWriteResult{version: version, changes: changes}, nil
}

func actorKey(actor model.Actor) string {
	return actor.Kind + "\x00" + actor.ID
}

func actorSlice(actors map[string]model.Actor) []model.Actor {
	keys := make([]string, 0, len(actors))
	for key := range actors {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]model.Actor, 0, len(keys))
	for _, key := range keys {
		result = append(result, actors[key])
	}
	return result
}
