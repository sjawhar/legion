package docs

import (
	"runtime"
	"slices"
	"sync"
	"weak"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
)

// roomInstance is what one resident room instance records about the durable history behind its
// document: hold, the updates whose writes can have recorded a pending author (roomHold), and
// cursor, the durable versions the instance's document holds (roomCursor). They are different
// sets, which is why the cursor is kept beside the hold rather than read off it: a browser append
// that changed no rendered content carries a nil credit and never enters the hold, and
// settlement's own repair append carries none either, so a hold equal to the head would say
// nothing about whether the instance holds it.
//
// catchUp is held by the one catch-up of this instance that may run at a time (catchUpRoom): it
// reads the cursor, reads the stored rows past it and applies them, and two of those side by side
// would apply one row twice. Lock order: the document's advisory lock (only a caller that already
// holds it, a settlement), then catchUp, then the room's state.mu; the catch-up itself never takes
// the advisory lock.
//
// unstored is every update made in this instance that only the room holds - a browser's, or a
// service repair the room's persistence stores - from the moment the room's update observer
// classified it until its append has returned, in the order the room committed them (the update
// log's order, which the observer classifies in). A room query of another process reads them to
// answer what this room holds and the store may not (unstoredShare). unstoredMu guards it alone.
type roomInstance struct {
	hold    roomHold
	cursor  roomCursor
	catchUp sync.Mutex
	// loadedHead is the head the instance's load read (roomLoad.head), the version through which
	// the document it loaded holds every stored update: the load folds the stored history through
	// that head (servicePersistenceAdapter.LoadDoc, PgVersioned.Load). It is not the cursor's
	// start, which also folds in the versions took recorded while the load ran - those are durable,
	// but not in the loaded document. Set once, when the load starts the instance (roomLoad.start).
	loadedHead int64

	unstoredMu sync.Mutex
	unstored   []*queuedClass
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
type roomLoad struct {
	mu   sync.Mutex
	head int64
	// state is the durable state the load read, which the room's load takes before anything that
	// can refuse it (takeLoadedState) and applies itself (applyLoadedState) rather than ygo.
	state []byte
	// appended are the versions took recorded before the instance existed, folded into its cursor
	// when it does (start). instance is that instance, which took advances directly from then on.
	appended []int64
	instance *roomInstance
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
// holds every update through, and the state through that head, which the room's load applies next
// (takeState).
func (load *roomLoad) read(head persistence.Version, state []byte) {
	load.mu.Lock()
	defer load.mu.Unlock()
	load.head = int64(head)
	load.state = state
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

// start is the instance this load leaves behind: it holds the head the load read, and every
// version took recorded since, added to its cursor as every advance is (advanceRoomCursor). The
// load holds it from here, so a version appended before ygo publishes the loaded room still
// reaches it.
func (load *roomLoad) start() *roomInstance {
	load.mu.Lock()
	defer load.mu.Unlock()
	load.instance = &roomInstance{hold: roomHold{loaded: load.head}, cursor: roomCursor{through: load.head}, loadedHead: load.head}
	for _, version := range load.appended {
		advanceRoomCursor(&load.instance.cursor, persistence.Version(version))
	}
	load.appended = nil
	return load.instance
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
// instance through it, which is what ygo's loading window needs it for.
//
// It also schedules one catch-up of the instance (roomCatchUps.schedule), which waits for ygo to
// publish it once this load hook returns (roomCatchUps.takeLoading): a load reads the store with
// no lock, so another process's append can commit between that read and the moment this room is
// resident, and its notification can be decided while the room is still loading, when there is no
// room to catch up. Without this the room would be born behind with nothing left to come for it.
func (s *Service) keepInstance(room string, live *crdt.Doc) *roomInstance {
	value, ok := s.roomLoads.Load(room)
	if !ok {
		// A room ygo loaded without reading the store through this service's persistence stands
		// at nothing, as a load that read an empty history does.
		value = s.beginRoomLoad(room)
	}
	load := value.(*roomLoad)
	instance := load.start()
	listResident(&s.instances, live, instance)
	runtime.AddCleanup(live, func(load *roomLoad) { s.roomLoads.CompareAndDelete(room, load) }, load)
	s.catchUps.schedule(room, live)
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

// roomCursor is which durable versions one instance of a room holds: every version through
// through - the head its load read, raised as the versions after it arrive - and the versions in
// above, each past through+1, that the instance holds with a gap below them. A version reaches it
// when that instance applied its update: a browser update its own append stored, each room update
// a flush of this process stored with it, a committed live write published into it, settlement's
// own repair, and another process's append its catch-up applied from the store
// (Service.catchUpRoom).
//
// Two processes serve one database, and a room resident in this one receives the other's appends
// only through catch-up, so between that append's commit and its catch-up the room is behind: its
// through is short of the head. Its own browsers' appends land at the head meanwhile, past the
// gap, and wait in above until the catch-up fills the gap and they fold into through, exactly as
// roomHold.since folds into roomHold.loaded. Every write that renders durable state from a room -
// a settlement's version and its ask reconciliation, an upload's or a conditional edit's version,
// a rebuild - is refused while through is not the durable head (the gate's appendDerived), because
// what it would write is rendered from a document missing that update.
type roomCursor struct {
	mu      sync.Mutex
	through int64
	above   []int64
	// replaced is set once the gate admitted a browser's append from the instance on a stored
	// history replaced under it (storedHistoryHolds, markReplaced): no stored row at or below
	// through remains, so no catch-up fills the gap above it and the instance never stands at the
	// head again. closeTaken is set once an append stored that way has asked for the instance's
	// close (takeReplaced), so later ones do not ask again.
	replaced, closeTaken bool
}

// advanceRoomCursor records that cursor's room instance now holds the durable update stored at
// version. It is the one funnel every advance goes through, so the sites that have to advance a
// cursor are one grep away. They are every site that makes a version durable for a document a
// room of this process holds:
//
//   - the room's load, which starts the cursor at the head it read and the versions this process
//     appended while it ran (keepInstance, roomLoad);
//   - a browser append that committed, inside the lock it holds (PgVersioned.appendUpdate), and
//     each room update its flush stored before it (PgVersioned.AppendRoomFlush);
//   - a live write's commit, for the versions its publish is about to apply to the room
//     (Ledger.takeRoomCursors), and that publish itself (publishLiveUpdate), and a joined
//     transaction's commit for the room updates it flushed (Ledger.takeRoomCursors);
//   - a settlement's commit, for its repairs and the room updates it stored around them, its
//     superseded path included (commitSettlement);
//   - the block-id backfill's stamp, which commits through that same path (backfillBlockIDs);
//   - catch-up, for each stored row it applied (catchUpRoom).
//
// Only one of them records its version on a second instance as well: a live write's commit,
// which follows this call with Service.tookResidentVersion so the room its publish is about to
// reach - a room still loading, or one that replaced the write's fork - takes the version in
// (Ledger.takeRoomCursors). That is sound there because the publish then applies the update to
// that very room (publishLiveUpdate): the instance credited with the version ends up holding it.
//
// The repair appends - a settlement's and the backfill's - are the opposite case, and none of
// them calls tookResidentVersion. Their update is written into one room instance before it is
// stored (applySuppressed), and nothing replays it into a room that loaded since but catch-up: a
// room that read the store before the repair committed does not hold the repair, so this advance
// must reach the instance the repair was written into and no other. Crediting a room load in
// flight with the version would say a document holds an update it does not, so its catch-up would
// skip the row and the first derived write rendered from it would be written over text this
// process never saw. Such a room is behind the head and has to be: its catch-up reads the repair
// from the store (TestARoomLoadingWhileAStampCommitsCatchesUpToIt).
//
// A nil cursor is a document no room instance holds, whose writes no cursor gates.
func advanceRoomCursor(cursor *roomCursor, version persistence.Version) {
	if cursor == nil {
		return
	}
	cursor.mu.Lock()
	defer cursor.mu.Unlock()
	at := int64(version)
	if at <= cursor.through {
		return
	}
	// Versions mostly arrive in ascending order, so the search lands at the end.
	index, found := slices.BinarySearch(cursor.above, at)
	if !found {
		cursor.above = slices.Insert(cursor.above, index, at)
	}
	folded := 0
	for folded < len(cursor.above) && cursor.above[folded] == cursor.through+1 {
		cursor.through++
		folded++
	}
	cursor.above = slices.Delete(cursor.above, 0, folded)
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

// held is the cursor as of one moment: through, and a copy of the versions above it.
func (cursor *roomCursor) held() (through int64, above []int64) {
	cursor.mu.Lock()
	defer cursor.mu.Unlock()
	return cursor.through, slices.Clone(cursor.above)
}

// markReplaced records that the gate found the stored history replaced under the instance and
// admitted a browser's append from it all the same (storedHistoryHolds).
func (cursor *roomCursor) markReplaced() {
	cursor.mu.Lock()
	defer cursor.mu.Unlock()
	cursor.replaced = true
}

// takeReplaced reports whether the instance was found on a replaced history (markReplaced) and no
// caller has taken that yet: true once, to the append whose close of the instance it asks for
// (Service.closeRoomOnReplacedHistory). A nil cursor answers false.
func (cursor *roomCursor) takeReplaced() bool {
	if cursor == nil {
		return false
	}
	cursor.mu.Lock()
	defer cursor.mu.Unlock()
	if !cursor.replaced || cursor.closeTaken {
		return false
	}
	cursor.closeTaken = true
	return true
}
