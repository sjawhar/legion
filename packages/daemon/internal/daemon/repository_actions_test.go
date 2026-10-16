package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// twoOwnersGitHub is GitHub's REST and GraphQL API for every repository, as the outbox's
// repository-dependent rows call it: each default branch's ref, each create of a ref, and each
// pull request read and disarm. It records each call as "<METHOD> <path or owner/name> <bearer>".
type twoOwnersGitHub struct {
	url   string
	mu    sync.Mutex
	calls []string
}

func newTwoOwnersGitHub(t *testing.T) *twoOwnersGitHub {
	t.Helper()
	g := &twoOwnersGitHub{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			var request struct {
				Query     string         `json:"query"`
				Variables map[string]any `json:"variables"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode the GraphQL request: %v", err)
			}
			g.record("GRAPHQL", request.Variables["owner"].(string)+"/"+request.Variables["name"].(string), bearer)
			_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequest":{"id":"PR_42","headRefOid":"head","autoMergeRequest":null,"mergeQueueEntry":null}}}}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/git/ref/heads/"):
			g.record(r.Method, r.URL.Path, bearer)
			_, _ = io.WriteString(w, `{"object":{"sha":"`+branchBaseCommit+`","type":"commit"}}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git/refs"):
			g.record(r.Method, r.URL.Path, bearer)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected GitHub request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	g.url = server.URL
	return g
}

func (g *twoOwnersGitHub) record(method, target, bearer string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, method+" "+target+" "+bearer)
}

func (g *twoOwnersGitHub) called() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.calls)
}

// Every repository-dependent action of the outbox reads the repository from the issue's record:
// two issues on repositories of two owners, each at a default branch of its own, have their
// branches created, their workspaces provisioned and removed and their pull requests disarmed in
// their own repositories, each with the implement App's token minted for its own owner.
func TestEachIssuesActionsActOnTheRepositoryItsRecordNames(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	github := newTwoOwnersGitHub(t)
	until := time.Now().Add(time.Hour)
	widgets := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-208", Title: "Widgets",
		Phase: phase.Done, Generation: 1, Status: "done", Rank: "U", LingerUntil: &until}
	tools := record.Issue{Key: "LEGION-209", Repo: "zeta/tools", DefaultBranch: "release/v7.1", GitHubRepo: "zeta/tools", Project: "LEGION", Tree: "LEGION-209", Title: "Tools",
		Phase: phase.Done, Generation: 1, Status: "done", Rank: "V", LingerUntil: &until}
	putOutboxIssue(t, pool, records, widgets)
	putOutboxIssue(t, pool, records, tools)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	stateDir := t.TempDir()
	var provisioned []string
	var removed []string
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, tokens: roleTokens{}, project: "legion",
		stateDir: stateDir, githubAPI: github.url,
		provision: func(_ context.Context, request workspace.Request) (workspace.Workspace, error) {
			provisioned = append(provisioned, request.Issue+" "+request.Repo.String()+"@"+request.DefaultBranch)
			return workspace.Workspace{Dir: t.TempDir()}, nil
		},
		remove: func(_ context.Context, working workspace.Workspace) error {
			removed = append(removed, working.Dir)
			return nil
		},
	}

	for _, issue := range []record.Issue{widgets, tools} {
		if err := runner.createBranch(context.Background(), issue); err != nil {
			t.Fatalf("create the branch of %s: %v", issue.Key, err)
		}
		if err := runner.provisionWorkspace(context.Background(), issue); err != nil {
			t.Fatalf("provision the workspace of %s: %v", issue.Key, err)
		}
		row := mustOutboxRow(t, issue.Key, record.AutoMergeDisable{Number: 42}, time.Now())
		if err := runner.autoMergeDisable(context.Background(), row, record.AutoMergeDisable{Number: 42}); err != nil {
			t.Fatalf("disarm the pull request of %s: %v", issue.Key, err)
		}
		row = mustOutboxRow(t, issue.Key, record.WorkspaceRemove{Linger: 1}, time.Now())
		if err := runner.removeWorkspace(context.Background(), row, record.WorkspaceRemove{Linger: 1}); err != nil {
			t.Fatalf("remove the workspace of %s: %v", issue.Key, err)
		}
	}

	want := []string{
		"GET /repos/acme/widgets/git/ref/heads/main implement-app-token-for-acme",
		"POST /repos/acme/widgets/git/refs implement-app-token-for-acme",
		"GRAPHQL acme/widgets implement-app-token-for-acme",
		"GET /repos/zeta/tools/git/ref/heads/release/v7.1 implement-app-token-for-zeta",
		"POST /repos/zeta/tools/git/refs implement-app-token-for-zeta",
		"GRAPHQL zeta/tools implement-app-token-for-zeta",
	}
	if got := github.called(); !slices.Equal(got, want) {
		t.Fatalf("GitHub calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if want := []string{"LEGION-208 acme/widgets@main", "LEGION-209 zeta/tools@release/v7.1"}; !slices.Equal(provisioned, want) {
		t.Fatalf("provisioned %v, want %v", provisioned, want)
	}
	var wantRemoved []string
	for _, issue := range []record.Issue{widgets, tools} {
		located, err := workspace.Location(stateDir, ghrepo.MustParse(issue.Repo), issue.Key)
		if err != nil {
			t.Fatal(err)
		}
		wantRemoved = append(wantRemoved, located.Dir)
	}
	if !slices.Equal(removed, wantRemoved) {
		t.Fatalf("removed %v, want each issue's clone in its own repository, %v", removed, wantRemoved)
	}
}

// A row queued before the boot filled its issue's record with a repository acts on the filled
// record's repository when it runs: the row names only the issue, and the outbox reads the
// repository from the record at that moment.
func TestARowQueuedBeforeTheFillActsOnTheFilledRepository(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := projectRecords{Store: record.NewStore(), project: "LEGION"}
	if _, err := pool.Exec(context.Background(), `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-7', 'LEGION-7', 'LEGION', 'queued', 'admitted', 1, 'in_progress', 'A', 1)`); err != nil {
		t.Fatal(err)
	}
	enqueueOutbox(t, pool, records.Store, mustOutboxRow(t, "LEGION-7", record.IssueBranch{Generation: 1}, time.Now()))
	enqueueOutbox(t, pool, records.Store, mustOutboxRow(t, "LEGION-7", record.AutoMergeDisable{Number: 42}, time.Now()))

	named := newRepositoryGitHubFor(t, "Zeta/Tools", "release/v7.1")
	client := &listingDispatch{summaries: []dispatch.IssueSummary{{Key: "LEGION-7", ResolvedRepo: "zeta/tools"}}}
	if err := backfillRepositories(context.Background(), pool, records, client, "LEGION", t.TempDir(),
		newDefaultBranches(roleTokens{}, named, defaultBranchTTL, time.Now), quietLogger()); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	github := newTwoOwnersGitHub(t)
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records.Store, tokens: roleTokens{}, project: "legion",
		githubAPI: github.url, now: time.Now}
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the queued rows: %v", err)
	}
	want := []string{
		"GET /repos/Zeta/Tools/git/ref/heads/release/v7.1 implement-app-token-for-Zeta",
		"POST /repos/Zeta/Tools/git/refs implement-app-token-for-Zeta",
		"GRAPHQL Zeta/Tools implement-app-token-for-Zeta",
	}
	if got := github.called(); !slices.Equal(got, want) {
		t.Fatalf("GitHub calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// newRepositoryGitHubFor answers GET /repos/{owner}/{repo} for any repository as fullName at
// defaultBranch.
func newRepositoryGitHubFor(t *testing.T, fullName, defaultBranch string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"full_name":"`+fullName+`","default_branch":"`+defaultBranch+`"}`)
	}))
	t.Cleanup(server.Close)
	return server.URL
}
