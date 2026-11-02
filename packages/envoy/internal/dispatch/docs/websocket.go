package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
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

// LoadDoc is the durable state a room loads. It records the load before it reads, and what it read
// after: the room's load (onLoadDocument) takes the record next on the same goroutine (ygo's
// loadRoom) to start the instance's roomHold and its cursor, and a version this process appends
// between the two reaches that instance through the record (roomLoad) rather than no instance at
// all. The read itself takes no advisory lock - a durable writer holds that one across its whole
// transaction, and a load that waited for it would wedge a write that forks the room it loads -
// so the record, not the lock, is what keeps a room from being born behind the head. The record
// also carries the history's size and the tree a socket's admission rendered from it when the
// load takes that socket's preload (roomLoad.read, takeLoaded).
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
			err = fmt.Errorf("panic: %v", recovered)
		}
		if err != nil {
			a.service.failRoomLoad(room, load, err)
		}
	}()
	if loaded, tree, ok := a.service.takePreload(room); ok {
		load.read(loaded.Version, loaded.Update, tree)
		return nil, nil
	}
	result, err := a.store.Load(context.Background(), room)
	if err == nil {
		err = loadableHistory(result.Update)
	}
	if err != nil {
		return nil, err
	}
	load.read(result.Version, result.Update, nil)
	return nil, nil
}

func (a *servicePersistenceAdapter) StoreUpdate(room string, update []byte) error {
	return a.StoreUpdateContext(context.Background(), room, update)
}

// StoreUpdateContext stores a room's update, which ygo's persistence worker hands on one call per
// update, in order: it appends, in one transaction under the document's advisory lock, every
// update of the same room instance its update observer recorded before this one and no writer has
// stored yet, this one, and the run of updates the room recorded after it that its batch takes
// (roomBatchThrough), in the room's commit order, each with the in-flight credit and the class its
// observer recorded (AppendRoomFlush, flushThrough). So the order ygo's persistence hands updates
// on in never decides the order they are stored in, and an update another writer stored first is
// skipped. Each append is gated on that observer's record of which room instance the update was
// made in (documentUpdateClass.cursor) standing at the durable head, and advances it when it
// commits. It returns nil only once the update is durable, by its own transaction or by one that
// stored it first, so ygo's flush still means every update it held is stored.
//
// Unless something waits on the room, the call lingers for the room's queue to fill before its
// batch goes, at most roomAppendLinger from the update's recording (room_appends.go). An update
// that never reaches the store - its room failed, or a live write or a settlement already made it
// durable - lands its credit here instead: the room drops it with the update, which the browser
// resends.
func (a *servicePersistenceAdapter) StoreUpdateContext(ctx context.Context, room string, update []byte) error {
	if a.service.roomFailed(room) {
		// The failed room's update is dropped and its browser resends it to the room that
		// replaces this one, but its class is taken all the same, so the work the record counts
		// for it ends here (dropUpdate).
		a.dropUpdate(room, update)
		return nil
	}
	class, counted, found := a.service.consumeUpdateClass(room, update)
	if found {
		defer a.service.finishUpdateClass(room, counted, class)
	}
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
	ctx = withRoomCursor(ctx, class.cursor)
	if !found {
		// An update no class was recorded for is stored on its own: the room's queue holds
		// nothing to flush it with, and the gate reads the cursor marked above.
		updates := []BrowserUpdate{{Update: update, ContentChanged: class.contentChanged, Credit: class.credit}}
		if _, err := a.store.AppendUpdatesWithCredit(ctx, room, updates); err != nil {
			a.service.failRoom(room, err)
			return err
		}
		return nil
	}
	// The update must read back into a fresh copy before anything of the room's is stored, as an
	// append of updates the store is handed checks (AppendUpdatesWithCredit): one the room took
	// but a copy cannot decode fails the room, which reloads the document from the store.
	if err := crdt.ApplyUpdateV1(newDocumentCopy(), update, nil); err != nil {
		a.service.failRoom(room, err)
		return err
	}
	through, wake, wait := a.service.roomBatchThrough(room, class, true)
	if wake != nil {
		timer := time.NewTimer(wait)
		select {
		case <-wake:
		case <-timer.C:
		}
		timer.Stop()
		if a.service.roomFailed(room) {
			return nil
		}
		through, _, _ = a.service.roomBatchThrough(room, class, false)
	}
	flush := &RoomFlush{service: a.service, room: room, cursor: class.cursor}
	if _, err := a.store.AppendRoomFlush(ctx, room, flush, through, class.credit); err != nil {
		a.service.failRoom(room, err)
		return err
	}
	if flush.storedContentChange() {
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
//
// The record goes first, and what it carries goes with it: the stored state the load read, which
// no document of this process will ever apply (roomLoad.state), and the versions this process
// appended while the load ran, which the room that loads next reads from the store instead
// (took). One other holder reads that record while a room loads: a cold write's shadow names the
// load when its commit found the room still loading, and measures the instance that load leaves
// behind (installShadowLocked, roomShadow.cursorLocked). Dropping the record before the room
// fails is what decides that case - a write committing in between installs no shadow at all
// rather than one naming a load that will leave no instance, and a shadow installed before that
// is freed by the failure on the next line, since a failed room measures no class of its own
// again (failRoomLocked, handOverFailedRoomLocked). So no shadow is left waiting on an instance
// this load will never leave (sjawhar/legion#2519).
func (s *Service) failRoomLoad(room string, load *roomLoad, cause error) {
	s.roomLoads.CompareAndDelete(room, load)
	s.failRoom(room, cause)
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
	s.srv.ServeHTTP(w, r.WithContext(ctx))
}

// admitDocument decides whether a socket may join room's document. A browser editor normalizes a
// tree it cannot represent and writes the result back, so no connection - a first one, or a
// provider's reconnect - joins a room outside the Proof schema until it is replaced from markdown.
// An ask holding blocks off its content rule is inside it: the editor keeps such an ask rather
// than normalizing it away, and settlement marks it `invalid`. The server decides it for every
// client at once, by the same document and rendering `/text` answers with (readDocument), so a
// socket is refused exactly when that read is ErrDocOutsideSchema. A resident room's update observer has rendered the room after every update
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
	replica.classify = func(logged crdt.LoggedUpdate, seq uint64, contentChanged bool, asks *observedAsks) (bool, bool) {
		return s.classifyDrained(room, instance, logged, seq, contentChanged, asks)
	}
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
		settles, changed := replica.observe(room)
		if changed && s.afterCreditUpdate != nil {
			s.afterCreditUpdate(room)
		}
		if settles {
			s.scheduleSettle(room)
		}
	})
	resident = true
	return nil
}

// classifyDrained records the class of logged, the update room's log handed over at commit
// position seq in instance, given whether it changed the room's rendered markdown and the ask
// blocks it left when it did (renderedReplica, observedAsks). It is the room's drainClassifier,
// which runs holding the replica's lock. It reports whether a settlement follows the update, which
// is every update but a repair's, and whether it changed the content.
//
// The cursor is the instance's, taken here rather than looked up when the update reaches the
// store: a room evicted and reloaded in between is a different instance, which did not take this
// update and must not be recorded as holding the version it is stored at.
func (s *Service) classifyDrained(room string, instance *roomInstance, logged crdt.LoggedUpdate, seq uint64, contentChanged bool, asks *observedAsks) (settles, changed bool) {
	class := documentUpdateClass{seq: seq, contentChanged: contentChanged, cursor: &instance.cursor}
	if _, identityRepair := logged.Origin.(*identityClosureOrigin); identityRepair {
		// A repair's writer appends its update in its own transaction, under the document's
		// advisory lock it holds while the room takes the update, so the class is queued stored:
		// no durable writer of the room stores it again, and it is no work the room's state
		// waits on. It is queued all the same, so the room's persistence, which ygo hands it as
		// any other update, finds it stored when it falls outside its suppression slot
		// (consumeSuppressedPersistence).
		s.queueUpdateClass(room, logged.Update, class, true)
		return false, false
	}
	if asks != nil {
		s.observeAskBlocksForUpdate(room, asks.askBlocks())
	}
	class.durable = true
	origin, published := logged.Origin.(*liveWriteOrigin)
	if contentChanged {
		class.credit = s.creditContentChange(room, logged.Origin, &instance.hold)
	} else if !published {
		class.roomCredit = s.unchangedCredit(room, logged.Origin, &instance.hold)
	}
	// A published write's update was stored by the transaction that wrote it, before the room
	// took it. Its last closes the window of the shadow its commit left (reachShadow), before any
	// update after it is queued.
	s.queueUpdateClass(room, logged.Update, class, published)
	if published && origin.shadow != nil {
		s.reachShadow(room, origin.shadow, &instance.cursor, seq)
	}
	return true, contentChanged
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
	record := connectedCreditLocked(state)
	record.seq = state.creditSeq.Add(1)
	state.inflight[record.seq] = record
	state.lastActor = record.lastActor
	state.unsettled = true
	return &UpdateCredit{service: s, room: room, state: state, record: record, hold: hold}
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

// registerConnectionLocked registers a browser connected to the room whose state is state, which
// the caller holds locked. It is credited only with browser edits observed while it is connected
// (creditContentChange), never for connecting or for an agent's edit. It counts nothing: the
// browser's count (connections) is the place under the connection cap reserveConnection took for
// it before, which removeConnection gives back as it forgets the browser. authorize registers each
// document socket's browser through it.
func (s *Service) registerConnectionLocked(state *roomState, id uint64, actor model.Actor) {
	state.connected[id] = actor
	forgetMarkAuthorDeparture(state, actor)
}

// removeConnection forgets a browser that left room, and with the last one, once ygo has retired
// the room, the room's state (releaseIfUnusedLocked). A document without state has none to forget.
// The service's count of connections drops only for an id the record held: a failed room's
// browsers are found on the record that replaced it (handOverFailedRoomLocked), so each leaves
// the count once. The departure of the last browser of an actor is when the unrecorded marks that
// actor wrote start to age (sweepUnrecordedMarks).
func (s *Service) removeConnection(room string, id uint64) {
	state := s.lockExistingState(room)
	if state == nil {
		return
	}
	actor, registered := state.connected[id]
	if registered {
		delete(state.connected, id)
		s.connections.Add(-1)
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
