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
	"time"

	"github.com/google/uuid"
	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
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
// transaction; the publish of a committed live write, whose transaction held that row until it
// committed; or the block-id backfill, which is a command run against a database nobody is
// serving from. Their injections skip the issue read in allowInject - for the settlement that
// read would be a second pooled connection taken while its transaction is open
// (store.ErrNestedAcquire), for the publish a read other writers' connections can starve (see
// publishLiveUpdate), and for the backfill it is a question already answered.
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

// roomDocument is the document a connection to room would sync: the resident room's, or else the
// durable one its load would decode, nil when nothing is persisted. A durable history that does
// not decode fails the room, as ygo's own load of it would.
func (s *Service) roomDocument(ctx context.Context, room string) (*crdt.Doc, error) {
	if doc := s.srv.GetDoc(room); doc != nil {
		return doc, nil
	}
	result, err := s.persistence.Load(ctx, room)
	var doc *crdt.Doc
	if err == nil && len(result.Update) > 0 {
		doc = crdt.New()
		err = crdt.ApplyUpdateV1(doc, result.Update, nil)
	}
	if err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			s.failRoom(room, err)
		}
		return nil, err
	}
	return doc, nil
}

// documentSchemaCloseCode closes a document websocket whose room is outside the Proof schema. It
// is in the private 4000-4999 range beside Hocuspocus's own 4401 and 4403, and the dashboard reads
// it as the document's repair state rather than as a dropped connection to retry.
const documentSchemaCloseCode = 4409

// refuseOutsideSchema completes the upgrade only to close it with documentSchemaCloseCode before
// anything of the document is sent: a browser reads a close code, never the status of a refused
// upgrade. The zero upgrader keeps the same-origin rule ygo's own upgrade applies with no
// AllowedOrigins configured.
func refuseOutsideSchema(w http.ResponseWriter, r *http.Request, room string, cause error) {
	slog.Warn("dispatch: refuse a document socket outside Proof schema; a replacement from markdown repairs it", "room", room, "error", cause)
	var upgrader gws.Upgrader
	connection, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // the upgrader has answered the request
	}
	defer connection.Close()
	deadline := time.Now().Add(time.Second)
	if err := connection.WriteControl(gws.CloseMessage, gws.FormatCloseMessage(documentSchemaCloseCode, "DOC_SCHEMA"), deadline); err != nil {
		return
	}
	// The client's close in reply completes the handshake; whatever it sent before that is
	// discarded unread.
	_ = connection.SetReadDeadline(deadline)
	for {
		if _, _, err := connection.NextReader(); err != nil {
			return
		}
	}
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
	doc, err := s.roomDocument(r.Context(), room)
	if err != nil {
		http.Error(w, ErrServiceUnavailable.Error(), http.StatusServiceUnavailable)
		return
	}
	// A browser editor normalizes a tree it cannot represent and writes the result back, so no
	// connection - a first one, or a provider's reconnect - joins a room outside the Proof schema
	// until it is replaced from markdown. The server decides it here, for every client at once.
	if doc != nil {
		if _, err := renderDocument(doc); outsideSchema(err) {
			refuseOutsideSchema(w, r, room, err)
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
	if token, present := auth.BearerToken(r); present {
		if !auth.MatchesSharedAgentToken(token, s.agentToken) {
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

// refuseIfRebuilding refuses a room's load or injection while a rebuild's transaction holds it.
func (s *Service) refuseIfRebuilding(room string) error {
	if _, rebuilding := s.rebuilding.Load(room); rebuilding {
		return fmt.Errorf("%w: the document is being rebuilt; retry", ErrServiceUnavailable)
	}
	return nil
}

// allowInject decides whether ygo may apply an injection to a room. Its issue read goes
// through the shared pool for a caller that need hold no connection of its own (the
// settlement warm-up in settleRoom), and that is outside the pool's deadlock cycle only
// because ygo runs OnInject before getOrCreateRoom (reearth/ygo v1.49.5,
// provider/websocket/inject.go:311-320): an injection refused here has published no room
// placeholder for a connection-holder to park on, so nothing holding a connection is waiting
// on this read. A vendored reordering of those two calls puts it back in the cycle.
func (s *Service) allowInject(ctx context.Context, info websocket.InjectInfo) error {
	if err := s.refuseIfRebuilding(info.Room); err != nil {
		return err
	}
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
	// A load never waits for its own room's recovery. That recovery's eviction waits in ygo's
	// CloseRoom for the ready barrier this load holds (reearth/ygo v1.49.5,
	// provider/websocket/inject.go:501-508) and closes the channel awaitRoomRecovery waits on
	// only once CloseRoom has returned (failRoomLocked), so a wait here is a cycle: the load
	// holds the eviction, and the eviction holds the load. No deadline breaks it either -
	// both loaders that reach this one carry context.Background(), the settlement warm-up
	// (settleRoom) and a committed write's publish (publishLiveUpdate) - so the room would stay
	// failed until the process restarted (LEGION-282). A failed room refuses the load
	// instead: ygo fails the load, closes the barrier with this error and removes the room,
	// which lets the eviction finish, and the next access loads the replacement.
	if err := s.refuseIfRebuilding(room); err != nil {
		return err
	}
	if err := s.roomFailure(room); err != nil {
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
	markdown, err := renderDocument(doc)
	var contentMarkdown *string
	switch {
	case outsideSchema(err):
		slog.Warn("dispatch: loaded document outside Proof schema; a replacement from markdown repairs it", "room", room, "error", err)
	case err != nil:
		return err
	default:
		contentMarkdown = &markdown
	}
	state := s.room(room)
	state.mu.Lock()
	state.closed = !open
	state.contentMarkdown = contentMarkdown
	// A failure dropped this document's settlement (failRoomLocked). This state is the
	// replacement it left the mark for, so it settles once rather than waiting for an edit to
	// arm one. A room that failed again while this load ran leaves the mark for its own
	// replacement, since failing a room always sets it.
	if state.failed == nil {
		if _, dropped := s.settleAfterReload.LoadAndDelete(room); dropped {
			s.scheduleSettleLocked(room, state)
		}
	}
	state.mu.Unlock()
	doc.OnUpdate(func(update []byte, origin any) {
		if _, identityRepair := origin.(*identityClosureOrigin); identityRepair {
			return
		}
		contentChanged := s.updateChangesMarkdown(room, doc)
		s.recordUpdateClass(room, update, contentChanged, true)
		if contentChanged {
			s.creditContentChange(room, origin)
		}
		s.scheduleSettle(room)
	})
	return nil
}

// updateChangesMarkdown reports whether the room's latest update changed its rendered markdown,
// the only document content a version stores. An update that changes only what no rendering
// carries - an anchor mark, or a heading id or list item label the browser editor derives - is no
// content change.
func (s *Service) updateChangesMarkdown(room string, doc *crdt.Doc) bool {
	markdown, err := renderDocument(doc)
	if err != nil {
		state := s.room(room)
		state.mu.Lock()
		state.contentMarkdown = nil
		state.mu.Unlock()
		if outsideSchema(err) {
			slog.Warn("dispatch: updated document outside Proof schema", "room", room, "error", err)
		} else {
			slog.Error("dispatch: read updated document", "room", room, "error", err)
		}
		return true
	}
	state := s.room(room)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.contentMarkdown != nil && *state.contentMarkdown == markdown {
		return false
	}
	state.contentMarkdown = &markdown
	return true
}

// creditContentChange credits an observed content change to its authors. A service mutation
// (origin registered by serviceTransact) is its actor's alone, who joins `pending` and
// becomes `lastActor`; a browser that was only connected while it happened is not credited. A
// committed transaction's live write, which Ledger.Commit applies, was credited when the
// transaction committed and is not credited again. Any other update is a browser edit by one of
// the peers, which ygo applies while that peer's connection is registered. ygo does not say which
// connection sent it, so every connected peer joins `pending`: when exactly one is connected it is
// the latest edit source and replaces `lastActor`, and otherwise the edit cannot be pinned on a
// single peer and no older actor may stand in for it.
func (s *Service) creditContentChange(room string, origin any) {
	if _, published := origin.(*liveWriteOrigin); published {
		return
	}
	value, service := s.serviceOrigins.Load(origin)
	state := s.room(room)
	state.mu.Lock()
	defer state.mu.Unlock()
	if service {
		if actor, credited := value.(*model.Actor); credited && actor != nil {
			state.pending[actorKey(*actor)] = *actor
			state.lastActor = new(*actor)
		}
		return
	}
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
	if ambiguous {
		sole = nil
	}
	state.lastActor = sole
}

// addConnection registers a browser connected to room. It is credited only with browser edits
// observed while it is connected (creditContentChange), never for connecting or for an agent's
// edit.
func (s *Service) addConnection(room string, id uint64, actor model.Actor) {
	state := s.room(room)
	state.mu.Lock()
	state.connected[id] = actor
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
