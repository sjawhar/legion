package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// transferredLegion is the pair a deployment configures over sjawhar/legion's transfer to
// cybermonklabs: the records of a tree opened before it keep sjawhar/legion, while GitHub holds
// the repository under cybermonklabs/legion, on cybermonklabs' installation.
var transferredLegion = ghrepo.MustRenames(ghrepo.MustRename("sjawhar/legion", "cybermonklabs/legion"))

// renameOutbox is the outbox a transfer's rows run on: the implement App (roleTokens, whose token
// names the owner it was minted for), GitHub at api, and the project's mapping — served, the one
// repository Dispatch maps, and the configured pairs — as the boot builds it.
func renameOutbox(t *testing.T, pool *pgxpool.Pool, records record.Store, api, served string, renames ghrepo.Renames) *outbox {
	t.Helper()
	repos := newRepositories("LEGION", renames)
	repos.set([]mappedRepository{{Repo: ghrepo.MustParse(served), Default: true}}, nil)
	return &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, tokens: roleTokens{},
		project: "legion", githubAPI: api, repositories: repos, now: time.Now}
}

// seedTransferredREADY records LEGION-208 on sjawhar/legion awaiting the merge of its READY head,
// its pull request #42 open there: a tree whose records were written before the transfer.
func seedTransferredREADY(t *testing.T, pool *pgxpool.Pool, records record.Store) {
	t.Helper()
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.AwaitingMerge, Generation: 1, Status: "retro", Rank: "U"})
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := records.PutPhase(ctx, tx, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: "ready-head"}); err != nil {
			return err
		}
		return records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
			Number: 42, Branch: "legion/LEGION-208", HeadSHA: "ready-head", Failing: []string{}})
	}); err != nil {
		t.Fatalf("seed LEGION-208's READY: %v", err)
	}
}

// renameGitHub is GitHub as a transferred repository's rows reach it: every REST path and every
// GraphQL variable pair naming a repository, with the bearer each request carried.
type renameGitHub struct {
	url     string
	mu      sync.Mutex
	calls   []renameCall
	answers func(body string) string
}

// renameCall is one request: the repository it named — the `<owner>/<name>` of a REST path or of
// a GraphQL query's variables, empty for a call that names none — and its Authorization header.
type renameCall struct {
	repo   string
	bearer string
}

func newRenameGitHub(t *testing.T, answers func(body string) string) *renameGitHub {
	t.Helper()
	g := &renameGitHub{answers: answers}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		call := renameCall{bearer: r.Header.Get("Authorization")}
		if parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/repos/"), "/", 3); r.URL.Path != "/graphql" && len(parts) >= 2 {
			call.repo = parts[0] + "/" + parts[1]
		}
		if r.URL.Path == "/graphql" {
			var query struct {
				Variables struct{ Owner, Name string } `json:"variables"`
			}
			if err := json.Unmarshal(body, &query); err != nil {
				t.Errorf("decode the GraphQL request %s: %v", body, err)
			}
			if query.Variables.Owner != "" {
				call.repo = query.Variables.Owner + "/" + query.Variables.Name
			}
		}
		g.mu.Lock()
		g.calls = append(g.calls, call)
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, g.answers(string(body)))
	}))
	t.Cleanup(server.Close)
	g.url = server.URL
	return g
}

// named is every repository GitHub was asked about, and every bearer it was asked with.
func (g *renameGitHub) named() (repos, bearers []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, call := range g.calls {
		if call.repo != "" {
			repos = append(repos, call.repo)
		}
		bearers = append(bearers, call.bearer)
	}
	return repos, bearers
}

// assertAddressed holds every call GitHub received to one repository and one bearer: the token
// minted for that repository's owner, and no call naming any other repository.
func (g *renameGitHub) assertAddressed(t *testing.T, repo string) {
	t.Helper()
	repos, bearers := g.named()
	if len(repos) == 0 {
		t.Fatalf("GitHub was asked about no repository at all, want %s", repo)
	}
	owner := ghrepo.MustParse(repo).Owner()
	for _, named := range repos {
		if named != repo {
			t.Errorf("GitHub was asked about %s, want %s: a record's name is no address once the repository has moved", named, repo)
		}
	}
	for _, bearer := range bearers {
		if want := "Bearer implement-app-token-for-" + owner; bearer != want {
			t.Errorf("a GitHub call carried %q, want %q: the token is minted for the owner whose installation holds the repository", bearer, want)
		}
	}
}

// A READY merge of a tree recorded before its repository's transfer is submitted on the
// repository GitHub holds it under now, with the implement App's token minted for that
// repository's owner: the record keeps sjawhar/legion, the configured pair and the project's
// served cybermonklabs/legion make cybermonklabs the owner whose installation holds it, and the
// GraphQL query names cybermonklabs/legion. A GraphQL query is not a git operation, so GitHub's
// redirect of a transferred repository does not cover it and a query naming the old repository
// would be answered by nothing, leaving a tree open at the transfer unmerged. With no pair
// configured the same record is minted for and addressed at sjawhar/legion, as it always was.
func TestAREADYMergeOfATreeRecordedBeforeItsTransferAddressesTheRepositoryGitHubServes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		renames ghrepo.Renames
		served  string
		want    string
	}{
		{name: "the pair configured", renames: transferredLegion, served: "cybermonklabs/legion", want: "cybermonklabs/legion"},
		{name: "no pair configured", served: "sjawhar/legion", want: "sjawhar/legion"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			merges := 0
			var mu sync.Mutex
			github := newRenameGitHub(t, func(body string) string {
				mu.Lock()
				defer mu.Unlock()
				if strings.Contains(body, "mergePullRequest") || strings.Contains(body, "enablePullRequestAutoMerge") {
					merges++
					return `{"data":{}}`
				}
				return pullJSON("OPEN", "ready-head", "CLEAN")
			})
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			seedTransferredREADY(t, pool, records)
			runner := renameOutbox(t, pool, records, github.url, tc.served, tc.renames)

			if err := runner.execute(context.Background(), mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, time.Now())); err != nil {
				t.Fatalf("execute the READY merge: %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			if merges != 1 {
				t.Fatalf("merges = %d, want the one submission of the awaited READY head", merges)
			}
			github.assertAddressed(t, tc.want)
		})
	}
}

// A disarm of the same tree's pull request reads and writes the same repository: readyMerge and
// autoMergeDisable share the one client (graphQL), so both follow the transfer together.
func TestAnAutoMergeDisableOfATreeRecordedBeforeItsTransferAddressesTheRepositoryGitHubServes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		renames ghrepo.Renames
		served  string
		want    string
	}{
		{name: "the pair configured", renames: transferredLegion, served: "cybermonklabs/legion", want: "cybermonklabs/legion"},
		{name: "no pair configured", served: "sjawhar/legion", want: "sjawhar/legion"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			disables := 0
			github := newRenameGitHub(t, func(body string) string {
				mu.Lock()
				defer mu.Unlock()
				if strings.Contains(body, "disablePullRequestAutoMerge") {
					disables++
					return `{"data":{"disablePullRequestAutoMerge":{"clientMutationId":null}}}`
				}
				return armedPullJSON("other-head", disables == 0)
			})
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			seedTransferredREADY(t, pool, records)
			runner := renameOutbox(t, pool, records, github.url, tc.served, tc.renames)

			if err := runner.execute(context.Background(), mustOutboxRow(t, "LEGION-208", record.AutoMergeDisable{Number: 42}, time.Now())); err != nil {
				t.Fatalf("execute the disarm: %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			if disables != 1 {
				t.Fatalf("auto-merge disables = %d, want the one disarm of a head the issue does not await", disables)
			}
			github.assertAddressed(t, tc.want)
		})
	}
}

// A production check's compare of a tree recorded before its repository's transfer is a REST read
// of the repository GitHub holds it under now, on the token minted for that repository's owner:
// the check's record keeps sjawhar/legion, and the compare that wakes the check is addressed at
// cybermonklabs/legion under the configured pair and at sjawhar/legion without it. A REST read
// addressed at a repository GitHub has transferred away is no git operation either, and the wake
// it carries is the check's only way forward.
func TestAProductionCheckCompareOfATreeRecordedBeforeItsTransferAddressesTheRepositoryGitHubServes(t *testing.T) {
	const merge, build = "a1b2c3d4e5f6a7b8c9d0a1b2c3d4e5f6a7b8c9d0", "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c"
	for _, tc := range []struct {
		name    string
		renames ghrepo.Renames
		served  string
		want    string
	}{
		{name: "the pair configured", renames: transferredLegion, served: "cybermonklabs/legion", want: "cybermonklabs/legion"},
		{name: "no pair configured", served: "sjawhar/legion", want: "sjawhar/legion"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newRenameGitHub(t, func(string) string { return `{"status":"ahead","ahead_by":1,"behind_by":0,"files":[]}` })
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			until := time.Now().Add(time.Hour)
			root := record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Project: "LEGION",
				Tree: "LEGION-208", Title: "root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "U", LingerUntil: &until}
			putOutboxIssue(t, pool, records, root)
			opened := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
			check := record.ProductionCheck{Issue: root.Key, Project: root.Project, Tree: root.Tree, Repo: "sjawhar/legion", Number: 42, MergeSHA: merge,
				OpenedAt: opened, BoundAt: opened.Add(336 * time.Hour), Looks: 1, LookOpen: true, LookWakeAt: &opened,
				LastWakeKind: "merge", LastWakeDetail: merge, LastWakeAt: &opened}
			putProductionCheck(t, pool, records, check)
			facts := &outboxFactHandler{}
			runner := renameOutbox(t, pool, records, github.url, tc.served, tc.renames)
			runner.handlers = []intake.Handler{facts}

			row := mustOutboxRow(t, root.Key, record.CheckCompare{Look: 1, Cause: "daemon boot", Build: build, OpenedAt: check.OpenedAt}, time.Now())
			row.ID = 71
			if err := runner.execute(context.Background(), row); err != nil {
				t.Fatalf("execute the check compare: %v", err)
			}

			github.assertAddressed(t, tc.want)
			want := intake.CheckWake{Issue: root.Key, Look: 1, Cause: "daemon boot", Detail: build, OpenedAt: check.OpenedAt}
			if got := facts.facts(); len(got) != 1 || fmt.Sprint(got[0]) != fmt.Sprint(want) {
				t.Fatalf("facts applied = %+v, want the check woken by %+v", got, want)
			}
		})
	}
}
