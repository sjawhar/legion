package classify

import (
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// Only a check the base branch requires decides whether CI is red at a head: one the settlement
// names failed is red, one it names cancelled is pending, and one it does not report is missing,
// since a settlement can come before a required check is decided, so the head has no verdict until
// a later settlement decides it; nothing decides while the required set is unread, and nothing
// decides at a base that requires no check and no workflow, where nothing was judged to pass. A
// settlement stands only for the head it names: a head pushed above it, its push known or not, has
// no verdict until its own settlement comes. A required workflow is judged by its run as the daemon
// read it at the head: a cancelled run is red there.
func TestHeadVerdictJudgesOnlyTheChecksTheBaseBranchRequires(t *testing.T) {
	// The live shape: the repository's one required gate passed, two workflow_dispatch lanes and an
	// advisory review check failed beside it, and an image build was cancelled.
	settled := record.PullRequest{HeadSHA: "code", CheckedHead: "code",
		CheckRuns: []record.AttemptRun{{Name: "pr-checks-result", ID: 1}, {Name: "dev-apply / dev-chain-tripwire", ID: 2}, {Name: "review", ID: 3}, {Name: "build-image", ID: 4}},
		Failing:   []string{"dev-apply / dev-chain-tripwire", "dev-apply / staging-e2e / staging-e2e", "review"},
		Cancelled: []string{"build-image"}}
	requiring := func(names ...string) func(record.PullRequest) record.PullRequest {
		return func(pr record.PullRequest) record.PullRequest {
			pr.Required = append([]string{}, names...)
			return pr
		}
	}
	// requiringReview requires the check pr-checks-result and the workflow review.yml, whose run the
	// daemon read at head with result.
	requiringReview := func(result, head string) func(record.PullRequest) record.PullRequest {
		return func(pr record.PullRequest) record.PullRequest {
			pr = requiring("pr-checks-result")(pr)
			pr.Workflows = []record.RequiredWorkflow{{Path: "review.yml", Result: result}}
			pr.WorkflowsHead = head
			return pr
		}
	}
	// requiringOnlyReview requires no check and only the workflow review.yml, whose run the daemon
	// read at the settlement's head with result.
	requiringOnlyReview := func(result string) func(record.PullRequest) record.PullRequest {
		return func(pr record.PullRequest) record.PullRequest {
			pr = requiring()(pr)
			pr.Workflows = []record.RequiredWorkflow{{Path: "review.yml", Result: result}}
			pr.WorkflowsHead = "code"
			return pr
		}
	}
	for _, tc := range []struct {
		name     string
		pr       func(record.PullRequest) record.PullRequest
		verdict  string
		standing []Standing
	}{
		{"reds the base branch does not require", requiring("pr-checks-result"), "green", []Standing{{"pr-checks-result", Success}}},
		{"a required check that failed", requiring("pr-checks-result", "review"), "red", []Standing{{"pr-checks-result", Success}, {"review", Failed}}},
		{"a required check cancelled in the head's own run", requiring("build-image", "pr-checks-result"), "", []Standing{{"build-image", Pending}, {"pr-checks-result", Success}}},
		{"a required check the head's own settlement reports no result for", requiring("lint", "pr-checks-result"), "", []Standing{{"lint", Missing}, {"pr-checks-result", Success}}},
		{"a failed required check beside a missing one", requiring("lint", "review"), "red", []Standing{{"lint", Missing}, {"review", Failed}}},
		{"a base branch that requires no check", requiring(), "", []Standing{}},
		{"a base branch that requires only a workflow whose run succeeded", requiringOnlyReview(Success), "green", []Standing{{"review.yml", Success}}},
		{"a base branch that requires only a workflow whose run failed", requiringOnlyReview("failure"), "red", []Standing{{"review.yml", "failure"}}},
		{"a base branch that requires only a workflow still running", requiringOnlyReview(Pending), "", []Standing{{"review.yml", Pending}}},
		{"a required set never read", func(pr record.PullRequest) record.PullRequest { return pr }, "", nil},
		{"a head pushed above the settled one, whose push is known", func(pr record.PullRequest) record.PullRequest {
			pr.Required = []string{"pr-checks-result"}
			pr.HeadSHA = "next"
			pr.Pushes = []record.ClassifiedPush{{SHA: "next", Before: "code"}}
			return pr
		}, "", nil},
		{"a head a push made, which no settlement stands for yet", func(pr record.PullRequest) record.PullRequest {
			pr.Required = []string{"pr-checks-result"}
			pr.HeadSHA = "fix"
			return pr
		}, "", nil},
		{"a required workflow whose run failed", requiringReview("failure", "code"), "red", []Standing{{"pr-checks-result", Success}, {"review.yml", "failure"}}},
		{"a required workflow whose run was cancelled on the head itself", requiringReview("cancelled", "code"), "red", []Standing{{"pr-checks-result", Success}, {"review.yml", "cancelled"}}},
		{"a required workflow whose run succeeded", requiringReview(Success, "code"), "green", []Standing{{"pr-checks-result", Success}, {"review.yml", Success}}},
		{"a required workflow still running", requiringReview(Pending, "code"), "", []Standing{{"pr-checks-result", Success}, {"review.yml", Pending}}},
		{"a required workflow the head has no run of", requiringReview(Missing, "code"), "", []Standing{{"pr-checks-result", Success}, {"review.yml", Missing}}},
		{"a required workflow's success read at an earlier head", requiringReview(Success, "earlier"), "", []Standing{{"pr-checks-result", Success}, {"review.yml", Pending}}},
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

// The workflow's rule: a required check still running is not red, only not passed yet, and neither
// is one the head reports nothing for, which leaves the verdict undecided until a later settlement
// decides it; one that ended anything but a pass is red.
func TestJudgeReadsAPendingRequiredCheckAsNotRedYet(t *testing.T) {
	standing := Judge([]string{"lint", "test", "typecheck"}, map[string]string{"lint": Pending, "test": "cancelled", "unrelated": "failure"})
	want := []Standing{{"lint", Pending}, {"test", "cancelled"}, {"typecheck", Missing}}
	if !slices.Equal(standing, want) {
		t.Fatalf("Judge = %+v, want %+v", standing, want)
	}
	if standing[0].Red() || !standing[1].Red() || standing[2].Red() {
		t.Fatalf("Red = %t %t %t, want pending and missing not red, cancelled red", standing[0].Red(), standing[1].Red(), standing[2].Red())
	}
}
