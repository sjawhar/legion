package docs

import (
	"context"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A restart loses the compaction queue, and nothing reopens the documents it held: the sweep of the
// service that replaces it folds a document whose rows came due while no process held it in a
// queue, as a sweep that reads the database rather than any process's memory does. The rows older
// than the tail fold into the fold point and the young row stays as written, byte for byte, and the
// document reads as it did.
func TestTheCompactionSweepFoldsADocumentTheRestartedServiceNeverOpened(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	before := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() { _ = before.Shutdown(context.Background()) })
	foldPoint := writeRoomRows(t, before, artifactID)
	// The rows are young as the service stops, so its rooms' close compactions leave them; they
	// come of age while no service runs.
	if err := before.Shutdown(ctx); err != nil {
		t.Fatalf("shut down the first service: %v", err)
	}
	backdateUpdates(t, database, artifactID, foldPoint)
	rows, cursor := storedRows(t, database, artifactID), settlementCursor(t, database, artifactID)
	reading := storedReading(t, NewPgVersioned(database), artifactID)

	restarted := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() { _ = restarted.Shutdown(context.Background()) })
	restarted.compactionSweepTick(ctx, excerptLeases(t, database))

	requireCompactedFrom(t, database, artifactID, rows, foldPoint, cursor)
	if got := storedReading(t, NewPgVersioned(database), artifactID); got != reading {
		t.Fatalf("the swept document reads\n%s\nwant\n%s", got, reading)
	}
}

// The sweep runs once an aligned hour across the processes that share the database, and across
// their restarts: a second process's tick inside the hour the first one swept finds the hour served
// and compacts nothing, however due a document is, and the duty record holds the first pass's hour.
func TestTheCompactionSweepRunsOnceAnHourAcrossProcesses(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	waitOutTheSweepHour(t, database)
	logs := &storetest.LockedLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	first := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() { _ = first.Shutdown(context.Background()) })
	other := New(Deps{Store: second, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() { _ = other.Shutdown(context.Background()) })

	swept := storeAgedRows(t, database, 3, 20*time.Minute)
	first.compactionSweepTick(ctx, excerptLeases(t, database))
	if got := storedRows(t, database, swept); len(got) != 1 {
		t.Fatalf("the first process's sweep left rows %v, want the document folded into one", rowVersions(got))
	}

	due := storeAgedRows(t, database, 3, 20*time.Minute)
	rows := storedRows(t, database, due)
	other.compactionSweepTick(ctx, excerptLeases(t, second))
	requireRowsAsStored(t, database, due, rows)

	ran := countExcerptLeaseLines(logs, "dispatch duty: leased pass ran", compactionSweepLease)
	skipped := countExcerptLeaseLines(logs, "dispatch duty: leased pass skipped", compactionSweepLease)
	if ran != 1 || skipped != 1 {
		t.Fatalf("the two ticks ran %d passes and skipped %d, want one of each:\n%s", ran, skipped, logs.Lines("lease="))
	}
	var thisHour bool
	if err := database.Pool.QueryRow(ctx, `
		select boundary = to_timestamp(floor(extract(epoch from now()) / 3600) * 3600)
		from duty_passes where lease = $1 and period_ns = $2
	`, compactionSweepLease, compactionSweepPeriod.Nanoseconds()).Scan(&thisHour); err != nil {
		t.Fatalf("read the sweep's duty record: %v", err)
	}
	if !thisHour {
		t.Fatal("the sweep's duty record does not hold this hour's pass")
	}
}

// The sweep compacts a document of a few rows by their age: one whose rows after the first are
// older than compactMaxTailAge folds, and one whose two rows after the first are five minutes old -
// past the tail, so a compaction would fold them, and fewer than compactMinRows - is left as stored
// for the queue of whichever process writes or reads it next.
func TestTheCompactionSweepLeavesADocumentWhoseRowsAreYoungerThanTheAgeRule(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	old := storeAgedRows(t, database, 3, 20*time.Minute)
	young := storeAgedRows(t, database, 3, 5*time.Minute)
	oldRows, youngRows := storedRows(t, database, old), storedRows(t, database, young)

	service.compactionSweepTick(ctx, excerptLeases(t, database))

	if got := storedRows(t, database, old); len(got) != 1 || got[0].version != oldRows[len(oldRows)-1].version {
		t.Fatalf("the old document holds rows %v, want the head %d alone", rowVersions(got), oldRows[len(oldRows)-1].version)
	}
	requireRowsAsStored(t, database, young, youngRows)
}

// The sweep's list of documents to compact holds every document with more than one row and no
// other: a document of one row, or of none, has nothing a compaction could fold.
func TestTheCompactionSweepListsOnlyDocumentsOfMoreThanOneRow(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	storeAgedRows(t, database, 1, time.Hour)
	createDocument(t, database, "none")
	two := storeAgedRows(t, database, 2, 0)
	three := storeAgedRows(t, database, 3, time.Hour)
	pool, err := database.Pool.Compaction()
	if err != nil {
		t.Fatalf("open the compaction pool: %v", err)
	}

	listed, err := compactionCandidates(ctx, pool)
	if err != nil {
		t.Fatalf("list the documents to compact: %v", err)
	}
	slices.Sort(listed)
	want := []string{two, three}
	slices.Sort(want)
	if !slices.Equal(listed, want) {
		t.Fatalf("the sweep lists %v, want the documents of two and three rows %v", listed, want)
	}
}

// A document whose lock another holder has when the sweep reaches it is handed to the service's
// compaction queue, not waited for: the pass goes on and compacts the next document, and the queue
// compacts the busy one once its lock is free.
func TestTheCompactionSweepHandsABusyDocumentToTheQueue(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	steps := quickCompactionQueue(service, 20*time.Millisecond)
	busy := storeAgedRows(t, database, 3, 20*time.Minute)
	free := storeAgedRows(t, database, 3, 20*time.Minute)
	busyRows := storedRows(t, database, busy)
	unlock := holdDocumentLock(t, database, busy)

	service.compactionSweepTick(ctx, excerptLeases(t, database))

	if got := storedRows(t, database, free); len(got) != 1 {
		t.Fatalf("the free document holds rows %v after the sweep, want it folded into one", rowVersions(got))
	}
	steps.await(t, busy, 0, "the queue to take the busy document", func(step compactionStepRecord) bool {
		return step.next.kind == compactionRetry
	})
	requireRowsAsStored(t, database, busy, busyRows)
	from := steps.count()
	unlock()
	steps.await(t, busy, from, "the queue to compact the document once its lock is free", func(step compactionStepRecord) bool {
		return step.next.kind == compactionAgain
	})
	if got := storedRows(t, database, busy); len(got) != 1 || got[0].version != busyRows[len(busyRows)-1].version {
		t.Fatalf("the busy document holds rows %v, want the head %d alone", rowVersions(got), busyRows[len(busyRows)-1].version)
	}
}

// waitOutTheSweepHour returns with at least half a minute of the database clock's aligned hour
// left, so two sweep ticks a test makes one after the other fall in one period (duty.Exclusive).
func waitOutTheSweepHour(t *testing.T, database *store.Store) {
	t.Helper()
	var left time.Duration
	if err := database.Pool.QueryRow(context.Background(),
		`select make_interval(secs => $1 - mod(extract(epoch from now()), $1))`, compactionSweepPeriod.Seconds(),
	).Scan(&left); err != nil {
		t.Fatalf("read how much of the hour is left: %v", err)
	}
	if left < 30*time.Second {
		time.Sleep(left + time.Second)
	}
}
