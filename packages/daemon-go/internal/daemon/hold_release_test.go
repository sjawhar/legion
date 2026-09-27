package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// B1: pollHoldRelease is a boot-owned ticker, not a per-delivery hook, precisely because a
// per-delivery hook depends on a message arriving to trigger its own re-check — and Ack() does not
// wait for the server, so a check taken immediately after a delivery's own ack can still see the
// previous position. This seeds a root behind a real, empty JetStream stream (nothing is ever
// published, so the Dispatch consumer is idle from its very first read) and proves the ticker
// alone, on its own schedule, releases and admits it within a bound — never waiting for a message
// that will never come.
func TestPollHoldReleaseClosesAHeldRootOnAQuietStreamWithinABound(t *testing.T) {
	pool := isolatedOutboxPool(t)
	url := workflowNATS(t)
	conn, err := nats.Connect(url, nats.Timeout(time.Second))
	if err != nil {
		t.Fatalf("connect NATS: %v", err)
	}
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	consumers, err := intake.OpenConsumers(context.Background(), js, intake.ConsumerSpec{
		Project: "CAPTURE", Repositories: []ghrepo.Repository{ghrepo.MustParse("sjawhar/legion")}, AckWait: time.Second, NakDelay: time.Millisecond, Logger: log,
	})
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}

	engine := workflow.New(record.NewStore(), workflow.Config{Project: "CAPTURE"}, log)
	admission := admit.New(record.NewStore(), engine, 1, "CAPTURE", log)
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return record.NewStore().PutIssue(context.Background(), tx, record.Issue{
			Key: "LEGION-EDGE", Project: "CAPTURE", Title: "edge", Tree: "LEGION-EDGE", Phase: phase.Admitted,
			Generation: 1, Status: "todo", Rank: "A", HandedOver: true, LastDispatchSeq: 1,
		})
	}); err != nil {
		t.Fatalf("seed root: %v", err)
	}
	// target is far past anything an ack floor on this empty stream will ever reach: only idle —
	// a fresh consumer with nothing ever published to it — can close this hold.
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return admission.Reconcile(context.Background(), tx, []dispatch.IssueSummary{
			{Key: "LEGION-EDGE", Title: "edge", Status: "todo", Rank: "A", HandedOver: true, LastSeq: 2},
		}, 1000, intake.DispatchConsumerPosition{AckFloorStream: 0, Idle: false})
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !admission.Held() {
		t.Fatal("Held() = false after seeding a behind record, want it held")
	}

	w := &workflowRuntime{
		pool: pool, admission: admission, handlers: []intake.Handler{engine, admission},
		dispatchProject: "CAPTURE", bootID: "test-boot", log: log,
		holdPollInterval: 20 * time.Millisecond, consumers: consumers,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { w.pollHoldRelease(ctx); close(done) }()

	testwait.Eventually(t, "the held root releases on the quiet stream's idle ticker", func() bool { return !admission.Held() })
	cancel()
	<-done
}

// A failed DispatchPosition read must be retried by the poll on its own schedule, logged, never
// silently ending the hold: a reader that fails a fixed number of times before succeeding proves
// the poll neither gives up nor drops the hold on an error, and that it actually calls the reader
// more times than the injected failure count once it does release.
func TestPollHoldReleaseRetriesAFailedPositionRead(t *testing.T) {
	pool := isolatedOutboxPool(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: "CAPTURE"}, log)
	admission := admit.New(record.NewStore(), engine, 1, "CAPTURE", log)
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return record.NewStore().PutIssue(context.Background(), tx, record.Issue{
			Key: "LEGION-EDGE", Project: "CAPTURE", Title: "edge", Tree: "LEGION-EDGE", Phase: phase.Admitted,
			Generation: 1, Status: "todo", Rank: "A", HandedOver: true, LastDispatchSeq: 1,
		})
	}); err != nil {
		t.Fatalf("seed root: %v", err)
	}
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return admission.Reconcile(context.Background(), tx, []dispatch.IssueSummary{
			{Key: "LEGION-EDGE", Title: "edge", Status: "todo", Rank: "A", HandedOver: true, LastSeq: 2},
		}, 5, intake.DispatchConsumerPosition{AckFloorStream: 0, Idle: false})
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !admission.Held() {
		t.Fatal("Held() = false after seeding a behind record, want it held")
	}

	reader := &failingPositionReader{failures: 3, position: intake.DispatchConsumerPosition{AckFloorStream: 5}}
	w := &workflowRuntime{
		pool: pool, admission: admission, handlers: []intake.Handler{engine, admission},
		dispatchProject: "CAPTURE", bootID: "test-boot", log: log, holdPollInterval: 20 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { w.pollHoldReleaseWith(ctx, reader); close(done) }()

	testwait.Eventually(t, "the poll retries the failed read and eventually releases", func() bool { return !admission.Held() })
	cancel()
	<-done

	reader.mu.Lock()
	calls := reader.calls
	reader.mu.Unlock()
	if calls <= reader.failures {
		t.Fatalf("reader calls = %d, want more than the %d injected failures: the poll must retry", calls, reader.failures)
	}
}

// P2, thread 4114574429: pollHoldReleaseWith adopted a position as last before ApplyFact ran on
// it, so a failed apply of an unchanged reading was never retried — the next tick saw the same
// position and skipped it, and a quiet stream's position never changes again to unstick it. A
// handler that fails the position fact once, then succeeds, proves the poll retries the same
// reading rather than adopting it as last before the transaction that carries it has actually
// committed.
func TestPollHoldReleaseRetriesAFailedApplyOfAnUnchangedPosition(t *testing.T) {
	pool := isolatedOutboxPool(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: "CAPTURE"}, log)
	admission := admit.New(record.NewStore(), engine, 1, "CAPTURE", log)
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return record.NewStore().PutIssue(context.Background(), tx, record.Issue{
			Key: "LEGION-EDGE", Project: "CAPTURE", Title: "edge", Tree: "LEGION-EDGE", Phase: phase.Admitted,
			Generation: 1, Status: "todo", Rank: "A", HandedOver: true, LastDispatchSeq: 1,
		})
	}); err != nil {
		t.Fatalf("seed root: %v", err)
	}
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return admission.Reconcile(context.Background(), tx, []dispatch.IssueSummary{
			{Key: "LEGION-EDGE", Title: "edge", Status: "todo", Rank: "A", HandedOver: true, LastSeq: 2},
		}, 5, intake.DispatchConsumerPosition{AckFloorStream: 0, Idle: false})
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !admission.Held() {
		t.Fatal("Held() = false after seeding a behind record, want it held")
	}

	failing := &failingOnceHandler{}
	reader := &failingPositionReader{position: intake.DispatchConsumerPosition{AckFloorStream: 5, Idle: true}}
	w := &workflowRuntime{
		pool: pool, admission: admission, handlers: []intake.Handler{engine, admission, failing},
		dispatchProject: "CAPTURE", bootID: "test-boot", log: log, holdPollInterval: 20 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { w.pollHoldReleaseWith(ctx, reader); close(done) }()

	testwait.Eventually(t, "the poll retries the failed apply of an unchanged position and eventually releases", func() bool { return !admission.Held() })
	cancel()
	<-done
}

// failingOnceHandler fails ApplyFact's first call to it — rolling back that whole transaction,
// admission's own effects included — and succeeds on every call after.
type failingOnceHandler struct {
	mu     sync.Mutex
	failed bool
}

func (h *failingOnceHandler) Apply(context.Context, pgx.Tx, intake.Fact) (intake.Result, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.failed {
		h.failed = true
		return intake.Result{}, errors.New("injected apply failure")
	}
	return intake.Result{}, nil
}

// failingPositionReader fails DispatchPosition the first failures calls, as a transient JetStream
// read error would, then answers position on every call after.
type failingPositionReader struct {
	mu       sync.Mutex
	failures int
	calls    int
	position intake.DispatchConsumerPosition
}

// pilotDispatch answers ListIssues with a fixed summary list, as workflowRuntime.reconcile reads
// it, and refuses everything else this test does not need.
type pilotDispatch struct{ summaries []dispatch.IssueSummary }

func (d *pilotDispatch) ListIssues(context.Context, string, []string) ([]dispatch.IssueSummary, error) {
	return d.summaries, nil
}
func (d *pilotDispatch) GetIssue(context.Context, string) (dispatch.Issue, error) {
	return dispatch.Issue{}, nil
}
func (d *pilotDispatch) SetStatus(context.Context, string, string) error   { return nil }
func (d *pilotDispatch) PostMessage(context.Context, string, string) error { return nil }
func (d *pilotDispatch) MessageBodiesSince(context.Context, string, time.Time) ([]string, error) {
	return nil, nil
}
func (d *pilotDispatch) Approval(context.Context, string) (dispatch.Approval, error) {
	return dispatch.Approval{}, nil
}

// filteringDispatch honors the statuses argument exactly as the real HTTPClient does: only
// summaries whose status is in the requested set, or every summary when the caller asks for none.
// pilotDispatch above ignores that argument entirely, so it cannot catch a boot listing that
// silently narrows its own status filter — only a stub that filters like Dispatch's real client
// can.
type filteringDispatch struct{ summaries []dispatch.IssueSummary }

func (d *filteringDispatch) ListIssues(_ context.Context, _ string, statuses []string) ([]dispatch.IssueSummary, error) {
	if len(statuses) == 0 {
		return d.summaries, nil
	}
	wanted := make(map[string]struct{}, len(statuses))
	for _, status := range statuses {
		wanted[status] = struct{}{}
	}
	var filtered []dispatch.IssueSummary
	for _, summary := range d.summaries {
		if _, ok := wanted[summary.Status]; ok {
			filtered = append(filtered, summary)
		}
	}
	return filtered, nil
}
func (d *filteringDispatch) GetIssue(context.Context, string) (dispatch.Issue, error) {
	return dispatch.Issue{}, nil
}
func (d *filteringDispatch) SetStatus(context.Context, string, string) error   { return nil }
func (d *filteringDispatch) PostMessage(context.Context, string, string) error { return nil }
func (d *filteringDispatch) MessageBodiesSince(context.Context, string, time.Time) ([]string, error) {
	return nil, nil
}
func (d *filteringDispatch) Approval(context.Context, string) (dispatch.Approval, error) {
	return dispatch.Approval{}, nil
}

// P2, thread 4114574424: reconcile's boot listing read only todo through retro, so a root moved to
// backlog while the daemon was down fell out of the window entirely — nothing held its key. The
// replayed labeled issue.created that follows then takes Apply's unrecorded, unheld branch and is
// promoted, and the backlog event that actually explains its current state only stops it
// afterward: one architect start already queued. reconcile now reads every status, so a key
// Dispatch shows out of the workflow's own window is held on its own listing summary exactly like
// one still inside it.
func TestReconcileHoldsAKeyTheBootListingShowsOutOfTheWorkflowStatusWindow(t *testing.T) {
	pool := isolatedOutboxPool(t)
	url := workflowNATS(t)
	conn, err := nats.Connect(url, nats.Timeout(time.Second))
	if err != nil {
		t.Fatalf("connect NATS: %v", err)
	}
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	consumers, err := intake.OpenConsumers(context.Background(), js, intake.ConsumerSpec{
		Project: "CAPTURE", Repositories: []ghrepo.Repository{ghrepo.MustParse("sjawhar/legion")}, AckWait: time.Second, NakDelay: time.Millisecond, Logger: log,
	})
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	// A message ahead of the consumer's own ack floor, never consumed in this test, is what keeps
	// caughtUp false: the label-adding LEGION-EDGE event itself, still sitting on the stream
	// unconsumed, is exactly the real event Reconcile's own listing snapshot cannot yet be told
	// apart from.
	if _, err := js.Publish(context.Background(), "notifications.dispatch.issue.LEGION-EDGE.issue.created", []byte(`{"seq":1,"type":"issue.created","payload":{"status":"backlog"}}`)); err != nil {
		t.Fatalf("publish LEGION-EDGE's own pending event: %v", err)
	}

	engine := workflow.New(record.NewStore(), workflow.Config{Project: "CAPTURE"}, log)
	admission := admit.New(record.NewStore(), engine, 1, "CAPTURE", log)
	w := &workflowRuntime{
		pool: pool, admission: admission, engine: engine, handlers: []intake.Handler{engine, admission},
		dispatch: &filteringDispatch{summaries: []dispatch.IssueSummary{
			{Key: "LEGION-EDGE", Title: "edge", Status: "backlog", Rank: "A", HandedOver: true, LastSeq: 2},
		}},
		dispatchProject: "CAPTURE", bootID: "test-boot", log: log, consumers: consumers,
	}
	if err := w.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !admission.Held() {
		t.Fatal("Held() = false after a boot listing showed a behind backlog key, want it held")
	}

	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "seq1-created", intake.DispatchIssue{
		Key: "LEGION-EDGE", Seq: 1, Type: "issue.created", Status: "todo", Title: "edge", Rank: "A", HandedOver: true,
	}, w.handlers...); err != nil {
		t.Fatalf("apply the replayed todo-created event: %v", err)
	}

	var slotted bool
	if err := pool.QueryRow(context.Background(), "select exists(select 1 from slots where issue = $1)", "LEGION-EDGE").Scan(&slotted); err != nil {
		t.Fatalf("query LEGION-EDGE slot: %v", err)
	}
	if slotted {
		t.Fatal("LEGION-EDGE was admitted (an architect start queued) before the backlog event that stops it ever arrived, want it held")
	}
}

// The pilot's actual shape: a fresh database, a newly opened consumer, other projects' events
// already sitting on the shared notification stream, one labeled todo root and a labeled root in
// backlog. Exactly the labeled todo root is admitted, and quickly — this regression row is the one
// oracle's own run confirmed and asked to keep.
func TestReconcileOnThePilotsShapeAdmitsOnlyTheLabeledTodoRootQuickly(t *testing.T) {
	pool := isolatedOutboxPool(t)
	url := workflowNATS(t)
	conn, err := nats.Connect(url, nats.Timeout(time.Second))
	if err != nil {
		t.Fatalf("connect NATS: %v", err)
	}
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	// Another project's daemon already published to the shared stream before this one ever opens
	// its consumer.
	if _, err := js.Publish(context.Background(), "notifications.dispatch.issue.OTHER-1.issue.updated", []byte(`{"seq":1,"type":"issue.updated","payload":{"status":"todo"}}`)); err != nil {
		t.Fatalf("publish another project's event: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	consumers, err := intake.OpenConsumers(context.Background(), js, intake.ConsumerSpec{
		Project: "CAPTURE", Repositories: []ghrepo.Repository{ghrepo.MustParse("sjawhar/legion")}, AckWait: time.Second, NakDelay: time.Millisecond, Logger: log,
	})
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}

	engine := workflow.New(record.NewStore(), workflow.Config{Project: "CAPTURE"}, log)
	admission := admit.New(record.NewStore(), engine, 1, "CAPTURE", log)
	w := &workflowRuntime{
		pool: pool, admission: admission, engine: engine, handlers: []intake.Handler{engine, admission},
		dispatch: &pilotDispatch{summaries: []dispatch.IssueSummary{
			{Key: "CAPTURE-1", Title: "todo root", Status: "todo", Rank: "A", HandedOver: true},
			{Key: "CAPTURE-2", Title: "backlog root", Status: "backlog", Rank: "B", HandedOver: true},
		}},
		dispatchProject: "CAPTURE", consumers: consumers, bootID: "pilot-boot", log: log,
		holdPollInterval: 10 * time.Millisecond,
	}

	started := time.Now()
	if err := w.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { w.pollHoldRelease(ctx); close(done) }()

	testwait.Eventually(t, "the labeled todo root is admitted", func() bool {
		var slotted bool
		if err := pool.QueryRow(context.Background(), "select exists(select 1 from slots where issue = $1)", "CAPTURE-1").Scan(&slotted); err != nil {
			return false
		}
		return slotted
	})
	elapsed := time.Since(started)
	cancel()
	<-done
	if elapsed > time.Second {
		t.Fatalf("admission took %s, want it quick", elapsed)
	}

	var backlogSlotted bool
	if err := pool.QueryRow(context.Background(), "select exists(select 1 from slots where issue = $1)", "CAPTURE-2").Scan(&backlogSlotted); err != nil {
		t.Fatalf("query CAPTURE-2 slot: %v", err)
	}
	if backlogSlotted {
		t.Fatal("the backlog root was admitted, want only the todo root")
	}
}
func (f *failingPositionReader) DispatchPosition(context.Context) (intake.DispatchConsumerPosition, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= f.failures {
		return intake.DispatchConsumerPosition{}, errors.New("injected transient failure")
	}
	return f.position, nil
}

// Non-blocking: a Dispatch event whose transaction always fails pins the ack floor forever (the
// consumer sets no MaxDeliver), so the hold never releases and, before this, nothing in the log
// said why. Once a hold has persisted past a bound, the poll warns on its own schedule — never
// releasing on the strength of the timer, only naming what it is still waiting for: the target,
// the current ack floor, and the stream sequence stuck behind it.
func TestPollHoldReleaseWarnsWhenAHoldPersistsPastItsBound(t *testing.T) {
	pool := isolatedOutboxPool(t)
	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, nil))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: "CAPTURE"}, log)
	admission := admit.New(record.NewStore(), engine, 1, "CAPTURE", log)
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return record.NewStore().PutIssue(context.Background(), tx, record.Issue{
			Key: "LEGION-EDGE", Project: "CAPTURE", Title: "edge", Tree: "LEGION-EDGE", Phase: phase.Admitted,
			Generation: 1, Status: "todo", Rank: "A", HandedOver: true, LastDispatchSeq: 1,
		})
	}); err != nil {
		t.Fatalf("seed root: %v", err)
	}
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return admission.Reconcile(context.Background(), tx, []dispatch.IssueSummary{
			{Key: "LEGION-EDGE", Title: "edge", Status: "todo", Rank: "A", HandedOver: true, LastSeq: 2},
		}, 5, intake.DispatchConsumerPosition{AckFloorStream: 0, Idle: false})
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !admission.Held() {
		t.Fatal("Held() = false after seeding a behind record, want it held")
	}

	// A reader stuck at an ack floor that never reaches target 5: the hold this poll would
	// otherwise never explain.
	reader := &failingPositionReader{position: intake.DispatchConsumerPosition{AckFloorStream: 0, Idle: false}}
	w := &workflowRuntime{
		pool: pool, admission: admission, handlers: []intake.Handler{engine, admission},
		dispatchProject: "CAPTURE", bootID: "test-boot", log: log,
		holdPollInterval: 5 * time.Millisecond, holdWarnAfter: 20 * time.Millisecond, holdWarnEvery: 20 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { w.pollHoldReleaseWith(ctx, reader); close(done) }()

	testwait.Eventually(t, "the poll warns about a hold that has not released", func() bool {
		return strings.Contains(logBuf.String(), "admission: a hold has not released")
	})
	cancel()
	<-done

	msg := logBuf.String()
	if !strings.Contains(msg, "target=5") || !strings.Contains(msg, "ack_floor=0") {
		t.Fatalf("warn log = %q, want it to name the target and the ack floor", msg)
	}
}
