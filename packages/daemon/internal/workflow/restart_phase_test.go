package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The operator's retry of a phase worker (intake.RestartPhase) starts the phase that worker works
// and gives it that phase's task, which is what a claim relaunched with none would otherwise sit
// without: the issue standing in the phase, where an operator's stop leaves it, and the issue held
// from it, where its worker's failure left it, both reach the same start, and the held one leaves
// its hold behind. A role that works neither is refused, and nothing is started under its name.
func TestTheOperatorsRestartOfAPhaseStartsItsWorkerWithThePhasesTask(t *testing.T) {
	root := "LEGION-208"
	until := time.Date(2026, 9, 26, 13, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		issue     record.Issue
		role      claim.Role
		lingers   bool
		wantPhase phase.Phase
		wantTask  string
		wantCode  string
	}{
		{
			name:      "the issue stands in the phase",
			issue:     record.Issue{Phase: phase.Implementing},
			role:      claim.RoleImplementer,
			wantPhase: phase.Implementing,
			wantTask:  "Continue child. Issue: LEGION-209. Phase: implementing. Reason: the operator retried this phase's worker.",
		},
		{
			name:      "the issue is held from the phase",
			issue:     record.Issue{Phase: phase.Held, Hold: &record.Hold{From: phase.Testing}},
			role:      claim.RoleTester,
			wantPhase: phase.Testing,
			wantTask:  "Continue child. Issue: LEGION-209. Phase: testing. Reason: the operator retried this phase's worker.",
		},
		{
			name:      "another role's phase",
			issue:     record.Issue{Phase: phase.Testing},
			role:      claim.RoleImplementer,
			wantPhase: phase.Testing,
			wantCode:  "PHASE_NOT_RESTARTED",
		},
		{
			name:      "an issue that left the workflow",
			issue:     record.Issue{Phase: phase.Done, Status: "done"},
			role:      claim.RoleImplementer,
			wantPhase: phase.Done,
			wantCode:  "PHASE_NOT_RESTARTED",
		},
		{
			name:      "a lingering tree, which holds every member where it stood",
			issue:     record.Issue{Phase: phase.Implementing},
			role:      claim.RoleImplementer,
			lingers:   true,
			wantPhase: phase.Implementing,
			wantCode:  "TREE_LINGERING",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			ctx := context.Background()
			rootIssue := record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root,
				Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"}
			if tc.lingers {
				rootIssue.Phase, rootIssue.Status, rootIssue.LingerUntil = phase.Done, "done", &until
			}
			seedIssue(t, pool, rootIssue)
			child := tc.issue
			child.Key, child.Repo, child.DefaultBranch, child.GitHubRepo = "LEGION-209", "acme/widgets", "main", "acme/widgets"
			child.Tree, child.Parent, child.Project, child.Title, child.Generation, child.Rank = root, &root, "LEGION", "child", 1, "V"
			if child.Status == "" {
				child.Status = "in_progress"
			}
			seedIssue(t, pool, child)

			result, err := intake.ApplyFact(ctx, pool, "api", "operator-restart",
				intake.RestartPhase{Issue: child.Key, Role: tc.role}, testEngine(config.DesignGateRootIssues, nil), admissionStub{})
			if err != nil {
				t.Fatalf("ApplyFact the operator's restart: %v", err)
			}

			var gotPhase phase.Phase
			var heldFrom *string
			if err := pool.QueryRow(ctx, "select phase, held_from from issues where key = $1", child.Key).Scan(&gotPhase, &heldFrom); err != nil {
				t.Fatalf("read the child: %v", err)
			}
			starts := superviseRowsOf(t, pool, child.Key, "start")
			if tc.wantCode != "" {
				if result.Refusal == nil || result.Refusal.Code != tc.wantCode {
					t.Fatalf("the restart answered %+v, want a %s refusal", result.Refusal, tc.wantCode)
				}
				if gotPhase != tc.wantPhase || len(starts) != 0 {
					t.Fatalf("the refused restart left the child in %s with %+v queued, want %s and nothing started", gotPhase, starts, tc.wantPhase)
				}
				return
			}
			if result.Refusal != nil {
				t.Fatalf("the restart was refused: %+v", result.Refusal)
			}
			if gotPhase != tc.wantPhase || heldFrom != nil {
				t.Fatalf("after the restart the child is in %s held from %v, want %s and no hold", gotPhase, heldFrom, tc.wantPhase)
			}
			if len(starts) != 1 || starts[0].Role != tc.role || starts[0].Phase != tc.wantPhase || starts[0].Task != tc.wantTask {
				t.Fatalf("the restart queued %+v, want one start of the %s on %s with %q", starts, tc.role, tc.wantPhase, tc.wantTask)
			}
		})
	}
}
