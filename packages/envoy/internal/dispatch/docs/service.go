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
	"github.com/reearth/ygo/persistence"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

var (
	// ErrIssueClosed rejects a document mutation after its issue is closed.
	ErrIssueClosed = errors.New("issue is closed")
	// ErrServiceUnavailable reports a room that could not load or persist durably.
	ErrServiceUnavailable = errors.New("document service unavailable")
)

// Deps configures the live document service.
type Deps struct {
	Store       *store.Store
	Persistence VersionedStore
	Events      *events.Broker
	Identity    identity.Identity
	AgentToken  string
	ServerURL   string
	Settle      time.Duration
	MarkWait    time.Duration
}

// VersionedStore is Dispatch's transactional extension of ygo's durable room
// store. Document writes that join an API transaction use AppendUpdateTx.
type VersionedStore interface {
	persistence.VersionedPersistence
	AppendUpdateTx(context.Context, pgx.Tx, string, []byte) (persistence.Version, error)
}

// Service owns live Yjs documents and their durable Dispatch versions.
type Service struct {
	srv            *websocket.Server
	store          *store.Store
	persistence    VersionedStore
	events         *events.Broker
	identity       identity.Identity
	agentToken     string
	serverURL      string
	settle         time.Duration
	markWait       time.Duration
	rooms          sync.Map
	nextConnection atomic.Uint64
	stopping       atomic.Bool
	settleWG       sync.WaitGroup
	suppressMu     sync.Mutex
	suppressed     map[string][]*suppressSlot
}

type roomState struct {
	connected       map[uint64]model.Actor
	pending         map[string]model.Actor
	pendingVersions map[int]versionPending
	settle          *time.Timer
	gen             uint64
	suppressSettle  int
	failed          error
	failedDone      chan struct{}
	closed          bool
	mu              sync.Mutex
}

type suppressSlot struct {
	ready    chan struct{}
	update   []byte
	canceled bool
	consumed bool
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

func (s *Service) finishSuppressedPersistence(slot *suppressSlot, update []byte) {
	s.suppressMu.Lock()
	defer s.suppressMu.Unlock()
	if slot.canceled || slot.update != nil {
		return
	}
	slot.update = append([]byte(nil), update...)
	close(slot.ready)
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
		if slot.canceled || !bytes.Equal(slot.update, update) {
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
	if deps.Events == nil {
		deps.Events = events.NewBroker()
	}
	persist := deps.Persistence
	if persist == nil {
		persist = NewPgVersioned(deps.Store)
	}
	service := &Service{
		store:       deps.Store,
		persistence: persist,
		events:      deps.Events,
		identity:    deps.Identity,
		agentToken:  deps.AgentToken,
		serverURL:   strings.TrimSuffix(deps.ServerURL, "/"),
		settle:      settle,
		markWait:    markWait,
		suppressed:  make(map[string][]*suppressSlot),
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
	srv.OnInject = service.allowInject
	srv.OnLoadDocument = service.onLoadDocument

	return service
}

// Shutdown stops queued settlements, joins any already-running callbacks, and
// flushes ygo's document persistence workers.
func (s *Service) Shutdown(ctx context.Context) error {
	s.stopping.Store(true)

	s.rooms.Range(func(_, value any) bool {
		room := value.(*roomState)
		room.mu.Lock()
		if room.settle != nil && room.settle.Stop() {
			s.settleWG.Done()
		}
		room.mu.Unlock()
		return true
	})
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
	if s.stopping.Load() || state.closed || state.failed != nil || state.suppressSettle > 0 {
		return
	}
	state.gen++
	generation := state.gen
	if state.settle != nil && state.settle.Stop() {
		s.settleWG.Done()
	}
	s.settleWG.Add(1)
	state.settle = time.AfterFunc(s.settle, func() {
		defer s.settleWG.Done()
		s.settleRoom(room, generation)
	})
}

func (s *Service) retrySettle(room string, generation uint64, err error) {
	slog.Error("dispatch: settle document", "room", room, "error", err)
	state := s.room(room)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.gen == generation {
		s.scheduleSettleLocked(room, state)
	}
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

func (s *Service) settleRoom(room string, generation uint64) {
	state := s.room(room)
	state.mu.Lock()
	if s.stopping.Load() || state.closed || state.failed != nil || state.gen != generation {
		state.mu.Unlock()
		return
	}
	doc := s.srv.GetDoc(room)
	if doc == nil {
		state.mu.Unlock()
		return
	}
	tree, err := treeOf(doc)
	var markdown string
	if err == nil {
		var markdownErr error
		markdown, markdownErr = renderTree(tree)
		err = markdownErr
	}
	state.mu.Unlock()
	if err != nil {
		if errors.Is(err, ErrDocSchema) {
			slog.Error("dispatch: settle document outside Proof schema", "room", room, "error", err)
			return
		}
		s.retrySettle(room, generation, err)
		return
	}
	ctx := context.Background()

	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		s.retrySettle(room, generation, fmt.Errorf("begin document transaction: %w", err))
		return
	}
	defer tx.Rollback(ctx)
	var issueKey, artifactName string
	var open bool
	if err := tx.QueryRow(ctx, `
		select a.issue_key, a.name, i.closed_at is null
		from artifacts a join issues i on i.key = a.issue_key
		where a.id = $1 for update of i
	`, room).Scan(&issueKey, &artifactName, &open); err != nil {
		s.retrySettle(room, generation, fmt.Errorf("lock document issue: %w", err))
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
	state.mu.Lock()
	if s.stopping.Load() || state.closed || state.failed != nil || state.gen != generation {
		state.mu.Unlock()
		return
	}
	doc = s.srv.GetDoc(room)
	if doc == nil {
		state.mu.Unlock()
		return
	}
	tree, err = treeOf(doc)
	if err == nil {
		markdown, err = renderTree(tree)
	}
	pending := make(map[string]model.Actor, len(state.pending))
	for key, actor := range state.pending {
		pending[key] = actor
	}
	state.mu.Unlock()
	if err != nil {
		if errors.Is(err, ErrDocSchema) {
			slog.Error("dispatch: settle document outside Proof schema", "room", room, "error", err)
			return
		}
		s.retrySettle(room, generation, err)
		return
	}
	if latest.markdown == markdown {
		return
	}
	authors := actorSlice(pending)
	version, err := s.writeVersionTx(ctx, tx, room, markdown, tree, &versionWrite{authors: authors})
	if err != nil {
		s.retrySettle(room, generation, err)
		return
	}
	eventActor := model.Actor{}
	if len(authors) > 0 {
		eventActor = authors[0]
	}
	event, err := s.events.Append(ctx, tx, model.Event{
		IssueKey: issueKey,
		Type:     "artifact.version",
		Actor:    eventActor,
		Payload:  artifactVersionEventPayload(room, artifactName, version, nil),
	})
	if err != nil {
		s.retrySettle(room, generation, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.retrySettle(room, generation, fmt.Errorf("commit document version: %w", err))
		return
	}
	state.mu.Lock()
	if state.gen == generation {
		for key := range pending {
			delete(state.pending, key)
		}
	}
	state.mu.Unlock()
	s.events.Publish(event)
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
	if state.failed != nil {
		state.mu.Unlock()
		return
	}
	state.failed = cause
	state.failedDone = make(chan struct{})
	done := state.failedDone
	state.gen++
	if state.settle != nil && state.settle.Stop() {
		s.settleWG.Done()
	}
	state.mu.Unlock()
	go func() {
		_ = s.evictRoom(room, state)
		close(done)
	}()
}

func (s *Service) roomFailure(room string) error {
	state := s.room(room)
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
// then reloads the persisted document into a new room state.
func (s *Service) awaitRoomRecovery(ctx context.Context, room string) error {
	state := s.room(room)
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
		select i.closed_at is null
		from artifacts a join issues i on i.key = a.issue_key
		where a.id = $1
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
	})
	return value.(*roomState)
}

var _ API = (*Service)(nil)
