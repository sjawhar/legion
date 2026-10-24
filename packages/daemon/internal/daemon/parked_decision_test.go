package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A person's spec decision that nobody inside its tree will hear goes to the controller: one
// parked-decision notice however often the undelivered row runs (its finish not committing),
// nothing to the architect, and the decision kept for the root's next admission. The three cases
// reach both undelivered branches: a stopped tree's suspended architect (the first), and a role
// publish refused for an architect nobody will hold (the second, a lingering tree; the third, a
// failed claim on a running tree).
func TestASpecDecisionNobodyInItsTreeHearsGoesToTheController(t *testing.T) {
	carried := "a human approved LEGION-9's spec document at version 3, which opens the gate LEGION-1 carried; any READY it refused is released and any child waiting on it starts"
	for _, tc := range []struct {
		name    string
		state   supervise.ClaimState
		lingers bool
		absent  bool
	}{
		{"the close suspended the architect", supervise.StateSuspended, true, false},
		{"the tree lingers before its architect's suspend ran", supervise.StateWorking, true, true},
		{"the architect's claim failed on a running tree", supervise.StateFailed, false, true},
	} {
		for _, decision := range []record.Notice{
			{Kind: "design-approved", Version: 3, Actor: "user:alice", Ref: "dispatch://LEGION-1/spec"},
			{Kind: "design-changes-requested", Version: 3, Reason: "name the rollback path", Actor: "user:alice", Ref: "dispatch://LEGION-1/spec"},
			{Kind: "carried-gate-opened", Version: 3, Reason: carried, Actor: "user:alice", Ref: "dispatch://LEGION-9/spec"},
		} {
			t.Run(tc.name+"/"+string(decision.Kind), func(t *testing.T) {
				ctx := context.Background()
				pool := isolatedOutboxPool(t)
				records := record.NewStore()
				noticeTree(t, pool, records, tc.lingers)
				sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
				architectClaim(t, sup, "LEGION-1", tc.state)
				publisher := &holderPublisher{}
				if tc.absent {
					publisher.setAbsent(architectTopic(t, "LEGION-1"))
				}
				runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion", now: time.Now}
				row := leasedOutboxRow(t, pool, records, mustOutboxRow(t, "LEGION-1", decision, time.Now()))
				for attempt := range 2 {
					if err := runner.notice(ctx, row, decision); err != nil {
						t.Fatalf("attempt %d: %v", attempt, err)
					}
				}
				if _, delivered := publisher.snapshot(); len(delivered) != 0 {
					t.Fatalf("delivered %+v, want nothing to the architect", delivered)
				}
				kind, _ := record.SpecDecision(decision.Kind)
				want := record.ControllerNotice{Kind: record.ParkedDecisionNotice, Decision: kind, Root: "LEGION-1",
					Ref: decision.Ref, Actor: "user:alice", Version: 3, Reason: decision.Reason}
				if got := parkedDecisions(t, pool); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
					t.Fatalf("parked decisions %#v, want one %#v", got, want)
				}
				if kept := keptDecisions(t, pool, records, "LEGION-1"); len(kept) != 1 || kept[0].Ref != decision.Ref || kept[0].Decision != kind || kept[0].Actor != "user:alice" {
					t.Fatalf("kept %#v, want the decision kept for LEGION-1's next admission", kept)
				}
			})
		}
	}
}

// A notice of the machinery to an architect that stopped with its finished tree (a child's status,
// here) stays dropped with its one line: no person decided anything, so the controller is told
// nothing and nothing is kept for the root.
func TestTheMachinerysNoticeToAStoppedTreeWritesNoParkedDecision(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, true)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateSuspended)
	var logged bytes.Buffer
	publisher := &holderPublisher{}
	runner := &outbox{log: slog.New(slog.NewTextHandler(&logged, nil)), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion", now: time.Now}
	notice := record.Notice{Kind: "child-status", Role: claim.RoleArchitect, Reason: "LEGION-2 is backlog"}
	row := leasedOutboxRow(t, pool, records, mustOutboxRow(t, "LEGION-2", notice, time.Now()))
	if err := runner.notice(ctx, row, notice); err != nil {
		t.Fatalf("notice: %v", err)
	}
	if got := parkedDecisions(t, pool); len(got) != 0 {
		t.Fatalf("parked decisions %#v, want none for the machinery's notice", got)
	}
	if kept := keptDecisions(t, pool, records, "LEGION-1"); len(kept) != 0 {
		t.Fatalf("kept %#v, want nothing kept for the machinery's notice", kept)
	}
	if strings.Count(logged.String(), `msg="outbox notice finished undelivered: its architect stopped with its finished tree"`) != 1 ||
		strings.Contains(logged.String(), "parked decision") {
		t.Fatalf("log %q; want one finished-undelivered line and no parked decision line", logged.String())
	}
}

// A spec decision that names no person relays nothing: gates.design off opened the gate, or the
// release before this one queued the notice, which then carries no ref either. The log line names
// the notice's ref, empty for the old release's notice, which is how A5 tells the two apart.
func TestASpecDecisionThatNamesNoPersonIsNotRelayed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		notice record.Notice
		ref    string
	}{
		{"gates.design off opened the gate", record.Notice{Kind: "design-approved", Version: 3, Ref: "dispatch://LEGION-1/spec"}, "ref=dispatch://LEGION-1/spec"},
		{"the release before this one queued it", record.Notice{Kind: "design-approved", Version: 3}, `ref=""`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			noticeTree(t, pool, records, true)
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			architectClaim(t, sup, "LEGION-1", supervise.StateSuspended)
			var logged bytes.Buffer
			publisher := &holderPublisher{}
			runner := &outbox{log: slog.New(slog.NewTextHandler(&logged, nil)), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion", now: time.Now}
			row := leasedOutboxRow(t, pool, records, mustOutboxRow(t, "LEGION-1", tc.notice, time.Now()))
			if err := runner.notice(ctx, row, tc.notice); err != nil {
				t.Fatalf("notice: %v", err)
			}
			if got := parkedDecisions(t, pool); len(got) != 0 {
				t.Fatalf("parked decisions %#v, want none: no person made the decision", got)
			}
			if kept := keptDecisions(t, pool, records, "LEGION-1"); len(kept) != 0 {
				t.Fatalf("kept %#v, want nothing kept", kept)
			}
			var line string
			for candidate := range strings.SplitSeq(logged.String(), "\n") {
				if strings.Contains(candidate, `msg="outbox parked decision not relayed: it names no person"`) {
					line = candidate
				}
			}
			if line == "" || !strings.Contains(line, tc.ref) || !strings.Contains(line, "root=LEGION-1") || !strings.Contains(line, "key=legion-outbox:") {
				t.Fatalf("log %q; want the not-relayed line naming %s, the root and the key", logged.String(), tc.ref)
			}
		})
	}
}

// The parked-decision notice goes where every controller notice goes: the outbox's next run
// publishes it on the project's controller topic, as "parked-decision on <issue>".
func TestAParkedDecisionIsPublishedOnTheControllerTopic(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, true)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateSuspended)
	publisher := &holderPublisher{}
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion", now: time.Now}
	decision := record.Notice{Kind: "design-approved", Version: 3, Actor: "user:alice", Ref: "dispatch://LEGION-1/spec"}
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-1", decision, time.Now()))
	for run := range 2 {
		if err := runner.RunOnce(ctx); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
	}
	_, delivered := publisher.snapshot()
	want := record.Notice{Kind: record.ParkedDecisionNotice, Decision: record.DecisionDesignApproved, Root: "LEGION-1", Ref: "dispatch://LEGION-1/spec", Actor: "user:alice", Version: 3}
	if len(delivered) != 1 || delivered[0].topic != notify.ControllerTopic("legion") || delivered[0].message != "parked-decision on LEGION-1" ||
		!reflect.DeepEqual(delivered[0].payload, want) {
		t.Fatalf("delivered %+v, want one %#v on %s as parked-decision on LEGION-1", delivered, want, notify.ControllerTopic("legion"))
	}
}

// parkedDecisions is every parked-decision controller notice in the outbox, oldest first.
func parkedDecisions(t *testing.T, pool *pgxpool.Pool) []record.ControllerNotice {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select id, kind, issue, payload from outbox
		where kind = $1 and payload->>'kind' = $2 order by id`, string(record.OutboxKindControllerNotice), string(record.ParkedDecisionNotice))
	if err != nil {
		t.Fatalf("read the parked decisions: %v", err)
	}
	defer rows.Close()
	var got []record.ControllerNotice
	for rows.Next() {
		var row record.OutboxRow
		if err := rows.Scan(&row.ID, &row.Kind, &row.Issue, &row.Payload); err != nil {
			t.Fatalf("scan: %v", err)
		}
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		got = append(got, payload.(record.ControllerNotice))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the parked decisions: %v", err)
	}
	return got
}

// keptDecisions is what root's next admission would take (Store.TakeParkedDecisions), read in a
// transaction rolled back afterwards, so the rows stay.
func keptDecisions(t *testing.T, pool *pgxpool.Pool, records record.Store, root string) []record.ParkedDecision {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	kept, err := records.TakeParkedDecisions(ctx, tx, root)
	if err != nil {
		t.Fatalf("take the kept decisions of %s: %v", root, err)
	}
	return kept
}
