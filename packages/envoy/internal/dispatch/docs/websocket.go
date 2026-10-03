package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"runtime"
	"strings"
	"sync"
	"time"
	"weak"

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
	// ygo's persistence worker calls this, at its exit among other times. A failed room's eviction
	// compacts under the room's lock, which every transaction meeting the failed room fails fast
	// instead of waiting for; any other compaction leaves a room whose lock another holder has,
	// which can be a settlement waiting for the worker's exit (compactIfIdle).
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
	owed, err := settlementPending(ctx, rooms, room)
	if err != nil {
		return err
	}
	// The room is still loading: ygo hands its document to no peer or caller until this hook
	// returns (reearth/ygo v1.49.5, provider/websocket/server.go:1716-1804: loadRoom closes the
	// room's ready barrier after it), so nothing writes the tree while this walks it.
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
	observed := &renderedReplica{markdown: contentMarkdown}
	doc.OnUpdate(func(update []byte, origin any) {
		// A published write's update is already durable. Its suppression slot is finished here,
		// before ygo's persistence observer, which the room registers after OnLoadDocument, hands
		// the update on: to the room's worker, or, once CloseRoom has retired the worker, to
		// stranded persistence on this goroutine, which waits on that slot (publishLiveUpdate).
		if published, ok := origin.(*liveWriteOrigin); ok {
			s.finishSuppressedPersistence(published.slot, update)
		}
		if repair, identityRepair := origin.(*identityClosureOrigin); identityRepair {
			s.recordSuppressedCommit(repair.slot, doc, update)
			return
		}
		observed.mu.Lock()
		observed.catchUp(room, doc)
		contentChanged := observed.updateChangesMarkdown(room)
		observed.mu.Unlock()
		s.recordUpdateClass(room, update, contentChanged, true)
		if contentChanged {
			s.creditContentChange(room, origin)
		}
		s.scheduleSettle(room)
	})
	return nil
}

// replica is a copy of a room's document, brought up to date with the room under the live
// document's lock before each walk (catchUp). ygo fires a room's update observers after the
// transaction has released the document's lock (reearth/ygo v1.49.5, crdt/doc.go:638-642), and a
// walk of the live tree takes no lock (crdt/yxml.go:195-211), so a render or read of the live tree
// can walk it while another transaction writes it, and a torn walk reads a healthy document as one
// outside the schema. Only a holder of mu writes or walks a replica, so each walk is of the room as
// of one moment.
type replica struct {
	mu  sync.Mutex
	doc *crdt.Doc
}

// renderedReplica is the replica a room's update observer renders after each update, so a false
// WARN or a content change no update made never comes of a torn render. The update the observer
// is handed cannot stand in for it: observers of two transactions run concurrently and in either
// order, and each update carries the room's whole delete set, so the later update applied first
// deletes what the earlier one replaced while its own insertions wait for the earlier one's - a
// tree no transaction left. Copying the whole room for every update would encode and decode the
// whole document per keystroke. The room's first update copies it once, so a room that is only read
// holds no rendered replica. No read walks it: the observer takes it for every peer update before
// ygo broadcasts the update to the room's other browsers (provider/websocket/peer.go), so a read
// that held it for a walk would hold up those browsers' copy of the keystroke (readLive).
type renderedReplica struct {
	replica
	// markdown is the room's rendered markdown when the update observer last saw it change, or at
	// the room's load before any update has; nil while the document is outside the schema.
	markdown *string
}

// readLive runs read against live, a room's resident document, as of one moment: the replica
// live's reads walk (readReplica), brought up to date under live's document lock, else, while
// another read holds that replica, a copy taken under the lock (snapshotDocument). The reads'
// replica is not the update observer's (renderedReplica), which the observer takes for every peer
// update before the update reaches the room's other browsers, so no read holds up a keystroke for
// its walk; and a read never waits for another, since reads queued behind one walk would wait for
// every walk ahead of them. It opens no transaction on live, since ygo hands the room's persistence
// an update for every transaction it commits, even one that only reads. The caller must not hold
// live's document lock - be inside a Yjs transaction on it - since the catch-up and the copy
// encode under that lock, and read must not keep doc past its return.
func (s *Service) readLive(room string, live *crdt.Doc, read func(doc *crdt.Doc)) error {
	if live == nil {
		return errDocUnloaded
	}
	if s.readReplica(live).read(room, live, read) {
		return nil
	}
	copied, err := snapshotDocument(live)
	if err != nil {
		return err
	}
	read(copied)
	return nil
}

// readReplica is the replica live's reads walk, made on its first read, which copies live whole
// (catchUp). It is listed under a weak pointer to live (Service.replicas), and the listing goes once
// live is collected: a reader holding an evicted instance of a room reaches that instance's replica,
// never its successor's, and an evicted room's replica, a whole copy of its document, goes with it.
func (s *Service) readReplica(live *crdt.Doc) *replica {
	key := weak.Make(live)
	if listed, ok := s.replicas.Load(key); ok {
		return listed.(*replica)
	}
	listed, loaded := s.replicas.LoadOrStore(key, new(replica))
	if !loaded {
		runtime.AddCleanup(live, func(key weak.Pointer[crdt.Doc]) { s.replicas.Delete(key) }, key)
	}
	return listed.(*replica)
}

// liveTree is the tree of live, the document resident under room, as of one moment (readLive).
func (s *Service) liveTree(room string, live *crdt.Doc) (*pmdoc.Node, error) {
	var tree *pmdoc.Node
	var treeErr error
	if err := s.readLive(room, live, func(doc *crdt.Doc) { tree, treeErr = treeOf(doc) }); err != nil {
		return nil, err
	}
	return tree, treeErr
}

// read runs read against the replica brought up to date with live, holding mu, and reports
// whether it did: a replica another read holds, or one whose copy failed, has nothing to read.
func (r *replica) read(room string, live *crdt.Doc, read func(doc *crdt.Doc)) bool {
	if !r.mu.TryLock() {
		return false
	}
	defer r.mu.Unlock()
	r.catchUp(room, live)
	if r.doc == nil {
		return false
	}
	read(r.doc)
	return true
}

// catchUp brings the replica up to date with live: what live gained since the replica's state
// vector, encoded under live's lock, as forkLive brings a transaction's fork up to date. Without a
// copy - the first catch-up, or one after an update the replica could not take, which leaves it in
// an unknown state - it copies live whole; a copy that fails leaves none, which the next catch-up
// copies again.
func (r *replica) catchUp(room string, live *crdt.Doc) {
	if r.doc != nil {
		err := crdt.ApplyUpdateV1(r.doc, crdt.EncodeStateAsUpdateV1(live, r.doc.StateVector()), nil)
		if err == nil {
			return
		}
		slog.Error("dispatch: bring the document's replica up to date; copying it again", "room", room, "error", err)
	}
	copied, err := snapshotDocument(live)
	if err != nil {
		slog.Error("dispatch: copy the document for its replica", "room", room, "error", err)
	}
	r.doc = copied
}

// updateChangesMarkdown reports whether the room's latest update changed its rendered markdown,
// the only document content a version stores, and keeps the new rendering. It renders the
// replica, the room's document as of that update, so its caller holds mu. An update that changes
// only what no rendering carries - an anchor mark, or a heading id or list item label the browser
// editor derives - is no content change.
func (r *renderedReplica) updateChangesMarkdown(room string) bool {
	markdown, err := renderDocument(r.doc)
	if err != nil {
		r.markdown = nil
		if errors.Is(err, ErrDocOutsideSchema) {
			slog.Warn("dispatch: updated document outside Proof schema", "room", room, "error", err)
		} else {
			slog.Error("dispatch: read updated document", "room", room, "error", err)
		}
		return true
	}
	if r.markdown != nil && *r.markdown == markdown {
		return false
	}
	r.markdown = &markdown
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
