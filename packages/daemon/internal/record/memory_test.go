package record

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// treeLifecyclesToken is the project tree_lifecycles is keyed by in these tests: the normalized
// runtime token, where the issue records are under the Dispatch project key LEGION. The hand-over
// is handed the token, so a lifecycle row of another project's tree of the same key is not this
// project's.
const treeLifecyclesToken = "legion"

// drawnAs is the three memory columns as a reader sees them, so two records are compared whole and
// a failure names what each carries.
func drawnAs(issue Issue) string {
	at := "no draw"
	if issue.MemoryDrawnAt != nil {
		at = issue.MemoryDrawnAt.Format(time.RFC3339Nano)
	}
	return fmt.Sprintf("group %q drawn as %q at %s", issue.MemoryGroup, issue.MemoryDrawMode, at)
}

// putHandedOverRoot records key as a tree of its own that was handed over out of left, the shape
// admit's readmit leaves behind: the root is recorded and the tree it left written over it, since
// PutIssue is the one way a test writes a record.
func putHandedOverRoot(t *testing.T, ctx context.Context, records Store, tx pgx.Tx, key, left string) {
	t.Helper()
	putRoot(t, ctx, records, tx, key)
	stored, err := records.Issue(ctx, tx, key)
	must(t, err)
	if stored == nil {
		t.Fatalf("%s is not recorded", key)
	}
	stored.HandedOverFrom = left
	must(t, records.PutIssue(ctx, tx, *stored))
}

// A tree's memory group is drawn once and then kept. The first DrawMemory of a root writes the
// group, the mode it was drawn under and the time, and reports that it wrote; a second draw of the
// same root writes nothing and reports it, whatever the project's mode says by then; and a draw of
// a key the daemon has no record of — an operator's spawn — writes nothing and is no error.
// CarryMemory is the hand-over: out of a tree the daemon had opened it copies the three verbatim,
// the original draw time included, and where that tree had no draw it settles the target at no
// draw, which the target's own first open then writes nothing over. Out of a tree nothing ever
// opened it writes nothing at all, and a target that already carries a draw of its own keeps it.
// PutIssue names none of the columns, so a copy of the issue read before the draw, written back
// over it, keeps what the record holds: that is what makes a re-admission, a new generation and
// ClearTreeGeneration keep the draw.
func TestAMemoryGroupIsDrawnOnceAndCarriedVerbatim(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	// The words a draw records are config.MemoryMode's, which the setter takes as a plain string.
	const (
		on     = string(config.MemoryOn)
		off    = string(config.MemoryOff)
		random = string(config.MemoryRandom)
	)
	drawnAt := time.Date(2026, 11, 4, 7, 31, 48, 123456000, time.UTC)
	later := drawnAt.Add(time.Hour)
	none := drawnAs(Issue{})

	inTx(t, st, func(tx pgx.Tx) {
		read := func(key string) Issue {
			t.Helper()
			issue, err := records.Issue(ctx, tx, key)
			must(t, err)
			if issue == nil {
				t.Fatalf("%s is not recorded", key)
			}
			return *issue
		}
		for _, key := range []string{"LEGION-1", "LEGION-2", "LEGION-3", "LEGION-4", "LEGION-5", "LEGION-6"} {
			putRoot(t, ctx, records, tx, key)
		}
		// The trees the carries below read: LEGION-1 and LEGION-4 were opened, LEGION-6 never was.
		for _, tree := range []string{"LEGION-1", "LEGION-4"} {
			if _, err := treelifecycle.Open(ctx, tx, treeLifecyclesToken, tree, treelifecycle.AuthorityWorkflow); err != nil {
				t.Fatalf("open the tree %s: %v", tree, err)
			}
		}
		stale := read("LEGION-1")
		if got := drawnAs(stale); got != none {
			t.Fatalf("a freshly recorded root carries %s, want %s: nothing has drawn for it", got, none)
		}

		// The draw writes the triple and says so.
		drew, err := records.DrawMemory(ctx, tx, "LEGION-1", "memory", random, drawnAt)
		must(t, err)
		if !drew {
			t.Fatal("the first DrawMemory wrote no row, want it to draw the tree's group")
		}
		want := drawnAs(Issue{MemoryGroup: "memory", MemoryDrawMode: random, MemoryDrawnAt: &drawnAt})
		if got := drawnAs(read("LEGION-1")); got != want {
			t.Fatalf("after the draw the root carries %s, want %s", got, want)
		}

		// A second draw changes nothing and reports that it wrote nothing, so a later pass under
		// another mode leaves the tree on the group it has been running under.
		again, err := records.DrawMemory(ctx, tx, "LEGION-1", "", off, later)
		must(t, err)
		if again {
			t.Fatal("a second DrawMemory of the same root wrote a row, want the draw written once")
		}
		if got := drawnAs(read("LEGION-1")); got != want {
			t.Fatalf("after a second draw the root carries %s, want the first draw's %s", got, want)
		}

		// A tree whose root the daemon never recorded takes no draw, and that is no error.
		spawned, err := records.DrawMemory(ctx, tx, "LEGION-404", "memory", on, drawnAt)
		must(t, err)
		if spawned {
			t.Fatal("DrawMemory on a key with no record wrote a row, want nothing written")
		}
		missing, err := records.Issue(ctx, tx, "LEGION-404")
		must(t, err)
		if missing != nil {
			t.Fatalf("LEGION-404 = %+v after a draw, want no record: a draw records no issue", missing)
		}

		// The copy read before the draw, written back over it, keeps what the record holds.
		must(t, records.PutIssue(ctx, tx, stale))
		if got := drawnAs(read("LEGION-1")); got != want {
			t.Fatalf("after a stale copy was written back the root carries %s, want the draw kept as %s", got, want)
		}

		// The hand-over copies all three verbatim, the original draw time included.
		carried, err := records.CarryMemory(ctx, tx, treeLifecyclesToken, "LEGION-2", "LEGION-1")
		must(t, err)
		if !carried {
			t.Fatal("CarryMemory wrote no row, want the hand-over to take the tree's draw with it")
		}
		if got := drawnAs(read("LEGION-2")); got != want {
			t.Fatalf("the handed-over root carries %s, want its old tree's %s verbatim", got, want)
		}

		// A source whose tree the daemon opened and drew nothing for has no draw to give, and that
		// is its answer: the target is settled at no draw, and its own first open writes nothing.
		settled, err := records.CarryMemory(ctx, tx, treeLifecyclesToken, "LEGION-3", "LEGION-4")
		must(t, err)
		if !settled {
			t.Fatal("CarryMemory from an opened tree with no draw wrote no row, want the hand-over to settle the target at no draw")
		}
		if got := drawnAs(read("LEGION-3")); got != none {
			t.Fatalf("after a carry from an opened tree with no draw the target carries %s, want %s", got, none)
		}
		refused, err := records.DrawMemory(ctx, tx, "LEGION-3", "memory", on, later)
		must(t, err)
		if refused {
			t.Fatal("DrawMemory drew a root whose hand-over settled it at no draw, want its old tree's answer kept")
		}
		if got := drawnAs(read("LEGION-3")); got != none {
			t.Fatalf("after its own first open the settled root carries %s, want %s", got, none)
		}

		// A source whose tree nothing ever opened hands nothing over.
		empty, err := records.CarryMemory(ctx, tx, treeLifecyclesToken, "LEGION-5", "LEGION-6")
		must(t, err)
		if empty {
			t.Fatal("CarryMemory from a tree nothing ever opened wrote a row, want nothing written")
		}
		if got := drawnAs(read("LEGION-5")); got != none {
			t.Fatalf("after a carry from a tree nothing ever opened the target carries %s, want %s", got, none)
		}

		// A target that already carries a draw — a tree off at its own draw is still drawn — keeps
		// its own.
		own, err := records.DrawMemory(ctx, tx, "LEGION-5", "none", off, later)
		must(t, err)
		if !own {
			t.Fatal("DrawMemory under mode off wrote no row, want an off tree's draw recorded too")
		}
		kept := drawnAs(Issue{MemoryGroup: "none", MemoryDrawMode: off, MemoryDrawnAt: &later})
		over, err := records.CarryMemory(ctx, tx, treeLifecyclesToken, "LEGION-5", "LEGION-1")
		must(t, err)
		if over {
			t.Fatal("CarryMemory onto a root that already carries a draw wrote a row, want nothing written")
		}
		if got := drawnAs(read("LEGION-5")); got != kept {
			t.Fatalf("after a carry onto a drawn root it carries %s, want its own %s", got, kept)
		}
	})
}

// The hand-over settles a handed-over root's memory, and what it settles is the answer of the tree
// the root leaves, read in the transaction that records the hand-over. A tree the daemon had
// opened hands its draw over where it had one and no draw at all where it had none, and either way
// the root's own first open writes nothing over that answer. A tree the daemon had opened but
// recorded no root for — an operator's — hands its answer over all the same. A tree nothing ever
// opened hands nothing over, so the root is drawn at its own first open like any other new tree,
// and a lifecycle row of another project's tree of the same key is no tree of this project's.
func TestTheHandOverSettlesTheMemoryOfARootOutOfAnOpenedTree(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	drawnAt := time.Date(2026, 11, 4, 13, 0, 0, 0, time.UTC)
	at := time.Date(2026, 11, 4, 14, 30, 0, 0, time.UTC)
	none := drawnAs(Issue{})
	ownDraw := drawnAs(Issue{MemoryGroup: "memory", MemoryDrawMode: string(config.MemoryOn), MemoryDrawnAt: &at})
	inTx(t, st, func(tx pgx.Tx) {
		// The trees the roots below left: LEGION-11 was opened and drawn, LEGION-12 opened with no
		// draw, LEGION-13 opened with no issue record of its own, LEGION-14 never opened, and
		// LEGION-15 opened only as the tree of another project.
		for _, old := range []struct {
			key, project  string
			recorded, has bool
		}{
			{key: "LEGION-11", project: treeLifecyclesToken, recorded: true, has: true},
			{key: "LEGION-12", project: treeLifecyclesToken, recorded: true},
			{key: "LEGION-13", project: treeLifecyclesToken},
			{key: "LEGION-14"},
			{key: "LEGION-15", project: "widgets"},
		} {
			if old.recorded {
				putRoot(t, ctx, records, tx, old.key)
			}
			if old.project != "" {
				if _, err := treelifecycle.Open(ctx, tx, old.project, old.key, treelifecycle.AuthorityWorkflow); err != nil {
					t.Fatalf("open the tree %s of project %s: %v", old.key, old.project, err)
				}
			}
			if old.has {
				drew, err := records.DrawMemory(ctx, tx, old.key, "memory", string(config.MemoryRandom), drawnAt)
				must(t, err)
				if !drew {
					t.Fatalf("the draw of the old tree %s wrote no row", old.key)
				}
			}
		}

		for _, tc := range []struct {
			name, root, left string
			// settled is whether the hand-over answered this root's memory, which is also exactly
			// what its own first open then writes nothing over.
			settled bool
			want    string
		}{
			{name: "out of a tree opened and drawn", root: "LEGION-1", left: "LEGION-11", settled: true,
				want: drawnAs(Issue{MemoryGroup: "memory", MemoryDrawMode: string(config.MemoryRandom), MemoryDrawnAt: &drawnAt})},
			{name: "out of a tree opened with no draw", root: "LEGION-2", left: "LEGION-12", settled: true, want: none},
			{name: "out of an operator's tree, with no issue record", root: "LEGION-3", left: "LEGION-13", settled: true, want: none},
			{name: "out of a tree nothing ever opened", root: "LEGION-4", left: "LEGION-14", want: ownDraw},
			{name: "out of a tree of that key in another project", root: "LEGION-5", left: "LEGION-15", want: ownDraw},
		} {
			putHandedOverRoot(t, ctx, records, tx, tc.root, tc.left)
			settled, err := records.CarryMemory(ctx, tx, treeLifecyclesToken, tc.root, tc.left)
			must(t, err)
			if settled != tc.settled {
				t.Errorf("%s: the hand-over of %s reported %t, want %t", tc.name, tc.root, settled, tc.settled)
			}
			drew, err := records.DrawMemory(ctx, tx, tc.root, "memory", string(config.MemoryOn), at)
			must(t, err)
			if want := !tc.settled; drew != want {
				t.Errorf("%s: the first open of %s drew %t, want %t: the draw is what the hand-over left open",
					tc.name, tc.root, drew, want)
			}
			stored, err := records.Issue(ctx, tx, tc.root)
			must(t, err)
			if got := drawnAs(*stored); got != tc.want {
				t.Errorf("%s: %s carries %s, want %s", tc.name, tc.root, got, tc.want)
			}
		}
	})
}
