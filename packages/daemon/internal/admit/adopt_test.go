package admit

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// seedOpenTree seeds root holding an admission slot with its design gate approved at its latest
// version, so a todo child entering its tree starts planning at once.
func seedOpenTree(t *testing.T, pool *pgxpool.Pool, root string) {
	t.Helper()
	seedSlotted(t, pool, root, "A")
	approved := 1
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutGate(context.Background(), tx, record.DesignGate{Issue: root, ArtifactID: "7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26", LatestVersion: 1, ApprovedVersion: &approved}); err != nil {
			t.Fatalf("seed gate: %v", err)
		}
	})
}

// queuedRoot is a root recorded waiting for a slot that never ran: its own tree, admitted at
// generation 1, todo with the label, its parent recorded as parent ("" for none).
func queuedRoot(key, parent, rank string, seq int64) record.Issue {
	return record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject, Title: key, Tree: key,
		Parent: record.ParentOf(parent), Phase: phase.Admitted, Generation: 1, Status: "todo", DispatchStatus: "todo", Rank: rank, HandedOver: true, LastDispatchSeq: seq}
}

// joinedNotices is the reason of every child-status notice queued on child.
func joinedNotices(t *testing.T, pool *pgxpool.Pool, child string) []string {
	t.Helper()
	var reasons []string
	for _, got := range effects(t, pool) {
		if notice, ok := got.payload.(record.Notice); ok && got.issue == child && notice.Kind == "child-status" {
			reasons = append(reasons, notice.Reason)
		}
	}
	return reasons
}

// assertJoinedAndPlanning asserts child left admission's queue for tree's run: recorded in tree at
// generation, planning under the open gate with its planner's start queued at that generation, the
// tree's architect told once, and — read again by a later admission pass with a slot to spare —
// no slot, no tree lifecycle and no architect of its own.
func assertJoinedAndPlanning(t *testing.T, pool *pgxpool.Pool, child, tree string, generation uint64) {
	t.Helper()
	got := issue(t, pool, child)
	if got.Tree != tree || got.Generation != generation || got.Phase != phase.Planning {
		t.Fatalf("%s = %#v, want it planning in the tree of %s at generation %d", child, got, tree, generation)
	}
	planners := 0
	for _, effect := range effects(t, pool) {
		if request, ok := effect.payload.(record.SuperviseRequest); ok && effect.issue == child && request.Op == "start" && request.Role == claim.RolePlanner {
			if request.Tree != tree || request.Generation != generation || request.Phase != phase.Planning {
				t.Fatalf("planner start of %s = %#v, want it in the tree of %s at generation %d", child, request, tree, generation)
			}
			planners++
		}
	}
	if planners != 1 {
		t.Fatalf("%s has %d planner starts queued, want one", child, planners)
	}
	if want := []string{child + " joined the tree of " + tree + " from the admission queue"}; !reflect.DeepEqual(joinedNotices(t, pool, child), want) {
		t.Fatalf("child-status notices of %s = %q, want %q", child, joinedNotices(t, pool, child), want)
	}
	assertWaiting(t, pool, nil)

	spare := newAdmission(t, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	applyFact(t, pool, spare, "tick-after-"+child, intake.ControllerTick{})
	for _, slot := range slots(t, pool) {
		if slot.Issue == child {
			t.Fatalf("%s holds slot %#v, want none: it runs in the tree of %s", child, slot, tree)
		}
	}
	var lifecycles int
	if err := pool.QueryRow(context.Background(), `select count(*) from tree_lifecycles where tree = $1`, child).Scan(&lifecycles); err != nil {
		t.Fatalf("count lifecycles: %v", err)
	}
	if lifecycles != 0 {
		t.Fatalf("%s has %d tree lifecycles, want none: it never ran as a tree of its own", child, lifecycles)
	}
	for _, effect := range effects(t, pool) {
		if request, ok := effect.payload.(record.SuperviseRequest); ok && request.Op == "start" && request.Role == claim.RoleArchitect && request.Tree == child {
			t.Fatalf("queued an architect start %#v of a tree %s, want none", request, child)
		}
	}
}

// A labelled root waiting in admission's queue that a person makes a child of a running tree
// leaves the queue on that event: it joins the tree at its next generation and, the tree's gate
// open, starts planning there, as any todo child of the tree does. It never takes a slot or an
// architect of its own, even once a slot stands free.
func TestAQueuedRootReparentedUnderALiveTreeJoinsItAndTakesNoSlot(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	const tree, child = "LEGION-168", "LEGION-214"
	seedOpenTree(t, pool, tree)

	apply(t, pool, admission, "queued", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated", Status: "todo", Title: child, Rank: "B", HandedOver: handed}, admission.engine)
	assertWaiting(t, pool, []string{child})

	apply(t, pool, admission, "reparented", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "todo", Title: child, Parent: tree, Rank: "B", HandedOver: handed}, admission.engine)
	if got := issue(t, pool, child); got.Parent == nil || *got.Parent != tree {
		t.Fatalf("%s = %#v, want its parent recorded as %s", child, got, tree)
	}
	assertJoinedAndPlanning(t, pool, child, tree, 2)
}

// The boot read adopts the same queued root when the re-parent happened while the daemon was down:
// the listing's parent reaches the record and the root joins the running tree before promotion.
func TestBootReconcileAdoptsAQueuedRootUnderALiveTree(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	const tree, child = "LEGION-168", "LEGION-214"
	seedOpenTree(t, pool, tree)
	putIssue(t, pool, queuedRoot(child, "", "B", 1))
	parent := tree

	reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{
		{Key: child, ResolvedRepo: "acme/widgets", Title: child, Status: "todo", Parent: &parent, Rank: "B", HandedOver: handed, LastSeq: 2},
	}, 10, 10, true)
	assertJoinedAndPlanning(t, pool, child, tree, 2)
}

// A root that already ran as a tree of its own keeps queueing as its own root under a running
// tree: its tree lifecycle says it ran, and it takes the slot that frees.
func TestARootThatRanAsATreeOfItsOwnKeepsQueueingUnderALiveTree(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	const tree, child = "LEGION-168", "LEGION-214"
	seedOpenTree(t, pool, tree)
	putIssue(t, pool, queuedRoot(child, tree, "B", 2))
	if _, err := pool.Exec(context.Background(), `insert into tree_lifecycles (project, tree, epoch, authority) values ('legion', $1, 1, 'workflow')`, child); err != nil {
		t.Fatalf("seed lifecycle: %v", err)
	}

	applyFact(t, pool, admission, "tick", intake.ControllerTick{})
	if got := issue(t, pool, child); got.Tree != child || got.Generation != 1 {
		t.Fatalf("%s = %#v, want it still its own root at generation 1", child, got)
	}
	assertWaiting(t, pool, []string{child})

	if _, err := intake.ApplyFact(context.Background(), pool, "test", "tree-done", intake.ControllerTick{}, engineStub{store: record.NewStore(), done: tree}, admission); err != nil {
		t.Fatalf("ApplyFact tree-done: %v", err)
	}
	if got := issue(t, pool, child); got.Tree != child || got.Status != "in_progress" {
		t.Fatalf("%s = %#v, want it promoted as its own root", child, got)
	}
	assertSlots(t, pool, []record.Slot{{Issue: child, Index: 0, AdmittedAt: fixedNow}})
}

// An orphan that carries a run handed over to it stays its own root when its old root comes back:
// it keeps its tree and takes its slot, as the hand-over contract says.
func TestACarriedOrphanUnderAReopenedOldRootStillGetsItsSlot(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	const old, child = "LEGION-100", "LEGION-214"
	seedOpenTree(t, pool, old)
	carried := queuedRoot(child, old, "B", 2)
	carried.Phase, carried.Generation = phase.Planning, 2
	putIssue(t, pool, carried)
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutHandedOverRoot(context.Background(), tx, record.HandedOverRoot{Issue: child, Generation: 2}); err != nil {
			t.Fatalf("seed handed-over root: %v", err)
		}
	})

	applyFact(t, pool, admission, "tick", intake.ControllerTick{})
	if got := issue(t, pool, child); got.Tree != child || got.Generation != 2 || got.Status != "in_progress" {
		t.Fatalf("%s = %#v, want its own root, promoted at generation 2", child, got)
	}
	assertSlots(t, pool, []record.Slot{{Issue: old, Index: 0, AdmittedAt: fixedNow}, {Issue: child, Index: 1, AdmittedAt: fixedNow}})
}

// Queued roots nested under each other join the running tree at the top: each takes one new
// generation, and the tree's architect is told each joined its own tree.
func TestNestedQueuedRootsJoinTheLiveTreeAtTheTop(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	const tree, middle, leaf = "LEGION-168", "LEGION-214", "LEGION-213"
	seedOpenTree(t, pool, tree)
	putIssue(t, pool, queuedRoot(middle, tree, "B", 2))
	putIssue(t, pool, queuedRoot(leaf, middle, "C", 2))

	applyFact(t, pool, admission, "tick", intake.ControllerTick{})
	for _, key := range []string{middle, leaf} {
		got := issue(t, pool, key)
		if got.Tree != tree || got.Generation != 2 || got.Phase != phase.Planning {
			t.Fatalf("%s = %#v, want it planning in the tree of %s at generation 2", key, got, tree)
		}
		if want := []string{key + " joined the tree of " + tree + " from the admission queue"}; !reflect.DeepEqual(joinedNotices(t, pool, key), want) {
			t.Fatalf("child-status notices of %s = %q, want %q", key, joinedNotices(t, pool, key), want)
		}
	}
	if got := issue(t, pool, leaf); got.Parent == nil || *got.Parent != middle {
		t.Fatalf("%s = %#v, want its parent kept as %s", leaf, got, middle)
	}
	assertWaiting(t, pool, nil)
	assertSlots(t, pool, []record.Slot{{Issue: tree, Index: 0, AdmittedAt: fixedNow}})
}

// A queued root that waits on an unfinished Dispatch issue still joins the live tree its recorded
// parent runs in, and the blocked-by rule then holds it there like any child: it is recorded in
// the tree at its next generation, admitted and not planning, though the tree's gate is open, and
// takes no slot. The close of its last blocker starts it.
func TestAQueuedRootThatWaitsOnAnUnfinishedIssueJoinsTheLiveTreeAndIsHeldThere(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	const tree, root, blocker = "LEGION-168", "LEGION-214", "LEGION-400"
	seedOpenTree(t, pool, tree)
	putIssue(t, pool, queuedRoot(root, tree, "B", 2))
	seedMirror(t, pool, root, "todo", blocker)
	seedMirror(t, pool, blocker, "in_progress")

	applyFact(t, pool, admission, "tick", intake.ControllerTick{})
	if got := issue(t, pool, root); got.Tree != tree || got.Generation != 2 || got.Phase != phase.Admitted {
		t.Fatalf("%s = %#v, want it held admitted in the tree of %s at generation 2", root, got, tree)
	}
	if want := []string{root + " joined the tree of " + tree + " from the admission queue"}; !reflect.DeepEqual(joinedNotices(t, pool, root), want) {
		t.Fatalf("child-status notices of %s = %q, want %q", root, joinedNotices(t, pool, root), want)
	}
	assertWaiting(t, pool, nil)
	assertSlots(t, pool, []record.Slot{{Issue: tree, Index: 0, AdmittedAt: fixedNow}})

	applyMirrored(t, pool, admission, "blocker-done", intake.DispatchIssue{Key: blocker, ResolvedRepo: "acme/widgets", Seq: 5, Type: "issue.updated",
		Status: "done", Title: blocker, Rank: "Z"}, admission.engine)
	if got := issue(t, pool, root); got.Tree != tree || got.Phase != phase.Planning {
		t.Fatalf("%s = %#v once %s is done, want it planning in the tree of %s", root, got, blocker, tree)
	}
}

// A child that left its tree as a root of its own stays its own tree when a person sets its old
// root back to todo with the label: the old root's re-admission adopts nothing back. The child
// keeps its tree, its generation and its place in the waiting line, held there only by the blocker
// Dispatch names (#2188's rule), never by the old root's tree on its chain, and is admitted into a
// slot of its own when that blocker closes. Both ways out of a tree are covered: a child still
// held on its blocker that its finished tree handed over without a run, and a child that closed in
// its tree which a person set back to todo once the tree had finished, admitted as an orphan.
func TestAHandedOverChildStaysItsOwnTreeWhenItsOldRootIsReadmitted(t *testing.T) {
	const old, child, blocker = "LEGION-208", "LEGION-209", "LEGION-BLOCKER"
	for _, tc := range []struct {
		name  string
		leave func(t *testing.T, pool *pgxpool.Pool, admission *Admission)
	}{
		{name: "a child handed over without a run", leave: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
			seedLingeringHandovers(t, pool, old, child)
			// Released by its tree but held on its blocker: still at the phase its tree's gate opened
			// it at when the tree finished, with the hand-over fence standing behind it.
			unstarted := issue(t, pool, child)
			unstarted.Phase = phase.Admitted
			putIssue(t, pool, unstarted)
			applyMirrored(t, pool, admission, "handover", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets",
				Seq: 2, Type: "issue.updated", Status: "todo", Title: child, Parent: old, Ancestry: []string{old}, Rank: "B",
				HandedOver: handed, DaemonActor: true, Blockers: []string{blocker}}, admission.engine)
		}},
		{name: "a closed child a person set back to todo", leave: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
			parent := old
			putIssue(t, pool, record.Issue{Key: old, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Project: testProject, Title: old, Tree: old, Phase: phase.Done, Generation: 1, Status: "done", DispatchStatus: "done",
				Rank: "A", HandedOver: true, TakenBack: true, LastDispatchSeq: 1})
			putIssue(t, pool, record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Project: testProject, Title: child, Tree: old, Parent: &parent, Phase: phase.Done, Generation: 1, Status: "done",
				DispatchStatus: "done", Rank: "B", TakenBack: true, LastDispatchSeq: 1})
			applyMirrored(t, pool, admission, "person-reopens", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets",
				Seq: 2, Type: "issue.updated", Status: "todo", Title: child, Parent: old, Ancestry: []string{old}, Rank: "B",
				HandedOver: handed, Blockers: []string{blocker}}, admission.engine)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := quiet(t, 2)
			seedMirror(t, pool, child, "todo", blocker)
			seedMirror(t, pool, blocker, "in_progress")
			tc.leave(t, pool, admission)

			left := issue(t, pool, child)
			if left.Tree != child || left.Generation != 2 || left.Phase != phase.Admitted || left.HandedOverFrom != old {
				t.Fatalf("%s = %#v, want a root of its own at generation 2, admitted, recorded as having left %s", child, left, old)
			}
			assertSlots(t, pool, nil)
			assertWaiting(t, pool, []string{child})

			applyMirrored(t, pool, admission, "old-readmitted", intake.DispatchIssue{Key: old, ResolvedRepo: "acme/widgets", Seq: 3,
				Type: "issue.updated", Status: "todo", Title: old, Rank: "A", HandedOver: handed}, admission.engine)
			if got := issue(t, pool, child); !reflect.DeepEqual(got, left) {
				t.Fatalf("%s after %s's re-admission = %#v, want it untouched as %#v", child, old, got, left)
			}
			if reasons := joinedNotices(t, pool, child); len(reasons) != 0 {
				t.Fatalf("child-status notices of %s = %q, want none: it joined no tree", child, reasons)
			}
			assertSlots(t, pool, []record.Slot{{Issue: old, Index: 0, AdmittedAt: fixedNow}})
			// Its chain names the tree it left, now live again, yet nothing holds it under that tree.
			if left.Ancestry != nil {
				t.Fatalf("%s records the chain %q, want none: it is never held under the tree it left", child, left.Ancestry)
			}
			assertWaiting(t, pool, []string{child})
			inTx(t, pool, func(tx pgx.Tx) {
				blocked, err := admission.blocked(context.Background(), tx)
				if err != nil {
					t.Fatalf("read what is blocked: %v", err)
				}
				if want := []record.BlockedIssue{{Issue: child, BlockedBy: []string{blocker}}}; !reflect.DeepEqual(blocked, want) {
					t.Fatalf("blocked = %#v, want %#v", blocked, want)
				}
			})

			applyMirrored(t, pool, admission, "blocker-done", intake.DispatchIssue{Key: blocker, ResolvedRepo: "acme/widgets",
				Seq: 5, Type: "issue.updated", Status: "done", Title: blocker, Rank: "Z"}, engineStub{})
			if got := issue(t, pool, child); got.Tree != child || got.Status != "in_progress" {
				t.Fatalf("%s = %#v once %s is done, want it promoted as its own root", child, got, blocker)
			}
			assertSlots(t, pool, []record.Slot{{Issue: old, Index: 0, AdmittedAt: fixedNow}, {Issue: child, Index: 1, AdmittedAt: fixedNow}})
		})
	}
}

// A queued root the boot read holds for the Dispatch consumer to catch up is not adopted on the
// record the stream may still correct; it is adopted once the consumer reaches the stream position.
func TestAHeldQueuedRootIsAdoptedOnlyOnceTheHoldReleases(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	const tree, child = "LEGION-168", "LEGION-214"
	seedOpenTree(t, pool, tree)
	putIssue(t, pool, queuedRoot(child, tree, "B", 2))
	parent := tree

	reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{
		{Key: child, ResolvedRepo: "acme/widgets", Title: child, Status: "todo", Parent: &parent, Rank: "B", HandedOver: handed, LastSeq: 3},
	}, 5, 2, false)
	if !admission.Held() {
		t.Fatal("Reconcile held nothing, want the queued root held behind the stream")
	}
	if got := issue(t, pool, child); got.Tree != child || got.Generation != 1 {
		t.Fatalf("held %s = %#v, want it left its own root at generation 1", child, got)
	}

	applyFact(t, pool, admission, "position-reached", intake.DispatchConsumerPosition{AckFloorStream: 5})
	if admission.Held() {
		t.Fatal("the hold is still standing once the consumer reached its position")
	}
	assertJoinedAndPlanning(t, pool, child, tree, 2)
}

// A labelled todo issue under a backlog child of a running tree, which that tree's architect has
// not released, is recorded as a root of its own (its parent is not recorded) but waits for no
// slot while the tree runs: it takes no slot and no architect, even with a slot free. Once the
// tree leaves the workflow, it waits as its own root again and takes the slot.
func TestAQueuedRootUnderAnUnreleasedChildOfALiveTreeTakesNoSlot(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	const tree, parent, child = "LEGION-168", "LEGION-169", "LEGION-214"
	seedOpenTree(t, pool, tree)

	// The preparation read the parent's chain from Dispatch: parent is not recorded, tree is.
	apply(t, pool, admission, "queued", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated", Status: "todo",
		Title: child, Parent: parent, Rank: "B", HandedOver: handed, Ancestry: []string{parent, tree}}, admission.engine)
	if got := issue(t, pool, child); got.Tree != child || got.Status != "todo" || got.Parent == nil || *got.Parent != parent {
		t.Fatalf("%s = %#v, want it recorded todo as a root of its own under %s", child, got, parent)
	}
	assertWaiting(t, pool, nil)
	applyFact(t, pool, admission, "tick", intake.ControllerTick{})
	assertWaiting(t, pool, nil)
	assertSlots(t, pool, []record.Slot{{Issue: tree, Index: 0, AdmittedAt: fixedNow}})
	if starts := architectStarts(t, pool, child); starts != 0 {
		t.Fatalf("%s has %d architect starts queued, want none while %s runs", child, starts, tree)
	}
	if got := issue(t, pool, child); got.Status != "todo" {
		t.Fatalf("%s = %#v, want it still todo", child, got)
	}

	apply(t, pool, admission, "tree-done", intake.DispatchIssue{Key: tree, ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated", Status: "done",
		Title: tree, Rank: "A", HandedOver: handed}, admission.engine)
	if got := issue(t, pool, tree); !got.Lingers() {
		t.Fatalf("%s = %#v, want it lingering after its close", tree, got)
	}
	if got := issue(t, pool, child); got.Tree != child || got.Status != "in_progress" {
		t.Fatalf("%s = %#v, want it promoted as its own root once %s left the workflow", child, got, tree)
	}
	assertSlots(t, pool, []record.Slot{{Issue: child, Index: 0, AdmittedAt: fixedNow}})
	if starts := architectStarts(t, pool, child); starts != 1 {
		t.Fatalf("%s has %d architect starts queued, want one once it took the slot", child, starts)
	}
}

// A labelled todo issue whose Dispatch chain runs through a queued root to a live tree stays held
// when admission's sweep adopts that queued root into the live tree its recorded parent runs in:
// the live tree's root is on the issue's chain, so the issue takes no slot with one free. Once that
// tree lingers, no live root is left on the chain, and the issue waits as its own root again and
// takes the slot.
func TestAnAncestryHoldStandsWhileAQueuedRootOnItsChainIsAdoptedIntoTheLiveTree(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	const tree, ancestor, parent, child = "LEGION-168", "LEGION-170", "LEGION-169", "LEGION-214"
	seedOpenTree(t, pool, tree)
	putIssue(t, pool, queuedRoot(ancestor, tree, "C", 1))

	// The preparation read the parent's chain from Dispatch: parent is not recorded, then the queued
	// root, a tree of its own when it was read, then the live tree.
	chain := []string{parent, ancestor, tree}
	apply(t, pool, admission, "queued", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated", Status: "todo",
		Title: child, Parent: parent, Rank: "B", HandedOver: handed, Ancestry: chain}, admission.engine)
	if got := issue(t, pool, ancestor); got.Tree != tree {
		t.Fatalf("%s = %#v, want it adopted into the tree of %s", ancestor, got, tree)
	}
	if got := issue(t, pool, child); got.Tree != child || got.Status != "todo" || !slices.Equal(got.Ancestry, chain) {
		t.Fatalf("%s = %#v, want it recorded todo as a root of its own under %v", child, got, chain)
	}
	applyFact(t, pool, admission, "tick", intake.ControllerTick{})
	assertWaiting(t, pool, nil)
	assertSlots(t, pool, []record.Slot{{Issue: tree, Index: 0, AdmittedAt: fixedNow}})
	if starts := architectStarts(t, pool, child); starts != 0 {
		t.Fatalf("%s has %d architect starts queued, want none while %s runs with %s in it", child, starts, tree, ancestor)
	}

	apply(t, pool, admission, "tree-done", intake.DispatchIssue{Key: tree, ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated", Status: "done",
		Title: tree, Rank: "A", HandedOver: handed}, admission.engine)
	if got := issue(t, pool, tree); !got.Lingers() {
		t.Fatalf("%s = %#v, want it lingering after its close", tree, got)
	}
	if got := issue(t, pool, child); got.Tree != child || got.Status != "in_progress" {
		t.Fatalf("%s = %#v, want it promoted as its own root once %s lingers", child, got, tree)
	}
	assertSlots(t, pool, []record.Slot{{Issue: child, Index: 0, AdmittedAt: fixedNow}})
	if starts := architectStarts(t, pool, child); starts != 1 {
		t.Fatalf("%s has %d architect starts queued, want one once it took the slot", child, starts)
	}
}

// A labelled todo issue whose Dispatch chain reaches a live tree past a recorded root parked out of
// the workflow (INFRA-214's state: a once-queued root a person moved under the live tree and did
// not release) is held under the live tree, whose release would reach it, not passed by the parked
// root, which is not live: it takes no slot and no architect with one free. Once the live tree
// lingers, no live root is left on its chain, and it waits as its own root and takes the slot.
func TestARootHeldPastAParkedRootOnItsChainQueuesOnceTheTreeLingers(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	const tree, parked, parent, child = "LEGION-168", "LEGION-180", "LEGION-169", "LEGION-214"
	seedOpenTree(t, pool, tree)
	recorded := queuedRoot(parked, tree, "C", 1)
	recorded.Status, recorded.DispatchStatus, recorded.Phase, recorded.HandedOver = "backlog", "backlog", phase.Done, false
	putIssue(t, pool, recorded)

	// The preparation read the parent's chain from Dispatch: parent is not recorded, then the parked
	// root, then the live tree.
	apply(t, pool, admission, "queued", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated", Status: "todo",
		Title: child, Parent: parent, Rank: "B", HandedOver: handed, Ancestry: []string{parent, parked, tree}}, admission.engine)
	applyFact(t, pool, admission, "tick", intake.ControllerTick{})
	assertWaiting(t, pool, nil)
	assertSlots(t, pool, []record.Slot{{Issue: tree, Index: 0, AdmittedAt: fixedNow}})
	if starts := architectStarts(t, pool, child); starts != 0 {
		t.Fatalf("%s has %d architect starts queued, want none while %s runs above %s", child, starts, tree, parked)
	}

	apply(t, pool, admission, "tree-done", intake.DispatchIssue{Key: tree, ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated", Status: "done",
		Title: tree, Rank: "A", HandedOver: handed}, admission.engine)
	if got := issue(t, pool, tree); !got.Lingers() {
		t.Fatalf("%s = %#v, want it lingering after its close", tree, got)
	}
	if got := issue(t, pool, child); got.Tree != child || got.Status != "in_progress" {
		t.Fatalf("%s = %#v, want it promoted as its own root once %s lingers", child, got, tree)
	}
	assertSlots(t, pool, []record.Slot{{Issue: child, Index: 0, AdmittedAt: fixedNow}})
}

// A labelled todo child of a root held under a running tree's unreleased child is recorded in the
// held root's tree, as any todo child of a waiting root is: it waits with the held root and takes no
// slot and no architect, even with a slot free. When the running tree's architect releases the held
// root, the child joins that tree with it.
func TestAChildOfAnAncestryHeldRootWaitsWithIt(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	const tree, parent, held, child = "LEGION-168", "LEGION-169", "LEGION-214", "LEGION-300"
	seedOpenTree(t, pool, tree)

	// The preparation read the held root's chain from Dispatch: parent is not recorded, tree is.
	apply(t, pool, admission, "held", intake.DispatchIssue{Key: held, ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated", Status: "todo",
		Title: held, Parent: parent, Rank: "B", HandedOver: handed, Ancestry: []string{parent, tree}}, admission.engine)
	apply(t, pool, admission, "child", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated", Status: "todo",
		Title: child, Parent: held, Rank: "A", HandedOver: handed}, admission.engine)
	if got := issue(t, pool, child); got.Tree != held || got.Status != "todo" || got.Phase != phase.Admitted {
		t.Fatalf("%s = %#v, want it recorded todo and admitted in the tree of %s", child, got, held)
	}
	applyFact(t, pool, admission, "tick", intake.ControllerTick{})
	assertWaiting(t, pool, nil)
	assertSlots(t, pool, []record.Slot{{Issue: tree, Index: 0, AdmittedAt: fixedNow}})
	for _, key := range []string{held, child} {
		if starts := architectStarts(t, pool, key); starts != 0 {
			t.Fatalf("%s has %d architect starts queued, want none while %s runs", key, starts, tree)
		}
	}

	applyFact(t, pool, admission, "release", intake.ChildAdoption{Tree: tree, Children: []intake.AdoptedChild{{Issue: held, Parent: parent}}})
	for _, key := range []string{held, child} {
		if got := issue(t, pool, key); got.Tree != tree || got.Generation != 2 || got.Phase != phase.Planning {
			t.Fatalf("%s = %#v, want it planning in the tree of %s at generation 2", key, got, tree)
		}
	}
	assertWaiting(t, pool, nil)
	assertSlots(t, pool, []record.Slot{{Issue: tree, Index: 0, AdmittedAt: fixedNow}})
	if starts := architectStarts(t, pool, child); starts != 0 {
		t.Fatalf("%s has %d architect starts queued, want none: it runs in the tree of %s", child, starts, tree)
	}
}

// A root that ran as a tree of its own, or carries a run handed over to it, keeps queueing as its
// own root under a running tree's unreleased child, since no release adopts it: it records no
// ancestor to be held under, waits in the line and takes the free slot.
func TestARootThatRanKeepsQueueingUnderAnUnreleasedChild(t *testing.T) {
	const tree, parent, root = "LEGION-168", "LEGION-169", "LEGION-214"
	for _, tc := range []struct {
		name string
		seed func(t *testing.T, pool *pgxpool.Pool) record.Issue
	}{
		{name: "a tree lifecycle under its key", seed: func(t *testing.T, pool *pgxpool.Pool) record.Issue {
			ran := queuedRoot(root, "", "B", 1)
			putIssue(t, pool, ran)
			if _, err := pool.Exec(context.Background(), `insert into tree_lifecycles (project, tree, epoch, authority) values ('legion', $1, 1, 'workflow')`, root); err != nil {
				t.Fatalf("seed lifecycle: %v", err)
			}
			return ran
		}},
		{name: "a carried orphan", seed: func(t *testing.T, pool *pgxpool.Pool) record.Issue {
			carried := queuedRoot(root, "", "B", 1)
			carried.Phase, carried.Generation = phase.Planning, 2
			putIssue(t, pool, carried)
			inTx(t, pool, func(tx pgx.Tx) {
				if err := record.NewStore().PutHandedOverRoot(context.Background(), tx, record.HandedOverRoot{Issue: root, Generation: 2}); err != nil {
					t.Fatalf("seed handed-over root: %v", err)
				}
			})
			return carried
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
			seedOpenTree(t, pool, tree)
			seeded := tc.seed(t, pool)

			// The preparation read the root's chain from Dispatch: parent is not recorded, tree is.
			apply(t, pool, admission, "reparented", intake.DispatchIssue{Key: root, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "todo",
				Title: root, Parent: parent, Rank: "B", HandedOver: handed, Ancestry: []string{parent, tree}}, admission.engine)
			got := issue(t, pool, root)
			if got.Tree != root || got.Generation != seeded.Generation || got.Status != "in_progress" || len(got.Ancestry) != 0 {
				t.Fatalf("%s = %#v, want it promoted as its own root at generation %d with no Dispatch chain", root, got, seeded.Generation)
			}
			if got.Parent == nil || *got.Parent != parent {
				t.Fatalf("%s = %#v, want its parent recorded as %s", root, got, parent)
			}
			assertSlots(t, pool, []record.Slot{{Issue: tree, Index: 0, AdmittedAt: fixedNow}, {Issue: root, Index: 1, AdmittedAt: fixedNow}})
			if starts := architectStarts(t, pool, root); starts != 1 {
				t.Fatalf("%s has %d architect starts queued, want one once it took the slot", root, starts)
			}
		})
	}
}

// A root that takes a slot leaves the chain it queued under behind: it runs as a tree of its own,
// which no ancestry hold holds. So when it later waits on a person and is answered, it comes back to
// the line on its answer alone, even with a live root on the chain it once queued under, rather than
// being held until its next observation reads that chain again.
func TestAnAnsweredRootIsNotHeldByTheChainItQueuedUnder(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	const root, ancestor = "LEGION-214", "LEGION-168"
	queued := queuedRoot(root, "LEGION-169", "B", 2)
	queued.Ancestry = []string{"LEGION-169", ancestor}
	putIssue(t, pool, queued)

	tick(t, pool, admission, "tick-promote")
	promoted := issue(t, pool, root)
	if promoted.Status != "in_progress" || promoted.Ancestry != nil {
		t.Fatalf("%s = %#v, want it promoted with no chain left", root, promoted)
	}

	// The root waits on a person, gives its slot back, and is answered; meanwhile the ancestor on
	// its old chain is a live root holding the slot.
	at := fixedNow
	promoted.Phase = phase.AwaitingPerson
	promoted.Awaiting = &record.Awaiting{Ask: "ask-q", AnsweredAt: &at, AnsweredBy: record.AnsweredByAsk, AnsweredDetail: "ask-q"}
	putIssue(t, pool, promoted)
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().ReleaseSlot(context.Background(), tx, root); err != nil {
			t.Fatalf("give the slot back: %v", err)
		}
	})
	seedSlotted(t, pool, ancestor, "A")

	assertWaiting(t, pool, []string{root})
}
