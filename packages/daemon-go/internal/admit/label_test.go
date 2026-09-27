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

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A root in triage is the controller's to triage once it is handed to Legion, so an observation of
// the unrecorded root that carries the label wakes the controller: its creation with the label, or
// the edit that adds it, since the dashboard creates an issue without labels. A root in triage
// without the label is someone else's and wakes nobody. The stream's redelivery of an event adds
// nothing, and the root is not recorded.
func TestAnUnrecordedRootInTriageWakesTheControllerWhenItCarriesTheLabel(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))

	apply(t, pool, admission, "created-unlabeled", intake.DispatchIssue{Key: "LEGION-300", Seq: 1, Type: "issue.created", Status: "triage", Title: "New", Rank: "A", HandedOver: false}, engineStub{})
	assertEffects(t, pool, nil)

	labeled := intake.DispatchIssue{Key: "LEGION-300", Seq: 2, Type: "issue.updated", Status: "triage", Title: "New", Rank: "A", HandedOver: true}
	apply(t, pool, admission, "labeled", labeled, engineStub{})
	apply(t, pool, admission, "labeled", labeled, engineStub{})
	apply(t, pool, admission, "created-labeled", intake.DispatchIssue{Key: "LEGION-305", Seq: 1, Type: "issue.created", Status: "triage", Title: "Handed over at creation", Rank: "B", HandedOver: handed}, engineStub{})
	assertEffects(t, pool, []effect{
		{kind: record.OutboxKindControllerNotice, issue: "LEGION-300", payload: record.ControllerNotice{Kind: "triage"}},
		{kind: record.OutboxKindControllerNotice, issue: "LEGION-305", payload: record.ControllerNotice{Kind: "triage"}},
	})
	assertWaiting(t, pool, nil)
	if got := maybeIssue(t, pool, "LEGION-300"); got != nil {
		t.Fatalf("the root in triage was recorded: %#v", got)
	}
}

// A child created in triage is its parent's architect's, and a recorded root set back to triage is
// a human's move on work the daemon already holds: neither wakes the controller for triage.
func TestNoTriageWakeForAChildOrARecordedRoot(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, pool *pgxpool.Pool, admission *Admission)
	}{
		{"a child created in triage", func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
			apply(t, pool, admission, "child", intake.DispatchIssue{Key: "LEGION-301", Seq: 1, Type: "issue.created", Status: "triage", Title: "Child", Parent: "LEGION-300", Rank: "A", HandedOver: handed}, engineStub{})
		}},
		{"a recorded root set back to triage", func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
			seedWaiting(t, pool, "LEGION-302", "A")
			apply(t, pool, admission, "back", intake.DispatchIssue{Key: "LEGION-302", Seq: 2, Type: "issue.updated", Status: "triage", Title: "waiting", Rank: "A", HandedOver: handed}, engineStub{})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			tc.run(t, pool, newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil))))
			assertEffects(t, pool, nil)
		})
	}
}

// The stream can redeliver a NAK'd issue.created after a later event, under another event id,
// recorded the root: by then the root is the daemon's, so the late creation wakes nobody.
func TestALateCreationOfARecordedRootWakesNobody(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	apply(t, pool, admission, "todo", intake.DispatchIssue{Key: "LEGION-304", Seq: 2, Type: "issue.updated", Status: "todo", Title: "New", Rank: "A", HandedOver: handed}, engineStub{})
	before := effects(t, pool)

	apply(t, pool, admission, "created", intake.DispatchIssue{Key: "LEGION-304", Seq: 1, Type: "issue.created", Status: "triage", Title: "New", Rank: "A", HandedOver: handed}, engineStub{})
	if got := effects(t, pool); !reflect.DeepEqual(got, before) {
		t.Fatalf("outbox effects after the late creation = %#v, want those before it, %#v", got, before)
	}
}

// Legion admits only an issue handed to it. A root or an orphan in todo without the legion label is
// someone else's work in a project Legion shares: it is not recorded, takes no slot, and nothing is
// written or started for it. The label, in any case and among any others, hands it over.
func TestOnlyAnIssueCarryingTheLegionLabelIsAdmitted(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))

	apply(t, pool, admission, "root-unlabeled", intake.DispatchIssue{Key: "LEGION-1", Seq: 1, Type: "issue.updated", Status: "todo", Title: "someone else's", Rank: "A", HandedOver: false}, engineStub{})
	apply(t, pool, admission, "orphan-unlabeled", intake.DispatchIssue{Key: "LEGION-2", Seq: 1, Type: "issue.updated", Status: "todo", Title: "a human's child", Parent: "LEGION-MISSING", Rank: "B"}, engineStub{})
	for _, key := range []string{"LEGION-1", "LEGION-2"} {
		if got := maybeIssue(t, pool, key); got != nil {
			t.Fatalf("unlabeled %s was recorded: %#v", key, got)
		}
	}
	assertSlots(t, pool, nil)
	assertEffects(t, pool, nil)

	apply(t, pool, admission, "root-labeled", intake.DispatchIssue{Key: "LEGION-1", Seq: 2, Type: "issue.updated", Status: "todo", Title: "handed over", Rank: "A", HandedOver: true}, engineStub{})
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-1", Index: 0, AdmittedAt: fixedNow}})
	assertEffects(t, pool, []effect{
		{kind: record.OutboxKindDispatchStatus, issue: "LEGION-1", payload: record.StatusWrite{Status: "in_progress", ObservedStatus: "todo"}},
		{kind: record.OutboxKindSupervise, issue: "LEGION-1", payload: record.SuperviseRequest{Op: "start", Tree: "LEGION-1", Role: claim.RoleArchitect, Generation: 1}},
	})
}

// Among the roots handed to Legion the waiting line is Dispatch rank order, as it was; an unlabeled
// root ranked above them all has no place in it and takes no slot when one frees.
func TestTheWaitingLineRanksOnlyLabeledRootsInDispatchOrder(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	seedSlotted(t, pool, "LEGION-ACTIVE", "A")

	for _, event := range []intake.DispatchIssue{
		{Key: "LEGION-TOP", Seq: 1, Type: "issue.updated", Status: "todo", Title: "ranked first, not handed over", Rank: "0"},
		{Key: "LEGION-C", Seq: 1, Type: "issue.updated", Status: "todo", Title: "rank C", Rank: "C", HandedOver: handed},
		{Key: "LEGION-B", Seq: 1, Type: "issue.updated", Status: "todo", Title: "rank B", Rank: "B", HandedOver: handed},
	} {
		apply(t, pool, admission, "arrive-"+event.Key, event, engineStub{})
	}
	assertWaiting(t, pool, []string{"LEGION-B", "LEGION-C"})

	apply(t, pool, admission, "active-done", intake.DispatchIssue{Key: "LEGION-ACTIVE", Seq: 2, Type: "issue.updated", Status: "done", Title: "active", Rank: "A", HandedOver: handed}, engineStub{})
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-B", Index: 0, AdmittedAt: fixedNow}})
	assertWaiting(t, pool, []string{"LEGION-C"})
}

// A child is its tree's: under a root handed to Legion, a child needs no label of its own. It joins
// the root's tree and takes no slot, as every child does.
func TestAnUnlabeledChildOfAnAdmittedRootRunsInItsTree(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)

	apply(t, pool, admission, "root", intake.DispatchIssue{Key: "LEGION-1", Seq: 1, Type: "issue.updated", Status: "todo", Title: "root", Rank: "A", HandedOver: handed}, engine)
	apply(t, pool, admission, "child", intake.DispatchIssue{Key: "LEGION-2", Seq: 1, Type: "issue.updated", Status: "todo", Title: "child", Parent: "LEGION-1", Rank: "B"}, engine)
	if got := issue(t, pool, "LEGION-2"); got.Tree != "LEGION-1" {
		t.Fatalf("unlabeled child = %#v, want it in LEGION-1's tree", got)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-1", Index: 0, AdmittedAt: fixedNow}})
	assertWaiting(t, pool, nil)
}

// Taking the label off a waiting root drops it from the line, as moving it out of todo does, and
// putting the label back queues it again. Taking it off a root already running changes nothing: the
// label hands work to Legion, and a tree it is running keeps its slot until the tree leaves.
func TestTakingTheLabelOffAWaitingRootDequeuesItButStopsNoRunningTree(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	seedSlotted(t, pool, "LEGION-ACTIVE", "A")
	seedWaiting(t, pool, "LEGION-NEXT", "B")

	apply(t, pool, admission, "next-unlabeled", intake.DispatchIssue{Key: "LEGION-NEXT", Seq: 2, Type: "issue.updated", Status: "todo", Title: "next", Rank: "B"}, engineStub{})
	assertWaiting(t, pool, nil)
	apply(t, pool, admission, "active-unlabeled", intake.DispatchIssue{Key: "LEGION-ACTIVE", Seq: 2, Type: "issue.updated", Status: "in_progress", Title: "active", Rank: "A"}, engineStub{})
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-ACTIVE", Index: 0, AdmittedAt: fixedNow}})

	apply(t, pool, admission, "active-done", intake.DispatchIssue{Key: "LEGION-ACTIVE", Seq: 3, Type: "issue.updated", Status: "done", Title: "active", Rank: "A"}, engineStub{})
	assertSlots(t, pool, nil)
	assertEffects(t, pool, nil)

	apply(t, pool, admission, "next-labeled", intake.DispatchIssue{Key: "LEGION-NEXT", Seq: 3, Type: "issue.updated", Status: "todo", Title: "next", Rank: "B", HandedOver: handed}, engineStub{})
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-NEXT", Index: 0, AdmittedAt: fixedNow}})
}

// The boot read applies the same rule as the stream: an unlabeled todo root is not recorded, a
// labeled one is admitted, a waiting root whose label was taken off while the daemon was down leaves
// the line, and a lingering root set back to todo without the label is not re-admitted.
func TestReconcileAdmitsOnlyLabeledRoots(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	seedWaiting(t, pool, "LEGION-GONE", "0")
	until := fixedNow.Add(time.Hour)
	putIssue(t, pool, record.Issue{Key: "LEGION-LINGER", Project: testProject, Title: "lingering", Tree: "LEGION-LINGER", Phase: phase.Done, Generation: 3, Status: "done", Rank: "1", LingerUntil: &until, HandedOver: true})

	reconcile(t, pool, admission, []dispatch.IssueSummary{
		{Key: "LEGION-GONE", Title: "LEGION-GONE", Status: "todo", Rank: "0"},
		{Key: "LEGION-LINGER", Title: "lingering", Status: "todo", Rank: "1"},
		{Key: "LEGION-OTHER", Title: "someone else's", Status: "todo", Rank: "A", HandedOver: false},
		{Key: "LEGION-MINE", Title: "handed over", Status: "todo", Rank: "B", HandedOver: true},
	})
	if got := maybeIssue(t, pool, "LEGION-OTHER"); got != nil {
		t.Fatalf("unlabeled LEGION-OTHER was recorded: %#v", got)
	}
	if got := issue(t, pool, "LEGION-LINGER"); got.Generation != 3 || got.LingerUntil == nil {
		t.Fatalf("unlabeled lingering root = %#v, want generation 3, still lingering", got)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-MINE", Index: 0, AdmittedAt: fixedNow}})
	assertWaiting(t, pool, nil)
}

// A lingering root set back to todo without the label is not re-admitted: its generation stands and
// its tree keeps lingering, so the linger still closes it. The label, added while the root stays in
// todo, re-admits it.
func TestReadmissionWaitsForTheLabel(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	until := fixedNow.Add(time.Hour)
	lingering := record.Issue{Key: "LEGION-LINGER", Project: testProject, Title: "lingering", Tree: "LEGION-LINGER", Phase: phase.Done, Generation: 3, Status: "done", Rank: "A", LingerUntil: &until, LastDispatchSeq: 1, HandedOver: true}
	putIssue(t, pool, lingering)

	apply(t, pool, admission, "todo-unlabeled", intake.DispatchIssue{Key: lingering.Key, Seq: 2, Type: "issue.updated", Status: "todo", Title: lingering.Title, Rank: lingering.Rank}, engine)
	if got := issue(t, pool, lingering.Key); got.Generation != 3 || got.LingerUntil == nil || got.Phase != phase.Done {
		t.Fatalf("root set to todo without the label = %#v, want generation 3, done, still lingering", got)
	}
	assertSlots(t, pool, nil)
	assertEffects(t, pool, nil)

	apply(t, pool, admission, "labeled", intake.DispatchIssue{Key: lingering.Key, Seq: 3, Type: "issue.updated", Status: "todo", Title: lingering.Title, Rank: lingering.Rank, HandedOver: handed}, engine)
	if got := issue(t, pool, lingering.Key); got.Generation != 4 || got.LingerUntil != nil || got.Phase != phase.Admitted {
		t.Fatalf("root once labeled = %#v, want generation 4, admitted, no linger", got)
	}
	assertSlots(t, pool, []record.Slot{{Issue: lingering.Key, Index: 0, AdmittedAt: fixedNow}})
}

// A child of a lingering tree set back to todo is an orphan, admitted as a root of its own once it
// carries the label. Without the label it stays its old tree's, and the label, added while it stays
// in todo, admits it then. A child waiting in a live tree gains nothing from the label: it runs under
// its tree, as before.
func TestALabelAddedToAReopenedChildOfALingeringTreeAdmitsItAsAnOrphan(t *testing.T) {
	for _, tc := range []struct {
		name      string
		lingering bool
	}{
		{name: "lingering tree", lingering: true},
		{name: "live tree"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
			engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
			const root, child = "LEGION-1", "LEGION-2"
			seedSlotted(t, pool, root, "A")
			parent := root
			inTx(t, pool, func(tx pgx.Tx) {
				records, ctx := record.NewStore(), context.Background()
				rootIssue, err := records.Issue(ctx, tx, root)
				if err != nil {
					t.Fatalf("read root: %v", err)
				}
				childIssue := record.Issue{Key: child, Tree: root, Project: testProject, Title: "child", Parent: &parent, Phase: phase.Admitted, Generation: 1, Status: "todo", Rank: "B", LastDispatchSeq: 2}
				if tc.lingering {
					until := fixedNow.Add(time.Hour)
					rootIssue.Phase, rootIssue.Status, rootIssue.LingerUntil = phase.Done, "done", &until
					childIssue.Phase, childIssue.Status = phase.Done, "done"
					if err := records.ReleaseSlot(ctx, tx, root); err != nil {
						t.Fatalf("release root slot: %v", err)
					}
				}
				for _, put := range []error{records.PutIssue(ctx, tx, *rootIssue), records.PutIssue(ctx, tx, childIssue)} {
					if put != nil {
						t.Fatalf("seed: %v", put)
					}
				}
			})

			if tc.lingering {
				apply(t, pool, admission, "reopened-unlabeled", intake.DispatchIssue{Key: child, Seq: 3, Type: "issue.updated", Status: "todo", Title: "child", Parent: root, Rank: "B"}, engine)
				if got := issue(t, pool, child); got.Tree != root {
					t.Fatalf("child reopened without the label = %#v, want it kept in %s's tree", got, root)
				}
				assertSlots(t, pool, nil)
			}
			apply(t, pool, admission, "labeled", intake.DispatchIssue{Key: child, Seq: 4, Type: "issue.updated", Status: "todo", Title: "child", Parent: root, Rank: "B", HandedOver: handed}, engine)
			got := issue(t, pool, child)
			if !tc.lingering {
				if got.Tree != root {
					t.Fatalf("labeled child of a live tree = %#v, want it kept in %s's tree", got, root)
				}
				assertSlots(t, pool, []record.Slot{{Issue: root, Index: 0, AdmittedAt: fixedNow}})
				return
			}
			if got.Tree != child || got.Phase != phase.Admitted {
				t.Fatalf("labeled orphan = %#v, want its own admitted root", got)
			}
			assertSlots(t, pool, []record.Slot{{Issue: child, Index: 0, AdmittedAt: fixedNow}})
		})
	}
}

// A child of a lingering tree reopened without the label is recorded todo with its old phase left
// at done, and stays there: nothing re-enters it until its root is re-admitted. The root's
// re-admission, once labeled, is what makes the tree live again, and it must re-enter this child
// too, not only the ones a live tree's own todo observation reaches — the child keeps its old
// tree's key, not become an orphan root of its own, since it never carried the label itself.
func TestReadmissionReentersAChildStrandedTodoWithoutTheLabel(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	const root, child = "LEGION-LINGER", "LEGION-2"
	until := fixedNow.Add(time.Hour)
	parent := root
	putIssue(t, pool, record.Issue{Key: root, Project: testProject, Title: "lingering", Tree: root, Phase: phase.Done, Generation: 3, Status: "done", Rank: "A", LingerUntil: &until, LastDispatchSeq: 1, HandedOver: true})
	putIssue(t, pool, record.Issue{Key: child, Project: testProject, Title: "child", Tree: root, Parent: &parent, Phase: phase.Done, Generation: 2, Status: "done", Rank: "B", LastDispatchSeq: 1})

	apply(t, pool, admission, "child-reopened-unlabeled", intake.DispatchIssue{Key: child, Seq: 2, Type: "issue.updated", Status: "todo", Title: "child", Parent: root, Rank: "B"}, engine)
	if got := issue(t, pool, child); got.Tree != root || got.Phase != phase.Done || got.Status != "todo" {
		t.Fatalf("child reopened without the label = %#v, want it left todo, phase done, in %s's tree", got, root)
	}

	apply(t, pool, admission, "root-readmitted", intake.DispatchIssue{Key: root, Seq: 2, Type: "issue.updated", Status: "todo", Title: "lingering", Rank: "A", HandedOver: handed}, engine)

	got := issue(t, pool, child)
	if got.Tree != root {
		t.Fatalf("child after the root's re-admission = %#v, want it kept in %s's tree, not its own", got, root)
	}
	if got.Phase != phase.Admitted || got.Generation != 3 {
		t.Fatalf("child after the root's re-admission = %#v, want phase admitted, generation 3", got)
	}
	assertSlots(t, pool, []record.Slot{{Issue: root, Index: 0, AdmittedAt: fixedNow}})
}

// A child stranded while its root merely waits, not lingers — the root's label taken off drops its
// tree from live, and a reopen of the child without its own label is declined the same way — is
// re-entered once the root's label returns and the root is actually promoted, the same path that
// re-enters one stranded under a re-admitted lingering root: promote's reenterStrandedChildren does
// not care why the tree was not live before, only that the candidate it is promoting now makes it so.
func TestPromotingAWaitingRootReentersAChildStrandedWhileItsLabelWasOff(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	seedSlotted(t, pool, "LEGION-ACTIVE", "A")
	const root, child = "LEGION-WAIT", "LEGION-CHILD"
	parent := root
	putIssue(t, pool, record.Issue{Key: root, Project: testProject, Title: root, Tree: root, Phase: phase.Admitted, Generation: 1, Status: "todo", Rank: "B", HandedOver: true, LastDispatchSeq: 1})
	putIssue(t, pool, record.Issue{Key: child, Project: testProject, Title: "child", Tree: root, Parent: &parent, Phase: phase.Done, Generation: 1, Status: "done", Rank: "C", LastDispatchSeq: 1})

	// The root's label is taken off: it drops from the waiting line, and its tree is no longer live.
	apply(t, pool, admission, "root-unlabeled", intake.DispatchIssue{Key: root, Seq: 2, Type: "issue.updated", Status: "todo", Title: root, Rank: "B"}, engine)
	assertWaiting(t, pool, nil)

	// The child is reopened without its own label while the tree is not live: declined, not an
	// orphan, and left stranded exactly as a lingering tree's would be.
	apply(t, pool, admission, "child-reopened-unlabeled", intake.DispatchIssue{Key: child, Seq: 2, Type: "issue.updated", Status: "todo", Title: "child", Parent: root, Rank: "C"}, engine)
	if got := issue(t, pool, child); got.Tree != root || got.Phase != phase.Done || got.Status != "todo" {
		t.Fatalf("child reopened without the label while its root's tree is not live = %#v, want it left todo, phase done, in %s's tree", got, root)
	}

	// The root's label comes back, but the root still holds no slot: the child is not yet re-entered.
	apply(t, pool, admission, "root-relabeled", intake.DispatchIssue{Key: root, Seq: 3, Type: "issue.updated", Status: "todo", Title: root, Rank: "B", HandedOver: handed}, engine)
	if got := issue(t, pool, child); got.Phase != phase.Done {
		t.Fatalf("child before its root is promoted = %#v, want it still phase done", got)
	}

	// The active slot frees, the waiting root is promoted, and its stranded child is re-entered too.
	apply(t, pool, admission, "active-done", intake.DispatchIssue{Key: "LEGION-ACTIVE", Seq: 2, Type: "issue.updated", Status: "done", Title: "LEGION-ACTIVE", Rank: "A"}, engine)
	assertSlots(t, pool, []record.Slot{{Issue: root, Index: 0, AdmittedAt: fixedNow}})
	got := issue(t, pool, child)
	if got.Tree != root || got.Phase != phase.Admitted || got.Generation != 2 {
		t.Fatalf("child after its root is promoted = %#v, want phase admitted, generation 2, kept in %s's tree", got, root)
	}
}
