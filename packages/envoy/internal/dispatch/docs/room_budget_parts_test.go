package docs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// chargedPeerRoom is peerUpdateRoom's room charged in a budget of roomBudgetBytes as a loaded
// room is (chargeLoadedRoom), so the hook charges it what it admits.
func chargedPeerRoom(t *testing.T) (*Service, *crdt.Doc, *renderedReplica, *roomCharge) {
	t.Helper()
	service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph of ordinary prose.\n", 0)
	service.budget.limit = roomBudgetBytes
	return service, room, replica, service.chargeLoadedRoom("room", room, replica)
}

// chargeParts is charge's structure part, its content and the content its last count measured, and
// its weight, read under the budget's lock.
func chargeParts(charge *roomCharge) (structure, content, measuredAt, weight int64) {
	charge.budget.mu.Lock()
	defer charge.budget.mu.Unlock()
	return charge.structure, charge.content, charge.measuredAt, charge.weight
}

// Each update the hook admits adds to its room's structure part every new struct with the two
// splits its integration can cause, two splits for each delete range, all at bytesOfHeapAStruct,
// and the content of its new structs no rendering prices at its kind's heap a byte, at both
// copies. A whole-state handoff of the document the room holds adds nothing, nor does an update
// the walk refuses, which is never applied. A count of the room's rendering re-bases its counted
// part and leaves the structure part as it was.
func TestTheRoomBudgetsStructurePartGrowsWithEachAdmittedStructAndSurvivesACount(t *testing.T) {
	service, room, replica, charge := chargedPeerRoom(t)
	service.measurer.interval = noInterval
	admit := func(what string, update []byte) int64 {
		t.Helper()
		before, _, _, _ := chargeParts(charge)
		if err, called := weighInto(service, room, update); err != nil || !called {
			t.Fatalf("%s: the hook answered %v (applied %t), want it admitted", what, err, called)
		}
		after, _, _, _ := chargeParts(charge)
		return after - before
	}

	if grew := admit("the whole state the room holds", crdt.EncodeStateAsUpdateV1(room, nil)); grew != 0 {
		t.Fatalf("a handoff of the document the room holds grew its structure part by %d bytes, want nothing", grew)
	}
	const gcStructs = 1_000
	if grew, want := admit("a frame of GC structs", gcFrameFrom(77, gcStructs)), int64(gcStructs*(1+splitsAStructCanCause)*bytesOfHeapAStruct*roomCopies); grew != want {
		t.Fatalf("a frame of %d GC structs grew the structure part by %d bytes, want %d", gcStructs, grew, want)
	}
	// Margin entries render nowhere, so their keys are charged as text and their Any values at the
	// heap a byte of values, beside the structs.
	frame := newV1Frame(1).group(10, 80, 0)
	for i := range 10 {
		frame.rooted(byte(structAny), marksMapName, fmt.Sprintf("c-%d", i)).uint(1).any(map[string]any{"kind": "comment", "by": "user:alice"})
	}
	entries := frame.noDeletes()
	walked, _, err := walkRecorded(entries, fromPeer)
	if err != nil {
		t.Fatal(err)
	}
	want := int64(0)
	for _, entry := range walked.structs {
		want += int64((1+splitsAStructCanCause)*bytesOfHeapAStruct + len(entry.key)*bytesOfHeapAByteOfText + len(entry.content)*bytesOfHeapAByteOfValues)
	}
	if grew := admit("margin entries", entries); grew != want*roomCopies {
		t.Fatalf("ten margin entries grew the structure part by %d bytes, want %d", grew, want*roomCopies)
	}
	if grew, want := admit("delete ranges", deleteRangesFrom(78, 5)), int64(5*splitsAStructCanCause*bytesOfHeapAStruct*roomCopies); grew != want {
		t.Fatalf("five delete ranges grew the structure part by %d bytes, want %d", grew, want)
	}
	if grew := admit("the same delete ranges again", deleteRangesFrom(78, 5)); grew != 0 {
		t.Fatalf("delete ranges the room already holds grew the structure part by %d bytes, want nothing", grew)
	}
	if grew := admit("the whole state the room holds, its delete ranges included", crdt.EncodeStateAsUpdateV1(room, nil)); grew != 0 {
		t.Fatalf("a handoff of the document the room holds grew its structure part by %d bytes, want nothing", grew)
	}
	structure, _, _, _ := chargeParts(charge)
	otherRoot := newV1Frame(1).group(1, 79, 0).rooted(byte(structString), "other", "").str("x").noDeletes()
	if err, called := weighInto(service, room, otherRoot); err == nil || called {
		t.Fatalf("an update under a root the editor never writes was answered %v (applied %t), want it refused unapplied", err, called)
	}
	if after, _, _, _ := chargeParts(charge); after != structure {
		t.Fatalf("a refused update moved the structure part from %d to %d bytes", structure, after)
	}

	rendering, err := renderDocument(room)
	if err != nil {
		t.Fatal(err)
	}
	replica.publishSize(&rendering)
	service.measurer.waitIdle()
	after, _, _, weight := chargeParts(charge)
	if heap := copyHeap(int64(len(rendering)), countElements(rendering)); after != structure || weight != heap*roomCopies+structure {
		t.Fatalf("after a count the room is charged %d bytes with a %d-byte structure part, want its count's %d at both copies beside the %d-byte structure part as it was",
			weight, after, heap*roomCopies, structure)
	}
	runtime.KeepAlive(replica)
}

// The structure part charges a delete range's splits once, for a range the room did not already
// hold deleted: the room's delete set at its load is listed (deletedRuns.seed), and so is every
// range of every update the hook admits. So a browser's whole-state handoff of a document edited
// into 2,000 deleted runs, and a keystroke from a client that encodes the whole delete set with
// each transaction as ygo does (crdt/doc.go:501-516), are charged nothing for the runs: the
// keystroke is charged its one struct alone. A new deletion is charged its range's two splits,
// and the same update sent again nothing.
func TestTheRoomBudgetChargesADeleteRangeOnlyOnce(t *testing.T) {
	service, room, replica := peerUpdateRoom(t, strings.Repeat("word ", 1_000)+"\n", 0)
	service.budget.limit = roomBudgetBytes
	text := firstText(room.GetXmlFragment(fragmentName))
	room.Transact(func(txn *crdt.Transaction) {
		for i := range 2_000 {
			text.Delete(txn, i, 1)
		}
	})
	charge := service.chargeLoadedRoom("room", room, replica)
	admit := func(what string, update []byte) int64 {
		t.Helper()
		before, _, _, _ := chargeParts(charge)
		if err, called := weighInto(service, room, update); err != nil || !called {
			t.Fatalf("%s: the hook answered %v (applied %t), want it admitted", what, err, called)
		}
		after, _, _, _ := chargeParts(charge)
		return after - before
	}

	handoff := crdt.EncodeStateAsUpdateV1(room, nil)
	if tally, err := walkUpdate(handoff, fromPeer, nil); err != nil || tally.deleteRanges < 2_000 {
		t.Fatalf("the room's whole state carries %d delete ranges (%v), want its 2,000 deleted runs", tally.deleteRanges, err)
	}
	if grew := admit("the whole state of a room of 2,000 deleted runs", handoff); grew != 0 {
		t.Fatalf("a handoff of the document the room holds grew its structure part by %d bytes, want nothing for the deleted runs it loaded with", grew)
	}

	keystroke := peerTyping(t, room, "y")
	struck := weighedFresh(t, keystroke)
	if struck.structs != 1 || struck.deletes == 0 {
		t.Fatalf("the keystroke carries %d new structs and %d delete ranges, want one struct beside the room's delete set", struck.structs, struck.deletes)
	}
	if grew, want := admit("a keystroke carrying the room's delete set", keystroke), int64(((1+splitsAStructCanCause)*bytesOfHeapAStruct+struck.content)*roomCopies); grew != want {
		t.Fatalf("a keystroke carrying %d delete ranges the room holds grew the structure part by %d bytes, want its one struct's %d", struck.deletes, grew, want)
	}

	deletion := peerDeleting(t, room, 1)
	if grew, want := admit("a new deletion", deletion), int64(splitsAStructCanCause*bytesOfHeapAStruct*roomCopies); grew != want {
		t.Fatalf("a deletion of one character grew the structure part by %d bytes, want one range's two splits, %d", grew, want)
	}
	if grew := admit("the same deletion again", deletion); grew != 0 {
		t.Fatalf("a deletion the room has already taken grew the structure part by %d bytes, want nothing", grew)
	}
	runtime.KeepAlive(replica)
}

// The room's update observer charges the content each transaction adds, whoever wrote it - here
// the server's own writes - to the uncounted part, at the densest price at both copies, and never
// the transaction's bytes: ygo's update for a transaction carries the room's whole delete set
// every time (crdt/doc.go:501-516), so a keystroke into a document of 2,000 deleted runs is a
// frame of thousands of bytes that adds two. A transaction that only deletes adds nothing, and a
// whole-state handoff of the document the room holds adds nothing. A count re-bases the uncounted
// part, here one the measurer makes early, as the content passes earlyCountCharge.
func TestTheRoomBudgetChargesATransactionsAddedContentNotItsDeleteSet(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service := newRoomBudgetService(t, database, NewPgVersioned(database))
	service.measurer.mu.Lock()
	service.measurer.interval = time.Hour
	service.measurer.mu.Unlock()
	room := createDocuments(t, database, 1)[0]
	write := func(what string, change func(doc *crdt.Doc, transact func(func(*crdt.Transaction)))) (payload int) {
		t.Helper()
		err := service.srv.Apply(ctx, room, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
			stop := doc.OnUpdate(func(update []byte, _ any) { payload = len(update) })
			defer stop()
			change(doc, transact)
		})
		if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
			t.Fatalf("%s: %v", what, err)
		}
		return payload
	}
	inText := func(edit func(text *crdt.YXmlText, txn *crdt.Transaction)) func(*crdt.Doc, func(func(*crdt.Transaction))) {
		return func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
			text := firstText(doc.GetXmlFragment(fragmentName))
			if text == nil {
				t.Fatal("the document holds no text")
			}
			transact(func(txn *crdt.Transaction) { edit(text, txn) })
		}
	}
	write("load the room", func(*crdt.Doc, func(func(*crdt.Transaction))) {})
	service.budget.mu.Lock()
	charge := service.budget.rooms[room].charges[0]
	service.budget.mu.Unlock()
	uncounted := func() int64 {
		_, content, measuredAt, _ := chargeParts(charge)
		return content - measuredAt
	}

	write("type 4,000 characters", inText(func(text *crdt.YXmlText, txn *crdt.Transaction) { text.Insert(txn, 0, strings.Repeat("x", 4_000), nil) }))
	typed := uncounted()
	if typed < 4_000 || typed > 4_010 {
		t.Fatalf("4,000 characters typed in one transaction left %d bytes uncounted, want their 4,000 and the content's length", typed)
	}
	// Every other character deleted: 2,000 deleted runs, which every later transaction's update
	// carries.
	deleted := write("delete every other character", inText(func(text *crdt.YXmlText, txn *crdt.Transaction) {
		for i := range 2_000 {
			text.Delete(txn, i, 1)
		}
	}))
	if after := uncounted(); after != typed {
		t.Fatalf("a transaction that only deletes, a %d-byte update, moved the uncounted part from %d to %d bytes", deleted, typed, after)
	}
	_, _, _, before := chargeParts(charge)
	keystroke := write("type one character", inText(func(text *crdt.YXmlText, txn *crdt.Transaction) { text.Insert(txn, 0, "y", nil) }))
	_, _, _, after := chargeParts(charge)
	added := uncounted() - typed
	if keystroke < 2_000 || added != 2 || after-before != 2*bytesOfHeapAByteAtTheDensest*roomCopies {
		t.Fatalf("a keystroke's %d-byte update added %d bytes of content and %d of charge, want a frame carrying the 2,000 deleted runs that adds its 2 bytes of content, %d of charge",
			keystroke, added, after-before, 2*bytesOfHeapAByteAtTheDensest*roomCopies)
	}
	held := uncounted()
	write("hand over the whole state the room holds", func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
		if err := crdt.ApplyUpdateV1(doc, crdt.EncodeStateAsUpdateV1(doc, nil), "peer"); err != nil {
			t.Fatal(err)
		}
	})
	if after := uncounted(); after != held {
		t.Fatalf("a whole-state handoff of the document the room holds moved the uncounted part from %d to %d bytes, want nothing added", held, after)
	}

	// A paste that takes the uncounted part past earlyCountCharge is counted at once, a count the
	// hour's interval would otherwise hold back, and the count clears every byte it holds.
	write("paste 8,000 characters", inText(func(text *crdt.YXmlText, txn *crdt.Transaction) { text.Insert(txn, 0, strings.Repeat("z", 8_000), nil) }))
	waitFor(t, 10*time.Second, "the early count to re-base the uncounted part", func() bool { return uncounted() == 0 })
	service.measurer.waitIdle()
	if structure, _, _, _ := chargeParts(charge); structure != 0 {
		t.Fatalf("the server's own writes grew the structure part to %d bytes, want nothing: only the peer hook adds to it", structure)
	}
}

// A room's rendering is counted at most once each roomCountInterval while its uncounted part stays
// under earlyCountCharge, which a thousand keystrokes in one interval, far past anyone's typing,
// do not reach; once a paste takes it past, the rendering is counted at once, both while the room
// waits out the interval and when it waits for nothing.
func TestTheMeasurerCountsARoomEarlyOnceItsUncountedPartPassesTheThresholdAndNeverForTyping(t *testing.T) {
	service := &Service{budget: roomBudget{limit: roomBudgetBytes}}
	service.measurer.interval = time.Hour
	counted := make(chan string, 4)
	service.measurer.beforeCount = func(markdown string) { counted <- markdown }
	loaded := headings(1)
	live := newDocumentCopy()
	replica := service.keepReplica(live, nil, &loaded)
	charge := service.chargeLoadedRoom("room", live, replica)
	early := int64(earlyCountCharge/(bytesOfHeapAByteAtTheDensest*roomCopies)) + 1

	const keystrokes, typedBytes = 1_000, 3
	if keystrokes*typedBytes >= early {
		t.Fatalf("%d keystrokes of %d bytes reach the %d bytes an early count needs", keystrokes, typedBytes, early)
	}
	for i := range keystrokes {
		charge.adds(typedBytes)
		rendering := fmt.Sprintf("%s%d\n", loaded, i)
		replica.publishSize(&rendering)
	}
	select {
	case markdown := <-counted:
		t.Fatalf("typing was counted (%d bytes) before the interval passed", len(markdown))
	case <-time.After(300 * time.Millisecond):
	}

	pasted := loaded + "pasted\n"
	charge.adds(early)
	replica.publishSize(&pasted)
	select {
	case markdown := <-counted:
		if markdown != pasted {
			t.Fatal("the early count is not of the pasted rendering")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a paste past the threshold was not counted while the room waited out the interval")
	}
	service.measurer.waitIdle()
	if _, content, measuredAt, _ := chargeParts(charge); content != measuredAt {
		t.Fatalf("the early count left %d bytes uncounted, want none", content-measuredAt)
	}

	again := loaded + "pasted again\n"
	charge.adds(early)
	replica.publishSize(&again)
	select {
	case markdown := <-counted:
		if markdown != again {
			t.Fatal("the second early count is not of the second pasted rendering")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a paste past the threshold into a room waiting for nothing was not counted at once")
	}
	runtime.KeepAlive(replica)
}

// fakeSwept is a budget whose sweeps are the test's: each start is reported on the channel it
// returns, and lasts until the test ends it (roomBudget.swept).
func fakeSwept(limit int64) (*roomBudget, <-chan struct{}) {
	sweeps := make(chan struct{}, 8)
	budget := &roomBudget{limit: limit}
	budget.sweep = func() { sweeps <- struct{}{} }
	return budget, sweeps
}

// waiting reads how many admissions wait on budget and what they want together.
func waiting(budget *roomBudget) (int, int64) {
	budget.mu.Lock()
	defer budget.mu.Unlock()
	return budget.waiting, budget.wanted
}

// An admission the budget has no room for waits for the sweep it starts, and is refused only once
// that sweep has ended with nothing left that something other than a browser holds; refusal()'s
// look at a budget past its limit waits the same way, and passes once a room's retirement brings
// the budget back within it.
func TestTheRoomBudgetRefusesAnAdmissionOnlyOnceItsSweepHasEnded(t *testing.T) {
	ctx := context.Background()
	budget, sweeps := fakeSwept(1_000)
	budget.load("held", 450)
	answered := make(chan error, 1)
	go func() {
		_, err := budget.reserve(ctx, time.Now().Add(time.Minute), "late", 300)
		answered <- err
	}()
	<-sweeps
	waitFor(t, 5*time.Second, "the admission to wait", func() bool { count, _ := waiting(budget); return count == 1 })
	select {
	case err := <-answered:
		t.Fatalf("the admission was answered %v while its sweep ran", err)
	case <-time.After(100 * time.Millisecond):
	}
	budget.swept(false)
	select {
	case err := <-answered:
		if !errors.Is(err, errRoomBudgetFull) {
			t.Fatalf("the admission was answered %v once its sweep ended with nothing pending, want it refused", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the admission still waits after its sweep ended with nothing pending")
	}
	if count, wanted := waiting(budget); count != 0 || wanted != 0 {
		t.Fatalf("a refused admission left %d waiting wanting %d bytes", count, wanted)
	}

	budget.limit = 800
	passed := make(chan error, 1)
	go func() { passed <- budget.refusal(ctx, time.Now().Add(time.Minute)) }()
	<-sweeps
	waitFor(t, 5*time.Second, "the look past the limit to wait", func() bool { count, _ := waiting(budget); return count == 1 })
	budget.swept(true)
	select {
	case err := <-passed:
		t.Fatalf("the look past the limit was answered %v while a room was pending", err)
	case <-time.After(100 * time.Millisecond):
	}
	budget.retire("held")
	select {
	case err := <-passed:
		if err != nil {
			t.Fatalf("the look past the limit was answered %v once a retirement brought the budget within it", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the look past the limit still waits with the budget within it")
	}
}

// What the admissions waiting on the budget want is kept for every one of them until each stops
// waiting: a sweep that ends with two still waiting leaves both their weights wanted, and the
// second, which began to wait while that sweep ran, has it run once more. Once a room retires
// both are admitted, and nothing is wanted.
func TestTheRoomBudgetKeepsWhatEveryWaitingAdmissionWantsAcrossASweep(t *testing.T) {
	ctx := context.Background()
	deadline := time.Now().Add(time.Minute)
	budget, sweeps := fakeSwept(1_000)
	budget.load("held", 450)
	admitted := make(chan error, 2)
	admit := func(room string, weight int64) {
		reservation, err := budget.reserve(ctx, deadline, room, weight)
		if err == nil && reservation == nil {
			err = errors.New("nothing reserved")
		}
		admitted <- err
	}
	go admit("first", 300)
	<-sweeps
	waitFor(t, 5*time.Second, "the first admission to wait", func() bool { count, _ := waiting(budget); return count == 1 })
	go admit("second", 200)
	waitFor(t, 5*time.Second, "the second admission to wait", func() bool { count, _ := waiting(budget); return count == 2 })
	budget.swept(true)
	select {
	case <-sweeps:
	case <-time.After(5 * time.Second):
		t.Fatal("the sweep an admission began to wait during did not run again")
	}
	if count, wanted := waiting(budget); count != 2 || wanted != 500 {
		t.Fatalf("after a sweep ended %d admissions wait wanting %d bytes, want both, wanting 500", count, wanted)
	}
	budget.retire("held")
	for range 2 {
		select {
		case err := <-admitted:
			if err != nil {
				t.Fatalf("a waiting admission was answered %v once a room retired, want it admitted", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a waiting admission was not admitted once a room retired")
		}
	}
	budget.swept(false)
	if count, wanted := waiting(budget); count != 0 || wanted != 0 || budget.used != 500 {
		t.Fatalf("after both were admitted %d wait wanting %d bytes and the budget holds %d, want none and 500", count, wanted, budget.used)
	}
}

// An admission whose budget is short waits for a room whose browser has just gone: such a room
// is not closable until what the browser left - here a latest edit source no settlement has read,
// which a browser's edit leaves until its settlement runs - is gone. Once it is, the room's state
// starts a sweep that closes it, and the waiting admission is admitted into the room it made. With
// what the browser left never ending, the admission is refused only once admissionWait has passed.
func TestTheRoomBudgetAdmissionWaitsForARoomWhoseBrowserJustLeft(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service := newRoomBudgetService(t, database, NewPgVersioned(database))
	service.measurer.interval = noInterval
	ids := createDocuments(t, database, 3)
	left, admitted, refused := ids[0], ids[1], ids[2]
	appendThroughAPI(t, service, left)
	waitFor(t, 10*time.Second, "the written room to settle and hold nothing", func() bool { return roomUnused(service, left) })
	service.measurer.waitIdle()
	hold := func(unsettled bool) {
		state := service.lockExistingState(left)
		if state == nil {
			t.Fatal("the room whose browser left has no state")
		}
		state.unsettled = unsettled
		service.unlockState(left, state)
	}
	hold(true)
	service.SetRoomBudgetForTest(budgetUsed(service))
	t.Cleanup(func() { service.SetRoomBudgetForTest(0) })

	type answer struct {
		reservation *roomReservation
		err         error
	}
	answered := make(chan answer, 1)
	go func() {
		reservation, err := service.reserveRoom(ctx, admitted)
		answered <- answer{reservation, err}
	}()
	waitFor(t, 5*time.Second, "the admission to wait", func() bool { count, _ := waiting(&service.budget); return count == 1 })
	select {
	case got := <-answered:
		t.Fatalf("the admission was answered %v (%v) while the room whose browser left still settled", got.reservation, got.err)
	case <-time.After(300 * time.Millisecond):
	}
	if service.srv.GetDoc(left) == nil {
		t.Fatal("the budget closed the room whose browser left before it was done with")
	}
	hold(false)
	select {
	case got := <-answered:
		if got.err != nil || got.reservation == nil {
			t.Fatalf("the admission was answered %v (%v) once the room whose browser left was closable, want it admitted", got.reservation, got.err)
		}
		service.budget.release(got.reservation)
	case <-time.After(10 * time.Second):
		t.Fatal("the admission still waits after the room whose browser left became closable")
	}
	if service.srv.GetDoc(left) != nil {
		t.Fatal("the room whose browser left is still open after the admission it made room for")
	}

	// The same wait with what the browser left never ending: the admission is refused once
	// admissionWait has passed, and not before. The room is written into again under the whole
	// budget, so it is not opened over it.
	service.SetRoomBudgetForTest(0)
	appendThroughAPI(t, service, left)
	waitFor(t, 10*time.Second, "the written room to settle and hold nothing", func() bool { return roomUnused(service, left) })
	service.measurer.waitIdle()
	hold(true)
	t.Cleanup(func() {
		if state := service.lockExistingState(left); state != nil {
			state.unsettled = false
			service.unlockState(left, state)
		}
	})
	service.SetRoomBudgetForTest(budgetUsed(service))
	service.admissionWait = 500 * time.Millisecond
	started := time.Now()
	reservation, err := service.reserveRoom(ctx, refused)
	if reservation != nil || !errors.Is(err, errRoomBudgetFull) {
		t.Fatalf("an admission nothing could make room for was answered %v (%v), want it refused", reservation, err)
	}
	if waited := time.Since(started); waited < service.admissionWait {
		t.Fatalf("the admission was refused after %v, before the %v it waits for a pending room", waited, service.admissionWait)
	}
	if service.srv.GetDoc(left) == nil {
		t.Fatal("the budget closed a room something still held")
	}
}

// The same wait with a real browser: one edits its document over the socket and leaves, which
// settles the room at once (settleLastPeer), and the next admission, to a budget that room fills,
// arrives while the settlement still runs - here held at its document lock, as a mebibyte's
// settlement takes seconds on a loaded pod. The admission waits for the settlement to commit and
// the sweep to close the room, and is admitted into the room the close made: the room is gone by
// the time the admission is answered, never refused first and closed after.
func TestTheRoomBudgetAdmitsABrowserOnceTheRoomAnEditingBrowserLeftIsClosed(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service := newRoomBudgetService(t, database, NewPgVersioned(database))
	service.measurer.interval = noInterval
	// The edit's own settle timer never fires: the browser's leaving takes it and settles at once.
	service.settle = time.Minute
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	ids := createDocuments(t, database, 2)
	left, admitted := ids[0], ids[1]
	settling, release := make(chan struct{}, 1), make(chan struct{})
	var released sync.Once
	t.Cleanup(func() { released.Do(func() { close(release) }) })
	service.afterSettleLock = func(room string) {
		if room == left {
			select {
			case settling <- struct{}{}:
			default:
			}
			<-release
		}
	}

	peer := connectPeer(t, httpServer.URL, left, "alice")
	select {
	case <-peer.Answers:
	case <-time.After(10 * time.Second):
		t.Fatal("the browser was never handed its document")
	}
	text := firstText(peer.Doc.GetXmlFragment(fragmentName))
	if text == nil {
		t.Fatal("the browser's document holds no text")
	}
	if _, err := peer.Send(func(txn *crdt.Transaction) { text.Insert(txn, 0, "Edited. ", nil) }); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "the room to take the browser's edit", func() bool {
		return strings.Contains(renderedLive(t, service, left), "Edited. ")
	})
	peer.Close()
	select {
	case <-settling:
	case <-time.After(10 * time.Second):
		t.Fatal("the browser left and no settlement ran")
	}
	// The browser's handler forgets it only once the settlement its leaving runs has returned, so
	// the room's state still names it, as departed; a sweep that took it for a browser holding the
	// room would leave the admission nothing to wait for.
	state := service.lockExistingState(left)
	departed := state != nil && len(state.connected) == 1 && state.onlyDepartedLocked() && !service.unusedLocked(state)
	if state != nil {
		service.unlockState(left, state)
	}
	if !departed {
		t.Fatal("while its settlement runs, the room whose browser left does not name that browser as departed")
	}
	service.SetRoomBudgetForTest(budgetUsed(service))
	t.Cleanup(func() { service.SetRoomBudgetForTest(0) })

	type answer struct {
		reservation *roomReservation
		err         error
	}
	answered := make(chan answer, 1)
	go func() {
		reservation, err := service.reserveRoom(ctx, admitted)
		answered <- answer{reservation, err}
	}()
	waitFor(t, 5*time.Second, "the admission to wait", func() bool { count, _ := waiting(&service.budget); return count == 1 })
	select {
	case got := <-answered:
		t.Fatalf("the admission was answered %v (%v) while the room whose browser left still settled", got.reservation, got.err)
	case <-time.After(300 * time.Millisecond):
	}
	released.Do(func() { close(release) })
	select {
	case got := <-answered:
		if got.err != nil || got.reservation == nil {
			t.Fatalf("the admission was answered %v (%v) once the room whose browser left had settled, want it admitted", got.reservation, got.err)
		}
		service.budget.release(got.reservation)
	case <-time.After(10 * time.Second):
		t.Fatal("the admission still waits after the room whose browser left settled")
	}
	if service.srv.GetDoc(left) != nil {
		t.Fatal("the admission was answered while the room whose browser left was still open")
	}
}

// roomHasBrowser is whether room's state has a browser connected.
func roomHasBrowser(service *Service, room string) bool {
	state := service.lockExistingState(room)
	if state == nil {
		return false
	}
	defer service.unlockState(room, state)
	return len(state.connected) > 0
}

// A room the server's own work loads while the budget is over its limit is closed once that work
// no longer holds it, though the budget is back within its limit and not full by then, so nothing
// else would close it before it retires a minute later. One a browser joins before the work ends is
// the browser's: it is not closed while the browser holds it, nor once the browser has left.
func TestTheRoomBudgetClosesARoomTheServerOpenedOverItOnceUnusedAndNeverABrowsersRoom(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service := newRoomBudgetService(t, database, NewPgVersioned(database))
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	ids := createDocuments(t, database, 2)
	opened, joined := ids[0], ids[1]
	// The server's work in each room outlasts the Apply that opens it, as a settlement or a write's
	// append does: a latest edit source no settlement has read yet, recorded inside the Apply, once
	// the room's load has taken the edit source its store row names (onLoadDocument, which clears
	// one a row does not name).
	hold := func(room string, unsettled bool) {
		state := service.lockState(room)
		state.unsettled = unsettled
		service.unlockState(room, state)
	}

	service.SetRoomBudgetForTest(1)
	for _, room := range []string{opened, joined} {
		if err := service.srv.Apply(ctx, room, func(*crdt.Doc, func(func(*crdt.Transaction))) { hold(room, true) }); err != nil && !errors.Is(err, websocket.ErrNoChanges) {
			t.Fatal(err)
		}
		if !service.budget.isMarked(room) {
			t.Fatalf("a room the server opened over the budget was not marked to close once unused")
		}
	}
	service.SetRoomBudgetForTest(0)
	waitFor(t, 10*time.Second, "the budget's sweeps to end", func() bool {
		service.budget.mu.Lock()
		defer service.budget.mu.Unlock()
		return !service.budget.sweeping
	})
	if service.budget.full.Load() || service.srv.GetDoc(opened) == nil || service.srv.GetDoc(joined) == nil {
		t.Fatalf("with the budget back within its limit, full is %t and the rooms are open: %t, %t; want full down and both open while the work holds them",
			service.budget.full.Load(), service.srv.GetDoc(opened) != nil, service.srv.GetDoc(joined) != nil)
	}

	connection, err := dialDocument(httpServer.URL, joined)
	if err != nil {
		t.Fatalf("open the room the server opened: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	waitFor(t, 10*time.Second, "the browser to join", func() bool { return roomHasBrowser(service, joined) })
	if service.budget.isMarked(joined) {
		t.Fatal("a room a browser joined is still marked to close once the server's work ends")
	}

	hold(opened, false)
	waitFor(t, 10*time.Second, "the room the server opened over the budget to close once unused", func() bool { return service.srv.GetDoc(opened) == nil })
	hold(joined, false)
	leaveDocument(connection)
	waitFor(t, 10*time.Second, "the browser to leave", func() bool { return !roomHasBrowser(service, joined) })
	time.Sleep(300 * time.Millisecond)
	if service.srv.GetDoc(joined) == nil {
		t.Fatal("the budget closed the room a browser had joined once the server's work in it ended")
	}
}

// While the budget is full, a delete-only update is admitted while its structure keeps the
// charges within the limit and the pressure reserve beyond it, and charged; past the reserve it is
// refused with the transient close, never applied, and charged nothing. An insert is refused
// within the reserve too.
func TestTheRoomBudgetAdmitsDeletesUnderPressureWithinTheReserveAndRefusesThemPastIt(t *testing.T) {
	service, room, replica, charge := chargedPeerRoom(t)
	full := func(limit int64) {
		service.budget.mu.Lock()
		service.budget.limit = limit
		service.budget.mu.Unlock()
		service.budget.full.Store(true)
	}
	busy := func(err error) bool {
		var refusal *websocket.PeerClose
		return errors.As(err, &refusal) && refusal.Code == contracts.DocumentBusyCloseCode && refusal.Reason == contracts.DocumentBusyCloseReason
	}

	used := budgetUsed(service)
	full(used - 1)
	deletion := peerDeleting(t, room, 3)
	weighed := peerWeighing{held: room.StateVector()}
	if _, err := walkUpdate(deletion, fromPeer, &weighed); err != nil {
		t.Fatal(err)
	}
	structure := weighed.structureHeap()
	if weighed.inserted != 0 || structure != int64(weighed.deletes*splitsAStructCanCause*bytesOfHeapAStruct*roomCopies) || structure == 0 {
		t.Fatalf("the deletion inserts %d and is charged %d bytes for %d delete ranges, want a delete-only update charged two splits a range", weighed.inserted, structure, weighed.deletes)
	}
	if err, called := weighInto(service, room, deletion); err != nil || !called {
		t.Fatalf("a deletion within the reserve was answered %v (applied %t), want it admitted", err, called)
	}
	if grown, _, _, _ := chargeParts(charge); grown != structure || budgetUsed(service) != used+structure {
		t.Fatalf("the admitted deletion charged a %d-byte structure part, the budget holds %d bytes, want %d and %d", grown, budgetUsed(service), structure, used+structure)
	}

	typing := peerTyping(t, room, "y")
	if err, called := weighInto(service, room, typing); !busy(err) || called {
		t.Fatalf("an insert while the budget is full was answered %v (applied %t), want it refused as transient", err, called)
	}

	used = budgetUsed(service)
	full(used - pressureReserveBytes)
	before := crdt.EncodeStateAsUpdateV1(room, nil)
	if err, called := weighInto(service, room, peerDeleting(t, room, 2)); !busy(err) || called {
		t.Fatalf("a deletion past the reserve was answered %v (applied %t), want it refused as transient", err, called)
	}
	if string(crdt.EncodeStateAsUpdateV1(room, nil)) != string(before) || budgetUsed(service) != used {
		t.Fatal("a deletion refused past the reserve changed the room or its charge")
	}
	runtime.KeepAlive(replica)
}

// browserTreeEdit is the update a browser holding a copy of room sends for change to its Proof
// tree, written by the tree diff the editor's writes come to (pmdoc.Update), as ygo's update
// observer encodes it: the transaction's structs and the copy's whole delete set
// (crdt/doc.go:501-516). browser is the copy, made from room when nil, so successive edits build on
// each other as one browser's do.
func browserTreeEdit(t *testing.T, room, browser *crdt.Doc, change func(tree *pmdoc.Node)) (*crdt.Doc, []byte) {
	t.Helper()
	if browser == nil {
		browser, _ = peerDocument(t, room)
	}
	tree, err := treeOf(browser)
	if err != nil {
		t.Fatal(err)
	}
	change(tree)
	var update []byte
	stop := browser.OnUpdate(func(made []byte, _ any) { update = made })
	defer stop()
	fragment := browser.GetXmlFragment(fragmentName)
	if err := browser.TransactE(func(txn *crdt.Transaction) error { return pmdoc.Update(txn, fragment, tree) }); err != nil {
		t.Fatal(err)
	}
	if update == nil {
		t.Fatal("the browser's edit changed nothing")
	}
	return browser, update
}

// The pressure reserve holds what A4 (cmd/dispatch, TestAFullServerRefusesWhatGrowsAndTakesWhatShrinks)
// asks a full server to take, measured on the same shapes: its budget is filled by rooms of a
// mebibyte of prose and then crossed by a growing room's round of 2,048 one-letter headings, which
// can leave the charges past the limit by the round's whole structure and count before the sweep
// raises full, and by as many rounds as are admitted before it does; then a hundred shrinking
// updates, one browser's four cuts of fifty bytes sent to twenty-five rooms, each carrying the
// copy's delete set with the cuts before it, must fit in what is left. Three such crossing rounds
// and the hundred shrinks fit inside pressureReserveBytes.
func TestTheRoomBudgetsPressureReserveHoldsWhatAFullServerIsAskedToTake(t *testing.T) {
	service, room, replica := peerUpdateRoom(t, "# Growth\n\nA small document its browser grows.\n", 0)
	service.budget.limit = roomBudgetBytes
	charge := service.chargeLoadedRoom("room", room, replica)
	before, err := renderDocument(room)
	if err != nil {
		t.Fatal(err)
	}
	added, err := pmdoc.Parse(strings.Repeat("# g\n", 2_048))
	if err != nil {
		t.Fatal(err)
	}
	_, round := browserTreeEdit(t, room, nil, func(tree *pmdoc.Node) {
		tree.Children = append(tree.Children, added.Children...)
		pmdoc.EnsureBlockIDs(tree)
	})
	if err, called := weighInto(service, room, round); err != nil || !called {
		t.Fatalf("the round of growth was answered %v (applied %t), want it admitted", err, called)
	}
	structure, _, _, _ := chargeParts(charge)
	after, err := renderDocument(room)
	if err != nil {
		t.Fatal(err)
	}
	counted := (countElements(after) - countElements(before)) * bytesOfHeapAnElement * roomCopies
	crossing := structure + counted
	t.Logf("a round of 2,048 headings: %d bytes of update, a %d-byte structure part and %d bytes of count, %d past the limit at most; %d bytes of content uncounted until its early count",
		len(round), structure, counted, crossing, contentAdded(round)*bytesOfHeapAByteAtTheDensest*roomCopies)

	runtime.KeepAlive(replica)

	const shrunk, cuts, cutBytes = 25, 4, 50
	service, room, replica = peerUpdateRoom(t, proseAtLeast(16<<10), 0)
	service.budget.limit = roomBudgetBytes
	charge = service.chargeLoadedRoom("room", room, replica)
	var shrinker *crdt.Doc
	for cut := range cuts {
		var shrink []byte
		shrinker, shrink = browserTreeEdit(t, room, shrinker, func(tree *pmdoc.Node) {
			text := tree.Children[2+cut].Children[0]
			text.Text = strings.Replace(text.Text, strings.Repeat("word ", cutBytes/5), "", 1)
		})
		if weighed := weighedFresh(t, shrink); weighed.inserted != 0 || weighed.deletes < uint64(cut+1) {
			t.Fatalf("cut %d inserts %d and carries %d delete ranges, want nothing inserted and the cuts so far", cut+1, weighed.inserted, weighed.deletes)
		}
		if err, called := weighInto(service, room, shrink); err != nil || !called {
			t.Fatalf("cut %d was answered %v (applied %t), want it admitted", cut+1, err, called)
		}
	}
	perRoom, _, _, _ := chargeParts(charge)
	if want := int64(cuts * splitsAStructCanCause * bytesOfHeapAStruct * roomCopies); perRoom != want {
		t.Fatalf("four cuts, each carrying the cuts before it, grew a room's structure part by %d bytes, want each cut's own range alone, %d", perRoom, want)
	}
	shrinks := shrunk * perRoom
	t.Logf("a hundred shrinks charge %d bytes; three crossing rounds and the shrinks come to %d of the %d-byte reserve", shrinks, 3*crossing+shrinks, int64(pressureReserveBytes))
	if 3*crossing+shrinks > pressureReserveBytes {
		t.Fatalf("three crossing rounds of %d bytes and a hundred shrinks of %d come to %d, past the %d-byte reserve", crossing, shrinks, 3*crossing+shrinks, int64(pressureReserveBytes))
	}
	runtime.KeepAlive(replica)
}
