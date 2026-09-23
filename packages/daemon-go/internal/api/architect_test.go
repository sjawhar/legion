package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
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
}

func (r *statusRecorder) ListIssues(context.Context, string, []string) ([]dispatch.IssueSummary, error) {
	return nil, nil
}

func (r *statusRecorder) GetIssue(context.Context, string) (dispatch.Issue, error) {
	return dispatch.Issue{}, nil
}

func (r *statusRecorder) SetStatus(_ context.Context, issue, status string) error {
	if r.err != nil {
		return r.err
	}
	r.writes = append(r.writes, statusWrite{issue: issue, status: status})
	return nil
}

func (r *statusRecorder) PostMessage(context.Context, string, string) error { return nil }

func (r *statusRecorder) Approval(context.Context, string) (dispatch.Approval, error) {
	return dispatch.Approval{}, nil
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
	session string
	secret  string
	tree    string
	issue   string
}

func newLiveClaim(t *testing.T, h *harness, issue string, role claim.Role) liveClaim {
	t.Helper()
	_, boot := h.launch(issue, role)
	session := "ses_" + string(role) + "_" + issue
	registration := h.registered(boot, session)
	return liveClaim{h: h, session: session, secret: registration.Secret, tree: issue, issue: issue}
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

func routeEventID(route, grantID string) string {
	hash := sha256.Sum256([]byte(grantID))
	return "api:" + route + ":" + hex.EncodeToString(hash[:])[:16]
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
	h, facts, _ := newArchitectHarness(t, nil, nil)
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
	assertProcessedEvent(t, h, "gate:LEGION-208:d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093:7")

	retryGrant := architect.grant(t)
	retry := h.request(http.MethodPost, "/legion/v1/phase/retry", map[string]any{
		"grantId": retryGrant, "issue": "LEGION-209", "decision": "retry",
	}, nil)
	if retry.Code != http.StatusOK {
		t.Fatalf("phase retry = %d: %s", retry.Code, retry.Body)
	}
	assertProcessedEvent(t, h, routeEventID("phase/retry", retryGrant))

	signoffGrant := architect.grant(t)
	signoff := h.request(http.MethodPost, "/legion/v1/signoff", map[string]any{
		"grantId": signoffGrant, "issue": "LEGION-208",
	}, nil)
	if signoff.Code != http.StatusOK {
		t.Fatalf("signoff = %d: %s", signoff.Code, signoff.Body)
	}
	assertProcessedEvent(t, h, routeEventID("signoff", signoffGrant))

	worker := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	backwardGrant := worker.grant(t)
	backward := h.request(http.MethodPost, "/legion/v1/phase/backward", map[string]any{
		"grantId": backwardGrant, "to": "implementing", "reason": "review requested changes",
	}, nil)
	if backward.Code != http.StatusOK {
		t.Fatalf("phase backward = %d: %s", backward.Code, backward.Body)
	}
	assertProcessedEvent(t, h, routeEventID("phase/backward", backwardGrant))

	got := facts.recorded()
	if len(got) != 4 {
		t.Fatalf("facts = %#v, want four", got)
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
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/signoff", map[string]any{
		"grantId": grant, "issue": "LEGION-208",
	}, nil), http.StatusForbidden, "GRANT_USED")

	invalid := architect.grant(t)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/gates/register", map[string]any{
		"grantId": invalid, "issue": "LEGION-208", "artifactId": "not-a-uuid", "version": 1,
	}, nil), http.StatusBadRequest, "INVALID_ARTIFACT_ID")
}

func TestWaveReleaseWritesDispatchSynchronously(t *testing.T) {
	h, _, statuses := newArchitectHarness(t, nil, nil)
	seedTree(t, h, "LEGION-208", "LEGION-209")
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	recorder := h.request(http.MethodPost, "/legion/v1/waves/release", map[string]any{
		"grantId": architect.grant(t), "issues": []string{"LEGION-209"},
	}, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("wave release = %d: %s", recorder.Code, recorder.Body)
	}
	var response WaveReleaseResponse
	decodeInto(t, recorder, &response)
	if len(response.Released) != 1 || response.Released[0] != "LEGION-209" {
		t.Fatalf("released = %#v", response.Released)
	}
	if len(statuses.writes) != 1 || statuses.writes[0] != (statusWrite{issue: "LEGION-209", status: "todo"}) {
		t.Fatalf("Dispatch writes = %#v", statuses.writes)
	}

	foreign := newLiveClaim(t, h, "LEGION-999", claim.RoleArchitect)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/waves/release", map[string]any{
		"grantId": foreign.grant(t), "issues": []string{"LEGION-209"},
	}, nil), http.StatusForbidden, "ISSUE_OUTSIDE_TREE")
}
