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
// store. Document writes that join an API transaction use AppendUpdateTx.
type VersionedStore interface {
	persistence.VersionedPersistence
	AppendUpdateTx(context.Context, pgx.Tx, string, []byte) (persistence.Version, error)
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
	nextConnection    atomic.Uint64
	stopping          atomic.Bool
	settleWG          sync.WaitGroup
	suppressMu        sync.Mutex
	suppressed        map[string][]*suppressSlot
}

type roomState struct {
	connected       map[uint64]model.Actor
	pending         map[string]model.Actor
	pendingVersions map[int]versionPending
	settle          *time.Timer
	unrecorded      map[pmdoc.MarkRef]time.Time
	gen             uint64
	suppressSettle  int
	settleFailures  int
	failed          error
	failedDone      chan struct{}
	closed          bool
	mu              sync.Mutex
}

type artifactOwner struct {
	IssueKey *string
	Project  string
	Slug     string
	Name     string
}

// lockArtifactOwner loads an artifact's owner, then locks that owner row.
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
			select true from artifacts where id = $1 and issue_key is null for update
		`, artifactID).Scan(&exists); err != nil {
			return artifactOwner{}, false, fmt.Errorf("lock document artifact: %w", err)
		}
		return owner, true, nil
	}
	var open bool
	if err := tx.QueryRow(ctx, `
		select closed_at is null from issues where key = $1 for update
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
	s.rooms.Range(func(key, value any) bool {
		room := value.(*roomState)
		room.mu.Lock()
		if room.settle != nil && room.settle.Stop() {
			s.settleWG.Done()
			pending = append(pending, pendingSettlement{room: key.(string), generation: room.gen})
		}
		room.mu.Unlock()
		return true
	})
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
		s.stopping.Store(true)
		return ctx.Err()
	}
	s.stopping.Store(true)
	s.waitSettles(ctx)
	return s.srv.Shutdown(ctx)

}

func (s *Service) scheduleSettle(room string) {
	if s.stopping.Load() {
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
	if s.stopping.Load() || state.closed || state.failed != nil || state.suppressSettle > 0 {
		return
	}
	state.gen++
	generation := state.gen
	if state.settle != nil && state.settle.Stop() {
		s.settleWG.Done()
	}
	s.settleWG.Add(1)
	state.settle = time.AfterFunc(delay, func() {
		defer s.settleWG.Done()
		s.settleRoom(room, generation)
	})
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

func artifactVersionEventPayload(
	artifactID, name string,
	version model.Version,
	diff *string,
) map[string]any {
	payload := map[string]any{"artifact_id": artifactID, "name": name, "version": version}
	if diff != nil {
		payload["diff"] = *diff
	}
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
	if s.srv.GetDoc(room) == nil {
		err := s.srv.Apply(context.Background(), room, func(_ *crdt.Doc, _ func(func(*crdt.Transaction))) {})
		if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
			s.retrySettle(room, generation, fmt.Errorf("warm document for settlement: %w", err))
			return
		}
	}

	ctx := context.Background()
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
	latest, err := latestVersion(ctx, tx, room)
	if err != nil {
		s.retrySettle(room, generation, err)
		return
	}
	tree, err := treeOf(s.srv.GetDoc(room))
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
	if state.gen != generation {
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
		if _, err := s.persistence.AppendUpdateTx(ctx, tx, room, identityUpdate); err != nil {
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
	state.mu.Unlock()
	authors := actorSlice(pending)
	eventActor := model.Actor{}
	if len(authors) > 0 {
		eventActor = authors[0]
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
		if _, appendErr := s.persistence.AppendUpdateTx(ctx, tx, room, update); appendErr != nil {
			s.discardSuppressedPersistence(room, slot)
			s.failRoom(room, appendErr)
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
	appendEvents := func(events []model.Event) error {
		for _, planned := range events {
			appended, appendErr := s.events.Append(ctx, tx, planned)
			if appendErr != nil {
				return appendErr
			}
			published = append(published, appended)
		}
		return nil
	}
	if latest.markdown != markdown {
		version, writeErr := s.writeVersionTx(ctx, tx, room, markdown, tree, &versionWrite{authors: authors})
		if writeErr != nil {
			if stamped > 0 {
				s.discardSuppressedPersistence(room, slot)
				s.failRoom(room, writeErr)
				return
			}
			s.retrySettle(room, generation, writeErr)
			return
		}
		versionEvent := model.Event{
			IssueKey: owner.IssueKey,
			Type:     "artifact.version",
			Actor:    eventActor,
			Payload:  artifactVersionEventPayload(room, owner.Name, version, nil),
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

	backfillCtx := withBackfillInjection(ctx)
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
	if _, err := s.persistence.AppendUpdateTx(ctx, tx, artifactID, update); err != nil {
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

func (s *Service) waitSettles(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		s.settleWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func (s *Service) SetIssueClosed(issueKey string, closed bool) {
	rows, err := s.store.Pool.Query(context.Background(), `
		select id::text from artifacts where issue_key = $1 and kind = 'doc'
	`, issueKey)
	if err != nil {
		return
	}
	var rooms []string
	for rows.Next() {
		var room string
		if err := rows.Scan(&room); err != nil {
			rows.Close()
			return
		}
		rooms = append(rooms, room)
	}
	if rows.Err() != nil {
		rows.Close()
		return
	}
	rows.Close()
	for _, room := range rooms {
		state := s.room(room)
		state.mu.Lock()
		changed := state.closed != closed
		state.closed = closed
		if closed && changed {
			state.gen++
			if state.settle != nil && state.settle.Stop() {
				s.settleWG.Done()
			}
		}
		state.mu.Unlock()
		if closed && changed {
			_ = s.srv.CloseRoom(room, true)
		}
	}
}

// Evict closes a live room and discards its resident state so the next access
// reloads the durable document without treating the room as failed.
func (s *Service) Evict(_ context.Context, artifactID string) error {
	value, _ := s.rooms.Load(artifactID)
	var state *roomState
	if value != nil {
		state = value.(*roomState)
		state.mu.Lock()
		state.gen++
		if state.settle != nil && state.settle.Stop() {
			s.settleWG.Done()
		}
		state.mu.Unlock()
	}
	return s.evictRoom(artifactID, state)
}

func (s *Service) evictRoom(room string, state *roomState) error {
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
	if state.settle != nil && state.settle.Stop() {
		s.settleWG.Done()
	}
	s.purgeSuppressedPersistence(room)
	go func() {
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

func (s *Service) issueOpen(ctx context.Context, artifactID string) (bool, error) {
	var open bool
	if err := s.store.Pool.QueryRow(ctx, `
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
