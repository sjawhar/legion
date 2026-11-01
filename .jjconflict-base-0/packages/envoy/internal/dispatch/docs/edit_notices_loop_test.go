package docs

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/pglisten"
)

// newEditNoticeSweeper is a second document service on database that only sweeps, as another
// process's would: given a traced store, every statement its loop makes is counted apart from the
// settlements the test drives through its own service.
func newEditNoticeSweeper(t *testing.T, database *store.Store) *Service {
	t.Helper()
	service := New(Deps{Store: database, Events: events.NewBroker(), ServerURL: "https://dispatch.example", Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	return service
}

// runEditNoticeLoop runs the sweep until the test ends, and fails the test if it does not return
// promptly once its context ends.
func runEditNoticeLoop(t *testing.T, run func(ctx context.Context)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the edit notice sweep did not return within 10s of its context ending")
		}
	})
}

// startEditNoticeHub runs a real pglisten.Hub on database's listener with the sweep's channel
// registered, until the test ends, and returns its wake once the hub has connected.
func startEditNoticeHub(t *testing.T, database *store.Store) <-chan struct{} {
	t.Helper()
	hub := &pglisten.Hub{Owner: "edit notices test", Dial: database.Pool.Listener}
	wake := hub.Wake(EditNoticesChannel)
	connected := make(chan struct{})
	hub.OnConnected(sync.OnceFunc(func() { close(connected) }))
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		hub.Run(ctx)
	}()
	t.Cleanup(func() {
		stop()
		<-done
	})
	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		t.Fatal("the hub did not connect within 10s")
	}
	return wake
}

// With no notice due - one held, naming nobody, and one an hour off - the sweep makes no statement
// at all after its boot pass, over a window longer than the five-second period of the timer it
// replaced: no sweep on a timer and no lease attempt. Its pass asks to run again at the hour-off
// notice's due time, the one timer it keeps.
func TestEditNoticeSweepMakesNoStatementWithNothingDue(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	held := createIssueDocument(t, database, 1, "Held.")
	later := createIssueDocument(t, database, 2, "Later.")
	if _, err := database.Pool.Exec(ctx, `
		insert into document_edit_notices (artifact_id, from_version, to_version, editors, last_editor, due_at)
		values ($1, 1, 2, '["alice@example.com"]', null, null),
		       ($2, 1, 2, '["alice@example.com"]', '{"kind":"user","id":"alice@example.com"}', now() + interval '1 hour')
	`, held, later); err != nil {
		t.Fatalf("open a held notice and one due in an hour: %v", err)
	}
	counter := &storetest.StatementCounter{}
	counted := &store.Store{Pool: storetest.Traced(t, database.Pool, counter, nil)}
	sweeper := newEditNoticeSweeper(t, counted)
	leases := excerptLeases(t, counted)
	runEditNoticeLoop(t, func(ctx context.Context) { sweeper.RunEditNotices(ctx, leases, nil) })

	storetest.AwaitQuiet(t, counter, time.Second, 10*time.Second)
	if counter.Count() == 0 {
		t.Fatal("the sweep made no statement at all, so it never ran its boot pass")
	}
	storetest.RequireNoStatements(t, counter, editNoticeSweepSchedule.minGap+2*time.Second, "the edit notice sweep with nothing due")
	next := sweeper.editNoticePass(ctx)
	if until := time.Until(next); until < 59*time.Minute || until > time.Hour {
		t.Fatalf("a pass with only an hour-off notice asked to run again in %v; want at that notice's due time, an hour off", until)
	}
	if edits := len(humanEdits(t, database, held)) + len(humanEdits(t, database, later)); edits != 0 {
		t.Fatalf("the sweep sent %d notices; want none, nothing being due", edits)
	}
}

// A person's settled edit while the sweep is idle wakes it through the notice's notification on a
// real pglisten.Hub; that sweep finds nothing due yet, arms one timer for the notice's due time and
// makes no statement until then, and the notice goes once its quiet window ends, not before. The
// loop had no due time before the edit, so only the wake can have scheduled the sweep that sends
// it. The sweep here starts passes 200 ms apart, not five seconds.
func TestEditNoticeSweepSendsAWokenNoticeAtItsDueTime(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	const quiet = 3 * time.Second
	service.editNoticeQuiet = quiet
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)

	wake := startEditNoticeHub(t, service.store)
	counter := &storetest.StatementCounter{}
	counted := &store.Store{Pool: storetest.Traced(t, service.store.Pool, counter, nil)}
	sweeper := newEditNoticeSweeper(t, counted)
	leases := excerptLeases(t, counted)
	schedule := editNoticeSchedule{minGap: 200 * time.Millisecond}
	runEditNoticeLoop(t, func(ctx context.Context) { sweeper.runEditNotices(ctx, leases, wake, schedule) })
	// The boot pass and the one the hub's signal after its LISTEN earns, then nothing.
	storetest.AwaitQuiet(t, counter, 500*time.Millisecond, 10*time.Second)

	woken := counter.Count()
	alice := model.Actor{Kind: "user", ID: "alice@example.com"}
	editAsConnectedPeer(t, service, artifactID, 1, alice, replaceRun("First.", "First, edited."))
	settleCurrentGeneration(t, service, artifactID)
	row, open := readEditNotice(t, service, artifactID)
	if !open {
		t.Fatal("alice's settled edit opened no edit notice")
	}
	due := time.Now().Add(row.dueAt.Sub(row.now))
	waitFor(t, 5*time.Second, "the notice's wake to run a sweep", func() bool { return counter.Count() > woken })
	storetest.AwaitQuiet(t, counter, 300*time.Millisecond, 5*time.Second)
	if edits := humanEdits(t, service.store, artifactID); len(edits) != 0 {
		t.Fatalf("the woken sweep sent %d notices inside the quiet window; want none before it ends", len(edits))
	}
	// Until shortly before the due time the sweep holds its timer and asks nothing.
	if idle := time.Until(due) - 500*time.Millisecond; idle > 0 {
		storetest.RequireNoStatements(t, counter, idle, "the edit notice sweep waiting for a notice's due time")
	}
	if edits := humanEdits(t, service.store, artifactID); len(edits) != 0 {
		t.Fatalf("the sweep sent %d notices before the quiet window ended; want none", len(edits))
	}

	waitFor(t, quiet+5*time.Second, "the notice to go once its quiet window ended", func() bool {
		return len(humanEdits(t, service.store, artifactID)) == 1
	})
	var sentAt time.Time
	if err := service.store.Pool.QueryRow(context.Background(), `
		select created_at from events where type = 'artifact.human_edited' and payload->>'artifact_id' = $1
	`, artifactID).Scan(&sentAt); err != nil {
		t.Fatalf("read when the notice went: %v", err)
	}
	if late := sentAt.Sub(row.dueAt); late < 0 || late > schedule.minGap+time.Second {
		t.Fatalf("the notice went %v after its due time; want at it, within the sweep's gap and a second", late)
	}
	if edit := humanEdits(t, service.store, artifactID)[0]; !edit.actor.SameAs(alice) {
		t.Fatalf("the notice names %+v; want alice", edit.actor)
	}
}

// A sweep that finds the edit-notices lease held - the other process's sweep inside its pass -
// keeps trying, every gap plus up to a tenth, until a sweep of its own runs, and that sweep sends
// the due notice: the other process's pass may have read before it was due.
func TestEditNoticeSweepRetriesAHeldLease(t *testing.T) {
	database := storetest.Open(t)
	document := createIssueDocument(t, database, 1, "First.")
	addPersonsVersion(t, database, document, 2, "Second.\n", model.Actor{Kind: "user", ID: "alice@example.com"})
	logs := captureLogs(t)
	leases := excerptLeases(t, database)
	taken, release := make(chan struct{}), make(chan struct{})
	held := make(chan bool, 1)
	go func() {
		held <- duty.Exclusive(context.Background(), leases, editNoticeLease, time.Time{}, 0, func(context.Context) {
			close(taken)
			<-release
		})
	}()
	select {
	case <-taken:
	case <-time.After(20 * time.Second):
		t.Fatal("the other caller never took the edit-notices lease")
	}

	sweeper := newEditNoticeSweeper(t, database)
	runEditNoticeLoop(t, func(ctx context.Context) {
		sweeper.runEditNotices(ctx, leases, nil, editNoticeSchedule{minGap: 100 * time.Millisecond})
	})
	waitFor(t, 20*time.Second, "the sweep to find the lease held twice", func() bool {
		return countExcerptLeaseLines(logs, "dispatch duty: leased pass skipped", editNoticeLease) >= 2
	})
	if edits := humanEdits(t, database, document); len(edits) != 0 {
		t.Fatalf("the sweep sent %d notices while the lease was held elsewhere; want none", len(edits))
	}
	close(release)
	if !<-held {
		t.Fatal("the other caller never held the lease, so the sweep was free to run")
	}
	waitFor(t, 10*time.Second, "the sweep to send the due notice once the lease was free", func() bool {
		return len(humanEdits(t, database, document)) == 1
	})
}
