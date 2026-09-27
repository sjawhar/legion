package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
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
