package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"strings"

	"github.com/google/uuid"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

type connectionState struct {
	room  string
	id    uint64
	actor model.Actor
	added bool
}

type connectionContextKey struct{}

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

// ServeHTTP serves the Hocuspocus-framed document websocket endpoint.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	room := r.PathValue("room")
	if room == "" {
		room = path.Base(r.URL.Path)
	}
	if _, err := s.requestActor(r); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if _, err := uuid.Parse(room); err != nil {
		http.Error(w, "document not found", http.StatusNotFound)
		return
	}
	if _, err := s.issueOpen(r.Context(), room); err != nil {
		http.Error(w, "document not found", http.StatusNotFound)
		return
	}
	if err := s.awaitRoomRecovery(r.Context(), room); err != nil {
		http.Error(w, ErrServiceUnavailable.Error(), http.StatusServiceUnavailable)
		return
	}
	if !s.canOpenRoom(room) || !s.canAddConnection() {
		http.Error(w, ErrServiceUnavailable.Error(), http.StatusServiceUnavailable)
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

func (s *Service) authorize(r *http.Request) (websocket.ConnectionConfig, bool) {
	actor, err := s.requestActor(r)
	if err != nil {
		return websocket.ConnectionConfig{}, false
	}
	room := r.PathValue("room")
	if room == "" {
		room = path.Base(r.URL.Path)
	}
	if !s.canAddConnection() {
		return websocket.ConnectionConfig{}, false
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
	if _, err := treeOf(doc); err != nil {
		slog.Error("dispatch: loaded document outside Proof schema", "room", room, "error", err)
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
