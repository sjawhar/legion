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

	"github.com/sjawhar/envoy/internal/contracts"
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
	if update, ok := a.service.takePreload(room); ok {
		return update, nil
	}
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
	// ygo's persistence worker calls this, at its exit among other times. roomServer guards a
	// repair but not a published live write (it is already durable), so a worker can still exit
	// under that Apply. Any nonfailed compaction leaves a busy document lock to the next pass;
	// a failed room's eviction compacts under the lock so recovery remains fail-fast (compactIfIdle).
	if !a.service.roomFailed(room) {
		ctx = compactIfIdle(ctx)
	}
	_, err := a.store.Compact(ctx, room, 500)
	return err
}

// validateUpdate decodes a stored history, or an update about to replace one, as the room will
// decode it: into a document with the server's pending queue (newDocumentCopy), so a history a room
// served is never judged one that cannot load.
func validateUpdate(update []byte) error {
	if len(update) == 0 {
		return nil
	}
	return crdt.ApplyUpdateV1(newDocumentCopy(), update, nil)
}

// preloadedDocument is the durable state a document socket's admission check decoded, kept for the
// load ygo makes of the room next (servicePersistenceAdapter.LoadDoc), so a cold connection reads
// and decodes the history once.
type preloadedDocument struct {
	loaded persistence.LoadResult
}

// takePreload is the update a socket's admission check loaded for room, while the durable head is
// still the one it loaded: every write that changes a document's stored state raises its head (an
// append, a rebuild), and compaction keeps both the head and the state. A stale or absent preload
// is no answer, and the caller loads the room itself.
func (s *Service) takePreload(room string) ([]byte, bool) {
	value, ok := s.preloads.LoadAndDelete(room)
	if !ok {
		return nil, false
	}
	preload := value.(*preloadedDocument)
	head, err := s.persistence.Head(context.Background(), room)
	if err != nil || head != preload.loaded.Version {
		return nil, false
	}
	return preload.loaded.Update, true
}

// documentSchemaCloseCode closes a document websocket whose room is outside the Proof schema
// (DOCUMENT_SCHEMA_CLOSE_CODE in packages/contracts), which the dashboard reads as the document's
// repair state rather than as a dropped connection to retry.
const documentSchemaCloseCode = contracts.DocumentSchemaCloseCode

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
	if err := connection.WriteControl(gws.CloseMessage, gws.FormatCloseMessage(documentSchemaCloseCode, contracts.DocumentSchemaCloseReason), deadline); err != nil {
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
	// A browser editor normalizes a tree it cannot represent and writes the result back, so no
	// connection - a first one, or a provider's reconnect - joins a room outside the Proof schema
	// until it is replaced from markdown. The server decides it here, for every client at once, by
	// the read and the rendering `/text` answers with (readTree), so a socket is refused exactly
	// when that read is ErrDocOutsideSchema.
	tree, loaded, err := s.loadTree(r.Context(), room)
	if err == nil && tree != nil {
		if _, renderErr := documentMarkdown(tree); errors.Is(renderErr, ErrDocOutsideSchema) {
			err = renderErr
		}
	}
	if errors.Is(err, ErrDocOutsideSchema) {
		refuseOutsideSchema(w, r, room, err)
		return
	}
	if err != nil {
		http.Error(w, ErrServiceUnavailable.Error(), http.StatusServiceUnavailable)
		return
	}
	if loaded != nil {
		preload := &preloadedDocument{loaded: *loaded}
		s.preloads.Store(room, preload)
		defer s.preloads.CompareAndDelete(room, preload)
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

// ActorHeader is the header a document websocket's bearer names its session in, as the JSON of a
// session actor.
const ActorHeader = "X-Dispatch-Actor"

func (s *Service) requestActor(r *http.Request) (model.Actor, error) {
	if token, present := auth.BearerToken(r); present {
		if !auth.MatchesSharedAgentToken(token, s.agentToken) {
			return model.Actor{}, errors.New("invalid document bearer token")
		}
		var supplied model.Actor
		if err := json.Unmarshal([]byte(r.Header.Get(ActorHeader)), &supplied); err != nil {
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
	owed, err := settlementPending(ctx, rooms, room)
	if err != nil {
		return err
	}
	// The room is still loading: ygo hands its document to no peer or caller until this hook
	// returns (sjawhar/ygo v1.50.1-sami.2, provider/websocket/server.go:1753-1842: loadRoom closes
	// the room's ready barrier after it), so nothing writes the tree while this walks it.
	markdown, err := renderDocument(doc)
	var contentMarkdown *string
	switch {
	case errors.Is(err, ErrDocOutsideSchema):
		slog.Warn("dispatch: loaded document outside Proof schema; a replacement from markdown repairs it", "room", room, "error", err)
	case err != nil:
		return err
	default:
		contentMarkdown = &markdown
	}
	state := s.room(room)
	state.mu.Lock()
	state.closed = !open
	// The document owes a settlement no settlement committed: one a shutdown's budget cut short,
	// or one a room failure dropped (failRoomLocked). Its timer lived in the process or the room
	// that is gone, so this load settles once rather than waiting for an edit to arm one - unless
	// the load is a settlement's own warm-up, which settles it next. A room that failed again
	// while this load ran leaves the row for its own replacement.
	if state.failed == nil && owed && !state.settleWarming {
		s.scheduleSettleLocked(room, state)
	}
	state.mu.Unlock()
	replica := s.keepReplica(doc, contentMarkdown)
	doc.OnUpdate(func(update []byte, origin any) {
		// A published write's update is already durable. Its suppression slot is finished here,
		// before ygo's persistence observer, which the room registers after OnLoadDocument, hands
		// the update on: to the room's worker, or, once CloseRoom has retired the worker, to
		// stranded persistence on this goroutine, which waits on that slot (publishLiveUpdate).
		if published, ok := origin.(*liveWriteOrigin); ok {
			s.finishSuppressedPersistence(published.slot, update)
		}
		if repair, identityRepair := origin.(*identityClosureOrigin); identityRepair {
			s.recordSuppressedCommit(repair.slot, update)
			return
		}
		contentChanged := replica.observe(room, doc)
		s.recordUpdateClass(room, update, contentChanged, true)
		if contentChanged {
			s.creditContentChange(room, origin)
		}
		s.scheduleSettle(room)
	})
	return nil
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
