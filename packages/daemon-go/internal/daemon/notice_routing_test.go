package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// holderPublisher is the listener as a role publish meets it: a role topic in absent has no live
// holder, and the publish is refused as the listener refuses it (notify.ErrNoHolder); any other
// publish is delivered and kept, in order.
type holderPublisher struct {
	mu        sync.Mutex
	absent    map[string]bool
	attempts  int
	delivered []outboxPublish
}

func (p *holderPublisher) Publish(_ context.Context, topic, _ string, payload any, key string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.attempts++
	if p.absent[topic] {
		return fmt.Errorf("publish notice to %s: listener returned 404 Not Found: {\"reason\":\"unclaimed\"}: %w", topic, notify.ErrNoHolder)
	}
	p.delivered = append(p.delivered, outboxPublish{topic: topic, key: key, payload: payload})
	return nil
}

func (p *holderPublisher) setAbsent(topics ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.absent = map[string]bool{}
	for _, topic := range topics {
		p.absent[topic] = true
	}
}

// architectTopic is the role topic of issue's architect claim.
func architectTopic(t *testing.T, issue string) string {
	t.Helper()
	return roleTopicPrefix + string(mustClaimToken(t, issue, claim.RoleArchitect))
}

// deliveredKinds is each delivered notice as "<kind> to <architect's issue>", in order.
func deliveredKinds(t *testing.T, delivered []outboxPublish) []string {
	t.Helper()
	var got []string
	for _, published := range delivered {
		owner := "?"
		for _, issue := range []string{"LEGION-1", "LEGION-2", "LEGION-3"} {
			if published.topic == architectTopic(t, issue) {
				owner = issue
			}
		}
		got = append(got, string(published.payload.(record.Notice).Kind)+" to "+owner)
	}
	return got
}

func (p *holderPublisher) snapshot() (int, []outboxPublish) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.attempts, append([]outboxPublish(nil), p.delivered...)
}

// noticeTree records a root, its child and the child's child, all of tree LEGION-1.
func noticeTree(t *testing.T, pool *pgxpool.Pool, records record.Store, lingers bool) {
	t.Helper()
	root, child := "LEGION-1", "LEGION-2"
	rootIssue := record.Issue{Key: root, Project: "LEGION", Tree: root, Title: "Root", Phase: phase.Implementing, Generation: 1, Status: "in_progress"}
	if lingers {
		until := time.Now().Add(time.Hour)
		rootIssue.Phase, rootIssue.Status, rootIssue.LingerUntil = phase.Done, "done", &until
	}
	putOutboxIssue(t, pool, records, rootIssue)
	putOutboxIssue(t, pool, records, record.Issue{Key: child, Project: "LEGION", Tree: root, Parent: &root, Title: "Child", Phase: phase.Planning, Generation: 1, Status: "in_progress"})
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-3", Project: "LEGION", Tree: root, Parent: &child, Title: "Grandchild", Phase: phase.Testing, Generation: 1, Status: "testing"})
}

func architectClaim(t *testing.T, sup *supervisor, issue string, state supervise.ClaimState) claim.Token {
	t.Helper()
	token := mustClaimToken(t, issue, claim.RoleArchitect)
	if _, _, err := sup.Create(context.Background(), supervise.Claim{
		Token: token, Project: "legion", Tree: "LEGION-1", Issue: issue, Role: claim.RoleArchitect, State: state,
	}, ""); err != nil {
		t.Fatalf("create the architect claim of %s: %v", issue, err)
	}
	return token
}

func mustClaimToken(t *testing.T, issue string, role claim.Role) claim.Token {
	t.Helper()
	token, err := claim.NewToken("legion", issue, role)
	if err != nil {
		t.Fatalf("claim token of %s %s: %v", issue, role, err)
	}
	return token
}

// Every notice is architect-facing, so it goes to the architect that owns its issue, on that
// architect's own role topic, by the TypeScript daemon's rule (owningArchitect): the nearest issue
// at or above it whose architect claim runs (not suspended, failed or retired), else the tree root;
// a child's close or status change starts at its parent. No issue topic carries it, since every
// issue topic is a subject that issue's phase workers subscribe to.
func TestANoticeGoesToTheOwningArchitectsRoleTopicAlone(t *testing.T) {
	for _, tc := range []struct {
		name         string
		issue        string
		kind         record.NoticeKind
		subArchitect supervise.ClaimState
		owner        string
	}{
		{name: "a child's notice with no sub-architect", issue: "LEGION-2", owner: "LEGION-1"},
		{name: "the root's own notice", issue: "LEGION-1", owner: "LEGION-1"},
		{name: "a child owned by its sub-architect", issue: "LEGION-2", subArchitect: supervise.StateWorking, owner: "LEGION-2"},
		{name: "a grandchild under the sub-architect's child", issue: "LEGION-3", subArchitect: supervise.StateWorking, owner: "LEGION-2"},
		{name: "a child whose sub-architect retired", issue: "LEGION-2", subArchitect: supervise.StateRetired, owner: "LEGION-1"},
		{name: "a child whose sub-architect was suspended", issue: "LEGION-2", subArchitect: supervise.StateSuspended, owner: "LEGION-1"},
		{name: "a child's close under its running sub-architect", issue: "LEGION-2", kind: "child-closed", subArchitect: supervise.StateWorking, owner: "LEGION-1"},
		{name: "a child's status change under its running sub-architect", issue: "LEGION-2", kind: "child-status", subArchitect: supervise.StateWorking, owner: "LEGION-1"},
		{name: "a grandchild's close under the sub-architect's child", issue: "LEGION-3", kind: "child-closed", subArchitect: supervise.StateWorking, owner: "LEGION-2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			noticeTree(t, pool, records, false)
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
			if tc.subArchitect != "" {
				architectClaim(t, sup, "LEGION-2", tc.subArchitect)
			}
			publisher := &holderPublisher{}
			notice := record.Notice{Kind: "phase-finished", Role: claim.RolePlanner, Phase: phase.Planning}
			if tc.kind != "" {
				notice = record.Notice{Kind: tc.kind, Role: claim.RoleArchitect, Reason: tc.issue + " moved"}
			}
			row := mustOutboxRow(t, tc.issue, notice, time.Now())
			row.ID = 56

			runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion"}
			if err := runner.execute(context.Background(), row); err != nil {
				t.Fatalf("execute notice: %v", err)
			}
			want := roleTopicPrefix + string(mustClaimToken(t, tc.owner, claim.RoleArchitect))
			_, delivered := publisher.snapshot()
			if len(delivered) != 1 || delivered[0].topic != want || delivered[0].key != "legion-outbox:56" {
				t.Fatalf("notice publishes = %+v, want one to %s under the row's key", delivered, want)
			}
			// No phase worker of the root, the child or the grandchild holds the subject it went to:
			// neither an issue topic nor a worker's role topic.
			for _, issue := range []string{"LEGION-1", "LEGION-2", "LEGION-3"} {
				subjects := []string{notify.Topic("legion", issue)}
				for _, role := range []claim.Role{claim.RolePlanner, claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer, claim.RoleMerger} {
					subjects = append(subjects, roleTopicPrefix+string(mustClaimToken(t, issue, role)))
				}
				if slices.Contains(subjects, delivered[0].topic) {
					t.Fatalf("the notice went to %s, a subject a phase worker of %s holds", delivered[0].topic, issue)
				}
			}
		})
	}
}

// A role publish with no live holder is the ordinary state while the owning architect relaunches:
// its claim has not ended, so the notice is held and retried on the outbox's backoff, logged once
// for the row rather than on every attempt. The notices behind it for the same tree wait too, so
// once the architect holds its role again every held notice arrives, in the order it was written.
func TestANoticeHeldForAnAbsentArchitectArrivesInOrderOnceItReturns(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateLaunching)
	first := mustOutboxRow(t, "LEGION-2", record.Notice{Kind: "phase-finished", Role: claim.RolePlanner, Phase: phase.Planning}, time.Now())
	second := mustOutboxRow(t, "LEGION-3", record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "max_fix_attempts"}, time.Now())
	third := mustOutboxRow(t, "LEGION-1", record.Notice{Kind: "design-approved", Version: 2}, time.Now())
	for _, row := range []record.OutboxRow{first, second, third} {
		enqueueOutbox(t, pool, records, row)
	}
	var logged bytes.Buffer
	clock := time.Now()
	publisher := &holderPublisher{}
	publisher.setAbsent(architectTopic(t, "LEGION-1"))
	runner := &outbox{log: slog.New(slog.NewTextHandler(&logged, nil)), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup,
		project: "legion", now: func() time.Time { return clock }}

	for tick := range 3 {
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
		clock = clock.Add(2 * time.Minute)
	}
	attempts, delivered := publisher.snapshot()
	if len(delivered) != 0 || attempts != 3 || outboxRows(t, pool) != 3 {
		t.Fatalf("while nobody holds the architect's role: %d attempts, delivered %+v, %d rows; want the first notice tried each tick, none delivered, all three kept",
			attempts, delivered, outboxRows(t, pool))
	}
	for _, row := range []int{1, 2, 3} {
		if got := strings.Count(logged.String(), fmt.Sprintf("msg=\"outbox notice waits for its architect\" row=%d ", row)); got != 1 {
			t.Fatalf("held notice row %d was logged %d times over 3 ticks, want once:\n%s", row, got, logged.String())
		}
	}

	publisher.setAbsent()
	for tick := range 3 {
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("tick %d after the architect returned: %v", tick, err)
		}
		clock = clock.Add(2 * time.Minute)
	}
	_, delivered = publisher.snapshot()
	if got := deliveredKinds(t, delivered); fmt.Sprint(got) != "[phase-finished to LEGION-1 pr-blocked to LEGION-1 design-approved to LEGION-1]" || outboxRows(t, pool) != 0 {
		t.Fatalf("after the architect returned: delivered %v with %d rows left, want the three notices in the order written and none left", got, outboxRows(t, pool))
	}
}

// The order held is each architect's own: a notice held for the root architect while it relaunches
// does not hold a later notice for a sub-architect that is running, which is delivered at once.
func TestANoticeHeldForOneArchitectHoldsNoOtherArchitectsNotice(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateLaunching)
	architectClaim(t, sup, "LEGION-2", supervise.StateWorking)
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-1", record.Notice{Kind: "design-approved", Version: 2}, time.Now()))
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-3", record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "max_fix_attempts"}, time.Now()))
	publisher := &holderPublisher{}
	publisher.setAbsent(architectTopic(t, "LEGION-1"))
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion", now: time.Now}

	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the notices: %v", err)
	}
	_, delivered := publisher.snapshot()
	if got := deliveredKinds(t, delivered); fmt.Sprint(got) != "[pr-blocked to LEGION-2]" || outboxRows(t, pool) != 1 {
		t.Fatalf("delivered %v with %d rows left, want the sub-architect's pr-blocked delivered and the root's design-approved still held", got, outboxRows(t, pool))
	}
}

// A sub-architect the workflow suspended (a human closed or moved its child) is not coming back for
// a notice: nothing resumes it but re-admission. A notice that would reach it goes to the architect
// above instead, and so does the child's close, which starts at the parent; neither waits on the
// suspended claim, and neither holds up the tree's later notices.
func TestNoNoticeWaitsOnASuspendedSubArchitect(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
	architectClaim(t, sup, "LEGION-2", supervise.StateSuspended)
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-2", record.Notice{Kind: "phase-finished", Role: claim.RolePlanner, Phase: phase.Planning}, time.Now()))
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-2", record.Notice{Kind: "child-closed", Role: claim.RoleArchitect, Reason: "LEGION-2 is done"}, time.Now()))
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-1", record.Notice{Kind: "phase-finished", Role: claim.RoleImplementer, Phase: phase.Implementing}, time.Now()))
	publisher := &holderPublisher{}
	publisher.setAbsent(architectTopic(t, "LEGION-2"))
	clock := time.Now()
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion",
		now: func() time.Time { return clock }}

	for tick := range 20 {
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
		clock = clock.Add(2 * time.Minute)
	}
	_, delivered := publisher.snapshot()
	if got := deliveredKinds(t, delivered); fmt.Sprint(got) != "[phase-finished to LEGION-1 child-closed to LEGION-1 phase-finished to LEGION-1]" || outboxRows(t, pool) != 0 {
		t.Fatalf("after 20 ticks over 40 minutes: delivered %v with %d rows left, want all three at the root architect and none left", got, outboxRows(t, pool))
	}
}

// A notice whose owning architect has ended waits on nobody: a claim that retired or failed will
// not hold its role again, a suspended one only when re-admission or the operator starts it again,
// with the tree's record rather than the notices it missed, and a lingering tree's architect is
// suspended until re-admission. Such a row finishes
// without delivery, with one log line naming the notice kind and the issue.
func TestANoticeForAnEndedArchitectFinishesWithoutDelivery(t *testing.T) {
	for _, tc := range []struct {
		name    string
		state   supervise.ClaimState
		lingers bool
	}{
		{name: "the architect's claim retired", state: supervise.StateRetired},
		{name: "the architect's claim failed", state: supervise.StateFailed},
		{name: "the architect's claim was suspended", state: supervise.StateSuspended},
		// The linger's suspend of the architect is an outbox row of its own, which may not have run.
		{name: "the tree lingers before its architect's suspend ran", state: supervise.StateWorking, lingers: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			noticeTree(t, pool, records, tc.lingers)
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			architectClaim(t, sup, "LEGION-1", tc.state)
			enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-2", record.Notice{Kind: "child-closed", Role: claim.RoleArchitect, Reason: "LEGION-2 is done"}, time.Now()))
			var logged bytes.Buffer
			publisher := &holderPublisher{}
			publisher.setAbsent(architectTopic(t, "LEGION-1"))
			runner := &outbox{log: slog.New(slog.NewTextHandler(&logged, nil)), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup,
				project: "legion", now: time.Now}

			if err := runner.RunOnce(context.Background()); err != nil {
				t.Fatalf("run the notice: %v", err)
			}
			_, delivered := publisher.snapshot()
			lines := strings.Split(strings.TrimSpace(logged.String()), "\n")
			if len(delivered) != 0 || outboxRows(t, pool) != 0 || len(lines) != 1 || !strings.Contains(lines[0], "kind=child-closed") || !strings.Contains(lines[0], "issue=LEGION-2") {
				t.Fatalf("delivered %+v, %d rows left, log %q; want no delivery, the row finished, one line naming child-closed and LEGION-2", delivered, outboxRows(t, pool), logged.String())
			}
		})
	}
}
