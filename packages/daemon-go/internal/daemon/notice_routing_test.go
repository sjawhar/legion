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

// holderPublisher is the listener as a role publish meets it: while absent, every role topic has
// no live holder and the publish is refused as the listener refuses it (notify.ErrNoHolder); once
// present, the publish is delivered and kept, in order.
type holderPublisher struct {
	mu        sync.Mutex
	absent    bool
	attempts  int
	delivered []outboxPublish
}

func (p *holderPublisher) Publish(_ context.Context, topic, _ string, payload any, key string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.attempts++
	if p.absent && strings.HasPrefix(topic, roleTopicPrefix) {
		return fmt.Errorf("publish notice to %s: listener returned 404 Not Found: {\"reason\":\"unclaimed\"}: %w", topic, notify.ErrNoHolder)
	}
	p.delivered = append(p.delivered, outboxPublish{topic: topic, key: key, payload: payload})
	return nil
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
// at or above it whose architect claim has not ended, else the tree root. No issue topic carries
// it, since every issue topic is a subject that issue's phase workers subscribe to.
func TestANoticeGoesToTheOwningArchitectsRoleTopicAlone(t *testing.T) {
	for _, tc := range []struct {
		name         string
		issue        string
		subArchitect supervise.ClaimState
		owner        string
	}{
		{name: "a child's notice with no sub-architect", issue: "LEGION-2", owner: "LEGION-1"},
		{name: "the root's own notice", issue: "LEGION-1", owner: "LEGION-1"},
		{name: "a child owned by its sub-architect", issue: "LEGION-2", subArchitect: supervise.StateWorking, owner: "LEGION-2"},
		{name: "a grandchild under the sub-architect's child", issue: "LEGION-3", subArchitect: supervise.StateWorking, owner: "LEGION-2"},
		{name: "a child whose sub-architect retired", issue: "LEGION-2", subArchitect: supervise.StateRetired, owner: "LEGION-1"},
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
			row := mustOutboxRow(t, tc.issue, record.Notice{Kind: "phase-finished", Role: claim.RolePlanner, Phase: phase.Planning}, time.Now())
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
	publisher := &holderPublisher{absent: true}
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
	if got := strings.Count(logged.String(), "msg=\"outbox notice waits for its architect"); got != 1 {
		t.Fatalf("the held notice was logged %d times over 3 attempts, want once:\n%s", got, logged.String())
	}

	publisher.mu.Lock()
	publisher.absent = false
	publisher.mu.Unlock()
	for tick := range 3 {
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("tick %d after the architect returned: %v", tick, err)
		}
		clock = clock.Add(2 * time.Minute)
	}
	_, delivered = publisher.snapshot()
	var kinds []string
	for _, published := range delivered {
		kinds = append(kinds, string(published.payload.(record.Notice).Kind))
	}
	if fmt.Sprint(kinds) != "[phase-finished pr-blocked design-approved]" || outboxRows(t, pool) != 0 {
		t.Fatalf("after the architect returned: delivered %v with %d rows left, want the three notices in the order written and none left", kinds, outboxRows(t, pool))
	}
}

// A notice whose owning architect has ended waits on nobody: a claim that retired or failed will
// not hold its role again, and a lingering tree's architect is suspended until re-admission, which
// starts it with the tree's record rather than the notices of its close. Such a row finishes
// without delivery, with one log line naming the notice kind and the issue.
func TestANoticeForAnEndedArchitectFinishesWithoutDelivery(t *testing.T) {
	for _, tc := range []struct {
		name    string
		state   supervise.ClaimState
		lingers bool
	}{
		{name: "the architect's claim retired", state: supervise.StateRetired},
		{name: "the architect's claim failed", state: supervise.StateFailed},
		{name: "the tree lingers", state: supervise.StateSuspended, lingers: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			noticeTree(t, pool, records, tc.lingers)
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			architectClaim(t, sup, "LEGION-1", tc.state)
			enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-2", record.Notice{Kind: "child-closed", Role: claim.RoleArchitect, Reason: "LEGION-2 is done"}, time.Now()))
			var logged bytes.Buffer
			publisher := &holderPublisher{absent: true}
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
