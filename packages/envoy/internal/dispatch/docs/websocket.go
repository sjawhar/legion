package docs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// connectionState is one document socket as its admission (ServeHTTP) found it and ygo's
// authorization of the same request (authorize) registered it: the caller, resolved once
// (requestActor), and the admission's one read of the document's issue, which authorize takes
// rather than resolving or reading either again; then the room, the connection id the service gave
// the browser, and whether authorize registered it, which tells the socket's end whether its place
// under the connection cap is the connection's count to forget (removeConnection) or a reservation
// to give back (releaseConnection). What else a socket's admission reads off its request goes here
// too.
type connectionState struct {
	room  string
	id    uint64
	actor model.Actor
	issue issueRead
	added bool
}

type connectionContextKey struct{}

type ownerVerifiedContextKey struct{}

// ownerVerifiedToken is installed only on the server's own calls, by a caller that already
// knows the document's owner state: a settlement, which holds the owner row locked in its
// transaction; the publish of a committed live write, whose transaction held that row until it
// committed; the block-id backfill, which is a command run against a database nobody is
// serving from; or one of a comment's anchor operations under WithCommentMarks, whose caller
// holds the owner row locked and decided under it that the comment is written. Their injections
// skip the issue read in allowInject - for the settlement that read would be a second pooled
// connection taken while its transaction is open (store.ErrNestedAcquire), for the publish a
// read other writers' connections can starve (see publishLiveUpdate), for the backfill a
// question already answered, and for a comment's anchor a refusal its closed issue does not make.
type ownerVerifiedToken struct{ _ byte }

func withOwnerVerified(ctx context.Context) context.Context {
	return context.WithValue(ctx, ownerVerifiedContextKey{}, &ownerVerifiedToken{})
}

func isOwnerVerified(ctx context.Context) bool {
	_, ok := ctx.Value(ownerVerifiedContextKey{}).(*ownerVerifiedToken)
	return ok
}

type commentMarksContextKey struct{}

// WithCommentMarks marks ctx as a comment's writes to the document its anchor is in, made by a
// caller holding the document's owner row locked in its joined transaction that decided there
// the comment is written. A closed issue still takes a comment, so its document still takes
// that comment's anchor: the comment mark MarkQuote places and VerifyMark reads, the version
// SnapshotVersion pins the anchor to, the margin record ProjectMark writes, and the
// SuggestionKind read of the thread root a reply projects. Each of those, and only those, runs
// owner-verified under this context (commentMarkOperation). A suggestion's mark, a content edit,
// a named version or a rebuild made under the same context is still refused on a closed issue.
func WithCommentMarks(ctx context.Context) context.Context {
	return context.WithValue(ctx, commentMarksContextKey{}, true)
}

// commentMarkOperation is ctx for one of the comment anchor operations WithCommentMarks names:
// owner-verified when the caller marked ctx, unchanged otherwise.
func commentMarkOperation(ctx context.Context) context.Context {
	if commentMarks(ctx) {
		return withOwnerVerified(ctx)
	}
	return ctx
}

// commentMarks is whether the caller marked ctx as a comment's writes to a document
// (WithCommentMarks). Only commentMarkOperation reads it: what every other caller asks is
// commentMarkAnchor, which is this inside the operation that marked itself owner-verified.
func commentMarks(ctx context.Context) bool {
	marked, _ := ctx.Value(commentMarksContextKey{}).(bool)
	return marked
}

// commentMarkAnchor is whether ctx is one of the comment anchor operations WithCommentMarks
// names, inside the operation that made it owner-verified (commentMarkOperation): the reads and
// writes a closed issue's document takes for a comment, and no other caller's. It is the one
// test of that, which the three decisions a closed issue turns share - the fork a comment's mark
// is written on (closedIssueCommentMark), the tree its mark_id is verified against and the
// version its anchor is pinned to (closedIssueCommentAnchor).
func commentMarkAnchor(ctx context.Context) bool {
	return commentMarks(ctx) && isOwnerVerified(ctx)
}

// closedIssueCommentAnchor is whether ctx is a comment's anchor operation (commentMarkAnchor) on
// a document whose issue is closed, read from q - the caller's transaction where it is inside
// one (queryFrom). It answers false for every other caller without reading anything, so no
// version and no mark read outside a comment's anchor pays for it.
//
// A closed issue's room holds updates the store will never take: on a process that has not heard
// the close its browsers are still writable, and the gate refuses a browser update the room took
// more than closeMargin after the close (gateAppend, appendBrowser). So the comment's anchor
// reads and writes the stored document alone - the fork its mark is written on (readFork), the
// tree its mark_id is verified against (VerifyMark) and the version that anchor is pinned to
// (captureLiveTextAndAuthors) - and all three see the same document. A mark only the room holds
// is then no anchor: VerifyMark answers ErrAnchorMissing, the API answers 409 ANCHOR_MISSING,
// and the browser's margin writes the mark again and sends the comment anew (margin-context.tsx,
// reanchor), by quote once the refused append has closed the room for its issue and its browsers
// have reconnected read-only (ProofDocument.tsx, the quote-comment action).
func (s *Service) closedIssueCommentAnchor(ctx context.Context, q Queryer, artifactID string) (bool, error) {
	if !commentMarkAnchor(ctx) {
		return false, nil
	}
	open, err := s.issueOpen(ctx, q, artifactID)
	if err != nil {
		return false, err
	}
	return !open, nil
}

type shutdownLoadContextKey struct{}

// withShutdownLoad marks the one injection allowInject lets through into a room Shutdown has
// begun closing: the empty warm-up with which Shutdown's own settlement loads a document whose
// settlement this process had armed with no room resident (a cold write's, publishLiveWrite), as
// the armed timer would have loaded it had it fired. It loads the room and writes nothing; a write
// that settlement then has to make into the room carries no mark and is refused like any other.
func withShutdownLoad(ctx context.Context) context.Context {
	return context.WithValue(ctx, shutdownLoadContextKey{}, true)
}

func isShutdownLoad(ctx context.Context) bool {
	load, _ := ctx.Value(shutdownLoadContextKey{}).(bool)
	return load
}

// servicePersistenceAdapter observes ygo's otherwise asynchronous persistence
// callbacks. A failed update evicts its room so the next access reloads durable
// state instead of retaining unsaved live state.
type servicePersistenceAdapter struct {
	store   VersionedStore
	service *Service
}

// LoadDoc reads the durable state a room loads and hands ygo none of it: the state goes on the
// load's record (roomLoad.read), and the room's own load hook decodes it (bootstrapLoadedState,
// whose comment states how), because ygo's own apply runs the format cleanup a remote apply runs
// and the room's stored history already holds every cleanup the room made. A preload
// (takePreload), which the socket's admission check decoded, goes on the record as a read would.
//
// The record also carries the history's size and what a socket's admission made of that history
// when the load takes that socket's preload: the tree it read from it and the rendering it made of
// that tree (roomLoad.read, takeLoaded, admissionRendering).
//
// It records the load before it reads, and what it read after: the room's load (onLoadDocument)
// takes the record next on the same goroutine (ygo's loadRoom) to start the instance's roomHold
// and its cursor, and a version this process appends between the two reaches that instance through
// the record (roomLoad) rather than no instance at all. The read itself takes no advisory lock - a
// durable writer holds that one across its whole transaction, and a load that waited for it would
// wedge a write that forks the room it loads - so the record, not the lock, is what keeps a room
// from being born behind the head.
//
// Nothing of ygo's own runs between this read and the hook it calls next: there is no decode of
// what this returns (sjawhar/ygo v1.55.1-sami.2, provider/websocket/server.go:2013-2018), and the
// only way a load fails with nothing of this service still to run on its goroutine is a panic out
// of this read. Every way the read ends short is therefore failed here, on the load's own
// goroutine and before ygo closes the room's barrier: the store's refusal, a stored history whose
// header declares more items than one update may hold (loadableHistory), and a panic out of any
// of it, recovered here as the hook recovers its own (bootstrapLoadedState, foldDocument). ygo
// recovers such a panic too and hands every waiter the load's error (server.go:2004-2023), but it
// does so with the record this load began still standing under the room's name and the room
// healthy, which left the failure to whichever caller's Apply met it - a caller that cannot tell
// the load it waited on from the load that began after it, since ygo removes the room before any
// waiter wakes (server.go:2025-2034). Recovered here, a load's failure is the load's own
// (sjawhar/legion#2519).
func (a *servicePersistenceAdapter) LoadDoc(room string) (_ []byte, err error) {
	load := a.service.beginRoomLoad(room)
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("panic: %v", recovered)
		}
		if err != nil {
			a.service.failRoomLoad(room, load, err)
		}
	}()
	if loaded, rendered, ok := a.service.takePreload(room); ok {
		load.read(loaded.Version, loaded.Update, rendered)
		return nil, nil
	}
	result, err := a.store.Load(context.Background(), room)
	if err == nil {
		err = loadableHistory(result.Update)
	}
	if err != nil {
		return nil, err
	}
	load.read(result.Version, result.Update, admissionRendering{})
	return nil, nil
}

func (a *servicePersistenceAdapter) StoreUpdate(room string, update []byte) error {
	return a.StoreUpdateContext(context.Background(), room, update)
}

// StoreUpdateContext stores a room's update, which ygo's persistence worker hands on one call per
// update, in order: it appends, in one transaction under the document's advisory lock, every
// update of the same room instance its update observer recorded before this one and no writer has
// stored yet, this one, and the run of updates the room recorded after it that its batch takes
// (roomBatchThrough), in the room's commit order, each with the in-flight credit and the class its
// observer recorded (AppendRoomFlush, flushRun). So the order ygo's persistence hands updates on
// in never decides the order they are stored in, and an update another writer stored first is
// skipped. It returns nil only once the update is durable, by its own transaction or by one that
// stored it first, so ygo's flush still means every update it held is stored.
//
// Unless something waits on the room, the call lingers for the room's queue to fill before its
// batch goes, at most roomAppendLinger from the update's recording (room_appends.go).
//
// Each append carries what the observer recorded with its updates: the room instance they were
// made in (documentUpdateClass.cursor) and when the room took each (documentUpdateClass.observed),
// which the store passes to the gate as a browser's append (BrowserUpdate, appendBrowser). So the
// gate admits them from a room behind the head with its history intact - they land at the head,
// and the instance's cursor folds them in once catch-up fills the gap - and refuses them only when
// the room's stored history was replaced under it, or when the document's issue is closed and the
// room took the update later than closeMargin after the close. A history a compaction folded past
// the room's cursor is no replacement for the run when it still holds every item the run builds
// on (storedHistoryHolds): it is stored, and the room, which cannot catch up to that history, is
// closed and reloaded at the head once it is (closeRoomOnReplacedHistory): this append asks for
// that itself, so it needs no room-sync lane and waits on no later notification. An update the
// room took before the close and whose append reaches the gate after it - queued behind the
// room's earlier appends, waiting on the document's lock behind a settlement or a compaction, or
// handed here by the close of the room itself (setRoomIssueOpen) - is stored, and settled once
// the issue reopens, a compaction's fold of the history meanwhile included: a room closing for
// its issue takes in no catch-up while its close waits for this append (holdOpen), and that close
// is the reload a folded history needs.
//
// A replaced history fails the room, as any failed append does: the room reloads from the store,
// which is the close-and-reload a replaced history needs (plan D3.3), and the browser resends the
// update to the reloaded room. A refused update on a closed issue is dropped and closes the room
// for its issue (refuseClosedIssueAppend), so its browsers reconnect read-only. Either is decided
// under the document's advisory lock, so it is never this process's own write in flight, and
// needs no room-sync lane to run.
//
// An update that never reaches the store - its room failed, or a live write, a settlement or a
// catch-up already made it durable - lands its credit here instead: the room drops it with the
// update, which the browser resends. Whichever way the append ends, the update leaves its
// instance's unstored updates once it has.
func (a *servicePersistenceAdapter) StoreUpdateContext(ctx context.Context, room string, update []byte) error {
	if a.service.roomFailed(room) {
		// The failed room's update is dropped and its browser resends it to the room that
		// replaces this one, but its class is taken all the same, so the work the record counts
		// for it ends here (dropUpdate).
		a.dropUpdate(room, update)
		return nil
	}
	class, counted, found := a.service.consumeUpdateClass(room, update)
	if found {
		defer a.service.finishUpdateClass(room, counted, class)
	}
	defer class.instance.unlistUnstored(class)
	defer class.credit.landed()
	// The update's suppression slot is consumed whatever its class says, since slots are taken in
	// order. The class decides when the observer recorded one: a published write's and a
	// repair's are queued stored, and a browser update whose bytes match a repair's - a deletion
	// leaves an update that carries no struct, only its own deletes, so a browser's deletion of
	// the items a repair deleted leaves the repair's bytes - takes that repair's slot while its own
	// class is not stored, and is stored all the same; the repair's own update then finds its
	// class stored.
	suppressed := a.service.consumeSuppressedPersistence(room, update)
	if class.stored.Load() || (suppressed && !found) || a.service.roomFailed(room) {
		return nil
	}
	a.service.hooks.awaitPersistenceHold(ctx, room)
	if !found {
		// An update no class was recorded for is stored on its own: the room's queue holds
		// nothing to flush it with, and the gate reads the cursor marked above.
		updates := []BrowserUpdate{{
			Update:         update,
			ContentChanged: class.contentChanged,
			Credit:         class.credit,
			Observed:       class.observed,
		}}
		if _, err := a.store.AppendUpdatesWithCredit(withRoomCursor(ctx, class.cursor), room, updates); err != nil {
			if errors.Is(err, ErrIssueClosed) {
				a.service.refuseClosedIssueAppend(room)
				return nil
			}
			a.service.failRoom(room, err)
			return err
		}
		return nil
	}
	// The call's own update must read back into a fresh copy before its batch is stored, as an
	// append of updates the store is handed checks (AppendUpdatesWithCredit): one the room took
	// but a copy cannot decode fails the room, which reloads the document from the store. Only
	// this update is read back here. The batch's other updates are stored as the room applied
	// them, decoded by the room when it took them and not again, as a joined transaction's flush
	// of the room stores them (AppendRoomUpdatesTx); a later call of an update the batch stored
	// finds its class stored and decodes nothing.
	if err := crdt.ApplyUpdateV1(newDocumentCopy(), update, nil); err != nil {
		a.service.failRoom(room, err)
		return err
	}
	through, wake, wait := a.service.roomBatchThrough(room, class, true)
	if wake != nil {
		timer := time.NewTimer(wait)
		select {
		case <-wake:
		case <-timer.C:
		}
		timer.Stop()
		// Another writer may have stored the update while it lingered (a settlement, a live
		// write, a peer's batch): there is nothing left for this call to store, and its batch
		// must not meet the gate for it, which refuses a room behind the head.
		if a.service.roomFailed(room) || class.stored.Load() {
			return nil
		}
		through, _, _ = a.service.roomBatchThrough(room, class, false)
	}
	flush := &RoomFlush{service: a.service, room: room, cursor: class.cursor}
	if _, err := a.store.AppendRoomFlush(ctx, room, flush, through, class.credit); err != nil {
		if errors.Is(err, ErrIssueClosed) {
			a.service.refuseClosedIssueAppend(room)
			return nil
		}
		a.service.failRoom(room, err)
		return err
	}
	if flush.storedContentChange() {
		a.service.scheduleSettleAfterAppend(room)
	}
	if class.cursor.takeReplaced() {
		a.service.closeRoomOnReplacedHistory(room, class.instance)
	}
	return nil
}

// Compact is ygo's room-close compaction: its persistence worker calls it as it exits, the server's
// shutdown included, and it runs the tail rule (compactRoom) inline on that worker. roomServer
// keeps that exit from racing a repair, but intentionally does not gate a published live write: it
// is already durable, and its room can still retire under publishLiveUpdate's Apply. Compaction is
// housekeeping, so the worker leaves a room whose lock another holder has to a later compaction
// rather than waiting behind that holder. A failed room's eviction is the exception: it waits for
// the lock and compacts before recovery, and with nothing to fold it still waits for the lock
// (passRoomLock), so a repair written into the failed room commits before the room reloads.
func (a *servicePersistenceAdapter) Compact(ctx context.Context, room string) error {
	// A failed room's eviction waits for the document's lock, so a repair written into it commits
	// before the room reloads; the hook counts those waiting compactions.
	failed := a.service.roomFailed(room)
	if failed {
		a.service.hooks.countLockWaitingCompaction(room)
	}
	_, err := a.store.compactRoom(ctx, room, tailRule, failed)
	return err
}

// failRoomLoad is one room load's failure: the room fails, so the next access to it reloads the
// document rather than meeting the half-loaded one (failRoom), and the record the load began goes
// with it. No instance will take that record (keepInstance), and nothing else takes it out: it is
// the document the load never made that releases it (roomLoads).
//
// The record goes first, and what it carries goes with it: the stored state the load read, which
// no document of this process will ever apply (roomLoad.state), and the versions this process
// appended while the load ran, which the room that loads next reads from the store instead
// (took). One other holder reads that record while a room loads: a cold write's shadow names the
// load when its commit found the room still loading, and measures the instance that load leaves
// behind (installShadowLocked, roomShadow.cursorLocked). Dropping the record before the room
// fails is what decides that case - a write committing in between installs no shadow at all
// rather than one naming a load that will leave no instance, and a shadow installed before that
// is freed by the failure on the next line, since a failed room measures no class of its own
// again (failRoomLocked, handOverFailedRoomLocked). So no shadow is left waiting on an instance
// this load will never leave (sjawhar/legion#2519).
func (s *Service) failRoomLoad(room string, load *roomLoad, cause error) {
	s.roomLoads.CompareAndDelete(room, load)
	s.failRoom(room, cause)
}

// validateUpdate decodes a stored history, or an update about to replace one, as the room will
// decode it: into a document with maxPending parked items, the caller's own pending queue
// (Service.maxPendingItems for a rebuild's check of the history its rooms would load,
// PgVersioned.maxPendingItems for the store's check of a rebuilt seed; see maxUpdateItems for the
// rule both follow), which is the cap a room's load hook decodes its stored state under
// (bootstrapLoadedState), so a history a room served is never judged one that cannot load. A
// rebuild runs it on its seed and on the history it replaces; a room's load does not, as its own
// decode is the check.
func validateUpdate(update []byte, maxPending int) error {
	if len(update) == 0 {
		return nil
	}
	return decodeStoredState(newCappedDocumentCopy(maxPending), update)
}

// decodeStoredState applies state, a stored history, one of its rows or the state a room or cold
// read loads, to doc, and is the one way the service decodes stored state, so every refusal of it
// reads the same. ygo's apply names a failure as ErrInvalidUpdate followed by the decoder's reason
// ("crdt: invalid update: encoding: unexpected end of input").
func decodeStoredState(doc *crdt.Doc, state []byte) error {
	return crdt.ApplyUpdateV1(doc, state, nil)
}

// bootstrapLoadedState decodes state, what room's load read (takeLoadedState), into doc, the
// room's document. It is the one decode of the stored state a room's load makes: ygo is handed no
// bytes (servicePersistenceAdapter.LoadDoc), so it decodes under the pending cap ygo set on doc
// before its load hook ran, the server's MaxPendingItems, which New sets from the service's own
// cap (Service.maxPendingItems; see maxUpdateItems), and through decodeStoredState, so a refusal
// reads as every other decode of stored state does. An empty state - an empty history - decodes
// nothing.
//
// It decodes with ygo's format cleanup off (crdt.Doc.SetFormatCleanup), which would follow an
// apply that touches formatted text with a commit of the room's own: that commit reaches no
// persistence (ygo registers it after the load), and the stored history already holds every
// cleanup the room made (newCappedDocumentCopy), so the reloaded room would hold a state the room
// before it never held. The cleanup is set back on once the apply has returned, before any peer or
// write reaches the room, so the room cleans up after a browser's update as Yjs does; the setter
// does not cancel a cleanup an earlier commit armed, so it is off before the apply starts, and it
// is never called inside a transaction, which holds the document's lock. The apply runs inside
// ygo's OnLoadDocument hook, which logs a panic and goes on as if the hook succeeded, so a panic
// is returned as the decode's error, which fails the room as a refusal does.
func (s *Service) bootstrapLoadedState(room string, doc *crdt.Doc, state []byte) (err error) {
	if len(state) == 0 {
		return nil
	}
	if s.bootstrapping != nil {
		s.bootstrapping(room, state)
	}
	doc.SetFormatCleanup(false)
	defer doc.SetFormatCleanup(true)
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("decode room %q: panic: %v", room, recovered)
		}
	}()
	return decodeStoredState(doc, state)
}

// loadableHistory is the refusal of a stored history whose header declares more items than one
// document update may hold (itemCapRefusal), which ygo refuses before it integrates any of it, so
// the failure names the room before the room's own load decodes anything. It decodes nothing: the
// room's own load decodes these same bytes into that room's document, in the hook ygo calls as
// soon as this read returns (onLoadDocument, bootstrapLoadedState), and a history that does not
// decode fails the room there. Decoding them here as well cost a second decode of every room load
// - on a history one client wrote, which is every document the server seeded, 65 ms and 36 MB of
// each cold settlement of an 8,000-item document (sjawhar/legion#2519), and on a history of many
// writers, the costliest decode there is, all of it again. A history of several clients declares
// no single count (updateItems), so the room's decode alone judges it.
func loadableHistory(update []byte) error {
	if items, named := updateItems(update); named && items > maxUpdateItems {
		return itemCapRefusal(items, maxUpdateItems)
	}
	return nil
}

// refuseIfRebuilding refuses a room's load or injection while a rebuild's transaction holds it.
func (s *Service) refuseIfRebuilding(room string) error {
	if _, rebuilding := s.rebuilding.Load(room); rebuilding {
		return fmt.Errorf("%w: the document is being rebuilt; retry", ErrServiceUnavailable)
	}
	return nil
}

// allowInject decides whether ygo may apply an injection to a room. Its issue read goes
// through the shared pool for a caller that need hold no connection of its own (the
// settlement warm-up in settleRoom), and that is outside the pool's deadlock cycle only
// because ygo runs OnInject before getOrCreateRoom (sjawhar/ygo v1.55.1-sami.2,
// provider/websocket/inject.go:319-328): an injection refused here has published no room
// placeholder for a connection-holder to park on, so nothing holding a connection is waiting
// on this read. A vendored reordering of those two calls puts it back in the cycle.
func (s *Service) allowInject(ctx context.Context, info websocket.InjectInfo) error {
	if err := s.refuseIfRebuilding(info.Room); err != nil {
		return err
	}
	if s.shuttingDown(info.Room) && !isShutdownLoad(ctx) {
		return ErrServiceUnavailable
	}
	if err := s.awaitRoomRecovery(ctx, info.Room); err != nil {
		return err
	}
	if isOwnerVerified(ctx) {
		return nil
	}
	if s.roomClosed(info.Room) {
		return ErrIssueClosed
	}
	open, err := s.issueOpen(ctx, s.queryFrom(ctx), info.Room)
	if err != nil {
		return err
	}
	if !open {
		return ErrIssueClosed
	}
	return nil
}

// onLoadDocument is the room's load hook, which ygo runs once it has made room's document and set
// its pending cap, and before it hands the document to anyone. It decodes the state the room's
// load read (servicePersistenceAdapter.LoadDoc) into doc (bootstrapLoadedState), and a state that
// does not decode fails the room and drops the load's record (failRoomLoad) with the decode's
// error, which it returns to ygo, so the room is not published and its next access loads it
// afresh. Then it records what the room starts from: its issue's state, the settlement it owes,
// its ask blocks, its replica and its instance, and the update observer that classifies and
// credits every update the room commits.
func (s *Service) onLoadDocument(ctx context.Context, room string, doc *crdt.Doc) (err error) {
	// A refusal of this hook is, but for one, this service's own answer to the load - the
	// document is being rebuilt, the room has already failed, the store did not answer - and not
	// one to fail the room for a second time. The load it refuses is failed here, where that
	// refusal is made, by dropping the record the load began: no instance will take it, and ygo
	// removes the room and answers every caller with this error, so the room's name is left
	// holding nothing of this load. The one that is not ours is the stored history this load
	// read, which is decoded here (bootstrapLoadedState): a history the room's document cannot take is
	// the load's own failure, and it fails the room so the next access reloads rather than meeting
	// a half-loaded document. ygo loads one room at a time under a name - the goroutine that
	// created the placeholder owns the load and every other caller for that name waits on its
	// ready barrier (sjawhar/ygo v1.55.1-sami.2, provider/websocket/server.go:1744-1764) - and
	// this runs on that load's own goroutine, so the record read here is this load's own
	// (sjawhar/legion#2519).
	load, held := s.roomLoads.Load(room)
	defer func() {
		if err != nil && held {
			s.roomLoads.CompareAndDelete(room, load)
		}
	}()
	// The state is taken off the load's record first, so a load refused below leaves none held.
	stored := s.takeLoadedState(room)
	// A load never waits for its own room's recovery. That recovery's eviction waits in ygo's
	// CloseRoom for the ready barrier this load holds (sjawhar/ygo v1.55.1-sami.2,
	// provider/websocket/inject.go:587-599) and closes the channel awaitRoomRecovery waits on
	// only once CloseRoom has returned (failRoomLocked), so a wait here is a cycle: the load
	// holds the eviction, and the eviction holds the load. No deadline breaks it either -
	// both loaders that reach this one carry context.Background(), the settlement warm-up
	// (settleRoom) and a committed write's publish (publishLiveUpdate) - so the room would stay
	// failed until the process restarted (LEGION-282). A failed room refuses the load
	// instead: ygo fails the load, closes the barrier with this error and removes the room,
	// which lets the eviction finish, and the next access loads the replacement.
	if err := s.refuseIfRebuilding(room); err != nil {
		return err
	}
	if err := s.roomFailure(room); err != nil {
		return err
	}
	// The room counts among the resident rooms, and holds its place under the room cap
	// (reserveRoom), from here until ygo's OnUnloadDocument (releaseUnloadedRoom), unless this
	// load fails: ygo then drops the room with no OnUnloadDocument, so the load takes its own count
	// back. The token the count answers with is this load's own: the bytes counted against it are
	// taken back when an unload of the name ends it, or by this load's own failure (residentLoad).
	counted := s.roomLoaded(room)
	resident := false
	defer func() {
		if !resident {
			s.roomUnloaded(room, counted)
		}
	}()
	// Decoded before anything reads or observes the document (bootstrapLoadedState).
	if err := s.bootstrapLoadedState(room, doc, stored); err != nil {
		// The load's own failure, not this service's refusal of it: these are the bytes the
		// store held for this document, and no room can serve them. The room fails, so the
		// reader who comes next reloads it (failRoomLoad, which the defer above then finds
		// nothing left to drop).
		if held {
			s.failRoomLoad(room, load.(*roomLoad), err)
		} else {
			s.failRoom(room, err)
		}
		return err
	}
	// Everything this load needs comes from the pool that owns loads. A writer holding a
	// pooled connection and the issue's row lock waits for this load, so a read of the shared
	// pool here waits for a connection only that writer can release: the wedge, arrived at
	// without a transaction of its own and so invisible to the pool's guard.
	rooms, err := s.store.Pool.Rooms()
	if err != nil {
		return err
	}
	// The room's browser updates are stored on the room-appends pool, which opens a connection
	// beside this load when it holds none, so the room's first batch dials none (warmRoomAppends).
	s.warmRoomAppends()
	// The issue is read once per room load. A cold socket's admission, which holds the room's state
	// while its load runs, has read it and recorded it there, as has a close or reopen of the issue
	// since (admittedIssueRead); a load no admission holds - a settlement's warm-up, a committed
	// write's publish - reads it here.
	issue, admitted := s.admittedIssueRead(room)
	if !admitted {
		if issue, err = s.readIssue(ctx, rooms, room); err != nil {
			return err
		}
	}
	// The load takes no advisory lock, which a durable writer can hold for as long as its
	// transaction runs: the row it reads is written whole by each writer's transaction, so a
	// plain read sees one writer's commit or the one before it. What the record has recorded by
	// then is taken first, so an edit source recorded after the read is told from one before it.
	mark := s.markEditSource(room)
	owed, lastActor, err := readOwedSettlement(ctx, rooms, room)
	if err != nil {
		return err
	}
	// The room is still loading: ygo hands its document to no peer or caller until this hook
	// returns (sjawhar/ygo v1.55.1-sami.2, provider/websocket/server.go:1903-1994: loadRoom closes
	// the room's ready barrier after it), so nothing writes the tree while this walks it. Its
	// rendering, with the layout recorded with it, goes to the room's replica (keepReplica, below),
	// whose first run takes it as its base's: nothing from here to OpenUpdateLog hands the document
	// to anything, so the state rendered here is the one the log opens on
	// (renderedReplica.start). This load holds it only until it hands it over. A load that took a
	// cold socket's preload takes the tree and the rendering that socket's admission made of the
	// same state, found inside the schema (roomLoad.takeLoaded), so the document is rendered once.
	var size int
	var rendered admissionRendering
	if value, ok := s.roomLoads.Load(room); ok {
		size, rendered = value.(*roomLoad).takeLoaded()
	}
	var renderErr error
	if rendered.tree == nil || rendered.rendering == nil {
		rendered.tree, renderErr = treeOf(doc)
		if renderErr == nil {
			if s.admissionRendered != nil {
				s.admissionRendered(room)
			}
			rendered.rendering, renderErr = replicaRenderingOf(rendered.tree)
		}
	}
	var askBlocks map[string]struct{}
	switch {
	case errors.Is(renderErr, ErrDocOutsideSchema):
		slog.Warn("dispatch: loaded document outside Proof schema; a replacement from markdown repairs it", "room", room, "error", renderErr)
	case renderErr != nil:
		return renderErr
	default:
		askBlocks = askBlockIDs(rendered.tree)
	}
	// The history this load decoded counts as resident against this load's own token, so the
	// unload that ends the token takes those bytes back wherever the room's record has gone by then
	// (countResidentBytes).
	s.countResidentBytes(counted, int64(size))
	// The room this load publishes holds the state it takes from here until ygo retires it
	// (releaseIfUnusedLocked). The issue's state is the newest the record holds, this load's read
	// or a close recorded after it began.
	state := s.lockState(room)
	state.recordIssueLocked(issue)
	open := !state.closed
	// The room has counted itself loaded here since roomLoaded, above, and stays counted until its
	// unload (roomLoadedHere), so a socket's admission offers it no preload from then on
	// (offerPreload). An offer that checked before that count and has stored its preload is under
	// the room's name now, and it is not this load's - LoadDoc's takePreload ran before any of this
	// - so it is one no load of this room will ever use, and it is taken out here, under the room's
	// record that the offer stored it under, rather than left for the socket that offered it to
	// withdraw hours later.
	s.preloads.Delete(room)
	// The ask blocks the room loaded with are the baseline its update observer tells new ones by.
	// They were not introduced by any update the observer sees, so none gains an author here: a
	// settlement finds the author of one no ask indexes yet where its writer recorded it, in
	// doc_ask_block_authors (askBlockSources.author).
	if askBlocks == nil {
		state.askBlocks = nil
	} else {
		state.observeAskBlocks(askBlocks, nil)
	}
	// The document owes a settlement no settlement committed: one a shutdown's budget cut short,
	// or one a room failure dropped (failRoomLocked). Its timer lived in the process or the room
	// that is gone, so an open room settles once rather than waiting for an edit to arm one - unless
	// the load is a settlement's own warm-up, which settles it next. Its authors are in the
	// document's pending authors, which that settlement reads; the room restores only the latest
	// edit source the settlement names. A closed room keeps the row; it settles only after its
	// issue reopens. A room that failed again while this load ran leaves the row for its own
	// replacement.
	//
	// The row is that source as the store has it, so it replaces whatever the record held for a
	// room that has gone (a forced close keeps the record, evictRoom): a row naming none - an edit
	// any of several connected browsers could have made - leaves the settlement its own actor
	// rather than the last source an earlier room saw, and no row leaves no source, as on a fresh
	// record. The record keeps its own when the row may be older than it (rowIsCurrent).
	switch {
	case !open || !mark.rowIsCurrent(state):
	case owed && lastActor != nil:
		state.lastActor = lastActor
		state.unsettled = true
	default:
		state.lastActor = nil
		state.unsettled = false
	}
	if state.failed == nil && open && owed && !state.settleWarming {
		s.scheduleSettleLocked(room, state)
	}
	s.unlockState(room, state)
	// The log is opened before the observer is registered and before ygo hands the room to anyone
	// (above), so it holds every transaction the room commits from here on, in commit order. The
	// replica's verdict starts as this load's rendering said, and the observer's renders move it
	// on from the first update (renderedReplica.verdict).
	replica := s.keepReplica(doc, doc.OpenUpdateLog(), rendered.rendering)
	replica.recordVerdict(renderErr)
	instance := s.keepInstance(room, doc, counted)
	replica.classify = func(logged crdt.LoggedUpdate, seq uint64, contentChanged bool, asks func() map[string]struct{}) (bool, bool, bool) {
		return s.classifyDrained(room, instance, logged, seq, contentChanged, asks)
	}
	doc.OnUpdate(func(update []byte, origin any) {
		// A published write's update is already durable, and so is a catch-up's batch of another
		// process's rows (remoteUpdateOrigin). Its suppression slot is finished here, before ygo's
		// persistence observer, which the room registers after OnLoadDocument, hands the update to
		// the room's worker, which waits on that slot (publishLiveUpdate, catchUpRoom). Both are
		// then classified like any other update, and neither is credited to a local peer
		// (creditContentChange) nor names one as the author of the ask blocks it introduces
		// (observeAskBlocksForUpdate).
		switch durable := origin.(type) {
		case *liveWriteOrigin:
			s.finishSuppressedPersistence(durable.slot, update)
		case *remoteUpdateOrigin:
			s.finishSuppressedPersistence(durable.slot, update)
		}
		if repair, identityRepair := origin.(*identityClosureOrigin); identityRepair {
			s.recordSuppressedCommit(repair.slot, update)
			// A repair's update is classified like any other, and its class names the room
			// instance it was made in: one that reaches the room's persistence outside its
			// suppression slot, which another writer's update can take first
			// (consumeSuppressedPersistence), is stored as any other update, and the room it was
			// made in has to take that version in or stand behind the head for good.
		} else if s.beforeObserveUpdate != nil {
			s.beforeObserveUpdate(room)
		}
		// Every observer drains the room's update log, a repair's too, and each committed update
		// is classified once, by whichever drainer reaches it first - an observer, or an exact
		// read (readPoint) - against the room as the commit before it left it
		// (renderedReplica). This observer's own update is in the log - ygo appended it under the
		// document's lock at its commit - so its class is recorded, in the drain and in commit
		// order, before ygo's persistence observer, which runs after this one on this goroutine,
		// hands it on.
		s.observeRoomUpdates(room, replica)
	})
	// A commit that changed nothing the room holds - one whose whole update waits for an edit the
	// room has not received, so it only parked - has no update observer (ygo calls OnUpdate only
	// for a transaction that changes the document, reearth/ygo#350), but ygo logs it, with what it
	// parked (crdt.LoggedUpdate.Parked). ygo calls OnAfterTransaction for every committed
	// transaction, outside the document's lock and before OnUpdate, so the replica is drained here
	// for a transaction that changed no shared type while the log holds something: the replica
	// takes the commit, and what it parked is queued for the store (classifyDrained), whichever
	// path the update reached the room by - a peer's connection, or a published write whose
	// update waits on an item a cold fork read from the store. A transaction that changed a
	// shared type is drained by the update observer above, after the steps it takes first.
	doc.OnAfterTransaction(func(txn *crdt.Transaction) {
		if replica.log.Pending() > 0 && len(txn.ChangedTypes()) == 0 {
			s.observeRoomUpdates(room, replica)
		}
	})
	resident = true
	return nil
}

// onPeerUpdate stores what a peer's update parked, once ygo has applied it. ygo hands the room's
// persistence only what an update integrated, through OnUpdate, and nothing at all for one that
// only parked bytes - it waits for an edit the room has not received - so this is the one place
// the parked bytes are made durable: the drain the apply ran (the room's update observer, or its
// after-transaction drain for a commit that changed nothing, onLoadDocument) queued them
// (classifyDrained), and storeParked appends them before ygo answers the peer and before ygo's
// Shutdown, which waits for this hook, can let the process exit. The hook runs on the peer's read
// loop, which holds no lock of the service's, so it can wait for the document's lock the store
// takes; an OnUpdate observer cannot, since it runs on the goroutine that committed, which can be
// a settlement holding that lock.
// An unbounded apply can fail after parking bytes, so its error does not skip the durable append.
func (s *Service) onPeerUpdate(info websocket.PeerUpdateInfo, apply func() error) error {
	err := apply()
	s.storeParked(info.Room)
	return err
}

// observeRoomUpdates drains replica's log and arms the settlement the drained updates owe. It runs
// from the room's update observer for every commit that changed the document, and from its
// after-transaction drain for one that changed nothing (onLoadDocument). The caller holds no
// replica lock.
func (s *Service) observeRoomUpdates(room string, replica *renderedReplica) {
	settles, changed, local := replica.observe(room)
	if changed && s.afterCreditUpdate != nil {
		s.afterCreditUpdate(room)
	}
	s.armDrainedSettle(room, settles, changed, local)
}

// observeRoomDoc drains live's update log for a caller that is not one of its observers, so every
// update committed to the room by now is classified (classifyDrained), and arms the settlement
// what it classified asks for, as the observer it stood in for would have. A document this
// process lists no replica for - one no room of its loaded, or whose room's document has been
// collected - has nothing to drain. It runs no room observer's test hook: it is no observer.
func (s *Service) observeRoomDoc(room string, live *crdt.Doc) {
	replica := s.replicaOf(live)
	if replica == nil {
		return
	}
	settles, changed, local := replica.observe(room)
	s.armDrainedSettle(room, settles, changed, local)
}

// armDrainedSettle arms room's settlement for what one drain of its update log classified
// (renderedReplica.observe).
func (s *Service) armDrainedSettle(room string, settles, changed, local bool) {
	switch {
	case local:
		s.scheduleSettle(room)
	case settles && changed:
		// A content change another process made, which a catch-up applied, only re-arms a
		// settlement already armed here and arms none where none is (rearmSettle): the process
		// whose peer made it armed its own when that peer's update reached its room, so editors
		// on two processes typing in turn make one burst, which settles once, a settle delay
		// after the last keystroke on either.
		s.rearmSettle(room)
	}
}

// storeParked appends, in the order the room committed them, the parked bytes its drains queued
// (classifyDrained), each on its own and gated on the cursor of the room instance that parked
// it (AppendParkedUpdate). It stores them with no class, no credit and no settlement owed: they
// changed nothing the room renders, and the commit that integrates them marks the document owed.
// A load of the stored history parks them again and integrates them once their predecessor
// arrives, which a browser that reconnects sends; the commit that integrates them stores them a
// second time, in its own update, which a load applies to no effect, since a struct a document
// already holds is skipped. The bytes are bounded by the room's pending queue (MaxPendingItems),
// which refuses an update that would park past it, and a struct sent again parks only once, so a
// peer that keeps sending what cannot integrate cannot grow the store past that bound per room
// instance. An append that fails fails the room, as any of the room's appends does.
func (s *Service) storeParked(room string) {
	for {
		state := s.lockExistingState(room)
		if state == nil || len(state.parked) == 0 {
			if state != nil {
				s.unlockState(room, state)
			}
			return
		}
		next := state.parked[0]
		state.parked = slices.Delete(state.parked, 0, 1)
		s.unlockState(room, state)
		if s.roomFailed(room) {
			continue
		}
		ctx := withRoomCursor(context.Background(), next.cursor)
		if _, err := s.persistence.AppendParkedUpdate(ctx, room, next.update); err != nil {
			s.failRoom(room, fmt.Errorf("store the parked bytes of a document update: %w", err))
		}
	}
}

// classifyDrained records the class of logged, the update room's log handed over at commit
// position seq in instance, given whether it changed the room's rendered markdown and the ask
// blocks it left when it did (renderedReplica). It is the room's drainClassifier, which runs
// holding the replica's lock. It reports whether a settlement follows the update, which is every
// update but a repair's, whether it changed the content, and whether this process made it - a
// peer's update or a published live write's, rather than another process's append a catch-up
// applied.
//
// A commit can park bytes - structs or deletions that wait for an edit the room has not received -
// and ygo logs them in Parked, beside an Update that carries only what the commit integrated, the
// bytes OnUpdate hands the room's persistence. So the room's persistence never stores parked bytes,
// and they are queued on the room's record for storeParked to store on their own, outside the
// replica's lock - a drain runs under a settlement's document lock too (readPoint), and the store
// takes that lock. Parked bytes a published live write left are already durable, stored by the
// transaction that wrote it, and a catch-up's are the stored rows themselves, so only another
// origin's bytes are queued. A commit that only parked
// is logged with an empty update (emptyUpdateV1), and ygo calls no OnUpdate for it, so no
// persistence call will ever take a class for it: none is queued.
//
// The cursor is the instance's, taken here rather than looked up when the update reaches the
// store: a room evicted and reloaded in between is a different instance, which did not take this
// update and must not be recorded as holding the version it is stored at. The instance's resident
// token is taken with it, so the update's bytes count against the load that committed it
// (queueUpdateClass). An update only the room holds is listed on the instance as unstored
// (roomInstance.unstored) as it is classified, so a room query of another process can answer with
// it (unstoredShare).
func (s *Service) classifyDrained(room string, instance *roomInstance, logged crdt.LoggedUpdate, seq uint64, contentChanged bool, asks func() map[string]struct{}) (settles, changed, local bool) {
	origin, published := logged.Origin.(*liveWriteOrigin)
	_, remote := logged.Origin.(*remoteUpdateOrigin)
	if len(logged.Parked) > 0 && !published && !remote {
		state := s.lockState(room)
		state.parked = append(state.parked, parkedBytes{update: logged.Parked, cursor: &instance.cursor})
		s.unlockState(room, state)
	}
	if len(logged.Parked) > 0 && bytes.Equal(logged.Update, emptyUpdateV1) {
		return false, false, false
	}
	class := documentUpdateClass{
		seq:            seq,
		contentChanged: contentChanged,
		cursor:         &instance.cursor,
		resident:       instance.resident,
		observed:       time.Now(),
	}
	if _, identityRepair := logged.Origin.(*identityClosureOrigin); identityRepair {
		// A repair's writer appends its update in its own transaction, under the document's
		// advisory lock it holds while the room takes the update, so the class is queued stored:
		// no durable writer of the room stores it again, and it is no work the room's state
		// waits on. It is queued all the same, so the room's persistence, which ygo hands it as
		// any other update, finds it stored when it falls outside its suppression slot
		// (consumeSuppressedPersistence).
		s.queueUpdateClass(room, logged.Update, class, true)
		return false, false, false
	}
	var attributed map[string]model.Actor
	if asks != nil {
		attributed = s.observeAskBlocksForUpdate(room, asks(), logged.Origin)
	}
	class.durable = true
	if contentChanged {
		// The authors of the ask blocks this render first showed go with the credit to the
		// update's append, which records them durably (recordAskAuthors): a tree is rendered
		// only for a content change, so a browser's update that names an author always carries
		// a credit. A published live write carries none; its commit recorded its blocks'
		// authors (Ledger.recordSettlementCredit).
		class.credit = s.creditContentChange(room, logged.Origin, &instance.hold, attributed)
	} else {
		class.roomCredit = s.unchangedCredit(room, logged.Origin, &instance.hold)
	}
	// A published write's update was stored by the transaction that wrote it, before the room
	// took it, and a catch-up's is the stored row itself. Every other update only the room holds,
	// so it names its instance and is listed there until its append has returned.
	if !published && !remote {
		class.instance = instance
	}
	queued := s.queueUpdateClass(room, logged.Update, class, published || remote)
	if class.instance != nil {
		instance.listUnstored(queued)
	}
	// A published write's last update closes the window of the shadow its commit left
	// (reachShadow), before any update after it is queued.
	if published && origin.shadow != nil {
		s.reachShadow(room, origin.shadow, &instance.cursor, seq)
	}
	return true, contentChanged, !remote
}

// soleConnectedActor is the one actor connected to a room, and whether more than one distinct
// actor is: an update made while exactly one of them is connected can be pinned on that actor; one
// made while several are cannot be pinned on any single one of them.
func soleConnectedActor(connected map[uint64]model.Actor) (sole *model.Actor, ambiguous bool) {
	for _, actor := range connected {
		if sole == nil {
			sole = new(actor)
		} else if actorKey(actor) != actorKey(*sole) {
			ambiguous = true
		}
	}
	if ambiguous {
		sole = nil
	}
	return sole, ambiguous
}

// observeAskBlocksForUpdate records askBlocks, the ask blocks an update left in room's document,
// and attributes each one not already recorded (observeAskBlocks), in the same critical section
// the update observer classifies the update in (renderedReplica.observe): the observer classifies
// each update in the order the replica took them, so the room's record of its ask blocks moves
// forward only. The walk that lists those ask blocks is the asks function the replica's measure
// of the update answers with (changedBy), which takeLocked hands the replica's drainClassifier,
// classifyDrained, the caller here: that measure answers one only for an update it found to be a
// real change, so the no-op path, the common one, pays for neither. A nil askBlocks is that
// walk's answer when it could not read the document, never a document holding no ask block
// (askBlockIDs): the room's record is left as it is. It returns the authors it named for the
// blocks no write registered, which the update's append records.
//
// An update another process stored, which a catch-up applied (origin is a remoteUpdateOrigin),
// introduced its blocks on that process: none of this room's connected peers wrote them, so the
// blocks are recorded with no author here, as the blocks a room loads with are, and a settlement
// finds the author that process recorded (askBlockSources.author).
func (s *Service) observeAskBlocksForUpdate(room string, askBlocks map[string]struct{}, origin any) map[string]model.Actor {
	if askBlocks == nil {
		// The walk could not read the document it had to walk, and logged that
		// (renderedReplica.spliceTouched): the room keeps the ask blocks and the authors it
		// recorded, as it does for an update whose own render failed, which is handed no asks
		// at all.
		return nil
	}
	state := s.lockState(room)
	defer s.unlockState(room, state)
	var author *model.Actor
	if _, remote := origin.(*remoteUpdateOrigin); remote {
		author = nil
	} else if sole, ambiguous := soleConnectedActor(state.connected); ambiguous {
		author = new(SettlementActor)
	} else {
		author = sole
	}
	return state.observeAskBlocks(askBlocks, author)
}

// originCreditsNoOne is whether a commit's origin names nobody to credit for the update, the one
// rule both credit paths follow (creditContentChange, unchangedCredit). Four origins credit no
// one. A service repair's origin, the one serviceTransact registered: the service made that
// commit, and a browser that was only connected while it happened is not credited for it. A
// published live write's *liveWriteOrigin: Ledger.Commit credited that write when its transaction
// committed, and it is not credited again. A catch-up's *remoteUpdateOrigin: another process
// stored that update, and its append recorded the authors that process credited it to, so none
// of this room's peers may be named for it on either path. And no origin at all: every commit a
// loaded room makes names an origin - a browser's update names the peer ygo applied it for (its
// *peer, ygo's provider/websocket peer_update.go), a service mutation names the sentinel ygo's
// Server.Apply makes for the call (applyOriginSentinel), which serviceTransact registers as a
// service origin, a repair names its own *identityClosureOrigin (applySuppressed), a published
// live write its *liveWriteOrigin and a catch-up its *remoteUpdateOrigin. The room's own load
// decodes the stored history with no origin (bootstrapLoadedState), earlier in the load hook
// that goes on to open the update log and register this observer, so no observer ever sees it. A
// nil origin here can therefore only be ygo's own cleanup transaction, which a document commits
// for itself after a remote delete leaves a text's formatting markers covering no text
// (reearth/ygo #284): nobody typed it, so neither credit path may name anyone for it. On the
// replica's recover path, where every update it could not place counts as a content change,
// crediting it would hand every connected browser an edit none of them made (INFRA-644); on the
// shadow path, a cleanup the room measured as no change is stored as a change wherever an open
// write's shadow measures one (storedClasses), which would store those browsers as its authors.
func (s *Service) originCreditsNoOne(origin any) bool {
	if origin == nil {
		return true
	}
	switch origin.(type) {
	case *liveWriteOrigin, *remoteUpdateOrigin:
		return true
	}
	_, service := s.serviceOrigins.Load(origin)
	return service
}

// creditContentChange credits an observed content change to its authors, or to no one where its
// origin credits no one (originCreditsNoOne). Any other update is a browser edit by one of the
// peers, which ygo applies while that peer's connection is registered. ygo does not say which
// connection sent it, so every connected peer is credited with it: when exactly one is connected
// it is the latest edit source and replaces `lastActor`, and otherwise the edit cannot be pinned
// on a single peer and no older actor may stand in for it.
//
// The credit goes to F, the room's in-flight credits, until the edit's append lands it
// (AppendUpdatesWithCredit): the returned UpdateCredit is how that append reaches it, and how
// askAuthors, the authors of the ask blocks the edit's render first showed (observeAskBlocks),
// reach the durable record in the same append. This takes no advisory lock and touches no
// database: nothing reads F without holding state.mu.
func (s *Service) creditContentChange(room string, origin any, hold *roomHold, askAuthors map[string]model.Actor) *UpdateCredit {
	if s.originCreditsNoOne(origin) {
		return nil
	}
	state := s.lockState(room)
	defer s.unlockState(room, state)
	record := connectedCreditLocked(state)
	record.seq = state.creditSeq.Add(1)
	state.inflight[record.seq] = record
	state.lastActor = record.lastActor
	state.unsettled = true
	return &UpdateCredit{service: s, room: room, state: state, record: record, hold: hold, askAuthors: askAuthors}
}

// unchangedCredit is the credit a browser update the room's update observer measured as no change
// carries for the room's shadow (documentUpdateClass.roomCredit): the peers connected as the room
// took it, the authors creditContentChange would credit the same commit to, recorded only while a
// live write is open, whose shadow may measure the update a change once the write is stored before
// it. It is not in F and takes no credit sequence: no version lists an update recorded as no
// change, and its append writes these authors only when it stores the update as a change. It
// weighs the origin by the same rule that path does (originCreditsNoOne), so a service repair, a
// published live write Ledger.Commit credited already and a commit with no origin at all - ygo's
// own cleanup, which nobody typed - carry no authors for a shadow to store them with either. Nil
// too while no write is open.
func (s *Service) unchangedCredit(room string, origin any, hold *roomHold) *UpdateCredit {
	if s.originCreditsNoOne(origin) {
		return nil
	}
	state := s.lockState(room)
	defer s.unlockState(room, state)
	if state.liveWriter == nil {
		return nil
	}
	return &UpdateCredit{service: s, room: room, state: state, record: connectedCreditLocked(state), hold: hold}
}

// connectedCreditLocked is a credit record naming the peers connected to state, and the one of
// them it pins the edit on, if one. The caller holds state.mu.
func connectedCreditLocked(state *roomState) *inflightCredit {
	record := &inflightCredit{authors: make(map[string]model.Actor, len(state.connected))}
	for _, actor := range state.connected {
		record.authors[actorKey(actor)] = actor
	}
	record.lastActor, _ = soleConnectedActor(state.connected)
	return record
}

// registerConnectionLocked registers a browser connected to the room whose state is state, which
// the caller holds locked. It is credited only with browser edits observed while it is connected
// (creditContentChange), never for connecting or for an agent's edit. It counts nothing: the
// browser's count (connections) is the place under the connection cap reserveConnection took for
// it before, which removeConnection gives back as it forgets the browser. authorize registers each
// document socket's browser through it.
func (s *Service) registerConnectionLocked(state *roomState, id uint64, actor model.Actor) {
	state.connected[id] = actor
	forgetMarkAuthorDeparture(state, actor)
}

// removeConnection forgets a browser that left room, and with the last one, once ygo has retired
// the room, the room's state (releaseIfUnusedLocked). A document without state has none to forget.
// The service's count of connections drops only for an id the record held: a failed room's
// browsers are found on the record that replaced it (handOverFailedRoomLocked), so each leaves
// the count once. The departure of the last browser of an actor is when the unrecorded marks that
// actor wrote start to age (sweepUnrecordedMarks).
func (s *Service) removeConnection(room string, id uint64) {
	state := s.lockExistingState(room)
	if state == nil {
		return
	}
	actor, registered := state.connected[id]
	if registered {
		delete(state.connected, id)
		s.connections.Add(-1)
		recordMarkAuthorDeparture(state, actor, s.now())
	}
	s.unlockState(room, state)
}

// settleLastPeer runs the settlement a room's last browser leaving owes now rather than when its
// timer fires. It takes over the timer's settleWG count, so Shutdown joins it as it would the
// timer's settlement: Shutdown reports which documents settled, and stops ygo, only once it has
// returned. It counts itself as running (settling) from the moment it takes the timer, as the timer
// it replaces counted as armed: a write that opens before the settlement reaches its own count
// then takes the settlement over (openLiveWrite) rather than ending it with nobody to arm it again.
func (s *Service) settleLastPeer(_ context.Context, room string) {
	state := s.lockExistingState(room)
	if state == nil {
		return
	}
	if !s.takeSettleTimer(state.settle) {
		s.unlockState(room, state)
		return
	}
	generation := state.roomGeneration
	state.settling++
	s.unlockState(room, state)
	defer s.settleWG.Done()
	defer func() {
		state.mu.Lock()
		state.settling--
		s.unlockState(room, state)
	}()
	s.settleRoom(room, generation)
}
