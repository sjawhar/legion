package admit

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

const (
	finishedRoot = "LEGION-1"
	planning     = "LEGION-2" // mid-run in its first phases: handed over
	heldTesting  = "LEGION-3" // held from testing: handed over
	inRetro      = "LEGION-4" // past an approved review: left for a person
	awaiting     = "LEGION-5" // its READY out: left for a person
	checking     = "LEGION-6" // its pull request merged: left for a person
	finished     = "LEGION-7" // done: stays
	parked       = "LEGION-8" // parked by a person: stays
	unstarted    = "LEGION-9" // still admitted: stays
	lingerOfRoot = uint64(1)
)

// A root whose tree stays done through its linger hands each child it left in its first phases
// (planning through reviewing, or held from one) to Legion as a root of its own: one Dispatch write
// sets the child todo with the legion label, and that write's event admits the child as an orphan
// with its own architect, as the label and the todo do when a person sets them. Before, the child
// stayed suspended, unlabelled and with no architect, and nothing ran it again (LEGION-624). A child
// past an approved review — in retro, merging, awaiting_merge or production_check — is not handed
// over, since its re-admission would clear its review, its READY and its pull request's record and
// run work already merged again; the issue says a person finishes it. A finished, parked, or
// still-admitted child stays with the tree. The settling waits for the linger to end: a root that
// comes back in it resumes its children where they stood, and hands none over. A root a person
// parked and then closed settles the same way.
func TestAFinishedRootSettlesItsUnfinishedChildrenWhenItsLingerEnds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		close func(t *testing.T, pool *pgxpool.Pool, admission *Admission, engine intake.Handler)
	}{
		{name: "signed off", close: func(t *testing.T, pool *pgxpool.Pool, admission *Admission, engine intake.Handler) {
			if result, err := intake.ApplyFact(context.Background(), pool, "api", "signoff", intake.SignOff{Issue: finishedRoot}, engine, admission); err != nil || result.Refusal != nil {
				t.Fatalf("sign off %s = %+v, %v", finishedRoot, result.Refusal, err)
			}
		}},
		{name: "parked, then closed by a person", close: func(t *testing.T, pool *pgxpool.Pool, admission *Admission, engine intake.Handler) {
			apply(t, pool, admission, "root-parked", intake.DispatchIssue{Key: finishedRoot, Seq: 2, Type: "issue.updated", Status: "backlog", Title: finishedRoot, Rank: "A", HandedOver: true}, engine)
			apply(t, pool, admission, "root-closed", intake.DispatchIssue{Key: finishedRoot, Seq: 3, Type: "issue.closed", Status: "done", Title: finishedRoot, Rank: "A", HandedOver: true}, engine)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, admission, engine := seedFinishingTree(t)
			tc.close(t, pool, admission, engine)
			if got := handOvers(t, pool); len(got) != 0 {
				t.Fatalf("hand-overs while the tree lingers = %v, want none: the root can still come back", got)
			}

			expireLinger(t, pool, admission, engine)
			if got, want := handOvers(t, pool), []string{planning, heldTesting}; !slices.Equal(got, want) {
				t.Fatalf("hand-overs once the linger ended = %v, want %v", got, want)
			}
			if got, want := messagedChildren(t, pool), []string{inRetro, awaiting, checking}; !slices.Equal(got, want) {
				t.Fatalf("children told a person finishes them = %v, want %v", got, want)
			}

			// The write's event carries the todo and the label.
			apply(t, pool, admission, "planning-handed-over", intake.DispatchIssue{Key: planning, Seq: 4, Type: "issue.updated", Status: "todo", Title: planning, Parent: finishedRoot, Rank: "B", HandedOver: true}, engine)
			got := issue(t, pool, planning)
			if got.Tree != planning || got.Phase != phase.Admitted || got.Generation != 2 || got.Status != "in_progress" || architectStarts(t, pool, planning) != 1 {
				t.Fatalf("the handed-over child = %#v with %d architect starts, want its own admitted root, generation 2, with its architect started once", got, architectStarts(t, pool, planning))
			}
			for _, key := range []string{inRetro, awaiting, checking, finished, parked, unstarted} {
				if got := issue(t, pool, key); got.Tree != finishedRoot || got.Generation != 1 {
					t.Fatalf("%s = %#v, want it left in %s's tree", key, got, finishedRoot)
				}
			}
		})
	}
}

// A root a person sets todo again while its tree lingers is re-admitted, and its mid-run children
// resume in its new run where they stood (startMidPhaseChildren). The end of the closed generation's
// linger hands none of them over: the reopen wins, and the two never both act on one child.
func TestARootReopenedInItsLingerKeepsItsChildren(t *testing.T) {
	pool, admission, engine := seedFinishingTree(t)
	if result, err := intake.ApplyFact(context.Background(), pool, "api", "signoff", intake.SignOff{Issue: finishedRoot}, engine, admission); err != nil || result.Refusal != nil {
		t.Fatalf("sign off %s = %+v, %v", finishedRoot, result.Refusal, err)
	}
	apply(t, pool, admission, "root-reopened", intake.DispatchIssue{Key: finishedRoot, Seq: 2, Type: "issue.updated", Status: "todo", Title: finishedRoot, Rank: "A", HandedOver: true}, engine)
	if root := issue(t, pool, finishedRoot); root.Generation != 2 || root.Lingers() {
		t.Fatalf("the reopened root = %#v, want generation 2, no longer lingering", root)
	}
	expireLinger(t, pool, admission, engine)
	if got := handOvers(t, pool); len(got) != 0 {
		t.Fatalf("hand-overs after the reopened root's old linger ended = %v, want none", got)
	}
	if got := issue(t, pool, planning); got.Tree != finishedRoot || got.Phase != phase.Planning {
		t.Fatalf("the reopened root's child = %#v, want it still in %s's tree, in planning", got, finishedRoot)
	}
}

func seedFinishingTree(t *testing.T) (*pgxpool.Pool, *Admission, intake.Handler) {
	t.Helper()
	pool := migratedPool(t)
	admission := newAdmission(t, 3, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, DesignGate: config.DesignGateRootIssues, ReviewRoundCap: 3, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	seedSlotted(t, pool, finishedRoot, "A")
	parent := finishedRoot
	child := func(key string, at phase.Phase, status string) record.Issue {
		return record.Issue{Key: key, Tree: finishedRoot, Project: testProject, Title: key, Parent: &parent, Phase: at, Generation: 1, Status: status, DispatchStatus: status, Rank: "B", LastDispatchSeq: 3}
	}
	held := child(heldTesting, phase.Held, "testing")
	held.Hold = &record.Hold{From: phase.Testing}
	inTx(t, pool, func(tx pgx.Tx) {
		records, ctx := record.NewStore(), context.Background()
		root, err := records.Issue(ctx, tx, finishedRoot)
		if err != nil {
			t.Fatalf("read root: %v", err)
		}
		root.Phase = phase.ProductionCheck
		for _, put := range []error{
			records.PutIssue(ctx, tx, *root),
			records.PutPhase(ctx, tx, record.PhaseRow{Issue: finishedRoot, Role: claim.RoleImplementer, Claim: "root-implementer", HandoffCommit: "production-check", LastHandoff: "production-check"}),
			records.PutIssue(ctx, tx, child(planning, phase.Planning, "in_progress")),
			records.PutIssue(ctx, tx, held),
			records.PutIssue(ctx, tx, child(inRetro, phase.Retro, "retro")),
			records.PutIssue(ctx, tx, child(awaiting, phase.AwaitingMerge, "retro")),
			records.PutIssue(ctx, tx, child(checking, phase.ProductionCheck, "retro")),
			records.PutIssue(ctx, tx, child(finished, phase.Done, "done")),
			records.PutIssue(ctx, tx, child(parked, phase.Done, "backlog")),
			records.PutIssue(ctx, tx, child(unstarted, phase.Admitted, "todo")),
		} {
			if put != nil {
				t.Fatalf("seed: %v", put)
			}
		}
	})
	return pool, admission, engine
}

func expireLinger(t *testing.T, pool *pgxpool.Pool, admission *Admission, engine intake.Handler) {
	t.Helper()
	if _, err := intake.ApplyFact(context.Background(), pool, "timer", "linger", intake.LingerExpired{Issue: finishedRoot, Generation: lingerOfRoot}, engine, admission); err != nil {
		t.Fatalf("expire the linger: %v", err)
	}
}

// handOvers is the issues of every hand-over write queued, in order, each checked to name the
// closed tree and the generation whose linger ended.
func handOvers(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	var got []string
	for _, effect := range effects(t, pool) {
		write, ok := effect.payload.(record.StatusWrite)
		if !ok || !write.HandOver {
			continue
		}
		if write.Status != "todo" || write.HandOverTree != finishedRoot || write.HandOverLinger != lingerOfRoot {
			t.Fatalf("hand-over of %s = %+v, want todo fenced on %s's linger %d", effect.issue, write, finishedRoot, lingerOfRoot)
		}
		got = append(got, effect.issue)
	}
	return got
}

func messagedChildren(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	var got []string
	for _, effect := range effects(t, pool) {
		if _, ok := effect.payload.(record.MessagePost); ok && effect.issue != finishedRoot {
			got = append(got, effect.issue)
		}
	}
	return got
}

func architectStarts(t *testing.T, pool *pgxpool.Pool, key string) int {
	t.Helper()
	starts := 0
	for _, effect := range effects(t, pool) {
		if request, ok := effect.payload.(record.SuperviseRequest); ok && effect.issue == key && request.Op == "start" && request.Role == claim.RoleArchitect {
			starts++
		}
	}
	return starts
}
