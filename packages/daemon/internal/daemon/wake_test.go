package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
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

// recordPodWake writes one resume row as a wake's own transaction does (store.RecordPodWakeTx),
// which is what these tests seed: the whole wake (store.TakePodWake) also marks the wake and
// cancels a backstop, and neither is what the pass under test reads.
func recordPodWake(t *testing.T, st *store.Store, res store.PodResume) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	if err := st.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		id, err = store.RecordPodWakeTx(ctx, tx, res)
		return err
	}); err != nil {
		t.Fatalf("record the wake of %s: %v", res.Issue, err)
	}
	return id
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
	id := recordPodWake(t, w.idleStore, store.PodResume{Project: "legion", Issue: "LEGION-208",
		Role: string(claim.RoleImplementer), Kind: pods.WakeBackstop, At: woke})

	// Nine minutes after the wake nothing is stuck yet.
	w.warnStuckResumes(ctx, woke.Add(workflow.StrandedReleaseWake-time.Minute))
	if strings.Contains(logged.String(), "has had no first turn") {
		t.Fatalf("the daemon's log = %q, want no warning before ten minutes", logged)
	}
	if stuck := stuckMark(t, pool, id); stuck {
		t.Fatalf("the resume is marked stuck before ten minutes")
	}

	// Ten minutes after it, one warning naming the issue, the role and the step.
	w.warnStuckResumes(ctx, woke.Add(workflow.StrandedReleaseWake))
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
	later := recordPodWake(t, w.idleStore, store.PodResume{Project: "legion", Issue: "LEGION-208",
		Role: string(claim.RoleTester), Kind: pods.WakeChecks, At: woke})
	if err := w.idleStore.MarkPodResumeReady(ctx, later, woke.Add(time.Minute)); err != nil {
		t.Fatalf("stamp the pod ready: %v", err)
	}
	logged.Reset()
	w.warnStuckResumes(ctx, woke.Add(workflow.StrandedReleaseWake))
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
	records, err := restarted.idleStore.PodRecordsOf("legion")
	if err != nil {
		t.Fatalf("read the pod records: %v", err)
	}
	applied := &backstopHandler{records: records}
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
type backstopHandler struct {
	records *store.PodRecords
	facts   int
}

func (h *backstopHandler) Apply(ctx context.Context, tx pgx.Tx, fact intake.Fact) (intake.Result, error) {
	due, ok := fact.(intake.PodBackstopDue)
	if !ok {
		return intake.Result{}, nil
	}
	fired, err := h.records.FirePodBackstop(ctx, tx, due.ID)
	if err != nil || !fired {
		return intake.Result{}, err
	}
	h.facts++
	return intake.Result{}, nil
}

// terminalHandler counts the workflow facts a claim's durable states applied, by their kind.
type terminalHandler struct {
	mu    sync.Mutex
	facts []string
}

func (h *terminalHandler) Apply(_ context.Context, _ pgx.Tx, fact intake.Fact) (intake.Result, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch fact.(type) {
	case intake.ClaimReady:
		h.facts = append(h.facts, "ready")
	case intake.ClaimFailed:
		h.facts = append(h.facts, "failed")
	}
	return intake.Result{}, nil
}

func (h *terminalHandler) applied() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.facts...)
}

// A supervisor keeps one durable-state callback (supervisor.OnDurableState assigns it), so the
// daemon's two readers of those states are one callback: the workflow facts a claim's ready and
// failure are, and the stamps a released pod's resume is measured by. Registering them as two
// callbacks left only the second, which dropped every ClaimReady and ClaimFailed the workflow
// runs on.
//
// The callback is taken from the supervisor the boot's attach installed it on, never called as a
// method: a second registration in attach would be the same defect again, and only the hook the
// supervisor ends up holding shows it.
//
// R5: the first turn is stamped from the woken claim reaching working, not from a delivery's
// confirmation, because a wake for a notice or a phase-ended message carries no delivery.
func TestTheDaemonsOneDurableStateCallbackCarriesTheFactsAndTheResumeStamps(t *testing.T) {
	w, pool, _ := wakeRuntime(t)
	facts := &terminalHandler{}
	w.handlers = []intake.Handler{facts}
	woke := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	id := recordPodWake(t, w.idleStore, store.PodResume{Project: "legion", Issue: "LEGION-208",
		Role: string(claim.RoleImplementer), Kind: pods.WakeRoleMessage, At: woke})
	held := supervise.Claim{Token: "legion-legion-208-implementer", Project: "legion", Tree: "LEGION-208",
		Issue: "LEGION-208", Role: claim.RoleImplementer, Generation: 1}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sup := newSupervisor(ctx, w.idleStore, "legion", t.TempDir(), quietLogger())
	w.attach(&supervision{supervisor: sup, log: quietLogger()})
	hook := sup.durableState
	if hook == nil {
		t.Fatal("attach left the supervisor no durable-state callback")
	}

	hook(held, supervise.StateReady)
	awaitResume(t, pool, id, "pod_ready_at")
	hook(held, supervise.StateWorking)
	awaitResume(t, pool, id, "first_turn_at")
	hook(held, supervise.StateFailed)

	deadline := time.Now().Add(10 * time.Second)
	for len(facts.applied()) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := facts.applied(); len(got) != 2 || got[0] != "ready" || got[1] != "failed" {
		t.Fatalf("workflow facts = %v, want the claim's ready and its failure", got)
	}
}

// awaitResume waits for the resume's column to be stamped: the stamps are written off the
// machine's goroutine, so the callback returns before the write lands.
func awaitResume(t *testing.T, pool *pgxpool.Pool, id int64, column string) {
	t.Helper()
	if !testwait.Holds(10*time.Second, testwait.Poll, func() bool {
		var stamped bool
		if err := pool.QueryRow(context.Background(),
			`select `+column+` is not null from pod_resumes where id = $1`, id).Scan(&stamped); err != nil {
			t.Fatalf("read the resume's %s: %v", column, err)
		}
		return stamped
	}) {
		t.Fatalf("the resume's %s was never stamped", column)
	}
}
