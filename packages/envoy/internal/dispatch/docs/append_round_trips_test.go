package docs

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A round trip is one exchange with Postgres: the client sends and waits for the server. It is
// what a keystroke's store costs under the document's advisory lock, where every other writer of
// the document waits, so the tests here count the trips of one store rather than its statements:
// a batch is one trip whatever it queues (appendWrites), and a statement is one of its own.

// roundTrip is one exchange the code under test made on one connection: the SQL it sent, the
// arguments it sent with it, and whether it was a batch.
type roundTrip struct {
	conn  *pgx.Conn
	sql   []string
	args  []any
	batch bool
}

// names reports whether the trip sent room as one of its arguments, which every statement of one
// document's store does.
func (t roundTrip) names(room string) bool {
	for _, arg := range t.args {
		if text, ok := arg.(string); ok && text == room {
			return true
		}
	}
	return false
}

// roundTripRecorder records every round trip the code under test makes, in order, with the
// connection it went on (storetest.Traced). pgx reports a batch once, at its start, and its
// queued statements are not trips of their own: the whole batch goes out and comes back in one
// exchange.
type roundTripRecorder struct {
	mu    sync.Mutex
	trips []roundTrip
}

func (r *roundTripRecorder) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	r.record(roundTrip{conn: conn, sql: []string{data.SQL}, args: slices.Clone(data.Args)})
	return ctx
}

func (r *roundTripRecorder) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (r *roundTripRecorder) TraceBatchStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	trip := roundTrip{conn: conn, batch: true}
	for _, queued := range data.Batch.QueuedQueries {
		trip.sql = append(trip.sql, queued.SQL)
		trip.args = append(trip.args, queued.Arguments...)
	}
	r.record(trip)
	return ctx
}

func (r *roundTripRecorder) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

func (r *roundTripRecorder) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

func (r *roundTripRecorder) record(trip roundTrip) {
	r.mu.Lock()
	r.trips = append(r.trips, trip)
	r.mu.Unlock()
}

// reset drops what the recorder holds, so the trips it reports next are the ones the step under
// test made.
func (r *roundTripRecorder) reset() {
	r.mu.Lock()
	r.trips = nil
	r.mu.Unlock()
}

func (r *roundTripRecorder) recorded() []roundTrip {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.trips)
}

// roomLockRuns are the runs of round trips the holders of room's session-level advisory lock made
// on their own connections: each from the pg_advisory_lock that took it to the pg_advisory_unlock
// that released it (withRoomLock). Nothing else runs on that connection meanwhile - the holder
// marks its context as holding the connection (store.HoldsConnection) and does all its work
// through it - so a run is the whole cost of one such store.
func (r *roundTripRecorder) roomLockRuns(room string) [][]roundTrip {
	var runs [][]roundTrip
	trips := r.recorded()
	for index, trip := range trips {
		if trip.batch || !trip.names(room) || !strings.Contains(trip.sql[0], "pg_advisory_lock(hashtext") {
			continue
		}
		run := []roundTrip{trip}
		for _, later := range trips[index+1:] {
			if later.conn != trip.conn {
				continue
			}
			run = append(run, later)
			if !later.batch && strings.Contains(later.sql[0], "pg_advisory_unlock(hashtext") {
				break
			}
		}
		runs = append(runs, run)
	}
	return runs
}

// beforeFirstAppend is the labels of the count round trips a connection made immediately before
// the first one that appended an update of room: the batch of writes an append sends
// (appendWrites), whose first statement inserts the row.
func (r *roundTripRecorder) beforeFirstAppend(t *testing.T, room string, count int) []string {
	t.Helper()
	trips := r.recorded()
	for index, trip := range trips {
		if !trip.batch || !trip.names(room) || !strings.Contains(trip.sql[0], "insert into doc_updates") {
			continue
		}
		var before []roundTrip
		for _, earlier := range trips[:index] {
			if earlier.conn == trip.conn {
				before = append(before, earlier)
			}
		}
		if len(before) < count {
			t.Fatalf("the append of %s made only %d round trips before it on its connection, want at least %d: %v",
				room, len(before), count, roundTripLabels(before))
		}
		return roundTripLabels(before[len(before)-count:])
	}
	t.Fatalf("no append of %s was recorded; the round trips were %v", room, roundTripLabels(trips))
	return nil
}

// roundTripLabels names each trip for an assertion's message: the statements one store runs are
// few and fixed, so each has a name of its own and anything else is reported as its own SQL.
func roundTripLabels(trips []roundTrip) []string {
	labels := make([]string, 0, len(trips))
	for _, trip := range trips {
		labels = append(labels, roundTripLabel(trip))
	}
	return labels
}

func roundTripLabel(trip roundTrip) string {
	if trip.batch {
		return "the append's writes (one batch)"
	}
	sql := oneLineSQL(trip.sql[0])
	switch {
	case sql == oneLineSQL(appendGateStatement):
		return "the append gate read"
	case strings.Contains(sql, "pg_advisory_lock(hashtext"):
		return "pg_advisory_lock"
	case strings.Contains(sql, "pg_advisory_unlock(hashtext"):
		return "pg_advisory_unlock"
	case strings.Contains(sql, "pg_advisory_xact_lock(hashtext"):
		return "pg_advisory_xact_lock"
	case strings.Contains(sql, "pg_try_advisory_lock(hashtext"):
		return "pg_try_advisory_lock"
	}
	return sql
}

func oneLineSQL(sql string) string {
	return strings.Join(strings.Fields(sql), " ")
}

// tracedRoomService is storedOrderService over a pool whose every round trip is recorded. held,
// when true, holds the room's first browser append in the store until the test ends, so a writer
// of the document meets an update the room has committed and no one has stored.
func tracedRoomService(t *testing.T, held bool) (*Service, string, *roundTripRecorder, <-chan struct{}) {
	t.Helper()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	recorder := &roundTripRecorder{}
	traced := &store.Store{Pool: storetest.Traced(t, database.Pool, recorder, nil)}
	deps := Deps{
		Store:     traced,
		Events:    events.NewBroker(),
		Identity:  headerIdentity(traced),
		ServerURL: "https://dispatch.example",
		Settle:    time.Hour,
	}
	entered := make(chan struct{})
	var releaseHeld func()
	if held {
		release := make(chan struct{})
		blocking := &blockingFirstAppendStore{VersionedStore: NewPgVersioned(traced), entered: entered, release: release}
		deps.Persistence = blocking
		releaseHeld = func() {
			if blocking.released.CompareAndSwap(false, true) {
				close(release)
			}
		}
	}
	service := New(deps)
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	// The held append is released before the service is shut down: the drain its shutdown runs
	// waits for it. Cleanups run in reverse, so this one is registered last.
	if releaseHeld != nil {
		t.Cleanup(releaseHeld)
	}
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)
	return service, artifactID, recorder, entered
}

// A warm browser keystroke's store is six round trips, all of them under the document's advisory
// lock the room's persistence worker holds on its own connection: the lock, the transaction, the
// gate read, the one batch of writes the append owes, the commit and the unlock (AppendRoomFlush,
// appendUpdateTxClass). The transaction-level lock is not among them: the session already holds
// the document's lock on this very connection, so taking it again would grant what the session
// has and cost a seventh trip - the one every other writer of the document waits out.
func TestAWarmBrowserKeystrokeFlushesInSixRoundTrips(t *testing.T) {
	service, artifactID, recorder, _ := tracedRoomService(t, false)
	// The first keystroke warms the room, the pool and the store's prepared statements; the
	// second is the one measured.
	editAsConnectedPeer(t, service, artifactID, 1, storedOrderBob, replaceRun("Second.", "Second, bob."))
	waitForLandedAppends(t, service, artifactID)
	recorder.reset()
	editAsConnectedPeer(t, service, artifactID, 1, storedOrderBob, replaceRun("Second, bob.", "Second, bob typed."))
	waitForLandedAppends(t, service, artifactID)

	runs := recorder.roomLockRuns(artifactID)
	if len(runs) != 1 {
		t.Fatalf("the keystroke took the document's lock %d times, want once: %v", len(runs), runs)
	}
	got := roundTripLabels(runs[0])
	want := []string{
		"pg_advisory_lock",
		"begin",
		"the append gate read",
		"the append's writes (one batch)",
		"commit",
		"pg_advisory_unlock",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("a warm keystroke's store made %d round trips:\n\t%s\nwant %d:\n\t%s",
			len(got), strings.Join(got, "\n\t"), len(want), strings.Join(want, "\n\t"))
	}
	if before := recorder.beforeFirstAppend(t, artifactID, 2); !slices.Equal(before, []string{"begin", "the append gate read"}) {
		t.Fatalf("the keystroke's append followed %v, want its transaction and the gate read alone", before)
	}
}

// A live write holds no session lock of the document: its transaction takes the room's
// transaction-level lock itself, and so does every append of the room's updates it stores ahead
// of its own (Ledger.flushThrough, lockDocumentRoom). The keystroke path's saving is the room's
// persistence worker's alone, and must not reach here: an append that took no lock in a
// transaction holding none would race every other writer of the document.
func TestALiveWritesFlushedAppendTakesTheRoomsTransactionLock(t *testing.T) {
	service, artifactID, recorder, entered := tracedRoomService(t, true)
	// Bob's keystroke is in the room and its append is held before the store, so the live write
	// below stores it ahead of its own append.
	editAsConnectedPeer(t, service, artifactID, 1, storedOrderBob, replaceRun("Second.", "Second, bob."))
	awaitHeldAppend(t, entered)
	recorder.reset()
	if _, err := joinedApplyOps(service, artifactID, []model.EditOp{
		{Op: "replace", Find: "First.", With: "First, written."},
	}, storedOrderAgent, nil); err != nil {
		t.Fatalf("make the live write: %v", err)
	}
	before := recorder.beforeFirstAppend(t, artifactID, 2)
	want := []string{"pg_advisory_xact_lock", "the append gate read"}
	if !slices.Equal(before, want) {
		t.Fatalf("the live write's flushed append followed %v, want %v", before, want)
	}
}
