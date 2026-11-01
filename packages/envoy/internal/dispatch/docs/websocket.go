package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"path"
	"slices"
	"sync"
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
// room's stored history already holds every cleanup the room made. So nothing of ygo's own runs
// between this read and the hook it calls next: there is no decode of what this returns
// (sjawhar/ygo v1.54.1-sami.3, provider/websocket/server.go:1913-1918), and the only way a load
// fails with nothing of this service still to run on its goroutine is a panic out of this read.
//
// Every way the read ends short is therefore failed here, on the load's own goroutine and before
// ygo closes the room's barrier: the store's refusal, a stored history whose header declares more
// items than one update may hold (loadableHistory), and a panic out of any of it, recovered here
// as the hook recovers its own (applyLoadedState, foldDocument). ygo recovers such a panic too
// and hands every waiter the load's error (server.go:1904-1923), but it does so with the record
// this load began still standing under the room's name and the room healthy, which left the
// failure to whichever caller's Apply met it - a caller that cannot tell the load it waited on
// from the load that began after it, since ygo removes the room before any waiter wakes
// (server.go:1932-1941). Recovered here, a load's failure is the load's own (sjawhar/legion#2519).
func (a *servicePersistenceAdapter) LoadDoc(room string) (_ []byte, err error) {
	load := a.service.beginRoomLoad(room)
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("loading room %q: panic: %v", room, recovered)
		}
		if err != nil {
			a.service.failRoomLoad(room, load, err)
		}
	}()
	if loaded, ok := a.service.takePreload(room); ok {
		load.read(loaded.Version, loaded.Update)
		return nil, nil
	}
	result, err := a.store.Load(context.Background(), room)
	if err == nil {
		err = loadableHistory(result.Update)
	}
	if err != nil {
		return nil, err
	}
	load.read(result.Version, result.Update)
	return nil, nil
}

func (a *servicePersistenceAdapter) StoreUpdate(room string, update []byte) error {
	return a.StoreUpdateContext(context.Background(), room, update)
}

// StoreUpdateContext appends a room's update with the in-flight credit its update observer
// recorded (AppendUpdateWithCredit), and with that observer's record of which room instance the
// update was made in (documentUpdateClass.cursor): the append is gated on that instance standing
// at the durable head, and advances it when it commits (appendUpdate). An update that never
// reaches the store - its room failed, or a live write or a settlement already made it durable -
// lands its credit here instead: the room drops it with the update, which the browser resends.
func (a *servicePersistenceAdapter) StoreUpdateContext(ctx context.Context, room string, update []byte) error {
	if a.service.roomFailed(room) {
		// The failed room's update is dropped and its browser resends it to the room that
		// replaces this one, but its class is taken all the same, so the work the record counts
		// for it ends here. Left counted, it would be an append no callback can ever finish:
		// the record - or the one registered in its place, which carries the updates the room's
		// persistence never took (handOverFailedRoomLocked) - would wait on it for ever, so its
		// settlement would retry without end, never closing a room behind the head
		// (reloadRoomBehindHead), and the record would never be released (unusedLocked).
		class, counted, found := a.service.consumeUpdateClass(room, update)
		if found && class.durable {
			a.service.finishDurableAppend(room, counted)
		}
		class.credit.landed()
		return nil
	}
	class, counted, found := a.service.consumeUpdateClass(room, update)
	if found && class.durable {
		defer a.service.finishDurableAppend(room, counted)
	}
	defer class.credit.landed()
	if a.service.consumeSuppressedPersistence(room, update) || a.service.roomFailed(room) {
		return nil
	}
	if _, err := a.store.AppendUpdateWithCredit(withRoomCursor(ctx, class.cursor), room, update, class.contentChanged, class.credit); err != nil {
		a.service.failRoom(room, err)
		return err
	}
	if found && class.durable && class.contentChanged {
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
	_, err := a.store.Compact(ctx, room, compactKeep)
	return err
}

// failRoomLoad is one room load's failure: the room fails, so the next access to it reloads the
// document rather than meeting the half-loaded one (failRoom), and the record the load began goes
// with it. No instance will take that record (keepInstance), and nothing else takes it out: it is
// the document the load never made that releases it (roomLoads).
func (s *Service) failRoomLoad(room string, load *roomLoad, cause error) {
	s.roomLoads.CompareAndDelete(room, load)
	s.failRoom(room, cause)
}

// roomLoad is one room load's record of the durable history behind the room it loads, kept from
// before the load reads the store (LoadDoc) until the instance that load leaves behind stands in
// its place (keepInstance, which onLoadDocument calls on the same goroutine). ygo publishes no
// document for a room while it loads (Server.GetDoc reports a loading room as absent), so a
// version this process appends in that window reaches no instance of its own: it reaches this
// instead (took), and the instance takes it in with the head the load read. Without that the
// fresh room would stand at the head of the read alone while the store stood one past it, and the
// gate would refuse its every durable write until it reloaded - a room born behind, which no
// reload is guaranteed to fix, since the next load can lose the same race.
type roomLoad struct {
	mu   sync.Mutex
	head int64
	// state is the durable state the load read, which the room's load takes before anything that
	// can refuse it (takeLoadedState) and applies itself (applyLoadedState) rather than ygo.
	state []byte
	// appended are the versions took recorded before the instance existed, folded into its cursor
	// when it does (start). instance is that instance, which took advances directly from then on.
	appended []int64
	instance *roomInstance
}

// beginRoomLoad records that room is loading, before the load reads the store: every version
// appended from here on is one the instance this load leaves behind takes in. It replaces the
// record of the room's last load, which ygo has finished with - one room loads at a time
// (getOrCreateRoom) - so the records never outnumber the documents this process has loaded. A
// record goes when the document of the load that left it is collected (keepInstance), or, for a
// load that left none, where the failure that ended it was met (LoadDoc, onLoadDocument).
func (s *Service) beginRoomLoad(room string) *roomLoad {
	load := &roomLoad{}
	s.roomLoads.Store(room, load)
	return load
}

// read records what the load's read of the store saw: the head, the version the room it loads
// holds every update through, and the state through it, which the room's load applies (takeState).
func (load *roomLoad) read(head persistence.Version, state []byte) {
	load.mu.Lock()
	defer load.mu.Unlock()
	load.head = int64(head)
	load.state = state
}

// takeState hands over the state the load read, once, so the record holds no copy of it for as
// long as the document it loaded lives.
func (load *roomLoad) takeState() []byte {
	load.mu.Lock()
	defer load.mu.Unlock()
	state := load.state
	load.state = nil
	return state
}

// took records version, which this process has made durable for the room while its load runs: on
// the load until the instance exists, and on that instance's cursor after (start).
func (load *roomLoad) took(version persistence.Version) {
	load.mu.Lock()
	defer load.mu.Unlock()
	if load.instance != nil {
		advanceRoomCursor(&load.instance.cursor, version)
		return
	}
	load.appended = append(load.appended, int64(version))
}

// start is the instance this load leaves behind: it stands at the head the load read, with every
// version took recorded since folded in, in order and only where each follows the one before, as
// every advance of a cursor is (advanceRoomCursor). The load holds it from here, so a version
// appended before ygo publishes the loaded room still reaches it.
func (load *roomLoad) start() *roomInstance {
	load.mu.Lock()
	defer load.mu.Unlock()
	through := load.head
	slices.Sort(load.appended)
	for _, version := range load.appended {
		if version == through+1 {
			through = version
		}
	}
	load.appended = nil
	load.instance = &roomInstance{hold: roomHold{loaded: load.head}, cursor: roomCursor{through: through}, loadedHead: load.head}
	return load.instance
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

// loadableHistory is the refusal of a stored history the room ygo is loading could not take, so
// the failure names the room (failRoom) rather than reaching a reader as a half-loaded document.
// The room's own load decodes these same bytes into that room's document, in the hook ygo calls
// as soon as this read returns (onLoadDocument, applyLoadedState), so decoding them here as well
// only moved that refusal one step earlier - and on a history one client wrote, which is every
// document the server seeded, it cost 65 ms and 36 MB of each cold settlement of an 8,000-item
// document, time and garbage the writes running beside it paid for on a two-core pod
// (sjawhar/legion#2519). Such a history is judged by the item count its header declares, which is
// what ygo refuses a history for before it integrates any of it (refuseUpdateItemCap); a history
// of several clients, and one whose header does not read, are decoded here as they were.
func loadableHistory(update []byte) error {
	if len(update) == 0 {
		return nil
	}
	return refuseUpdateItemCap(update)
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

// ServeHTTP serves the Hocuspocus-framed document websocket endpoint. A caller that does not
// resolve is refused 401, except a person whose lapsed confirmation the sign-in issuer could not
// renew (identity.ErrSignInUnavailable), who is still signed in and is refused 503
// SIGNIN_UNAVAILABLE as every other route refuses them (identity.WriteError).
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

func (s *Service) onLoadDocument(ctx context.Context, room string, doc *crdt.Doc) (err error) {
	// A refusal of this hook is, but for one, this service's own answer to the load - the
	// document is being rebuilt, the room has already failed, the store did not answer - and not
	// one to fail the room for a second time. The load it refuses is failed here, where that
	// refusal is made, by dropping the record the load began: no instance will take it, and ygo
	// removes the room and answers every caller with this error, so the room's name is left
	// holding nothing of this load. The one that is not ours is the stored history this load
	// read, which is applied here (applyLoadedState): a history the room's document cannot take is
	// the load's own failure, and it fails the room so the next access reloads rather than meeting
	// a half-loaded document. ygo loads one room at a time under a name - the goroutine that
	// created the placeholder owns the load and every other caller for that name waits on its
	// ready barrier (sjawhar/ygo v1.54.1-sami.3, provider/websocket/server.go:1655-1675) - and
	// this runs on that load's own goroutine, so the record read here is this load's own
	// (sjawhar/legion#2519).
	load, held := s.roomLoads.Load(room)
	defer func() {
		if err != nil && held {
			s.roomLoads.CompareAndDelete(room, load)
		}
	}()
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
		// The load's own failure, not this service's refusal of it: these are the bytes the
		// store held for this document, and no room can serve them. The room fails, so the
		// reader who comes next reloads it (failRoomLoad, which the defer above then finds
		// nothing left to drop).
		if held {
			s.failRoomLoad(room, load.(*roomLoad), err)
		} else {
			s.failRoom(room, err)
		}
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
	// (above), so it holds every transaction the room commits from here on, in commit order.
	replica := s.keepReplica(doc, doc.OpenUpdateLog())
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
// (AppendUpdateWithCredit): the returned UpdateCredit is how that append reaches it. This takes
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
