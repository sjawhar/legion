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

	"github.com/sjawhar/envoy/internal/dispatch/auth"
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
	// errRoomReloading refuses a transaction the writer slot of a room a close has reserved
	// because the room stands behind the durable head (reserveRoomCloseLocked, openLiveWrite):
	// the caller's retry meets the reloaded room.
	errRoomReloading = fmt.Errorf("%w: the document's room is reloading", ErrServiceUnavailable)
)

const maxSettleFailures = 3

const (
	// maxLiveRooms is how many live rooms - ygo's resident rooms, loaded or loading, whoever
	// opened them - a document socket may find before it is refused a room of its own
	// (canOpenRoom).
	maxLiveRooms       = 1_000
	maxRoomConnections = 1_000
	// roomIdleTimeout is how long a room stays resident after its last peer leaves, or after the
	// last Server.Apply on a room no peer is in returns (New). A peer that returns within it
	// rejoins the warm room rather than reloading the document.
	roomIdleTimeout = time.Minute
)

// Deps configures the live document service.
type Deps struct {
	Store             *store.Store
	Persistence       VersionedStore
	Events            *events.Broker
	Identity          identity.Identity
	AgentTokens       *auth.SharedAgentTokens
	ServerURL         string
	Settle            time.Duration
	MarkWait          time.Duration
	UnrecordedMarkTTL time.Duration
	// TestHooks keeps the state the test hooks read and steer (testhooks_rooms.go); cmd/dispatch
	// sets it under DISPATCH_TEST_HOOKS=1. Without it every hook answers ErrTestHooksOff.
	TestHooks bool
}

// VersionedStore is Dispatch's transactional extension of ygo's durable room
// store. AppendUpdateWithCredit is the one way a room's own update reaches the store: it appends
// the update, classified as content or not, with the browser edit's in-flight credit, which it
// takes and lands under the document's advisory lock (UpdateCredit). Document writes that join an
// API transaction use AppendUpdateTx, classifying the update the way the room's update observer
// classifies a live one. RebuildTx replaces an unreadable history inside the rebuild's
// transaction, through the same persistence boundary as its preflight load. Head is the version
// Load would fold up to now, which says whether a state loaded earlier is still the stored one.
// DocumentStamp names the stored state a read of the document meets now, and LoadDocument is that
// state decoded once with the stamp it was read under, which a read of a document no room holds
// caches its rendering by (coldRead).
type VersionedStore interface {
	persistence.VersionedPersistence
	AppendUpdateWithCredit(ctx context.Context, room string, update []byte, contentChanged bool, credit *UpdateCredit) (persistence.Version, error)
	AppendUpdateTx(ctx context.Context, tx pgx.Tx, room string, update []byte, contentChanged bool) (persistence.Version, error)
	RebuildTx(ctx context.Context, tx pgx.Tx, room string, seed []byte) (RebuildReport, error)
	Head(ctx context.Context, room string) (persistence.Version, error)
	DocumentStamp(ctx context.Context, room string) (DocumentStamp, error)
	LoadDocument(ctx context.Context, room string) (LoadedDocument, error)
}

// Service owns live Yjs documents and their durable Dispatch versions.
type Service struct {
	srv               *roomServer
	store             *store.Store
	persistence       VersionedStore
	events            *events.Broker
	identity          identity.Identity
	agentTokens       *auth.SharedAgentTokens
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
	// beforeSettleStart runs as a settlement starts (settleRoomWithin), before it compares the
	// room's generation under the state lock: after its timer fired, while the timer is still
	// registered, or after settleLastPeer took the timer and counted the settlement running. Nil
	// outside tests; tests use it to open a write in that window.
	beforeSettleStart func(room string)
	// afterReadWarm runs after a version capture or VerifyMark has warmed a room and before the
	// version takes its document lock and reads it. Nil outside tests; tests use it to evict the
	// warmed room or edit it in that window.
	afterReadWarm func(room string)
	// afterCaptureAuthorsTake runs after a room-backed version capture takes its authors and before
	// it copies the document. Nil outside tests; tests use it to edit the room in that window.
	afterCaptureAuthorsTake func(room string)
	// afterCaptureFork runs once a version's capture (captureLiveTextAndAuthors) has brought the
	// calling transaction's fork up to date with the room, and before it renders the fork. Nil
	// outside tests; tests use it to edit the room in that window.
	afterCaptureFork func(room string)
	// afterForkRead runs once a write's fork has been brought up to date with the room
	// (forkLive), before the write's operation runs on it. Nil outside tests; tests use it to edit
	// the room after the fork read it.
	afterForkRead func(room string)
	// afterSettleAuthorsTake runs after a settlement has taken its authors and before it copies
	// the tree it will version. Nil outside tests; tests use it to edit the room in that window.
	afterSettleAuthorsTake func(room string)
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
	// afterSettleVersionWrite runs once a settlement that is writing a version has written it
	// (writeVersionTx) and deleted the pending authors it lists, and before the settlement appends
	// that version's own event. Nil outside tests; tests return an error from it to force the
	// settlement to abandon after its version write, before its commit.
	afterSettleVersionWrite func(room string) error
	// beforeObserveUpdate runs in a room's update observer, which ygo runs once the update's
	// transaction has released the document, before the observer drains the room's update log -
	// classifying and crediting its own update and every other committed before it that no
	// observer has reached yet. A repair's observer does not run it. Nil outside tests; tests use
	// it to hold an observer before it classifies anything: the held update stays unclassified
	// unless another transaction's observer reaches the log first, which classifies it in commit
	// order (renderedReplica).
	beforeObserveUpdate func(room string)
	// afterCreditUpdate runs in a room's update observer once its drain of the update log has
	// classified a content change and credited its authors, outside the replica's lock, and before
	// it records its own update's class and arms the room's settlement. Nil outside tests; tests
	// use it to hold that window, in which the observer's own update is credited but neither
	// pending (recordUpdateClass) nor handed to persistence, whose observer runs after this one.
	afterCreditUpdate func(room string)
	// afterLiveCommit runs once a transaction's writes to a document have committed and the
	// document's advisory lock is gone with them, before the publish applies them to the room.
	// Nil outside tests; tests use it to append from the room in the window the lock's next
	// holder takes - the window the commit's cursors must already stand at the head for
	// (Ledger.takeRoomCursors).
	afterLiveCommit func(room string)
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
	// beforeCloseBehindHeadLock runs at the start of a close of a room behind the durable head,
	// before it takes the room's state lock (closeRoomBehindHead). Nil outside tests; tests use it
	// to open a write in the one window between the evictor's own check for an open live write and
	// the locked check that decides the close.
	beforeCloseBehindHeadLock func(room string)
	// afterRoomCloseReserved runs once a close of a room behind the durable head has reserved the
	// room (reserveRoomCloseLocked) and released its state lock, before the close. Nil outside
	// tests; tests use it to open a write, or to retire and reload the room, in that window.
	afterRoomCloseReserved func(room string)
	// afterRoomClosePinned runs once that close holds the room it checked in a Server.Apply, before
	// it closes it (closeRoomInstance). Nil outside tests.
	afterRoomClosePinned func(room string)
	// afterRoomClosed runs once a forced close has closed the room and before the service settles
	// the room's record (closeReservedRoom, and evictRoom and evictFailed through forgetEvicted,
	// evictFailed's with the close gate still held): the window in which a browser's connect, the
	// room that connect loads, and an edit typed in it reach that record. Nil outside tests.
	afterRoomClosed func(room string)
	// beforeFailedEviction runs on a failed room's eviction goroutine (failRoomLocked) before it
	// closes the room (evictFailed). Nil outside tests; tests use it to close and reload the room
	// another way first.
	beforeFailedEviction func(room string)
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
	// roomGenerations is where every roomState.roomGeneration is drawn from (nextRoomGeneration),
	// so no two states of the service ever stand at one generation.
	roomGenerations atomic.Uint64
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
	// replicas holds, for each resident room document, a weak pointer to the replica its update
	// observer keeps (weak.Pointer[renderedReplica]), which the document's reads walk (readLive),
	// keyed by a weak pointer to that document (keepReplica).
	replicas sync.Map
	// instances lists, for each resident room document, what that instance records about the
	// durable history behind it (*roomInstance): the updates whose writes can have recorded a
	// pending author, and the durable version the instance stands at, keyed by a weak pointer to
	// the document (keepInstance). roomLoads holds what each room's load records between its read
	// of the store (servicePersistenceAdapter.LoadDoc) and the instance the hook ygo calls next on
	// the same goroutine starts (onLoadDocument): the head that read saw, and every version this
	// process appended while the load ran, which reaches no instance of its own (roomLoad).
	instances sync.Map
	roomLoads sync.Map
	// reads holds the renderings of the documents cold reads served, each under the stored state
	// it was rendered from (coldRead).
	reads *documentReads
	// hooks is the test hooks' state (testhooks_rooms.go), nil unless Deps.TestHooks is set.
	hooks *testHooks
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
		select issue_key, coalesce(project_key, ''), slug, name
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
	// committed is the update a repair's transaction committed (recordSuppressedCommit), set before
	// ygo hands that update to the room's persistence.
	committed []byte
}

// identityClosureOrigin identifies a server-owned repair transaction - settlement's, or the
// block-id backfill's - and carries the suppression slot its update is held from the room's
// persistence by. Being non-zero sized also keeps each origin distinct, since ygo compares origins
// by interface equality.
type identityClosureOrigin struct{ slot *suppressSlot }

// recordSuppressedCommit records on slot the update its repair transaction committed. The room's
// update observer runs it (onLoadDocument) on the committing goroutine before ygo's own
// persistence observer, which the room registers after OnLoadDocument, so the slot holds the
// update before the room's persistence is handed it.
func (s *Service) recordSuppressedCommit(slot *suppressSlot, update []byte) {
	s.suppressMu.Lock()
	defer s.suppressMu.Unlock()
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
// read - the room was evicted, by ygo's idle sweep once its last browser left or the API last
// touched it, or by a CloseRoom, and the write would load a replacement from the store - or
// because a close of the room is under way (roomServer).
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
// it. It is written only into the document the room holds while no close of the room is under
// way, and it holds off those closes until its transaction's update is in the room's persistence
// (roomServer). A refused repair writes nothing and returns errRoomReplaced.
func (s *Service) applySuppressed(ctx context.Context, room string, want *crdt.Doc, mutate func(doc *crdt.Doc, origin any) (bool, error)) (*suppressSlot, []byte, error) {
	slot := s.prepareSuppressedPersistence(room)
	origin := &identityClosureOrigin{slot: slot}
	wrote := false
	updates, err := s.applyCaptured(ctx, room, origin, func(doc *crdt.Doc) error {
		if want != nil && doc != want {
			return errRoomReplaced
		}
		// Taken after Server.Apply's injection check, which waits for a failed room's eviction
		// outside a transaction. A repair therefore never holds the gate while waiting for that
		// eviction, and holdOpen rechecks that this room still holds doc before it does.
		release, open := s.srv.holdOpen(room, doc)
		if !open {
			return errRoomReplaced
		}
		defer release()
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
		s.suppressMu.Unlock()

		<-ready

		s.suppressMu.Lock()
		slots = s.suppressed[room]
		if len(slots) == 0 || slots[0] != slot {
			s.suppressMu.Unlock()
			continue
		}
		if slot.canceled {
			// A discarded repair's slot is consumed by that repair's own update alone. ygo runs the
			// observers of two transactions in either order (renderedReplica), so another writer's
			// update can reach the worker ahead of the repair's, and it is stored as any other.
			if !bytes.Equal(slot.committed, update) {
				s.suppressMu.Unlock()
				return false
			}
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
		agentTokens:       deps.AgentTokens,
		serverURL:         strings.TrimSuffix(deps.ServerURL, "/"),
		settle:            settle,
		markWait:          markWait,
		now:               time.Now,
		timers:            make(map[uint64]*time.Timer),
		unrecordedMarkTTL: unrecordedMarkTTL,
		reads:             newDocumentReads(documentReadBudget),
	}
	if deps.TestHooks {
		service.hooks = newTestHooks()
	}
	adapter := &servicePersistenceAdapter{store: persist, service: service}
	srv := websocket.NewServerWithPersistence(adapter)
	srv.HocuspocusFraming = true
	// Transactional server-side edits suppress one automatic ygo write and
	// append it inside the API transaction, so persistence stays per update.
	srv.PersistCoalesceWindow = -1
	srv.CompactEvery = 200
	// The rooms decode under maxUpdateItems, the queue every decode of document bytes takes (see
	// maxUpdateItems). It is also the most a room's peers can park in it, about ten times ygo's
	// default; bounding what one peer's update can do to a room is LEGION-487. ygo's check of each
	// update the service broadcasts (Server.BroadcastUpdate) takes no pending queue: the room has
	// already applied the update (reearth/ygo#268, in the pinned fork).
	srv.MaxPendingItems = maxUpdateItems
	// A room whose last peer leaves, or that only Server.Apply touches, stays resident until it has
	// been idle for roomIdleTimeout: ygo stamps a room idle when its last peer leaves, and when an
	// Apply on a room no peer is in returns (reearth/ygo#269, in the pinned fork). Eager eviction,
	// ygo's default, evicts the room the moment its last peer leaves, even while
	// a Server.Apply is inside its callback on that room (sjawhar/ygo v1.51.3-sami.1,
	// provider/websocket/peer.go:496-580 checks peers alone): the callback's write then lands on
	// the evicted room and reaches the store only through its retiring persistence worker, while
	// the next access has already loaded the store without it and serves, and takes, the next
	// write on a state missing the first. The two writes, each made from the same document, merge
	// into a document neither wrote, which can hold no block at all. Idle eviction refuses a
	// room any Apply holds (evictIdleRoom's inflight check) and counts its idle time from the last
	// Apply's return, and the sweeper flushes the room before it evicts it, so every write a room
	// the sweeper evicts has taken is durable before a successor can load. CloseRoom
	// checks peers alone too (Server.CloseRoom), and the service still calls it to close an
	// issue's rooms (SetIssueClosed), for a room with an editor at Shutdown, and to evict one
	// (evictRoom): a write that commits on a room it has retired reaches the store through ygo's
	// stranded persistence, on the committing goroutine: the room's doc.OnUpdate observer, which
	// loadRoom registers (provider/websocket/server.go:1784-1826) and which runs on the goroutine
	// that committed once the commit has released the document's lock (crdt/doc.go:647-652), calls
	// persistStranded (persistence.go:126-164) after the room's persistence worker retires. The
	// service's CloseRoom waits for a repair's commit (roomServer), so a repair never meets it; a
	// published write's suppression slot is finished before ygo's persistence observer runs
	// (onLoadDocument), so that persistence never waits on the publish it is running in.
	srv.RoomIdleTimeout = roomIdleTimeout

	service.srv = newRoomServer(srv)
	srv.Authorize = service.authorize
	srv.OnTokenAuth = service.authorizeSchemaVersion
	srv.OnInject = service.allowInject
	srv.OnLoadDocument = service.onLoadDocument
	srv.OnLastPeer = service.settleLastPeer
	// ygo calls it once for every room it retires, whichever way: the idle sweep, CloseRoom.
	srv.OnUnloadDocument = service.releaseUnloadedRoom

	return service
}

// ShutdownDrainBudget bounds the part of Shutdown that waits on document work - each room's durable
// appends landing, and the settlements owed - inside whatever deadline its caller
// passes. It is sized 5 s short of the document service's own budget (cmd/dispatch's
// documentShutdownTimeout), so a burst of writes still landing when Shutdown begins - not only the
// ones already queued - has room to finish: the live durableAppends counter waits for every append
// a room's writer records, whenever Shutdown's scan found the room, not just the ones it already
// held. A settlement it cuts short, and the close of its room's editors that follows it, get what
// is left of the caller's deadline, so a caller's deadline has to exceed it.
const ShutdownDrainBudget = 9 * time.Second

// Shutdown stops queued settlements and runs one worker per loaded room inside the drain budget:
// it waits for the room's durable appends to land, reads whether the room's document owes a
// settlement, settles it if so, and closes the editors connected to the room once that settlement
// has returned. Shutdown then joins the settlements and evictions already running and flushes
// ygo's document persistence workers.
//
// Each room spends the budget on its own work alone: a room whose append is slow to store, or
// whose editor keeps sending updates, leaves only its own settlement to the next process, never
// another room's, and Shutdown returns that room's drain error. A room with an editor connected
// is settled while it is still loaded and only then closed: ygo's CloseRoom evicts the room as it
// closes its peers, and a settlement does not load a room during shutdown, so a room closed first
// would leave its settlement to the next process. An edit made while its room settles is left to
// that process too.
//
// A settlement the budget cuts short is not lost. Its database work is cancelled and its
// transaction rolls back, and the pending-settlement row the document's updates wrote
// (markSettlementPending) arms it again on the room's next load (onLoadDocument) or at the next
// resumption (RunSettlementResumption). For each document that owed a settlement Shutdown logs
// whether it settled or was left to resume; once the caller's deadline has passed it cannot read
// that back, and its error names those documents.
func (s *Service) Shutdown(ctx context.Context) error {
	// loadedRoom is a room as Shutdown found it: the generation its settlement runs at, and
	// whether an editor was connected.
	type loadedRoom struct {
		name       string
		generation uint64
		connected  bool
	}
	var loaded []loadedRoom
	s.rooms.Range(func(key, value any) bool {
		name := key.(string)
		state := value.(*roomState)
		s.shutdownRooms.Store(name, struct{}{})
		state.mu.Lock()
		loaded = append(loaded, loadedRoom{name: name, generation: state.roomGeneration, connected: len(state.connected) > 0})
		state.mu.Unlock()
		return true
	})
	s.stopAllSettleTimers()
	drainCtx, cancelDrain := context.WithTimeout(ctx, ShutdownDrainBudget)
	defer cancelDrain()
	rooms := make([]string, 0, len(loaded))
	for _, room := range loaded {
		rooms = append(rooms, room.name)
	}
	var (
		mu sync.Mutex
		// owed is whether each room's document owed a settlement; a room missing from it could
		// not read that.
		owed = make(map[string]bool, len(loaded))
		// drainErr is a durable append the budget did not see through, which Shutdown cannot
		// leave to a later load; a settlement it cuts short it can.
		drainErr error
		workers  sync.WaitGroup
	)
	for _, room := range loaded {
		workers.Add(1)
		go func() {
			defer workers.Done()
			settles := false
			if err := s.waitForDurableAppends(drainCtx, room.name); err != nil {
				slog.Warn("dispatch: stop document settlement before durable append drain", "room", room.name, "error", err)
				mu.Lock()
				drainErr = err
				mu.Unlock()
			} else if owes, err := settlementPending(drainCtx, s.store.Pool, room.name); err != nil {
				// Nothing says the settlement can be skipped, so it runs.
				slog.Warn("dispatch: read whether the document owes a settlement before shutdown", "room", room.name, "error", err)
				settles = true
			} else {
				// A document whose updates have all been settled would spend the budget
				// repeating that work - a 1 MiB document's for seconds, holding the issue's row.
				mu.Lock()
				owed[room.name] = owes
				mu.Unlock()
				settles = owes
			}
			if settles {
				s.settleRoomWithin(drainCtx, room.name, room.generation)
			}
			if room.connected {
				if err := s.srv.CloseRoom(room.name, true); err != nil && !errors.Is(err, websocket.ErrRoomNotFound) {
					slog.Warn("dispatch: close document peers before shutdown", "room", room.name, "error", err)
				}
			}
		}()
	}
	// The budget ending does not end this wait: a cancelled settlement may still be rendering,
	// which no context interrupts, and the store has to outlast it, and its room's editors are
	// closed once it has returned, so their updates reach the store.
	waitGroup(ctx, &workers)
	if err := ctx.Err(); err != nil {
		s.stopAccepting()
		// A worker may still be running, so its outcome is read under its lock.
		mu.Lock()
		defer mu.Unlock()
		return settlementsUnconfirmed(err, owed, rooms)
	}
	// A settlement that a timer, or a room's last browser leaving, started before the timers
	// stopped runs under no budget of its own; it gets what is left of this one before the gate
	// closes and abandons it.
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

// reportShutdownSettlements logs, for each document that owed a settlement when its Shutdown worker
// read it, whether it settled or is left to resume, and whether the drain budget ended first. A room
// missing from owed could not read whether it owed one, and is named only when it is left.
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
// read back which settlements committed. It names the documents that owed one, and each room that
// could not read whether it did; each that did not settle resumes from its pending-settlement row.
func settlementsUnconfirmed(cause error, owed map[string]bool, rooms []string) error {
	named := make([]string, 0, len(rooms))
	for _, room := range rooms {
		if owes, read := owed[room]; !read || owes {
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
// database between scenarios with every document the previous scenario had open already
// written: a settlement still running would write to tables the truncate has just emptied, or
// wait on the truncate's locks and fail once it commits.
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
		state.roomGeneration = s.nextRoomGeneration()
		s.stopSettleTimer(state.settle)
		// The closed room's settlement failures are not the next room's (see
		// reserveRoomCloseLocked).
		state.settleFailures = 0
		// Quiesce cancels this document's settlement, and its caller empties the database behind
		// it, so the edit source that settlement would have named goes with it: left here it
		// would hold the record for a document nothing will settle (unusedLocked).
		state.lastActor = nil
		state.unsettled = false
		state.mu.Unlock()
		if err := s.evictRoom(room, state); err != nil && firstErr == nil {
			firstErr = err
		}
		return true
	})
	s.waitSettles(ctx)
	s.waitEvictions(ctx)
	// While the rooms closed, a browser's reconnect (onLoadDocument), its edit
	// (creditContentChange) or a committed write (Ledger.creditRooms) can record an edit source on
	// a record again, and quiescing refuses the settlement that would read it (scheduleSettle), so
	// that source would hold the record of a document the harness truncates next (unusedLocked).
	// Once every close and settlement has ended, a record whose room is gone and whose settlement
	// is neither armed nor running drops it, and goes when nothing else holds it.
	s.rooms.Range(func(key, value any) bool {
		room := key.(string)
		state := value.(*roomState)
		state.mu.Lock()
		if !state.released && s.srv.GetDoc(room) == nil && state.settling == 0 && !s.isSettleTimerArmed(state.settle) {
			state.lastActor = nil
			state.unsettled = false
		}
		s.unlockState(room, state)
		return true
	})
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
	state.roomGeneration = s.nextRoomGeneration()
	generation := state.roomGeneration
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

// stopSettleTimer stops an armed settlement timer and releases the settleWG count it held.
func (s *Service) stopSettleTimer(timer *time.Timer) bool {
	if !s.takeSettleTimer(timer) {
		return false
	}
	s.settleWG.Done()
	return true
}

// takeOverSettlementLocked is for a caller that has just moved state's generation, which ends every
// settlement that read the old one: it stops the room's armed timer, and answers whether that ended
// a settlement the caller now owes - one the timer would have run, one whose timer has fired and
// whose callback has not returned (still registered, isSettleTimerArmed), or one running on the
// state (settling, which settleLastPeer counts from the moment it takes the timer). A settlement
// past its supersession check commits its own work, and the one its caller arms finds nothing new.
// Its caller holds state.mu.
func (s *Service) takeOverSettlementLocked(state *roomState) bool {
	stopped := s.stopSettleTimer(state.settle)
	return stopped || s.isSettleTimerArmed(state.settle) || state.settling > 0
}

// takeSettleTimer stops an armed settlement timer and hands its caller the settleWG count the timer
// held, for a settlement the caller runs in the timer's place, so Shutdown joins it as it would the
// timer's own. The caller releases the count once that settlement has returned.
func (s *Service) takeSettleTimer(timer *time.Timer) bool {
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

// isSettleTimerArmed is whether timer, a state's settle timer, is still registered: armed, or fired
// with its callback not yet returned (the callback leaves the register as it returns), and not
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

// nextRoomGeneration is the generation a state moves to when something invalidates what a
// settlement read of its room, drawn from one counter for the whole service rather than counted
// per state. A settlement finds its room's state by name when it runs and when it retries
// (settleRoomWithin, retrySettle), and an eviction replaces the state: a reload's state counted
// from zero again, so a settlement the eviction did not end - one already past its generation
// check, which the stale-room evictor no longer waits for - could find the reload standing at the
// very generation it was armed with, and count its refusal against that healthy room, failing it
// at the third (maxSettleFailures). Drawn from one counter, a generation names one state's one
// arming, and a stale one matches no other state's.
func (s *Service) nextRoomGeneration() uint64 {
	return s.roomGenerations.Add(1)
}

func (s *Service) retrySettle(room string, generation uint64, err error) {
	state := s.lockState(room)
	defer s.unlockState(room, state)
	s.retrySettleLocked(room, state, generation, err)
}

func (s *Service) retrySettleLocked(room string, state *roomState, generation uint64, err error) {
	counted := state.roomGeneration == generation
	fails := counted && state.settleFailures+1 >= maxSettleFailures
	switch {
	case errors.Is(err, errRoomReplaced):
		// Expected, not a fault: the settlement wrote nothing (one that wrote fails the room
		// instead) because the room it read was replaced first, and the replacement's load arms
		// the settlement the document still owes.
		slog.Info("dispatch: document settlement wrote nothing into a replaced room", "room", room, "error", err)
	default:
		slog.Error("dispatch: settle document", "room", room, "error", err)
	}
	if !counted {
		return
	}
	state.settleFailures++
	if fails {
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
// (rewriteLive), and returns the stamped tree, how many ids it minted, and each ask block whose
// id it changed. Its caller decides from a read of its own whether any id needs repair, so a
// document that needs none opens no transaction, and records the stamped asks
// (recordStampedAskBlocks) once it holds the room's state lock, which no Yjs transaction takes.
func stampBlockIDs(doc *crdt.Doc, origin any) (*pmdoc.Node, int, []stampedAsk, error) {
	minted := 0
	var stamped []stampedAsk
	tree, _, err := rewriteLive(doc, origin, func(live *pmdoc.Node) bool {
		before := askBlockOrder(live)
		minted = pmdoc.EnsureBlockIDsCount(live)
		if minted > 0 && len(before) > 0 {
			stamped = stampedAskBlocks(before, askBlockOrder(live))
		}
		return minted > 0
	})
	return tree, minted, stamped, err
}

// settlementVersionCredit is whom a settlement's version credits, taken before the tree it records:
// capture, its authors in order, and actor, whom its derived events name. state is the room the
// capture was taken from (consumeAskAuthors). askSources, copied once the settlement holds the
// tree it reconciles, names who introduced each new ask block in it.
type settlementVersionCredit struct {
	capture    authorCapture
	state      *roomState
	authors    []model.Actor
	actor      model.Actor
	askSources askBlockSources
}

// readSettlementTree takes the authors a settlement's version lists before it copies doc: owed,
// the pending authors the settlement read under the document's advisory lock, and the room's
// in-flight credits no committed version has listed. An update observer credits a change only
// once the room holds it, so every in-flight credit taken is for a change the copy holds; one
// observed after the take stays pending for the next version. Holding state.mu across the copy
// would block the update observer that credits and broadcasts every peer's keystroke.
func (s *Service) readSettlementTree(state *roomState, owed map[string]model.Actor, doc *crdt.Doc, room string, afterTake func(room string)) (*pmdoc.Node, settlementVersionCredit, error) {
	state.mu.Lock()
	credit := settlementAuthors(state, owed)
	state.mu.Unlock()
	if afterTake != nil {
		afterTake(room)
	}
	tree, err := s.liveTree(room, doc)
	return tree, credit, err
}

// settlementAuthors takes the authors a settlement's version lists (captureAuthors), and names
// the actor its derived events carry: the latest editor when the version lists them or lists no
// one, otherwise SettlementActor. The caller holds state.mu.
func settlementAuthors(state *roomState, owed map[string]model.Actor) settlementVersionCredit {
	capture := captureAuthors(state, owed, nil, nil)
	credit := settlementVersionCredit{capture: capture, state: state, authors: actorSlice(capture.authors)}
	if latest := state.lastActor; latest != nil {
		if _, found := capture.authors[actorKey(*latest)]; found || len(credit.authors) == 0 {
			credit.actor = *latest
			return credit
		}
	}
	credit.actor = SettlementActor
	return credit
}

// consumeAskAuthors forgets the recorded author of each block a committed settlement indexed or
// found indexed: its ask row names the author from then on.
func (credit settlementVersionCredit) consumeAskAuthors(blockIDs []string) {
	if len(blockIDs) == 0 {
		return
	}
	state := credit.state
	state.mu.Lock()
	for _, blockID := range blockIDs {
		delete(state.askAuthors, blockID)
	}
	state.mu.Unlock()
}

func (s *Service) settleRoom(room string, generation uint64) {
	s.settleRoomWithin(context.Background(), room, generation)
}

// settleRoomWithin settles room under parent. A timer's settlement runs under no deadline; the one
// Shutdown runs gets its drain budget. When parent ends first the settlement's database work is
// cancelled and its transaction rolls back, and nothing retries it or counts it as a failure: the
// document's pending-settlement row leaves it to resume (onLoadDocument, RunSettlementResumption).
// So does a settlement that has to write into a room Shutdown has begun closing, which refuses the
// write (allowInject).
func (s *Service) settleRoomWithin(parent context.Context, room string, generation uint64) {
	retry := func(err error) {
		if parent.Err() != nil {
			slog.Warn("dispatch: document settlement stopped at the shutdown budget", "room", room, "error", err)
			return
		}
		if s.shuttingDown(room) && errors.Is(err, ErrServiceUnavailable) {
			slog.Warn("dispatch: skip shutdown document settlement that has to write into its room", "room", room, "error", err)
			return
		}
		s.retrySettle(room, generation, err)
	}
	if s.beforeSettleStart != nil {
		s.beforeSettleStart(room)
	}
	state := s.lockState(room)
	if s.stopping.Load() || state.closed || state.failed != nil || state.roomGeneration != generation {
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
	// Hold the live document for the whole settlement. A close that lands after the generation
	// check above (a failure's eviction, a close of a room behind the durable head) retires the
	// room and moves the generation; reading the room again later would return nil and every
	// later step would dereference it. A nil here means exactly that close: the next access
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
	generation = state.roomGeneration
	s.unlockState(room, state)

	ledger := &Ledger{service: s, settling: true}
	// The cursor of the instance this settlement holds (doc), which gates the repairs it appends
	// and takes them in once they are stored (advanceRoomCursor). Its own first gate is the
	// comparison below, before it reads the document at all.
	cursor := s.cursorOf(doc)
	ctx := withRoomCursor(store.WithTransactionTracking(withLedger(parent, ledger)), cursor)
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
	// The gate, before this settlement reads the document or writes anything. The room it holds
	// must stand at that head: with two envoy-dispatch processes on one Postgres, an update the
	// other process appended is not in this room, and settling from it corrupts three things at
	// once. Its version would record doc_update_version = snapshotCursor over text that is
	// missing the other process's update, so contentChangedSinceVersion then reports nothing past
	// that cursor and those words are never versioned; clearSettlementPending would delete the one
	// durable record that the work is still owed; and reconcileAskBlocks would retract every open
	// ask whose block this stale tree cannot see.
	//
	// Nothing is lost by refusing. The transaction rolls back, so doc_settlements_pending still
	// stands. The refusal is a verdict, not a fault to retry: a room behind the head never catches
	// up without a reload (advanceRoomCursor), so every retry of this settlement on it is refused
	// the same way. Counted toward maxSettleFailures, the third refusal failed the room, and every
	// transaction that met the document until the failure's eviction had run answered
	// ErrServiceUnavailable (INFRA-218). The room is reloaded instead (reloadRoomBehindHead), and
	// the settlement that re-arms settles every update, this process's and the other's.
	//
	// The cursor is the one this settlement marked its context with, read through the accessor
	// its repairs' appends read (appendingCursor), so the two can never disagree.
	if err := refuseRoomBehindHead(room, appendingCursor(ctx, room), snapshotCursor); err != nil {
		// The rollback releases the document's advisory lock, which the room's close needs: a close
		// flushes the room's persistence, whose appends take that lock.
		_ = tx.Rollback(ctx)
		if parent.Err() != nil {
			retry(err)
			return
		}
		s.reloadRoomBehindHead(parent, room, state, generation, doc, err)
		return
	}
	// The pending authors as this settlement's lock holds them: no append or write changes them
	// until its transaction ends. Its version lists them and deletes them; a settlement that
	// writes no version leaves them for the next version.
	owed, err := readPendingAuthors(ctx, tx, room)
	if err != nil {
		retry(err)
		return
	}
	// The settlement reads the document three times, and each read has its own name: read is this
	// one, before the database work; reconciled is the tree the ask blocks are reconciled on, the
	// stamp's when it stamped ids; versioned is the one the version is rendered from, read again
	// after the repairs when the settlement wrote any. A browser's edit made in between is in a
	// later one and not an earlier one. read and versioned are each taken as of one moment under the
	// document's lock (liveTree), and the stamp reads inside its own transaction: the room's peers
	// and the service can write it while a walk of the live tree, which takes no lock, reads it, and
	// a torn read would be versioned as the document. read and versioned take their authors before
	// their copy (readSettlementTree), so a normal credit, which follows its room update, is for a
	// change the copy holds. The one change a copy can lack whose author it takes is a committed
	// transaction's write, credited at its commit and published after the copy; the supersession
	// check below then writes no version, since the write still holds its slot or its publish moved
	// the generation.
	read, credit, err := s.readSettlementTree(state, owed, doc, room, s.afterSettleAuthorsTake)
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
	// (applyLive): a `doc_updates` row says whether the update changed the rendered markdown,
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
	// A settlement that wrote nothing is retried. Its transaction rolls back, so the authors it read
	// stay where they were, and it marked no in-flight credit.
	abandon := func(err error) {
		if len(slots) > 0 {
			s.discardSuppressedPersistence(room, slots...)
			s.failRoom(room, err)
			return
		}
		retry(err)
	}
	reconciled := read
	var renamed []stampedAsk
	if pmdoc.BlockIDRepairCount(read) > 0 {
		slot, update, err := s.applySuppressed(ctx, room, doc, func(doc *crdt.Doc, origin any) (bool, error) {
			var minted int
			var stampErr error
			reconciled, minted, renamed, stampErr = stampBlockIDs(doc, origin)
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
	state.recordStampedAskBlocks(renamed)
	if s.stopping.Load() || state.closed || state.failed != nil {
		s.unlockState(room, state)
		s.discardSuppressedPersistence(room, slots...)
		return
	}
	// An open live write is not in the room yet, and since this settlement holds the owner row,
	// the write's transaction has already committed: the cursor this settlement versions against
	// includes its row. Write no version; finishLiveWrite arms a settlement once it is published.
	superseded := state.roomGeneration != generation
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
		repaired, err := s.persistence.AppendUpdateTx(ctx, tx, room, identityUpdate, identityChanged)
		if err != nil {
			abandon(err)
			return
		}
		// The room already holds this repair (applySuppressed wrote it there), so the instance
		// takes the version it is stored at. It is recorded before the commit, under the advisory
		// lock this transaction holds until then, so nothing reads the cursor in between; a
		// rollback here fails the room, which reloads the document (abandon).
		advanceRoomCursor(cursor, repaired)
		if err := tx.Commit(ctx); err != nil {
			abandon(fmt.Errorf("commit superseded document identity update: %w", err))
			return
		}
		finishSlots()
		return
	}
	// The ask blocks' sources are read now, after the tree they are reconciled on: an update in that
	// tree whose observer has already run has recorded who introduced its blocks, and one whose
	// observer has not left its blocks unseen, for the settlement that observer arms (askBlockSources).
	credit.askSources = state.askSources()
	state.mu.Unlock()
	reconciliation, err := s.reconcileAskBlocks(ctx, tx, room, owner, reconciled, credit, beforeMarkdown)
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
		repaired, err := s.persistence.AppendUpdateTx(ctx, tx, room, update, closureChanged)
		if err != nil {
			abandon(err)
			return
		}
		advanceRoomCursor(cursor, repaired)
		snapshotCursor, err = currentUpdateCursor(ctx, tx, room)
		if err != nil {
			abandon(err)
			return
		}
		// The version is the document as it stands after the repairs, read under its lock, not the
		// tree settlement reconciled before them: a peer's edit made since is in the room, its
		// browsers and its stored updates, and a version rendered from the earlier tree would leave
		// it out (LEGION-479). The authors are taken before this copy, so the version lists no
		// author of an edit it lacks. An edit made after the take and before the copy is in this
		// version but credited after the take; its credit stays pending, and the next version lists
		// its author. The same is true when the copy holds an edit whose update observer had not yet
		// credited it - ygo runs the observer only once the edit's transaction has released the
		// document.
		versioned, credit, err = s.readSettlementTree(state, owed, doc, room, s.afterSettleAuthorsTake)
		if err != nil {
			abandon(err)
			return
		}

		if s.afterSettleVersionRead != nil {
			s.afterSettleVersionRead(room)
		}
	}
	markdown, offsets, err := renderTreeWithOffsets(versioned)
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
	if s.stopping.Load() || state.closed || state.failed != nil || (state.roomGeneration != generation && len(slots) == 0) {
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
	// Every update this settlement read is settled once it commits, so the row that left the
	// settlement to a later load goes with that commit. It goes before the first event this
	// settlement appends: a transaction marking this document owed as a copy (owedCopiesOf) holds
	// its row while it waits for the events' commit-order lock, which this settlement would then be
	// holding. The pending authors stay unless this settlement's version lists them (deleted
	// below): a settlement that writes no version lists no one.
	if err := clearSettlementPending(ctx, tx, room); err != nil {
		abandon(err)
		return
	}
	if err := reconciliation.nameVersion(ctx, tx, room, owner, settledVersion); err != nil {
		abandon(err)
		return
	}
	if versioning {
		result, writeErr := s.writeVersionTx(ctx, tx, room, renderedTree{tree: versioned, markdown: markdown, offsets: offsets}, credit.actor, &versionWrite{
			authors:          credit.authors,
			capture:          &credit.capture,
			docUpdateVersion: &snapshotCursor,
		})
		if writeErr != nil {
			abandon(writeErr)
			return
		}
		// The version lists the pending authors this settlement read, so its commit deletes them.
		if err := deleteCapturedPendingAuthors(ctx, tx, room, credit.capture); err != nil {
			abandon(err)
			return
		}
		if s.afterSettleVersionWrite != nil {
			if err := s.afterSettleVersionWrite(room); err != nil {
				abandon(err)
				return
			}
		}
		published = append(published, ledger.events...)
		// A document body cites nodes whose rows carry a backlink count, so the version event
		// names what this settle moved exactly as a message or comment write does.
		versionEvent := model.Event{
			IssueKey: owner.IssueKey,
			Type:     "artifact.version",
			Actor:    credit.actor,
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
	// The commit marks the in-flight credits the version listed consumed before it releases the
	// room's state (commitConsuming); a settlement that writes no version takes out none.
	committed := authorCapture{state: state}
	if versioning {
		committed = credit.capture
	}
	if err := s.commitConsuming(ctx, tx, []roomCapture{{room: room, capture: committed}}, func(err error) {
		// This runs before the publish below, the order Ledger.Commit keeps for every other
		// version write, so a subscriber acting on this version's artifact.version event acts
		// after it.
		if err == nil && state.roomGeneration == generation {
			state.settleFailures = 0
			state.unsettled = len(state.inflight) > 0
			if !state.unsettled {
				state.lastActor = nil
			}
		}
	}); err != nil {
		abandon(fmt.Errorf("commit document settlement: %w", err))
		return
	}
	credit.consumeAskAuthors(reconciliation.indexedAskBlocks)
	finishSlots()
	for _, event := range published {
		s.events.Publish(event)
	}
	reconciliation.logCopiedBlocks(room)
	// A copy of an ask this settlement retracted opens its own ask at its next settlement, which
	// its pending row (reconcileAskBlocks, owedCopiesOf) also leaves to the resumption should this
	// process stop first.
	for _, copy := range reconciliation.owedCopies {
		s.scheduleSettle(copy)
	}
	s.sweepUnrecordedMarks(room, versioned)
}

// reloadRoomBehindHead answers a settlement the gate refused (refusal, ErrRoomBehindHead): live, the
// room instance the settlement holds, stands behind the document's durable head, because another
// envoy-dispatch process appended an update this instance never took, and only a reload brings
// that update in. The settlement has written nothing, and its caller has rolled its transaction
// back, which releases the document's advisory lock that the close below needs. It closes the
// room as the stale-room evictor does and arms the settlement again: its timer loads the document
// at the head and settles every update, this process's and the other's, and
// doc_settlements_pending, which the rollback kept, says the work is owed should this process stop
// first.
//
// A room this process is still writing is not closed, since the close would take that write's
// publish with it. An open live write gets its settlement when it finishes (settleDeferred,
// finishLiveWrite). An update the room's persistence has not stored yet arms one a settle delay
// later: that update's append from this room is refused, which fails the room, and the reload the
// failure makes arms one too (onLoadDocument), so either timer meets the failed room or the
// reloaded one. A shutdown's settlement leaves the room for the next process, as it does for any
// other write into a room it is closing. Those checks and the close's reservation are made under
// one hold of the room's state lock (reserveRoomCloseLocked), so no write opens between them.
//
// A write that opened while this settlement ran took the settlement over as it opened
// (openLiveWrite), and arms it when it ends whether or not it is still open here. Otherwise the
// settlement compares its generation, under that lock, with the one it read: whatever moved the
// generation since owns the settlement (an armed timer, another close's reservation, a write), and
// the reservation moves it, so of two settlements that meet the same refusal only one closes the
// room. A room the close does not reach - live already retired or replaced - is left to whatever
// holds the document, and the settlement is armed again on it. Every outcome is logged but a
// service that is stopping and a room already closed or failed, which its close or failure
// accounts for, so a refused settlement on a room still serving leaves a line naming what became
// of its room.
func (s *Service) reloadRoomBehindHead(ctx context.Context, room string, state *roomState, generation uint64, live *crdt.Doc, refusal error) {
	if s.shuttingDown(room) {
		slog.Warn("dispatch: skip shutdown document settlement from a room behind the durable head; the next process settles it",
			"room", room, "error", refusal)
		return
	}
	leftOpen := func(reason string) {
		slog.Info("dispatch: left open a document room its settlement found behind the durable head",
			"room", room, "reason", reason, "error", refusal)
	}
	state.mu.Lock()
	if s.stopping.Load() || state.closed || state.failed != nil {
		s.unlockState(room, state)
		return
	}
	if state.liveWriter != nil {
		state.settleDeferred = true
		s.unlockState(room, state)
		leftOpen("a live write of this process is open on it, and arms the settlement when it ends")
		return
	}
	if state.roomGeneration != generation {
		s.unlockState(room, state)
		leftOpen("the room's generation moved since the settlement read it, and what moved it settles the document")
		return
	}
	if state.pendingUpdates > 0 || state.durableAppends.Load() > 0 {
		s.scheduleSettleLocked(room, state)
		s.unlockState(room, state)
		leftOpen("an update of this room's has not been stored yet; the settlement is armed again")
		return
	}
	reservation, reserved := s.reserveRoomCloseLocked(room, state, live)
	s.unlockState(room, state)
	if !reserved {
		leftOpen("the room it read was retired, replaced or reserved by another close; the settlement is armed again")
		s.scheduleSettle(room)
		return
	}
	// The refused settlement is itself owed, so the close arms it again: on the reloaded room when
	// it closed live, and on the room that holds the document then when it did not
	// (closeReservedRoom).
	reservation.owesSettlement = true
	closed, err := s.closeReservedRoom(ctx, reservation)
	switch {
	case err != nil:
		slog.Warn("dispatch: close a document room its settlement found behind the durable head; the settlement is armed again",
			"room", room, "error", err)
	case closed:
		slog.Info("dispatch: closed a document room its settlement found behind the durable head; settling it again from the store",
			"room", room, "error", refusal)
	default:
		leftOpen("the room it read was retired or replaced before the close reached it; the settlement is armed again")
	}
}

// errRoomCloseHeldOff is closeRoomBehindHead's answer when something of this process holds the
// room open for now: it closed nothing, and the room is exactly as stale as its caller found it.
var errRoomCloseHeldOff = errors.New("the close of the room is held off")

// closeRoomBehindHead closes live, room's resident instance, which the stale-room evictor found
// behind the durable head, so the next access loads the document at the head. It closes nothing,
// and answers errRoomCloseHeldOff, while something of this process holds the room open: an open
// live write, or a close reserved for an instance the room no longer holds, whose reservation
// refuses the writer slot of the instance that replaced it until that close returns. The second
// close answers for its own instance, not for live, so live is closed only by trying again. Both
// are read under the state lock that reserves the close (reserveRoomCloseLocked), so no writer is
// admitted between the check and the close. The other writers of this process are not asked
// about, for the reason the evictor's own check gives (hasOpenLiveWrite): they take the
// document's lock, and the evictor's confirmation under that lock already orders them.
//
// It closes nothing, and answers false with no error, when the room is another close's or another
// instance's: live is no longer the instance room holds, the room failed (its own eviction
// reloads it, evictFailed) before the reservation or after it, before the close's Apply admitted
// the close, or another close has reserved live itself (that close closes it, or arms the
// settlement it ended again). A failed room answers so even when something else would hold the
// close off: nothing but its own eviction will close it.
// Otherwise the close reaches live alone (closeRoomInstance). The state is taken as lockState
// takes it: a room that loaded onto a state an earlier release then forgot has none registered,
// and one made for it here is the one the close settles. A settlement the reservation ended is
// armed again (closeReservedRoom).
func (s *Service) closeRoomBehindHead(ctx context.Context, room string, live *crdt.Doc) (closed bool, err error) {
	if s.beforeCloseBehindHeadLock != nil {
		s.beforeCloseBehindHeadLock(room)
	}
	state := s.lockState(room)
	if state.failed != nil {
		s.unlockState(room, state)
		return false, nil
	}
	if state.openLiveWriteLocked() || (state.reloading && state.reservedFor != live) {
		s.unlockState(room, state)
		return false, errRoomCloseHeldOff
	}
	reservation, reserved := s.reserveRoomCloseLocked(room, state, live)
	s.unlockState(room, state)
	if !reserved {
		return false, nil
	}
	closed, err = s.closeReservedRoom(ctx, reservation)
	if err != nil && reservation.state.failure() != nil {
		// The room failed while the close ran, and the failure refused the close's Apply
		// (allowInject): the failure's own eviction closes it.
		return false, nil
	}
	return closed, err
}

// roomCloseReservation is a close of a room behind the durable head that its caller has reserved
// (reserveRoomCloseLocked) and has yet to make (closeReservedRoom): live is the room instance the
// caller checked, and state the room's state the reservation marks. owesSettlement is whether a
// settlement is owed that the reservation ended - one armed, fired or running on the state
// (takeOverSettlementLocked) - which the close arms again.
type roomCloseReservation struct {
	room           string
	state          *roomState
	live           *crdt.Doc
	owesSettlement bool
}

// reserveRoomCloseLocked reserves state, room's, for a close of live, the room instance its caller
// found behind the durable head and found no write of this process in flight on under this same
// hold of state.mu. It reserves nothing when live is no longer the instance room holds, when the
// state has been forgotten, when the room has failed (its failure's own eviction reloads it), or
// when another close has reserved it already. Otherwise it marks the state reloading, which
// refuses every transaction that would take the room's writer slot from here until the close has
// returned (openLiveWrite), moves the room's generation, which ends the settlements that read it,
// and stops its timer.
//
// Without the reservation a write could take the slot after its caller's check and before the
// close, and the close would retire the instance under a write in flight on it: the very case its
// caller's check leaves the room open for (reloadRoomBehindHead defers to a live writer, and
// closeRoomBehindHead refuses while one runs). Refused instead (ErrServiceUnavailable), the write's
// retry lands on the room that replaces it. The state's lock is not held across the close itself,
// whose peers' disconnects and persistence flush take it. Its caller holds state.mu.
func (s *Service) reserveRoomCloseLocked(room string, state *roomState, live *crdt.Doc) (roomCloseReservation, bool) {
	if state.reloading || state.released || state.failed != nil || s.srv.GetDoc(room) != live {
		return roomCloseReservation{}, false
	}
	state.reloading = true
	state.reservedFor = live
	state.roomGeneration = s.nextRoomGeneration()
	// The closed room's settlement failures are not the next room's, and the record outlives the
	// close. retrySettleLocked counts a failure only for the generation its settlement read, and
	// that moves here; a settlement that reads the generation again after its warm-up
	// (settleRoomWithin) can still count one against the room that replaces this one.
	state.settleFailures = 0
	return roomCloseReservation{
		room:           room,
		state:          state,
		live:           live,
		owesSettlement: s.takeOverSettlementLocked(state),
	}, true
}

// closeReservedRoom makes reservation's close and lifts the reservation once the close has
// returned. When the room no longer holds the instance the reservation named by the time the close
// reaches it - the idle sweep or another close retired it, and a load may have replaced it - or
// the close is refused, nothing is closed, so a write on the room that holds the document now is
// not refused either. A settlement the reservation owes is armed again: on the reloaded room,
// which its timer loads at the head, or on the room that holds the document now.
//
// The room's record stays registered while anything holds it (unlockState): the browser that
// opened the document while the close ran, the room that replaced the closed one and what its load
// read, and the closed room's own unfinished work, which its late callbacks come back for. A
// record nothing holds, whose room has gone, is forgotten there as any other is.
func (s *Service) closeReservedRoom(ctx context.Context, reservation roomCloseReservation) (closed bool, err error) {
	if s.afterRoomCloseReserved != nil {
		s.afterRoomCloseReserved(reservation.room)
	}
	closed, err = s.closeRoomInstance(ctx, reservation.room, reservation.live)
	if s.afterRoomClosed != nil {
		s.afterRoomClosed(reservation.room)
	}
	state := reservation.state
	state.mu.Lock()
	if !state.released {
		state.reloading = false
	}
	// Cleared on a released record too: it holds the instance, which nothing should keep alive.
	state.reservedFor = nil
	s.unlockState(reservation.room, state)
	if reservation.owesSettlement {
		s.scheduleSettle(reservation.room)
	}
	return closed, err
}

// closeRoomInstance closes live, room's instance, and never a room that replaced it, answering
// whether it closed it. ygo's CloseRoom closes whatever room the name holds when it looks the name
// up, and ygo's idle sweep retires a room without the close gate every other close the service
// makes takes (roomServer), so a check of the instance made before the close can be stale by the
// time ygo looks: the sweep retires live, a load replaces it, and the close drops the
// replacement's browsers and the state a write on it holds. So the close runs inside a
// Server.Apply on the room, which the idle sweep never retires while an Apply is in it (sjawhar/ygo
// v1.51.3-sami.1, provider/websocket/idle_sweep.go, evictIdleRoom: an Apply holds the room's
// in-flight count from entry until it returns, whatever idle stamp a leaving browser puts on the
// room meanwhile), with the close gate held (roomServer.closeInstance): a room that still holds
// live once both are held is the room ygo then closes. A room already gone from the name is not
// loaded for the check; one retired between that look and the Apply is loaded by the Apply at the
// head, and left open. The Apply waits for no failed room's recovery (WithoutRecoveryWait): a
// failed room's own eviction closes it. It is the service's own injection, not a caller's write,
// so it carries the service's mark (withOwnerVerified): allowInject refuses an unmarked injection
// on a closed issue, and this close has to run on one - a room loaded while the issue was open and
// left behind the head by the other process's write is closed whenever its issue closed, rather
// than kept resident with text the store no longer holds until its idle minute runs out.
func (s *Service) closeRoomInstance(ctx context.Context, room string, live *crdt.Doc) (closed bool, err error) {
	if s.srv.GetDoc(room) != live {
		return false, nil
	}
	var closeErr error
	err = s.srv.Apply(withOwnerVerified(WithoutRecoveryWait(ctx)), room, func(held *crdt.Doc, _ func(func(*crdt.Transaction))) {
		if held != live {
			return
		}
		if s.afterRoomClosePinned != nil {
			s.afterRoomClosePinned(room)
		}
		closed, closeErr = s.srv.closeInstance(room, live)
	})
	if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
		return false, err
	}
	return closed, closeErr
}

// currentUpdateCursor is the version q sees the document's stored history standing at: the read
// the gate compares a room's cursor against (refuseRoomBehindHead). A settlement and a live write
// read it inside their own transaction, under the document's advisory lock, so no append can land
// between it and what they write; the stale-room evictor reads it through the shared pool,
// holding nothing, because a room it finds behind a head that has moved on again is behind all
// the same.
func currentUpdateCursor(ctx context.Context, q Queryer, artifactID string) (int64, error) {
	var cursor int64
	if err := q.QueryRow(ctx, `
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
	var renamed []stampedAsk
	// The cursor of the room instance the stamp is written into, taken inside the Apply that
	// holds that instance: the backfill appends a durable version of its own below, and a room
	// that did not take it in would stand behind the head and be refused its next write.
	var cursor *roomCursor
	slot, update, err := s.applySuppressed(backfillCtx, artifactID, nil, func(doc *crdt.Doc, origin any) (bool, error) {
		cursor = s.cursorOf(doc)
		read, err := s.liveTree(artifactID, doc)
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
		stamped, minted, stampRenamed, err := stampBlockIDs(doc, origin)
		report.Stamped = minted
		renamed = stampRenamed
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
	// The stamp is in the room, whatever happens to its store write below.
	state := s.lockState(artifactID)
	state.recordStampedAskBlocks(renamed)
	s.unlockState(artifactID, state)
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
	stamped, err := s.persistence.AppendUpdateTx(withRoomCursor(ctx, cursor), tx, artifactID, update, stampedChanged)
	if err != nil {
		return abandon(fmt.Errorf("append identity update: %w", err))
	}
	// The stamp is in the room, so that instance takes the version it is stored at, under the
	// advisory lock this transaction holds until it commits. A rollback fails the room (abandon).
	advanceRoomCursor(cursor, stamped)
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
// wedged. That context is one caller holding at most one connection, so the rooms are recorded and
// closed one at a time.
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
		var (
			lastActor *model.Actor
			creditSeq uint64
		)
		if state != nil {
			changed = state.closed != closed
			state.closed = closed
			if closed && changed {
				state.roomGeneration = s.nextRoomGeneration()
				// The closed room's settlement failures are not the next room's (see
				// reserveRoomCloseLocked): a record kept across the close and a reopen starts
				// over.
				state.settleFailures = 0
				s.stopSettleTimer(state.settle)
				// Snapshot while the state is locked, then write after unlocking: settlement holds
				// the document's advisory lock before it takes state.mu, so taking that lock here
				// while holding the state would invert the lock order. The authors are already in
				// the document's pending authors or in flight to them; only the latest edit source
				// lives in the state alone.
				lastActor, creditSeq = state.lastActor, state.creditSeq.Load()
			}
			s.unlockState(room, state)
		}
		if closed && changed {
			persisted := lastActor == nil
			if lastActor != nil {
				if err := s.persistLastActor(ctx, room, *lastActor); err != nil {
					slog.Error("dispatch: record closing document's latest edit source", "room", room, "error", err)
				} else {
					persisted = true
				}
			}
			if persisted {
				s.lastActorPersisted(room, creditSeq)
			}
			// A room still loading has no state yet, and is closed once it has loaded.
			_ = s.srv.CloseRoom(room, true)
		}
	}
}

// lastActorPersisted forgets a closed document's latest edit source once its pending-settlement
// row holds it, unless an edit observed since (a later creditSeq) replaced it.
func (s *Service) lastActorPersisted(room string, creditSeq uint64) {
	state := s.lockExistingState(room)
	if state == nil {
		return
	}
	if state.closed && state.creditSeq.Load() == creditSeq {
		state.lastActor = nil
		state.unsettled = false
	}
	s.unlockState(room, state)
}

// Evict closes a live room so the next access reloads the durable document without treating the
// room as failed. Tests, including those in package api, call it to force a room to reload. It
// checks nothing about the room: the service's own closes of a room behind the durable head go
// through closeRoomBehindHead and reloadRoomBehindHead, which reserve the room first
// (reserveRoomCloseLocked) and close the instance they checked (closeRoomInstance).
func (s *Service) Evict(_ context.Context, artifactID string) error {
	return s.evictRoom(artifactID, s.invalidateForEviction(artifactID))
}

// evictFailed is a failed room's eviction (failRoomLocked): it closes the room that runs on
// state, the failed one, and hands state over, unless state has been handed over already. The
// failure evicts on a goroutine of its own, and another close can take the failed instance first
// - Evict or Quiesce, which tests and the browser-test harness's own route call - after which the
// browser reconnects and a replacement loads with a state of its own. A close by name here closed
// that replacement under the browser. The stale-room evictor's close does not take a room that
// has already failed (reserveRoomCloseLocked). A room that fails while that close runs keeps its
// record, which nothing else can load onto while it is failed: when the failure refused the
// close's Apply (allowInject), this close closes the room; when that close closed it first, this
// close finds the name holding no room and closes nothing.
//
// Whose room is under the name is read from state, not from the document, because the load that
// replaces the failed room can attach to the failed state: ygo's own load takes the state after
// it has published the room (onLoadDocument), so a load that was already under way when the room
// failed is refused by the failure (websocket.go's roomFailure check) only if it reaches that
// check after it, and one that passed it is closed here with the state it took, as before. A
// state the service holds is the one every load under the name takes, so the room under the name
// is state's until the state is handed over (handOverFailedRoomLocked, which marks it released):
// Evict or Quiesce, having taken the failed room first, hands it over in forgetEvicted, so this
// check finds it released.
//
// Evict and Quiesce hand their states over after letting go of the gate (evictRoom), leaving a
// moment in which this close can still meet a load that took the state after it, as every close
// by name could. ygo's own retirement of an idle room forgets no failed state: a state with a
// failure is in use until its eviction (unusedLocked).
func (s *Service) evictFailed(room string, state *roomState) error {
	return s.srv.closeOwned(room,
		func() bool {
			state.mu.Lock()
			defer state.mu.Unlock()
			return !state.released
		},
		func(err error) error { return s.forgetEvicted(room, state, err) })
}

// invalidateForEviction ends what room's state has armed, ahead of the room's close: the
// generation bump ends a settlement whose timer already fired before it opens its transaction,
// and the timer's stop the one still armed. It returns the state, for the eviction to settle once
// the room is closed (forgetEvicted: kept while held, forgotten when nothing holds it, or handed
// over when it failed), or nil when the service holds none for room.
func (s *Service) invalidateForEviction(room string) *roomState {
	state := s.lockExistingState(room)
	if state != nil {
		state.roomGeneration = s.nextRoomGeneration()
		state.settleFailures = 0
		s.stopSettleTimer(state.settle)
		s.unlockState(room, state)
	}
	return state
}

// evictRoom closes room and then settles what becomes of its record. A failed room's record has to
// go, since it carries the failure (handOverFailedRoomLocked); any other is kept while anything
// holds it and forgotten when nothing does, as any record whose room has gone is (unlockState).
//
// ygo takes the closed room out of its map before CloseRoom returns, so a browser that opens the
// document while the close runs registers on this record (authorize registers it before its room
// is loaded or joined), the room that replaces the closed one loads onto it, and an edit made in
// that room is credited and recorded on it. Dropping the record outright lost all of that: the
// person's keystrokes were credited to nobody, the document forgot its issue was closed, and the
// settlement the new load armed was left to a timer that found its record gone (INFRA-352).
func (s *Service) evictRoom(room string, state *roomState) error {
	return s.forgetEvicted(room, state, s.srv.CloseRoom(room, true))
}

// forgetEvicted is an eviction's end once its close has returned err: it settles state, the
// closed room's record, and reports a close that failed, a room that had already gone being none.
func (s *Service) forgetEvicted(room string, state *roomState, err error) error {
	// Close first, then settle the state: the close's flush-before-evict consults the state's
	// persistence-suppression slot, so a state removed before the close would let a failed
	// settlement's discarded identity update reach the store. A settlement armed on this
	// state during the close is re-armed by its own timer (see scheduleSettleAfterLocked).
	if s.afterRoomClosed != nil {
		s.afterRoomClosed(room)
	}
	if state != nil {
		state.mu.Lock()
		if !state.released {
			if state.failed != nil {
				s.handOverFailedRoomLocked(room, state)
			} else {
				s.releaseIfUnusedLocked(room, state)
			}
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
	s.hooks.countRoomFailure(room)
	state.failedDone = make(chan struct{})
	done := state.failedDone
	state.roomGeneration = s.nextRoomGeneration()
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
		if s.beforeFailedEviction != nil {
			s.beforeFailedEviction(room)
		}
		_ = s.evictFailed(room, state)
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

// waitForPendingUpdates waits until the room has handed persistence every update its update
// observer recorded, from every peer and the service alike. It counts the room, not a writer: a
// peer still writing keeps it waiting for as long as the room stores more slowly than it writes.
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
