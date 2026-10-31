package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
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
// also takes the state itself and the state's one decode of the load's own (loadedState), a
// socket admission's (loadTree) or, without one, this one's (decodeStoredState): its scans are
// the room's scan total at its load (roomScans), and its copy the room's replica (keepReplica).
// Those decodes, and a fold before them, run under the load's one copy grant (loadedState.grant).
//
// It hands ygo no bytes: the state goes on the record, and the room's load applies it
// (applyLoadedState), because ygo's own apply runs the format cleanup a remote apply runs and the
// room's stored history already holds every cleanup the room made. So the stored state is decoded
// twice on a room's load, that copy and the room's own apply of the same bytes, which the grant
// covers too.
func (a *servicePersistenceAdapter) LoadDoc(room string) ([]byte, error) {
	load := a.service.beginRoomLoad(room)
	grant := newCopyGrant()
	if preload, ok := a.service.takePreload(room); ok {
		_ = grant.cover(context.Background(), len(preload.loaded.Update))
		load.read(preload.loaded.Version, preload.loaded.Update, preload.decoded, grant)
		return nil, nil
	}
	result, decoded, err := a.loadDecoded(room, grant)
	if err != nil {
		grant.release()
		a.service.failRoom(room, err)
		return nil, err
	}
	load.read(result.Version, result.Update, decoded, grant)
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

// validateUpdate decodes a stored history, or an update about to replace one, as the room will
// decode it: into a document with the server's pending queue (newDocumentCopy, rescan), so a
// history a room served is never judged one that cannot load.
func validateUpdate(update []byte) error {
	_, err := rescan(update)
	return err
}

// preloadedDocument is the durable state a document socket's admission check read and its decode
// of it (loadTree, decodeStoredState), kept for the load ygo makes of the room next
// (servicePersistenceAdapter.LoadDoc), so a cold connection reads the history once and decodes it
// once of its own, and that decode's copy becomes the room's replica. Whoever takes a preload off
// Service.preloads takes its state with it (take): the socket's handler keeps the preload until
// the socket ends only to delete it from there (ServeHTTP), and so holds neither a copy a room's
// replica took nor one no load will take.
type preloadedDocument struct {
	loaded  persistence.LoadResult
	decoded decodedState
}

// take is the preload's state, which the preload gives up. Only the caller that took the preload
// off Service.preloads calls it, so nothing else reads the preload meanwhile: the handler that
// stored it only compares it (ServeHTTP).
func (p *preloadedDocument) take() preloadedDocument {
	taken := *p
	p.decoded = decodedState{}
	return taken
}

// takePreload is the state a socket's admission check loaded for room, while the durable head is
// still the one it loaded: every write that changes a document's stored state raises its head (an
// append, a rebuild), and compaction keeps both the head and the state. A stale or absent preload
// is no answer, and the caller loads the room itself; a stale one's copy goes with it.
func (s *Service) takePreload(room string) (preloadedDocument, bool) {
	value, ok := s.preloads.LoadAndDelete(room)
	if !ok {
		return preloadedDocument{}, false
	}
	preload := value.(*preloadedDocument).take()
	head, err := s.persistence.Head(context.Background(), room)
	if err != nil || head != preload.loaded.Version {
		return preloadedDocument{}, false
	}
	return preload, true
}

// dropPreload drops the preload a socket's admission left for room, which no load will take: a
// load of room has run since that admission stored it (onLoadDocument), so the socket joins the
// room that load made. A copy left there would be a whole decode of the document beside the
// room's two, held for as long as the socket stays.
func (s *Service) dropPreload(room string) {
	if value, ok := s.preloads.LoadAndDelete(room); ok {
		value.(*preloadedDocument).take()
	}
}

// leavePreload leaves preload, a socket admission's (loadTree), for the load ygo makes of room next
// (takePreload), and returns what the socket's handler runs as the socket ends: the preload's
// deletion, if no load took it. A preload another admission left there first is replaced, and its
// copy goes with it: a load takes one decode of the stored state, and while the head stands either
// is the state it loads (takePreload). A room resident by now, loaded since this admission found
// none (heldTree), takes nothing of it either, so it is dropped at once; one that loads while this
// admission decodes drops it as its load ends (onLoadDocument, dropPreload). Only a preload left
// in the moment between that load's end and ygo publishing its room stays, until the socket ends
// or the room's next load takes or replaces it, as the preload's bytes stayed before it carried a
// copy.
func (s *Service) leavePreload(room string, preload *preloadedDocument) (forget func()) {
	if previous, replaced := s.preloads.Swap(room, preload); replaced {
		previous.(*preloadedDocument).take()
	}
	if s.srv.GetDoc(room) != nil && s.preloads.CompareAndDelete(room, preload) {
		preload.take()
	}
	return func() { s.preloads.CompareAndDelete(room, preload) }
}

// documentSchemaCloseCode closes a document websocket whose room is outside the Proof schema
// (DOCUMENT_SCHEMA_CLOSE_CODE in packages/contracts), which the dashboard reads as the document's
// repair state rather than as a dropped connection to retry.
const documentSchemaCloseCode = contracts.DocumentSchemaCloseCode

// refuseOutsideSchema completes the upgrade only to close it with documentSchemaCloseCode before
// anything of the document is sent: a browser reads a close code, never the status of a refused
// upgrade.
func refuseOutsideSchema(w http.ResponseWriter, r *http.Request, room string, cause error) {
	slog.Warn("dispatch: refuse a document socket outside Proof schema; a replacement from markdown repairs it", "room", room, "error", cause)
	closeAtUpgrade(w, r, documentSchemaCloseCode, contracts.DocumentSchemaCloseReason)
}

// refuseOverRoomBudget refuses a browser the room budget has no room for (reserveRoom), as
// refuseOutsideSchema refuses one, before any room is made for it and any document loaded: it
// completes the upgrade only to close it with DocumentBusyCloseCode, the transient close, which the
// editor shows and retries rather than dropping anything. The log names the bound and what the
// open rooms are charged against it.
func refuseOverRoomBudget(w http.ResponseWriter, r *http.Request, room string, cause error) {
	slog.Warn("dispatch: refuse a document socket; the open documents fill their memory budget", "room", room, "error", cause)
	closeAtUpgrade(w, r, contracts.DocumentBusyCloseCode, contracts.DocumentBusyCloseReason)
}

// closeAtUpgrade completes the upgrade only to close the socket with code and reason. The zero
// upgrader keeps the same-origin rule ygo's own upgrade applies with no AllowedOrigins configured.
func closeAtUpgrade(w http.ResponseWriter, r *http.Request, code int, reason string) {
	var upgrader gws.Upgrader
	connection, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // the upgrader has answered the request
	}
	defer connection.Close()
	deadline := time.Now().Add(time.Second)
	if err := connection.WriteControl(gws.CloseMessage, gws.FormatCloseMessage(code, reason), deadline); err != nil {
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
	// The room budget admits the browser before anything below loads the document, holding what
	// the stored document would cost until the room's load charges the room itself, and every way
	// out of this handler returns what it still holds (reserveRoom). maxLiveRooms stays the count
	// backstop above.
	if wait, held := s.admissionHolds.LoadAndDelete(room); held {
		select {
		case <-time.After(wait.(time.Duration)):
		case <-r.Context().Done():
			return
		}
	}
	admitting := time.Now()
	reservation, err := s.reserveRoom(r.Context(), room)
	defer s.budget.release(reservation)
	if errors.Is(err, errRoomBudgetFull) {
		refuseOverRoomBudget(w, r, room, err)
		return
	}
	if err != nil {
		// Anything but the budget's refusal is answered 503, which the editor's backoff retries;
		// the line says why, and whether the browser had already gone.
		slog.Warn("dispatch: a document socket's admission failed; answered 503", "room", room, "error", err,
			"waited", time.Since(admitting), "request_ended", r.Context().Err() != nil)
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
	tree, preload, err := s.loadTree(r.Context(), room)
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
	if preload != nil {
		defer s.leavePreload(room, preload)()
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
// because ygo runs OnInject before getOrCreateRoom (sjawhar/ygo v1.51.3-sami.7,
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
	// What LoadDoc recorded is taken first, so a load refused below leaves neither the state nor
	// its copy on the load's record (loadedState). The two copies the load made are the room's
	// from here: a socket's admission reserved them in the room budget before the load
	// (reserveRoom), and the room is charged them below (chargeLoadedRoom).
	taken, recorded := s.takeLoadedState(room)
	loaded := taken.decoded
	// A load never waits for its own room's recovery. That recovery's eviction waits in ygo's
	// CloseRoom for the ready barrier this load holds (sjawhar/ygo v1.51.3-sami.7,
	// provider/websocket/inject.go:521-529) and closes the channel awaitRoomRecovery waits on
	// only once CloseRoom has returned (failRoomLocked), so a wait here is a cycle: the load
	// holds the eviction, and the eviction holds the load. No deadline breaks it either -
	// both loaders that reach this one carry context.Background(), the settlement warm-up
	// (settleRoom) and a committed write's publish (publishLiveUpdate) - so the room would stay
	// failed until the process restarted (LEGION-282). A failed room refuses the load
	// instead: ygo fails the load, closes the barrier with this error and removes the room,
	// which lets the eviction finish, and the next access loads the replacement.
	if err := s.refuseIfRebuilding(room); err != nil {
		taken.grant.release()
		return err
	}
	if err := s.roomFailure(room); err != nil {
		taken.grant.release()
		return err
	}
	// The state goes into the room's own document here rather than through ygo (LoadDoc). It is
	// the last thing the load's grant of the copy-heap bound covers, so the grant is given back
	// as soon as it returns: the reads below wait on the rooms pool, and must hold no part of the
	// bound while they do (copyHeaps).
	applyErr := s.applyLoadedState(room, doc, taken.state)
	taken.grant.release()
	if applyErr != nil {
		return applyErr
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
	// returns (sjawhar/ygo v1.51.3-sami.7, provider/websocket/server.go:1765-1860: loadRoom closes
	// the room's ready barrier after it), so nothing writes the tree while this walks it.
	tree, err := treeOf(doc)
	var markdown string
	if err == nil {
		markdown, err = documentMarkdown(tree)
	}
	// The load's rendering is the room's first measurement (keepReplica), which the peer update
	// hook weighs the room's first update against and the room budget charges the load by.
	var contentMarkdown *string
	var askBlocks map[string]struct{}
	switch {
	case errors.Is(err, ErrDocOutsideSchema):
		slog.Warn("dispatch: loaded document outside Proof schema; a replacement from markdown repairs it", "room", room, "error", err)
	case err != nil:
		return err
	default:
		contentMarkdown = &markdown
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
	// The room takes peer updates only once this hook returns, and every one of them under the
	// room's scan budget, which keepReplica sets below (newRoomScans): the state the load decoded
	// is the server's own and went in unbounded. ygo set the room's pending cap, maxUpdateItems,
	// before it ran this hook (sjawhar/ygo v1.54.1-sami.3, provider/websocket/server.go:1804-1806),
	// and this hook's own apply of the state lifted it only for that apply (applyLoadedState), so
	// this hook's other applies and every peer's are capped
	// (TestAPeerApplyIsHeldToTheRoomsPendingCapAfterTheLoad).
	//
	// The room's scan total starts at what the decode of the state the load read scanned, and the
	// room's replica is the copy that decode made: the decode the load made of its own
	// (LoadDoc, decodeStoredState), which this hook's apply of the same bytes into this document
	// matches item for item - both run with the format cleanup off and the pending queue lifted -
	// scanning the same items, and reporting none, since the apply ran before newRoomScans listed
	// the document. Nothing else holds that copy - the preload and the load's record gave it up as
	// it was taken (takePreload, takeLoadedState) - so the replica owns it, and the room's first
	// transaction decodes nothing more (renderedReplica.observe). A room ygo loaded without
	// reading the store through this service's persistence is decoded again here, from its own
	// document's state, under a grant of its own (holdServerCopy), given back once the copy is
	// the replica's.
	if !recorded {
		encoded := crdt.EncodeStateAsUpdateV1(doc, nil)
		release := holdServerCopy(len(encoded))
		loaded, _ = decodeStoredState(encoded)
		release()
	}
	// The log is opened before the observer is registered and before ygo hands the room to anyone
	// (above), so it holds every transaction the room commits from here on, in commit order, and
	// the copy is the room as the log's first transaction finds it.
	replica := s.keepReplica(doc, doc.OpenUpdateLog(), contentMarkdown, loaded.doc)
	replica.scans.rescanned(loaded.scans)
	// The room's charge in the room budget replaces the reservations of the sockets admitted to it
	// (reserveRoom), which counted the stored version at both copies, with the count its load just
	// made of the room's own document, at both copies too (roomCopies). From here the content every
	// transaction adds is charged, once per committed update as the replica drains the room's
	// update log (renderedReplica.chargeAdded), until a later count of its rendering re-bases it,
	// and what the peer hook admits adds to the room's structure part (roomCharge).
	s.chargeLoadedRoom(room, doc, replica)
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
		// is charged and classified once, by whichever observer reaches it first, against the
		// room as the commit before it left it (renderedReplica). This observer's own update is in
		// the log - ygo appended it under the document's lock at its commit - so its class is
		// recorded before ygo's persistence observer, which runs after this one on this goroutine,
		// hands it on. Another observer's update is recorded while the replica is held: that
		// observer can be waiting for the replica, and runs its persistence observer as soon as it
		// has it.
		var classified, changed bool
		var own *documentUpdateClass
		replica.observe(room, func(logged crdt.LoggedUpdate, contentChanged bool, asks *observedAsks) {
			if _, identityRepair := logged.Origin.(*identityClosureOrigin); identityRepair {
				// Its own observer records its class (above). The replica takes it all the same,
				// so the update after it is measured against the room the repair left, and its
				// content is charged as every transaction's is (chargeAdded).
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
	// A socket admitted while this load ran left its preload after this load's LoadDoc took
	// whatever stood there, and joins the room this load makes, so no load takes it.
	s.dropPreload(room)
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
	delete(state.departedPeers, id)
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
// First it marks every browser the room's state names as connected departed: ygo has none left in
// the room, and their handlers forget them only once this returns (departedPeers).
func (s *Service) settleLastPeer(_ context.Context, room string) {
	state := s.lockExistingState(room)
	if state == nil {
		return
	}
	for id := range state.connected {
		if state.departedPeers == nil {
			state.departedPeers = make(map[uint64]struct{})
		}
		state.departedPeers[id] = struct{}{}
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

// loadDecoded is LoadDoc's own read of room's stored state and its decode (decodeStoredState),
// both under grant, the load's (loadedState.grant): the read's fold of a history of several
// updates covers it inside the read's transaction (fold), and the decode covers it for the state
// the read returned, which the room's load applies into the room's own document on the same grant
// (applyLoadedState). ygo hands LoadDoc no context, so each cover waits until it holds its weight,
// holding no part of the bound meanwhile (copyGrant.cover).
func (a *servicePersistenceAdapter) loadDecoded(room string, grant *copyGrant) (persistence.LoadResult, decodedState, error) {
	ctx := grant.carriedBy(context.Background())
	result, err := a.store.Load(ctx, room)
	if err == nil {
		err = grant.cover(ctx, len(result.Update))
	}
	if err != nil {
		return persistence.LoadResult{}, decodedState{}, err
	}
	decoded, err := decodeStoredState(result.Update)
	return result, decoded, err
}
