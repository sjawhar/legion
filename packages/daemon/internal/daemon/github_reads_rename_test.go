package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A record keeps the repository's name for its whole life, and GitHub, after a transfer, holds
// the repository under the other name on the other owner's installation. Every GitHub call the
// daemon makes for a recorded tree therefore reads the name GitHub serves (repositories.serving),
// and the two reads of a pull request are no exception:
//
//   - readPullRequest, which the per-issue read and the merge-state read both go through, would
//     otherwise GET /repos/<recorded>/pulls/<n> with a token minted on the installation the
//     repository has left;
//   - openPullRequestOn, which also names an owner in its `head=` filter, would otherwise ask the
//     old installation for heads of an owner that holds none, find nothing, and have its caller
//     report the pull request missing to the architect.
//
// With no pair configured the same record is read under its own name, as a deployment that
// configures no rename always did.
func TestThePullRequestReadsOfATreeRecordedBeforeItsTransferAddressTheRepositoryGitHubServes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		renames ghrepo.Renames
		served  string
		want    string
	}{
		{name: "the pair configured", renames: transferredCapture, served: "cybermonklabs/widgets", want: "cybermonklabs/widgets"},
		{name: "no pair configured", served: "acme/widgets", want: "acme/widgets"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, read := range []struct {
				name string
				run  func(t *testing.T, w *workflowRuntime, pr record.PullRequest)
			}{
				{name: "the read of a recorded pull request", run: func(t *testing.T, w *workflowRuntime, pr record.PullRequest) {
					if _, pull, err := w.readPullRequest(context.Background(), pr); err != nil || pull.Number != pr.Number {
						t.Fatalf("read the pull request = %+v, %v; want GitHub's answer for #%d", pull, err, pr.Number)
					}
				}},
				{name: "the listing of an issue's branch", run: func(t *testing.T, w *workflowRuntime, pr record.PullRequest) {
					pull, found, err := w.openPullRequestOn(context.Background(), pr.Repo, pr.Branch)
					if err != nil || !found || pull.Number != pr.Number {
						t.Fatalf("list the open pull request = %+v, %v, %v; want #%d found on %s",
							pull, found, err, pr.Number, pr.Branch)
					}
				}},
			} {
				t.Run(read.name, func(t *testing.T) {
					github := newReadRenameGitHub(t, tc.want)
					pool := isolatedOutboxPool(t)
					pr := record.PullRequest{State: record.PullRequestOpen, Issue: "CAPTURE-1", Repo: "acme/widgets",
						Number: 86, Branch: "legion/CAPTURE-1", HeadSHA: "code"}
					seedRenamedCapture(t, pool, pr)
					w := readRuntime(pool, github.url, quietLogger())
					repos := newRepositories("CAPTURE", tc.renames)
					repos.set([]mappedRepository{{Repo: ghrepo.MustParse(tc.served), Default: true}}, nil)
					w.repositories = repos
					// roleTokens names the owner each lease was minted for, which is half of what
					// this case reads: the other half is the repository the path addresses.
					w.tokens = roleTokens{}

					read.run(t, w, pr)

					github.assertAddressed(t, tc.want, pr.Branch)
				})
			}
		})
	}
}

// transferredCapture is the pair a deployment configures over the CAPTURE project's own transfer:
// its records keep acme/widgets, and GitHub holds the repository as cybermonklabs/widgets.
var transferredCapture = ghrepo.MustRenames(ghrepo.MustRename("acme/widgets", "cybermonklabs/widgets"))

// readRenameGitHub answers both reads for one repository and records how each was addressed: the
// `<owner>/<name>` of the path, the `head=` filter of a listing, and the bearer the call carried.
type readRenameGitHub struct {
	url   string
	repo  string
	mu    sync.Mutex
	paths []string
	heads []string
	calls []string
}

func newReadRenameGitHub(t *testing.T, repo string) *readRenameGitHub {
	t.Helper()
	g := &readRenameGitHub{repo: repo}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.paths = append(g.paths, strings.TrimPrefix(r.URL.Path, "/repos/"))
		g.calls = append(g.calls, r.Header.Get("Authorization"))
		if head := r.URL.Query().Get("head"); head != "" {
			g.heads = append(g.heads, head)
		}
		g.mu.Unlock()
		pull := map[string]any{"number": 86, "state": "open", "merged": false, "mergeable_state": "clean",
			"head": map[string]any{"sha": "code", "ref": "legion/CAPTURE-1"},
			"base": map[string]any{"ref": "main", "sha": "base"}}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/repos/") && strings.Contains(r.URL.Path, "/pulls") && r.URL.RawQuery != "" {
			_ = json.NewEncoder(w).Encode([]any{pull})
			return
		}
		_ = json.NewEncoder(w).Encode(pull)
	}))
	t.Cleanup(server.Close)
	g.url = server.URL
	return g
}

// assertAddressed holds every call to the repository GitHub serves: the path names it, the token
// is minted for its owner, and a listing's head filter names that owner too.
func (g *readRenameGitHub) assertAddressed(t *testing.T, repo, branch string) {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.paths) == 0 {
		t.Fatalf("GitHub was asked nothing at all, want a read of %s", repo)
	}
	owner := ghrepo.MustParse(repo).Owner()
	for _, path := range g.paths {
		if named := strings.Join(strings.Split(path, "/")[:2], "/"); named != repo {
			t.Errorf("GitHub was asked about %s, want %s: a record's name is no address once the repository has moved", named, repo)
		}
	}
	for _, bearer := range g.calls {
		if want := "Bearer implement-app-token-for-" + owner; bearer != want {
			t.Errorf("a read carried %q, want %q: the token is minted for the owner whose installation holds the repository", bearer, want)
		}
	}
	for _, head := range g.heads {
		if want := owner + ":" + branch; head != want {
			t.Errorf("a listing filtered on head %q, want %q: the owner of the heads is the one GitHub serves", head, want)
		}
	}
}

// seedRenamedCapture records the issue and its pull request under the name the record was written
// with, which is the name GitHub no longer serves.
func seedRenamedCapture(t *testing.T, pool *pgxpool.Pool, pr record.PullRequest) {
	t.Helper()
	records := record.NewStore()
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		ctx := context.Background()
		if err := records.PutIssue(ctx, tx, record.Issue{Key: pr.Issue, Repo: pr.Repo, DefaultBranch: "main",
			GitHubRepo: pr.Repo, Tree: pr.Issue, Project: "CAPTURE", Title: "root", Phase: phase.Implementing,
			Generation: 1, Status: "in_progress", Rank: "U"}); err != nil {
			return err
		}
		if err := records.PutPullRequest(ctx, tx, pr); err != nil {
			return err
		}
		return records.PutPhase(ctx, tx, record.PhaseRow{Issue: pr.Issue, Role: claim.RoleImplementer, Claim: "implement-claim"})
	}); err != nil {
		t.Fatalf("seed the transferred tree: %v", err)
	}
}
