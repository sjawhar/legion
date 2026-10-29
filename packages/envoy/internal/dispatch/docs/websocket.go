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
	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// connectionState is one document socket as its admission (ServeHTTP) found it and ygo's
// authorization of the same request (authorize) registered it: the caller, resolved once
// (requestActor), and the admission's one read of the document's issue, which authorize takes
// rather than resolving or reading either again; then the room, the connection id the service gave
// the browser, and whether authorize registered it, which tells the socket's end whether its place
// under the connection cap is the connection's count to forget (removeConnection) or a reservation
// to give back (releaseConnection). What else a socket's admission reads off its request goes here
// too.
type connectionState struct {
	room  string
	id    uint64
	actor model.Actor
	issue issueRead
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

// LoadDoc is the durable state a room loads. It records the load before it reads, and the head it
// read after: the room's load (onLoadDocument) takes the record next on the same goroutine (ygo's
// loadRoom) to start the instance's roomHold and its cursor, and a version this process appends
// between the two reaches that instance through the record (roomLoad) rather than no instance at
// all. The read itself takes no advisory lock - a durable writer holds that one across its whole
// transaction, and a load that waited for it would wedge a write that forks the room it loads -
// so the record, not the lock, is what keeps a room from being born behind the head. The record
// also carries what the load hands ygo (roomLoad.loaded): the history's size, and the tree a
// socket's admission rendered from it when the load takes that socket's preload.
func (a *servicePersistenceAdapter) LoadDoc(room string) ([]byte, error) {
	load := a.service.beginRoomLoad(room)
	if loaded, tree, ok := a.service.takePreload(room); ok {
		load.readHead(loaded.Version)
		load.loaded(loaded.Update, tree)
		return loaded.Update, nil
	}
	result, err := a.store.Load(context.Background(), room)
	if err == nil {
		err = validateUpdate(result.Update)
	}
	if err != nil {
		a.service.failRoom(room, err)
		return nil, err
	}
	load.readHead(result.Version)
	load.loaded(result.Update, nil)
	return result.Update, nil
}

func (a *servicePersistenceAdapter) StoreUpdate(room string, update []byte) error {
	return a.StoreUpdateContext(context.Background(), room, update)
}

// StoreUpdateContext stores one of a room's updates, which ygo's persistence worker hands on one
// call per update, in order, with the in-flight credit its update observer recorded and that
// observer's record of which room instance the update was made in (documentUpdateClass.cursor):
// every append is gated on that instance standing at the durable head, and advances it as it
// commits (AppendUpdatesWithCredit). It returns nil only once the update is durable, so ygo's flush
// still means every update it held is stored.
//
// How is room_appends.go's. An update another append of the room already stored is only consumed
// (planRoomAppend consumes it). One the room's queued updates can go with lingers for them, at most
// roomAppendLinger from its recording, and then goes in one transaction with the updates the room
// recorded after it (appendBatch). Any other goes alone once its suppression slot check has passed
// (appendAlone). Either way its append stores only what no other append of the
// room stored (storeUpdates). An update that never reaches the store - its room failed, or a live
// write or a settlement already made it durable - lands its credit here instead: the room drops
// it with the update, which the browser resends.
func (a *servicePersistenceAdapter) StoreUpdateContext(ctx context.Context, room string, update []byte) error {
	if a.service.roomFailed(room) {
		a.dropUpdate(room, update)
		return nil
	}
	plan, members, linger, wait := a.service.planRoomAppend(room, update, true)
	if linger != nil {
		timer := time.NewTimer(wait)
		select {
		case <-linger:
		case <-timer.C:
		}
		timer.Stop()
		if a.service.roomFailed(room) {
			a.dropUpdate(room, update)
			return nil
		}
		plan, members, _, _ = a.service.planRoomAppend(room, update, false)
	}
	switch plan {
	case appendStored:
		return nil
	case appendAlone:
		// The class, if any, is marked handed (planRoomAppend) before the slot check, so no
		// batch takes a slot's update once its slot has left.
		if a.service.consumeSuppressedPersistence(room, update) || a.service.roomFailed(room) {
			a.dropUpdate(room, update)
			return nil
		}
	}
	return a.storeUpdates(ctx, room, update, members)
}

func (a *servicePersistenceAdapter) Compact(ctx context.Context, room string) error {
	// ygo's persistence worker calls this, at its exit among other times. roomServer guards a
	// repair but not a published live write (it is already durable), so a worker can still exit
	// under that Apply. Any nonfailed compaction leaves a busy document lock to the next pass;
	// a failed room's eviction compacts under the lock so recovery remains fail-fast (compactIfIdle).
	if !a.service.roomFailed(room) {
		ctx = compactIfIdle(ctx)
	}
	_, err := a.store.Compact(ctx, room, compactKeep)
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
// and decodes the history once, and the tree the check read from it and rendered, which the room's
// load takes in place of reading and rendering the room again (onLoadDocument), so a cold
// connection renders the document once too. The map holds the first socket's offer of a room
// (offerPreload): a socket whose offer loses keeps no reference to its own, and the entry holds
// its state only until a load takes it (takePreload), so neither outlasts the room's load by the
// socket's whole lifetime.
type preloadedDocument struct {
	loaded persistence.LoadResult
	tree   *pmdoc.Node
}

// takePreload is the update a socket's admission check loaded for room, with the tree it read from
// it (nil for an empty history), while the durable head is still the one it loaded: every write
// that changes a document's stored state raises its head (an append, a rebuild), and compaction
// keeps both the head and the state. The head is the head tracker's when it trusts itself
// (headTracker.head), which costs no read, and otherwise read from the store. A stale or absent
// preload is no answer, and the caller loads the room itself. Either way the entry gives up the
// state it held: the socket that offered it holds the entry until it ends (offerPreload). The
// preload's hold on room's head-tracker entry ends once the head is read.
func (s *Service) takePreload(room string) (persistence.LoadResult, *pmdoc.Node, bool) {
	value, ok := s.preloads.LoadAndDelete(room)
	if !ok {
		return persistence.LoadResult{}, nil, false
	}
	defer s.heads.release(room)
	preload := value.(*preloadedDocument)
	loaded, tree := preload.loaded, preload.tree
	preload.loaded, preload.tree = persistence.LoadResult{}, nil
	head, trusted := s.heads.head(room)
	if !trusted {
		stored, err := s.persistence.Head(context.Background(), room)
		if err != nil {
			return persistence.LoadResult{}, nil, false
		}
		head = int64(stored)
	}
	if head != int64(loaded.Version) {
		return persistence.LoadResult{}, nil, false
	}
	return loaded, tree, true
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

// socketRoom is the document a socket request names: its route's room, else its path's last
// segment.
func socketRoom(r *http.Request) string {
	if room := r.PathValue("room"); room != "" {
		return room
	}
	return path.Base(r.URL.Path)
}

// ServeHTTP serves the Hocuspocus-framed document websocket endpoint. A caller that does not
// resolve is refused 401, except a person whose lapsed confirmation the sign-in issuer could not
// renew (identity.ErrSignInUnavailable), who is still signed in and is refused 503
// SIGNIN_UNAVAILABLE as every other route refuses them (identity.WriteError). A socket's admission
// resolves the caller and reads the document's issue once each, here, and hands both to ygo's
// authorization of the same request (authorize) on the connection's state; a warm room's schema
// verdict is the one its update observer recorded (residentVerdict), so admitting a socket to a
// warm room walks no rooms and renders nothing, and a cold one renders its document once
// (admitDocument).
//
// Before it loads anything the socket takes its places under the caps, a connection's
// (reserveConnection) and its room's (reserveRoom), and a socket that cannot take either is
// refused 503 before the upgrade. Each place is taken in the same step that checks the cap, so
// sockets that open at once never pass it, and each is a count, so the check walks no rooms. The
// socket holds both until it ends. The connection's place is the connection's count once
// authorize registers it, and removeConnection, which forgets the browser as the socket closes,
// ends it; a socket that ends unregistered - its admission or its authorization refused, its room's
// load failed or panicked - gives it back. The room's place the socket gives back however it ends,
// and a room ygo loaded keeps a place of its own until it unloads (releaseUnloadedRoom).
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	room := socketRoom(r)
	actor, err := s.requestActor(r)
	if err != nil {
		if errors.Is(err, identity.ErrSignInUnavailable) {
			identity.WriteError(w, err)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if _, err := uuid.Parse(room); err != nil {
		http.Error(w, "document not found", http.StatusNotFound)
		return
	}
	issue, err := s.readIssue(r.Context(), s.queryFrom(r.Context()), room)
	if err != nil {
		http.Error(w, "document not found", http.StatusNotFound)
		return
	}
	if err := s.awaitRoomRecovery(r.Context(), room); err != nil {
		http.Error(w, ErrServiceUnavailable.Error(), http.StatusServiceUnavailable)
		return
	}
	if !s.reserveConnection() {
		http.Error(w, ErrServiceUnavailable.Error(), http.StatusServiceUnavailable)
		return
	}
	if !s.reserveRoom(room) {
		s.releaseConnection()
		http.Error(w, ErrServiceUnavailable.Error(), http.StatusServiceUnavailable)
		return
	}
	connection := &connectionState{actor: actor, issue: issue}
	defer func() {
		s.releaseRoom(room)
		if connection.added {
			s.removeConnection(connection.room, connection.id)
		} else {
			s.releaseConnection()
		}
	}()
	held := s.holdForAdmission(room, issue)
	defer s.releaseAdmission(room, held)
	if s.afterSocketIssueRead != nil {
		s.afterSocketIssueRead(room)
	}
	withdraw, err := s.admitDocument(r.Context(), room)
	if errors.Is(err, ErrDocOutsideSchema) {
		refuseOutsideSchema(w, r, room, err)
		return
	}
	if err != nil {
		http.Error(w, ErrServiceUnavailable.Error(), http.StatusServiceUnavailable)
		return
	}
	defer withdraw()
	ctx := context.WithValue(r.Context(), connectionContextKey{}, connection)
	// The socket's peer hears from it at least every s.keepAlive (documentKeepAlive), however idle
	// the room (keepalive.go), so its provider never takes the socket for dead.
	keepAlive := newKeepAliveWriter(w, room, s.keepAlive)
	s.srv.ServeHTTP(keepAlive, r.WithContext(ctx))
	keepAlive.stop()
}

// admitDocument decides whether a socket may join room's document. A browser editor normalizes a
// tree it cannot represent and writes the result back, so no connection - a first one, or a
// provider's reconnect - joins a room outside the Proof schema until it is replaced from markdown.
// The server decides it for every client at once, by the same document and rendering `/text`
// answers with (readDocument), so a socket is refused exactly when that read is
// ErrDocOutsideSchema. A resident room's update observer has rendered the room after every update
// it took, as that read renders it, and recorded the verdict (residentVerdict), which is taken
// as it stands; only a room with no verdict - not resident, or with a replica that lost the room's
// state - is read and rendered here. A cold socket offers the state it read to the room's load
// (offerPreload), and withdraw is what the socket's end runs to take that offer back.
func (s *Service) admitDocument(ctx context.Context, room string) (withdraw func(), err error) {
	switch s.residentVerdict(room) {
	case verdictInsideSchema:
		return func() {}, nil
	case verdictOutsideSchema:
		return nil, ErrDocOutsideSchema
	}
	tree, loaded, err := s.loadTree(ctx, room)
	if err == nil && tree != nil {
		if s.admissionRendered != nil {
			s.admissionRendered(room)
		}
		if _, renderErr := documentMarkdown(tree); errors.Is(renderErr, ErrDocOutsideSchema) {
			err = renderErr
		}
	}
	if err != nil {
		return nil, err
	}
	if loaded == nil {
		return func() {}, nil
	}
	return s.offerPreload(room, loaded, tree), nil
}

// offerPreload offers loaded, the durable state a cold socket's admission read for room, and tree,
// the tree it read from it and rendered, to the room's load (takePreload). The first offer stands
// (LoadOrStore): every offer is of a state the load checks against the head before it takes it,
// and a socket whose offer lost keeps no reference to it, so its whole history is released at
// once rather than when the socket ends, which can be hours later. It returns what the socket's
// end runs: withdraw its own offer if no load took it. An offer that stands holds room's
// head-tracker entry (headTracker.hold) from before it is stored until a load takes it or the
// socket withdraws it, so the head the load checks it against is one the tracker heard of while
// the offer stood, or else one read from the store.
func (s *Service) offerPreload(room string, loaded *persistence.LoadResult, tree *pmdoc.Node) (withdraw func()) {
	preload := &preloadedDocument{loaded: *loaded, tree: tree}
	s.heads.hold(room)
	_, lost := s.preloads.LoadOrStore(room, preload)
	if s.afterSocketPreload != nil {
		s.afterSocketPreload(room, loaded)
	}
	if lost {
		s.heads.release(room)
		return func() {}
	}
	return func() {
		if s.preloads.CompareAndDelete(room, preload) {
			s.heads.release(room)
		}
	}
}

// authorize is ygo's authorization of a document socket ServeHTTP admitted. It registers the
// browser as the caller ServeHTTP resolved, and takes the issue read ServeHTTP made, so it resolves
// and reads neither again. The connection is counted already: ServeHTTP took its place under the
// connection cap (reserveConnection) before it loaded anything, so registering it counts nothing
// more, and no cap refuses it here, where a refusal would reach the browser as ygo's 401, which
// says it is not signed in. The issue is closed for the socket when that read says so or the
// room's state records it closed: ServeHTTP's hold on the state (holdForAdmission) is where a close
// made while the socket loaded its document is recorded (SetIssueClosed), so such a socket is
// admitted read-only, as one opened after the close is. A closed issue, or a client schema version
// other than the server's, admits the socket read-only; a request no admission read the issue for
// is refused, as is a socket to a failed room.
func (s *Service) authorize(r *http.Request) (websocket.ConnectionConfig, bool) {
	connection, _ := r.Context().Value(connectionContextKey{}).(*connectionState)
	if connection == nil {
		return websocket.ConnectionConfig{}, false
	}
	room := socketRoom(r)
	state := s.lockState(room)
	if state.failed != nil {
		s.unlockState(room, state)
		return websocket.ConnectionConfig{}, false
	}
	state.recordIssueLocked(connection.issue)
	open := connection.issue.open && !state.closed
	connection.room = room
	connection.id = s.nextConnection.Add(1)
	connection.added = true
	s.registerConnectionLocked(state, connection.id, connection.actor)
	s.unlockState(room, state)
	return websocket.ConnectionConfig{
		ReadOnly: schemaReadOnly(open, r.URL.Query().Get("schema_version")),
	}, true
}

func schemaReadOnly(open bool, clientSchemaVersion string) bool {
	return !open || clientSchemaVersion != fmt.Sprintf("%d", pmdoc.SchemaVersion())
}

// authorizeSchemaVersion confirms the existing HTTP-authorized connection's schema admission
// through Hocuspocus's authenticated scope, which is the provider's client-visible signal. ygo
// calls it with the room and the client's token alone. The issue's state is what room's state
// records, which is at least as fresh as the read the connection's own admission made (authorize
// recorded it there); only a room that records none reads the issue.
func (s *Service) authorizeSchemaVersion(room, clientSchemaVersion string) (websocket.ConnectionConfig, error) {
	closed, recorded := s.recordedIssueClosed(room)
	if !recorded {
		ctx := context.Background()
		issue, err := s.readIssue(ctx, s.queryFrom(ctx), room)
		if err != nil {
			return websocket.ConnectionConfig{}, err
		}
		s.recordIssueRead(room, issue)
		closed = !issue.open
	}
	return websocket.ConnectionConfig{ReadOnly: schemaReadOnly(!closed, clientSchemaVersion)}, nil
}

// ActorHeader is the header a document websocket's bearer names its session in, as the JSON of a
// session actor.
const ActorHeader = "X-Dispatch-Actor"

func (s *Service) requestActor(r *http.Request) (model.Actor, error) {
	if token, present := auth.BearerToken(r); present {
		if !auth.MatchesSharedAgentToken(r, token, s.agentTokens) {
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
		accesslog.Note(r.Context(), accesslog.KindSharedToken, "")
		accesslog.Note(r.Context(), accesslog.KindSession, supplied.ID)
		return model.Actor{Kind: "session", ID: supplied.ID, Origin: supplied.Origin}, nil
	}
	if s.identity == nil {
		return model.Actor{}, errors.New("document identity required")
	}
	login, err := s.identity.Login(r)
	if err != nil {
		return model.Actor{}, err
	}
	accesslog.Note(r.Context(), accesslog.KindUser, login)
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
// because ygo runs OnInject before getOrCreateRoom (sjawhar/ygo v1.51.3-sami.1,
// provider/websocket/inject.go:321-330): an injection refused here has published no room
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
	// CloseRoom for the ready barrier this load holds (sjawhar/ygo v1.51.3-sami.1,
	// provider/websocket/inject.go:517-525) and closes the channel awaitRoomRecovery waits on
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
	// The room counts among the resident rooms, and holds its place under the room cap
	// (reserveRoom), from here until ygo's OnUnloadDocument (releaseUnloadedRoom), unless this
	// load fails: ygo then drops the room with no OnUnloadDocument, so the load takes its own count
	// back.
	s.roomLoaded(room)
	resident := false
	defer func() {
		if !resident {
			s.roomUnloaded(room)
		}
	}()
	// Everything this load needs comes from the pool that owns loads. A writer holding a
	// pooled connection and the issue's row lock waits for this load, so a read of the shared
	// pool here waits for a connection only that writer can release: the wedge, arrived at
	// without a transaction of its own and so invisible to the pool's guard.
	rooms, err := s.store.Pool.Rooms()
	if err != nil {
		return err
	}
	// The room's browser updates are stored on the room-appends pool, which opens a connection
	// beside this load when it holds none, so the room's first batch dials none (warmRoomAppends).
	s.warmRoomAppends()
	// The issue is read once per room load. A cold socket's admission, which holds the room's state
	// while its load runs, has read it and recorded it there, as has a close or reopen of the issue
	// since (admittedIssueRead); a load no admission holds - a settlement's warm-up, a committed
	// write's publish - reads it here.
	issue, admitted := s.admittedIssueRead(room)
	if !admitted {
		if issue, err = s.readIssue(ctx, rooms, room); err != nil {
			return err
		}
	}
	// The load takes no advisory lock, which a durable writer can hold for as long as its
	// transaction runs: the row it reads is written whole by each writer's transaction, so a
	// plain read sees one writer's commit or the one before it. What the record has recorded by
	// then is taken first, so an edit source recorded after the read is told from one before it.
	mark := s.markEditSource(room)
	owed, lastActor, err := readOwedSettlement(ctx, rooms, room)
	if err != nil {
		return err
	}
	// The room is still loading: ygo hands its document to no peer or caller until this hook
	// returns (sjawhar/ygo v1.51.3-sami.1, provider/websocket/server.go:1749-1837: loadRoom closes
	// the room's ready barrier after it), so nothing writes the tree while this walks it. A load
	// that took a cold socket's preload takes the tree that socket's admission read from the same
	// state and found inside the schema (roomLoad.takeLoaded), so the document is rendered once.
	var size int
	var tree *pmdoc.Node
	if value, ok := s.roomLoads.Load(room); ok {
		size, tree = value.(*roomLoad).takeLoaded()
	}
	var renderErr error
	if tree == nil {
		tree, renderErr = treeOf(doc)
		if renderErr == nil {
			if s.admissionRendered != nil {
				s.admissionRendered(room)
			}
			_, renderErr = documentMarkdown(tree)
		}
	}
	var askBlocks map[string]struct{}
	switch {
	case errors.Is(renderErr, ErrDocOutsideSchema):
		slog.Warn("dispatch: loaded document outside Proof schema; a replacement from markdown repairs it", "room", room, "error", renderErr)
	case renderErr != nil:
		return renderErr
	default:
		askBlocks = askBlockIDs(tree)
	}
	// The room this load publishes holds the state it takes from here until ygo retires it
	// (releaseIfUnusedLocked). The issue's state is the newest the record holds, this load's read
	// or a close recorded after it began; the history the load decoded counts as resident.
	state := s.lockState(room)
	state.recordIssueLocked(issue)
	open := !state.closed
	state.residentBytes += int64(size)
	s.residentBytes.Add(int64(size))
	// The ask blocks the room loaded with are the baseline its update observer tells new ones by.
	// They were not introduced by any update the observer sees, so none gains an author here.
	if askBlocks == nil {
		state.askBlocks = nil
	} else {
		state.observeAskBlocks(askBlocks, nil)
	}
	// The document owes a settlement no settlement committed: one a shutdown's budget cut short,
	// or one a room failure dropped (failRoomLocked). Its timer lived in the process or the room
	// that is gone, so an open room settles once rather than waiting for an edit to arm one - unless
	// the load is a settlement's own warm-up, which settles it next. Its authors are in the
	// document's pending authors, which that settlement reads; the room restores only the latest
	// edit source the settlement names. A closed room keeps the row; it settles only after its
	// issue reopens. A room that failed again while this load ran leaves the row for its own
	// replacement.
	//
	// The row is that source as the store has it, so it replaces whatever the record held for a
	// room that has gone (a forced close keeps the record, evictRoom): a row naming none - an edit
	// any of several connected browsers could have made - leaves the settlement its own actor
	// rather than the last source an earlier room saw, and no row leaves no source, as on a fresh
	// record. The record keeps its own when the row may be older than it (rowIsCurrent).
	switch {
	case !open || !mark.rowIsCurrent(state):
	case owed && lastActor != nil:
		state.lastActor = lastActor
		state.unsettled = true
	default:
		state.lastActor = nil
		state.unsettled = false
	}
	if state.failed == nil && open && owed && !state.settleWarming {
		s.scheduleSettleLocked(room, state)
	}
	s.unlockState(room, state)
	// The log is opened before the observer is registered and before ygo hands the room to anyone
	// (above), so it holds every transaction the room commits from here on, in commit order. The
	// replica's verdict starts as this load's rendering said, and the observer's renders move it
	// on from the first update (renderedReplica.verdict).
	replica := s.keepReplica(doc, doc.OpenUpdateLog())
	replica.recordVerdict(renderErr)
	instance := s.keepInstance(room, doc)
	hold := &instance.hold
	doc.OnUpdate(func(update []byte, origin any) {
		// A published write's update is already durable. Its suppression slot is finished here,
		// before ygo's persistence observer, which the room registers after OnLoadDocument, hands
		// the update on: to the room's worker, or, once CloseRoom has retired the worker, to
		// stranded persistence on this goroutine, which waits on that slot (publishLiveUpdate).
		if published, ok := origin.(*liveWriteOrigin); ok {
			s.finishSuppressedPersistence(published.slot, update)
		}
		repair, identityRepair := origin.(*identityClosureOrigin)
		if identityRepair {
			s.recordSuppressedCommit(repair.slot, update)
			// A repair's update is not classified or credited - its writer appends it in its own
			// transaction - but it names this instance all the same: one that reaches the room's
			// persistence outside its suppression slot, which another writer's update can take
			// first (consumeSuppressedPersistence), is stored as any other update, and the room
			// it was made in has to take that version in or stand behind the head for good. The
			// class counts for nothing else: it is not durable work the room's state waits on.
			s.recordUpdateClass(room, update, documentUpdateClass{contentChanged: true, cursor: &instance.cursor})
		} else if s.beforeObserveUpdate != nil {
			s.beforeObserveUpdate(room)
		}
		// Every observer drains the room's update log, a repair's too, and each committed update
		// is classified once, by whichever observer reaches it first, against the room as the
		// commit before it left it (renderedReplica). This observer's own update is in the log -
		// ygo appended it under the document's lock at its commit - so its class is recorded
		// before ygo's persistence observer, which runs after this one on this goroutine, hands it
		// on. Another observer's update is recorded while the replica is held: that observer can
		// be waiting for the replica, and runs its persistence observer as soon as it has it.
		var classified, changed bool
		var own *documentUpdateClass
		replica.observe(room, func(logged crdt.LoggedUpdate, contentChanged bool, asks *observedAsks) {
			if _, identityRepair := logged.Origin.(*identityClosureOrigin); identityRepair {
				// Its own observer records its class (above). The replica takes it all the same,
				// so the update after it is measured against the room the repair left.
				return
			}
			classified = true
			if asks != nil {
				s.observeAskBlocksForUpdate(room, asks.askBlocks())
			}
			// The cursor is this instance's, taken here rather than looked up when the update
			// reaches the store: a room evicted and reloaded in between is a different instance,
			// which did not take this update and must not be recorded as holding the version it
			// is stored at.
			class := documentUpdateClass{contentChanged: contentChanged, durable: true, cursor: &instance.cursor}
			if contentChanged {
				class.credit = s.creditContentChange(room, logged.Origin, hold)
				changed = true
			}
			if sameUpdate(logged.Update, update) {
				own = &class
				return
			}
			s.recordUpdateClass(room, logged.Update, class)
		})
		if changed && s.afterCreditUpdate != nil {
			s.afterCreditUpdate(room)
		}
		if own != nil {
			s.recordUpdateClass(room, update, *own)
		}
		if classified {
			s.scheduleSettle(room)
		}
	})
	resident = true
	return nil
}

// sameUpdate reports whether logged, an update the room's update log handed over, is update, the
// one an observer was called with: ygo hands both the very slice it encoded at the commit
// (crdt.LoggedUpdate), so the same backing array is the same commit, where equal bytes need not be
// - two commits that change nothing encode alike.
func sameUpdate(logged, update []byte) bool {
	return len(logged) > 0 && len(logged) == len(update) && &logged[0] == &update[0]
}

// soleConnectedActor is the one actor connected to a room, and whether more than one distinct
// actor is: an update made while exactly one of them is connected can be pinned on that actor; one
// made while several are cannot be pinned on any single one of them.
func soleConnectedActor(connected map[uint64]model.Actor) (sole *model.Actor, ambiguous bool) {
	for _, actor := range connected {
		if sole == nil {
			sole = new(actor)
		} else if actorKey(actor) != actorKey(*sole) {
			ambiguous = true
		}
	}
	if ambiguous {
		sole = nil
	}
	return sole, ambiguous
}

// observeAskBlocksForUpdate records askBlocks, the ask blocks an update left in room's document,
// and attributes each one not already recorded (observeAskBlocks), in the same critical section
// the update observer classifies the update in (renderedReplica.observe): the observer classifies
// each update in the order the replica took them, so the room's record of its ask blocks moves
// forward only. The tree walk that lists those ask blocks (observedAsks.askBlocks) runs only once
// the cheap markdown comparison inside observe already showed a real change, so the no-op path,
// the common one, pays for neither.
func (s *Service) observeAskBlocksForUpdate(room string, askBlocks map[string]struct{}) {
	state := s.lockState(room)
	defer s.unlockState(room, state)
	var author *model.Actor
	if sole, ambiguous := soleConnectedActor(state.connected); ambiguous {
		author = new(SettlementActor)
	} else {
		author = sole
	}
	state.observeAskBlocks(askBlocks, author)
}

// creditContentChange credits an observed content change to its authors. A service repair (origin
// registered by serviceTransact) is credited to no one; a browser that was only connected while it
// happened is not credited either. A committed transaction's live write, which Ledger.Commit
// applies, was credited when the transaction committed and is not credited again. Any other update
// is a browser edit by one of the peers, which ygo applies while that peer's connection is
// registered. ygo does not say which connection sent it, so every connected peer is credited with
// it: when exactly one is connected it is the latest edit source and replaces `lastActor`, and
// otherwise the edit cannot be pinned on a single peer and no older actor may stand in for it.
//
// The credit goes to F, the room's in-flight credits, until the edit's append lands it
// (AppendUpdatesWithCredit): the returned UpdateCredit is how that append reaches it. This takes
// no advisory lock and touches no database: nothing reads F without holding state.mu.
func (s *Service) creditContentChange(room string, origin any, hold *roomHold) *UpdateCredit {
	if _, published := origin.(*liveWriteOrigin); published {
		return nil
	}
	if _, service := s.serviceOrigins.Load(origin); service {
		return nil
	}
	state := s.lockState(room)
	defer s.unlockState(room, state)
	record := &inflightCredit{seq: state.creditSeq.Add(1), authors: make(map[string]model.Actor, len(state.connected))}
	for _, actor := range state.connected {
		record.authors[actorKey(actor)] = actor
	}
	record.lastActor, _ = soleConnectedActor(state.connected)
	state.inflight[record.seq] = record
	state.lastActor = record.lastActor
	state.unsettled = true
	return &UpdateCredit{service: s, room: room, state: state, record: record, hold: hold}
}

// registerConnectionLocked registers a browser connected to the room whose state is state, which
// the caller holds locked. It is credited only with browser edits observed while it is connected
// (creditContentChange), never for connecting or for an agent's edit. It counts nothing: the
// browser's count (connections) is the place under the connection cap reserveConnection took for
// it before, which removeConnection gives back as it forgets the browser. authorize registers each
// document socket's browser through it.
func (s *Service) registerConnectionLocked(state *roomState, id uint64, actor model.Actor) {
	state.connected[id] = actor
}

// removeConnection forgets a browser that left room, and with the last one, once ygo has retired
// the room, the room's state (releaseIfUnusedLocked). A document without state has none to forget.
// The service's count of connections drops only for an id the record held: a failed room's
// browsers are found on the record that replaced it (handOverFailedRoomLocked), so each leaves
// the count once.
func (s *Service) removeConnection(room string, id uint64) {
	state := s.lockExistingState(room)
	if state == nil {
		return
	}
	if _, registered := state.connected[id]; registered {
		delete(state.connected, id)
		s.connections.Add(-1)
	}
	s.unlockState(room, state)
}

// settleLastPeer runs the settlement a room's last browser leaving owes now rather than when its
// timer fires. It takes over the timer's settleWG count, so Shutdown joins it as it would the
// timer's settlement: Shutdown reports which documents settled, and stops ygo, only once it has
// returned. It counts itself as running (settling) from the moment it takes the timer, as the timer
// it replaces counted as armed: a write that opens before the settlement reaches its own count
// then takes the settlement over (openLiveWrite) rather than ending it with nobody to arm it again.
func (s *Service) settleLastPeer(_ context.Context, room string) {
	state := s.lockExistingState(room)
	if state == nil {
		return
	}
	if !s.takeSettleTimer(state.settle) {
		s.unlockState(room, state)
		return
	}
	generation := state.roomGeneration
	state.settling++
	s.unlockState(room, state)
	defer s.settleWG.Done()
	defer func() {
		state.mu.Lock()
		state.settling--
		s.unlockState(room, state)
	}()
	s.settleRoom(room, generation)
}
