package docs

import (
	"context"
	"fmt"
	"runtime"
	"slices"
	"sync"
	"weak"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
)

// roomInstance is what one resident room instance records about the durable history behind its
// document: hold, the updates whose writes can have recorded a pending author (roomHold), and
// cursor, the durable version the instance's document stands at (roomCursor). They are different
// sets, which is why the cursor is kept beside the hold rather than read off it: a browser append
// that changed no rendered content carries a nil credit and never enters the hold, and
// settlement's own repair append carries none either, so a hold equal to the head would say
// nothing about whether the instance holds it.
type roomInstance struct {
	hold   roomHold
	cursor roomCursor
	// resident is the token of the load this instance stands for, which the bytes of every update
	// the instance commits count against (classifyDrained, queueUpdateClass), so the unload that
	// ends the token takes them back (residentLoad). Set once, where the instance is kept (keepInstance).
	resident *residentLoad
}

// roomLoad is one room load's record of the durable history behind the room it loads, kept from
// before the load reads the store (servicePersistenceAdapter.LoadDoc) until the instance that
// load leaves behind stands in its place (keepInstance, which onLoadDocument calls on the same
// goroutine, ygo's loadRoom). ygo publishes no document for a room while it loads (Server.GetDoc
// reports a loading room as absent), so a version this process appends in that window reaches no
// instance of its own: it reaches this instead (took), and the instance takes it in with the head
// the load read. Without that the fresh room would stand at the head of the read alone while the
// store stood one past it, and the gate would refuse its every durable write until it reloaded -
// a room born behind, which no reload is guaranteed to fix, since the next load can lose the same
// race.
//
// Another process's write in that window reaches no instance here either, and took never hears of
// it: what reaches this process is its documentHeadChannel notice, which the stale-room evictor
// hands here (notice) rather than to a room it cannot find, and the instance is decided on it
// once it is resident (start, keepInstance).
type roomLoad struct {
	mu   sync.Mutex
	head int64
	// state is the durable state the load read (read), until the room's load hook takes it
	// (takeState, takeLoadedState) and decodes it itself (bootstrapLoadedState) rather than ygo:
	// nil once taken, and for a load that read an empty history.
	state []byte
	// appended are the versions took recorded before the instance existed, folded into its cursor
	// when it does (start). instance is that instance, which took advances directly from then on.
	appended []int64
	instance *roomInstance
	// noticed is the last documentHeadChannel notice the stale-room evictor heard for the room
	// before the instance existed, and recheck the evictor's decision on it, nil while none came
	// (notice). The notices of one document arrive in the order their writes committed, so the
	// last one names the head the history stood at when it was heard.
	noticed documentHeadNotice
	recheck func(documentHeadNotice)
	// size is the encoded bytes of the history the load read, which the room counts as resident
	// once it loads (onLoadDocument). rendered is what a document socket's admission made of the
	// state the load took (takePreload, admissionRendering): the tree it read from it and what it
	// rendered of that tree, which the room's load takes rather than reading and rendering the
	// room again (takeLoaded) and hands the room's replica (keepReplica); zero when the load read
	// the store itself.
	size     int
	rendered admissionRendering
}

// beginRoomLoad records that room is loading, before the load reads the store: every version
// appended from here on is one the instance this load leaves behind takes in. It replaces the
// record of the room's last load, which ygo has finished with - one room loads at a time
// (getOrCreateRoom) - so the records never outnumber the documents this process has loaded. A
// record goes when the document of the load that left it is collected (keepInstance), or, for a
// load that left none, where the failure that ended it was met (LoadDoc, onLoadDocument).
func (s *Service) beginRoomLoad(room string) *roomLoad {
	load := &roomLoad{}
	s.roomLoads.Store(room, load)
	return load
}

// read records what the load's read of the store saw: the head, the version the room it loads
// holds every update through, the state through that head, which the room's load applies next
// (takeState), and rendered, what a document socket's admission read and rendered of that state,
// when the load took that socket's preload, zero when the load read the store itself (takeLoaded).
func (load *roomLoad) read(head persistence.Version, state []byte, rendered admissionRendering) {
	load.mu.Lock()
	defer load.mu.Unlock()
	load.head = int64(head)
	load.state, load.size, load.rendered = state, len(state), rendered
}

// takeState hands over the state the load read, once, so the record holds no copy of it for as
// long as the document it loaded lives.
func (load *roomLoad) takeState() []byte {
	load.mu.Lock()
	defer load.mu.Unlock()
	state := load.state
	load.state = nil
	return state
}

// takeLoaded is the size of the history the load read, and what a socket's admission read and
// rendered of it, and drops that: the load's record stays with the room's document for as long as
// it is resident (keepInstance), and the tree is a whole copy of it.
func (load *roomLoad) takeLoaded() (size int, rendered admissionRendering) {
	load.mu.Lock()
	defer load.mu.Unlock()
	size, rendered = load.size, load.rendered
	load.rendered = admissionRendering{}
	return size, rendered
}

// takeLoadedState is the state room's load read (servicePersistenceAdapter.LoadDoc), taken off its
// record (roomLoad.takeState): nil for a room ygo loads without that read, and for an empty
// history.
func (s *Service) takeLoadedState(room string) []byte {
	if loading, ok := s.roomLoads.Load(room); ok {
		return loading.(*roomLoad).takeState()
	}
	return nil
}

// took records version, which this process has made durable for the room while its load runs: on
// the load until the instance exists, and on that instance's cursor after (start).
func (load *roomLoad) took(version persistence.Version) {
	load.mu.Lock()
	defer load.mu.Unlock()
	if load.instance != nil {
		advanceRoomCursor(&load.instance.cursor, version)
		return
	}
	load.appended = append(load.appended, int64(version))
}

// start is the instance this load leaves behind: it stands at the head the load read, with every
// version took recorded since folded in, in order and only where each follows the one before, as
// every advance of a cursor is (advanceRoomCursor). The load holds it from here, so a version
// appended before ygo publishes the loaded room still reaches it. recheck is the evictor's
// decision on the last notice heard before the instance existed (notice), bound to that notice,
// when that notice names a head other than the one the instance stands at, and nil otherwise. The
// load then read the store before the write that notice announced - a prune among them, whose
// head is lower - or after a later write whose own notice had not arrived yet, which the
// decision's read of the head tells apart; its caller hands the room to that decision once the
// instance is listed (keepInstance).
func (load *roomLoad) start() (instance *roomInstance, recheck func()) {
	load.mu.Lock()
	defer load.mu.Unlock()
	through := load.head
	slices.Sort(load.appended)
	for _, version := range load.appended {
		if version == through+1 {
			through = version
		}
	}
	load.appended = nil
	load.instance = &roomInstance{hold: roomHold{loaded: load.head}, cursor: roomCursor{through: through}}
	if decide, notice := load.recheck, load.noticed; decide != nil && notice.Head != through {
		recheck = func() { decide(notice) }
	}
	load.noticed, load.recheck = documentHeadNotice{}, nil
	return load.instance, recheck
}

// notice records notice, a documentHeadChannel notice the stale-room evictor heard for the room
// while ygo reported no room of it, with recheck, the evictor's decision on such a notice
// (staleRoomEvictor.noticeLoading). Before the instance exists the load keeps the latest notice
// for start, and reports false. Once it exists - between start and ygo publishing the room, or
// after the room has gone and before its document is collected - it reports whether notice names
// a head other than the one the instance stands at, which its caller then decides on itself.
func (load *roomLoad) notice(notice documentHeadNotice, recheck func(documentHeadNotice)) bool {
	load.mu.Lock()
	defer load.mu.Unlock()
	if load.instance != nil {
		return load.instance.cursor.heldThrough() != notice.Head
	}
	load.noticed, load.recheck = notice, recheck
	return false
}

// tookResidentVersion records version, which this process has just made durable for room outside
// the room itself, on the instance that has to take it in: the resident one, or the one the load
// under way is about to leave behind (roomLoad). A document this process holds no room or load
// for takes nothing: the room that loads next reads the version from the store.
func (s *Service) tookResidentVersion(room string, version persistence.Version) {
	if loading, ok := s.roomLoads.Load(room); ok {
		loading.(*roomLoad).took(version)
	}
	if live := s.srv.GetDoc(room); live != nil {
		advanceRoomCursor(s.cursorOf(live), version)
	}
}

// keepInstance starts the bookkeeping of live, the document room's load read, and lists it for
// the instance's writes (instanceOf) while live is resident (listResident). The load is the first
// advance of the instance's cursor: the instance holds every durable update through the head that
// load read, and every version this process appended while it ran (roomLoad). The load's record
// goes with the document from here, as the listing does: until then took still reaches this
// instance through it, which is what ygo's loading window needs it for. counted is the token of
// the load the instance stands for, which its updates' bytes count against (classifyDrained); it
// is set before the instance is listed, so every reader of a listed instance reads it too. A
// notice the load heard that the instance does not stand at goes to the stale-room evictor's
// decision for the room (start), which waits for ygo to publish the room before it decides
// (staleRoomEvictor.awaitResidence).
func (s *Service) keepInstance(room string, live *crdt.Doc, counted *residentLoad) *roomInstance {
	value, ok := s.roomLoads.Load(room)
	if !ok {
		// A room ygo loaded without reading the store through this service's persistence stands
		// at nothing, as a load that read an empty history does.
		value = s.beginRoomLoad(room)
	}
	load := value.(*roomLoad)
	instance, recheck := load.start()
	instance.resident = counted
	listResident(&s.instances, live, instance)
	runtime.AddCleanup(live, func(load *roomLoad) { s.roomLoads.CompareAndDelete(room, load) }, load)
	if recheck != nil {
		recheck()
	}
	return instance
}

// instanceOf is live's bookkeeping, nil for a document no room load listed.
func (s *Service) instanceOf(live *crdt.Doc) *roomInstance {
	if listed, ok := s.instances.Load(weak.Make(live)); ok {
		return listed.(*roomInstance)
	}
	return nil
}

// cursorOf is live's roomCursor, nil for a document no room load listed.
func (s *Service) cursorOf(live *crdt.Doc) *roomCursor {
	if instance := s.instanceOf(live); instance != nil {
		return &instance.cursor
	}
	return nil
}

// roomCursor is the durable version one instance of a room stands at: the head its load read, and
// every version appended since whose update that instance applied - a browser update its own
// append stored, a committed live write published into it, and settlement's own repair. Two
// processes serve one database, and a room resident in this one never receives the updates the
// other appended, so an append that raised the head elsewhere is in no cursor here: only a reload
// brings it in. Every write that renders durable state from a room - a settlement's version and
// its ask reconciliation, an upload's or a conditional edit's version, a rebuild - is refused
// while this is not the durable head (ErrRoomBehindHead), because what it would write is rendered
// from a document missing that update.
//
// It advances only by the version that follows the one it stands at, through advanceRoomCursor.
// A version with a gap before it - another process's append this instance never took - leaves the
// cursor where it was, since the instance does not hold the history through that version, and
// only a reload fills the gap: every gate refuses until then, which is the point.
type roomCursor struct {
	mu      sync.Mutex
	through int64
}

// advanceRoomCursor records that cursor's room instance now holds the durable update stored at
// version. It is the one funnel every advance goes through, so the sites that have to advance a
// cursor are one grep away. They are every site that makes a version durable for a document a
// room of this process holds:
//
//   - the room's load, which starts the cursor at the head it read and the versions this process
//     appended while it ran (keepInstance, roomLoad);
//   - a room's batch of browser updates that committed, inside the lock it holds, for each
//     update its flush stored (PgVersioned.AppendRoomFlush), and an append of updates no class
//     holds (PgVersioned.AppendUpdatesWithCredit);
//   - a live write's commit, for the versions its publish is about to apply to the room
//     (Ledger.takeRoomCursors), and that publish itself (publishLiveUpdate), and a joined
//     transaction's commit for the room updates it flushed (Ledger.takeRoomCursors);
//   - a settlement's commit, for its repairs and the room updates it stored around them, its
//     superseded path included (commitSettlement);
//   - the block-id backfill's stamp, which commits through that same path (backfillBlockIDs).
//
// Only one of them records its version on a second instance as well: a live write's commit,
// which follows this call with Service.tookResidentVersion so the room its publish is about to
// reach - a room still loading, or one that replaced the write's fork - takes the version in
// (Ledger.takeRoomCursors). That is sound there because the publish then applies the update to
// that very room (publishLiveUpdate): the instance credited with the version ends up holding it.
//
// The repair appends - a settlement's and the backfill's - are the opposite case, and none of
// them calls tookResidentVersion. Their update is written into one room instance before it is
// stored (applySuppressed), and nothing replays it into a room that loaded since: a room that
// read the store before the repair committed does not hold the repair, so this advance must
// reach the instance the repair was written into and no other. Crediting a room load in flight
// with the version would say a document holds an update it does not, and the first durable
// write rendered from it would be written over text this process never saw - the whole point of
// the gate (refuseRoomBehindHead). No room of this process loads in that window: the repair's
// writer holds the document's advisory lock from before it reads the room until it commits, and
// a failed room's eviction compacts under that lock
// (TestAFailedRoomsEvictionWaitsForTheStampsCommitAndReloadsIt). A room of the other process can
// stand behind the head all the same, and the refusal fails it, the reload reading the repair
// from the store (TestARoomBehindTheHeadIsReloadedAndWritesAgain).
//
// A nil cursor is a document no room instance holds, whose writes no cursor gates.
func advanceRoomCursor(cursor *roomCursor, version persistence.Version) {
	if cursor == nil {
		return
	}
	cursor.mu.Lock()
	defer cursor.mu.Unlock()
	if int64(version) == cursor.through+1 {
		cursor.through = int64(version)
	}
}

// advanceRoomCursorThrough advances cursor through versions, the run one transaction appended for
// its instance, ascending (RoomFlush.appended): a flush's room updates, the repairs stored between
// them and the transaction's own appends interleave by version, and a cursor advances only by the
// version that follows the one it stands at, so the instance takes them in that one order or
// stops where the run has a gap.
func advanceRoomCursorThrough(cursor *roomCursor, versions []persistence.Version) {
	for _, version := range versions {
		advanceRoomCursor(cursor, version)
	}
}

// heldThrough is the durable version the instance holds every update through.
func (cursor *roomCursor) heldThrough() int64 {
	cursor.mu.Lock()
	defer cursor.mu.Unlock()
	return cursor.through
}

// documentCursor is the durable version the document a write was made on holds every update
// through: a room instance's cursor for an update made in a room, or a transaction's own view of
// it (liveWrite.heldThrough), which adds the versions that transaction has appended itself and no
// room holds until it publishes.
type documentCursor interface {
	heldThrough() int64
}

// ErrRoomBehindHead refuses a write that would record durable state for a document from a room
// whose instance does not stand at the durable head: with two envoy-dispatch processes on one
// Postgres, the room resident here never received the updates the other process appended, and a
// version, an ask reconciliation or a conditional edit rendered from it would be written over text
// this process has never seen. It is ErrServiceUnavailable, which is what it is for a caller: this
// process cannot serve that write until the room reloads.
var ErrRoomBehindHead = fmt.Errorf("%w: the document's room is behind its durable head", ErrServiceUnavailable)

// refuseRoomBehindHead is the gate itself: cursor, the durable version the document the write was
// made on holds every update through, must be the head, read in the same transaction under the
// document's advisory lock so no append can land between the two reads.
//
// Equality, not `cursor < head`: a prune lowers the head (PruneAfter's checkpoint, finished by
// recoverPruneTx) and a document's deletion removes it altogether, so a room resident across one
// of those stands ahead of the head and holds a history the store no longer has; `<` would admit
// it. A nil cursor is a write with no room's document behind it to be behind the head - a new
// document's seed (SeedText), a test's own append - and is not gated.
func refuseRoomBehindHead(room string, cursor documentCursor, head int64) error {
	return refuseThroughBehindHead(room, cursorThrough(cursor), head)
}

// refuseThroughBehindHead is refuseRoomBehindHead's decision on through, the version the cursor
// held when it was read, or ungatedAppend for a write no cursor gates.
func refuseThroughBehindHead(room string, through, head int64) error {
	if through == ungatedAppend {
		return nil
	}
	if through != head {
		return fmt.Errorf("%w: room %s holds the document through update %d, the stored document stands at %d",
			ErrRoomBehindHead, room, through, head)
	}
	return nil
}

// cursorThrough is the version cursor holds every update through, read now, or ungatedAppend for
// a nil cursor, a write no room's document stands behind.
func cursorThrough(cursor documentCursor) int64 {
	if cursor == nil {
		return ungatedAppend
	}
	return cursor.heldThrough()
}

// roomCursorContextKey marks a context with the cursor of the room instance whose document an
// append's update was made on.
type roomCursorContextKey struct{}

// withRoomCursor marks ctx with cursor for the gate in appendUpdateTxClass. The appends made from
// a room outside any live write mark their own context, since no ledger of theirs names the
// document they were made on: the room's own appends (servicePersistenceAdapter.StoreUpdateContext,
// which takes the cursor the update's observer recorded with it), settlement's repairs
// (settleRoomWithin) and the block-id backfill's stamp (backfillBlockIDs).
func withRoomCursor(ctx context.Context, cursor *roomCursor) context.Context {
	if cursor == nil {
		return ctx
	}
	return context.WithValue(ctx, roomCursorContextKey{}, cursor)
}

// roomCursorFrom is the cursor withRoomCursor marked ctx with, nil for a context no room marked.
func roomCursorFrom(ctx context.Context) *roomCursor {
	cursor, _ := ctx.Value(roomCursorContextKey{}).(*roomCursor)
	return cursor
}

// appendingCursor is the cursor the gate reads for an append to room: the calling transaction's
// own write to room when it has one, whose fork is what the write was made on; otherwise, for one
// of a flush's own appends, flush, the room instance its updates were made in with the versions
// the flush has appended folded in (RoomFlush); otherwise the room cursor its caller marked the
// context with, with the versions the caller's transaction has appended for that instance folded
// in when it flushed the room (a settlement's repairs, storeRepair). Nil is an append no cursor
// gates.
//
// flush is passed by the appends of a flush alone, and is the only way the room's own persistence
// worker reaches its flush here: its batch (appendRoomTx) passes the flush to the gate it reads in
// its first pipeline, since its transaction has no ledger. A joined transaction's flush appends
// pass it too (Service.flushThrough passes flush to the one append of the run it makes,
// AppendRoomUpdatesTx), but the write branch comes first: a live write's own flush
// (Ledger.flushThrough, from recordLiveOperation) is decided by the write, whose heldThrough folds
// the flushed versions in (withOwnAppends). The flush branch decides the flush appends of a
// transaction that holds no write of the room: a settlement's, a version's, the block-id
// backfill's. What the ledger lookup in the room-cursor branch is for is the transaction's other
// appends of that instance, the repairs storeRepair stores through AppendUpdateTx, which the
// flush's versions gate.
//
// A transaction's write decides for itself: a cold write, whose fork came from the store because
// no room of this process held the document (coldFork), carries no cursor and is not gated. The
// gate refuses a write rendered from a room behind the head, and a cold write renders from the
// store instead - it holds no room, its update commutes with whatever landed since its read, and
// the room that loads after its publish reads the store, which holds both by then.
func appendingCursor(ctx context.Context, room string, flush *RoomFlush) documentCursor {
	if write := ledgerFrom(ctx).liveWriteFor(room); write != nil {
		if write.point.cursor == nil {
			return nil
		}
		return write
	}
	if flush != nil && flush.room == room {
		if flush.cursor == nil {
			return nil
		}
		return flush
	}
	if cursor := roomCursorFrom(ctx); cursor != nil {
		return transactionCursor(ctx, room, cursor)
	}
	return nil
}

// transactionCursor is cursor, a room instance's, as the transaction ctx carries sees it: the
// flush that transaction keeps for room when it flushed that very instance (Ledger.flushFor), whose
// heldThrough folds in the versions the transaction has stored of the instance and not yet
// committed, else cursor itself. Both a transaction's appends (appendingCursor) and its fork read
// (roomBehindHead) compare this against the head they read under the document's lock.
func transactionCursor(ctx context.Context, room string, cursor *roomCursor) documentCursor {
	if flush := ledgerFrom(ctx).flushFor(room); flush != nil && flush.cursor == cursor {
		return flush
	}
	return cursor
}

// appendingThrough is appendingCursor's version, read now, or ungatedAppend when no cursor gates
// the append: the through an append made under ctx hands its gate's decision (gateAppend,
// appendGate.head). Its caller reads it once it holds the document's advisory lock, so a peer of
// the same room whose append committed under that lock has already advanced the cursor.
func appendingThrough(ctx context.Context, room string, flush *RoomFlush) int64 {
	return cursorThrough(appendingCursor(ctx, room, flush))
}
