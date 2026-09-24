package docs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	maxLiveRooms       = 1_000
	maxRoomConnections = 1_000
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
type VersionedStore interface {
	persistence.VersionedPersistence
	AppendUpdateTx(ctx context.Context, tx pgx.Tx, room string, update []byte, contentChanged bool) (persistence.Version, error)
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
	// afterSettleWarm runs after settleRoom has warmed the live document and before it
	// reads it. Nil outside tests; tests use it to evict the room in that window.
	afterSettleWarm func(room string)
	settleWG        sync.WaitGroup
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
}

type roomState struct {
	mu        sync.Mutex
	connected map[uint64]model.Actor
	pending   map[string]model.Actor
	// lastActor is the most recent edit's source: the actor of a service mutation, or the sole
	// connected peer of a browser edit. Version writes clear `pending`, so a settlement that
	// runs after an edit's own version was committed would otherwise attribute the block asks
	// it indexes to nobody.
	lastActor       *model.Actor
	pendingVersions map[int]versionPending
	contentTree     *pmdoc.Node
	updateClasses   []documentUpdateClass
	pendingUpdates  int
	settle          *time.Timer
	unrecorded      map[pmdoc.MarkRef]time.Time
	durableAppends  atomic.Int64
	gen             uint64
	suppressSettle  int
	// settleDeferred records a settlement asked for while suppressSettle held it off; the last
	// release arms it.
	settleDeferred bool
	// liveWriter is the open transaction writing this document (see liveWrite), or nil.
	liveWriter     *liveWrite
	settleFailures int
	closed         bool
	failed         error
	failedDone     chan struct{}
}

type documentUpdateClass struct {
	update         []byte
	contentChanged bool
	durable        bool
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
}

// identityClosureOrigin identifies a server-owned identity repair transaction.
// It must remain non-zero sized because Ygo compares origins by interface equality.
type identityClosureOrigin struct{ _ byte }

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

func (s *Service) finishSuppressedPersistence(slot *suppressSlot, update []byte) {
	s.suppressMu.Lock()
	defer s.suppressMu.Unlock()
	if slot.canceled || slot.update != nil {
		return
	}
	slot.update = append([]byte(nil), update...)
	close(slot.ready)
}

// discardSuppressedPersistence prevents a completed live mutation from falling
// back to ygo's independent persistence after its enclosing transaction failed.
// The persistence callback consumes the slot and discards its matching update.
func (s *Service) discardSuppressedPersistence(room string, slot *suppressSlot) {
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
			slot.consumed = true
			slots = slots[1:]
			if len(slots) == 0 {
				delete(s.suppressed, room)
			} else {
				s.suppressed[room] = slots
			}
			discarded := slot.discarded
			s.suppressMu.Unlock()
			return discarded
		}
		if !bytes.Equal(slot.update, update) {
			s.suppressMu.Unlock()
			return false
		}
		slot.consumed = true
		slots = slots[1:]
		if len(slots) == 0 {
			delete(s.suppressed, room)
		} else {
			s.suppressed[room] = slots
		}
		s.suppressMu.Unlock()
		return true
	}
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

	service.srv = srv
	srv.Authorize = service.authorize
	srv.OnTokenAuth = service.authorizeSchemaVersion
	srv.OnInject = service.allowInject
	srv.OnLoadDocument = service.onLoadDocument
	srv.OnLastPeer = service.settleLastPeer

	return service
}

// Shutdown stops queued settlements, joins any already-running callbacks, and
// flushes ygo's document persistence workers.
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
	drainCtx, cancelDrain := context.WithTimeout(ctx, 5*time.Second)
	defer cancelDrain()
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
		}
	}
	for _, settlement := range pending {
		if err := s.waitForDurableAppends(drainCtx, settlement.room); err != nil {
			slog.Warn("dispatch: stop document settlement before durable append drain", "room", settlement.room, "error", err)
		}
	}
	settled := make(chan struct{})
	go func() {
		var drain sync.WaitGroup
		for _, settlement := range pending {
			drain.Add(1)
			go func(room string, generation uint64) {
				defer drain.Done()
				s.settleRoom(room, generation)
			}(settlement.room, settlement.generation)
		}
		drain.Wait()
		close(settled)
	}()
	select {
	case <-settled:
	case <-ctx.Done():
		s.stopAccepting()
		return ctx.Err()
	}
	s.stopAccepting()
	s.waitSettles(ctx)
	// A room that failed evicts itself on its own goroutine, and that eviction flushes the
	// room through the store, so it has to finish before the store can go.
	s.waitEvictions(ctx)
	if err := drainCtx.Err(); err != nil {
		return err
	}
	return s.srv.Shutdown(ctx)
}

func (s *Service) scheduleSettle(room string) {
	if s.stopping.Load() || s.shuttingDown(room) {
		return
	}
	state := s.room(room)
	state.mu.Lock()
	defer state.mu.Unlock()
	s.scheduleSettleLocked(room, state)
}

func (s *Service) scheduleSettleLocked(room string, state *roomState) {
	s.scheduleSettleAfterLocked(room, state, s.settle)
}

func (s *Service) scheduleSettleAfterLocked(room string, state *roomState, delay time.Duration) {
	if s.stopping.Load() || s.shuttingDown(room) || state.closed || state.failed != nil {
		return
	}
	if state.suppressSettle > 0 {
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
		defer s.unregisterSettleTimer(timerID)
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

func (s *Service) isSettleTimerArmed(timer *time.Timer) bool {
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
	state := s.room(room)
	state.mu.Lock()
	defer state.mu.Unlock()
	if s.isSettleTimerArmed(state.settle) {
		return
	}
	s.scheduleSettleLocked(room, state)
}

func (s *Service) retrySettleSoon(room string) {
	if s.stopping.Load() || s.shuttingDown(room) {
		return
	}
	state := s.room(room)
	state.mu.Lock()
	defer state.mu.Unlock()
	s.scheduleSettleAfterLocked(room, state, 10*time.Millisecond)
}

func (s *Service) retrySettle(room string, generation uint64, err error) {
	state := s.room(room)
	state.mu.Lock()
	defer state.mu.Unlock()
	s.retrySettleLocked(room, state, generation, err)
}

func (s *Service) retrySettleLocked(room string, state *roomState, generation uint64, err error) {
	slog.Error("dispatch: settle document", "room", room, "error", err)
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

func ensureBlockIDsInDocument(doc *crdt.Doc, origin any) (*pmdoc.Node, int, error) {
	fragment := doc.GetXmlFragment(fragmentName)
	tree, err := treeOf(doc)
	if err != nil {
		return nil, 0, err
	}
	stamped := pmdoc.EnsureBlockIDsCount(tree)
	if stamped == 0 {
		return tree, 0, nil
	}
	if err := doc.TransactE(func(transaction *crdt.Transaction) error {
		return pmdoc.Update(transaction, fragment, tree)
	}, origin); err != nil {
		return nil, 0, err
	}
	return tree, stamped, nil
}

func (s *Service) settleRoom(room string, generation uint64) {
	state := s.room(room)
	state.mu.Lock()
	if s.stopping.Load() || state.closed || state.failed != nil || state.gen != generation {
		state.mu.Unlock()
		return
	}
	state.mu.Unlock()
	if s.shuttingDown(room) && s.srv.GetDoc(room) == nil {
		return
	}
	if s.srv.GetDoc(room) == nil {
		err := s.srv.Apply(context.Background(), room, func(_ *crdt.Doc, _ func(func(*crdt.Transaction))) {})
		if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
			s.retrySettle(room, generation, fmt.Errorf("warm document for settlement: %w", err))
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
	state.mu.Unlock()

	eventCollector := NewEventCollector()
	ctx := store.WithTransactionTracking(WithEventCollector(context.Background(), eventCollector))
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		s.retrySettle(room, generation, fmt.Errorf("begin document transaction: %w", err))
		return
	}
	defer tx.Rollback(ctx)
	owner, open, err := lockArtifactOwner(ctx, tx, room)
	if err != nil {
		s.retrySettle(room, generation, err)
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
		s.retrySettle(room, generation, err)
		return
	}
	if err := s.lockSettlementCursor(ctx, tx, room); err != nil {
		if s.shuttingDown(room) {
			slog.Warn("dispatch: skip shutdown document settlement while cursor lock is held", "room", room, "error", err)
			return
		}
		s.retrySettle(room, generation, fmt.Errorf("lock document cursor: %w", err))
		return
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
		s.retrySettle(room, generation, err)
		return
	}
	tree, err := treeOf(doc)
	if err != nil {
		if errors.Is(err, ErrDocSchema) {
			slog.Error("dispatch: settle document outside Proof schema", "room", room, "error", err)
			return
		}
		s.retrySettle(room, generation, err)
		return
	}

	stamped := pmdoc.BlockIDRepairCount(tree)
	var slot *suppressSlot
	var updates [][]byte
	if stamped > 0 {
		slot = s.prepareSuppressedPersistence(room)
		origin := &identityClosureOrigin{}
		var mutationErr error
		err = s.srv.Apply(ctx, room, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
			unsubscribe := doc.OnUpdate(func(update []byte, updateOrigin any) {
				if updateOrigin == origin {
					updates = append(updates, append([]byte(nil), update...))
				}
			})
			defer unsubscribe()
			tree, stamped, mutationErr = ensureBlockIDsInDocument(doc, origin)
		})
		if errors.Is(err, websocket.ErrNoChanges) {
			err = nil
		}
		if mutationErr != nil || err != nil {
			s.cancelSuppressedPersistence(room, slot)
			if mutationErr != nil {
				err = mutationErr
			}
			if errors.Is(err, ErrDocSchema) {
				slog.Error("dispatch: settle document outside Proof schema", "room", room, "error", err)
				return
			}
			s.retrySettle(room, generation, err)
			return
		}
	}

	state.mu.Lock()
	if s.stopping.Load() || state.closed || state.failed != nil {
		state.mu.Unlock()
		if stamped > 0 {
			s.discardSuppressedPersistence(room, slot)
		}
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
		state.mu.Unlock()
		if stamped == 0 {
			return
		}
		identityUpdate, mergeErr := mergeUpdates(updates)
		if mergeErr != nil {
			s.discardSuppressedPersistence(room, slot)
			s.failRoom(room, mergeErr)
			return
		}
		if err := s.srv.BroadcastUpdate(ctx, room, identityUpdate); err != nil {
			s.discardSuppressedPersistence(room, slot)
			s.failRoom(room, fmt.Errorf("broadcast superseded document identity update: %w", err))
			return
		}
		if _, err := s.persistence.AppendUpdateTx(ctx, tx, room, identityUpdate, true); err != nil {
			s.discardSuppressedPersistence(room, slot)
			s.failRoom(room, err)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			s.discardSuppressedPersistence(room, slot)
			s.failRoom(room, fmt.Errorf("commit superseded document identity update: %w", err))
			return
		}
		s.finishSuppressedPersistence(slot, identityUpdate)
		return
	}
	pending := make(map[string]model.Actor, len(state.pending))
	for key, actor := range state.pending {
		pending[key] = actor
	}
	lastActor := state.lastActor
	state.mu.Unlock()
	authors := actorSlice(pending)
	eventActor := model.Actor{}
	if len(authors) > 0 {
		eventActor = authors[0]
	} else if lastActor != nil {
		eventActor = *lastActor
	}
	reconciliation, err := s.reconcileAskBlocks(ctx, tx, room, owner, tree, eventActor, latest.Number+1)
	if err != nil {
		if stamped > 0 {
			s.discardSuppressedPersistence(room, slot)
			s.failRoom(room, err)
			return
		}
		s.retrySettle(room, generation, err)
		return
	}
	if reconciliation.changed {
		if slot == nil {
			slot = s.prepareSuppressedPersistence(room)
		}
		origin := &identityClosureOrigin{}
		var mutationErr error
		err = s.srv.Apply(ctx, room, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
			unsubscribe := doc.OnUpdate(func(update []byte, updateOrigin any) {
				if updateOrigin == origin {
					updates = append(updates, append([]byte(nil), update...))
				}
			})
			defer unsubscribe()
			fragment := doc.GetXmlFragment(fragmentName)
			mutationErr = doc.TransactE(func(transaction *crdt.Transaction) error {
				return pmdoc.Update(transaction, fragment, tree)
			}, origin)
		})
		if errors.Is(err, websocket.ErrNoChanges) {
			err = nil
		}
		if mutationErr != nil || err != nil {
			s.cancelSuppressedPersistence(room, slot)
			if mutationErr != nil {
				err = mutationErr
			}
			s.failRoom(room, fmt.Errorf("write reconciled typed blocks: %w", err))
			return
		}
		stamped = 1
	}

	var update []byte
	if stamped > 0 {
		update, err = mergeUpdates(updates)
		if err != nil {
			s.discardSuppressedPersistence(room, slot)
			s.failRoom(room, err)
			return
		}
		if err := s.srv.BroadcastUpdate(ctx, room, update); err != nil {
			s.discardSuppressedPersistence(room, slot)
			s.failRoom(room, fmt.Errorf("broadcast document closure update: %w", err))
			return
		}
		if _, appendErr := s.persistence.AppendUpdateTx(ctx, tx, room, update, true); appendErr != nil {
			s.discardSuppressedPersistence(room, slot)
			s.failRoom(room, appendErr)
			return
		}
	}
	if stamped > 0 {
		snapshotCursor, err = currentUpdateCursor(ctx, tx, room)
		if err != nil {
			s.discardSuppressedPersistence(room, slot)
			s.failRoom(room, err)
			return
		}
	}
	markdown, err := renderTree(tree)
	if err != nil {
		if stamped > 0 {
			s.discardSuppressedPersistence(room, slot)
			s.failRoom(room, err)
			return
		}
		if errors.Is(err, ErrDocSchema) {
			slog.Error("dispatch: settle document outside Proof schema", "room", room, "error", err)
			return
		}
		s.retrySettle(room, generation, err)
		return
	}

	state.mu.Lock()
	if s.stopping.Load() || state.closed || state.failed != nil || state.gen != generation {
		state.mu.Unlock()
		if stamped > 0 {
			s.discardSuppressedPersistence(room, slot)
		}
		return
	}
	state.mu.Unlock()

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
	contentChanged, err := contentChangedSinceVersion(ctx, tx, room, latest.docUpdateVersion)
	if err != nil {
		if stamped > 0 {
			s.discardSuppressedPersistence(room, slot)
			s.failRoom(room, err)
			return
		}
		s.retrySettle(room, generation, err)
		return
	}
	if contentChanged || reconciliation.changed || len(reconciliation.events) > 0 {
		result, writeErr := s.writeVersionTx(ctx, tx, room, markdown, tree, eventActor, &versionWrite{
			authors:          authors,
			docUpdateVersion: &snapshotCursor,
		})
		if writeErr != nil {
			if stamped > 0 {
				s.discardSuppressedPersistence(room, slot)
				s.failRoom(room, writeErr)
				return
			}
			s.retrySettle(room, generation, writeErr)
			return
		}
		published = append(published, eventCollector.Events()...)
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
			if stamped > 0 {
				s.discardSuppressedPersistence(room, slot)
				s.failRoom(room, err)
				return
			}
			s.retrySettle(room, generation, err)
			return
		}
	}
	if err := appendEvents(reconciliation.events); err != nil {
		if stamped > 0 {
			s.discardSuppressedPersistence(room, slot)
			s.failRoom(room, err)
			return
		}
		s.retrySettle(room, generation, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		if stamped > 0 {
			s.discardSuppressedPersistence(room, slot)
			s.failRoom(room, fmt.Errorf("commit document settlement: %w", err))
			return
		}
		s.retrySettle(room, generation, fmt.Errorf("commit document settlement: %w", err))
		return
	}
	if stamped > 0 {
		s.finishSuppressedPersistence(slot, update)
	}
	state.mu.Lock()
	if state.gen == generation {
		state.settleFailures = 0
		for key := range pending {
			delete(state.pending, key)
		}
	}
	state.mu.Unlock()
	for _, event := range published {
		s.events.Publish(event)
	}
	s.sweepUnrecordedMarks(room, tree)
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
	state := s.room(artifactID)
	state.mu.Lock()
	if s.stopping.Load() {
		state.mu.Unlock()
		report.Skipped = "service stopping"
		return report
	}
	state.mu.Unlock()

	backfillCtx := withOwnerVerified(ctx)
	slot := s.prepareSuppressedPersistence(artifactID)
	origin := &identityClosureOrigin{}
	var updates [][]byte
	var mutationErr error
	err := s.srv.Apply(backfillCtx, artifactID, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
		unsubscribe := doc.OnUpdate(func(update []byte, updateOrigin any) {
			if updateOrigin == origin {
				updates = append(updates, append([]byte(nil), update...))
			}
		})
		defer unsubscribe()
		_, report.Stamped, mutationErr = ensureBlockIDsInDocument(doc, origin)
	})
	if errors.Is(err, websocket.ErrNoChanges) {
		err = nil
	}

	if mutationErr != nil {
		s.cancelSuppressedPersistence(artifactID, slot)
		report.Err = fmt.Errorf("stamp document: %w", mutationErr)
		return report
	}
	if err != nil {
		s.cancelSuppressedPersistence(artifactID, slot)
		report.Err = fmt.Errorf("open document: %w", err)
		return report
	}
	if report.Stamped == 0 {
		s.cancelSuppressedPersistence(artifactID, slot)
		return report
	}
	update, err := mergeUpdates(updates)
	if err != nil {
		s.discardSuppressedPersistence(artifactID, slot)
		s.failRoom(artifactID, err)
		report.Err = fmt.Errorf("capture identity update: %w", err)
		return report
	}
	if err := s.srv.BroadcastUpdate(backfillCtx, artifactID, update); err != nil {
		s.discardSuppressedPersistence(artifactID, slot)
		s.failRoom(artifactID, fmt.Errorf("broadcast identity update: %w", err))
		report.Err = fmt.Errorf("broadcast identity update: %w", err)
		return report
	}
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		s.discardSuppressedPersistence(artifactID, slot)
		s.failRoom(artifactID, err)
		report.Err = fmt.Errorf("begin document transaction: %w", err)
		return report
	}
	defer tx.Rollback(ctx)
	if _, err := s.persistence.AppendUpdateTx(ctx, tx, artifactID, update, true); err != nil {
		s.discardSuppressedPersistence(artifactID, slot)
		s.failRoom(artifactID, err)
		report.Err = fmt.Errorf("append identity update: %w", err)
		return report
	}
	if err := tx.Commit(ctx); err != nil {
		s.discardSuppressedPersistence(artifactID, slot)
		s.failRoom(artifactID, err)
		report.Err = fmt.Errorf("commit identity update: %w", err)
		return report
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

// SetIssueClosed refreshes the closed state every room of an issue remembers. Its caller runs
// it after its own transaction has committed, and it takes that caller's context so the pool
// can see it: a caller that ever runs it with a transaction still open is refused, not wedged.
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
		state := s.room(room)
		state.mu.Lock()
		changed := state.closed != closed
		state.closed = closed
		if closed && changed {
			state.gen++
			s.stopSettleTimer(state.settle)
		}
		state.mu.Unlock()
		if closed && changed {
			_ = s.srv.CloseRoom(room, true)
		}
	}
}

func (s *Service) evictRoom(room string, state *roomState) error {
	// Close first, then remove the state: the close's flush-before-evict consults the state's
	// persistence-suppression slot, so a state removed before the close would let a failed
	// settlement's discarded identity update reach the store. A settlement armed on this
	// state during the close is re-armed by its own timer (see scheduleSettleAfterLocked).
	err := s.srv.CloseRoom(room, true)
	if state != nil {
		s.rooms.CompareAndDelete(room, state)
	}
	if err != nil && !errors.Is(err, websocket.ErrRoomNotFound) {
		return fmt.Errorf("evict live document: %w", err)
	}
	return nil
}

func (s *Service) failRoom(room string, cause error) {
	state := s.room(room)
	state.mu.Lock()
	defer state.mu.Unlock()
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

func (s *Service) roomFailure(room string) error {
	value, ok := s.rooms.Load(room)
	if !ok {
		return nil
	}
	state := value.(*roomState)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.failed == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrServiceUnavailable, state.failed)
}

func (s *Service) roomFailed(room string) bool {
	value, ok := s.rooms.Load(room)
	if !ok {
		return false
	}
	state := value.(*roomState)
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.failed != nil
}

// awaitRoomRecovery waits for a failed room's forced eviction. Its next caller
// then reloads the persisted document into a new room state. A room without
// state has nothing to recover, so this lookup must not allocate one.
func (s *Service) awaitRoomRecovery(ctx context.Context, room string) error {
	value, ok := s.rooms.Load(room)
	if !ok {
		return nil
	}
	state := value.(*roomState)
	state.mu.Lock()
	failure := state.failed
	done := state.failedDone
	state.mu.Unlock()
	if failure == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) roomClosed(room string) bool {
	state := s.room(room)
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.closed
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

func (s *Service) room(name string) *roomState {
	value, _ := s.rooms.LoadOrStore(name, &roomState{
		connected:       make(map[uint64]model.Actor),
		pending:         make(map[string]model.Actor),
		pendingVersions: make(map[int]versionPending),
		unrecorded:      make(map[pmdoc.MarkRef]time.Time),
	})
	return value.(*roomState)
}

func (s *Service) recordUpdateClass(room string, update []byte, contentChanged, durable bool) {
	state := s.room(room)
	state.mu.Lock()
	defer state.mu.Unlock()
	state.updateClasses = append(state.updateClasses, documentUpdateClass{
		update: append([]byte(nil), update...), contentChanged: contentChanged, durable: durable,
	})
	state.pendingUpdates++
	if durable {
		state.durableAppends.Add(1)
	}
}

func (s *Service) consumeUpdateClass(room string, update []byte) (bool, bool, bool) {
	state := s.room(room)
	state.mu.Lock()
	defer state.mu.Unlock()
	for index, class := range state.updateClasses {
		if !bytes.Equal(class.update, update) {
			continue
		}
		state.updateClasses = append(state.updateClasses[:index], state.updateClasses[index+1:]...)
		state.pendingUpdates--
		return class.contentChanged, class.durable, true
	}
	return true, false, false
}

func (s *Service) hasPendingUpdates(room string) bool {
	state := s.room(room)
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.pendingUpdates > 0
}

func (s *Service) finishDurableAppend(room string) {
	s.room(room).durableAppends.Add(-1)
}

func (s *Service) hasDurableAppend(room string) bool {
	return s.room(room).durableAppends.Load() > 0
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

func (s *Service) canOpenRoom(room string) bool {
	if _, exists := s.rooms.Load(room); exists {
		return true
	}
	count := 0
	s.rooms.Range(func(_, _ any) bool {
		count++
		return count < maxLiveRooms
	})
	return count < maxLiveRooms
}

func (s *Service) canAddConnection() bool {
	count := 0
	s.rooms.Range(func(_, value any) bool {
		state := value.(*roomState)
		state.mu.Lock()
		count += len(state.connected)
		state.mu.Unlock()
		return count < maxRoomConnections
	})
	return count < maxRoomConnections
}

var _ API = (*Service)(nil)
