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

// LoadDoc reads the durable state a room loads and hands ygo none of it: the state goes on the
// load's record (roomLoad.read), and the room's own load hook decodes it (bootstrapLoadedState,
// whose comment states how). ygo, given no bytes, decodes nothing itself. A preload
// (takePreload), which the socket's admission check decoded, goes on the record as a read would.
//
// It records the load before it reads, and what it read after: the room's load (onLoadDocument)
// takes the record next on the same goroutine (ygo's loadRoom) to start the instance's roomHold
// and its cursor, and a version this process appends between the two reaches that instance through
// the record (roomLoad) rather than no instance at all. The read itself takes no advisory lock - a
// durable writer holds that one across its whole transaction, and a load that waited for it would
// wedge a write that forks the room it loads - so the record, not the lock, is what keeps a room
// from being born behind the head. A read that fails fails the room (failRoom).
func (a *servicePersistenceAdapter) LoadDoc(room string) ([]byte, error) {
	load := a.service.beginRoomLoad(room)
	if loaded, ok := a.service.takePreload(room); ok {
		load.read(loaded.Version, loaded.Update)
		return nil, nil
	}
	result, err := a.store.Load(context.Background(), room)
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
// update another writer stored first is skipped. Each append is gated on that observer's record of
// which room instance the update was made in (documentUpdateClass.cursor) standing at the durable
// head, and advances it when it commits. An update that never reaches the store - its
// room failed, or a live write or a settlement already made it durable - lands its credit here
// instead: the room drops it with the update, which the browser resends.
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
		if found {
			a.service.finishUpdateClass(room, counted, class)
		}
		class.credit.landed()
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
		if _, err := a.store.AppendUpdateWithCredit(ctx, room, update, class.contentChanged, class.credit); err != nil {
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
		a.service.failRoom(room, err)
		return err
	}
	if flush.storedContentChange() {
		a.service.scheduleSettleAfterAppend(room)
	}
	return nil
}

// Compact is ygo's room-close compaction: its persistence worker calls it as it exits, the server's
// shutdown included, and it runs the tail rule (compactRoom) inline on that worker. roomServer
// keeps that exit from racing a repair, but intentionally does not gate a published live write: it
// is already durable, and its room can still retire under publishLiveUpdate's Apply. Compaction is
// housekeeping, so the worker leaves a room whose lock another holder has to a later compaction
// rather than waiting behind that holder. A failed room's eviction is the exception: it waits for
// the lock and compacts before recovery, and with nothing to fold it still waits for the lock
// (passRoomLock), so a repair written into the failed room commits before the room reloads.
func (a *servicePersistenceAdapter) Compact(ctx context.Context, room string) error {
	_, err := a.store.compactRoom(ctx, room, tailRule, a.service.roomFailed(room))
	return err
}

// validateUpdate decodes a stored history, or an update about to replace one, as the room will
// decode it: into a document with maxPending parked items, the caller's own pending queue
// (Service.maxPendingItems for a rebuild's check of the history its rooms would load,
// PgVersioned.maxPendingItems for the store's check of a rebuilt seed; see maxUpdateItems for the
// rule both follow), which is the cap a room's load hook decodes its stored state under
// (bootstrapLoadedState), so a history a room served is never judged one that cannot load. A
// rebuild runs it on its seed and on the history it replaces; a room's load does not, as its own
// decode is the check.
func validateUpdate(update []byte, maxPending int) error {
	if len(update) == 0 {
		return nil
	}
	return decodeStoredState(newCappedDocumentCopy(maxPending), update)
}

// decodeStoredState applies state, a stored history, one of its rows or the state a room or cold
// read loads, to doc, and is the one way the service decodes stored state, so every refusal of it
// reads the same. ygo's apply names a failure as ErrInvalidUpdate followed by the decoder's reason
// ("crdt: invalid update: encoding: unexpected end of input"). The fork release this module pins
// (go.mod) regressed on that: where its reader of an update's structs fails it reports the
// sentinel wrapped in itself ("crdt: invalid update: crdt: invalid update") and no reason, which
// invalidOnce returns as the sentinel alone so the line a refusal logs states it once. The fix
// ships in the fork's next release
// (dispatch://INFRA-593/artifact/ygo-truncated-reason-patch); once the pinned tag carries it,
// delete invalidOnce, doubledInvalidUpdate and the reasonless cases of decode_reason_test.go and
// fold_reason_test.go, which then fail saying nothing reasonless is left.
func decodeStoredState(doc *crdt.Doc, state []byte) error {
	return invalidOnce(crdt.ApplyUpdateV1(doc, state, nil))
}

// invalidOnce is err, a ygo error from reading stored state, with the reasonless refusal ygo words
// as ErrInvalidUpdate wrapped in itself returned as the sentinel alone (decodeStoredState).
func invalidOnce(err error) error {
	if err != nil && err.Error() == doubledInvalidUpdate {
		return crdt.ErrInvalidUpdate
	}
	return err
}

// doubledInvalidUpdate is how ygo words an update it refuses with no reason (invalidOnce).
var doubledInvalidUpdate = crdt.ErrInvalidUpdate.Error() + ": " + crdt.ErrInvalidUpdate.Error()

// bootstrapLoadedState decodes state, what room's load read (takeLoadedState), into doc, the
// room's document. It is the one decode of the stored state a room's load makes: ygo is handed no
// bytes (servicePersistenceAdapter.LoadDoc), so it decodes under the pending cap ygo set on doc
// before its load hook ran, the server's MaxPendingItems, which New sets from the service's own
// cap (Service.maxPendingItems; see maxUpdateItems), and through decodeStoredState, so a refusal
// reads as every other decode of stored state does. An empty state - an empty history - decodes
// nothing.
//
// It decodes with ygo's format cleanup off (crdt.Doc.SetFormatCleanup), which would follow an
// apply that touches formatted text with a commit of the room's own: that commit reaches no
// persistence (ygo registers it after the load), and the stored history already holds every
// cleanup the room made (newCappedDocumentCopy), so the reloaded room would hold a state the room
// before it never held. The cleanup is set back on once the apply has returned, before any peer or
// write reaches the room, so the room cleans up after a browser's update as Yjs does; the setter
// does not cancel a cleanup an earlier commit armed, so it is off before the apply starts, and it
// is never called inside a transaction, which holds the document's lock. The apply runs inside
// ygo's OnLoadDocument hook, which logs a panic and goes on as if the hook succeeded, so a panic
// is returned as the decode's error, which fails the room as a refusal does.
func (s *Service) bootstrapLoadedState(room string, doc *crdt.Doc, state []byte) (err error) {
	if len(state) == 0 {
		return nil
	}
	if s.bootstrapping != nil {
		s.bootstrapping(room, state)
	}
	doc.SetFormatCleanup(false)
	defer doc.SetFormatCleanup(true)
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("decode room %q: panic: %v", room, recovered)
		}
	}()
	return decodeStoredState(doc, state)
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

// onLoadDocument is the room's load hook, which ygo runs once it has made room's document and set
// its pending cap, and before it hands the document to anyone. It decodes the state the room's
// load read (servicePersistenceAdapter.LoadDoc) into doc (bootstrapLoadedState), and a state that
// does not decode fails the room (failRoom) with the decode's error, which it returns to ygo, so
// the room is not published and its next access loads it afresh. Then it records what the room
// starts from: its issue's state, the settlement it owes, its ask blocks, its replica and its
// instance, and the update observer that classifies and credits every update the room commits.
func (s *Service) onLoadDocument(ctx context.Context, room string, doc *crdt.Doc) error {
	// The state is taken off the load's record first, so a load refused below leaves none held.
	stored := s.takeLoadedState(room)
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
	// Decoded before anything reads or observes the document (bootstrapLoadedState).
	if err := s.bootstrapLoadedState(room, doc, stored); err != nil {
		s.failRoom(room, err)
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
