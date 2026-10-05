package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// putRemovableIssue plants issue so TreeIssues reads it back; a helper distinct from
// putOutboxIssue only in intent (dispatch://LEGION-583's removable_test.go).
func putRemovableIssue(t *testing.T, pool *pgxpool.Pool, records record.Store, issue record.Issue) {
	t.Helper()
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return records.PutIssue(context.Background(), tx, issue)
	}); err != nil {
		t.Fatalf("put issue %s: %v", issue.Key, err)
	}
}

// putMergedPullRequest records issue's pull request as merged at head, what removableWorkspaces
// pairs a done or parked candidate with so workspace-init's push-safety check can tell a squash
// merge's now-branchless head from a commit that was never pushed at all.
func putMergedPullRequest(t *testing.T, pool *pgxpool.Pool, records record.Store, issue, head string) {
	t.Helper()
	pr := record.PullRequest{
		Issue: issue, Repo: "acme/widgets", Number: 1, Branch: "legion/" + issue,
		HeadSHA: head, HeadUpdatedAt: time.Now().UTC(), State: record.PullRequestMerged,
	}
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return records.PutPullRequest(context.Background(), tx, pr)
	}); err != nil {
		t.Fatalf("put pull request of %s: %v", issue, err)
	}
}

// claimOn creates issue's role claim in state, the architectClaimOn of notice_exceptions_test.go
// generalized to any role: removableWorkspaces asks after every role's claim, not the architect's
// alone.
func claimOn(t *testing.T, sup *supervisor, issue string, role claim.Role, state supervise.ClaimState) {
	t.Helper()
	if _, _, err := sup.Create(context.Background(), supervise.Claim{
		Token: mustClaimToken(t, issue, role), Project: "legion", Tree: "LEGION-1", Issue: issue, Role: role, State: state, Session: "ses_" + issue + "_" + string(role),
	}, ""); err != nil {
		t.Fatalf("create the %s claim of %s: %v", role, issue, err)
	}
}

func callRemovable(t *testing.T, pool *pgxpool.Pool, records record.Store, sup *supervisor, tree, exclude string) []runtime.RemovableWorkspace {
	t.Helper()
	got, err := removableWorkspaces(pool, records, sup, "legion")(context.Background(), tree, exclude)
	if err != nil {
		t.Fatalf("removableWorkspaces: %v", err)
	}
	return got
}

// A done child is a candidate, paired with its merged pull request's head: workspace-init's
// push-safety check needs the head once GitHub deletes the squash merge's branch.
func TestRemovableWorkspacesIncludesADoneChildWithItsMergedHead(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	putRemovableIssue(t, pool, records, record.Issue{Key: "LEGION-1", Project: "LEGION", Tree: "LEGION-1", Title: "Root", Phase: phase.Implementing, Generation: 1, Status: "in_progress"})
	putRemovableIssue(t, pool, records, record.Issue{Key: "LEGION-2", Project: "LEGION", Tree: "LEGION-1", Title: "Done child", Phase: phase.Done, Generation: 1, Status: "done"})
	putMergedPullRequest(t, pool, records, "LEGION-2", "deadbeef")

	got := callRemovable(t, pool, records, sup, "LEGION-1", "")
	if len(got) != 1 || got[0].Issue != "LEGION-2" || got[0].MergedHead != "deadbeef" {
		t.Fatalf("candidates = %+v, want one: LEGION-2 with merged head deadbeef", got)
	}
}

// A done child whose every role's claim is suspended, failed, retired, or never made is a
// candidate, whatever Dispatch status brought it there (done, backlog, icebox, and triage all
// force phase done on the same leave, workflow/linger.go): a human shelved it, or its work is
// genuinely finished, and nothing of its role is coming back on its own.
func TestRemovableWorkspacesIncludesADoneChildWithEveryClaimParkedOrAbsent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
		states map[claim.Role]supervise.ClaimState
	}{
		{"backlog, a suspended claim", "backlog", map[claim.Role]supervise.ClaimState{
			claim.RoleImplementer: supervise.StateSuspended,
		}},
		{"icebox, a failed and a retired claim", "icebox", map[claim.Role]supervise.ClaimState{
			claim.RoleImplementer: supervise.StateFailed,
			claim.RoleTester:      supervise.StateRetired,
		}},
		{"backlog, no claim ever made", "backlog", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			putRemovableIssue(t, pool, records, record.Issue{Key: "LEGION-1", Project: "LEGION", Tree: "LEGION-1", Title: "Root", Phase: phase.Implementing, Generation: 1, Status: "in_progress"})
			putRemovableIssue(t, pool, records, record.Issue{Key: "LEGION-2", Project: "LEGION", Tree: "LEGION-1", Title: "Done child", Phase: phase.Done, Generation: 1, Status: tc.status})
			for role, state := range tc.states {
				claimOn(t, sup, "LEGION-2", role, state)
			}

			got := callRemovable(t, pool, records, sup, "LEGION-1", "")
			if len(got) != 1 || got[0].Issue != "LEGION-2" {
				t.Fatalf("candidates = %+v, want one: LEGION-2", got)
			}
		})
	}
}

// A child merely between phases — awaiting_merge, a review round the architect has not yet
// decided, or plain todo waiting on the workflow — never reaches phase done, so it is never a
// candidate whatever its claims or Dispatch status: removing its workspace here would force its
// next phase into a multi-minute re-clone, the shape this correction exists to stop.
func TestRemovableWorkspacesExcludesAChildBetweenPhases(t *testing.T) {
	for _, tc := range []struct {
		name   string
		phase  phase.Phase
		status string
	}{
		{"awaiting_merge", phase.AwaitingMerge, "in_progress"},
		{"a review round still in_progress", phase.Reviewing, "in_progress"},
		{"todo, waiting on the workflow", phase.Admitted, "todo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			putRemovableIssue(t, pool, records, record.Issue{Key: "LEGION-1", Project: "LEGION", Tree: "LEGION-1", Title: "Root", Phase: phase.Implementing, Generation: 1, Status: "in_progress"})
			putRemovableIssue(t, pool, records, record.Issue{Key: "LEGION-2", Project: "LEGION", Tree: "LEGION-1", Title: "Mid-phase child", Phase: tc.phase, Generation: 1, Status: tc.status})
			// Every claim suspended: the predicate excludes this issue by its phase alone, never
			// reaching the claim check, so a suspended claim here proves nothing about claims and
			// everything about phase.
			claimOn(t, sup, "LEGION-2", claim.RoleImplementer, supervise.StateSuspended)

			got := callRemovable(t, pool, records, sup, "LEGION-1", "")
			if len(got) != 0 {
				t.Fatalf("candidates = %+v, want none", got)
			}
		})
	}
}

// A live claim excludes its issue whatever its phase: an ordinary mid-phase child with a working
// claim, and the race leave (workflow/linger.go) opens — its phase-done write and its claim
// suspends are not one transaction, so a probe between them can see phase done on an issue whose
// implementer claim the outbox has not yet suspended. Without the claim check on the done branch
// this second case was a candidate on phase alone; both must be excluded.
func TestRemovableWorkspacesExcludesALiveChild(t *testing.T) {
	for _, tc := range []struct {
		name  string
		phase phase.Phase
	}{
		{"mid-phase, a working claim", phase.Implementing},
		{"phase done, a claim the outbox has not yet suspended", phase.Done},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			putRemovableIssue(t, pool, records, record.Issue{Key: "LEGION-1", Project: "LEGION", Tree: "LEGION-1", Title: "Root", Phase: phase.Implementing, Generation: 1, Status: "in_progress"})
			putRemovableIssue(t, pool, records, record.Issue{Key: "LEGION-2", Project: "LEGION", Tree: "LEGION-1", Title: "Live child", Phase: tc.phase, Generation: 1, Status: "backlog"})
			claimOn(t, sup, "LEGION-2", claim.RoleImplementer, supervise.StateWorking)

			got := callRemovable(t, pool, records, sup, "LEGION-1", "")
			if len(got) != 0 {
				t.Fatalf("candidates = %+v, want none", got)
			}
		})
	}
}

// The tree's own root and the issue excluded for this launch are never candidates, whatever their
// phase or status.
func TestRemovableWorkspacesExcludesTheRootAndTheExcludedIssue(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	putRemovableIssue(t, pool, records, record.Issue{Key: "LEGION-1", Project: "LEGION", Tree: "LEGION-1", Title: "Root", Phase: phase.Done, Generation: 1, Status: "done"})
	putRemovableIssue(t, pool, records, record.Issue{Key: "LEGION-2", Project: "LEGION", Tree: "LEGION-1", Title: "Done, but this launch's own issue", Phase: phase.Done, Generation: 1, Status: "done"})

	got := callRemovable(t, pool, records, sup, "LEGION-1", "LEGION-2")
	if len(got) != 0 {
		t.Fatalf("candidates = %+v, want none", got)
	}
}
