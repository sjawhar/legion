package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// TestApprovingADecidedMachineLoginAgainIsRecordTerminal pins that a machine login already
// decided answers any second decision by its approver 409 RECORD_TERMINAL, as an agent_secret
// record does, and one by another login 403 NOT_APPROVER, whatever the record's state. A second
// approve used to mint a second launcher credential before recording its event, hit the
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
	mallory := map[string]any{"approver": "mallory", "code": login.Code}
	for _, action := range []string{"approve", "deny"} {
		status, body := ts.ui(t, http.MethodPost, "/v1/credential-requests/"+looked.RecordID+"/"+action, mallory)
		if status != http.StatusForbidden || decode[wireError](t, body).Code != "NOT_APPROVER" {
			t.Fatalf("%s after approval as mallory = %d %s, want 403 NOT_APPROVER", action, status, body)
		}
	}
}

// TestConcurrentApprovesOfAMachineLoginDecideItOnce pins the concurrent half of a second
// decision: two approves of one pending machine login sent together answer exactly one 200 and
// one 409 RECORD_TERMINAL, and the record backs exactly one launcher credential. The second waits
// on the record's row lock and then finds the first's decision, rather than failing at the mint's
// live-key unique index with a 500.
func TestConcurrentApprovesOfAMachineLoginDecideItOnce(t *testing.T) {
	ts := newTestServer(t)
	machineKey := newSigningKey(t)
	compact := signMachineLoginRequest(t, machineKey, ts.URL, "sjawhar", "example-host-devbox")
	_, body := ts.req(t, http.MethodPost, "/v1/launcher-credentials", nil, map[string]any{"request": compact})
	login := decode[struct {
		Code string `json:"code"`
	}](t, body)
	_, body = ts.ui(t, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": login.Code})
	looked := decode[wireRecord](t, body)
	decision, err := json.Marshal(map[string]any{"approver": testApprover, "code": login.Code})
	if err != nil {
		t.Fatal(err)
	}

	type answer struct {
		status int
		body   []byte
		err    error
	}
	start := make(chan struct{})
	answers := make(chan answer, 2)
	for range 2 {
		go func() {
			<-start
			request, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/credential-requests/"+looked.RecordID+"/approve", bytes.NewReader(decision))
			if err != nil {
				answers <- answer{err: err}
				return
			}
			request.Header.Set("Authorization", "Bearer "+testUIToken)
			request.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(request)
			if err != nil {
				answers <- answer{err: err}
				return
			}
			defer resp.Body.Close()
			b, err := io.ReadAll(resp.Body)
			answers <- answer{status: resp.StatusCode, body: b, err: err}
		}()
	}
	close(start)

	statuses := map[int]int{}
	for range 2 {
		a := <-answers
		if a.err != nil {
			t.Fatalf("concurrent approve: %v", a.err)
		}
		statuses[a.status]++
		switch a.status {
		case http.StatusOK:
		case http.StatusConflict:
			if werr := decode[wireError](t, a.body); werr.Code != "RECORD_TERMINAL" {
				t.Fatalf("concurrent approve 409 code = %q, want RECORD_TERMINAL", werr.Code)
			}
		default:
			t.Fatalf("concurrent approve = %d, want 200 or 409: %s", a.status, a.body)
		}
	}
	if statuses[http.StatusOK] != 1 || statuses[http.StatusConflict] != 1 {
		t.Fatalf("concurrent approves answered %v, want one 200 and one 409", statuses)
	}
	var credentials int
	if err := ts.Store.Pool.QueryRow(context.Background(), `select count(*) from launcher_credentials where record_id=$1`, looked.RecordID).
		Scan(&credentials); err != nil || credentials != 1 {
		t.Fatalf("launcher credentials for the record = %d, %v, want 1", credentials, err)
	}
}

// TestApprovingASecondLoginOnAKeyWithALiveCredentialIsKeyHoldsLiveCredential pins the answer for
// two machine logins signed with one key: once the first is approved its key holds a live launcher
// credential, so approving the second is 409 KEY_HOLDS_LIVE_CREDENTIAL, not RECORD_TERMINAL, since
// the second record is still pending and stays so, with no credential minted for it.
func TestApprovingASecondLoginOnAKeyWithALiveCredentialIsKeyHoldsLiveCredential(t *testing.T) {
	ts := newTestServer(t)
	machineKey := newSigningKey(t)
	lookUp := func() (recordID, code string) {
		compact := signMachineLoginRequest(t, machineKey, ts.URL, "sjawhar", "example-host-devbox")
		_, body := ts.req(t, http.MethodPost, "/v1/launcher-credentials", nil, map[string]any{"request": compact})
		login := decode[struct {
			Code string `json:"code"`
		}](t, body)
		_, body = ts.ui(t, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": login.Code})
		return decode[wireRecord](t, body).RecordID, login.Code
	}
	firstID, firstCode := lookUp()
	secondID, secondCode := lookUp()

	if status, body := ts.ui(t, http.MethodPost, "/v1/credential-requests/"+firstID+"/approve",
		map[string]any{"approver": testApprover, "code": firstCode}); status != http.StatusOK {
		t.Fatalf("approve the first login = %d, want 200: %s", status, body)
	}
	status, body := ts.ui(t, http.MethodPost, "/v1/credential-requests/"+secondID+"/approve",
		map[string]any{"approver": testApprover, "code": secondCode})
	if status != http.StatusConflict || decode[wireError](t, body).Code != "KEY_HOLDS_LIVE_CREDENTIAL" {
		t.Fatalf("approve the second login on the same key = %d %s, want 409 KEY_HOLDS_LIVE_CREDENTIAL", status, body)
	}
	_, body = ts.ui(t, http.MethodGet, "/v1/credential-requests/"+secondID, nil)
	if read := decode[wireRecord](t, body); read.State != "pending" || read.Decided != nil {
		t.Fatalf("second login after the refused approve = %+v, want pending and undecided", read)
	}
	var credentials int
	if err := ts.Store.Pool.QueryRow(context.Background(), `select count(*) from launcher_credentials where record_id=$1`, secondID).
		Scan(&credentials); err != nil || credentials != 0 {
		t.Fatalf("launcher credentials for the second login = %d, %v, want 0", credentials, err)
	}
}
