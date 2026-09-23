package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// seedIssueAt records key as a root at the given phase, which is where the handoff route reads the
// phase a completion finishes.
func seedIssueAt(t *testing.T, h *harness, key string, at phase.Phase) {
	t.Helper()
	err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		return record.NewStore().PutIssue(context.Background(), tx, record.Issue{
			Key: key, Tree: key, Project: testProject, Title: key, Phase: at, Generation: 1, Status: "in_progress",
		})
	})
	if err != nil {
		t.Fatalf("seed %s at %s: %v", key, at, err)
	}
}

func TestHandoffCompleteEnforcesRoleSpecificFieldsAndDeduplicatesCommit(t *testing.T) {
	h, facts, _ := newArchitectHarness(t, nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Testing)

	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/handoff/complete", HandoffCompleteRequest{
		GrantID: tester.grant(t), Summary: "ran the suite", Commit: "aabbcc",
	}, nil), http.StatusBadRequest, "TESTER_VERDICT_REQUIRED")

	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/handoff/complete", HandoffCompleteRequest{
		GrantID: implementer.grant(t), Summary: "implemented", Verdict: "pass", Commit: "bbccdd",
	}, nil), http.StatusBadRequest, "VERDICT_ROLE_FORBIDDEN")

	worker := newLiveClaim(t, h, "LEGION-208", claim.RoleReviewer)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/handoff/complete", HandoffCompleteRequest{
		GrantID: worker.grant(t), Summary: "reviewed", Ready: true, Commit: "ccddee",
	}, nil), http.StatusBadRequest, "READY_ROLE_FORBIDDEN")

	request := HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass", Commit: "ddeeff"}
	if recorder := h.request(http.MethodPost, "/legion/v1/handoff/complete", request, nil); recorder.Code != http.StatusOK {
		t.Fatalf("handoff = %d: %s", recorder.Code, recorder.Body)
	}
	// A retried completion of the same phase at the same commit changes nothing, and says so.
	request.GrantID = tester.grant(t)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/handoff/complete", request, nil), http.StatusConflict, "HANDOFF_ALREADY_RECORDED")
	got := facts.recorded()
	if len(got) != 1 {
		t.Fatalf("handoff facts = %#v, want one deduplicated fact", got)
	}
	fact, ok := got[0].(intake.HandoffComplete)
	if !ok || fact.Role != claim.RoleTester || fact.Verdict != "pass" || fact.Commit != "ddeeff" {
		t.Fatalf("handoff fact = %#v", got[0])
	}
}

// The implementer runs retro and then, after the merge, the production check, and it may report
// both from the one commit carrying its handoff. Each is its own phase's completion.
func TestHandoffCompleteRecordsRetroThenProductionCheckAtOneCommit(t *testing.T) {
	h, facts, _ := newArchitectHarness(t, nil, nil)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	for _, at := range []phase.Phase{phase.Retro, phase.ProductionCheck} {
		seedIssueAt(t, h, "LEGION-208", at)
		recorder := h.request(http.MethodPost, "/legion/v1/handoff/complete", HandoffCompleteRequest{
			GrantID: implementer.grant(t), Summary: string(at) + " done", Commit: "c0ffee",
		}, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s handoff = %d: %s", at, recorder.Code, recorder.Body)
		}
	}
	got := facts.recorded()
	if len(got) != 2 {
		t.Fatalf("handoff facts = %#v, want the retro and the production-check completions", got)
	}
	for i, summary := range []string{"retro done", "production_check done"} {
		if fact, ok := got[i].(intake.HandoffComplete); !ok || fact.Summary != summary || fact.Commit != "c0ffee" {
			t.Fatalf("handoff fact %d = %#v, want summary %q at c0ffee", i, got[i], summary)
		}
	}
}

func TestHandoffCompleteRefusesAnUnrecordedIssue(t *testing.T) {
	h, facts, _ := newArchitectHarness(t, nil, nil)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/handoff/complete", HandoffCompleteRequest{
		GrantID: implementer.grant(t), Summary: "implemented", Commit: "aabbcc",
	}, nil), http.StatusNotFound, "ISSUE_NOT_FOUND")
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("handoff facts = %#v, want none for an unrecorded issue", got)
	}
}

func TestHandoffCompleteRefusesExpiredAndRevokedGrants(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	h, facts, _ := newArchitectHarness(t, credential.New(func() time.Time { return now }), nil)
	seedIssueAt(t, h, "LEGION-208", phase.Implementing)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	expired := implementer.grant(t)
	now = now.Add(time.Minute)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/handoff/complete", HandoffCompleteRequest{
		GrantID: expired, Summary: "implemented", Commit: "aabbcc",
	}, nil), http.StatusForbidden, "GRANT_EXPIRED")

	now = now.Add(-time.Minute)
	grant := implementer.grant(t)
	if recorder := h.request(http.MethodPost, "/legion/v1/handoff/complete", HandoffCompleteRequest{
		GrantID: grant, Summary: "implemented", Commit: "bbccdd",
	}, nil); recorder.Code != http.StatusOK {
		t.Fatalf("first handoff = %d: %s", recorder.Code, recorder.Body)
	}
	// The same command's grant still authenticates a second call; the phase's dedupe answers it.
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/handoff/complete", HandoffCompleteRequest{
		GrantID: grant, Summary: "implemented", Commit: "bbccdd",
	}, nil), http.StatusConflict, "HANDOFF_ALREADY_RECORDED")

	implementer.replaceRegistration(t)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/handoff/complete", HandoffCompleteRequest{
		GrantID: grant, Summary: "implemented again", Commit: "ccddee",
	}, nil), http.StatusForbidden, "GRANT_REVOKED")
	if got := facts.recorded(); len(got) != 1 {
		t.Fatalf("handoff facts = %#v, want only the completion made before the claim was replaced", got)
	}
}

// A merger whose completion was refused READY_REQUIRED corrects it with --ready at the same
// commit. That corrected call is a different fact and reaches the workflow; before, it carried the
// refused call's event id, was answered "already received", and left the issue in merging with no
// way out but a new commit nothing asked for.
func TestHandoffCompleteAppliesTheMergersCorrectedReadyAtTheSameCommit(t *testing.T) {
	h, facts, _ := newArchitectHarness(t, nil, &intake.Refusal{Status: http.StatusConflict, Code: "READY_REQUIRED", Message: "run legion handoff complete --ready"})
	seedIssueAt(t, h, "LEGION-208", phase.Merging)
	merger := newLiveClaim(t, h, "LEGION-208", claim.RoleMerger)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/handoff/complete", HandoffCompleteRequest{
		GrantID: merger.grant(t), Summary: "merge-ready", Commit: "facade",
	}, nil), http.StatusConflict, "READY_REQUIRED")

	facts.mu.Lock()
	facts.refusal = nil
	facts.mu.Unlock()
	corrected := HandoffCompleteRequest{GrantID: merger.grant(t), Summary: "merge-ready", Ready: true, Commit: "facade"}
	if recorder := h.request(http.MethodPost, "/legion/v1/handoff/complete", corrected, nil); recorder.Code != http.StatusOK {
		t.Fatalf("corrected READY = %d: %s", recorder.Code, recorder.Body)
	}
	corrected.GrantID = merger.grant(t)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/handoff/complete", corrected, nil), http.StatusConflict, "HANDOFF_ALREADY_RECORDED")
	got := facts.recorded()
	if len(got) != 2 {
		t.Fatalf("handoff facts = %#v, want the refused completion and the corrected READY", got)
	}
	if fact, ok := got[1].(intake.HandoffComplete); !ok || !fact.Ready || fact.Commit != "facade" {
		t.Fatalf("corrected fact = %#v, want READY at facade", got[1])
	}
}
