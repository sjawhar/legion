package admit

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// quiet is an admission of this cap that logs nowhere: these tests read records and outbox rows,
// never the log.
func quiet(t *testing.T, cap int) *Admission {
	t.Helper()
	return newAdmission(t, cap, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// seedMirror writes what Dispatch says of key — its status and, in order, the keys it waits on —
// as the blockers.Mirror handler writes one from an event. Its sequence is 1, so any event a test
// then applies through the mirror (sequence 2 or more) wins the row's fence.
func seedMirror(t *testing.T, pool *pgxpool.Pool, key, status string, waits ...string) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		row := record.DispatchIssueRow{Key: key, Project: testProject, Status: status, Blockers: waits, Seq: 1}
		if err := record.NewStore().PutDispatchIssue(context.Background(), tx, row); err != nil {
			t.Fatalf("seed the mirror of %s: %v", key, err)
		}
	})
}

// applyMirrored applies fact through the daemon's own handler order (daemon/workflow.go): the
// mirror first, so the deciders behind it read the row this very event writes.
func applyMirrored(t *testing.T, pool *pgxpool.Pool, admission *Admission, eventID string, fact intake.DispatchIssue, engine intake.Handler) {
	t.Helper()
	mirror := blockers.NewMirror(record.NewStore(), testProject)
	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", eventID, fact, mirror, engine, admission); err != nil {
		t.Fatalf("ApplyFact %s: %v", eventID, err)
	}
}

// A root that waits on an issue Dispatch has not finished takes no slot: promotion skips it and
// admits the next root the waiting line offers, while the blocked root keeps its place for the
// pass that finds its blocker done.
func TestABlockedRootIsSkippedAndTheNextRootTakesItsSlot(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	seedWaiting(t, pool, "LEGION-FIRST", "A")
	seedWaiting(t, pool, "LEGION-SECOND", "B")
	seedMirror(t, pool, "LEGION-FIRST", "todo", "LEGION-BLOCKER")
	seedMirror(t, pool, "LEGION-BLOCKER", "in_progress")
	seedMirror(t, pool, "LEGION-SECOND", "todo")

	tick(t, pool, admission, "tick")

	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-SECOND", Index: 0, AdmittedAt: fixedNow}})
	if first := issue(t, pool, "LEGION-FIRST"); first.Status != "todo" || first.Phase != phase.Admitted {
		t.Fatalf("the blocked root = %s in %s, want it left waiting in todo at admitted", first.Status, first.Phase)
	}
	assertWaiting(t, pool, []string{"LEGION-FIRST"})
}

// Done is the only Dispatch status that finishes a blocker: a root waiting on one in backlog or
// icebox waits on, and so does one waiting on a blocker the daemon keeps no record of, which may
// be an issue whose creation it has not consumed yet.
func TestABlockerThatIsNotDoneHoldsTheRoot(t *testing.T) {
	for _, status := range []string{"backlog", "icebox", "in_progress", "todo"} {
		t.Run("a blocker in "+status, func(t *testing.T) {
			pool := migratedPool(t)
			seedWaiting(t, pool, "LEGION-ROOT", "A")
			seedMirror(t, pool, "LEGION-ROOT", "todo", "LEGION-BLOCKER")
			seedMirror(t, pool, "LEGION-BLOCKER", status)

			tick(t, pool, quiet(t, 1), "tick")

			assertSlots(t, pool, nil)
		})
	}
	t.Run("a blocker the daemon has no record of", func(t *testing.T) {
		pool := migratedPool(t)
		seedWaiting(t, pool, "LEGION-ROOT", "A")
		seedMirror(t, pool, "LEGION-ROOT", "todo", "LEGION-UNKNOWN")

		tick(t, pool, quiet(t, 1), "tick")

		assertSlots(t, pool, nil)
		assertEffects(t, pool, nil)
	})
}

// A root whose blockers are all done is admitted exactly as a root that waited on nothing: the
// close of the last issue it waited on writes the same records and the same outbox rows an
// unblocked admission writes, compared here row for row against a control admission of the same
// root. The blocker is a recorded child of another tree, since admission promotes on an
// observation of an issue it records.
func TestTheRootIsAdmittedOnItsLastBlockersCloseExactlyAsAnUnblockedOneIs(t *testing.T) {
	seed := func(t *testing.T, pool *pgxpool.Pool, blockerStatus string) {
		t.Helper()
		seedSlotted(t, pool, "LEGION-OTHER", "A")
		seedWaiting(t, pool, "LEGION-ROOT", "B")
		putIssue(t, pool, record.Issue{Key: "LEGION-BLOCKER", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: testProject, Title: "the blocker", Tree: "LEGION-OTHER", Parent: new("LEGION-OTHER"), Phase: phase.Implementing,
			Generation: 1, Status: blockerStatus, Rank: "C", LastDispatchSeq: 1})
		seedMirror(t, pool, "LEGION-ROOT", "todo", "LEGION-BLOCKER")
		seedMirror(t, pool, "LEGION-BLOCKER", blockerStatus)
	}

	control := migratedPool(t)
	seed(t, control, "done")
	tick(t, control, quiet(t, 2), "tick")

	subject := migratedPool(t)
	admission := quiet(t, 2)
	seed(t, subject, "in_progress")
	tick(t, subject, admission, "tick")
	assertSlots(t, subject, []record.Slot{{Issue: "LEGION-OTHER", Index: 0, AdmittedAt: fixedNow}})
	assertEffects(t, subject, nil)

	applyMirrored(t, subject, admission, "blocker-done", intake.DispatchIssue{
		Key: "LEGION-BLOCKER", ResolvedRepo: "acme/widgets", Seq: 5, Type: "issue.updated", Status: "done",
		Title: "the blocker", Parent: "LEGION-OTHER", Rank: "C"}, engineStub{})

	if got, want := effects(t, subject), effects(t, control); !reflect.DeepEqual(got, want) {
		t.Fatalf("the admission of the unblocked root wrote %#v, want the rows a control admission writes, %#v", got, want)
	}
	if got, want := issue(t, subject, "LEGION-ROOT"), issue(t, control, "LEGION-ROOT"); !reflect.DeepEqual(got, want) {
		t.Fatalf("the admitted root = %#v, want the record a control admission leaves, %#v", got, want)
	}
	if got, want := slots(t, subject), slots(t, control); !reflect.DeepEqual(got, want) {
		t.Fatalf("slots = %#v, want a control admission's, %#v", got, want)
	}
}

// A blocker a person adds to a root already waiting stops it where it stands, and the link taken
// off again lets it through: each is one Dispatch event of the root itself, whose row the mirror
// writes before admission reads it in that same transaction.
func TestABlockerAddedToAWaitingRootStopsItAndTakenOffLetsItThrough(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	seedWaiting(t, pool, "LEGION-ROOT", "A")
	seedMirror(t, pool, "LEGION-ROOT", "todo")
	root := func(seq int64, waits ...string) intake.DispatchIssue {
		return intake.DispatchIssue{Key: "LEGION-ROOT", ResolvedRepo: "acme/widgets", Seq: seq, Type: "issue.updated",
			Status: "todo", Title: "LEGION-ROOT", Rank: "A", HandedOver: handed, Blockers: waits}
	}

	applyMirrored(t, pool, admission, "blocked", root(2, "LEGION-UNKNOWN"), engineStub{})
	assertSlots(t, pool, nil)
	assertEffects(t, pool, nil)

	applyMirrored(t, pool, admission, "unblocked", root(3), engineStub{})
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-ROOT", Index: 0, AdmittedAt: fixedNow}})
}

// A blocked root is still the live tree of its children: it keeps its place in the waiting line
// (record.Waiting), so a child of it is recorded under its tree rather than as an orphan admitted
// as a root of its own.
func TestAChildOfABlockedRootIsRecordedUnderItAndNotAsARootOfItsOwn(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	seedWaiting(t, pool, "LEGION-ROOT", "A")
	seedMirror(t, pool, "LEGION-ROOT", "todo", "LEGION-UNKNOWN")

	applyMirrored(t, pool, admission, "child-todo", intake.DispatchIssue{
		Key: "LEGION-CHILD", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "todo",
		Title: "a child", Parent: "LEGION-ROOT", Rank: "B"}, admission.engine)

	child := issue(t, pool, "LEGION-CHILD")
	if child.Tree != "LEGION-ROOT" || child.Parent == nil || *child.Parent != "LEGION-ROOT" || child.Phase != phase.Admitted {
		t.Fatalf("the child = tree %s parent %v in %s, want it recorded under LEGION-ROOT at admitted", child.Tree, child.Parent, child.Phase)
	}
	assertSlots(t, pool, nil)
	assertWaiting(t, pool, []string{"LEGION-ROOT"})
}

// A blocked root takes no slot, so the slot stands free: the controller is woken about it and may
// hand Legion work that can actually run, rather than the slot being held for a root nothing will
// promote until an issue outside Legion closes.
func TestASlotIsFreeWhenEveryRootInTheQueueIsBlocked(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	registerController(t, pool)
	seedSlotted(t, pool, "LEGION-ACTIVE", "A")
	seedWaiting(t, pool, "LEGION-WAITING", "B")
	seedMirror(t, pool, "LEGION-WAITING", "todo", "LEGION-UNKNOWN")

	applyMirrored(t, pool, admission, "phase-done", intake.DispatchIssue{
		Key: "LEGION-ACTIVE", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "in_progress",
		Title: "active", Rank: "A", HandedOver: handed}, engineStub{store: record.NewStore(), done: "LEGION-ACTIVE"})

	assertSlots(t, pool, nil)
	assertControllerNotices(t, pool, []effect{{kind: record.OutboxKindControllerNotice, issue: "LEGION-ACTIVE",
		payload: record.ControllerNotice{Kind: record.SlotFreeNotice}}})
}

// Work already under way is never stopped and never re-checked: a blocker put on the issue of a
// tree that already runs leaves its slot and its record alone.
func TestATreeAlreadyRunningIsUntouchedWhenABlockerIsAddedToItsIssue(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	seedSlotted(t, pool, "LEGION-ACTIVE", "A")

	applyMirrored(t, pool, admission, "blocked", intake.DispatchIssue{
		Key: "LEGION-ACTIVE", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "in_progress",
		Title: "active", Rank: "A", HandedOver: handed, Blockers: []string{"LEGION-UNKNOWN"}}, engineStub{})

	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-ACTIVE", Index: 0, AdmittedAt: fixedNow}})
	if active := issue(t, pool, "LEGION-ACTIVE"); active.Status != "in_progress" {
		t.Fatalf("the running root = %s, want it left in_progress", active.Status)
	}
}

// A candidate that carries a run into a tree of its own — a handed-over child, a re-admitted tree
// — is work already under way, and is admitted whatever its links say (startedRun).
func TestACandidateThatCarriesARunIsAdmittedWhateverItsLinksSay(t *testing.T) {
	pool := migratedPool(t)
	// A carried candidate's own phase role is started with it (startMidPhaseMembers), which mints
	// a claim token, so this one is keyed as Dispatch keys an issue.
	putIssue(t, pool, record.Issue{Key: "LEGION-701", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: testProject, Title: "a carried root", Tree: "LEGION-701", Phase: phase.Implementing, Generation: 2,
		Status: "todo", Rank: "A", HandedOver: true})
	seedMirror(t, pool, "LEGION-701", "todo", "LEGION-UNKNOWN")

	tick(t, pool, quiet(t, 1), "tick")

	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-701", Index: 0, AdmittedAt: fixedNow}})
}

// countingStore counts the mirror reads of the calls under test; every other read and write is
// the real store's.
type countingStore struct {
	record.Store
	mirrorReads int
}

func (s *countingStore) DispatchIssues(ctx context.Context, tx pgx.Tx, project string) ([]record.DispatchIssueRow, error) {
	s.mirrorReads++
	return s.Store.DispatchIssues(ctx, tx, project)
}

// Promotion judges every candidate from one read of the project's mirror, however many candidates
// the waiting line holds: the rows are read before the loop, not inside it.
func TestPromotionReadsTheMirrorOnceWhateverTheNumberOfBlockedRoots(t *testing.T) {
	pool := migratedPool(t)
	store := &countingStore{Store: record.NewStore()}
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
		Clock: func() time.Time { return fixedNow }}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	admission := New(store, engine, 1, testProject, slog.New(slog.NewTextHandler(io.Discard, nil)))
	admission.now = func() time.Time { return fixedNow }
	for _, key := range []string{"LEGION-ONE", "LEGION-TWO", "LEGION-THREE"} {
		seedWaiting(t, pool, key, key)
		seedMirror(t, pool, key, "todo", "LEGION-UNKNOWN")
	}

	if _, err := intake.ApplyFact(context.Background(), pool, "controller", "tick", intake.ControllerTick{}, admission); err != nil {
		t.Fatalf("ApplyFact tick: %v", err)
	}

	if store.mirrorReads != 1 {
		t.Fatalf("the mirror was read %d times for three blocked candidates, want exactly once per promotion", store.mirrorReads)
	}
	assertSlots(t, pool, nil)
}
