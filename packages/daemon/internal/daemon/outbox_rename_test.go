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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// transferredLegion is the pair a deployment configures over sjawhar/legion's transfer to
// cybermonklabs: the records of a tree opened before it keep sjawhar/legion, while GitHub holds
// the repository under cybermonklabs/legion, on cybermonklabs' installation.
var transferredLegion = ghrepo.MustRenames(ghrepo.MustRename("sjawhar/legion", "cybermonklabs/legion"))

// transferStates are the states a transferred repository's calls are driven in: the configured
// pair with the project serving the repository GitHub holds it under, that same pair with one
// more repository of the owner the transfer moved it away from served beside it, and no pair at
// all. The second is the state of the production failure of 2026-11-06 — sjawhar/oh-my-pi added
// to the project's mapping made sjawhar an owner of the project, which switched the live pair
// off under the owner rule this replaced, and 47 trees could not push for 42 minutes — and it
// must address exactly what the first does. The third is the control: with no pair configured
// every call is addressed at the recorded repository, as it always was.
var transferStates = []struct {
	name    string
	renames ghrepo.Renames
	// served is the project's mapping, as GitHub names each repository it serves.
	served []string
	// want is the repository every GitHub call for a tree recorded on sjawhar/legion addresses,
	// and whose owner its token is minted for.
	want string
}{
	{name: "state A: the pair configured", renames: transferredLegion,
		served: []string{"cybermonklabs/legion"}, want: "cybermonklabs/legion"},
	{name: "state B: the pair configured and the old owner's other repository served", renames: transferredLegion,
		served: []string{"cybermonklabs/legion", "sjawhar/oh-my-pi"}, want: "cybermonklabs/legion"},
	{name: "no pair configured", served: []string{"sjawhar/legion"}, want: "sjawhar/legion"},
}

// renamedRepositories is the project's mapping as the boot builds it: the repositories Dispatch
// maps, the first of them the project's default, under the configured pairs.
func renamedRepositories(served []string, renames ghrepo.Renames) *repositories {
	repos := newRepositories("LEGION", renames)
	mapped := make([]mappedRepository, 0, len(served))
	for i, repo := range served {
		mapped = append(mapped, mappedRepository{Repo: ghrepo.MustParse(repo), Default: i == 0})
	}
	repos.set(mapped, nil)
	return repos
}

// renameOutbox is the outbox a transfer's rows run on: the implement App (roleTokens, whose token
// names the owner it was minted for), GitHub at api, and the project's mapping — served, the
// repositories Dispatch maps, and the configured pairs — as the boot builds it.
func renameOutbox(t *testing.T, pool *pgxpool.Pool, records record.Store, api string, served []string, renames ghrepo.Renames) *outbox {
	t.Helper()
	return &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, tokens: roleTokens{},
		project: "legion", githubAPI: api, repositories: renamedRepositories(served, renames), now: time.Now}
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

// renameGitHub is GitHub as a transferred repository's calls reach it: every REST path and every
// GraphQL variable pair naming a repository, with the bearer each request carried.
type renameGitHub struct {
	url     string
	mu      sync.Mutex
	calls   []renameCall
	answers func(r *http.Request, body string) string
}

// renameCall is one request: the method and path GitHub received, with the query where the call
// carries one, the repository it named — the `<owner>/<name>` of a REST path or of a GraphQL
// query's variables, empty for a call that names none — and its Authorization header.
type renameCall struct {
	method string
	path   string
	repo   string
	bearer string
}

func newRenameGitHub(t *testing.T, answers func(r *http.Request, body string) string) *renameGitHub {
	t.Helper()
	g := &renameGitHub{answers: answers}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		call := renameCall{method: r.Method, path: r.URL.RequestURI(), bearer: r.Header.Get("Authorization")}
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
		_, _ = io.WriteString(w, g.answers(r, string(body)))
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

// requests is every call GitHub received, as `<METHOD> <path> <bearer>`: the record of which
// repository each request addressed and which owner its token was minted for.
func (g *renameGitHub) requests() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	made := make([]string, 0, len(g.calls))
	for _, call := range g.calls {
		made = append(made, call.method+" "+call.path+" "+call.bearer)
	}
	return made
}

// assertAddressed holds every call GitHub received to one repository and one bearer: app's token
// minted for that repository's owner, and no call naming any other repository.
func (g *renameGitHub) assertAddressed(t *testing.T, app appauth.AppRole, repo string) {
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
		if want := "Bearer " + string(app) + "-app-token-for-" + owner; bearer != want {
			t.Errorf("a GitHub call carried %q, want %q: the token is minted for the owner whose installation holds the repository", bearer, want)
		}
	}
}

// A READY merge of a tree recorded before its repository's transfer is submitted on the
// repository GitHub holds it under now, with the implement App's token minted for that
// repository's owner: the record keeps sjawhar/legion, the configured pair and the project's
// served cybermonklabs/legion make cybermonklabs the owner whose installation holds it, and the
// GraphQL query names cybermonklabs/legion. The token is the half the old name cannot supply:
// after the transfer the installation holding the repository is cybermonklabs', and sjawhar's
// token reaches nothing, so a tree open at the transfer would never be merged. GitHub does
// answer the old address — a REST call to it redirects, and a GraphQL query resolves the
// repository — so naming the served one is for the log line and for keeping a write off a
// redirect. Serving one more repository of the owner the transfer moved it away from changes
// none of it — that state is what the owner rule this replaced got wrong in production — and
// with no pair configured the same record is minted for and addressed at sjawhar/legion, as it
// always was.
func TestAREADYMergeOfATreeRecordedBeforeItsTransferAddressesTheRepositoryGitHubServes(t *testing.T) {
	for _, tc := range transferStates {
		t.Run(tc.name, func(t *testing.T) {
			merges := 0
			var mu sync.Mutex
			github := newRenameGitHub(t, func(_ *http.Request, body string) string {
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
			github.assertAddressed(t, appauth.Implement, tc.want)
		})
	}
}

// A disarm of the same tree's pull request reads and writes the same repository: readyMerge and
// autoMergeDisable share the one client (graphQL), so both follow the transfer together.
func TestAnAutoMergeDisableOfATreeRecordedBeforeItsTransferAddressesTheRepositoryGitHubServes(t *testing.T) {
	for _, tc := range transferStates {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			disables := 0
			github := newRenameGitHub(t, func(_ *http.Request, body string) string {
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
			github.assertAddressed(t, appauth.Implement, tc.want)
		})
	}
}

// identityTokens is roleTokens with a bot identity of its own for each owner, so a lease shows
// the owner it was minted for.
type identityTokens struct{ roleTokens }

func (t identityTokens) Token(ctx context.Context, role appauth.AppRole, repo ghrepo.Repository) (appauth.Lease, error) {
	lease, err := t.roleTokens.Token(ctx, role, repo)
	owner := repo.Owner()
	lease.Identity = runtime.GitIdentity{Name: "legion-" + string(role) + "[bot] of " + owner, Email: owner + "@users.noreply.github.com"}
	return lease, err
}

// The git identity a tree recorded before its repository's transfer commits as is minted on the
// installation that holds the repository now: the record keeps sjawhar/legion, and under the
// configured pair with cybermonklabs served the lease is cybermonklabs', which is the half the
// old owner cannot supply — the installation covering a transferred repository is the new
// owner's, so without this a launch and a working-copy adoption of such a tree both fail. Both
// of identity's callers, a launch's spec (specs.SpawnSpec) and a working copy's adoption
// (adoptIdentity), hand it the recorded repository and leave the owner to it. With no pair
// configured the same record is minted for sjawhar, as it always was.
func TestTheGitIdentityOfATreeRecordedBeforeAConfiguredRenameIsMintedForTheOwnerGitHubServes(t *testing.T) {
	recorded := ghrepo.MustParse("sjawhar/legion")
	for _, tc := range transferStates {
		t.Run(tc.name, func(t *testing.T) {
			owner := ghrepo.MustParse(tc.want).Owner()
			w := &workflowRuntime{tokens: identityTokens{}, repositories: renamedRepositories(tc.served, tc.renames)}
			want := runtime.GitIdentity{Name: "legion-implement[bot] of " + owner, Email: owner + "@users.noreply.github.com"}

			launch, err := w.identity(context.Background(), claim.RoleImplementer, recorded)
			if err != nil || launch != want {
				t.Fatalf("the identity of a launch on %s = %+v, %v; want %+v", recorded, launch, err, want)
			}
			adopt := adoptIdentity(w.identity, func(context.Context, string) (ghrepo.Repository, string, error) {
				return recorded, "main", nil
			})
			adopted, err := adopt(context.Background(), claim.RoleImplementer, "LEGION-208")
			if err != nil || adopted != want {
				t.Fatalf("the identity of an adoption of %s = %+v, %v; want %+v", recorded, adopted, err, want)
			}
		})
	}
}

// The branch of a tree recorded before its repository's transfer is created on the repository
// GitHub holds it under now: the record keeps sjawhar/legion, and both calls the create costs —
// the read of the base branch's ref and the create of the issue's own — are addressed at
// cybermonklabs/legion on the implement App's token minted for cybermonklabs. The token is the
// half the old name cannot supply, and the address matters here as it does nowhere else: a
// create is a write, and GitHub's redirect of a transferred repository carries no write, so a
// branch created under the name the repository moved away from is created nowhere and every
// role of the tree would push to a branch that does not exist.
func TestTheBranchOfATreeRecordedBeforeItsTransferIsCreatedOnTheRepositoryGitHubServes(t *testing.T) {
	issue := record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Project: "LEGION",
		Tree: "LEGION-208", Title: "Workflow", Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "U"}
	for _, tc := range transferStates {
		t.Run(tc.name, func(t *testing.T) {
			github := newRenameGitHub(t, func(r *http.Request, _ string) string {
				if r.Method == http.MethodGet {
					return `{"object":{"sha":"` + branchBaseCommit + `","type":"commit"}}`
				}
				return `{}`
			})
			// The create reads no record of its own: the issue it is given carries the
			// repository, so this outbox needs no database.
			runner := &outbox{log: quietLogger(), dispatchProject: "LEGION", records: record.NewStore(), tokens: roleTokens{},
				project: "legion", githubAPI: github.url, repositories: renamedRepositories(tc.served, tc.renames), now: time.Now}

			if err := runner.createBranch(context.Background(), issue); err != nil {
				t.Fatalf("create the branch of %s: %v", issue.Key, err)
			}

			github.assertAddressed(t, appauth.Implement, tc.want)
			bearer := "Bearer implement-app-token-for-" + ghrepo.MustParse(tc.want).Owner()
			want := []string{
				"GET /repos/" + tc.want + "/git/ref/heads/main " + bearer,
				"POST /repos/" + tc.want + "/git/refs " + bearer,
			}
			if got := github.requests(); !slices.Equal(got, want) {
				t.Fatalf("GitHub calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}
