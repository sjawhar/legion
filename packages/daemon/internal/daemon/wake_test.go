package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// wakeRuntime is the smallest workflowRuntime the wake pass and the release gate read: the
// project, the store and a log a test can read back.
func wakeRuntime(t *testing.T) (*workflowRuntime, *pgxpool.Pool, *bytes.Buffer) {
	t.Helper()
	pool := isolatedOutboxPool(t)
	st := outboxTreeStore(t, pool, "LEGION-208")
	logged := &bytes.Buffer{}
	w := &workflowRuntime{pool: pool, projectID: "legion", idleStore: st, idlePeriod: 10 * time.Minute,
		log: slog.New(slog.NewTextHandler(logged, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	return w, pool, logged
}

// recordListenerReady stamps the listener's liveness report at at, as the wake fact does.
func recordListenerReady(t *testing.T, pool *pgxpool.Pool, at time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `insert into listener_reports (project, ready_at) values ('legion', $1)
		on conflict (project) do update set ready_at = excluded.ready_at`, at); err != nil {
		t.Fatalf("record the listener's report: %v", err)
	}
}

// R7: the daemon releases nothing unless the Envoy listener has reported itself ready within
// fifteen minutes. The gate runs before the pass reads a single pod, so a daemon that has heard
// nothing releases nothing at all rather than deciding each pod and then declining.
func TestTheDaemonReleasesNothingWithoutAFreshListenerReport(t *testing.T) {
	t.Run("no report at all", func(t *testing.T) {
		w, _, logged := wakeRuntime(t)
		w.releaseIdlePods(context.Background())
		if !strings.Contains(logged.String(), "has not once reported itself ready") {
			t.Fatalf("the daemon's log = %q, want it to say the listener has never reported", logged)
		}
	})
	t.Run("a report older than the window", func(t *testing.T) {
		w, pool, logged := wakeRuntime(t)
		recordListenerReady(t, pool, time.Now().UTC().Add(-listenerReadyWindow-time.Minute))
		if w.listenerLive(context.Background()) {
			t.Fatalf("the listener reads live on a report older than %s", listenerReadyWindow)
		}
		w.releaseIdlePods(context.Background())
		if !strings.Contains(logged.String(), "too old to release a pod on") {
			t.Fatalf("the daemon's log = %q, want it to say the report is too old", logged)
		}
	})
	t.Run("a report inside the window", func(t *testing.T) {
		w, pool, _ := wakeRuntime(t)
		recordListenerReady(t, pool, time.Now().UTC().Add(-listenerReadyWindow+time.Minute))
		if !w.listenerLive(context.Background()) {
			t.Fatalf("the listener does not read live on a report inside %s", listenerReadyWindow)
		}
	})
}

// R5: a resume with no first turn ten minutes after its wake writes one warning naming the
// issue, the role and the step it waits on, and is marked, so it warns once however many passes
// read it. The clock is the test's: every time comes from the pass's own now.
func TestAStuckResumeIsWarnedAboutOnceAtTenMinutes(t *testing.T) {
	w, pool, logged := wakeRuntime(t)
	ctx := context.Background()
	woke := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	id, err := w.idleStore.RecordPodWake(ctx, store.PodResume{Project: "legion", Issue: "LEGION-208",
		Role: string(claim.RoleImplementer), Kind: store.WakeBackstop, At: woke})
	if err != nil {
		t.Fatalf("record the wake: %v", err)
	}

	// Nine minutes after the wake nothing is stuck yet.
	w.warnStuckResumes(ctx, woke.Add(stuckResumeAfter-time.Minute))
	if strings.Contains(logged.String(), "has had no first turn") {
		t.Fatalf("the daemon's log = %q, want no warning before ten minutes", logged)
	}
	if stuck := stuckMark(t, pool, id); stuck {
		t.Fatalf("the resume is marked stuck before ten minutes")
	}

	// Ten minutes after it, one warning naming the issue, the role and the step.
	w.warnStuckResumes(ctx, woke.Add(stuckResumeAfter))
	line := logged.String()
	for _, want := range []string{"has had no first turn", "LEGION-208", string(claim.RoleImplementer), "the replacement pod to be ready"} {
		if !strings.Contains(line, want) {
			t.Fatalf("the warning = %q, want it to name %q", line, want)
		}
	}
	if !stuckMark(t, pool, id) {
		t.Fatalf("the resume is not marked stuck at ten minutes")
	}

	// A later pass warns about it no second time: the mark is what takes it out of the read.
	logged.Reset()
	w.warnStuckResumes(ctx, woke.Add(time.Hour))
	if strings.Contains(logged.String(), "has had no first turn") {
		t.Fatalf("the daemon's log = %q, want the stuck resume warned about once", logged)
	}

	// A resume whose pod came up but whose role has taken no turn waits on the first turn.
	later, err := w.idleStore.RecordPodWake(ctx, store.PodResume{Project: "legion", Issue: "LEGION-208",
		Role: string(claim.RoleTester), Kind: store.WakeChecks, At: woke})
	if err != nil {
		t.Fatalf("record the second wake: %v", err)
	}
	if err := w.idleStore.MarkPodResumeReady(ctx, later, woke.Add(time.Minute)); err != nil {
		t.Fatalf("stamp the pod ready: %v", err)
	}
	logged.Reset()
	w.warnStuckResumes(ctx, woke.Add(stuckResumeAfter))
	if !strings.Contains(logged.String(), "the woken role's first turn") {
		t.Fatalf("the warning = %q, want it to say the pod is up and the first turn is what waits", logged)
	}
}

func stuckMark(t *testing.T, pool *pgxpool.Pool, id int64) bool {
	t.Helper()
	var stuck bool
	if err := pool.QueryRow(context.Background(), `select stuck from pod_resumes where id = $1`, id).Scan(&stuck); err != nil {
		t.Fatalf("read the resume's stuck mark: %v", err)
	}
	return stuck
}

// R5 and R9: a backstop that came due while the daemon was down fires at the first pass after it
// starts. The row is read from the database the restarted daemon opens, and the pass applies one
// fact per row under the row's own id, so the backstop fires once.
func TestABackstopThatCameDueWhileTheDaemonWasDownFiresAfterItStarts(t *testing.T) {
	w, pool, _ := wakeRuntime(t)
	ctx := context.Background()
	due := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	if err := w.idleStore.ArmPodBackstop(ctx, store.PodWakeBackstop{Project: "legion", Issue: "LEGION-208",
		Role: claim.RoleImplementer, DueAt: due}); err != nil {
		t.Fatalf("arm the backstop: %v", err)
	}

	// The daemon stops and starts again: a new runtime over the same database, holding nothing
	// of the first one. The armed backstop is still due.
	restarted := &workflowRuntime{pool: pool, projectID: "legion", idleStore: outboxTreeStore(t, pool),
		idlePeriod: 10 * time.Minute, log: quietLogger()}
	applied := &backstopHandler{}
	restarted.handlers = []intake.Handler{applied}
	restarted.fireDueBackstops(ctx, time.Now().UTC())
	if applied.facts != 1 {
		t.Fatalf("facts applied after the restart = %d, want the one backstop", applied.facts)
	}
	restarted.fireDueBackstops(ctx, time.Now().UTC())
	if applied.facts != 1 {
		t.Fatalf("facts applied by a second pass = %d, want the backstop fired once", applied.facts)
	}
}

// backstopHandler is the workflow engine's backstop arm as this test needs it: it counts the
// facts it is handed and fires the row conditionally, as the engine does, so a backstop two
// passes read is seen to fire once.
type backstopHandler struct{ facts int }

func (h *backstopHandler) Apply(ctx context.Context, tx pgx.Tx, fact intake.Fact) (intake.Result, error) {
	due, ok := fact.(intake.PodBackstopDue)
	if !ok {
		return intake.Result{}, nil
	}
	fired, err := store.FirePodBackstop(ctx, tx, due.ID)
	if err != nil || !fired {
		return intake.Result{}, err
	}
	h.facts++
	return intake.Result{}, nil
}
