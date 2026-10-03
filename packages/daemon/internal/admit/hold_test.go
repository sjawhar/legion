package admit

import (
	"bytes"
	"context"
	"errors"
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

// Boot's Dispatch read is a snapshot with no actor on it, and a session's own status write looks
// exactly like a human's in it. Dispatch says how far each issue's event log has run, so an issue
// whose log is ahead of the record is left to the stream, which carries the actor and applies the
// same change with it. An issue the stream has nothing newer for — a summary genuinely ahead of
// the record's own sequence, with the consumer caught up to it — applies immediately, including
// through the engine when its own status changed.
func TestReconcileLeavesAnIssueTheStreamHoldsNewerEventsFor(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	seedSlotted(t, pool, "LEGION-ACTIVE", "A")
	inTx(t, pool, func(tx pgx.Tx) {
		stored, err := record.NewStore().Issue(context.Background(), tx, "LEGION-ACTIVE")
		if err != nil || stored == nil {
			t.Fatalf("read the seeded issue: (%+v, %v)", stored, err)
		}
		stored.LastDispatchSeq = 10
		if err := record.NewStore().PutIssue(context.Background(), tx, *stored); err != nil {
			t.Fatalf("seed the issue's applied sequence: %v", err)
		}
	})

	reconcile(t, pool, admission, []dispatch.IssueSummary{
		{Key: "LEGION-ACTIVE", Title: "active", Status: "done", Rank: "A", LastSeq: 11},
	})

	if got := issue(t, pool, "LEGION-ACTIVE"); got.Status != "in_progress" {
		t.Fatalf("reconciled status = %q, want in_progress: the stream holds the event that changed it", got.Status)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-ACTIVE", Index: 0, AdmittedAt: fixedNow}})

	// The same issue once the Dispatch consumer has actually caught up to the same event: nothing
	// newer is coming, so the summary — genuinely newer than the record's own sequence — applies
	// through the engine (level with what is already recorded would instead leave it alone).
	reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{
		{Key: "LEGION-ACTIVE", Title: "active", Status: "done", Rank: "A", LastSeq: 11},
	}, 11, 11, true)
	if got := issue(t, pool, "LEGION-ACTIVE"); got.Status != "done" {
		t.Fatalf("reconciled status = %q, want done once the consumer has caught up", got.Status)
	}
	assertSlots(t, pool, nil)
}

// Reconcile's boot read defers a record behind Dispatch's own log to the stream, holding it back
// from promote's waiting line until the Dispatch consumer's own ack floor reaches the stream
// position Reconcile measured at boot — not until any one matching event arrives, since the event
// that put the record behind may be a comment or another type intake never turns into a fact at
// all, and not while an unrelated fact runs in between. Only a synthetic DispatchConsumerPosition
// fact whose ack floor has actually reached target releases it.
func TestReconcileHoldsARootUntilTheDispatchConsumerReachesTheStreamPosition(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	putIssue(t, pool, record.Issue{Key: "LEGION-EDGE", Project: testProject, Title: "LEGION-EDGE", Tree: "LEGION-EDGE", Phase: phase.Admitted, Generation: 1, Status: "todo", Rank: "A", HandedOver: true, LastDispatchSeq: 1})

	reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{
		{Key: "LEGION-EDGE", Title: "LEGION-EDGE", Status: "todo", Rank: "A", HandedOver: handed, LastSeq: 2},
	}, 5, 2, false)
	assertSlots(t, pool, nil)

	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "position-short", intake.DispatchConsumerPosition{AckFloorStream: 4}, engineStub{}, admission); err != nil {
		t.Fatalf("ApplyFact position-short: %v", err)
	}
	if _, err := intake.ApplyFact(context.Background(), pool, "timer", "unrelated", intake.LingerExpired{Issue: "LEGION-UNRELATED", Generation: 1}, engineStub{}, admission); err != nil {
		t.Fatalf("ApplyFact unrelated: %v", err)
	}
	assertSlots(t, pool, nil)

	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "position-reached", intake.DispatchConsumerPosition{AckFloorStream: 5}, engineStub{}, admission); err != nil {
		t.Fatalf("ApplyFact position-reached: %v", err)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-EDGE", Index: 0, AdmittedAt: fixedNow}})
}

// Reconcile logs the hold it starts once, naming how many roots it deferred, the stream position
// (target) it is waiting for, and the consumer's own ack floor at that moment — the three numbers
// an operator needs to tell a genuinely stuck hold from one still catching up, without a per-issue
// count.
func TestReconcileLogsTheHoldItStartsWithItsTargetAndAckFloor(t *testing.T) {
	pool := migratedPool(t)
	var logs bytes.Buffer
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(&logs, nil)))
	putIssue(t, pool, record.Issue{Key: "LEGION-EDGE", Project: testProject, Title: "LEGION-EDGE", Tree: "LEGION-EDGE", Phase: phase.Admitted, Generation: 1, Status: "todo", Rank: "A", HandedOver: true, LastDispatchSeq: 1})

	reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{
		{Key: "LEGION-EDGE", Title: "LEGION-EDGE", Status: "todo", Rank: "A", HandedOver: handed, LastSeq: 2},
	}, 5, 2, false)

	if !strings.Contains(logs.String(), "count=1") || !strings.Contains(logs.String(), "target=5") || !strings.Contains(logs.String(), "ack_floor=2") {
		t.Fatalf("hold log = %q, want it to name count 1, target 5 and ack_floor 2", logs.String())
	}
}

// release logs once, naming the ack floor it reached or that the consumer went idle, and how many
// roots its own promote actually admitted — not merely how many it released from the held set,
// since the cap may still leave some of them waiting.
func TestReleaseLogsThePositionItReachedAndHowManyRootsItAdmitted(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	putIssue(t, pool, record.Issue{Key: "LEGION-EDGE", Project: testProject, Title: "LEGION-EDGE", Tree: "LEGION-EDGE", Phase: phase.Admitted, Generation: 1, Status: "todo", Rank: "A", HandedOver: true, LastDispatchSeq: 1})
	reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{
		{Key: "LEGION-EDGE", Title: "LEGION-EDGE", Status: "todo", Rank: "A", HandedOver: handed, LastSeq: 2},
	}, 5, 2, false)

	var logs bytes.Buffer
	admission.log = slog.New(slog.NewTextHandler(&logs, nil))
	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "position-reached", intake.DispatchConsumerPosition{AckFloorStream: 5}, engineStub{}, admission); err != nil {
		t.Fatalf("ApplyFact position-reached: %v", err)
	}

	if !strings.Contains(logs.String(), "ack_floor=5") || !strings.Contains(logs.String(), "idle=false") || !strings.Contains(logs.String(), "admitted=1") {
		t.Fatalf("release log = %q, want it to name ack_floor 5, idle false and admitted 1", logs.String())
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-EDGE", Index: 0, AdmittedAt: fixedNow}})
}

// The deferred set holds every key Reconcile placed in it back across any number of unrelated
// facts — a non-Dispatch one, or another recorded tree's Dispatch event — since ordinary fact
// processing is not what clears the held set; only a DispatchConsumerPosition fact whose ack floor
// reaches Reconcile's own target does. Once it does, the next promote-triggering call admits the
// deferred candidate.
func TestPromotionHoldsADeferredRootAcrossUnrelatedFactsUntilTheStreamPositionCatchesUp(t *testing.T) {
	for _, tc := range []struct {
		name   string
		replay func(t *testing.T, pool *pgxpool.Pool, admission *Admission)
	}{
		{
			name: "a non-Dispatch fact",
			replay: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				if _, err := intake.ApplyFact(context.Background(), pool, "timer", "unrelated-linger", intake.LingerExpired{Issue: "LEGION-UNRELATED", Generation: 1}, engineStub{}, admission); err != nil {
					t.Fatalf("ApplyFact linger: %v", err)
				}
			},
		},
		{
			name: "another recorded tree's issue.updated",
			replay: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				apply(t, pool, admission, "other-rename", intake.DispatchIssue{Key: "LEGION-OTHER", Seq: 2, Type: "issue.updated", Status: "in_progress", Title: "renamed", Rank: "Z"}, engineStub{})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
			seedSlotted(t, pool, "LEGION-OTHER", "Z")
			putIssue(t, pool, record.Issue{Key: "LEGION-EDGE", Project: testProject, Title: "LEGION-EDGE", Tree: "LEGION-EDGE", Phase: phase.Admitted, Generation: 1, Status: "todo", Rank: "A", HandedOver: true, LastDispatchSeq: 1})

			reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{
				{Key: "LEGION-EDGE", Title: "LEGION-EDGE", Status: "todo", Rank: "A", HandedOver: handed, LastSeq: 2},
			}, 3, 1, false)
			assertSlots(t, pool, []record.Slot{{Issue: "LEGION-OTHER", Index: 0, AdmittedAt: fixedNow}})

			tc.replay(t, pool, admission)

			if got := issue(t, pool, "LEGION-EDGE"); got.Status != "todo" {
				t.Fatalf("LEGION-EDGE promoted by an unrelated fact while the stream position was still owed = %#v", got)
			}
			assertSlots(t, pool, []record.Slot{{Issue: "LEGION-OTHER", Index: 0, AdmittedAt: fixedNow}})

			if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "position-reached", intake.DispatchConsumerPosition{AckFloorStream: 3}, engineStub{}, admission); err != nil {
				t.Fatalf("ApplyFact position-reached: %v", err)
			}
			if _, err := intake.ApplyFact(context.Background(), pool, "timer", "another-unrelated", intake.LingerExpired{Issue: "LEGION-UNRELATED-2", Generation: 1}, engineStub{}, admission); err != nil {
				t.Fatalf("ApplyFact another-unrelated: %v", err)
			}
			assertWaiting(t, pool, nil)
			if got := issue(t, pool, "LEGION-EDGE"); got.Status != "in_progress" {
				t.Fatalf("LEGION-EDGE = %#v, want in_progress once the stream position catches up", got)
			}
		})
	}
}

// A restart whose Dispatch consumer is idle releases every root the same boot read would
// otherwise defer immediately, in that same Reconcile call, rather than wait for an ack floor that
// can never reach a target past the consumer's own last matching message: a fresh consumer with
// nothing pending or unacknowledged has already delivered everything it owes.
// B's record is genuinely behind Dispatch's own log on the restart's boot read (the second
// listing shows a newer sequence than what the first boot recorded for it); with the consumer
// idle, that must not hold B back once A's slot frees.
func TestReconcileReleasesImmediatelyWhenTheDispatchConsumerIsIdle(t *testing.T) {
	pool := migratedPool(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	admission := newAdmission(t, 1, log)

	reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{
		{Key: "LEGION-A", Title: "A", Status: "todo", Rank: "A", HandedOver: handed, LastSeq: 5},
		{Key: "LEGION-B", Title: "B", Status: "todo", Rank: "B", HandedOver: handed, LastSeq: 7},
	}, 0, 0, true)
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-A", Index: 0, AdmittedAt: fixedNow}})
	assertWaiting(t, pool, []string{"LEGION-B"})

	// Restart before A leaves: a fresh Admission, the same boot read. A still holds its slot; B's
	// record — from the first boot's own putNewRoot, which records no sequence — reads behind
	// Dispatch's log again, but the consumer is idle.
	admission = newAdmission(t, 1, log)
	reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{
		{Key: "LEGION-A", Title: "A", Status: "in_progress", Rank: "A", HandedOver: handed, LastSeq: 5},
		{Key: "LEGION-B", Title: "B", Status: "todo", Rank: "B", HandedOver: handed, LastSeq: 8},
	}, 0, 0, true)
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-A", Index: 0, AdmittedAt: fixedNow}})

	// A leaves after the restart: B must not still be waiting. Seq 6 is newer than the 5 the idle
	// reconcile already applied to A's record, as a real subsequent live event would be.
	apply(t, pool, admission, "a-done", intake.DispatchIssue{Key: "LEGION-A", Seq: 6, Type: "issue.updated", Status: "done", Title: "A", Rank: "A"}, engineStub{})
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-B", Index: 0, AdmittedAt: fixedNow}})
}

// A root Reconcile finds behind the stream, whose own summary already shows its label removed, is
// held like any other behind record — the listing already knows the truth a live event might
// never arrive to confirm. release must apply that listing's own snapshot before promote decides
// anything, not merely clear the hold and promote on the record's own stale HandedOver: true, or a
// root whose label was removed while the daemon was down would still be admitted once the
// consumer catches up.
func TestReleaseAppliesTheHeldSummaryBeforePromotingSoAnUnlabeledRootIsNeverAdmitted(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	putIssue(t, pool, record.Issue{Key: "LEGION-EDGE", Project: testProject, Title: "LEGION-EDGE", Tree: "LEGION-EDGE", Phase: phase.Admitted, Generation: 1, Status: "todo", Rank: "A", HandedOver: true, LastDispatchSeq: 1})

	// The listing itself already shows the label gone; LastSeq is ahead of the record, so
	// Reconcile holds it on this summary rather than deciding now.
	reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{
		{Key: "LEGION-EDGE", Title: "LEGION-EDGE", Status: "todo", Rank: "A", LastSeq: 2},
	}, 5, 2, false)
	assertSlots(t, pool, nil)

	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "position-reached", intake.DispatchConsumerPosition{AckFloorStream: 5}, engineStub{}, admission); err != nil {
		t.Fatalf("ApplyFact position-reached: %v", err)
	}
	if got := issue(t, pool, "LEGION-EDGE"); got.HandedOver {
		t.Fatalf("released LEGION-EDGE = %#v, want HandedOver false: the held summary showed the label gone", got)
	}
	assertSlots(t, pool, nil)
	assertWaiting(t, pool, nil)
}

// An unrecorded key the listing shows behind the stream is held whatever its listed status, not
// only when it shows todo: a root that moved from todo to backlog while the daemon was down has no
// record yet, and a stale todo event still in flight — a nak's redelivery, the outbox's own
// publish backoff — must not create and admit it before the backlog event that actually explains
// its current state ever arrives, if it arrives at all. Reconcile holds it on the listing's own
// backlog summary; the stale replay, reaching Apply while held, decides nothing — only release,
// from that summary, does.
func TestReconcileHoldsAnUnrecordedKeyBehindTheStreamWhateverItsListedStatus(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))

	reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{
		{Key: "LEGION-EDGE", Title: "LEGION-EDGE", Status: "backlog", Rank: "A", HandedOver: handed, LastSeq: 5},
	}, 10, 2, false)
	if got := maybeIssue(t, pool, "LEGION-EDGE"); got != nil {
		t.Fatalf("unrecorded backlog key was recorded at boot = %#v, want none until release", got)
	}
	assertSlots(t, pool, nil)

	apply(t, pool, admission, "stale-todo-replay", intake.DispatchIssue{Key: "LEGION-EDGE", Seq: 3, Type: "issue.updated", Status: "todo", Title: "LEGION-EDGE", Rank: "A", HandedOver: handed}, engineStub{})
	if got := maybeIssue(t, pool, "LEGION-EDGE"); got != nil {
		t.Fatalf("stale replay recorded LEGION-EDGE while held = %#v, want it to wait for release", got)
	}
	assertSlots(t, pool, nil)

	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "position-reached", intake.DispatchConsumerPosition{AckFloorStream: 10}, engineStub{}, admission); err != nil {
		t.Fatalf("ApplyFact position-reached: %v", err)
	}
	if got := maybeIssue(t, pool, "LEGION-EDGE"); got != nil {
		t.Fatalf("released LEGION-EDGE = %#v, want it left unrecorded: the listing never showed it todo", got)
	}
	assertSlots(t, pool, nil)
}

// While a key is held, Apply dropped every event for it, including ones newer than the summary it
// is held on — so an owner labeling the root after boot could never reach the record, and the
// stale unlabeled listing snapshot applied at release was the last word forever. Only a replay at
// or behind the held summary's own sequence is stale; a newer event is recorded normally — promote
// still holds the candidate back until release, and applySummary leaves a record already past its
// summary alone (see the daemon package's own test of the exact commit/commit-hook race this
// depends on).
func TestALabelAddedWhileAKeyIsHeldReachesTheRecordAndIsAdmittedAfterRelease(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))

	reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{
		{Key: "LEGION-8", Title: "LEGION-8", Status: "todo", Rank: "A", HandedOver: false, LastSeq: 1},
	}, 10, 0, false)
	if !admission.Held() {
		t.Fatal("Held() = false after seeding an unlabeled behind record, want it held")
	}

	// The label-adding event, newer than the held summary, arrives while LEGION-8 is still held.
	apply(t, pool, admission, "label-added", intake.DispatchIssue{Key: "LEGION-8", Seq: 2, Type: "issue.updated", Status: "todo", Title: "LEGION-8", Rank: "A", HandedOver: true}, engineStub{})
	if got := issue(t, pool, "LEGION-8"); !got.HandedOver || got.Status != "todo" {
		t.Fatalf("LEGION-8 while held = %#v, want the newer label-adding event recorded (todo, handed over)", got)
	}
	assertSlots(t, pool, nil)

	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "position-reached", intake.DispatchConsumerPosition{AckFloorStream: 10}, engineStub{}, admission); err != nil {
		t.Fatalf("ApplyFact position-reached: %v", err)
	}
	if got := issue(t, pool, "LEGION-8"); !got.HandedOver || got.Status != "in_progress" {
		t.Fatalf("released LEGION-8 = %#v, want admitted (the newer label-adding event, not the stale unlabeled listing)", got)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-8", Index: 0, AdmittedAt: fixedNow}})
}

// A release whose own promote fails after it decided the consumer had caught up must leave the
// hold intact: clearing pending before this call's transaction actually commits would say the hold
// is resolved when nothing of it is durable, so a later, unrelated call would trust a release that
// never happened. A failing store on the first attempt, a working one on the second, proves the
// hold survives the failure and a later attempt still admits.
func TestReleaseLeavesTheHoldIntactWhenItsOwnTransactionFails(t *testing.T) {
	pool := migratedPool(t)
	real := record.NewStore()
	failing := &failingSlotsStore{Store: real}
	engine := workflow.New(real, workflow.Config{Project: testProject, Clock: func() time.Time { return fixedNow }}, nil)
	admission := New(failing, engine, 1, testProject, slog.New(slog.NewTextHandler(io.Discard, nil)))
	admission.now = func() time.Time { return fixedNow }
	putIssue(t, pool, record.Issue{Key: "LEGION-EDGE", Project: testProject, Title: "LEGION-EDGE", Tree: "LEGION-EDGE", Phase: phase.Admitted, Generation: 1, Status: "todo", Rank: "A", HandedOver: true, LastDispatchSeq: 1})

	reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{
		{Key: "LEGION-EDGE", Title: "LEGION-EDGE", Status: "todo", Rank: "A", HandedOver: handed, LastSeq: 2},
	}, 5, 2, false)
	assertSlots(t, pool, nil)

	failing.fail = true
	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "position-reached", intake.DispatchConsumerPosition{AckFloorStream: 5}, engineStub{}, admission); err == nil {
		t.Fatal("ApplyFact with a failing store succeeded, want the injected error")
	}
	if !admission.Held() {
		t.Fatal("Held() = false after a failed release, want the hold left intact")
	}
	assertSlots(t, pool, nil)

	failing.fail = false
	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "position-retried", intake.DispatchConsumerPosition{AckFloorStream: 5}, engineStub{}, admission); err != nil {
		t.Fatalf("ApplyFact retry: %v", err)
	}
	if admission.Held() {
		t.Fatal("Held() = true after a successful release, want the hold cleared")
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-EDGE", Index: 0, AdmittedAt: fixedNow}})
}

// failingSlotsStore fails Slots on demand, forcing release's own promote call to error out after
// caughtUp has already been decided — the transaction release runs in still rolls back, since
// ApplyFact's caller sees the error and never commits.
type failingSlotsStore struct {
	record.Store
	fail bool
}

func (f *failingSlotsStore) Slots(ctx context.Context, tx pgx.Tx) ([]record.Slot, error) {
	if f.fail {
		return nil, errors.New("injected Slots failure")
	}
	return f.Store.Slots(ctx, tx)
}

// The held check at Apply's unrecorded branch used to run before the triage wake, so a labeled
// triage root created while the daemon was down — held because Reconcile's boot listing now covers
// every status — never woke the controller: its replayed issue.created (seq at or behind the held
// summary) returned before the wake branch ever ran. The held check now gates only the todo
// record-and-admit path below it; the triage wake runs on every observation of an unrecorded root,
// held or not.
func TestATriageRootIsWokenEvenWhileItsKeyIsHeld(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))

	reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{
		{Key: "LEGION-TRIAGE", Title: "triage root", Status: "triage", Rank: "A", HandedOver: true, LastSeq: 5},
	}, 10, 0, false)
	if !admission.Held() {
		t.Fatal("Held() = false after seeding a behind triage record, want it held")
	}

	apply(t, pool, admission, "triage-created", intake.DispatchIssue{Key: "LEGION-TRIAGE", Seq: 1, Type: "issue.created", Status: "triage", Title: "triage root", Rank: "A", HandedOver: true}, engineStub{})
	assertEffects(t, pool, []effect{
		{kind: record.OutboxKindControllerNotice, issue: "LEGION-TRIAGE", payload: record.ControllerNotice{Kind: record.TriageNotice}},
	})
	if got := maybeIssue(t, pool, "LEGION-TRIAGE"); got != nil {
		t.Fatalf("triage root recorded while held = %#v, want none: a triage root is never a todo candidate", got)
	}
}

// A newer event that creates no record for a held unrecorded key was lost — an event moving the
// key out of todo, or taking its label off, records nothing, so release then applied the stale
// boot summary as if that event had never arrived. The held summary now updates to match a newer
// event even when the event itself creates no record.
func TestANewerEventThatCreatesNoRecordRefreshesTheHeldSummary(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))

	reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{
		{Key: "LEGION-G", Title: "g", Status: "todo", Rank: "A", HandedOver: true, LastSeq: 2},
	}, 10, 0, false)
	if !admission.Held() {
		t.Fatal("Held() = false after seeding a behind labeled todo record, want it held")
	}

	// seq 3, newer than the held summary, takes the label off: creates no record (still
	// unrecorded, unlabeled), but must not leave the stale labeled summary in place.
	apply(t, pool, admission, "label-removed", intake.DispatchIssue{Key: "LEGION-G", Seq: 3, Type: "issue.updated", Status: "todo", Title: "g", Rank: "A", HandedOver: false}, engineStub{})
	if got := maybeIssue(t, pool, "LEGION-G"); got != nil {
		t.Fatalf("LEGION-G recorded while held after an unlabeling event = %#v, want none", got)
	}

	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "position-reached", intake.DispatchConsumerPosition{AckFloorStream: 10}, engineStub{}, admission); err != nil {
		t.Fatalf("ApplyFact position-reached: %v", err)
	}
	if got := maybeIssue(t, pool, "LEGION-G"); got != nil {
		t.Fatalf("released LEGION-G = %#v, want it left unrecorded: the newer unlabeling event, not the stale labeled listing, is what release must apply", got)
	}
	assertSlots(t, pool, nil)
}
