package docs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// grantWriteHeading is the one heading the writes weighed here replace, unique in the document
// grantTestState builds, so an edit names it without an occurrence.
const grantWriteHeading = "Grant Heading"

// grantWriter is who makes those writes.
var grantWriter = model.Actor{Kind: "user", ID: "alice"}

// grantTestState is the state of a document of at least bytes of prose under one heading the
// writes weighed here replace (grantWriteHeading).
func grantTestState(t *testing.T, bytes int) []byte {
	t.Helper()
	tree, err := pmdoc.Parse("# " + grantWriteHeading + "\n\n" + proseAtLeast(bytes))
	if err != nil {
		t.Fatal(err)
	}
	state, err := encodeDocumentTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

// grantWrite replaces find with replacement in artifactID, inside a transaction joined to
// request, as an API write runs inside the request that asked for it (api's handlers join once
// per request) and as a write of the server's own runs inside a context with no deadline and no
// cancel. It returns the write's own error, so a goroutine may call it.
func grantWrite(request context.Context, service *Service, artifactID, find, replacement string) error {
	tx, err := service.store.Pool.Begin(request)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	joined, ledger := service.Join(request, tx)
	defer ledger.Discard()
	if _, err := service.ApplyOps(joined, artifactID, []model.EditOp{{Op: "replace", Find: find, With: replacement}}, grantWriter, nil); err != nil {
		return err
	}
	return ledger.Commit(request)
}

// requireHeading fails unless artifactID's text still holds the heading the writes replace, or no
// longer holds it, as held says: a write refused for its slot must leave the document exactly as
// it was, and one that waited for its slot must land.
func requireHeading(t *testing.T, service *Service, artifactID string, held bool) {
	t.Helper()
	text, err := service.Text(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read the document: %v", err)
	}
	if strings.Contains(text, grantWriteHeading) != held {
		t.Fatalf("the document holds %q: %v, want %v", grantWriteHeading, !held, held)
	}
}

// Writes to four rooms at once, against a load concurrency of two slots, never hold more slots
// together than it has, and every one of them lands: the third waits for one of the two to drop
// its fork. Each slot is held a tenth of a second past its take, so the writes overlap. The
// rooms are resident, so every fork is the replica cloned (Service.forkPoint), which is the
// usual fork and which took no slot at all before this change; and each is on an issue of its
// own, since a write takes its document's owner row and writes to one issue queue on it
// (lockArtifactOwner) rather than on the concurrency.
func TestConcurrentLiveWritesNeverHoldMoreSlotsThanThePodHas(t *testing.T) {
	const together = 2
	service, _ := newTestService(t)
	service.settle = time.Hour
	state := grantTestState(t, 16<<10)
	var rooms []string
	for issue := range 4 {
		room := createIssueDocument(t, service.store, 11+issue, "Written whole by this test.")
		rooms = append(rooms, room)
		storeHistory(t, service, room, state)
		if err := loadRoomOf(service, room); err != nil {
			t.Fatalf("load room %s: %v", room, err)
		}
	}
	slots := useLoadSlots(t, newLoadSlots(together))
	var most atomic.Int64
	var taken slotsHeld
	slots.taken = func(held int64) {
		taken.record(held)
		for seen := most.Load(); held > seen && !most.CompareAndSwap(seen, held); seen = most.Load() {
		}
		time.Sleep(100 * time.Millisecond)
	}
	errs := make(chan error, len(rooms))
	for _, room := range rooms {
		go func() { errs <- grantWrite(context.Background(), service, room, grantWriteHeading, "Written") }()
	}
	for range rooms {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatalf("a write failed: %v", err)
			}
		case <-time.After(60 * time.Second):
			t.Fatal("the writes did not all finish in 60 s")
		}
	}
	if got := most.Load(); got > slots.size || got < together {
		t.Fatalf("the writes held %d slots at most, want the %d the concurrency has and never more", got, together)
	}
	if got := taken.all(); len(got) != len(rooms) {
		t.Fatalf("the %d writes took %v slots, want one each: a fork that takes none is a whole document the concurrency does not hold",
			len(rooms), got)
	}
	for _, room := range rooms {
		requireHeading(t, service, room, false)
	}
	if held := slots.held.Load(); held != 0 {
		t.Fatalf("the writes still hold %d slots once they have landed", held)
	}
}

// A write's own decode of its fork runs inside the slot the write already holds, so a write that
// took the pod's only slot finishes rather than wait for a second nothing could serve. The
// concurrency here holds one slot and the test holds it, so the write waits for it, and its
// decode of the stored history then runs with nothing left to take. One wait is the write's own
// cover; a second would be a nested take, and would never return.
func TestAColdWriteWhoseForkDecodesWhileTheSlotsAreFullTakesOneSlot(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	state := grantTestState(t, 16<<10)
	storeHistory(t, service, artifactID, state)
	if service.srv.GetDoc(artifactID) != nil {
		t.Fatal("the room is resident, so the write would fork warm and decode nothing")
	}
	slots := useLoadSlots(t, newLoadSlots(1))
	holder := newLoadSlot()
	if !holder.tryTake() {
		t.Fatal("the empty concurrency refused its only slot")
	}
	var waits atomic.Int64
	waiting := make(chan struct{})
	var waitOnce sync.Once
	slots.waiting = func() {
		waits.Add(1)
		waitOnce.Do(func() { close(waiting) })
	}
	written := make(chan error, 1)
	go func() {
		written <- grantWrite(context.Background(), service, artifactID, grantWriteHeading, "Written")
	}()
	select {
	case <-waiting:
	case err := <-written:
		t.Fatalf("the write finished (%v) while the only slot was held", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the write neither waited for a slot nor finished in 30 s")
	}
	holder.release()
	select {
	case err := <-written:
		if err != nil {
			t.Fatalf("the write failed: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the write did not finish in 60 s once the slot was free: its decode waits for a second slot inside its own")
	}
	if got := waits.Load(); got != 1 {
		t.Fatalf("%d holds waited for a slot, want only the write's own cover: a decode inside the write must cover the slot the write carries", got)
	}
	requireHeading(t, service, artifactID, false)
	if held := slots.held.Load(); held != 0 {
		t.Fatalf("the write still holds %d slots once it has landed", held)
	}
}

// An API write waits for its fork's slot within its request and no longer: one whose request
// ends while it waits is refused ErrServiceUnavailable, which the API answers 503
// DOC_SERVICE_UNAVAILABLE for the caller to retry (api's documentErrorCode), leaves the document
// exactly as it was, and holds no slot, so the next write lands once a slot is free.
func TestAnAPIWriteWhoseRequestEndsWhileItWaitsForASlotIsRefusedAndChangesNothing(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	state := grantTestState(t, 16<<10)
	storeHistory(t, service, artifactID, state)
	if err := loadRoomOf(service, artifactID); err != nil {
		t.Fatalf("load the room: %v", err)
	}
	slots := useLoadSlots(t, newLoadSlots(1))
	holder := newLoadSlot()
	if !holder.tryTake() {
		t.Fatal("the empty concurrency refused its only slot")
	}
	waiting := make(chan struct{})
	var waitOnce sync.Once
	slots.waiting = func() { waitOnce.Do(func() { close(waiting) }) }
	request, cancel := context.WithCancel(context.Background())
	refused := make(chan error, 1)
	go func() { refused <- grantWrite(request, service, artifactID, grantWriteHeading, "Written") }()
	select {
	case <-waiting:
	case err := <-refused:
		t.Fatalf("the write finished (%v) while the only slot was held", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the write did not wait for a slot in 30 s")
	}
	cancel()
	var err error
	select {
	case err = <-refused:
	case <-time.After(30 * time.Second):
		t.Fatal("the write did not answer in 30 s once its request ended")
	}
	if !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("the write answered %v, want ErrServiceUnavailable, which the API answers 503", err)
	}
	if held := slots.held.Load(); held != 1 {
		t.Fatalf("%d slots are held, want the one the test holds and none of the refused write's", held)
	}
	requireHeading(t, service, artifactID, true)
	holder.release()
	if err := grantWrite(context.Background(), service, artifactID, grantWriteHeading, "Written"); err != nil {
		t.Fatalf("the write after it failed: %v", err)
	}
	requireHeading(t, service, artifactID, false)
}

// A write of the server's own, whose context carries no deadline and no cancel, is never refused
// for a slot: it waits while every slot is held, however long that is, and lands once one is
// free for its fork.
func TestAServerWriteWaitsForASlotWithoutADeadlineAndThenLands(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	state := grantTestState(t, 16<<10)
	storeHistory(t, service, artifactID, state)
	if err := loadRoomOf(service, artifactID); err != nil {
		t.Fatalf("load the room: %v", err)
	}
	slots := useLoadSlots(t, newLoadSlots(1))
	holder := newLoadSlot()
	if !holder.tryTake() {
		t.Fatal("the empty concurrency refused its only slot")
	}
	waiting := make(chan struct{})
	var waitOnce sync.Once
	slots.waiting = func() { waitOnce.Do(func() { close(waiting) }) }
	written := make(chan error, 1)
	go func() {
		written <- grantWrite(context.Background(), service, artifactID, grantWriteHeading, "Written")
	}()
	select {
	case <-waiting:
	case err := <-written:
		t.Fatalf("the write finished (%v) while the only slot was held", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the write did not wait for a slot in 30 s")
	}
	select {
	case err := <-written:
		t.Fatalf("the write answered %v while the only slot was held, want it to wait", err)
	case <-time.After(500 * time.Millisecond):
	}
	requireHeading(t, service, artifactID, true)
	holder.release()
	select {
	case err := <-written:
		if err != nil {
			t.Fatalf("the write failed once a slot was free: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the write did not land in 60 s once a slot was free")
	}
	requireHeading(t, service, artifactID, false)
	if held := slots.held.Load(); held != 0 {
		t.Fatalf("the write still holds %d slots once it has landed", held)
	}
}

// streamHeading is what the stream's writes swap grantWriteHeading for and back, so each write
// of a goroutine's loop finds the heading its last one left
// (TestAStreamOfAPILiveWritesStarvesNeitherARoomLoadNorAServerWrite).
const streamHeading = "Heading Grant"

// A stream of API live writes that keeps the pod's load concurrency busy starves neither a
// browser's room load nor a write of the server's own: both wait their turn and both run. Three
// goroutines write to resident rooms of their own, each starting its next write as soon as its
// last one returned, against a concurrency of two slots, so one of them is always waiting;
// while that runs, a room no process holds is loaded (the path a browser's join takes, which
// covers a grant of its own with no deadline, loadDecodedHistory) and one write of the server's
// own, whose context carries no deadline and no cancel, is made on a fourth room.
//
// Neither can starve, because the concurrency's waiters are served in the order they came. A
// caller that finds a slot free takes it at once only while nothing is queued - golang.org/x/sync
// v0.24.0, semaphore/semaphore.go:57-65, whose fast path is guarded by `s.waiters.Len() == 0` -
// and a release notifies the waiters from the front of the list, stopping at the first whose
// weight does not fit rather than passing over it (notifyWaiters, semaphore.go:145-165, whose
// comment names the starvation it is avoiding). So a stream of writes cannot overtake a load or
// a server write already queued, however fast it writes: each new write of the stream joins the
// queue behind them.
//
// This test cannot be made to fail by taking that ordering away, which is why it asserts the
// waits rather than the ordering: the ordering is semaphore.Weighted's own, inside the
// concurrency (loadSlots.semaphore), and the service has no seam for a barging one to be swapped
// in.
// What a failure would look like is a probe that never returns, which is what the deadlines here
// catch.
func TestAStreamOfAPILiveWritesStarvesNeitherARoomLoadNorAServerWrite(t *testing.T) {
	service, _ := newTestService(t)
	service.settle = time.Hour
	state := grantTestState(t, 16<<10)
	// Each room is on an issue of its own: writes to one issue queue on its owner row
	// (lockArtifactOwner), not on the bound, and this test is about the bound.
	const writers = 3
	rooms := make([]string, 0, writers)
	for issue := range writers {
		room := createIssueDocument(t, service.store, 21+issue, "Written whole by this test.")
		storeHistory(t, service, room, state)
		if err := loadRoomOf(service, room); err != nil {
			t.Fatalf("load room %s: %v", room, err)
		}
		rooms = append(rooms, room)
	}
	// The load's own document, which no room holds, so loading it reads and decodes the stored
	// history under a grant of its own; and the server write's, resident, so its fork is a clone.
	loadRoom := createIssueDocument(t, service.store, 31, "Written whole by this test.")
	storeHistory(t, service, loadRoom, state)
	serverRoom := createIssueDocument(t, service.store, 32, "Written whole by this test.")
	storeHistory(t, service, serverRoom, state)
	if err := loadRoomOf(service, serverRoom); err != nil {
		t.Fatalf("load the server write's room: %v", err)
	}
	slots := useLoadSlots(t, newLoadSlots(2))
	var waits atomic.Int64
	slots.waiting = func() { waits.Add(1) }

	// One write with the bound to itself, which the probes' deadline is a multiple of.
	started := time.Now()
	if err := grantWrite(context.Background(), service, rooms[0], grantWriteHeading, streamHeading); err != nil {
		t.Fatalf("the uncontended write: %v", err)
	}
	one := time.Since(started)
	if err := grantWrite(context.Background(), service, rooms[0], streamHeading, grantWriteHeading); err != nil {
		t.Fatalf("the uncontended write back: %v", err)
	}
	// A few times one write, and never under a floor: a probe that waits its turn behind two
	// forks waits about one write, and one that starves waits forever.
	within := max(20*one, 15*time.Second)

	stop := make(chan struct{})
	var streaming sync.WaitGroup
	var written atomic.Int64
	failed := make(chan error, writers)
	for _, room := range rooms {
		streaming.Add(1)
		go func() {
			defer streaming.Done()
			find, with := grantWriteHeading, streamHeading
			for {
				select {
				case <-stop:
					return
				default:
				}
				// An API write: a request context, which a refusal would end (coverFork).
				request, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				err := grantWrite(request, service, room, find, with)
				cancel()
				if err != nil {
					failed <- err
					return
				}
				written.Add(1)
				find, with = with, find
			}
		}()
	}
	// However this test ends, the stream stops: a goroutine still writing would hold the bound
	// and the pool for the rest of the package's run.
	stopped := sync.OnceFunc(func() { close(stop); streaming.Wait() })
	defer stopped()
	// Start the probes only once the stream has the slots crowded, so each of them queues behind
	// a write rather than finding a slot free.
	for crowded := time.Now(); waits.Load() == 0; {
		if time.Since(crowded) > within {
			t.Fatalf("the stream did not crowd the slots in %s: %d writes, no waiter", within, written.Load())
		}
		select {
		case err := <-failed:
			t.Fatalf("a write of the stream failed: %v", err)
		case <-time.After(time.Millisecond):
		}
	}

	loaded := make(chan time.Duration, 1)
	go func() {
		started := time.Now()
		if err := loadRoomOf(service, loadRoom); err != nil {
			t.Errorf("the room load behind the stream failed: %v", err)
		}
		loaded <- time.Since(started)
	}()
	served := make(chan time.Duration, 1)
	go func() {
		started := time.Now()
		// A write of the server's own: no deadline and no cancel, so the bound never refuses it.
		if err := grantWrite(context.Background(), service, serverRoom, grantWriteHeading, streamHeading); err != nil {
			t.Errorf("the server write behind the stream failed: %v", err)
		}
		served <- time.Since(started)
	}()
	var loadTook, serverTook time.Duration
	for range 2 {
		select {
		case loadTook = <-loaded:
		case serverTook = <-served:
		case err := <-failed:
			t.Fatalf("a write of the stream failed: %v", err)
		case <-time.After(within):
			t.Fatalf("the room load (%s) or the server write (%s) did not finish in %s behind a stream of %d API writes: one of them is starving",
				loadTook, serverTook, within, written.Load())
		}
	}
	stopped()
	select {
	case err := <-failed:
		t.Fatalf("a write of the stream failed: %v", err)
	default:
	}
	t.Logf("one uncontended write took %s; behind %d API writes over %d goroutines against a concurrency of %d slots, the room load took %s and the server write %s, and %d holds waited for a slot",
		one, written.Load(), writers, slots.size, loadTook, serverTook, waits.Load())
	if loadTook > within || serverTook > within {
		t.Fatalf("the room load took %s and the server write %s, past the %s a write of %s allows", loadTook, serverTook, within, one)
	}
	if roomDocument(t, service, loadRoom) == nil {
		t.Fatal("the room the load was asked for is not resident")
	}
	requireHeading(t, service, serverRoom, false)
	if held := slots.held.Load(); held != 0 {
		t.Fatalf("%d slots are still held once the stream has stopped", held)
	}
}
