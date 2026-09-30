package api_test

import (
	"net/http"
	"testing"
)

// TestApprovingADecidedMachineLoginAgainIsRecordTerminal pins that a machine login already
// decided answers any second decision 409 RECORD_TERMINAL, as an agent_secret record does. A
// second approve used to mint a second launcher credential before recording its event, hit the
// live-credential unique key, and answer 500 INTERNAL: the path a second click on Dispatch's
// machine-login page takes, since that page keeps the looked-up record, and its Approve button,
// after the first approval lands.
func TestApprovingADecidedMachineLoginAgainIsRecordTerminal(t *testing.T) {
	ts := newTestServer(t)
	machineKey := newSigningKey(t)
	compact := signMachineLoginRequest(t, machineKey, ts.URL, "sjawhar", "example-host-devbox")
	_, body := ts.req(t, http.MethodPost, "/v1/launcher-credentials", nil, map[string]any{"request": compact})
	login := decode[struct {
		PendingID string `json:"pending_id"`
		Code      string `json:"code"`
	}](t, body)
	_, body = ts.ui(t, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": login.Code})
	looked := decode[wireRecord](t, body)
	decision := map[string]any{"approver": testApprover, "code": login.Code}

	if status, body := ts.ui(t, http.MethodPost, "/v1/credential-requests/"+looked.RecordID+"/approve", decision); status != http.StatusOK {
		t.Fatalf("first approve = %d, want 200: %s", status, body)
	}
	for _, action := range []string{"approve", "deny"} {
		status, body := ts.ui(t, http.MethodPost, "/v1/credential-requests/"+looked.RecordID+"/"+action, decision)
		if status != http.StatusConflict {
			t.Fatalf("%s after approval = %d, want 409: %s", action, status, body)
		}
		if werr := decode[wireError](t, body); werr.Code != "RECORD_TERMINAL" {
			t.Fatalf("%s after approval: code = %q, want RECORD_TERMINAL", action, werr.Code)
		}
	}
}
