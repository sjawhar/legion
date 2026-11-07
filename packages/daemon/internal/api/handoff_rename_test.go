package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// transferredLegion is the pair a deployment configures over sjawhar/legion's transfer to
// cybermonklabs: the records of a tree opened before it keep sjawhar/legion, while GitHub holds
// the repository under cybermonklabs/legion, on cybermonklabs' installation.
var transferredLegion = ghrepo.MustRenames(ghrepo.MustRename("sjawhar/legion", "cybermonklabs/legion"))

// transferStates are the states a tree recorded before its repository's transfer is driven in:
// the configured pair with the project serving the repository GitHub holds it under, that same
// pair with one more repository of the owner the transfer moved it away from served beside it,
// and no pair at all. The second is the state of the production failure of 2026-11-06, where
// adding sjawhar/oh-my-pi to the project's mapping made sjawhar an owner of the project and the
// owner rule this replaced switched the live pair off; it must read exactly what the first does.
// The third is the control: with no pair configured the record's own repository is read.
var transferStates = []struct {
	name    string
	renames ghrepo.Renames
	served  []string
	// serving is the repository GitHub holds the record's under, which every call addresses and
	// whose owner every token is minted for.
	serving string
}{
	{name: "state A: the pair configured", renames: transferredLegion,
		served: []string{"cybermonklabs/legion"}, serving: "cybermonklabs/legion"},
	{name: "state B: the pair configured and the old owner's other repository served", renames: transferredLegion,
		served: []string{"cybermonklabs/legion", "sjawhar/oh-my-pi"}, serving: "cybermonklabs/legion"},
	{name: "no pair configured", served: []string{"sjawhar/legion"}, serving: "sjawhar/legion"},
}

// newTransferHandoffHarness is newHandoffHarness for a deployment that configures renames and
// serves repositories: the serving rule the daemon hands the server (Options.Serving), built
// here from what the daemon's own is built from — the repositories it serves and the configured
// pairs, read one repository at a time (ghrepo.Serves, which is what internal/daemon's
// repositories.serving reads) — where the harnesses above hand none and a recorded repository
// is served under its own name. GitHub is read at api, and the harness's Dispatch holds the
// handoff records a completion names (writeHandoff).
func newTransferHandoffHarness(t *testing.T, api string, renames ghrepo.Renames, served []string) (*harness, *factRecorder) {
	t.Helper()
	h := newHarness(t)
	facts := &factRecorder{}
	h.handoffs = &statusRecorder{}
	repositories := make([]ghrepo.Repository, 0, len(served))
	for _, repo := range served {
		repositories = append(repositories, ghrepo.MustParse(repo))
	}
	serving := func(recorded ghrepo.Repository) ghrepo.Repository {
		return renames.Serving(recorded, func(repo ghrepo.Repository) bool { return ghrepo.Serves(repositories, repo) })
	}
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Steps:      config.DesignGateRootIssues.Steps(),
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
		Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: record.NewStore(),
		Handlers: []intake.Handler{facts}, Dispatch: h.handoffs, PluginRelaunchable: true,
		Tokens: handoffTokens{}, GitHubAPI: api,
		GitHubRepositories: func() []ghrepo.Repository { return repositories }, RepositoryRenames: renames,
		Serving:    serving,
		MergeRules: func(string) config.MergeRules { return config.MergeRules{} },
	}).Handler
	return h, facts
}

// READY's checks of a tree recorded before its repository's transfer are read on the installation
// that holds the repository now, and addressed at the name GitHub serves it under: the record
// keeps sjawhar/legion, and with the pair configured and cybermonklabs served every read is made
// with the implement App's token for cybermonklabs. That token is the half the old owner cannot
// supply — after a transfer the installation covering the repository is the new owner's, so a
// mint on sjawhar answers GITHUB_TOKEN_FAILED once that installation is gone, and READY is never
// published while the tree waits in merging. GitHub does answer the old address, with a 301 to
// the repository's id, so the served name is for the log line and for keeping a write off a
// redirect. Serving one more repository of the owner the transfer moved it away from changes
// none of it, and with no pair configured the same record is read on sjawhar, as it always was.
func TestHandoffCompleteReadyOfATreeRecordedBeforeItsTransferReadsTheRepositoryGitHubServes(t *testing.T) {
	for _, tc := range transferStates {
		t.Run(tc.name, func(t *testing.T) {
			github := newHandoffGitHub(t)
			github.set(func(g *handoffGitHub) { g.repo = tc.serving })
			owner := ghrepo.MustParse(tc.serving).Owner()
			h, facts := newTransferHandoffHarness(t, github.url, tc.renames, tc.served)
			if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
				records := record.NewStore()
				if err := records.PutIssue(context.Background(), tx, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main",
					GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: testProject, Title: "LEGION-208", Phase: phase.Merging,
					Generation: 1, Status: "in_progress"}); err != nil {
					return err
				}
				return records.PutPullRequest(context.Background(), tx, record.PullRequest{
					State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: headSHA,
				})
			}); err != nil {
				t.Fatal(err)
			}
			seedApproval(t, h, "LEGION-208", approvedSHA)
			merger := newLiveClaim(t, h, "LEGION-208", claim.RoleMerger)

			if code, body := ready(t, h, merger); code != http.StatusOK || body != "" {
				t.Fatalf("READY of a tree recorded as sjawhar/legion = %d %q; want it published off %s", code, body, tc.serving)
			}
			if got := facts.recorded(); len(got) != 1 {
				t.Fatalf("handoff facts = %#v, want the READY", got)
			}
			github.mu.Lock()
			defer github.mu.Unlock()
			if len(github.served) == 0 {
				t.Fatalf("READY read nothing of %s: a record's name is no address once the repository has moved", tc.serving)
			}
			for _, bearer := range github.bearers {
				if want := "Bearer implement-token-for-" + owner; bearer != want {
					t.Fatalf("GitHub was read with %q, want %q: the token is minted for the owner whose installation holds the repository", bearer, want)
				}
			}
		})
	}
}

// transferGitHub is GitHub over sjawhar/legion's transfer to cybermonklabs, as the implementer's
// completion reads it. The repository answers under either name, since a REST call to the one it
// moved away from redirects to it; what no redirect translates is the `head=<owner>:<branch>`
// filter of the pull-request list, whose owner GitHub reads literally. So a list asked for
// head=sjawhar:legion/LEGION-208 names an owner that heads no pull request of the repository and
// is answered the empty list, while the same list asked for head=cybermonklabs: holds #42. Every
// request is recorded as `<METHOD> <path> head=<head> <bearer>`: the repository the request
// addressed, the filter it carried and the owner its token was minted for.
type transferGitHub struct {
	url string
	// serving is the owner GitHub holds the repository under now, the one a head= filter must
	// name for the list to hold the pull request.
	serving string
	mu      sync.Mutex
	made    []string
}

func newTransferGitHub(t *testing.T, serving string) *transferGitHub {
	t.Helper()
	g := &transferGitHub{serving: serving}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		head := r.URL.Query().Get("head")
		g.mu.Lock()
		g.made = append(g.made, r.Method+" "+r.URL.Path+" head="+head+" "+r.Header.Get("Authorization"))
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if head != g.serving+":legion/LEGION-208" {
			_, _ = io.WriteString(w, "[]")
			return
		}
		_, _ = fmt.Fprintf(w, `[{"number":42,"state":"open","html_url":"https://github.com/%s/legion/pull/42","body":"Dispatch: LEGION-208",`+
			`"head":{"ref":"legion/LEGION-208","sha":%q},"base":{"ref":"main"}}]`, g.serving, headSHA)
	}))
	t.Cleanup(server.Close)
	g.url = server.URL
	return g
}

// requests is every call GitHub received so far.
func (g *transferGitHub) requests() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.made)
}

// An implementer completing implementing on a tree recorded before its repository's transfer is
// the handoff the production refusal came from. The route reads the open pull request on the
// issue's branch (implementationPullRequest, through githubrest.OpenPullRequestOn), and that read
// is a `head=<owner>:<branch>` list whose owner GitHub translates across no transfer: the path
// redirects, the filter does not. So the owner the filter names has to be the owner GitHub holds
// the repository under now, which is what the resolution answers from the record's own name.
//
// In both owner-set states the route answers 200, the request addresses the repository the
// project serves and the filter names its owner. Under the owner rule this replaces, state B
// asked head=sjawhar:, GitHub answered the empty list, and the route refused the completion 409
// HANDOFF_NO_PULL_REQUEST: that is what held 47 trees for 42 minutes on 2026-11-06. The pull
// request the read finds is recorded under the name the record keeps, which no resolution moves.
func TestHandoffCompleteOfAnImplementerOnATransferredRepositoryListsTheHeadOfTheOwnerGitHubServes(t *testing.T) {
	for _, tc := range transferStates {
		t.Run(tc.name, func(t *testing.T) {
			owner := ghrepo.MustParse(tc.serving).Owner()
			github := newTransferGitHub(t, owner)
			h, facts := newTransferHandoffHarness(t, github.url, tc.renames, tc.served)
			seedIssueOn(t, h, "LEGION-208", phase.Implementing, "sjawhar/legion")
			implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
			written := implementer.writeHandoff("implement", 2)

			recorder := h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "implemented",
				Commit: headSHA, HandoffID: written, HandoffVersion: 2})

			if recorder.Code != http.StatusOK {
				t.Fatalf("the implementer's completion of a tree recorded as sjawhar/legion = %d %s; want 200 off %s",
					recorder.Code, recorder.Body, tc.serving)
			}
			want := []string{"GET /repos/" + tc.serving + "/pulls head=" + owner + ":legion/LEGION-208 Bearer implement-token-for-" + owner}
			if got := github.requests(); !slices.Equal(got, want) {
				t.Fatalf("GitHub calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
			opened := 0
			for _, fact := range facts.recorded() {
				read, ok := fact.(intake.PullRequestOpened)
				if !ok {
					continue
				}
				opened++
				if read.Repo != "sjawhar/legion" || read.Number != 42 {
					t.Errorf("the pull request read was recorded on %s#%d, want sjawhar/legion#42: a record keeps the name it was written under", read.Repo, read.Number)
				}
			}
			if opened != 1 {
				t.Fatalf("the completion recorded %d pull requests read from GitHub, want the one open on the branch", opened)
			}
			t.Logf("a record naming sjawhar/legion: the request addressed /repos/%s/pulls, it carried head=%s:legion/LEGION-208, its token was minted for %s, and the route answered %d",
				tc.serving, owner, owner, recorder.Code)
		})
	}
}
