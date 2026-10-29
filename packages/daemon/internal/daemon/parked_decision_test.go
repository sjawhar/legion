package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workspace"
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

// A decision kept for a root is forgotten when, and only when, the architect start task that named
// it reaches its claim: a start that finishes without acting (here one of an earlier generation)
// leaves it for the next start to name, and a decision kept on a named reference since the naming
// outlives the delivery, since the forget names the row it read, not the reference.
func TestOnlyADeliveredStartForgetsTheDecisionsItNamed(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-1", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: "LEGION-1", Title: "Root", Phase: phase.Admitted, Generation: 2, Status: "in_progress"}
	putOutboxIssue(t, pool, records, issue)
	spec, at := "dispatch://LEGION-1/spec", time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	kept := record.ParkedDecision{Root: "LEGION-1", Ref: spec, Decision: record.DecisionDesignApproved, Actor: "user:alice", At: at}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error { return records.AddParkedDecision(ctx, tx, kept) }); err != nil {
		t.Fatalf("keep the decision: %v", err)
	}
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup,
		tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), now: time.Now,
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-1"}, nil
		},
	}
	start := func(generation uint64, id int64) record.OutboxRow {
		row := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleArchitect, Generation: generation,
			Task: "a person decided on the tree", Kept: &record.KeptDecisions{Decisions: []record.DecisionMark{{Ref: spec, At: at}}}}, time.Now())
		row.ID = id
		return row
	}
	if err := runner.execute(ctx, start(1, 57)); err != nil {
		t.Fatalf("the start of an earlier generation: %v", err)
	}
	if left := keptDecisions(t, pool, records, "LEGION-1"); len(left) != 1 {
		t.Fatalf("kept after a start that finished without acting = %#v, want the decision left for the next start", left)
	}
	// A person decides again on the reference the start named, before that start is delivered.
	newer := record.ParkedDecision{Root: "LEGION-1", Ref: spec, Decision: record.DecisionDesignChangesRequested, Actor: "user:bob", At: at.Add(time.Hour)}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error { return records.AddParkedDecision(ctx, tx, newer) }); err != nil {
		t.Fatalf("keep the newer decision: %v", err)
	}
	if err := runner.execute(ctx, start(2, 58)); err != nil {
		t.Fatalf("the start of this generation: %v", err)
	}
	token, err := claim.NewToken("legion", issue.Key, claim.RoleArchitect)
	if err != nil {
		t.Fatalf("claim token: %v", err)
	}
	machine, found := sup.Machine(token)
	if !found {
		t.Fatal("the start created no architect claim")
	}
	if pending := machine.Claim().Pending; pending == nil || pending.ID != "outbox:58" {
		t.Fatalf("the architect's pending task = %#v, want the start's delivery", pending)
	}
	left := keptDecisions(t, pool, records, "LEGION-1")
	if len(left) != 1 || left[0].Actor != "user:bob" || !left[0].At.Equal(newer.At) {
		t.Fatalf("kept after the delivered start = %#v, want only the decision kept since it was named", left)
	}
}

// A catch-up forgets the decisions it named when it is published to its architect, and leaves them
// when it finishes undelivered, so the next catch-up names them again.
func TestOnlyADeliveredCatchUpForgetsTheDecisionsItNamed(t *testing.T) {
	spec, at := "dispatch://LEGION-1/spec", time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		state     supervise.ClaimState
		absent    bool
		stillKept int
	}{
		{"published to its architect", supervise.StateWorking, false, 0},
		{"finished undelivered: its architect will not hold its role again", supervise.StateFailed, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			noticeTree(t, pool, records, false)
			if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
				return records.AddParkedDecision(ctx, tx, record.ParkedDecision{Root: "LEGION-1", Ref: spec,
					Decision: record.DecisionDesignApproved, Actor: "user:alice", At: at})
			}); err != nil {
				t.Fatalf("keep the decision: %v", err)
			}
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			architectClaim(t, sup, "LEGION-1", tc.state)
			publisher := &holderPublisher{}
			if tc.absent {
				publisher.setAbsent(architectTopic(t, "LEGION-1"))
			}
			runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion", now: time.Now}
			catchUp := record.Notice{Kind: "catch-up", Role: claim.RoleArchitect, Reason: "the tree of LEGION-1", CatchUp: &record.CatchUp{
				Generation: 1, Launch: 1, Decided: "a person decided on the tree",
				Kept: &record.KeptDecisions{Decisions: []record.DecisionMark{{Ref: spec, At: at}}}}}
			row := leasedOutboxRow(t, pool, records, mustOutboxRow(t, "LEGION-1", catchUp, time.Now()))
			if err := runner.notice(ctx, row, catchUp); err != nil {
				t.Fatalf("run the catch-up: %v", err)
			}
			if left := keptDecisions(t, pool, records, "LEGION-1"); len(left) != tc.stillKept {
				t.Fatalf("kept after the catch-up %s = %#v, want %d", tc.name, left, tc.stillKept)
			}
		})
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

// keptDecisions is what root's next start or catch-up would name (Store.ParkedDecisions).
func keptDecisions(t *testing.T, pool *pgxpool.Pool, records record.Store, root string) []record.ParkedDecision {
	t.Helper()
	ctx := context.Background()
	var kept []record.ParkedDecision
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		kept, err = records.ParkedDecisions(ctx, tx, root)
		return err
	}); err != nil {
		t.Fatalf("read the kept decisions of %s: %v", root, err)
	}
	return kept
}
