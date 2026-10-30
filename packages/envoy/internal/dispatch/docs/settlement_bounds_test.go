package docs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A settlement holding one of the settlementWorkers slots takes its transaction's connection from
// the settlements pool, which has one connection for each slot, so no settlement holding a slot waits
// for a connection behind another settlement.
func TestSettlementSlotsMatchTheSettlementsPool(t *testing.T) {
	database := storetest.Open(t)
	settlements, err := database.Pool.Settlements()
	if err != nil {
		t.Fatalf("open the settlements pool: %v", err)
	}
	if size := settlements.Stat().MaxConns(); int(size) != settlementWorkers {
		t.Fatalf("the settlements pool holds %d connections, want one per settlement slot (%d)", size, settlementWorkers)
	}
}

// Twelve documents owing a settlement, all armed at once by the resumption, never run more than
// settlementWorkers settlements at a time; the rest wait for a slot, counted as waiting, and every
// one of them settles.
func TestOwedDocumentsNeverRunMoreSettlementsAtOnceThanThereAreSlots(t *testing.T) {
	service, first := newTestService(t)
	service.settle = time.Hour
	const documents = 12
	rooms := []string{first}
	for number := 2; len(rooms) < documents; number++ {
		rooms = append(rooms, createIssueDocument(t, service.store, number, "# Document"))
	}
	for index, room := range rooms {
		seedServiceText(t, service, room, fmt.Sprintf("Owed %d.\n", index))
	}
	if _, err := service.store.Pool.Exec(context.Background(), `
		insert into doc_settlements_pending (artifact_id) select unnest($1::uuid[]) on conflict (artifact_id) do nothing
	`, rooms); err != nil {
		t.Fatalf("mark every document owed: %v", err)
	}

	var running, highest, waited, entered atomic.Int64
	service.afterSettleWarm = func(string) {
		now := running.Add(1)
		for seen := highest.Load(); now > seen && !highest.CompareAndSwap(seen, now); seen = highest.Load() {
		}
		if load := service.SettlementLoad(); load.Waiting > waited.Load() {
			waited.Store(load.Waiting)
		}
		entered.Add(1)
		// Long enough that every slot is taken while the first settlements hold theirs.
		time.Sleep(200 * time.Millisecond)
		running.Add(-1)
	}
	service.settle = 10 * time.Millisecond
	if err := service.resumeOwedSettlements(context.Background(), 0); err != nil {
		t.Fatalf("resume the owed settlements: %v", err)
	}
	waitFor(t, 60*time.Second, "every owed document to settle", func() bool {
		return pendingSettlements(t, service) == 0
	})
	waitFor(t, 10*time.Second, "every settlement to give its slot back", func() bool {
		return service.SettlementLoad() == SettlementLoad{}
	})
	service.afterSettleWarm = nil

	if got := highest.Load(); got != settlementWorkers {
		t.Fatalf("at most %d settlements ran at once, want exactly %d: never more than the slots, and every slot used", got, settlementWorkers)
	}
	if entered.Load() < documents {
		t.Fatalf("%d settlements took a slot, want one for each of the %d documents", entered.Load(), documents)
	}
	if waited.Load() == 0 {
		t.Fatal("no settlement was counted waiting for a slot while every slot was held")
	}
}

// A settlement that cannot finish by its deadline - here another transaction holds the document's
// owner row past it - rolls back: the document stays owed, its room stays resident, and the
// deadline counts as no settlement failure however often it recurs, so overload fails no room. The
// resumption then settles the document.
func TestASettlementPastItsDeadlineRollsBackStaysOwedAndCountsNoFailure(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	versions := latestVersionNumber(t, service, artifactID)

	ctx := context.Background()
	hold, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the owner row's holder: %v", err)
	}
	defer hold.Rollback(ctx)
	if _, err := hold.Exec(ctx, `select key from issues where key = 'DOC-1' for no key update`); err != nil {
		t.Fatalf("hold the document's owner row: %v", err)
	}
	const deadline = 300 * time.Millisecond
	service.settleTimeout = deadline
	for attempt := range maxSettleFailures + 1 {
		started := time.Now()
		settleCurrentGeneration(t, service, artifactID)
		if took := time.Since(started); took > deadline+5*time.Second {
			t.Fatalf("attempt %d: the settlement returned after %v, want soon after its %v deadline", attempt, took, deadline)
		}
		owes, err := settlementPending(ctx, service.store.Pool, artifactID)
		if err != nil {
			t.Fatalf("read the document's pending settlement: %v", err)
		}
		if !owes {
			t.Fatalf("attempt %d: the document's pending-settlement row is gone, want it kept for the resumption", attempt)
		}
		if service.srv.GetDoc(artifactID) == nil {
			t.Fatalf("attempt %d: the document's room is gone, want it resident", attempt)
		}
		state := service.room(artifactID)
		state.mu.Lock()
		failures, failed := state.settleFailures, state.failed
		state.mu.Unlock()
		if failures != 0 || failed != nil {
			t.Fatalf("attempt %d: settlement failures = %d, room failure = %v; want none for a deadline", attempt, failures, failed)
		}
	}
	if latest := latestVersionNumber(t, service, artifactID); latest != versions {
		t.Fatalf("latest version = %d, want %d: no settlement past its deadline commits", latest, versions)
	}

	if err := hold.Rollback(ctx); err != nil {
		t.Fatalf("release the owner row: %v", err)
	}
	service.settleTimeout = settlementTimeout
	service.settle = 10 * time.Millisecond
	if err := service.resumeOwedSettlements(ctx, 0); err != nil {
		t.Fatalf("resume the owed settlement: %v", err)
	}
	waitForDocumentVersion(t, service.store, artifactID, versions+1)
	requireLatestVersionMarkdown(t, service, artifactID, "after\n")
}

// A settlement whose transaction finds every connection of the settlements pool taken for the
// pool's whole acquire bound is refused it (store.ErrPoolBusy) and rolls back: the document stays
// owed, its room stays resident, and the refusal counts as no settlement failure however often it
// recurs, so overload fails no room. Once a connection is free the resumption settles it.
func TestASettlementRefusedAConnectionStaysOwedAndCountsNoFailure(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	service.store.Pool.SetAcquireBoundForTest(store.SettlementsPool, 200*time.Millisecond)
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	versions := latestVersionNumber(t, service, artifactID)

	settlements, err := service.store.Pool.Settlements()
	if err != nil {
		t.Fatalf("open the settlements pool: %v", err)
	}
	ctx := context.Background()
	var held []*pgxpool.Conn
	release := func() {
		for _, conn := range held {
			conn.Release()
		}
		held = nil
	}
	defer release()
	for range settlements.Stat().MaxConns() {
		conn, err := settlements.Acquire(ctx)
		if err != nil {
			t.Fatalf("hold a settlements connection: %v", err)
		}
		held = append(held, conn)
	}
	for attempt := range maxSettleFailures + 1 {
		settleCurrentGeneration(t, service, artifactID)
		owes, err := settlementPending(ctx, service.store.Pool, artifactID)
		if err != nil {
			t.Fatalf("read the document's pending settlement: %v", err)
		}
		if !owes {
			t.Fatalf("attempt %d: the document's pending-settlement row is gone, want it kept for the resumption", attempt)
		}
		if service.srv.GetDoc(artifactID) == nil {
			t.Fatalf("attempt %d: the document's room is gone, want it resident", attempt)
		}
		state := service.room(artifactID)
		state.mu.Lock()
		failures, failed := state.settleFailures, state.failed
		state.mu.Unlock()
		if failures != 0 || failed != nil {
			t.Fatalf("attempt %d: settlement failures = %d, room failure = %v; want none for a busy pool", attempt, failures, failed)
		}
	}
	if timeouts := settlementTimeouts(service); timeouts < maxSettleFailures+1 {
		t.Fatalf("the settlements pool refused %d acquires, want one for each of the %d settlements", timeouts, maxSettleFailures+1)
	}
	if latest := latestVersionNumber(t, service, artifactID); latest != versions {
		t.Fatalf("latest version = %d, want %d: no settlement refused a connection commits", latest, versions)
	}

	release()
	service.settle = 10 * time.Millisecond
	if err := service.resumeOwedSettlements(ctx, 0); err != nil {
		t.Fatalf("resume the owed settlement: %v", err)
	}
	waitForDocumentVersion(t, service.store, artifactID, versions+1)
	requireLatestVersionMarkdown(t, service, artifactID, "after\n")
}

// settlementTimeouts is how many acquires the settlements pool has refused at its bound.
func settlementTimeouts(service *Service) uint64 {
	for _, pool := range service.store.Pool.Stats() {
		if pool.Name == store.SettlementsPool {
			return pool.Acquire.Timeouts
		}
	}
	return 0
}

// A settlement that cannot open the settlements pool stops short before it warms its cold room.
// Every read a settlement makes goes through that pool - one through the request pool would take a
// connection admission counts for requests - so a settlement that has no settlements pool warms
// nothing, takes no request connection, and reports the pool's error with the document left owed.
func TestASettlementWithoutItsPoolWarmsNoColdRoomThroughTheRequestPool(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	ctx := context.Background()
	seedServiceText(t, service, artifactID, "Cold.\n")
	settleCurrentGeneration(t, service, artifactID)
	editLiveTree(t, service, artifactID, replaceRun("Cold.", "Cold again."))
	settleCurrentGeneration(t, service, artifactID)
	// The document's room, left cold with a settlement owed: the settlement below has to warm it
	// before it can read anything.
	if err := service.srv.CloseRoom(artifactID, true); err != nil {
		t.Fatalf("close the document's room: %v", err)
	}
	waitForNoLiveDocument(t, service, artifactID)
	if _, err := service.store.Pool.Exec(ctx, `
		insert into doc_settlements_pending (artifact_id) values ($1) on conflict (artifact_id) do nothing
	`, artifactID); err != nil {
		t.Fatalf("mark the cold document owed: %v", err)
	}
	state := service.room(artifactID)
	state.mu.Lock()
	generation := state.roomGeneration
	state.mu.Unlock()
	refused := errors.New("test: the settlements pool cannot open")
	service.store.Pool.SetOpenFailureForTest(store.SettlementsPool, refused)

	// What the request pool has handed out, before and after: the settlement must take none of it,
	// neither for the room's load nor for the issue read its warm-up's injection makes.
	before := service.store.Pool.Stat().AcquireCount()
	outcome := service.settleRoomWithin(ctx, artifactID, generation)

	if outcome.next != settleRetry || !errors.Is(outcome.err, refused) {
		t.Fatalf("the settlement ended as %+v, want a stop short carrying the settlements pool's own error", outcome)
	}
	if took := service.store.Pool.Stat().AcquireCount() - before; took != 0 {
		t.Fatalf("the settlement took %d request-pool connection(s), want none: settlement work reads through the settlements pool", took)
	}
	if service.srv.GetDoc(artifactID) != nil {
		t.Fatal("the settlement warmed the cold room, want it left cold: it has no pool to warm it through")
	}
	owes, err := settlementPending(ctx, service.store.Pool, artifactID)
	if err != nil {
		t.Fatalf("read the document's pending settlement: %v", err)
	}
	if !owes {
		t.Fatal("the document's pending-settlement row is gone, want it kept for the resumption")
	}
}

// A settlement whose deadline passes once it has written into its room finishes what it started.
// The room and its browsers hold the repair, so failing the room there would close every editor's
// socket and reload the document over work the settlement had all but committed. The room stays
// resident with its editor connected and counts no failure, the document stays owed rather than
// failed, and the settlement after it writes the version.
func TestASettlementPastItsDeadlineAfterWritingIntoTheRoomKeepsTheRoomResident(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"open\"}\nShip it?\n:::\n\nContext before.\n")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	editor := connectPeer(t, httpServer.URL, artifactID, "alice")
	settleCurrentGeneration(t, service, artifactID)
	answer := answerBlockAsk(t, service, artifactID)
	editAsPeer(t, service, artifactID, replaceRun("Context before.", "Context after."))
	versions := latestVersionNumber(t, service, artifactID)
	room := service.srv.GetDoc(artifactID)
	if room == nil {
		t.Fatal("the document's room is not resident")
	}
	// The first settlement stamped every block id, so the one write the held settlement makes into
	// the room is the repair of the ask block answered above.
	tree, err := service.liveTree(artifactID, room)
	if err != nil {
		t.Fatalf("read the room's tree: %v", err)
	}
	if unstamped := pmdoc.BlockIDRepairCount(tree); unstamped != 0 {
		t.Fatalf("%d block(s) still want an id, so the held settlement would stamp before it repairs", unstamped)
	}

	const deadline = 2 * time.Second
	service.settleTimeout = deadline
	// When the settlement's deadline has certainly passed: its context is made just before the
	// settlement starts, so a hold until here outlives it.
	var passed atomic.Int64
	service.beforeSettleStart = func(room string) {
		if room == artifactID {
			passed.Store(time.Now().Add(deadline + 100*time.Millisecond).UnixNano())
		}
	}
	var held atomic.Bool
	service.afterSettleRepairAppend = func(room string) {
		if room != artifactID || !held.CompareAndSwap(false, true) {
			return
		}
		// The repair is in the room and its browsers, and appended; hold the pass here until the
		// settlement's deadline has passed, so what it has left to do meets a deadline gone.
		for until := time.Unix(0, passed.Load()); time.Now().Before(until); {
			time.Sleep(5 * time.Millisecond)
		}
	}
	settleCurrentGeneration(t, service, artifactID)
	service.beforeSettleStart, service.afterSettleRepairAppend = nil, nil
	service.settleTimeout = settlementTimeout

	if !held.Load() {
		t.Fatal("the settlement wrote no repair into the room, so its deadline never passed after a write")
	}
	state := service.room(artifactID)
	state.mu.Lock()
	failed, failures := state.failed, state.settleFailures
	state.mu.Unlock()
	if failed != nil || failures != 0 {
		t.Fatalf("the room failed with %v and counted %d settlement failure(s), want neither: the deadline passed after the settlement had written into the room", failed, failures)
	}
	if resident := service.srv.GetDoc(artifactID); resident != room {
		t.Fatalf("the room instance is %p, want the resident %p: the settlement evicted the room it wrote into", resident, room)
	}
	select {
	case <-editor.Ended:
		t.Fatal("the editor's socket was closed, want it open: the room it edits never failed")
	default:
	}
	owes, err := settlementPending(context.Background(), service.store.Pool, artifactID)
	if err != nil {
		t.Fatalf("read the document's pending settlement: %v", err)
	}
	if !owes {
		t.Fatal("the document's pending-settlement row is gone, want it kept: the settlement ended at its deadline before its version")
	}

	// The repair the held settlement committed is in the store, and the settlement after it writes
	// the version that holds it.
	settleCurrentGeneration(t, service, artifactID)
	want := ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"answered\" answered_by=\"alice\" answered_at=\"" +
		answer.At.Format(time.RFC3339Nano) + "\" selected=\"[]\"}\nShip it?\n:::\n\nContext after.\n"
	requireLatestVersionMarkdown(t, service, artifactID, want)
	if latest := latestVersionNumber(t, service, artifactID); latest != versions+1 {
		t.Fatalf("latest version = %d, want %d", latest, versions+1)
	}
	requireNoSuppressedSlots(t, service, artifactID, "after the settlement held past its deadline")
}

// A settlement renders its document before it locks the document's owner row: while its render is
// held for a second, another connection takes that row at once.
func TestASettlementRendersWithoutHoldingItsDocumentsOwnerRow(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	versions := latestVersionNumber(t, service, artifactID)

	rendering := make(chan struct{})
	var once sync.Once
	service.beforeSettleRender = func(room string) {
		if room == artifactID {
			once.Do(func() {
				close(rendering)
				time.Sleep(time.Second)
			})
		}
	}
	generation := roomGeneration(t, service, artifactID)
	settled := make(chan struct{})
	go func() {
		defer close(settled)
		service.settleRoom(artifactID, generation)
	}()
	select {
	case <-rendering:
	case <-time.After(10 * time.Second):
		t.Fatal("the settlement never reached its render")
	}
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	var key string
	err = tx.QueryRow(ctx, `select key from issues where key = 'DOC-1' for no key update nowait`).Scan(&key)
	_ = tx.Rollback(ctx)
	if err != nil {
		t.Fatalf("lock the owner row while the settlement renders: %v, want it free", err)
	}
	select {
	case <-settled:
	case <-time.After(30 * time.Second):
		t.Fatal("the settlement never returned")
	}
	service.beforeSettleRender = nil
	if latest := latestVersionNumber(t, service, artifactID); latest != versions+1 {
		t.Fatalf("latest version = %d, want %d", latest, versions+1)
	}
	requireLatestVersionMarkdown(t, service, artifactID, "after\n")
}

// A settlement that stamps block ids and repairs an ask block renders nothing under its document's
// owner row either: while each of its renders - the live tree each pass read, the stamped copy, the
// planned repairs - is held for a moment, another connection takes that row at once. The document
// still gets its stamp, its repair and its version.
func TestARepairingSettlementRendersNothingUnderItsDocumentsOwnerRow(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"open\"}\nShip it?\n:::\n\nContext before.\n")
	service.settleRoom(artifactID, 0)
	answer := answerBlockAsk(t, service, artifactID)
	editLiveTree(t, service, artifactID, appendUnidentifiedBlocks(t, "Added."))
	versions := latestVersionNumber(t, service, artifactID)

	var renders atomic.Int64
	service.beforeSettleRender = func(room string) {
		if room != artifactID {
			return
		}
		renders.Add(1)
		ctx := context.Background()
		tx, err := service.store.Pool.Begin(ctx)
		if err != nil {
			t.Errorf("begin: %v", err)
			return
		}
		defer tx.Rollback(ctx)
		var key string
		if err := tx.QueryRow(ctx, `select key from issues where key = 'DOC-1' for no key update nowait`).Scan(&key); err != nil {
			t.Errorf("render %d: lock the owner row while the settlement renders: %v, want it free", renders.Load(), err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	settleCurrentGeneration(t, service, artifactID)
	service.beforeSettleRender = nil
	// The live tree, its stamped copy, the stamped tree, the planned repairs, the tree they are
	// committed from and the repaired tree versioned.
	if got := renders.Load(); got < 6 {
		t.Fatalf("the settlement rendered %d times, want a render for each of its stamp, plan, repairs and version", got)
	}
	want := ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"answered\" answered_by=\"alice\" answered_at=\"" +
		answer.At.Format(time.RFC3339Nano) + "\" selected=\"[]\"}\nShip it?\n:::\n\nContext before.\n\nAdded.\n"
	requireLatestVersionMarkdown(t, service, artifactID, want)
	if latest := latestVersionNumber(t, service, artifactID); latest != versions+1 {
		t.Fatalf("latest version = %d, want %d", latest, versions+1)
	}
	requireNoSuppressedSlots(t, service, artifactID, "after the settlement")
}

// An update that becomes durable while a settlement renders is missing from the tree it renders.
// The settlement writes no version of that tree, whose cursor would claim the update: under its
// locks it finds the room past where it stood when it copied the tree, rolls back, and versions the
// document read again, the update included.
func TestAnUpdateLandingDuringTheRenderProducesNoVersionOfTheStaleTree(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)
	editAsPeer(t, service, artifactID, replaceRun("First.", "First, edited."))
	before := latestVersionNumber(t, service, artifactID)

	var landed atomic.Bool
	service.beforeSettleRender = func(room string) {
		if room != artifactID || !landed.CompareAndSwap(false, true) {
			return
		}
		editAsPeer(t, service, artifactID, replaceRun("Second.", "Second, edited."))
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := service.waitForPendingUpdates(ctx, artifactID); err != nil {
			t.Errorf("wait for the update to reach persistence: %v", err)
		}
		if err := service.waitForDurableAppends(ctx, artifactID); err != nil {
			t.Errorf("wait for the update to become durable: %v", err)
		}
	}
	settleCurrentGeneration(t, service, artifactID)
	service.beforeSettleRender = nil
	if !landed.Load() {
		t.Fatal("the settlement never reached its render")
	}
	var stale int
	if err := service.store.Pool.QueryRow(context.Background(), `
		select count(*) from artifact_versions where artifact_id = $1 and number > $2 and markdown = $3
	`, artifactID, before, "First, edited.\n\nSecond.\n").Scan(&stale); err != nil {
		t.Fatalf("count versions of the stale tree: %v", err)
	}
	if stale != 0 {
		t.Fatalf("%d version(s) of the tree rendered without the update that landed during its render", stale)
	}
	requireLatestVersionMarkdown(t, service, artifactID, "First, edited.\n\nSecond, edited.\n")
	if latest := latestVersionNumber(t, service, artifactID); latest != before+1 {
		t.Fatalf("latest version = %d, want %d", latest, before+1)
	}
	if markdown, cursor := latestVersionOf(t, service.store, artifactID); cursor != storedHead(t, service.store, artifactID) {
		t.Fatalf("version %q records update cursor %d, want the head %d", markdown, cursor, storedHead(t, service.store, artifactID))
	}
}

// A document whose settlement repairs it - an ask block whose row was answered - gets the repair in
// one commit, with no version, and its version in the next: when the last pass renders the document,
// the repair is stored and the latest version is still the one before it. The version holds the
// repair and the edit that owed it, records the repair's update in its cursor, and the repair's
// event names that version.
func TestADocumentNeedingRepairsGetsThemInOneCommitAndItsVersionInTheNext(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"open\"}\nShip it?\n:::\n\nContext before.\n")
	service.settleRoom(artifactID, 0)
	answer := answerBlockAsk(t, service, artifactID)
	editAsPeer(t, service, artifactID, replaceRun("Context before.", "Context after."))
	generation := roomGeneration(t, service, artifactID)
	before := latestVersionNumber(t, service, artifactID)
	headBefore := storedHead(t, service.store, artifactID)

	// What the database holds as each render starts; the last is the versioning pass's.
	var renders, headAtLast, versionAtLast atomic.Int64
	service.beforeSettleRender = func(room string) {
		if room != artifactID {
			return
		}
		renders.Add(1)
		headAtLast.Store(storedHead(t, service.store, artifactID))
		versionAtLast.Store(int64(latestVersionNumber(t, service, artifactID)))
	}
	service.settleRoom(artifactID, generation)
	service.beforeSettleRender = nil
	if renders.Load() < 3 {
		t.Fatalf("the settlement rendered the document %d time(s), want its repairs planned and committed and its version written by passes of their own", renders.Load())
	}
	if headAtLast.Load() <= headBefore {
		t.Fatalf("when the version's pass rendered, the head stood at %d, want the repair stored past %d", headAtLast.Load(), headBefore)
	}
	if versionAtLast.Load() != int64(before) {
		t.Fatalf("when the version's pass rendered, the latest version was %d, want still %d: the repair commits no version", versionAtLast.Load(), before)
	}
	want := ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"answered\" answered_by=\"alice\" answered_at=\"" +
		answer.At.Format(time.RFC3339Nano) + "\" selected=\"[]\"}\nShip it?\n:::\n\nContext after.\n"
	requireLatestVersionMarkdown(t, service, artifactID, want)
	if latest := latestVersionNumber(t, service, artifactID); latest != before+1 {
		t.Fatalf("latest version = %d, want %d", latest, before+1)
	}
	if _, cursor := latestVersionOf(t, service.store, artifactID); cursor != headAtLast.Load() {
		t.Fatalf("the version records update cursor %d, want the repair's %d", cursor, headAtLast.Load())
	}
	var named string
	if err := service.store.Pool.QueryRow(context.Background(), `
		select payload->>'version' from events
		where type = 'block.repaired' and payload->>'block_id' = 'ask-block' order by id desc limit 1
	`).Scan(&named); err != nil {
		t.Fatalf("read the repair's event: %v", err)
	}
	if named != strconv.Itoa(before+1) {
		t.Fatalf("the repair's event names version %s, want %d, the version that holds it", named, before+1)
	}
}
