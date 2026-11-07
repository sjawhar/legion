package events

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
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

// The xmax hole, scripted with the counterexample the package documentation names. T2 took
// transaction id 100, T took 101 and drew event 7, then T2 drew event 8 and committed. The first
// observation's snapshot sees 8 and lists nothing in progress -- 101 is not below its xmax, so it
// is not in xip -- which is exactly the case where a rule trusting one snapshot's in-progress
// list would publish 8 while 7 can still commit. Only a later snapshot whose xmin has passed the
// observer's own transaction id proves it.
func TestHorizonProvesAnObservationOnlyFromALaterSnapshot(t *testing.T) {
	ctx := context.Background()
	db := &scriptedHorizonDB{script: []scriptedObservation{
		{newest: 8, xmin: 101, xid: 102},
		{newest: 8, xmin: 101, xid: 103}, // T is still open
		{newest: 9, xmin: 104, xid: 104}, // T has ended, and so have both observers
		{newest: 9, xmin: 105, xid: 105},
	}}
	horizon := NewHorizon(db)
	for step, want := range []struct {
		safe   int64
		behind bool
	}{
		{safe: 0, behind: true},
		{safe: 0, behind: true},
		{safe: 8, behind: true},
		{safe: 9, behind: false},
	} {
		safe, err := horizon.Safe(ctx)
		if err != nil {
			t.Fatalf("observation %d: %v", step+1, err)
		}
		if behind := horizon.Behind(safe); safe != want.safe || behind != want.behind {
			t.Fatalf("after observation %d: Safe = %d, Behind(%d) = %v; want %d, %v",
				step+1, safe, safe, behind, want.safe, want.behind)
		}
	}
}

// Writes that overlap without a break keep every observation unproven for as long as the open
// transactions it overlaps. Scripted: observation i takes transaction id 1000+10i and sees id i,
// and its snapshot's xmin is just above observation i-window's transaction id, so it proves the
// observations up to i-window and no later one. Observations are 10 ms apart. H must then follow
// i-window closely after every observation, however many observations the window holds, and the
// pending list must stay within its bound. A window shorter than the bound keeps every
// observation, so H is exactly i-window. A longer one is thinned: thinning keeps neighbours at
// most step = ceil(4·span/horizonPendingLimit) apart, and span < window·10 ms, so neighbours are
// at most 4·window/horizonPendingLimit observations apart and H trails i-window by fewer than
// that: lagBound = 4·window/horizonPendingLimit (0 for the short window, 12 for the long one).
func TestHorizonFollowsContinuouslyOverlappingWrites(t *testing.T) {
	for _, window := range []int{200, 3 * horizonPendingLimit} {
		t.Run(fmt.Sprintf("window of %d observations", window), func(t *testing.T) {
			ctx := context.Background()
			lagBound := int64(4 * window / horizonPendingLimit)
			db := &scriptedHorizonDB{}
			horizon := NewHorizon(db)
			clock := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
			horizon.now = func() time.Time { return clock }
			horizon.stallAfter = 24 * time.Hour // the stall warning is not under test
			xid := func(i int) uint64 { return uint64(1000 + 10*i) }
			for i := range window + 4000 {
				clock = clock.Add(10 * time.Millisecond)
				xmin := xid(0) // proves nothing
				if i >= window {
					xmin = xid(i-window) + 1
				}
				db.script = []scriptedObservation{{newest: int64(i), xmin: xmin, xid: xid(i)}}
				safe, err := horizon.Safe(ctx)
				if err != nil {
					t.Fatalf("observation %d: %v", i, err)
				}
				if kept := len(horizon.pending); kept > horizonPendingLimit {
					t.Fatalf("after observation %d: %d pending observations, want at most %d", i, kept, horizonPendingLimit)
				}
				provable := max(int64(i-window), 0)
				if safe > provable || safe < provable-lagBound {
					t.Fatalf("after observation %d: Safe = %d, want it in [%d, %d]: proven through observation %d, within %d of it",
						i, safe, max(provable-lagBound, 0), provable, provable, lagBound)
				}
			}
		})
	}
}

// A writer held open across more observations than the pending list holds must cost no memory
// past the list's bound, and once it ends the horizon must reach the newest id observed, not the
// newest one that happened to fit in the list.
func TestHorizonReachesTheNewestIDAfterAStallLongerThanThePendingList(t *testing.T) {
	ctx := context.Background()
	const stuck = 3 * horizonPendingLimit
	db := &scriptedHorizonDB{}
	for i := range stuck {
		db.script = append(db.script, scriptedObservation{newest: int64(i + 1), xmin: 10, xid: uint64(300 + i)})
	}
	db.script = append(db.script, scriptedObservation{newest: stuck, xmin: 100_000, xid: 100_000})
	horizon := NewHorizon(db)
	clock := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	horizon.now = func() time.Time { return clock }
	horizon.stallAfter = 24 * time.Hour // the stall warning is not under test
	for range stuck {
		clock = clock.Add(10 * time.Millisecond)
		if safe, err := horizon.Safe(ctx); err != nil || safe != 0 {
			t.Fatalf("Safe behind a writer open since transaction 10 = %d, %v; want 0", safe, err)
		}
		if kept := len(horizon.pending); kept > horizonPendingLimit {
			t.Fatalf("pending observations = %d, want at most %d", kept, horizonPendingLimit)
		}
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
		db.script = append(db.script, scriptedObservation{newest: 5, xmin: 10, xid: uint64(20 + i)})
	}
	logged := &storetest.LockedLog{}
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	horizon := NewHorizon(db)
	horizon.now = func() time.Time { return now }
	horizon.logger = slog.New(slog.NewTextHandler(logged, nil))

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
		if got := waitForStallWarnings(logged, want); got != want {
			t.Fatalf("stall warnings after step %d = %d, want %d; log:\n%s", step, got, want, logged.String())
		}
	}
	lines := strings.Split(strings.TrimSpace(logged.String()), "\n")
	if !strings.Contains(lines[0], "diagnosis_error=") || !strings.Contains(lines[0], "pending_for=6s") {
		t.Fatalf("first warning does not report its failed diagnosis and its 6 s wait: %s", lines[0])
	}
	for _, field := range []string{"pid=4242", "application_name=holder-app", "state=\"idle in transaction\"", "xact_start="} {
		if !strings.Contains(lines[1], field) {
			t.Fatalf("second warning lacks %s: %s", field, lines[1])
		}
	}
}

// A long-lived Await (the delivery hub's event seed, the relay's start and its reseed) waits on a
// lifetime, so an observation that fails every time (a horizon pool that cannot dial) must reach
// its caller as an error to log and retry on once observations have failed for
// horizonAwaitFailFor, not only when ctx ends. One success between failures restarts the bound:
// a database that fails now and then is one an Await rides out. Each observation advances the
// clock a second: five failures, one success that proves nothing, then failures, of which Await
// must return the first one horizonAwaitFailFor after the first since the success.
func TestHorizonAwaitReturnsAnObservationErrorThatPersists(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	refused := errors.New("horizon pool: dial refused")
	const tick = time.Second
	failFor := int(horizonAwaitFailFor / tick)
	db := &scriptedHorizonDB{}
	for range 5 {
		db.script = append(db.script, scriptedObservation{err: refused})
	}
	db.script = append(db.script, scriptedObservation{newest: 5, xmin: 10, xid: 20})
	for range failFor + 5 {
		db.script = append(db.script, scriptedObservation{err: refused})
	}
	clock := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	observations := 0
	db.observed = func() { observations++; clock = clock.Add(tick) }
	horizon := NewHorizon(db)
	horizon.now = func() time.Time { return clock }
	horizon.stallAfter = 24 * time.Hour // the stall warning is not under test
	horizon.logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	safe, err := horizon.Await(ctx)
	if !errors.Is(err, refused) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Await while every observation fails = %d, %v; want the observation's error before ctx's deadline", safe, err)
	}
	if want := 5 + 1 + failFor + 1; observations != want {
		t.Fatalf("Await returned its error after %d observations, want %d: five failures, a success, then %d s of failures",
			observations, want, failFor)
	}
}

// An observation that fails must say so in the log, since a long-lived Await otherwise waits
// horizonAwaitFailFor with nothing to show why. It is logged at most once every warnEvery, since
// Await re-observes every 250 ms, and not at all when the caller's own cancellation failed it.
func TestHorizonLogsAFailedObservationAtMostOnceEveryWarnEvery(t *testing.T) {
	refused := errors.New("horizon pool: dial refused")
	db := &scriptedHorizonDB{}
	logged := &storetest.LockedLog{}
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	horizon := NewHorizon(db)
	horizon.now = func() time.Time { return now }
	horizon.warnEvery = 30 * time.Second
	horizon.logger = slog.New(slog.NewTextHandler(logged, nil))
	warnings := func() int { return strings.Count(logged.String(), "event horizon observation failed") }

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	db.script = []scriptedObservation{{err: context.Canceled}}
	if _, err := horizon.Safe(cancelled); err == nil {
		t.Fatal("Safe on a cancelled context returned no error")
	}
	if got := warnings(); got != 0 {
		t.Fatalf("failure warnings after the caller's own cancellation = %d, want 0; log:\n%s", got, logged.String())
	}

	// At 0, 1, 11, 29, 30 and 31 s: the second warning is due at 30 s, warnEvery after the first.
	for step, advance := range []time.Duration{0, time.Second, 10 * time.Second, 18 * time.Second, time.Second, time.Second} {
		now = now.Add(advance)
		db.script = []scriptedObservation{{err: refused}}
		if _, err := horizon.Safe(context.Background()); !errors.Is(err, refused) {
			t.Fatalf("Safe at step %d = %v, want the observation's error", step, err)
		}
		if got, want := warnings(), []int{1, 1, 1, 1, 2, 2}[step]; got != want {
			t.Fatalf("failure warnings after step %d = %d, want %d; log:\n%s", step, got, want, logged.String())
		}
	}
	if !strings.Contains(logged.String(), "dial refused") {
		t.Fatalf("failure warning does not carry the observation's error:\n%s", logged.String())
	}
}

// A write that drew a lower id and is still open keeps the horizon below it however often a reader
// asks, while a higher id beside it has committed; once it commits, Await reaches past both.
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

// The xmax hole on a real Postgres. T2 takes its transaction id first; T takes a higher one
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
		var xmaxText string
		if err := database.Pool.QueryRow(ctx,
			`select coalesce(max(id), 0), pg_snapshot_xmax(pg_current_snapshot())::text from events`,
		).Scan(&visible, &xmaxText); err != nil {
			t.Fatalf("read one snapshot: %v", err)
		}
		xmax, err := strconv.ParseUint(xmaxText, 10, 64)
		if err != nil {
			t.Fatalf("parse snapshot xmax %q: %v", xmaxText, err)
		}
		// A snapshot lists in-progress ids only below its xmax, so an id at or above it is not
		// listed: T is open, yet missing from the snapshot's in-progress list.
		hole := writerXid >= xmax && visible >= higherID
		if !hole && attempt < attempts {
			if err := writer.Commit(ctx); err != nil {
				t.Fatalf("commit T: %v", err)
			}
			continue
		}
		t.Logf("xmax hole reproduced: %v on attempt %d (snapshot xmax %d sees id %d; T holds xid %d and id %d)",
			hole, attempt, xmax, visible, writerXid, lowerID)

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
	if horizon.Behind(0) {
		t.Fatal("Behind the forgotten log's floor right after Reset")
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

// Safe must not settle for an observation already in flight when it is called: that one's
// snapshot can predate an event committed just before the call, and with its A at or below H it
// leaves Behind false for the H it answers, so a reader would arm no re-read and the event would
// wait for the next notification however long that takes. Whatever Safe returns, an event
// committed before the call must be at or below H or leave Behind true for it.
func TestHorizonSafeWaitsOutAnObservationInFlightBeforeTheCall(t *testing.T) {
	database := storetest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	gated := &gatedHorizonDB{HorizonDB: database.Pool, observed: make(chan struct{}), release: make(chan struct{})}
	horizon := NewHorizon(gated)
	first := commitHorizonEvent(ctx, t, database)
	if safe, err := horizon.Await(ctx); err != nil || safe != first {
		t.Fatalf("Await on a quiet log = %d, %v; want %d", safe, err, first)
	}

	gated.armed.Store(true)
	stale := make(chan error, 1)
	go func() {
		_, err := horizon.Safe(ctx)
		stale <- err
	}()
	<-gated.observed // the stale observation has read the log and committed
	id := commitHorizonEvent(ctx, t, database)
	type result struct {
		safe int64
		err  error
	}
	called := make(chan result, 1)
	go func() {
		safe, err := horizon.Safe(ctx)
		called <- result{safe, err}
	}()
	select {
	case got := <-called:
		t.Fatalf("Safe = %d, %v while the observation in flight before the call is held", got.safe, got.err)
	case <-time.After(100 * time.Millisecond): // let the call reach the observation in flight
	}
	close(gated.release)
	if err := <-stale; err != nil {
		t.Fatalf("Safe that ran the stale observation: %v", err)
	}
	got := <-called
	if got.err != nil {
		t.Fatalf("Safe after committing id %d: %v", id, got.err)
	}
	if got.safe < id && !horizon.Behind(got.safe) {
		t.Fatalf("Safe after committing id %d = %d with Behind false: a reader would never re-read for it", id, got.safe)
	}
	horizon.mu.Lock()
	observed := horizon.newest
	horizon.mu.Unlock()
	if observed < id {
		t.Fatalf("Safe after committing id %d returned from observations that saw only up to %d", id, observed)
	}
}

// A reader decides whether to re-read on the H it read the log through, and the Horizon's own H
// moves under it while it reads: the reader takes Safe, pages the log up to what it answered,
// and asks only then whether anything observed stands above that. Scripted here as the race
// production runs: a write holding id 7 keeps H at 6 while 9 is committed and observed, the
// reader reads through 6, and before it asks, the write ends (rolls back, which notifies
// nothing) and another reader's observation proves 9 final, so H is 9 and nothing stands above
// the Horizon's own H. Event 9 has been read by nobody, so the decision must be taken against
// the reader's own 6, which it still stands behind; taken against the Horizon's H it would say
// there is nothing to re-read for, and 9 would wait for the next commit.
func TestHorizonBehindAnswersTheSafeItsCallerReadThrough(t *testing.T) {
	ctx := context.Background()
	db := &scriptedHorizonDB{script: []scriptedObservation{
		{newest: 6, xmin: 100, xid: 101},
		{newest: 9, xmin: 102, xid: 103}, // proves the first: H = 6, with 7 open below the committed 9
		{newest: 9, xmin: 104, xid: 105}, // another reader's, once 7 has ended: it proves 9
	}}
	horizon := NewHorizon(db)
	if safe, err := horizon.Safe(ctx); err != nil || safe != 0 {
		t.Fatalf("Safe before any observation is proven = %d, %v; want 0", safe, err)
	}
	reader, err := horizon.Safe(ctx)
	if err != nil || reader != 6 {
		t.Fatalf("the reader's Safe while id 7 is open below the committed 9 = %d, %v; want 6", reader, err)
	}
	proven, err := horizon.Safe(ctx)
	if err != nil || proven != 9 {
		t.Fatalf("the other reader's Safe once id 7 has ended = %d, %v; want 9", proven, err)
	}
	if horizon.Behind(proven) {
		t.Fatalf("Behind(%d) is true with the newest id observed proven safe", proven)
	}
	if !horizon.Behind(reader) {
		t.Fatalf("Behind(%d) is false while the committed event %d, which that reader did not read, stands above it",
			reader, proven)
	}
}

// Every reader of a process calls Safe; concurrent calls must share observations, since each one
// costs a round trip and a transaction id. Callers that arrive while one is in flight wait it out
// and share the next.
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
	waitForStallWarnings(logged, 1)
	warning := logged.Lines("event horizon held back")
	if warning == "" || strings.Contains(warning, "diagnosis_error") || !strings.Contains(warning, "pid=") {
		t.Fatalf("stall warning does not name an open transaction:\n%s", logged.String())
	}
	t.Log(strings.TrimSpace(warning))
}

// waitForStallWarnings waits up to ten seconds for logged to hold want stall warnings, which a
// Horizon logs on a goroutine of its own, and returns how many it holds.
func waitForStallWarnings(logged *storetest.LockedLog, want int) int {
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := strings.Count(logged.String(), "event horizon held back")
		if got >= want || time.Now().After(deadline) {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// requireHorizonBelow asserts that Safe stays below id however often it is asked, and that Await
// does not return within a short wait, while the write holding id is open.
func requireHorizonBelow(ctx context.Context, t *testing.T, horizon *Horizon, id int64) {
	t.Helper()
	var safe int64
	for range 10 {
		observed, err := horizon.Safe(ctx)
		if err != nil {
			t.Fatalf("Safe: %v", err)
		}
		if observed >= id {
			t.Fatalf("Safe = %d while the write holding id %d is open", observed, id)
		}
		safe = observed
	}
	if !horizon.Behind(safe) {
		t.Fatalf("Behind(%d) is false while a committed id above the open write's %d is observed", safe, id)
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

// scriptedHorizonDB answers each observation with the next scripted row (or its scripted
// failure) and the stall diagnosis with holder. observed, when set, runs as each observation
// starts.
type scriptedHorizonDB struct {
	script   []scriptedObservation
	holder   func(dest ...any) error
	observed func()
}

type scriptedObservation struct {
	newest int64
	xmin   uint64
	xid    uint64
	err    error
}

func (db *scriptedHorizonDB) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults {
	if db.observed != nil {
		db.observed()
	}
	if len(db.script) == 0 {
		return scriptedResults{err: errors.New("observation script exhausted")}
	}
	next := db.script[0]
	db.script = db.script[1:]
	return scriptedResults{observation: next, err: next.err}
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
		*dest[1].(**time.Time) = nil
		*dest[2].(*string) = strconv.FormatUint(r.observation.xmin, 10)
		*dest[3].(*string) = strconv.FormatUint(r.observation.xid, 10)
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

// WriterFloor bounds the ids a writer can still hold open by the newest mark older than every
// transaction that holds a write lock on events: an event insert open since before a mark may hold
// an id below its Newest, drawn before the mark's snapshot. An insert open since before the newer
// mark leaves only the older one, and once it ends the newer one counts again; with no mark older
// than it, the floor is H alone.
func TestWriterFloorTakesTheNewestMarkOlderThanEveryEventInsert(t *testing.T) {
	database := storetest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	horizon := NewHorizon(database.Pool)

	first := commitHorizonEvent(ctx, t, database)
	older, err := horizon.Mark(ctx)
	if err != nil || older.Newest != first {
		t.Fatalf("older mark = %+v, %v; want newest %d", older, err, first)
	}
	writer, _ := openEventWriter(ctx, t, database)
	open := insertHorizonEvent(ctx, t, writer)
	second := commitHorizonEvent(ctx, t, database)
	newer, err := horizon.Mark(ctx)
	if err != nil || newer.Newest != second {
		t.Fatalf("newer mark = %+v, %v; want newest %d", newer, err, second)
	}

	if floor, ok, err := horizon.WriterFloor(ctx, newer, older); err != nil || !ok || floor != first {
		t.Fatalf("WriterFloor with an insert of event %d open since before the newer mark = %d, %t, %v; want the older mark's %d", open, floor, ok, err, first)
	}
	if floor, ok, err := horizon.WriterFloor(ctx, newer); err != nil || ok || floor != 0 {
		t.Fatalf("WriterFloor with only a mark newer than an open write = %d, %t, %v; want H (0), not ok", floor, ok, err)
	}
	if err := writer.Rollback(ctx); err != nil {
		t.Fatalf("end the open write: %v", err)
	}
	if floor, ok, err := horizon.WriterFloor(ctx, newer, older); err != nil || !ok || floor != second {
		t.Fatalf("WriterFloor once the write ended = %d, %t, %v; want the newer mark's %d", floor, ok, err, second)
	}
}

// Only a transaction in Dispatch's own database can write its event log, so one holding a
// transaction id in another database on the instance, which holds H back, leaves every mark good.
func TestWriterFloorIgnoresATransactionInAnotherDatabase(t *testing.T) {
	database := storetest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	horizon := NewHorizon(database.Pool)

	config, err := pgx.ParseConfig(os.Getenv(storetest.DatabaseURLEnv))
	if err != nil {
		t.Fatalf("parse %s: %v", storetest.DatabaseURLEnv, err)
	}
	config.Database = "postgres"
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect to the cluster's postgres database: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	elsewhere, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the other database's transaction: %v", err)
	}
	t.Cleanup(func() { _ = elsewhere.Rollback(context.Background()) })
	if _, err := elsewhere.Exec(ctx, `select pg_current_xact_id()`); err != nil {
		t.Fatalf("take the other database's transaction id: %v", err)
	}

	newest := commitHorizonEvent(ctx, t, database)
	mark, err := horizon.Mark(ctx)
	if err != nil {
		t.Fatalf("mark: %v", err)
	}
	if floor, ok, err := horizon.WriterFloor(ctx, mark); err != nil || !ok || floor != newest {
		t.Fatalf("WriterFloor with a transaction open in another database = %d, %t, %v; want the mark's %d", floor, ok, err, newest)
	}
}

// A mark counts only while the log it saw stands. A reset restarts the ids at 1, so a writer of the
// new log draws ids at or below an old mark's Newest after that mark: once the new log has climbed
// past it, the mark would vouch for ids still open. WriterFloor passes over a mark whose newest row
// is gone or carries another created_at.
func TestWriterFloorPassesOverAMarkFromBeforeAReset(t *testing.T) {
	database := storetest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	horizon := NewHorizon(database.Pool)

	commitHorizonEvent(ctx, t, database)
	old := commitHorizonEvent(ctx, t, database)
	before, err := horizon.Mark(ctx)
	if err != nil || before.Newest != old {
		t.Fatalf("mark before the reset = %+v, %v; want newest %d", before, err, old)
	}
	resetEventLog(ctx, t, database)
	for range 3 {
		commitHorizonEvent(ctx, t, database)
	}
	writer, _ := openEventWriter(ctx, t, database)
	open := insertHorizonEvent(ctx, t, writer)
	if open <= before.Newest {
		t.Fatalf("the new log's open id %d is not above the old mark's newest %d, so this test proves nothing", open, before.Newest)
	}
	if floor, ok, err := horizon.WriterFloor(ctx, before); err != nil || ok || floor != 0 {
		t.Fatalf("WriterFloor with only a mark from before the reset = %d, %t, %v; want H (0), not ok", floor, ok, err)
	}
}

// A transaction that holds a transaction id but has inserted no event holds no event id: any it
// draws later it draws after every mark taken meanwhile, above their Newest. So it keeps no mark
// from counting, in Dispatch's own database as in another (a read in a transaction, a write to
// another table, an operator's open session).
func TestWriterFloorIgnoresATransactionThatHasInsertedNoEvent(t *testing.T) {
	database := storetest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	horizon := NewHorizon(database.Pool)

	holder, _ := openEventWriter(ctx, t, database)
	newest := commitHorizonEvent(ctx, t, database)
	mark, err := horizon.Mark(ctx)
	if err != nil {
		t.Fatalf("mark: %v", err)
	}
	if floor, ok, err := horizon.WriterFloor(ctx, mark); err != nil || !ok || floor != newest {
		t.Fatalf("WriterFloor with a transaction id held and no event inserted = %d, %t, %v; want the mark's %d", floor, ok, err, newest)
	}
	// Once it inserts, its event id stands above the mark.
	if id := insertHorizonEvent(ctx, t, holder); id <= mark.Newest {
		t.Fatalf("the holder's insert drew id %d, not above the mark's newest %d", id, mark.Newest)
	}
}

// A Horizon that persists writes its H to event_horizon once it advances, and another Horizon (a
// later process) reads it: every event at or below it is final, whichever process proved it. A
// value of a log since reset is not read: its event is gone, or carries another created_at.
func TestAPersistedHorizonIsReadByALaterProcessAndNotAcrossAReset(t *testing.T) {
	database := storetest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	first := NewHorizon(database.Pool)
	first.PersistProven()
	later := NewHorizon(database.Pool)

	newest := commitHorizonEvent(ctx, t, database)
	persisted := int64(0)
	for deadline := time.Now().Add(30 * time.Second); persisted < newest; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the persisted horizon = %d after %s, want at least %d", persisted, 30*time.Second, newest)
		}
		if _, err := first.Safe(ctx); err != nil {
			t.Fatalf("observe: %v", err)
		}
		var err error
		if persisted, err = later.Persisted(ctx); err != nil {
			t.Fatalf("read the persisted horizon: %v", err)
		}
	}
	if later.Proven() != 0 {
		t.Fatalf("the later process proved %d itself, so this test proves nothing", later.Proven())
	}

	resetEventLog(ctx, t, database)
	for range int(newest) + 2 {
		commitHorizonEvent(ctx, t, database)
	}
	if got, err := later.Persisted(ctx); err != nil || got != 0 {
		t.Fatalf("the persisted horizon after a reset of the log = %d, %v; want 0, the old log's value passed over", got, err)
	}
}

// persistCountingDB counts the persist writes (persistHorizonSQL) sent through it, and refuses
// each while fail is set; every other batch and read goes to the wrapped database.
type persistCountingDB struct {
	HorizonDB
	fail     bool
	persists atomic.Int32
}

func (db *persistCountingDB) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	if len(batch.QueuedQueries) == 1 && batch.QueuedQueries[0].SQL == persistHorizonSQL {
		db.persists.Add(1)
		if db.fail {
			return scriptedResults{err: errors.New("connection refused")}
		}
	}
	return db.HorizonDB.SendBatch(ctx, batch)
}

// A persist write the database refuses is tried again only once horizonPersistEvery has passed,
// not by every observation that finds H above what was last written: each try runs on an
// observer's goroutine for up to horizonPersistTimeout, so a refusing database would otherwise hold
// a reader up at every observation.
func TestAFailedPersistIsTriedAgainOnlyOnceEveryPersistInterval(t *testing.T) {
	database := storetest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	db := &persistCountingDB{HorizonDB: database.Pool, fail: true}
	horizon := NewHorizon(db)
	horizon.PersistProven()
	horizon.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	var clockMu sync.Mutex
	clock := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	horizon.now = func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return clock
	}
	advance := func(by time.Duration) {
		clockMu.Lock()
		defer clockMu.Unlock()
		clock = clock.Add(by)
	}
	observeAdvancing := func(times int) {
		t.Helper()
		for range times {
			before := horizon.Proven()
			commitHorizonEvent(ctx, t, database)
			for horizon.Proven() == before {
				if _, err := horizon.Safe(ctx); err != nil {
					t.Fatalf("observe: %v", err)
				}
			}
		}
	}

	observeAdvancing(10)
	if got := db.persists.Load(); got != 1 {
		t.Fatalf("ten advances of H within one persist interval, every write refused, tried %d persist writes, want 1", got)
	}
	advance(horizonPersistEvery)
	observeAdvancing(3)
	if got := db.persists.Load(); got != 2 {
		t.Fatalf("after one more persist interval, %d persist writes tried, want 2", got)
	}
}

// A persist write decided before a Reset sends nothing once the Reset has moved the generation:
// the H it carries is the old log's.
func TestAPersistDecidedBeforeAResetWritesNothing(t *testing.T) {
	database := storetest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	db := &persistCountingDB{HorizonDB: database.Pool}
	horizon := NewHorizon(db)
	newest := commitHorizonEvent(ctx, t, database)
	for horizon.Proven() < newest {
		if _, err := horizon.Safe(ctx); err != nil {
			t.Fatalf("observe: %v", err)
		}
	}
	horizon.mu.Lock()
	safe, safeAt, generation := horizon.safe, horizon.safeAt, horizon.generation
	horizon.persisting = true
	horizon.mu.Unlock()

	horizon.Reset()
	horizon.writePersisted(safe, safeAt, generation)
	if got := db.persists.Load(); got != 0 {
		t.Fatalf("a persist decided before a Reset sent %d writes after it, want none", got)
	}
	horizon.mu.Lock()
	persisting := horizon.persisting
	horizon.mu.Unlock()
	if persisting {
		t.Fatal("a persist skipped for a Reset left the Horizon persisting, so it would never persist again")
	}
}

// A persist write that reaches the database after a reset of the log, with the Horizon not yet
// Reset, writes nothing: the event now at H's id is the new log's, which does not carry the
// created_at H was observed with, so the old log's H is never stamped as a value of the new log
// that a later process would trust (Persisted).
func TestAPersistOfAnOldLogsHorizonWritesNothingAfterTheLogIsReset(t *testing.T) {
	database := storetest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	horizon := NewHorizon(database.Pool)
	newest := commitHorizonEvent(ctx, t, database)
	for horizon.Proven() < newest {
		if _, err := horizon.Safe(ctx); err != nil {
			t.Fatalf("observe: %v", err)
		}
	}
	horizon.mu.Lock()
	safe, safeAt, generation := horizon.safe, horizon.safeAt, horizon.generation
	horizon.persisting = true
	horizon.mu.Unlock()
	if safeAt.IsZero() {
		t.Fatalf("H %d carries no created_at", safe)
	}

	resetEventLog(ctx, t, database)
	for range int(safe) + 1 {
		commitHorizonEvent(ctx, t, database)
	}
	horizon.writePersisted(safe, safeAt, generation)
	var rows int
	if err := database.Pool.QueryRow(ctx, `select count(*) from event_horizon`).Scan(&rows); err != nil {
		t.Fatalf("count event_horizon: %v", err)
	}
	if rows != 0 {
		t.Fatalf("a persist of the old log's H %d after a reset wrote %d rows, want none", safe, rows)
	}
	if got, err := horizon.Persisted(ctx); err != nil || got != 0 {
		t.Fatalf("Persisted after a reset = %d, %v; want 0", got, err)
	}
}
