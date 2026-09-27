package agentsecrets

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// capturedRequest snapshots what the client actually put on the wire, so a subtest can assert
// on method, path (including querystring), the UI bearer header, and the body byte-for-byte.
type capturedRequest struct {
	Method string
	Path   string
	Auth   string
	Body   []byte
}

// serveCapturing starts a broker double that records the inbound request and answers with the
// given status and body.
func serveCapturing(t *testing.T, status int, response string) (*httptest.Server, *capturedRequest) {
	t.Helper()
	captured := &capturedRequest{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		captured.Method = r.Method
		captured.Path = r.URL.RequestURI()
		captured.Auth = r.Header.Get("Authorization")
		captured.Body = body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(response))
	}))
	return server, captured
}

func TestPendingSendsBearerAndApproverQuery(t *testing.T) {
	server, captured := serveCapturing(t, http.StatusOK, `{"requests":[]}`)
	defer server.Close()

	body, err := New(server.URL, "ui-token").Pending(context.Background(), "sjawhar")
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if captured.Method != http.MethodGet || captured.Path != "/v1/pending?approver=sjawhar" {
		t.Fatalf("request = %s %s, want GET /v1/pending?approver=sjawhar", captured.Method, captured.Path)
	}
	if captured.Auth != "Bearer ui-token" {
		t.Fatalf("Authorization = %q, want Bearer ui-token", captured.Auth)
	}
	if string(body) != `{"requests":[]}` {
		t.Fatalf("body = %s, want the broker's response relayed verbatim", body)
	}
}

func TestRecordSendsBearerAndPath(t *testing.T) {
	server, captured := serveCapturing(t, http.StatusOK, `{"id":"req-1"}`)
	defer server.Close()

	if _, err := New(server.URL, "ui-token").Record(context.Background(), "req-1"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if captured.Method != http.MethodGet || captured.Path != "/v1/credential-requests/req-1" {
		t.Fatalf("request = %s %s, want GET /v1/credential-requests/req-1", captured.Method, captured.Path)
	}
	if captured.Auth != "Bearer ui-token" {
		t.Fatalf("Authorization = %q, want Bearer ui-token", captured.Auth)
	}
}

func TestApproveSendsBearerPathAndBodyByteIdentical(t *testing.T) {
	server, captured := serveCapturing(t, http.StatusOK, `{"ok":true}`)
	defer server.Close()

	requestBody := json.RawMessage(`{"note":"looks fine","ttl_seconds":300}`)
	if _, err := New(server.URL, "ui-token").Approve(context.Background(), "req-1", requestBody); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if captured.Method != http.MethodPost || captured.Path != "/v1/credential-requests/req-1/approve" {
		t.Fatalf("request = %s %s, want POST /v1/credential-requests/req-1/approve", captured.Method, captured.Path)
	}
	if captured.Auth != "Bearer ui-token" {
		t.Fatalf("Authorization = %q, want Bearer ui-token", captured.Auth)
	}
	if string(captured.Body) != string(requestBody) {
		t.Fatalf("relayed body = %s, want byte-identical %s", captured.Body, requestBody)
	}
}

func TestDenySendsBearerAndPath(t *testing.T) {
	server, captured := serveCapturing(t, http.StatusOK, `{"ok":true}`)
	defer server.Close()

	requestBody := json.RawMessage(`{"reason":"suspicious"}`)
	if _, err := New(server.URL, "ui-token").Deny(context.Background(), "req-1", requestBody); err != nil {
		t.Fatalf("Deny: %v", err)
	}
	if captured.Method != http.MethodPost || captured.Path != "/v1/credential-requests/req-1/deny" {
		t.Fatalf("request = %s %s, want POST /v1/credential-requests/req-1/deny", captured.Method, captured.Path)
	}
	if captured.Auth != "Bearer ui-token" {
		t.Fatalf("Authorization = %q, want Bearer ui-token", captured.Auth)
	}
	if string(captured.Body) != string(requestBody) {
		t.Fatalf("relayed body = %s, want byte-identical %s", captured.Body, requestBody)
	}
}

func TestMachineLookupSendsBearerAndPath(t *testing.T) {
	server, captured := serveCapturing(t, http.StatusOK, `{"login":"sjawhar"}`)
	defer server.Close()

	requestBody := json.RawMessage(`{"public_key":"ssh-ed25519 AAAA..."}`)
	if _, err := New(server.URL, "ui-token").MachineLookup(context.Background(), requestBody); err != nil {
		t.Fatalf("MachineLookup: %v", err)
	}
	if captured.Method != http.MethodPost || captured.Path != "/v1/machine-logins/lookup" {
		t.Fatalf("request = %s %s, want POST /v1/machine-logins/lookup", captured.Method, captured.Path)
	}
	if captured.Auth != "Bearer ui-token" {
		t.Fatalf("Authorization = %q, want Bearer ui-token", captured.Auth)
	}
	if string(captured.Body) != string(requestBody) {
		t.Fatalf("relayed body = %s, want byte-identical %s", captured.Body, requestBody)
	}
}

func TestKeysSendsBearerAndPath(t *testing.T) {
	server, captured := serveCapturing(t, http.StatusOK, `{"keys":[]}`)
	defer server.Close()

	if _, err := New(server.URL, "ui-token").Keys(context.Background(), "sjawhar"); err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if captured.Method != http.MethodGet || captured.Path != "/v1/approvers/sjawhar/keys" {
		t.Fatalf("request = %s %s, want GET /v1/approvers/sjawhar/keys", captured.Method, captured.Path)
	}
	if captured.Auth != "Bearer ui-token" {
		t.Fatalf("Authorization = %q, want Bearer ui-token", captured.Auth)
	}
}

func TestCeremonySendsBearerAndPathForEveryKindStepCombination(t *testing.T) {
	for _, combination := range []struct{ kind, step string }{
		{"register", "begin"}, {"register", "finish"}, {"endorse", "begin"}, {"endorse", "finish"},
	} {
		server, captured := serveCapturing(t, http.StatusOK, `{"ok":true}`)
		requestBody := json.RawMessage(`{"step":"data"}`)
		_, err := New(server.URL, "ui-token").Ceremony(context.Background(), "sjawhar", combination.kind, combination.step, requestBody)
		server.Close()
		if err != nil {
			t.Fatalf("Ceremony(%s,%s): %v", combination.kind, combination.step, err)
		}
		wantPath := "/v1/approvers/sjawhar/keys/" + combination.kind + "/" + combination.step
		if captured.Method != http.MethodPost || captured.Path != wantPath {
			t.Fatalf("request = %s %s, want POST %s", captured.Method, captured.Path, wantPath)
		}
		if captured.Auth != "Bearer ui-token" {
			t.Fatalf("Authorization = %q, want Bearer ui-token", captured.Auth)
		}
		if string(captured.Body) != string(requestBody) {
			t.Fatalf("relayed body = %s, want byte-identical %s", captured.Body, requestBody)
		}
	}
}

func TestGrantsSendsBearerAndApproverQuery(t *testing.T) {
	server, captured := serveCapturing(t, http.StatusOK, `{"grants":[]}`)
	defer server.Close()

	if _, err := New(server.URL, "ui-token").Grants(context.Background(), "sjawhar"); err != nil {
		t.Fatalf("Grants: %v", err)
	}
	if captured.Method != http.MethodGet || captured.Path != "/v1/grants?approver=sjawhar" {
		t.Fatalf("request = %s %s, want GET /v1/grants?approver=sjawhar", captured.Method, captured.Path)
	}
	if captured.Auth != "Bearer ui-token" {
		t.Fatalf("Authorization = %q, want Bearer ui-token", captured.Auth)
	}
}

func TestRevokeByApproverSendsBearerAndPath(t *testing.T) {
	server, captured := serveCapturing(t, http.StatusOK, `{"ok":true}`)
	defer server.Close()

	requestBody := json.RawMessage(`{"reason":"rotated"}`)
	if _, err := New(server.URL, "ui-token").RevokeByApprover(context.Background(), "grant-1", requestBody); err != nil {
		t.Fatalf("RevokeByApprover: %v", err)
	}
	if captured.Method != http.MethodPost || captured.Path != "/v1/grants/grant-1/revoke-by-approver" {
		t.Fatalf("request = %s %s, want POST /v1/grants/grant-1/revoke-by-approver", captured.Method, captured.Path)
	}
	if captured.Auth != "Bearer ui-token" {
		t.Fatalf("Authorization = %q, want Bearer ui-token", captured.Auth)
	}
	if string(captured.Body) != string(requestBody) {
		t.Fatalf("relayed body = %s, want byte-identical %s", captured.Body, requestBody)
	}
}

// A broker refusal must come back typed, with its status, code and full body preserved, so
// Task 2's proxy handlers can forward the broker's exact verdict to the browser rather than
// flattening it into a generic error string.
func TestNonTwoXXResponseReturnsTypedErrorWithStatusCodeAndBody(t *testing.T) {
	responseBody := `{"code":"ASSERTION_INVALID","error":"the client assertion failed verification"}`
	server, _ := serveCapturing(t, http.StatusForbidden, responseBody)
	defer server.Close()

	_, err := New(server.URL, "ui-token").Pending(context.Background(), "sjawhar")
	var brokerErr *Error
	if !errors.As(err, &brokerErr) {
		t.Fatalf("err = %v (%T), want *Error", err, err)
	}
	if brokerErr.Status != http.StatusForbidden {
		t.Fatalf("Status = %d, want %d", brokerErr.Status, http.StatusForbidden)
	}
	if brokerErr.Code != "ASSERTION_INVALID" {
		t.Fatalf("Code = %q, want ASSERTION_INVALID", brokerErr.Code)
	}
	if string(brokerErr.Body) != responseBody {
		t.Fatalf("Body = %s, want the broker's response preserved verbatim: %s", brokerErr.Body, responseBody)
	}
}

// A response body that doesn't parse as {"code": "..."} still preserves the raw bytes; only
// Code is left empty. Best-effort decoding must never turn an opaque broker failure into a
// dropped body.
func TestNonTwoXXResponseWithUnparsableBodyPreservesBodyWithEmptyCode(t *testing.T) {
	server, _ := serveCapturing(t, http.StatusInternalServerError, "not json at all")
	defer server.Close()

	_, err := New(server.URL, "ui-token").Pending(context.Background(), "sjawhar")
	var brokerErr *Error
	if !errors.As(err, &brokerErr) {
		t.Fatalf("err = %v (%T), want *Error", err, err)
	}
	if brokerErr.Status != http.StatusInternalServerError {
		t.Fatalf("Status = %d, want 500", brokerErr.Status)
	}
	if brokerErr.Code != "" {
		t.Fatalf("Code = %q, want empty for an unparsable body", brokerErr.Code)
	}
	if string(brokerErr.Body) != "not json at all" {
		t.Fatalf("Body = %s, want the raw bytes preserved", brokerErr.Body)
	}
}

// A transport failure (here: nothing listening) must never surface as *Error — Task 2 treats
// anything that is not *agentsecrets.Error as 503 AGENT_SECRETS_UNAVAILABLE, and a transport
// failure wrongly typed as *Error would forward a fabricated status/code to the browser.
func TestTransportFailureIsNotATypedError(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()

	_, err := New(closed.URL, "ui-token").Pending(context.Background(), "sjawhar")
	if err == nil {
		t.Fatal("Pending against a closed listener: got nil error")
	}
	var brokerErr *Error
	if errors.As(err, &brokerErr) {
		t.Fatalf("err = %v, want a plain transport error, not *Error", err)
	}
}
