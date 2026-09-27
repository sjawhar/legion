package workflow

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A root architect whose human decided no change at the design gate ends its tree while the root
// is admitted: the daemon writes done to Dispatch with the architect's reason, which the write
// posts first, and the tree lingers as any close does. The journal follows the tree: the close with
// its status, the linger armed, and, once it expires, the linger's close. The Dispatch event the
// daemon's own write brings back changes nothing and logs nothing more.
func TestAnArchitectClosesItsAdmittedRootBeforeItsFirstPhase(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-1", Tree: "LEGION-1", Project: "LEGION", Title: "Root", Phase: phase.Admitted, Generation: 2, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	var logged bytes.Buffer
	engine := testEngine(config.DesignGateRootIssues, slog.New(slog.NewTextHandler(&logged, nil)))
	apply := func(id string, fact intake.Fact) intake.Result {
		t.Helper()
		result, err := intake.ApplyFact(context.Background(), pool, "api", id, fact, engine, admissionStub{})
		if err != nil {
			t.Fatalf("apply %s: %v", id, err)
		}
		return result
	}

	if result := apply("close", intake.CloseRoot{Issue: "LEGION-1", Reason: "the human decided no change is needed"}); result.Refusal != nil {
		t.Fatalf("close refused: %+v", result.Refusal)
	}
	var status string
	var rootPhase phase.Phase
	var until *time.Time
	if err := pool.QueryRow(context.Background(), "select status, phase, linger_until from issues where key = 'LEGION-1'").Scan(&status, &rootPhase, &until); err != nil {
		t.Fatalf("read the root: %v", err)
	}
	if status != "done" || rootPhase != phase.Done || until == nil {
		t.Fatalf("root status=%s phase=%s linger=%v; want done, parked in done, lingering", status, rootPhase, until)
	}
	var write record.StatusWrite
	if err := pool.QueryRow(context.Background(), "select payload->>'status', payload->>'observedStatus', payload->>'reason' from outbox where kind = 'dispatch_status'").
		Scan(&write.Status, &write.ObservedStatus, &write.Reason); err != nil {
		t.Fatalf("read the status write: %v", err)
	}
	if write != (record.StatusWrite{Status: "done", ObservedStatus: "in_progress", Reason: "Closed by its architect before its first phase: the human decided no change is needed"}) {
		t.Fatalf("status write %+v; want done over in_progress with the architect's reason", write)
	}
	assertOutboxCount(t, pool, "linger_close", 1)
	var reasons []string
	rows, err := pool.Query(context.Background(), "select distinct payload->>'reason' from outbox where kind = 'supervise' and payload->>'op' = 'suspend'")
	if err != nil {
		t.Fatalf("read the suspends: %v", err)
	}
	if reasons, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil || fmt.Sprint(reasons) != "[the tree of LEGION-1 lingers]" {
		t.Fatalf("suspend reasons %q, %v; want the linger's", reasons, err)
	}

	apply("done-event", intake.DispatchIssue{Key: "LEGION-1", Seq: 2, Type: "issue.closed", Status: "done", Title: "Root", Rank: "U"})
	apply("linger", intake.LingerExpired{Issue: "LEGION-1", Generation: 2})
	want := []string{
		`msg="workflow: issue left the workflow" issue=LEGION-1 tree=LEGION-1 status=done`,
		`msg="workflow: tree lingers" tree=LEGION-1 generation=2 until=2026-09-23T01:00:00.000Z`,
		`msg="workflow: linger closed the tree" tree=LEGION-1 generation=2 issues=1`,
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
		got = append(got, line[strings.Index(line, "msg="):])
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("logged\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// close_root ends a tree only while its root is admitted and no phase has started, and only a root:
// past that the tree ends through its workflow or a human, and a child leaves with park_child.
func TestCloseRootIsRefusedOnceATreeHasStartedOrForAChild(t *testing.T) {
	for _, tc := range []struct {
		name  string
		issue record.Issue
		code  string
	}{
		{"a root in planning", record.Issue{Key: "LEGION-1", Tree: "LEGION-1", Phase: phase.Planning, Status: "in_progress"}, "CLOSE_AFTER_PHASE_STARTED"},
		{"a root already lingering", record.Issue{Key: "LEGION-1", Tree: "LEGION-1", Phase: phase.Admitted, Status: "done", LingerUntil: new(time.Date(2026, 9, 23, 1, 0, 0, 0, time.UTC))}, "CLOSE_AFTER_PHASE_STARTED"},
		{"a root moved to backlog", record.Issue{Key: "LEGION-1", Tree: "LEGION-1", Phase: phase.Admitted, Status: "backlog"}, "CLOSE_AFTER_PHASE_STARTED"},
		{"a child", record.Issue{Key: "LEGION-2", Tree: "LEGION-1", Parent: new("LEGION-1"), Phase: phase.Admitted, Status: "in_progress"}, "ROOT_REQUIRED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			if tc.issue.Key != "LEGION-1" {
				seedIssue(t, pool, record.Issue{Key: "LEGION-1", Tree: "LEGION-1", Project: "LEGION", Title: "Root", Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "U"})
			}
			tc.issue.Project, tc.issue.Title, tc.issue.Generation, tc.issue.Rank = "LEGION", tc.issue.Key, 1, "V"
			seedIssue(t, pool, tc.issue)
			result, err := intake.ApplyFact(context.Background(), pool, "api", "close", intake.CloseRoot{Issue: tc.issue.Key, Reason: "moot"}, testEngine(config.DesignGateRootIssues, nil), admissionStub{})
			if err != nil || result.Refusal == nil || result.Refusal.Code != tc.code {
				t.Fatalf("close = %+v, %v; want refused %s", result.Refusal, err, tc.code)
			}
			assertOutboxCount(t, pool, "dispatch_status", 0)
			assertOutboxCount(t, pool, "linger_close", 0)
		})
	}
}

// Every phase change is one journal line once its fact commits: the issue, its tree, the phases it
// moved between, the trigger, and the status it now holds. Here the design gate is off, so the
// architect's registration opens it and the admitted root moves to planning.
func TestAPhaseChangeIsOneJournalLine(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-1", Tree: "LEGION-1", Project: "LEGION", Title: "Root", Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "U"})
	var logged bytes.Buffer
	engine := testEngine(config.DesignGateOff, slog.New(slog.NewTextHandler(&logged, nil)))
	if _, err := intake.ApplyFact(context.Background(), pool, "api", "gate", intake.GateRegistered{Issue: "LEGION-1", ArtifactID: "art-1", Version: 1}, engine, admissionStub{}); err != nil {
		t.Fatalf("register the gate: %v", err)
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
		if strings.Contains(line, `msg="workflow: phase changed"`) {
			got = append(got, line[strings.Index(line, "msg="):])
		}
	}
	if want := `msg="workflow: phase changed" issue=LEGION-1 tree=LEGION-1 from=admitted to=planning trigger=gate_opened status=in_progress`; fmt.Sprint(got) != "["+want+"]" {
		t.Fatalf("phase lines %q, want %q", got, want)
	}
}
