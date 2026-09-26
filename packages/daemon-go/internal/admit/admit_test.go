package admit

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/projection"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

var _ intake.Handler = (*Admission)(nil)

func TestApplyFactAdmitsRootAndQueuesWhenFull(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))

	apply(t, pool, admission, "root-todo", intake.DispatchIssue{Key: "LEGION-208", Seq: 1, Type: "issue.updated", Status: "todo", Title: "Admission", Rank: "B"}, engineStub{})
	root := issue(t, pool, "LEGION-208")
	if root.Phase != phase.Admitted || root.Status != "in_progress" || root.Tree != root.Key {
		t.Fatalf("admitted root = %#v, want admitted root with truthful in_progress status and its own tree", root)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-208", Index: 0, AdmittedAt: fixedNow}})
	assertEffects(t, pool, []effect{
		{kind: record.OutboxKindDispatchStatus, issue: "LEGION-208", payload: record.StatusWrite{Status: "in_progress", ObservedStatus: "todo"}},
		{kind: record.OutboxKindSupervise, issue: "LEGION-208", payload: record.SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleArchitect, Generation: 1}},
	})

	apply(t, pool, admission, "queued-todo", intake.DispatchIssue{Key: "LEGION-209", Seq: 1, Type: "issue.updated", Status: "todo", Title: "Queued", Rank: "A"}, engineStub{})
	queued := issue(t, pool, "LEGION-209")
	if queued.Status != "todo" || queued.Tree != queued.Key {
		t.Fatalf("queued root = %#v, want slotless root with its observed todo status", queued)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-208", Index: 0, AdmittedAt: fixedNow}})
	assertWaiting(t, pool, []string{"LEGION-209"})
	assertEffects(t, pool, []effect{
		{kind: record.OutboxKindDispatchStatus, issue: "LEGION-208", payload: record.StatusWrite{Status: "in_progress", ObservedStatus: "todo"}},
		{kind: record.OutboxKindSupervise, issue: "LEGION-208", payload: record.SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleArchitect, Generation: 1}},
	})
}

func TestApplyFactPromotesWaitingRootsInRankOrder(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	seedSlotted(t, pool, "LEGION-ACTIVE-1", "A")
	seedSlotted(t, pool, "LEGION-ACTIVE-2", "B")

	for _, event := range []intake.DispatchIssue{
		{Key: "LEGION-C", Seq: 1, Type: "issue.updated", Status: "todo", Title: "rank C", Rank: "C"},
		{Key: "LEGION-A", Seq: 1, Type: "issue.updated", Status: "todo", Title: "rank A", Rank: "A"},
		{Key: "LEGION-B", Seq: 1, Type: "issue.updated", Status: "todo", Title: "rank B", Rank: "B"},
	} {
		apply(t, pool, admission, "arrive-"+event.Key, event, engineStub{})
	}

	apply(t, pool, admission, "leave-first", intake.DispatchIssue{Key: "LEGION-ACTIVE-1", Seq: 2, Type: "issue.updated", Status: "done", Title: "active", Rank: "A"}, engineStub{})
	assertSlots(t, pool, []record.Slot{
		{Issue: "LEGION-A", Index: 0, AdmittedAt: fixedNow},
		{Issue: "LEGION-ACTIVE-2", Index: 1, AdmittedAt: fixedNow},
	})

	apply(t, pool, admission, "leave-second", intake.DispatchIssue{Key: "LEGION-ACTIVE-2", Seq: 2, Type: "issue.updated", Status: "done", Title: "active", Rank: "B"}, engineStub{})
	assertSlots(t, pool, []record.Slot{
		{Issue: "LEGION-A", Index: 0, AdmittedAt: fixedNow},
		{Issue: "LEGION-B", Index: 1, AdmittedAt: fixedNow},
	})
	assertWaiting(t, pool, []string{"LEGION-C"})
}

func TestApplyFactLeavesEngineRecordedChildWithoutSlotOrEffects(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	seedSlotted(t, pool, "LEGION-ROOT", "A")
	parent := "LEGION-ROOT"
	child := record.Issue{Key: "LEGION-CHILD", Project: "LEGION", Title: "child", Parent: &parent, Tree: "LEGION-ROOT", Phase: phase.Admitted, Generation: 1, Status: "todo", Rank: "B", LastDispatchSeq: 1}

	apply(t, pool, admission, "child-todo", intake.DispatchIssue{Key: child.Key, Seq: 1, Type: "issue.updated", Status: "todo", Title: child.Title, Parent: parent, Rank: child.Rank}, engineStub{store: record.NewStore(), recordChild: &child})
	if got := issue(t, pool, child.Key); !reflect.DeepEqual(got, child) {
		t.Fatalf("child record = %#v, want engine-owned child %#v", got, child)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-ROOT", Index: 0, AdmittedAt: fixedNow}})
	assertWaiting(t, pool, nil)
	assertEffects(t, pool, nil)
}

func TestApplyFactAdmitsOrphanAndLogsOnce(t *testing.T) {
	pool := migratedPool(t)
	var logs bytes.Buffer
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(&logs, nil)))

	apply(t, pool, admission, "orphan-todo", intake.DispatchIssue{Key: "LEGION-ORPHAN", Seq: 1, Type: "issue.updated", Status: "todo", Title: "orphan", Parent: "LEGION-MISSING", Rank: "A"}, engineStub{})
	orphan := issue(t, pool, "LEGION-ORPHAN")
	if orphan.Parent == nil || *orphan.Parent != "LEGION-MISSING" || orphan.Tree != orphan.Key {
		t.Fatalf("orphan record = %#v, want root tree preserving missing parent", orphan)
	}
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "LEGION-ORPHAN") || !strings.Contains(lines[0], "LEGION-MISSING") {
		t.Fatalf("orphan logs = %q, want exactly one line naming orphan and parent", logs.String())
	}
	assertSlots(t, pool, []record.Slot{{Issue: orphan.Key, Index: 0, AdmittedAt: fixedNow}})
}

func TestApplyFactDropsWaitingIssueWhenHumanMovesItOutOfTodo(t *testing.T) {
	for _, status := range []string{"triage", "icebox", "backlog", "done"} {
		t.Run(status, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
			seedWaiting(t, pool, "LEGION-WAITING", "A")

			apply(t, pool, admission, "leave-"+status, intake.DispatchIssue{Key: "LEGION-WAITING", Seq: 2, Type: "issue.updated", Status: status, Title: "waiting", Rank: "A"}, engineStub{})
			if got := issue(t, pool, "LEGION-WAITING"); got.Status != status {
				t.Fatalf("status = %q, want %q", got.Status, status)
			}
			assertSlots(t, pool, nil)
			assertWaiting(t, pool, nil)
		})
	}
}

// A root created in triage is the controller's to triage, so its creation, and nothing after it,
// wakes the controller: the stream's redelivery of that event and a later edit made while it is
// still in triage add nothing, and the root is not recorded.
func TestARootCreatedInTriageWakesTheControllerOnce(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	created := intake.DispatchIssue{Key: "LEGION-300", Seq: 1, Type: "issue.created", Status: "triage", Title: "New", Rank: "A"}

	apply(t, pool, admission, "created", created, engineStub{})
	apply(t, pool, admission, "created", created, engineStub{})
	apply(t, pool, admission, "renamed", intake.DispatchIssue{Key: "LEGION-300", Seq: 2, Type: "issue.updated", Status: "triage", Title: "Renamed", Rank: "A"}, engineStub{})
	assertEffects(t, pool, []effect{
		{kind: record.OutboxKindControllerNotice, issue: "LEGION-300", payload: record.ControllerNotice{Kind: "triage"}},
	})
	assertWaiting(t, pool, nil)
}

// A child created in triage is its parent's architect's, a recorded root set back to triage is a
// human's move on work the daemon already holds, and the boot listing re-reads every issue at every
// restart: none of them wakes the controller for triage.
func TestNoTriageWakeForAChildARecordedRootOrTheBootListing(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, pool *pgxpool.Pool, admission *Admission)
	}{
		{"a child created in triage", func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
			apply(t, pool, admission, "child", intake.DispatchIssue{Key: "LEGION-301", Seq: 1, Type: "issue.created", Status: "triage", Title: "Child", Parent: "LEGION-300", Rank: "A"}, engineStub{})
		}},
		{"a recorded root set back to triage", func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
			seedWaiting(t, pool, "LEGION-302", "A")
			apply(t, pool, admission, "back", intake.DispatchIssue{Key: "LEGION-302", Seq: 2, Type: "issue.updated", Status: "triage", Title: "waiting", Rank: "A"}, engineStub{})
		}},
		{"the boot listing", func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
			reconcile(t, pool, admission, []dispatch.IssueSummary{{Key: "LEGION-303", Status: "triage", Title: "Listed", Rank: "A", LastSeq: 1}})
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
	apply(t, pool, admission, "todo", intake.DispatchIssue{Key: "LEGION-304", Seq: 2, Type: "issue.updated", Status: "todo", Title: "New", Rank: "A"}, engineStub{})
	before := effects(t, pool)

	apply(t, pool, admission, "created", intake.DispatchIssue{Key: "LEGION-304", Seq: 1, Type: "issue.created", Status: "triage", Title: "New", Rank: "A"}, engineStub{})
	if got := effects(t, pool); !reflect.DeepEqual(got, before) {
		t.Fatalf("outbox effects after the late creation = %#v, want those before it, %#v", got, before)
	}
}

func TestApplyFactReleasesSlotWhenEngineCompletesPhaseAndPromotesHead(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	seedSlotted(t, pool, "LEGION-ACTIVE", "A")
	seedWaiting(t, pool, "LEGION-NEXT", "B")

	apply(t, pool, admission, "phase-done", intake.DispatchIssue{Key: "LEGION-ACTIVE", Seq: 2, Type: "issue.updated", Status: "in_progress", Title: "active", Rank: "A"}, engineStub{store: record.NewStore(), done: "LEGION-ACTIVE"})
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-NEXT", Index: 0, AdmittedAt: fixedNow}})
	if got := issue(t, pool, "LEGION-NEXT"); got.Status != "in_progress" {
		t.Fatalf("promoted status = %q, want in_progress", got.Status)
	}
	assertEffects(t, pool, []effect{
		{kind: record.OutboxKindDispatchStatus, issue: "LEGION-NEXT", payload: record.StatusWrite{Status: "in_progress", ObservedStatus: "todo"}},
		{kind: record.OutboxKindSupervise, issue: "LEGION-NEXT", payload: record.SuperviseRequest{Op: "start", Tree: "LEGION-NEXT", Role: claim.RoleArchitect, Generation: 1}},
	})
}

func TestApplyFactReleasesSlotWhenDispatchLeavesActiveSetAndPromotesHead(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	seedSlotted(t, pool, "LEGION-ACTIVE", "A")
	seedWaiting(t, pool, "LEGION-NEXT", "B")

	apply(t, pool, admission, "status-done", intake.DispatchIssue{Key: "LEGION-ACTIVE", Seq: 2, Type: "issue.updated", Status: "done", Title: "active", Rank: "A"}, engineStub{})
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-NEXT", Index: 0, AdmittedAt: fixedNow}})
	if got := issue(t, pool, "LEGION-ACTIVE"); got.Status != "done" {
		t.Fatalf("released issue status = %q, want done", got.Status)
	}
}

// A lingering or closed root set back to todo is re-admitted as a new generation. It runs through
// the real workflow engine, which applies every Dispatch issue fact before admission sees it: an
// engine that records the todo itself leaves admission nothing to re-admit, and the old tree's
// linger deadline then stops the new architect.
func TestApplyFactReadmitsLingeringRootAndIgnoresOwnStatusEcho(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	until := fixedNow.Add(time.Hour)
	lingering := record.Issue{Key: "LEGION-LINGER", Project: "LEGION", Title: "lingering", Tree: "LEGION-LINGER", Phase: phase.Done, Generation: 3, Status: "done", Rank: "A", LingerUntil: &until, LastDispatchSeq: 1}
	putIssue(t, pool, lingering)

	fact := intake.DispatchIssue{Key: lingering.Key, Seq: 2, Type: "issue.updated", Status: "todo", Title: lingering.Title, Rank: lingering.Rank}
	apply(t, pool, admission, "readmit", fact, engine)
	readmitted := issue(t, pool, lingering.Key)
	if readmitted.Generation != 4 || readmitted.LingerUntil != nil || readmitted.Phase != phase.Admitted || readmitted.Status != "in_progress" {
		t.Fatalf("readmitted root = %#v, want generation 4, admitted, and no linger", readmitted)
	}
	assertSlots(t, pool, []record.Slot{{Issue: lingering.Key, Index: 0, AdmittedAt: fixedNow}})
	readmittedEffects := []effect{
		{kind: record.OutboxKindDispatchStatus, issue: lingering.Key, payload: record.StatusWrite{Status: "in_progress", ObservedStatus: "todo"}},
		{kind: record.OutboxKindSupervise, issue: lingering.Key, payload: record.SuperviseRequest{Op: "start", Tree: lingering.Key, Role: claim.RoleArchitect, Generation: 4}},
	}
	assertEffects(t, pool, readmittedEffects)

	apply(t, pool, admission, "own-echo", intake.DispatchIssue{Key: lingering.Key, Seq: 3, Type: "issue.updated", Status: "in_progress", Title: lingering.Title, Rank: lingering.Rank}, engine)
	assertSlots(t, pool, []record.Slot{{Issue: lingering.Key, Index: 0, AdmittedAt: fixedNow}})
	assertEffects(t, pool, readmittedEffects)

	// The previous generation's linger deadline is stale: it stops nothing in the new tree.
	if _, err := intake.ApplyFact(context.Background(), pool, "timer", "old-linger", intake.LingerExpired{Issue: lingering.Key, Generation: 3}, engine, admission); err != nil {
		t.Fatalf("ApplyFact old linger expiry: %v", err)
	}
	assertEffects(t, pool, readmittedEffects)
}

// A tree that closed retired every member's claim, so a re-admitted root's children sit mid-phase
// with nothing running: the architect cannot release a child already in the workflow, and only a
// worker's own handoff moves it. Admission starts each mid-phase child's role with its architect,
// and tells it to carry that phase on: the run whose handoff began the phase is over, and a start
// with no task leaves the resumed agent holding its old transcript with nothing asked of it.
func TestReadmissionStartsTheTreesMidPhaseChildren(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	root := record.Issue{Key: "LEGION-208", Project: "LEGION", Title: "root", Tree: "LEGION-208", Phase: phase.Done, Generation: 1, Status: "done", Rank: "A", LastDispatchSeq: 1}
	putIssue(t, pool, root)
	parentKey := root.Key
	putIssue(t, pool, record.Issue{Key: "LEGION-209", Project: "LEGION", Title: "mid-phase child", Tree: root.Key, Parent: &parentKey,
		Phase: phase.Testing, Generation: 1, Status: "in_progress", Rank: "B", LastDispatchSeq: 1})
	putIssue(t, pool, record.Issue{Key: "LEGION-210", Project: "LEGION", Title: "finished child", Tree: root.Key, Parent: &parentKey,
		Phase: phase.Done, Generation: 1, Status: "done", Rank: "C", LastDispatchSeq: 1})

	apply(t, pool, admission, "readmit", intake.DispatchIssue{Key: root.Key, Seq: 2, Type: "issue.updated", Status: "todo", Title: root.Title, Rank: root.Rank}, engine)

	assertEffects(t, pool, []effect{
		{kind: record.OutboxKindDispatchStatus, issue: root.Key, payload: record.StatusWrite{Status: "in_progress", ObservedStatus: "todo"}},
		{kind: record.OutboxKindSupervise, issue: root.Key, payload: record.SuperviseRequest{Op: "start", Tree: root.Key, Role: claim.RoleArchitect, Generation: 2}},
		{kind: record.OutboxKindSupervise, issue: "LEGION-209", payload: record.SuperviseRequest{Op: "start", Tree: root.Key, Role: claim.RoleTester, Generation: 1, Phase: phase.Testing,
			Task: "Continue mid-phase child. Issue: LEGION-209. Phase: testing. Resume the existing phase work."}},
	})
}

// Every newer Dispatch observation is recorded, not only a status change: re-ranking a waiting
// root, renaming it, or re-parenting it arrives as an issue.updated at the same status, and the
// waiting line has to follow Dispatch rank order at once, not after the next boot's read. It runs
// through the real engine, which sees every fact before admission.
func TestApplyFactRecordsRankTitleAndParentChangesAtTheSameStatus(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	seedSlotted(t, pool, "LEGION-1", "A")
	seedWaiting(t, pool, "LEGION-2", "B")
	seedWaiting(t, pool, "LEGION-3", "C")

	apply(t, pool, admission, "rerank", intake.DispatchIssue{Key: "LEGION-3", Seq: 2, Type: "issue.updated", Status: "todo", Title: "renamed", Parent: "LEGION-9", Rank: "AB"}, engine)
	got := issue(t, pool, "LEGION-3")
	if got.Rank != "AB" || got.Title != "renamed" || got.Parent == nil || *got.Parent != "LEGION-9" || got.LastDispatchSeq != 2 {
		t.Fatalf("observed record = %#v, want rank AB, title renamed, parent LEGION-9, seq 2", got)
	}
	assertWaiting(t, pool, []string{"LEGION-3", "LEGION-2"})
}

// A generation owns its facts. Generation 1 left a merged, approved pull request, an implementer
// two review rounds in, a READY pending, and an approved design gate; the re-admitted generation 2
// starts with none of them. Its architect registers the spec again, the approval opens the gate,
// and planning moves it to implementing, where the implementer's first handoff waits for its own
// pull request instead of starting the tester on generation 1's.
func TestReadmissionStartsTheNewGenerationWithoutTheOldGenerationsFacts(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, DesignGate: config.DesignGateRootIssues, ReviewRoundCap: 3, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	const key, artifact = "LEGION-LINGER", "4f2a9c1e-8b3d-4e7f-9a60-2c5d8e1b7f34"
	until, pending, approved := fixedNow.Add(time.Hour), 1, 1
	putIssue(t, pool, record.Issue{Key: key, Project: "LEGION", Title: "lingering", Tree: key, Phase: phase.Done, Generation: 1, Status: "done", Rank: "A", LingerUntil: &until, LastDispatchSeq: 1, ReadyPendingVersion: &pending})
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		ctx := context.Background()
		if err := records.PutGate(ctx, tx, record.DesignGate{Issue: key, ArtifactID: artifact, LatestVersion: 1, ApprovedVersion: &approved}); err != nil {
			t.Fatalf("seed gate: %v", err)
		}
		if err := records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestMerged, Issue: key, Repo: "sjawhar/legion", Number: 86, Branch: "legion/" + key, HeadSHA: "merged", Verdict: "green", ReviewDecision: "approved", Failing: []string{}, FailingStatuses: []string{}}); err != nil {
			t.Fatalf("seed pull request: %v", err)
		}
		if err := records.PutPhase(ctx, tx, record.PhaseRow{Issue: key, Role: claim.RoleImplementer, Claim: "implementer", HandoffCommit: "gen1-handoff", LastHandoff: "gen1-handoff", Rounds: 2}); err != nil {
			t.Fatalf("seed implementer: %v", err)
		}
	})

	apply(t, pool, admission, "readmit", intake.DispatchIssue{Key: key, Seq: 2, Type: "issue.updated", Status: "todo", Title: "lingering", Rank: "A"}, engine)
	readmitted := issue(t, pool, key)
	if readmitted.Generation != 2 || readmitted.Phase != phase.Admitted || readmitted.ReadyPendingVersion != nil {
		t.Fatalf("readmitted = %#v, want generation 2, admitted, no READY pending", readmitted)
	}
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		if pr, err := records.PullRequest(context.Background(), tx, key); err != nil || pr != nil {
			t.Fatalf("generation 2 pull request = %#v, %v; want none", pr, err)
		}
		if gate, err := records.Gate(context.Background(), tx, key); err != nil || gate != nil {
			t.Fatalf("generation 2 gate = %#v, %v; want none until its architect registers", gate, err)
		}
		rows, err := records.Phases(context.Background(), tx, key)
		if err != nil {
			t.Fatalf("read phases: %v", err)
		}
		for _, row := range rows {
			if row.HandoffCommit != "" || row.Rounds != 0 || row.Verdict != "" {
				t.Fatalf("generation 2 %s row = %#v, want no handoff, rounds, or verdict", row.Role, row)
			}
		}
	})

	for _, step := range []struct {
		id   string
		fact intake.Fact
	}{
		{id: "gen2-register", fact: intake.GateRegistered{Issue: key, ArtifactID: artifact, Version: 1}},
		{id: "gen2-approved", fact: intake.DispatchArtifact{Key: key, ArtifactID: artifact, Kind: intake.DispatchArtifactApproved, Version: 1}},
		{id: "gen2-plan", fact: intake.HandoffComplete{Generation: 2, Issue: key, Role: claim.RolePlanner, Claim: "planner", Summary: "planned", Commit: "gen2-plan"}},
		{id: "gen2-implement", fact: intake.HandoffComplete{Generation: 2, Issue: key, Role: claim.RoleImplementer, Claim: "implementer", Summary: "implemented", Commit: "gen2-handoff"}},
	} {
		if result, err := intake.ApplyFact(context.Background(), pool, "api", step.id, step.fact, engine, admission); err != nil || result.Refusal != nil || result.Duplicate {
			t.Fatalf("%s = %#v, %v", step.id, result, err)
		}
	}
	if got := issue(t, pool, key); got.Phase != phase.Implementing {
		t.Fatalf("generation 2 phase = %s, want implementing until its own pull request opens", got.Phase)
	}
}

// A signed-off child reopened to todo belongs to its tree while the tree is live: a child under a
// live tree takes no slot and runs under that tree's architect (decision 11; the shipped
// admitOnTodo). The workflow re-enters it, starting the child's next generation under the open
// gate; its tree and slots stay the tree's. Under a lingering tree the child is an orphan,
// and admission admits it as a root of its own, as the shipped daemon does.
func TestAReopenedChildReentersALiveTreeAndIsAnOrphanRootOfALingeringOne(t *testing.T) {
	for _, tc := range []struct {
		name      string
		lingering bool
	}{
		{name: "live tree"},
		{name: "lingering tree", lingering: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
			engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, DesignGate: config.DesignGateRootIssues, ReviewRoundCap: 3, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
			const root, child, artifact = "LEGION-1", "LEGION-2", "7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26"
			approved := 1
			seedSlotted(t, pool, root, "A")
			parent := root
			inTx(t, pool, func(tx pgx.Tx) {
				records, ctx := record.NewStore(), context.Background()
				rootIssue, err := records.Issue(ctx, tx, root)
				if err != nil {
					t.Fatalf("read root: %v", err)
				}
				rootIssue.Phase = phase.Implementing
				if tc.lingering {
					until := fixedNow.Add(time.Hour)
					rootIssue.Phase, rootIssue.Status, rootIssue.LingerUntil = phase.Done, "done", &until
					if err := records.ReleaseSlot(ctx, tx, root); err != nil {
						t.Fatalf("release root slot: %v", err)
					}
				}
				for _, put := range []error{
					records.PutIssue(ctx, tx, *rootIssue),
					records.PutGate(ctx, tx, record.DesignGate{Issue: root, ArtifactID: artifact, LatestVersion: 1, ApprovedVersion: &approved}),
					records.PutIssue(ctx, tx, record.Issue{Key: child, Tree: root, Project: testProject, Title: "child", Parent: &parent, Phase: phase.Done, Generation: 1, Status: "done", Rank: "B", LastDispatchSeq: 2}),
					records.PutPhase(ctx, tx, record.PhaseRow{Issue: child, Role: claim.RoleImplementer, Claim: "child-implementer", HandoffCommit: "child-check", LastHandoff: "child-check", Rounds: 1}),
				} {
					if put != nil {
						t.Fatalf("seed: %v", put)
					}
				}
			})

			apply(t, pool, admission, "child-reopened", intake.DispatchIssue{Key: child, Seq: 3, Type: "issue.updated", Status: "todo", Title: "child", Parent: root, Rank: "B"}, engine)
			reopened := issue(t, pool, child)
			var architectStarts, told int
			for _, got := range effects(t, pool) {
				if request, ok := got.payload.(record.SuperviseRequest); ok && got.issue == child && request.Op == "start" && request.Role == claim.RoleArchitect {
					architectStarts++
				}
				if notice, ok := got.payload.(record.Notice); ok && got.issue == child && notice.Kind == "child-status" {
					told++
				}
			}
			if !tc.lingering {
				if reopened.Tree != root || reopened.Generation != 2 || reopened.Phase != phase.Planning || architectStarts != 0 || told != 1 {
					t.Fatalf("reopened child = %#v with %d architect starts and %d child-status notices, want planning again in tree %s as the child's generation 2, no architect of its own, and the tree's architect told once", reopened, architectStarts, told, root)
				}
				assertSlots(t, pool, []record.Slot{{Issue: root, Index: 0, AdmittedAt: fixedNow}})
				return
			}
			if reopened.Tree != child || reopened.Phase != phase.Admitted || architectStarts != 1 {
				t.Fatalf("reopened orphan = %#v with %d architect starts, want its own admitted root with its architect started", reopened, architectStarts)
			}
			assertSlots(t, pool, []record.Slot{{Issue: child, Index: 0, AdmittedAt: fixedNow}})
		})
	}
}

// The boot read re-admits only a root: a signed-off child the human reopened while the daemon was
// down keeps its tree and takes no slot.
func TestReconcileNeverReadmitsAChildAsARoot(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	seedSlotted(t, pool, "LEGION-1", "A")
	parent := "LEGION-1"
	putIssue(t, pool, record.Issue{Key: "LEGION-2", Tree: "LEGION-1", Project: testProject, Title: "child", Parent: &parent, Phase: phase.Done, Generation: 1, Status: "done", Rank: "B"})

	reconcile(t, pool, admission, []dispatch.IssueSummary{
		{Key: "LEGION-1", Title: "LEGION-1", Status: "in_progress", Rank: "A"},
		{Key: "LEGION-2", Title: "child", Status: "todo", Parent: &parent, Rank: "B"},
	})
	if got := issue(t, pool, "LEGION-2"); got.Tree != "LEGION-1" || got.Generation != 1 {
		t.Fatalf("reconciled child = %#v, want it kept in LEGION-1's tree at generation 1", got)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-1", Index: 0, AdmittedAt: fixedNow}})
}

// A lifecycle status change written by an agent holding a claim in the tree is never a human move:
// here the implementer closes its own issue during the production check instead of completing the
// phase. The workflow does not react (no linger, the slot kept, the phase unchanged) and the daemon
// re-asserts its own status through the outbox; the same write by a human closes the tree.
func TestAnAgentsLifecycleStatusWriteIsNotAHumanMove(t *testing.T) {
	for _, tc := range []struct {
		name  string
		actor string
		agent bool
	}{
		{name: "the tree's implementer", actor: "ses-impl", agent: true},
		{name: "a human", actor: ""},
		{name: "another tree's session", actor: "ses-other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
			engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
			seedSlotted(t, pool, "LEGION-1", "A")
			seedSlotted(t, pool, "LEGION-9", "Z")
			inTx(t, pool, func(tx pgx.Tx) {
				records, ctx := record.NewStore(), context.Background()
				root, err := records.Issue(ctx, tx, "LEGION-1")
				if err != nil {
					t.Fatalf("read root: %v", err)
				}
				root.Phase, root.Status, root.LastDispatchSeq = phase.ProductionCheck, "retro", 1
				if err := records.PutIssue(ctx, tx, *root); err != nil {
					t.Fatalf("put root: %v", err)
				}
				for _, c := range []struct{ token, tree, issue, session string }{
					{"legion-legion-legion-1-implementer", "LEGION-1", "LEGION-1", "ses-impl"},
					{"legion-legion-legion-9-implementer", "LEGION-9", "LEGION-9", "ses-other"},
				} {
					if _, err := tx.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
						launch_failures, prompt_failures, prompt_retires, uncertain_streak)
						values ($1, 'legion', $2, $3, 'implementer', 1, $4, '/tmp/impl.jsonl', 'working', 0, 0, 0, 0)`, c.token, c.tree, c.issue, c.session); err != nil {
						t.Fatalf("put claim: %v", err)
					}
				}
			})

			apply(t, pool, admission, "closed-by-"+tc.name, intake.DispatchIssue{Key: "LEGION-1", Seq: 2, Type: "issue.updated", Status: "done", Title: "LEGION-1", Rank: "A", ActorSession: tc.actor}, engine)
			got := issue(t, pool, "LEGION-1")
			var reasserted int
			for _, effect := range effects(t, pool) {
				if write, ok := effect.payload.(record.StatusWrite); ok && effect.issue == "LEGION-1" && write == (record.StatusWrite{Status: "retro", ObservedStatus: "done"}) {
					reasserted++
				}
			}
			if !tc.agent {
				if got.Phase != phase.Done || got.LingerUntil == nil {
					t.Fatalf("root after a human's done = %#v, want its tree lingering", got)
				}
				return
			}
			if got.Phase != phase.ProductionCheck || got.LingerUntil != nil || got.Status != "retro" || got.LastDispatchSeq != 2 || reasserted != 1 {
				t.Fatalf("root after its implementer's done = %#v with %d re-asserted status writes, want production_check, not lingering, status retro kept, seq 2, and retro re-asserted over done once", got, reasserted)
			}
			assertSlots(t, pool, []record.Slot{{Issue: "LEGION-1", Index: 0, AdmittedAt: fixedNow}, {Issue: "LEGION-9", Index: 1, AdmittedAt: fixedNow}})
		})
	}
}

// The boot read re-admits a lingering root the human set back to todo while the daemon was down,
// exactly as the live event does: a new generation, admitted, its linger cleared.
func TestReconcileReadmitsALingeringRootSetBackToTodo(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	until := fixedNow.Add(time.Hour)
	putIssue(t, pool, record.Issue{Key: "LEGION-LINGER", Project: "LEGION", Title: "lingering", Tree: "LEGION-LINGER", Phase: phase.Done, Generation: 3, Status: "done", Rank: "A", LingerUntil: &until})

	reconcile(t, pool, admission, []dispatch.IssueSummary{{Key: "LEGION-LINGER", Title: "lingering", Status: "todo", Rank: "A"}})
	readmitted := issue(t, pool, "LEGION-LINGER")
	if readmitted.Generation != 4 || readmitted.LingerUntil != nil || readmitted.Phase != phase.Admitted || readmitted.Status != "in_progress" {
		t.Fatalf("reconciled root = %#v, want generation 4, admitted, and no linger", readmitted)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-LINGER", Index: 0, AdmittedAt: fixedNow}})
}

// A root set back to todo is a new generation of the whole tree. Only the root's own generation
// facts were cleared, so a child re-entered at the new generation still carried the last one's
// design gate and recorded handoff — and read them as its own.
func TestReadmissionClearsTheGenerationOfEveryIssueOfTheTree(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	until := fixedNow.Add(time.Hour)
	putIssue(t, pool, record.Issue{Key: "LEGION-LINGER", Project: "LEGION", Title: "lingering", Tree: "LEGION-LINGER", Phase: phase.Done, Generation: 3, Status: "done", Rank: "A", LingerUntil: &until})
	putIssue(t, pool, record.Issue{Key: "LEGION-CHILD", Project: "LEGION", Title: "child", Tree: "LEGION-LINGER", Parent: record.ParentOf("LEGION-LINGER"), Phase: phase.Done, Generation: 3, Status: "done", Rank: "B"})
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		if err := records.PutGate(context.Background(), tx, record.DesignGate{Issue: "LEGION-CHILD", ArtifactID: "artifact-child", LatestVersion: 2}); err != nil {
			t.Fatalf("seed the child's gate: %v", err)
		}
		if err := records.PutPhase(context.Background(), tx, record.PhaseRow{Issue: "LEGION-CHILD", Role: claim.RoleImplementer, Claim: "implementer-claim", HandoffCommit: "abc123", Rounds: 2, Verdict: "pass"}); err != nil {
			t.Fatalf("seed the child's handoff: %v", err)
		}
		// An open pull request survives the new generation; the verdict and review decision it
		// carried are the last generation's reading of a head nobody has reviewed since.
		if err := records.PutPullRequest(context.Background(), tx, record.PullRequest{
			Issue: "LEGION-CHILD", Repo: "acme/widgets", Number: 9, Branch: "legion/LEGION-CHILD", HeadSHA: "abc",
			HeadUpdatedAt: fixedNow, HeadUpdatedAtSource: "webhook", Failing: []string{}, FailingStatuses: []string{},
			Verdict: "failing", ReviewDecision: "CHANGES_REQUESTED", FixAttempts: 2, State: record.PullRequestOpen,
		}); err != nil {
			t.Fatalf("seed the child's open pull request: %v", err)
		}
	})

	reconcile(t, pool, admission, []dispatch.IssueSummary{{Key: "LEGION-LINGER", Title: "lingering", Status: "todo", Rank: "A"}})

	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		gate, err := records.Gate(context.Background(), tx, "LEGION-CHILD")
		if err != nil {
			t.Fatalf("read the child's gate: %v", err)
		}
		if gate != nil {
			t.Errorf("the child kept the last generation's design gate: %#v", gate)
		}
		phases, err := records.Phases(context.Background(), tx, "LEGION-CHILD")
		if err != nil {
			t.Fatalf("read the child's phases: %v", err)
		}
		for _, row := range phases {
			if row.HandoffCommit != "" || row.Rounds != 0 || row.Verdict != "" {
				t.Errorf("the child kept the last generation's handoff: %#v", row)
			}
		}
		pr, err := records.PullRequest(context.Background(), tx, "LEGION-CHILD")
		if err != nil {
			t.Fatalf("read the child's pull request: %v", err)
		}
		if pr == nil {
			t.Fatal("the child's open pull request went with the generation; an open one is kept")
		}
		if pr.Verdict != "" || pr.ReviewDecision != "" || pr.FixAttempts != 0 {
			t.Errorf("the kept pull request carried the last generation's reading: %#v", pr)
		}
	})
}

func TestReconcileFillsRaisedCapInRankOrderAndIsIdempotent(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	seedWaiting(t, pool, "LEGION-B", "B")
	seedWaiting(t, pool, "LEGION-A", "A")
	seedWaiting(t, pool, "LEGION-C", "C")
	admission.cap = 2
	summaries := []dispatch.IssueSummary{
		{Key: "LEGION-C", Title: "C", Status: "todo", Rank: "C"},
		{Key: "LEGION-B", Title: "B", Status: "todo", Rank: "B"},
		{Key: "LEGION-A", Title: "A", Status: "todo", Rank: "A"},
	}

	reconcile(t, pool, admission, summaries)
	assertSlots(t, pool, []record.Slot{
		{Issue: "LEGION-A", Index: 0, AdmittedAt: fixedNow},
		{Issue: "LEGION-B", Index: 1, AdmittedAt: fixedNow},
	})
	assertWaiting(t, pool, []string{"LEGION-C"})
	firstEffects := effects(t, pool)
	if len(firstEffects) != 4 {
		t.Fatalf("effects after raised-cap reconcile = %#v, want two status and two supervise rows", firstEffects)
	}

	reconcile(t, pool, admission, summaries)
	if got := effects(t, pool); !reflect.DeepEqual(got, firstEffects) {
		t.Fatalf("second reconcile effects = %#v, want unchanged %#v", got, firstEffects)
	}
}

func TestReconcileReleasesSlotWhoseDispatchStatusLeftActiveSet(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	seedSlotted(t, pool, "LEGION-ACTIVE", "A")

	reconcile(t, pool, admission, []dispatch.IssueSummary{{Key: "LEGION-ACTIVE", Title: "active", Status: "done", Rank: "A"}})
	assertSlots(t, pool, nil)
	if got := issue(t, pool, "LEGION-ACTIVE"); got.Status != "done" {
		t.Fatalf("reconciled status = %q, want done", got.Status)
	}
}

// Boot's Dispatch read is a snapshot with no actor on it, and an agent's own status write looks
// exactly like a human's in it. Dispatch says how far each issue's event log has run, so an issue
// whose log is ahead of the record is left to the stream, which carries the actor and applies the
// same change with it. An issue the stream has nothing newer for is reconciled as before.
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

	// The same issue once the record has caught up with Dispatch's log: nothing newer is coming.
	reconcile(t, pool, admission, []dispatch.IssueSummary{
		{Key: "LEGION-ACTIVE", Title: "active", Status: "done", Rank: "A", LastSeq: 10},
	})
	if got := issue(t, pool, "LEGION-ACTIVE"); got.Status != "done" {
		t.Fatalf("reconciled status = %q, want done once the record has caught up", got.Status)
	}
	assertSlots(t, pool, nil)
}

func TestCapturedDispatchTodoEventAdmitsAndProjectsActiveSlot(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	js := testJetStream(t)
	consumers, err := intake.OpenConsumers(context.Background(), js, intake.ConsumerSpec{Project: "CAPTURE", Repositories: []ghrepo.Repository{ghrepo.MustParse("sjawhar/legion")}, AckWait: time.Second, NakDelay: time.Millisecond, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- consumers.Run(ctx, pool, engineStub{}, admission) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	})

	captured, err := os.ReadFile("../intake/testdata/dispatch/issue-updated.json")
	if err != nil {
		t.Fatalf("read captured Dispatch issue.updated event: %v", err)
	}
	if _, err := js.Publish(context.Background(), "notifications.dispatch.issue.CAPTURE-3.issue.updated", captured); err != nil {
		t.Fatalf("publish captured Dispatch event: %v", err)
	}
	testwait.Eventually(t, "captured event admission", func() bool {
		tx, err := pool.Begin(context.Background())
		if err != nil {
			return false
		}
		defer tx.Rollback(context.Background())
		state, err := projection.Project(context.Background(), tx, record.NewStore(), "CAPTURE", nil)
		if err != nil {
			return false
		}
		return reflect.DeepEqual(state.Admission.Active, []string{"CAPTURE-3"}) && state.Issues["CAPTURE-3"].Slot != nil
	})
}
