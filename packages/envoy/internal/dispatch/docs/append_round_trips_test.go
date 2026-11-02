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
// a batch is one trip whatever it queues (appendWrites), a statement is one of its own, and so is
// a prepare the connection's statement cache misses (TracePrepareStart).

// roundTrip is one exchange the code under test made on one connection: the SQL it sent, the
// arguments it sent with it, whether it was a batch, and whether it was a prepare of that SQL
// rather than a run of it.
type roundTrip struct {
	conn    *pgx.Conn
	sql     []string
	args    []any
	batch   bool
	prepare bool
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

// TracePrepareStart records a prepare as the round trip it is. The pool runs pgx's default
// statement cache, which prepares a statement the connection has not seen before the trip that
// runs it, so a prepare inside a store under the room's lock is part of what that store costs.
// pgx reports a prepare of a statement the connection already holds too, which sends nothing
// (TracePrepareEndData.AlreadyPrepared); nothing here prepares by hand, and the cache prepares
// only on a miss, so every prepare recorded is a trip. The query that triggers one is reported
// first (TraceQueryStart), so the prepare is recorded just after it.
func (r *roundTripRecorder) TracePrepareStart(ctx context.Context, conn *pgx.Conn, data pgx.TracePrepareStartData) context.Context {
	r.record(roundTrip{conn: conn, sql: []string{data.SQL}, prepare: true})
	return ctx
}

func (r *roundTripRecorder) TracePrepareEnd(context.Context, *pgx.Conn, pgx.TracePrepareEndData) {}

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

// prepares are the labels of the statements prepared since the last reset, each a round trip of
// its own (TracePrepareStart). A measured store runs on a connection a warm-up store has already
// used, so it should prepare nothing; one that does is reported rather than left out of the
// count.
func (r *roundTripRecorder) prepares() []string {
	var prepared []string
	for _, trip := range r.recorded() {
		if trip.prepare {
			prepared = append(prepared, roundTripLabel(trip))
		}
	}
	return prepared
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
	label := sql
	switch {
	case sql == oneLineSQL(appendGateStatement):
		label = "the append gate read"
	case strings.Contains(sql, "pg_advisory_lock(hashtext"):
		label = "pg_advisory_lock"
	case strings.Contains(sql, "pg_advisory_unlock(hashtext"):
		label = "pg_advisory_unlock"
	case strings.Contains(sql, "pg_advisory_xact_lock(hashtext"):
		label = "pg_advisory_xact_lock"
	case strings.Contains(sql, "pg_try_advisory_lock(hashtext"):
		label = "pg_try_advisory_lock"
	}
	if trip.prepare {
		return "a prepare of " + label
	}
	return label
}

func oneLineSQL(sql string) string {
	return strings.Join(strings.Fields(sql), " ")
}

// tracedRoomService is storedOrderService over a pool whose every round trip is recorded.
func tracedRoomService(t *testing.T) (*Service, string, *roundTripRecorder) {
	t.Helper()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	recorder := &roundTripRecorder{}
	traced := &store.Store{Pool: storetest.Traced(t, database.Pool, recorder, nil)}
	service := New(Deps{
		Store:     traced,
		Events:    events.NewBroker(),
		Identity:  headerIdentity(traced),
		ServerURL: "https://dispatch.example",
		Settle:    time.Hour,
	})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)
	return service, artifactID, recorder
}

// A warm browser keystroke's store is six round trips, all of them under the document's advisory
// lock the room's persistence worker holds on its own connection: the lock, the transaction, the
// gate read, the one batch of writes the append owes, the commit and the unlock (AppendRoomFlush,
// appendUpdateTxClass). The transaction-level lock is not among them: the session already holds
// the document's lock on this very connection, so taking it again would grant what the session
// has and cost a seventh trip - the one every other writer of the document waits out. A prepare
// is counted as the trip it is (TracePrepareStart), and the measured keystroke runs on a
// connection the warm-up keystroke has already prepared every statement of, so it makes none.
func TestAWarmBrowserKeystrokeFlushesInSixRoundTrips(t *testing.T) {
	service, artifactID, recorder := tracedRoomService(t)
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
	if prepares := recorder.prepares(); len(prepares) != 0 {
		t.Errorf("the measured keystroke prepared %d statement(s), each a round trip of its own:\n\t%s",
			len(prepares), strings.Join(prepares, "\n\t"))
	}
}

// What the saving above must not cost: the room's two durable writers of one document, a live
// write and the room's own persistence worker, are serialised by the room's advisory lock. The
// write takes it in its own transaction at its one read of the document (readFork, lockedHead)
// and holds it until the transaction ends; the worker holds the session-level form on its own
// connection for the whole of its store (withRoomLock). That lock is also what lets every append
// of a flush skip a lock of its own (Service.flushThrough's rule): a flush runs inside the
// transaction its caller already locked the room in.
//
// Here a transaction of the test's own holds the room's lock, Bob's keystroke queues on it and
// the live write queues behind the keystroke. Nothing either of them writes is stored while the
// lock is held, and once it is free both land, in the order they queued, with both edits in the
// document and neither lost. The holder is what isolates this lock: two live writes queue on the
// document's owner row before they ever read (lockArtifactOwner), so they serialise whatever the
// room's lock does, while nothing but this lock orders a live write against the room's worker.
func TestALiveWriteAndAKeystrokeQueueOnTheRoomsLockAndNeitherIsLost(t *testing.T) {
	service, artifactID, _ := tracedRoomService(t)
	head := storedHead(t, service.store, artifactID)
	ctx := context.Background()
	holder, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction that holds the room's lock: %v", err)
	}
	defer holder.Rollback(ctx)
	if err := lockDocumentRoom(ctx, holder, artifactID); err != nil {
		t.Fatalf("hold the room's lock: %v", err)
	}

	editAsConnectedPeer(t, service, artifactID, 1, storedOrderBob, replaceRun("Second.", "Second, bob."))
	if waiting := storetest.WaitForLockWaiters(t, holder, 1, roomWait(t)); waiting < 1 {
		t.Fatal("the keystroke's append never came to wait on the room's lock")
	}
	written := make(chan error, 1)
	go func() {
		_, err := joinedApplyOps(service, artifactID, []model.EditOp{
			{Op: "replace", Find: "First.", With: "First, written."},
		}, storedOrderAgent, nil)
		written <- err
	}()
	if waiting := storetest.WaitForLockWaiters(t, holder, 2, roomWait(t)); waiting < 2 {
		t.Fatal("the live write never came to wait on the room's lock behind the keystroke's append")
	}
	select {
	case err := <-written:
		t.Fatalf("the live write answered %v while the room's lock was held: it waited on no lock of the room", err)
	default:
	}
	if moved := storedHead(t, service.store, artifactID); moved != head {
		t.Fatalf("the document moved from update %d to %d while the room's lock was held", head, moved)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("release the room's lock: %v", err)
	}

	select {
	case err := <-written:
		if err != nil {
			t.Fatalf("the live write: %v", err)
		}
	case <-time.After(roomWait(t)):
		t.Fatal("the live write never finished once the room's lock was free")
	}
	waitForLandedAppends(t, service, artifactID)
	report := assertStoredOrder(t, service, artifactID)
	if report.UpdatesChecked < 2 {
		t.Fatalf("the stored history holds %d updates to check, want the keystroke's and the write's", report.UpdatesChecked)
	}
	if moved := storedHead(t, service.store, artifactID); moved != head+2 {
		t.Fatalf("the document moved from update %d to %d, want one append each, in the order they queued", head, moved)
	}
	stored := storedMarkdown(t, service.store, artifactID)
	for _, want := range []string{"First, written.", "Second, bob."} {
		if !strings.Contains(stored, want) {
			t.Fatalf("the stored document is %q, want %q in it: one of the two writers lost its edit", stored, want)
		}
	}
}
