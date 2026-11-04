package daemon

import (
	"context"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/projection"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// slotTakenLine is what admission logs as an issue takes one of the cap's slots (admit's logSlot).
const slotTakenLine = "admission: slot taken"

// heldRuntimeTick applies a controller tick through the runtime's own handler pair, which is one
// admission pass over the whole waiting line.
func heldRuntimeTick(t *testing.T, w *workflowRuntime, eventID string) {
	t.Helper()
	if _, err := intake.ApplyFact(context.Background(), w.pool, "controller", eventID, intake.ControllerTick{}, w.handlers...); err != nil {
		t.Fatalf("apply the tick %s: %v", eventID, err)
	}
}

// waitingLine is the line the state route serves, in its order.
func waitingLine(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	var line []string
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		state, err := projection.Project(context.Background(), tx, record.NewStore(), "CAPTURE", nil, nil, catchUpClock())
		if err != nil {
			return err
		}
		line = state.Admission.Waiting
		return nil
	}); err != nil {
		t.Fatalf("read the waiting line: %v", err)
	}
	return line
}

// dispatch://INFRA-1067's rule on the daemon's own runtime, for the boot catch-up's hold.
// TestAHeldRootKeepsItsPlaceOnTheRuntimeUntilItsOwnReleaseAdmitsIt, in hold_release_test.go,
// proves the same rule there for the Dispatch consumer's hold, which a quiet consumer releases;
// this one is the other hold the boot places, which only the catch-up releases.
//
// A hold says the pass does not yet know enough to decide the candidate it stops at, so the pass
// breaks there and nothing behind that candidate takes a slot, whatever tree it belongs to; the
// held candidate keeps its place, and its own release is what admits it. internal/admit proves
// the rule over the admission pass; this proves that the daemon's boot path drives it end to
// end, read through the lines the daemon logs as it takes a slot.
//
// The boot holds every root its catch-up will read before its first admission pass
// (holdForCatchUp), and the catch-up releases each root in the same fact that decides it
// (finishCatchUp, intake.CatchUpApplied). The line is an answered root whose architect has still
// to write its spec, which takes a slot, and behind it a root that never ran. With one slot
// between them, a pass that passed the held root over would hand the root behind it a slot that
// is never taken back.
func TestTheBootCatchUpHoldKeepsItsRootsPlaceOnTheRuntime(t *testing.T) {
	answered := catchUpClock().Add(-time.Hour)
	client := &awaitingDispatch{}
	var logs syncBuffer
	pool, w := catchUpRigLogging(t, client, 1, slog.New(slog.NewJSONHandler(&logs, nil)))
	seedCatchUp(t, pool, func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error {
		held := record.Issue{Key: "CAPTURE-1", Tree: "CAPTURE-1", Project: "CAPTURE", Title: "answered root", Phase: phase.AwaitingPerson,
			Generation: 1, Status: "in_progress", Rank: "A", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			LastDispatchSeq: catchUpHandout,
			Awaiting:        &record.Awaiting{Ask: "ask-CAPTURE-1", AnsweredAt: &answered, AnsweredBy: record.AnsweredByAsk}}
		if err := records.PutIssue(ctx, tx, held); err != nil {
			return err
		}
		return records.PutIssue(ctx, tx, record.Issue{Key: "CAPTURE-2", Tree: "CAPTURE-2", Project: "CAPTURE", Title: "root that never ran",
			Phase: phase.Admitted, Generation: 1, Status: "todo", DispatchStatus: "todo", Rank: "B", HandedOver: true,
			Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"})
	})
	if want := []string{"CAPTURE-1", "CAPTURE-2"}; !reflect.DeepEqual(waitingLine(t, pool), want) {
		t.Fatalf("the line before the boot = %v, want %v: the answered root first, the root that never ran behind it",
			waitingLine(t, pool), want)
	}

	// The boot holds the roots its catch-up will read, in the reconcile's own transaction, and
	// then makes its admission pass: the pass breaks at the held root, so the one behind it takes
	// nothing and both keep their places.
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return w.holdForCatchUp(context.Background(), tx)
	}); err != nil {
		t.Fatalf("hold the roots to catch up: %v", err)
	}
	heldRuntimeTick(t, w, "tick-held")
	if lines := loggedLines(t, logs.String(), slotTakenLine); len(lines) != 0 {
		t.Fatalf("the pass logged %v, want no slot taken: a hold stops it where it stands", lines)
	}
	if holders := slotHolders(t, pool); len(holders) != 0 {
		t.Fatalf("the slots are held by %v, want nobody while the root at the front is held", holders)
	}
	if want := []string{"CAPTURE-1", "CAPTURE-2"}; !reflect.DeepEqual(waitingLine(t, pool), want) {
		t.Fatalf("the line under the hold = %v, want %v: a held root keeps its place", waitingLine(t, pool), want)
	}

	// The catch-up is the hold's own release: it reads the root's window, releases it and applies
	// the fact admission decides it on, all in the boot's own path.
	if err := w.catchUpAwaiting(context.Background()); err != nil {
		t.Fatalf("catch up the held root: %v", err)
	}
	lines := loggedLines(t, logs.String(), slotTakenLine)
	if len(lines) != 1 || lines[0]["issue"] != "CAPTURE-1" {
		t.Fatalf("the slots taken after the release = %v, want the held root's one", lines)
	}
	if holders := slotHolders(t, pool); !reflect.DeepEqual(holders, []string{"CAPTURE-1"}) {
		t.Fatalf("the slots are held by %v, want the released root alone: the cap is one", holders)
	}
	if want := []string{"CAPTURE-2"}; !reflect.DeepEqual(waitingLine(t, pool), want) {
		t.Fatalf("the line after the release = %v, want %v: the root behind the held one waits for the next slot",
			waitingLine(t, pool), want)
	}
}
