package requiredchecks

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
)

// GitHub pages a branch's rules, 30 to a page unless asked for more: a required_status_checks rule
// on a later page is required as much as one on the first, so a reader that stops at one page
// fails open. The stand-in serves 30 other rules on page 1 and the required one on page 2, and
// answers whatever page size the reader asks for with 30 rules a page all the same.
func TestRequiredReadsEveryPageOfTheBranchsRules(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widgets/rules/branches/main":
			if r.URL.Query().Get("page") == "2" {
				w.Write([]byte(`[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"pr-checks-result"}]}}]`))
				return
			}
			w.Header().Set("Link", fmt.Sprintf(`<http://%s/repos/acme/widgets/rules/branches/main?page=2>; rel="next", <http://%s/repos/acme/widgets/rules/branches/main?page=2>; rel="last"`, r.Host, r.Host))
			w.Write([]byte("[" + strings.TrimSuffix(strings.Repeat(`{"type":"pull_request"},`, 30), ",") + "]"))
		case "/repos/acme/widgets/branches/main":
			w.Write([]byte(`{"name":"main","protected":false}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	required, err := Required(context.Background(), githubrest.Client{Token: "token", API: server.URL + "/repos/acme/widgets"}, "main")
	if err != nil || !slices.Equal(required.Checks, []string{"pr-checks-result"}) {
		t.Fatalf("Required = %#v, %v; want the rule on page 2, pr-checks-result", required, err)
	}
}

// A branch GitHub calls protected, answered with no protection summary, is a read that failed, not
// a branch that requires nothing: reading it as none would fail open, and the workflow would judge
// the head with no required check at all.
func TestRequiredRefusesAProtectedBranchWithNoProtectionSummary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widgets/rules/branches/main":
			w.Write([]byte(`[]`))
		case "/repos/acme/widgets/branches/main":
			w.Write([]byte(`{"name":"main","protected":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	required, err := Required(context.Background(), githubrest.Client{Token: "token", API: server.URL + "/repos/acme/widgets"}, "main")
	if err == nil || !strings.Contains(err.Error(), "no protection summary") {
		t.Fatalf("Required = %#v, %v; want an error naming the missing protection summary", required, err)
	}
}

// A ruleset's workflows rule requires a workflow to succeed, beside the required_status_checks
// rule's checks. The rule shapes are a live repository's: a workflows rule naming its review
// workflow by path, ref and repository id, a required_status_checks rule for one aggregator check,
// a pull_request rule requiring no approval, and no branch protection.
func TestRequiredReadsARulesetsRequiredWorkflowsBesideItsChecks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widgets/rules/branches/main":
			w.Write([]byte(`[{"type":"workflows","parameters":{"do_not_enforce_on_create":true,"workflows":[{"repository_id":4242,"path":".github/workflows/claude-pr-review.yml","ref":"refs/heads/main"}]}},` +
				`{"type":"deletion","parameters":null},{"type":"pull_request","parameters":{"required_approving_review_count":0}},` +
				`{"type":"required_status_checks","parameters":{"strict_required_status_checks_policy":false,"required_status_checks":[{"context":"pr-checks-result","integration_id":15368}]}}]`))
		case "/repos/acme/widgets/branches/main":
			w.Write([]byte(`{"name":"main","protected":false}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	required, err := Required(context.Background(), githubrest.Client{Token: "token", API: server.URL + "/repos/acme/widgets"}, "main")
	want := []Workflow{{Path: ".github/workflows/claude-pr-review.yml", RepositoryID: 4242}}
	if err != nil || !slices.Equal(required.Checks, []string{"pr-checks-result"}) || !slices.Equal(required.Workflows, want) {
		t.Fatalf("Required = %#v, %v; want the check pr-checks-result and the workflow %#v", required, err, want)
	}
}

// A required workflow stands as its latest run on the head that a pull request event started: a
// rerun after a failure is what GitHub judges, a run a push started is not the pull request's, a
// run of the same path in another repository is another workflow, and a workflow with no such run
// is missing. The runs come a hundred to a page, and a run on the second page counts.
func TestWorkflowsJudgesEachRequiredWorkflowByItsLatestPullRequestRun(t *testing.T) {
	run := func(id int, path, event, status, conclusion string, repository int) string {
		return fmt.Sprintf(`{"id":%d,"path":%q,"event":%q,"status":%q,"conclusion":%s,"repository":{"id":%d}}`, id, path, event, status, conclusion, repository)
	}
	pages := map[string][]string{
		"1": {
			run(10, ".github/workflows/review.yml", "pull_request", "completed", `"failure"`, 4242),
			run(11, ".github/workflows/review.yml", "push", "completed", `"failure"`, 4242),
			run(12, ".github/workflows/lint.yml", "pull_request", "completed", `"failure"`, 4242),
			run(13, ".github/workflows/lint.yml", "push", "completed", `"success"`, 4242),
			run(14, ".github/workflows/build.yml", "pull_request", "completed", `"success"`, 9999),
		},
		"2": {
			run(20, ".github/workflows/review.yml", "pull_request", "completed", `"success"`, 4242),
			run(21, ".github/workflows/e2e.yml", "pull_request_target", "in_progress", "null", 4242),
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widgets/actions/runs" || r.URL.Query().Get("head_sha") != "head" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"total_count":105,"workflow_runs":[%s]}`, strings.Join(pages[r.URL.Query().Get("page")], ","))
	}))
	defer server.Close()
	workflows := []Workflow{
		{Path: ".github/workflows/build.yml", RepositoryID: 4242},
		{Path: ".github/workflows/e2e.yml", RepositoryID: 4242},
		{Path: ".github/workflows/lint.yml", RepositoryID: 4242},
		{Path: ".github/workflows/review.yml", RepositoryID: 4242},
	}
	got, err := Workflows(context.Background(), githubrest.Client{Token: "token", API: server.URL + "/repos/acme/widgets"}, "head", workflows)
	want := []classify.Standing{
		{Name: ".github/workflows/build.yml", Result: classify.Missing},
		{Name: ".github/workflows/e2e.yml", Result: classify.Pending},
		{Name: ".github/workflows/lint.yml", Result: "failure"},
		{Name: ".github/workflows/review.yml", Result: classify.Success},
	}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("Workflows = %+v, %v; want %+v", got, err, want)
	}
}
