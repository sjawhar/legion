package workflow

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The review event decides the round, and the reviewer's handoff records its findings
// (dispatch://INFRA-714/spec R1): a request for changes on the current head returns the issue to
// implementing the moment GitHub reports it, counting the round, and an approval of a head GitHub
// reports clean moves it to accepting. The reviewer is still mid-turn when either happens, so the
// completion that follows is recorded on its own row with the issue where the review left it -
// the findings the next implementer reads - rather than refused as a phase it no longer runs. A
// completion that arrives first records the same row and moves nothing, the review deciding when
// it lands.
func TestTheReviewEventDecidesAndTheCompletionRecordsItsFindings(t *testing.T) {
	for _, tc := range []struct {
		name          string
		decision      string
		completeFirst bool
		want          phase.Phase
		rounds        int
	}{
		{name: "changes requested, then the reviewer completes", decision: "changes_requested", want: phase.Implementing, rounds: 1},
		{name: "the reviewer completes, then changes are requested", decision: "changes_requested", completeFirst: true, want: phase.Implementing, rounds: 1},
		{name: "approved on a clean head, then the reviewer completes", decision: "approved", want: phase.Accepting},
		{name: "the reviewer completes, then an approval on a clean head", decision: "approved", completeFirst: true, want: phase.Accepting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			ctx := context.Background()
			seedReview(t, pool, "green")
			engine := testEngine(config.DesignGateRootIssues, nil)
			review := func() {
				t.Helper()
				if _, err := intake.ApplyFact(ctx, pool, "github", "review", intake.PullRequestReview{
					Repo: "sjawhar/legion", Number: 42, State: tc.decision, CommitID: "head", Author: testReviewApp, Body: "the review"}, engine); err != nil {
					t.Fatalf("apply the review: %v", err)
				}
			}
			complete := func() {
				t.Helper()
				result, err := intake.ApplyFact(ctx, pool, "api", "reviewer-handoff", intake.HandoffComplete{Generation: 1,
					Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-1"}, engine)
				if err != nil || result.Refusal != nil {
					t.Fatalf("the reviewer's completion = %+v, %v; want it recorded", result.Refusal, err)
				}
			}
			if tc.completeFirst {
				complete()
				if got := issuePhase(t, pool); got != phase.Reviewing {
					t.Fatalf("after the completion alone the issue is in %s, want reviewing until GitHub reports the decision", got)
				}
				review()
			} else {
				review()
				if got := issuePhase(t, pool); got != tc.want {
					t.Fatalf("after the review alone the issue is in %s, want %s: the review event decides", got, tc.want)
				}
				complete()
			}

			if got := issuePhase(t, pool); got != tc.want {
				t.Fatalf("the issue is in %s, want %s", got, tc.want)
			}
			var commit string
			if err := pool.QueryRow(ctx, "select handoff_commit from phases where issue = $1 and role = $2",
				"LEGION-208", "reviewer").Scan(&commit); err != nil {
				t.Fatal(err)
			}
			if commit != "review-1" {
				t.Fatalf("the reviewer's handoff commit = %q, want its completion's review-1", commit)
			}
			if got := implementerRounds(t, pool); got != tc.rounds {
				t.Fatalf("review rounds = %d, want %d: only a request for changes ends a round", got, tc.rounds)
			}
		})
	}
}

// An approval moves the issue on only once GitHub reports the head one the issue may advance on
// (record.MergeState.Mergeable). While GitHub has given no answer for the head - it has never been
// read, or GitHub answers `unknown` because it is still computing the merge - the round waits
// where it stands and nobody is told: the answer is about GitHub, not about the head, and the
// read that finds the state clean is what moves the issue to accepting.
func TestAnApprovalWaitsWhileGitHubHasNotAnsweredForTheHead(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state record.MergeState
	}{
		{"never read", record.MergeStateUnread},
		{"GitHub still computing it", record.MergeStateUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedMergeStateRound(t, pool, tc.state)
			apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))

			apply("approve", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: "head",
				HeadSHA: "head", Author: testReviewApp, Body: "ship it"})
			if got := issuePhase(t, pool); got != phase.Reviewing {
				t.Fatalf("after the approval the issue is in %s, want reviewing: GitHub has not answered for the head", got)
			}
			if result := apply("complete", intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer,
				Claim: "review-claim", Summary: "reviewed", Commit: "review-1"}); result.Refusal != nil {
				t.Fatalf("the reviewer's completion = %+v, want it recorded", result.Refusal)
			}
			if got := issuePhase(t, pool); got != phase.Reviewing {
				t.Fatalf("after the completion the issue is in %s, want reviewing: GitHub has still not answered", got)
			}
			if told := reviewStuckNotices(t, pool); len(told) != 0 {
				t.Fatalf("the architect was told the round was stuck %+v, want nothing: an approval waiting for GitHub's answer is not stuck", told)
			}

			apply("merge-state", intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "main", HeadSHA: "head",
				Mergeable: record.MergeabilityMergeable, MergeState: record.MergeStateClean})
			if got := issuePhase(t, pool); got != phase.Accepting {
				t.Fatalf("after GitHub reported the head clean the issue is in %s, want accepting", got)
			}
			if told := reviewStuckNotices(t, pool); len(told) != 0 {
				t.Fatalf("the architect was told the round was stuck %+v, want nothing", told)
			}
		})
	}
}

// GitHub answers `mergeable: true` for a head it holds blocked on a check or a review the
// repository requires just as it does for one it would merge now, so the merge state is the only
// answer that tells the two apart. An approval standing on a head GitHub answers for with a state
// it would not merge from moves nothing, and nothing the daemon does would ever move it: only a
// push that earns what the base requires can. So the tree's architect is told, in the round's
// review-stuck notice naming the pull request, the head and the state GitHub gave - once for that
// head, however many facts ask the round again: the reviewer's completion after it, and every
// further read of the same head that answers the same, tell nothing more. The notice names no role
// topic: the decision is not the reviewer's to give.
func TestAnApprovalOnAHeadGitHubWillNotMergeTellsTheArchitectOnce(t *testing.T) {
	pool := migratedPool(t)
	seedMergeStateRound(t, pool, record.MergeStateBlocked)
	apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))

	apply("approve", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: "head",
		HeadSHA: "head", Author: testReviewApp, Body: "ship it"})
	if got := issuePhase(t, pool); got != phase.Reviewing {
		t.Fatalf("after the approval of a blocked head the issue is in %s, want reviewing: GitHub would not merge the head", got)
	}
	told := reviewStuckNotices(t, pool)
	if len(told) != 1 || !strings.Contains(told[0].Reason, "approved head head on pull request #42") ||
		!strings.Contains(told[0].Reason, "GitHub reports that head blocked") || told[0].Topic != "" {
		t.Fatalf("review-stuck notices %+v, want one naming the pull request, the head and blocked, with no role topic", told)
	}

	if result := apply("complete", intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer,
		Claim: "review-claim", Summary: "reviewed", Commit: "review-1"}); result.Refusal != nil {
		t.Fatalf("the reviewer's completion = %+v, want it recorded", result.Refusal)
	}
	apply("second-read", intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "main", HeadSHA: "head",
		Mergeable: record.MergeabilityMergeable, MergeState: record.MergeStateBlocked})
	if got := issuePhase(t, pool); got != phase.Reviewing {
		t.Fatalf("after the second read the issue is in %s, want reviewing: the head is still blocked", got)
	}
	if again := reviewStuckNotices(t, pool); len(again) != 1 {
		t.Fatalf("the architect was told %d times (%+v), want once for the head", len(again), again)
	}

	apply("merge-state", intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "main", HeadSHA: "head",
		Mergeable: record.MergeabilityMergeable, MergeState: record.MergeStateClean})
	if got := issuePhase(t, pool); got != phase.Accepting {
		t.Fatalf("after GitHub reported the head clean the issue is in %s, want accepting", got)
	}
	if again := reviewStuckNotices(t, pool); len(again) != 1 {
		t.Fatalf("the architect was told %d times (%+v), want the one notice of the blocked head", len(again), again)
	}
}

// A merge state is GitHub's answer about the head as GitHub holds it now, and it changes under a
// head nobody pushed to: a check the base requires that has not run reads `blocked`, and the same
// head reads `clean` once it runs. So the stuck round an approval on a blocked head leaves is
// there to stop a second notice, never to stop the move: the read that answers clean for that very
// head ends the round and moves the issue to accepting, as it would have had GitHub answered clean
// the first time. The architect and the reviewer are told once, when the head was blocked, and the
// clean read tells neither of them again - it is the answer they were waiting for, not news of a
// round that goes nowhere.
func TestAnApprovalOnABlockedHeadGitHubLaterReportsCleanMovesOnAndTellsOnce(t *testing.T) {
	pool := migratedPool(t)
	seedMergeStateRound(t, pool, record.MergeStateUnread)
	apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))

	apply("approve", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: "head",
		HeadSHA: "head", Author: testReviewApp, Body: "ship it"})
	if got := issuePhase(t, pool); got != phase.Reviewing {
		t.Fatalf("after the approval the issue is in %s, want reviewing: GitHub has not answered for the head", got)
	}

	apply("blocked", intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "main", HeadSHA: "head",
		Mergeable: record.MergeabilityMergeable, MergeState: record.MergeStateBlocked})
	if got := issuePhase(t, pool); got != phase.Reviewing {
		t.Fatalf("after GitHub reported the head blocked the issue is in %s, want reviewing: GitHub would not merge the head", got)
	}
	told := reviewStuckNotices(t, pool)
	if len(told) != 1 || !strings.Contains(told[0].Reason, "approved head head on pull request #42") ||
		!strings.Contains(told[0].Reason, "GitHub reports that head blocked") {
		t.Fatalf("review-stuck notices %+v, want one naming the pull request, the head and blocked", told)
	}
	stuck := []record.RoleMessage{{Role: treeRoleToken(t, "LEGION-208", claim.RoleReviewer),
		Body: "The round you approved is stuck: " + told[0].Reason + "."}}
	assertRoleMessages(t, pool, "LEGION-208", stuck)

	apply("clean", intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "main", HeadSHA: "head",
		Mergeable: record.MergeabilityMergeable, MergeState: record.MergeStateClean})
	if got := issuePhase(t, pool); got != phase.Accepting {
		t.Fatalf("after GitHub reported the same head clean the issue is in %s, want accepting: the round recorded stuck stops a second notice, not the approval", got)
	}
	if again := reviewStuckNotices(t, pool); len(again) != 1 {
		t.Fatalf("the architect was told %d times (%+v), want the one notice of the blocked head: the clean read is the answer it waited for", len(again), again)
	}
	assertRoleMessages(t, pool, "LEGION-208", stuck)
}

// The notice of a head GitHub will not merge from is read by a person deciding what to push next,
// so it names GitHub's state and every check GitHub reported on that head with its conclusion.
// The daemon reads no repository ruleset (dispatch://INFRA-714/spec R1), so it cannot name the
// checks the base requires: a required check that never ran has no check run at all and is simply
// absent from the list, which the notice says in words, so a short list reads as the finding
// rather than as the daemon leaving something out. The reviewer is told as well as the architect,
// since it approved that head and is the role that reads the head's checks on GitHub; both are
// told once for the head and the state.
func TestAnApprovalOnABlockedHeadNamesTheStateAndEveryCheckGitHubReported(t *testing.T) {
	pool := migratedPool(t)
	seedMergeStateRound(t, pool, record.MergeStateBlocked)
	// GitHub settled the head reporting the one check that ran. The check the base requires and
	// that never ran has no check run of its own, so it is in nothing GitHub reported.
	blocked := record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
		Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", Failing: []string{}, Cancelled: []string{},
		Mergeability: record.MergeabilityMergeable, MergeState: record.MergeStateBlocked,
		CheckedHead: "head", CheckRuns: []record.AttemptRun{{Name: "unit tests", ID: 1}}}
	seedPR(t, pool, blocked)
	apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))

	apply("approve", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: "head",
		HeadSHA: "head", Author: testReviewApp, Body: "ship it"})

	want := stuckMergeStateReason(blocked)
	told := reviewStuckNotices(t, pool)
	if len(told) != 1 || told[0].Reason != want {
		t.Fatalf("review-stuck notices %+v, want one reading %q", told, want)
	}
	for _, phrase := range []string{"GitHub reports that head blocked", `"unit tests" succeeded`, "reads no ruleset", "absent from the list above"} {
		if !strings.Contains(told[0].Reason, phrase) {
			t.Fatalf("the notice reads %q, want it to name %q", told[0].Reason, phrase)
		}
	}
	assertRoleMessages(t, pool, "LEGION-208", []record.RoleMessage{
		{Role: treeRoleToken(t, "LEGION-208", claim.RoleReviewer), Body: "The round you approved is stuck: " + want + "."},
	})

	// A second read of the same head answering the same state tells neither of them again.
	apply("second-read", intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "main", HeadSHA: "head",
		Mergeable: record.MergeabilityMergeable, MergeState: record.MergeStateBlocked})
	if again := reviewStuckNotices(t, pool); len(again) != 1 {
		t.Fatalf("the architect was told %d times (%+v), want once for the head and state", len(again), again)
	}
	assertRoleMessages(t, pool, "LEGION-208", []record.RoleMessage{
		{Role: treeRoleToken(t, "LEGION-208", claim.RoleReviewer), Body: "The round you approved is stuck: " + want + "."},
	})

	// GitHub answering the same head `behind` is a different finding, and is told once in its turn.
	apply("behind", intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "main", HeadSHA: "head",
		Mergeable: record.MergeabilityMergeable, MergeState: record.MergeStateBehind})
	behind := blocked
	behind.MergeState = record.MergeStateBehind
	if again := reviewStuckNotices(t, pool); len(again) != 2 || again[1].Reason != stuckMergeStateReason(behind) {
		t.Fatalf("review-stuck notices %+v, want a second naming behind", again)
	}
}

// seedMergeStateRound seeds LEGION-208 in reviewing, its pull request at head with GitHub's
// mergeability read mergeable and its merge state state, and its reviewer's round not yet decided.
func seedMergeStateRound(t *testing.T, pool *pgxpool.Pool, state record.MergeState) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
		Phase: phase.Reviewing, Generation: 1, Status: "needs_review", Rank: "U"})
	pr := record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
		Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", Failing: []string{},
		Mergeability: record.MergeabilityMergeable, MergeState: state}
	if state == record.MergeStateUnread {
		// A head GitHub has never been read for has neither answer recorded, where seedPR's
		// default clean is for the tests that are about something else.
		pr.Mergeability = ""
	}
	seedPR(t, pool, pr)
	if state == record.MergeStateUnread {
		if _, err := pool.Exec(t.Context(), "update pull_requests set mergeability = '', merge_state = '' where issue = $1", pr.Issue); err != nil {
			t.Fatalf("clear GitHub's answer on the seeded pull request: %v", err)
		}
	}
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
	seedClaimSession(t, pool, "review-claim", "")
}

// GitHub's answer for a head is an answer about the tree that head carries, so it is kept across
// a push that changed only the issue's own handoffs and dropped across one that may have changed
// code (classify's advance). The reviewer's own handoff push is the first: it carries the
// skip-checks trailer, GitHub starts no workflow on the head it leaves and so reports it blocked
// on the checks the base requires, and the head the reviewer approved - the one with the checks -
// is the head GitHub answered clean for. Without the carry no approval would ever reach accepting
// over that push. A code push is the second: it changes what GitHub would check, so the recorded
// answer says nothing about the new head, and the round waits for GitHub's next read rather than
// advancing on the old one.
func TestGitHubsAnswerIsCarriedAcrossAHandoffPushAndDroppedAcrossACodePush(t *testing.T) {
	for _, tc := range []struct {
		name    string
		paths   string
		want    phase.Phase
		carried record.MergeState
	}{
		{name: "the reviewer's handoff push", paths: ".legion/LEGION-208/review.json", want: phase.Accepting, carried: record.MergeStateClean},
		{name: "a code push", paths: ".legion/LEGION-208/review.json\nsrc/widget.go", want: phase.Reviewing, carried: record.MergeStateUnread},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedMergeStateRound(t, pool, record.MergeStateClean)
			apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
			truncated := "false"
			forced := "false"

			apply("push", intake.Push{Repo: "sjawhar/legion", Branch: "legion/LEGION-208", Before: "head", After: "head-2",
				ChangedPaths: &tc.paths, Truncated: &truncated, Forced: &forced, Pusher: "legion-reviewer[bot]"})
			apply("sync", intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head-2"})
			if got := recordedPR(t, pool, "LEGION-208"); got.MergeState != tc.carried {
				t.Fatalf("after the push the recorded merge state is %q, want %q", got.MergeState, tc.carried)
			}

			// The approval names the head the reviewer was shown; over the handoff push it is the
			// head before it, and over the code push it is the new head, which GitHub's recorded
			// answer was never about.
			approved := "head"
			if tc.want == phase.Reviewing {
				approved = "head-2"
			}
			apply("approve", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: approved,
				HeadSHA: approved, Author: testReviewApp, Body: "ship it"})
			if got := issuePhase(t, pool); got != tc.want {
				t.Fatalf("after the approval the issue is in %s, want %s", got, tc.want)
			}
			if told := reviewStuckNotices(t, pool); len(told) != 0 {
				t.Fatalf("the architect was told the round was stuck %+v, want nothing: GitHub answered nothing against the head", told)
			}
			if tc.want == phase.Accepting {
				return
			}

			apply("merge-state", intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "main", HeadSHA: "head-2",
				Mergeable: record.MergeabilityMergeable, MergeState: record.MergeStateClean})
			if got := issuePhase(t, pool); got != phase.Accepting {
				t.Fatalf("after GitHub answered for the new head the issue is in %s, want accepting", got)
			}
		})
	}
}

// A read of GitHub's merge state is one round trip outside the record's transaction, so a push can
// land between GitHub's answer and the apply. A read that answers for a head the record has moved
// past is dropped whole: it records neither verdict, and it decides nothing - not even the
// conflict that sends an issue back to implementing from any phase, since the head that conflicts
// is not the head the branch now carries.
func TestAMergeStateReadForAHeadTheRecordHasMovedPastIsDropped(t *testing.T) {
	pool := migratedPool(t)
	seedMergeStateRound(t, pool, record.MergeStateClean)
	apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
	paths, truncated, forced := ".legion/LEGION-208/review.json", "false", "false"
	apply("push", intake.Push{Repo: "sjawhar/legion", Branch: "legion/LEGION-208", Before: "head", After: "head-2",
		ChangedPaths: &paths, Truncated: &truncated, Forced: &forced, Pusher: "legion-reviewer[bot]"})
	apply("sync", intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head-2"})

	apply("stale-read", intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "other", HeadSHA: "head",
		Mergeable: record.MergeabilityConflicting, MergeState: record.MergeStateDirty})
	got := recordedPR(t, pool, "LEGION-208")
	if got.Mergeability != record.MergeabilityMergeable || got.MergeState != record.MergeStateClean || got.Base != "" {
		t.Fatalf("after the stale read the record holds %q/%q on base %q, want GitHub's answer for the head it stands at",
			got.Mergeability, got.MergeState, got.Base)
	}
	if got := issuePhase(t, pool); got != phase.Reviewing {
		t.Fatalf("after the stale read the issue is in %s, want reviewing: the conflict was of a head the branch has left", got)
	}
}

// An approval of the head ends the round whatever CI says at that head: red, green, or no verdict
// yet. The round reads no CI verdict - the reviewer reads the code commit's checks on GitHub before
// it approves, and a red at the head is the daemon's separate rule, acted on by the CI fact that
// makes it stand - so the issue moves to accepting by the review's approval in either order of the
// round's two halves, with no checks-red told and no review-stuck notice but the one a completion
// before its decision always writes.
func TestAnApprovalEndsTheRoundWhateverCISays(t *testing.T) {
	for _, verdict := range []string{"red", "green", "undecided"} {
		for _, completeFirst := range []bool{false, true} {
			name := verdict + ", the approval then the completion"
			if completeFirst {
				name = verdict + ", the completion then the approval"
			}
			t.Run(name, func(t *testing.T) {
				pool := migratedPool(t)
				if verdict == "undecided" {
					seedReviewUndecided(t, pool, "head")
				} else {
					seedReviewOf(t, pool, "head", verdict)
				}
				apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
				approve := intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: "head", Author: testReviewApp,
					Body: "ship it", SubmittedAt: time.Date(2026, 9, 22, 23, 59, 0, 0, time.UTC)}
				complete := intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-1"}
				halves := []intake.Fact{approve, complete}
				if completeFirst {
					halves[0], halves[1] = halves[1], halves[0]
				}
				for i, half := range halves {
					if result := apply(fmt.Sprintf("half-%d", i), half); result.Refusal != nil {
						t.Fatalf("half %d (%T) was refused: %+v", i, half, result.Refusal)
					}
				}
				if got := issuePhase(t, pool); got != phase.Accepting {
					t.Fatalf("the issue is in %s, want accepting: the approval ends the round whatever CI says", got)
				}
				kinds := noticeKinds(t, pool, "LEGION-208")
				if !containsNotice(kinds, "phase-finished") || containsNotice(kinds, "checks-red") {
					t.Fatalf("notices %v, want the reviewer's phase-finished and no checks-red", kinds)
				}
				told, want := reviewStuckNotices(t, pool), 0
				if completeFirst {
					// The completion before the decision leaves the round undecided until the
					// approval lands, as it always has; the approval tells nothing more.
					want = 1
				}
				if len(told) != want {
					t.Fatalf("the architect was told the round was stuck %d times (%+v), want %d", len(told), told, want)
				}
			})
		}
	}
}

// seedReviewUndecided is seedReviewOf with a settlement in that reports one check, lint, and
// names none failing: the round reads none of it either way (dispatch://INFRA-714/spec R1).
func seedReviewUndecided(t *testing.T, pool *pgxpool.Pool, head string) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
		Phase: phase.Reviewing, Generation: 1, Status: "needs_review", Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
		Number: 42, Branch: "legion/LEGION-208", HeadSHA: head, CheckedHead: head, Failing: []string{},
		CheckRuns: []record.AttemptRun{{Name: "lint", ID: 1}}})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
	seedClaimSession(t, pool, "review-claim", "")
}

func seedReview(t *testing.T, pool *pgxpool.Pool, verdict string) {
	t.Helper()
	seedReviewOf(t, pool, "head", verdict)
}

// seedReviewOf seeds LEGION-208 in reviewing, its pull request's head at head with the settlement
// verdict names on it ("green", "red", or "" for none yet), and its reviewer's round not yet
// completed. The settlement reports the one check ci.
func seedReviewOf(t *testing.T, pool *pgxpool.Pool, head, verdict string) {
	t.Helper()
	seedReviewReporting(t, pool, head, verdict, "ci")
}

// seedReviewReporting is seedReviewOf whose settlement reports the checks named: it ran each of
// them, and in a red one each failed.
func seedReviewReporting(t *testing.T, pool *pgxpool.Pool, head, verdict string, checks ...string) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
		Phase: phase.Reviewing, Generation: 1, Status: "needs_review", Rank: "U"})
	pr := record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
		Number: 42, Branch: "legion/LEGION-208", HeadSHA: head}
	if verdict != "" {
		pr.CheckedHead = head
		for i, name := range checks {
			pr.CheckRuns = append(pr.CheckRuns, record.AttemptRun{Name: name, ID: int64(i + 1)})
		}
	}
	if verdict == "red" {
		pr.Failing = append([]string{}, checks...)
	}
	seedPR(t, pool, pr)
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
	seedClaimSession(t, pool, "review-claim", "")
}

func issuePhase(t *testing.T, pool *pgxpool.Pool) phase.Phase {
	t.Helper()
	var got string
	if err := pool.QueryRow(context.Background(), "select phase from issues where key = $1", "LEGION-208").Scan(&got); err != nil {
		t.Fatal(err)
	}
	return phase.Phase(got)
}

// An approval is of the code the head it names carries. The reviewer approves the head it was
// shown - the tester's handoff head - and its own handoff push then replaces that head, while the
// approval event and the push arrive by different webhook paths in no promised order. So an
// approval stands for the current head when every push since the head it names changed only
// .legion/, whichever of the approval, the new heads and the pushes' paths is processed first. A
// push that changes code, whose changed paths are not known, or that rewrote history - or did not
// say whether it did - carries no approval across. Deciding reviews are ordered by when they were
// submitted, then by GitHub's review id, so the newest one submitted is the one the round records;
// a review without a submission time is ordered by id against any other. That ordering decides the
// round while it is open: the review event moves the issue as it lands
// (dispatch://INFRA-714/spec R1), so a second deciding review, however it is ordered, finds the
// issue in the phase the first sent it to and decides nothing. A pull request recorded before the
// daemon kept its pushes has none, so only an approval of its current head stands. Only the review
// App's reviews and those of an account with write access to the repository decide anything: an
// outsider's approval neither overrides the reviewer's request for changes nor, delivered first,
// makes it look old.
func TestAnApprovalStandsForEveryHeadThatChangesNothingButTheHandoff(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steps []string
		want  phase.Phase
		// unsettled seeds the approved head with its checks still running, rather than green.
		unsettled bool
		// quiet is a round the architect is told nothing of: no review-stuck notice.
		quiet bool
		// told is what the round's one review-stuck notice names, when it has one.
		told string
	}{
		{name: "approval, then the reviewer's handoff push", steps: []string{"approve head", "sync", "push handoff", "green", "complete"}, want: phase.Accepting},
		{name: "the reviewer's handoff push, then the approval", steps: []string{"sync", "push handoff", "green", "approve head", "complete"}, want: phase.Accepting},
		{name: "the push's paths arrive after the new head and the approval", steps: []string{"sync", "approve head", "green", "complete", "push handoff"}, want: phase.Accepting},
		{name: "the push's paths arrive before the new head", steps: []string{"push handoff", "approve head", "sync", "green", "complete"}, want: phase.Accepting},
		{name: "a new head whose push never arrives", steps: []string{"sync", "approve head", "green", "complete"}, want: phase.Reviewing},
		{name: "a code push, then the approval of the head before it", steps: []string{"sync", "push code", "green", "approve head", "complete"}, want: phase.Reviewing},
		{name: "the approval of the head, then a code push: the round had already ended", steps: []string{"approve head", "sync", "push code", "green", "complete"}, want: phase.Accepting},
		{name: "a push of truncated paths, then the approval of the head before it", steps: []string{"sync", "push truncated", "green", "approve head", "complete"}, want: phase.Reviewing},
		{name: "a push with no paths marker, then the approval of the head before it", steps: []string{"sync", "push paths unmarked", "green", "approve head", "complete"}, want: phase.Reviewing},
		{name: "a comment between a code push and its new head", steps: []string{"push code", "approve head", "comment", "sync", "green", "complete"}, want: phase.Reviewing},
		{name: "a comment on the handoff head after the approval", steps: []string{"approve head", "sync", "push handoff", "comment", "green", "complete"}, want: phase.Accepting},
		{name: "a code push whose new head has not arrived", steps: []string{"push code", "approve head", "complete"}, want: phase.Reviewing},
		{name: "the round ends on its approval of the head before a code push arrives", steps: []string{"approve head", "complete", "push code", "green head"}, want: phase.Accepting, unsettled: true},
		{name: "a code push, then a handoff push on top of it, neither head arrived", steps: []string{"push code", "push handoff head-3", "approve head", "complete"}, want: phase.Reviewing},
		{name: "a code push that does not say which head it replaced", steps: []string{"push code unplaced", "approve head", "complete"}, want: phase.Reviewing},
		{name: "a code push that did not say which head it replaced, then the branch moves on without it", steps: []string{"approve head", "sync", "push handoff", "push code unplaced head-x", "sync head-3", "push handoff head-3", "green head-3", "complete"}, want: phase.Accepting},
		{name: "a code head never synchronized, under a handoff head, pushes first", steps: []string{"push code", "push handoff head-3", "sync head-3", "green head-3", "approve head", "complete"}, want: phase.Reviewing},
		{name: "a code head never synchronized, under a handoff head, the head first", steps: []string{"sync head-3", "push code", "push handoff head-3", "green head-3", "approve head", "complete"}, want: phase.Reviewing},
		{name: "a late code push for a head already replaced", steps: []string{"approve head", "sync", "push handoff", "push code head-x", "green", "complete"}, want: phase.Accepting},
		{name: "two handoff pushes, each before its head", steps: []string{"approve head", "push handoff", "sync", "push handoff head-3", "sync head-3", "green head-3", "complete"}, want: phase.Accepting},
		{name: "two handoff pushes, each after its head", steps: []string{"approve head", "sync", "push handoff", "sync head-3", "push handoff head-3", "green head-3", "complete"}, want: phase.Accepting},
		{name: "two handoff pushes, both classified after both heads, the second first", steps: []string{"approve head", "sync", "sync head-3", "push handoff head-3", "push handoff", "green head-3", "complete"}, want: phase.Accepting},
		{name: "two handoff pushes, both classified after both heads, in order", steps: []string{"approve head", "sync", "sync head-3", "push handoff", "push handoff head-3", "green head-3", "complete"}, want: phase.Accepting},
		{name: "two handoff pushes, the second classified between the heads", steps: []string{"approve head", "sync", "push handoff head-3", "sync head-3", "push handoff", "green head-3", "complete"}, want: phase.Accepting},
		{name: "a force push to a handoff commit on an older base, its push first", steps: []string{"push handoff forced head-l", "approve head", "sync head-l", "green head-l", "complete"}, want: phase.Reviewing},
		{name: "a force push to a handoff commit on an older base, its head first", steps: []string{"sync head-l", "approve head", "push handoff forced head-l", "green head-l", "complete"}, want: phase.Reviewing},
		{name: "a handoff push that does not say whether it was forced", steps: []string{"sync", "approve head", "push handoff unmarked", "green", "complete"}, want: phase.Reviewing},
		{name: "a request for changes written after the approval, delivered first", steps: []string{"cr head id=12", "approve head id=11", "complete"}, want: phase.Implementing},
		{name: "a request for changes written after the approval, delivered after it moved the issue", steps: []string{"approve head id=11", "cr head id=12", "complete"}, want: phase.Accepting},
		{name: "a comment written after a request for changes, delivered first", steps: []string{"comment id=12", "cr head id=11", "complete"}, want: phase.Implementing},
		{name: "a comment written after an approval, delivered first", steps: []string{"comment id=12", "approve head id=11", "complete"}, want: phase.Accepting},
		{name: "the reviewer's approval written after its request for changes, which moved the issue first", steps: []string{"cr head id=11", "approve head id=12", "complete"}, want: phase.Implementing},
		{name: "an outsider's approval written after the reviewer's request for changes", steps: []string{"cr head id=11", "approve head id=12 by=outsider", "complete"}, want: phase.Implementing},
		{name: "an outsider's approval written after the reviewer's request for changes, delivered first", steps: []string{"approve head id=12 by=outsider", "cr head id=11", "complete"}, want: phase.Implementing},
		{name: "an outsider's approval the only review of the round", steps: []string{"approve head id=12 by=outsider", "complete"}, want: phase.Reviewing, told: "no review that decides it"},
		{name: "a request for changes by an account with write access, delivered after the approval moved the issue", steps: []string{"approve head id=11", "cr head id=12 by=writer", "complete"}, want: phase.Accepting},
		{name: "an approval by an account with write access, delivered after the request for changes moved the issue", steps: []string{"cr head id=11", "approve head id=12 by=writer", "complete"}, want: phase.Implementing},
		{name: "a draft's request for changes submitted after a one-step approval that moved the issue", steps: []string{"approve head id=101 at=2", "cr head id=100 at=3", "complete"}, want: phase.Accepting},
		{name: "a draft's request for changes submitted after a one-step approval, delivered first", steps: []string{"cr head id=100 at=3", "approve head id=101 at=2", "complete"}, want: phase.Implementing},
		{name: "a draft's approval submitted after a request for changes that moved the issue", steps: []string{"cr head id=101 at=2", "approve head id=100 at=3", "complete"}, want: phase.Implementing},
		{name: "an untimed request for changes, then an older approval redelivered with a time", steps: []string{"cr head id=12", "approve head id=11 at=2", "complete"}, want: phase.Implementing},
		{name: "a timed approval, then a newer untimed request for changes", steps: []string{"approve head id=11 at=2", "cr head id=12", "complete"}, want: phase.Accepting},
		// The code push head..head-2 arrives after head-2 arrived and the branch was reset to head: it
		// is the move the record already made, spent as it arrives (classify.unspentPushes), not code
		// on its way. A second push of the same two heads looks the same until its synchronize moves
		// the head; meanwhile no merge can land it, since the READY merge carries the approved head as
		// GitHub's expected head (daemon's TestAREADYMergeNeverLandsAHeadReplacedAfterTheREADY).
		{name: "a code push delivered after the branch was reset to the approved head", steps: []string{"approve head", "sync", "sync head-3", "push code", "push handoff forced from=head-3 head", "sync head", "green head", "complete"}, want: phase.Accepting},
		{name: "a code push delivered after the branch was reset to the approved head, then the reviewer's handoff push", steps: []string{"approve head", "sync", "sync head-3", "push code", "push handoff forced from=head-3 head", "sync head", "green head", "complete", "push handoff from=head head-4", "sync head-4", "green head-4"}, want: phase.Accepting},
		// A handoff push can carry GitHub's skip-checks trailer and start no CI, so the code head's
		// settlement stands for the handoff head that replaced it, whenever it arrives. A required
		// check cancelled, whether in the handoff head's own run or in the code head's carried to it,
		// is not red: a push or concurrency can cancel a run whose replacement has not shown yet, so
		// a later settlement decides it. None of it holds the round or tells anybody: the approval of
		// the head ends the round with the verdict still open, and a red that later stands for the
		// head is the CI rule's, sent back from wherever the issue then is.
		{name: "the handoff head's own run cancelled after the code head's green", steps: []string{"green head", "approve head", "sync", "push handoff", "cancelled", "complete"}, want: phase.Accepting, unsettled: true, quiet: true},
		{name: "a cancelled re-settlement of the code head after its green", steps: []string{"green head", "approve head", "sync", "push handoff", "cancelled head", "complete"}, want: phase.Accepting, unsettled: true, quiet: true},
		{name: "the code head's run cancelled by the reviewer's handoff push", steps: []string{"approve head", "sync", "push handoff", "cancelled head", "complete"}, want: phase.Accepting, unsettled: true, quiet: true},
		{name: "the handoff head's own run cancelled before the code head's green", steps: []string{"approve head", "sync", "push handoff", "cancelled", "green head", "complete"}, want: phase.Accepting, unsettled: true, quiet: true},
		// A code push since the approval leaves it standing for nothing, whatever the new head's
		// own settlement left pending: the round is told once that the approval does not approve the
		// head, never that it stands, and the head's later green tells nothing more.
		{name: "an approval, a code push, then the new head's own pending settlement and its green", steps: []string{"sync", "push code", "approve head", "cancelled", "complete", "rerun green"}, want: phase.Reviewing, unsettled: true, told: "does not approve head head-2"},
		{name: "the code head's checks settle after the reviewer's handoff head", steps: []string{"approve head", "sync", "push handoff", "green head", "complete"}, want: phase.Accepting, unsettled: true},
		{name: "the code head's checks settle before the reviewer's handoff head, its push last", steps: []string{"green head", "approve head", "sync", "push handoff", "complete"}, want: phase.Accepting, unsettled: true},
		{name: "the code head's checks settle before the reviewer's handoff head, its push first", steps: []string{"green head", "approve head", "push handoff", "sync", "complete"}, want: phase.Accepting, unsettled: true},
		{name: "the code head's checks settle under two handoff heads", steps: []string{"approve head", "sync", "push handoff", "sync head-3", "push handoff head-3", "green head", "complete"}, want: phase.Accepting, unsettled: true},
		{name: "the checks of a head a code push replaced", steps: []string{"sync", "push code", "approve head", "green head", "complete"}, want: phase.Reviewing, unsettled: true},
		// The approval is of head-2, which the code push left, so GitHub's answer for the head
		// before it says nothing about this one and the read of head-2 is what the round advances
		// on (classify's advance, the mergeability handler).
		{name: "a late settlement of a head a code push replaced leaves the head's own", steps: []string{"sync", "push code", "green", "red head", "read head-2", "approve", "complete"}, want: phase.Accepting, unsettled: true},
		{name: "the checks of a head a force push replaced", steps: []string{"sync head-l", "approve head", "push handoff forced head-l", "green head", "complete"}, want: phase.Reviewing, unsettled: true},
		// The handoff head's own settlement outranks the code head's late one
		// (classify.SettlementFor), and neither moves the issue: the round is the reviewer's to
		// decide, and a CI result is told to the roles and read by them
		// (dispatch://INFRA-714/spec R1).
		{name: "the handoff head's own failed check outranks the code head's late green", steps: []string{"approve head", "sync", "push handoff", "red", "green head", "complete"}, want: phase.Accepting, unsettled: true},
		{name: "the handoff head's own green outranks the code head's late failed check", steps: []string{"approve head", "sync", "push handoff", "green", "red head", "complete"}, want: phase.Accepting, unsettled: true},
		{name: "recorded before the chain: an approval of the current head", steps: []string{"approve head", "complete"}, want: phase.Accepting},
		{name: "recorded before the chain: an approval of an earlier head", steps: []string{"approve older", "complete"}, want: phase.Reviewing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			ctx := context.Background()
			verdict := "green"
			if tc.unsettled {
				verdict = ""
			}
			seedReview(t, pool, verdict)
			engine := testEngine(config.DesignGateRootIssues, nil)
			// A head's push replaces the head before it; head-x is a late push's, whose head was
			// never the current one.
			before := map[string]string{"head-2": "head", "head-3": "head-2", "head-x": "older", "head-l": "head"}
			for i, step := range tc.steps {
				// A step is its name, then optional words: a head it names (head, head-2, ...), a
				// review id (id=N), a review's submission time (at=N, minutes past noon), a review's
				// author when it is not the review App (by=outsider, an account with no write access
				// to the repository, or by=writer), the head a push replaced when the map below
				// does not say (from=H), "forced" for a push that rewrote history, and "unmarked" for a
				// push whose listener did not say. A "read" step is the daemon's read of GitHub's
				// merge state for the head it names, answering clean: the seeded record holds that
				// answer for its first head, and a code push drops it.
				head, id, forced, from, by := "head-2", int64(0), "false", "", ""
				var submitted time.Time
				var name []string
				for _, word := range strings.Fields(step) {
					switch {
					case strings.HasPrefix(word, "head"):
						head = word
					case strings.HasPrefix(word, "id="):
						id, _ = strconv.ParseInt(strings.TrimPrefix(word, "id="), 10, 64)
					case strings.HasPrefix(word, "at="):
						minutes, _ := strconv.Atoi(strings.TrimPrefix(word, "at="))
						submitted = time.Date(2026, 9, 26, 12, minutes, 0, 0, time.UTC)
					case strings.HasPrefix(word, "from="):
						from = strings.TrimPrefix(word, "from=")
					case strings.HasPrefix(word, "by="):
						by = strings.TrimPrefix(word, "by=")
					case word == "forced":
						forced = "true"
					case word == "unmarked":
						forced = ""
					default:
						name = append(name, word)
					}
				}
				step := strings.Join(name, " ")
				var fact intake.Fact
				switch step {
				case "approve", "approve older", "cr":
					commit, state := head, "approved"
					if step == "approve older" {
						commit = "older"
					}
					if step == "cr" {
						state = "changes_requested"
					}
					// The review App decides by its login; another account decides only with write
					// access to the repository, which intake read before the fact arrived.
					author, canWrite := testReviewApp, false
					switch by {
					case "outsider":
						author = "a-stranger"
					case "writer":
						author, canWrite = "a-writer", true
					}
					fact = intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: id, SubmittedAt: submitted, State: state,
						CommitID: commit, Author: author, AuthorCanWrite: canWrite, Body: step}
				case "sync":
					fact = intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: head}
				case "comment":
					fact = intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: id, State: "commented", CommitID: "head-2", Body: "a comment"}
				case "push handoff", "push code", "push code unplaced", "push truncated", "push paths unmarked":
					paths, truncated := ".legion/LEGION-208/review.json", "false"
					marker := &truncated
					switch step {
					case "push code", "push code unplaced":
						paths = ".legion/LEGION-208/review.json\nsrc/widget.go"
					case "push truncated":
						truncated = "true"
					case "push paths unmarked":
						marker = nil
					}
					var forcedMarker *string
					if forced != "" {
						forcedMarker = &forced
					}
					replaced := before[head]
					if from != "" {
						replaced = from
					}
					if step == "push code unplaced" {
						replaced = ""
					}
					fact = intake.Push{Repo: "sjawhar/legion", Branch: "legion/LEGION-208", Before: replaced, After: head,
						ChangedPaths: &paths, Truncated: marker, Forced: forcedMarker, Pusher: "legion-reviewer[bot]"}
				case "green":
					fact = intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: head,
						CheckRuns: []record.AttemptRun{{Name: "ci", ID: 2}}, Generation: 2, Snapshot: "green-" + head, Failing: []string{}}
				case "red":
					fact = intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: head,
						CheckRuns: []record.AttemptRun{{Name: "ci", ID: 3}}, Generation: 2, Snapshot: "red-" + head, Failing: []string{"ci"}}
				case "cancelled":
					// Every check ended cancelled and none failed: the listener's cancelled group.
					fact = intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: head,
						CheckRuns: []record.AttemptRun{{Name: "ci", ID: 4}}, Generation: 3, Snapshot: "cancelled-" + head, Failing: []string{}, Cancelled: []string{"ci"}}
				case "rerun green":
					// The run concurrency cancelled, run again and passed.
					fact = intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: head,
						CheckRuns: []record.AttemptRun{{Name: "ci", ID: 5}}, Generation: 4, Snapshot: "rerun-green-" + head, Failing: []string{}}
				case "complete":
					fact = intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim",
						Summary: "reviewed", Commit: "review-1"}
				case "read":
					fact = intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "main", HeadSHA: head,
						Mergeable: record.MergeabilityMergeable, MergeState: record.MergeStateClean}
				}
				result, err := intake.ApplyFact(ctx, pool, "test", fmt.Sprintf("%d-%s", i, step), fact, engine)
				if err != nil || result.Refusal != nil {
					t.Fatalf("%s = %+v, %v", step, result.Refusal, err)
				}
			}
			if got := issuePhase(t, pool); got != tc.want {
				t.Fatalf("the issue is in %s, want %s", got, tc.want)
			}
			got := reviewStuckNotices(t, pool)
			if tc.quiet && len(got) != 0 {
				t.Fatalf("the architect was told the round was stuck: %+v, want nothing", got)
			}
			if tc.told != "" && (len(got) != 1 || !strings.Contains(got[0].Reason, tc.told)) {
				t.Fatalf("review-stuck notices %+v, want one naming %q", got, tc.told)
			}
		})
	}
}

// A review that carries no decision - a comment - leaves the round's decision and its reason as
// they are: the next implementer is told what the request for changes said, not the comment
// posted after it.
func TestACommentLeavesTheRoundsRequestForChanges(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedReview(t, pool, "")
	engine := testEngine(config.DesignGateRootIssues, nil)
	for i, fact := range []intake.Fact{
		intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "changes_requested", CommitID: "head", Author: testReviewApp, Body: "rename the widget"},
		intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "commented", CommitID: "head", Body: "one more thought"},
		intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-1"},
	} {
		if result, err := intake.ApplyFact(ctx, pool, "test", fmt.Sprintf("step-%d", i), fact, engine); err != nil || result.Refusal != nil {
			t.Fatalf("step %d = %+v, %v", i, result.Refusal, err)
		}
	}
	if got := issuePhase(t, pool); got != phase.Implementing {
		t.Fatalf("the issue is in %s, want implementing", got)
	}
	var task string
	if err := pool.QueryRow(ctx, "select payload->>'task' from outbox where kind = 'supervise' and payload->>'op' = 'start' order by id desc limit 1").Scan(&task); err != nil {
		t.Fatalf("read the implementer's start task: %v", err)
	}
	if !strings.Contains(task, "rename the widget") || strings.Contains(task, "one more thought") {
		t.Fatalf("the implementer's task = %q, want the request for changes' body and not the comment's", task)
	}
}

// Deciding reviews are ordered by when they were submitted, then by GitHub's review id, and the
// order holds across rounds: a review from an earlier round, delivered again while a later round is
// open, is no newer than one already processed and records nothing, so the reviewer's completion
// waits for the round's own review.
func TestAReviewFromAnEarlierRoundDeliveredAgainRecordsNothing(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedReview(t, pool, "green")
	engine := testEngine(config.DesignGateRootIssues, nil)
	requestChanges := intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 10, State: "changes_requested", CommitID: "head", Author: testReviewApp, Body: "round 1"}
	for i, fact := range []intake.Fact{
		requestChanges,
		intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-1"},
		intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", Summary: "fixed", Commit: "round-2"},
		intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleTester, Claim: "test-claim", Summary: "tested", Verdict: "pass", Commit: "test-2"},
		requestChanges,
		intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-2"},
		intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 11, State: "approved", CommitID: "head", Author: testReviewApp, Body: "round 2"},
	} {
		if result, err := intake.ApplyFact(ctx, pool, "test", fmt.Sprintf("step-%d", i), fact, engine); err != nil || result.Refusal != nil {
			t.Fatalf("step %d = %+v, %v", i, result.Refusal, err)
		}
		switch got := issuePhase(t, pool); {
		case i == 1 && got != phase.Implementing:
			t.Fatalf("after round 1 the issue is in %s, want implementing", got)
		case i == 5 && got != phase.Reviewing:
			t.Fatalf("after round 1's review delivered again and round 2's completion the issue is in %s, want reviewing", got)
		}
	}
	if got := issuePhase(t, pool); got != phase.Accepting {
		t.Fatalf("the issue is in %s, want accepting: round 2's approval is the newest review", got)
	}
}

// A review that arrives once the round has ended - in accepting, after the approval moved the issue on
// - decides nothing and counts no round: the rounds and the pull request's blocked notice belong
// to the round that was open.
func TestAReviewOutsideReviewingRecordsNoRound(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedReview(t, pool, "green")
	engine := testEngine(config.DesignGateRootIssues, nil)
	for i, fact := range []intake.Fact{
		intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 11, State: "approved", CommitID: "head", Author: testReviewApp},
		intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-1"},
	} {
		if result, err := intake.ApplyFact(ctx, pool, "test", fmt.Sprintf("step-%d", i), fact, engine); err != nil || result.Refusal != nil {
			t.Fatalf("step %d = %+v, %v", i, result.Refusal, err)
		}
	}
	if got := issuePhase(t, pool); got != phase.Accepting {
		t.Fatalf("the issue is in %s, want accepting", got)
	}
	before := implementerRounds(t, pool)
	if _, err := intake.ApplyFact(ctx, pool, "test", "late", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 12,
		State: "changes_requested", CommitID: "head", Author: testReviewApp, Body: "too late"}, engine); err != nil {
		t.Fatalf("apply the late review: %v", err)
	}
	if got := issuePhase(t, pool); got != phase.Accepting {
		t.Fatalf("after the late review the issue is in %s, want accepting", got)
	}
	if got := implementerRounds(t, pool); got != before {
		t.Fatalf("the implementer's rounds went from %d to %d on a review outside reviewing", before, got)
	}
}

// A review can be processed while its issue is held from reviewing: the reviewer's launch budget
// ran out after it posted the review. That review still decides the round, so once the retry puts
// the issue back in reviewing, the reviewer's completion ends the round on it.
func TestAReviewWhileHeldFromReviewingDecidesTheRound(t *testing.T) {
	for _, tc := range []struct {
		state string
		want  phase.Phase
	}{
		{state: "approved", want: phase.Accepting},
		{state: "changes_requested", want: phase.Implementing},
	} {
		t.Run(tc.state, func(t *testing.T) {
			pool := migratedPool(t)
			ctx := context.Background()
			seedReview(t, pool, "green")
			engine := testEngine(config.DesignGateRootIssues, nil)
			for i, fact := range []intake.Fact{
				intake.ClaimFailed{Issue: "LEGION-208", Role: claim.RoleReviewer},
				intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 11, State: tc.state, CommitID: "head", Author: testReviewApp, Body: "the review"},
				intake.RetryOrEscalate{Issue: "LEGION-208", Decision: intake.RetryDecision},
				intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-1"},
			} {
				if i == 1 {
					if got := issuePhase(t, pool); got != phase.Held {
						t.Fatalf("before the review the issue is in %s, want held", got)
					}
				}
				result, err := intake.ApplyFact(ctx, pool, "test", fmt.Sprintf("held-%d", i), fact, engine)
				if err != nil || result.Refusal != nil {
					t.Fatalf("fact %d (%T) = %+v, %v", i, fact, result.Refusal, err)
				}
			}
			if got := issuePhase(t, pool); got != tc.want {
				t.Fatalf("the issue is in %s, want %s", got, tc.want)
			}
		})
	}
}

// Reviews are ordered across the pull request's whole life: reopening it keeps the newest deciding
// review it has had, so an earlier review delivered again after the reopen records nothing.
func TestAReopenedPullRequestKeepsItsNewestReview(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedReview(t, pool, "green")
	engine := testEngine(config.DesignGateRootIssues, nil)
	for i, fact := range []intake.Fact{
		intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 12, State: "approved", CommitID: "head", Author: testReviewApp, Body: "approved"},
		intake.PullRequestOpened{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head"},
		intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: "head", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 3}},
			Generation: 3, Snapshot: "green-again", Failing: []string{}},
		intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 11, State: "changes_requested", CommitID: "head", Author: testReviewApp, Body: "redelivered"},
		intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-1"},
	} {
		result, err := intake.ApplyFact(ctx, pool, "test", fmt.Sprintf("reopen-%d", i), fact, engine)
		if err != nil || result.Refusal != nil {
			t.Fatalf("fact %d (%T) = %+v, %v", i, fact, result.Refusal, err)
		}
	}
	if got := issuePhase(t, pool); got != phase.Accepting {
		t.Fatalf("the issue is in %s, want accepting: the redelivered request for changes is older than the approval", got)
	}
}

// The reviewer can complete before its review arrives and before the issue is held from reviewing:
// the review then lands while held, and the retry that restores reviewing finds both halves of the
// round already in. It ends the round there rather than starting the reviewer again, whose second
// completion of the same commit would be refused as not new and leave the issue in reviewing.
func TestARetryEndsARoundWhoseReviewerCompletedBeforeTheHold(t *testing.T) {
	for _, tc := range []struct {
		state  string
		want   phase.Phase
		rounds int
	}{
		{state: "approved", want: phase.Accepting},
		{state: "changes_requested", want: phase.Implementing, rounds: 1},
	} {
		t.Run(tc.state, func(t *testing.T) {
			pool := migratedPool(t)
			ctx := context.Background()
			seedReview(t, pool, "green")
			engine := testEngine(config.DesignGateRootIssues, nil)
			for i, fact := range []intake.Fact{
				intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-1"},
				intake.ClaimFailed{Issue: "LEGION-208", Role: claim.RoleReviewer},
				intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 11, State: tc.state, CommitID: "head", Author: testReviewApp, Body: "the review"},
				intake.RetryOrEscalate{Issue: "LEGION-208", Decision: intake.RetryDecision},
			} {
				result, err := intake.ApplyFact(ctx, pool, "test", fmt.Sprintf("early-%d", i), fact, engine)
				if err != nil || result.Refusal != nil {
					t.Fatalf("fact %d (%T) = %+v, %v", i, fact, result.Refusal, err)
				}
			}
			if got := issuePhase(t, pool); got != tc.want {
				t.Fatalf("the issue is in %s, want %s", got, tc.want)
			}
			var starts int
			if err := pool.QueryRow(ctx, `select count(*) from outbox where kind = 'supervise' and payload->>'op' = 'start'
				and payload->>'role' = 'reviewer' and payload->>'phase' = 'reviewing'`).Scan(&starts); err != nil {
				t.Fatal(err)
			}
			if starts != 0 {
				t.Fatalf("the retry started the reviewer %d times, want none: its round had already ended", starts)
			}
			if rounds := implementerRounds(t, pool); rounds != tc.rounds {
				t.Fatalf("the implementer's rounds = %d, want %d: the round ended once", rounds, tc.rounds)
			}
		})
	}
}

// A round the retry restores to reviewing that its reviewer completed before the hold, and that no
// review decides, is stuck as it was: the restarted reviewer's task says why, so it submits the
// decision rather than reviewing afresh. The architect ordered the retry, so it is told nothing new:
// the one review-stuck notice is the completion's, and a second would read as the reviewer's answer.
func TestARetryOfAStuckRoundTellsTheReviewerWhy(t *testing.T) {
	pool := migratedPool(t)
	seedReviewOf(t, pool, "c0ffee", "green")
	apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
	for i, fact := range []intake.Fact{
		intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-1"},
		intake.ClaimFailed{Issue: "LEGION-208", Role: claim.RoleReviewer},
		intake.RetryOrEscalate{Issue: "LEGION-208", Decision: intake.RetryDecision},
	} {
		if result := apply(fmt.Sprintf("stuck-%d", i), fact); result.Refusal != nil {
			t.Fatalf("fact %d (%T) was refused: %+v", i, fact, result.Refusal)
		}
	}
	if got := issuePhase(t, pool); got != phase.Reviewing {
		t.Fatalf("the issue is in %s, want reviewing", got)
	}
	told := reviewStuckNotices(t, pool)
	if len(told) != 1 {
		t.Fatalf("the architect was told %d times (%+v), want once: at the completion, not at the retry", len(told), told)
	}
	var task string
	if err := pool.QueryRow(t.Context(), `select payload->>'task' from outbox where kind = 'supervise' and payload->>'op' = 'start'
		and payload->>'role' = 'reviewer' order by id desc limit 1`).Scan(&task); err != nil {
		t.Fatalf("read the reviewer's retry task: %v", err)
	}
	if !strings.Contains(task, told[0].Reason) {
		t.Fatalf("the reviewer's retry task = %q, want it to carry the reason the architect was told, %q", task, told[0].Reason)
	}
}

// The retry restores the phase the hold came from and starts its worker. It re-decides no CI
// result: a settlement that reached the issue while it was held moved nothing, as it would have
// moved nothing had it arrived with the issue in its phase (dispatch://INFRA-714/spec R1), so the
// restored worker reads the head's checks itself. Nothing goes back to implementing and the
// architect hears no checks-red, from whatever phase the hold was taken.
func TestARetryOfAHoldWithASettledHeadRestoresThePhase(t *testing.T) {
	for _, tc := range []struct {
		from   phase.Phase
		status string
	}{
		{from: phase.Testing, status: "testing"},
		{from: phase.Retro, status: "retro"},
		{from: phase.Merging, status: "retro"},
	} {
		t.Run(string(tc.from), func(t *testing.T) {
			pool := migratedPool(t)
			role := RoleFor(tc.from)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: tc.from, Generation: 1, Status: tc.status, Rank: "U"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "c0ffee"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			if role != claim.RoleImplementer {
				seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: role, Claim: claim.Token(string(role) + "-claim")})
			}
			apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
			for i, fact := range []intake.Fact{
				intake.ClaimFailed{Issue: "LEGION-208", Role: role},
				intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: "c0ffee", CheckRuns: []record.AttemptRun{{Name: "lint", ID: 1}},
					Generation: 1, Snapshot: "red-c0ffee", Failing: []string{"lint"}},
				intake.RetryOrEscalate{Issue: "LEGION-208", Decision: intake.RetryDecision},
			} {
				if result := apply(fmt.Sprintf("held-red-%d", i), fact); result.Refusal != nil {
					t.Fatalf("fact %d (%T) was refused: %+v", i, fact, result.Refusal)
				}
			}
			if got := issuePhase(t, pool); got != tc.from {
				t.Fatalf("the issue is in %s, want %s restored: a settlement moves nothing", got, tc.from)
			}
			if got := noticeKinds(t, pool, "LEGION-208"); containsNotice(got, "checks-red") {
				t.Fatalf("notices %v, want no checks-red", got)
			}
			starts := func(role claim.Role) int {
				var n int
				if err := pool.QueryRow(t.Context(), `select count(*) from outbox where kind = 'supervise' and payload->>'op' = 'start'
					and payload->>'role' = $1`, string(role)).Scan(&n); err != nil {
					t.Fatalf("count the %s's starts: %v", role, err)
				}
				return n
			}
			if starts(role) != 1 {
				t.Fatalf("the retry started the %s %d times, want once: the restored phase is its own", role, starts(role))
			}
			if role != claim.RoleImplementer && starts(claim.RoleImplementer) != 0 {
				t.Fatalf("the retry started the implementer %d times, want none: nothing sent the work back", starts(claim.RoleImplementer))
			}
		})
	}
}

// The reviewer's approval submitted before its completion but delivered after it ends the round as
// any approval does, and tells nothing beyond the completion's notice.
func TestALateDeliveredApprovalEndsAStuckRound(t *testing.T) {
	pool := migratedPool(t)
	seedReviewOf(t, pool, "c0ffee", "green")
	apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
	apply("complete", intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-1"})
	apply("approve", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: "c0ffee", Author: testReviewApp,
		Body: "ship it", SubmittedAt: time.Date(2026, 9, 22, 23, 59, 0, 0, time.UTC)})
	if got := issuePhase(t, pool); got != phase.Accepting {
		t.Fatalf("the issue is in %s, want accepting", got)
	}
	if got := reviewStuckNotices(t, pool); len(got) != 1 {
		t.Fatalf("the architect was told %d times (%+v), want once: at the completion", len(got), got)
	}
}

// A round ends only on a review that decides it: an APPROVE of the head or a REQUEST_CHANGES. A
// reviewer that completes its round with none - it submitted a COMMENT, a review in any other
// state, or no review at all - leaves the issue in reviewing with nothing on its way to end it, so
// its architect is told, naming the head, and told again each time the reviewer (the review App)
// submits another review that decides nothing. A reply on a review thread is a review GitHub gives
// no body, not a submitted one, and it tells nothing; nor does anyone else's comment, which changes
// nothing about the round. A review that arrives before the reviewer completes tells nobody: the
// completion is still to come. A deciding review from the same round still ends it, as always.
func TestARoundNoReviewDecidesTellsTheArchitect(t *testing.T) {
	// testEngine's clock applies the completion at midnight; GitHub stamps each review's submission,
	// and answerSkew bounds how far the two clocks can disagree.
	early, late := time.Date(2026, 9, 22, 23, 59, 0, 0, time.UTC), time.Date(2026, 9, 23, 0, 0, 30, 0, time.UTC)
	withinSkew := time.Date(2026, 9, 23, 0, 0, 5, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		before   []string
		after    []intake.PullRequestReview
		told     int
		decision string
		want     phase.Phase
	}{
		{name: "a COMMENT, then the reviewer completes", before: []string{"commented"}, told: 1, decision: "approved", want: phase.Accepting},
		{name: "no review at all", told: 1, decision: "approved", want: phase.Accepting},
		{name: "the reviewer completes, then its COMMENT submitted after the completion", after: []intake.PullRequestReview{{State: "commented", Author: testReviewApp, Body: "one more thought", SubmittedAt: late}},
			told: 2, decision: "changes_requested", want: phase.Implementing},
		{name: "the reviewer completes, then its COMMENT submitted before the completion, delivered after", after: []intake.PullRequestReview{{State: "commented", Author: testReviewApp, Body: "one more thought", SubmittedAt: early}},
			told: 1, decision: "changes_requested", want: phase.Implementing},
		{name: "the reviewer completes, then its COMMENT with no submission time", after: []intake.PullRequestReview{{State: "commented", Author: testReviewApp, Body: "one more thought"}},
			told: 1, decision: "approved", want: phase.Accepting},
		{name: "the reviewer completes, then its COMMENT submitted within the clocks' skew of the completion", after: []intake.PullRequestReview{{State: "commented", Author: testReviewApp, Body: "one more thought", SubmittedAt: withinSkew}},
			told: 1, decision: "approved", want: phase.Accepting},
		{name: "the reviewer completes, then its reply on a review thread", after: []intake.PullRequestReview{{State: "commented", Author: testReviewApp, SubmittedAt: late}},
			told: 1, decision: "approved", want: phase.Accepting},
		{name: "the reviewer completes, then another account's COMMENT", after: []intake.PullRequestReview{{State: "commented", Author: "a-human", Body: "a thought", SubmittedAt: late}},
			told: 1, decision: "approved", want: phase.Accepting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedReviewOf(t, pool, "c0ffee", "green")
			apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
			review := func(id string, review intake.PullRequestReview) {
				t.Helper()
				review.Repo, review.Number, review.CommitID = "sjawhar/legion", 42, "c0ffee"
				apply(id, review)
			}
			for i, state := range tc.before {
				review(fmt.Sprintf("before-%d", i), intake.PullRequestReview{State: state, Author: testReviewApp, Body: "the " + state + " review"})
				if got := reviewStuckNotices(t, pool); len(got) != 0 {
					t.Fatalf("a %s review before the reviewer completed told the architect %+v, want nothing", state, got)
				}
			}
			if result := apply("complete", intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer,
				Claim: "review-claim", Summary: "reviewed", Commit: "review-1"}); result.Refusal != nil {
				t.Fatalf("the reviewer's completion was refused: %+v", result.Refusal)
			}
			for i, after := range tc.after {
				review(fmt.Sprintf("after-%d", i), after)
			}
			if got := issuePhase(t, pool); got != phase.Reviewing {
				t.Fatalf("after the reviews that followed the completion the issue is in %s, want reviewing", got)
			}
			told := reviewStuckNotices(t, pool)
			if len(told) != tc.told {
				t.Fatalf("the architect was told %d times (%+v), want %d", len(told), told, tc.told)
			}
			for _, notice := range told {
				if notice.Role != claim.RoleReviewer || notice.Phase != phase.Reviewing || !strings.Contains(notice.Reason, "c0ffee") ||
					notice.Topic != "notifications.role.legion-legion-legion-208-reviewer" {
					t.Fatalf("notice %+v, want the reviewer's reviewing round, naming head c0ffee, at the reviewer's role topic", notice)
				}
			}
			if told[0].Summary != "the reviewer's completion" || len(told) == 2 && told[1].Summary != "the reviewer's review" {
				t.Fatalf("notices %+v, want the first written by the reviewer's completion and any second by its review", told)
			}
			review("decides", intake.PullRequestReview{State: tc.decision, Author: testReviewApp, Body: "the decision", SubmittedAt: late})
			if got := issuePhase(t, pool); got != tc.want {
				t.Fatalf("after the %s review the issue is in %s, want %s", tc.decision, got, tc.want)
			}
			if got := reviewStuckNotices(t, pool); len(got) != tc.told {
				t.Fatalf("the deciding review told the architect again: %+v", got)
			}
		})
	}
}

// On a public repository any GitHub account can review a pull request, so a review decides a round
// only when the review App submitted it or its author has write access or higher to the repository,
// which intake reads from GitHub before the fact arrives (intake.PullRequestReview.AuthorCanWrite).
// Anyone else's APPROVE or REQUEST_CHANGES decides nothing, as a COMMENT does, and so does one by
// an author GitHub answers 404, or a 403 that is not its rate limit, for (intake retries every other
// failed read before the review arrives): a round the reviewer completed undecided stays stuck, the
// architect is told nothing more, and the review App's decision, submitted before the other review
// but delivered after it, still ends the round. The review App's own reviews decide by its login:
// GitHub gives its bot account no collaborator permission of its own.
func TestOnlyTheReviewAppOrAWriterDecidesARound(t *testing.T) {
	submitted := time.Date(2026, 9, 23, 0, 1, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, author, state string
		canWrite            bool
		want                phase.Phase
	}{
		{name: "the review App's approval", author: testReviewApp, state: "approved", want: phase.Accepting},
		{name: "the review App's request for changes", author: testReviewApp, state: "changes_requested", want: phase.Implementing},
		{name: "an approval by an account with write access", author: "a-writer", canWrite: true, state: "approved", want: phase.Accepting},
		{name: "a request for changes by an account with write access", author: "a-writer", canWrite: true, state: "changes_requested", want: phase.Implementing},
		{name: "an approval by an account without write access", author: "a-reader", state: "approved", want: phase.Reviewing},
		{name: "a request for changes by an account without write access", author: "a-reader", state: "changes_requested", want: phase.Reviewing},
		{name: "another App's approval", author: "another-app[bot]", state: "approved", want: phase.Reviewing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedReviewOf(t, pool, "c0ffee", "green")
			apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
			apply("complete", intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-1"})
			if got := reviewStuckNotices(t, pool); len(got) != 1 {
				t.Fatalf("the reviewer's completion told the architect %d times (%+v), want once", len(got), got)
			}
			apply("review", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 8, SubmittedAt: submitted.Add(time.Minute), State: tc.state,
				CommitID: "c0ffee", Author: tc.author, AuthorCanWrite: tc.canWrite, Body: "the review"})
			if got := issuePhase(t, pool); got != tc.want {
				t.Fatalf("after the review the issue is in %s, want %s", got, tc.want)
			}
			if tc.want != phase.Reviewing {
				return
			}
			if got := reviewStuckNotices(t, pool); len(got) != 1 {
				t.Fatalf("a review that decides nothing told the architect again: %+v", got)
			}
			apply("the reviewer's decision", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 7, SubmittedAt: submitted, State: "approved",
				CommitID: "c0ffee", Author: testReviewApp, Body: "the decision"})
			if got := issuePhase(t, pool); got != phase.Accepting {
				t.Fatalf("after the review App's approval the issue is in %s, want accepting", got)
			}
		})
	}
}

// An approval ends the round only while it approves the head's code. One that does not - the pull
// request has moved on since the head the approval names, by a push that may have changed code -
// leaves the round as stuck as no review at all, so the architect is told, once, when the round
// becomes stuck, by whichever fact made it so: the
// completion, or the arrival of the new head a code push was still on its way from. A push
// classified late that leaves the round stuck the same way tells nothing more. An approval of the
// head, or a request for changes, then ends the round and tells nothing further. A round whose
// approved head a code push is still on its way from is open, not stuck, and tells nobody: the
// push's new head decides whether the approval stands. Each round's push comes before its
// approval, since an approval of the head the pull request stands at ends the round as it lands
// (dispatch://INFRA-714/spec R1). No CI verdict is read anywhere here
// (TestAnApprovalEndsTheRoundWhateverCISays).
func TestARoundItsApprovalCannotEndTellsTheArchitect(t *testing.T) {
	approve := func(head string) intake.Fact {
		return intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: head, Author: testReviewApp, Body: "approved"}
	}
	requestChanges := intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "changes_requested", CommitID: "head-2", Author: testReviewApp, Body: "fix the widget"}
	complete := intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-1"}
	sync := func(head string) intake.Fact {
		return intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: head}
	}
	// A code push leaves GitHub's answer for the head before it saying nothing about the new head
	// (classify's advance), so the round that ends on an approval of that head ends on a read of
	// it: the daemon's own, which every push asks for (workflow's askMergeState).
	read := func(head string) intake.Fact {
		return intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "main", HeadSHA: head,
			Mergeable: record.MergeabilityMergeable, MergeState: record.MergeStateClean}
	}
	push := func(from, to, paths string) intake.Fact {
		unforced, whole := "false", "false"
		return intake.Push{Repo: "sjawhar/legion", Branch: "legion/LEGION-208", Before: from, After: to,
			ChangedPaths: &paths, Truncated: &whole, Forced: &unforced, Pusher: "legion-reviewer[bot]"}
	}
	const handoff, code = ".legion/LEGION-208/review.json", ".legion/LEGION-208/review.json\nsrc/widget.go"
	// Another issue's handoffs are code to this round: its reviewer never read them, and READY
	// refuses them as it refuses code.
	const otherHandoffs = ".legion/LEGION-209/review.json"
	for _, tc := range []struct {
		name  string
		steps []intake.Fact
		// told is what the one notice's reason names, and by the fact that wrote it; empty, the
		// architect is told nothing.
		told []string
		by   string
		// then is what ends the round, read before approved where a code push dropped GitHub's
		// answer for the head.
		then []intake.Fact
		want phase.Phase
	}{
		{name: "a push since the approved head changed code, then an approval of the head",
			steps: []intake.Fact{sync("head-2"), push("head", "head-2", code), approve("head"), complete},
			told:  []string{"head-2"}, by: "the reviewer's completion", then: []intake.Fact{read("head-2"), approve("head-2")}, want: phase.Accepting},
		{name: "a push since the approved head changed code, then a request for changes",
			steps: []intake.Fact{sync("head-2"), push("head", "head-2", code), approve("head"), complete},
			told:  []string{"head-2"}, by: "the reviewer's completion", then: []intake.Fact{requestChanges}, want: phase.Implementing},
		{name: "a push since the approved head changed another issue's handoffs",
			steps: []intake.Fact{sync("head-2"), push("head", "head-2", otherHandoffs), approve("head"), complete},
			told:  []string{"head-2"}, by: "the reviewer's completion", then: []intake.Fact{read("head-2"), approve("head-2")}, want: phase.Accepting},
		{name: "the handoff push over a code push is classified after the completion",
			steps: []intake.Fact{sync("head-2"), push("head", "head-2", code), sync("head-3"), approve("head"), complete, push("head-2", "head-3", handoff)},
			told:  []string{"head-3"}, by: "the reviewer's completion", then: []intake.Fact{read("head-3"), approve("head-3")}, want: phase.Accepting},
		{name: "a code push whose new head has not arrived",
			steps: []intake.Fact{push("head", "head-2", code), approve("head"), complete}, want: phase.Reviewing},
		{name: "a code push whose new head arrives after the completion",
			steps: []intake.Fact{push("head", "head-2", code), approve("head"), complete, sync("head-2")},
			told:  []string{"head-2"}, by: "a push", then: []intake.Fact{read("head-2"), approve("head-2")}, want: phase.Accepting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedReviewOf(t, pool, "head", "green")
			apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
			for i, fact := range tc.steps {
				if result := apply(fmt.Sprintf("step-%d", i), fact); result.Refusal != nil {
					t.Fatalf("step %d (%T) was refused: %+v", i, fact, result.Refusal)
				}
			}
			if got := issuePhase(t, pool); got != phase.Reviewing {
				t.Fatalf("the issue is in %s, want reviewing", got)
			}
			told := reviewStuckNotices(t, pool)
			if len(tc.told) == 0 && len(told) != 0 {
				t.Fatalf("the architect was told %+v, want nothing while the push's new head is on its way", told)
			}
			if len(tc.told) != 0 {
				if len(told) != 1 {
					t.Fatalf("the architect was told %d times (%+v), want once", len(told), told)
				}
				for _, want := range tc.told {
					if !strings.Contains(told[0].Reason, want) {
						t.Fatalf("the notice's reason %q does not name %q", told[0].Reason, want)
					}
				}
				if told[0].Summary != tc.by {
					t.Fatalf("the notice was written by %q, want %q", told[0].Summary, tc.by)
				}
			}
			for i, fact := range tc.then {
				if result := apply(fmt.Sprintf("then-%d", i), fact); result.Refusal != nil {
					t.Fatalf("then %d (%T) was refused: %+v", i, fact, result.Refusal)
				}
			}
			if got := issuePhase(t, pool); got != tc.want {
				t.Fatalf("the issue is in %s, want %s", got, tc.want)
			}
			if got := reviewStuckNotices(t, pool); len(got) != len(told) {
				t.Fatalf("the round's end told the architect again: %+v", got)
			}
		})
	}
}

// GitHub delivers a branch's push and the pull request's synchronize in either order. A round
// already stuck - its reviewer completed before its approval landed - is re-asked by a head's
// arrival only once the head's push is known: asked before, the round would read stuck again at the
// new head, a notice the architect acts on, though the push then proves the head handoff-only and
// the approval ends the round. So the head arriving ahead of its push tells nothing, and the push's
// own arrival settles the round; the approval between them still tells that it is of other code
// than the head's, as it would at any head whose push is unknown. The push arriving ahead of its
// head leaves the round stuck at the head it still has, and the head's arrival then tells once that
// the decision must now be of the new head.
func TestAHeadArrivingBeforeItsPushTellsNothingOfAStuckRound(t *testing.T) {
	// A step is a fact and how many review-stuck notices the outbox holds once it is applied.
	type step struct {
		fact intake.Fact
		told int
	}
	complete := intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-1"}
	approve := intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: "head", Author: testReviewApp, Body: "approved"}
	sync := intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head-2"}
	handoff, unforced, whole := ".legion/LEGION-208/review.json", "false", "false"
	push := intake.Push{Repo: "sjawhar/legion", Branch: "legion/LEGION-208", Before: "head", After: "head-2",
		ChangedPaths: &handoff, Truncated: &whole, Forced: &unforced, Pusher: "legion-reviewer[bot]"}
	for _, tc := range []struct {
		name  string
		steps []step
		// told is each review-stuck notice in the order written: the fact that wrote it, and what
		// its reason names.
		told []record.Notice
	}{
		{name: "the handoff head before its push", steps: []step{{complete, 1}, {sync, 1}, {approve, 2}, {push, 2}},
			told: []record.Notice{{Summary: byCompletion, Reason: "APPROVE of head head or"}, {Summary: byOtherReview, Reason: "does not approve head head-2"}}},
		{name: "the handoff push before its head", steps: []step{{complete, 1}, {push, 1}, {sync, 2}, {approve, 2}},
			told: []record.Notice{{Summary: byCompletion, Reason: "APPROVE of head head or"}, {Summary: byPush, Reason: "APPROVE of head head-2 or"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedReviewOf(t, pool, "head", "green")
			apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
			for i, s := range tc.steps {
				if result := apply(fmt.Sprintf("step-%d", i), s.fact); result.Refusal != nil {
					t.Fatalf("step %d (%T) was refused: %+v", i, s.fact, result.Refusal)
				}
				if got := reviewStuckNotices(t, pool); len(got) != s.told {
					t.Fatalf("after step %d (%T) the architect was told %d times (%+v), want %d", i, s.fact, len(got), got, s.told)
				}
			}
			if got := issuePhase(t, pool); got != phase.Accepting {
				t.Fatalf("the issue is in %s, want accepting", got)
			}
			got := reviewStuckNotices(t, pool)
			if len(got) != len(tc.told) {
				t.Fatalf("the architect was told %d times (%+v), want %d", len(got), got, len(tc.told))
			}
			for i, want := range tc.told {
				if got[i].Summary != want.Summary || !strings.Contains(got[i].Reason, want.Reason) {
					t.Fatalf("notice %d was written by %q naming %q, want by %q naming %q", i, got[i].Summary, got[i].Reason, want.Summary, want.Reason)
				}
			}
		})
	}
}

// An approval of a head the record has not reached - the reviewer approved its own handoff head
// while the synchronize that brings it was still on its way - leaves the round stuck at the head the
// record holds. That head's arrival ends the round even when its push is lost, since no push can
// change what an approval of the head itself approves; any other head arriving without its push
// leaves the round as it was, told nothing more, for the push or the approval carry's compare
// to decide.
func TestTheArrivalOfTheApprovedHeadEndsTheRoundWithoutItsPush(t *testing.T) {
	complete := intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-1"}
	approve := intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: "head-2", Author: testReviewApp, Body: "approved"}
	sync := func(head string) intake.Fact {
		return intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: head}
	}
	for _, tc := range []struct {
		name string
		head string
		want phase.Phase
	}{
		{name: "the approved head arrives", head: "head-2", want: phase.Accepting},
		{name: "a head above the approved one arrives", head: "head-3", want: phase.Reviewing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedReviewOf(t, pool, "head", "green")
			apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
			for i, fact := range []intake.Fact{approve, complete, sync(tc.head)} {
				if result := apply(fmt.Sprintf("step-%d", i), fact); result.Refusal != nil {
					t.Fatalf("step %d (%T) was refused: %+v", i, fact, result.Refusal)
				}
			}
			if got := issuePhase(t, pool); got != tc.want {
				t.Fatalf("the issue is in %s, want %s", got, tc.want)
			}
			if got := reviewStuckNotices(t, pool); len(got) != 1 || got[0].Summary != byCompletion {
				t.Fatalf("the architect was told %+v, want once, by the reviewer's completion", got)
			}
		})
	}
}

// reviewStuckNotices is every review-stuck notice the outbox holds, in order.
func reviewStuckNotices(t *testing.T, pool *pgxpool.Pool) []record.Notice {
	t.Helper()
	rows, err := pool.Query(t.Context(), `select coalesce(payload->>'role', ''), coalesce(payload->>'phase', ''), coalesce(payload->>'summary', ''),
		coalesce(payload->>'reason', ''), coalesce(payload->>'topic', '')
		from outbox where kind = 'notice' and payload->>'kind' = 'review-stuck' order by id`)
	if err != nil {
		t.Fatalf("read review-stuck notices: %v", err)
	}
	defer rows.Close()
	notices := []record.Notice{}
	for rows.Next() {
		notice := record.Notice{Kind: "review-stuck"}
		if err := rows.Scan(&notice.Role, &notice.Phase, &notice.Summary, &notice.Reason, &notice.Topic); err != nil {
			t.Fatalf("scan review-stuck notice: %v", err)
		}
		notices = append(notices, notice)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate review-stuck notices: %v", err)
	}
	return notices
}

// seedClaimSession records a claims row for token holding session, so the workflow's read of the
// reviewer's claim (record.Store.ClaimSession) finds it (byReviewer) and the daemon is known to
// have started the reviewer on the issue (engine's leftItsPhase). A row already seeded takes the
// session given, so a fixture that records the claim and a test that then names its session do not
// collide.
func seedClaimSession(t *testing.T, pool *pgxpool.Pool, token claim.Token, session string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ($1, 'LEGION', 'LEGION-208', 'LEGION-208', 'reviewer', 1, $2, '', 'working', 0, 0, 0, 0)
		on conflict (token) do update set session = excluded.session`,
		string(token), session); err != nil {
		t.Fatalf("seed the claim's session: %v", err)
	}
}

// reviewerDecision is LEGION-208's reviewer row's recorded decision, nil until a review decides.
func reviewerDecision(t *testing.T, pool *pgxpool.Pool) *record.ReviewDecision {
	t.Helper()
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(context.Background())
	phases, err := record.NewStore().Phases(context.Background(), tx, "LEGION-208")
	if err != nil {
		t.Fatalf("read phases: %v", err)
	}
	for _, p := range phases {
		if p.Role == claim.RoleReviewer {
			return p.Decision
		}
	}
	return nil
}

// The controller now holds the review App's token too, so its own reviews, and a sibling tree's or
// an architect's, arrive under the review App's own login. The workflow tells the reviewer's own
// review from any other review-App session's by the Legion footer every Legion role appends
// (intake.PullRequestReview.LegionSession), matched against the session recorded on the reviewer's
// claim (record.Store.ClaimSession). A review-App review naming another session decides nothing:
// it is set aside, the round stays undecided, and the stuck-round path logs why. One naming the
// reviewer's own session decides exactly as before.
func TestAReviewAppReviewFromAnotherSessionIsSetAside(t *testing.T) {
	pool := migratedPool(t)
	seedReviewOf(t, pool, "c0ffee", "green")
	seedClaimSession(t, pool, "review-claim", "ses-reviewer")
	var logs bytes.Buffer
	engine := testEngine(config.DesignGateRootIssues, slog.New(slog.NewTextHandler(&logs, nil)))
	apply := applyFacts(t, pool, engine)
	apply("complete", intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-1"})

	apply("another session's approval", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 1, State: "approved", CommitID: "c0ffee",
		Author: testReviewApp, Body: "ship it\n\n<!-- legion: {\"session\":\"ses-controller\",\"phase\":\"review\"} -->",
		SubmittedAt: time.Date(2026, 9, 23, 0, 1, 0, 0, time.UTC)})
	if got := issuePhase(t, pool); got != phase.Reviewing {
		t.Fatalf("after another session's review the issue is in %s, want reviewing", got)
	}
	if got := reviewerDecision(t, pool); got != nil {
		t.Fatalf("the reviewer's decision = %+v, want nil", got)
	}
	if got := logs.String(); !strings.Contains(got, "a review decides nothing: the review App submitted it from a session that is not the reviewer's") ||
		!strings.Contains(got, "footer_session=ses-controller") || !strings.Contains(got, "reviewer_session=ses-reviewer") {
		t.Fatalf("logs = %s, want the set-aside line naming both sessions", got)
	}

	apply("the reviewer's own approval", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 2, State: "approved", CommitID: "c0ffee",
		Author: testReviewApp, Body: "ship it\n\n<!-- legion: {\"session\":\"ses-reviewer\",\"phase\":\"review\"} -->",
		SubmittedAt: time.Date(2026, 9, 23, 0, 2, 0, 0, time.UTC)})
	if got := issuePhase(t, pool); got != phase.Accepting {
		t.Fatalf("after the reviewer's own review the issue is in %s, want accepting", got)
	}
}

// A comment delivered after the reviewer completes an undecided round is the reviewer's answer
// only from the reviewer's own review-App session (byReviewer); one from another session - the
// controller's - is nobody's answer and tells the architect nothing more.
func TestACommentFromAnotherSessionIsNotTheReviewersAnswer(t *testing.T) {
	late := time.Date(2026, 9, 23, 0, 1, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		session string
		want    int
	}{
		{name: "a comment from another review-App session", session: "ses-controller", want: 1},
		{name: "a comment from the reviewer's own session", session: "ses-reviewer", want: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedReviewOf(t, pool, "c0ffee", "green")
			seedClaimSession(t, pool, "review-claim", "ses-reviewer")
			apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
			apply("complete", intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-1"})
			if got := reviewStuckNotices(t, pool); len(got) != 1 {
				t.Fatalf("the completion told the architect %d times (%+v), want once", len(got), got)
			}
			apply("comment", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "commented", CommitID: "c0ffee", Author: testReviewApp,
				Body: "one more thought\n\n<!-- legion: {\"session\":\"" + tc.session + "\",\"phase\":\"review\"} -->", SubmittedAt: late})
			if got := reviewStuckNotices(t, pool); len(got) != tc.want {
				t.Fatalf("after the comment the architect was told %d times (%+v), want %d", len(got), got, tc.want)
			}
		})
	}
}

// A review-App review with no Legion footer, while the reviewer's session is recorded, decides
// nothing: it has nothing to match the reviewer's claim against, so it is set aside exactly as one
// naming another session is, and its set-aside log line's footer_session is empty.
func TestAReviewWithNoFooterWhileTheReviewersSessionIsRecordedIsSetAside(t *testing.T) {
	pool := migratedPool(t)
	seedReviewOf(t, pool, "c0ffee", "green")
	seedClaimSession(t, pool, "review-claim", "ses-reviewer")
	var logs bytes.Buffer
	engine := testEngine(config.DesignGateRootIssues, slog.New(slog.NewTextHandler(&logs, nil)))
	apply := applyFacts(t, pool, engine)
	apply("complete", intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-1"})
	apply("no footer", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 1, State: "approved", CommitID: "c0ffee", Author: testReviewApp,
		Body: "ship it", SubmittedAt: time.Date(2026, 9, 23, 0, 1, 0, 0, time.UTC)})
	if got := issuePhase(t, pool); got != phase.Reviewing {
		t.Fatalf("after the footer-less review the issue is in %s, want reviewing", got)
	}
	if got := reviewerDecision(t, pool); got != nil {
		t.Fatalf("the reviewer's decision = %+v, want nil", got)
	}
	if got := logs.String(); !strings.Contains(got, "a review decides nothing: the review App submitted it from a session that is not the reviewer's") ||
		!strings.Contains(got, `footer_session=""`) {
		t.Fatalf("logs = %s, want the set-aside line with an empty footer_session", got)
	}
}

// With no session recorded on the reviewer's claim - a reviewer claim with no row, as every
// existing fixture seeds - the login decides a round by itself, as it always has, since there is
// nothing recorded to tell its sessions apart; this is logged once at info.
func TestWithNoRecordedReviewerSessionTheLoginDecides(t *testing.T) {
	pool := migratedPool(t)
	seedReviewOf(t, pool, "c0ffee", "green")
	var logs bytes.Buffer
	engine := testEngine(config.DesignGateRootIssues, slog.New(slog.NewTextHandler(&logs, nil)))
	apply := applyFacts(t, pool, engine)
	apply("complete", intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-1"})
	apply("review", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 1, State: "approved", CommitID: "c0ffee", Author: testReviewApp,
		Body: "ship it", SubmittedAt: time.Date(2026, 9, 23, 0, 1, 0, 0, time.UTC)})
	if got := issuePhase(t, pool); got != phase.Accepting {
		t.Fatalf("after the review the issue is in %s, want accepting", got)
	}
	if got := logs.String(); !strings.Contains(got, "the reviewer's session is not recorded; the review App's login decides") {
		t.Fatalf("logs = %s, want the unrecorded-session line", got)
	}
}

// Envoy's normalizer caps a review's body before the daemon sees it, which can cut a long review's
// trailing Legion footer off (intake.PullRequestReview.BodyTruncated). A truncated review-App
// review with no footer, while the reviewer's session is recorded, is set aside exactly as any
// other footer-less one is, and its log line names the truncation.
func TestATruncatedReviewWithNoFooterIsSetAsideAndLogsTruncation(t *testing.T) {
	pool := migratedPool(t)
	seedReviewOf(t, pool, "c0ffee", "green")
	seedClaimSession(t, pool, "review-claim", "ses-reviewer")
	var logs bytes.Buffer
	engine := testEngine(config.DesignGateRootIssues, slog.New(slog.NewTextHandler(&logs, nil)))
	apply := applyFacts(t, pool, engine)
	apply("complete", intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-1"})
	apply("truncated", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 1, State: "approved", CommitID: "c0ffee", Author: testReviewApp,
		Body: "a capped body", BodyTruncated: true, SubmittedAt: time.Date(2026, 9, 23, 0, 1, 0, 0, time.UTC)})
	if got := issuePhase(t, pool); got != phase.Reviewing {
		t.Fatalf("after the truncated review the issue is in %s, want reviewing", got)
	}
	if got := reviewerDecision(t, pool); got != nil {
		t.Fatalf("the reviewer's decision = %+v, want nil", got)
	}
	if got := logs.String(); !strings.Contains(got, "body_truncated=true") {
		t.Fatalf("logs = %s, want the set-aside line to carry body_truncated=true", got)
	}
}

// A push the record's bound lets go - the oldest of more pushes waiting for a link than the bound
// keeps, each behind a webhook that never arrived - is logged as a warning that names it, once, by
// the push that let it go: the move it would have linked into is judged without it.
func TestAPushTheBoundLetsGoIsWarnedByName(t *testing.T) {
	const message = "workflow: a pull request holds the most pushes waiting for a link; the oldest is let go"
	pool := migratedPool(t)
	seedReviewOf(t, pool, "head", "green")
	var logged bytes.Buffer
	apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, slog.New(slog.NewTextHandler(&logged, nil))))
	// Sixteen pushes of a line the branch never takes are the most the record keeps waiting
	// (classify's maxWaitingPushes); the seventeenth lets the first go.
	for i := 1; i <= 17; i++ {
		logged.Reset()
		if result := apply(fmt.Sprintf("push-%d", i), pushedFromTo(fmt.Sprintf("x%d", i-1), fmt.Sprintf("x%d", i), "src/widget.go", "legion-implementer[bot]")); result.Refusal != nil {
			t.Fatalf("push %d was refused: %+v", i, result.Refusal)
		}
		var warned []string
		for _, line := range strings.Split(logged.String(), "\n") {
			if strings.Contains(line, message) {
				warned = append(warned, line)
			}
		}
		if i < 17 {
			if len(warned) != 0 {
				t.Fatalf("push %d of 16 waiting warned %q, want nothing", i, warned)
			}
			continue
		}
		if len(warned) != 1 {
			t.Fatalf("the push past the bound warned %d times (%q), want once", len(warned), warned)
		}
		for _, want := range []string{"level=WARN", "issue=LEGION-208", "pullRequest=42", "before=x0", "after=x1"} {
			if !strings.Contains(warned[0], want) {
				t.Fatalf("the warning %q does not name %s, want the push let go named", warned[0], want)
			}
		}
	}
}

// A pull request holds at most seventeen pushes of the same two heads on their way, before their
// move is made (classify's maxCopies): the eighteenth lets the oldest after the first go, logged as
// a warning that names it, once, and no push waiting for a link is let go with it.
func TestACopyOfTwoHeadsOnTheirWayLetGoIsWarnedByName(t *testing.T) {
	const message = "workflow: a pull request holds the most pushes of the same two heads on their way; the oldest after the first is let go"
	pool := migratedPool(t)
	seedReviewOf(t, pool, "head", "green")
	var logged bytes.Buffer
	apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, slog.New(slog.NewTextHandler(&logged, nil))))
	for i := 1; i <= 18; i++ {
		logged.Reset()
		if result := apply(fmt.Sprintf("push-%d", i), pushedFromTo("head", "h1", "src/widget.go", "legion-implementer[bot]")); result.Refusal != nil {
			t.Fatalf("push %d was refused: %+v", i, result.Refusal)
		}
		var warned []string
		for _, line := range strings.Split(logged.String(), "\n") {
			if strings.Contains(line, "let go") {
				warned = append(warned, line)
			}
		}
		if i < 18 {
			if len(warned) != 0 {
				t.Fatalf("copy %d of 17 warned %q, want nothing", i, warned)
			}
			continue
		}
		if len(warned) != 1 || !strings.Contains(warned[0], message) {
			t.Fatalf("the copy past the bound warned %q, want the one copy let go", warned)
		}
		for _, want := range []string{"level=WARN", "issue=LEGION-208", "pullRequest=42", "before=head", "after=h1"} {
			if !strings.Contains(warned[0], want) {
				t.Fatalf("the warning %q does not name %s, want the copy let go named", warned[0], want)
			}
		}
	}
}

// A move holds at most sixteen later pushes of its two heads (classify's maxRepeats), each the
// move made again or its push delivered again; the seventeenth lets the oldest go, logged as a
// warning that names it, once, and no push waiting for a link is let go with it.
func TestARepeatAMoveLetsGoIsWarnedByName(t *testing.T) {
	const message = "workflow: a move holds the most pushes of its two heads made again; the oldest is let go"
	pool := migratedPool(t)
	seedReviewOf(t, pool, "head", "green")
	var logged bytes.Buffer
	apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, slog.New(slog.NewTextHandler(&logged, nil))))
	if result := apply("push-0", pushedFromTo("head", "h1", "src/widget.go", "legion-implementer[bot]")); result.Refusal != nil {
		t.Fatalf("the first push was refused: %+v", result.Refusal)
	}
	if result := apply("sync", intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", Before: "head", HeadSHA: "h1"}); result.Refusal != nil {
		t.Fatalf("h1's synchronize was refused: %+v", result.Refusal)
	}
	for i := 1; i <= 17; i++ {
		logged.Reset()
		pusher := "legion-implementer[bot]"
		if i%2 == 0 {
			pusher = testReviewApp
		}
		if result := apply(fmt.Sprintf("push-%d", i), pushedFromTo("head", "h1", "src/widget.go", pusher)); result.Refusal != nil {
			t.Fatalf("push %d was refused: %+v", i, result.Refusal)
		}
		var warned []string
		for _, line := range strings.Split(logged.String(), "\n") {
			if strings.Contains(line, message) || strings.Contains(line, "waiting for a link") {
				warned = append(warned, line)
			}
		}
		if i < 17 {
			if len(warned) != 0 {
				t.Fatalf("repeat %d of 16 warned %q, want nothing", i, warned)
			}
			continue
		}
		if len(warned) != 1 || !strings.Contains(warned[0], message) {
			t.Fatalf("the repeat past the bound warned %q, want the one repeat let go", warned)
		}
		for _, want := range []string{"level=WARN", "issue=LEGION-208", "pullRequest=42", "before=head", "after=h1"} {
			if !strings.Contains(warned[0], want) {
				t.Fatalf("the warning %q does not name %s, want the repeat let go named", warned[0], want)
			}
		}
	}
}
