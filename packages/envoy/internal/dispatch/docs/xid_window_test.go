package docs

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The document service's own write transactions under storetest's xid rig (INFRA-636, plan test
// T1): settlement's version write, the edit-notice sweep and the task-progress reconciliation run
// no HTTP route, so these tests run them directly under a marked context (storetest.MarkXid) on
// documents the size of the load harness's large specs, and judge each transaction as the API's
// xid window tests judge a route's.
//
// Two background transactions of the plan's scope have no test here. The block-index backfill's
// owed arms (ReconcileBlockIndex) keep D6 at base on this fixture - their largest gap is about
// 3 ms and their gaps a fraction of their statements, even with hundreds of references - so a
// test of them could not be red now; the tick's window arm, which writes in transactions of its
// own, is judged below. RestoreDecidedWordings does no document work under its xid (only
// refs.ReplaceCounted on short questions), and reaching it needs the reworded state an earlier
// release leaves.

// xidService is a document service on a traced pool of a fresh database, settling after an hour so
// only the settlement a test runs itself settles.
func xidService(t *testing.T) (*Service, *store.Store, *storetest.XidTracer) {
	t.Helper()
	tracer := storetest.NewXidTracer()
	database := &store.Store{Pool: storetest.Traced(t, storetest.Open(t).Pool, tracer, nil)}
	service := New(Deps{
		Store:     database,
		Events:    events.NewBroker(),
		Identity:  headerIdentity(database),
		ServerURL: "https://dispatch.example",
		Settle:    time.Hour,
	})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	return service, database, tracer
}

// The rig is not vacuous: a transaction whose statements run back to back after its first write
// keeps D6 even when it spent long in Go before that write, since a transaction holds no xid until
// a statement gives it one.
func TestXidWindowRigPassesBackToBackStatementsAfterTheFirstWrite(t *testing.T) {
	_, database, tracer := xidService(t)
	report := tracer.Measure(t, database.Pool, func() {
		runRigTransaction(t, database, 0)
	})
	if err := report.Err(); err != nil {
		t.Fatalf("a back-to-back transaction broke D6: %v", err)
	}
	if len(report.Windows) != 1 {
		t.Fatalf("windows = %d, want the one transaction", len(report.Windows))
	}
	window := report.Windows[0]
	if !strings.Contains(window.Statements[0].SQL, "insert into projects") {
		t.Fatalf("the window starts at %q, want the insert that gave the transaction its xid:\n%s", window.Statements[0].SQL, window.Detail())
	}
	t.Logf("%s\n%s", window.Summary(), window.Detail())
}

// rigInjectedGap is the statement-free gap the deliberate-fault self-tests put inside the window:
// the fault gap of the build they run in (storetest.XidInjectedFaultGap), a multiple of that
// build's bound for a window whose statements are as short as this transaction's. A sleep cannot
// run shorter than it asks for, so it is also the floor that bound must stay under: were the
// plain bound or the race floor ever raised to it, these self-tests would report no violation and
// would say so.
var rigInjectedGap = storetest.XidInjectedFaultGap()

// The rig sees a gap inside the window: the same transaction with Go work between its write and
// its commit breaks D6, and the report names the statement the gap follows. The gap is injected
// behind the rig transaction's own short statements and is a multiple of what either build allows
// there, so this test says the same thing under the race detector as without it; a fault the size
// of the scheduler's own stalls would only say which build it ran in.
func TestXidWindowRigCatchesAGapAfterTheFirstWrite(t *testing.T) {
	_, database, tracer := xidService(t)
	report := tracer.Measure(t, database.Pool, func() {
		runRigTransaction(t, database, rigInjectedGap)
	})
	err := report.Err()
	if err == nil || len(report.Problems) > 0 || !strings.Contains(err.Error(), "XID WINDOW VIOLATION") ||
		!strings.Contains(err.Error(), "follows statement 0: insert into projects") {
		t.Fatalf("a %s gap after the write: verdict %v, want a D6 violation after the insert", rigInjectedGap, err)
	}
	t.Logf("the rig's verdict on a %s gap after the write:\n%v", rigInjectedGap, err)
}

// The control on every build's bound: the fault this change removed - a document call inside the
// window - still breaks D6 in a plain build and under the race detector alike. The call is the
// parse every write route does on the text it writes (parseInput), on a document the size of the
// load harness's large specs, and the sleep it replaces is gone: nothing here waits. It is a long
// gap behind about 3 ms of statements, so the race build judges it against its floor however slow
// the box is, and a build whose bound reached what that parse costs would let the real fault
// through: this is the test that would go red first if either bound were ever widened too far.
func TestXidWindowCatchesADocumentCallInsideTheWindow(t *testing.T) {
	_, database, tracer := xidService(t)
	spec, _ := storetest.XidSpec("DOC", 1)
	report := tracer.Measure(t, database.Pool, func() {
		runRigTransactionDoing(t, database, func(context.Context, pgx.Tx) {
			if _, err := parseInput(spec); err != nil {
				t.Errorf("parse the %d-byte spec: %v", len(spec), err)
			}
		})
	})
	err := report.Err()
	if err == nil || len(report.Problems) > 0 || !strings.Contains(err.Error(), "XID WINDOW VIOLATION") ||
		!strings.Contains(err.Error(), "follows statement 0: insert into projects") {
		t.Fatalf("a parse of the %d-byte spec after the write: verdict %v, want a D6 violation after the insert",
			len(spec), err)
	}
	t.Logf("the rig's verdict on a parse of the %d-byte spec after the write:\n%v", len(spec), err)
}

// rigLongStatement is how long the proportionate-stall test below makes one statement of its
// window run inside Postgres, and rigProportionateStall the Go stall it then puts behind it. The
// stall stands above the race build's floor (storetest.XidRaceFloorGap), so the floor cannot be
// what lets it pass, and far below that build's share of a window this long, so the stretch the
// race detector puts on a sleep - up to six times its length in this plan's measurements - cannot
// carry it over either.
const (
	rigLongStatement      = 1500 * time.Millisecond
	rigProportionateStall = 50 * time.Millisecond
)

// The race build's rule read the other way round: a statement-free gap that is a modest share of
// a long batch is the race detector's own scheduling and not document work, so the race build
// allows it, while the plain build, whose bound is D6's own 5 ms whatever the window holds,
// refuses it. The window's statements are made long by a statement and not by Go: a sleep
// Postgres runs is time the transaction spends inside a statement, which is what the share is
// taken of.
func TestXidWindowRigPassesALongBatchWithAProportionateStall(t *testing.T) {
	_, database, tracer := xidService(t)
	report := tracer.Measure(t, database.Pool, func() {
		runRigTransactionDoing(t, database, func(ctx context.Context, tx pgx.Tx) {
			if _, err := tx.Exec(ctx, `select pg_sleep($1)`, rigLongStatement.Seconds()); err != nil {
				t.Errorf("the long statement: %v", err)
			}
			time.Sleep(rigProportionateStall)
		})
	})
	if len(report.Problems) > 0 || len(report.Windows) != 1 {
		t.Fatalf("windows = %d, problems = %v, want the one transaction judged", len(report.Windows), report.Problems)
	}
	window := report.Windows[0]
	if window.StatementTime < rigLongStatement {
		t.Fatalf("the window's statements took %s, under the %s the long statement alone should take, so this run tested no long batch:\n%s",
			storetest.XidMillis(window.StatementTime), rigLongStatement, window.Detail())
	}
	err := report.Err()
	switch proportionate := storetest.XidGapAllowanceFor(rigLongStatement) > rigProportionateStall; {
	case proportionate && err != nil:
		t.Fatalf("a %s stall behind %s of statements broke the race build's rule, which allows %s here: %v",
			rigProportionateStall, storetest.XidMillis(window.StatementTime),
			storetest.XidMillis(storetest.XidGapAllowanceFor(window.StatementTime)), err)
	case !proportionate && (err == nil || !strings.Contains(err.Error(), "XID WINDOW VIOLATION")):
		t.Fatalf("a %s stall behind %s of statements: verdict %v, want the plain build's fixed %s to refuse it",
			rigProportionateStall, storetest.XidMillis(window.StatementTime), err,
			storetest.XidMillis(storetest.XidMaxGap))
	}
	t.Logf("%s\n%s", window.Summary(), window.Detail())
}

// A write phase that is one batch can hold its xid for less than the poller's interval, which on a
// loaded two-core box reaches tens of milliseconds, so the poller can miss the transaction
// altogether. Such a transaction is still judged, from the window its own statements derive - the
// first one whose result shows it wrote - and never reported as a rig problem; a sleep after that
// write is caught there as it is under a poller that saw the xid.
func TestXidWindowRigJudgesAWriteThePollerNeverSaw(t *testing.T) {
	for _, probe := range []struct {
		name string
		gap  time.Duration
	}{
		{name: "back to back", gap: 0},
		{name: fmt.Sprintf("a %s sleep after the write", rigInjectedGap), gap: rigInjectedGap},
	} {
		t.Run(probe.name, func(t *testing.T) {
			_, database, tracer := xidService(t)
			// Blind for longer than the whole operation: no poll is sent while the transaction is
			// open, so nothing but its own statements says it held an xid.
			tracer.BlindPoller(time.Minute)
			report := tracer.Measure(t, database.Pool, func() {
				runRigTransaction(t, database, probe.gap)
			})
			if len(report.Problems) > 0 {
				t.Fatalf("the rig reported a problem about a transaction no poll saw: %v", report.Problems)
			}
			if len(report.Windows) != 1 {
				t.Fatalf("windows = %d, unsighted = %d, want the one transaction judged",
					len(report.Windows), len(report.Unsighted))
			}
			window := report.Windows[0]
			if !window.Transaction.Sighted.IsZero() {
				t.Fatalf("a poll saw the xid at %s, so this run did not test an unsighted transaction",
					window.Transaction.Sighted)
			}
			if !strings.Contains(window.Statements[0].SQL, "insert into projects") {
				t.Fatalf("the window starts at %q, want the insert that gave the transaction its xid:\n%s",
					window.Statements[0].SQL, window.Detail())
			}
			err := report.Err()
			switch {
			case probe.gap == 0 && err != nil:
				t.Fatalf("a back-to-back transaction the poller never saw broke D6: %v", err)
			case probe.gap > 0 && (err == nil || !strings.Contains(err.Error(), "XID WINDOW VIOLATION") ||
				!strings.Contains(err.Error(), "follows statement 0: insert into projects")):
				t.Fatalf("a %s gap after the write: verdict %v, want a D6 violation after the insert", probe.gap, err)
			}
			t.Logf("%s\n%s", window.Summary(), window.Detail())
		})
	}
}

// runRigTransaction runs, under a marked context, a transaction that reads, spends 20 ms in Go
// before its first write, writes, waits gap, reads twice more and commits.
func runRigTransaction(t *testing.T, database *store.Store, gap time.Duration) {
	t.Helper()
	runRigTransactionDoing(t, database, func(context.Context, pgx.Tx) { time.Sleep(gap) })
}

// runRigTransactionDoing is runRigTransaction with between run after the write instead of a
// sleep: what the transaction does while it holds its transaction id, in Go or in a statement of
// its own, which it runs on the transaction it is handed.
func runRigTransactionDoing(t *testing.T, database *store.Store, between func(ctx context.Context, tx pgx.Tx)) {
	t.Helper()
	ctx := storetest.MarkXid(context.Background())
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	var count int
	if err := tx.QueryRow(ctx, `select count(*) from projects`).Scan(&count); err != nil {
		t.Fatalf("read before the write: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := tx.Exec(ctx, `insert into projects (key, name) values ('RIG', 'Rig')`); err != nil {
		t.Fatalf("write: %v", err)
	}
	between(ctx, tx)
	for range 2 {
		if err := tx.QueryRow(ctx, `select count(*) from projects`).Scan(&count); err != nil {
			t.Fatalf("read after the write: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// Settlement versions a document a person edited live: it renders the room, refreshes every open
// anchor against the new tree and writes the version, its anchors and its events in one
// transaction (settleRoomWithin).
func TestXidWindowSettlementVersionWrite(t *testing.T) {
	service, database, tracer := xidService(t)
	spec, quotes := storetest.XidSpec("DOC", 1)
	artifactID := createDocument(t, database, "# First")
	seedServiceText(t, service, artifactID, spec)
	step := len(quotes) / 24
	for i := range 20 {
		seedAnchoredAsk(t, service, artifactID, fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1), quotes[i*step], quotes[i*step])
	}
	alice := model.Actor{Kind: "user", ID: "alice@example.com"}
	editAsConnectedPeer(t, service, artifactID, 1, alice, replaceRun("Revision token r0", "Revision token r1"))
	before := latestVersionNumber(t, service, artifactID)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := service.waitForPendingUpdates(ctx, artifactID); err != nil {
		t.Fatalf("wait for live updates to reach persistence: %v", err)
	}
	if err := service.waitForDurableAppends(ctx, artifactID); err != nil {
		t.Fatalf("wait for live updates to become durable: %v", err)
	}
	state := service.room(artifactID)
	state.mu.Lock()
	generation := state.roomGeneration
	state.mu.Unlock()
	t.Logf("xid fixture: document of %d bytes with 20 open anchored asks", len(spec))

	report := tracer.Measure(t, database.Pool, func() {
		service.settleRoomWithin(storetest.MarkXid(context.Background()), artifactID, generation)
	})
	if after := latestVersionNumber(t, service, artifactID); after != before+1 {
		t.Fatalf("XID RIG PROBLEM (not a finding about the code): settlement left the document at version %d, want %d", after, before+1)
	}
	storetest.AssertXidWindows(t, report)
}

// The edit-notice sweep claims a due notice, then reads both versions it names, parses them to
// tell a server-state-only change from a person's edit and diffs them (sendEditNotice).
func TestXidWindowEditNoticeSweep(t *testing.T) {
	service, database, tracer := xidService(t)
	spec, quotes := storetest.XidSpec("DOC", 1)
	document := createIssueDocument(t, database, 2, spec)
	revised := versionMarkdown(t, database, document, 1)
	revised = strings.Replace(revised, "Revision token r0", "Revision token r1", 1)
	for _, quote := range []string{quotes[10], quotes[len(quotes)/2], quotes[len(quotes)-10]} {
		revised = strings.Replace(revised, quote, strings.Replace(quote, "holds", "stands", 1), 1)
	}
	addPersonsVersion(t, database, document, 2, revised, model.Actor{Kind: "user", ID: "alice@example.com"})
	t.Logf("xid fixture: two versions of %d and %d bytes", len(spec), len(revised))

	var sent int
	report := tracer.Measure(t, database.Pool, func() {
		var err error
		sent, err = service.sendDueEditNotices(storetest.MarkXid(context.Background()))
		if err != nil {
			t.Errorf("sweep: %v", err)
		}
	})
	if sent != 1 {
		t.Fatalf("XID RIG PROBLEM (not a finding about the code): the sweep sent %d notices, want 1", sent)
	}
	storetest.AssertXidWindows(t, report)
}

// The task-progress reconciliation counts the tasks of each issue whose recorded count is not of
// its spec's latest version, one batch of issues per transaction, holding their rows: twenty issues
// with large specs, as a deploy whose overlap wrote versions through code that records no count
// leaves them.
func TestXidWindowReconcileTaskProgress(t *testing.T) {
	service, database, tracer := xidService(t)
	spec, _ := storetest.XidSpec("DOC", 1)
	const issues = 20
	for number := 1; number <= issues; number++ {
		document := createIssueDocument(t, database, number, spec)
		if _, err := database.Pool.Exec(context.Background(), `update artifacts set is_primary = true where id = $1`, document); err != nil {
			t.Fatalf("make document %d its issue's spec: %v", number, err)
		}
	}
	t.Logf("xid fixture: %d issues whose specs of %d bytes have no task count", issues, len(spec))

	report := tracer.Measure(t, database.Pool, func() {
		service.ReconcileTaskProgress(storetest.MarkXid(context.Background()))
	})
	var drifted int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from issues i where `+taskProgressDrift).Scan(&drifted); err != nil {
		t.Fatalf("count drifted issues: %v", err)
	}
	if drifted != 0 {
		t.Fatalf("XID RIG PROBLEM (not a finding about the code): %d issues still have no count of their spec's latest version", drifted)
	}
	storetest.AssertXidWindows(t, report)
}

// The block-index tick's window arm gives windows to the owners whose blocks carry none: a
// document in a transaction of its own, which reads the document the server serves before it
// takes the owner row (windowDocument), and a comment in one of its own, which reads the body
// its blocks were split from under the owner's lock and takes no row lock (windowItem). Either
// way the whole write is the one statement that writes the windows, so the window is that
// statement and the commit after it.
func TestXidWindowBlockIndexWindowArm(t *testing.T) {
	service, database, tracer := xidService(t)
	ctx := context.Background()
	spec, _ := storetest.XidSpec("DOC", 1)
	document := createIssueDocument(t, database, 1, spec)
	// The live text, which the index and the window arm both read: a document whose room holds
	// none is indexed with no block, and a block index with no block owes no window.
	seedServiceText(t, service, document, spec)
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (issue_key, author, body) values ('DOC-1', '{"kind":"user","id":"alice"}', $1)
	`, spec); err != nil {
		t.Fatalf("write the comment: %v", err)
	}
	// Ticks until nothing is owed index the blocks of both and write their windows; dropping
	// those rows leaves each owed its windows and nothing else, which is what the arm repairs.
	for range 5 {
		tick, err := service.ReconcileBlockIndex(ctx, BlockIndexCursors{}, liveAheadDocuments{})
		if err != nil {
			t.Fatalf("index the blocks of the document and the comment: %v", err)
		}
		if tick.Documents == 0 && tick.ItemBlocks == 0 {
			break
		}
	}
	if _, err := database.Pool.Exec(ctx, `delete from embedding_window_states; delete from embeddings`); err != nil {
		t.Fatalf("drop the windows: %v", err)
	}
	owedDocuments, err := refs.DocumentsOwedWindows(ctx, database.Pool, "", 10)
	if err != nil {
		t.Fatalf("list the documents owed their windows: %v", err)
	}
	owedComments, err := refs.ItemsOwedWindows(ctx, database.Pool, refs.ItemComment, "", 10)
	if err != nil {
		t.Fatalf("list the comments owed their windows: %v", err)
	}
	if len(owedDocuments) != 1 || len(owedComments) != 1 {
		t.Fatalf("XID RIG PROBLEM (not a finding about the code): %d documents and %d comments are owed their windows, want one of each",
			len(owedDocuments), len(owedComments))
	}
	t.Logf("xid fixture: a document and a comment of %d bytes whose blocks hold no window", len(spec))

	var tick BlockIndexTick
	report := tracer.Measure(t, database.Pool, func() {
		var err error
		tick, err = service.ReconcileBlockIndex(storetest.MarkXid(ctx), BlockIndexCursors{}, liveAheadDocuments{})
		if err != nil {
			t.Errorf("window the owners whose blocks hold none: %v", err)
		}
	})
	if tick.Windows != 2 {
		t.Fatalf("XID RIG PROBLEM (not a finding about the code): the tick windowed %d owners, want the document and the comment", tick.Windows)
	}
	var windows int
	if err := database.Pool.QueryRow(ctx,
		`select count(*) from embeddings where split_part(id, '#', 1) = $1`, document).Scan(&windows); err != nil {
		t.Fatalf("count the document's windows: %v", err)
	}
	if windows == 0 {
		t.Fatalf("XID RIG PROBLEM (not a finding about the code): the arm left the document with no window")
	}
	storetest.AssertXidWindows(t, report)
}
