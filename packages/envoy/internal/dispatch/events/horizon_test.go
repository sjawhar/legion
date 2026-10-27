package events

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestParseHorizonSnapshot(t *testing.T) {
	for _, tc := range []struct {
		text string
		want horizonSnapshot
	}{
		{text: "740:740:", want: horizonSnapshot{xmin: 740, xmax: 740}},
		{text: "10:20:10,14,19", want: horizonSnapshot{xmin: 10, xmax: 20, xip: []uint64{10, 14, 19}}},
		{
			text: "4294967296:4294967300:4294967297",
			want: horizonSnapshot{xmin: 4294967296, xmax: 4294967300, xip: []uint64{4294967297}},
		},
	} {
		got, err := parseHorizonSnapshot(tc.text)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.text, err)
		}
		if got.xmin != tc.want.xmin || got.xmax != tc.want.xmax || !slices.Equal(got.xip, tc.want.xip) {
			t.Fatalf("parse %q = %+v, want %+v", tc.text, got, tc.want)
		}
	}
	for _, text := range []string{
		"", "10", "10:20", "10:20:14:15", "x:20:", "10:x:", "-1:20:", "20:10:",
		"10:20:x", "10:20:14,", "10:20:,14", "10:20:9", "10:20:20", "10:20:14,12", "10:20:14,14",
	} {
		if got, err := parseHorizonSnapshot(text); err == nil {
			t.Fatalf("parse %q = %+v, want it refused", text, got)
		}
	}
}

// The xmax hole, scripted with the counterexample the package documentation names. T2 took
// transaction id 100, T took 101 and drew event 7, then T2 drew event 8 and committed. The first
// observation's snapshot sees 8 and lists nothing in progress -- 101 is not below its xmax, so it
// is not in xip -- which is exactly the case where a rule trusting one snapshot's in-progress
// list would publish 8 while 7 can still commit. Only a later snapshot whose xmin has passed the
// observer's own transaction id proves it.
func TestHorizonProvesAnObservationOnlyFromALaterSnapshot(t *testing.T) {
	ctx := context.Background()
	db := &scriptedHorizonDB{script: []scriptedObservation{
		{newest: 8, snapshot: "101:101:", xid: 102},
		{newest: 8, snapshot: "101:103:101", xid: 103}, // T is still open
		{newest: 9, snapshot: "104:104:", xid: 104},    // T has ended, and so have both observers
		{newest: 9, snapshot: "105:105:", xid: 105},
	}}
	horizon := NewHorizon(db)
	for step, want := range []struct {
		safe    int64
		pending bool
	}{
		{safe: 0, pending: true},
		{safe: 0, pending: true},
		{safe: 8, pending: true},
		{safe: 9, pending: false},
	} {
		safe, err := horizon.Safe(ctx)
		if err != nil {
			t.Fatalf("observation %d: %v", step+1, err)
		}
		if safe != want.safe || horizon.Pending() != want.pending {
			t.Fatalf("after observation %d: Safe = %d, Pending = %v; want %d, %v",
				step+1, safe, horizon.Pending(), want.safe, want.pending)
		}
	}
}

// A writer held open for longer than the pending list holds observations must not cost memory
// past the list's bound, and once it ends the horizon must reach the newest id observed, not
// the newest one that happened to fit in the list.
func TestHorizonKeepsTheNewestObservationWhenThePendingListIsFull(t *testing.T) {
	ctx := context.Background()
	const stuck = 3 * horizonPendingLimit
	db := &scriptedHorizonDB{}
	for i := range stuck {
		db.script = append(db.script, scriptedObservation{
			newest:   int64(i + 1),
			snapshot: "10:" + strconv.Itoa(300+i) + ":10",
			xid:      uint64(300 + i),
		})
	}
	db.script = append(db.script, scriptedObservation{newest: stuck, snapshot: "1000:1000:", xid: 1000})
	horizon := NewHorizon(db)
	for range stuck {
		if safe, err := horizon.Safe(ctx); err != nil || safe != 0 {
			t.Fatalf("Safe behind a writer open since transaction 10 = %d, %v; want 0", safe, err)
		}
	}
	if kept := len(horizon.pending); kept > horizonPendingLimit {
		t.Fatalf("pending observations = %d, want at most %d", kept, horizonPendingLimit)
	}
	if safe, err := horizon.Safe(ctx); err != nil || safe != stuck {
		t.Fatalf("Safe once the writer ended = %d, %v; want %d", safe, err, stuck)
	}
}

// A stall must name what causes it, at most once every warnEvery, and a diagnosis that fails
// must still leave the reader its horizon.
func TestHorizonStallWarningNamesTheHolderAndNeverFailsSafe(t *testing.T) {
	ctx := context.Background()
	db := &scriptedHorizonDB{holder: func(...any) error { return errors.New("pg_stat_activity unavailable") }}
	for i := range 4 {
		db.script = append(db.script, scriptedObservation{
			newest: 5, snapshot: "10:" + strconv.Itoa(20+i) + ":10", xid: uint64(20 + i),
		})
	}
	var logged bytes.Buffer
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	horizon := NewHorizon(db)
	horizon.now = func() time.Time { return now }
	horizon.logger = slog.New(slog.NewTextHandler(&logged, nil))
	warnings := func() int { return strings.Count(logged.String(), "event horizon held back") }

	for step, advance := range []time.Duration{0, 6 * time.Second, 14 * time.Second, 20 * time.Second} {
		now = now.Add(advance)
		if step == 3 {
			started := now.Add(-time.Minute)
			db.holder = func(dest ...any) error {
				*dest[0].(*string) = "backend"
				*dest[1].(*int32) = 4242
				*dest[2].(**time.Time) = &started
				*dest[3].(*string) = "idle in transaction"
				*dest[4].(*string) = "holder-app"
				*dest[5].(*string) = "dispatch"
				*dest[6].(*string) = "update issues set title = 'x'"
				return nil
			}
		}
		if safe, err := horizon.Safe(ctx); err != nil || safe != 0 {
			t.Fatalf("Safe at step %d = %d, %v; want 0 and no error", step, safe, err)
		}
		want := []int{0, 1, 1, 2}[step]
		if got := warnings(); got != want {
			t.Fatalf("stall warnings after step %d = %d, want %d; log:\n%s", step, got, want, logged.String())
		}
	}
	lines := strings.Split(strings.TrimSpace(logged.String()), "\n")
	if !strings.Contains(lines[0], "diagnosis_error=") {
		t.Fatalf("first warning does not report its failed diagnosis: %s", lines[0])
	}
	for _, field := range []string{"pid=4242", "application_name=holder-app", "state=\"idle in transaction\"", "xact_start="} {
		if !strings.Contains(lines[1], field) {
			t.Fatalf("second warning lacks %s: %s", field, lines[1])
		}
	}
}

// T3, the horizon's part: a write that drew a lower id and is still open keeps the horizon below
// it however often a reader asks, while a higher id beside it has committed; once it commits,
// Await reaches past both.
func TestHorizonStaysBelowAnOpenLowerWrite(t *testing.T) {
	database := storetest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	horizon := NewHorizon(database.Pool)
	if _, err := horizon.Await(ctx); err != nil {
		t.Fatalf("await the empty log: %v", err)
	}

	lower, _ := openEventWriter(ctx, t, database)
	a := insertHorizonEvent(ctx, t, lower)
	higher, _ := openEventWriter(ctx, t, database)
	b := insertHorizonEvent(ctx, t, higher)
	if err := higher.Commit(ctx); err != nil {
		t.Fatalf("commit the higher write: %v", err)
	}
	if b <= a {
		t.Fatalf("higher write drew id %d, not above the open write's %d", b, a)
	}

	requireHorizonBelow(ctx, t, horizon, a)
	if err := lower.Commit(ctx); err != nil {
		t.Fatalf("commit the lower write: %v", err)
	}
	if safe, err := horizon.Await(ctx); err != nil || safe < b {
		t.Fatalf("Await after both writes committed = %d, %v; want at least %d", safe, err, b)
	}
}

// T3b, the xmax hole on a real Postgres. T2 takes its transaction id first; T takes a higher one
// and draws the lower event id; T2 draws the higher id and commits. A snapshot taken then has its
// xmax one past T2's id, the newest completed one, so T's id is not below it and T is missing
// from its in-progress list although T is open and holds a lower id than the snapshot sees: a
// rule that trusted that one snapshot would publish T2's id. The horizon must stay below T's id
// until T commits.
//
// The snapshot is instance-wide: a transaction elsewhere on the instance (another test's, or this
// process's background database drops) that completes with a higher id in between moves xmax past
// T and closes the hole. So the scenario is built again, up to a bound, until a snapshot shows the
// hole, and the test logs whether it did.
func TestHorizonDoesNotPassAWriterAboveTheSnapshotXmax(t *testing.T) {
	database := storetest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	horizon := NewHorizon(database.Pool)
	if _, err := horizon.Await(ctx); err != nil {
		t.Fatalf("await the empty log: %v", err)
	}

	const attempts = 10
	for attempt := 1; ; attempt++ {
		t2, _ := openEventWriter(ctx, t, database)
		writer, writerXid := openEventWriter(ctx, t, database)
		lowerID := insertHorizonEvent(ctx, t, writer)
		higherID := insertHorizonEvent(ctx, t, t2)
		if err := t2.Commit(ctx); err != nil {
			t.Fatalf("commit T2: %v", err)
		}

		var visible int64
		var snapshotText string
		if err := database.Pool.QueryRow(ctx,
			`select coalesce(max(id), 0), pg_current_snapshot()::text from events`,
		).Scan(&visible, &snapshotText); err != nil {
			t.Fatalf("read one snapshot: %v", err)
		}
		snapshot, err := parseHorizonSnapshot(snapshotText)
		if err != nil {
			t.Fatal(err)
		}
		hole := writerXid >= snapshot.xmax && !slices.Contains(snapshot.xip, writerXid) && visible >= higherID
		if !hole && attempt < attempts {
			if err := writer.Commit(ctx); err != nil {
				t.Fatalf("commit T: %v", err)
			}
			continue
		}
		t.Logf("xmax hole reproduced: %v on attempt %d (snapshot %s sees id %d; T holds xid %d and id %d)",
			hole, attempt, snapshotText, visible, writerXid, lowerID)

		requireHorizonBelow(ctx, t, horizon, lowerID)
		if err := writer.Commit(ctx); err != nil {
			t.Fatalf("commit T: %v", err)
		}
		if safe, err := horizon.Await(ctx); err != nil || safe < higherID {
			t.Fatalf("Await after T committed = %d, %v; want at least %d", safe, err, higherID)
		}
		return
	}
}

// Await must cover every id committed before it was called, so it may not settle for an
// observation taken before the call: on a quiet log each Await after a commit returns exactly
// the new head.
func TestHorizonAwaitCoversEveryIDCommittedBeforeTheCall(t *testing.T) {
	database := storetest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	horizon := NewHorizon(database.Pool)
	for range 4 {
		id := commitHorizonEvent(ctx, t, database)
		if safe, err := horizon.Await(ctx); err != nil || safe != id {
			t.Fatalf("Await after committing id %d on a quiet log = %d, %v; want %d", id, safe, err, id)
		}
	}
}

// A reset log (truncate events restart identity) hands out ids from 1 again, so a horizon that
// kept its old H would tell a reader that ids it has not yet seen are final, and skip them.
func TestHorizonResetForgetsTheOldLog(t *testing.T) {
	database := storetest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	horizon := NewHorizon(database.Pool)
	for range 3 {
		commitHorizonEvent(ctx, t, database)
	}
	if safe, err := horizon.Await(ctx); err != nil || safe != 3 {
		t.Fatalf("Await before the reset = %d, %v; want 3", safe, err)
	}

	resetEventLog(ctx, t, database)
	head := commitHorizonEvent(ctx, t, database)
	horizon.Reset()
	if horizon.Pending() {
		t.Fatal("Pending right after Reset")
	}
	if safe, err := horizon.Safe(ctx); err != nil || safe != 0 {
		t.Fatalf("Safe right after Reset = %d, %v; want 0", safe, err)
	}
	if safe, err := horizon.Await(ctx); err != nil || safe != head {
		t.Fatalf("Await after Reset = %d, %v; want the new head %d", safe, err, head)
	}
}

// An observation of the old log that is still in flight when Reset runs must not be recorded
// after it: a later observation would prove it safe and raise H back to the old log's head.
func TestHorizonResetDiscardsAnObservationInFlight(t *testing.T) {
	database := storetest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	gated := &gatedHorizonDB{HorizonDB: database.Pool, observed: make(chan struct{}), release: make(chan struct{})}
	horizon := NewHorizon(gated)
	for range 3 {
		commitHorizonEvent(ctx, t, database)
	}
	if safe, err := horizon.Await(ctx); err != nil || safe != 3 {
		t.Fatalf("Await before the reset = %d, %v; want 3", safe, err)
	}

	gated.armed.Store(true)
	done := make(chan error, 1)
	go func() {
		_, err := horizon.Safe(ctx)
		done <- err
	}()
	<-gated.observed // the observation has read the old log and committed
	resetEventLog(ctx, t, database)
	head := commitHorizonEvent(ctx, t, database)
	horizon.Reset()
	close(gated.release)
	if err := <-done; err != nil {
		t.Fatalf("Safe across the reset: %v", err)
	}
	if safe, err := horizon.Await(ctx); err != nil || safe != head {
		t.Fatalf("Await after Reset = %d, %v; want the new head %d", safe, err, head)
	}
}

// Every reader of a process calls Safe; concurrent calls must share an observation, since each
// one costs a round trip and a transaction id.
func TestHorizonConcurrentSafeCallsShareAnObservation(t *testing.T) {
	database := storetest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	counting := &countingHorizonDB{HorizonDB: database.Pool, entered: make(chan struct{}), release: make(chan struct{})}
	horizon := NewHorizon(counting)

	const callers = 8
	var calling sync.WaitGroup
	errs := make(chan error, callers)
	for range callers {
		calling.Add(1)
		go func() {
			calling.Done()
			_, err := horizon.Safe(ctx)
			errs <- err
		}()
	}
	<-counting.entered
	calling.Wait()
	time.Sleep(100 * time.Millisecond) // let every caller reach the observation in flight
	close(counting.release)
	for range callers {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent Safe: %v", err)
		}
	}
	if observations := counting.batches.Load(); observations >= callers {
		t.Fatalf("%d concurrent Safe calls ran %d observations, want fewer", callers, observations)
	}
}

// The stall warning's diagnosis query must run on a real Postgres and name a holder while one
// is open. The holder named may be another test's on the same instance, which is older.
func TestHorizonStallWarningNamesAnOpenTransaction(t *testing.T) {
	database := storetest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	commitHorizonEvent(ctx, t, database)
	holder, _ := openEventWriter(ctx, t, database)
	if _, err := holder.Exec(ctx, `set local application_name = 'horizon-stall-holder'`); err != nil {
		t.Fatalf("name the holder: %v", err)
	}
	logged := &storetest.LockedLog{}
	horizon := NewHorizon(database.Pool)
	horizon.stallAfter = 0
	horizon.logger = slog.New(slog.NewTextHandler(logged, nil))
	if _, err := horizon.Safe(ctx); err != nil {
		t.Fatalf("Safe: %v", err)
	}
	warning := logged.Lines("event horizon held back")
	if warning == "" || strings.Contains(warning, "diagnosis_error") || !strings.Contains(warning, "pid=") {
		t.Fatalf("stall warning does not name an open transaction:\n%s", logged.String())
	}
	t.Log(strings.TrimSpace(warning))
}

// requireHorizonBelow asserts that Safe stays below id however often it is asked, and that Await
// does not return within a short wait, while the write holding id is open.
func requireHorizonBelow(ctx context.Context, t *testing.T, horizon *Horizon, id int64) {
	t.Helper()
	for range 10 {
		safe, err := horizon.Safe(ctx)
		if err != nil {
			t.Fatalf("Safe: %v", err)
		}
		if safe >= id {
			t.Fatalf("Safe = %d while the write holding id %d is open", safe, id)
		}
	}
	if !horizon.Pending() {
		t.Fatalf("Pending is false while a committed id above the open write's %d is observed", id)
	}
	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if safe, err := horizon.Await(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Await while the write holding id %d is open = %d, %v; want it to wait", id, safe, err)
	}
}

// openEventWriter begins a transaction and takes its transaction id before anything else, as
// Broker.Append does before it draws an event id (the writer invariant).
func openEventWriter(ctx context.Context, t *testing.T, database *store.Store) (pgx.Tx, uint64) {
	t.Helper()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin event writer: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	var text string
	if err := tx.QueryRow(ctx, `select pg_current_xact_id()::text`).Scan(&text); err != nil {
		t.Fatalf("take the writer's transaction id: %v", err)
	}
	xid, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		t.Fatalf("parse transaction id %q: %v", text, err)
	}
	return tx, xid
}

// insertHorizonEvent inserts one ownerless event row in tx and returns its id.
func insertHorizonEvent(ctx context.Context, t *testing.T, tx pgx.Tx) int64 {
	t.Helper()
	var id int64
	if err := tx.QueryRow(ctx, `
		insert into events (seq, type, actor, payload, notify)
		values (0, 'horizon.test', '{"kind":"user","id":"alice"}', '{}', false)
		returning id
	`).Scan(&id); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	return id
}

// commitHorizonEvent inserts one event row in a transaction of its own and returns its id.
func commitHorizonEvent(ctx context.Context, t *testing.T, database *store.Store) int64 {
	t.Helper()
	tx, _ := openEventWriter(ctx, t, database)
	id := insertHorizonEvent(ctx, t, tx)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit event: %v", err)
	}
	return id
}

func resetEventLog(ctx context.Context, t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(ctx, `truncate events restart identity`); err != nil {
		t.Fatalf("reset the event log: %v", err)
	}
}

// scriptedHorizonDB answers each observation with the next scripted row and the stall
// diagnosis with holder.
type scriptedHorizonDB struct {
	script []scriptedObservation
	holder func(dest ...any) error
}

type scriptedObservation struct {
	newest   int64
	snapshot string
	xid      uint64
}

func (db *scriptedHorizonDB) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults {
	if len(db.script) == 0 {
		return scriptedResults{err: errors.New("observation script exhausted")}
	}
	next := db.script[0]
	db.script = db.script[1:]
	return scriptedResults{observation: next}
}

func (db *scriptedHorizonDB) QueryRow(context.Context, string, ...any) pgx.Row {
	if db.holder == nil {
		return scanRow(func(...any) error { return pgx.ErrNoRows })
	}
	return scanRow(db.holder)
}

type scriptedResults struct {
	observation scriptedObservation
	err         error
}

func (r scriptedResults) Exec() (pgconn.CommandTag, error) { return pgconn.CommandTag{}, r.err }
func (r scriptedResults) Query() (pgx.Rows, error)         { return nil, errors.New("not scripted") }
func (r scriptedResults) Close() error                     { return r.err }

func (r scriptedResults) QueryRow() pgx.Row {
	return scanRow(func(dest ...any) error {
		if r.err != nil {
			return r.err
		}
		*dest[0].(*int64) = r.observation.newest
		*dest[1].(*string) = r.observation.snapshot
		*dest[2].(*string) = strconv.FormatUint(r.observation.xid, 10)
		return nil
	})
}

type scanRow func(dest ...any) error

func (s scanRow) Scan(dest ...any) error { return s(dest...) }

// countingHorizonDB counts observations and holds the first until release closes.
type countingHorizonDB struct {
	HorizonDB
	batches atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (db *countingHorizonDB) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	if db.batches.Add(1) == 1 {
		close(db.entered)
		<-db.release
	}
	return db.HorizonDB.SendBatch(ctx, batch)
}

// gatedHorizonDB, once armed, lets one observation run to its commit and then holds it, before
// the Horizon sees its result, until release closes.
type gatedHorizonDB struct {
	HorizonDB
	armed    atomic.Bool
	observed chan struct{}
	release  chan struct{}
}

func (db *gatedHorizonDB) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	results := db.HorizonDB.SendBatch(ctx, batch)
	if !db.armed.CompareAndSwap(true, false) {
		return results
	}
	return &gatedResults{BatchResults: results, db: db}
}

type gatedResults struct {
	pgx.BatchResults
	db *gatedHorizonDB
}

func (r *gatedResults) Close() error {
	err := r.BatchResults.Close()
	close(r.db.observed)
	<-r.db.release
	return err
}
