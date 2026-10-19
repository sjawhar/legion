package api

import (
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// TestRealGitHubReadyHoldsTheHeadToRetrosCommits drives READY's check of the commits above the
// approved head (unreviewedChange) through the daemon's own completion route, against GitHub
// itself, as the implement App, over a real Postgres: a merger of LEGION_READY_SCOPE_ISSUE posts
// ready: true while its review round approved LEGION_READY_SCOPE_APPROVED and its pull request
// LEGION_READY_SCOPE_PR of LEGION_READY_SCOPE_REPO heads at LEGION_READY_SCOPE_HEAD. With
// LEGION_READY_SCOPE_REFUSE naming a path, READY must be refused READY_UNREVIEWED_CHANGE naming it;
// unset, READY must not be refused for its change above the approved head. A pull request already
// merged is published unread whatever its compare holds, so the proof also runs the check itself on
// the same heads with the implement App's token, which is what READY reads of an open one. It is
// gated as the credential proof is: CI holds no Legion App keys.
func TestRealGitHubReadyHoldsTheHeadToRetrosCommits(t *testing.T) {
	if os.Getenv(realGitHubProofEnabled) != "1" {
		t.Skip("set LEGION_REAL_GITHUB=1 to run the real GitHub READY scope proof")
	}
	get := func(name string) string {
		value := os.Getenv(name)
		if value == "" {
			t.Fatalf("%s is required", name)
		}
		return value
	}
	repository := ghrepo.MustParse(get("LEGION_READY_SCOPE_REPO"))
	number, err := strconv.Atoi(get("LEGION_READY_SCOPE_PR"))
	if err != nil {
		t.Fatalf("LEGION_READY_SCOPE_PR: %v", err)
	}
	issue, approved, head := get("LEGION_READY_SCOPE_ISSUE"), get("LEGION_READY_SCOPE_APPROVED"), get("LEGION_READY_SCOPE_HEAD")
	refuse := os.Getenv("LEGION_READY_SCOPE_REFUSE")
	tokens := appauth.New(realGitHubApps(t), appauth.Options{})

	h := newHarness(t)
	h.handler = NewServer("127.0.0.1", 0, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, Controller: h.store, Grants: credential.New(nil),
		Pool: h.store.Pool(), Record: record.NewStore(), Handlers: []intake.Handler{&factRecorder{}}, Dispatch: &statusRecorder{},
		Tokens: tokens, GitHubOwners: func() []string { return []string{repository.Owner()} },
	}).Handler
	seedIssueOn(t, h, issue, phase.Merging, repository.String())
	if err := h.store.Tx(h.ctx, func(tx pgx.Tx) error {
		return record.NewStore().PutPullRequest(h.ctx, tx, record.PullRequest{
			State: record.PullRequestOpen, Issue: issue, Repo: repository.String(), Number: number, Branch: "legion/" + issue, HeadSHA: head,
		})
	}); err != nil {
		t.Fatalf("seed the pull request: %v", err)
	}
	seedApproval(t, h, issue, approved)
	merger := newLiveClaim(t, h, issue, claim.RoleMerger)

	started := time.Now()
	recorder := h.complete(HandoffCompleteRequest{GrantID: merger.grant(t), Summary: "READY scope proof", Ready: true, Commit: head})
	var failure Failure
	var answer HandoffCompleteResponse
	if recorder.Code == http.StatusOK {
		decodeInto(t, recorder, &answer)
	} else {
		decodeInto(t, recorder, &failure)
	}
	t.Logf("READY of %s#%d at %s, approved at %s: %d %s %q note %q (%s)", repository, number, head, approved,
		recorder.Code, failure.Code, failure.Error, answer.Note, time.Since(started).Round(time.Millisecond))
	// READY's refusals that only a head past its scope check can get: the handoffs, the conflict,
	// the by-hand file list and the required checks. Any other refusal ends READY before the check,
	// or is the check's own, so it proves nothing about the head's change above the approved one.
	afterScope := map[string]bool{"READY_HEAD_CARRIES_HANDOFFS": true, "READY_HEAD_CONFLICTS": true, "READY_FILES_TRUNCATED": true, "READY_CHECKS_NOT_GREEN": true}
	switch {
	case refuse != "":
		if failure.Code != "READY_UNREVIEWED_CHANGE" || !strings.Contains(failure.Error, refuse) {
			t.Fatalf("READY = %d %s %q; want READY_UNREVIEWED_CHANGE naming %s", recorder.Code, failure.Code, failure.Error, refuse)
		}
	case recorder.Code != http.StatusOK && !afterScope[failure.Code]:
		t.Fatalf("READY = %d %s %q; want it accepted, or refused only by a check after its scope check", recorder.Code, failure.Code, failure.Error)
	}

	lease, err := tokens.Token(h.ctx, appauth.Implement, repository.Owner())
	if err != nil {
		t.Fatalf("mint the implement App's lease: %v", err)
	}
	started = time.Now()
	refused := unreviewedChange(h.ctx, githubrest.Client{Token: lease.Token, API: githubrest.RepositoryAPI("", repository)}, issue, number, approved, head)
	t.Logf("unreviewedChange(%s...%s) as the implement App = %+v (%s)", approved, head, refused, time.Since(started).Round(time.Millisecond))
	if refuse != "" {
		if refused == nil || refused.code != "READY_UNREVIEWED_CHANGE" || !strings.Contains(refused.message, refuse) {
			t.Fatalf("unreviewedChange = %+v; want READY_UNREVIEWED_CHANGE naming %s", refused, refuse)
		}
	} else if refused != nil {
		t.Fatalf("unreviewedChange = %+v; want nil", refused)
	}
}
