package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// seedMirror writes what Dispatch says of key — its status and, in order, the keys it waits on —
// as the blockers.Mirror handler writes one from an event. Its sequence is 1, so an event a test
// then applies through the mirror (sequence 2 or more) wins the row's fence.
func seedMirror(t *testing.T, pool *pgxpool.Pool, key, status string, waits ...string) {
	t.Helper()
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().PutDispatchIssue(t.Context(), tx, record.DispatchIssueRow{
			Key: key, Project: "LEGION", Status: status, Blockers: waits, Seq: 1})
	})
}

// applyMirrored applies fact through the daemon's own handler order (daemon/workflow.go): the
// mirror first, so the engine behind it reads the row this very event writes.
func applyMirrored(t *testing.T, pool *pgxpool.Pool, engine *Engine, eventID string, fact intake.Fact) {
	t.Helper()
	mirror := blockers.NewMirror(record.NewStore(), "LEGION")
	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", eventID, fact, mirror, engine, admissionStub{}); err != nil {
		t.Fatalf("ApplyFact %s: %v", eventID, err)
	}
}

// startUnblockedChildren is admission's call of the engine's own pass over the held children
// (admit.Admission.promoteHolds): every issue, and the project's whole mirror, read once.
func startUnblockedChildren(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
	t.Helper()
	scoped, committed := intake.WithCommitHooks(context.Background())
	tx, err := pool.Begin(scoped)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(scoped)
	store := record.NewStore()
	issues, err := store.Issues(scoped, tx)
	if err != nil {
		t.Fatalf("list issues: %v", err)
	}
	mirrored, err := store.DispatchIssues(scoped, tx, "LEGION")
	if err != nil {
		t.Fatalf("list the mirror: %v", err)
	}
	if err := engine.StartUnblockedChildren(scoped, tx, issues, blockers.Rows(mirrored)); err != nil {
		t.Fatalf("StartUnblockedChildren: %v", err)
	}
	if err := tx.Commit(scoped); err != nil {
		t.Fatalf("commit: %v", err)
	}
	committed()
}

// seedHeldTree records a running root whose design gate is gate, admitted at the engine's own
// clock: the tree the held children of these tests belong to.
func seedHeldTree(t *testing.T, pool *pgxpool.Pool, gate record.DesignGate) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Planning, Generation: 4, Status: "in_progress", Rank: "U"})
	seedGate(t, pool, gate)
	seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
}

// seedAdmittedChild records a child of that tree where enterChild leaves one it does not start.
func seedAdmittedChild(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: key, Parent: new("LEGION-208"), Phase: phase.Admitted,
		Generation: 4, Status: "todo", Rank: "V", LastDispatchSeq: 1})
}

// A child released into a tree whose gate is open, but which waits on an issue Dispatch has not
// finished, is recorded and left at phase.Admitted with nothing queued for it — exactly where a
// child whose tree's gate is still closed is left. A sibling that waits on nothing is released as
// it always was.
func TestAChildReleasedWhileABlockerIsUnfinishedStaysAdmitted(t *testing.T) {
	pool := migratedPool(t)
	seedHeldTree(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 3, ApprovedVersion: new(3)})
	seedMirror(t, pool, "LEGION-300", "in_progress")
	engine := testEngine(config.DesignGateRootIssues, nil)

	applyMirrored(t, pool, engine, "child-blocked", intake.DispatchIssue{
		Key: "LEGION-209", ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.updated", Status: "todo",
		Title: "a blocked child", Parent: "LEGION-208", Rank: "V", Blockers: []string{"LEGION-300"}})

	if got := phaseOf(t, pool, "LEGION-209"); got != phase.Admitted {
		t.Fatalf("the blocked child = %s, want it left at admitted", got)
	}
	assertOutboxKinds(t, pool, []string{})

	applyMirrored(t, pool, engine, "child-free", intake.DispatchIssue{
		Key: "LEGION-210", ResolvedRepo: "sjawhar/legion", Seq: 3, Type: "issue.updated", Status: "todo",
		Title: "a child that waits on nothing", Parent: "LEGION-208", Rank: "W"})

	if got := phaseOf(t, pool, "LEGION-210"); got != phase.Planning {
		t.Fatalf("the unblocked child = %s, want planning", got)
	}
	assertOutboxKinds(t, pool, []string{"issue_branch", "supervise", "notice"})
}

// The close of the last issue a held child waited on starts it: its branch row, its planner's
// start behind that row, and one child-status notice to the tree's architect naming the issues it
// waited on. A child that waited on nothing hears nothing from this pass.
func TestAHeldChildStartsWhenItsLastBlockerCloses(t *testing.T) {
	pool := migratedPool(t)
	seedHeldTree(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 3, ApprovedVersion: new(3)})
	seedAdmittedChild(t, pool, "LEGION-209")
	seedAdmittedChild(t, pool, "LEGION-210")
	seedMirror(t, pool, "LEGION-209", "todo", "LEGION-300", "LEGION-301")
	seedMirror(t, pool, "LEGION-210", "todo")
	seedMirror(t, pool, "LEGION-300", "done")
	seedMirror(t, pool, "LEGION-301", "in_progress")
	engine := testEngine(config.DesignGateRootIssues, nil)

	startUnblockedChildren(t, pool, engine)
	if got := phaseOf(t, pool, "LEGION-209"); got != phase.Admitted {
		t.Fatalf("the child = %s while one issue it waits on is unfinished, want admitted", got)
	}
	assertOutboxKinds(t, pool, []string{})

	applyMirrored(t, pool, engine, "blocker-done", intake.DispatchIssue{
		Key: "LEGION-301", ResolvedRepo: "sjawhar/legion", Seq: 4, Type: "issue.updated", Status: "done", Title: "the last blocker"})
	startUnblockedChildren(t, pool, engine)

	if got := phaseOf(t, pool, "LEGION-209"); got != phase.Planning {
		t.Fatalf("the child = %s once every issue it waited on is done, want planning", got)
	}
	if got := phaseOf(t, pool, "LEGION-210"); got != phase.Admitted {
		t.Fatalf("the child that waited on nothing = %s, want it untouched at admitted", got)
	}
	assertOutboxKinds(t, pool, []string{"issue_branch", "supervise", "notice", "notice"})
	assertBranchRow(t, pool, "LEGION-209", 4)
	assertPlannerStart(t, pool, "LEGION-209")
	want := "LEGION-209 starts planning under LEGION-208: every issue it waited on (LEGION-300, LEGION-301) is done"
	if got := childStatusNotices(t, pool); len(got) != 1 || got[0] != want {
		t.Fatalf("child-status notices = %#v, want one reading %q", got, want)
	}
}

// The opening of a tree's design gate starts every admitted member it was holding, but not one
// that waits on an unfinished issue of its own: that child's own blockers hold it where the gate
// left it.
func TestTheGateOpeningDoesNotStartAHeldChild(t *testing.T) {
	pool := migratedPool(t)
	seedHeldTree(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 2})
	seedAdmittedChild(t, pool, "LEGION-209")
	seedAdmittedChild(t, pool, "LEGION-210")
	seedMirror(t, pool, "LEGION-209", "todo", "LEGION-300")
	seedMirror(t, pool, "LEGION-300", "backlog")
	engine := testEngine(config.DesignGateRootIssues, nil)

	applyMirrored(t, pool, engine, "approved", intake.DispatchArtifact{
		Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactApproved, Version: 2})

	if got := phaseOf(t, pool, "LEGION-209"); got != phase.Admitted {
		t.Fatalf("the blocked child = %s when the tree's gate opened, want it left at admitted", got)
	}
	if got := phaseOf(t, pool, "LEGION-210"); got != phase.Planning {
		t.Fatalf("the child that waits on nothing = %s when the tree's gate opened, want planning", got)
	}
	assertOutboxKinds(t, pool, []string{"notice", "issue_branch", "supervise", "notice"})
}

// An unblocked child whose tree would not start it now is not started by this pass either: the
// tree's gate is what holds it, and the gate's own opening starts it (advanceAdmittedTree).
func TestAnUnblockedChildWaitsForItsTreesGate(t *testing.T) {
	pool := migratedPool(t)
	seedHeldTree(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 2})
	seedAdmittedChild(t, pool, "LEGION-209")
	seedMirror(t, pool, "LEGION-209", "todo", "LEGION-300")
	seedMirror(t, pool, "LEGION-300", "done")

	startUnblockedChildren(t, pool, testEngine(config.DesignGateRootIssues, nil))

	if got := phaseOf(t, pool, "LEGION-209"); got != phase.Admitted {
		t.Fatalf("the child = %s while its tree's gate is closed, want admitted", got)
	}
	assertOutboxKinds(t, pool, []string{})
}

func phaseOf(t *testing.T, pool *pgxpool.Pool, key string) phase.Phase {
	t.Helper()
	var got string
	if err := pool.QueryRow(t.Context(), "select phase from issues where key = $1", key).Scan(&got); err != nil {
		t.Fatalf("read the phase of %s: %v", key, err)
	}
	return phase.Phase(got)
}

func assertBranchRow(t *testing.T, pool *pgxpool.Pool, issue string, generation int) {
	t.Helper()
	var gotIssue string
	var gotGeneration int
	if err := pool.QueryRow(t.Context(), "select issue, (payload->>'generation')::int from outbox where kind = 'issue_branch'").Scan(&gotIssue, &gotGeneration); err != nil {
		t.Fatalf("read the branch row: %v", err)
	}
	if gotIssue != issue || gotGeneration != generation {
		t.Fatalf("branch row = %s at generation %d, want %s at %d", gotIssue, gotGeneration, issue, generation)
	}
}

func assertPlannerStart(t *testing.T, pool *pgxpool.Pool, issue string) {
	t.Helper()
	var gotIssue, op, role string
	if err := pool.QueryRow(t.Context(), "select issue, payload->>'op', payload->>'role' from outbox where kind = 'supervise'").Scan(&gotIssue, &op, &role); err != nil {
		t.Fatalf("read the supervise row: %v", err)
	}
	if gotIssue != issue || op != "start" || role != "planner" {
		t.Fatalf("supervise row = %s %s %s, want a planner start for %s", gotIssue, op, role, issue)
	}
}

func childStatusNotices(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select payload->>'reason' from outbox where kind = 'notice' and payload->>'kind' = 'child-status' order by id")
	if err != nil {
		t.Fatalf("read the child-status notices: %v", err)
	}
	defer rows.Close()
	var reasons []string
	for rows.Next() {
		var reason string
		if err := rows.Scan(&reason); err != nil {
			t.Fatalf("scan a child-status notice: %v", err)
		}
		reasons = append(reasons, reason)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the child-status notices: %v", err)
	}
	return reasons
}
