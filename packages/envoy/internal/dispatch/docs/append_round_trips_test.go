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
// the prepare of a statement the connection's cache misses. A statement run on its own is
// prepared in a trip pgx reports (TracePrepareStart); a batch's uncached statements are prepared
// in a pipeline trip of its own that pgx reports nothing for, so the recorder counts that one
// itself, from the SQL it has seen each connection send (TraceBatchStart, sawSQL).

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
	// seen is the SQL each connection has sent, which is what its statement cache holds. It
	// outlives reset, as the cache outlives the step under test: a statement a warm-up store
	// prepared on this connection is not prepared again by the store being measured.
	seen map[*pgx.Conn]map[string]bool
}

func (r *roundTripRecorder) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	r.sawSQL(conn, data.SQL)
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
	r.sawSQL(conn, data.SQL)
	r.record(roundTrip{conn: conn, sql: []string{data.SQL}, prepare: true})
	return ctx
}

func (r *roundTripRecorder) TracePrepareEnd(context.Context, *pgx.Conn, pgx.TracePrepareEndData) {}

// TraceBatchStart records the batch and, ahead of it, the prepare trip it owes. pgx prepares a
// batch's uncached statements in a pipeline exchange of its own before it sends the batch
// (SendPrepare, Sync, GetResults in pgx v5.11 conn.go), and reports no TracePrepareStart for it,
// so a statement new to this connection costs a trip nothing else here would see. One trip covers
// however many of the batch's statements are new, since they are prepared in that one exchange.
func (r *roundTripRecorder) TraceBatchStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	trip := roundTrip{conn: conn, batch: true}
	for _, queued := range data.Batch.QueuedQueries {
		trip.sql = append(trip.sql, queued.SQL)
		trip.args = append(trip.args, queued.Arguments...)
	}
	if r.sawSQL(conn, trip.sql...) {
		r.record(roundTrip{conn: conn, sql: slices.Clone(trip.sql), batch: true, prepare: true})
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

// sawSQL marks each of sql as sent on conn and reports whether any of them was new to it, which
// is whether the connection's statement cache has to prepare it.
func (r *roundTripRecorder) sawSQL(conn *pgx.Conn, sql ...string) (fresh bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen == nil {
		r.seen = make(map[*pgx.Conn]map[string]bool)
	}
	cached := r.seen[conn]
	if cached == nil {
		cached = make(map[string]bool)
		r.seen[conn] = cached
	}
	for _, statement := range sql {
		if !cached[statement] {
			cached[statement] = true
			fresh = true
		}
	}
	return fresh
}

// reset drops the trips the recorder holds, so the ones it reports next are the ones the step
// under test made. What each connection has sent (seen) stays: its statement cache does.
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

// connOf waits up to within for a round trip naming room whose SQL holds statement, and returns
// the connection it went on: the one the call that made it runs on, whose backend a lock wait
// watches (storetest.WaitForQueuedBehind). pgx reports a query as it starts, before the server
// answers, so the connection is named while the call still waits on its lock. The caller resets
// the recorder first, so the trip found is the call's own.
func (r *roundTripRecorder) connOf(t *testing.T, room, statement string, within time.Duration) *pgx.Conn {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		for _, trip := range r.recorded() {
			if !trip.batch && !trip.prepare && trip.names(room) && strings.Contains(trip.sql[0], statement) {
				return trip.conn
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no round trip ran %q for document %s within %s", statement, room, within)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// labelsOn names, in order, the round trips the recorder holds for conn, which is how far the
// call running on it had got.
func (r *roundTripRecorder) labelsOn(conn *pgx.Conn) []string {
	var trips []roundTrip
	for _, trip := range r.recorded() {
		if trip.conn == conn {
			trips = append(trips, trip)
		}
	}
	return roundTripLabels(trips)
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
		if trip.prepare {
			return "a prepare of the append's writes (one batch)"
		}
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
// is one of the trips the list below names, a statement's own and a batch's alike, and the
// measured keystroke runs on a connection the warm-up keystroke has already prepared every
// statement of, so it makes none: a statement new to that connection, in the batch or beside it,
// adds a trip the list does not hold and fails this test.
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
// the live write queues behind the keystroke, watched on its own backend: the write's one read
// of the document is where it waits. Nothing either of them writes is stored while the lock is
// held, and once it is free both land, one append each, with both edits in the document and
// neither lost. Which of the two updates each stored version holds is not what this pins: the
// write stores the keystroke's update ahead of its own (flushThrough), so either order of the
// two writers leaves the same stored history.
//
// The holder is what isolates this lock: two live writes of one document never reach it
// together, since each takes the document's owner row first (lockArtifactOwner), while nothing
// but this lock orders a live write against the room's worker.
func TestALiveWriteAndAKeystrokeQueueOnTheRoomsLockAndNeitherIsLost(t *testing.T) {
	service, artifactID, recorder := tracedRoomService(t)
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
	// The write's trips from here on are its own, so the first of them that takes the room's
	// transaction-level lock names the connection it runs on (connOf).
	recorder.reset()
	go func() {
		_, err := joinedApplyOps(service, artifactID, []model.EditOp{
			{Op: "replace", Find: "First.", With: "First, written."},
		}, storedOrderAgent, nil)
		written <- err
	}()
	// The write's own backend, not a count of the backends queued: a count is met by any session
	// behind this lock, the write's own later lock at its append included, so it passes a write
	// that never waited at its read. The wait returns at once if the write answers without having
	// queued, and the trips it has made say where it queued: its transaction, the document's
	// owner row and the room's lock, with none of its append's.
	writeConn := recorder.connOf(t, artifactID, "pg_advisory_xact_lock(hashtext", roomWait(t))
	if err, early := storetest.WaitForQueuedBehind(t, holder, writeConn.PgConn().PID(), written, roomWait(t)); early {
		t.Fatalf("the live write answered %v before it queued behind the room's lock: it waited on no lock of the room", err)
	}
	for _, appended := range []string{"the append gate read", "the append's writes (one batch)"} {
		if labels := recorder.labelsOn(writeConn); slices.Contains(labels, appended) {
			t.Fatalf("the live write queued behind the room's lock only after %q, so it did not wait at its one read of the document (readFork, lockedHead); it ran:\n\t%s",
				appended, strings.Join(labels, "\n\t"))
		}
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
		t.Fatalf("the document moved from update %d to %d, want one append each", head, moved)
	}
	stored := storedMarkdown(t, service.store, artifactID)
	for _, want := range []string{"First, written.", "Second, bob."} {
		if !strings.Contains(stored, want) {
			t.Fatalf("the stored document is %q, want %q in it: one of the two writers lost its edit", stored, want)
		}
	}
}
