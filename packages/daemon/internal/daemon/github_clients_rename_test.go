package daemon

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The three GitHub clients a tree recorded before its repository's transfer reaches that no other
// rename test drives: the required-checks pass's read of a recorded pull request
// (required_checks.go's implementClient), the review App's read of a review author's permission
// (review_permission.go's reviewAppClient) and the read a record's repository and default branch
// are filled from (defaultbranch.go's fetch). Each is one of the places the issue's first
// requirement names, and each takes a repository a record names — sjawhar/legion, the name the
// record keeps for its whole life — into a GitHub call and a token mint.
//
// Each is driven in both owner-set states, since the two differ in nothing a GitHub call should
// see: state B serves one more repository of the owner the transfer moved the repository away
// from, which is what production did on 2026-11-06 when it added sjawhar/oh-my-pi to the
// project's mapping, and under the owner rule this replaced that one addition switched the pair
// off and addressed every call at sjawhar. What each case records is the repository path of the
// request and the owner the token was minted for; none of these three calls carries a `head=`.
func TestEachGitHubClientOfATreeRecordedBeforeItsTransferAddressesTheRepositoryGitHubServes(t *testing.T) {
	recorded := ghrepo.MustParse("sjawhar/legion")
	for _, site := range []struct {
		name string
		// app is the App whose installation token the call is made on.
		app appauth.AppRole
		// path is what the call asks for under the repository it addresses, "" for the read of
		// the repository itself.
		path string
		// answer is GitHub's answer, given the `<owner>/<name>` the request addressed.
		answer func(addressed string) string
		// call makes the one call over a runtime whose mapping is the state's, and holds what it
		// answered.
		call func(t *testing.T, repos *repositories, api string)
	}{
		{
			name: "the required-checks pass's read of a recorded pull request",
			app:  appauth.Implement, path: "/pulls/42",
			answer: func(string) string { return `{"number":42,"state":"open","base":{"ref":"main"},"head":{"sha":"head"}}` },
			call: func(t *testing.T, repos *repositories, api string) {
				t.Helper()
				w := &workflowRuntime{tokens: roleTokens{}, repositories: repos, githubAPI: api, log: quietLogger()}
				_, pull, err := w.readPullRequest(context.Background(), record.PullRequest{State: record.PullRequestOpen,
					Issue: "LEGION-208", Repo: recorded.String(), Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head"})
				if err != nil {
					t.Fatalf("read the recorded pull request: %v", err)
				}
				if pull.Number != 42 || pull.Base.Ref != "main" {
					t.Errorf("the pull request read = #%d on %q, want #42 on main", pull.Number, pull.Base.Ref)
				}
			},
		},
		{
			name: "the review App's read of a review author's permission",
			app:  appauth.Review, path: "/collaborators/a-writer/permission",
			answer: func(string) string { return `{"permission":"write","user":{"login":"a-writer"}}` },
			call: func(t *testing.T, repos *repositories, api string) {
				t.Helper()
				w := &workflowRuntime{tokens: roleTokens{}, repositories: repos, githubAPI: api, log: quietLogger(),
					reviewAppLogin: "legion-reviewer[bot]"}
				canWrite, err := w.readPermission(context.Background(), recorded, "a-writer")
				if err != nil {
					t.Fatalf("read a-writer's permission: %v", err)
				}
				if !canWrite {
					t.Error("a-writer can write = false, want true: GitHub gives write access on the repository it holds the record's under now")
				}
			},
		},
		{
			name: "the read a record's repository and default branch are filled from",
			app:  appauth.Implement,
			answer: func(addressed string) string {
				return `{"full_name":"` + addressed + `","default_branch":"main"}`
			},
			call: func(t *testing.T, repos *repositories, api string) {
				t.Helper()
				branches := newDefaultBranches(roleTokens{}, repos, api, defaultBranchTTL, time.Now)
				named, err := branches.Named(context.Background(), recorded)
				if err != nil {
					t.Fatalf("read the default branch of %s: %v", recorded, err)
				}
				if named.DefaultBranch != "main" {
					t.Errorf("the default branch = %q, want main", named.DefaultBranch)
				}
			},
		},
	} {
		t.Run(site.name, func(t *testing.T) {
			for _, tc := range transferStates {
				t.Run(tc.name, func(t *testing.T) {
					github := newRenameGitHub(t, func(r *http.Request, _ string) string {
						addressed := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/"), site.path)
						return site.answer(addressed)
					})

					site.call(t, renamedRepositories(tc.served, tc.renames), github.url)

					github.assertAddressed(t, site.app, tc.want)
					owner := ghrepo.MustParse(tc.want).Owner()
					want := []string{"GET /repos/" + tc.want + site.path + " Bearer " + string(site.app) + "-app-token-for-" + owner}
					if got := github.requests(); !slices.Equal(got, want) {
						t.Fatalf("GitHub calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
					}
					t.Logf("a record naming %s: the request addressed /repos/%s%s, it carries no head=, and its token was minted for %s",
						recorded, tc.want, site.path, owner)
				})
			}
		})
	}
}
