package docs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
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

type shutdownLoadContextKey struct{}

// withShutdownLoad marks the one injection allowInject lets through into a room Shutdown has
// begun closing: the empty warm-up with which Shutdown's own settlement loads a document whose
// settlement this process had armed with no room resident (a cold write's, publishLiveWrite), as
// the armed timer would have loaded it had it fired. It loads the room and writes nothing; a write
// that settlement then has to make into the room carries no mark and is refused like any other.
func withShutdownLoad(ctx context.Context) context.Context {
	return context.WithValue(ctx, shutdownLoadContextKey{}, true)
}

func isShutdownLoad(ctx context.Context) bool {
	load, _ := ctx.Value(shutdownLoadContextKey{}).(bool)
	return load
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
// so the record, not the lock, is what keeps a room from being born behind the head.
func (a *servicePersistenceAdapter) LoadDoc(room string) ([]byte, error) {
	load := a.service.beginRoomLoad(room)
	if loaded, ok := a.service.takePreload(room); ok {
		load.readHead(loaded.Version)
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
	return result.Update, nil
}

func (a *servicePersistenceAdapter) StoreUpdate(room string, update []byte) error {
	return a.StoreUpdateContext(context.Background(), room, update)
}

// StoreUpdateContext appends a room's update with the in-flight credit its update observer
// recorded (AppendUpdateWithCredit), and with that observer's record of which room instance the
// update was made in (documentUpdateClass.cursor), when it took the update
// (documentUpdateClass.observed) and where a live write of this process may have stored it
// already (documentUpdateClass.ahead), which the append takes as one argument of its own
// (BrowserAppend): the append is a browser's (appendBrowser), so the gate admits it
// from a room behind the head with its history intact - it lands at the head, and the instance's
// cursor folds it in once catch-up fills the gap (appendUpdate) - and refuses it only when the
// room's stored history was replaced under it, or when the document's issue is closed and the room
// took the update later than closeMargin after the close. A history a compaction folded past the
// room's cursor is no replacement for the update when it still holds every item the update builds
// on (storedHistoryHolds): it is stored, and the room, which cannot catch up to that history, is
// closed and reloaded at the head once it is (closeRoomOnReplacedHistory): this append asks for
// that itself, so it needs no room-sync lane and waits on no later notification. An update the
// room took before the close and whose append reaches the gate after it - queued behind the room's
// earlier appends, waiting on the document's lock behind a settlement or a compaction, or handed
// here by the close of the room itself (setRoomIssueOpen) - is stored, and settled once the issue
// reopens, a compaction's fold of the history meanwhile included: a room closing for its issue
// takes in no catch-up while its close waits for this append (holdOpen), and that close is the
// reload a folded history needs.
// A replaced history fails the room, as any failed append does: the room reloads from the store,
// which is the close-and-reload a replaced history needs (plan D3.3), and the browser resends the
// update to the reloaded room. A refused update on a closed issue is dropped and closes the room
// for its issue (refuseClosedIssueAppend), so its browsers reconnect read-only. Either is decided
// under the document's advisory lock, so it is never this process's own write in flight, and needs
// no room-sync lane to run.
// An update that never reaches the store - its room failed, or a live write, a settlement or a
// catch-up already made it durable - lands its credit here instead: the room drops it with the
// update, which the browser resends. An update a live write of this process stored ahead of its own
// row (storeRoomUpdatesAhead) is found there by its append, which stores nothing and writes no
// credit, since the write's transaction wrote both (PgVersioned.appendUpdate). Whichever way the
// append ends, the update leaves its instance's unstored updates once it has.
func (a *servicePersistenceAdapter) StoreUpdateContext(ctx context.Context, room string, update []byte) error {
	if a.service.roomFailed(room) {
		// The failed room's update is dropped and its browser resends it to the room that
		// replaces this one, but its class is taken all the same, so the work the record counts
		// for it ends here. Left counted, it would be an append no callback can ever finish:
		// the record - or the one registered in its place, which carries the updates the room's
		// persistence never took (handOverFailedRoomLocked) - would wait on it for ever, so its
		// settlement would retry without end, and the record would never be released
		// (unusedLocked).
		class, counted, found := a.service.consumeUpdateClass(room, update)
		if found && class.durable {
			a.service.finishDurableAppend(room, counted)
		}
		class.instance.unlistUnstored(class)
		class.credit.landed()
		return nil
	}
	class, counted, found := a.service.consumeUpdateClass(room, update)
	if found && class.durable {
		defer a.service.finishDurableAppend(room, counted)
	}
	defer class.instance.unlistUnstored(class)
	defer class.credit.landed()
	if a.service.consumeSuppressedPersistence(room, update) || a.service.roomFailed(room) {
		return nil
	}
	a.service.hooks.awaitPersistenceHold(ctx, room)
	browser := &BrowserAppend{cursor: class.cursor, observed: class.observed, ahead: class.ahead}
	if _, err := a.store.AppendUpdateWithCredit(ctx, room, update, class.contentChanged, class.credit, browser); err != nil {
		if errors.Is(err, ErrIssueClosed) {
			a.service.refuseClosedIssueAppend(room)
			return nil
		}
		a.service.failRoom(room, err)
		return err
	}
	if found && class.durable && class.contentChanged {
		a.service.scheduleSettleAfterAppend(room)
	}
	if class.cursor.takeReplaced() {
		a.service.closeRoomOnReplacedHistory(room, class.instance)
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
	} else {
		a.service.hooks.countLockWaitingCompaction(room)
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
// and decodes the history once.
type preloadedDocument struct {
	loaded persistence.LoadResult
}

// takePreload is the update a socket's admission check loaded for room, while the durable head is
// still the one it loaded: every write that changes a document's stored state raises its head (an
// append, a rebuild), and compaction keeps both the head and the state. A stale or absent preload
// is no answer, and the caller loads the room itself.
func (s *Service) takePreload(room string) (persistence.LoadResult, bool) {
	value, ok := s.preloads.LoadAndDelete(room)
	if !ok {
		return persistence.LoadResult{}, false
	}
	preload := value.(*preloadedDocument)
	head, err := s.persistence.Head(context.Background(), room)
	if err != nil || head != preload.loaded.Version {
		return persistence.LoadResult{}, false
	}
	return preload.loaded, true
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

// ProcessHeader names, on every answer to a document socket's request and on its upgrade above
// all, the host name of the process serving it - the pod's name in production - so a check that
// opens sockets through a load balancer can record which process each one reached (plan D10).
const ProcessHeader = "X-Dispatch-Process"

// ServeHTTP serves the Hocuspocus-framed document websocket endpoint. A caller that does not
// resolve is refused 401, except a person whose lapsed confirmation the sign-in issuer could not
// renew (identity.ErrSignInUnavailable), who is still signed in and is refused 503
// SIGNIN_UNAVAILABLE as every other route refuses them (identity.WriteError).
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.hostname != "" {
		w.Header().Set(ProcessHeader, s.hostname)
		w = &processHeaderWriter{ResponseWriter: w, line: []byte(ProcessHeader + ": " + s.hostname + "\r\n")}
	}
	room := r.PathValue("room")
	if room == "" {
		room = path.Base(r.URL.Path)
	}
	if _, err := s.requestActor(r); err != nil {
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
	// until it is replaced from markdown. An ask holding blocks off its content rule is inside it:
	// the editor keeps such an ask rather than normalizing it away, and settlement marks it
	// `invalid`. The server decides it here, for every client at once, by the same stored document
	// and rendering `/text` answers with (readDocument), so a socket is refused exactly when that
	// read is ErrDocOutsideSchema.
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
	// The socket's peer hears from it at least every s.keepAlive (documentKeepAlive), however idle
	// the room (keepalive.go), so its provider never takes the socket for dead; the awareness
	// clients the peer may announce are bounded before ygo reads its frames (awareness_limit.go).
	keepAlive := newKeepAliveWriter(w, room, s.keepAlive)
	s.srv.ServeHTTP(&awarenessLimitWriter{ResponseWriter: keepAlive, room: room}, r.WithContext(ctx))
	keepAlive.stop()
	if connection.added {
		s.removeConnection(connection.room, connection.id)
	}
}

// processHeaderWriter puts its line, ProcessHeader's, on a websocket upgrade made through it. The
// upgrader (gorilla/websocket, which ygo's ServeHTTP and refuseOutsideSchema both use) takes the
// connection and writes its 101 answer on it in one write, reading no header of the writer's, and
// ygo passes it none of its own; so the connection this Hijack hands out adds the line to that
// first write, right after its status line. Every other answer carries the header the ordinary
// way (ServeHTTP sets it).
type processHeaderWriter struct {
	http.ResponseWriter
	line []byte
}

func (w *processHeaderWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	conn, buffered, err := hijacker.Hijack()
	if err != nil {
		return nil, nil, err
	}
	return &processHeaderConn{Conn: conn, line: w.line}, buffered, nil
}

// Unwrap gives http.ResponseController the writer underneath.
func (w *processHeaderWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// processHeaderConn is a hijacked connection whose first write, the upgrade's answer, gains line
// after its status line. line is cleared by that write, which the upgrade makes before it hands
// the connection to any other goroutine, so every later write passes straight through.
type processHeaderConn struct {
	net.Conn
	line []byte
}

func (c *processHeaderConn) Write(p []byte) (int, error) {
	line := c.line
	if line == nil {
		return c.Conn.Write(p)
	}
	c.line = nil
	status := bytes.Index(p, []byte("\r\n"))
	if status < 0 || !bytes.HasPrefix(p, []byte("HTTP/1.1 101 ")) {
		return c.Conn.Write(p)
	}
	status += len("\r\n")
	answer := make([]byte, 0, len(p)+len(line))
	answer = append(answer, p[:status]...)
	answer = append(answer, line...)
	answer = append(answer, p[status:]...)
	written, err := c.Conn.Write(answer)
	// What was written, counted in p's bytes.
	switch {
	case written <= status:
	case written < status+len(line):
		written = status
	default:
		written -= len(line)
	}
	return written, err
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
	// A reader joining a room already resident here asks the other processes for their readers'
	// awareness, so their cursors reach it at once; a room this join has yet to load asks once its
	// load finishes (awarenessRelay.RoomActivated), and the connection registered above is what
	// lets the answers in (holdsRoomForReaders).
	if s.srv.GetDoc(room) != nil {
		s.awarenessRelay.query(room)
	}
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
	if s.shuttingDown(info.Room) && !isShutdownLoad(ctx) {
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
	// the room's ready barrier after it), so nothing writes the tree while this walks it.
	tree, err := treeOf(doc)
	if err == nil {
		_, err = documentMarkdown(tree)
	}
	var askBlocks map[string]struct{}
	switch {
	case errors.Is(err, ErrDocOutsideSchema):
		slog.Warn("dispatch: loaded document outside Proof schema; a replacement from markdown repairs it", "room", room, "error", err)
	case err != nil:
		return err
	default:
		askBlocks = askBlockIDs(tree)
	}
	// The room this load publishes holds the state it takes from here until ygo retires it
	// (releaseIfUnusedLocked).
	state := s.lockState(room)
	state.closed = !open
	// The ask blocks the room loaded with are the baseline its update observer tells new ones by.
	// They were not introduced by any update the observer sees, so none gains an author here: a
	// settlement finds the author of one no ask indexes yet where its writer recorded it, in
	// doc_ask_block_authors (askBlockSources.author).
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
	// (above), so it holds every transaction the room commits from here on, in commit order.
	replica := s.keepReplica(doc, doc.OpenUpdateLog())
	instance := s.keepInstance(room, doc)
	doc.OnUpdate(func(update []byte, origin any) {
		// A published write's update is already durable, and so is a catch-up's batch of another
		// process's rows (remoteUpdateOrigin). Its suppression slot is finished here, before ygo's
		// persistence observer, which the room registers after OnLoadDocument, hands the update on:
		// to the room's worker, or, once CloseRoom has retired the worker, to stranded persistence
		// on this goroutine, which waits on that slot (publishLiveUpdate, catchUpRoom). Both are
		// then classified like any other update, and neither is credited to a local peer
		// (creditContentChange) nor names one as the author of the ask blocks it introduces
		// (observeAskBlocksForUpdate).
		switch durable := origin.(type) {
		case *liveWriteOrigin:
			s.finishSuppressedPersistence(durable.slot, update)
		case *remoteUpdateOrigin:
			s.finishSuppressedPersistence(durable.slot, update)
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
			s.recordUpdateClass(room, update, &documentUpdateClass{contentChanged: true, cursor: &instance.cursor})
		} else if s.beforeObserveUpdate != nil {
			s.beforeObserveUpdate(room)
		}
		// This observer's own update is in the log - ygo appended it under the document's lock at
		// its commit - so its class is recorded before ygo's persistence observer, which runs after
		// this one on this goroutine, hands it on.
		observed := s.observeRoomLog(room, instance, replica, update)
		if observed.changed && s.afterCreditUpdate != nil {
			s.afterCreditUpdate(room)
		}
		if observed.own != nil {
			s.recordUpdateClass(room, update, observed.own)
		}
		s.settleAfterObserving(room, observed)
	})
	return nil
}

// roomObservation is what one drain of a room's update log classified (observeRoomLog): own, the
// class of the update the draining observer was handed, which that observer records itself; local,
// whether it classified an update this process made - a peer's, a service repair's, a published
// live write's; remote, whether it classified a content change another process made, which a
// catch-up applied; and changed, whether it classified any content change at all.
type roomObservation struct {
	own                    *documentUpdateClass
	local, remote, changed bool
}

// observeRoomLog drains the update log of instance, room's resident instance, through its replica
// (renderedReplica.observe), classifying each committed update once against the room as the
// commit before it left it: whichever observer reaches the log first classifies every update
// committed by then, and a live write about to store the room's updates ahead of its own row
// drains it too (observeRoomUpdates), with own nil. Another observer's update is recorded while
// the replica is held: that observer can be waiting for the replica, and runs its persistence
// observer as soon as it has it. own, the update the draining observer was handed, is returned
// for it to record.
//
// An update only the room holds is listed on the instance as unstored (roomInstance.unstored) here,
// as it is classified, so a live write that drains the log after it forked the room finds every
// update its fork can hold, the draining observer's own included, whose class that observer
// records only later.
func (s *Service) observeRoomLog(room string, instance *roomInstance, replica *renderedReplica, own []byte) roomObservation {
	var observed roomObservation
	replica.observe(room, func(logged crdt.LoggedUpdate, contentChanged bool, asks *observedAsks) {
		if _, identityRepair := logged.Origin.(*identityClosureOrigin); identityRepair {
			// Its own observer records its class (onLoadDocument). The replica takes it all the
			// same, so the update after it is measured against the room the repair left.
			return
		}
		var attributed map[string]model.Actor
		if asks != nil {
			attributed = s.observeAskBlocksForUpdate(room, asks.askBlocks(), logged.Origin)
		}
		// The cursor is this instance's, taken here rather than looked up when the update
		// reaches the store: a room evicted and reloaded in between is a different instance,
		// which did not take this update and must not be recorded as holding the version it
		// is stored at.
		class := &documentUpdateClass{update: append([]byte(nil), logged.Update...), contentChanged: contentChanged, durable: true, cursor: &instance.cursor, observed: time.Now()}
		if contentChanged {
			// The authors of the ask blocks this render first showed go with the credit to the
			// update's append, which records them durably (recordAskAuthors): a tree is rendered
			// only for a content change, so a browser's update that names an author always carries
			// a credit. A published live write carries none; its commit recorded its blocks'
			// authors (Ledger.recordSettlementCredit).
			class.credit = s.creditContentChange(room, logged.Origin, &instance.hold, attributed)
			observed.changed = true
		}
		switch logged.Origin.(type) {
		case *remoteUpdateOrigin:
			observed.remote = observed.remote || contentChanged
		case *liveWriteOrigin:
			observed.local = true
		default:
			observed.local = true
			class.instance, class.ahead = instance, &storedAhead{}
			instance.listUnstored(class)
		}
		if own != nil && sameUpdate(logged.Update, own) {
			observed.own = class
			return
		}
		s.recordUpdateClass(room, logged.Update, class)
	})
	return observed
}

// observeRoomUpdates drains live's update log for a caller that is not one of its observers
// (observeRoomLog), and arms the settlement what it classified asks for, as the observer it
// stood in for would have. A document this process lists no replica or no instance for - one no
// room of its loaded, or whose room's document has been collected - has nothing to drain.
func (s *Service) observeRoomUpdates(room string, live *crdt.Doc) {
	instance := s.instanceOf(live)
	replica := s.replicaOf(live)
	if instance == nil || replica == nil {
		return
	}
	s.settleAfterObserving(room, s.observeRoomLog(room, instance, replica, nil))
}

// settleAfterObserving arms room's settlement for what a drain of its update log classified: any
// update this process made arms it a settle delay from now, re-arming one already armed, as
// always. A content change another process made only re-arms a settlement already armed here and
// arms none where none is (rearmSettle): the process whose peer made it armed its own when that
// peer's update reached its room, so editors on two processes typing in turn make one burst, which
// settles once, a settle delay after the last keystroke on either.
func (s *Service) settleAfterObserving(room string, observed roomObservation) {
	switch {
	case observed.local:
		s.scheduleSettle(room)
	case observed.remote:
		s.rearmSettle(room)
	}
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
// the common one, pays for neither. It returns the authors it named for blocks no write
// registered, which the update's append records.
//
// An update another process stored, which a catch-up applied (origin is a remoteUpdateOrigin),
// introduced its blocks on that process: none of this room's connected peers wrote them, so the
// blocks are recorded with no author here, as the blocks a room loads with are, and a settlement
// finds the author that process recorded (askBlockSources.author).
func (s *Service) observeAskBlocksForUpdate(room string, askBlocks map[string]struct{}, origin any) map[string]model.Actor {
	state := s.lockState(room)
	defer s.unlockState(room, state)
	var author *model.Actor
	if _, remote := origin.(*remoteUpdateOrigin); remote {
		author = nil
	} else if sole, ambiguous := soleConnectedActor(state.connected); ambiguous {
		author = new(SettlementActor)
	} else {
		author = sole
	}
	return state.observeAskBlocks(askBlocks, author)
}

// creditContentChange credits an observed content change to its authors. A service repair (origin
// registered by serviceTransact) is credited to no one; a browser that was only connected while it
// happened is not credited either. A committed transaction's live write, which Ledger.Commit
// applies, was credited when the transaction committed and is not credited again; another
// process's update, which a catch-up applies (remoteUpdateOrigin), was credited on that process,
// whose append recorded its authors, and none of this room's peers made it. Any other update is a
// browser edit by one of the peers, which ygo applies while that peer's connection is registered.
// ygo does not say which connection sent it, so every connected peer is credited with it: when
// exactly one is connected it is the latest edit source and replaces `lastActor`, and otherwise
// the edit cannot be pinned on a single peer and no older actor may stand in for it.
//
// The credit goes to F, the room's in-flight credits, until the edit's append lands it
// (AppendUpdateWithCredit): the returned UpdateCredit is how that append reaches it, and how
// askAuthors, the authors of the ask blocks the edit's render first showed (observeAskBlocks),
// reach the durable record in the same append. This takes no advisory lock and touches no
// database: nothing reads F without holding state.mu.
func (s *Service) creditContentChange(room string, origin any, hold *roomHold, askAuthors map[string]model.Actor) *UpdateCredit {
	switch origin.(type) {
	case *liveWriteOrigin, *remoteUpdateOrigin:
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
	return &UpdateCredit{service: s, room: room, state: state, record: record, hold: hold, askAuthors: askAuthors}
}

// addConnection registers a browser connected to room. It is credited only with browser edits
// observed while it is connected (creditContentChange), never for connecting or for an agent's
// edit.
func (s *Service) addConnection(room string, id uint64, actor model.Actor) {
	state := s.lockState(room)
	state.connected[id] = actor
	s.unlockState(room, state)
}

// removeConnection forgets a browser that left room, and with the last one, once ygo has retired
// the room, the room's state (releaseIfUnusedLocked). A document without state has none to forget.
func (s *Service) removeConnection(room string, id uint64) {
	state := s.lockExistingState(room)
	if state == nil {
		return
	}
	delete(state.connected, id)
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
