package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Each mutating architecture operation must be explicitly registered. A 404 here silently turns
// an agent action into a no-op at its caller, while a 400 identifies that the API received it and
// rejected the missing capability before it can mutate anything.
func TestArchitectFactRoutesRejectMissingGrantID(t *testing.T) {
	h := newHarness(t)
	for _, route := range []string{
		"/legion/v1/gates/register",
		"/legion/v1/waves/release",
		"/legion/v1/phase/backward",
		"/legion/v1/phase/retry",
		"/legion/v1/signoff",
		"/legion/v1/children/park",
		"/legion/v1/children/rerun",
	} {
		t.Run(route, func(t *testing.T) {
			recorder := h.request(http.MethodPost, route, map[string]any{}, nil)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body %s", recorder.Code, recorder.Body)
			}
		})
	}
}

type factRecorder struct {
	mu      sync.Mutex
	facts   []intake.Fact
	refusal *intake.Refusal
}

func (r *factRecorder) Apply(_ context.Context, _ pgx.Tx, fact intake.Fact) (intake.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.facts = append(r.facts, fact)
	return intake.Result{Refusal: r.refusal}, nil
}

func (r *factRecorder) recorded() []intake.Fact {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]intake.Fact(nil), r.facts...)
}

type statusWrite struct {
	issue  string
	status string
}

type statusRecorder struct {
	err    error
	writes []statusWrite
	// documents maps each Dispatch document id this fake knows to the issue carrying it.
	documents map[string]string
	// approved maps a document id to the version Dispatch shows approved, its latest; absent means
	// no approval.
	approved map[string]int
	// approvals maps a document id to the whole approval Dispatch shows for it, read ahead of
	// approved.
	approvals map[string]dispatch.Approval
	issues    map[string]dispatch.Issue
	// readFails names the issues whose detail read fails with the error given, as Dispatch's did
	// for some children in a deployment; the listing still carries them unless listFails is set.
	readFails map[string]error
	listFails error
	// reads is every issue and document read, in order: `issue:<key>` and `approval:<id>`.
	reads []string
}

// ListIssues answers Dispatch's project listing: every issue the fake knows, in key order, as the
// listing summarizes it.
func (r *statusRecorder) ListIssues(context.Context, string, []string) ([]dispatch.IssueSummary, error) {
	r.reads = append(r.reads, "listing")
	if r.listFails != nil {
		return nil, r.listFails
	}
	keys := make([]string, 0, len(r.issues))
	for key := range r.issues {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	summaries := make([]dispatch.IssueSummary, 0, len(keys))
	for _, key := range keys {
		issue := r.issues[key]
		summaries = append(summaries, dispatch.IssueSummary{Key: issue.Key, Title: issue.Title, Status: issue.Status, Parent: issue.Parent, Rank: issue.Rank,
			HandedOver: issue.HandedOver, LastSeq: issue.LastSeq, ResolvedRepo: issue.ResolvedRepo, BlockedBy: issue.BlockedBy,
			ClaimedBy: issue.Claim.Holder(), ClaimUnread: issue.ClaimUnread})
	}
	return summaries, nil
}

func (r *statusRecorder) GetIssue(_ context.Context, key string) (dispatch.Issue, error) {
	r.reads = append(r.reads, "issue:"+key)
	if err, fails := r.readFails[key]; fails {
		return dispatch.Issue{}, err
	}
	issue, ok := r.issues[key]
	if !ok {
		return dispatch.Issue{}, &dispatch.Error{Status: http.StatusNotFound, Code: "NOT_FOUND", Message: "issue not found"}
	}
	return issue, nil
}

// SetStatus answers as Dispatch's PATCH does: the issue as the write left it, its sequence moved on
// by the one event a change of status makes, and as it stood when the status was already that one,
// with no event. The fake keeps the written issue, so a later read sees the write.
func (r *statusRecorder) SetStatus(_ context.Context, issue, status string) (dispatch.Issue, error) {
	if r.err != nil {
		return dispatch.Issue{}, r.err
	}
	r.writes = append(r.writes, statusWrite{issue: issue, status: status})
	written, ok := r.issues[issue]
	if !ok {
		written = dispatch.Issue{Key: issue}
	}
	if written.Status != status {
		written.Status = status
		written.LastSeq++
	}
	if r.issues == nil {
		r.issues = map[string]dispatch.Issue{}
	}
	r.issues[issue] = written
	return written, nil
}

func (r *statusRecorder) HandOver(context.Context, dispatch.Issue, string) error {
	return errors.New("the architect routes hand nothing over")
}

func (r *statusRecorder) PostMessage(context.Context, string, string) error { return nil }
func (r *statusRecorder) MessageBodiesSince(context.Context, string, time.Time) ([]string, error) {
	return nil, nil
}

func (r *statusRecorder) Approval(_ context.Context, artifactID string) (dispatch.Approval, error) {
	r.reads = append(r.reads, "approval:"+artifactID)
	issue, ok := r.documents[artifactID]
	if !ok {
		return dispatch.Approval{}, &dispatch.Error{Status: http.StatusNotFound, Code: "NOT_FOUND", Message: "artifact not found"}
	}
	if approval, shown := r.approvals[artifactID]; shown {
		approval.IssueKey = issue
		return approval, nil
	}
	if version, approved := r.approved[artifactID]; approved {
		return dispatch.Approval{IssueKey: issue, State: "approved", LatestVersion: version, Version: &version}, nil
	}
	return dispatch.Approval{IssueKey: issue, State: "none", LatestVersion: 1}, nil
}

func (r *statusRecorder) BuildCommit(context.Context) (string, error) { return "", nil }

func (r *statusRecorder) ListRepoProjects(context.Context) ([]dispatch.RepoProject, error) {
	return nil, nil
}

var _ dispatch.Client = (*statusRecorder)(nil)

func newArchitectHarness(t *testing.T, grants *credential.Grants, refusal *intake.Refusal) (*harness, *factRecorder, *statusRecorder) {
	t.Helper()
	h := newHarness(t)
	if grants == nil {
		grants = credential.New(nil)
	}
	// Dispatch shows the tree root LEGION-208 at event sequence 7, which register_gate reads before
	// the approval; a test that reads other issues replaces the map.
	facts, statuses := &factRecorder{refusal: refusal}, &statusRecorder{issues: map[string]dispatch.Issue{"LEGION-208": {Key: "LEGION-208", LastSeq: 7}}}
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor:    h.supervisor,
		BootTokens:    h.tokens,
		Project:       testProject,
		OperatorToken: testOperatorToken,
		Controller:    h.store,
		Grants:        grants,
		Pool:          h.store.Pool(),
		Record:        record.NewStore(),
		Handlers:      []intake.Handler{facts},
		Dispatch:      statuses,
	}).Handler
	return h, facts, statuses
}

func seedTree(t *testing.T, h *harness, root string, children ...string) {
	t.Helper()
	records := record.NewStore()
	err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		if err := records.PutIssue(context.Background(), tx, record.Issue{
			Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: testProject, Title: root, Phase: phase.Planning, Status: "in_progress",
		}); err != nil {
			return err
		}
		for _, child := range children {
			parent := root
			if err := records.PutIssue(context.Background(), tx, record.Issue{
				Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: testProject, Title: child, Parent: &parent,
				Phase: phase.Planning, Status: "in_progress",
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed tree: %v", err)
	}
}

type liveClaim struct {
	h       *harness
	token   claim.Token
	session string
	secret  string
	tree    string
	issue   string
}

func newLiveClaim(t *testing.T, h *harness, issue string, role claim.Role) liveClaim {
	t.Helper()
	return newLiveClaimIn(t, h, issue, issue, role)
}

// newLiveClaimIn is newLiveClaim for an issue of tree: a sub-architect or a worker on a child.
func newLiveClaimIn(t *testing.T, h *harness, tree, issue string, role claim.Role) liveClaim {
	t.Helper()
	token, boot := h.launchIn(tree, issue, role)
	session := "ses_" + string(role) + "_" + issue
	registration := h.registered(boot, session)
	c := liveClaim{h: h, token: token, session: session, secret: registration.Secret, tree: tree, issue: issue}
	// A worker reports a completion inside the turn of the task it was given, and the daemon
	// attributes the completion to that delivery, so a live claim in these tests holds one.
	working(t, h, token, issue)
	return c
}

// working hands the claim a task of the run the issue is on and starts its turn, which is what a
// pane looks like when it calls handoff complete. A test that wants a worker still holding an
// older run's task delivers that one itself.
func working(t *testing.T, h *harness, token claim.Token, issue string) {
	t.Helper()
	machine, ok := h.supervisor.Machine(token)
	if !ok {
		t.Fatalf("no machine for %s", token)
	}
	generation := issueGeneration(t, h, issue)
	if err := machine.Handle(context.Background(), supervise.RequestReady{Claim: token, Generation: machine.Claim().Generation, Session: machine.Claim().Session}); err != nil {
		t.Fatalf("ready %s: %v", token, err)
	}
	id := "outbox:" + string(token)
	if err := machine.Handle(context.Background(), supervise.RequestDeliver{Claim: token, Task: "the task", ID: id, Generation: generation}); err != nil {
		t.Fatalf("deliver to %s: %v", token, err)
	}
	if err := machine.Handle(context.Background(), supervise.StreamTurnStart{Claim: token, DeliveryID: id}); err != nil {
		t.Fatalf("start the turn on %s: %v", token, err)
	}
}

// issueGeneration is the run the issue is recorded on, which is the run a task delivered now
// belongs to. An issue no workflow records — an operator's own claim — has none, and its task
// carries the first run, which is what the daemon's own first start would deliver.
func issueGeneration(t *testing.T, h *harness, issue string) uint64 {
	t.Helper()
	var generation uint64
	err := h.store.Pool().QueryRow(context.Background(),
		"select generation from issues where key = $1", issue).Scan(&generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return 1
	}
	if err != nil {
		t.Fatalf("read the generation of %s: %v", issue, err)
	}
	return generation
}

// replaceRegistration registers the claim's session again, which issues a new secret and so revokes
// every grant the previous registration minted.
func (c *liveClaim) replaceRegistration(t *testing.T) {
	t.Helper()
	c.secret = c.h.registered(c.h.bootToken(c.token), c.session).Secret
}

func (c liveClaim) grant(t *testing.T) string {
	t.Helper()
	recorder := c.h.request(http.MethodPost, "/legion/v1/grants", GrantRequest{
		SessionID: c.session, Secret: c.secret, Tree: c.tree, Issue: c.issue,
	}, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("mint grant = %d: %s", recorder.Code, recorder.Body)
	}
	var response GrantResponse
	decodeInto(t, recorder, &response)
	return response.GrantID
}

func assertFailure(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d; body %s", recorder.Code, status, recorder.Body)
	}
	var failure Failure
	decodeInto(t, recorder, &failure)
	if failure.Code != code {
		t.Fatalf("code = %q, want %q", failure.Code, code)
	}
}

func assertProcessedEvent(t *testing.T, h *harness, eventID string) {
	t.Helper()
	var count int
	err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			"select count(*) from processed_events where source = 'api' and event_id = $1", eventID,
		).Scan(&count)
	})
	if err != nil {
		t.Fatalf("read processed event: %v", err)
	}
	if count != 1 {
		t.Fatalf("processed event %q count = %d, want 1", eventID, count)
	}
}

func TestArchitectAndPhaseRoutesApplyTheRequiredFacts(t *testing.T) {
	h, facts, statuses := newArchitectHarness(t, nil, nil)
	statuses.documents = map[string]string{"d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093": "LEGION-208"}
	statuses.approvals = map[string]dispatch.Approval{"d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093": {State: "none", LatestVersion: 7}}
	seedTree(t, h, "LEGION-208", "LEGION-209")
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)

	gateGrant := architect.grant(t)
	gate := h.request(http.MethodPost, "/legion/v1/gates/register", map[string]any{
		"grantId": gateGrant, "issue": "LEGION-208",
		"artifactId": "D2F1C6B4-8E07-4A53-9C1D-6B8F2E5A7093", "version": 7,
	}, nil)
	if gate.Code != http.StatusOK {
		t.Fatalf("gate register = %d: %s", gate.Code, gate.Body)
	}
	// The id names the read as well as the version: Dispatch shows LEGION-208 at sequence 7.
	assertProcessedEvent(t, h, "gate:LEGION-208:0:d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093:7:7")

	retryGrant := architect.grant(t)
	retry := h.request(http.MethodPost, "/legion/v1/phase/retry", map[string]any{
		"grantId": retryGrant, "issue": "LEGION-209", "decision": "retry",
	}, nil)
	if retry.Code != http.StatusOK {
		t.Fatalf("phase retry = %d: %s", retry.Code, retry.Body)
	}

	signoffGrant := architect.grant(t)
	signoff := h.request(http.MethodPost, "/legion/v1/signoff", map[string]any{
		"grantId": signoffGrant, "issue": "LEGION-208",
	}, nil)
	if signoff.Code != http.StatusOK {
		t.Fatalf("signoff = %d: %s", signoff.Code, signoff.Body)
	}

	worker := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	backwardGrant := worker.grant(t)
	backward := h.request(http.MethodPost, "/legion/v1/phase/backward", map[string]any{
		"grantId": backwardGrant, "to": "implementing", "reason": "review requested changes",
	}, nil)
	if backward.Code != http.StatusOK {
		t.Fatalf("phase backward = %d: %s", backward.Code, backward.Body)
	}

	// One command's grant serves each of its requests, and each is its own fact.
	againGrant := worker.grant(t)
	for _, to := range []string{"implementing", "planning"} {
		if again := h.request(http.MethodPost, "/legion/v1/phase/backward", map[string]any{
			"grantId": againGrant, "to": to, "reason": "one command, two moves",
		}, nil); again.Code != http.StatusOK {
			t.Fatalf("phase backward to %s on a reused grant = %d: %s", to, again.Code, again.Body)
		}
	}

	got := facts.recorded()
	if len(got) != 6 {
		t.Fatalf("facts = %#v, want six", got)
	}
	if fact, ok := got[0].(intake.GateRegistered); !ok || fact.Issue != "LEGION-208" ||
		fact.ArtifactID != "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093" || fact.Version != 7 {
		t.Fatalf("gate fact = %#v", got[0])
	}
	if fact, ok := got[1].(intake.RetryOrEscalate); !ok || fact.Issue != "LEGION-209" ||
		fact.Decision != intake.RetryDecision {
		t.Fatalf("retry fact = %#v", got[1])
	}
	if fact, ok := got[2].(intake.SignOff); !ok || fact.Issue != "LEGION-208" {
		t.Fatalf("signoff fact = %#v", got[2])
	}
	if fact, ok := got[3].(intake.BackwardMove); !ok || fact.Issue != "LEGION-208" ||
		fact.Requester != claim.RoleImplementer || fact.To != phase.Implementing {
		t.Fatalf("backward fact = %#v", got[3])
	}
	for i, to := range []phase.Phase{phase.Implementing, phase.Planning} {
		if fact, ok := got[4+i].(intake.BackwardMove); !ok || fact.To != to || fact.Reason != "one command, two moves" {
			t.Fatalf("backward fact %d on the reused grant = %#v, want the move to %s", 4+i, got[4+i], to)
		}
	}
}

func TestArchitectRoutesRefuseWrongRoleAndForeignTree(t *testing.T) {
	h, facts, _ := newArchitectHarness(t, nil, nil)
	seedTree(t, h, "LEGION-208", "LEGION-209")
	worker := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/gates/register", map[string]any{
		"grantId": worker.grant(t), "issue": "LEGION-208",
		"artifactId": "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093", "version": 1,
	}, nil), http.StatusForbidden, "ARCHITECT_REQUIRED")

	foreign := newLiveClaim(t, h, "LEGION-999", claim.RoleArchitect)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/signoff", map[string]any{
		"grantId": foreign.grant(t), "issue": "LEGION-209",
	}, nil), http.StatusForbidden, "ISSUE_OUTSIDE_TREE")

	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/phase/backward", map[string]any{
		"grantId": architect.grant(t), "to": "planning", "reason": "architect names no issue",
	}, nil), http.StatusBadRequest, "MISSING_FIELD")
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/phase/backward", map[string]any{
		"grantId": worker.grant(t), "to": "planning", "reason": "a worker names another issue", "issue": "LEGION-209",
	}, nil), http.StatusBadRequest, "ISSUE_NOT_GRANTS")
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("refused calls applied facts %#v", got)
	}
}

// sign_off and retry_or_escalate are the owning architect's: its own issue or one under it. A
// sub-architect holds a grant for the whole tree and can name any issue of it, but signing off the
// root ends the tree, and a sibling is another architect's; both are refused, and nothing applies.
// The root's architect owns every issue of its tree, and a sub-architect its own.
func TestSignOffAndRetryAreTheOwningArchitects(t *testing.T) {
	h, facts, _ := newArchitectHarness(t, nil, nil)
	seedTree(t, h, "LEGION-208", "LEGION-209", "LEGION-210")
	root := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	subArchitect := newLiveClaimIn(t, h, "LEGION-208", "LEGION-209", claim.RoleArchitect)
	for n, route := range []struct {
		path, op string
		extra    map[string]any
	}{{"/legion/v1/signoff", "sign_off", nil}, {"/legion/v1/phase/retry", "retry_or_escalate", map[string]any{"decision": "retry"}}} {
		body := func(grant, issue string) map[string]any {
			request := map[string]any{"grantId": grant, "issue": issue}
			for key, value := range route.extra {
				request[key] = value
			}
			return request
		}
		for _, issue := range []string{"LEGION-208", "LEGION-210"} {
			refusal := h.request(http.MethodPost, route.path, body(subArchitect.grant(t), issue), nil)
			if want := route.op + " of " + issue; !strings.Contains(refusal.Body.String(), want) {
				t.Fatalf("%s refusal %s does not name %q", route.path, refusal.Body, want)
			}
			assertFailure(t, refusal, http.StatusForbidden, "ISSUE_NOT_OWNED")
		}
		if got := len(facts.recorded()); got != 3*n {
			t.Fatalf("%s: refused calls applied facts %#v", route.path, facts.recorded()[3*n:])
		}
		for _, allowed := range []struct {
			grant liveClaim
			issue string
		}{{subArchitect, "LEGION-209"}, {root, "LEGION-208"}, {root, "LEGION-210"}} {
			if got := h.request(http.MethodPost, route.path, body(allowed.grant.grant(t), allowed.issue), nil); got.Code != http.StatusOK {
				t.Fatalf("%s by the architect of %s on %s = %d: %s", route.path, allowed.grant.issue, allowed.issue, got.Code, got.Body)
			}
		}
		if got := len(facts.recorded()); got != 3*(n+1) {
			t.Fatalf("%s: %d facts applied, want one per owning architect's call", route.path, got-3*n)
		}
	}
}

// The architect's send-back from accepting takes the backward route with the issue named: its
// grant serves the whole tree, so the request says which issue, and the fact carries that issue
// with the architect as its requester. Ownership is sign_off's: a sub-architect moves its own
// issue, not the root or a sibling. A phase worker's grant keeps its own issue whether or not the
// request names it.
func TestTheArchitectsBackwardMoveNamesItsIssue(t *testing.T) {
	h, facts, _ := newArchitectHarness(t, nil, nil)
	seedTree(t, h, "LEGION-208", "LEGION-209", "LEGION-210")
	root := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	subArchitect := newLiveClaimIn(t, h, "LEGION-208", "LEGION-209", claim.RoleArchitect)
	body := func(grant, issue string) map[string]any {
		return map[string]any{"grantId": grant, "to": "implementing", "reason": "the evidence does not cover the migration", "issue": issue}
	}
	for _, issue := range []string{"LEGION-208", "LEGION-210"} {
		refusal := h.request(http.MethodPost, "/legion/v1/phase/backward", body(subArchitect.grant(t), issue), nil)
		if want := "request_backward_move of " + issue; !strings.Contains(refusal.Body.String(), want) {
			t.Fatalf("refusal %s does not name %q", refusal.Body, want)
		}
		assertFailure(t, refusal, http.StatusForbidden, "ISSUE_NOT_OWNED")
	}
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/phase/backward", body(root.grant(t), "not-a-key"), nil), http.StatusBadRequest, "INVALID_ISSUE")
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("refused calls applied facts %#v", got)
	}
	for _, allowed := range []struct {
		grant liveClaim
		issue string
	}{{subArchitect, "LEGION-209"}, {root, "LEGION-208"}, {root, "LEGION-210"}} {
		if got := h.request(http.MethodPost, "/legion/v1/phase/backward", body(allowed.grant.grant(t), allowed.issue), nil); got.Code != http.StatusOK {
			t.Fatalf("backward move by the architect of %s on %s = %d: %s", allowed.grant.issue, allowed.issue, got.Code, got.Body)
		}
	}
	worker := newLiveClaim(t, h, "LEGION-210", claim.RoleTester)
	if got := h.request(http.MethodPost, "/legion/v1/phase/backward", body(worker.grant(t), "LEGION-210"), nil); got.Code != http.StatusOK {
		t.Fatalf("backward move by the tester naming its own issue = %d: %s", got.Code, got.Body)
	}
	got := facts.recorded()
	if len(got) != 4 {
		t.Fatalf("facts = %#v, want four", got)
	}
	for i, want := range []struct {
		issue     string
		requester claim.Role
	}{{"LEGION-209", claim.RoleArchitect}, {"LEGION-208", claim.RoleArchitect}, {"LEGION-210", claim.RoleArchitect}, {"LEGION-210", claim.RoleTester}} {
		fact, ok := got[i].(intake.BackwardMove)
		if !ok || fact.Issue != want.issue || fact.Requester != want.requester || fact.To != phase.Implementing || fact.Reason != "the evidence does not cover the migration" {
			t.Fatalf("backward fact %d = %#v, want the %s's move of %s to implementing with its reason", i, got[i], want.requester, want.issue)
		}
	}
}

// A child whose parent chain leaves the tree before reaching an architect's issue - no parent, or a
// parent not recorded, as one human re-parent in Dispatch leaves it - is the tree root's, as the
// notice router decides (owningArchitect): the root architect the reviewer's phase-finished notice
// reaches may accept it and retry it, and a sub-architect still may not.
func TestABrokenParentChainIsTheTreeRootArchitects(t *testing.T) {
	h, facts, _ := newArchitectHarness(t, nil, nil)
	seedTree(t, h, "LEGION-208", "LEGION-209")
	unrecorded := "LEGION-999"
	records := record.NewStore()
	if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		for _, child := range []record.Issue{
			{Key: "LEGION-211", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208", Project: testProject, Title: "no parent", Phase: phase.Accepting, Status: "needs_review"},
			{Key: "LEGION-212", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208", Project: testProject, Title: "unrecorded parent", Parent: &unrecorded, Phase: phase.Accepting, Status: "needs_review"},
		} {
			if err := records.PutIssue(context.Background(), tx, child); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	root := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	subArchitect := newLiveClaimIn(t, h, "LEGION-208", "LEGION-209", claim.RoleArchitect)
	for _, issue := range []string{"LEGION-211", "LEGION-212"} {
		for _, route := range []struct {
			path  string
			extra map[string]any
		}{{"/legion/v1/signoff", nil}, {"/legion/v1/phase/retry", map[string]any{"decision": "retry"}}} {
			request := func(grant string) map[string]any {
				body := map[string]any{"grantId": grant, "issue": issue}
				for key, value := range route.extra {
					body[key] = value
				}
				return body
			}
			before := len(facts.recorded())
			assertFailure(t, h.request(http.MethodPost, route.path, request(subArchitect.grant(t)), nil), http.StatusForbidden, "ISSUE_NOT_OWNED")
			if got := h.request(http.MethodPost, route.path, request(root.grant(t)), nil); got.Code != http.StatusOK {
				t.Fatalf("%s on %s by the root architect = %d: %s", route.path, issue, got.Code, got.Body)
			}
			if got := len(facts.recorded()) - before; got != 1 {
				t.Fatalf("%s on %s applied %d facts, want the root architect's one", route.path, issue, got)
			}
		}
	}
}

func TestArchitectureRouteRefusalsExposeTheFactResultAndGrantState(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	h, _, _ := newArchitectHarness(t, credential.New(func() time.Time { return now }),
		&intake.Refusal{Status: http.StatusConflict, Code: "PHASE_HELD", Message: "phase is held"})
	seedTree(t, h, "LEGION-208")
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	grant := architect.grant(t)
	now = now.Add(time.Minute)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/signoff", map[string]any{
		"grantId": grant, "issue": "LEGION-208",
	}, nil), http.StatusForbidden, "GRANT_EXPIRED")

	now = now.Add(-time.Minute)
	grant = architect.grant(t)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/signoff", map[string]any{
		"grantId": grant, "issue": "LEGION-208",
	}, nil), http.StatusConflict, "PHASE_HELD")
	// The grant still authenticates a repeated call, and the repeated fact is refused again rather
	// than answered as though it had applied.
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/signoff", map[string]any{
		"grantId": grant, "issue": "LEGION-208",
	}, nil), http.StatusConflict, "PHASE_HELD")
	architect.replaceRegistration(t)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/signoff", map[string]any{
		"grantId": grant, "issue": "LEGION-208",
	}, nil), http.StatusForbidden, "GRANT_REVOKED")

	invalid := architect.grant(t)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/gates/register", map[string]any{
		"grantId": invalid, "issue": "LEGION-208", "artifactId": "not-a-uuid", "version": 1,
	}, nil), http.StatusBadRequest, "INVALID_ARTIFACT_ID")
}

// release_children exists for children the architect created and has not yet released, which the
// record does not hold: it records a child only once the child is todo under a live tree. So tree
// membership is read over Dispatch's issue graph, as the shipped daemon checks it over the mirrored
// graph (routes/issues.ts handleWaveRelease). A child already in the workflow is refused, since
// writing todo would move an in-progress child back.
func TestWaveReleaseReleasesUnreleasedChildrenOfTheTreeOverTheDispatchGraph(t *testing.T) {
	h, _, statuses := newArchitectHarness(t, nil, nil)
	seedTree(t, h, "LEGION-208")
	root, child := "LEGION-208", "LEGION-209"
	statuses.issues = map[string]dispatch.Issue{
		"LEGION-208": {Key: "LEGION-208", ResolvedRepo: "acme/widgets", Status: "in_progress"},
		"LEGION-209": {Key: "LEGION-209", ResolvedRepo: "acme/widgets", Status: "backlog", Parent: &root},
		"LEGION-210": {Key: "LEGION-210", ResolvedRepo: "acme/widgets", Status: "triage", Parent: &child},
		"LEGION-211": {Key: "LEGION-211", ResolvedRepo: "acme/widgets", Status: "in_progress", Parent: &root},
		"LEGION-212": {Key: "LEGION-212", ResolvedRepo: "acme/widgets", Status: "backlog"},
	}
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	recorder := h.request(http.MethodPost, "/legion/v1/waves/release", map[string]any{
		"grantId": architect.grant(t), "issues": []string{"LEGION-209", "LEGION-210"},
	}, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("wave release = %d: %s", recorder.Code, recorder.Body)
	}
	var response WaveReleaseResponse
	decodeInto(t, recorder, &response)
	if len(response.Released) != 2 || response.Released[0] != "LEGION-209" || response.Released[1] != "LEGION-210" {
		t.Fatalf("released = %#v", response.Released)
	}
	if len(statuses.writes) != 2 || statuses.writes[0] != (statusWrite{issue: "LEGION-209", status: "todo"}) || statuses.writes[1] != (statusWrite{issue: "LEGION-210", status: "todo"}) {
		t.Fatalf("Dispatch writes = %#v", statuses.writes)
	}

	for _, tc := range []struct {
		issue  string
		status int
		code   string
	}{
		{issue: "LEGION-211", status: http.StatusConflict, code: "ISSUE_ALREADY_RELEASED"},
		{issue: "LEGION-212", status: http.StatusForbidden, code: "ISSUE_OUTSIDE_TREE"},
		{issue: "LEGION-213", status: http.StatusNotFound, code: "ISSUE_NOT_FOUND"},
	} {
		assertFailure(t, h.request(http.MethodPost, "/legion/v1/waves/release", map[string]any{
			"grantId": architect.grant(t), "issues": []string{"LEGION-209", tc.issue},
		}, nil), tc.status, tc.code)
	}
	foreign := newLiveClaim(t, h, "LEGION-999", claim.RoleArchitect)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/waves/release", map[string]any{
		"grantId": foreign.grant(t), "issues": []string{"LEGION-209"},
	}, nil), http.StatusForbidden, "ISSUE_OUTSIDE_TREE")
	if len(statuses.writes) != 2 {
		t.Fatalf("Dispatch writes after refusals = %#v, want only the released pair", statuses.writes)
	}
}

// release_children writes one child at a time, so a Dispatch failure part way through leaves the
// earlier children released and answers 502. The architect's retry is the whole wave again, and a
// child already todo is exactly what this call produces: it is released, not refused, and is not
// written a second time. Before, the retry met ISSUE_ALREADY_RELEASED on its own earlier work and
// no retry could ever finish the wave.
func TestWaveReleaseRetryFinishesAWaveAPartialFailureLeftHalfReleased(t *testing.T) {
	h, _, statuses := newArchitectHarness(t, nil, nil)
	seedTree(t, h, "LEGION-208", "LEGION-209") // the first attempt's todo event recorded LEGION-209
	root := "LEGION-208"
	statuses.issues = map[string]dispatch.Issue{
		"LEGION-208": {Key: "LEGION-208", ResolvedRepo: "acme/widgets", Status: "in_progress"},
		"LEGION-209": {Key: "LEGION-209", ResolvedRepo: "acme/widgets", Status: "todo", Parent: &root},    // the first attempt released it
		"LEGION-210": {Key: "LEGION-210", ResolvedRepo: "acme/widgets", Status: "backlog", Parent: &root}, // its write is the one that failed
	}
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)

	recorder := h.request(http.MethodPost, "/legion/v1/waves/release", map[string]any{
		"grantId": architect.grant(t), "issues": []string{"LEGION-209", "LEGION-210"},
	}, nil)

	if recorder.Code != http.StatusOK {
		t.Fatalf("wave release retry = %d: %s", recorder.Code, recorder.Body)
	}
	var response WaveReleaseResponse
	decodeInto(t, recorder, &response)
	if len(response.Released) != 2 {
		t.Fatalf("released = %#v, want the whole wave", response.Released)
	}
	if len(statuses.writes) != 1 || statuses.writes[0] != (statusWrite{issue: "LEGION-210", status: "todo"}) {
		t.Fatalf("Dispatch writes = %#v, want only the child the failed attempt had not released", statuses.writes)
	}
}

// An architect parks a child of its tree and runs a parked or signed-off child again by the same
// Dispatch status a human would move it by: backlog, then todo. Each write's event takes the human
// move's path through the workflow, which parks the child (leave) or starts its next run under the
// live tree (reenterChild); the routes apply no fact of their own. The tree root is never a child,
// a child already out of the workflow has nothing to park, a running child is parked before it
// runs again, and a lingering tree runs nothing more.
func TestParkAndRerunMoveAChildOfTheTreeByItsDispatchStatus(t *testing.T) {
	h, facts, statuses := newArchitectHarness(t, nil, nil)
	seedTree(t, h, "LEGION-208", "LEGION-209", "LEGION-210")
	setIssue(t, h, "LEGION-210", func(issue *record.Issue) { issue.Phase, issue.Status = phase.Done, "done" })
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	child := func(route, issue string) *httptest.ResponseRecorder {
		return h.request(http.MethodPost, "/legion/v1/children/"+route, map[string]any{"grantId": architect.grant(t), "issue": issue}, nil)
	}

	if recorder := child("park", "LEGION-209"); recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != "{}" {
		t.Fatalf("park = %d %s, want 200 {}", recorder.Code, recorder.Body)
	}
	if recorder := child("rerun", "LEGION-210"); recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != "{}" {
		t.Fatalf("rerun = %d %s, want 200 {}", recorder.Code, recorder.Body)
	}
	want := []statusWrite{{issue: "LEGION-209", status: "backlog"}, {issue: "LEGION-210", status: "todo"}}
	if len(statuses.writes) != 2 || statuses.writes[0] != want[0] || statuses.writes[1] != want[1] {
		t.Fatalf("Dispatch writes = %#v, want %#v", statuses.writes, want)
	}

	for _, tc := range []struct {
		route, issue string
		status       int
		code         string
	}{
		{route: "park", issue: "LEGION-208", status: http.StatusForbidden, code: "CHILD_REQUIRED"},
		{route: "rerun", issue: "LEGION-208", status: http.StatusForbidden, code: "CHILD_REQUIRED"},
		{route: "park", issue: "LEGION-210", status: http.StatusConflict, code: "CHILD_NOT_RUNNING"},
		{route: "rerun", issue: "LEGION-209", status: http.StatusConflict, code: "CHILD_RUNNING"},
		{route: "park", issue: "LEGION-777", status: http.StatusNotFound, code: "ISSUE_NOT_FOUND"},
		{route: "rerun", issue: "not a key", status: http.StatusBadRequest, code: "INVALID_ISSUE"},
	} {
		t.Run(tc.route+" "+tc.issue, func(t *testing.T) {
			recorder := child(tc.route, tc.issue)
			if tc.status != http.StatusBadRequest && tc.status != http.StatusNotFound && !strings.Contains(recorder.Body.String(), tc.issue) {
				t.Fatalf("refusal %s does not name %s", recorder.Body, tc.issue)
			}
			assertFailure(t, recorder, tc.status, tc.code)
		})
	}
	foreign := newLiveClaim(t, h, "LEGION-999", claim.RoleArchitect)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/children/park", map[string]any{
		"grantId": foreign.grant(t), "issue": "LEGION-209",
	}, nil), http.StatusForbidden, "ISSUE_OUTSIDE_TREE")
	worker := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/children/rerun", map[string]any{
		"grantId": worker.grant(t), "issue": "LEGION-210",
	}, nil), http.StatusForbidden, "ARCHITECT_REQUIRED")

	until := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	setIssue(t, h, "LEGION-208", func(issue *record.Issue) { issue.Phase, issue.LingerUntil = phase.Done, &until })
	assertFailure(t, child("rerun", "LEGION-210"), http.StatusConflict, "TREE_LINGERING")
	setIssue(t, h, "LEGION-208", func(issue *record.Issue) { issue.Phase, issue.LingerUntil = phase.Implementing, nil })

	statuses.err = &dispatch.Error{Status: http.StatusServiceUnavailable, Code: "UNAVAILABLE", Message: "Dispatch is down"}
	assertFailure(t, child("rerun", "LEGION-210"), http.StatusBadGateway, "UNAVAILABLE")
	statuses.err = errors.New("connection refused")
	assertFailure(t, child("park", "LEGION-209"), http.StatusBadGateway, "DISPATCH_FAILED")

	if len(statuses.writes) != 2 {
		t.Fatalf("Dispatch writes after refusals = %#v, want only the park and the re-run", statuses.writes)
	}
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("facts = %#v, want none: the Dispatch write's event moves the child", got)
	}
}

// setIssue rewrites one recorded issue in place.
func setIssue(t *testing.T, h *harness, key string, change func(*record.Issue)) {
	t.Helper()
	err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		records := record.NewStore()
		issue, err := records.Issue(context.Background(), tx, key)
		if err != nil {
			return err
		}
		change(issue)
		return records.PutIssue(context.Background(), tx, *issue)
	})
	if err != nil {
		t.Fatalf("set issue %s: %v", key, err)
	}
}

// The design gate is the only human checkpoint before planning, so it opens only on the tree
// root's own document. A document another issue carries is refused naming both, as the shipped
// daemon refuses it (routes/issues.ts seedGateFromDispatch), and so is a gate on a child, whose
// gate would advance the whole tree; neither reaches the workflow.
func TestGateRegistrationRefusesAnotherIssuesDocumentAndAChildIssue(t *testing.T) {
	h, facts, statuses := newArchitectHarness(t, nil, nil)
	seedTree(t, h, "LEGION-208", "LEGION-209")
	statuses.documents = map[string]string{
		"0b7e6a2c-4f1d-4c8e-9a35-2d6f1e8b7c40": "LEGION-777",
		"5c3d9e1f-7a2b-4e6c-8d40-1f9b3a7e2c65": "LEGION-209",
	}
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)

	foreign := h.request(http.MethodPost, "/legion/v1/gates/register", map[string]any{
		"grantId": architect.grant(t), "issue": "LEGION-208", "artifactId": "0b7e6a2c-4f1d-4c8e-9a35-2d6f1e8b7c40", "version": 1,
	}, nil)
	if body := foreign.Body.String(); !strings.Contains(body, "0b7e6a2c-4f1d-4c8e-9a35-2d6f1e8b7c40") || !strings.Contains(body, "LEGION-208") {
		t.Fatalf("refusal %s does not name the document and the issue", body)
	}
	assertFailure(t, foreign, http.StatusNotFound, "ARTIFACT_NOT_ON_ISSUE")
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/gates/register", map[string]any{
		"grantId": architect.grant(t), "issue": "LEGION-208", "artifactId": "9e8d7c6b-5a4f-4e3d-8c2b-1a0f9e8d7c6b", "version": 1,
	}, nil), http.StatusNotFound, "ARTIFACT_NOT_ON_ISSUE")
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/gates/register", map[string]any{
		"grantId": architect.grant(t), "issue": "LEGION-209", "artifactId": "5c3d9e1f-7a2b-4e6c-8d40-1f9b3a7e2c65", "version": 1,
	}, nil), http.StatusForbidden, "GATE_ROOT_ONLY")
	// The root's gate is its own architect's to register: a sub-architect of the tree holding the
	// root document's id is refused too.
	statuses.documents["7d1e4f2a-3b5c-4d6e-8f70-9a1b2c3d4e5f"] = "LEGION-208"
	subArchitect := newLiveClaimIn(t, h, "LEGION-208", "LEGION-209", claim.RoleArchitect)
	refusal := h.request(http.MethodPost, "/legion/v1/gates/register", map[string]any{
		"grantId": subArchitect.grant(t), "issue": "LEGION-208", "artifactId": "7d1e4f2a-3b5c-4d6e-8f70-9a1b2c3d4e5f", "version": 1,
	}, nil)
	if body := refusal.Body.String(); !strings.Contains(body, "its root architect registers it") {
		t.Fatalf("sub-architect refusal %s does not say the root architect registers the gate", body)
	}
	assertFailure(t, refusal, http.StatusForbidden, "GATE_ROOT_ONLY")
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("facts = %#v, want no gate registered", got)
	}
}

// A re-admitted root is a new generation whose architect registers its spec again, often at the
// version generation 1 registered. That registration is its own fact and reaches the workflow;
// a duplicate of generation 1's would be answered as accepted while it changed nothing, leaving
// the tree admitted with no gate.
func TestGateRegistrationIsItsOwnFactInEachGeneration(t *testing.T) {
	h, facts, statuses := newArchitectHarness(t, nil, nil)
	statuses.documents = map[string]string{"d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093": "LEGION-208"}
	seedTree(t, h, "LEGION-208")
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	register := func() {
		t.Helper()
		if recorder := h.request(http.MethodPost, "/legion/v1/gates/register", map[string]any{
			"grantId": architect.grant(t), "issue": "LEGION-208", "artifactId": "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093", "version": 1,
		}, nil); recorder.Code != http.StatusOK {
			t.Fatalf("gate register = %d: %s", recorder.Code, recorder.Body)
		}
	}
	register()
	err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		records := record.NewStore()
		root, err := records.Issue(context.Background(), tx, "LEGION-208")
		if err != nil {
			return err
		}
		root.Generation++
		return records.PutIssue(context.Background(), tx, *root)
	})
	if err != nil {
		t.Fatalf("re-admit LEGION-208: %v", err)
	}
	register()
	if got := facts.recorded(); len(got) != 2 {
		t.Fatalf("gate facts = %#v, want one registration per generation", got)
	}
}

// A person who approved the spec from the document header before its architect registered it:
// register_gate's own read of the document sees the approval, and carries it into the fact, so the
// workflow opens the gate at registration.
func TestGateRegistrationCarriesAnApprovalDispatchAlreadyShows(t *testing.T) {
	h, facts, statuses := newArchitectHarness(t, nil, nil)
	statuses.documents = map[string]string{
		"d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093": "LEGION-208",
		"4a6b8c0d-2e4f-4a6b-8c0d-2e4f6a8b0c1d": "LEGION-208",
	}
	statuses.approved = map[string]int{"d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093": 3}
	statuses.approvals = map[string]dispatch.Approval{"4a6b8c0d-2e4f-4a6b-8c0d-2e4f6a8b0c1d": {State: "none", LatestVersion: 3}}
	seedTree(t, h, "LEGION-208")
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	for _, artifact := range []string{"d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093", "4a6b8c0d-2e4f-4a6b-8c0d-2e4f6a8b0c1d"} {
		if recorder := h.request(http.MethodPost, "/legion/v1/gates/register", map[string]any{
			"grantId": architect.grant(t), "issue": "LEGION-208", "artifactId": artifact, "version": 3,
		}, nil); recorder.Code != http.StatusOK {
			t.Fatalf("gate register %s = %d: %s", artifact, recorder.Code, recorder.Body)
		}
	}
	want := []intake.Fact{
		intake.GateRegistered{Issue: "LEGION-208", ArtifactID: "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093", Version: 3, ApprovedVersion: 3, DispatchVersion: 3, Since: record.EventMark{Seq: 7}},
		intake.GateRegistered{Issue: "LEGION-208", ArtifactID: "4a6b8c0d-2e4f-4a6b-8c0d-2e4f6a8b0c1d", Version: 3, DispatchVersion: 3, Since: record.EventMark{Seq: 7}},
	}
	if got := facts.recorded(); !reflect.DeepEqual(got, want) {
		t.Fatalf("gate facts = %#v, want the approved document to carry its approval and the other none", got)
	}
}

// register_gate carries a changes request Dispatch shows on the document's latest version, with its
// reason and the review's place in the issue's event log, so a request given between the
// architect's approval request and its registration closes the gate with the architect told. An
// open request carries no review, and a changes request a newer version superseded carries nothing.
// Each registration reads the issue's event sequence before the approval, and the read begins at
// that sequence alone: the latest review's time is that review's, never the read's.
func TestGateRegistrationCarriesAChangesRequestDispatchAlreadyShows(t *testing.T) {
	h, facts, statuses := newArchitectHarness(t, nil, nil)
	const changed, open, superseded = "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093", "4a6b8c0d-2e4f-4a6b-8c0d-2e4f6a8b0c1d", "7c1e3a5b-9d2f-4b6a-8e0c-1d3f5a7b9c2e"
	statuses.documents = map[string]string{changed: "LEGION-208", open: "LEGION-208", superseded: "LEGION-208"}
	reason := "name the rollback path"
	reviewed := time.Date(2026, 10, 23, 17, 0, 0, 123456000, time.UTC)
	statuses.approvals = map[string]dispatch.Approval{
		changed:    {State: "changes_requested", LatestVersion: 4, Version: new(4), Reason: &reason, At: reviewed, Seq: 6},
		open:       {State: "awaiting", LatestVersion: 4, Version: new(3), At: reviewed},
		superseded: {State: "changes_requested", LatestVersion: 5, Version: new(4), Reason: &reason, At: reviewed, Seq: 5},
	}
	seedTree(t, h, "LEGION-208")
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	for _, artifact := range []string{changed, open, superseded} {
		if recorder := h.request(http.MethodPost, "/legion/v1/gates/register", map[string]any{
			"grantId": architect.grant(t), "issue": "LEGION-208", "artifactId": artifact, "version": 4,
		}, nil); recorder.Code != http.StatusOK {
			t.Fatalf("gate register %s = %d: %s", artifact, recorder.Code, recorder.Body)
		}
	}
	since := record.EventMark{Seq: 7}
	want := []intake.Fact{
		intake.GateRegistered{Issue: "LEGION-208", ArtifactID: changed, Version: 4, ChangesRequestedVersion: 4, ChangesReason: reason,
			ChangesRequestedMark: record.EventMark{Seq: 6, At: reviewed}, DispatchVersion: 4, Since: since},
		intake.GateRegistered{Issue: "LEGION-208", ArtifactID: open, Version: 4, DispatchVersion: 4, Since: since},
		intake.GateRegistered{Issue: "LEGION-208", ArtifactID: superseded, Version: 4, DispatchVersion: 5, Since: since},
	}
	if got := facts.recorded(); !reflect.DeepEqual(got, want) {
		t.Fatalf("gate facts = %#v, want %#v", got, want)
	}
	wantReads := []string{"issue:LEGION-208", "approval:" + changed, "issue:LEGION-208", "approval:" + open, "issue:LEGION-208", "approval:" + superseded}
	if !reflect.DeepEqual(statuses.reads, wantReads) {
		t.Fatalf("Dispatch reads = %q, want the issue's sequence read before each approval: %q", statuses.reads, wantReads)
	}
}

// A read that shows an approval request open names it on the registration, with whose turn it
// waits on, so the registration mirrors it as the read found it; a request waiting on the person
// is not the agent's.
func TestGateRegistrationCarriesTheOpenRequestAndItsTurn(t *testing.T) {
	h, facts, statuses := newArchitectHarness(t, nil, nil)
	const withPerson, withAgent = "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093", "4a6b8c0d-2e4f-4a6b-8c0d-2e4f6a8b0c1d"
	statuses.documents = map[string]string{withPerson: "LEGION-208", withAgent: "LEGION-208"}
	statuses.approvals = map[string]dispatch.Approval{
		withPerson: {State: "awaiting", LatestVersion: 4, AskID: "ask-person", WaitingOn: "human"},
		withAgent:  {State: "awaiting", LatestVersion: 4, AskID: "ask-agent", WaitingOn: "agent"},
	}
	seedTree(t, h, "LEGION-208")
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	for _, artifact := range []string{withPerson, withAgent} {
		if recorder := h.request(http.MethodPost, "/legion/v1/gates/register", map[string]any{
			"grantId": architect.grant(t), "issue": "LEGION-208", "artifactId": artifact, "version": 4,
		}, nil); recorder.Code != http.StatusOK {
			t.Fatalf("gate register %s = %d: %s", artifact, recorder.Code, recorder.Body)
		}
	}
	since := record.EventMark{Seq: 7}
	want := []intake.Fact{
		intake.GateRegistered{Issue: "LEGION-208", ArtifactID: withPerson, Version: 4, DispatchVersion: 4, Since: since, RequestAsk: "ask-person"},
		intake.GateRegistered{Issue: "LEGION-208", ArtifactID: withAgent, Version: 4, DispatchVersion: 4, Since: since, RequestAsk: "ask-agent", RequestWaitsOnAgent: true},
	}
	if got := facts.recorded(); !reflect.DeepEqual(got, want) {
		t.Fatalf("gate facts = %#v, want %#v", got, want)
	}
}

// register_gate refuses a version Dispatch does not show for the document: the caller names the
// version, and a gate recorded past the document's own latest version is one no person's review
// can open.
func TestGateRegistrationRefusesAVersionDispatchDoesNotShow(t *testing.T) {
	h, facts, statuses := newArchitectHarness(t, nil, nil)
	const artifact = "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093"
	statuses.documents = map[string]string{artifact: "LEGION-208"}
	statuses.approvals = map[string]dispatch.Approval{artifact: {State: "awaiting", LatestVersion: 4}}
	seedTree(t, h, "LEGION-208")
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/gates/register", map[string]any{
		"grantId": architect.grant(t), "issue": "LEGION-208", "artifactId": artifact, "version": 5,
	}, nil), http.StatusConflict, "VERSION_NOT_IN_DISPATCH")
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("gate facts = %#v, want none for a refused registration", got)
	}
}

// Two registrations of one version are two facts when their reads of Dispatch differ: the second
// carries a review the first could not see, and the fact's id names the read it began at, so the
// handlers see it. A retry of one read stays one fact.
func TestASecondRegistrationAtOneVersionCarriesWhatItsOwnReadFound(t *testing.T) {
	h, facts, statuses := newArchitectHarness(t, nil, nil)
	const artifact = "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093"
	statuses.documents = map[string]string{artifact: "LEGION-208"}
	statuses.approvals = map[string]dispatch.Approval{artifact: {State: "awaiting", LatestVersion: 4}}
	seedTree(t, h, "LEGION-208")
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	register := func() {
		t.Helper()
		response := h.request(http.MethodPost, "/legion/v1/gates/register", map[string]any{
			"grantId": architect.grant(t), "issue": "LEGION-208", "artifactId": artifact, "version": 4,
		}, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("register gate = %d: %s", response.Code, response.Body)
		}
	}

	register()
	register()
	if got := facts.recorded(); len(got) != 1 {
		t.Fatalf("facts after two registrations of one read = %#v, want the retry deduplicated", got)
	}

	// The person requests changes, which moves the issue's sequence, and the architect registers
	// the same version again.
	statuses.issues["LEGION-208"] = dispatch.Issue{Key: "LEGION-208", LastSeq: 11}
	statuses.approvals[artifact] = dispatch.Approval{State: "changes_requested", LatestVersion: 4, Version: new(4), Reason: new("name the rollback path"), Seq: 11}
	register()
	got := facts.recorded()
	if len(got) != 2 {
		t.Fatalf("facts after a registration carrying a new review = %#v, want two", got)
	}
	registered, ok := got[1].(intake.GateRegistered)
	if !ok || registered.ChangesRequestedVersion != 4 || registered.ChangesRequestedMark.Seq != 11 || registered.Since.Seq != 11 {
		t.Fatalf("second registration fact = %#v, want the changes request at sequence 11 and a read beginning there", got[1])
	}
}

// A root architect ends its own tree before its first phase starts: a human decided no change at
// the design gate, or the issue is moot. The route takes the reason; nothing else ends an admitted
// root from the architect's side, since sign_off needs the production check and a backward move
// needs a phase worker.
func TestAnArchitectClosesItsAdmittedRoot(t *testing.T) {
	h, facts, _ := newArchitectHarness(t, nil, nil)
	seedTree(t, h, "LEGION-208", "LEGION-209")
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	closed := h.request(http.MethodPost, "/legion/v1/roots/close", map[string]any{
		"grantId": architect.grant(t), "issue": "LEGION-208", "reason": "The human decided no change is needed.",
	}, nil)
	if closed.Code != http.StatusOK {
		t.Fatalf("close = %d: %s", closed.Code, closed.Body)
	}
	if got := facts.recorded(); len(got) != 1 || got[0] != (intake.CloseRoot{Issue: "LEGION-208", Reason: "The human decided no change is needed."}) {
		t.Fatalf("close facts = %#v", got)
	}

	// A child is not the root, a sub-architect of the tree is not the root's architect though it
	// names the root, a phase worker is not the architect, and a reason too long for one message
	// would never post.
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/roots/close", map[string]any{
		"grantId": architect.grant(t), "issue": "LEGION-209", "reason": "moot",
	}, nil), http.StatusForbidden, "ROOT_REQUIRED")
	subArchitect := newLiveClaimIn(t, h, "LEGION-208", "LEGION-209", claim.RoleArchitect)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/roots/close", map[string]any{
		"grantId": subArchitect.grant(t), "issue": "LEGION-208", "reason": "a sub-architect closing the root",
	}, nil), http.StatusForbidden, "ROOT_REQUIRED")
	worker := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/roots/close", map[string]any{
		"grantId": worker.grant(t), "issue": "LEGION-208", "reason": "moot",
	}, nil), http.StatusForbidden, "ARCHITECT_REQUIRED")
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/roots/close", map[string]any{
		"grantId": architect.grant(t), "issue": "LEGION-208", "reason": strings.Repeat("x", record.MessagePostLimit),
	}, nil), http.StatusBadRequest, "CLOSE_REASON_TOO_LONG")
	if got := facts.recorded(); len(got) != 1 {
		t.Fatalf("refused closes applied facts %#v", got[1:])
	}
}
