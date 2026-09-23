package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/intake"
)

func TestHandoffCompleteEnforcesRoleSpecificFieldsAndDeduplicatesCommit(t *testing.T) {
	h, facts, _ := newArchitectHarness(t, nil, nil)

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

	for _, grant := range []string{tester.grant(t), tester.grant(t)} {
		recorder := h.request(http.MethodPost, "/legion/v1/handoff/complete", HandoffCompleteRequest{
			GrantID: grant, Summary: "tests pass", Verdict: "pass", Commit: "ddeeff",
		}, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("handoff = %d: %s", recorder.Code, recorder.Body)
		}
	}
	got := facts.recorded()
	if len(got) != 1 {
		t.Fatalf("handoff facts = %#v, want one deduplicated fact", got)
	}
	fact, ok := got[0].(intake.HandoffComplete)
	if !ok || fact.Role != claim.RoleTester || fact.Verdict != "pass" || fact.Commit != "ddeeff" {
		t.Fatalf("handoff fact = %#v", got[0])
	}
}

func TestHandoffCompleteReportsExpiredAndUsedGrants(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	h, _, _ := newArchitectHarness(t, credential.New(func() time.Time { return now }), nil)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	expired := implementer.grant(t)
	now = now.Add(time.Minute)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/handoff/complete", HandoffCompleteRequest{
		GrantID: expired, Summary: "implemented", Commit: "aabbcc",
	}, nil), http.StatusForbidden, "GRANT_EXPIRED")

	now = now.Add(-time.Minute)
	request := HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "implemented", Commit: "bbccdd"}
	if recorder := h.request(http.MethodPost, "/legion/v1/handoff/complete", request, nil); recorder.Code != http.StatusOK {
		t.Fatalf("first handoff = %d: %s", recorder.Code, recorder.Body)
	}
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/handoff/complete", request, nil), http.StatusForbidden, "GRANT_USED")
}
