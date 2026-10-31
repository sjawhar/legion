package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// When CI settles red the daemon starts the implementer at once — it waits for no turn to end and
// stops nobody — so the role whose phase that red ended would otherwise learn of it from nothing:
// the task the daemon next has for a role is delivered only when that role's turn is over. The
// phase-ended row (record.PhaseEnded) is what tells it, published to that role's own Envoy topic in
// the same drain as the start: the tester in testing, the reviewer in reviewing whether or not it
// completed its round, the merger in merging, and the implementer in retro, which is told besides
// that its implementing task follows. The message is the daemon's own sentence, pinned here for
// each of the five, and it ends with the reason the architect's checks-red notice carries, behind
// the `Reason:` label a started worker's task carries it under, so the check and workflow names
// GitHub supplies stand apart from the daemon's own words. It is no
// architect notice: the architect's own notice goes out beside it, to the architect's topic.
func TestACIRedTellsTheRoleWhosePhaseEndedOnItsOwnTopic(t *testing.T) {
	const red = "CI is red at sha-1: ci"
	tail := " Nothing of yours is stopped: your turn, your jobs, your process and your session run on." +
		" The workspace is the implementing phase's now, so stop writing it at once and leave whatever you have uncommitted." +
		" Reason: " + red + "."
	for _, tc := range []struct {
		name   string
		from   phase.Phase
		status string
		role   claim.Role
		// uncompleted seeds the reviewer's row without its completion: its round still at work.
		uncompleted bool
		message     string
	}{
		{name: "the tester in testing", from: phase.Testing, status: "testing", role: claim.RoleTester,
			message: "LEGION-208: CI went red, so your testing phase has ended and the issue is in implementing now." + tail},
		{name: "the reviewer in a round it completed", from: phase.Reviewing, status: "needs_review", role: claim.RoleReviewer,
			message: "LEGION-208: CI went red, so your reviewing phase has ended and the issue is in implementing now." + tail},
		{name: "the reviewer in a round it did not complete", from: phase.Reviewing, status: "needs_review", role: claim.RoleReviewer, uncompleted: true,
			message: "LEGION-208: CI went red, so your reviewing phase has ended and the issue is in implementing now." + tail},
		{name: "the merger in merging", from: phase.Merging, status: "retro", role: claim.RoleMerger,
			message: "LEGION-208: CI went red, so your merging phase has ended and the issue is in implementing now." + tail},
		{name: "the implementer in retro", from: phase.Retro, status: "retro", role: claim.RoleImplementer,
			message: "LEGION-208: CI went red, so your retro phase has ended and the issue is in implementing now." +
				" Your implementing task follows." + tail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			publisher := &outboxPublisher{}
			runner, sup, pool := redRunner(t, tc.from, tc.status, tc.uncompleted, publisher)
			implementer := startedClaim(t, sup, claim.RoleImplementer)

			applyTheRed(t, ctx, pool, runner)
			if err := runner.RunOnce(ctx); err != nil {
				t.Fatalf("run the red's effects: %v", err)
			}

			// The start ran in the same drain, and the implementer holds the CI-red task.
			implementer.Wait()
			held := implementer.Claim()
			if held.Pending == nil || !strings.Contains(held.Pending.Task, "Phase: implementing.") || !strings.Contains(held.Pending.Task, red) {
				t.Fatalf("the implementer is %s holding %+v, want the implementing task naming the red: its start waits for nothing", held.State, held.Pending)
			}
			topic := notify.RoleTopicPrefix + string(mustClaimToken(t, "LEGION-208", tc.role))
			var told []outboxPublish
			for _, published := range publisher.publishes() {
				if published.topic == topic {
					told = append(told, published)
				}
			}
			if len(told) != 1 {
				t.Fatalf("publishes to %s = %#v, want the one phase-ended message", topic, told)
			}
			if told[0].message != tc.message || told[0].payload != any(tc.message) {
				t.Fatalf("the %s was told\n\t%q\nwant\n\t%q", tc.role, told[0].message, tc.message)
			}
			t.Logf("the %s was told: %s", tc.role, told[0].message)
			if _, ok := record.ParseOutboxKey(told[0].key); !ok {
				t.Fatalf("the message went under key %q, want its row's own (legion-outbox:<id>): a retried row is one delivery", told[0].key)
			}
			// The architect's own notice is a row of its own, to its own topic, as before.
			architect := notify.RoleTopicPrefix + string(mustClaimToken(t, "LEGION-208", claim.RoleArchitect))
			var notices int
			for _, published := range publisher.publishes() {
				if notice, ok := published.payload.(record.Notice); ok && notice.Kind == "checks-red" && published.topic == architect {
					notices++
				}
			}
			if notices != 1 {
				t.Fatalf("the architect was told checks-red %d times, want once beside the phase-ended message", notices)
			}
			if left := outboxRows(t, pool); left != 0 {
				t.Fatalf("outbox rows left = %d, want every row of the red run", left)
			}
		})
	}
}

// A role topic with no live holder is the ordinary state before a session claims it, and the role's
// next task from the daemon names its phase anyway: the row ends with one log line rather than
// waiting as an architect's notice waits, and the implementer's start goes on all the same.
func TestAPhaseEndedMessageNobodyHoldsEndsAndTheStartGoesOn(t *testing.T) {
	ctx := context.Background()
	publisher := &holderPublisher{}
	runner, sup, pool := redRunner(t, phase.Testing, "testing", false, publisher)
	implementer := startedClaim(t, sup, claim.RoleImplementer)
	topic := notify.RoleTopicPrefix + string(mustClaimToken(t, "LEGION-208", claim.RoleTester))
	publisher.setAbsent(topic)

	applyTheRed(t, ctx, pool, runner)
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the red's effects: %v", err)
	}

	implementer.Wait()
	if pending := implementer.Claim().Pending; pending == nil || !strings.Contains(pending.Task, "Phase: implementing.") {
		t.Fatalf("the implementer holds %+v, want its implementing task: the message nobody took holds back no start", pending)
	}
	attempts, delivered := publisher.snapshot()
	for _, published := range delivered {
		if published.topic == topic {
			t.Fatalf("publish to the tester's topic %#v, want none: nobody holds it", published)
		}
	}
	if attempts != 2 {
		t.Fatalf("publish attempts = %d, want two: the refused message and the architect's notice", attempts)
	}
	if left := outboxRows(t, pool); left != 0 {
		t.Fatalf("outbox rows left = %d, want the message finished rather than held", left)
	}
}

// The three answers of the confirmed publish route are other failures, and keep the ordinary retry:
// a holder that did not confirm the message in time can take it on the next attempt, a holder that
// does not advertise the delivery mode can be replaced by another session on the role, and a
// forward the listener's own NATS grant denies goes through once an operator changes that grant.
// Only a role with no live holder ends the row.
func TestAPhaseEndedMessageRetriesEveryFailureButNoHolder(t *testing.T) {
	ctx := context.Background()
	row := mustOutboxRow(t, "LEGION-208", record.PhaseEnded{Role: claim.RoleTester, From: phase.Testing,
		To: phase.Implementing, Reason: "CI is red at sha-1: ci"}, time.Now())
	row.ID = 61
	publisher := &outboxPublisher{}
	runner := &outbox{project: "legion", notices: publisher, log: quietLogger()}
	const denial = `envoy: the message bus denied the publish under this listener's own permissions: bus: NATS denied the publish to "notifications.agent.ses_t"`
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "a holder that did not confirm it", err: notify.ErrNotDelivered{Reason: notify.ReasonReceiptTimeout, Holder: "ses_t"}},
		{name: "a holder that does not advertise the delivery mode", err: notify.ErrRefusedByHolder{Holder: "ses_t"}},
		{name: "a forward the listener's grant denies", err: notify.ErrPublishDenied{Message: denial}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			publisher.err = fmt.Errorf("publish notice to the tester's topic: %w", tc.err)
			err := runner.execute(ctx, row)
			if err == nil || !errors.Is(err, tc.err) {
				t.Fatalf("the phase-ended message answered %v = %v, want the answer as a failure, retried", tc.err, err)
			}
			t.Logf("%s: %v", tc.name, err)
		})
	}
	publisher.err = fmt.Errorf("publish notice to the tester's topic: %w: no holder", notify.ErrNoHolder)
	if err := runner.execute(ctx, row); err != nil {
		t.Fatalf("the phase-ended message no session holds = %v, want the row finished", err)
	}
}

// redRunner is an outbox runner over an isolated database holding LEGION-208 in from, with the
// pull request a red is about to settle on and the phase rows of the roles that worked it, wired to
// the workflow engine as production wires it: a fact applied against the pool queues its rows, and
// one drain runs them.
func redRunner(t *testing.T, from phase.Phase, status string, uncompleted bool, publisher notify.Publisher) (*outbox, *supervisor, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: "LEGION-208", Title: "Workflow", Phase: from, Generation: 1, Status: status, Rank: "U", LastDispatchSeq: 5}
	putOutboxIssue(t, pool, records, issue)
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: issue.Key, Repo: "acme/widgets",
			Number: 86, Branch: "legion/LEGION-208", HeadSHA: "sha-1", HeadUpdatedAt: time.Now(), Failing: []string{},
			CheckRuns: []record.AttemptRun{}, Required: []string{"ci"}}); err != nil {
			return err
		}
		if err := records.PutPhase(ctx, tx, record.PhaseRow{Issue: issue.Key, Role: claim.RoleImplementer, Claim: "implement-claim"}); err != nil {
			return err
		}
		if from == phase.Reviewing {
			reviewer := record.PhaseRow{Issue: issue.Key, Role: claim.RoleReviewer, Claim: "review-claim", HandoffCommit: "sha-1", Summary: "reviewed"}
			if uncompleted {
				reviewer.HandoffCommit, reviewer.Summary = "", ""
			}
			return records.PutPhase(ctx, tx, reviewer)
		}
		if from == phase.Merging {
			return records.PutPhase(ctx, tx, record.PhaseRow{Issue: issue.Key, Role: claim.RoleMerger, Claim: "merger-claim"})
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the pull request and the phase rows: %v", err)
	}
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.PhaseHolds = (&workflowRuntime{pool: pool, records: records}).phaseHolds
	engine := workflow.New(records, workflow.Config{Project: "legion", Repositories: testRepositories}, quietLogger())
	runner := &outbox{
		dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup, trees: outboxTreeStore(t, pool, issue.Tree),
		tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
		dispatch: &outboxDispatch{issue: dispatch.Issue{Key: issue.Key, Status: status}}, notices: publisher,
		// The drain runs an hour after the rows the red writes, so every one of them is due.
		handlers: []intake.Handler{engine}, now: func() time.Time { return time.Now().Add(time.Hour) }, log: quietLogger(),
	}
	return runner, sup, pool
}

// startedClaim creates role's claim on LEGION-208 as a launch that has not registered yet, so the
// start the red queues finds it and hands it its task: what the claim then holds is what the start
// delivered.
func startedClaim(t *testing.T, sup *supervisor, role claim.Role) *supervise.Machine {
	t.Helper()
	machine, _, err := sup.Create(context.Background(), supervise.Claim{Token: mustClaimToken(t, "LEGION-208", role), Project: "legion",
		Tree: "LEGION-208", Issue: "LEGION-208", Role: role, State: supervise.StateQueued}, "")
	if err != nil {
		t.Fatalf("create the %s's claim: %v", role, err)
	}
	return machine
}

// applyTheRed settles the required check red on the pull request's head, the fact that sends the
// work back to implementing.
func applyTheRed(t *testing.T, ctx context.Context, pool *pgxpool.Pool, runner *outbox) {
	t.Helper()
	result, err := intake.ApplyFact(ctx, pool, "github", "red", intake.PullRequestChecks{Repo: "acme/widgets", Number: 86, HeadSHA: "sha-1",
		CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Generation: 1, Snapshot: "red-1", Failing: []string{"ci"}}, runner.handlers...)
	if err != nil || result.Refusal != nil {
		t.Fatalf("apply the red verdict = %+v, %v", result, err)
	}
}
