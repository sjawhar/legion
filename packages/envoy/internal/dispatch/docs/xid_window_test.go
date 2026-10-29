package docs

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The document service's own write transactions under storetest's xid rig (INFRA-636, plan test
// T1): settlement's version write, the edit-notice sweep and the task-progress reconciliation run
// no HTTP route, so these tests run them directly under a marked context (storetest.MarkXid) on
// documents the size of the load harness's large specs, and judge each transaction as the API's
// xid window tests judge a route's.
//
// Two background transactions of the plan's scope have no test here. The block-index backfill
// (ReconcileBlockIndex) keeps D6 at base on this fixture - its largest gap is about 3 ms and its
// gaps a fraction of its statements, even with hundreds of references - so a test of it could not
// be red now. RestoreDecidedWordings does no document work under its xid (only refs.ReplaceCounted
// on short questions), and reaching it needs the reworded state an earlier release leaves.

// xidService is a document service on a traced pool of a fresh database, settling after an hour so
// only the settlement a test runs itself settles.
func xidService(t *testing.T) (*Service, *store.Store, *storetest.XidTracer) {
	t.Helper()
	tracer := storetest.NewXidTracer()
	database := storetest.Traced(t, storetest.Open(t), tracer)
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

// The rig sees a gap inside the window: the same transaction with Go work between its write and
// its commit breaks D6, and the report names the statement the gap follows.
func TestXidWindowRigCatchesAGapAfterTheFirstWrite(t *testing.T) {
	_, database, tracer := xidService(t)
	report := tracer.Measure(t, database.Pool, func() {
		runRigTransaction(t, database, 20*time.Millisecond)
	})
	err := report.Err()
	if err == nil || len(report.Problems) > 0 || !strings.Contains(err.Error(), "XID WINDOW VIOLATION") ||
		!strings.Contains(err.Error(), "follows statement 0: insert into projects") {
		t.Fatalf("a 20 ms gap after the write: verdict %v, want a D6 violation after the insert", err)
	}
	t.Logf("the rig's verdict on a 20 ms gap after the write:\n%v", err)
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
		{name: "a 20 ms sleep after the write", gap: 20 * time.Millisecond},
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
	time.Sleep(gap)
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
