package admit

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/projection"
	"github.com/sjawhar/legion/daemon/internal/record"
	legionstore "github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testnats"
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
		{kind: record.OutboxKindSupervise, issue: "LEGION-208", payload: record.SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleArchitect}},
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
		{kind: record.OutboxKindSupervise, issue: "LEGION-208", payload: record.SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleArchitect}},
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
		{kind: record.OutboxKindSupervise, issue: "LEGION-NEXT", payload: record.SuperviseRequest{Op: "start", Tree: "LEGION-NEXT", Role: claim.RoleArchitect}},
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
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, LingerHours: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
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
		{kind: record.OutboxKindSupervise, issue: lingering.Key, payload: record.SuperviseRequest{Op: "start", Tree: lingering.Key, Role: claim.RoleArchitect}},
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

// Every newer Dispatch observation is recorded, not only a status change: re-ranking a waiting
// root, renaming it, or re-parenting it arrives as an issue.updated at the same status, and the
// waiting line has to follow Dispatch rank order at once, not after the next boot's read. It runs
// through the real engine, which sees every fact before admission.
func TestApplyFactRecordsRankTitleAndParentChangesAtTheSameStatus(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, LingerHours: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
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

func TestCapturedDispatchTodoEventAdmitsAndProjectsActiveSlot(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	js := testJetStream(t)
	consumers, err := intake.OpenConsumers(context.Background(), js, intake.ConsumerSpec{Project: "CAPTURE", Repositories: []string{"sjawhar/legion"}, AckWait: time.Second, NakDelay: time.Millisecond, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
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
	eventually(t, "captured event admission", func() bool {
		tx, err := pool.Begin(context.Background())
		if err != nil {
			return false
		}
		defer tx.Rollback(context.Background())
		state, err := projection.Project(context.Background(), tx, record.NewStore(), nil)
		if err != nil {
			return false
		}
		return reflect.DeepEqual(state.Admission.Active, []string{"CAPTURE-3"}) && state.Issues["CAPTURE-3"].Slot != nil
	})
}

type engineStub struct {
	store       record.Store
	recordChild *record.Issue
	done        string
}

func (e engineStub) Apply(ctx context.Context, tx pgx.Tx, fact intake.Fact) (intake.Result, error) {
	if e.recordChild != nil {
		if dispatch, ok := fact.(intake.DispatchIssue); ok && dispatch.Key == e.recordChild.Key {
			if err := e.store.PutIssue(ctx, tx, *e.recordChild); err != nil {
				return intake.Result{}, err
			}
		}
	}
	if e.done != "" {
		stored, err := e.store.Issue(ctx, tx, e.done)
		if err != nil {
			return intake.Result{}, err
		}
		if stored == nil {
			return intake.Result{}, nil
		}
		stored.Phase = phase.Done
		if err := e.store.PutIssue(ctx, tx, *stored); err != nil {
			return intake.Result{}, err
		}
	}
	return intake.Result{}, nil
}

const testProject = "LEGION"

var fixedNow = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

func newAdmission(t *testing.T, cap int, log *slog.Logger) *Admission {
	t.Helper()
	admission := New(record.NewStore(), cap, testProject, log)
	admission.now = func() time.Time { return fixedNow }
	return admission
}

func apply(t *testing.T, pool *pgxpool.Pool, admission *Admission, eventID string, fact intake.DispatchIssue, engine intake.Handler) {
	t.Helper()
	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", eventID, fact, engine, admission); err != nil {
		t.Fatalf("ApplyFact %s: %v", eventID, err)
	}
}

type effect struct {
	kind    record.OutboxKind
	issue   string
	payload record.OutboxPayload
}

func assertEffects(t *testing.T, pool *pgxpool.Pool, want []effect) {
	t.Helper()
	if got := effects(t, pool); !reflect.DeepEqual(got, want) {
		t.Fatalf("outbox effects = %#v, want %#v", got, want)
	}
}

func effects(t *testing.T, pool *pgxpool.Pool) []effect {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select kind, issue, payload from outbox order by id`)
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	defer rows.Close()
	var got []effect
	for rows.Next() {
		var row record.OutboxRow
		if err := rows.Scan(&row.Kind, &row.Issue, &row.Payload); err != nil {
			t.Fatalf("scan outbox: %v", err)
		}
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			t.Fatalf("decode outbox: %v", err)
		}
		got = append(got, effect{kind: row.Kind, issue: row.Issue, payload: payload})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate outbox: %v", err)
	}
	return got
}

func assertSlots(t *testing.T, pool *pgxpool.Pool, want []record.Slot) {
	t.Helper()
	got := slots(t, pool)
	if len(got) != len(want) {
		t.Fatalf("slots = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i].Issue != want[i].Issue || got[i].Index != want[i].Index || !got[i].AdmittedAt.Equal(want[i].AdmittedAt) {
			t.Fatalf("slots = %#v, want %#v", got, want)
		}
	}
}

func assertWaiting(t *testing.T, pool *pgxpool.Pool, want []string) {
	t.Helper()
	var got []string
	inTx(t, pool, func(tx pgx.Tx) {
		issues, err := record.NewStore().Issues(context.Background(), tx)
		if err != nil {
			t.Fatalf("list issues: %v", err)
		}
		slots, err := record.NewStore().Slots(context.Background(), tx)
		if err != nil {
			t.Fatalf("list slots: %v", err)
		}
		for _, waiting := range record.Waiting(issues, slots) {
			got = append(got, waiting.Key)
		}
	})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("waiting = %#v, want %#v", got, want)
	}
}

func slots(t *testing.T, pool *pgxpool.Pool) []record.Slot {
	t.Helper()
	var got []record.Slot
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		got, err = record.NewStore().Slots(context.Background(), tx)
		if err != nil {
			t.Fatalf("list slots: %v", err)
		}
	})
	return got
}

func issue(t *testing.T, pool *pgxpool.Pool, key string) record.Issue {
	t.Helper()
	var got *record.Issue
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		got, err = record.NewStore().Issue(context.Background(), tx, key)
		if err != nil {
			t.Fatalf("read issue: %v", err)
		}
	})
	if got == nil {
		t.Fatalf("issue %s is missing", key)
	}
	return *got
}

func putIssue(t *testing.T, pool *pgxpool.Pool, issue record.Issue) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutIssue(context.Background(), tx, issue); err != nil {
			t.Fatalf("put issue: %v", err)
		}
	})
}

func seedSlotted(t *testing.T, pool *pgxpool.Pool, key, rank string) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		if err := records.PutIssue(context.Background(), tx, record.Issue{Key: key, Project: testProject, Title: key, Tree: key, Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: rank}); err != nil {
			t.Fatalf("put active issue: %v", err)
		}
		slots, err := records.Slots(context.Background(), tx)
		if err != nil {
			t.Fatalf("list slots: %v", err)
		}
		if err := records.PutSlot(context.Background(), tx, record.Slot{Issue: key, Index: len(slots), AdmittedAt: fixedNow}); err != nil {
			t.Fatalf("put slot: %v", err)
		}
	})
}

func seedWaiting(t *testing.T, pool *pgxpool.Pool, key, rank string) {
	t.Helper()
	putIssue(t, pool, record.Issue{Key: key, Project: testProject, Title: key, Tree: key, Phase: phase.Admitted, Generation: 1, Status: "todo", Rank: rank})
}

func inTx(t *testing.T, pool *pgxpool.Pool, fn func(pgx.Tx)) {
	t.Helper()
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	defer func() {
		if err := tx.Rollback(context.Background()); err != nil && err != pgx.ErrTxClosed {
			t.Errorf("rollback transaction: %v", err)
		}
	}()
	fn(tx)
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit transaction: %v", err)
	}
}

func migratedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("LEGION_TEST_PG_DSN")
	if dsn == "" {
		t.Fatal("LEGION_TEST_PG_DSN is required for real Postgres admission tests")
	}
	base, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse LEGION_TEST_PG_DSN: %v", err)
	}
	adminURL := *base
	adminURL.Path = "/postgres"
	admin, err := pgxpool.New(context.Background(), adminURL.String())
	if err != nil {
		t.Fatalf("connect admin database: %v", err)
	}
	t.Cleanup(admin.Close)
	name := "legion_admit_test_" + randomSuffix(t)
	if _, err := admin.Exec(context.Background(), "create database "+name); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "drop database "+name+" with (force)"); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})
	url := *base
	url.Path = "/" + name
	st, err := legionstore.Open(context.Background(), url.String())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := st.Migrate(context.Background()); err != nil {
		st.Close()
		t.Fatalf("migrate store: %v", err)
	}
	st.Close()
	pool, err := pgxpool.New(context.Background(), url.String())
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	return hex.EncodeToString(bytes[:])
}

func testJetStream(t *testing.T) jetstream.JetStream {
	t.Helper()
	js := testnats.JetStream(t)
	if _, err := js.CreateStream(t.Context(), jetstream.StreamConfig{Name: "ENVOY_NOTIFICATIONS", Subjects: []string{"notifications.>"}}); err != nil {
		t.Fatalf("create notification stream: %v", err)
	}
	return js
}

func eventually(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func reconcile(t *testing.T, pool *pgxpool.Pool, admission *Admission, summaries []dispatch.IssueSummary) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		if err := admission.Reconcile(context.Background(), tx, summaries); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	})
}
