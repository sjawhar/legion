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

// A tree's memory group is drawn once and then kept. The first DrawMemory of a root writes the
// group, the mode it was drawn under and the time, and reports that it wrote; a second draw of the
// same root writes nothing and reports it, whatever the project's mode says by then; and a draw of
// a key the daemon has no record of — an operator's spawn — writes nothing and is no error.
// CarryMemory is the hand-over: it copies the three verbatim, the original draw time included, and
// writes nothing when the source has no draw to give or the target already carries one of its own.
// PutIssue names none of the three, so a copy of the issue read before the draw, written back over
// it, keeps what the record holds: that is what makes a re-admission, a new generation and
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
	// draw is the three columns as a reader sees them, so two rows are compared whole and a
	// failure names what each carries.
	draw := func(issue Issue) string {
		at := "no draw"
		if issue.MemoryDrawnAt != nil {
			at = issue.MemoryDrawnAt.Format(time.RFC3339Nano)
		}
		return fmt.Sprintf("group %q drawn as %q at %s", issue.MemoryGroup, issue.MemoryDrawMode, at)
	}
	none := draw(Issue{})

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
		for _, key := range []string{"LEGION-1", "LEGION-2", "LEGION-3", "LEGION-4"} {
			putRoot(t, ctx, records, tx, key)
		}
		stale := read("LEGION-1")
		if got := draw(stale); got != none {
			t.Fatalf("a freshly recorded root carries %s, want %s: nothing has drawn for it", got, none)
		}

		// The draw writes the triple and says so.
		drew, err := records.DrawMemory(ctx, tx, "LEGION-1", "memory", random, drawnAt)
		must(t, err)
		if !drew {
			t.Fatal("the first DrawMemory wrote no row, want it to draw the tree's group")
		}
		want := draw(Issue{MemoryGroup: "memory", MemoryDrawMode: random, MemoryDrawnAt: &drawnAt})
		if got := draw(read("LEGION-1")); got != want {
			t.Fatalf("after the draw the root carries %s, want %s", got, want)
		}

		// A second draw changes nothing and reports that it wrote nothing, so a later pass under
		// another mode leaves the tree on the group it has been running under.
		again, err := records.DrawMemory(ctx, tx, "LEGION-1", "", off, later)
		must(t, err)
		if again {
			t.Fatal("a second DrawMemory of the same root wrote a row, want the draw written once")
		}
		if got := draw(read("LEGION-1")); got != want {
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
		if got := draw(read("LEGION-1")); got != want {
			t.Fatalf("after a stale copy was written back the root carries %s, want the draw kept as %s", got, want)
		}

		// The hand-over copies all three verbatim, the original draw time included.
		carried, err := records.CarryMemory(ctx, tx, "LEGION-2", "LEGION-1")
		must(t, err)
		if !carried {
			t.Fatal("CarryMemory wrote no row, want the hand-over to take the tree's draw with it")
		}
		if got := draw(read("LEGION-2")); got != want {
			t.Fatalf("the handed-over root carries %s, want its old tree's %s verbatim", got, want)
		}

		// A source with no draw has nothing to give.
		empty, err := records.CarryMemory(ctx, tx, "LEGION-3", "LEGION-4")
		must(t, err)
		if empty {
			t.Fatal("CarryMemory from a root with no draw wrote a row, want nothing written")
		}
		if got := draw(read("LEGION-3")); got != none {
			t.Fatalf("after a carry from a root with no draw the target carries %s, want %s", got, none)
		}

		// A target that already carries a draw — a tree off at its own draw is still drawn — keeps
		// its own.
		own, err := records.DrawMemory(ctx, tx, "LEGION-3", "none", off, later)
		must(t, err)
		if !own {
			t.Fatal("DrawMemory under mode off wrote no row, want an off tree's draw recorded too")
		}
		kept := draw(Issue{MemoryGroup: "none", MemoryDrawMode: off, MemoryDrawnAt: &later})
		over, err := records.CarryMemory(ctx, tx, "LEGION-3", "LEGION-1")
		must(t, err)
		if over {
			t.Fatal("CarryMemory onto a root that already carries a draw wrote a row, want nothing written")
		}
		if got := draw(read("LEGION-3")); got != kept {
			t.Fatalf("after a carry onto a drawn root it carries %s, want its own %s", got, kept)
		}
	})
}

// The draw reads what the store holds about the tree a root left, not the fact that it left one.
// A root whose handed_over_from names a tree with a tree_lifecycles row takes that tree's answer
// and is drawn by nothing: the tree ran, and CarryMemory has already copied its draw where it
// had one. A root whose old tree holds no such row left a tree nothing ever opened, so its work
// never ran under a group at all and its own first open draws it like any other new tree. An
// empty handed_over_from names no tree and matches no row.
func TestADrawIsRefusedOnlyForARootWhoseOldTreeWasOpened(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	at := time.Date(2026, 11, 4, 14, 30, 0, 0, time.UTC)
	inTx(t, st, func(tx pgx.Tx) {
		// LEGION-1 left LEGION-9, which was opened; LEGION-2 left LEGION-8, which never was.
		for _, root := range []struct{ key, left string }{{"LEGION-1", "LEGION-9"}, {"LEGION-2", "LEGION-8"}} {
			putRoot(t, ctx, records, tx, root.key)
			stored, err := records.Issue(ctx, tx, root.key)
			must(t, err)
			stored.HandedOverFrom = root.left
			must(t, records.PutIssue(ctx, tx, *stored))
		}
		if _, err := treelifecycle.Open(ctx, tx, "legion", "LEGION-9", treelifecycle.AuthorityWorkflow); err != nil {
			t.Fatalf("open the tree LEGION-1 left: %v", err)
		}

		refused, err := records.DrawMemory(ctx, tx, "LEGION-1", "memory", string(config.MemoryOn), at)
		must(t, err)
		if refused {
			t.Error("DrawMemory drew a root that left a tree the daemon had opened, want its old tree's answer kept")
		}
		drew, err := records.DrawMemory(ctx, tx, "LEGION-2", "memory", string(config.MemoryOn), at)
		must(t, err)
		if !drew {
			t.Error("DrawMemory drew no root that left a tree nothing ever opened, want it drawn like any other new tree")
		}

		read := func(key string) string {
			t.Helper()
			stored, err := records.Issue(ctx, tx, key)
			must(t, err)
			when := "no draw"
			if stored.MemoryDrawnAt != nil {
				when = stored.MemoryDrawnAt.Format(time.RFC3339Nano)
			}
			return fmt.Sprintf("group %q drawn as %q at %s", stored.MemoryGroup, stored.MemoryDrawMode, when)
		}
		if got, want := read("LEGION-1"), `group "" drawn as "" at no draw`; got != want {
			t.Errorf("the root out of an opened tree carries %s, want %s", got, want)
		}
		if got, want := read("LEGION-2"), `group "memory" drawn as "on" at `+at.Format(time.RFC3339Nano); got != want {
			t.Errorf("the root out of a tree never opened carries %s, want %s", got, want)
		}
	})
}
