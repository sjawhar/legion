package daemon

import (
	"cmp"
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// workerClaim creates issue's claim on role, in state, suspended by by, with a session the resume
// would carry.
func workerClaim(t *testing.T, sup *supervisor, issue string, role claim.Role, state supervise.ClaimState, by supervise.SuspendSource) claim.Token {
	t.Helper()
	token := mustClaimToken(t, issue, role)
	if _, _, err := sup.Create(context.Background(), supervise.Claim{
		Token: token, Project: "legion", Tree: "LEGION-1", Issue: issue, Role: role, Generation: 1, State: state, SuspendedBy: by,
		Session: "ses_" + string(role), SessionFile: "/sessions/" + string(role) + ".jsonl",
	}, ""); err != nil {
		t.Fatalf("create the %s claim of %s: %v", role, issue, err)
	}
	return token
}

// A review round that ends with no decision is the one path the daemon cannot end on its own: the
// reviewer must submit a review, and the daemon tells the architect so with a review-stuck notice
// whose Topic names the reviewer's role topic, which the architect answers with its own
// envoy_publish. While the idle suspend holds that reviewer, nobody holds the role and the
// listener refuses the architect's publish with its 404. The role_unheld report of that refusal
// resumes the reviewer too (TestARoleLaneExceptionResumesTheIdleSuspendedWorker), but only after
// the architect's first publish has failed. So the notice's own publish resumes the reviewer first,
// and the reviewer holds its role by the time the architect answers.
//
// It resumes nothing else: an operator's suspension is the operator's to end, a daemon suspension
// is its workflow's, a failed or retired claim is not coming back for a message, a worker of a
// lingering tree stays stopped with it, and a live reviewer needs nothing.
func TestAReviewStuckNoticeResumesTheIdleSuspendedReviewer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		state   supervise.ClaimState
		by      supervise.SuspendSource
		lingers bool
		resumes int
	}{
		{name: "the idle suspend holds the reviewer", state: supervise.StateSuspended, by: supervise.SuspendedByIdle, resumes: 1},
		{name: "an operator suspended the reviewer", state: supervise.StateSuspended, by: supervise.SuspendedByOperator},
		{name: "the workflow suspended the reviewer", state: supervise.StateSuspended, by: supervise.SuspendedByDaemon},
		{name: "the reviewer's claim failed", state: supervise.StateFailed},
		{name: "the reviewer's claim retired", state: supervise.StateRetired},
		{name: "the reviewer is live", state: supervise.StateIdle},
		{name: "the tree lingers", state: supervise.StateSuspended, by: supervise.SuspendedByIdle, lingers: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			noticeTree(t, pool, records, tc.lingers)
			sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
			architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
			reviewer := workerClaim(t, sup, "LEGION-2", claim.RoleReviewer, tc.state, tc.by)
			publisher := &holderPublisher{}
			row := leasedOutboxRow(t, pool, records, mustOutboxRow(t, "LEGION-2", record.Notice{
				Kind: "review-stuck", Role: claim.RoleReviewer, Phase: phase.Reviewing, Summary: "the reviewer's completion",
				Reason: "no review decides the round", Topic: notify.RoleTopicPrefix + string(reviewer),
			}, time.Now()))
			runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion"}

			if err := runner.execute(context.Background(), row); err != nil {
				t.Fatalf("execute the review-stuck notice: %v", err)
			}

			if got := len(rt.CallsOf("Resume")); got != tc.resumes {
				t.Fatalf("resumes = %d, want %d for %s", got, tc.resumes, tc.name)
			}
			machine, _ := sup.Machine(reviewer)
			if tc.resumes > 0 && machine.Claim().State == supervise.StateSuspended {
				t.Fatalf("the reviewer is %s, want it out of its idle suspension", machine.Claim().State)
			}
			if tc.resumes == 0 && tc.state == supervise.StateSuspended && machine.Claim().State != supervise.StateSuspended {
				t.Fatalf("the reviewer is %s, want its suspension kept", machine.Claim().State)
			}
			// The architect is told either way: the wake is not what the notice waits on.
			want := architectTopic(t, "LEGION-1")
			if _, delivered := publisher.snapshot(); len(delivered) != 1 || delivered[0].topic != want {
				t.Fatalf("notice publishes = %+v, want one to %s", delivered, want)
			}
		})
	}
}

// The listener reports a role message it could not deliver, whoever published: a peer agent's
// envoy_publish to a worker that finished its phase as much as a message of the daemon's own. That
// is a forward it accepted and then failed, or a publish it refused with its 404 because nobody
// holds the role (role_unheld): the role was never claimed, or its holder's registration lapsed,
// which is how a worker the idle suspend stopped looks once its session's registration has gone. A
// report for a worker the idle suspend holds resumes it, so the sender's next attempt finds its role
// held. Every other report changes nothing here: a reason the listener does not name, a topic of
// another project, a role the idle suspend is not for (an architect's, which keeps its own re-hold
// path), or a claim an operator suspended or that failed. A caller can choose the idempotency key
// that becomes a report's dedupe key, so a key shaped like this daemon's outbox key is not evidence
// that the daemon published the message and does not suppress a role_unheld wake.
func TestARoleLaneExceptionResumesTheIdleSuspendedWorker(t *testing.T) {
	for _, tc := range []struct {
		name      string
		role      claim.Role
		state     supervise.ClaimState
		by        supervise.SuspendSource
		reason    string
		topic     string
		key       string
		recipient string
		resumes   int
	}{
		{name: "a peer's message found the holder gone", role: claim.RoleReviewer, state: supervise.StateSuspended, by: supervise.SuspendedByIdle, reason: "no_holder", resumes: 1},
		{name: "a forward that failed", role: claim.RoleTester, state: supervise.StateSuspended, by: supervise.SuspendedByIdle, reason: "delivery_failed", resumes: 1},
		{name: "a receipt that never came", role: claim.RoleMerger, state: supervise.StateSuspended, by: supervise.SuspendedByIdle, reason: "receipt_timeout", resumes: 1},
		{name: "a peer's publish the listener refused: the role is unclaimed", role: claim.RoleImplementer, state: supervise.StateSuspended, by: supervise.SuspendedByIdle, reason: "role_unheld", resumes: 1},
		{name: "a peer's publish the listener refused: the holder's registration lapsed", role: claim.RoleReviewer, state: supervise.StateSuspended, by: supervise.SuspendedByIdle, reason: "role_unheld",
			recipient: "ses_reviewer", resumes: 1},
		{name: "a peer's publish with an outbox-shaped idempotency key", role: claim.RoleTester, state: supervise.StateSuspended, by: supervise.SuspendedByIdle, reason: "role_unheld",
			key: "legion-outbox:9", resumes: 1},
		{name: "a reason the listener does not name", role: claim.RoleReviewer, state: supervise.StateSuspended, by: supervise.SuspendedByIdle, reason: "something_else"},
		{name: "an operator's suspension", role: claim.RoleReviewer, state: supervise.StateSuspended, by: supervise.SuspendedByOperator, reason: "no_holder"},
		{name: "an operator's suspension the listener reports unheld", role: claim.RoleReviewer, state: supervise.StateSuspended, by: supervise.SuspendedByOperator, reason: "role_unheld"},
		{name: "a failed claim", role: claim.RoleReviewer, state: supervise.StateFailed, reason: "no_holder"},
		{name: "a live worker", role: claim.RoleReviewer, state: supervise.StateIdle, reason: "no_holder"},
		{name: "a topic of another project", role: claim.RoleReviewer, state: supervise.StateSuspended, by: supervise.SuspendedByIdle, reason: "no_holder",
			topic: notify.RoleTopicPrefix + "legion-other-legion-2-reviewer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			noticeTree(t, pool, records, false)
			sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
			architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
			worker := workerClaim(t, sup, "LEGION-2", tc.role, tc.state, tc.by)
			topic := notify.RoleTopicPrefix + string(worker)
			if tc.topic != "" {
				topic = tc.topic
			}
			runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, project: "legion", now: time.Now}
			// A peer publish normally has a generated key, but its caller may choose an idempotency
			// key that becomes an outbox-shaped dedupe key. The report must never treat that spelling
			// as authenticated daemon provenance.
			key := cmp.Or(tc.key, "publish.peer-1")
			report := laneReport{"evt-peer", tc.reason, topic, "a question about the review", map[string]string{"message": "why did you set this aside?"}, key, tc.recipient}

			if err := runner.rehold(context.Background(), report.envelope(t)); err != nil {
				t.Fatalf("rehold the peer report: %v", err)
			}

			if got := len(rt.CallsOf("Resume")); got != tc.resumes {
				t.Fatalf("resumes = %d, want %d for %s", got, tc.resumes, tc.name)
			}
			if rows := outboxRows(t, pool); rows != 0 {
				t.Fatalf("the peer report queued %d outbox rows, want none", rows)
			}
		})
	}
}
