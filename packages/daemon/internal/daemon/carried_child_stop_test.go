package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// A finished root's close suspends every member of its tree, and the suspension of a role in a
// turn is held for that turn's end. A child carried into a tree of its own before that turn ends —
// a person's label on it (admission's orphan carry) or the root's hand-over at its linger's expiry
// (admission's consumption of the hand-over) — moves to a new generation, and the close's suspend
// row, of the earlier generation, finishes without acting. The new tree's starts then stop that
// role themselves: they hold its suspension, as the daemon and keeping its session, and launch
// nothing of the new tree while it runs in the old one. Once its turn ends, the new tree's
// architect launches and the role resumes its session in the new tree.
func TestACarriedChildsRoleStillInATurnStopsBeforeItsNewTreeLaunches(t *testing.T) {
	const rootKey, childKey = "LEGION-207", "LEGION-208"
	for _, tc := range []struct {
		name string
		// carry moves the child into a tree of its own, through admission.
		carry func(apply func(id string, fact intake.Fact))
	}{
		{"a person's label on the child", func(apply func(string, intake.Fact)) {
			apply("person-labels-the-child", intake.DispatchIssue{Key: childKey, Seq: 2, Type: "issue.updated", Status: "todo",
				Title: "child", Parent: rootKey, Rank: "B", HandedOver: true})
		}},
		{"the root's hand-over at its linger's expiry", func(apply func(string, intake.Fact)) {
			apply("handover:"+childKey, intake.Handover{Issue: childKey, Tree: rootKey, Generation: 1})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			until := time.Now().Add(time.Hour)
			parent := rootKey
			if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
				for _, put := range []error{
					records.PutIssue(ctx, tx, record.Issue{Key: rootKey, Tree: rootKey, Project: "LEGION", Title: "root", Phase: phase.Done,
						Generation: 1, Status: "done", Rank: "A", LingerUntil: &until, LastDispatchSeq: 1}),
					records.PutIssue(ctx, tx, record.Issue{Key: childKey, Tree: rootKey, Project: "LEGION", Title: "child", Parent: &parent,
						Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "B", LastDispatchSeq: 1}),
					records.PutHandover(ctx, tx, record.Handover{Issue: childKey, Tree: rootKey, Generation: 1}),
				} {
					if put != nil {
						return put
					}
				}
				return nil
			}); err != nil {
				t.Fatalf("seed the finished root and its child: %v", err)
			}
			sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
			engine := workflow.New(records, workflow.Config{Project: "legion", Linger: time.Hour}, quietLogger())
			admission := admit.New(records, engine, 2, "legion", quietLogger())
			var logs bytes.Buffer
			clock := time.Now()
			runner := &outbox{
				dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup, trees: outboxTreeStore(t, pool, rootKey),
				tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), repo: ghrepo.MustParse("acme/widgets"),
				githubAPI: newBranchGitHub(t, nil, branchExists).url, notices: &outboxPublisher{},
				dispatch: &outboxDispatch{issue: dispatch.Issue{Key: childKey, Status: "todo", HandedOver: true, Labels: []string{"legion"}}},
				handlers: []intake.Handler{engine, admission}, now: func() time.Time { return clock },
				log: slog.New(slog.NewJSONHandler(&logs, nil)),
				provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
					return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/" + childKey}, nil
				},
			}
			run := func(what string) {
				t.Helper()
				for range 3 {
					if err := runner.RunOnce(ctx); err != nil {
						t.Fatalf("%s: %v", what, err)
					}
					clock = clock.Add(5 * time.Minute)
				}
			}

			// The child's implementer works in its old tree, in a turn when the root's close suspends it.
			implementer := launchedClaim(t, sup, rootKey, childKey, claim.RoleImplementer)
			machine, _ := sup.Machine(implementer)
			if err := machine.Handle(ctx, supervise.StreamTurnStart{Claim: implementer}); err != nil {
				t.Fatalf("the implementer's turn: %v", err)
			}
			enqueueOutbox(t, pool, records, mustOutboxRow(t, childKey, record.SuperviseRequest{Op: "suspend", Tree: rootKey,
				Role: claim.RoleImplementer, Generation: 1, Reason: "the tree of LEGION-207 lingers"}, clock))
			if err := runner.RunOnce(ctx); err != nil {
				t.Fatalf("run the close's suspend: %v", err)
			}
			if got := machine.Claim(); got.State != supervise.StateWorking || !got.SuspensionHeld {
				t.Fatalf("the implementer after the close's suspend = %s, held %t; want working with the suspension held", got.State, got.SuspensionHeld)
			}

			tc.carry(func(id string, fact intake.Fact) {
				t.Helper()
				if _, err := intake.ApplyFact(ctx, pool, "dispatch", id, fact, engine, admission); err != nil {
					t.Fatalf("apply %s: %v", id, err)
				}
			})
			var carried *record.Issue
			if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
				var err error
				carried, err = records.Issue(ctx, tx, childKey)
				return err
			}); err != nil || carried == nil || carried.Tree != childKey || carried.Generation != 2 || carried.Phase != phase.Implementing {
				t.Fatalf("the carried child = %+v, %v; want its own tree at generation 2 in implementing", carried, err)
			}

			run("the carry's rows while the implementer's turn runs")
			if !strings.Contains(logs.String(), `"msg":"outbox supervise row serves an earlier generation; finished without acting"`) {
				t.Fatalf("the close's suspend row did not finish as one of an earlier generation; log:\n%s", logs.String())
			}
			architect := mustClaimToken(t, childKey, claim.RoleArchitect)
			launchesOf := func(method string, token claim.Token) []fake.Call {
				var of []fake.Call
				for _, call := range rt.CallsOf(method) {
					if call.Spec.Claim == token {
						of = append(of, call)
					}
				}
				return of
			}
			if spawns := launchesOf("Spawn", architect); len(spawns) != 0 {
				t.Fatalf("the new tree's architect launched %d times while the implementer still ran its turn in tree %s; want none", len(spawns), rootKey)
			}
			if got := machine.Claim(); got.State != supervise.StateWorking || !got.SuspensionHeld || got.SuspensionHeldBy != supervise.SuspendedByDaemon || got.Tree != rootKey {
				t.Fatalf("the implementer while its turn runs = %s in tree %s, held %t by %q; want working in %s with the daemon's suspension held",
					got.State, got.Tree, got.SuspensionHeld, got.SuspensionHeldBy, rootKey)
			}

			if err := machine.Handle(ctx, supervise.StreamTurnEnd{Claim: implementer}); err != nil {
				t.Fatalf("end the implementer's turn: %v", err)
			}
			if got := machine.Claim(); got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByDaemon ||
				got.Session != "ses-implementer" || got.SessionFile != "/tmp/implementer.jsonl" {
				t.Fatalf("the implementer after its turn = %+v; want suspended by the daemon with its session kept", got)
			}
			// The work the old generation did — its pushed branch, its handoff, a completion refused as
			// a stale generation — is the resumed session's own: the start that resumes it delivers
			// its phase's resume task at the new generation.
			var task string
			if err := pool.QueryRow(ctx, `select payload->>'task' from outbox where kind = 'supervise' and issue = $1
				and payload->>'op' = 'start' and payload->>'role' = 'implementer' and (payload->>'generation')::int = 2`, childKey).Scan(&task); err != nil {
				t.Fatalf("read the implementer's start at generation 2: %v", err)
			}
			if want := "Issue: " + childKey + ". Phase: implementing. Resume the existing phase work."; !strings.Contains(task, want) {
				t.Fatalf("the implementer's start task = %q, want it to say %q", task, want)
			}
			run("the carry's rows once the implementer has stopped")
			if spawns := launchesOf("Spawn", architect); len(spawns) != 1 || spawns[0].Spec.Tree != childKey {
				t.Fatalf("the architect's launches = %+v; want one, in tree %s", spawns, childKey)
			}
			resumes := launchesOf("Resume", implementer)
			if len(resumes) != 1 || resumes[0].Spec.Tree != childKey || resumes[0].Spec.ResumeSessionFile != "/tmp/implementer.jsonl" {
				t.Fatalf("the implementer's resumes = %+v; want its session resumed once, in tree %s", resumes, childKey)
			}
			if got := machine.Claim(); got.Tree != childKey {
				t.Fatalf("the implementer's claim names tree %s, want %s", got.Tree, childKey)
			}
		})
	}
}
