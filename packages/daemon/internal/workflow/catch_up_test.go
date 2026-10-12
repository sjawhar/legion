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
			told := func(generation, launch int) string {
				return fmt.Sprintf("gen=%d launch=%d policy=%s issues=LEGION-1,LEGION-2 reason=the tree of LEGION-1 at generation %d as the daemon records it now; start or resume it as your role says",
					generation, launch, tc.policy, generation)
			}

			claimReady(t, pool, engine, "LEGION-1", claim.RoleArchitect, 1)
			if got, want := catchUps(t, pool), []string{told(1, 1)}; fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("after the root architect's first ready, catch-ups %v; want %v", got, want)
			}

			// The first launch dies before its catch-up is delivered; the relaunch's replaces it.
			first := catchUpRows(t, pool)
			claimReady(t, pool, engine, "LEGION-1", claim.RoleArchitect, 2)
			if got, want := catchUps(t, pool), []string{told(1, 2)}; fmt.Sprint(got) != fmt.Sprint(want) || fmt.Sprint(catchUpRows(t, pool)) == fmt.Sprint(first) {
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
			if got, want := catchUps(t, pool), []string{told(2, 3)}; fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("after the re-admitted tree's ready, catch-ups %v; want %v", got, want)
			}
		})
	}
}

// A root a hand-over created at this generation is said to be handed over in its architect's
// catch-up, whatever the phase, so the architect resumes rather than decomposing the tree anew; it
// is said to be carried as well when its old root's gate came with it, so the architect registers
// no spec. The same root at a later generation (closed and reopened) is a root like any other.
func TestACatchUpMarksAHandedOverRootInEveryPhase(t *testing.T) {
	for _, at := range []phase.Phase{phase.Planning, phase.Reviewing, phase.ProductionCheck} {
		for _, gateCarried := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, gate carried %t", at, gateCarried), func(t *testing.T) {
				pool := migratedPool(t)
				root := record.Issue{Key: "LEGION-1", Tree: "LEGION-1", Project: "LEGION", Title: "Root", Phase: at, Generation: 2, Status: "in_progress", Rank: "U"}
				seedIssue(t, pool, root)
				if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
					return record.NewStore().PutHandedOverRoot(context.Background(), tx, record.HandedOverRoot{Issue: root.Key, Generation: 2, GateCarried: gateCarried})
				}); err != nil {
					t.Fatalf("seed handed-over root: %v", err)
				}
				engine := testEngine(config.DesignGateRootIssues, nil)
				claimReady(t, pool, engine, root.Key, claim.RoleArchitect, 1)
				if got := lastCatchUp(t, pool); !got.HandedOver || got.Carried != gateCarried {
					t.Fatalf("handed-over root in %s: catch-up says handed over %t, carried %t; want true, %t", at, got.HandedOver, got.Carried, gateCarried)
				}
				if _, err := pool.Exec(t.Context(), "delete from outbox"); err != nil {
					t.Fatal(err)
				}
				root.Generation = 3
				seedIssue(t, pool, root)
				claimReady(t, pool, engine, root.Key, claim.RoleArchitect, 2)
				if got := lastCatchUp(t, pool); got.HandedOver || got.Carried {
					t.Fatalf("the root's later generation in %s: catch-up still says handed over %t, carried %t", at, got.HandedOver, got.Carried)
				}
			})
		}
	}
}

func lastCatchUp(t *testing.T, pool *pgxpool.Pool) record.CatchUp {
	t.Helper()
	var got *record.CatchUp
	for _, payload := range noticeRows(t, pool) {
		if notice, ok := payload.(record.Notice); ok && notice.Kind == "catch-up" {
			got = notice.CatchUp
		}
	}
	if got == nil {
		t.Fatal("no catch-up queued")
	}
	return *got
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
		intake.ClaimReady{Issue: issue, Role: role, Launch: uint64(launch)}, engine, admissionStub{}); err != nil {
		t.Fatalf("apply the ready of %s: %v", token, err)
	}
}

// catchUps is every catch-up notice the outbox holds, in order: the generation it names, the launch
// it was written for, the gate policy it states, the tree's issue keys it lists, and its reason.
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
		got = append(got, fmt.Sprintf("gen=%d launch=%d policy=%s issues=%s reason=%s", notice.CatchUp.Generation, notice.CatchUp.Launch, notice.CatchUp.Gate.Policy, strings.Join(keys, ","), notice.Reason))
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
