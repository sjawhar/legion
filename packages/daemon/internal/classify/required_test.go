package classify

import (
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// Only a check the base branch requires decides whether CI is red at a head, under the rule READY
// refuses by: failed or cancelled (the settlement's failing names) or missing from a settled head is
// red, a pending check is not red yet, and nothing decides while the required set is unread.
func TestHeadVerdictJudgesOnlyTheChecksTheBaseBranchRequires(t *testing.T) {
	// The live shape: the repository's one required gate passed, and two workflow_dispatch lanes
	// and an advisory review check failed beside it.
	settled := record.PullRequest{HeadSHA: "code", CheckedHead: "code", Verdict: "red",
		CheckRuns: []record.AttemptRun{{Name: "pr-checks-result", ID: 1}, {Name: "dev-apply / dev-chain-tripwire", ID: 2}, {Name: "review", ID: 3}},
		Failing:   []string{"dev-apply / dev-chain-tripwire", "dev-apply / staging-e2e / staging-e2e", "review"}}
	for _, tc := range []struct {
		name     string
		pr       func(record.PullRequest) record.PullRequest
		verdict  string
		standing []Standing
	}{
		{"reds the base branch does not require", func(pr record.PullRequest) record.PullRequest {
			pr.Required = []string{"pr-checks-result"}
			return pr
		}, "green", []Standing{{"pr-checks-result", Success}}},
		{"a required check that failed or was cancelled", func(pr record.PullRequest) record.PullRequest {
			pr.Required = []string{"pr-checks-result", "review"}
			return pr
		}, "red", []Standing{{"pr-checks-result", Success}, {"review", failed}}},
		{"a required check the settled head reports no result for", func(pr record.PullRequest) record.PullRequest {
			pr.Required = []string{"lint", "pr-checks-result"}
			return pr
		}, "red", []Standing{{"lint", Missing}, {"pr-checks-result", Success}}},
		{"a base branch that requires no check", func(pr record.PullRequest) record.PullRequest {
			pr.Required = []string{}
			return pr
		}, "green", []Standing{}},
		{"a required set never read", func(pr record.PullRequest) record.PullRequest {
			return pr
		}, "", nil},
		{"the code head's settlement carried to the handoff head that replaced it", func(pr record.PullRequest) record.PullRequest {
			pr.Required = []string{"pr-checks-result"}
			pr.HeadSHA = "handoff"
			pr.Pushes = []record.ClassifiedPush{{SHA: "handoff", Before: "code", HandoffOnly: true}}
			return pr
		}, "green", []Standing{{"pr-checks-result", Success}}},
		{"a head a push that may change code made, which no settlement stands for yet", func(pr record.PullRequest) record.PullRequest {
			pr.Required = []string{"pr-checks-result"}
			pr.HeadSHA = "fix"
			return pr
		}, "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := tc.pr(settled)
			if got := HeadVerdict(pr); got != tc.verdict {
				t.Errorf("HeadVerdict = %q, want %q", got, tc.verdict)
			}
			standing, _ := HeadChecks(pr)
			if !slices.Equal(standing, tc.standing) {
				t.Errorf("HeadChecks = %+v, want %+v", standing, tc.standing)
			}
		})
	}
}

// READY and the workflow share one rule: a required check still running is not red, only not
// passed yet; one the head reports nothing for, or that ended anything but a pass, is.
func TestJudgeReadsAPendingRequiredCheckAsNotRedYet(t *testing.T) {
	standing := Judge([]string{"lint", "test", "typecheck"}, map[string]string{"lint": Pending, "test": "cancelled", "unrelated": "failure"})
	want := []Standing{{"lint", Pending}, {"test", "cancelled"}, {"typecheck", Missing}}
	if !slices.Equal(standing, want) {
		t.Fatalf("Judge = %+v, want %+v", standing, want)
	}
	if standing[0].Red() || !standing[1].Red() || !standing[2].Red() {
		t.Fatalf("Red = %t %t %t, want pending not red, cancelled and missing red", standing[0].Red(), standing[1].Red(), standing[2].Red())
	}
}
