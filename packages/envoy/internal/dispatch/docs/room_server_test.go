package docs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A room's close gate leaves with the last close or repair that held it: the server keeps none
// for a room nothing is closing or repairing, however many documents were repaired, evicted, or
// closed with their issue without ever loading.
func TestCloseGatesDoNotOutliveTheirRooms(t *testing.T) {
	service, first := newTestService(t)
	service.settle = time.Hour
	ctx := context.Background()
	generation := owedStamp(t, service, first)
	service.settleRoom(first, generation) // a repair (applySuppressed)
	if err := service.srv.CloseRoom(first, true); err != nil {
		t.Fatalf("close the settled room: %v", err)
	}
	var evicted []string
	for number := 2; number <= 26; number++ { // rooms the service evicts
		room := createIssueDocument(t, service.store, number, "before")
		seedServiceText(t, service, room, "before")
		liveTree(t, service, room)
		if service.srv.GetDoc(room) == nil {
			t.Fatalf("document %d is not resident after its live read", number)
		}
		if err := service.Evict(ctx, room); err != nil {
			t.Fatalf("evict document %d: %v", number, err)
		}
		evicted = append(evicted, room)
	}
	for number := 27; number <= 51; number++ { // closed issues, documents never loaded
		createIssueDocument(t, service.store, number, "before")
		service.SetIssueClosed(ctx, fmt.Sprintf("DOC-%d", number), true)
	}
	live := 0
	for _, room := range append([]string{first}, evicted...) {
		if service.srv.GetDoc(room) != nil {
			live++
		}
	}
	if gates := closeGates(service); gates > live {
		t.Fatalf("%d close gates kept for %d live rooms after 51 documents were repaired, evicted or closed", gates, live)
	}
}

// A repair that meets a close of its room under way writes nothing and returns rather than waiting
// for the close: the close waits for repairs already committing, and a settlement writing a repair
// holds the document's lock, which a failed room's close compacts under (roomServer).
func TestARepairRefusesRatherThanWaitsForACloseUnderWay(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	generation := owedStamp(t, service, artifactID)

	closing := service.srv.acquireGate(artifactID)
	closing.Lock()
	released := false
	release := func() {
		if !released {
			released = true
			closing.Unlock()
			service.srv.releaseGate(artifactID, closing)
		}
	}
	t.Cleanup(release)
	settled := make(chan struct{})
	go func() {
		defer close(settled)
		service.settleRoom(artifactID, generation)
	}()
	select {
	case <-settled:
	case <-time.After(10 * time.Second):
		release()
		t.Fatal("the settlement waited for the close under way instead of refusing its repair")
	}
	release()
	if repairs := pmdoc.BlockIDRepairCount(liveTree(t, service, artifactID)); repairs == 0 {
		t.Fatal("the refused repair stamped the room")
	}
	requireNoSuppressedSlots(t, service, artifactID, "after the refused repair")
	requireNextSettlementStamps(t, service, artifactID, "before\n\nadded\n")
	if gates := closeGates(service); gates != 0 {
		t.Fatalf("%d close gates kept once nothing closes or repairs the room", gates)
	}
}

// A repair holds the exact document Server.Apply found. Once its room has retired that document,
// holdOpen refuses and releases the gate it acquired for the check.
func TestHoldOpenRefusesRoomThatNoLongerHoldsItsDocument(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	owedStamp(t, service, artifactID)
	doc := service.srv.GetDoc(artifactID)
	if doc == nil {
		t.Fatal("document room is not resident")
	}
	if err := service.srv.CloseRoom(artifactID, true); err != nil {
		t.Fatalf("close document room: %v", err)
	}
	if release, open := service.srv.holdOpen(artifactID, doc); open {
		release()
		t.Fatal("a repair was held open on a document its room no longer holds")
	}
	if gates := closeGates(service); gates != 0 {
		t.Fatalf("%d close gates kept after the refusal", gates)
	}
}

// The room budget's sweep leaves a room an Apply is inside its callback on, and closes it once
// that Apply has returned. ygo's Apply holds the room's in-flight count "to protect rm from a
// concurrent eviction while in use" (sjawhar/ygo v1.55.1-sami.1, provider/websocket/inject.go:337-340),
// its idle sweep honours that count, and its CloseRoom answers websocket.ErrRoomInUse rather than
// taking the room out from under the apply (reearth/ygo#342). The budget's close takes the room's
// applies hold before it tries at all (closeUnused), so it leaves the room without an error and
// without touching the room's record, and nothing of the apply's write is lost: a decision whose
// body a browser broke would otherwise read as the body it replaced, and an ask edit of it would
// be made on that body rather than refused.
func TestTheBudgetSweepLeavesARoomAnApplyIsWriting(t *testing.T) {
	service, _ := newTestService(t)
	service.settle = time.Hour
	ctx := context.Background()
	artifactID := createIssueDocument(t, service.store, 2, "# First")
	seedServiceText(t, service, artifactID, ":::ask{#ask-lists urgency=\"med\" multiple=\"false\" state=\"open\"}\nShould we ship?\n\n- Ship: Release it\n- Hold: Wait for review\n:::\n")
	settleCurrentGeneration(t, service, artifactID)
	// Every charged room is past a budget of one byte, so the sweep closes whatever it may.
	service.SetRoomBudgetForTest(1)

	// A decision body the content rule refuses: a list where the question's paragraph must be.
	broken := []*pmdoc.Node{askBodyBlock(t, "L", 1), askBodyBlock(t, "P", 2)}
	err := service.srv.Apply(ctx, artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		fragment := doc.GetXmlFragment(fragmentName)
		tree, readErr := pmdoc.Read(fragment)
		if readErr != nil {
			t.Fatalf("read the live tree: %v", readErr)
		}
		firstAsk(tree).Children = broken
		// The sweep runs on a goroutine of its own (roomBudget.startSweepLocked); run here, it
		// meets this apply exactly as that goroutine can.
		service.sweepRooms()
		if service.srv.GetDoc(artifactID) == nil {
			t.Fatal("the budget's sweep closed the room while an apply was inside its callback on it")
		}
		transact(func(txn *crdt.Transaction) {
			if updateErr := pmdoc.Update(txn, fragment, tree); updateErr != nil {
				t.Errorf("write the broken decision body: %v", updateErr)
			}
		})
	})
	if err != nil {
		t.Fatalf("apply the browser's edit: %v", err)
	}
	// Nothing of the write is lost to the sweep that gave way: the room keeps it, the room's
	// persistence takes it, and the close the budget makes once the room is unused flushes it.
	settleCurrentGeneration(t, service, artifactID)
	evicted, _ := service.invalidateForEviction(artifactID)
	if closed, err := service.evictUnusedRoom(artifactID, evicted); !closed || err != nil {
		t.Fatalf("the budget's close of the room once the apply had returned = %t, %v, want it closed", closed, err)
	}

	// The room the next read loads holds what the apply wrote, so the decision still breaks its
	// content rule and the ask edit of it is refused rather than made on the body it replaced.
	if kinds := len(firstAsk(liveTree(t, service, artifactID)).Children); kinds != len(broken) {
		t.Fatalf("the reloaded decision holds %d blocks, want the %d the apply wrote", kinds, len(broken))
	}
	var unrepresentable *ErrAskBlockUnrepresentable
	if askErr := sendAskOptions(service, artifactID, AskBlockEdit{Urgency: new("high")}); !errors.As(askErr, &unrepresentable) {
		t.Fatalf("ask edit of the broken body = %v, want ErrAskBlockUnrepresentable", askErr)
	}
}

// The rebuild's close of the room whose history it replaces gives way to a Server.Apply inside
// its callback on the room, and closes it once that Apply has returned (evictBeforeRebuild). The
// close must not land under that apply: the apply's commit would reach the store only through the
// room's retiring persistence worker, landing before RebuildTx, where it is deleted with the
// history the rebuild replaces, or after it, where it puts the replaced history's structs on top
// of the rebuilt seed. ygo refuses such a close itself (websocket.ErrRoomInUse, reearth/ygo#342),
// and the guard the budget's closes take refuses it before it is tried, rather than waiting for
// the apply as Evict's own close does (roomServer.closeWhenFree): a refused close leaves the
// room's record as it found it, its armed settlement included, since the rebuild it refuses goes
// no further.
func TestTheRebuildsCloseLeavesARoomAnApplyIsWriting(t *testing.T) {
	service, _ := newTestService(t)
	ctx := context.Background()
	artifactID := createIssueDocument(t, service.store, 2, "# First")
	seedServiceText(t, service, artifactID, "before\n")
	settleCurrentGeneration(t, service, artifactID)

	generation := roomGeneration(t, service, artifactID)
	err := service.srv.Apply(ctx, artifactID, func(*crdt.Doc, func(func(*crdt.Transaction))) {
		closed, closeErr := service.evictBeforeRebuild(artifactID)
		if closed || closeErr != nil {
			t.Errorf("the rebuild's close while an apply was inside its callback = %t, %v, want it refused", closed, closeErr)
		}
		if service.srv.GetDoc(artifactID) == nil {
			t.Error("the rebuild's close closed the room while an apply was inside its callback on it")
		}
	})
	if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
		t.Fatalf("apply on the room: %v", err)
	}
	if armed := roomGeneration(t, service, artifactID); armed != generation {
		t.Fatalf("the refused close moved the room's generation from %d to %d, want the record left as it was", generation, armed)
	}
	if closed, err := service.evictBeforeRebuild(artifactID); !closed || err != nil {
		t.Fatalf("the rebuild's close once the apply had returned = %t, %v, want it closed", closed, err)
	}
	if service.srv.GetDoc(artifactID) != nil {
		t.Fatal("the rebuild's close left the room live")
	}
}

// settleAfterRefusedClose is the settle delay the test below leaves its service at once its
// room's settlement is armed: short enough that the settlement a refused close arms again runs
// while the test waits for the version it writes.
const settleAfterRefusedClose = 20 * time.Millisecond

// A close that ends what the room's state has armed before it closes the room, and then does not
// close it, has to leave the live room able to version its edits again. Both closes that
// invalidate a room's state ahead of the close - the rebuild's, which ygo refuses while any
// caller is inside the room (websocket.ErrRoomInUse, reearth/ygo#342), and Evict's, whose
// caller's context can end while it waits for that caller to leave (roomServer.closeWhenFree) -
// can leave the room live. The invalidation runs first because a settlement that ran against a
// room already closing would write a version of a document the rebuild is about to replace, so
// what it ended it owes: the moved generation ends the settlement whose timer had fired before
// it opens its transaction (settleRoom), the timer it took was the one still armed, and nothing
// arms another - the room's last browser leaving settles only a room whose timer it can take
// (settleLastPeer), so the room's edits would stay unversioned until the next edit. The refused
// close arms that settlement again, at the generation the invalidation moved to: that ends only
// the settlements that read the generation before it, and the one armed here reads the new one.
func TestARefusedCloseArmsTheSettlementItTook(t *testing.T) {
	for _, test := range []struct {
		name  string
		close func(*testing.T, *Service, string)
	}{
		{"the rebuild's, which ygo refuses while a caller is inside the room", func(t *testing.T, service *Service, artifactID string) {
			if closed, err := service.evictBeforeRebuild(artifactID); closed || err != nil {
				t.Fatalf("the rebuild's close of a room ygo says is in use = %t, %v, want it refused with no error", closed, err)
			}
		}},
		{"an eviction whose caller's context ends while it waits for that caller to leave", func(t *testing.T, service *Service, artifactID string) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer cancel()
			if err := service.Evict(ctx, artifactID); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("the eviction of a room a caller is inside, with a context that ends first = %v, want %v", err, context.DeadlineExceeded)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			// An hour until the room's settlement is armed, so the edit's own settlement cannot
			// run before the close takes it over.
			service.settle = time.Hour
			seedServiceText(t, service, artifactID, "before\n")
			settleCurrentGeneration(t, service, artifactID)
			settled, _, _ := latestDocumentVersion(t, service.store, artifactID)
			editLiveTree(t, service, artifactID, replaceRun("before", "after"))

			state := service.room(artifactID)
			state.mu.Lock()
			armed := service.isSettleTimerArmed(state.settle)
			generation := state.roomGeneration
			// Written under the state's lock, which every arming of a settlement holds while it
			// reads the delay (scheduleSettleLocked), so no settlement of the room's reads it as
			// it changes.
			service.settle = settleAfterRefusedClose
			state.mu.Unlock()
			if !armed {
				t.Fatal("the edit armed no settlement, so nothing here is tested")
			}

			letGo := holdRoomThroughYgosApply(t, service.srv.Server, artifactID)
			test.close(t, service, artifactID)
			if service.srv.GetDoc(artifactID) == nil {
				t.Fatal("the close closed the room while a caller was inside it")
			}
			state.mu.Lock()
			armed = service.isSettleTimerArmed(state.settle)
			moved := state.roomGeneration != generation
			state.mu.Unlock()
			if !moved {
				t.Fatal("the close left the room's generation where it was, so its invalidation never ran and nothing here is tested")
			}
			if !armed {
				t.Fatal("the close that closed nothing left the live room with no settlement armed: its edits stay unversioned until something else arms one")
			}

			// The caller leaves, and the settlement the close armed again versions the edit the
			// room still holds.
			letGo()
			waitForDocumentVersion(t, service.store, artifactID, settled+1)
			number, markdown, _ := latestDocumentVersion(t, service.store, artifactID)
			if number != settled+1 || !strings.Contains(markdown, "after") {
				t.Fatalf("the document's latest version is %d, %q, want version %d holding the edit the room kept", number, markdown, settled+1)
			}
		})
	}
}

// holdRoomThroughYgosApply holds room open from inside ygo's own Server.Apply, as a browser
// joining or a relay's Inject holds it: the service's close gate knows nothing of this caller,
// so a close of the service's takes its applies hold and runs its before, and ygo is what
// refuses the close (websocket.ErrRoomInUse). The hold ends with the returned release, which
// waits for the Apply to return, and with the test if it never calls it.
func holdRoomThroughYgosApply(t *testing.T, server *websocket.Server, room string) func() {
	t.Helper()
	entered, release, applied := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var released sync.Once
	letGo := func() {
		released.Do(func() { close(release) })
		<-applied
	}
	t.Cleanup(letGo)
	go func() {
		defer close(applied)
		if err := server.Apply(context.Background(), room, func(*crdt.Doc, func(func(*crdt.Transaction))) {
			close(entered)
			<-release
		}); err != nil && !errors.Is(err, websocket.ErrNoChanges) {
			t.Errorf("hold the room open: %v", err)
		}
	}()
	<-entered
	return letGo
}

// A close whose caller stopped waiting must not still take a free room. Evict receives its
// context from an API caller, and a cancellation before the close begins leaves the resident
// editor connected for the next request rather than reporting a close that happened after its
// caller gave up.
func TestEvictWithACancelledContextLeavesTheRoomResident(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
		t.Fatalf("load document room: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := service.Evict(ctx, artifactID); !errors.Is(err, context.Canceled) {
		t.Fatalf("evict with a cancelled context = %v, want %v", err, context.Canceled)
	}
	if service.srv.GetDoc(artifactID) == nil {
		t.Fatal("evict with a cancelled context closed the resident room")
	}
}

func TestACancelledEvictionLeavesTheFailedRoomForItsOwnEviction(t *testing.T) {
	server := websocket.NewServer()
	server.RoomIdleTimeout = time.Hour
	service := &Service{srv: newRoomServer(server)}
	entered, release, applied := make(chan struct{}), make(chan struct{}), make(chan struct{})
	evicting, evict := make(chan struct{}), make(chan struct{})
	var released, evicted sync.Once
	letGo := func() { released.Do(func() { close(release) }) }
	recoverRoom := func() { evicted.Do(func() { close(evict) }) }
	service.beforeFailedEviction = func(string) {
		close(evicting)
		<-evict
	}
	t.Cleanup(func() {
		letGo()
		recoverRoom()
		<-applied
		service.evictWG.Wait()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
	go func() {
		defer close(applied)
		if err := server.Apply(context.Background(), "room", func(*crdt.Doc, func(func(*crdt.Transaction))) {
			close(entered)
			<-release
		}); err != nil && !errors.Is(err, websocket.ErrNoChanges) {
			t.Errorf("hold the room open: %v", err)
		}
	}()
	<-entered
	service.failRoom("room", errors.New("failed room"))
	<-evicting
	state := service.lockExistingState("room")
	done := state.failedDone
	service.unlockState("room", state)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := service.Evict(ctx, "room"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled eviction = %v, want %v", err, context.DeadlineExceeded)
	}
	if !service.roomFailed("room") || server.GetDoc("room") == nil {
		t.Fatal("cancelled eviction must keep the resident room's failure until its own eviction closes it")
	}
	letGo()
	<-applied
	recoverRoom()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the failed room's eviction did not finish")
	}
	if server.GetDoc("room") != nil || service.roomFailed("room") {
		t.Fatal("the failed room's eviction must close the room and clear its failure")
	}
}

func TestAnEvictionHandsOverFailureOnlyAfterTheRoomCloses(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		closed bool
	}{
		{"closed", nil, true},
		{"already gone", websocket.ErrRoomNotFound, true},
		{"cancelled", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, false},
		{"still in use", websocket.ErrRoomInUse, false},
		{"shutdown", websocket.ErrServerShutdown, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &Service{srv: newRoomServer(websocket.NewServer())}
			state := newRoomState()
			state.failed = errors.New("failed room")
			service.rooms.Store("room", state)
			var closed bool
			service.afterRoomClosed = func(string) { closed = true }
			err := service.forgetEvicted("room", state, test.err)
			if test.closed && err != nil || !test.closed && !errors.Is(err, test.err) {
				t.Fatalf("eviction error = %v, close error = %v", err, test.err)
			}
			if state.released != test.closed || closed != test.closed || service.roomFailed("room") == test.closed {
				t.Fatalf("released=%v afterRoomClosed=%v failure=%v, want closed=%v", state.released, closed, service.roomFailed("room"), test.closed)
			}
		})
	}
}

// A caller inside the room that is not a Server.Apply of the service's own - a browser joining,
// a relay's Inject, an Apply that has not taken the budget's hold yet - is ygo's to refuse: its
// CloseRoom answers websocket.ErrRoomInUse (reearth/ygo#342). The budget's close must read that
// as the room it left, not as an eviction that failed: the sweep counts a close it is told
// happened as memory freed (sweepRooms), so a room reported closed while it is still resident
// would leave the budget over its limit with nothing pending to free it, and every admission
// waiting on it refused until something else moved. Here the room is held by an Apply the
// service's own gate knows nothing of, so ygo is the one that refuses, and the close answers
// "not closed" with no error and closes the room once the caller has left.
func TestTheBudgetsCloseLeavesARoomYgoSaysIsInUse(t *testing.T) {
	server := websocket.NewServer()
	server.RoomIdleTimeout = time.Hour
	service := &Service{srv: newRoomServer(server)}
	entered, release, applied := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var released sync.Once
	letGo := func() { released.Do(func() { close(release) }) }
	t.Cleanup(func() {
		letGo()
		<-applied
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
	go func() {
		defer close(applied)
		// Taken through ygo's own Apply, not roomServer.Apply, so the close's applies hold is
		// free and ygo is what refuses the close, as it does for a browser joining the room.
		if err := server.Apply(context.Background(), "room", func(*crdt.Doc, func(func(*crdt.Transaction))) {
			close(entered)
			<-release
		}); err != nil && !errors.Is(err, websocket.ErrNoChanges) {
			t.Errorf("hold the room open: %v", err)
		}
	}()
	<-entered

	if closed, err := service.srv.closeUnused("room", nil); closed || err != nil {
		t.Fatalf("the budget's close of a room ygo says is in use = %t, %v, want it to leave the room with no error", closed, err)
	}
	if server.GetDoc("room") == nil {
		t.Fatal("the budget's close closed a room a caller was inside")
	}
	letGo()
	<-applied
	if closed, err := service.srv.closeUnused("room", nil); !closed || err != nil {
		t.Fatalf("the budget's close once the caller had left = %t, %v, want it closed", closed, err)
	}
	if server.GetDoc("room") != nil {
		t.Fatal("the budget's close left the room resident")
	}
}

// closeGates counts the close gates service's server holds.
func closeGates(service *Service) int {
	service.srv.gatesMu.Lock()
	defer service.srv.gatesMu.Unlock()
	return len(service.srv.gates)
}
