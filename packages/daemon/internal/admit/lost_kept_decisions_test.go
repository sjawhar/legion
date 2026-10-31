package admit

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// namings is everything the architect of tree is told of what a person decided while nobody in
// that tree could hear it: the task of each architect start queued for it
// (workflow.Engine.ParkedDecisionTask) and what each of its catch-ups says was decided
// (record.CatchUp.Decided). Those are the only two texts that name a kept decision to an
// architect, so a decision named in none of them reaches that architect never.
func namings(t *testing.T, pool *pgxpool.Pool, tree string) []string {
	t.Helper()
	var told []string
	for _, queued := range effects(t, pool) {
		if queued.issue != tree {
			continue
		}
		switch payload := queued.payload.(type) {
		case record.SuperviseRequest:
			if payload.Op == "start" && payload.Role == claim.RoleArchitect {
				told = append(told, payload.Task)
			}
		case record.Notice:
			if payload.CatchUp != nil {
				told = append(told, payload.CatchUp.Decided)
			}
		}
	}
	return told
}

// timesNamed is how many of the texts name ref.
func timesNamed(told []string, ref string) int {
	named := 0
	for _, text := range told {
		if strings.Contains(text, ref) {
			named++
		}
	}
	return named
}

// A child a linger hands to Legion without its run — one still at phase.Admitted that a person's
// issue holds there, which is the one admitted child a close hands over (workflow's
// unfinishedHandOvers) — takes the decisions a person made on it while the old root lingered with
// nobody in its tree to hear them: it is a tree of its own from here on, and its own architect is
// the one that owes the person an answer. Left under the old root, they wait under a root nobody
// runs again and that architect is never told.
func TestAChildHandedOverWithoutItsRunTakesTheDecisionKeptOnIt(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 2)
	const old, child, blocker = "LEGION-208", "LEGION-209", "LEGION-300"
	seedLingeringHandovers(t, pool, old, child)
	// The child never started a phase and waits on an issue Dispatch has not finished, which is
	// why its tree's close hands it over at all: it carries no run (record.Issue.CarriesRun).
	held := issue(t, pool, child)
	held.Phase = phase.Admitted
	putIssue(t, pool, held)
	seedMirror(t, pool, blocker, "in_progress")
	seedMirror(t, pool, child, "in_progress", blocker)
	childAsk := "dispatch://LEGION-209/ask/ask-1"
	keepParkedDecision(t, pool, old, child, childAsk)

	// The linger's end hands the child over, which admission re-admits as a root of its own.
	applyMirrored(t, pool, admission, "handover", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets",
		Seq: 2, Type: "issue.updated", Status: "todo", Title: child, Parent: old, Blockers: []string{blocker},
		Rank: "B", HandedOver: handed, DaemonActor: true}, admission.engine)
	if got := issue(t, pool, child); got.Tree != child || got.CarriesRun() {
		t.Fatalf("%s = %#v, want a tree of its own carrying no run", child, got)
	}
	// What held it is finished, so the child takes a slot and its architect starts.
	applyMirrored(t, pool, admission, "blocker-done", intake.DispatchIssue{Key: blocker, ResolvedRepo: "acme/widgets",
		Seq: 2, Type: "issue.updated", Status: "done", Title: blocker}, admission.engine)

	task := architectStartTask(t, pool, child)
	if !strings.Contains(task, childAsk) {
		t.Fatalf("the start task of %s = %q, want it to name %s, which a person decided on %s while its old root lingered: "+
			"a child handed over without its run takes what was decided on it, or nobody is ever told",
			child, task, childAsk, child)
	}
	if left := keptRefs(t, pool, old); len(left) != 0 {
		t.Fatalf("%s keeps %q after the hand-over, want none: %s was decided on an issue that is now a tree of its own, "+
			"and no start or catch-up of %s names it again", old, left, childAsk, old)
	}
}

// A person who sets a parked child of a closed tree back to todo makes it an orphan root of its
// own, carrying no run (its phase is done). The decisions a person made on it while that tree was
// nobody's to hear are owed to the architect of the root it becomes: its start names them. Left
// under the closed root, they stand where nothing names them again.
func TestAnOrphanedParkedChildTakesTheDecisionKeptOnIt(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 2)
	const old, child = "LEGION-208", "LEGION-209"
	putIssue(t, pool, record.Issue{Key: old, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Tree: old, Project: testProject, Title: old, Phase: phase.Done, Generation: 1, Status: "done", Rank: "A",
		HandedOver: true, LastDispatchSeq: 1})
	parent := old
	putIssue(t, pool, record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Tree: old, Project: testProject, Parent: &parent, Title: child, Phase: phase.Done, Generation: 1,
		Status: "backlog", Rank: "B", LastDispatchSeq: 1})
	childAsk := "dispatch://LEGION-209/ask/ask-1"
	keepParkedDecision(t, pool, old, child, childAsk)

	applyMirrored(t, pool, admission, "person-labels", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets",
		Seq: 2, Type: "issue.updated", Status: "todo", Title: child, Parent: old, Rank: "B", HandedOver: handed},
		admission.engine)
	if got := issue(t, pool, child); got.Tree != child || got.CarriesRun() {
		t.Fatalf("%s = %#v, want a tree of its own carrying no run", child, got)
	}

	task := architectStartTask(t, pool, child)
	if !strings.Contains(task, childAsk) {
		t.Fatalf("the start task of %s = %q, want it to name %s, which a person decided on %s while its tree was closed: "+
			"an orphan takes what was decided on it, or nobody is ever told", child, task, childAsk, child)
	}
	if left := keptRefs(t, pool, old); len(left) != 0 {
		t.Fatalf("%s keeps %q after the orphan's admission, want none: %s was decided on an issue that is now a tree of its own",
			old, left, childAsk)
	}
}

// A root that waits for its slot keeps a person's decision for the start its admission will queue
// (record.RelayParkedDecision, RelayKept). When a person makes that root a child of a live tree
// instead, admission adopts it into that tree (adoptQueuedRoots) and it takes no start of its own
// ever again: the decision is owed to the architect of the tree it joined, which is told it once —
// at that architect's next ready, whose catch-up names every decision nobody claims. Left kept
// under the adopted root, it is named to nobody.
func TestAQueuedRootAdoptedIntoALiveTreeOwesItsDecisionToThatTreesArchitect(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	const tree, queued = "LEGION-168", "LEGION-214"
	seedOpenTree(t, pool, tree)
	putIssue(t, pool, queuedRoot(queued, tree, "B", 1))
	ask := "dispatch://LEGION-214/ask/ask-1"
	keepParkedDecision(t, pool, queued, queued, ask)

	applyFact(t, pool, admission, "tick", intake.ControllerTick{})
	if got := issue(t, pool, queued); got.Tree != tree {
		t.Fatalf("%s = %#v, want it adopted into the live tree of %s", queued, got, tree)
	}

	// The tree's architect comes up: its catch-up names every decision nobody else claims, which
	// is where a decision of an issue of its tree reaches it.
	applyFact(t, pool, admission, "architect-ready", intake.ClaimReady{Issue: tree, Role: claim.RoleArchitect, Launch: 1})
	if told := namings(t, pool, tree); timesNamed(told, ask) != 1 {
		t.Fatalf("the architect of %s was told %q, want %s named once: a person decided on %s, which now stands in that tree, "+
			"and no start or catch-up of %s will ever name it", tree, told, ask, queued, queued)
	}
	if left := keptRefs(t, pool, queued); len(left) != 0 {
		t.Fatalf("%s keeps %q after it was adopted into the tree of %s, want none: it is no root, so nothing of its own names them again",
			queued, left, tree)
	}
}

// A linger's end hands a child and the grandchild below it to Legion, each with a fence of its
// own, and admission re-admits them in whichever order their Dispatch events arrive. Whatever that
// order, the decisions a person made on the grandchild are the grandchild's architect's to be
// told, and the child's architect is told none of them: the grandchild is a tree of its own too,
// and what was decided on it went with it, not with the parent that left beside it.
func TestAGrandchildHandedOverBesideItsParentTakesTheDecisionKeptOnIt(t *testing.T) {
	const old, child, grand = "LEGION-208", "LEGION-209", "LEGION-210"
	childAsk, grandAsk := "dispatch://LEGION-209/ask/ask-1", "dispatch://LEGION-210/ask/ask-2"
	for _, tc := range []struct {
		name  string
		order []string
	}{
		{"the child's re-admission first", []string{child, grand}},
		{"the grandchild's first", []string{grand, child}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := quiet(t, 2)
			seedLingeringHandovers(t, pool, old, child)
			parent := child
			putIssue(t, pool, record.Issue{Key: grand, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Tree: old, Project: testProject, Parent: &parent, Title: grand, Phase: phase.Implementing, Generation: 1,
				Status: "in_progress", Rank: "C", LastDispatchSeq: 1})
			inTx(t, pool, func(tx pgx.Tx) {
				if err := record.NewStore().PutHandover(context.Background(), tx, record.Handover{Issue: grand, Tree: old, Generation: 1}); err != nil {
					t.Fatalf("seed the grandchild's fence: %v", err)
				}
			})
			keepParkedDecision(t, pool, old, child, childAsk)
			keepParkedDecision(t, pool, old, grand, grandAsk)

			for _, key := range tc.order {
				handOverParent := old
				if key == grand {
					handOverParent = child
				}
				applyMirrored(t, pool, admission, "handover-"+key, intake.DispatchIssue{Key: key, ResolvedRepo: "acme/widgets",
					Seq: 2, Type: "issue.updated", Status: "todo", Title: key, Parent: handOverParent, Rank: "B",
					HandedOver: handed, DaemonActor: true}, admission.engine)
				if got := issue(t, pool, key); got.Tree != key {
					t.Fatalf("%s = %#v, want a tree of its own", key, got)
				}
			}

			childTask := architectStartTask(t, pool, child)
			if task := architectStartTask(t, pool, grand); !strings.Contains(task, grandAsk) {
				t.Fatalf("the start task of %s = %q, want it to name %s, which a person decided on %s: a grandchild handed over "+
					"beside its parent takes what was decided on it, whichever of the two is re-admitted first "+
					"(the start task of its parent %s = %q)",
					grand, task, grandAsk, grand, child, childTask)
			}
			if strings.Contains(childTask, grandAsk) {
				t.Fatalf("the start task of %s = %q, want nothing of %s, which a person decided on %s, a tree of its own",
					child, childTask, grandAsk, grand)
			}
			if !strings.Contains(childTask, childAsk) {
				t.Fatalf("the start task of %s = %q, want it to name %s, which a person decided on it", child, childTask, childAsk)
			}
			if left := keptRefs(t, pool, old); len(left) != 0 {
				t.Fatalf("%s keeps %q after both hand-overs, want none: each was decided on an issue that is now a tree of its own",
					old, left)
			}
		})
	}
}
