package admit

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// statusWriteOutcome is what a lifecycle status write on a running root does to its tree.
type statusWriteOutcome int

const (
	// takesTheTreeOut: the tree lingers, and its slot is freed.
	takesTheTreeOut statusWriteOutcome = iota
	// setBack: the tree runs on, and the daemon writes its own status over the write.
	setBack
	// setBackAndTold: set back, and the tree's architect is told who wrote what.
	setBackAndTold
)

// Who wrote a lifecycle status on a running root decides whether it takes the tree out of the
// workflow. A person's move does: a user actor, and the daemon's own write, which decode passes with
// no session (TestDecodeDispatchIssueNamesASessionActor). A session holding a claim in the tree is an
// agent, never a human move, since a phase ends only by its completion and a tree only by its
// architect's sign-off; here the implementer closes its own issue during the production check
// instead of completing the phase. The workflow does not react (no linger, the slot kept, the phase
// unchanged) and the daemon re-asserts its own status through the outbox. Any other session —
// another tree's agent, or one outside Legion that took the issue for its own — cannot end or park
// the tree either: the daemon re-asserts its status, and the tree's architect is told who wrote what.
func TestWhoseStatusWriteTakesARunningRootOutOfTheWorkflow(t *testing.T) {
	for _, tc := range []struct {
		name    string
		actor   string
		status  string
		outcome statusWriteOutcome
	}{
		{name: "the tree's implementer closes it", actor: "ses-impl", status: "done", outcome: setBack},
		{name: "another tree's session closes it", actor: "ses-other", status: "done", outcome: setBackAndTold},
		{name: "a session outside Legion closes it", actor: "ses-outsider", status: "done", outcome: setBackAndTold},
		{name: "a session outside Legion parks it", actor: "ses-outsider", status: "backlog", outcome: setBackAndTold},
		{name: "a session outside Legion ices it", actor: "ses-outsider", status: "icebox", outcome: setBackAndTold},
		{name: "a person closes it", actor: "", status: "done", outcome: takesTheTreeOut},
		{name: "a person parks it", actor: "", status: "backlog", outcome: takesTheTreeOut},
		{name: "a person ices it", actor: "", status: "icebox", outcome: takesTheTreeOut},
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

			apply(t, pool, admission, "written-by-"+tc.name, intake.DispatchIssue{Key: "LEGION-1", Seq: 2, Type: "issue.updated", Status: tc.status, Title: "LEGION-1", Rank: "A", ActorSession: tc.actor}, engine)
			got := issue(t, pool, "LEGION-1")
			var reasserted int
			var told []record.Notice
			for _, effect := range effects(t, pool) {
				if write, ok := effect.payload.(record.StatusWrite); ok && effect.issue == "LEGION-1" && write == (record.StatusWrite{Status: "retro", ObservedStatus: tc.status}) {
					reasserted++
				}
				if notice, ok := effect.payload.(record.Notice); ok && notice.Kind == "status-reasserted" {
					told = append(told, notice)
				}
			}
			switch tc.outcome {
			case takesTheTreeOut:
				if got.Phase != phase.Done || got.LingerUntil == nil || reasserted != 0 || len(told) != 0 {
					t.Fatalf("root after %s = %#v with %d re-asserted status writes and notices %+v, want its tree lingering and nothing re-asserted or told", tc.name, got, reasserted, told)
				}
				assertSlots(t, pool, []record.Slot{{Issue: "LEGION-9", Index: 1, AdmittedAt: fixedNow}})
			case setBack, setBackAndTold:
				if got.Phase != phase.ProductionCheck || got.LingerUntil != nil || got.Status != "retro" || got.LastDispatchSeq != 2 || reasserted != 1 {
					t.Fatalf("root after %s = %#v with %d re-asserted status writes, want production_check, not lingering, status retro kept, seq 2, and retro re-asserted over %s once", tc.name, got, reasserted, tc.status)
				}
				assertSlots(t, pool, []record.Slot{{Issue: "LEGION-1", Index: 0, AdmittedAt: fixedNow}, {Issue: "LEGION-9", Index: 1, AdmittedAt: fixedNow}})
				wantTold := 0
				if tc.outcome == setBackAndTold {
					wantTold = 1
				}
				if len(told) != wantTold {
					t.Fatalf("notices %+v after %s, want %d status-reasserted", told, tc.name, wantTold)
				}
				if wantTold == 1 && (told[0].Role != claim.RoleArchitect || !strings.Contains(told[0].Reason, tc.actor) || !strings.Contains(told[0].Reason, tc.status)) {
					t.Fatalf("notice %+v, want it for the architect, naming %s and the %s it wrote", told[0], tc.actor, tc.status)
				}
			}
		})
	}
}

// Where no tree runs — on a child of a running tree, a root still waiting for its slot, or a
// lingering root — a session with no claim there writes a status as a person does: the child parks
// and its architect is told, the waiting root leaves the line, the lingering root is re-admitted.
// Each case compares the outside session's write with a person's on the same record: the issue, its
// root, the slots and every queued effect must come out the same.
func TestAnOutsideSessionsWriteWhereNoTreeRunsIsAPersonsMove(t *testing.T) {
	until := fixedNow.Add(time.Hour)
	parent := "LEGION-1"
	for _, tc := range []struct {
		name   string
		issues []record.Issue
		slots  []string
		key    string
		status string
		// taken says what the person's write did, so the comparison is not of two no-ops.
		taken func(t *testing.T, pool *pgxpool.Pool)
	}{
		{name: "a running tree's child parked", key: "LEGION-2", status: "backlog",
			issues: []record.Issue{
				{Key: "LEGION-1", Project: testProject, Title: "LEGION-1", Tree: "LEGION-1", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "A", HandedOver: true, LastDispatchSeq: 1},
				{Key: "LEGION-2", Project: testProject, Title: "LEGION-2", Tree: "LEGION-1", Parent: &parent, Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "B", LastDispatchSeq: 1},
			},
			slots: []string{"LEGION-1"},
			taken: func(t *testing.T, pool *pgxpool.Pool) {
				if got := issue(t, pool, "LEGION-2"); got.Phase != phase.Done || got.Status != "backlog" {
					t.Fatalf("child after a person's backlog = %#v, want it parked", got)
				}
			}},
		{name: "a root waiting for its slot parked", key: "LEGION-5", status: "backlog",
			issues: []record.Issue{
				{Key: "LEGION-1", Project: testProject, Title: "LEGION-1", Tree: "LEGION-1", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "A", HandedOver: true, LastDispatchSeq: 1},
				{Key: "LEGION-5", Project: testProject, Title: "LEGION-5", Tree: "LEGION-5", Phase: phase.Admitted, Generation: 1, Status: "todo", Rank: "B", HandedOver: true, LastDispatchSeq: 1},
			},
			slots: []string{"LEGION-1"},
			taken: func(t *testing.T, pool *pgxpool.Pool) { assertWaiting(t, pool, nil) }},
		{name: "a lingering root set back to todo", key: "LEGION-5", status: "todo",
			issues: []record.Issue{
				{Key: "LEGION-5", Project: testProject, Title: "LEGION-5", Tree: "LEGION-5", Phase: phase.Done, Generation: 1, Status: "done", Rank: "B", HandedOver: true, LingerUntil: &until, LastDispatchSeq: 1},
			},
			taken: func(t *testing.T, pool *pgxpool.Pool) {
				if got := issue(t, pool, "LEGION-5"); got.Generation != 2 || got.LingerUntil != nil {
					t.Fatalf("lingering root after a person's todo = %#v, want it re-admitted at generation 2", got)
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type result struct {
				issues  []record.Issue
				slots   []record.Slot
				effects []effect
			}
			write := func(actor string) (*pgxpool.Pool, result) {
				pool := migratedPool(t)
				admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
				engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
				for _, seeded := range tc.issues {
					putIssue(t, pool, seeded)
				}
				inTx(t, pool, func(tx pgx.Tx) {
					for i, key := range tc.slots {
						if err := record.NewStore().PutSlot(context.Background(), tx, record.Slot{Issue: key, Index: i, AdmittedAt: fixedNow}); err != nil {
							t.Fatalf("seed slot: %v", err)
						}
					}
				})
				apply(t, pool, admission, "written", intake.DispatchIssue{Key: tc.key, Seq: 2, Type: "issue.updated", Status: tc.status, Title: tc.key,
					Parent: deref(tc.issues[len(tc.issues)-1].Parent), Rank: "B", HandedOver: true, ActorSession: actor}, engine)
				var got result
				for _, seeded := range tc.issues {
					got.issues = append(got.issues, issue(t, pool, seeded.Key))
				}
				got.slots, got.effects = slots(t, pool), effects(t, pool)
				return pool, got
			}
			personPool, person := write("")
			tc.taken(t, personPool)
			_, outsider := write("ses-outsider")
			if !reflect.DeepEqual(outsider, person) {
				t.Fatalf("an outside session's %s on %s = %+v, want what a person's did: %+v", tc.status, tc.key, outsider, person)
			}
		})
	}
}

// An issue event carries the whole issue, so it writes a status only when its status differs from
// the one Dispatch showed at the event before it. The record takes a status the moment the daemon
// queues its write, before Dispatch shows it, so comparing an event with the record reads an edit
// made in between — a rank, title or label change — as a write by whoever made it. Compared with
// what Dispatch showed, it is an edit: recorded with the daemon's status kept, nothing set back,
// told or ended, whether or not the daemon's write has finished by the time the event is applied. A
// write that does change the status is still one, whatever the daemon has queued: a person's park
// after two outside writes were set back parks the tree, and neither set-back lands over it.
func TestOnlyAChangeFromTheStatusDispatchLastShowedIsAStatusWrite(t *testing.T) {
	type step struct{ actor, status, title string }
	inFlight := &record.StatusWrite{Status: "testing", ObservedStatus: "in_progress"}
	for _, tc := range []struct {
		name string
		// queued is a status write the daemon already queued, so the record holds its status;
		// finished says the outbox already ran it, so no row is left.
		queued   *record.StatusWrite
		finished bool
		steps    []step
		ends     bool
		rows     []record.StatusWrite
		notices  int
	}{
		{name: "an outside session's rename while the daemon's testing is on its way to Dispatch", queued: inFlight,
			steps: []step{{"ses-outsider", "in_progress", "renamed"}}, rows: []record.StatusWrite{*inFlight}},
		{name: "an outside session's rename sent before the daemon's testing landed and applied after the write finished", queued: inFlight, finished: true,
			steps: []step{{"ses-outsider", "in_progress", "renamed"}}},
		{name: "an outside session's rename before its backlog is set back",
			steps: []step{{"ses-outsider", "backlog", "LEGION-1"}, {"ses-outsider", "backlog", "renamed"}},
			rows:  []record.StatusWrite{{Status: "in_progress", ObservedStatus: "backlog"}}, notices: 1},
		{name: "a person's rename before an outside backlog is set back",
			steps: []step{{"ses-outsider", "backlog", "LEGION-1"}, {"", "backlog", "renamed"}},
			rows:  []record.StatusWrite{{Status: "in_progress", ObservedStatus: "backlog"}}, notices: 1},
		{name: "an outside session's backlog while the daemon's testing is on its way", queued: inFlight,
			steps: []step{{"ses-outsider", "backlog", "renamed"}},
			rows:  []record.StatusWrite{*inFlight, {Status: "testing", ObservedStatus: "backlog"}}, notices: 1},
		{name: "a person parks the tree after two outside writes were set back",
			steps: []step{{"ses-outsider", "backlog", "LEGION-1"}, {"ses-outsider", "icebox", "LEGION-1"}, {"", "backlog", "renamed"}},
			ends:  true, notices: 2},
		{name: "a person parks the tree after the set-back landed and before its row finished",
			steps: []step{{"ses-outsider", "backlog", "LEGION-1"}, {"", "in_progress", "LEGION-1"}, {"", "backlog", "renamed"}},
			ends:  true, notices: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
			engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
			seedSlotted(t, pool, "LEGION-1", "A")
			event := func(seq int64, actor, status, title string) {
				apply(t, pool, admission, "event-"+strconv.FormatInt(seq, 10), intake.DispatchIssue{Key: "LEGION-1", Seq: seq, Type: "issue.updated", Status: status,
					Title: title, Rank: "A", HandedOver: true, ActorSession: actor}, engine)
			}
			// Dispatch shows the admitted root in_progress: the daemon's own write, which decode passes
			// with no session.
			event(2, "", "in_progress", "LEGION-1")
			wantPhase, wantStatus := phase.Admitted, "in_progress"
			if tc.queued != nil {
				// A transition moves the record on to testing and queues its write.
				wantPhase, wantStatus = phase.Testing, tc.queued.Status
				inTx(t, pool, func(tx pgx.Tx) {
					records, ctx := record.NewStore(), context.Background()
					root, err := records.Issue(ctx, tx, "LEGION-1")
					if err != nil {
						t.Fatalf("read root: %v", err)
					}
					root.Phase, root.Status = wantPhase, wantStatus
					if err := records.PutIssue(ctx, tx, *root); err != nil {
						t.Fatalf("put root: %v", err)
					}
					if tc.finished {
						return
					}
					row, err := record.NewOutboxRow("LEGION-1", *tc.queued, fixedNow)
					if err != nil {
						t.Fatalf("build the queued status write: %v", err)
					}
					if err := records.Enqueue(ctx, tx, row); err != nil {
						t.Fatalf("queue the status write: %v", err)
					}
				})
			}

			for i, s := range tc.steps {
				event(int64(i+3), s.actor, s.status, s.title)
			}
			got := issue(t, pool, "LEGION-1")
			var rows []record.StatusWrite
			var notices int
			for _, effect := range effects(t, pool) {
				switch payload := effect.payload.(type) {
				case record.StatusWrite:
					rows = append(rows, payload)
				case record.Notice:
					if payload.Kind == "status-reasserted" {
						notices++
					}
				}
			}
			if !reflect.DeepEqual(rows, tc.rows) || notices != tc.notices {
				t.Fatalf("queued status writes %+v and %d status-reasserted notices, want %+v and %d", rows, notices, tc.rows, tc.notices)
			}
			if tc.ends {
				if got.Phase != phase.Done || got.LingerUntil == nil {
					t.Fatalf("root after the person's park = %#v, want its tree lingering", got)
				}
				assertSlots(t, pool, nil)
				return
			}
			last := tc.steps[len(tc.steps)-1]
			if got.Phase != wantPhase || got.LingerUntil != nil || got.Status != wantStatus || got.LastDispatchSeq != int64(len(tc.steps)+2) || got.Title != last.title {
				t.Fatalf("root after the events = %#v, want phase %s, not lingering, status %s kept, and the last event's seq and title", got, wantPhase, wantStatus)
			}
			assertSlots(t, pool, []record.Slot{{Issue: "LEGION-1", Index: 0, AdmittedAt: fixedNow}})
		})
	}
}

// An edit Dispatch sent while it still showed an outside backlog is the same edit whenever it is
// applied: before the daemon's set-back has finished, or after — the consumer lagging, or the event
// redelivered after a nak. Either way it changes no status, so the tree runs on and the architect is
// told of the one outside write once, whoever made the edit.
func TestAnEditReachesTheSameOutcomeWhetherOrNotTheSetBackHasFinished(t *testing.T) {
	for _, editor := range []struct{ name, actor string }{{"the outside session", "ses-outsider"}, {"a person", ""}} {
		t.Run("edited by "+editor.name, func(t *testing.T) {
			type outcome struct {
				root    record.Issue
				slots   []record.Slot
				notices int
			}
			run := func(finished bool) outcome {
				pool := migratedPool(t)
				admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
				seedSlotted(t, pool, "LEGION-1", "A")
				event := func(seq int64, actor, status, title string) {
					apply(t, pool, admission, "event-"+strconv.FormatInt(seq, 10), intake.DispatchIssue{Key: "LEGION-1", Seq: seq, Type: "issue.updated", Status: status,
						Title: title, Rank: "A", HandedOver: true, ActorSession: actor}, admission.engine)
				}
				event(2, "", "in_progress", "LEGION-1")
				event(3, "ses-outsider", "backlog", "LEGION-1")
				if finished {
					if _, err := pool.Exec(context.Background(), "delete from outbox where kind = 'dispatch_status'"); err != nil {
						t.Fatalf("finish the set-back: %v", err)
					}
				}
				event(4, editor.actor, "backlog", "renamed")
				got := outcome{root: issue(t, pool, "LEGION-1"), slots: slots(t, pool)}
				for _, effect := range effects(t, pool) {
					if notice, ok := effect.payload.(record.Notice); ok && notice.Kind == "status-reasserted" {
						got.notices++
					}
				}
				return got
			}
			queued, finished := run(false), run(true)
			if queued.root.Phase != phase.Admitted || queued.root.LingerUntil != nil || queued.root.Status != "in_progress" || queued.notices != 1 {
				t.Fatalf("with the set-back queued, the edit left %+v, want the tree running at in_progress and one notice", queued)
			}
			if !reflect.DeepEqual(finished, queued) {
				t.Fatalf("with the set-back finished, the edit left %+v; with it queued, %+v: want the same", finished, queued)
			}
		})
	}
}

// A person parks a tree the daemon has just admitted, while the promotion's in_progress is still on
// its way to Dispatch, and sets it back to todo: that todo changes Dispatch's status from the backlog
// before it, so it re-admits the tree, and the promotion's write, queued over the todo Dispatch showed
// then, does not land over the park.
func TestAParkAndATodoWhileThePromotionsWriteIsOnItsWayRunTheTreeAgain(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	putIssue(t, pool, record.Issue{Key: "LEGION-1", Project: testProject, Title: "LEGION-1", Tree: "LEGION-1", Phase: phase.Admitted, Generation: 1, Status: "todo", Rank: "A", HandedOver: true, LastDispatchSeq: 1})
	write := func(seq int64, status string) {
		apply(t, pool, admission, "person-"+strconv.FormatInt(seq, 10), intake.DispatchIssue{Key: "LEGION-1", Seq: seq, Type: "issue.updated", Status: status, Title: "LEGION-1", Rank: "A", HandedOver: true}, engine)
	}
	write(2, "todo")
	if got := issue(t, pool, "LEGION-1"); got.Status != "in_progress" || got.Generation != 1 {
		t.Fatalf("root after promotion = %#v, want generation 1 promoted to in_progress", got)
	}
	write(3, "backlog")
	write(4, "todo")
	got := issue(t, pool, "LEGION-1")
	if got.Generation != 2 || got.LingerUntil != nil || got.Phase != phase.Admitted {
		t.Fatalf("root after the park and todo = %#v, want generation 2 re-admitted and not lingering", got)
	}
	var writes []record.StatusWrite
	for _, effect := range effects(t, pool) {
		if payload, ok := effect.payload.(record.StatusWrite); ok {
			writes = append(writes, payload)
		}
	}
	if want := []record.StatusWrite{{Status: "in_progress", ObservedStatus: "todo"}}; !reflect.DeepEqual(writes, want) {
		t.Fatalf("queued status writes %+v, want only the new generation's promotion %+v", writes, want)
	}
}
