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
	parent := root
	putIssue(t, pool, record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Tree: root, Project: testProject, Title: "child", Parent: &parent, Phase: phase.Done, Generation: 1,
		Status: "done", Rank: "B", LastDispatchSeq: 2})
	if got := memoryOf(t, pool, child); got != noDraw {
		t.Fatalf("a child of the tree carries %s, want %s: only a tree's root carries the group", got, noDraw)
	}
	// The root signs off and lingers; an hour later the project is off, and the child is handed to
	// Legion as a root of its own.
	linger(t, pool, root)
	later := fixedNow.Add(time.Hour)
	admission.now = func() time.Time { return later }
	admission.UseMemory(config.Memory{Mode: config.MemoryOff})

	apply(t, pool, admission, "child-handed-over", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 3,
		Type: "issue.updated", Status: "todo", Title: "child", Parent: root, Rank: "B", HandedOver: handed}, engine)

	handedOver := issue(t, pool, child)
	if handedOver.Tree != child || handedOver.HandedOverFrom != root {
		t.Fatalf("the handed-over child = %+v, want a tree of its own left by %s", handedOver, root)
	}
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
