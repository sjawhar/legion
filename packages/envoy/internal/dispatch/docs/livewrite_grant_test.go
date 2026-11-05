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

// grantRoomWeight is what a live write's fork of room's resident document is granted of the
// copy-heap bound (liveWriteWeight, Service.roomStateBytes).
func grantRoomWeight(t *testing.T, service *Service, room string) int64 {
	t.Helper()
	return liveWriteWeight(service.roomStateBytes(roomDocument(t, service, room)))
}

// requireHeading fails unless artifactID's text still holds the heading the writes replace, or no
// longer holds it, as held says: a write refused for its grant must leave the document exactly as
// it was, and one that waited for its grant must land.
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

// Writes to four rooms at once, against a bound with room for two of their forks, never hold
// more of it together than it has, and every one of them lands: the third waits for one of the
// two to drop its fork. Each grant is held a tenth of a second past its cover, so the writes
// overlap. The rooms are resident, so every fork is the replica cloned (Service.forkPoint),
// which is the usual fork and which took no grant at all before this change; and each is on an
// issue of its own, since a write takes its document's owner row and writes to one issue queue
// on it (lockArtifactOwner) rather than on the bound.
func TestConcurrentLiveWritesNeverHoldMoreOfTheCopyBoundThanItHas(t *testing.T) {
	service, _ := newTestService(t)
	service.settle = time.Hour
	service.measurer.interval = time.Hour
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
	weight := grantRoomWeight(t, service, rooms[0])
	heaps := useCopyHeaps(t, newWeighedHeaps(copyHeapName, 2*weight+weight/2))
	var most atomic.Int64
	var grants heldGrants
	heaps.granted = func(held int64) {
		grants.record(held)
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
	if got := most.Load(); got > heaps.size || got < 2*weight {
		t.Fatalf("the writes held %d bytes of the bound at most, want two %d-byte forks at once and never past the %d-byte bound",
			got, weight, heaps.size)
	}
	if got := grants.all(); len(got) != len(rooms) {
		t.Fatalf("the %d writes were granted %v, want one grant each: a fork that takes none is a whole document the bound does not hold",
			len(rooms), got)
	}
	for _, room := range rooms {
		requireHeading(t, service, room, false)
	}
	if held := heaps.held.Load(); held != 0 {
		t.Fatalf("the writes still hold %d bytes of the bound once they have landed", held)
	}
}

// A write's own decode of its fork runs inside the grant the write already holds, so a write that
// filled the bound finishes rather than wait for a second grant nothing could serve. The bound
// here has room for exactly one write's fork and a byte of it is held, so the write waits for all
// of it, and its decode of the stored history then runs with nothing left to acquire. One wait is
// the write's own cover; a second would be a nested acquire, and would never return.
func TestAColdWriteWhoseForkDecodesWhileTheCopyBoundIsFullTakesOneGrant(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	service.measurer.interval = time.Hour
	state := grantTestState(t, 16<<10)
	storeHistory(t, service, artifactID, state)
	if service.srv.GetDoc(artifactID) != nil {
		t.Fatal("the room is resident, so the write would fork warm and decode nothing")
	}
	weight := liveWriteWeight(len(state))
	heaps := useCopyHeaps(t, newWeighedHeaps(copyHeapName, weight))
	if !heaps.semaphore().TryAcquire(1) {
		t.Fatal("the empty bound refused a byte")
	}
	var waits atomic.Int64
	waiting := make(chan struct{})
	var waitOnce sync.Once
	heaps.waiting = func(int64) {
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
		t.Fatalf("the write finished (%v) while a byte of the bound was held", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the write neither waited for the bound nor finished in 30 s")
	}
	heaps.semaphore().Release(1)
	select {
	case err := <-written:
		if err != nil {
			t.Fatalf("the write failed: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the write did not finish in 60 s once the bound was free: its decode waits for a second grant inside its own")
	}
	if got := waits.Load(); got != 1 {
		t.Fatalf("%d holds waited for the bound, want only the write's own cover: a decode inside the write must cover the grant the write carries", got)
	}
	requireHeading(t, service, artifactID, false)
	if held := heaps.held.Load(); held != 0 {
		t.Fatalf("the write still holds %d bytes of the bound once it has landed", held)
	}
}

// An API write waits for its fork's grant within its request and no longer: one whose request
// ends while it waits is refused ErrServiceUnavailable, which the API answers 503
// DOC_SERVICE_UNAVAILABLE for the caller to retry (api's documentErrorCode), leaves the document
// exactly as it was, and holds nothing of the bound, so the next write lands once the bound has
// room.
func TestAnAPIWriteWhoseRequestEndsWhileItWaitsForTheCopyBoundIsRefusedAndChangesNothing(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	service.measurer.interval = time.Hour
	state := grantTestState(t, 16<<10)
	storeHistory(t, service, artifactID, state)
	if err := loadRoomOf(service, artifactID); err != nil {
		t.Fatalf("load the room: %v", err)
	}
	heaps := useCopyHeaps(t, newWeighedHeaps(copyHeapName, 2*grantRoomWeight(t, service, artifactID)))
	if !heaps.semaphore().TryAcquire(heaps.size) {
		t.Fatal("the empty bound refused all of itself")
	}
	waiting := make(chan struct{})
	var waitOnce sync.Once
	heaps.waiting = func(int64) { waitOnce.Do(func() { close(waiting) }) }
	request, cancel := context.WithCancel(context.Background())
	refused := make(chan error, 1)
	go func() { refused <- grantWrite(request, service, artifactID, grantWriteHeading, "Written") }()
	select {
	case <-waiting:
	case err := <-refused:
		t.Fatalf("the write finished (%v) while the whole bound was held", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the write did not wait for the bound in 30 s")
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
	if held := heaps.held.Load(); held != 0 {
		t.Fatalf("the refused write holds %d bytes of the bound", held)
	}
	requireHeading(t, service, artifactID, true)
	heaps.semaphore().Release(heaps.size)
	if err := grantWrite(context.Background(), service, artifactID, grantWriteHeading, "Written"); err != nil {
		t.Fatalf("the write after it failed: %v", err)
	}
	requireHeading(t, service, artifactID, false)
}

// A write of the server's own, whose context carries no deadline and no cancel, is never refused
// for the bound: it waits while the whole bound is held, however long that is, and lands once the
// bound has room for its fork.
func TestAServerWriteWaitsForTheCopyBoundWithoutADeadlineAndThenLands(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	service.measurer.interval = time.Hour
	state := grantTestState(t, 16<<10)
	storeHistory(t, service, artifactID, state)
	if err := loadRoomOf(service, artifactID); err != nil {
		t.Fatalf("load the room: %v", err)
	}
	heaps := useCopyHeaps(t, newWeighedHeaps(copyHeapName, 2*grantRoomWeight(t, service, artifactID)))
	if !heaps.semaphore().TryAcquire(heaps.size) {
		t.Fatal("the empty bound refused all of itself")
	}
	waiting := make(chan struct{})
	var waitOnce sync.Once
	heaps.waiting = func(int64) { waitOnce.Do(func() { close(waiting) }) }
	written := make(chan error, 1)
	go func() {
		written <- grantWrite(context.Background(), service, artifactID, grantWriteHeading, "Written")
	}()
	select {
	case <-waiting:
	case err := <-written:
		t.Fatalf("the write finished (%v) while the whole bound was held", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the write did not wait for the bound in 30 s")
	}
	select {
	case err := <-written:
		t.Fatalf("the write answered %v while the whole bound was held, want it to wait", err)
	case <-time.After(500 * time.Millisecond):
	}
	requireHeading(t, service, artifactID, true)
	heaps.semaphore().Release(heaps.size)
	select {
	case err := <-written:
		if err != nil {
			t.Fatalf("the write failed once the bound had room: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the write did not land in 60 s once the bound had room")
	}
	requireHeading(t, service, artifactID, false)
	if held := heaps.held.Load(); held != 0 {
		t.Fatalf("the write still holds %d bytes of the bound once it has landed", held)
	}
}
