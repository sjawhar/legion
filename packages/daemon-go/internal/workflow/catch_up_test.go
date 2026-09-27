package workflow

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// An admitted root's architect has no turn of its own: admission's start launches it with no task.
// Once its claim is ready, the daemon tells it what its tree holds, which is its first instruction.
// Each launch's ready tells it again, as the TypeScript daemon sends the catch-up at every ready of
// an active root: a relaunch in the same generation may be an architect that died during its first
// turn, or a fresh agent whose workspace was lost, and neither was woken. A ready is one fact per
// launch, so the boot's replay of one already applied tells nothing more. A catch-up the launch
// before never had delivered is dropped, so the new agent is given one catch-up, the current one.
// The notice states the design gate policy, so the architect knows whether to ask for approval.
func TestARootArchitectIsToldItsTreeAtEachLaunchsReady(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy config.DesignGate
	}{
		{"the design gate armed", config.DesignGateRootIssues},
		{"the design gate off", config.DesignGateOff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedAdmittedTree(t, pool, 1)
			engine := testEngine(tc.policy, nil)
			// The reason is worded for a relaunch mid-tree as much as for the first launch: an
			// architect told to start over would edit its spec, and a new version closes its gate.
			told := func(generation int) string {
				return fmt.Sprintf("gen=%d policy=%s issues=LEGION-1,LEGION-2 reason=the tree of LEGION-1 at generation %d as the daemon records it at this launch; start or resume it as your role says",
					generation, tc.policy, generation)
			}

			claimReady(t, pool, engine, "LEGION-1", claim.RoleArchitect, 1)
			if got, want := catchUps(t, pool), []string{told(1)}; fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("after the root architect's first ready, catch-ups %v; want %v", got, want)
			}

			// The first launch dies before its catch-up is delivered; the relaunch's replaces it.
			first := catchUpRows(t, pool)
			claimReady(t, pool, engine, "LEGION-1", claim.RoleArchitect, 2)
			if got, want := catchUps(t, pool), []string{told(1)}; fmt.Sprint(got) != fmt.Sprint(want) || fmt.Sprint(catchUpRows(t, pool)) == fmt.Sprint(first) {
				t.Fatalf("after a relaunch in the same generation, catch-ups %v in rows %v (before, %v); want the relaunch's own, alone", got, catchUpRows(t, pool), first)
			}

			claimReady(t, pool, engine, "LEGION-1", claim.RoleArchitect, 2)
			if got := catchUps(t, pool); len(got) != 1 {
				t.Fatalf("after the boot's replay of that ready, catch-ups %v; want still the one", got)
			}

			// That catch-up is delivered, and the tree is re-admitted.
			if _, err := pool.Exec(t.Context(), "delete from outbox"); err != nil {
				t.Fatalf("deliver the queued notices: %v", err)
			}
			seedAdmittedTree(t, pool, 2)
			claimReady(t, pool, engine, "LEGION-1", claim.RoleArchitect, 3)
			if got, want := catchUps(t, pool), []string{told(2)}; fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("after the re-admitted tree's ready, catch-ups %v; want %v", got, want)
			}
		})
	}
}

// Only a live tree's root architect is told its tree. A tree that lingers or has left the
// workflow is told nothing, nor is a sub-architect, nor a phase worker on the root.
func TestOnlyALiveTreesRootArchitectIsToldItsTree(t *testing.T) {
	// A lingering root set back to todo still lingers until admission takes it again: only its
	// linger, not its status, says the tree has left the workflow.
	lingering := func(root *record.Issue) {
		until := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
		root.Phase, root.Status, root.LingerUntil = phase.Done, "todo", &until
	}
	for _, tc := range []struct {
		name  string
		root  func(*record.Issue)
		issue string
		role  claim.Role
	}{
		{"a lingering tree set back to todo", lingering, "LEGION-1", claim.RoleArchitect},
		{"a tree moved to backlog", func(root *record.Issue) { root.Status = "backlog" }, "LEGION-1", claim.RoleArchitect},
		{"a sub-architect", func(*record.Issue) {}, "LEGION-2", claim.RoleArchitect},
		{"a phase worker on the root", func(*record.Issue) {}, "LEGION-1", claim.RolePlanner},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			root := seedAdmittedTree(t, pool, 1)
			tc.root(&root)
			seedIssue(t, pool, root)
			claimReady(t, pool, testEngine(config.DesignGateRootIssues, nil), tc.issue, tc.role, 1)
			if got := catchUps(t, pool); len(got) != 0 {
				t.Fatalf("catch-ups %v; want none", got)
			}
		})
	}
}

// seedAdmittedTree records a root as admission leaves it, in_progress at generation, with one
// child, and returns the root.
func seedAdmittedTree(t *testing.T, pool *pgxpool.Pool, generation uint64) record.Issue {
	t.Helper()
	root := record.Issue{Key: "LEGION-1", Tree: "LEGION-1", Project: "LEGION", Title: "Root", Phase: phase.Admitted,
		Generation: generation, Status: "in_progress", Rank: "U"}
	parent := root.Key
	seedIssue(t, pool, root)
	seedIssue(t, pool, record.Issue{Key: "LEGION-2", Tree: "LEGION-1", Project: "LEGION", Title: "Child", Parent: &parent,
		Phase: phase.Admitted, Generation: generation, Status: "todo", Rank: "V"})
	return root
}

// claimReady applies the supervision observation the workflow runtime sends when a claim is ready
// (workflowRuntime.applyTerminal), under the event id it uses: one per launch of the claim.
func claimReady(t *testing.T, pool *pgxpool.Pool, engine *Engine, issue string, role claim.Role, launch int) {
	t.Helper()
	token, err := claim.NewToken("legion", issue, role)
	if err != nil {
		t.Fatalf("claim token: %v", err)
	}
	if _, err := intake.ApplyFact(context.Background(), pool, "supervise", fmt.Sprintf("supervise:%s:%d:ready", token, launch),
		intake.ClaimReady{Issue: issue, Role: role}, engine, admissionStub{}); err != nil {
		t.Fatalf("apply the ready of %s: %v", token, err)
	}
}

// catchUps is every catch-up notice the outbox holds, in order: the generation it names, the gate
// policy it states, the tree's issue keys it lists, and its reason.
func catchUps(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	got := []string{}
	for _, payload := range noticeRows(t, pool) {
		notice, ok := payload.(record.Notice)
		if !ok || notice.Kind != "catch-up" {
			continue
		}
		keys := []string{}
		for _, issue := range notice.CatchUp.Issues {
			keys = append(keys, issue.Key)
		}
		got = append(got, fmt.Sprintf("gen=%d policy=%s issues=%s reason=%s", notice.CatchUp.Generation, notice.CatchUp.Gate.Policy, strings.Join(keys, ","), notice.Reason))
	}
	return got
}

// catchUpRows is the outbox row id of every catch-up notice the outbox holds, in order.
func catchUpRows(t *testing.T, pool *pgxpool.Pool) []int64 {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select id from outbox where kind = 'notice' and payload->>'kind' = 'catch-up' order by id")
	if err != nil {
		t.Fatalf("read the catch-up rows: %v", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		t.Fatalf("scan the catch-up rows: %v", err)
	}
	return ids
}
