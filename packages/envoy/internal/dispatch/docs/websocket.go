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
	"sync"

	"github.com/google/uuid"
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
	// returns (Server.loadRoom closes the room's ready barrier after it, provider/websocket/
	// server.go:1723-1804 in the pinned fork), so nothing writes the tree while this walks it.
	tree, err := treeOf(doc)
	if err != nil {
		slog.Error("dispatch: loaded document outside Proof schema", "room", room, "error", err)
		return err
	}
	markdown, err := renderTree(tree)
	if err != nil {
		slog.Error("dispatch: render loaded document", "room", room, "error", err)
		return err
	}
	state := s.room(room)
	state.mu.Lock()
	state.closed = !open
	state.contentMarkdown = &markdown
	// The document owes a settlement no settlement committed: one a shutdown's budget cut short,
	// or one a room failure dropped (failRoomLocked). Its timer lived in the process or the room
	// that is gone, so this load settles once rather than waiting for an edit to arm one - unless
	// the load is a settlement's own warm-up, which settles it next. A room that failed again
	// while this load ran leaves the row for its own replacement.
	if state.failed == nil && owed && !state.settleWarming {
		s.scheduleSettleLocked(room, state)
	}
	state.mu.Unlock()
	replica := &renderedReplica{}
	doc.OnUpdate(func(update []byte, origin any) {
		if _, identityRepair := origin.(*identityClosureOrigin); identityRepair {
			return
		}
		replica.mu.Lock()
		replica.catchUp(room, doc)
		contentChanged := s.updateChangesMarkdown(room, replica.doc)
		replica.mu.Unlock()
		s.recordUpdateClass(room, update, contentChanged, true)
		if contentChanged {
			s.creditContentChange(room, origin)
		}
		s.scheduleSettle(room)
	})
	return nil
}

// renderedReplica is the copy of a room's document its update observer renders. ygo fires the
// observer after the transaction has released the document's lock (crdt/doc.go:638-643 in the
// pinned fork), and a walk of the live tree takes no lock (YXmlFragment.Children, crdt/yxml.go:301),
// so a render of the live tree there can walk it while another transaction writes it. Only the
// observer, holding mu, writes or renders the replica, and it brings the replica up to date under
// the live document's lock, so each render is of the room as of one moment.
//
// The update the observer is handed cannot stand in for that: observers of two transactions run
// concurrently and in either order, and each update carries the room's whole delete set, so the
// later update applied first deletes what the earlier one replaced while its own insertions wait
// for the earlier one's - a tree no transaction left. Copying the whole room for every update would
// encode and decode the whole document per keystroke. The room's first update copies it once, so a
// room that is only read holds no replica.
type renderedReplica struct {
	mu  sync.Mutex
	doc *crdt.Doc
}

// catchUp brings the replica up to date with live: what live gained since the replica's state
// vector, encoded under live's lock, as forkLive brings a transaction's fork up to date. Without a
// replica - the room's first update, or one after an update the replica could not take, which
// leaves it in an unknown state - it copies live whole; a copy that fails leaves no replica, which
// the next update copies again.
func (r *renderedReplica) catchUp(room string, live *crdt.Doc) {
	if r.doc != nil {
		err := crdt.ApplyUpdateV1(r.doc, crdt.EncodeStateAsUpdateV1(live, r.doc.StateVector()), nil)
		if err == nil {
			return
		}
		slog.Error("dispatch: bring the document's rendered copy up to date; copying it again", "room", room, "error", err)
	}
	copied, err := snapshotDocument(live)
	if err != nil {
		slog.Error("dispatch: copy updated document for its update observer", "room", room, "error", err)
	}
	r.doc = copied
}

// updateChangesMarkdown reports whether the room's latest update changed its rendered markdown,
// the only document content a version stores. It renders replica, the room's document as of that
// update (renderedReplica). An update that changes only what no rendering carries - an anchor
// mark, or a heading id or list item label the browser editor derives - is no content change.
func (s *Service) updateChangesMarkdown(room string, replica *crdt.Doc) bool {
	tree, err := treeOf(replica)
	if err != nil {
		slog.Error("dispatch: read updated document", "room", room, "error", err)
		return true
	}
	markdown, err := renderTree(tree)
	if err != nil {
		slog.Error("dispatch: render updated document", "room", room, "error", err)
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
