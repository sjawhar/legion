package api

import (
	"context"
	"net/http"
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

// newTransferHandoffHarness is newHandoffHarness for a deployment that configures renames and
// serves repositories of owners: the two the server's serving rule reads (Options.GitHubOwners
// and Options.RepositoryRenames), which the harnesses above leave empty.
func newTransferHandoffHarness(t *testing.T, github *handoffGitHub, renames ghrepo.Renames, owners []string) (*harness, *factRecorder) {
	t.Helper()
	h := newHarness(t)
	facts := &factRecorder{}
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
		Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: record.NewStore(),
		Handlers: []intake.Handler{facts}, Dispatch: &statusRecorder{}, PluginRelaunchable: true,
		Tokens: handoffTokens{}, GitHubAPI: github.url,
		GitHubOwners: func() []string { return owners }, RepositoryRenames: renames,
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
// redirect. With no pair configured the same record is read on sjawhar, as it always was.
func TestHandoffCompleteReadyOfATreeRecordedBeforeItsTransferReadsTheRepositoryGitHubServes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		renames ghrepo.Renames
		served  string
	}{
		{name: "the pair configured", renames: transferredLegion, served: "cybermonklabs/legion"},
		{name: "no pair configured", served: "sjawhar/legion"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newHandoffGitHub(t)
			github.set(func(g *handoffGitHub) { g.repo = tc.served })
			owner := ghrepo.MustParse(tc.served).Owner()
			h, facts := newTransferHandoffHarness(t, github, tc.renames, []string{owner})
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
				t.Fatalf("READY of a tree recorded as sjawhar/legion = %d %q; want it published off %s", code, body, tc.served)
			}
			if got := facts.recorded(); len(got) != 1 {
				t.Fatalf("handoff facts = %#v, want the READY", got)
			}
			github.mu.Lock()
			defer github.mu.Unlock()
			if len(github.served) == 0 {
				t.Fatalf("READY read nothing of %s: a record's name is no address once the repository has moved", tc.served)
			}
			for _, bearer := range github.bearers {
				if want := "Bearer implement-token-for-" + owner; bearer != want {
					t.Fatalf("GitHub was read with %q, want %q: the token is minted for the owner whose installation holds the repository", bearer, want)
				}
			}
		})
	}
}
