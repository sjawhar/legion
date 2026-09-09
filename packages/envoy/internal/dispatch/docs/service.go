package docs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
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
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
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
	Settle      time.Duration
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
	settle         time.Duration
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
	failed          error
	failedDone      chan struct{}
	closed          bool
	mu              sync.Mutex
}

type versionPending struct {
	generation uint64
	authors    map[string]model.Actor
}

type connectionState struct {
	room  string
	id    uint64
	actor model.Actor
	added bool
}

type connectionContextKey struct{}

type suppressSlot struct {
	ready    chan struct{}
	update   []byte
	canceled bool
	consumed bool
}

// servicePersistenceAdapter observes ygo's otherwise asynchronous persistence
// callbacks. A failed update evicts its room so the next access reloads durable
// state instead of retaining unsaved live state.
type servicePersistenceAdapter struct {
	store   VersionedStore
	service *Service
}

func (a *servicePersistenceAdapter) LoadDoc(room string) ([]byte, error) {
	result, err := a.store.Load(context.Background(), room)
	if err == nil {
		err = validateUpdate(result.Update)
	}
	if err != nil {
		a.service.failRoom(room, err)
		return nil, err
	}
	return result.Update, nil
}

func (a *servicePersistenceAdapter) StoreUpdate(room string, update []byte) error {
	if a.service.roomFailed(room) || a.service.consumeSuppressedPersistence(room, update) {
		return nil
	}
	_, err := a.store.AppendUpdate(context.Background(), room, update)
	if err != nil {
		a.service.failRoom(room, err)
	}
	return err
}

func (a *servicePersistenceAdapter) StoreUpdateContext(ctx context.Context, room string, update []byte) error {
	if a.service.roomFailed(room) || a.service.consumeSuppressedPersistence(room, update) {
		return nil
	}
	_, err := a.store.AppendUpdate(ctx, room, update)
	if err != nil {
		a.service.failRoom(room, err)
	}
	return err
}

func (a *servicePersistenceAdapter) Compact(ctx context.Context, room string) error {
	_, err := a.store.Compact(ctx, room, 500)
	return err
}

func validateUpdate(update []byte) error {
	if len(update) == 0 {
		return nil
	}
	return crdt.ApplyUpdateV1(crdt.New(), update, nil)
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

func (s *Service) applyLive(ctx context.Context, artifactID string, mutate func(*crdt.Doc, func(func(*crdt.Transaction))) bool) error {
	tx, joinedTransaction := txFromContext(ctx)
	var slot *suppressSlot
	if joinedTransaction {
		slot = s.prepareSuppressedPersistence(artifactID)
	}
	changed := false
	var updates [][]byte
	err := s.srv.Apply(ctx, artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		var unsubscribe func()
		if joinedTransaction {
			unsubscribe = doc.OnUpdate(func(update []byte, _ any) {
				updates = append(updates, append([]byte(nil), update...))
			})
			defer unsubscribe()
		}
		changed = mutate(doc, transact)
	})
	if !joinedTransaction {
		return err
	}
	if err != nil || !changed {
		s.cancelSuppressedPersistence(artifactID, slot)
		return err
	}
	update, err := mergeUpdates(updates)
	if err != nil {
		s.cancelSuppressedPersistence(artifactID, slot)
		return err
	}
	s.finishSuppressedPersistence(slot, update)
	if _, err := s.persistence.AppendUpdateTx(ctx, tx, artifactID, update); err != nil {
		s.failRoom(artifactID, err)
		return fmt.Errorf("append transactional live document update: %w", err)
	}
	return nil
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

func (s *Service) validateRoomLoad(ctx context.Context, room string) error {
	result, err := s.persistence.Load(ctx, room)
	if err == nil {
		err = validateUpdate(result.Update)
	}
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		s.failRoom(room, err)
	}
	return err
}

// New constructs the ygo server used by Dispatch's document API and websocket
// endpoint.
func New(deps Deps) *Service {
	settle := deps.Settle
	if settle <= 0 {
		settle = 2 * time.Second
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
		settle:      settle,
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

// ServeHTTP serves the Hocuspocus-framed document websocket endpoint.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	room := r.PathValue("room")
	if room == "" {
		room = path.Base(r.URL.Path)
	}
	if err := s.awaitRoomRecovery(r.Context(), room); err != nil {
		http.Error(w, ErrServiceUnavailable.Error(), http.StatusServiceUnavailable)
		return
	}
	if _, err := s.requestActor(r); err != nil {
		s.srv.ServeHTTP(w, r)
		return
	}
	if s.srv.GetDoc(room) == nil {
		if err := s.validateRoomLoad(r.Context(), room); err != nil {
			http.Error(w, ErrServiceUnavailable.Error(), http.StatusServiceUnavailable)
			return
		}
	}
	connection := &connectionState{}
	ctx := context.WithValue(r.Context(), connectionContextKey{}, connection)
	s.srv.ServeHTTP(w, r.WithContext(ctx))
	if connection.added {
		s.removeConnection(connection.room, connection.id)
	}
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

// SeedText writes a new room's first Yjs update inside the caller's artifact
// creation transaction. A new room is loaded from this update on first use.
func (s *Service) SeedText(ctx context.Context, tx pgx.Tx, artifactID, markdown string) error {
	doc := crdt.New()
	content := doc.GetText("content")
	doc.Transact(func(transaction *crdt.Transaction) {
		content.Insert(transaction, 0, markdown, nil)
	})
	if _, err := s.persistence.AppendUpdateTx(ctx, tx, artifactID, crdt.EncodeStateAsUpdateV1(doc, nil)); err != nil {
		return fmt.Errorf("seed live document: %w", err)
	}
	return nil
}

// ReplaceText replaces the entire live Yjs text so connected clients receive
// document uploads as a regular server-side transaction.
func (s *Service) ReplaceText(ctx context.Context, artifactID, markdown string, actor model.Actor) error {
	var unchanged bool
	err := s.applyLive(ctx, artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) bool {
		content := doc.GetText("content")
		length := content.Len()
		if content.ToString() == markdown {
			unchanged = true
			return false
		}
		s.recordActor(artifactID, actor)
		transact(func(transaction *crdt.Transaction) {
			if length > 0 {
				content.Delete(transaction, 0, length)
			}
			if markdown != "" {
				content.Insert(transaction, 0, markdown, nil)
			}
		})
		return true
	})
	if unchanged && errors.Is(err, websocket.ErrNoChanges) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("replace live document: %w", err)
	}
	return nil
}

// Text returns the current Yjs text, loading and rendering persisted state when
// the document room is not resident.
func (s *Service) Text(ctx context.Context, artifactID string) (string, error) {
	if err := s.awaitRoomRecovery(ctx, artifactID); err != nil {
		return "", err
	}
	if doc := s.srv.GetDoc(artifactID); doc != nil {
		return doc.GetText("content").ToString(), nil
	}
	loaded, err := s.persistence.Load(ctx, artifactID)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", err
		}
		s.failRoom(artifactID, err)
		return "", fmt.Errorf("%w: %w", ErrServiceUnavailable, err)
	}
	if len(loaded.Update) == 0 {
		return "", nil
	}
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, loaded.Update, nil); err != nil {
		s.failRoom(artifactID, fmt.Errorf("decode live document: %w", err))
		return "", fmt.Errorf("%w: decode live document: %w", ErrServiceUnavailable, err)
	}
	return doc.GetText("content").ToString(), nil
}

// SnapshotVersion returns the current immutable version, adding an unnamed
// version only when the live text has diverged since the previous one.
func (s *Service) SnapshotVersion(ctx context.Context, tx pgx.Tx, artifactID string, actor model.Actor) (model.Version, bool, error) {
	markdown, capture, authors, err := s.captureLiveTextAndAuthors(ctx, artifactID, nil)
	if err != nil {
		return model.Version{}, false, err
	}
	latest, err := latestVersion(ctx, tx, artifactID)
	if err != nil {
		return model.Version{}, false, err
	}
	if latest.markdown == markdown {
		return latest.Version, false, nil
	}
	capture.authors[actorKey(actor)] = actor
	authors = actorSlice(capture.authors)
	version, err := writeVersion(ctx, tx, artifactID, markdown, false, nil, authors)
	if err != nil {
		return model.Version{}, false, err
	}
	s.rememberPendingVersion(artifactID, version, capture)
	return version, true, nil
}

// CommitVersion clears authors consumed by a version only after its enclosing
// transaction has committed.
func (s *Service) CommitVersion(artifactID string, version model.Version) {
	state := s.room(artifactID)
	state.mu.Lock()
	defer state.mu.Unlock()
	capture, ok := state.pendingVersions[version.Number]
	if !ok {
		return
	}
	delete(state.pendingVersions, version.Number)
	if state.gen != capture.generation {
		return
	}
	for key := range capture.authors {
		delete(state.pending, key)
	}
}

// ApplyOps resolves every requested operation against the document locked by
// Server.Apply, then applies the complete plan in one transaction.
func (s *Service) ApplyOps(ctx context.Context, artifactID string, ops []model.EditOp, actor model.Actor) (int, error) {
	if len(ops) == 0 {
		return 0, nil
	}
	var applyErr error
	err := s.applyLive(ctx, artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) bool {
		content := doc.GetText("content")
		mutations, err := resolveOperations(content.ToString(), ops)
		if err != nil {
			applyErr = err
			return false
		}
		s.recordActor(artifactID, actor)
		transact(func(transaction *crdt.Transaction) {
			for _, mutation := range mutations {
				if mutation.to > mutation.from {
					content.Delete(transaction, mutation.from, mutation.to-mutation.from)
				}
				if mutation.with != "" {
					content.Insert(transaction, mutation.from, mutation.with, nil)
				}
			}
		})
		return true
	})
	if applyErr != nil {
		return 0, applyErr
	}
	if err != nil {
		return 0, fmt.Errorf("apply live document operations: %w", err)
	}
	return len(ops), nil
}

// ApplyReplace resolves a stored anchor against the document locked by
// Server.Apply and replaces only its resolved range.
func (s *Service) ApplyReplace(ctx context.Context, artifactID string, anchor model.Anchor, with string, actor model.Actor) error {
	var applyErr error
	err := s.applyLive(ctx, artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) bool {
		content := doc.GetText("content")
		resolved := text.Reresolve(content.ToString(), anchor)
		if resolved.Orphaned {
			applyErr = text.ErrTargetNotFound
			return false
		}
		s.recordActor(artifactID, actor)
		transact(func(transaction *crdt.Transaction) {
			if resolved.To > resolved.From {
				content.Delete(transaction, resolved.From, resolved.To-resolved.From)
			}
			if with != "" {
				content.Insert(transaction, resolved.From, with, nil)
			}
		})
		return true
	})
	if applyErr != nil {
		return applyErr
	}
	if err != nil {
		return fmt.Errorf("apply live document replacement: %w", err)
	}
	return nil
}

// NamedVersion records the live text as a deliberately named immutable version.
func (s *Service) NamedVersion(ctx context.Context, artifactID, summary string, actor model.Actor) (model.Version, error) {
	markdown, capture, authors, err := s.captureLiveTextAndAuthors(ctx, artifactID, &actor)
	if err != nil {
		return model.Version{}, err
	}
	_, joinedTransaction := txFromContext(ctx)
	var version model.Version
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		var open bool
		if err := tx.QueryRow(ctx, `
			select i.closed_at is null
			from artifacts a join issues i on i.key = a.issue_key
			where a.id = $1 for update
		`, artifactID).Scan(&open); err != nil {
			return fmt.Errorf("lock document artifact: %w", err)
		}
		if !open {
			return ErrIssueClosed
		}

		version, err = writeVersion(ctx, tx, artifactID, markdown, true, new(summary), authors)
		return err
	})
	if err != nil {
		return model.Version{}, err
	}
	if joinedTransaction {
		s.rememberPendingVersion(artifactID, version, capture)
	} else {
		s.clearPending(capture, artifactID)
	}
	return version, nil
}

func (s *Service) CompactAll(ctx context.Context, keep int) error {
	rows, err := s.store.Pool.Query(ctx, `select id::text from artifacts where kind = 'doc'`)
	if err != nil {
		return fmt.Errorf("list document rooms: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var artifactID string
		if err := rows.Scan(&artifactID); err != nil {
			return fmt.Errorf("scan document room: %w", err)
		}
		if _, err := s.persistence.Compact(ctx, artifactID, keep); err != nil {
			return fmt.Errorf("compact document %s: %w", artifactID, err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list document rooms: %w", err)
	}
	return nil
}

func (s *Service) authorize(r *http.Request) (websocket.ConnectionConfig, bool) {
	actor, err := s.requestActor(r)
	if err != nil {
		return websocket.ConnectionConfig{}, false
	}
	room := r.PathValue("room")
	if room == "" {
		room = path.Base(r.URL.Path)
	}
	if s.roomFailure(room) != nil {
		return websocket.ConnectionConfig{}, false
	}
	open, err := s.issueOpen(r.Context(), room)
	if err != nil {
		return websocket.ConnectionConfig{}, false
	}
	connection, _ := r.Context().Value(connectionContextKey{}).(*connectionState)
	if connection == nil {
		return websocket.ConnectionConfig{}, false
	}
	connection.room = room
	connection.id = s.nextConnection.Add(1)
	connection.actor = actor
	connection.added = true
	s.addConnection(room, connection.id, actor)
	return websocket.ConnectionConfig{ReadOnly: !open}, true
}

func (s *Service) requestActor(r *http.Request) (model.Actor, error) {
	if authorization := strings.TrimSpace(r.Header.Get("Authorization")); authorization != "" {
		if s.agentToken == "" || authorization != "Bearer "+s.agentToken {
			return model.Actor{}, errors.New("invalid document bearer token")
		}
		var actor model.Actor
		if err := json.Unmarshal([]byte(r.Header.Get("X-Dispatch-Actor")), &actor); err != nil {
			return model.Actor{}, fmt.Errorf("decode document bearer actor: %w", err)
		}
		if actor.Kind != "session" || strings.TrimSpace(actor.ID) == "" {
			return model.Actor{}, errors.New("document bearer requires a session actor")
		}
		return actor, nil
	}
	if s.identity == nil {
		return model.Actor{}, errors.New("document identity required")
	}
	login, err := s.identity.Login(r)
	if err != nil {
		return model.Actor{}, err
	}
	return model.Actor{Kind: "user", ID: login}, nil
}

func (s *Service) allowInject(ctx context.Context, info websocket.InjectInfo) error {
	if err := s.awaitRoomRecovery(ctx, info.Room); err != nil {
		return err
	}
	if s.roomClosed(info.Room) {
		return ErrIssueClosed
	}
	open, err := s.issueOpen(ctx, info.Room)
	if err != nil {
		return err
	}
	if !open {
		return ErrIssueClosed
	}
	return nil
}

func (s *Service) onLoadDocument(ctx context.Context, room string, doc *crdt.Doc) error {
	if err := s.awaitRoomRecovery(ctx, room); err != nil {
		return err
	}
	open, err := s.issueOpen(ctx, room)
	if err != nil {
		return err
	}
	state := s.room(room)
	state.mu.Lock()
	state.closed = !open
	state.mu.Unlock()
	doc.OnUpdate(func(_ []byte, _ any) {
		s.recordConnectedActors(room)
		s.scheduleSettle(room)
	})
	return nil
}

func (s *Service) scheduleSettle(room string) {
	if s.stopping.Load() {
		return
	}
	state := s.room(room)
	state.mu.Lock()
	defer state.mu.Unlock()
	if s.stopping.Load() || state.closed || state.failed != nil {
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
	markdown := doc.GetText("content").ToString()
	state.mu.Unlock()

	ctx := context.Background()
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return
	}
	defer tx.Rollback(ctx)
	var issueKey, artifactName string
	var open bool
	if err := tx.QueryRow(ctx, `
		select a.issue_key, a.name, i.closed_at is null
		from artifacts a join issues i on i.key = a.issue_key
		where a.id = $1 for update
	`, room).Scan(&issueKey, &artifactName, &open); err != nil || !open {
		return
	}
	latest, err := latestVersion(ctx, tx, room)
	if err != nil {
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
	markdown = doc.GetText("content").ToString()
	pending := make(map[string]model.Actor, len(state.pending))
	for key, actor := range state.pending {
		pending[key] = actor
	}
	state.mu.Unlock()
	if latest.markdown == markdown {
		return
	}
	authors := actorSlice(pending)
	version, err := writeVersion(ctx, tx, room, markdown, false, nil, authors)
	if err != nil {
		return
	}
	if err := s.reresolveAnchors(ctx, tx, room, markdown); err != nil {
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
		return
	}
	if err := tx.Commit(ctx); err != nil {
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
		_ = s.srv.CloseRoom(room, true)
		s.rooms.CompareAndDelete(room, state)
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

func (s *Service) reresolveAnchors(ctx context.Context, tx pgx.Tx, artifactID, markdown string) error {
	for _, target := range []struct {
		table string
		where string
	}{
		{table: "asks", where: "state = 'open'"},
		{table: "comments", where: "not resolved"},
	} {
		rows, err := tx.Query(ctx, fmt.Sprintf(`
			select id::text, anchor from %s
			where anchor is not null and anchor->>'artifact_id' = $1 and %s
		`, target.table, target.where), artifactID)
		if err != nil {
			return fmt.Errorf("list %s anchors: %w", target.table, err)
		}
		type row struct {
			id      string
			encoded []byte
		}
		var anchored []row
		for rows.Next() {
			var anchor row
			if err := rows.Scan(&anchor.id, &anchor.encoded); err != nil {
				rows.Close()
				return fmt.Errorf("scan %s anchor: %w", target.table, err)
			}
			anchored = append(anchored, anchor)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("list %s anchors: %w", target.table, err)
		}
		rows.Close()
		for _, row := range anchored {
			var anchor model.Anchor
			if err := json.Unmarshal(row.encoded, &anchor); err != nil {
				return fmt.Errorf("decode %s anchor: %w", target.table, err)
			}
			encoded, err := json.Marshal(text.Reresolve(markdown, anchor))
			if err != nil {
				return fmt.Errorf("encode %s anchor: %w", target.table, err)
			}
			if _, err := tx.Exec(ctx, fmt.Sprintf(`update %s set anchor = $2 where id = $1`, target.table), row.id, encoded); err != nil {
				return fmt.Errorf("update %s anchor: %w", target.table, err)
			}
		}
	}
	return nil
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

func (s *Service) recordActor(room string, actor model.Actor) {
	state := s.room(room)
	state.mu.Lock()
	state.pending[actorKey(actor)] = actor
	state.mu.Unlock()
}

func (s *Service) captureLiveTextAndAuthors(ctx context.Context, room string, actor *model.Actor) (string, versionPending, []model.Actor, error) {
	if s.srv.GetDoc(room) == nil {
		err := s.srv.Apply(ctx, room, func(_ *crdt.Doc, _ func(func(*crdt.Transaction))) {})
		if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
			return "", versionPending{}, nil, fmt.Errorf("warm live document: %w", err)
		}
	}

	state := s.room(room)
	state.mu.Lock()
	defer state.mu.Unlock()
	doc := s.srv.GetDoc(room)
	if doc == nil {
		return "", versionPending{}, nil, errors.New("warm live document did not retain room")
	}
	capture, authors := captureAuthors(state, actor)
	return doc.GetText("content").ToString(), capture, authors, nil
}

func captureAuthors(state *roomState, actor *model.Actor) (versionPending, []model.Actor) {
	authors := make(map[string]model.Actor, len(state.pending)+1)
	for key, pendingActor := range state.pending {
		authors[key] = pendingActor
	}
	if actor != nil {
		authors[actorKey(*actor)] = *actor
	}
	capture := versionPending{generation: state.gen, authors: authors}
	return capture, actorSlice(authors)
}

func (s *Service) rememberPendingVersion(room string, version model.Version, capture versionPending) {
	state := s.room(room)
	state.mu.Lock()
	state.pendingVersions[version.Number] = capture
	state.mu.Unlock()
}

func (s *Service) clearPending(capture versionPending, room string) {
	state := s.room(room)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.gen != capture.generation {
		return
	}
	for key := range capture.authors {
		delete(state.pending, key)
	}
}

func (s *Service) recordConnectedActors(room string) {
	state := s.room(room)
	state.mu.Lock()
	for _, actor := range state.connected {
		state.pending[actorKey(actor)] = actor
	}
	state.mu.Unlock()
}

func (s *Service) addConnection(room string, id uint64, actor model.Actor) {
	state := s.room(room)
	state.mu.Lock()
	state.connected[id] = actor
	state.pending[actorKey(actor)] = actor
	state.mu.Unlock()
}

func (s *Service) removeConnection(room string, id uint64) {
	state := s.room(room)
	state.mu.Lock()
	delete(state.connected, id)
	state.mu.Unlock()
}

func latestVersion(ctx context.Context, tx pgx.Tx, artifactID string) (struct {
	model.Version
	markdown string
}, error) {
	var version struct {
		model.Version
		markdown string
	}
	var authors []byte
	if err := tx.QueryRow(ctx, `
		select number, named, summary, authors, created_at, markdown
		from artifact_versions where artifact_id = $1
		order by number desc limit 1
	`, artifactID).Scan(&version.Number, &version.Named, &version.Summary, &authors, &version.CreatedAt, &version.markdown); err != nil {
		return version, fmt.Errorf("read latest document version: %w", err)
	}
	if err := json.Unmarshal(authors, &version.Authors); err != nil {
		return version, fmt.Errorf("decode latest document version authors: %w", err)
	}
	return version, nil
}

func writeVersion(ctx context.Context, tx pgx.Tx, artifactID, markdown string, named bool, summary *string, authors []model.Actor) (model.Version, error) {
	encodedAuthors, err := json.Marshal(authors)
	if err != nil {
		return model.Version{}, fmt.Errorf("encode document version authors: %w", err)
	}
	var version model.Version
	var authorsRaw []byte
	if err := tx.QueryRow(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors, named, summary)
		select $1, coalesce(max(number), 0) + 1, $2, $3, $4, $5
		from artifact_versions where artifact_id = $1
		returning number, named, summary, authors, created_at
	`, artifactID, markdown, encodedAuthors, named, summary).Scan(
		&version.Number, &version.Named, &version.Summary, &authorsRaw, &version.CreatedAt,
	); err != nil {
		return model.Version{}, fmt.Errorf("write document version: %w", err)
	}
	if err := json.Unmarshal(authorsRaw, &version.Authors); err != nil {
		return model.Version{}, fmt.Errorf("decode document version authors: %w", err)
	}
	if err := indexDocumentReferences(ctx, tx, artifactID, markdown); err != nil {
		return model.Version{}, err
	}
	return version, nil
}

func indexDocumentReferences(ctx context.Context, tx pgx.Tx, artifactID, markdown string) error {
	if _, err := tx.Exec(ctx, `delete from refs where from_kind = 'artifact' and from_id = $1`, artifactID); err != nil {
		return fmt.Errorf("clear document references: %w", err)
	}
	for _, ref := range text.Extract(markdown, "") {
		if ref.Kind == "url" {
			continue
		}
		toID := ref.ID
		if ref.Kind == "artifact" {
			toID = ref.IssueKey + "/" + ref.ID
		}
		if _, err := tx.Exec(ctx, `
			insert into refs (from_kind, from_id, to_kind, to_id)
			values ('artifact', $1, $2, $3)
			on conflict do nothing
		`, artifactID, ref.Kind, toID); err != nil {
			return fmt.Errorf("write document reference: %w", err)
		}
	}
	return nil
}

type textMutation struct {
	from int
	to   int
	with string
}

func resolveOperations(markdown string, ops []model.EditOp) ([]textMutation, error) {
	mutations := make([]textMutation, 0, len(ops))
	for index, op := range ops {
		mutation, err := resolveOperation(markdown, op)
		if err != nil {
			return nil, fmt.Errorf("operation %d: %w", index, err)
		}
		mutations = append(mutations, mutation)
		markdown = replaceRange(markdown, mutation.from, mutation.to, mutation.with)
	}
	return mutations, nil
}

func resolveOperation(markdown string, op model.EditOp) (textMutation, error) {
	switch op.Op {
	case "replace":
		if op.Find == "" {
			return textMutation{}, invalidOp("find")
		}
		from, to, err := text.Resolve(markdown, op.Find, op.Occurrence)
		if err != nil {
			return textMutation{}, err
		}
		return textMutation{from: from, to: to, with: op.With}, nil
	case "delete":
		if op.Find == "" {
			return textMutation{}, invalidOp("find")
		}
		from, to, err := text.Resolve(markdown, op.Find, op.Occurrence)
		if err != nil {
			return textMutation{}, err
		}
		return textMutation{from: from, to: to}, nil
	case "insert":
		if op.Markdown == "" {
			return textMutation{}, invalidOp("markdown")
		}
		if (op.After == "" && op.Before == "") || (op.After != "" && op.Before != "") {
			return textMutation{}, invalidOp("after or before")
		}
		anchor := op.After
		after := anchor != ""
		if !after {
			anchor = op.Before
		}
		position, err := insertPosition(markdown, anchor, after, op.Occurrence)
		if err != nil {
			return textMutation{}, err
		}
		return textMutation{from: position, to: position, with: op.Markdown}, nil
	default:
		return textMutation{}, invalidOp("op")
	}
}

func insertPosition(markdown, anchor string, after bool, occurrence *int) (int, error) {
	switch anchor {
	case "start":
		return 0, nil
	case "end":
		return text.Len16(markdown), nil
	}
	if title, ok := strings.CutPrefix(anchor, "heading:"); ok {
		if title == "" {
			return 0, invalidOp("heading")
		}
		return headingPosition(markdown, title, after, occurrence)
	}
	from, to, err := text.Resolve(markdown, anchor, occurrence)
	if err != nil {
		return 0, err
	}
	if after {
		return to, nil
	}
	return from, nil
}

func headingPosition(markdown, title string, after bool, occurrence *int) (int, error) {
	position := 0
	var positions []int
	for _, line := range strings.SplitAfter(markdown, "\n") {
		trimmed := strings.TrimSuffix(line, "\n")
		heading := strings.TrimLeft(trimmed, " ")
		if strings.HasPrefix(heading, "#") {
			marker := strings.TrimLeft(heading, "#")
			if len(marker) < len(heading) && strings.HasPrefix(marker, " ") && strings.TrimSpace(marker) == title {
				if after {
					positions = append(positions, position+text.Len16(line))
				} else {
					positions = append(positions, position)
				}
			}
		}
		position += text.Len16(line)
	}
	if len(positions) == 0 {
		return 0, text.ErrTargetNotFound
	}
	if occurrence != nil {
		if *occurrence < 0 || *occurrence >= len(positions) {
			return 0, text.ErrTargetNotFound
		}
		return positions[*occurrence], nil
	}
	if len(positions) > 1 {
		return 0, &text.ErrTargetAmbiguous{}
	}
	return positions[0], nil
}

func replaceRange(markdown string, from, to int, with string) string {
	return text.Slice16(markdown, 0, from) + with + text.Slice16(markdown, to, text.Len16(markdown))
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

// ErrInvalidOp identifies the malformed user-facing operation field.
type ErrInvalidOp struct {
	Field string
}

func (e *ErrInvalidOp) Error() string {
	return fmt.Sprintf("invalid document operation field %q", e.Field)
}

func invalidOp(field string) error {
	return &ErrInvalidOp{Field: field}
}

func (s *Service) withTx(ctx context.Context, fn func(pgx.Tx) error) error {
	if tx, ok := txFromContext(ctx); ok {
		return fn(tx)
	}
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin document transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit document transaction: %w", err)
	}
	return nil
}

var _ API = (*Service)(nil)
