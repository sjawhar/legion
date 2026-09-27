package workflow

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// An admitted root's architect has no turn of its own: admission's start launches it with no task.
// Once its claim is ready, the daemon tells it what its tree holds, which is its first instruction.
// It is told once per generation: a relaunch in the same generation resumes a session that was
// told already, and a re-admitted tree's next generation is told again. The notice states the
// design gate policy, so the architect knows whether to ask for approval.
func TestAnAdmittedRootArchitectIsToldItsTreeOnceItIsReady(t *testing.T) {
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
			engine := catchUpEngine(tc.policy)

			claimReady(t, pool, engine, "LEGION-1", claim.RoleArchitect, 1)
			want := fmt.Sprintf("LEGION-1 gen=1 policy=%s issues=LEGION-1,LEGION-2", tc.policy)
			if got := catchUps(t, pool); fmt.Sprint(got) != "["+want+"]" {
				t.Fatalf("after the root architect's first ready, catch-ups %v; want [%s]", got, want)
			}

			claimReady(t, pool, engine, "LEGION-1", claim.RoleArchitect, 2)
			if got := catchUps(t, pool); len(got) != 1 {
				t.Fatalf("after a relaunch in the same generation, catch-ups %v; want still the one", got)
			}

			seedAdmittedTree(t, pool, 2)
			claimReady(t, pool, engine, "LEGION-1", claim.RoleArchitect, 3)
			if got := catchUps(t, pool); len(got) != 2 || got[1] != fmt.Sprintf("LEGION-1 gen=2 policy=%s issues=LEGION-1,LEGION-2", tc.policy) {
				t.Fatalf("after the re-admitted tree's ready, catch-ups %v; want one more, for generation 2", got)
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
			claimReady(t, pool, catchUpEngine(config.DesignGateRootIssues), tc.issue, tc.role, 1)
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

func catchUpEngine(policy config.DesignGate) *Engine {
	return New(record.NewStore(), Config{
		Project: "LEGION", DesignGate: policy, ReviewRoundCap: 3, MaxFixAttempts: 3, Linger: time.Hour,
		Clock: func() time.Time { return time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC) },
	}, nil)
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

// catchUps is every catch-up notice the outbox holds, in order: the issue it is queued for, the
// generation it names, the gate policy it states, and the tree's issue keys it lists.
func catchUps(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `select issue, coalesce(payload->'catch_up'->>'generation', ''), coalesce(payload->'catch_up'->'gate'->>'policy', ''),
		coalesce((select string_agg(value->>'key', ',') from jsonb_array_elements(payload->'catch_up'->'issues')), '')
		from outbox where kind = 'notice' and payload->>'kind' = 'catch-up' order by id`)
	if err != nil {
		t.Fatalf("read catch-up notices: %v", err)
	}
	defer rows.Close()
	got := []string{}
	for rows.Next() {
		var issue, generation, policy, keys string
		if err := rows.Scan(&issue, &generation, &policy, &keys); err != nil {
			t.Fatalf("scan catch-up notice: %v", err)
		}
		got = append(got, fmt.Sprintf("%s gen=%s policy=%s issues=%s", issue, generation, policy, keys))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate catch-up notices: %v", err)
	}
	return got
}
