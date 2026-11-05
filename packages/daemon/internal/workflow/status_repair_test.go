package workflow

import (
	"bytes"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A status write the daemon lost leaves Dispatch showing todo while Legion implements the issue.
// The controller tick's repair queues that write again, with no restart in between, and moves the
// record's own status with it. The status Dispatch last showed stays where it is: only a Dispatch
// event or admission's own observation writes that, once the queued write echoes back.
func TestTheStatusRepairQueuesTheStatusWriteTheDaemonLost(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, repairIssue("LEGION-208", "LEGION-208", phase.Implementing, "todo", "todo"))
	seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0})

	applyRefusingNothing(t, pool, testEngine(config.DesignGateRootIssues, nil), intake.ControllerTick{})

	assertQueuedStatusWrites(t, pool, statusRow{"LEGION-208", "in_progress", "todo"})
	got := issueRecord(t, pool, "LEGION-208")
	if got.Status != "in_progress" || got.DispatchStatus != "todo" {
		t.Fatalf("the repaired record = %+v, want status in_progress and the todo Dispatch last showed", got)
	}
}

// The row carries the status of the phase the issue stands in, not the status its record holds. A
// person moved this issue from needs_review to in_progress, and the daemon recorded that move in
// both the record's own status and the status Dispatch showed; the issue still stands in
// reviewing, so the repair writes needs_review. Engine.status would have queued nothing here,
// since the record's own status already reads in_progress, which is why the repair enqueues its
// row directly.
func TestTheStatusRepairWritesThePhasesStatusAndNotTheRecords(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, repairIssue("LEGION-208", "LEGION-208", phase.Reviewing, "in_progress", "in_progress"))
	seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0})

	applyRefusingNothing(t, pool, testEngine(config.DesignGateRootIssues, nil), intake.ControllerTick{})

	assertQueuedStatusWrites(t, pool, statusRow{"LEGION-208", "needs_review", "in_progress"})
}

// One case per rule the repair skips an issue by. Each of these issues differs from the status of
// its phase, and each is left alone.
func TestTheStatusRepairSkipsEveryIssueItsRulesExclude(t *testing.T) {
	lingerUntil := time.Date(2026, 9, 23, 1, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		seed func(*testing.T, *pgxpool.Pool)
		want []statusRow
	}{
		{name: "an issue of another project", seed: func(t *testing.T, pool *pgxpool.Pool) {
			other := repairIssue("OTHER-1", "OTHER-1", phase.Implementing, "todo", "todo")
			other.Project = "OTHER"
			seedIssue(t, pool, other)
			seedSlot(t, pool, record.Slot{Issue: "OTHER-1", Index: 0})
		}},
		{name: "an issue a person parked, which the daemon no longer works", seed: func(t *testing.T, pool *pgxpool.Pool) {
			seedIssue(t, pool, repairIssue("LEGION-208", "LEGION-208", phase.Implementing, "backlog", "backlog"))
			seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0})
		}},
		{name: "an issue a merge closed", seed: func(t *testing.T, pool *pgxpool.Pool) {
			seedIssue(t, pool, repairIssue("LEGION-208", "LEGION-208", phase.Done, "done", "done"))
			seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0})
		}},
		// Admitted and held are the phases an issue waits in rather than works in, so each keeps
		// the status it already shows and PhaseStatus gives it none of its own.
		{name: "an issue waiting at admitted", seed: func(t *testing.T, pool *pgxpool.Pool) {
			seedIssue(t, pool, repairIssue("LEGION-208", "LEGION-208", phase.Admitted, "todo", "todo"))
			seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0})
		}},
		{name: "an issue held", seed: func(t *testing.T, pool *pgxpool.Pool) {
			held := repairIssue("LEGION-208", "LEGION-208", phase.Held, "in_progress", "todo")
			held.Hold = &record.Hold{From: phase.Implementing, Reason: record.HoldEscalated}
			seedIssue(t, pool, held)
			seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0})
		}},
		// PhaseStatus(phase.Done) is "done", so the phase rule alone would queue a done write for
		// this issue. InWorkflow is the whole protection for it: an issue a person reopened under
		// a record the daemon already closed stands exactly this way, at phase done with a status
		// of the workflow, and six issues of the live deployment stand there now.
		{name: "an issue reopened under a closed record, its root slotted", seed: func(t *testing.T, pool *pgxpool.Pool) {
			seedIssue(t, pool, repairIssue("LEGION-208", "LEGION-208", phase.Done, "in_progress", "in_progress"))
			seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0})
		}},
		{name: "an issue whose tree's root holds no slot", seed: func(t *testing.T, pool *pgxpool.Pool) {
			seedIssue(t, pool, repairIssue("LEGION-208", "LEGION-208", phase.Implementing, "in_progress", "todo"))
		}},
		// The lingering root is seeded as the daemon leaves one: beginLinger sets the root's phase
		// to done and arms its deadline. The member is the case the rule exists for, since for the
		// root itself the linger rule and the phase rule agree.
		{name: "an issue whose tree's root lingers", seed: func(t *testing.T, pool *pgxpool.Pool) {
			root := repairIssue("LEGION-208", "LEGION-208", phase.Done, "done", "done")
			root.LingerUntil = &lingerUntil
			seedIssue(t, pool, root)
			seedIssue(t, pool, repairIssue("LEGION-209", "LEGION-208", phase.Implementing, "in_progress", "todo"))
			seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0})
		}},
		{name: "an issue whose status write is already queued", seed: func(t *testing.T, pool *pgxpool.Pool) {
			seedIssue(t, pool, repairIssue("LEGION-208", "LEGION-208", phase.Implementing, "in_progress", "todo"))
			seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0})
			seedStatusWrite(t, pool, "LEGION-208", record.OrdinaryStatusWrite("in_progress", "todo", ""))
		}, want: []statusRow{{"LEGION-208", "in_progress", "todo"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			tc.seed(t, pool)

			applyRefusingNothing(t, pool, testEngine(config.DesignGateRootIssues, nil), intake.ControllerTick{})

			assertQueuedStatusWrites(t, pool, tc.want...)
		})
	}
}

// A recorded Dispatch status out of the workflow is left alone whatever the phase says. The row it
// would queue could never be written: the outbox writes a status only where Dispatch shows the
// row's observed or target status, and a park the record holds is the park Dispatch shows.
func TestTheStatusRepairSkipsARecordedStatusOutOfTheWorkflow(t *testing.T) {
	for _, shown := range []string{"backlog", "icebox", "triage", "done"} {
		t.Run(shown, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, repairIssue("LEGION-208", "LEGION-208", phase.Implementing, "in_progress", shown))
			seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0})

			applyRefusingNothing(t, pool, testEngine(config.DesignGateRootIssues, nil), intake.ControllerTick{})

			assertQueuedStatusWrites(t, pool)
		})
	}
}

// The boot runs the repair too, so a write lost before a restart is queued again by the restart.
func TestTheBootQueuesTheStatusWriteTheDaemonLost(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, repairIssue("LEGION-208", "LEGION-208", phase.Testing, "testing", "in_progress"))
	seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0})

	applyRefusingNothing(t, pool, testEngine(config.DesignGateRootIssues, nil), intake.DaemonBooted{Boot: 1})

	assertQueuedStatusWrites(t, pool, statusRow{"LEGION-208", "testing", "in_progress"})
}

// The log is the only account of what the repair did: the daemon's state carries the record's own
// status, not the status Dispatch last showed, and the repair adds no field to it. So every run
// writes one line with its trigger, how many rows it queued, the issues it passed over and the
// issues whose write is already on its way, and each repair writes a line of its own naming the
// issue, the status recorded and the status written. A run that queues nothing writes its line
// too, which is how a reader tells a quiet run from a run that never happened.
func TestTheStatusRepairLogsEachRepairAndEveryRun(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, repairIssue("LEGION-208", "LEGION-208", phase.Implementing, "todo", "todo"))
	seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0})
	// Its root holds no slot, so it is passed over.
	seedIssue(t, pool, repairIssue("LEGION-300", "LEGION-300", phase.Implementing, "in_progress", "todo"))
	// Its write is already queued, so it waits for the outbox rather than for the repair.
	seedIssue(t, pool, repairIssue("LEGION-400", "LEGION-400", phase.Testing, "testing", "in_progress"))
	seedSlot(t, pool, record.Slot{Issue: "LEGION-400", Index: 1})
	seedStatusWrite(t, pool, "LEGION-400", record.OrdinaryStatusWrite("testing", "in_progress", ""))

	var logged bytes.Buffer
	engine := testEngine(config.DesignGateRootIssues, slog.New(slog.NewTextHandler(&logged, nil)))
	applyRefusingNothing(t, pool, engine, intake.ControllerTick{})

	for _, want := range []string{
		`msg="workflow: the status repair queues the status of the issue's phase" issue=LEGION-208 phase=implementing recorded=todo wrote=in_progress`,
		`msg="workflow: the status repair ran" trigger=tick queued=1 passedOver=[LEGION-300] pending=[LEGION-400]`,
	} {
		if !strings.Contains(logged.String(), want) {
			t.Fatalf("the first run logged %q, want a line with %q", logged.String(), want)
		}
	}

	// The second run queues nothing: the row the first run wrote for LEGION-208 is still
	// unfinished, so that issue joins the pending list. The run line is written all the same.
	logged.Reset()
	applyRefusingNothing(t, pool, engine, intake.ControllerTick{})

	want := `msg="workflow: the status repair ran" trigger=tick queued=0 passedOver=[LEGION-300] pending="[LEGION-208 LEGION-400]"`
	if !strings.Contains(logged.String(), want) {
		t.Fatalf("the second run logged %q, want a line with %q", logged.String(), want)
	}
	if strings.Contains(logged.String(), "the status repair queues") {
		t.Fatalf("the second run logged %q, want no repair line", logged.String())
	}
}

// repairIssue is one issue of the status repair's tests: key, a member of tree, standing in at,
// with the record's own status and the status Dispatch last showed.
func repairIssue(key, tree string, at phase.Phase, status, shown string) record.Issue {
	return record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: tree,
		Project: "LEGION", Title: key, Phase: at, Generation: 1, Status: status, DispatchStatus: shown,
		Rank: "U", HandedOver: true, LastDispatchSeq: 1}
}

// statusRow is one queued Dispatch status write, as the repair's tests read it back.
type statusRow struct{ issue, status, observed string }

// seedStatusWrite queues a Dispatch status write for issue, as a write the outbox has not finished.
func seedStatusWrite(t *testing.T, pool *pgxpool.Pool, issue string, write record.StatusWrite) {
	t.Helper()
	row, err := record.NewOutboxRow(issue, write, time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("build the queued status write of %s: %v", issue, err)
	}
	seedRecord(t, pool, func(tx pgx.Tx) error { return record.NewStore().Enqueue(t.Context(), tx, row) })
}

func assertQueuedStatusWrites(t *testing.T, pool *pgxpool.Pool, want ...statusRow) {
	t.Helper()
	rows, err := pool.Query(t.Context(), `select issue, payload->>'status', payload->>'observedStatus'
		from outbox where kind = $1 order by id`, string(record.OutboxKindDispatchStatus))
	if err != nil {
		t.Fatalf("read the queued status writes: %v", err)
	}
	defer rows.Close()
	got := []statusRow{}
	for rows.Next() {
		var row statusRow
		if err := rows.Scan(&row.issue, &row.status, &row.observed); err != nil {
			t.Fatalf("scan a queued status write: %v", err)
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the queued status writes: %v", err)
	}
	if !reflect.DeepEqual(got, append([]statusRow{}, want...)) {
		t.Fatalf("queued status writes = %+v, want %+v", got, want)
	}
}
