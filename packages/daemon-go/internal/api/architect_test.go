package api

import (
	"context"
	"net/http"
	"net/http/httptest"
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
	issues    map[string]dispatch.Issue
}

func (r *statusRecorder) ListIssues(context.Context, string, []string) ([]dispatch.IssueSummary, error) {
	return nil, nil
}

func (r *statusRecorder) GetIssue(_ context.Context, key string) (dispatch.Issue, error) {
	issue, ok := r.issues[key]
	if !ok {
		return dispatch.Issue{}, &dispatch.Error{Status: http.StatusNotFound, Code: "NOT_FOUND", Message: "issue not found"}
	}
	return issue, nil
}

func (r *statusRecorder) SetStatus(_ context.Context, issue, status string) error {
	if r.err != nil {
		return r.err
	}
	r.writes = append(r.writes, statusWrite{issue: issue, status: status})
	return nil
}

func (r *statusRecorder) PostMessage(context.Context, string, string) error { return nil }
func (r *statusRecorder) MessageBodiesSince(context.Context, string, time.Time) ([]string, error) {
	return nil, nil
}

func (r *statusRecorder) Approval(_ context.Context, artifactID string) (dispatch.Approval, error) {
	issue, ok := r.documents[artifactID]
	if !ok {
		return dispatch.Approval{}, &dispatch.Error{Status: http.StatusNotFound, Code: "NOT_FOUND", Message: "artifact not found"}
	}
	return dispatch.Approval{IssueKey: issue, State: "none", LatestVersion: 1}, nil
}

var _ dispatch.Client = (*statusRecorder)(nil)

func newArchitectHarness(t *testing.T, grants *credential.Grants, refusal *intake.Refusal) (*harness, *factRecorder, *statusRecorder) {
	t.Helper()
	h := newHarness(t)
	if grants == nil {
		grants = credential.New(nil)
	}
	facts, statuses := &factRecorder{refusal: refusal}, &statusRecorder{}
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor:    h.supervisor,
		BootTokens:    h.tokens,
		Project:       testProject,
		OperatorToken: testOperatorToken,
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
			Key: root, Tree: root, Project: testProject, Title: root, Phase: phase.Planning, Status: "in_progress",
		}); err != nil {
			return err
		}
		for _, child := range children {
			parent := root
			if err := records.PutIssue(context.Background(), tx, record.Issue{
				Key: child, Tree: root, Project: testProject, Title: child, Parent: &parent,
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
	token, boot := h.launch(issue, role)
	session := "ses_" + string(role) + "_" + issue
	registration := h.registered(boot, session)
	return liveClaim{h: h, token: token, session: session, secret: registration.Secret, tree: issue, issue: issue}
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
	assertProcessedEvent(t, h, "gate:LEGION-208:0:d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093:7")

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
		"grantId": architect.grant(t), "to": "planning", "reason": "architect cannot move a phase",
	}, nil), http.StatusForbidden, "PHASE_WORKER_REQUIRED")
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("refused calls applied facts %#v", got)
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
		"LEGION-208": {Key: "LEGION-208", Status: "in_progress"},
		"LEGION-209": {Key: "LEGION-209", Status: "backlog", Parent: &root},
		"LEGION-210": {Key: "LEGION-210", Status: "triage", Parent: &child},
		"LEGION-211": {Key: "LEGION-211", Status: "in_progress", Parent: &root},
		"LEGION-212": {Key: "LEGION-212", Status: "backlog"},
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
