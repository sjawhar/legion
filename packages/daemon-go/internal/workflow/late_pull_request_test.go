package workflow

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// lateApplied is the clock of the newest lifecycle event the seeded pull request has applied.
var lateApplied = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

// pullRequestView is the pull request state and reviewer-round decision a lifecycle event must
// preserve or update.
type pullRequestView struct {
	head, verdict, decision string
	state                   record.PullRequestState
}

// afterEvents seeds a reviewing issue whose pull request is at head-c, green and whose reviewer
// round is approved, with lateApplied as its clock. It applies facts and reads both records back.
func afterEvents(t *testing.T, state record.PullRequestState, facts ...intake.Fact) pullRequestView {
	t.Helper()
	return pullRequestAt(t, appliedEvents(t, state, facts...))
}

// appliedEvents is afterEvents' database: the seeded issue and pull request, with facts applied in
// order.
func appliedEvents(t *testing.T, state record.PullRequestState, facts ...intake.Fact) *pgxpool.Pool {
	t.Helper()
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Reviewing, Generation: 1, Status: "needs_review", Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: state, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208",
		HeadSHA: "head-c", HeadUpdatedAt: lateApplied, HeadUpdatedAtSource: "webhook", Verdict: "green",
		Failing: []string{}, FailingStatuses: []string{}, CheckRuns: []record.AttemptRun{}})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer,
		Decision: &record.ReviewDecision{State: "approved", Head: "head-c"}})
	for i, fact := range facts {
		if _, err := intake.ApplyFact(context.Background(), pool, "github", fmt.Sprintf("step-%d", i), fact, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	return pool
}

// pullRequestAt reads back the pull request and the reviewer round's decision.
func pullRequestAt(t *testing.T, pool *pgxpool.Pool) pullRequestView {
	t.Helper()
	var got pullRequestView
	if err := pool.QueryRow(context.Background(), `select pr.head_sha, pr.verdict, coalesce(reviewer.decision ->> 'state', ''), pr.state
		from pull_requests pr left join phases reviewer on reviewer.issue = pr.issue and reviewer.role = $1
		where pr.issue = $2`, string(claim.RoleReviewer), "LEGION-208").
		Scan(&got.head, &got.verdict, &got.decision, &got.state); err != nil {
		t.Fatalf("read pull request: %v", err)
	}
	return got
}

func lateSync(head string, at time.Time) intake.Fact {
	return intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: head, UpdatedAt: at}
}

func lateOpened(head string, at time.Time) intake.Fact {
	return intake.PullRequestOpened{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: head, UpdatedAt: at}
}

func lateReopened(head string, at time.Time) intake.Fact {
	return intake.PullRequestOpened{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: head, UpdatedAt: at, Reopened: true}
}

func lateClosed(head string, at time.Time) intake.Fact {
	return intake.PullRequestClosed{Repo: "sjawhar/legion", Number: 42, HeadSHA: head, UpdatedAt: at}
}

// GitHub redelivers a failed delivery on request, hours late if need be, and nothing orders a
// webhook against those that followed it. Every pull request lifecycle event carries the pull
// request's updated_at, so one older than the newest applied is a late redelivery and changes
// nothing: it neither moves the head back nor reopens or closes the pull request against a newer
// close or reopen. The decision belongs to the reviewer round, rather than the pull request,
// because the reviewer's handoff push itself moves the head; lifecycle observations leave it until
// the round ends. An event at the same clock applies (GitHub's clock is to the second, so two real
// events can share one), and so does one that carries no clock.
func TestALatePullRequestEventChangesNothing(t *testing.T) {
	earlier, later := lateApplied.Add(-time.Hour), lateApplied.Add(time.Minute)
	kept := pullRequestView{head: "head-c", verdict: "green", decision: "approved", state: record.PullRequestOpen}
	closed := pullRequestView{head: "head-c", verdict: "green", decision: "approved", state: record.PullRequestClosed}
	for _, tc := range []struct {
		name string
		seed record.PullRequestState
		fact intake.Fact
		want pullRequestView
	}{
		{"a synchronize older than the head", record.PullRequestOpen, lateSync("head-b", earlier), kept},
		{"an opened older than the head", record.PullRequestOpen, lateOpened("head-a", earlier), kept},
		// GitHub sends opened once per pull request, so another one is a redelivery whatever its
		// clock: an opened and a synchronize can share GitHub's one-second clock.
		{"an opened at the same clock as the head", record.PullRequestOpen, lateOpened("head-a", lateApplied), kept},
		{"an opened newer than the head", record.PullRequestOpen, lateOpened("head-a", later), kept},
		{"a close older than the reopen", record.PullRequestOpen, lateClosed("head-c", earlier), kept},
		{"a reopen older than the close", record.PullRequestClosed, lateReopened("head-c", earlier), closed},

		{"a newer synchronize", record.PullRequestOpen, lateSync("head-d", later), pullRequestView{head: "head-d", decision: "approved", state: record.PullRequestOpen}},
		{"a synchronize at the same clock", record.PullRequestOpen, lateSync("head-d", lateApplied), pullRequestView{head: "head-d", decision: "approved", state: record.PullRequestOpen}},
		{"a synchronize with no clock", record.PullRequestOpen, lateSync("head-d", time.Time{}), pullRequestView{head: "head-d", decision: "approved", state: record.PullRequestOpen}},
		{"a newer close", record.PullRequestOpen, lateClosed("head-c", later), closed},
		{"a close with no clock", record.PullRequestOpen, lateClosed("head-c", time.Time{}), closed},
		{"a newer reopen", record.PullRequestClosed, lateReopened("head-c", later), pullRequestView{head: "head-c", decision: "approved", state: record.PullRequestOpen}},
		{"a reopen at the same clock as the close", record.PullRequestClosed, lateReopened("head-c", lateApplied), pullRequestView{head: "head-c", decision: "approved", state: record.PullRequestOpen}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := afterEvents(t, tc.seed, tc.fact); got != tc.want {
				t.Fatalf("pull request %+v, want %+v", got, tc.want)
			}
		})
	}
}

// An event with no clock (from a listener that did not carry updated_at) is applied, but it never
// lowers the pull request's clock: the stored clock stays the latest one known, so an older
// timestamped event redelivered afterwards is still late and changes nothing.
func TestAnEventWithNoClockKeepsTheLatestClock(t *testing.T) {
	earlier, later := lateApplied.Add(-time.Hour), lateApplied.Add(time.Minute)
	for _, tc := range []struct {
		name  string
		facts []intake.Fact
		head  string
		state record.PullRequestState
	}{
		{"a synchronize with no clock, then an older one",
			[]intake.Fact{lateSync("head-d", later), lateSync("head-e", time.Time{}), lateSync("head-b", lateApplied)}, "head-e", record.PullRequestOpen},
		{"a reopen with no clock, then an older synchronize",
			[]intake.Fact{lateReopened("head-c", time.Time{}), lateSync("head-b", earlier)}, "head-c", record.PullRequestOpen},
		{"a close with no clock, then an older reopen",
			[]intake.Fact{lateClosed("head-c", time.Time{}), lateReopened("head-c", earlier)}, "head-c", record.PullRequestClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := afterEvents(t, record.PullRequestOpen, tc.facts...); got.head != tc.head || got.state != tc.state {
				t.Fatalf("pull request at %s, %s; want %s, %s", got.head, got.state, tc.head, tc.state)
			}
		})
	}
}

// GitHub's close and reopen payloads carry the head every synchronize before them left, so a close
// or reopen that is not late records that head with its clock. A synchronize older than the close,
// delivered after it, is late and changes nothing, and the pull request still ends at the head
// GitHub ended at. A close that finds the pull request already closed is not late either: closed,
// reopened and closed again, with the reopen delivered last, the second close records its head and
// clock, so the reopen changes nothing.
func TestACloseOrReopenCarriesItsHead(t *testing.T) {
	synchronized, finished := lateApplied.Add(time.Minute), lateApplied.Add(2*time.Minute)
	reopened, resynchronized := lateApplied.Add(time.Minute), lateApplied.Add(90*time.Second)
	for _, tc := range []struct {
		name  string
		seed  record.PullRequestState
		facts []intake.Fact
		want  pullRequestView
	}{
		{"a close delivered before the synchronize it followed", record.PullRequestOpen,
			[]intake.Fact{lateClosed("head-d", finished), lateSync("head-d", synchronized)}, pullRequestView{head: "head-d", decision: "approved", state: record.PullRequestClosed}},
		{"a reopen delivered before the synchronize it followed", record.PullRequestClosed,
			[]intake.Fact{lateReopened("head-e", finished), lateSync("head-e", synchronized)}, pullRequestView{head: "head-e", decision: "approved", state: record.PullRequestOpen}},
		{"a close delivered before the reopen it followed", record.PullRequestClosed,
			[]intake.Fact{lateClosed("head-d", finished), lateReopened("head-c", reopened)}, pullRequestView{head: "head-d", decision: "approved", state: record.PullRequestClosed}},
		{"a close delivered before the reopen and the synchronize it followed", record.PullRequestClosed,
			[]intake.Fact{lateClosed("head-e", finished), lateReopened("head-c", reopened), lateSync("head-e", resynchronized)}, pullRequestView{head: "head-e", decision: "approved", state: record.PullRequestClosed}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := afterEvents(t, tc.seed, tc.facts...); got != tc.want {
				t.Fatalf("pull request %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A branch can carry a second pull request: after a merge, or once park and rerun open a new one
// from the same branch. A late opened or reopened of the earlier pull request, older than the
// newer one's clock, is a redelivery and changes nothing: the record stays the newer pull request,
// whose later events still apply. The same holds for an older pull request's late opened while the
// newer one is open.
func TestALateEventOfAnEarlierPullRequestLeavesTheNewerOne(t *testing.T) {
	opened43, synced43 := lateApplied.Add(time.Minute), lateApplied.Add(2*time.Minute)
	pr43 := intake.PullRequestOpened{Repo: "sjawhar/legion", Number: 43, Branch: "legion/LEGION-208", HeadSHA: "head-e", UpdatedAt: opened43}
	sync43 := intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 43, Branch: "legion/LEGION-208", HeadSHA: "head-f", UpdatedAt: synced43}
	pr41 := intake.PullRequestOpened{Repo: "sjawhar/legion", Number: 41, Branch: "legion/LEGION-208", HeadSHA: "head-a", UpdatedAt: lateApplied.Add(-time.Hour)}
	for _, tc := range []struct {
		name   string
		seed   record.PullRequestState
		facts  []intake.Fact
		number int
		head   string
		state  record.PullRequestState
	}{
		{"merged #42, #43 opened, then #42's late opened", record.PullRequestMerged, []intake.Fact{pr43, lateOpened("head-c", lateApplied), sync43}, 43, "head-f", record.PullRequestOpen},
		{"merged #42, #43 opened, then #42's late reopen", record.PullRequestMerged, []intake.Fact{pr43, lateReopened("head-c", lateApplied.Add(30*time.Second)), sync43}, 43, "head-f", record.PullRequestOpen},
		{"open #42, then an older #41's late opened", record.PullRequestOpen, []intake.Fact{pr41}, 42, "head-c", record.PullRequestOpen},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := appliedEvents(t, tc.seed, tc.facts...)
			var number int
			var head string
			var state record.PullRequestState
			if err := pool.QueryRow(t.Context(), "select number, head_sha, state from pull_requests where issue = 'LEGION-208'").Scan(&number, &head, &state); err != nil {
				t.Fatalf("read the pull request: %v", err)
			}
			if number != tc.number || head != tc.head || state != tc.state {
				t.Fatalf("the record is #%d at %s, %s; want #%d at %s, %s", number, head, state, tc.number, tc.head, tc.state)
			}
		})
	}
}
