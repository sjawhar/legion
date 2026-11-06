package admit

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/memorygroup"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// drawnAs is the three memory columns a record carries, as one line, so two records are compared
// whole and a failure names what each holds. A record with no draw at all reads the same whatever
// the other two columns say, which is the state of every tree opened before the draw existed.
func drawnAs(group, mode string, at *time.Time) string {
	when := "no draw"
	if at != nil {
		when = at.UTC().Format(time.RFC3339Nano)
	}
	return fmt.Sprintf("group %q drawn as %q at %s", group, mode, when)
}

// memoryOf is drawnAs for the record key holds now.
func memoryOf(t *testing.T, pool *pgxpool.Pool, key string) string {
	t.Helper()
	stored := issue(t, pool, key)
	return drawnAs(stored.MemoryGroup, stored.MemoryDrawMode, stored.MemoryDrawnAt)
}

// noDraw is what a record with no memory group of its own reads as.
var noDraw = drawnAs("", "", nil)

// refusingCoin fails the test when it is flipped: the coin of a pass that must draw nothing, and of
// a mode that decides without one.
func refusingCoin(t *testing.T) func() (bool, error) {
	return func() (bool, error) {
		t.Error("admission flipped the memory coin on a pass that draws nothing")
		return false, nil
	}
}

// Admission draws a tree's memory group when it opens that tree for the first time, under the mode
// the project's `memory` block names: `off`, and a project whose file names no block, draw none;
// `on` draws memory; and `random` draws what the coin says, both ways. The draw is the root's
// record, written with the mode it was drawn under and the time of the draw. A later pass under a
// changed mode leaves the whole record exactly as it stands: the tree runs under the group it was
// drawn, for as long as it lives.
func TestAdmissionDrawsEachTreesMemoryGroupUnderTheProjectsMode(t *testing.T) {
	const root = "LEGION-1"
	for _, tc := range []struct {
		name string
		mode config.MemoryMode
		coin bool
		// word is the mode the draw records, which is never the empty string: a project that
		// names no block is recorded `off`, the word an operator would have written for it
		// (config.Memory.Word).
		word  config.MemoryMode
		group memorygroup.Group
	}{
		{name: "a project with no memory block", mode: "", word: config.MemoryOff, group: memorygroup.GroupNone},
		{name: "off", mode: config.MemoryOff, word: config.MemoryOff, group: memorygroup.GroupNone},
		{name: "on", mode: config.MemoryOn, word: config.MemoryOn, group: memorygroup.GroupMemory},
		{name: "random, the coin memory", mode: config.MemoryRandom, word: config.MemoryRandom, coin: true, group: memorygroup.GroupMemory},
		{name: "random, the coin none", mode: config.MemoryRandom, word: config.MemoryRandom, coin: false, group: memorygroup.GroupNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := quiet(t, 1)
			admission.UseMemory(config.Memory{Mode: tc.mode})
			admission.coin = func() (bool, error) { return tc.coin, nil }
			seedWaiting(t, pool, root, "A")

			tick(t, pool, admission, "admit")

			want := drawnAs(string(tc.group), string(tc.word), &fixedNow)
			if got := memoryOf(t, pool, root); got != want {
				t.Fatalf("the admitted root carries %s, want %s", got, want)
			}
			// A second pass, under a mode the operator changed and a coin that must not be
			// flipped, writes nothing at all: not the group, and no other column either.
			before := issue(t, pool, root)
			admission.UseMemory(config.Memory{Mode: config.MemoryOn})
			admission.coin = refusingCoin(t)
			tick(t, pool, admission, "second-pass")
			if after := issue(t, pool, root); !reflect.DeepEqual(before, after) {
				t.Fatalf("after a pass under a changed mode the root reads %+v, want %+v unchanged", after, before)
			}
		})
	}
}

// A tree is drawn once, and everything that happens to it afterwards keeps that draw byte for
// byte: a re-admission of its root (a new generation), the clearing of that generation's rows
// (record.Store.ClearTreeGeneration), and the next epoch its lifecycle opens after a cleanup
// confirmed. Each of those passes runs under a mode the operator has since changed and a coin that
// fails the test when it is flipped, so a second draw would be loud.
func TestADrawnTreeKeepsItsMemoryGroupThroughEveryLaterPass(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	admission := quiet(t, 1)
	admission.UseMemory(config.Memory{Mode: config.MemoryOn})
	admission.coin = refusingCoin(t)
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
		Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	const root = "LEGION-1"
	seedWaiting(t, pool, root, "A")

	tick(t, pool, admission, "admit")

	want := drawnAs(string(memorygroup.GroupMemory), string(config.MemoryOn), &fixedNow)
	if got := memoryOf(t, pool, root); got != want {
		t.Fatalf("the admitted root carries %s, want %s", got, want)
	}
	// From here the project is a random one whose coin nothing may flip: every pass below opens a
	// tree that was drawn already, so none of them draws.
	admission.UseMemory(config.Memory{Mode: config.MemoryRandom})

	// The root signs off, lingers, and a person hands it back to Legion: a new generation of the
	// same tree, recorded from the copy read before the draw.
	linger(t, pool, root)
	apply(t, pool, admission, "root-readmitted", intake.DispatchIssue{Key: root, ResolvedRepo: "acme/widgets", Seq: 2,
		Type: "issue.updated", Status: "todo", Title: root, Rank: "A", HandedOver: handed}, engine)
	if readmitted := issue(t, pool, root); readmitted.Generation != 2 {
		t.Fatalf("the re-admitted root is generation %d, want the tree's second", readmitted.Generation)
	}
	if got := memoryOf(t, pool, root); got != want {
		t.Fatalf("after a re-admission the root carries %s, want its first draw's %s", got, want)
	}

	// Clearing the generation's rows is not the tree's memory group's business either.
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().ClearTreeGeneration(ctx, tx, root); err != nil {
			t.Fatalf("clear the tree's generation: %v", err)
		}
	})
	if got := memoryOf(t, pool, root); got != want {
		t.Fatalf("after ClearTreeGeneration the root carries %s, want its first draw's %s", got, want)
	}

	// The tree's cleanup is reserved and confirmed and the root goes back into the waiting line:
	// its promotion opens the tree's second epoch, which creates no row and so draws nothing. The
	// seed writes the record again from a copy that carries none of the three columns — which is
	// what a re-admission does too (record.Store.PutIssue names none of them) — so what the tree
	// reads afterwards is what the draw wrote and nothing else.
	inTx(t, pool, func(tx pgx.Tx) {
		if _, err := treelifecycle.ReserveCleanup(ctx, tx, "legion", root, treelifecycle.AuthorityWorkflow); err != nil {
			t.Fatalf("reserve the tree's cleanup: %v", err)
		}
		if err := treelifecycle.ConfirmCleanup(ctx, tx, "legion", root, 1); err != nil {
			t.Fatalf("confirm the tree's cleanup: %v", err)
		}
	})
	linger(t, pool, root)
	seedWaiting(t, pool, root, "A")

	tick(t, pool, admission, "next-epoch")

	var epoch int64
	if err := pool.QueryRow(ctx, `select epoch from tree_lifecycles where project = 'legion' and tree = $1`, root).Scan(&epoch); err != nil {
		t.Fatalf("read the tree's lifecycle: %v", err)
	}
	if epoch != 2 {
		t.Fatalf("the tree's lifecycle is at epoch %d, want the second epoch its promotion opened", epoch)
	}
	if got := memoryOf(t, pool, root); got != want {
		t.Fatalf("after the tree's next epoch the root carries %s, want its first draw's %s", got, want)
	}
}

// A child handed over as a tree of its own keeps the group it has been running under: the carry
// copies its old tree's three columns verbatim, the time of that tree's draw included, and the
// draw its own promotion would make writes nothing. The hand-over here runs an hour later under a
// project the operator has moved to `off`, so a child that drew for itself would carry `none`,
// drawn as `off`, at the later hour — three differences from what it must carry.
func TestAHandedOverChildCarriesItsOldTreesMemoryGroupVerbatim(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 2)
	admission.UseMemory(config.Memory{Mode: config.MemoryOn})
	admission.coin = refusingCoin(t)
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
		DesignGate: config.DesignGateRootIssues, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	const root, child = "LEGION-1", "LEGION-2"
	seedWaiting(t, pool, root, "A")

	tick(t, pool, admission, "admit")

	want := drawnAs(string(memorygroup.GroupMemory), string(config.MemoryOn), &fixedNow)
	if got := memoryOf(t, pool, root); got != want {
		t.Fatalf("the admitted root carries %s, want %s", got, want)
	}
	// The root signs off and lingers; an hour later the project is off, and the child is handed to
	// Legion as a root of its own.
	later := fixedNow.Add(time.Hour)
	admission.UseMemory(config.Memory{Mode: config.MemoryOff})

	handedOver := handOverChild(t, pool, admission, engine, root, child, "B", later)

	if got := drawnAs(handedOver.MemoryGroup, handedOver.MemoryDrawMode, handedOver.MemoryDrawnAt); got != want {
		t.Fatalf("the handed-over child carries %s, want its old tree's %s verbatim", got, want)
	}
	if got := memoryOf(t, pool, root); got != want {
		t.Fatalf("after the hand-over the old root carries %s, want its own %s", got, want)
	}
}

// A root with no memory before this release — the shape of every tree open when the draw shipped:
// its three columns empty and its tree_lifecycles row already there — is never drawn for. Its
// lifecycle row exists, so no later open creates one, and the project's mode says nothing about a
// tree that is already running. It goes on being admitted, it releases a child into its tree, and
// that child carries no group either, since only a tree's root ever carries one. Nothing here is
// refused and nothing errors: such a tree simply runs without memory, as it has all along.
func TestARootWithNoMemoryBeforeThisReleaseIsNeverDrawnAndItsChildCarriesNone(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	admission := quiet(t, 2)
	// A project that would draw every new tree `memory`: a draw of this tree would be loud.
	admission.UseMemory(config.Memory{Mode: config.MemoryOn})
	admission.coin = refusingCoin(t)
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
		Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	const root, child = "LEGION-1", "LEGION-2"
	// The tree as the release found it: a lifecycle row opened before the draw existed, and a root
	// recorded with none of the three columns set.
	inTx(t, pool, func(tx pgx.Tx) {
		if _, err := treelifecycle.Open(ctx, tx, "legion", root, treelifecycle.AuthorityWorkflow); err != nil {
			t.Fatalf("open the tree's lifecycle as the release found it: %v", err)
		}
	})
	seedWaiting(t, pool, root, "A")

	tick(t, pool, admission, "admit")

	if got := memoryOf(t, pool, root); got != noDraw {
		t.Fatalf("a root whose tree was open before the draw existed carries %s, want %s", got, noDraw)
	}
	// A person makes a queued root a child of this tree: the release admission applies on its next
	// pass (adoptQueuedRoots). The child joins the tree and carries no group of its own.
	parent := root
	putIssue(t, pool, record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Tree: child, Project: testProject, Title: "child", Parent: &parent, Phase: phase.Admitted, Generation: 1,
		Status: "todo", Rank: "B", HandedOver: handed, LastDispatchSeq: 1})

	tick(t, pool, admission, "release-the-child")

	released := issue(t, pool, child)
	if released.Tree != root {
		t.Fatalf("the released child is in tree %s, want %s: it never joined the tree", released.Tree, root)
	}
	if got := drawnAs(released.MemoryGroup, released.MemoryDrawMode, released.MemoryDrawnAt); got != noDraw {
		t.Fatalf("the released child carries %s, want %s", got, noDraw)
	}
	// And no later pass draws for the root either: a re-admission opens the same row again.
	linger(t, pool, root)
	apply(t, pool, admission, "root-readmitted", intake.DispatchIssue{Key: root, ResolvedRepo: "acme/widgets", Seq: 2,
		Type: "issue.updated", Status: "todo", Title: root, Rank: "A", HandedOver: handed}, engine)
	if readmitted := issue(t, pool, root); readmitted.Generation != 2 {
		t.Fatalf("the re-admitted root is generation %d, want the tree's second", readmitted.Generation)
	}
	if got := memoryOf(t, pool, root); got != noDraw {
		t.Fatalf("after a re-admission the undrawn root carries %s, want %s", got, noDraw)
	}
}

// A child handed over from a tree nothing ever drew for is never drawn either: a root that leaves
// another tree takes that tree's answer at the hand-over, and the answer of a tree opened before
// this release is no draw at all. The carry has no draw to copy and settles the child at none,
// which is the answer its own first promotion then writes nothing over (record.Store.CarryMemory,
// which the lifecycle row this test seeds for the old tree is what it reads). The project here is
// at `on` with a coin nothing may flip, so a draw would be loud: the child would carry `memory`,
// drawn as `on`, at the hour of its hand-over, where the tree it left carries nothing at all.
func TestAChildHandedOverFromAnUndrawnTreeIsNeverDrawn(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	admission := quiet(t, 2)
	admission.UseMemory(config.Memory{Mode: config.MemoryOn})
	admission.coin = refusingCoin(t)
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
		DesignGate: config.DesignGateRootIssues, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	const root, child = "LEGION-1", "LEGION-2"
	// The tree as the release found it: a lifecycle row opened before the draw existed, and a root
	// recorded with none of the three columns set.
	inTx(t, pool, func(tx pgx.Tx) {
		if _, err := treelifecycle.Open(ctx, tx, "legion", root, treelifecycle.AuthorityWorkflow); err != nil {
			t.Fatalf("open the tree's lifecycle as the release found it: %v", err)
		}
	})
	seedWaiting(t, pool, root, "A")

	tick(t, pool, admission, "admit")

	if got := memoryOf(t, pool, root); got != noDraw {
		t.Fatalf("a root whose tree was open before the draw existed carries %s, want %s", got, noDraw)
	}
	// The root signs off and lingers; an hour later the child is handed to Legion as a root of its
	// own, and the hand-over records which tree it left (record.Issue.HandedOverFrom).
	later := fixedNow.Add(time.Hour)

	handedOver := handOverChild(t, pool, admission, engine, root, child, "B", later)

	if got := drawnAs(handedOver.MemoryGroup, handedOver.MemoryDrawMode, handedOver.MemoryDrawnAt); got != noDraw {
		t.Fatalf("the handed-over child carries %s, want %s: its old tree had none to give it", got, noDraw)
	}

	// And the pass that opens the child's own tree for the first time draws nothing for it: the
	// row is created there, which is the one moment a draw is made, and this root is one that left
	// another tree.
	tick(t, pool, admission, "promote-the-handed-over-child")

	var epoch int64
	if err := pool.QueryRow(ctx, `select epoch from tree_lifecycles where project = 'legion' and tree = $1`, child).Scan(&epoch); err != nil {
		t.Fatalf("read the lifecycle of the child's own tree: %v", err)
	}
	if got := memoryOf(t, pool, child); got != noDraw {
		t.Fatalf("after its own tree opened at epoch %d the child carries %s, want %s", epoch, got, noDraw)
	}
	if got := memoryOf(t, pool, root); got != noDraw {
		t.Fatalf("after the hand-over the old root carries %s, want %s", got, noDraw)
	}
}

// A root that left a tree nothing ever opened is drawn at its own first open, like any other new
// tree: the answer it takes with it is its old tree's, and a tree with no tree_lifecycles row was
// never opened, so its work never ran under a group at all. Here the old root waits for a slot
// this admission never gives it, a child is recorded under it, and the person takes the root out
// of todo, which brings the child back as a root of its own (record.Issue.HandedOverFrom, whose
// tree the carry finds no lifecycle row for, so it settles nothing). The project is at `random`
// with the coin pinned to memory, so the pass that re-admits that child opens its own tree — the
// one moment a lifecycle row is created — and draws it `memory`, recorded as `random` at the hour
// of that pass.
func TestAChildHandedOverFromATreeNeverOpenedIsDrawnAtItsOwnFirstOpen(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 2)
	admission.UseMemory(config.Memory{Mode: config.MemoryRandom})
	admission.coin = func() (bool, error) { return true, nil }
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
		DesignGate: config.DesignGateRootIssues, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	const root, child = "LEGION-1", "LEGION-2"
	// The old tree as the record holds it: a root recorded and waiting for a slot, and a child
	// recorded under it. No pass admits the root here, so nothing ever opens its tree.
	seedWaiting(t, pool, root, "A")
	if rows := lifecycleRows(t, pool, root); rows != 0 {
		t.Fatalf("the old tree has %d lifecycle rows, want none: nothing ever opened it", rows)
	}
	// The person takes the root out of todo; an hour later the child comes back as a root of its
	// own, and the hand-over records which tree it left.
	later := fixedNow.Add(time.Hour)

	handOverChild(t, pool, admission, engine, root, child, "B", later)

	if rows := lifecycleRows(t, pool, child); rows != 1 {
		t.Fatalf("the child's own tree has %d lifecycle rows, want the one its promotion created", rows)
	}
	// And the draw is made once: a later pass over the same tree creates no row and draws
	// nothing, under a coin that fails the test when it is flipped.
	admission.coin = refusingCoin(t)

	tick(t, pool, admission, "another-pass-over-the-childs-own-tree")

	want := drawnAs(string(memorygroup.GroupMemory), string(config.MemoryRandom), &later)
	if got := memoryOf(t, pool, child); got != want {
		t.Fatalf("after its own tree opened the child carries %s, want %s", got, want)
	}
	if got := memoryOf(t, pool, root); got != noDraw {
		t.Fatalf("the root it left carries %s, want %s: its own tree was never opened", got, noDraw)
	}
}

// The hand-over decides a handed-over child's memory, so the order the two roots are promoted in
// changes nothing. The tree this child leaves is one nothing ever opened at the moment of the
// hand-over, which settles the child at no answer, so its own first open draws it like any other
// new tree. The old root is handed back to Legion too, and one pass with slots for both takes
// them in rank order, in one transaction: this test runs that pass both ways round — the slots
// they take say which went first — and reads the same triple off the child in each. A draw that
// read the old tree's lifecycle rows at the child's own first open would refuse the child its
// draw in the run where the old root goes first, and draw it in the other.
func TestAHandedOverChildsMemoryDoesNotTurnOnWhichRootIsPromotedFirst(t *testing.T) {
	const root, child = "LEGION-1", "LEGION-2"
	later := fixedNow.Add(time.Hour)
	for _, tc := range []struct {
		name, childRank string
		order           []string
	}{
		{name: "the old root promoted first", childRank: "C", order: []string{root, child}},
		{name: "the child promoted first", childRank: "A", order: []string{child, root}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			// No slot stands free while the hand-over and the hand-back run, so neither root is
			// promoted before the one pass below, and the coin fails the test if anything draws.
			handing := quiet(t, 0)
			handing.UseMemory(config.Memory{Mode: config.MemoryRandom})
			handing.coin = refusingCoin(t)
			engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
				DesignGate: config.DesignGateRootIssues, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
			// The old tree as the record holds it: a root waiting for a slot it never gets, so
			// nothing ever opens its tree.
			seedWaiting(t, pool, root, "B")
			if rows := lifecycleRows(t, pool, root); rows != 0 {
				t.Fatalf("the old tree has %d lifecycle rows, want none: nothing ever opened it", rows)
			}

			handedOver := handOverChild(t, pool, handing, engine, root, child, tc.childRank, later)
			if got := drawnAs(handedOver.MemoryGroup, handedOver.MemoryDrawMode, handedOver.MemoryDrawnAt); got != noDraw {
				t.Fatalf("the handed-over child carries %s, want %s: the tree it left was never opened", got, noDraw)
			}
			// The person hands the old root back to Legion, so both roots wait for a slot.
			apply(t, pool, handing, "root-handed-back", intake.DispatchIssue{Key: root, ResolvedRepo: "acme/widgets", Seq: 4,
				Type: "issue.updated", Status: "todo", Title: root, Rank: "B", HandedOver: handed}, engine)

			promoting := quiet(t, 2)
			promoting.UseMemory(config.Memory{Mode: config.MemoryRandom})
			promoting.coin = func() (bool, error) { return true, nil }
			promoting.now = func() time.Time { return later }

			tick(t, pool, promoting, "promote-both-roots")

			if got := slotOrder(t, pool); !reflect.DeepEqual(got, tc.order) {
				t.Fatalf("the pass promoted %v, want %v: this run is meant to be the other order", got, tc.order)
			}
			want := drawnAs(string(memorygroup.GroupMemory), string(config.MemoryRandom), &later)
			if got := memoryOf(t, pool, child); got != want {
				t.Fatalf("after its own first open the child carries %s, want %s", got, want)
			}
			if got := memoryOf(t, pool, root); got != want {
				t.Fatalf("the old root carries %s, want %s: its own first open drew it", got, want)
			}
		})
	}
}

// handOverChild records child in root's tree, signs root off, and hands child to Legion as a tree
// of its own at at, the hour of the hand-over, which is where every test of a hand-over above
// starts. The child is recorded carrying no memory of its own, as every child of a tree is, since
// only a tree's root carries the group; the record the hand-over leaves is a tree of its own
// carrying whatever that hand-over settled, which this returns. It is checked on the tree alone:
// the hand-over writes the tree it left (record.Issue.HandedOverFrom), and the promotion that
// opens the new root's own tree clears that mark in the same pass, so what the mark holds here
// turns on whether a slot was free for the child, which is no part of what a carry settles.
func handOverChild(t *testing.T, pool *pgxpool.Pool, admission *Admission, engine intake.Handler,
	root, child, rank string, at time.Time) record.Issue {
	t.Helper()
	parent := root
	putIssue(t, pool, record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Tree: root, Project: testProject, Title: "child", Parent: &parent, Phase: phase.Done, Generation: 1,
		Status: "done", Rank: rank, LastDispatchSeq: 2})
	if got := memoryOf(t, pool, child); got != noDraw {
		t.Fatalf("a child of the tree carries %s, want %s: only a tree's root carries the group", got, noDraw)
	}
	linger(t, pool, root)
	admission.now = func() time.Time { return at }
	apply(t, pool, admission, "child-handed-over", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 3,
		Type: "issue.updated", Status: "todo", Title: "child", Parent: root, Rank: rank, HandedOver: handed}, engine)
	handedOver := issue(t, pool, child)
	if handedOver.Tree != child {
		t.Fatalf("the handed-over child = %+v, want a tree of its own left by %s", handedOver, root)
	}
	return handedOver
}

// slotOrder is the issues holding admission slots, in the order a pass took them: each takes the
// lowest free index (nextSlotIndex), so index order is the order they were promoted in.
func slotOrder(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select issue from slots order by index`)
	if err != nil {
		t.Fatalf("list the admission slots: %v", err)
	}
	defer rows.Close()
	var issues []string
	for rows.Next() {
		var issue string
		if err := rows.Scan(&issue); err != nil {
			t.Fatalf("read an admission slot: %v", err)
		}
		issues = append(issues, issue)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list the admission slots: %v", err)
	}
	return issues
}

// lifecycleRows is how many tree_lifecycles rows the store holds for this project's tree, the fact
// that says whether that tree was ever opened and the fact the hand-over reads
// (record.Store.CarryMemory, by the project and the tree).
func lifecycleRows(t *testing.T, pool *pgxpool.Pool, tree string) int {
	t.Helper()
	var rows int
	if err := pool.QueryRow(context.Background(), `select count(*) from tree_lifecycles where project = 'legion' and tree = $1`, tree).Scan(&rows); err != nil {
		t.Fatalf("count the lifecycle rows of %s: %v", tree, err)
	}
	return rows
}

// linger signs key off and leaves it lingering with its slot given back, the state a root is in
// when a person hands it to Legion again or its children are handed over as trees of their own.
func linger(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	ctx := context.Background()
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		stored, err := records.Issue(ctx, tx, key)
		if err != nil || stored == nil {
			t.Fatalf("read %s: %v", key, err)
		}
		until := fixedNow.Add(time.Hour)
		stored.Phase, stored.Status, stored.LingerUntil = phase.Done, "done", &until
		if err := records.PutIssue(ctx, tx, *stored); err != nil {
			t.Fatalf("sign %s off: %v", key, err)
		}
		if err := records.ReleaseSlot(ctx, tx, key); err != nil {
			t.Fatalf("release the slot of %s: %v", key, err)
		}
	})
}
