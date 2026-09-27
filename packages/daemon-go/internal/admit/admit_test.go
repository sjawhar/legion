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

	apply(t, pool, admission, "root-todo", intake.DispatchIssue{Key: "LEGION-208", Seq: 1, Type: "issue.updated", Status: "todo", Title: "Admission", Rank: "B", Labels: handed}, engineStub{})
	root := issue(t, pool, "LEGION-208")
	if root.Phase != phase.Admitted || root.Status != "in_progress" || root.Tree != root.Key {
		t.Fatalf("admitted root = %#v, want admitted root with truthful in_progress status and its own tree", root)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-208", Index: 0, AdmittedAt: fixedNow}})
	assertEffects(t, pool, []effect{
		{kind: record.OutboxKindDispatchStatus, issue: "LEGION-208", payload: record.StatusWrite{Status: "in_progress", ObservedStatus: "todo"}},
		{kind: record.OutboxKindSupervise, issue: "LEGION-208", payload: record.SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleArchitect, Generation: 1}},
	})

	apply(t, pool, admission, "queued-todo", intake.DispatchIssue{Key: "LEGION-209", Seq: 1, Type: "issue.updated", Status: "todo", Title: "Queued", Rank: "A", Labels: handed}, engineStub{})
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
		{Key: "LEGION-C", Seq: 1, Type: "issue.updated", Status: "todo", Title: "rank C", Rank: "C", Labels: handed},
		{Key: "LEGION-A", Seq: 1, Type: "issue.updated", Status: "todo", Title: "rank A", Rank: "A", Labels: handed},
		{Key: "LEGION-B", Seq: 1, Type: "issue.updated", Status: "todo", Title: "rank B", Rank: "B", Labels: handed},
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

	apply(t, pool, admission, "orphan-todo", intake.DispatchIssue{Key: "LEGION-ORPHAN", Seq: 1, Type: "issue.updated", Status: "todo", Title: "orphan", Parent: "LEGION-MISSING", Rank: "A", Labels: handed}, engineStub{})
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

// A root in triage is the controller's to triage once it is handed to Legion, so an observation of
// the unrecorded root that carries the label wakes the controller: its creation with the label, or
// the edit that adds it, since the dashboard creates an issue without labels. A root in triage
// without the label is someone else's and wakes nobody. The stream's redelivery of an event adds
// nothing, and the root is not recorded.
func TestAnUnrecordedRootInTriageWakesTheControllerWhenItCarriesTheLabel(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))

	apply(t, pool, admission, "created-unlabeled", intake.DispatchIssue{Key: "LEGION-300", Seq: 1, Type: "issue.created", Status: "triage", Title: "New", Rank: "A", Labels: []string{"frontend"}}, engineStub{})
	assertEffects(t, pool, nil)

	labeled := intake.DispatchIssue{Key: "LEGION-300", Seq: 2, Type: "issue.updated", Status: "triage", Title: "New", Rank: "A", Labels: []string{"frontend", "Legion"}}
	apply(t, pool, admission, "labeled", labeled, engineStub{})
	apply(t, pool, admission, "labeled", labeled, engineStub{})
	apply(t, pool, admission, "created-labeled", intake.DispatchIssue{Key: "LEGION-305", Seq: 1, Type: "issue.created", Status: "triage", Title: "Handed over at creation", Rank: "B", Labels: handed}, engineStub{})
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
			apply(t, pool, admission, "child", intake.DispatchIssue{Key: "LEGION-301", Seq: 1, Type: "issue.created", Status: "triage", Title: "Child", Parent: "LEGION-300", Rank: "A", Labels: handed}, engineStub{})
		}},
		{"a recorded root set back to triage", func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
			seedWaiting(t, pool, "LEGION-302", "A")
			apply(t, pool, admission, "back", intake.DispatchIssue{Key: "LEGION-302", Seq: 2, Type: "issue.updated", Status: "triage", Title: "waiting", Rank: "A", Labels: handed}, engineStub{})
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
	apply(t, pool, admission, "todo", intake.DispatchIssue{Key: "LEGION-304", Seq: 2, Type: "issue.updated", Status: "todo", Title: "New", Rank: "A", Labels: handed}, engineStub{})
	before := effects(t, pool)

	apply(t, pool, admission, "created", intake.DispatchIssue{Key: "LEGION-304", Seq: 1, Type: "issue.created", Status: "triage", Title: "New", Rank: "A", Labels: handed}, engineStub{})
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

	fact := intake.DispatchIssue{Key: lingering.Key, Seq: 2, Type: "issue.updated", Status: "todo", Title: lingering.Title, Rank: lingering.Rank, Labels: handed}
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

	apply(t, pool, admission, "readmit", intake.DispatchIssue{Key: root.Key, Seq: 2, Type: "issue.updated", Status: "todo", Title: root.Title, Rank: root.Rank, Labels: handed}, engine)

	assertEffects(t, pool, []effect{
		{kind: record.OutboxKindDispatchStatus, issue: root.Key, payload: record.StatusWrite{Status: "in_progress", ObservedStatus: "todo"}},
		{kind: record.OutboxKindSupervise, issue: root.Key, payload: record.SuperviseRequest{Op: "start", Tree: root.Key, Role: claim.RoleArchitect, Generation: 2}},
		{kind: record.OutboxKindSupervise, issue: "LEGION-209", payload: record.SuperviseRequest{Op: "start", Tree: root.Key, Role: claim.RoleTester, Generation: 1, Phase: phase.Testing,
			Task: "Continue mid-phase child. Issue: LEGION-209. Phase: testing. Resume the existing phase work.", ResumeTask: true}},
	})
}

// A merge is the one fact GitHub never sends again, and a child whose READY was posted can be
// merged after its root closed. The lingering tree starts no worker, but the child moves on to its
// production check, so re-admission, which keeps no merged pull request, starts its implementer
// there instead of leaving it in awaiting_merge, where no role could move it.
func TestAMergeWhileTheTreeLingersIsTheChildsProductionCheckOnceTheTreeRunsAgain(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	until := fixedNow.Add(time.Hour)
	root := record.Issue{Key: "LEGION-208", Project: "LEGION", Title: "root", Tree: "LEGION-208", Phase: phase.Done, Generation: 1, Status: "done", Rank: "A", LingerUntil: &until, LastDispatchSeq: 1}
	putIssue(t, pool, root)
	parentKey := root.Key
	putIssue(t, pool, record.Issue{Key: "LEGION-209", Project: "LEGION", Title: "merged child", Tree: root.Key, Parent: &parentKey,
		Phase: phase.AwaitingMerge, Generation: 1, Status: "in_progress", Rank: "B", LastDispatchSeq: 1})
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutPullRequest(context.Background(), tx, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-209", Repo: "sjawhar/legion", Number: 42,
			Branch: "legion/LEGION-209", HeadSHA: "head", Failing: []string{}, FailingStatuses: []string{}}); err != nil {
			t.Fatalf("seed pull request: %v", err)
		}
	})
	implementerStarts := func() int {
		t.Helper()
		var starts int
		if err := pool.QueryRow(context.Background(), `select count(*) from outbox where issue = 'LEGION-209' and kind = 'supervise'
			and payload->>'op' = 'start' and payload->>'role' = 'implementer' and payload->>'phase' = 'production_check'`).Scan(&starts); err != nil {
			t.Fatalf("count the child's starts: %v", err)
		}
		return starts
	}

	if _, err := intake.ApplyFact(context.Background(), pool, "github", "merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge"}, engine, admission); err != nil {
		t.Fatalf("ApplyFact merge: %v", err)
	}
	if got, starts := issue(t, pool, "LEGION-209"), implementerStarts(); got.Phase != phase.ProductionCheck || starts != 0 {
		t.Fatalf("the merged child while its tree lingers = %s with %d implementer starts, want production_check with none", got.Phase, starts)
	}
	apply(t, pool, admission, "readmit", intake.DispatchIssue{Key: root.Key, Seq: 2, Type: "issue.updated", Status: "todo", Title: root.Title, Rank: root.Rank, Labels: handed}, engine)
	if got, starts := issue(t, pool, "LEGION-209"), implementerStarts(); got.Phase != phase.ProductionCheck || starts != 1 {
		t.Fatalf("the merged child after re-admission = %s with %d implementer starts, want production_check with its implementer started", got.Phase, starts)
	}
}

// A re-admitted root that waits for a slot no longer lingers, so a fact in that window can move a
// child and start its worker. The root's promotion then starts its mid-phase children: a child's
// worker is started unless the newest of its role's operations that will still act is a start.
// Each row is the child tester's outbox and claim when the root is promoted, and how many starts
// the promotion adds: a second start would give the same task twice, and a missing one leaves the
// child mid-phase with nobody working it once a queued stop suspends its worker.
func TestPromotionStartsAChildUnlessItsWorkerIsStartedForTheRun(t *testing.T) {
	type seed struct {
		enqueue func(op record.SuperviseOp, generation uint64) int64
		claim   func(state string, serving uint64, lastStart int64)
		// pending gives the claim a task it holds undelivered or unconfirmed.
		pending func(generation uint64, p phase.Phase)
		// confirmedPending gives the claim a task whose turn already started.
		confirmedPending func(generation uint64, p phase.Phase)
		// otherClaim records a claim on the same issue and role under another daemon's project token.
		otherClaim func(state string, lastStart int64)
		// suspendLeaving queues a transition's suspend, which ends phase leaves.
		suspendLeaving func(leaves phase.Phase)
	}
	for _, tc := range []struct {
		name  string
		setup func(s seed)
		want  int
	}{
		{name: "no worker", setup: func(seed) {}, want: 1},
		{name: "the window's start still queued", setup: func(s seed) { s.enqueue("start", 1) }, want: 0},
		{name: "a live claim the window's start resumed", setup: func(s seed) { s.claim("working", 1, 7) }, want: 0},
		{name: "a claim new to the run, its first task not yet confirmed", setup: func(s seed) { s.claim("ready", 0, 7) }, want: 0},
		{name: "a live claim the close's suspend will still stop", setup: func(s seed) {
			s.claim("working", 1, 0)
			s.enqueue("suspend", 1)
		}, want: 1},
		{name: "a live claim whose window start superseded the close's suspend", setup: func(s seed) {
			s.claim("working", 1, s.enqueue("suspend", 1)+1)
		}, want: 0},
		{name: "a start queued before the close's suspend", setup: func(s seed) {
			s.claim("working", 1, 0)
			s.enqueue("start", 1)
			s.enqueue("suspend", 1)
		}, want: 1},
		{name: "a start queued after the close's suspend", setup: func(s seed) {
			s.claim("working", 1, 0)
			s.enqueue("suspend", 1)
			s.enqueue("start", 1)
		}, want: 0},
		{name: "a live claim with the ended linger's tree close still queued", setup: func(s seed) {
			s.claim("working", 1, 0)
			s.enqueue("tree_close", 1)
		}, want: 0},
		{name: "a live claim with only an earlier generation's suspend queued", setup: func(s seed) {
			s.claim("working", 1, 0)
			s.enqueue("suspend", 0)
		}, want: 0},
		{name: "a suspended claim serving the generation", setup: func(s seed) { s.claim("suspended", 1, 7) }, want: 1},
		{name: "a failed claim serving the generation", setup: func(s seed) { s.claim("failed", 1, 7) }, want: 1},
		{name: "a launch-uncertain claim holding the phase's task", setup: func(s seed) {
			s.claim("launch_uncertain", 1, 7)
			s.pending(1, phase.Testing)
		}, want: 0},
		{name: "a launch-uncertain claim holding the phase's task confirmed", setup: func(s seed) {
			s.claim("launch_uncertain", 1, 7)
			s.confirmedPending(1, phase.Testing)
		}, want: 1},
		{name: "a launch-uncertain claim holding an earlier phase's task", setup: func(s seed) {
			s.claim("launch_uncertain", 1, 7)
			s.pending(1, phase.Implementing)
		}, want: 1},
		{name: "a claim holding the phase's task that the close's suspend will still stop", setup: func(s seed) {
			s.claim("launch_uncertain", 1, 0)
			s.pending(1, phase.Testing)
			s.enqueue("suspend", 1)
		}, want: 1},
		{name: "a failed claim holding the phase's task", setup: func(s seed) {
			s.claim("failed", 1, 7)
			s.pending(1, phase.Testing)
		}, want: 1},
		{name: "a retired claim holding the phase's task", setup: func(s seed) {
			s.claim("retired", 1, 7)
			s.pending(1, phase.Testing)
		}, want: 1},
		{name: "a suspended claim holding the phase's task", setup: func(s seed) {
			s.claim("suspended", 1, 7)
			s.pending(1, phase.Testing)
		}, want: 1},
		{name: "a queued claim holding the phase's task", setup: func(s seed) {
			s.claim("queued", 1, 7)
			s.pending(1, phase.Testing)
		}, want: 1},
		{name: "another daemon's live claim on the same issue and role", setup: func(s seed) { s.otherClaim("working", 0) }, want: 1},
		{name: "another daemon's claim beside this daemon's, with the close's suspend queued", setup: func(s seed) {
			s.claim("working", 1, 0)
			s.otherClaim("working", 0)
			s.enqueue("suspend", 1)
		}, want: 1},
		{name: "a live claim with only a suspend of the phase the child is back in queued", setup: func(s seed) {
			s.claim("working", 1, 0)
			s.suspendLeaving(phase.Testing)
		}, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
			engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
			seedSlotted(t, pool, "LEGION-100", "A")
			root := record.Issue{Key: "LEGION-208", Project: "LEGION", Title: "root", Tree: "LEGION-208", Phase: phase.Admitted, Generation: 2, Status: "todo", Rank: "B", HandedOver: true, LastDispatchSeq: 2}
			putIssue(t, pool, root)
			parentKey := root.Key
			child := record.Issue{Key: "LEGION-209", Project: "LEGION", Title: "child", Tree: root.Key, Parent: &parentKey,
				Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "C", LastDispatchSeq: 1}
			putIssue(t, pool, child)
			project, err := claim.ProjectToken(testProject)
			if err != nil {
				t.Fatal(err)
			}
			token, err := claim.NewToken(project, child.Key, claim.RoleTester)
			if err != nil {
				t.Fatal(err)
			}
			enqueueRequest := func(payload record.SuperviseRequest) int64 {
				t.Helper()
				row, err := record.NewOutboxRow(child.Key, payload, fixedNow)
				if err != nil {
					t.Fatal(err)
				}
				inTx(t, pool, func(tx pgx.Tx) {
					if err := record.NewStore().Enqueue(context.Background(), tx, row); err != nil {
						t.Fatalf("enqueue %s: %v", payload.Op, err)
					}
				})
				var id int64
				if err := pool.QueryRow(context.Background(), `select max(id) from outbox`).Scan(&id); err != nil {
					t.Fatalf("read the %s row's id: %v", payload.Op, err)
				}
				return id
			}
			putClaim := func(token claim.Token, project, state string, serving uint64, lastStart int64) {
				t.Helper()
				if _, err := pool.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
					launch_failures, prompt_failures, prompt_retires, uncertain_streak, serving_generation, last_start_row)
					values ($1, $2, 'LEGION-208', 'LEGION-209', 'tester', 1, 'ses_tester', '', $3, 0, 0, 0, 0, $4, $5)`,
					string(token), project, state, int64(serving), lastStart); err != nil {
					t.Fatalf("record the %s claim %s: %v", state, token, err)
				}
			}
			putPending := func(generation uint64, p phase.Phase, confirmedAt *time.Time) {
				t.Helper()
				if _, err := pool.Exec(context.Background(), `insert into pending_task_deliveries (claim_token, delivery_id, task, queued_at, generation, phase, confirmed_at)
					values ($1, 'outbox:7', 'Carry on.', now(), $2, $3, $4)`, string(token), int64(generation), string(p), confirmedAt); err != nil {
					t.Fatalf("record the pending task: %v", err)
				}
			}
			tc.setup(seed{
				enqueue: func(op record.SuperviseOp, generation uint64) int64 {
					payload := record.SuperviseRequest{Op: op, Tree: root.Key, Role: claim.RoleTester, Generation: generation}
					switch op {
					case "start":
						payload.Phase, payload.Task = child.Phase, workflow.ResumePhaseTask(child)
					case "tree_close":
						// The close of the linger the re-admission ended: the root's generation 1.
						payload.Linger = 1
					}
					return enqueueRequest(payload)
				},
				claim: func(state string, serving uint64, lastStart int64) {
					putClaim(token, project, state, serving, lastStart)
				},
				pending: func(generation uint64, p phase.Phase) {
					putPending(generation, p, nil)
				},
				confirmedPending: func(generation uint64, p phase.Phase) {
					confirmed := fixedNow
					putPending(generation, p, &confirmed)
				},
				otherClaim: func(state string, lastStart int64) {
					other, err := claim.NewToken("otherlegion", child.Key, claim.RoleTester)
					if err != nil {
						t.Fatal(err)
					}
					putClaim(other, "otherlegion", state, 1, lastStart)
				},
				suspendLeaving: func(leaves phase.Phase) {
					enqueueRequest(record.SuperviseRequest{Op: "suspend", Tree: root.Key, Role: claim.RoleTester, Generation: child.Generation, Leaves: leaves})
				},
			})
			// resumes counts the starts that carry the phase's task marked as its resume task.
			testerStarts := func() (starts, resumes int) {
				t.Helper()
				if err := pool.QueryRow(context.Background(), `select count(*), count(*) filter (where coalesce(payload->>'task', '') <> '' and payload->>'resumeTask' = 'true') from outbox
					where issue = 'LEGION-209' and kind = 'supervise' and payload->>'op' = 'start' and payload->>'role' = 'tester' and payload->>'phase' = 'testing'`).
					Scan(&starts, &resumes); err != nil {
					t.Fatalf("count the child's starts: %v", err)
				}
				return starts, resumes
			}
			before, beforeResumes := testerStarts()

			apply(t, pool, admission, "free-the-slot", intake.DispatchIssue{Key: "LEGION-100", Seq: 2, Type: "issue.closed", Status: "done", Title: "LEGION-100", Rank: "A"}, engine)
			if slotted := issue(t, pool, root.Key); slotted.Status != "in_progress" {
				t.Fatalf("the waiting root after the slot freed = %s, want it promoted", slotted.Status)
			}
			after, afterResumes := testerStarts()
			if added, addedResumes := after-before, afterResumes-beforeResumes; added != tc.want || addedResumes != tc.want {
				t.Fatalf("tester starts the promotion added = %d, %d of them with the phase's resume task; want %d, each with it", added, addedResumes, tc.want)
			}
		})
	}
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

	apply(t, pool, admission, "rerank", intake.DispatchIssue{Key: "LEGION-3", Seq: 2, Type: "issue.updated", Status: "todo", Title: "renamed", Parent: "LEGION-9", Rank: "AB", Labels: handed}, engine)
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
// pull request instead of starting the tester on generation 1's. A child's READY the old gate
// refused goes with the rest: its packet is cleared with the handoffs, so the new gate's approval
// neither posts a READY with no packet nor moves the child on.
func TestReadmissionStartsTheNewGenerationWithoutTheOldGenerationsFacts(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, DesignGate: config.DesignGateRootIssues, ReviewRoundCap: 3, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	const key, artifact = "LEGION-LINGER", "4f2a9c1e-8b3d-4e7f-9a60-2c5d8e1b7f34"
	until, pending, approved := fixedNow.Add(time.Hour), 1, 1
	putIssue(t, pool, record.Issue{Key: key, Project: "LEGION", Title: "lingering", Tree: key, Phase: phase.Done, Generation: 1, Status: "done", Rank: "A", LingerUntil: &until, LastDispatchSeq: 1, ReadyPendingVersion: &pending})
	const child = "LEGION-2"
	parent := key
	putIssue(t, pool, record.Issue{Key: child, Project: "LEGION", Title: "child", Tree: key, Parent: &parent, Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "B", LastDispatchSeq: 1, ReadyPendingVersion: &pending})
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		ctx := context.Background()
		if err := records.PutGate(ctx, tx, record.DesignGate{Issue: key, ArtifactID: artifact, LatestVersion: 1, ApprovedVersion: &approved}); err != nil {
			t.Fatalf("seed gate: %v", err)
		}
		if err := records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestMerged, Issue: key, Repo: "sjawhar/legion", Number: 86, Branch: "legion/" + key, HeadSHA: "merged", Verdict: "green", Failing: []string{}, FailingStatuses: []string{}}); err != nil {
			t.Fatalf("seed pull request: %v", err)
		}
		if err := records.PutPhase(ctx, tx, record.PhaseRow{Issue: key, Role: claim.RoleImplementer, Claim: "implementer", HandoffCommit: "gen1-handoff", LastHandoff: "gen1-handoff", Rounds: 2}); err != nil {
			t.Fatalf("seed implementer: %v", err)
		}
		if err := records.PutPhase(ctx, tx, record.PhaseRow{Issue: child, Role: claim.RoleMerger, Claim: "merger", Summary: "READY #85 at gen1 (approved at gen1) for " + child}); err != nil {
			t.Fatalf("seed the child's merger: %v", err)
		}
	})

	apply(t, pool, admission, "readmit", intake.DispatchIssue{Key: key, Seq: 2, Type: "issue.updated", Status: "todo", Title: "lingering", Rank: "A", Labels: handed}, engine)
	readmitted := issue(t, pool, key)
	if readmitted.Generation != 2 || readmitted.Phase != phase.Admitted || readmitted.ReadyPendingVersion != nil {
		t.Fatalf("readmitted = %#v, want generation 2, admitted, no READY pending", readmitted)
	}
	if got := issue(t, pool, child); got.ReadyPendingVersion != nil {
		t.Fatalf("the child's READY pending after re-admission = %d, want none", *got.ReadyPendingVersion)
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
	var messages int
	if err := pool.QueryRow(context.Background(), "select count(*) from outbox where issue = $1 and kind = 'dispatch_message'", child).Scan(&messages); err != nil || messages != 0 {
		t.Fatalf("the child's Dispatch messages = %d, %v; want no READY posted by generation 2's approval", messages, err)
	}
	if got := issue(t, pool, child); got.Phase != phase.Merging {
		t.Fatalf("the child is in %s, want it left in merging", got.Phase)
	}
}

// A signed-off child reopened to todo belongs to its tree while the tree is live: a child under a
// live tree takes no slot and runs under that tree's architect (decision 11; the shipped
// admitOnTodo), and needs no label of its own. The workflow re-enters it, starting the child's next
// generation under the open gate; its tree and slots stay the tree's. Under a lingering tree the
// child is an orphan, and admission admits it as a root of its own, as the shipped daemon does —
// handed to Legion by its label, as every root is.
func TestAReopenedChildReentersALiveTreeAndIsAnOrphanRootOfALingeringOne(t *testing.T) {
	for _, tc := range []struct {
		name      string
		lingering bool
		labels    []string
	}{
		{name: "live tree"},
		{name: "lingering tree", lingering: true, labels: handed},
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

			apply(t, pool, admission, "child-reopened", intake.DispatchIssue{Key: child, Seq: 3, Type: "issue.updated", Status: "todo", Title: "child", Parent: root, Rank: "B", Labels: tc.labels}, engine)
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

	reconcile(t, pool, admission, []dispatch.IssueSummary{{Key: "LEGION-LINGER", Title: "lingering", Status: "todo", Rank: "A", Labels: handed}})
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
		// An open pull request survives the new generation; the verdict it carried is the last
		// generation's reading of a head nobody has judged since.
		if err := records.PutPullRequest(context.Background(), tx, record.PullRequest{
			Issue: "LEGION-CHILD", Repo: "acme/widgets", Number: 9, Branch: "legion/LEGION-CHILD", HeadSHA: "abc",
			HeadUpdatedAt: fixedNow, HeadUpdatedAtSource: "webhook", Failing: []string{}, FailingStatuses: []string{},
			Verdict: "failing", FixAttempts: 2, State: record.PullRequestOpen,
		}); err != nil {
			t.Fatalf("seed the child's open pull request: %v", err)
		}
	})

	reconcile(t, pool, admission, []dispatch.IssueSummary{{Key: "LEGION-LINGER", Title: "lingering", Status: "todo", Rank: "A", Labels: handed}})

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
		if pr.Verdict != "" || pr.FixAttempts != 0 {
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
		{Key: "LEGION-C", Title: "C", Status: "todo", Rank: "C", Labels: handed},
		{Key: "LEGION-B", Title: "B", Status: "todo", Rank: "B", Labels: handed},
		{Key: "LEGION-A", Title: "A", Status: "todo", Rank: "A", Labels: handed},
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
	go func() { done <- consumers.Run(ctx, pool, admission, engineStub{}, admission) }()
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
	// The captured issue carries no labels; this one is handed to Legion.
	labeled := strings.Replace(string(captured), `\"labels\":[]`, `\"labels\":[\"legion\"]`, 1)
	if labeled == string(captured) {
		t.Fatal("the captured event has no empty labels to hand the issue over with")
	}
	if _, err := js.Publish(context.Background(), "notifications.dispatch.issue.CAPTURE-3.issue.updated", []byte(labeled)); err != nil {
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

// Legion admits only an issue handed to it. A root or an orphan in todo without the legion label is
// someone else's work in a project Legion shares: it is not recorded, takes no slot, and nothing is
// written or started for it. The label, in any case and among any others, hands it over.
func TestOnlyAnIssueCarryingTheLegionLabelIsAdmitted(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))

	apply(t, pool, admission, "root-unlabeled", intake.DispatchIssue{Key: "LEGION-1", Seq: 1, Type: "issue.updated", Status: "todo", Title: "someone else's", Rank: "A", Labels: []string{"frontend"}}, engineStub{})
	apply(t, pool, admission, "orphan-unlabeled", intake.DispatchIssue{Key: "LEGION-2", Seq: 1, Type: "issue.updated", Status: "todo", Title: "a human's child", Parent: "LEGION-MISSING", Rank: "B"}, engineStub{})
	for _, key := range []string{"LEGION-1", "LEGION-2"} {
		if got := maybeIssue(t, pool, key); got != nil {
			t.Fatalf("unlabeled %s was recorded: %#v", key, got)
		}
	}
	assertSlots(t, pool, nil)
	assertEffects(t, pool, nil)

	apply(t, pool, admission, "root-labeled", intake.DispatchIssue{Key: "LEGION-1", Seq: 2, Type: "issue.updated", Status: "todo", Title: "handed over", Rank: "A", Labels: []string{"frontend", "Legion"}}, engineStub{})
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
		{Key: "LEGION-C", Seq: 1, Type: "issue.updated", Status: "todo", Title: "rank C", Rank: "C", Labels: handed},
		{Key: "LEGION-B", Seq: 1, Type: "issue.updated", Status: "todo", Title: "rank B", Rank: "B", Labels: handed},
	} {
		apply(t, pool, admission, "arrive-"+event.Key, event, engineStub{})
	}
	assertWaiting(t, pool, []string{"LEGION-B", "LEGION-C"})

	apply(t, pool, admission, "active-done", intake.DispatchIssue{Key: "LEGION-ACTIVE", Seq: 2, Type: "issue.updated", Status: "done", Title: "active", Rank: "A", Labels: handed}, engineStub{})
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-B", Index: 0, AdmittedAt: fixedNow}})
	assertWaiting(t, pool, []string{"LEGION-C"})
}

// A child is its tree's: under a root handed to Legion, a child needs no label of its own. It joins
// the root's tree and takes no slot, as every child does.
func TestAnUnlabeledChildOfAnAdmittedRootRunsInItsTree(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)

	apply(t, pool, admission, "root", intake.DispatchIssue{Key: "LEGION-1", Seq: 1, Type: "issue.updated", Status: "todo", Title: "root", Rank: "A", Labels: handed}, engine)
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

	apply(t, pool, admission, "next-labeled", intake.DispatchIssue{Key: "LEGION-NEXT", Seq: 3, Type: "issue.updated", Status: "todo", Title: "next", Rank: "B", Labels: handed}, engineStub{})
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
		{Key: "LEGION-OTHER", Title: "someone else's", Status: "todo", Rank: "A", Labels: []string{"frontend"}},
		{Key: "LEGION-MINE", Title: "handed over", Status: "todo", Rank: "B", Labels: []string{"LEGION"}},
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

	apply(t, pool, admission, "labeled", intake.DispatchIssue{Key: lingering.Key, Seq: 3, Type: "issue.updated", Status: "todo", Title: lingering.Title, Rank: lingering.Rank, Labels: handed}, engine)
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
			apply(t, pool, admission, "labeled", intake.DispatchIssue{Key: child, Seq: 4, Type: "issue.updated", Status: "todo", Title: "child", Parent: root, Rank: "B", Labels: handed}, engine)
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

	apply(t, pool, admission, "root-readmitted", intake.DispatchIssue{Key: root, Seq: 2, Type: "issue.updated", Status: "todo", Title: "lingering", Rank: "A", Labels: handed}, engine)

	got := issue(t, pool, child)
	if got.Tree != root {
		t.Fatalf("child after the root's re-admission = %#v, want it kept in %s's tree, not its own", got, root)
	}
	if got.Phase != phase.Admitted || got.Generation != 3 {
		t.Fatalf("child after the root's re-admission = %#v, want phase admitted, generation 3", got)
	}
	assertSlots(t, pool, []record.Slot{{Issue: root, Index: 0, AdmittedAt: fixedNow}})
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
		{Key: "LEGION-EDGE", Title: "LEGION-EDGE", Status: "todo", Rank: "A", LastSeq: 2},
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
		{Key: "LEGION-EDGE", Title: "LEGION-EDGE", Status: "todo", Rank: "A", LastSeq: 2},
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
		{Key: "LEGION-EDGE", Title: "LEGION-EDGE", Status: "todo", Rank: "A", LastSeq: 2},
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
				{Key: "LEGION-EDGE", Title: "LEGION-EDGE", Status: "todo", Rank: "A", LastSeq: 2},
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
// B is discovered by an earlier boot's Reconcile — whose own putNewRoot records no sequence — so
// this restart's boot read finds Dispatch's log ahead of B's record; with the consumer idle, that
// must not hold B back once A's slot frees.
func TestReconcileReleasesImmediatelyWhenTheDispatchConsumerIsIdle(t *testing.T) {
	pool := migratedPool(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	admission := newAdmission(t, 1, log)

	reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{
		{Key: "LEGION-A", Title: "A", Status: "todo", Rank: "A", Labels: handed, LastSeq: 5},
		{Key: "LEGION-B", Title: "B", Status: "todo", Rank: "B", Labels: handed, LastSeq: 7},
	}, 0, 0, true)
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-A", Index: 0, AdmittedAt: fixedNow}})
	assertWaiting(t, pool, []string{"LEGION-B"})

	// Restart before A leaves: a fresh Admission, the same boot read. A still holds its slot; B's
	// record — from the first boot's own putNewRoot, which records no sequence — reads behind
	// Dispatch's log again, but the consumer is idle.
	admission = newAdmission(t, 1, log)
	reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{
		{Key: "LEGION-A", Title: "A", Status: "in_progress", Rank: "A", Labels: handed, LastSeq: 5},
		{Key: "LEGION-B", Title: "B", Status: "todo", Rank: "B", Labels: handed, LastSeq: 7},
	}, 0, 0, true)
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-A", Index: 0, AdmittedAt: fixedNow}})

	// A leaves after the restart: B must not still be waiting. Seq 6 is newer than the 5 the idle
	// reconcile already applied to A's record, as a real subsequent live event would be.
	apply(t, pool, admission, "a-done", intake.DispatchIssue{Key: "LEGION-A", Seq: 6, Type: "issue.updated", Status: "done", Title: "A", Rank: "A"}, engineStub{})
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-B", Index: 0, AdmittedAt: fixedNow}})
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
	apply(t, pool, admission, "root-relabeled", intake.DispatchIssue{Key: root, Seq: 3, Type: "issue.updated", Status: "todo", Title: root, Rank: "B", Labels: handed}, engine)
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
