package docs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"path"
	"time"

	"github.com/google/uuid"
	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
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

// LoadDoc is the durable state a room loads. It records the load before it reads, and what it read
// after: the room's load (onLoadDocument) takes the record next on the same goroutine (ygo's
// loadRoom) to start the instance's roomHold and its cursor, and a version this process appends
// between the two reaches that instance through the record (roomLoad) rather than no instance at
// all. The read itself takes no advisory lock - a durable writer holds that one across its whole
// transaction, and a load that waited for it would wedge a write that forks the room it loads -
// so the record, not the lock, is what keeps a room from being born behind the head.
//
// It hands ygo no bytes: the state goes on the record, and the room's load applies it
// (applyLoadedState), because ygo's own apply runs the format cleanup a remote apply runs and the
// room's stored history already holds every cleanup the room made.
func (a *servicePersistenceAdapter) LoadDoc(room string) ([]byte, error) {
	load := a.service.beginRoomLoad(room)
	if loaded, ok := a.service.takePreload(room); ok {
		load.read(loaded.Version, loaded.Update)
		return nil, nil
	}
	result, err := a.store.Load(context.Background(), room)
	if err == nil {
		err = validateUpdate(result.Update)
	}
	if err != nil {
		a.service.failRoom(room, err)
		return nil, err
	}
	load.read(result.Version, result.Update)
	return nil, nil
}

func (a *servicePersistenceAdapter) StoreUpdate(room string, update []byte) error {
	return a.StoreUpdateContext(context.Background(), room, update)
}

// StoreUpdateContext stores a room's update: it appends, in one transaction under the document's
// advisory lock, every update of the same room instance its update observer recorded before this
// one and no writer has stored yet, then this one, in the room's commit order, each with the
// in-flight credit and the class its observer recorded (AppendRoomFlush, flushThrough). So the
// order ygo's persistence hands updates on in never decides the order they are stored in, and an
// update another writer stored first is skipped.
//
// Each append carries what the observer recorded with its update: the room instance it was made
// in (documentUpdateClass.cursor) and when the room took it (documentUpdateClass.observed), which
// the store passes to the gate as a browser's append (BrowserAppend, appendBrowser). So the gate
// admits it from a room behind the head with its history intact - it lands at the head, and the
// instance's cursor folds it in once catch-up fills the gap (appendUpdate) - and refuses it only
// when the room's stored history was replaced under it, or when the document's issue is closed and
// the room took the update later than closeMargin after the close. A history a compaction folded
// past the room's cursor is no replacement for the update when it still holds every item the
// update builds on (storedHistoryHolds): it is stored, and the room, which cannot catch up to that
// history, is closed and reloaded at the head once it is (closeRoomOnReplacedHistory): this append
// asks for that itself, so it needs no room-sync lane and waits on no later notification. An
// update the room took before the close and whose append reaches the gate after it - queued behind
// the room's earlier appends, waiting on the document's lock behind a settlement or a compaction,
// or handed here by the close of the room itself (setRoomIssueOpen) - is stored, and settled once
// the issue reopens, a compaction's fold of the history meanwhile included: a room closing for its
// issue takes in no catch-up while its close waits for this append (holdOpen), and that close is
// the reload a folded history needs.
// A replaced history fails the room, as any failed append does: the room reloads from the store,
// which is the close-and-reload a replaced history needs (plan D3.3), and the browser resends the
// update to the reloaded room. A refused update on a closed issue is dropped and closes the room
// for its issue (refuseClosedIssueAppend), so its browsers reconnect read-only. Either is decided
// under the document's advisory lock, so it is never this process's own write in flight, and needs
// no room-sync lane to run.
// An update that never reaches the store - its room failed, or a live write, a settlement or a
// catch-up already made it durable - lands its credit here instead: the room drops it with the
// update, which the browser resends. Whichever way the append ends, the update leaves its
// instance's unstored updates once it has.
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
		if found {
			a.service.finishUpdateClass(room, counted, class)
		}
		class.instance.unlistUnstored(class)
		class.credit.landed()
		return nil
	}
	class, counted, found := a.service.consumeUpdateClass(room, update)
	if found {
		defer a.service.finishUpdateClass(room, counted, class)
	}
	defer class.instance.unlistUnstored(class)
	defer class.credit.landed()
	// The update's suppression slot is consumed whatever its class says, since slots are taken in
	// order. The class decides when the observer recorded one: a published write's and a
	// repair's are queued stored, and a browser update whose bytes match a repair's - a deletion
	// leaves an update that carries no struct, only the room's delete set, which is what a repair
	// that wrote nothing leaves as well - takes that repair's slot while its own class is not
	// stored, and is stored all the same; the repair's own update then finds its class stored.
	suppressed := a.service.consumeSuppressedPersistence(room, update)
	if class.stored.Load() || (suppressed && !found) || a.service.roomFailed(room) {
		return nil
	}
	a.service.hooks.awaitPersistenceHold(ctx, room)
	if !found {
		// An update no class was recorded for is stored on its own: the room's queue holds
		// nothing to flush it with, and the gate reads the cursor it carries.
		browser := &BrowserAppend{cursor: class.cursor, observed: class.observed}
		if _, err := a.store.AppendUpdateWithCredit(withRoomCursor(ctx, class.cursor), room, update, class.contentChanged, class.credit, browser); err != nil {
			if errors.Is(err, ErrIssueClosed) {
				a.service.refuseClosedIssueAppend(room)
				return nil
			}
			a.service.failRoom(room, err)
			return err
		}
		return nil
	}
	// The update must read back into a fresh copy before anything of the room's is stored, as a
	// single append checks (PgVersioned.appendUpdate): one the room took but a copy cannot decode
	// fails the room, which reloads the document from the store.
	if err := crdt.ApplyUpdateV1(newDocumentCopy(), update, nil); err != nil {
		a.service.failRoom(room, err)
		return err
	}
	flush := &RoomFlush{service: a.service, room: room, cursor: class.cursor}
	if _, err := a.store.AppendRoomFlush(ctx, room, flush, class.seq, class.credit); err != nil {
		if errors.Is(err, ErrIssueClosed) {
			a.service.refuseClosedIssueAppend(room)
			return nil
		}
		a.service.failRoom(room, err)
		return err
	}
	if flush.storedContentChange() {
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
	// The awareness clients the socket's peer may announce are bounded before ygo reads its
	// frames (awareness_limit.go).
	s.srv.ServeHTTP(&awarenessLimitWriter{ResponseWriter: w, room: room}, r.WithContext(ctx))
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

// takeLoadedState takes the durable state room's load read (LoadDoc, roomLoad), once. The load
// takes it before anything that can refuse the room, so a refused load holds no state.
func (s *Service) takeLoadedState(room string) []byte {
	record, ok := s.roomLoads.Load(room)
	if !ok {
		return nil
	}
	return record.(*roomLoad).takeState()
}

// applyLoadedState applies state, the durable state room's load read, to the room's document.
// ygo would apply it with the format cleanup on, which follows a remote apply that touches
// formatted text with a commit of the room's own; that commit reaches no persistence (ygo
// registers it after the load), and the stored history already holds every cleanup the room
// made, so the reloaded room would hold a state the room before it never held. So the state is
// applied with the cleanup off, and with the pending queue ygo lifts for a load, and both are set
// back before any peer or write reaches the room: it cleans up after a browser's update as Yjs
// does. The setter does not cancel a cleanup an earlier commit armed, so the cleanup is off before
// the apply starts and on again only once it has returned; and it is never called inside a
// transaction, which holds the document's lock. The apply runs inside ygo's OnLoadDocument hook,
// which logs a panic and goes on as if the hook succeeded, so a panic is returned as the load's
// error, as ygo's own load returns one.
func (s *Service) applyLoadedState(room string, doc *crdt.Doc, state []byte) (err error) {
	if len(state) == 0 {
		return nil
	}
	doc.SetFormatCleanup(false)
	doc.SetMaxPendingItems(math.MaxInt)
	defer func() {
		doc.SetMaxPendingItems(s.srv.MaxPendingItems)
		doc.SetFormatCleanup(true)
	}()
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("bootstrapping room %q: panic: %v", room, recovered)
		}
	}()
	if err := crdt.ApplyUpdateV1(doc, state, nil); err != nil {
		return fmt.Errorf("bootstrapping room %q: %w", room, err)
	}
	return nil
}

func (s *Service) onLoadDocument(ctx context.Context, room string, doc *crdt.Doc) error {
	loaded := s.takeLoadedState(room)
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
	if err := s.applyLoadedState(room, doc, loaded); err != nil {
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
	replica.classify = func(logged crdt.LoggedUpdate, seq uint64, contentChanged bool, asks *observedAsks) (bool, bool, bool) {
		return s.classifyDrained(room, instance, logged, seq, contentChanged, asks)
	}
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
		if repair, identityRepair := origin.(*identityClosureOrigin); identityRepair {
			s.recordSuppressedCommit(repair.slot, update)
			// A repair's update is classified like any other, and its class names the room
			// instance it was made in: one that reaches the room's persistence outside its
			// suppression slot, which another writer's update can take first
			// (consumeSuppressedPersistence), is stored as any other update, and the room it was
			// made in has to take that version in or stand behind the head for good.
		} else if s.beforeObserveUpdate != nil {
			s.beforeObserveUpdate(room)
		}
		// Every observer drains the room's update log, a repair's too, and each committed update
		// is classified once, by whichever drainer reaches it first - an observer, or an exact
		// read (readPoint) - against the room as the commit before it left it
		// (renderedReplica). This observer's own update is in the log - ygo appended it under the
		// document's lock at its commit - so its class is recorded, in the drain and in commit
		// order, before ygo's persistence observer, which runs after this one on this goroutine,
		// hands it on.
		settles, changed, local := replica.observe(room)
		if changed && s.afterCreditUpdate != nil {
			s.afterCreditUpdate(room)
		}
		switch {
		case local:
			s.scheduleSettle(room)
		case settles && changed:
			// A content change another process made, which a catch-up applied, only re-arms a
			// settlement already armed here and arms none where none is (rearmSettle): the
			// process whose peer made it armed its own when that peer's update reached its room,
			// so editors on two processes typing in turn make one burst, which settles once, a
			// settle delay after the last keystroke on either.
			s.rearmSettle(room)
		}
	})
	return nil
}

// classifyDrained records the class of logged, the update room's log handed over at commit
// position seq in instance, given whether it changed the room's rendered markdown and the ask
// blocks it left when it did (renderedReplica, observedAsks). It is the room's drainClassifier,
// which runs holding the replica's lock. It reports whether a settlement follows the update, which
// is every update but a repair's, whether it changed the content, and whether this process made
// it - a peer's update or a published live write's, rather than another process's append a
// catch-up applied.
//
// The cursor is the instance's, taken here rather than looked up when the update reaches the
// store: a room evicted and reloaded in between is a different instance, which did not take this
// update and must not be recorded as holding the version it is stored at. An update only the room
// holds is listed on the instance as unstored (roomInstance.unstored) as it is classified, so a
// room query of another process can answer with it (unstoredShare).
func (s *Service) classifyDrained(room string, instance *roomInstance, logged crdt.LoggedUpdate, seq uint64, contentChanged bool, asks *observedAsks) (settles, changed, local bool) {
	class := documentUpdateClass{seq: seq, contentChanged: contentChanged, cursor: &instance.cursor, observed: time.Now()}
	if _, identityRepair := logged.Origin.(*identityClosureOrigin); identityRepair {
		// A repair's writer appends its update in its own transaction, under the document's
		// advisory lock it holds while the room takes the update, so the class is queued stored:
		// no durable writer of the room stores it again, and it is no work the room's state
		// waits on. It is queued all the same, so the room's persistence, which ygo hands it as
		// any other update, finds it stored when it falls outside its suppression slot
		// (consumeSuppressedPersistence).
		s.queueUpdateClass(room, logged.Update, class, true)
		return false, false, false
	}
	var attributed map[string]model.Actor
	if asks != nil {
		attributed = s.observeAskBlocksForUpdate(room, asks.askBlocks(), logged.Origin)
	}
	class.durable = true
	origin, published := logged.Origin.(*liveWriteOrigin)
	_, remote := logged.Origin.(*remoteUpdateOrigin)
	if contentChanged {
		// The authors of the ask blocks this render first showed go with the credit to the
		// update's append, which records them durably (recordAskAuthors): a tree is rendered
		// only for a content change, so a browser's update that names an author always carries
		// a credit. A published live write carries none; its commit recorded its blocks'
		// authors (Ledger.recordSettlementCredit).
		class.credit = s.creditContentChange(room, logged.Origin, &instance.hold, attributed)
	} else if !published {
		class.roomCredit = s.unchangedCredit(room, logged.Origin, &instance.hold)
	}
	// A published write's update was stored by the transaction that wrote it, before the room
	// took it, and a catch-up's is the stored row itself. Every other update only the room holds,
	// so it names its instance and is listed there until its append has returned.
	if !published && !remote {
		class.instance = instance
	}
	queued := s.queueUpdateClass(room, logged.Update, class, published || remote)
	if class.instance != nil {
		instance.listUnstored(queued)
	}
	// A published write's last update closes the window of the shadow its commit left
	// (reachShadow), before any update after it is queued.
	if published && origin.shadow != nil {
		s.reachShadow(room, origin.shadow, &instance.cursor, seq)
	}
	return true, contentChanged, !remote
}

// observeRoomUpdates drains live's update log for a caller that is not one of its observers, so
// every update committed to the room by now is classified (classifyDrained), and arms the
// settlement what it classified asks for, as the observer it stood in for would have. A document
// this process lists no replica for - one no room of its loaded, or whose room's document has
// been collected - has nothing to drain.
func (s *Service) observeRoomUpdates(room string, live *crdt.Doc) {
	replica := s.replicaOf(live)
	if replica == nil {
		return
	}
	settles, changed, local := replica.observe(room)
	switch {
	case local:
		s.scheduleSettle(room)
	case settles && changed:
		s.rearmSettle(room)
	}
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
	record := connectedCreditLocked(state)
	record.seq = state.creditSeq.Add(1)
	state.inflight[record.seq] = record
	state.lastActor = record.lastActor
	state.unsettled = true
	return &UpdateCredit{service: s, room: room, state: state, record: record, hold: hold, askAuthors: askAuthors}
}

// unchangedCredit is the credit a browser update the room's update observer measured as no change
// carries for the room's shadow (documentUpdateClass.roomCredit): the peers connected as the room
// took it, as creditContentChange would credit them, recorded only while a live write is open,
// whose shadow may measure the update a change once the write is stored before it. It is not in F
// and takes no credit sequence: no version lists an update recorded as no change, and its append
// writes these authors only when it stores the update as a change. Nil for a service repair, and
// while no write is open.
func (s *Service) unchangedCredit(room string, origin any, hold *roomHold) *UpdateCredit {
	if _, service := s.serviceOrigins.Load(origin); service {
		return nil
	}
	state := s.lockState(room)
	defer s.unlockState(room, state)
	if state.liveWriter == nil {
		return nil
	}
	return &UpdateCredit{service: s, room: room, state: state, record: connectedCreditLocked(state), hold: hold}
}

// connectedCreditLocked is a credit record naming the peers connected to state, and the one of
// them it pins the edit on, if one. The caller holds state.mu.
func connectedCreditLocked(state *roomState) *inflightCredit {
	record := &inflightCredit{authors: make(map[string]model.Actor, len(state.connected))}
	for _, actor := range state.connected {
		record.authors[actorKey(actor)] = actor
	}
	record.lastActor, _ = soleConnectedActor(state.connected)
	return record
}

// addConnection registers a browser connected to room. It is credited only with browser edits
// observed while it is connected (creditContentChange), never for connecting or for an agent's
// edit.
func (s *Service) addConnection(room string, id uint64, actor model.Actor) {
	state := s.lockState(room)
	state.connected[id] = actor
	forgetMarkAuthorDeparture(state, actor)
	s.unlockState(room, state)
}

// removeConnection forgets a browser that left room, and with the last one, once ygo has retired
// the room, the room's state (releaseIfUnusedLocked). A document without state has none to forget.
// The departure of the last browser of an actor is when the unrecorded marks that actor wrote
// start to age (sweepUnrecordedMarks).
func (s *Service) removeConnection(room string, id uint64) {
	state := s.lockExistingState(room)
	if state == nil {
		return
	}
	actor, registered := state.connected[id]
	delete(state.connected, id)
	if registered {
		recordMarkAuthorDeparture(state, actor, s.now())
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
