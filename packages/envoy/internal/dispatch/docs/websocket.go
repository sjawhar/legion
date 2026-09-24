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
	"github.com/reearth/ygo/persistence"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

type connectionState struct {
	room  string
	id    uint64
	actor model.Actor
	added bool
}

type connectionContextKey struct{}

type ownerVerifiedContextKey struct{}

// ownerVerifiedToken is installed only on the server's own calls, by a caller that already
// knows the document's owner state: a settlement, which holds the owner row locked in its
// transaction, or the block-id backfill, which is a command run against a database nobody is
// serving from. Their injections skip the issue read in allowInject - for the settlement that
// read would be a second pooled connection taken while its transaction is open
// (store.ErrNestedAcquire), and for the backfill it is a question already answered.
type ownerVerifiedToken struct{ _ byte }

func withOwnerVerified(ctx context.Context) context.Context {
	return context.WithValue(ctx, ownerVerifiedContextKey{}, &ownerVerifiedToken{})
}

func isOwnerVerified(ctx context.Context) bool {
	_, ok := ctx.Value(ownerVerifiedContextKey{}).(*ownerVerifiedToken)
	return ok
}

// servicePersistenceAdapter observes ygo's otherwise asynchronous persistence
// callbacks. A failed update evicts its room so the next access reloads durable
// state instead of retaining unsaved live state.
type servicePersistenceAdapter struct {
	store   VersionedStore
	service *Service
}

type classifiedUpdateStore interface {
	AppendUpdateWithClass(context.Context, string, []byte, bool) (persistence.Version, error)
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
	if a.service.roomFailed(room) {
		return nil
	}
	contentChanged, durable, found := a.service.consumeUpdateClass(room, update)
	if found && durable {
		defer a.service.finishDurableAppend(room)
	}
	if a.service.consumeSuppressedPersistence(room, update) || a.service.roomFailed(room) {
		return nil
	}
	var err error
	if store, ok := a.store.(classifiedUpdateStore); ok {
		_, err = store.AppendUpdateWithClass(context.Background(), room, update, contentChanged)
	} else {
		_, err = a.store.AppendUpdate(context.Background(), room, update)
	}
	if err != nil {
		a.service.failRoom(room, err)
		return err
	}
	if found && durable && contentChanged {
		a.service.scheduleSettleAfterAppend(room)
	}
	return nil
}

func (a *servicePersistenceAdapter) StoreUpdateContext(ctx context.Context, room string, update []byte) error {
	if a.service.roomFailed(room) {
		return nil
	}
	contentChanged, durable, found := a.service.consumeUpdateClass(room, update)
	if found && durable {
		defer a.service.finishDurableAppend(room)
	}
	if a.service.consumeSuppressedPersistence(room, update) || a.service.roomFailed(room) {
		return nil
	}
	var err error
	if store, ok := a.store.(classifiedUpdateStore); ok {
		_, err = store.AppendUpdateWithClass(ctx, room, update, contentChanged)
	} else {
		_, err = a.store.AppendUpdate(ctx, room, update)
	}
	if err != nil {
		a.service.failRoom(room, err)
		return err
	}
	if found && durable && contentChanged {
		a.service.scheduleSettleAfterAppend(room)
	}
	return nil
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
	if _, err := s.issueOpen(r.Context(), s.queryFrom(r.Context()), room); err != nil {
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
	open, err := s.issueOpen(r.Context(), s.queryFrom(r.Context()), room)
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
	return websocket.ConnectionConfig{
		ReadOnly: schemaReadOnly(open, r.URL.Query().Get("schema_version")),
	}, true
}

func schemaReadOnly(open bool, clientSchemaVersion string) bool {
	return !open || clientSchemaVersion != fmt.Sprintf("%d", pmdoc.SchemaVersion())
}

// authorizeSchemaVersion confirms the existing HTTP-authorized connection's schema admission
// through Hocuspocus's authenticated scope, which is the provider's client-visible signal.
func (s *Service) authorizeSchemaVersion(room, clientSchemaVersion string) (websocket.ConnectionConfig, error) {
	open, err := s.issueOpen(context.Background(), s.queryFrom(context.Background()), room)
	if err != nil {
		return websocket.ConnectionConfig{}, err
	}
	return websocket.ConnectionConfig{ReadOnly: schemaReadOnly(open, clientSchemaVersion)}, nil
}

func (s *Service) requestActor(r *http.Request) (model.Actor, error) {
	if authorization := strings.TrimSpace(r.Header.Get("Authorization")); authorization != "" {
		if s.agentToken == "" || authorization != "Bearer "+s.agentToken {
			return model.Actor{}, errors.New("invalid document bearer token")
		}
		var supplied model.Actor
		if err := json.Unmarshal([]byte(r.Header.Get("X-Dispatch-Actor")), &supplied); err != nil {
			return model.Actor{}, fmt.Errorf("decode document bearer actor: %w", err)
		}
		if supplied.Kind != "session" || strings.TrimSpace(supplied.ID) == "" {
			return model.Actor{}, errors.New("document bearer requires a session actor")
		}
		// Rebuilt field by field, exactly as the HTTP API's bearerSessionActor does:
		// the header is caller-supplied, and Owner and Service are the server's to
		// set. Service means "the server verified this Kubernetes subject", so a
		// shared-token holder copying one into the header would forge a verified
		// identity onto every document version it writes.
		return model.Actor{Kind: "session", ID: supplied.ID, Origin: supplied.Origin}, nil
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

// allowInject decides whether ygo may apply an injection to a room. Its issue read goes
// through the shared pool for a caller that need hold no connection of its own (the
// settlement warm-up in settleRoom), and that is outside the pool's deadlock cycle only
// because ygo runs OnInject before getOrCreateRoom (reearth/ygo v1.49.5,
// provider/websocket/inject.go:311-320): an injection refused here has published no room
// placeholder for a connection-holder to park on, so nothing holding a connection is waiting
// on this read. A vendored reordering of those two calls puts it back in the cycle.
func (s *Service) allowInject(ctx context.Context, info websocket.InjectInfo) error {
	if s.shuttingDown(info.Room) {
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

func (s *Service) onLoadDocument(ctx context.Context, room string, doc *crdt.Doc) error {
	if err := s.awaitRoomRecovery(ctx, room); err != nil {
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
	open, err := s.issueOpen(ctx, rooms, room)
	if err != nil {
		return err
	}
	tree, err := treeOf(doc)
	if err != nil {
		slog.Error("dispatch: loaded document outside Proof schema", "room", room, "error", err)
		return err
	}
	state := s.room(room)
	state.mu.Lock()
	state.closed = !open
	state.contentTree = pmdoc.StripAnchorMarks(tree)
	state.mu.Unlock()
	doc.OnUpdate(func(update []byte, origin any) {
		if _, identityRepair := origin.(*identityClosureOrigin); identityRepair {
			return
		}
		contentChanged := s.updateChangesMarkdown(room, doc)
		s.recordUpdateClass(room, update, contentChanged, true)
		if contentChanged {
			s.recordConnectedActors(room, origin)
		}
		s.scheduleSettle(room)
	})
	return nil
}

func (s *Service) updateChangesMarkdown(room string, doc *crdt.Doc) bool {
	tree, err := treeOf(doc)
	if err != nil {
		slog.Error("dispatch: read updated document", "room", room, "error", err)
		return true
	}
	content := pmdoc.StripAnchorMarks(tree)
	state := s.room(room)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.contentTree != nil && state.contentTree.Equal(content) {
		return false
	}
	state.contentTree = content
	return true
}

// recordConnectedActors credits an observed document update to the room's connected peers,
// who all join `pending`. A service mutation (origin registered by serviceTransact) was
// already credited to its actor by recordActor; any other update is a browser edit by one of
// the peers, so when exactly one peer is connected it is the latest edit source and replaces
// `lastActor`, and otherwise the edit cannot be pinned on a single peer and no older actor
// may stand in for it.
func (s *Service) recordConnectedActors(room string, origin any) {
	_, service := s.serviceOrigins.Load(origin)
	state := s.room(room)
	state.mu.Lock()
	defer state.mu.Unlock()
	var sole *model.Actor
	ambiguous := false
	for _, actor := range state.connected {
		key := actorKey(actor)
		state.pending[key] = actor
		if sole == nil {
			sole = new(actor)
		} else if key != actorKey(*sole) {
			ambiguous = true
		}
	}
	if service {
		return
	}
	if ambiguous {
		sole = nil
	}
	state.lastActor = sole
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

func (s *Service) settleLastPeer(_ context.Context, room string) {
	state := s.room(room)
	state.mu.Lock()
	if !s.stopSettleTimer(state.settle) {
		state.mu.Unlock()
		return
	}
	generation := state.gen
	state.mu.Unlock()
	s.settleRoom(room, generation)
}
