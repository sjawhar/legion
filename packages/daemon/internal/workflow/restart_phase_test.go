package workflow

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// restart applies the operator's retry of one phase worker, as api's retryClaim applies it.
func restart(t *testing.T, pool *pgxpool.Pool, issue string, role claim.Role) intake.Result {
	t.Helper()
	result, err := intake.ApplyFact(context.Background(), pool, "api", "operator-retry:"+issue+":"+string(role),
		intake.RestartPhase{Issue: issue, Role: role}, testEngine(config.DesignGateRootIssues, nil), admissionStub{})
	if err != nil {
		t.Fatalf("restart %s's %s: %v", issue, role, err)
	}
	return result
}

// An operator's stop leaves the issue standing in its phase with no worker and no hold, so nothing
// of the workflow's own starts that role again. The operator's retry of the stopped worker starts
// the phase it works, which is what hands the relaunched role that phase's task
// (dispatch://INFRA-956 R3).
func TestTheOperatorsRestartOfAStoppedWorkerStartsItsPhase(t *testing.T) {
	pool := migratedPool(t)
	seedTreeForRestart(t, pool, phase.Implementing, nil)

	restart(t, pool, "LEGION-209", claim.RoleImplementer)

	starts := superviseRowsOf(t, pool, "LEGION-209", "start")
	if len(starts) != 1 || starts[0].Role != claim.RoleImplementer || starts[0].Phase != phase.Implementing || starts[0].Task == "" {
		t.Fatalf("start rows = %+v, want one implementer start of implementing carrying its task", starts)
	}
}

// An issue held for the role's own phase takes the same restart: the hold goes and the phase it
// was held from comes back, as the architect's retry_or_escalate restores it.
func TestTheOperatorsRestartOfAHeldIssueRestoresThePhase(t *testing.T) {
	pool := migratedPool(t)
	seedTreeForRestart(t, pool, phase.Held, &record.Hold{From: phase.Implementing})

	restart(t, pool, "LEGION-209", claim.RoleImplementer)

	if got, reason := projectedHold(t, pool, "LEGION-209"); got != phase.Implementing || reason != "" {
		t.Fatalf("after the restart the issue reads phase %s hold reason %q, want implementing and none", got, reason)
	}
	if starts := superviseRowsOf(t, pool, "LEGION-209", "start"); len(starts) != 1 || starts[0].Role != claim.RoleImplementer {
		t.Fatalf("start rows = %+v, want one implementer start", starts)
	}
}

// Starting a phase another role works under this claim's name would run the wrong role, so the
// restart is refused and nothing is started.
func TestTheOperatorsRestartOfAPhaseAnotherRoleWorksIsRefused(t *testing.T) {
	pool := migratedPool(t)
	seedTreeForRestart(t, pool, phase.Testing, nil)

	result := restart(t, pool, "LEGION-209", claim.RoleImplementer)

	if result.Refusal == nil || result.Refusal.Code != "PHASE_NOT_RESTARTED" {
		t.Fatalf("the restart answered %+v, want PHASE_NOT_RESTARTED", result.Refusal)
	}
	if starts := superviseRowsOf(t, pool, "LEGION-209", "start"); len(starts) != 0 {
		t.Fatalf("start rows = %+v, want none: the tester works that phase", starts)
	}
}

// seedTreeForRestart records LEGION-208 and its child LEGION-209 at the phase at, with hold when
// the child is held.
func seedTreeForRestart(t *testing.T, pool *pgxpool.Pool, at phase.Phase, hold *record.Hold) {
	t.Helper()
	root := "LEGION-208"
	seedIssue(t, pool, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root,
		Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"})
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root,
		Parent: &root, Project: "LEGION", Title: "child", Phase: at, Hold: hold, Generation: 1, Status: "in_progress", Rank: "V"})
}
