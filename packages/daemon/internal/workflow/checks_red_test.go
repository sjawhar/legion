package workflow

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A CI settlement moves no issue, in any phase (dispatch://INFRA-714/spec R1): a failed check is
// information the worker holding the phase decides on - re-run, wait, or fix - and not the
// daemon's to read the semantics of. What the settlement says is told instead, as one message per
// role to three roles: the worker holding the issue's phase, the implementer, whose code the
// checks ran on, and the tree's architect, who owns the tree. Each message names every check the
// settlement reports and the conclusion it reports for it, in name order. A role named twice - the
// implementer holding the phase, the architect holding accepting - is told once, and a phase no
// worker holds names none. Nothing else happens: no phase moves, no architect
// hears checks-red, no worker is started, and no return is counted.
func TestASettlementMovesNothingAndTellsTheThreeRoles(t *testing.T) {
	for _, tc := range []struct {
		from   phase.Phase
		status string
		// roles are the roles told, in the order tellChecks addresses them.
		roles []claim.Role
	}{
		{phase.Implementing, "in_progress", []claim.Role{claim.RoleArchitect, claim.RoleImplementer}},
		{phase.Testing, "testing", []claim.Role{claim.RoleArchitect, claim.RoleTester, claim.RoleImplementer}},
		{phase.Reviewing, "needs_review", []claim.Role{claim.RoleArchitect, claim.RoleReviewer, claim.RoleImplementer}},
		{phase.Accepting, "needs_review", []claim.Role{claim.RoleArchitect, claim.RoleImplementer}},
		{phase.Retro, "retro", []claim.Role{claim.RoleArchitect, claim.RoleImplementer}},
		{phase.Merging, "retro", []claim.Role{claim.RoleArchitect, claim.RoleMerger, claim.RoleImplementer}},
	} {
		t.Run(string(tc.from), func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: tc.from, Generation: 1, Status: tc.status, Rank: "U"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			if role := RoleFor(tc.from); role != "" && role != claim.RoleImplementer {
				seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: role, Claim: claim.Token(string(role) + "-claim")})
			}
			applyRefusingNothing(t, pool, mergingEngine(), settledAt("head", 1, "unit"))

			if got := issuePhase(t, pool); got != tc.from {
				t.Fatalf("after the settlement the issue is in %s, want it left in %s: a CI result moves nothing", got, tc.from)
			}
			if got := noticeKinds(t, pool, "LEGION-208"); len(got) != 0 {
				t.Fatalf("the architect was told %v, want nothing but the role message", got)
			}
			assertOutboxCount(t, pool, "supervise", 0)
			assertOutboxCount(t, pool, "dispatch_status", 0)
			if pass := implementerPass(t, pool); pass != 0 {
				t.Fatalf("pass = %d, want none counted: nothing went back", pass)
			}
			want := make([]record.RoleMessage, 0, len(tc.roles))
			for _, role := range tc.roles {
				want = append(want, record.RoleMessage{Role: roleToken(t, role), Body: toldBody("CI settled at head: \"build\" succeeded, \"unit\" failed")})
			}
			assertRoleMessages(t, pool, "LEGION-208", want)
		})
	}
}

// The message is the whole of what the daemon knows of the head's CI, so a settlement that leaves
// it the same says nothing new and tells nobody again: the listener redelivers a settlement, and a
// re-run that ends as the run before it did settles the same head with the same conclusions. A
// settlement that changes a conclusion is told, so a check flapping red, green and red again tells
// each change - and moves nothing any of the three times, where the rule this replaced sent the
// work back on the first red and could not take it back.
func TestASettlementThatSaysNothingNewTellsNobodyAgain(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
		Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
		Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleTester, Claim: "test-claim"})
	engine := mergingEngine()
	messages := []record.RoleMessage{}
	for i, step := range []struct {
		fact intake.Fact
		// settled is the line the roles are told, "" when the settlement says nothing new.
		settled string
	}{
		{settledAt("head", 1, "unit"), "CI settled at head: \"build\" succeeded, \"unit\" failed"},
		{settledAt("head", 2, "unit"), ""},
		{settledAt("head", 3), "CI settled at head: \"build\" succeeded, \"unit\" succeeded"},
		{settledAt("head", 4, "unit"), "CI settled at head: \"build\" succeeded, \"unit\" failed"},
	} {
		applyRefusingNothing(t, pool, engine, step.fact)
		if step.settled != "" {
			for _, role := range []claim.Role{claim.RoleArchitect, claim.RoleTester, claim.RoleImplementer} {
				messages = append(messages, record.RoleMessage{Role: roleToken(t, role), Body: toldBody(step.settled)})
			}
		}
		if got := issuePhase(t, pool); got != phase.Testing {
			t.Fatalf("after settlement %d the issue is in %s, want testing", i+1, got)
		}
		assertRoleMessages(t, pool, "LEGION-208", messages)
	}
}

// A check that failed and was run again settles the same head twice, and what the three roles are
// told is the latest attempt of each check: GitHub gives a re-run its own check runs, so the
// second settlement's runs outrank the first's (classify.CompareAttemptSets) and the conclusions
// it reports replace the ones they outrank (classify.EffectiveOutcome). On sjawhar/legion#2395 the
// required check failed at the code head in one attempt of a run and passed in the next, and a
// handoff-only push followed; the three roles are told the red, then the green, and the handoff
// head carries that green forward, whichever of the re-run and the push arrives first. The rule
// this replaced carried the first settlement's red to the handoff head and sent the issue back on
// it, hours after the re-run had passed.
func TestTheLatestAttemptOfEachCheckIsWhatTheRolesAreTold(t *testing.T) {
	handoffPush := pushedFromTo("code", "handoff", ".legion/LEGION-208/test.json", "legion-tester[bot]")
	for _, tc := range []struct {
		name string
		// after are the facts that followed the first attempt's red, in the order they arrived:
		// the re-run's settlement of the same code head, and the handoff-only push that replaced
		// it with a head GitHub runs nothing on.
		after []intake.Fact
	}{
		{"the re-run settles before the handoff push", []intake.Fact{settledAt("code", 2), handoffPush, syncedTo("handoff")}},
		{"the handoff push lands before the re-run settles", []intake.Fact{handoffPush, syncedTo("handoff"), settledAt("code", 2)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "code"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleTester, Claim: "test-claim"})
			engine := mergingEngine()
			applyRefusingNothing(t, pool, engine, settledAt("code", 1, "unit"))
			applyRefusingNothing(t, pool, engine, tc.after...)

			var want []record.RoleMessage
			for _, settled := range []string{"CI settled at code: \"build\" succeeded, \"unit\" failed", "CI settled at code: \"build\" succeeded, \"unit\" succeeded"} {
				for _, role := range []claim.Role{claim.RoleArchitect, claim.RoleTester, claim.RoleImplementer} {
					want = append(want, record.RoleMessage{Role: roleToken(t, role), Body: toldBody(settled)})
				}
			}
			assertRoleMessages(t, pool, "LEGION-208", want)
			if got := recordedPR(t, pool, "LEGION-208"); got.HeadSHA != "handoff" || got.CheckedHead != "code" || len(got.Failing) != 0 {
				t.Fatalf("the handoff head %q carries the settlement of %q with %v failing, want it carrying the code head's re-run with nothing failing",
					got.HeadSHA, got.CheckedHead, got.Failing)
			}
			if got := issuePhase(t, pool); got != phase.Testing {
				t.Fatalf("the issue is in %s, want it left in testing: no settlement moves it", got)
			}
			assertOutboxCount(t, pool, "supervise", 0)
			if got := noticeKinds(t, pool, "LEGION-208"); len(got) != 0 {
				t.Fatalf("the architect was told %v, want nothing but the role messages", got)
			}
		})
	}
}

// A check GitHub produced no run for - an aggregate job whose needs: never ran, so it never
// started - reaches the three roles like any other conclusion, and moves nothing, because nothing
// classifies it (dispatch://INFRA-622). The settlement reports its runs and names that check as
// failed; the message names it beside the checks that ran, and the worker holding the phase
// decides what it means.
func TestACheckGitHubNeverRanIsToldLikeAnyOtherConclusion(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
		Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
		Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleTester, Claim: "test-claim"})
	applyRefusingNothing(t, pool, mergingEngine(), intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: "head",
		CheckRuns: []record.AttemptRun{{Name: "unit", ID: 1}}, Generation: 1, Snapshot: "never-ran",
		Failing: []string{"pr-checks-result"}})

	if got := issuePhase(t, pool); got != phase.Testing {
		t.Fatalf("after the settlement the issue is in %s, want testing: nothing classifies a conclusion", got)
	}
	if got := noticeKinds(t, pool, "LEGION-208"); len(got) != 0 {
		t.Fatalf("the architect was told %v, want nothing but the role message", got)
	}
	assertOutboxCount(t, pool, "supervise", 0)
	want := []record.RoleMessage{}
	for _, role := range []claim.Role{claim.RoleArchitect, claim.RoleTester, claim.RoleImplementer} {
		want = append(want, record.RoleMessage{Role: roleToken(t, role), Body: toldBody("CI settled at head: \"pr-checks-result\" failed, \"unit\" succeeded")})
	}
	assertRoleMessages(t, pool, "LEGION-208", want)
}

// Linger holds a member of a closed tree where it stood (record.TreeLingers): every role of the
// tree is suspended there, so a settlement that reaches a lingering tree's issue is recorded and
// nobody is told - a message to a role nobody holds reaches nobody, and the tree is done.
func TestASettlementOfALingeringTreeTellsNobody(t *testing.T) {
	pool := migratedPool(t)
	until := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
		Phase: phase.Done, Generation: 1, Status: "done", Rank: "U", LingerUntil: &until})
	parent := "LEGION-208"
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Parent: &parent,
		Project: "LEGION", Title: "child", Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "V"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-209", Repo: "sjawhar/legion",
		Number: 42, Branch: "legion/LEGION-209", HeadSHA: "head"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-209", Role: claim.RoleImplementer, Claim: "implement-claim"})
	applyRefusingNothing(t, pool, mergingEngine(), settledAt("head", 1, "unit"))

	assertRoleMessages(t, pool, "LEGION-209", nil)
	if checked := recordedPR(t, pool, "LEGION-209").CheckedHead; checked != "head" {
		t.Fatalf("the settlement stands for %q, want it recorded at head while the tree lingers", checked)
	}
}

// A settlement of a commit the head's settlement is not - an earlier head no push shows the
// current one replaced with handoffs alone - stands for no head, so there is nothing to tell: the
// daemon knows nothing of the head's CI, and a message naming another commit's checks would be
// read as the head's.
func TestASettlementThatStandsForNoHeadTellsNobody(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
		Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
		Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
	applyRefusingNothing(t, pool, mergingEngine(), settledAt("another-commit", 1, "unit"))

	assertRoleMessages(t, pool, "LEGION-208", nil)
	if got := issuePhase(t, pool); got != phase.Testing {
		t.Fatalf("the issue is in %s, want testing", got)
	}
}

// A push classified after its head arrived completes the carry of a code head's settlement to the
// handoff head that replaced it (classify.SettlementFor), and so does the head arriving over a
// push already known. Either way nothing moves and nobody is told again: the roles were told what
// the settlement said when it arrived, and the carry says nothing new about the code it names. The
// rule this replaced sent the work back from whatever phase the carry completed in.
func TestAPushThatCompletesTheCarryMovesNothingAndTellsNobody(t *testing.T) {
	for _, tc := range []struct {
		name string
		// head is the pull request's recorded head before the fact, and fact the one that
		// completes the carry.
		head string
		fact intake.Fact
	}{
		{"the push arrives after its head", "head", pushedFromTo("code", "head", ".legion/LEGION-208/implement.json", "legion-implementer[bot]")},
		{"the head arrives over a push already known", "code", syncedTo("head")},
	} {
		for _, from := range []phase.Phase{phase.Testing, phase.Merging} {
			t.Run(tc.name+", in "+string(from), func(t *testing.T) {
				pool := migratedPool(t)
				status := map[phase.Phase]string{phase.Testing: "testing", phase.Merging: "retro"}[from]
				seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
					Phase: from, Generation: 1, Status: status, Rank: "U"})
				pr := record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
					Number: 42, Branch: "legion/LEGION-208", HeadSHA: tc.head, CheckedHead: "code",
					CheckRuns: []record.AttemptRun{{Name: "unit", ID: 1}}, Failing: []string{"unit"}}
				if tc.head == "code" {
					pr.Pushes = []record.ClassifiedPush{{SHA: "head", Before: "code", HandoffOnly: true}}
				}
				seedPR(t, pool, pr)
				seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
				applyRefusingNothing(t, pool, mergingEngine(), tc.fact)

				if got := recordedPR(t, pool, "LEGION-208"); got.HeadSHA != "head" || got.CheckedHead != "code" {
					t.Fatalf("after the carry the head is %q and its settlement is of %q, want head carrying the code head's settlement", got.HeadSHA, got.CheckedHead)
				}
				if got := issuePhase(t, pool); got != from {
					t.Fatalf("after the carry the issue is in %s, want it left in %s", got, from)
				}
				assertRoleMessages(t, pool, "LEGION-208", nil)
				if got := noticeKinds(t, pool, "LEGION-208"); len(got) != 0 {
					t.Fatalf("the architect was told %v, want nothing", got)
				}
				assertOutboxCount(t, pool, "supervise", 0)
			})
		}
	}
}

// A reopen starts the pull request's CI settlement afresh (classify.Reopen): the settlement
// recorded before the close is of a head GitHub has run nothing on since, so the daemon knows
// nothing of the reopened head's CI until one arrives. The first settlement after the reopen is
// therefore told, even when it says what the settlement before the close said.
func TestAReopenStartsTheCheckSettlementAfreshAndTellsItAgain(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
		Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
		Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", HeadUpdatedAt: time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleTester, Claim: "test-claim"})
	engine := mergingEngine()
	applyRefusingNothing(t, pool, engine, settledAt("head", 1, "unit"))
	applyRefusingNothing(t, pool, engine,
		intake.PullRequestClosed{Repo: "sjawhar/legion", Number: 42, HeadSHA: "head", UpdatedAt: time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)},
		intake.PullRequestOpened{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head",
			UpdatedAt: time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC), Reopened: true},
		settledAt("head", 2, "unit"))

	want := []record.RoleMessage{}
	for range 2 {
		for _, role := range []claim.Role{claim.RoleArchitect, claim.RoleTester, claim.RoleImplementer} {
			want = append(want, record.RoleMessage{Role: roleToken(t, role), Body: toldBody("CI settled at head: \"build\" succeeded, \"unit\" failed")})
		}
	}
	assertRoleMessages(t, pool, "LEGION-208", want)
}

// The implementer pushes and completes without waiting for CI, so a check can settle on its head
// while the issue is still in implementing. The completion reads none of it: the issue goes on to
// testing whatever the settlement said, since a CI result moves nothing and the roles were told
// what it said when it arrived. The rule this replaced read the settlement here and sent the work
// straight back to implementing rather than through a tester round.
func TestACompletionHandsOnASettledHeadWithoutReadingIt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failing []string
	}{
		{"a check failed", []string{"unit"}},
		{"every check passed", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			engine := testEngine(config.DesignGateRootIssues, nil)
			applyRefusingNothing(t, pool, engine, settledAt("head", 1, tc.failing...))
			if got := issuePhase(t, pool); got != phase.Implementing {
				t.Fatalf("after the settlement the issue is in %s, want implementing", got)
			}
			applyRefusingNothing(t, pool, engine, intake.HandoffComplete{Generation: 1, Issue: "LEGION-208",
				Role: claim.RoleImplementer, Claim: "implement-claim", Commit: "head", Summary: "implemented"})

			if got := issuePhase(t, pool); got != phase.Testing {
				t.Fatalf("after the implementer's completion the issue is in %s, want testing", got)
			}
			if got := noticeKinds(t, pool, "LEGION-208"); containsNotice(got, "checks-red") {
				t.Fatalf("notices %v, want no checks-red", got)
			}
			if pass := implementerPass(t, pool); pass != 0 {
				t.Fatalf("pass = %d, want none: nothing went back", pass)
			}
		})
	}
}

// A head that conflicts with its base sends its issue back to implementing from whatever later
// phase it stands in: GitHub computes no merge ref for a conflicting head, so it runs no checks on
// it at all and no CI result will ever arrive for it, and only the implementer merging the base
// forward clears it. The daemon's own read of GitHub's mergeability (workflow's mergeability) is
// the only path to it, since GitHub sends no event when it recomputes one. The move names the
// base the implementer must merge forward and counts the return as a pass, so the implementer's
// completion of the merge is told from its completion of the pass before. GitHub reports
// mergeability UNKNOWN until it has computed it - that
// is "not yet known", never a conflict - so an unknown or a mergeable read moves nothing, and
// neither does a conflict read in implementing, where the work it needs already is.
func TestAConflictingHeadSendsTheIssueBackToImplementingFromEveryLaterPhase(t *testing.T) {
	for _, tc := range []struct {
		name      string
		from      phase.Phase
		status    string
		mergeable record.Mergeability
		want      phase.Phase
	}{
		{"conflicting in testing", phase.Testing, "testing", record.MergeabilityConflicting, phase.Implementing},
		{"conflicting in reviewing", phase.Reviewing, "needs_review", record.MergeabilityConflicting, phase.Implementing},
		{"conflicting in accepting", phase.Accepting, "needs_review", record.MergeabilityConflicting, phase.Implementing},
		{"conflicting in retro", phase.Retro, "retro", record.MergeabilityConflicting, phase.Implementing},
		{"conflicting in merging", phase.Merging, "retro", record.MergeabilityConflicting, phase.Implementing},
		{"conflicting in implementing: the work it needs is already there", phase.Implementing, "in_progress", record.MergeabilityConflicting, phase.Implementing},
		{"unknown in merging: GitHub has not computed it yet", phase.Merging, "retro", record.MergeabilityUnknown, phase.Merging},
		{"mergeable in merging", phase.Merging, "retro", record.MergeabilityMergeable, phase.Merging},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: tc.from, Generation: 1, Status: tc.status, Rank: "U"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			applyRefusingNothing(t, pool, mergingEngine(),
				intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "main", HeadSHA: "head", Mergeable: tc.mergeable})
			if got := issuePhase(t, pool); got != tc.want {
				t.Fatalf("after the mergeability read the issue is in %s, want %s", got, tc.want)
			}
			if tc.from == tc.want {
				assertOutboxCount(t, pool, "supervise", 0)
				if pass := implementerPass(t, pool); pass != 0 {
					t.Fatalf("pass = %d, want none counted: nothing went back", pass)
				}
				return
			}
			want := "the head conflicts with main: GitHub runs no checks on it; merge main forward"
			reason, from := checksRedNotice(t, pool)
			var status string
			if err := pool.QueryRow(context.Background(), "select payload->>'status' from outbox where kind = 'dispatch_status'").Scan(&status); err != nil {
				t.Fatalf("read the status write: %v", err)
			}
			if task := implementerTask(t, pool); !strings.Contains(task, want) || !strings.Contains(reason, want) || from != tc.from || status != "in_progress" {
				t.Fatalf("task %q, notice %q from %q, status %q; want both to say %q, from %s, and the issue in_progress", task, reason, from, status, want, tc.from)
			}
			if pass := implementerPass(t, pool); pass != 1 {
				t.Fatalf("pass = %d, want the return to implementing counted", pass)
			}
		})
	}
}

// The handler decides on every CONFLICTING read, whether or not the record already said so: the
// read that found the conflict first may have run while the issue stood in a phase the trigger has
// no row for, and deciding only on a read that changes the stored value would leave that conflict
// unacted on for good - every later read of the same CONFLICTING answer "changes" nothing. What
// stops the repeat from acting twice is the move itself: implementing, where the conflict sends
// the issue, has no row for the trigger, so the same read again changes nothing and tells nobody
// a second time.
func TestEveryConflictingReadDecidesAndTheMoveItselfStopsTheRepeat(t *testing.T) {
	pool := migratedPool(t)
	engine := testEngine(config.DesignGateRootIssues, nil)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
		Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "U"})
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 1, ApprovedVersion: new(1)})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
		Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim"})

	conflict := intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "main", HeadSHA: "head", Mergeable: record.MergeabilityConflicting}
	applyRefusingNothing(t, pool, engine, conflict)
	if got := issuePhase(t, pool); got != phase.Implementing {
		t.Fatalf("after the conflicting read the issue is in %s, want implementing: the merger cannot merge a head GitHub will not", got)
	}

	applyRefusingNothing(t, pool, engine, conflict)
	if got := issuePhase(t, pool); got != phase.Implementing {
		t.Fatalf("after the same conflicting read again the issue is in %s, want implementing", got)
	}
	want := "the head conflicts with main: GitHub runs no checks on it; merge main forward"
	var notices int
	if err := pool.QueryRow(context.Background(), "select count(*) from outbox where kind = 'notice' and payload->>'kind' = 'checks-red'").Scan(&notices); err != nil {
		t.Fatalf("count the architect's checks-red notices: %v", err)
	}
	reason, _ := checksRedNotice(t, pool)
	if notices != 1 || !strings.Contains(reason, want) {
		t.Fatalf("checks-red notices = %d, reason %q; want exactly one saying %q", notices, reason, want)
	}
	if pass := implementerPass(t, pool); pass != 1 {
		t.Fatalf("pass = %d, want the one return to implementing counted once", pass)
	}
}

// settledAt is a CI settlement of head reporting the two check runs build and unit, generation
// numbering it and its fence, with the checks named failing.
func settledAt(head string, generation int64, failing ...string) intake.Fact {
	return intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: head,
		CheckRuns:  []record.AttemptRun{{Name: "build", ID: generation}, {Name: "unit", ID: generation}},
		Generation: generation, Snapshot: head + "-" + strconv.FormatInt(generation, 10),
		Failing: append([]string{}, failing...)}
}

// syncedTo is GitHub's synchronize of the pull request to head.
func syncedTo(head string) intake.Fact {
	return intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: head}
}

// pushedFromTo is the branch push that replaced from with to, changing paths (newline-separated),
// by pusher.
func pushedFromTo(from, to, paths, pusher string) intake.Fact {
	unforced, whole := "false", "false"
	return intake.Push{Repo: "sjawhar/legion", Branch: "legion/LEGION-208", Before: from, After: to,
		ChangedPaths: &paths, Truncated: &whole, Forced: &unforced, Pusher: pusher}
}

// toldBody is the whole message a role is sent for one settlement of pull request 42 (workflow's
// tellChecks): what the settlement says, and that nothing moved on it.
func toldBody(settled string) string {
	return "Pull request #42: " + settled + ". Nothing moved on it: decide whether to re-run the checks, wait for them, or fix the code."
}

// roleToken is the Envoy role token of LEGION-208's role, the address a role message carries.
func roleToken(t *testing.T, role claim.Role) string {
	t.Helper()
	token, err := claim.NewToken("legion", "LEGION-208", role)
	if err != nil {
		t.Fatalf("role token of %s: %v", role, err)
	}
	return string(token)
}

// assertRoleMessages holds the role messages the outbox has queued for issue, in order, to want.
func assertRoleMessages(t *testing.T, pool *pgxpool.Pool, issue string, want []record.RoleMessage) {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select payload->>'role', payload->>'body' from outbox where kind = 'role_message' and issue = $1 order by id", issue)
	if err != nil {
		t.Fatalf("read the role messages: %v", err)
	}
	defer rows.Close()
	var got []record.RoleMessage
	for rows.Next() {
		var message record.RoleMessage
		if err := rows.Scan(&message.Role, &message.Body); err != nil {
			t.Fatalf("scan a role message: %v", err)
		}
		got = append(got, message)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the role messages: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("%d role messages %+v, want %d %+v", len(got), got, len(want), want)
	}
	for i, message := range got {
		if message != want[i] {
			t.Fatalf("role message %d is %+v, want %+v", i+1, message, want[i])
		}
	}
}

// checksRedNotice is the architect's checks-red notice the outbox holds: its reason, and the phase
// the conflict or the failed merge stopped.
func checksRedNotice(t *testing.T, pool *pgxpool.Pool) (reason string, from phase.Phase) {
	t.Helper()
	var stopped string
	if err := pool.QueryRow(context.Background(), "select payload->>'reason', coalesce(payload->>'phase', '') from outbox where kind = 'notice' and payload->>'kind' = 'checks-red'").Scan(&reason, &stopped); err != nil {
		t.Fatalf("read the architect's checks-red notice: %v", err)
	}
	return reason, phase.Phase(stopped)
}

// implementerStart is the implementer's start the outbox holds: its task, and the role the start
// waits for before it acts (record.SuperviseRequest's Quiesce), "" for none.
func implementerStart(t *testing.T, pool *pgxpool.Pool) (task string, quiesce claim.Role) {
	t.Helper()
	var waits string
	if err := pool.QueryRow(context.Background(), "select payload->>'task', coalesce(payload->>'quiesce', '') from outbox where kind = 'supervise' and payload->>'op' = 'start' and payload->>'role' = 'implementer'").Scan(&task, &waits); err != nil {
		t.Fatalf("read the implementer's start: %v", err)
	}
	return task, claim.Role(waits)
}

// applyRefusingNothing applies each fact as its own event, and fails the test on a refusal.
func applyRefusingNothing(t *testing.T, pool *pgxpool.Pool, engine *Engine, facts ...intake.Fact) {
	t.Helper()
	apply := applyFacts(t, pool, engine)
	for _, fact := range facts {
		if result := apply(t.Name()+randomSuffix(t), fact); result.Refusal != nil {
			t.Fatalf("apply %T = %+v", fact, result.Refusal)
		}
	}
}

// implementerTask is the task the implementer was last started with, "" when it was not started.
func implementerTask(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	return startTask(t, pool, claim.RoleImplementer)
}

// startTask is the task role was last started with, "" when it was not started.
func startTask(t *testing.T, pool *pgxpool.Pool, role claim.Role) string {
	t.Helper()
	var task string
	if err := pool.QueryRow(context.Background(), `select coalesce(max(payload->>'task'), '') from outbox
		where kind = 'supervise' and payload->>'op' = 'start' and payload->>'role' = $1`, string(role)).Scan(&task); err != nil {
		t.Fatalf("read the %s's start: %v", role, err)
	}
	return task
}

// implementerPass is the implementer's count of the issue's moves back into an earlier phase.
func implementerPass(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var pass int
	if err := pool.QueryRow(context.Background(), "select pass from phases where issue = 'LEGION-208' and role = 'implementer'").Scan(&pass); err != nil {
		t.Fatalf("read the implementer's pass: %v", err)
	}
	return pass
}

// implementerRounds is the implementer's count of the review rounds a review ended by requesting
// changes, which review_round_cap bounds.
func implementerRounds(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var rounds int
	if err := pool.QueryRow(context.Background(), "select rounds from phases where issue = 'LEGION-208' and role = 'implementer'").Scan(&rounds); err != nil {
		t.Fatalf("read the implementer's rounds: %v", err)
	}
	return rounds
}

// recordedPR is issue's pull request as the store reads it back.
func recordedPR(t *testing.T, pool *pgxpool.Pool, issue string) record.PullRequest {
	t.Helper()
	var pr *record.PullRequest
	seedRecord(t, pool, func(tx pgx.Tx) (err error) {
		pr, err = record.NewStore().PullRequest(t.Context(), tx, issue)
		return err
	})
	if pr == nil {
		t.Fatalf("%s has no pull request recorded", issue)
	}
	return *pr
}
