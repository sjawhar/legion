package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"time"

	"github.com/google/uuid"
	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/persistence"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// preloadedDocument is the durable state a document socket's admission check decoded, kept for the
// load ygo makes of the room next (servicePersistenceAdapter.LoadDoc), so a cold connection reads
// and decodes the history once, and the tree the check read from it with the rendering it made of
// that tree (replicaRendering: the markdown and its pmdoc.Layout), which the room's load takes in
// place of reading and rendering the room again (onLoadDocument) and hands its replica as the
// rendering of the state the replica starts from (keepReplica), so a cold connection renders the
// document once too. The map holds the first socket's offer of a room
// (offerPreload): a socket whose offer loses keeps no reference to its own, and the entry holds
// its state only until a load takes it (takePreload), so neither outlasts the room's load by the
// socket's whole lifetime.
type preloadedDocument struct {
	loaded    persistence.LoadResult
	tree      *pmdoc.Node
	rendering *replicaRendering
}

// takePreload is the update a socket's admission check loaded for room, with the tree it read from
// it and the rendering it made of that tree (nil for an empty history), while the durable head is
// still the one it loaded: every write that changes a document's stored state raises its head (an
// append, a rebuild), and compaction keeps both the head and the state. The head is the head
// tracker's when it trusts itself (headTracker.head), which costs no read, and otherwise read from
// the store. A stale or absent preload is no answer, and the caller loads the room itself. Either
// way the entry gives up the state it held: the socket that offered it holds the entry until it
// ends (offerPreload). The preload's hold on room's head-tracker entry ends once the head is read.
func (s *Service) takePreload(room string) (persistence.LoadResult, *pmdoc.Node, *replicaRendering, bool) {
	value, ok := s.preloads.LoadAndDelete(room)
	if !ok {
		return persistence.LoadResult{}, nil, nil, false
	}
	defer s.heads.release(room)
	preload := value.(*preloadedDocument)
	loaded, tree, rendering := preload.loaded, preload.tree, preload.rendering
	preload.loaded, preload.tree, preload.rendering = persistence.LoadResult{}, nil, nil
	head, trusted := s.heads.head(room)
	if !trusted {
		stored, err := s.persistence.Head(context.Background(), room)
		if err != nil {
			return persistence.LoadResult{}, nil, nil, false
		}
		head = int64(stored)
	}
	if head != int64(loaded.Version) {
		return persistence.LoadResult{}, nil, nil, false
	}
	return loaded, tree, rendering, true
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
	s.srv.ServeHTTP(w, r.WithContext(ctx))
}

// admitDocument decides whether a socket may join room's document. A browser editor normalizes a
// tree it cannot represent and writes the result back, so no connection - a first one, or a
// provider's reconnect - joins a room outside the Proof schema until it is replaced from markdown.
// An ask holding blocks off its content rule is inside it: the editor keeps such an ask rather
// than normalizing it away, and settlement marks it `invalid`. The server decides it for every
// client at once, by the same document and rendering `/text` answers with (readDocument), so a
// socket is refused exactly when that read is ErrDocOutsideSchema. A resident room's update
// observer has rendered the room after every update it took, as that read renders it, and recorded
// the verdict (residentVerdict), which is taken as it stands; only a room with no verdict - not
// resident, or with a replica that lost the room's state - is read and rendered here. A cold
// socket offers the state it read, and what it rendered of it, to the room's load (offerPreload),
// and withdraw is what the socket's end runs to take that offer back.
func (s *Service) admitDocument(ctx context.Context, room string) (withdraw func(), err error) {
	switch s.residentVerdict(room) {
	case verdictInsideSchema:
		return func() {}, nil
	case verdictOutsideSchema:
		return nil, ErrDocOutsideSchema
	}
	tree, loaded, err := s.loadTree(ctx, room)
	var rendering *replicaRendering
	if err == nil && tree != nil {
		if s.admissionRendered != nil {
			s.admissionRendered(room)
		}
		var renderErr error
		if rendering, renderErr = replicaRenderingOf(tree); errors.Is(renderErr, ErrDocOutsideSchema) {
			err = renderErr
		}
	}
	if err != nil {
		return nil, err
	}
	if loaded == nil {
		return func() {}, nil
	}
	return s.offerPreload(room, loaded, tree, rendering), nil
}

// offerPreload offers loaded, the durable state a cold socket's admission read for room, tree, the
// tree it read from it, and rendering, what it rendered of that tree (nil when the render failed
// for a reason other than the schema, which the room's load then renders again), to the room's
// load (takePreload). The first offer stands
// (LoadOrStore): every offer is of a state the load checks against the head before it takes it,
// and a socket whose offer lost keeps no reference to it, so its whole history is released at
// once rather than when the socket ends, which can be hours later. It returns what the socket's
// end runs: withdraw its own offer if no load took it. An offer that stands holds room's
// head-tracker entry (headTracker.hold) from before it is stored until a load takes it or the
// socket withdraws it, so the head the load checks it against is one the tracker heard of while
// the offer stood, or else one read from the store.
func (s *Service) offerPreload(room string, loaded *persistence.LoadResult, tree *pmdoc.Node, rendering *replicaRendering) (withdraw func()) {
	preload := &preloadedDocument{loaded: *loaded, tree: tree, rendering: rendering}
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

// requestActor is the person a document connection is: the identity service's answer, and nothing
// else. The socket once took the shared agent token too, with the session named in a header the
// caller wrote; that was a second, weaker door into Dispatch, and only the browser - a same-origin
// cookie - ever connected through it (INFRA-264).
func (s *Service) requestActor(r *http.Request) (model.Actor, error) {
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
