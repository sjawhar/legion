package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/credential"
)

func TestIssueStatusUsesAControllerGrantForSynchronousDispatch(t *testing.T) {
	h, _, statuses := newArchitectHarness(t, nil, nil)
	grant := controllerGrant(t, h)
	recorder := h.request(http.MethodPost, "/legion/v1/issues/status", IssueStatusRequest{
		GrantID: grant.GrantID, Issue: "LEGION-208", Status: "todo",
	}, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("issue status = %d: %s", recorder.Code, recorder.Body)
	}
	var response IssueStatusResponse
	decodeInto(t, recorder, &response)
	if len(statuses.writes) != 1 || statuses.writes[0] != (statusWrite{issue: "LEGION-208", status: "todo"}) {
		t.Fatalf("Dispatch writes = %#v", statuses.writes)
	}
}

func TestIssueStatusReportsAnExpiredControllerGrant(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	h, _, _ := newArchitectHarness(t, credential.New(func() time.Time { return now }), nil)
	grant := controllerGrant(t, h)
	now = now.Add(time.Minute)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/issues/status", IssueStatusRequest{
		GrantID: grant.GrantID, Issue: "LEGION-208", Status: "todo",
	}, nil), http.StatusForbidden, "GRANT_EXPIRED")
}
