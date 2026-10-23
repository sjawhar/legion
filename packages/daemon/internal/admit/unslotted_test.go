package admit

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A root waiting for its slot that a person or an outside session moves to a status only a running
// tree has (in_progress, testing, needs_review, retro) leaves the line, and no slot would ever reach
// it again. It leaves the workflow as a parked root does: its phase is done and its tree
// lingers, the controller is told, and Legion starts nothing for it, so a slot that frees later goes
// elsewhere and never starts a second copy of someone else's work. Set back to todo with the label,
// it is queued again and takes the next slot. A root a hand-over carried into a phase past admitted
// waits in the line the same way, and leaves it the same way.
func TestAWaitingRootMovedToARunningTreesStatusLeavesTheWorkflow(t *testing.T) {
	for _, tc := range []struct {
		name, actor, status string
		phase               phase.Phase
	}{
		{name: "a person's in_progress", status: "in_progress", phase: phase.Admitted},
		{name: "an outside session's in_progress", actor: "ses-outsider", status: "in_progress", phase: phase.Admitted},
		{name: "an outside session's needs_review", actor: "ses-outsider", status: "needs_review", phase: phase.Admitted},
		{name: "a person's in_progress on a root carried into implementing", status: "in_progress", phase: phase.Implementing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
			engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
			seedSlotted(t, pool, "LEGION-1", "A")
			seedWaiting(t, pool, "LEGION-5", "B")
			waiting := issue(t, pool, "LEGION-5")
			waiting.Phase = tc.phase
			putIssue(t, pool, waiting)
			assertWaiting(t, pool, []string{"LEGION-5"})

			apply(t, pool, admission, "taken", intake.DispatchIssue{Key: "LEGION-5", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: tc.status, Title: "LEGION-5", Rank: "B", HandedOver: true, ActorSession: tc.actor}, engine)
			got := issue(t, pool, "LEGION-5")
			if got.Phase != phase.Done || got.LingerUntil == nil || got.Status != tc.status {
				t.Fatalf("LEGION-5 after the %s = %#v, want it out of the workflow: phase done, lingering, status %s", tc.status, got, tc.status)
			}
			assertWaiting(t, pool, nil)
			assertSlots(t, pool, []record.Slot{{Issue: "LEGION-1", Index: 0, AdmittedAt: fixedNow}})
			notices := controllerNotices(t, pool)
			if len(notices) != 1 || notices[0].issue != "LEGION-5" {
				t.Fatalf("controller notices = %#v, want one on LEGION-5", notices)
			}
			told := notices[0].payload.(record.ControllerNotice)
			if told.Kind != record.UnslottedNotice || !strings.Contains(told.Reason, tc.status) {
				t.Fatalf("controller notice = %#v, want kind %s naming the status %s", told, record.UnslottedNotice, tc.status)
			}

			// The running tree finishes: its slot frees, and the released root does not take it.
			apply(t, pool, admission, "first-done", intake.DispatchIssue{Key: "LEGION-1", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "done", Title: "LEGION-1", Rank: "A", HandedOver: true}, engine)
			assertSlots(t, pool, nil)
			if started := architectStarts(t, pool, "LEGION-5"); started != 0 {
				t.Fatalf("architect starts for LEGION-5 = %d, want none: Legion runs nothing someone else took", started)
			}

			// Set back to todo with the label, it is queued again and takes the free slot.
			apply(t, pool, admission, "queued-again", intake.DispatchIssue{Key: "LEGION-5", ResolvedRepo: "acme/widgets", Seq: 3, Type: "issue.updated", Status: "todo", Title: "LEGION-5", Rank: "B", HandedOver: true}, engine)
			if got := issue(t, pool, "LEGION-5"); got.Phase != phase.Admitted || got.Generation != 2 || got.LingerUntil != nil || got.Status != "in_progress" {
				t.Fatalf("LEGION-5 set back to todo = %#v, want it admitted at generation 2 and running", got)
			}
			assertSlots(t, pool, []record.Slot{{Issue: "LEGION-5", Index: 0, AdmittedAt: fixedNow}})
			if started := architectStarts(t, pool, "LEGION-5"); started != 1 {
				t.Fatalf("architect starts for LEGION-5 = %d, want one, for its generation 2", started)
			}
		})
	}
}

// A root admitted into a slot whose slot then went without its tree finishing — the state an
// earlier release left, at any status only a running tree has — is released at the boot read, even
// with a slot free: the controller is told, and nothing starts for it. A running root, one taken out
// of the line by its label (a pause the label ends), and another project's root in the same store
// are left as they are.
func TestTheBootReadReleasesARootThatHeldNoSlotAndWaitedForNone(t *testing.T) {
	for _, status := range []string{"in_progress", "testing", "needs_review", "retro"} {
		t.Run(status, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 3, slog.New(slog.NewTextHandler(io.Discard, nil)))
			seedSlotted(t, pool, "LEGION-RUN", "A")
			seedSlotted(t, pool, "LEGION-81", "B")
			inTx(t, pool, func(tx pgx.Tx) {
				records := record.NewStore()
				stranded, err := records.Issue(context.Background(), tx, "LEGION-81")
				if err != nil || stranded == nil {
					t.Fatalf("read LEGION-81: %#v, %v", stranded, err)
				}
				stranded.Status, stranded.DispatchStatus, stranded.LastDispatchSeq = status, status, 5
				if err := records.PutIssue(context.Background(), tx, *stranded); err != nil {
					t.Fatalf("put LEGION-81: %v", err)
				}
				if err := records.ReleaseSlot(context.Background(), tx, "LEGION-81"); err != nil {
					t.Fatalf("release LEGION-81's slot: %v", err)
				}
			})
			putIssue(t, pool, record.Issue{Key: "LEGION-OFF", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject, Title: "LEGION-OFF", Tree: "LEGION-OFF",
				Phase: phase.Admitted, Generation: 1, Status: "todo", DispatchStatus: "todo", Rank: "C", LastDispatchSeq: 5})
			putIssue(t, pool, record.Issue{Key: "OTHER-1", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "OTHER", Title: "OTHER-1", Tree: "OTHER-1",
				Phase: phase.Admitted, Generation: 1, Status: status, DispatchStatus: status, Rank: "D", HandedOver: true, LastDispatchSeq: 5})

			reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{
				{Key: "LEGION-81", ResolvedRepo: "acme/widgets", Title: "LEGION-81", Status: status, Rank: "B", LastSeq: 5, HandedOver: true},
				{Key: "LEGION-OFF", ResolvedRepo: "acme/widgets", Title: "LEGION-OFF", Status: "todo", Rank: "C", LastSeq: 5},
			}, 1, 1, true)

			if got := issue(t, pool, "LEGION-81"); got.Phase != phase.Done || got.LingerUntil == nil || got.Status != status {
				t.Fatalf("LEGION-81 after the boot read = %#v, want it out of the workflow: phase done, lingering, status %s", got, status)
			}
			if got := issue(t, pool, "LEGION-OFF"); got.Phase != phase.Admitted || got.LingerUntil != nil {
				t.Fatalf("LEGION-OFF after the boot read = %#v, want it left admitted: the label puts it back in the line", got)
			}
			if got := issue(t, pool, "LEGION-RUN"); got.Phase != phase.Admitted || got.Status != "in_progress" {
				t.Fatalf("LEGION-RUN after the boot read = %#v, want it running as it was", got)
			}
			if got := issue(t, pool, "OTHER-1"); got.Phase != phase.Admitted || got.LingerUntil != nil {
				t.Fatalf("OTHER-1 after the boot read = %#v, want another project's root left to its own daemon", got)
			}
			assertSlots(t, pool, []record.Slot{{Issue: "LEGION-RUN", Index: 0, AdmittedAt: fixedNow}})
			assertWaiting(t, pool, nil)
			if started := architectStarts(t, pool, "LEGION-81"); started != 0 {
				t.Fatalf("architect starts for LEGION-81 = %d, want none", started)
			}
			notices := controllerNotices(t, pool)
			if len(notices) != 1 || notices[0].issue != "LEGION-81" || notices[0].payload.(record.ControllerNotice).Kind != record.UnslottedNotice {
				t.Fatalf("controller notices = %#v, want one %s notice on LEGION-81", notices, record.UnslottedNotice)
			}
		})
	}
}

// A boot listing behind the Dispatch consumer may already show the status that takes a waiting
// root out of its line. Until the consumer reaches its captured position, neither promotion nor
// unslotted repair acts on that snapshot. Release applies it first, then repairs the root without
// ever starting it.
func TestReleaseAppliesAHeldRunningStatusBeforeUnslottedRepair(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	seedWaiting(t, pool, "LEGION-HELD", "A")
	waiting := issue(t, pool, "LEGION-HELD")
	waiting.LastDispatchSeq = 1
	putIssue(t, pool, waiting)

	reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{
		{Key: waiting.Key, ResolvedRepo: "acme/widgets", Title: waiting.Title, Status: "in_progress", Rank: waiting.Rank, HandedOver: true, LastSeq: 2},
	}, 5, 2, false)
	if got := issue(t, pool, waiting.Key); got.Status != "todo" || got.Phase != phase.Admitted {
		t.Fatalf("held root before release = %#v, want its stored todo admission left untouched", got)
	}
	assertSlots(t, pool, nil)
	if notices := controllerNotices(t, pool); len(notices) != 0 {
		t.Fatalf("controller notices before release = %#v, want none", notices)
	}

	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "held-position-reached", intake.DispatchConsumerPosition{AckFloorStream: 5}, engine, admission); err != nil {
		t.Fatalf("release held position: %v", err)
	}
	got := issue(t, pool, waiting.Key)
	if got.Status != "in_progress" || got.Phase != phase.Done || got.LingerUntil == nil {
		t.Fatalf("held root after release = %#v, want it released from the workflow", got)
	}
	if started := architectStarts(t, pool, waiting.Key); started != 0 {
		t.Fatalf("architect starts after held release = %d, want none", started)
	}
	notices := controllerNotices(t, pool)
	if len(notices) != 1 || notices[0].issue != waiting.Key || notices[0].payload.(record.ControllerNotice).Kind != record.UnslottedNotice {
		t.Fatalf("controller notices after release = %#v, want one unslotted notice", notices)
	}
}

// A status write the daemon queued before the move — a todo still on its way to Dispatch, say —
// would land over the status someone else set and put the root back in the line beside their work.
// Releasing the root drops it, as every move out of the workflow drops the daemon's queued writes.
func TestReleasingAnUnslottedRootDropsTheStatusWritesQueuedBeforeIt(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	seedSlotted(t, pool, "LEGION-RUN", "A")
	seedWaiting(t, pool, "LEGION-CLEANUP", "B")
	inTx(t, pool, func(tx pgx.Tx) {
		row, err := record.NewOutboxRow("LEGION-CLEANUP", record.StatusWrite{Status: "todo", ObservedStatus: "backlog"}, fixedNow)
		if err != nil {
			t.Fatalf("build the queued status write: %v", err)
		}
		if err := record.NewStore().Enqueue(context.Background(), tx, row); err != nil {
			t.Fatalf("queue the status write: %v", err)
		}
	})

	apply(t, pool, admission, "taken", intake.DispatchIssue{Key: "LEGION-CLEANUP", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "in_progress", Title: "LEGION-CLEANUP", Rank: "B", HandedOver: true}, engine)

	if got := issue(t, pool, "LEGION-CLEANUP"); got.Phase != phase.Done || got.Status != "in_progress" {
		t.Fatalf("LEGION-CLEANUP after the in_progress = %#v, want it out of the workflow", got)
	}
	for _, effect := range effects(t, pool) {
		if _, ok := effect.payload.(record.StatusWrite); ok && effect.issue == "LEGION-CLEANUP" {
			t.Fatalf("status write %#v still queued for the released root, want it dropped", effect.payload)
		}
	}
}

// Admission's unslotted repair is narrow: the running-tree statuses are the only statuses where
// a slotless root has definitely left its queue. An unknown Dispatch status must remain recorded
// for its own handler rather than being silently taken out of Legion's workflow.
func TestAnUnknownStatusDoesNotReleaseAnUnslottedRoot(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	stranded := record.Issue{Key: "LEGION-UNKNOWN", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject, Title: "unknown status", Tree: "LEGION-UNKNOWN",
		Phase: phase.Admitted, Generation: 1, Status: "todo", DispatchStatus: "todo", Rank: "A", HandedOver: true, LastDispatchSeq: 1}
	putIssue(t, pool, stranded)

	apply(t, pool, admission, "unknown-status", intake.DispatchIssue{Key: stranded.Key, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "blocked", Title: stranded.Title, Rank: stranded.Rank, HandedOver: true}, engine)

	got := issue(t, pool, stranded.Key)
	if got.Phase != phase.Admitted || got.LingerUntil != nil || got.Status != "blocked" {
		t.Fatalf("unknown-status root = %#v, want it left admitted without a linger", got)
	}
	if notices := controllerNotices(t, pool); len(notices) != 0 {
		t.Fatalf("controller notices = %#v, want none for an unknown status", notices)
	}
}

func controllerNotices(t *testing.T, pool *pgxpool.Pool) []effect {
	t.Helper()
	var got []effect
	for _, row := range effects(t, pool) {
		if row.kind == record.OutboxKindControllerNotice {
			got = append(got, row)
		}
	}
	return got
}
