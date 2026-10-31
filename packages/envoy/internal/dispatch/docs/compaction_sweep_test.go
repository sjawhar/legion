package docs

import (
	"context"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
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
	ageUpdatesThrough(t, database, artifactID, foldPoint, time.Hour)
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

// The sweep's list of documents to compact names a document once an append gives it a row after
// its first, and no other: a document of one row, or of none, has nothing a compaction could fold,
// and the append of a document's first row names nothing.
func TestTheCompactionSweepListsADocumentOnceAnAppendGivesItASecondRow(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	one := storeAgedRows(t, database, 1, time.Hour)
	createDocument(t, database, "none")
	if got := owedDocuments(t, database); len(got) != 0 {
		t.Fatalf("the sweep lists %v after documents of one row and of none, want nothing", got)
	}
	two := storeAgedRows(t, database, 2, 0)
	three := storeAgedRows(t, database, 3, time.Hour)
	requireOwed(t, database, "the documents of two and three rows", two, three)
	appendUpdates(t, NewPgVersioned(database), one, newProofHistory(t, 202).write("Second.\n"))
	requireOwed(t, database, "the documents of two and three rows and the one given its second", one, two, three)

	pool, err := database.Pool.Compaction()
	if err != nil {
		t.Fatalf("open the compaction pool: %v", err)
	}
	listed, err := compactionCandidates(ctx, pool)
	if err != nil {
		t.Fatalf("list the documents to compact: %v", err)
	}
	slices.Sort(listed)
	if want := owedDocuments(t, database); !slices.Equal(listed, want) {
		t.Fatalf("the sweep's candidates are %v, want the documents doc_compaction_owed names %v", listed, want)
	}
}

// A compaction takes its document off the sweep's list only when it leaves no row above its fold
// point: one that folds every row, all of them old, takes it off, and one that leaves a row younger
// than the tail above the fold point keeps it there until a later compaction folds that row too.
func TestACompactionTakesItsDocumentOffTheSweepsListOnceNoRowStaysAboveTheFoldPoint(t *testing.T) {
	database := storetest.Open(t)
	versioned := NewPgVersioned(database)
	whole := storeAgedRows(t, database, 3, time.Hour)
	tail := storeAgedRows(t, database, 3, 0)
	ageUpdatesThrough(t, database, tail, 2, time.Hour)
	requireOwed(t, database, "both documents", whole, tail)

	if result := compactUnderTail(t, versioned, whole); result.outcome != compactCompacted {
		t.Fatalf("the whole compaction ended %v, want it compacted", result.outcome)
	}
	if result := compactUnderTail(t, versioned, tail); result.outcome != compactCompacted {
		t.Fatalf("the tail compaction ended %v, want it compacted", result.outcome)
	}
	if got := storedRows(t, database, tail); len(got) != 2 || got[0].version != 2 {
		t.Fatalf("the tail compaction left rows %v, want the fold point 2 and the young row above it", rowVersions(got))
	}
	requireOwed(t, database, "the document whose young row stays above its fold point", tail)

	if result := compactAged(t, versioned, database, tail); result.outcome != compactCompacted {
		t.Fatalf("the later compaction ended %v, want it compacted", result.outcome)
	}
	requireOwed(t, database, "nothing once the young row folded too")
}

// A document's deletion and its rebuild, which leaves it one row, each take it off the sweep's
// list in their own transaction; the rebuilt document's next append names it again.
func TestDeleteAndRebuildTakeTheDocumentOffTheSweepsList(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	versioned := NewPgVersioned(database)
	deleted := storeAgedRows(t, database, 2, time.Hour)
	rebuilt := storeAgedRows(t, database, 3, time.Hour)
	requireOwed(t, database, "both documents", deleted, rebuilt)

	if err := versioned.Delete(ctx, deleted); err != nil {
		t.Fatalf("delete the document: %v", err)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the rebuild: %v", err)
	}
	defer tx.Rollback(ctx)
	writer := newProofHistory(t, 303)
	if _, err := versioned.RebuildTx(ctx, tx, rebuilt, writer.write("Rebuilt.\n")); err != nil {
		t.Fatalf("rebuild the document: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the rebuild: %v", err)
	}
	if got := storedRows(t, database, rebuilt); len(got) != 1 {
		t.Fatalf("the rebuilt document holds rows %v, want its seed alone", rowVersions(got))
	}
	requireOwed(t, database, "nothing after the delete and the rebuild")

	appendUpdates(t, versioned, rebuilt, writer.write("Rebuilt.\n\nAgain.\n"))
	requireOwed(t, database, "the rebuilt document once an append follows its seed", rebuilt)
}

// The sweep takes off its list a document that holds no row after its first - one a prune left a
// single row, one whose rows went with its artifact - and keeps a document whose rows after its
// first are too young to be due, which it leaves as stored.
func TestTheCompactionSweepForgetsADocumentWithNoRowAfterItsFirst(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	pruned := storeAgedRows(t, database, 3, time.Hour)
	if err := NewPgVersioned(database).PruneAfter(ctx, pruned, 1, nil); err != nil {
		t.Fatalf("prune to version 1: %v", err)
	}
	emptied := storeAgedRows(t, database, 2, time.Hour)
	// What an artifact's deletion leaves: its rows go, and nothing deletes its entry.
	if _, err := database.Pool.Exec(ctx, `delete from doc_updates where artifact_id = $1`, emptied); err != nil {
		t.Fatalf("delete the document's rows: %v", err)
	}
	young := storeAgedRows(t, database, 3, 0)
	youngRows := storedRows(t, database, young)
	requireOwed(t, database, "every document an append gave a second row", pruned, emptied, young)

	service.compactionSweepTick(ctx, excerptLeases(t, database))

	requireOwed(t, database, "the document whose young rows are not yet due", young)
	requireRowsAsStored(t, database, young, youngRows)
}

// owedDocuments is every document doc_compaction_owed names, sorted.
func owedDocuments(t *testing.T, database *store.Store) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `select artifact_id::text from doc_compaction_owed`)
	if err != nil {
		t.Fatalf("read the documents owed a compaction: %v", err)
	}
	owed, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the documents owed a compaction: %v", err)
	}
	slices.Sort(owed)
	return owed
}

// requireOwed fails unless doc_compaction_owed names want and no other document.
func requireOwed(t *testing.T, database *store.Store, what string, want ...string) {
	t.Helper()
	want = slices.Clone(want)
	slices.Sort(want)
	if got := owedDocuments(t, database); !slices.Equal(got, want) {
		t.Fatalf("doc_compaction_owed names %v, want %s %v", got, what, want)
	}
}

// The hourly sweep is a timer that runs whether or not anything is due, so what it reads must not
// grow with the documents that have nothing to fold: a pass over a database of many documents of one
// row each and one due document reads that document's rows and no row of the others. A pass that
// lists its candidates by reading every document's rows (a scan of doc_updates, by its heap or its
// primary key) reads at least one row per document, every hour, on a table of production's size.
func TestTheCompactionSweepReadsNoRowOfADocumentThatIsNotDue(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	const notDue = 2000
	createDocuments(t, database, notDue)
	due := storeAgedRows(t, database, 3, 20*time.Minute)
	before := docUpdatesTuplesRead(t, database)

	service.compactionSweepTick(ctx, excerptLeases(t, database))

	if got := storedRows(t, database, due); len(got) != 1 {
		t.Fatalf("the due document holds rows %v after the sweep, want it folded into one", rowVersions(got))
	}
	if read := docUpdatesTuplesRead(t, database) - before; read >= notDue {
		t.Fatalf("the sweep read %d doc_updates rows to fold one document of 3 rows among %d documents with nothing to fold, want fewer than %d: it reads every document", read, notDue, notDue)
	}
}

// docUpdatesTuplesRead is how many doc_updates rows the database's backends have read, by a
// sequential scan or through any index of the table, once every compaction pool connection has
// flushed the statistics it holds: a backend flushes them when it goes idle after
// pg_stat_force_next_flush.
func docUpdatesTuplesRead(t *testing.T, database *store.Store) int64 {
	t.Helper()
	ctx := context.Background()
	pool, err := database.Pool.Compaction()
	if err != nil {
		t.Fatalf("open the compaction pool: %v", err)
	}
	var held []func()
	for range pool.Config().MaxConns {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire a compaction connection: %v", err)
		}
		if _, err := conn.Exec(ctx, `select pg_stat_force_next_flush()`); err != nil {
			t.Fatalf("flush a compaction connection's statistics: %v", err)
		}
		held = append(held, conn.Release)
	}
	for _, release := range held {
		release()
	}
	if _, err := database.Pool.Exec(ctx, `select pg_stat_force_next_flush()`); err != nil {
		t.Fatalf("flush the test connection's statistics: %v", err)
	}
	read := func() int64 {
		var n int64
		if err := database.Pool.QueryRow(ctx, `
			select coalesce(t.seq_tup_read, 0) + coalesce((
				select sum(i.idx_tup_read) from pg_stat_user_indexes i where i.relid = t.relid
			), 0)::bigint
			from pg_stat_user_tables t where t.relname = 'doc_updates'
		`).Scan(&n); err != nil {
			t.Fatalf("read doc_updates statistics: %v", err)
		}
		return n
	}
	// The flushes land as each connection goes idle; read until two reads agree.
	last := read()
	for range 20 {
		time.Sleep(100 * time.Millisecond)
		now := read()
		if now == last {
			return now
		}
		last = now
	}
	return last
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

// compactionSweepTick is one tick of the sweep, due now, as RunCompactionSweep's ticker makes one:
// the pass, when this process takes the lease and no process has swept this hour.
func (s *Service) compactionSweepTick(ctx context.Context, leases duty.Leases) {
	duty.Exclusive(ctx, leases, compactionSweepLease, time.Now(), compactionSweepPeriod, s.compactionSweepPass)
}
