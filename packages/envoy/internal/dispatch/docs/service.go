package docs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

var (
	// ErrIssueClosed rejects a document mutation after its issue is closed.
	ErrIssueClosed = errors.New("issue is closed")
	// ErrServiceUnavailable reports a room that could not load or persist durably.
	ErrServiceUnavailable = errors.New("document service unavailable")
)

const maxSettleFailures = 3

const (
	// maxLiveRooms is how many live rooms - ygo's resident rooms, loaded or loading, whoever
	// opened them - a document socket may find before it is refused a room of its own
	// (canOpenRoom).
	maxLiveRooms       = 1_000
	maxRoomConnections = 1_000
	// roomIdleTimeout is how long a room stays resident after its last peer leaves (New). A peer
	// that returns within it rejoins the warm room rather than reloading the document.
	roomIdleTimeout = time.Minute
)

// Deps configures the live document service.
type Deps struct {
	Store             *store.Store
	Persistence       VersionedStore
	Events            *events.Broker
	Identity          identity.Identity
	AgentToken        string
	ServerURL         string
	Settle            time.Duration
	MarkWait          time.Duration
	UnrecordedMarkTTL time.Duration
}

// VersionedStore is Dispatch's transactional extension of ygo's durable room
// store. Document writes that join an API transaction use AppendUpdateTx, classifying
// the update as content or not the way the room's update observer classifies a live one.
// RebuildTx replaces an unreadable history inside the rebuild's transaction, through the same
// persistence boundary as its preflight load. Head is the version Load would fold up to now, which
// says whether a state loaded earlier is still the stored one.
type VersionedStore interface {
	persistence.VersionedPersistence
	AppendUpdateTx(ctx context.Context, tx pgx.Tx, room string, update []byte, contentChanged bool) (persistence.Version, error)
	RebuildTx(ctx context.Context, tx pgx.Tx, room string, seed []byte) (RebuildReport, error)
	Head(ctx context.Context, room string) (persistence.Version, error)
}

// Service owns live Yjs documents and their durable Dispatch versions.
type Service struct {
	srv               *websocket.Server
	store             *store.Store
	persistence       VersionedStore
	events            *events.Broker
	identity          identity.Identity
	agentToken        string
	serverURL         string
	settle            time.Duration
	markWait          time.Duration
	unrecordedMarkTTL time.Duration
	rooms             sync.Map
	shutdownRooms     sync.Map
	nextConnection    atomic.Uint64
	stopping          atomic.Bool
	// quiescing holds off every settlement while Quiesce empties the rooms, so a timer that
	// fires mid-quiesce cannot re-arm the room Quiesce just closed.
	quiescing atomic.Bool
	// afterSettleWarm runs after settleRoom has warmed the live document and before it
	// reads it. Nil outside tests; tests use it to evict the room in that window.
	afterSettleWarm func(room string)
	// afterSettleLock runs after settleRoom has taken the document's advisory lock and before
	// it touches the room. Nil outside tests; tests use it to fail the room in that window.
	afterSettleLock func(room string)
	// afterReadWarm runs once a read that may load its room holds that room - a version's
	// capture (captureLiveTextAndAuthors) and VerifyMark - and before the read takes anything from
	// it. Nil outside tests; tests use it to evict the room in that window.
	afterReadWarm func(room string)
	// afterSettleRead runs after settleRoom has read the document and before it stamps block ids
	// into the room. Nil outside tests; tests use it to edit the room in that window.
	afterSettleRead func(room string)
	// afterSettleReconcile runs after settleRoom has reconciled the document's ask blocks with
	// their rows and before it writes their repairs into the room. Nil outside tests; tests use it
	// to edit the room in that window.
	afterSettleReconcile func(room string)
	// afterSettleVersionRead runs after a settlement that wrote into the room has read the tree
	// its version is rendered from, and before it renders it. Nil outside tests; tests use it to
	// edit the room while the version renders.
	afterSettleVersionRead func(room string)
	// afterBackfillRead runs after the block-id backfill has read a document that needs stamping
	// and before it stamps it. Nil outside tests; tests use it to edit the room in that window.
	afterBackfillRead func(room string)
	// afterPublishRefused runs when a committed write's publish is refused by its room, before
	// the publish decides whether to fail that room. Nil outside tests; tests use it to let the
	// refused room's recovery finish in that window.
	afterPublishRefused func(room string)
	// afterStateLookup runs when a state lookup (lookUpState) has found a document's state and
	// before it takes the state's lock. Nil outside tests; tests use it to release the state in
	// that window.
	afterStateLookup func(room string)
	// inServiceTransaction runs first in every transaction a service mutation makes through
	// Server.Apply (serviceTransact). Nil outside tests; tests use it to change the live document
	// between the mutation's read of it and its write, where a peer's update can land: Apply runs
	// the mutation outside the document's lock and takes the lock for each transaction alone.
	inServiceTransaction func(txn *crdt.Transaction)
	// now is the clock the unrecorded-mark sweep ages marks by (sweepUnrecordedMarks).
	now      func() time.Time
	settleWG sync.WaitGroup
	// evictWG counts the forced evictions failRoomLocked spawns. They flush the room through
	// the store, so shutdown joins them before it closes.
	evictWG sync.WaitGroup
	// gateMu makes "is the service stopping?" and the Add that follows it one step, against
	// Shutdown's store. sync.WaitGroup panics if an Add from zero lands while a Wait is
	// registered, and without this the check and the Add straddle the store.
	//
	// Lock order: a room's mu is taken BEFORE this - failRoomLocked and
	// scheduleSettleAfterLocked both run under it - so nothing may take a room's mu while
	// holding this one. Shutdown therefore holds it around the stopping store alone, never
	// across the rooms.Range that locks each room.
	gateMu          sync.Mutex
	nextSettleTimer atomic.Uint64
	timerMu         sync.Mutex
	// timers reserve their ID before creating an immediate timer, whose callback
	// can run before time.AfterFunc returns the *time.Timer to its caller.
	timers     map[uint64]*time.Timer
	suppressMu sync.Mutex
	suppressed map[string][]*suppressSlot
	// serviceOrigins holds the transaction origins of the service's own in-flight Server.Apply
	// calls (see serviceTransact), so a room's update observer can tell a service mutation from
	// a browser peer's edit. Every other origin a live document reports, but a published live
	// write's (liveWriteOrigin), is a connected peer.
	serviceOrigins   sync.Map
	conditionalGates sync.Map
	// rebuilding names the documents a rebuild holds (RebuildDocument): their rooms refuse
	// loads and injections until the rebuild's transaction ends (Ledger.endRebuilds).
	rebuilding sync.Map
	// preloads holds, per room, the durable state a document socket's admission check decoded
	// (*preloadedDocument), for the room load that socket makes next (takePreload).
	preloads sync.Map
}

type artifactOwner struct {
	IssueKey *string
	Project  string
	Slug     string
	Name     string
}

// lockArtifactOwner loads an artifact's owner, then locks that owner row: the artifact itself
// for a project document, its issue otherwise. The lock is `for no key update`, so it
// serialises this writer against every other writer that takes it without blocking the
// `for key share` a child insert takes - see lockDocumentRoom for why that is the rule.
func lockArtifactOwner(ctx context.Context, tx pgx.Tx, artifactID string) (artifactOwner, bool, error) {
	var owner artifactOwner
	if err := tx.QueryRow(ctx, `
		select issue_key, project_key, slug, name
		from artifacts where id = $1
	`, artifactID).Scan(&owner.IssueKey, &owner.Project, &owner.Slug, &owner.Name); err != nil {
		return artifactOwner{}, false, fmt.Errorf("load document owner: %w", err)
	}
	if owner.IssueKey == nil {
		var exists bool
		if err := tx.QueryRow(ctx, `
			select true from artifacts where id = $1 and issue_key is null for no key update
		`, artifactID).Scan(&exists); err != nil {
			return artifactOwner{}, false, fmt.Errorf("lock document artifact: %w", err)
		}
		return owner, true, nil
	}
	var open bool
	if err := tx.QueryRow(ctx, `
		select closed_at is null from issues where key = $1 for no key update
	`, *owner.IssueKey).Scan(&open); err != nil {
		return artifactOwner{}, false, fmt.Errorf("lock document issue: %w", err)
	}
	return owner, open, nil
}

type suppressSlot struct {
	ready     chan struct{}
	update    []byte
	canceled  bool
	discarded bool
	consumed  bool
	// committedTo and committed are the document a repair's transaction committed into and the
	// update it committed (recordSuppressedCommit), set before ygo hands that update to the room's
	// persistence.
	committedTo *crdt.Doc
	committed   []byte
}

// identityClosureOrigin identifies a server-owned repair transaction - settlement's, or the
// block-id backfill's - and carries the suppression slot its update is held from the room's
// persistence by. Being non-zero sized also keeps each origin distinct, since ygo compares origins
// by interface equality.
type identityClosureOrigin struct{ slot *suppressSlot }

// recordSuppressedCommit records on slot the update its repair transaction committed and the
// document it committed it into. The room's update observer runs it (onLoadDocument) on the
// committing goroutine before ygo's own persistence observer, which the room registers after
// OnLoadDocument, so the slot holds both before either path to the store is handed the update.
func (s *Service) recordSuppressedCommit(slot *suppressSlot, doc *crdt.Doc, update []byte) {
	s.suppressMu.Lock()
	defer s.suppressMu.Unlock()
	slot.committedTo = doc
	slot.committed = append([]byte(nil), update...)
}

func (s *Service) prepareSuppressedPersistence(room string) *suppressSlot {
	slot := &suppressSlot{ready: make(chan struct{})}
	s.suppressMu.Lock()
	defer s.suppressMu.Unlock()
	if s.suppressed == nil {
		s.suppressed = make(map[string][]*suppressSlot)
	}
	s.suppressed[room] = append(s.suppressed[room], slot)
	return slot
}

// applyCaptured runs mutate on room's live document, whose transactions it tags with origin,
// and returns the updates the room recorded for that origin, in order: the bytes the room's
// persistence observer is handed, which a suppression slot must match. A mutation that changes
// nothing is no error. A mutation that fails returns the updates it recorded before it failed.
func (s *Service) applyCaptured(ctx context.Context, room string, origin any, mutate func(*crdt.Doc) error) ([][]byte, error) {
	var updates [][]byte
	var mutationErr error
	err := s.srv.Apply(ctx, room, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
		unsubscribe := doc.OnUpdate(func(update []byte, updateOrigin any) {
			if updateOrigin == origin {
				updates = append(updates, append([]byte(nil), update...))
			}
		})
		defer unsubscribe()
		mutationErr = mutate(doc)
	})
	if mutationErr != nil {
		return updates, mutationErr
	}
	if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
		return nil, err
	}
	return updates, nil
}

// errRoomReplaced is a repair refused because the room no longer holds the document its caller
// read - the room was evicted, its last browser having left or a CloseRoom having closed it, and
// the write would load a replacement from the store - or given up because the room left the
// server while the repair's transaction committed into it.
var errRoomReplaced = errors.New("document room was replaced")

// applySuppressed writes one repair - settlement's, or the block-id backfill's - into room's live
// document under a suppression slot of its own: mutate writes at most one transaction, tagged with
// the origin it is handed, and reports whether that transaction changed the document. The room's
// persistence worker is handed each transaction's update on its own, so a slot matches exactly
// one. It returns the slot with the update when the transaction changed the document, and
// otherwise neither, having released the slot: a slot left queued holds the worker at the room's
// next update. A repair computed against the document as it stands finds nothing to write when
// another writer has made it unnecessary since the caller read the document (LEGION-479), and its
// transaction still reports an update, the document's delete set, which ygo reports for every
// transaction it commits. The slot is finished with that update, so the worker takes it with the
// slot rather than storing it. A mutation that ran no transaction leaves nothing to match, and its
// slot is cancelled. One that fails after its transaction ran returns the slot and the update with
// its error: what it wrote is in the room, so its caller discards the slot and fails the room.
//
// A repair is written only into want, the document its caller read, when want is not nil: one
// written into a replacement the room loaded since would be versioned from want, which never got
// it. It is refused, writing nothing, with errRoomReplaced. A room can also retire under the
// repair's transaction, between Server.Apply finding it and the commit: the commit's update then
// reaches the store's adapter on this goroutine, which consumeSuppressedPersistence answers by
// discarding it, so a repair whose room is gone once it has written returns its slot with
// errRoomReplaced, and its caller fails the room as for any write it gives up.
func (s *Service) applySuppressed(ctx context.Context, room string, want *crdt.Doc, mutate func(doc *crdt.Doc, origin any) (bool, error)) (*suppressSlot, []byte, error) {
	slot := s.prepareSuppressedPersistence(room)
	origin := &identityClosureOrigin{slot: slot}
	var written *crdt.Doc
	wrote := false
	updates, err := s.applyCaptured(ctx, room, origin, func(doc *crdt.Doc) error {
		if want != nil && doc != want {
			return errRoomReplaced
		}
		written = doc
		var mutateErr error
		wrote, mutateErr = mutate(doc, origin)
		return mutateErr
	})
	if len(updates) == 0 {
		s.cancelSuppressedPersistence(room, slot)
		return nil, nil, err
	}
	if err == nil && len(updates) > 1 {
		err = fmt.Errorf("a repair wrote %d updates, want one", len(updates))
	}
	if err == nil && !wrote {
		s.finishSuppressedPersistence(slot, updates[0])
		return nil, nil, nil
	}
	if err == nil && s.srv.GetDoc(room) != written {
		err = errRoomReplaced
	}
	return slot, updates[0], err
}

func (s *Service) finishSuppressedPersistence(slot *suppressSlot, update []byte) {
	s.suppressMu.Lock()
	defer s.suppressMu.Unlock()
	if slot.canceled || slot.update != nil {
		return
	}
	slot.update = append([]byte(nil), update...)
	close(slot.ready)
}

// discardSuppressedPersistence prevents completed live mutations from falling
// back to ygo's independent persistence after their enclosing transaction failed.
// The persistence callback consumes each slot and discards its matching update.
func (s *Service) discardSuppressedPersistence(room string, slots ...*suppressSlot) {
	for _, slot := range slots {
		s.discardSuppressedSlot(room, slot)
	}
}

func (s *Service) discardSuppressedSlot(room string, slot *suppressSlot) {
	s.suppressMu.Lock()
	defer s.suppressMu.Unlock()
	if slot == nil || slot.consumed || slot.canceled {
		return
	}
	for _, candidate := range s.suppressed[room] {
		if candidate == slot {
			slot.canceled = true
			slot.discarded = true
			close(slot.ready)
			return
		}
	}
}

func (s *Service) consumeSuppressedPersistence(room string, update []byte) bool {
	for {
		s.suppressMu.Lock()
		slots := s.suppressed[room]
		if len(slots) == 0 {
			s.suppressMu.Unlock()
			return false
		}
		slot := slots[0]
		ready := slot.ready
		committedTo := slot.committedTo
		own := committedTo != nil && bytes.Equal(slot.committed, update)
		s.suppressMu.Unlock()

		// A repair's update committed into a room whose persistence worker has retired reaches
		// this adapter on the repair's own goroutine, inside its commit (ygo's persistStranded),
		// and a room retires only once it has left the server. Waiting there for the slot, which
		// that goroutine finishes or discards once its commit returns, would never end. The repair
		// finds its room gone and gives the write up (applySuppressed), so its update is
		// discarded here instead.
		if own && s.srv.GetDoc(room) != committedTo {
			s.suppressMu.Lock()
			if current := s.suppressed[room]; len(current) > 0 && current[0] == slot && !slot.consumed {
				if !slot.canceled && slot.update == nil {
					slot.canceled = true
					close(slot.ready)
				}
				s.consumeHeadSlotLocked(room)
				s.suppressMu.Unlock()
				return true
			}
			s.suppressMu.Unlock()
			continue
		}

		<-ready

		s.suppressMu.Lock()
		slots = s.suppressed[room]
		if len(slots) == 0 || slots[0] != slot {
			s.suppressMu.Unlock()
			continue
		}
		if slot.canceled {
			s.consumeHeadSlotLocked(room)
			discarded := slot.discarded
			s.suppressMu.Unlock()
			return discarded
		}
		if !bytes.Equal(slot.update, update) {
			s.suppressMu.Unlock()
			return false
		}
		s.consumeHeadSlotLocked(room)
		s.suppressMu.Unlock()
		return true
	}
}

// consumeHeadSlotLocked marks room's first suppression slot consumed and removes it. The caller
// holds suppressMu.
func (s *Service) consumeHeadSlotLocked(room string) {
	slots := s.suppressed[room]
	slots[0].consumed = true
	if len(slots) == 1 {
		delete(s.suppressed, room)
		return
	}
	s.suppressed[room] = slots[1:]
}

func (s *Service) cancelSuppressedPersistence(room string, slot *suppressSlot) {
	s.suppressMu.Lock()
	defer s.suppressMu.Unlock()
	if slot == nil || slot.consumed {
		return
	}
	slots := s.suppressed[room]
	for index, candidate := range slots {
		if candidate == slot {
			slots = append(slots[:index], slots[index+1:]...)
			break
		}
	}
	if len(slots) == 0 {
		delete(s.suppressed, room)
	} else {
		s.suppressed[room] = slots
	}
	if !slot.canceled && slot.update == nil {
		slot.canceled = true
		close(slot.ready)
	}
}

// purgeSuppressedPersistence releases callbacks held for a failed room without
// allowing its discarded slot to apply to a successor room with the same name.
func (s *Service) purgeSuppressedPersistence(room string) {
	s.suppressMu.Lock()
	defer s.suppressMu.Unlock()
	for _, slot := range s.suppressed[room] {
		if !slot.canceled && slot.update == nil {
			slot.canceled = true
			close(slot.ready)
		}
	}
	delete(s.suppressed, room)
}

// New constructs the ygo server used by Dispatch's document API and websocket
// endpoint.
func New(deps Deps) *Service {
	settle := deps.Settle
	if settle <= 0 {
		settle = 2 * time.Second
	}
	markWait := deps.MarkWait
	if markWait <= 0 {
		markWait = time.Second
	}
	unrecordedMarkTTL := deps.UnrecordedMarkTTL
	if unrecordedMarkTTL <= 0 {
		unrecordedMarkTTL = time.Minute
	}
	if deps.Events == nil {
		deps.Events = events.NewBroker()
	}
	persist := deps.Persistence
	if persist == nil {
		persist = NewPgVersioned(deps.Store)
	}
	service := &Service{
		store:             deps.Store,
		persistence:       persist,
		events:            deps.Events,
		identity:          deps.Identity,
		agentToken:        deps.AgentToken,
		serverURL:         strings.TrimSuffix(deps.ServerURL, "/"),
		settle:            settle,
		markWait:          markWait,
		now:               time.Now,
		timers:            make(map[uint64]*time.Timer),
		unrecordedMarkTTL: unrecordedMarkTTL,
	}
	adapter := &servicePersistenceAdapter{store: persist, service: service}
	srv := websocket.NewServerWithPersistence(adapter)
	srv.HocuspocusFraming = true
	// Transactional server-side edits suppress one automatic ygo write and
	// append it inside the API transaction, so persistence stays per update.
	srv.PersistCoalesceWindow = -1
	srv.CompactEvery = 200
	// A room decodes its stored history into its own document, so it takes the pending queue
	// every other decode of that history takes (newDocumentCopy). At ygo's default of 100,000 a
	// room refuses a history the room that wrote it served, once a lower-numbered client wrote
	// more items than that after one ygo had to defer (LEGION-502), and the document reads as one
	// whose history cannot load, which offers its rebuild. The queue is maxUpdateItems because ygo
	// refuses any one update declaring more items than that, so no load of a stored history can
	// park past it. It is also the most a room's peers can park in it, about ten times ygo's
	// default; bounding what one peer's update can do to a room is LEGION-487.
	srv.MaxPendingItems = maxUpdateItems
	// A room whose last peer leaves stays resident until it has been idle for roomIdleTimeout.
	// Eager eviction, ygo's default, evicts the room the moment its last peer leaves, even while
	// a Server.Apply is inside its callback on that room (reearth/ygo v1.49.5,
	// provider/websocket/peer.go:477-504 checks peers alone): the callback's write then lands on
	// the evicted room and reaches the store only through its retiring persistence worker, while
	// the next access has already loaded the store without it and serves, and takes, the next
	// write on a state missing the first. The two writes, each made from the same document, merge
	// into a document neither wrote, which can hold no block at all. Idle eviction refuses a
	// room any Apply holds, or has touched since its last peer left (idle_sweep.go:185), so every
	// write a room the sweeper evicts has taken is durable before a successor can load. CloseRoom
	// checks peers alone too (inject.go:475-621), and the service still calls it to close an
	// issue's rooms (SetIssueClosed), for a room with an editor at Shutdown, and to evict one
	// (evictRoom): a write that commits on a room it has retired reaches the store through ygo's
	// stranded persistence, on the committing goroutine (persistence.go:126-163). A published
	// write's suppression slot is finished before ygo's persistence observer runs
	// (onLoadDocument), so that persistence never waits on the publish it is running in.
	srv.RoomIdleTimeout = roomIdleTimeout

	service.srv = srv
	srv.Authorize = service.authorize
	srv.OnTokenAuth = service.authorizeSchemaVersion
	srv.OnInject = service.allowInject
	srv.OnLoadDocument = service.onLoadDocument
	srv.OnLastPeer = service.settleLastPeer
	// ygo calls it once for every room it retires, whichever way: the idle sweep, CloseRoom.
	srv.OnUnloadDocument = service.releaseUnloadedRoom

	return service
}

// shutdownDrainBudget bounds the part of Shutdown that waits on document work - connected peers
// closing, queued durable appends landing, and the settlements owed - inside whatever deadline its
// caller passes.
const shutdownDrainBudget = 5 * time.Second

// Shutdown stops queued settlements, runs the settlement of each loaded room whose document owes
// one inside the drain budget, joins the settlements and evictions already running, and flushes
// ygo's document persistence workers.
//
// A settlement the budget cuts short is not lost. Its database work is cancelled and its
// transaction rolls back, and the pending-settlement row the document's updates wrote
// (markSettlementPending) arms it again on the room's next load (onLoadDocument) or at the next
// resumption (RunSettlementResumption). For each document that owed a settlement Shutdown logs
// whether it settled or was left to resume; once the caller's deadline has passed it cannot read
// that back, and its error names those documents.
func (s *Service) Shutdown(ctx context.Context) error {
	type pendingSettlement struct {
		room       string
		generation uint64
	}
	var pending []pendingSettlement
	var connectedRooms []string
	s.rooms.Range(func(key, value any) bool {
		name := key.(string)
		room := value.(*roomState)
		s.shutdownRooms.Store(name, struct{}{})
		room.mu.Lock()
		if len(room.connected) > 0 {
			connectedRooms = append(connectedRooms, name)
		}
		pending = append(pending, pendingSettlement{room: name, generation: room.gen})
		room.mu.Unlock()
		return true
	})
	s.stopAllSettleTimers()
	drainCtx, cancelDrain := context.WithTimeout(ctx, shutdownDrainBudget)
	defer cancelDrain()
	// drainErr is a peer or a durable append the budget did not see through, which Shutdown
	// cannot leave to a later load; a settlement it cuts short it can.
	var drainErr error
	for _, room := range connectedRooms {
		closed := make(chan error, 1)
		go func(room string) {
			closed <- s.srv.CloseRoom(room, true)
		}(room)
		select {
		case err := <-closed:
			if err != nil && !errors.Is(err, websocket.ErrRoomNotFound) {
				slog.Warn("dispatch: close document peers before shutdown", "room", room, "error", err)
			}
		case <-drainCtx.Done():
			slog.Warn("dispatch: peer close exceeded shutdown budget", "room", room, "error", drainCtx.Err())
			drainErr = drainCtx.Err()
		}
	}
	rooms := make([]string, 0, len(pending))
	for _, settlement := range pending {
		rooms = append(rooms, settlement.room)
		if err := s.waitForDurableAppends(drainCtx, settlement.room); err != nil {
			slog.Warn("dispatch: stop document settlement before durable append drain", "room", settlement.room, "error", err)
			drainErr = err
		}
	}
	// Only a document that owes a settlement is settled. One whose updates have all been settled
	// would spend the budget repeating that work - a 1 MiB document's for seconds, one at a time
	// per issue, since each holds the issue's row - and push the settlement that is owed past it.
	// When the read fails every room is settled, since nothing says which can be skipped.
	owed, err := s.roomsOwingSettlement(drainCtx, rooms)
	if err != nil {
		slog.Warn("dispatch: read the documents owing a settlement before shutdown", "error", err)
	}
	finished := make(chan string, len(pending))
	running := make(map[string]bool, len(pending))
	for _, settlement := range pending {
		if owed != nil && !owed[settlement.room] {
			continue
		}
		running[settlement.room] = true
		go func(room string, generation uint64) {
			s.settleRoomWithin(drainCtx, room, generation)
			finished <- room
		}(settlement.room, settlement.generation)
	}
	budget := drainCtx.Done()
	for len(running) > 0 {
		select {
		case room := <-finished:
			delete(running, room)
		case <-budget:
			// The cancelled settlements still have to return: one may be rendering, which
			// no context interrupts, and the store has to outlast it.
			budget = nil
		case <-ctx.Done():
			s.stopAccepting()
			return settlementsUnconfirmed(ctx.Err(), owed, rooms)
		}
	}
	// A settlement a timer started before the timers stopped runs under no budget of its own; it
	// gets what is left of this one before the gate closes and abandons it.
	waitGroup(drainCtx, &s.settleWG)
	s.stopAccepting()
	s.waitSettles(ctx)
	// A room that failed evicts itself on its own goroutine, and that eviction flushes the
	// room through the store, so it has to finish before the store can go.
	s.waitEvictions(ctx)
	if err := ctx.Err(); err != nil {
		return settlementsUnconfirmed(err, owed, rooms)
	}
	s.reportShutdownSettlements(ctx, owed, rooms, drainCtx.Err() != nil)
	if drainErr != nil {
		return drainErr
	}
	return s.srv.Shutdown(ctx)
}

// roomsOwingSettlement is which of rooms owe a settlement no settlement has committed.
func (s *Service) roomsOwingSettlement(ctx context.Context, rooms []string) (map[string]bool, error) {
	pending := make(map[string]bool)
	if len(rooms) == 0 {
		return pending, nil
	}
	rows, err := s.store.Pool.Query(ctx, `
		select artifact_id::text from doc_settlements_pending where artifact_id::text = any($1)
	`, rooms)
	if err != nil {
		return nil, fmt.Errorf("read pending document settlements: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var room string
		if err := rows.Scan(&room); err != nil {
			return nil, fmt.Errorf("scan pending document settlement: %w", err)
		}
		pending[room] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read pending document settlements: %w", err)
	}
	return pending, nil
}

// reportShutdownSettlements logs, for each document that owed a settlement when Shutdown's
// settlements started, whether it settled or is left to resume, and whether the drain budget ended
// first. owed is nil when Shutdown could not read it, and then only the documents left are named.
func (s *Service) reportShutdownSettlements(ctx context.Context, owed map[string]bool, rooms []string, budgetEnded bool) {
	left, err := s.roomsOwingSettlement(ctx, rooms)
	if err != nil {
		slog.Warn("dispatch: read the documents left owing a settlement at shutdown", "error", err)
		return
	}
	for _, room := range rooms {
		switch {
		case left[room]:
			slog.Warn("dispatch: document settlement left to resume after shutdown",
				"room", room, "shutdown_budget_ended", budgetEnded)
		case owed[room]:
			slog.Info("dispatch: document settled before shutdown", "room", room)
		}
	}
}

// settlementsUnconfirmed is the error of a Shutdown whose caller's deadline passed before it could
// read back which settlements committed. It names the documents that owed one, every room when
// that read failed too; each that did not settle resumes from its pending-settlement row.
func settlementsUnconfirmed(cause error, owed map[string]bool, rooms []string) error {
	named := make([]string, 0, len(rooms))
	for _, room := range rooms {
		if owed == nil || owed[room] {
			named = append(named, room)
		}
	}
	if len(named) == 0 {
		return cause
	}
	sort.Strings(named)
	return fmt.Errorf("document settlement unconfirmed at shutdown for %s; one that did not commit resumes from its pending-settlement row: %w",
		strings.Join(named, ", "), cause)
}

// Quiesce closes every live document room, flushing each through the store, and waits for the
// settlements already in flight to finish. Unlike Shutdown it leaves the service able to load
// rooms again, so the next document read starts from what the database now holds.
//
// Nothing in production calls it. It exists so the browser-test harness can truncate its
// database between scenarios without racing a settlement midway through its own transaction:
// a settlement locks the document's owner row and then reads artifact_versions, while TRUNCATE
// takes an exclusive lock on every table in its own order, and PostgreSQL resolves the crossing
// by aborting one of them (LEGION-168).
func (s *Service) Quiesce(ctx context.Context) error {
	s.quiescing.Store(true)
	defer s.quiescing.Store(false)
	s.stopAllSettleTimers()
	var firstErr error
	s.rooms.Range(func(key, value any) bool {
		room := key.(string)
		state := value.(*roomState)
		state.mu.Lock()
		// A settlement whose timer already fired reads the generation it was armed with, so
		// bumping it here ends that settlement before it opens a transaction.
		state.gen++
		s.stopSettleTimer(state.settle)
		state.mu.Unlock()
		if err := s.evictRoom(room, state); err != nil && firstErr == nil {
			firstErr = err
		}
		return true
	})
	s.waitSettles(ctx)
	s.waitEvictions(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	return firstErr
}

func (s *Service) scheduleSettle(room string) {
	if s.stopping.Load() || s.quiescing.Load() || s.shuttingDown(room) {
		return
	}
	state := s.lockState(room)
	defer s.unlockState(room, state)
	s.scheduleSettleLocked(room, state)
}

func (s *Service) scheduleSettleLocked(room string, state *roomState) {
	s.scheduleSettleAfterLocked(room, state, s.settle)
}

func (s *Service) scheduleSettleAfterLocked(room string, state *roomState, delay time.Duration) {
	if s.stopping.Load() || s.quiescing.Load() || s.shuttingDown(room) || state.closed || state.failed != nil {
		return
	}
	if state.liveWriter != nil {
		state.settleDeferred = true
		return
	}
	// The advisory read above skips the work; this one registers the timer against Shutdown's
	// store, so the Add cannot land after waitSettles has begun.
	if !s.addUnlessStopping(&s.settleWG) {
		return
	}
	state.gen++
	generation := state.gen
	s.stopSettleTimer(state.settle)
	timerID := s.nextSettleTimer.Add(1)
	s.registerSettleTimer(timerID)
	timer := time.AfterFunc(delay, func() {
		defer s.settleWG.Done()
		// An armed timer holds its state (unusedLocked), so the state can go only once the timer
		// has left the register.
		defer func() {
			s.unregisterSettleTimer(timerID)
			s.releaseIfUnused(room)
		}()
		if current, _ := s.rooms.Load(room); current != state {
			s.scheduleSettle(room)
			return
		}
		s.settleRoom(room, generation)
	})
	state.settle = timer
	s.attachSettleTimer(timerID, timer)
}

func (s *Service) registerSettleTimer(timerID uint64) {
	s.timerMu.Lock()
	s.timers[timerID] = nil
	s.timerMu.Unlock()
}

func (s *Service) attachSettleTimer(timerID uint64, timer *time.Timer) {
	s.timerMu.Lock()
	if _, found := s.timers[timerID]; found {
		s.timers[timerID] = timer
	}
	s.timerMu.Unlock()
}

func (s *Service) unregisterSettleTimer(timerID uint64) {
	s.timerMu.Lock()
	delete(s.timers, timerID)
	s.timerMu.Unlock()
}

func (s *Service) stopSettleTimer(timer *time.Timer) bool {
	if timer == nil {
		return false
	}
	s.timerMu.Lock()
	defer s.timerMu.Unlock()
	for timerID, armed := range s.timers {
		if armed != timer {
			continue
		}
		if !timer.Stop() {
			return false
		}
		delete(s.timers, timerID)
		s.settleWG.Done()
		return true
	}
	return false
}

func (s *Service) stopAllSettleTimers() {
	s.timerMu.Lock()
	timers := make([]*time.Timer, 0, len(s.timers))
	for _, timer := range s.timers {
		if timer != nil {
			timers = append(timers, timer)
		}
	}
	s.timerMu.Unlock()
	for _, timer := range timers {
		s.stopSettleTimer(timer)
	}
}

// isSettleTimerArmed is whether timer, a state's settle timer, is registered and not yet fired or
// stopped. A state that never armed one has none: the register's nil entries are other rooms'
// timers between their registration and attachSettleTimer.
func (s *Service) isSettleTimerArmed(timer *time.Timer) bool {
	if timer == nil {
		return false
	}
	s.timerMu.Lock()
	defer s.timerMu.Unlock()
	for _, armed := range s.timers {
		if armed == timer {
			return true
		}
	}
	return false
}

func (s *Service) scheduleSettleAfterAppend(room string) {
	if s.stopping.Load() || s.shuttingDown(room) {
		return
	}
	state := s.lockState(room)
	defer s.unlockState(room, state)
	if s.isSettleTimerArmed(state.settle) {
		return
	}
	s.scheduleSettleLocked(room, state)
}

func (s *Service) retrySettleSoon(room string) {
	if s.stopping.Load() || s.shuttingDown(room) {
		return
	}
	state := s.lockState(room)
	defer s.unlockState(room, state)
	s.scheduleSettleAfterLocked(room, state, 10*time.Millisecond)
}

func (s *Service) retrySettle(room string, generation uint64, err error) {
	state := s.lockState(room)
	defer s.unlockState(room, state)
	s.retrySettleLocked(room, state, generation, err)
}

func (s *Service) retrySettleLocked(room string, state *roomState, generation uint64, err error) {
	if errors.Is(err, errRoomReplaced) {
		// Expected, not a fault: the settlement wrote nothing (one that wrote fails the room
		// instead) because the room it read was replaced first, and the replacement's load arms
		// the settlement the document still owes.
		slog.Info("dispatch: document settlement wrote nothing into a replaced room", "room", room, "error", err)
	} else {
		slog.Error("dispatch: settle document", "room", room, "error", err)
	}
	if state.gen != generation {
		return
	}
	state.settleFailures++
	if state.settleFailures >= maxSettleFailures {
		s.failRoomLocked(room, state, fmt.Errorf("document settlement failed %d times: %w", state.settleFailures, err))
		return
	}
	s.scheduleSettleLocked(room, state)
}

// ArtifactVersionEventPayload is the one shape an `artifact.version` event is built from,
// wherever it is appended, and it takes what the write moved in the reference graph, so no
// producer can stay silent about the batched backlink counts it changed.
func ArtifactVersionEventPayload(
	artifactID, name string,
	version model.Version,
	diff *string,
	changes model.ReferenceChanges,
) map[string]any {
	payload := map[string]any{"artifact_id": artifactID, "name": name, "version": version}
	if diff != nil {
		payload["diff"] = *diff
	}
	model.NameReferenceChanges(payload, changes)
	return payload
}

// stampBlockIDs stamps the block ids doc's tree lacks or repeats on the tree as it stands
// (rewriteLive), and returns the stamped tree with how many ids it minted. Its caller decides
// from a read of its own whether any id needs repair, so a document that needs none opens no
// transaction.
func stampBlockIDs(doc *crdt.Doc, origin any) (*pmdoc.Node, int, error) {
	minted := 0
	stamped, _, err := rewriteLive(doc, origin, func(live *pmdoc.Node) bool {
		minted = pmdoc.EnsureBlockIDsCount(live)
		return minted > 0
	})
	return stamped, minted, err
}

// settlementAuthors copies state's pending authors, which a settlement's version credits, and names
// the actor its events carry: the first of those authors, or else the room's latest editor. The
// caller holds state.mu.
func settlementAuthors(state *roomState) (map[string]model.Actor, []model.Actor, model.Actor) {
	pending := make(map[string]model.Actor, len(state.pending))
	for key, actor := range state.pending {
		pending[key] = actor
	}
	authors := actorSlice(pending)
	actor := model.Actor{}
	if len(authors) > 0 {
		actor = authors[0]
	} else if state.lastActor != nil {
		actor = *state.lastActor
	}
	return pending, authors, actor
}

func (s *Service) settleRoom(room string, generation uint64) {
	s.settleRoomWithin(context.Background(), room, generation)
}

// settleRoomWithin settles room under parent. A timer's settlement runs under no deadline; the one
// Shutdown runs gets its drain budget. When parent ends first the settlement's database work is
// cancelled and its transaction rolls back, and nothing retries it or counts it as a failure: the
// document's pending-settlement row leaves it to resume (onLoadDocument, RunSettlementResumption).
func (s *Service) settleRoomWithin(parent context.Context, room string, generation uint64) {
	retry := func(err error) {
		if parent.Err() != nil {
			slog.Warn("dispatch: document settlement stopped at the shutdown budget", "room", room, "error", err)
			return
		}
		s.retrySettle(room, generation, err)
	}
	state := s.lockState(room)
	if s.stopping.Load() || state.closed || state.failed != nil || state.gen != generation {
		s.unlockState(room, state)
		return
	}
	state.settling++
	s.unlockState(room, state)
	defer func() {
		state.mu.Lock()
		state.settling--
		s.unlockState(room, state)
	}()
	if s.shuttingDown(room) && s.srv.GetDoc(room) == nil {
		return
	}
	if s.srv.GetDoc(room) == nil {
		state.mu.Lock()
		state.settleWarming = true
		s.unlockState(room, state)
		err := s.srv.Apply(parent, room, func(_ *crdt.Doc, _ func(func(*crdt.Transaction))) {})
		state.mu.Lock()
		state.settleWarming = false
		s.unlockState(room, state)
		if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
			retry(fmt.Errorf("warm document for settlement: %w", err))
			return
		}
	}
	// Hold the live document for the whole settlement. An Evict that lands after the
	// generation check above (its timer Stop misses a timer that already fired) closes the
	// room and bumps the generation; reading the room again later would return nil and every
	// later step would dereference it. A nil here means exactly that eviction: the next access
	// reloads the durable document and schedules its own settlement, so this one just ends.
	if s.afterSettleWarm != nil {
		s.afterSettleWarm(room)
	}
	doc := s.srv.GetDoc(room)
	if doc == nil {
		return
	}
	if s.hasPendingUpdates(room) {
		pendingCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		err := s.waitForPendingUpdates(pendingCtx, room)
		cancel()
		if err != nil {
			if s.shuttingDown(room) {
				slog.Warn("dispatch: skip shutdown document settlement before persistence queue drains", "room", room, "error", err)
				return
			}
			s.retrySettleSoon(room)
			return
		}
	}
	if s.hasDurableAppend(room) {
		appendCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		err := s.waitForDurableAppends(appendCtx, room)
		cancel()
		if err != nil {
			if s.shuttingDown(room) {
				slog.Warn("dispatch: skip shutdown document settlement before durable append", "room", room, "error", err)
				return
			}
			s.retrySettleSoon(room)
			return
		}
	}
	state.mu.Lock()
	generation = state.gen
	s.unlockState(room, state)

	ledger := &Ledger{service: s, settling: true}
	ctx := store.WithTransactionTracking(withLedger(parent, ledger))
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		retry(fmt.Errorf("begin document transaction: %w", err))
		return
	}
	defer tx.Rollback(ctx)
	owner, open, err := lockArtifactOwner(ctx, tx, room)
	if err != nil {
		retry(err)
		return
	}
	if !open {
		return
	}
	// The owner row is read and locked in this transaction, so the repairs settlement injects
	// below do not read it again: that read would be a second pooled connection taken while
	// this transaction holds one, which is what deadlocks the pool.
	ctx = withOwnerVerified(ctx)
	latest, err := latestVersion(ctx, tx, room)
	if err != nil {
		retry(err)
		return
	}
	if err := s.lockSettlementCursor(ctx, tx, room); err != nil {
		if s.shuttingDown(room) {
			slog.Warn("dispatch: skip shutdown document settlement while cursor lock is held", "room", room, "error", err)
			return
		}
		retry(fmt.Errorf("lock document cursor: %w", err))
		return
	}
	if s.afterSettleLock != nil {
		s.afterSettleLock(room)
	}
	if s.hasPendingUpdates(room) {
		if s.shuttingDown(room) {
			slog.Warn("dispatch: skip shutdown document settlement with undurable updates", "room", room)
			return
		}
		s.scheduleSettle(room)
		return
	}
	snapshotCursor, err := currentUpdateCursor(ctx, tx, room)
	if err != nil {
		retry(err)
		return
	}
	// The settlement reads the document three times, and each read has its own name: read is this
	// one, before the database work; reconciled is the tree the ask blocks are reconciled on, the
	// stamp's when it stamped ids; versioned is the one the version is rendered from, read again
	// after the repairs when the settlement wrote any. A browser's edit made in between is in a
	// later one and not an earlier one. read and versioned are each taken from a copy under the
	// document's lock (lockedTreeOf), and the stamp reads inside its own transaction: the room's
	// peers and the service can write it while a walk of the live tree, which takes no lock, reads
	// it, and a torn read would be versioned as the document.
	read, err := lockedTreeOf(doc)
	if err != nil {
		if errors.Is(err, ErrDocSchema) {
			slog.Error("dispatch: settle document outside Proof schema", "room", room, "error", err)
			return
		}
		retry(err)
		return
	}

	// What the document renders before its closure runs. The closure's own update is classified
	// against it (closureChangedMarkdown), as a transactional live write classifies its own
	// (applyJoined): a `doc_updates` row says whether the update changed the rendered markdown,
	// and a repair that renders the document exactly as it was changed none. A row that claimed
	// otherwise would sit past every later version's cursor, since no version follows it to move
	// the cursor, and the first settlement after a renderer change would version a document
	// nobody had touched.
	beforeMarkdown, beforeRenderErr := renderTree(read)
	if s.afterSettleRead != nil {
		s.afterSettleRead(room)
	}

	// slots holds a suppression slot for each update this settlement writes into the room, in the
	// order the room's persistence worker is handed them, and updates holds those updates. Every
	// path after a write finishes or discards all of them.
	var slots []*suppressSlot
	var updates [][]byte
	keep := func(slot *suppressSlot, update []byte) {
		if slot != nil {
			slots, updates = append(slots, slot), append(updates, update)
		}
	}
	finishSlots := func() {
		for index, slot := range slots {
			s.finishSuppressedPersistence(slot, updates[index])
		}
	}
	// abandon ends a settlement that cannot finish. What it wrote is in the room and its browsers
	// and will never reach the store through this settlement, so its slots are discarded and the
	// room fails, which reloads the document from the store and leaves its settlement to that load.
	// A settlement that wrote nothing is retried.
	abandon := func(err error) {
		if len(slots) > 0 {
			s.discardSuppressedPersistence(room, slots...)
			s.failRoom(room, err)
			return
		}
		retry(err)
	}
	reconciled := read
	if pmdoc.BlockIDRepairCount(read) > 0 {
		slot, update, err := s.applySuppressed(ctx, room, doc, func(doc *crdt.Doc, origin any) (bool, error) {
			var minted int
			var stampErr error
			reconciled, minted, stampErr = stampBlockIDs(doc, origin)
			return minted > 0, stampErr
		})
		keep(slot, update)
		if err != nil {
			if len(slots) == 0 && errors.Is(err, ErrDocSchema) {
				slog.Error("dispatch: settle document outside Proof schema", "room", room, "error", err)
				return
			}
			abandon(err)
			return
		}
	}

	state.mu.Lock()
	if s.stopping.Load() || state.closed || state.failed != nil {
		s.unlockState(room, state)
		s.discardSuppressedPersistence(room, slots...)
		return
	}
	// An open live write is not in the room yet, and since this settlement holds the owner row,
	// the write's transaction has already committed: the cursor this settlement versions against
	// includes its row. Write no version; finishLiveWrite arms a settlement once it is published.
	superseded := state.gen != generation
	if state.liveWriter != nil {
		state.settleDeferred = true
		superseded = true
	}
	if superseded {
		s.unlockState(room, state)
		if len(slots) == 0 {
			return
		}
		identityUpdate, err := mergeUpdates(updates)
		if err != nil {
			abandon(err)
			return
		}
		if err := s.srv.BroadcastUpdate(ctx, room, identityUpdate); err != nil {
			abandon(fmt.Errorf("broadcast superseded document identity update: %w", err))
			return
		}
		identityChanged := closureChangedMarkdown(beforeMarkdown, beforeRenderErr, reconciled)
		if _, err := s.persistence.AppendUpdateTx(ctx, tx, room, identityUpdate, identityChanged); err != nil {
			abandon(err)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			abandon(fmt.Errorf("commit superseded document identity update: %w", err))
			return
		}
		finishSlots()
		return
	}
	pending, authors, eventActor := settlementAuthors(state)
	s.unlockState(room, state)
	reconciliation, err := s.reconcileAskBlocks(ctx, tx, room, owner, reconciled, eventActor)
	if err != nil {
		abandon(err)
		return
	}
	if s.afterSettleReconcile != nil {
		s.afterSettleReconcile(room)
	}
	if len(reconciliation.repairs) > 0 {
		// The repairs are made on the document as it stands (writeLive): the tree they were
		// reconciled on was read before settlement's database work.
		slot, update, err := s.applySuppressed(ctx, room, doc, reconciliation.writeLive)
		keep(slot, update)
		if err != nil {
			abandon(fmt.Errorf("write reconciled typed blocks: %w", err))
			return
		}
	}

	versioned := reconciled
	if len(slots) > 0 {
		update, err := mergeUpdates(updates)
		if err != nil {
			abandon(err)
			return
		}
		if err := s.srv.BroadcastUpdate(ctx, room, update); err != nil {
			abandon(fmt.Errorf("broadcast document closure update: %w", err))
			return
		}
		closureChanged := closureChangedMarkdown(beforeMarkdown, beforeRenderErr, reconciled)
		if _, err := s.persistence.AppendUpdateTx(ctx, tx, room, update, closureChanged); err != nil {
			abandon(err)
			return
		}
		snapshotCursor, err = currentUpdateCursor(ctx, tx, room)
		if err != nil {
			abandon(err)
			return
		}
		// The version is the document as it stands after the repairs, read under its lock, not the
		// tree settlement reconciled before them: a peer's edit made since is in the room, its
		// browsers and its stored updates, and a version rendered from the earlier tree would leave
		// it out (LEGION-479).
		versioned, err = lockedTreeOf(doc)
		if err != nil {
			abandon(err)
			return
		}
		// That tree holds the edits made since the authors were taken above, so the version is
		// credited to their authors too, taken again with the tree: an edit's own settlement finds
		// the document already versioned and writes no version to credit them on. They are taken
		// here, not after the render, where an edit made while the version renders would be
		// credited on this version, which lacks it, rather than on the version its own settlement
		// writes. ygo runs the update observer that credits an edit after the edit's transaction
		// releases the document, so an edit this tree holds that its observer has not yet credited
		// is not credited here; its author stays pending for a later version.
		state.mu.Lock()
		pending, authors, eventActor = settlementAuthors(state)
		s.unlockState(room, state)
		if s.afterSettleVersionRead != nil {
			s.afterSettleVersionRead(room)
		}
	}
	markdown, err := renderTree(versioned)
	if err != nil {
		if len(slots) == 0 && errors.Is(err, ErrDocSchema) {
			slog.Error("dispatch: settle document outside Proof schema", "room", room, "error", err)
			return
		}
		abandon(err)
		return
	}

	state.mu.Lock()
	// A settlement that wrote into the room commits what it wrote even when the document has moved
	// since its read: the room and its peers hold those updates, and dropped here they would never
	// reach the store. Its version is the document as it stood after the repairs, the move
	// included; the move's own update is appended once this transaction releases the room lock.
	// Should that append fail, the room fails and reloads without the move's text, which the
	// version holds, until the browser that made it resends it on reconnecting.
	if s.stopping.Load() || state.closed || state.failed != nil || (state.gen != generation && len(slots) == 0) {
		s.unlockState(room, state)
		s.discardSuppressedPersistence(room, slots...)
		return
	}
	s.unlockState(room, state)

	published := make([]model.Event, 0, len(reconciliation.events)+1)
	// Each event that carries a reconciled source's text stamps that source's new mention edges
	// with the event that introduced them: the version event for the document itself, and each
	// ask's own opened/edited event. writeVersionTx reconciled the edges earlier in this
	// transaction, so stamping runs after the append and never touches sequence allocation.
	appendEvents := func(events []model.Event) error {
		for _, planned := range events {
			appended, appendErr := s.events.Append(ctx, tx, planned)
			if appendErr != nil {
				return appendErr
			}
			if sourceKind, sourceID, ok := referenceSource(planned, room); ok {
				if err := refs.Stamp(ctx, tx, sourceKind, sourceID, appended.ID); err != nil {
					return err
				}
			}
			published = append(published, appended)
		}
		return nil
	}
	// Settlement writes a version only when the document now reads differently from the latest
	// one. Indexing an ask block writes an `asks` row and an `ask.opened` event over words the
	// edit that wrote the block already versioned, so a version for that event alone would be
	// byte-identical, credited to nobody, and stale an approval pinned to what an agent had just
	// written (LEGION-273). Its tree writes - stamping block ids, restoring an ask block's
	// server-owned attributes - move the stored Proof state and often render the same markdown,
	// and a version for one of those would repeat the version before it too (LEGION-229
	// requirement 2).
	// contentChanged stays the first half of the test: a version an older renderer wrote is not
	// this settlement's to canonicalise when nothing has touched the document since.
	contentChanged, err := contentChangedSinceVersion(ctx, tx, room, latest.docUpdateVersion)
	if err != nil {
		abandon(err)
		return
	}
	versioning := contentChanged && markdown != latest.markdown
	settledVersion := latest.Number
	if versioning {
		settledVersion++
	}
	if err := reconciliation.nameVersion(ctx, tx, room, owner, settledVersion); err != nil {
		abandon(err)
		return
	}
	if versioning {
		result, writeErr := s.writeVersionTx(ctx, tx, room, markdown, versioned, eventActor, &versionWrite{
			authors:          authors,
			docUpdateVersion: &snapshotCursor,
		})
		if writeErr != nil {
			abandon(writeErr)
			return
		}
		published = append(published, ledger.events...)
		// A document body cites nodes whose rows carry a backlink count, so the version event
		// names what this settle moved exactly as a message or comment write does.
		versionEvent := model.Event{
			IssueKey: owner.IssueKey,
			Type:     "artifact.version",
			Actor:    eventActor,
			Payload:  ArtifactVersionEventPayload(room, owner.Name, result.version, nil, result.changes),
		}
		if owner.IssueKey == nil {
			versionEvent.ArtifactID = &room
		}
		if err := appendEvents([]model.Event{versionEvent}); err != nil {
			abandon(err)
			return
		}
	}
	if err := appendEvents(reconciliation.events); err != nil {
		abandon(err)
		return
	}
	// Every update this settlement read is settled once it commits, so the row that left the
	// settlement to a later load goes with that commit.
	if err := clearSettlementPending(ctx, tx, room); err != nil {
		abandon(err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		abandon(fmt.Errorf("commit document settlement: %w", err))
		return
	}
	finishSlots()
	// This release runs before the publish below, the order Ledger.Commit keeps for every other
	// version write, so a subscriber acting on this version's artifact.version event acts after
	// it. Publishing first would let that subscriber's write be credited to these authors again.
	state.mu.Lock()
	if state.gen == generation {
		state.settleFailures = 0
		for key := range pending {
			delete(state.pending, key)
		}
		// An author credited after this settlement read the room's still waits for the next.
		state.unsettled = len(state.pending) > 0
	}
	s.unlockState(room, state)
	for _, event := range published {
		s.events.Publish(event)
	}
	s.sweepUnrecordedMarks(room, versioned)
}

func currentUpdateCursor(ctx context.Context, tx pgx.Tx, artifactID string) (int64, error) {
	var cursor int64
	if err := tx.QueryRow(ctx, `
		select coalesce(max(version), 0) from doc_updates where artifact_id = $1
	`, artifactID).Scan(&cursor); err != nil {
		return 0, fmt.Errorf("read document update cursor: %w", err)
	}
	return cursor, nil
}

func (s *Service) lockSettlementCursor(ctx context.Context, tx pgx.Tx, room string) error {
	if !s.shuttingDown(room) {
		return lockDocumentRoom(ctx, tx, room)
	}
	// Shutdown will not wait behind a live writer, so it try-locks the room instead of blocking
	// on it, and gives up after the deadline rather than queueing. That is what makes this
	// branch safe: it never joins a lock queue, so it can never be a party to a cycle.
	deadline := time.NewTimer(100 * time.Millisecond)
	defer deadline.Stop()
	for {
		var locked bool
		if err := tx.QueryRow(ctx, `select pg_try_advisory_xact_lock(hashtext($1))`, room).Scan(&locked); err != nil {
			return err
		}
		if locked {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("document cursor lock remained held during shutdown")
		case <-time.After(time.Millisecond):
		}
	}
}

// referenceSource names the reference source whose mention edges an event introduces: the
// document for its version event, an ask for its opened or edited event. Other events (answers,
// resolutions, block repairs) change no text and stamp nothing.
func referenceSource(event model.Event, artifactID string) (string, string, bool) {
	switch event.Type {
	case "artifact.version":
		return "artifact", artifactID, true
	case "ask.opened":
		if opened, ok := event.Payload.(model.AskEventPayload); ok {
			return "ask", opened.Ask.ID, true
		}
	case "ask.edited":
		if edit, ok := event.Payload.(model.AskEditEventPayload); ok {
			return "ask", edit.Ask.ID, true
		}
	}
	return "", "", false
}

// ScheduleSettlement queues the document closer after its caller's transaction commits.
func (s *Service) ScheduleSettlement(artifactID string) {
	s.scheduleSettle(artifactID)
}

type BlockIDBackfill struct {
	ArtifactID string
	Stamped    int
	Skipped    string
	Err        error
}

// BackfillBlockIDs runs the identity closure against every document. A document
// failure is reported with that document so later documents can still be stamped.
func (s *Service) BackfillBlockIDs(ctx context.Context) ([]BlockIDBackfill, error) {
	ctx = store.WithTransactionTracking(ctx)
	rows, err := s.store.Pool.Query(ctx, `select id::text from artifacts where kind = 'doc' order by id`)
	if err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	var artifactIDs []string
	for rows.Next() {
		var artifactID string
		if err := rows.Scan(&artifactID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan document: %w", err)
		}
		artifactIDs = append(artifactIDs, artifactID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate documents: %w", err)
	}
	rows.Close()

	result := make([]BlockIDBackfill, 0, len(artifactIDs))
	for _, artifactID := range artifactIDs {
		result = append(result, s.backfillBlockIDs(ctx, artifactID))
	}
	return result, nil
}

func (s *Service) backfillBlockIDs(ctx context.Context, artifactID string) BlockIDBackfill {
	report := BlockIDBackfill{ArtifactID: artifactID}
	if err := s.awaitRoomRecovery(ctx, artifactID); err != nil {
		report.Err = fmt.Errorf("recover document: %w", err)
		return report
	}
	if s.stopping.Load() {
		report.Skipped = "service stopping"
		return report
	}

	backfillCtx := withOwnerVerified(ctx)
	// The backfill writes the same closure the settlement does, so its row is classified the same
	// way: stamping a block a rendering never names changes no text, while re-minting a typed
	// block's repeated id changes the `#id` its directive carries.
	var stampedChanged bool
	slot, update, err := s.applySuppressed(backfillCtx, artifactID, nil, func(doc *crdt.Doc, origin any) (bool, error) {
		read, err := lockedTreeOf(doc)
		if err != nil {
			return false, err
		}
		if pmdoc.BlockIDRepairCount(read) == 0 {
			return false, nil
		}
		if s.afterBackfillRead != nil {
			s.afterBackfillRead(artifactID)
		}
		before, beforeErr := renderTree(read)
		stamped, minted, err := stampBlockIDs(doc, origin)
		report.Stamped = minted
		if err != nil {
			return false, err
		}
		stampedChanged = closureChangedMarkdown(before, beforeErr, stamped)
		return minted > 0, nil
	})
	if slot == nil {
		if err != nil {
			report.Err = fmt.Errorf("stamp document: %w", err)
		}
		return report
	}
	// abandon gives up a stamp that is in the room but will not reach the store through the
	// backfill: its slot is discarded and the room fails, which reloads the document from the store.
	abandon := func(err error) BlockIDBackfill {
		s.discardSuppressedPersistence(artifactID, slot)
		s.failRoom(artifactID, err)
		report.Err = err
		return report
	}
	if err != nil {
		return abandon(fmt.Errorf("stamp document: %w", err))
	}
	if err := s.srv.BroadcastUpdate(backfillCtx, artifactID, update); err != nil {
		return abandon(fmt.Errorf("broadcast identity update: %w", err))
	}
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return abandon(fmt.Errorf("begin document transaction: %w", err))
	}
	defer tx.Rollback(ctx)
	if _, err := s.persistence.AppendUpdateTx(ctx, tx, artifactID, update, stampedChanged); err != nil {
		return abandon(fmt.Errorf("append identity update: %w", err))
	}
	if err := tx.Commit(ctx); err != nil {
		return abandon(fmt.Errorf("commit identity update: %w", err))
	}
	s.finishSuppressedPersistence(slot, update)
	return report
}

// addUnlessStopping registers one worker with group unless Shutdown has already begun, and
// reports whether it did. The check and the Add are one step against Shutdown's stopping store
// (gateMu), so an Add can never follow the Wait that store precedes.
func (s *Service) addUnlessStopping(group *sync.WaitGroup) bool {
	s.gateMu.Lock()
	defer s.gateMu.Unlock()
	if s.stopping.Load() {
		return false
	}
	group.Add(1)
	return true
}

// stopAccepting closes the gate: after it returns, no addUnlessStopping registers a worker, so a
// Wait that follows cannot race an Add. It holds gateMu around the store alone - never across
// work that locks a room, which would invert the room-then-gate order.
func (s *Service) stopAccepting() {
	s.gateMu.Lock()
	defer s.gateMu.Unlock()
	s.stopping.Store(true)
}

func (s *Service) waitSettles(ctx context.Context) {
	waitGroup(ctx, &s.settleWG)
}

func (s *Service) waitEvictions(ctx context.Context) {
	waitGroup(ctx, &s.evictWG)
}

// waitGroup blocks until wg drains or ctx ends, so a bounded shutdown never waits forever on
// work it cannot cancel.
func waitGroup(ctx context.Context, wg *sync.WaitGroup) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// SetIssueClosed refreshes the closed state every room of an issue remembers, and closes them when
// the issue closes. A document with no state has no room to tell: its next load reads the issue.
// Its caller runs it after its own transaction has committed, and it takes that caller's context
// so the pool can see it: a caller that ever runs it with a transaction still open is refused, not
// wedged.
func (s *Service) SetIssueClosed(ctx context.Context, issueKey string, closed bool) {
	rooms, err := s.documentRooms(ctx, "the issue's document rooms", `
		select id::text from artifacts where issue_key = $1 and kind = 'doc'
	`, issueKey)
	if err != nil {
		// Every room keeps the closed flag it already had. A caller that ran this while it
		// still held a connection is refused (store.ErrNestedAcquire) and has to be able to
		// see which issue went stale.
		slog.Error("dispatch: refresh the closed state of an issue's rooms",
			"issue", issueKey, "error", err)
		return
	}
	for _, room := range rooms {
		state := s.lockExistingState(room)
		changed := true
		if state != nil {
			changed = state.closed != closed
			state.closed = closed
			if closed && changed {
				state.gen++
				s.stopSettleTimer(state.settle)
				// A closed issue's documents settle nothing, so the authors waiting for this
				// document's settlement no longer hold its state; its pending-settlement row
				// settles it once the issue reopens (RunSettlementResumption) or the room loads.
				clear(state.pending)
				state.unsettled = false
			}
			s.unlockState(room, state)
		}
		// A room still loading has no state yet, and is closed once it has loaded.
		if closed && changed {
			_ = s.srv.CloseRoom(room, true)
		}
	}
}

// Evict closes a live room and discards its resident state so the next access reloads the
// durable document without treating the room as failed. No production code calls it: it exists
// so tests, including those in package api, can force a room to reload.
func (s *Service) Evict(_ context.Context, artifactID string) error {
	state := s.lockExistingState(artifactID)
	if state != nil {
		state.gen++
		s.stopSettleTimer(state.settle)
		s.unlockState(artifactID, state)
	}
	return s.evictRoom(artifactID, state)
}

func (s *Service) evictRoom(room string, state *roomState) error {
	// Close first, then remove the state: the close's flush-before-evict consults the state's
	// persistence-suppression slot, so a state removed before the close would let a failed
	// settlement's discarded identity update reach the store. A settlement armed on this
	// state during the close is re-armed by its own timer (see scheduleSettleAfterLocked).
	err := s.srv.CloseRoom(room, true)
	if state != nil {
		state.mu.Lock()
		if !state.released {
			s.forgetLocked(room, state)
		}
		state.mu.Unlock()
	}
	if err != nil && !errors.Is(err, websocket.ErrRoomNotFound) {
		return fmt.Errorf("evict live document: %w", err)
	}
	return nil
}

func (s *Service) failRoom(room string, cause error) {
	state := s.lockState(room)
	defer s.unlockState(room, state)
	s.failRoomLocked(room, state, cause)
}

func (s *Service) failRoomLocked(room string, state *roomState, cause error) {
	if state.failed != nil {
		return
	}
	state.failed = cause
	state.failedDone = make(chan struct{})
	done := state.failedDone
	state.gen++
	s.stopSettleTimer(state.settle)
	s.purgeSuppressedPersistence(room)
	// The failure drops this room's settlement: a queued one is stopped just above, one
	// already running refuses on the failed room (settleRoom), and its retry stops because
	// the generation moved (retrySettleLocked). The document's pending-settlement row, which
	// only a committed settlement deletes, makes the replacement's load settle it once
	// (onLoadDocument).
	// Shutdown joins the evictions it did not cause. The consequence differs from settleWG's
	// gate, which skips the work as well as the waiting: this eviction runs either way, because
	// awaitRoomRecovery blocks every reader of a failed room on the close(done) below, and a
	// gated eviction that never ran would strand them. Only the joining is skipped.
	tracked := s.addUnlessStopping(&s.evictWG)
	go func() {
		if tracked {
			defer s.evictWG.Done()
		}
		_ = s.evictRoom(room, state)
		close(done)
	}()
}

// awaitRoomRecovery waits for a failed room's forced eviction. Its next caller
// then reloads the persisted document into a new room state. A room without
// state has nothing to recover, so this lookup must not allocate one.
//
// An operation inside a database transaction does not wait: it fails with
// ErrServiceUnavailable, and the transaction rolls back. The eviction flushes and
// compacts the document under its advisory lock, which the transaction may hold,
// or which a transaction waiting on one of its locks may hold; Postgres cannot see
// a wait here, so it would never break the cycle. Neither does a read whose context
// WithoutRecoveryWait marked.
//
// A room's own load never calls this: the eviction it would wait for waits for that load
// (onLoadDocument).
func (s *Service) awaitRoomRecovery(ctx context.Context, room string) error {
	state := s.lockExistingState(room)
	if state == nil {
		return nil
	}
	failure := state.failed
	done := state.failedDone
	s.unlockState(room, state)
	if failure == nil {
		return nil
	}
	if ledgerFrom(ctx).inTransaction() {
		// A room that stays failed is otherwise visible only in its callers' 503s, which
		// reach no server log at all: the refusal is what the server knows about the
		// failure, so it names the room and the cause it refused for.
		slog.Warn("dispatch: refuse a transaction's operation on a failed document room",
			"room", room, "error", failure)
		return fmt.Errorf("%w: %w", ErrServiceUnavailable, failure)
	}
	if ctx.Value(withoutRecoveryWait{}) != nil {
		// The caller logs what it went without, which says more than the room alone.
		return fmt.Errorf("%w: %w", ErrServiceUnavailable, failure)
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type withoutRecoveryWait struct{}

// WithoutRecoveryWait marks ctx so that a document read under it fails with ErrServiceUnavailable
// on a failed room instead of waiting for the room's eviction, as an operation inside a
// transaction does (awaitRoomRecovery). It is for a read whose answer only decorates the response
// it is part of - a comment's or ask's anchor_block - since the eviction compacts under the
// document's advisory lock, which any transaction can hold, and a request's context carries no
// deadline to end the wait.
func WithoutRecoveryWait(ctx context.Context) context.Context {
	return context.WithValue(ctx, withoutRecoveryWait{}, true)
}

// queryFrom returns the caller's transaction when it is inside one. A document read made while
// an API write transaction is open must go through that transaction: a second connection from
// the shared pool is what deadlocks it (store.ErrNestedAcquire).
func (s *Service) queryFrom(ctx context.Context) Queryer {
	if tx, ok := txFromContext(ctx); ok {
		return tx
	}
	return s.store.Pool
}

// Queryer is the read surface shared by the pool and a transaction, so a read can be pointed
// at whichever the caller is inside.
type Queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// issueOpen reads the document's issue state from q. Which q is the whole question: an API
// handler that anchors a comment or an ask is inside a transaction here, and its own
// connection is the only one it may use, while a room load must read from the pool that owns
// the load - a writer holding a connection waits for that load, so a load that waits for the
// shared pool closes the same cycle the transaction rule closes.
func (s *Service) issueOpen(ctx context.Context, q Queryer, artifactID string) (bool, error) {
	var open bool
	if err := q.QueryRow(ctx, `
		select coalesce(i.closed_at is null, true)
		from artifacts a left join issues i on i.key = a.issue_key
		where a.id = $1 and a.kind = 'doc'
	`, artifactID).Scan(&open); err != nil {
		return false, fmt.Errorf("check document issue: %w", err)
	}
	return open, nil
}

func (s *Service) waitForPendingUpdates(ctx context.Context, room string) error {
	for s.hasPendingUpdates(room) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
	return nil
}

func (s *Service) waitForDurableAppends(ctx context.Context, room string) error {
	for s.hasDurableAppend(room) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
	return nil
}

func (s *Service) shuttingDown(room string) bool {
	_, ok := s.shutdownRooms.Load(room)
	return ok
}

var _ API = (*Service)(nil)
