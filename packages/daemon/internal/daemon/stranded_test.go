package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// strandedLine is the error the daemon logs once a boot has re-adopted its claims, naming every
// claim nothing will wake.
const strandedLine = "legion daemon started with claims nothing will wake"

// previousRun is what a previous daemon of cfg's project left: issue records, the admission slots
// of the roots that ran, the claims on them, and outbox rows. Every claim is bound to its tree's
// open lifecycle under the workflow's authority, as the workflow's starts bind theirs.
type previousRun struct {
	issues []record.Issue
	slots  []string
	claims []supervise.Claim
	rows   []record.OutboxRow
}

func putPreviousRun(t *testing.T, cfg config.Config, run previousRun) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	records := record.NewStore()
	now := time.Now().UTC()
	if err := pgx.BeginFunc(ctx, st.Pool(), func(tx pgx.Tx) error {
		// A slot's index is unique across every project's slots, so this run's take indices above
		// any the shared database already holds.
		taken, err := records.Slots(ctx, tx)
		if err != nil {
			return err
		}
		base := 0
		for _, slot := range taken {
			base = max(base, slot.Index+1)
		}
		for _, issue := range run.issues {
			if err := records.PutIssue(ctx, tx, issue); err != nil {
				return err
			}
		}
		for i, key := range run.slots {
			if err := records.PutSlot(ctx, tx, record.Slot{Issue: key, Index: base + i, AdmittedAt: now}); err != nil {
				return err
			}
		}
		for _, row := range run.rows {
			if err := records.Enqueue(ctx, tx, row); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("record the previous run's issues: %v", err)
	}
	for _, c := range run.claims {
		if _, err := st.OpenTreeLifecycle(ctx, c.Project, c.Tree, treelifecycle.AuthorityWorkflow); err != nil {
			t.Fatalf("open the tree of %s: %v", c.Token, err)
		}
		if _, err := st.AdmitClaim(ctx, c); err != nil {
			t.Fatalf("put %s: %v", c.Token, err)
		}
	}
}

// previousClaim is the claim role held on issue of tree when the previous daemon stopped.
func previousClaim(t *testing.T, cfg config.Config, tree, issue string, role claim.Role, state supervise.ClaimState) supervise.Claim {
	t.Helper()
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	token, err := claim.NewToken(project, issue, role)
	if err != nil {
		t.Fatal(err)
	}
	return supervise.Claim{
		Token: token, Project: project, Tree: tree, Issue: issue, Role: role, Generation: 1, State: state,
		Session: "ses_" + string(token), SessionFile: "/sessions/" + string(token) + ".jsonl",
	}
}

// runningRoot is a root admitted at generation 1 and working phase p.
func runningRoot(key string, p phase.Phase) record.Issue {
	return record.Issue{Key: key, Tree: key, Project: projectOf(key), Title: key, Phase: p, Generation: 1, Status: "in_progress", Rank: "m", HandedOver: true}
}

// projectOf is the Dispatch project key an issue key names.
func projectOf(key string) string {
	for i := len(key) - 1; i >= 0; i-- {
		if key[i] == '-' {
			return key[:i]
		}
	}
	return key
}

// bootState is the state document a booted daemon serves, decoded as JSON, so the test reads the
// wire rather than the Go type that writes it.
func bootState(t *testing.T, d *daemon) map[string]json.RawMessage {
	t.Helper()
	status, body := d.request(http.MethodGet, "/legion/v1/state", nil, false)
	if status != http.StatusOK {
		t.Fatalf("GET the state = %d; body %s", status, body)
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode the state %s: %v", body, err)
	}
	return state
}

// strandedOnTheWire is the state's strandedClaims, failing the test when the state has none: the
// field is never absent, an empty list included.
func strandedOnTheWire(t *testing.T, state map[string]json.RawMessage) []map[string]string {
	t.Helper()
	raw, ok := state["strandedClaims"]
	if !ok {
		t.Fatalf("the state carries no strandedClaims: %v", keysOf(state))
	}
	var stranded []map[string]string
	if err := json.Unmarshal(raw, &stranded); err != nil {
		t.Fatalf("decode strandedClaims %s: %v", raw, err)
	}
	if stranded == nil {
		t.Fatalf("strandedClaims = %s, want a list", raw)
	}
	return stranded
}

func keysOf(state map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(state))
	for key := range state {
		keys = append(keys, key)
	}
	return keys
}

// A daemon restarted after an operator suspended its fleet mid-phase names, once it has re-adopted
// every claim, the claims nothing will wake: a suspended worker owning its issue's phase (nothing
// but a start for that phase resumes it, and its start already ran), a suspended tree architect (a
// notice to it is held, and only an admission starts it), a failed phase worker (its issue is held
// from that phase) and a failed tree architect. It names none of the claims something will still
// start: a worker whose phase is not its own, a worker whose phase start the outbox still holds,
// and the members of a tree that lingers, which re-admission starts again. One error line names
// them all, the suspended ones under `resume` and the failed ones, which resume does not move,
// under `needsWorkflowStart`; the state carries the same claims, each with its state.
func TestABootNamesTheClaimsNothingWillWake(t *testing.T) {
	cfg := workflowConfig(t, workflowNATS(t))
	key := func(n string) string { return cfg.Project + "-" + n }
	implementing, reviewing, tested, lingering, lingeringChild, planning := key("1"), key("2"), key("3"), key("4"), key("5"), key("6")
	until := time.Now().Add(time.Hour)
	parked := record.Issue{Key: lingering, Tree: lingering, Project: cfg.Project, Title: lingering, Phase: phase.Done, Generation: 1, Status: "backlog", Rank: "n", HandedOver: true, LingerUntil: &until}
	child := runningRoot(lingeringChild, phase.ProductionCheck)
	child.Tree, child.Parent = lingering, &parked.Key
	// The tester's start for its phase is due in an hour: the outbox holds it, and it will resume
	// the tester when it runs.
	queuedStart, err := record.NewOutboxRow(tested, record.SuperviseRequest{Op: "start", Tree: tested, Role: claim.RoleTester, Generation: 1, Phase: phase.Testing, Task: "Test it."}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	putPreviousRun(t, cfg, previousRun{
		issues: []record.Issue{runningRoot(implementing, phase.Implementing), runningRoot(reviewing, phase.Reviewing), runningRoot(tested, phase.Testing), parked, child, runningRoot(planning, phase.Planning)},
		slots:  []string{implementing, reviewing, tested, planning},
		claims: []supervise.Claim{
			previousClaim(t, cfg, implementing, implementing, claim.RoleArchitect, supervise.StateSuspended),
			previousClaim(t, cfg, implementing, implementing, claim.RolePlanner, supervise.StateSuspended),
			previousClaim(t, cfg, implementing, implementing, claim.RoleImplementer, supervise.StateSuspended),
			previousClaim(t, cfg, reviewing, reviewing, claim.RoleReviewer, supervise.StateFailed),
			previousClaim(t, cfg, tested, tested, claim.RoleTester, supervise.StateSuspended),
			previousClaim(t, cfg, lingering, lingeringChild, claim.RoleImplementer, supervise.StateSuspended),
			previousClaim(t, cfg, planning, planning, claim.RoleArchitect, supervise.StateFailed),
		},
		rows: []record.OutboxRow{queuedStart},
	})
	var logs syncBuffer
	o := fakeRuntime(fake.NewRuntime(), &built{})
	o.workflowTokens = &workflowTokenRecorder{}
	reported := strandedReportDone(&o)

	d := startDaemonLogging(t, cfg, o, slog.New(slog.NewJSONHandler(&logs, nil)))

	awaitStrandedReport(t, reported)
	lines := loggedLines(t, logs.String(), strandedLine)
	if len(lines) != 1 {
		t.Fatalf("the boot logged %d lines %q, want one; log:\n%s", len(lines), strandedLine, logs.String())
	}
	line := lines[0]
	listed := func(attribute string) []string {
		named := []string{}
		values, _ := line[attribute].([]any)
		for _, value := range values {
			named = append(named, value.(string))
		}
		return named
	}
	resume := []string{implementing + " architect", implementing + " implementer"}
	workflowStart := []string{reviewing + " reviewer", planning + " architect"}
	if line["level"] != "ERROR" || line["project"] != cfg.Project || line["count"] != float64(len(resume)+len(workflowStart)) ||
		!reflect.DeepEqual(listed("resume"), resume) || !reflect.DeepEqual(listed("needsWorkflowStart"), workflowStart) {
		t.Fatalf("the boot's line = %v, want an error for project %s naming %q to resume and %q needing a workflow start", line, cfg.Project, resume, workflowStart)
	}
	wantWire := []map[string]string{
		{"issue": implementing, "role": "architect", "state": "suspended"},
		{"issue": implementing, "role": "implementer", "state": "suspended"},
		{"issue": reviewing, "role": "reviewer", "state": "failed"},
		{"issue": planning, "role": "architect", "state": "failed"},
	}
	if got := strandedOnTheWire(t, bootState(t, d)); !reflect.DeepEqual(got, wantWire) {
		t.Fatalf("strandedClaims = %v, want %v", got, wantWire)
	}
}

// A daemon restarted with only claims something will still start — workers suspended once their
// phase was over or before it began, and the members of a lingering tree, its architect included —
// reports nothing: no line, and an empty list in the state.
func TestABootWithOnlyIdleSuspendedClaimsReportsNothing(t *testing.T) {
	cfg := workflowConfig(t, workflowNATS(t))
	key := func(n string) string { return cfg.Project + "-" + n }
	root, lingering, lingeringChild := key("1"), key("2"), key("3")
	until := time.Now().Add(time.Hour)
	parked := record.Issue{Key: lingering, Tree: lingering, Project: cfg.Project, Title: lingering, Phase: phase.Done, Generation: 1, Status: "backlog", Rank: "n", HandedOver: true, LingerUntil: &until}
	child := runningRoot(lingeringChild, phase.Implementing)
	child.Tree, child.Parent = lingering, &parked.Key
	putPreviousRun(t, cfg, previousRun{
		issues: []record.Issue{runningRoot(root, phase.Implementing), parked, child},
		slots:  []string{root},
		claims: []supervise.Claim{
			previousClaim(t, cfg, root, root, claim.RolePlanner, supervise.StateSuspended),
			previousClaim(t, cfg, root, root, claim.RoleTester, supervise.StateSuspended),
			previousClaim(t, cfg, lingering, lingering, claim.RoleArchitect, supervise.StateSuspended),
			previousClaim(t, cfg, lingering, lingeringChild, claim.RoleImplementer, supervise.StateSuspended),
		},
	})
	var logs syncBuffer
	o := fakeRuntime(fake.NewRuntime(), &built{})
	o.workflowTokens = &workflowTokenRecorder{}
	reported := strandedReportDone(&o)

	d := startDaemonLogging(t, cfg, o, slog.New(slog.NewJSONHandler(&logs, nil)))

	// The report runs beside the API: /healthz answering says nothing about whether it has read
	// yet, so "no line" is asserted only once it has finished.
	awaitStrandedReport(t, reported)
	if lines := loggedLines(t, logs.String(), strandedLine); len(lines) != 0 {
		t.Fatalf("the boot logged %v, want no line %q", lines, strandedLine)
	}
	if got := strandedOnTheWire(t, bootState(t, d)); len(got) != 0 {
		t.Fatalf("strandedClaims = %v, want none", got)
	}
}

// strandedReportDone hooks o so the returned channel closes once the boot's stranded report has
// finished, logged or not.
func strandedReportDone(o *overrides) <-chan struct{} {
	reported := make(chan struct{})
	o.strandedReported = func() { close(reported) }
	return reported
}

// awaitStrandedReport waits for the report strandedReportDone hooked, bounded by the report's own
// timeout with room for the boot around it.
func awaitStrandedReport(t *testing.T, reported <-chan struct{}) {
	t.Helper()
	select {
	case <-reported:
	case <-time.After(strandedReportTimeout + 30*time.Second):
		t.Fatal("the boot's stranded report did not finish")
	}
}

// The report reads the issue record only after supervision has restored every claim. That read is
// advisory: a held database lock must not put it between boot restoration and the API, which is
// the same boot-cycle deadlock unfinished relaunches used to cause. Holding `issues` exclusively
// makes the report's read wait on a real Postgres connection; /healthz must still answer.
func TestABootServesHealthzWhileTheStrandedReportStoreReadWaits(t *testing.T) {
	cfg := testConfig(t)
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate the store: %v", err)
	}
	lock, err := st.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin the report lock: %v", err)
	}
	locked := true
	if _, err := lock.Exec(ctx, "lock table issues in access exclusive mode"); err != nil {
		t.Fatalf("lock the report's issues read: %v", err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(runCtx, cfg, quietLogger(), fakeRuntime(fake.NewRuntime(), &built{})) }()
	t.Cleanup(func() {
		if locked {
			_ = lock.Rollback(context.Background())
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("run: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("run did not return after the report lock released")
		}
	})

	base := "http://127.0.0.1:" + strconv.Itoa(cfg.Port)
	deadline := time.Now().Add(3 * time.Second)
	for !answersHealthz(base) {
		select {
		case err := <-done:
			t.Fatalf("the daemon exited before /healthz answered: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("/healthz did not answer while the stranded report read waited on the store")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := lock.Rollback(ctx); err != nil {
		t.Fatalf("release the report lock: %v", err)
	}
	locked = false
}

// A state transaction is a snapshot: when a claim changes after that snapshot begins, its worker
// view and strandedClaims must both say what the snapshot saw. Reading worker state from the live
// supervisor after beginning the transaction, while RoleRuns stayed in the transaction, used to
// answer the newer `suspended` worker with no stranded entry.
func TestStateSnapshotKeepsStrandedClaimsConsistentWithWorkerStates(t *testing.T) {
	cfg := testConfig(t)
	issue := cfg.Project + "-1"
	held := previousClaim(t, cfg, issue, issue, claim.RoleImplementer, supervise.StateIdle)
	putPreviousRun(t, cfg, previousRun{
		issues: []record.Issue{runningRoot(issue, phase.Implementing)},
		slots:  []string{issue},
		claims: []supervise.Claim{held},
	})

	ctx := context.Background()
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	tx, err := st.Pool().BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("begin the state snapshot: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := st.SchemaVersionTx(ctx, tx); err != nil {
		t.Fatalf("establish the state snapshot: %v", err)
	}

	stored, err := st.Claims(ctx)
	if err != nil {
		t.Fatalf("read the stored claim: %v", err)
	}
	var current supervise.Claim
	for _, candidate := range stored {
		if candidate.Token == held.Token {
			current = candidate
			break
		}
	}
	if current.Token == "" {
		t.Fatalf("the stored claims omit %s", held.Token)
	}
	current.State = supervise.StateSuspended
	if err := st.PutClaim(ctx, current); err != nil {
		t.Fatalf("suspend the claim after the snapshot: %v", err)
	}

	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	view := &source{
		store: st, records: record.NewStore(), project: cfg.Project, projectToken: project, runtime: "tmux", admissionCap: cfg.AdmissionCap,
		startedAt: time.Now(), deployment: func() capabilities.Deployment { return capabilities.Deployment{} },
		reports: func() map[claim.Token]capabilities.Report { return nil },
	}
	state, err := view.State(ctx, tx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	worker := state.Issues[issue].Workers[claim.RoleImplementer].Claim
	if worker.State != string(supervise.StateIdle) || len(state.StrandedClaims) != 0 {
		t.Fatalf("snapshot state = worker %s, stranded %v; want idle worker and no stranded claims", worker.State, state.StrandedClaims)
	}
}
