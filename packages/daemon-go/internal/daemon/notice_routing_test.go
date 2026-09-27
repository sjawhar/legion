package daemon

import (
	"bytes"
	"context"
	"errors"
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

// Every notice is architect-facing, so it goes to the architect that owns its issue, by the
// TypeScript daemon's rule (owningArchitect): the nearest issue at or above it whose sub-architect
// claim runs (not suspended, failed or retired), else the tree root; a child's close or status
// change starts at its parent. The walk stays inside the issue's tree: admission records a Dispatch
// re-parent as it comes and leaves the issue's tree as it was, so a parent that is not an issue of
// that tree — not recorded, or recorded in another tree — ends the walk at the tree's root.
func TestTheArchitectThatOwnsANotice(t *testing.T) {
	root, child, unrecorded, elsewhere := "LEGION-1", "LEGION-2", "LEGION-77", "LEGION-50"
	tree := map[string]record.Issue{
		root:       {Key: root, Tree: root},
		child:      {Key: child, Tree: root, Parent: &root},
		"LEGION-3": {Key: "LEGION-3", Tree: root, Parent: &child},
		"LEGION-4": {Key: "LEGION-4", Tree: root, Parent: &unrecorded},
		"LEGION-5": {Key: "LEGION-5", Tree: root, Parent: &elsewhere},
	}
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
		{name: "a child whose sub-architect is launching", issue: "LEGION-2", subArchitect: supervise.StateLaunching, owner: "LEGION-2"},
		{name: "a grandchild under the sub-architect's child", issue: "LEGION-3", subArchitect: supervise.StateWorking, owner: "LEGION-2"},
		{name: "a child whose sub-architect retired", issue: "LEGION-2", subArchitect: supervise.StateRetired, owner: "LEGION-1"},
		{name: "a child whose sub-architect failed", issue: "LEGION-2", subArchitect: supervise.StateFailed, owner: "LEGION-1"},
		{name: "a child whose sub-architect was suspended", issue: "LEGION-2", subArchitect: supervise.StateSuspended, owner: "LEGION-1"},
		{name: "a child's close under its running sub-architect", issue: "LEGION-2", kind: "child-closed", subArchitect: supervise.StateWorking, owner: "LEGION-1"},
		{name: "a child's status change under its running sub-architect", issue: "LEGION-2", kind: "child-status", subArchitect: supervise.StateWorking, owner: "LEGION-1"},
		{name: "a grandchild's close under the sub-architect's child", issue: "LEGION-3", kind: "child-closed", subArchitect: supervise.StateWorking, owner: "LEGION-2"},
		{name: "the root's own close", issue: "LEGION-1", kind: "child-closed", owner: "LEGION-1"},
		{name: "a child whose parent is not recorded", issue: "LEGION-4", owner: "LEGION-1"},
		{name: "a child whose parent is in another tree", issue: "LEGION-5", owner: "LEGION-1"},
		{name: "the close of a child whose parent is not recorded", issue: "LEGION-4", kind: "child-closed", owner: "LEGION-1"},
		{name: "the close of a child whose parent is in another tree", issue: "LEGION-5", kind: "child-status", owner: "LEGION-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			states := map[claim.Token]supervise.ClaimState{
				mustClaimToken(t, root, claim.RoleArchitect):  supervise.StateWorking,
				mustClaimToken(t, child, claim.RoleArchitect): tc.subArchitect,
			}
			kind := tc.kind
			if kind == "" {
				kind = "phase-finished"
			}
			got, err := owningArchitect("legion", tree, tree[tc.issue], kind, func(token claim.Token) bool { return claimRuns(states[token]) })
			if want := mustClaimToken(t, tc.owner, claim.RoleArchitect); err != nil || got != want {
				t.Fatalf("owner = %s, %v; want %s", got, err, want)
			}
		})
	}
}

// The executor publishes a notice to its owner's role topic and to nothing else: no issue topic,
// since every issue topic is a subject that issue's phase workers subscribe to, and no worker's role
// topic.
func TestANoticeGoesToTheOwningArchitectsRoleTopicAlone(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
	architectClaim(t, sup, "LEGION-2", supervise.StateWorking)
	publisher := &holderPublisher{}
	row := mustOutboxRow(t, "LEGION-3", record.Notice{Kind: "phase-finished", Role: claim.RoleTester, Phase: phase.Testing}, time.Now())
	row.ID = 56

	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion"}
	if err := runner.execute(context.Background(), row); err != nil {
		t.Fatalf("execute notice: %v", err)
	}
	want := architectTopic(t, "LEGION-2")
	_, delivered := publisher.snapshot()
	if len(delivered) != 1 || delivered[0].topic != want || delivered[0].key != "legion-outbox:56" {
		t.Fatalf("notice publishes = %+v, want one to %s under the row's key", delivered, want)
	}
	for _, issue := range []string{"LEGION-1", "LEGION-2", "LEGION-3"} {
		subjects := []string{"notifications.legion.legion." + issue}
		for _, role := range []claim.Role{claim.RolePlanner, claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer, claim.RoleMerger} {
			subjects = append(subjects, roleTopicPrefix+string(mustClaimToken(t, issue, role)))
		}
		if slices.Contains(subjects, delivered[0].topic) {
			t.Fatalf("the notice went to %s, a subject a phase worker of %s holds", delivered[0].topic, issue)
		}
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
	// The held row's error carries the listener's refusal, which is what the log line and the row's
	// last_error show an operator.
	var lastError string
	if err := pool.QueryRow(context.Background(), "select id, last_error from outbox order by id limit 1").Scan(&first.ID, &lastError); err != nil {
		t.Fatalf("read the first held row: %v", err)
	}
	if held := runner.execute(context.Background(), first); !errors.Is(held, errNoticeWaits) || !errors.Is(held, notify.ErrNoHolder) ||
		!strings.Contains(lastError, `"reason":"unclaimed"`) {
		t.Fatalf("held error %v, last_error %q: want a wait that carries the listener's no-holder refusal", held, lastError)
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

// A root architect the operator suspended outside a linger comes back when the operator resumes it
// (`legion claims resume` or `deliver`), so its notices are held for it, not finished: the row waits,
// logged once, and is delivered once the architect holds its role again. The fence is per
// architect, so the held rows hold no other architect's notices.
func TestANoticeForAnOperatorSuspendedRootIsHeldUntilItHoldsItsRoleAgain(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateSuspended)
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-2", record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "max_fix_attempts"}, time.Now()))
	var logged bytes.Buffer
	clock := time.Now()
	publisher := &holderPublisher{}
	publisher.setAbsent(architectTopic(t, "LEGION-1"))
	runner := &outbox{log: slog.New(slog.NewTextHandler(&logged, nil)), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup,
		project: "legion", now: func() time.Time { return clock }}

	for tick := range 2 {
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
		clock = clock.Add(2 * time.Minute)
	}
	if _, delivered := publisher.snapshot(); len(delivered) != 0 || outboxRows(t, pool) != 1 || strings.Count(logged.String(), "msg=\"outbox notice waits for its architect\"") != 1 {
		t.Fatalf("while the suspended root holds no role: delivered %+v, %d rows, log %q; want the row held, logged once", delivered, outboxRows(t, pool), logged.String())
	}
	publisher.setAbsent()
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run after the root holds its role again: %v", err)
	}
	if _, delivered := publisher.snapshot(); fmt.Sprint(deliveredKinds(t, delivered)) != "[pr-blocked to LEGION-1]" || outboxRows(t, pool) != 0 {
		t.Fatalf("after the root holds its role again: delivered %v, %d rows left; want the held pr-blocked delivered", deliveredKinds(t, delivered), outboxRows(t, pool))
	}
}

// An earlier notice's architect is found as its own kind routes it: a child's close starts at the
// parent. A close held for the absent root architect therefore holds the root's later notices, even
// though the closed child has a running sub-architect of its own, and when the root returns the
// close arrives first, though the later notice falls due before the held close does.
func TestAHeldChildCloseHoldsTheArchitectsLaterNotices(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateLaunching)
	architectClaim(t, sup, "LEGION-2", supervise.StateWorking)
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-2", record.Notice{Kind: "child-closed", Role: claim.RoleArchitect, Reason: "LEGION-2 is done"}, time.Now()))
	clock := time.Now()
	publisher := &holderPublisher{}
	publisher.setAbsent(architectTopic(t, "LEGION-1"))
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion",
		now: func() time.Time { return clock }}
	// The close is tried and held six times, so its backoff (1 s doubling, 32 s after the sixth)
	// outlasts the 20 s step, and the later notice, written now, falls due before the held close.
	for tick := range 6 {
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
		clock = clock.Add(20 * time.Second)
	}
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-1", record.Notice{Kind: "design-approved", Version: 3}, clock))
	var closeDue, laterDue time.Time
	if err := pool.QueryRow(context.Background(), `select
		(select next_at from outbox where payload ->> 'kind' = 'child-closed'),
		(select next_at from outbox where payload ->> 'kind' = 'design-approved')`).Scan(&closeDue, &laterDue); err != nil {
		t.Fatalf("read the two rows' due times: %v", err)
	}
	if !laterDue.Before(closeDue) {
		t.Fatalf("the later notice is due at %s and the held close at %s; want the later notice due first", laterDue, closeDue)
	}
	publisher.setAbsent()
	for tick := range 8 {
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("tick %d after the root returned: %v", tick, err)
		}
		clock = clock.Add(20 * time.Second)
	}
	if _, delivered := publisher.snapshot(); fmt.Sprint(deliveredKinds(t, delivered)) != "[child-closed to LEGION-1 design-approved to LEGION-1]" || outboxRows(t, pool) != 0 {
		t.Fatalf("delivered %v with %d rows left, want the held close first, then the later notice, both to the root", deliveredKinds(t, delivered), outboxRows(t, pool))
	}
}

// reparent records a Dispatch re-parent of LEGION-2 as admission does: its parent changes and its
// tree does not.
func reparent(t *testing.T, pool *pgxpool.Pool, records record.Store, parent string) {
	t.Helper()
	root := "LEGION-1"
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-2", Project: "LEGION", Tree: root, Parent: &parent, Title: "Child", Phase: phase.Planning, Generation: 1, Status: "in_progress"})
}

// A child re-parented under an issue the workflow has not recorded stays in its tree: its notice
// goes to the tree's root architect, and neither it nor the tree's other notices wait on the
// unrecorded parent. The root's own notice and a grandchild's, to the grandchild's running
// sub-architect, are delivered on the same tick.
func TestANoticeOfAChildReparentedOutsideTheRecordReachesItsTreesRoot(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	reparent(t, pool, records, "LEGION-99")
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
	architectClaim(t, sup, "LEGION-3", supervise.StateWorking)
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-2", record.Notice{Kind: "phase-finished", Role: claim.RolePlanner, Phase: phase.Planning}, time.Now()))
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-1", record.Notice{Kind: "design-approved", Version: 2}, time.Now()))
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-3", record.Notice{Kind: "phase-finished", Role: claim.RoleTester, Phase: phase.Testing}, time.Now()))
	publisher := &holderPublisher{}
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion", now: time.Now}
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, delivered := publisher.snapshot(); fmt.Sprint(deliveredKinds(t, delivered)) != "[phase-finished to LEGION-1 design-approved to LEGION-1 phase-finished to LEGION-3]" || outboxRows(t, pool) != 0 {
		t.Fatalf("delivered %v with %d rows left, want the re-parented child's and the root's notices to the root, the grandchild's to its sub-architect, none left",
			deliveredKinds(t, delivered), outboxRows(t, pool))
	}
}

// A child re-parented under another tree's root stays in its own tree: its notices, its close
// included, go to its own tree's root architect, never to the other tree's.
func TestANoticeOfAChildReparentedUnderAnotherTreeStaysWithItsOwnTree(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-50", Project: "LEGION", Tree: "LEGION-50", Title: "Other root", Phase: phase.Implementing, Generation: 1, Status: "in_progress"})
	reparent(t, pool, records, "LEGION-50")
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
	architectClaim(t, sup, "LEGION-50", supervise.StateWorking)
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-2", record.Notice{Kind: "phase-finished", Role: claim.RolePlanner, Phase: phase.Planning}, time.Now()))
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-2", record.Notice{Kind: "child-closed", Role: claim.RoleArchitect, Reason: "LEGION-2 is done"}, time.Now()))
	publisher := &holderPublisher{}
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion", now: time.Now}
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, delivered := publisher.snapshot(); fmt.Sprint(deliveredKinds(t, delivered)) != "[phase-finished to LEGION-1 child-closed to LEGION-1]" || outboxRows(t, pool) != 0 {
		t.Fatalf("delivered %v with %d rows left, want both notices to LEGION-1's architect", deliveredKinds(t, delivered), outboxRows(t, pool))
	}
}

// A notice whose architect cannot be resolved from the record (here an issue whose key makes no
// claim token) holds back no later notice of its tree, and finishes undelivered on its own attempt
// with one log line: retrying cannot change the record, and the outbox has no dead-letter path.
func TestAnUnroutableNoticeHoldsNothingBackAndFinishesOnItsOwnAttempt(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	root := "LEGION-1"
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-4x", Project: "LEGION", Tree: root, Parent: &root, Title: "Malformed", Phase: phase.Planning, Generation: 1, Status: "in_progress"})
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
	clock := time.Now()
	// The unroutable row is written first but falls due after the root's notice, so the root's
	// notice meets it as an earlier notice of the tree.
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-4x", record.Notice{Kind: "phase-finished", Role: claim.RolePlanner, Phase: phase.Planning}, clock.Add(time.Minute)))
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-1", record.Notice{Kind: "design-approved", Version: 2}, clock))
	var logged bytes.Buffer
	publisher := &holderPublisher{}
	runner := &outbox{log: slog.New(slog.NewTextHandler(&logged, nil)), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup,
		project: "legion", now: func() time.Time { return clock }}
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if _, delivered := publisher.snapshot(); fmt.Sprint(deliveredKinds(t, delivered)) != "[design-approved to LEGION-1]" || outboxRows(t, pool) != 1 {
		t.Fatalf("delivered %v with %d rows left, want the root's notice delivered past the unroutable one", deliveredKinds(t, delivered), outboxRows(t, pool))
	}
	clock = clock.Add(2 * time.Minute)
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if outboxRows(t, pool) != 0 || strings.Count(logged.String(), `msg="outbox notice finished undelivered: it has no architect"`) != 1 || !strings.Contains(logged.String(), "issue=LEGION-4x") {
		t.Fatalf("%d rows left, log %q; want the unroutable row finished with one line naming its issue", outboxRows(t, pool), logged.String())
	}
}
