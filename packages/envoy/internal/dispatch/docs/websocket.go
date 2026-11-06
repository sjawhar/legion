package docs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"slices"
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
// whose comment states how), because ygo's own apply runs the format cleanup a remote apply runs
// and the room's stored history already holds every cleanup the room made. A preload
// (takePreload), which the socket's admission check decoded, goes on the record as a read would.
// The record also takes the state's one decode of the load's own (loadedState), that admission's
// or, without one, this one's (decodeStoredCopy): its scans are the room's scan total at its load
// (roomScans), and its copy the room's replica (keepReplica). So the stored state is decoded
// twice on a room's load, that copy and the load hook's own decode of the same bytes; those
// decodes, and a fold before them, run under the load's one copy grant (loadedState.grant).
//
// It records the load before it reads, and what it read after: the room's load (onLoadDocument)
// takes the record next on the same goroutine (ygo's loadRoom) to start the instance's roomHold
// and its cursor, and a version this process appends between the two reaches that instance through
// the record (roomLoad) rather than no instance at all. The read itself takes no advisory lock - a
// durable writer holds that one across its whole transaction, and a load that waited for it would
// wedge a write that forks the room it loads - so the record, not the lock, is what keeps a room
// from being born behind the head.
//
// Nothing of ygo's own runs between this read and the hook it calls next: there is no decode of
// what this returns (sjawhar/ygo v1.55.1-sami.1, provider/websocket/server.go:2006-2011), and the
// only way a load fails with nothing of this service still to run on its goroutine is a panic out
// of this read. Every way the read ends short is therefore failed here, on the load's own
// goroutine and before ygo closes the room's barrier: the store's refusal, a stored history whose
// header declares more items than one update may hold (loadDecoded, loadableHistory), and a panic
// out of any of it, recovered here as the hook recovers its own (bootstrapLoadedState,
// foldDocument). A history ygo refuses the bytes of is not one of them: the room's own decode
// fails the room for it (onLoadDocument). ygo
// recovers such a panic too and hands every waiter the load's error (server.go:1904-1923), but it
// does so with the record this load began still standing under the room's name and the room
// healthy, which left the failure to whichever caller's Apply met it - a caller that cannot tell
// the load it waited on from the load that began after it, since ygo removes the room before any
// waiter wakes (server.go:1932-1941). Recovered here, a load's failure is the load's own
// (sjawhar/legion#2519).
func (a *servicePersistenceAdapter) LoadDoc(room string) (_ []byte, err error) {
	load := a.service.beginRoomLoad(room)
	slot := newLoadSlot()
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("panic: %v", recovered)
		}
		if err != nil {
			// A failed load leaves the slot to no one: the record takes the state, the copy and
			// the slot together, and only where this returns no error (read), so what this load
			// waited for is given back here.
			slot.release()
			a.service.failRoomLoad(room, load, err)
		}
	}()
	if preload, ok := a.service.takePreload(room); ok {
		_ = slot.take(context.Background())
		load.read(preload.loaded.Version, preload.loaded.Update, preload.decoded, slot)
		return nil, nil
	}
	result, decoded, err := a.loadDecoded(room, slot)
	if err != nil {
		return nil, err
	}
	load.read(result.Version, result.Update, decoded, slot)
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
	// leaves an update that carries no struct, only its own deletes, so a browser's deletion of
	// the items a repair deleted leaves the repair's bytes - takes that repair's slot while its own
	// class is not stored, and is stored all the same; the repair's own update then finds its
	// class stored.
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

// failRoomLoad is one room load's failure: the room fails, so the next access to it reloads the
// document rather than meeting the half-loaded one (failRoom), and the record the load began goes
// with it. No instance will take that record (keepInstance), and nothing else takes it out: it is
// the document the load never made that releases it (roomLoads).
//
// The record goes first, and what it carries goes with it: the stored state the load read and
// the copy it decoded, which no document of this process will ever hold, and the grant of the
// copy-heap bound that covered them, given back here as a replaced record's is (loadedState,
// beginRoomLoad) - the room's load, which refuses the load after it has taken all three, leaves
// the record empty and gives the grant back itself (takeLoadedState, onLoadDocument) - and the
// versions this process appended while the load ran, which the room that loads next reads from
// the store instead (took). One other holder reads that record while a room loads: a cold
// write's shadow names the load when its commit found the room still loading, and measures the
// instance that load leaves behind (installShadowLocked, roomShadow.cursorLocked). Dropping the
// record before the room fails is what decides that case - a write committing in between
// installs no shadow at all rather than one naming a load that will leave no instance, and a
// shadow installed before that is freed by the failure on the next line, since a failed room
// measures no class of its own again (failRoomLocked, handOverFailedRoomLocked). So no shadow is
// left waiting on an instance this load will never leave (sjawhar/legion#2519).
func (s *Service) failRoomLoad(room string, load *roomLoad, cause error) {
	if s.roomLoads.CompareAndDelete(room, load) {
		load.take().slot.release()
	}
	s.failRoom(room, cause)
}

// validateUpdate decodes an update about to replace a stored history, as the room will decode it:
// into a document with maxPending parked items, the caller's own pending queue
// (PgVersioned.maxPendingItems for the store's check of a rebuilt seed; see maxUpdateItems for
// the rule it follows), which is the cap a room's load hook decodes its stored state under
// (bootstrapLoadedState), so a seed a room would serve is never judged one that cannot load. A
// rebuild runs it on its seed alone (RebuildTx); the history it replaces is judged by the decode
// the rebuild's source already made of it (rebuildSource), and a room's load by its own.
func validateUpdate(update []byte, maxPending int) error {
	if len(update) == 0 {
		return nil
	}
	return decodeStoredState(newCappedDocumentCopy(maxPending), update)
}

// decodeStoredState applies state, a stored history, one of its rows or the state a room or cold
// read loads, to doc, and is the one way the service decodes stored state, so every refusal of it
// reads the same. ygo's apply names a failure as ErrInvalidUpdate followed by the decoder's reason
// ("crdt: invalid update: encoding: unexpected end of input").
func decodeStoredState(doc *crdt.Doc, state []byte) error {
	return crdt.ApplyUpdateV1(doc, state, nil)
}

// bootstrapLoadedState decodes state, what room's load read (takeLoadedState), into doc, the
// room's document. It is the decode the room's load makes of its own: ygo is handed no bytes
// (servicePersistenceAdapter.LoadDoc), so it decodes under the pending cap ygo set on doc before
// its load hook ran, the server's MaxPendingItems, which New sets from the service's own cap
// (Service.maxPendingItems; see maxUpdateItems), and through decodeStoredState, so a refusal
// reads as every other decode of stored state does. The load decoded the same bytes once more,
// into the copy the room's replica takes (decodeStoredCopy), under the same cap and with the same
// cleanup off, so the room holds item for item what that copy holds. An empty state - an empty
// history - decodes nothing.
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

// loadableHistory is the refusal of a stored history whose header declares more items than one
// document update may hold (itemCapRefusal), which ygo refuses before it integrates any of it, so
// the failure names the room before the load decodes anything (loadDecoded). It decodes nothing:
// the decode that follows it is the copy the room's replica takes (decodeStoredCopy), and the
// room's own load decodes the same bytes into the room's document, in the hook ygo calls as soon
// as this read returns (onLoadDocument, bootstrapLoadedState), so a history that does not decode
// fails the room at one of those rather than at a decode made to judge it. Decoding them here as
// well cost a third decode of every room load - on a history one client wrote, which is every
// document the server seeded, 65 ms and 36 MB of each cold settlement of an 8,000-item document
// (sjawhar/legion#2519), and on a history of many writers, the costliest decode there is, all of
// it again. A history of several clients declares no single count (updateItems), so those decodes
// alone judge it.
func loadableHistory(update []byte) error {
	if items, named := updateItems(update); named && items > maxUpdateItems {
		return itemCapRefusal(items, maxUpdateItems)
	}
	return nil
}

// preloadedDocument is the durable state a document socket's admission check read and its decode
// of it (loadTree, decodeStoredCopy), kept for the load ygo makes of the room next
// (servicePersistenceAdapter.LoadDoc), so a cold connection reads the history once and decodes it
// once of its own, and that decode's copy becomes the room's replica. Whoever takes a preload off
// Service.preloads takes its state with it (take): the socket's handler keeps the preload until
// the socket ends only to delete it from there (ServeHTTP), and so holds neither a copy a room's
// replica took nor one no load will take.
type preloadedDocument struct {
	loaded  persistence.LoadResult
	decoded decodedState
}

// take is the preload's state, which the preload gives up. Only the caller that took the
// preload off Service.preloads calls it, so nothing else reads the preload meanwhile: the
// handler that stored it only compares it (ServeHTTP).
func (p *preloadedDocument) take() preloadedDocument {
	taken := *p
	p.decoded = decodedState{}
	return taken
}

// takePreload is the state a socket's admission check loaded for room, while the durable head is
// still the one it loaded: every write that changes a document's stored state raises its head (an
// append, a rebuild), and compaction keeps both the head and the state. A stale or absent preload
// is no answer, and the caller loads the room itself; a stale one's copy and its slot go with it.
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
// room's two, held for as long as the socket stays, and its slot would be one the pod's load
// concurrency never gets back.
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

// refuseOverRoomBudget refuses a browser the room budget has no room for (ServeHTTP), as
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
	// The room budget admits the browser after the read below, not before it: it joins a room
	// the budget already charges, and is otherwise weighed carrying the weight of the room it is
	// about to load, which the read has just measured exactly (roomBudget.refusal). It is
	// refused while the open rooms and the other admissions' reservations leave no room for it
	// and the sweeps it starts can close nothing. maxLiveRooms stays the count backstop above.
	//
	// The budget charges a room only when its load ends, so an admission weighed on the charges
	// alone would read the same headroom as every other admission in flight and they would all
	// pass: measured, twenty-four browsers arriving together were all admitted, their rooms
	// charged 5,976,639,564 bytes against a 2,147,483,648-byte budget, and the pod's kernel
	// charged 7,225 MiB of its 4,096 (INFRA-179). What closes that is the reservation each
	// admission holds from its weighing until its room's load charges it, so each is weighed
	// against every other's room as well as against the rooms already open.
	if wait, held := s.admissionHolds.LoadAndDelete(room); held {
		select {
		case <-time.After(wait.(time.Duration)):
		case <-r.Context().Done():
			return
		}
	}
	// A browser editor normalizes a tree it cannot represent and writes the result back, so no
	// connection - a first one, or a provider's reconnect - joins a room outside the Proof schema
	// until it is replaced from markdown. An ask holding blocks off its content rule is inside it:
	// the editor keeps such an ask rather than normalizing it away, and settlement marks it
	// `invalid`. The server decides it here, for every client at once, by the same stored document
	// and rendering `/text` answers with (readDocument), so a socket is refused exactly when that
	// read is ErrDocOutsideSchema.
	//
	// It runs before the budget weighs the admission, because what it reads is what the budget
	// weighs: the stored state it decoded is the state the room is about to load, so its length
	// is the weight of the room this admission adds. The read is a whole-document decode, so it
	// takes a slot of the pod's load concurrency for the read alone and gives it back before
	// the weighing: fifty admissions do not decode fifty documents at once, and no admission
	// holds a slot while it waits on the budget, which would couple it to every other holder.
	readSlot := newLoadSlot()
	if s.srv.GetDoc(room) == nil {
		if err := readSlot.take(r.Context()); err != nil {
			slog.Warn("dispatch: a document socket's admission gave up waiting for a load slot; answered 503",
				"room", room, "error", err)
			http.Error(w, ErrServiceUnavailable.Error(), http.StatusServiceUnavailable)
			return
		}
	}
	tree, preload, err := s.loadTree(readSlot.carriedBy(r.Context()), room)
	readSlot.release()
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
	admitting := time.Now()
	var reservation *roomReservation
	if !s.budget.join(room) {
		// The weight is the room this admission is about to load, which the budget does not
		// charge until that load ends: the stored state the read above decoded, at roomWeight.
		// A read that left no preload loaded nothing and adds no room (heldTree), and a room
		// the budget already charges is joined above and weighed at nothing. The reservation is
		// held until that room's load charges it or this handler returns, whichever comes
		// first, so every other admission in flight is weighed against this room too.
		weight := int64(0)
		if preload != nil {
			weight = roomWeight(int64(len(preload.loaded.Update)))
		}
		reservation, err = s.budget.refusal(r.Context(), time.Now().Add(s.admissionWait), room, weight)
	}
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
// because ygo runs OnInject before getOrCreateRoom (sjawhar/ygo v1.55.1-sami.1,
// provider/websocket/inject.go:324-333): an injection refused here has published no room
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
// does not decode fails the room and drops the load's record (failRoomLoad) with the decode's
// error, which it returns to ygo, so the room is not published and its next access loads it
// afresh. Then it records what the room starts from: its issue's state, the settlement it owes,
// its ask blocks, its replica and its instance, and the update observer that classifies and
// credits every update the room commits.
func (s *Service) onLoadDocument(ctx context.Context, room string, doc *crdt.Doc) (err error) {
	// A refusal of this hook is, but for one, this service's own answer to the load - the
	// document is being rebuilt, the room has already failed, the store did not answer - and not
	// one to fail the room for a second time. The load it refuses is failed here, where that
	// refusal is made, by dropping the record the load began: no instance will take it, and ygo
	// removes the room and answers every caller with this error, so the room's name is left
	// holding nothing of this load. The one that is not ours is the stored history this load
	// read, which is decoded here (bootstrapLoadedState): a history the room's document cannot take is
	// the load's own failure, and it fails the room so the next access reloads rather than meeting
	// a half-loaded document. ygo loads one room at a time under a name - the goroutine that
	// created the placeholder owns the load and every other caller for that name waits on its
	// ready barrier (sjawhar/ygo v1.55.1-sami.1, provider/websocket/server.go:1744-1763) - and
	// this runs on that load's own goroutine, so the record read here is this load's own
	// (sjawhar/legion#2519).
	//
	// What LoadDoc recorded is taken first, so a load refused below leaves neither the state nor
	// its copy on the record it drops (loadedState). The two copies the load made are the room's
	// from here: the budget charges them below, at the bytes of the state they were built from
	// (chargeLoadedRoom).
	taken, record := s.takeLoadedState(room)
	defer func() {
		if err != nil && record != nil {
			s.roomLoads.CompareAndDelete(room, record)
		}
	}()
	loaded := taken.decoded
	// A load never waits for its own room's recovery. That recovery's eviction waits in ygo's
	// CloseRoom for the ready barrier this load holds (sjawhar/ygo v1.55.1-sami.1,
	// provider/websocket/inject.go:568-576) and closes the channel awaitRoomRecovery waits on
	// only once CloseRoom has returned (failRoomLocked), so a wait here is a cycle: the load
	// holds the eviction, and the eviction holds the load. No deadline breaks it either -
	// both loaders that reach this one carry context.Background(), the settlement warm-up
	// (settleRoom) and a committed write's publish (publishLiveUpdate) - so the room would stay
	// failed until the process restarted (LEGION-282). A failed room refuses the load
	// instead: ygo fails the load, closes the barrier with this error and removes the room,
	// which lets the eviction finish, and the next access loads the replacement.
	if err := s.refuseIfRebuilding(room); err != nil {
		taken.slot.release()
		return err
	}
	if err := s.roomFailure(room); err != nil {
		taken.slot.release()
		return err
	}
	// The state is decoded into the room's own document here rather than through ygo (LoadDoc),
	// before anything reads or observes the document (bootstrapLoadedState). It is the last thing
	// the load's slot covers, so the slot is given back as soon as it returns: the reads below
	// wait on the rooms pool, and must hold no slot while they do (podLoadSlots).
	decodeErr := s.bootstrapLoadedState(room, doc, taken.state)
	taken.slot.release()
	if decodeErr != nil {
		// The load's own failure, not this service's refusal of it: these are the bytes the
		// store held for this document, and no room can serve them. The room fails, so the
		// reader who comes next reloads it (failRoomLoad, which the defer above then finds
		// nothing left to drop).
		if record != nil {
			s.failRoomLoad(room, record, decodeErr)
		} else {
			s.failRoom(room, decodeErr)
		}
		return decodeErr
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
	// returns (sjawhar/ygo v1.55.1-sami.1, provider/websocket/server.go:1896-1986: loadRoom closes
	// the room's ready barrier after it), so nothing writes the tree while this walks it. Its
	// rendering, with the layout recorded with it, is the room's replica's own (keepReplica,
	// below): the replica is the copy this load decoded, which stands at the state rendered here,
	// since nothing from here to OpenUpdateLog hands the document to anything, and a replica the
	// load hands no copy takes this rendering for the base its first run starts from
	// (renderedReplica.start). It is also the room's first measurement, which the peer update
	// hook weighs the room's first update against and the room budget charges the load by.
	var rendering *replicaRendering
	tree, err := treeOf(doc)
	if err == nil {
		rendered := &replicaRendering{}
		rendered.markdown, _, rendered.layout, err = documentRenderingWithLayout(tree)
		if err == nil {
			rendering = rendered
		}
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
	// The room takes peer updates only once this hook returns, and every one of them under the
	// room's scan budget, which keepReplica sets below (newRoomScans): the state the load decoded
	// is the server's own and went in unbounded. ygo set the room's pending cap, maxUpdateItems,
	// before it ran this hook (sjawhar/ygo v1.55.1-sami.1, provider/websocket/server.go:1897-1899),
	// and this hook's own decode of the state ran under it (bootstrapLoadedState), as every peer's
	// apply does (TestAPeerApplyIsHeldToTheRoomsPendingCapAfterTheLoad).
	//
	// The room's scan total starts at what the decode of the state the load read scanned, and the
	// room's replica is the copy that decode made: the decode the load made of its own
	// (LoadDoc, decodeStoredCopy), which this hook's own decode of the same bytes into this
	// document matches item for item - both run with the format cleanup off and under the same
	// pending cap - scanning the same items, and reporting none, since that decode ran before
	// newRoomScans listed the document. Nothing else holds that copy - the preload and the load's
	// record gave it up as it was taken (takePreload, takeLoadedState) - so the replica owns it,
	// and the room's first transaction decodes nothing more (renderedReplica.observe). A room ygo
	// loaded without reading the store through this service's persistence is decoded again here,
	// from its own document's state, under a slot of its own (holdServerLoad), given back once
	// the copy is the replica's.
	stateBytes := int64(len(taken.state))
	if record == nil {
		encoded := crdt.EncodeStateAsUpdateV1(doc, nil)
		release := holdServerLoad(ctx)
		loaded, _ = decodeStoredCopy(encoded, s.maxPendingItems)
		release()
		stateBytes = int64(len(encoded))
	}
	// The log is opened before the observer is registered and before ygo hands the room to anyone
	// (above), so it holds every transaction the room commits from here on, in commit order, and
	// the copy is the room as the log's first transaction finds it, which the rendering above is
	// of.
	replica := s.keepReplica(doc, doc.OpenUpdateLog(), rendering, loaded.doc)
	replica.scans.rescanned(loaded.scans)
	// The room's charge in the room budget is the bytes of the stored state this load decoded,
	// at both copies (roomCopies, roomWeight): the room's own document and the replica's copy
	// were both built from them. From here every committed update's bytes are added to the
	// room's tracked state as the replica drains the room's update log
	// (renderedReplica.chargeAdded), and a compaction that folds the room's whole stored history
	// re-bases it (rebaseRoomState).
	s.chargeLoadedRoom(room, replica, stateBytes)
	instance := s.keepInstance(room, doc)
	replica.classify = func(logged crdt.LoggedUpdate, seq uint64, contentChanged bool, asks func() map[string]struct{}) (bool, bool) {
		return s.classifyDrained(room, instance, logged, seq, contentChanged, asks)
	}
	doc.OnUpdate(func(update []byte, origin any) {
		// A published write's update is already durable. Its suppression slot is finished here,
		// before ygo's persistence observer, which the room registers after OnLoadDocument, hands
		// the update to the room's worker, which waits on that slot (publishLiveUpdate).
		if published, ok := origin.(*liveWriteOrigin); ok {
			s.finishSuppressedPersistence(published.slot, update)
		}
		if repair, identityRepair := origin.(*identityClosureOrigin); identityRepair {
			s.recordSuppressedCommit(repair.slot, update)
		} else if s.beforeObserveUpdate != nil {
			s.beforeObserveUpdate(room)
		}
		// Every observer drains the room's update log, a repair's too, and each committed update
		// is charged and classified once, by whichever drainer reaches it first - an observer, or
		// an exact read (atPoint) - against the room as the commit before it left it
		// (renderedReplica). This observer's own update is in the log - ygo appended it under the
		// document's lock at its commit - so its class is recorded, in the drain and in commit
		// order, before ygo's persistence observer, which runs after this one on this goroutine,
		// hands it on.
		s.observeRoomUpdates(room, replica)
	})
	// A commit that changed nothing the room holds - one whose whole update waits for an edit the
	// room has not received, so it only parked - has no update observer (ygo calls OnUpdate only
	// for a transaction that changes the document, reearth/ygo#350), but ygo logs it, with what it
	// parked (crdt.LoggedUpdate.Parked). ygo calls OnAfterTransaction for every committed
	// transaction, outside the document's lock and before OnUpdate, so the replica is drained here
	// for a transaction that changed no shared type while the log holds something: the replica
	// takes the commit, and what it parked is queued for the store (classifyDrained), whichever
	// path the update reached the room by - a peer's connection, or a published write whose
	// update waits on an item a cold fork read from the store. A transaction that changed a
	// shared type is drained by the update observer above, after the steps it takes first.
	doc.OnAfterTransaction(func(txn *crdt.Transaction) {
		if replica.log.Pending() > 0 && len(txn.ChangedTypes()) == 0 {
			s.observeRoomUpdates(room, replica)
		}
	})
	// A socket admitted while this load ran left its preload after this load's LoadDoc took
	// whatever stood there, and joins the room this load makes, so no load takes it.
	s.dropPreload(room)
	return nil
}

// onPeerUpdate is ygo's Server.OnPeerUpdate, the service's one hook on a browser's update: it
// weighs the update and runs the apply the room hands it (weighPeerUpdate), and then stores what
// that apply parked. ygo hands the room's persistence only what an update integrated, through
// OnUpdate, and nothing at all for one that only parked bytes - it waits for an edit the room
// has not received - so this is the one place the parked bytes are made durable: the drain the
// apply ran (the room's update observer, or its after-transaction drain for a commit that
// changed nothing, onLoadDocument) queued them (classifyDrained), and storeParked appends them
// before ygo answers the peer and before ygo's Shutdown, which waits for this hook, can let the
// process exit. The hook runs on the peer's read loop, which holds no lock of the service's, so
// it can wait for the document's lock the store takes; an OnUpdate observer cannot, since it
// runs on the goroutine that committed, which can be a settlement holding that lock.
//
// An apply can fail after parking bytes, so its error does not skip the durable append; an
// update the weighing refuses never reaches the apply, so it parks nothing and queues nothing.
func (s *Service) onPeerUpdate(info websocket.PeerUpdateInfo, apply func() error) error {
	err := s.weighPeerUpdate(info, apply)
	s.storeParked(info.Room)
	return err
}

// observeRoomUpdates drains replica's log and arms the settlement the drained updates owe. It runs
// from the room's update observer for every commit that changed the document, and from its
// after-transaction drain for one that changed nothing (onLoadDocument). The caller holds no
// replica lock.
func (s *Service) observeRoomUpdates(room string, replica *renderedReplica) {
	settles, changed := replica.observe(room)
	if changed && s.afterCreditUpdate != nil {
		s.afterCreditUpdate(room)
	}
	if settles {
		s.scheduleSettle(room)
	}
}

// storeParked appends, in the order the room committed them, the parked bytes its drains queued
// (classifyDrained), each on its own and gated on the cursor of the room instance that parked
// it (AppendParkedUpdate). It stores them with no class, no credit and no settlement owed: they
// changed nothing the room renders, and the commit that integrates them marks the document owed.
// A load of the stored history parks them again and integrates them once their predecessor
// arrives, which a
// browser that reconnects sends; the commit that integrates them stores them a second time, in
// its own update, which a load applies to no effect, since a struct a document already holds is
// skipped. The bytes are bounded by the room's pending queue (MaxPendingItems), which refuses an
// update that would park past it, and a struct sent again parks only once, so a peer that keeps
// sending what cannot integrate cannot grow the store past that bound per room instance. An
// append that fails fails the room, as any of the room's appends does.
func (s *Service) storeParked(room string) {
	for {
		state := s.lockExistingState(room)
		if state == nil || len(state.parked) == 0 {
			if state != nil {
				s.unlockState(room, state)
			}
			return
		}
		next := state.parked[0]
		state.parked = slices.Delete(state.parked, 0, 1)
		s.unlockState(room, state)
		if s.roomFailed(room) {
			continue
		}
		ctx := withRoomCursor(context.Background(), next.cursor)
		if _, err := s.persistence.AppendParkedUpdate(ctx, room, next.update); err != nil {
			s.failRoom(room, fmt.Errorf("store the parked bytes of a document update: %w", err))
		}
	}
}

// classifyDrained records the class of logged, the update room's log handed over at commit
// position seq in instance, given whether it changed the room's rendered markdown and the ask
// blocks it left when it did (renderedReplica). It is the room's drainClassifier, which runs
// holding the replica's lock. It reports whether a settlement follows the update, which
// is every update but a repair's, and whether it changed the content.
//
// A commit can park bytes - structs or deletions that wait for an edit the room has not received -
// and ygo logs them in Parked, beside an Update that carries only what the commit integrated, the
// bytes OnUpdate hands the room's persistence. So the room's persistence never stores parked bytes,
// and they are queued on the room's record for storeParked to store on their own, outside the
// replica's lock - a drain runs under a settlement's document lock too (treePoint), and the store
// takes that lock. A published live write that parked is already durable, stored by the
// transaction that wrote it, so only another origin's bytes are queued. A commit that only parked
// is logged with an empty update (emptyUpdateV1), and ygo calls no OnUpdate for it, so no
// persistence call will ever take a class for it: none is queued.
//
// The cursor is the instance's, taken here rather than looked up when the update reaches the
// store: a room evicted and reloaded in between is a different instance, which did not take this
// update and must not be recorded as holding the version it is stored at.
func (s *Service) classifyDrained(room string, instance *roomInstance, logged crdt.LoggedUpdate, seq uint64, contentChanged bool, asks func() map[string]struct{}) (settles, changed bool) {
	if _, published := logged.Origin.(*liveWriteOrigin); len(logged.Parked) > 0 && !published {
		state := s.lockState(room)
		state.parked = append(state.parked, parkedBytes{update: logged.Parked, cursor: &instance.cursor})
		s.unlockState(room, state)
	}
	if len(logged.Parked) > 0 && bytes.Equal(logged.Update, emptyUpdateV1) {
		return false, false
	}
	class := documentUpdateClass{seq: seq, contentChanged: contentChanged, cursor: &instance.cursor}
	if _, identityRepair := logged.Origin.(*identityClosureOrigin); identityRepair {
		// A repair's writer appends its update in its own transaction, under the document's
		// advisory lock it holds while the room takes the update, so the class is queued stored:
		// no durable writer of the room stores it again, and it is no work the room's state
		// waits on. It is queued all the same, so the room's persistence, which ygo hands it as
		// any other update, finds it stored when it falls outside its suppression slot
		// (consumeSuppressedPersistence). What it added is charged to the room budget as every
		// transaction's content is, by the drain that handed it over (chargeAdded).
		s.queueUpdateClass(room, logged.Update, class, true)
		return false, false
	}
	if asks != nil {
		s.observeAskBlocksForUpdate(room, asks())
	}
	class.durable = true
	origin, published := logged.Origin.(*liveWriteOrigin)
	if contentChanged {
		class.credit = s.creditContentChange(room, logged.Origin, &instance.hold)
	} else {
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
// forward only. The walk that lists those ask blocks is the asks function the replica's measure
// of the update answers with (changedBy), which takeLocked hands the replica's drainClassifier,
// classifyDrained, the caller here: that measure answers one only for an update it found to be a
// real change, so the no-op path, the common one, pays for neither. A nil askBlocks is that
// walk's answer when it could not read the document, never a document holding no ask block
// (askBlockIDs): the room's record is left as it is.
func (s *Service) observeAskBlocksForUpdate(room string, askBlocks map[string]struct{}) {
	if askBlocks == nil {
		// The walk could not read the document it had to walk, and logged that
		// (renderedReplica.spliceTouched): the room keeps the ask blocks and the authors it
		// recorded, as it does for an update whose own render failed, which is handed no asks
		// at all.
		return
	}
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

// originCreditsNoOne is whether a commit's origin names nobody to credit for the update, the one
// rule both credit paths follow (creditContentChange, unchangedCredit). Three origins credit no
// one. A service repair's origin, the one serviceTransact registered: the service made that
// commit, and a browser that was only connected while it happened is not credited for it. A
// published live write's *liveWriteOrigin: Ledger.Commit credited that write when its transaction
// committed, and it is not credited again. And no origin at all: every commit a loaded room makes
// names an origin - a browser's update names the peer ygo applied it for (its *peer, ygo's
// provider/websocket peer_update.go), a service mutation names the sentinel ygo's Server.Apply
// makes for the call (applyOriginSentinel), which serviceTransact registers as a service origin, a
// repair names its own *identityClosureOrigin (applySuppressed) and a published live write its
// *liveWriteOrigin. The room's own load decodes the stored history with no origin
// (bootstrapLoadedState), earlier in the load hook that goes on to open the update log and
// register this observer, so no observer ever sees it. A nil origin here can therefore only be
// ygo's own cleanup transaction, which a document commits for itself after a remote delete leaves
// a text's formatting markers covering no text (reearth/ygo #284): nobody typed it, so neither
// credit path may name anyone for it. On the replica's recover path, where every update it could
// not place counts as a content change, crediting it would hand every connected browser an edit
// none of them made (INFRA-644);
// on the shadow path, a cleanup the room measured as no change is stored as a change wherever an
// open write's shadow measures one (storedClasses), which would store those browsers as its
// authors.
func (s *Service) originCreditsNoOne(origin any) bool {
	if origin == nil {
		return true
	}
	if _, published := origin.(*liveWriteOrigin); published {
		return true
	}
	_, service := s.serviceOrigins.Load(origin)
	return service
}

// creditContentChange credits an observed content change to its authors, or to no one where its
// origin credits no one (originCreditsNoOne). Any other update is a browser edit by one of the
// peers, which ygo applies while that peer's connection is registered. ygo does not say which
// connection sent it, so every connected peer is credited with it: when exactly one is connected
// it is the latest edit source and replaces `lastActor`, and otherwise the edit cannot be pinned
// on a single peer and no older actor may stand in for it.
//
// The credit goes to F, the room's in-flight credits, until the edit's append lands it
// (AppendUpdateWithCredit): the returned UpdateCredit is how that append reaches it. This takes
// no advisory lock and touches no database: nothing reads F without holding state.mu.
func (s *Service) creditContentChange(room string, origin any, hold *roomHold) *UpdateCredit {
	if s.originCreditsNoOne(origin) {
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
// took it, the authors creditContentChange would credit the same commit to, recorded only while a
// live write is open, whose shadow may measure the update a change once the write is stored before
// it. It is not in F and takes no credit sequence: no version lists an update recorded as no
// change, and its append writes these authors only when it stores the update as a change. It
// weighs the origin by the same rule that path does (originCreditsNoOne), so a service repair, a
// published live write Ledger.Commit credited already and a commit with no origin at all - ygo's
// own cleanup, which nobody typed - carry no authors for a shadow to store them with either. Nil
// too while no write is open.
func (s *Service) unchangedCredit(room string, origin any, hold *roomHold) *UpdateCredit {
	if s.originCreditsNoOne(origin) {
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

// loadDecoded is LoadDoc's own read of room's stored state and its decode
// (loadDecodedHistory), both under slot, the load's (loadedState.slot): the read's fold of a
// history of several updates takes it where it reads the rows (foldRead), and the decode runs
// inside it, as the room's load hook's decode into the room's own document does
// (bootstrapLoadedState). ygo hands LoadDoc no context, so the wait has no deadline and holds
// no slot meanwhile (loadSlot.take), and the slot is the caller's to give back rather than the
// helper's.
//
// A history too large for one update is refused from its header, before the slot is taken and
// before anything is decoded (loadableHistory): no room could take it, and the refusal names the
// item count the header declares rather than whatever a decode of those bytes would say. A
// history ygo refuses the bytes of is not refused here: the state goes on the record with no copy
// decoded from it, and the room's own decode of the same bytes fails the room (onLoadDocument,
// bootstrapLoadedState), so a history that does not decode fails where the room decodes it and
// not one step earlier. The replica of such a room is never made, so its missing copy costs
// nothing.
func (a *servicePersistenceAdapter) loadDecoded(room string, slot *loadSlot) (persistence.LoadResult, decodedState, error) {
	history, err := loadDecodedHistory(slot.carriedBy(context.Background()), a.store, room, a.service.maxPendingItems, loadableHistory)
	if err != nil {
		return persistence.LoadResult{}, decodedState{}, err
	}
	return history.loaded, history.decoded, nil
}
