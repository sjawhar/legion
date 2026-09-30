package daemon

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The runtime's tick is what wakes a controller when no event does: on its period it applies a
// ControllerTick, and with a slot free and a controller registered, admission queues one `tick`
// controller notice named by the project, however many ticks pass before it is published.
func TestTheControllerTickQueuesOneWakeWhileASlotIsFree(t *testing.T) {
	pool := isolatedOutboxPool(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// The daemon's own path: `legion controller start` mints the capability and its session
	// registers with it, both under the project token (api/controller.go).
	st, err := store.Open(context.Background(), pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	token, err := claim.ProjectToken("CAPTURE")
	if err != nil {
		t.Fatal(err)
	}
	generation, err := st.MintController(context.Background(), token, []byte("capability"))
	if err != nil {
		t.Fatalf("mint the controller capability: %v", err)
	}
	if ok, err := st.RegisterController(context.Background(), token, generation, "ses-controller", []byte("secret"), time.Now()); err != nil || !ok {
		t.Fatalf("register the controller = %t, %v", ok, err)
	}
	engine := workflow.New(record.NewStore(), workflow.Config{Project: "CAPTURE"}, log)
	admission := admit.New(record.NewStore(), engine, 1, "CAPTURE", log)
	w := &workflowRuntime{
		pool: pool, admission: admission, handlers: []intake.Handler{engine, admission},
		dispatchProject: "CAPTURE", bootID: "test-boot", log: log, controllerWake: 10 * time.Millisecond,
	}
	ticks := func() (rows int, issue string) {
		t.Helper()
		if err := pool.QueryRow(context.Background(), `select count(*), coalesce(min(issue), '') from outbox
			where kind = $1 and payload->>'kind' = 'tick'`, string(record.OutboxKindControllerNotice)).Scan(&rows, &issue); err != nil {
			t.Fatalf("read the tick notices: %v", err)
		}
		return rows, issue
	}
	processed := func() (n int) {
		t.Helper()
		if err := pool.QueryRow(context.Background(), `select count(*) from processed_events where event_id like 'controller-tick:CAPTURE:test-boot:%'`).Scan(&n); err != nil {
			t.Fatalf("read the processed ticks: %v", err)
		}
		return n
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.tickController(ctx); close(done) }()
	testwait.Eventually(t, "three ticks applied", func() bool { return processed() >= 3 })
	cancel()
	<-done
	if rows, issue := ticks(); rows != 1 || issue != "CAPTURE" {
		t.Fatalf("tick notices = %d on %q; want one, named by the project", rows, issue)
	}
}

// The runtime ticks once shortly after it starts, not a full interval later, so a daemon restarted
// more often than controller_wake_interval_seconds still wakes its controller: with an hour's
// interval, the first tick is the early one.
func TestTheControllerTickComesShortlyAfterTheRuntimeStarts(t *testing.T) {
	pool := isolatedOutboxPool(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: "CAPTURE"}, log)
	admission := admit.New(record.NewStore(), engine, 1, "CAPTURE", log)
	w := &workflowRuntime{
		pool: pool, admission: admission, handlers: []intake.Handler{engine, admission},
		dispatchProject: "CAPTURE", bootID: "test-boot", log: log,
		controllerWake: time.Hour, controllerFirstWake: 10 * time.Millisecond,
	}
	processed := func() bool {
		var n int
		if err := pool.QueryRow(context.Background(), `select count(*) from processed_events where event_id like 'controller-tick:CAPTURE:test-boot:%'`).Scan(&n); err != nil {
			t.Fatalf("read the processed ticks: %v", err)
		}
		return n == 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.tickController(ctx); close(done) }()
	testwait.Eventually(t, "the first tick, an hour before the interval's", processed)
	cancel()
	<-done
}
