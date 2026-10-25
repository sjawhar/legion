package agentsecrets

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// capturedRequest snapshots what the client actually put on the wire, so a subtest can assert
// on method, path (including querystring), the bearer header carrying Dispatch's ServiceAccount
// token, and the body byte-for-byte.
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

// tokenFile writes token to a file of the test's own directory and answers its path: the
// projected ServiceAccount token file a client reads its bearer from.
func tokenFile(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatalf("write the token file: %v", err)
	}
	return path
}

func TestPendingSendsBearerAndApproverQuery(t *testing.T) {
	server, captured := serveCapturing(t, http.StatusOK, `{"requests":[]}`)
	defer server.Close()

	body, err := New(server.URL, tokenFile(t, "account-token")).Pending(context.Background(), "sjawhar")
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if captured.Method != http.MethodGet || captured.Path != "/v1/pending?approver=sjawhar" {
		t.Fatalf("request = %s %s, want GET /v1/pending?approver=sjawhar", captured.Method, captured.Path)
	}
	if captured.Auth != "Bearer account-token" {
		t.Fatalf("Authorization = %q, want Bearer account-token", captured.Auth)
	}
	if string(body) != `{"requests":[]}` {
		t.Fatalf("body = %s, want the broker's response relayed verbatim", body)
	}
}

// Approve and Deny send exactly the Decision Dispatch built — the approver and, for a machine
// login, the code — as the broker's strict decideBody reads it; a decision with no code carries
// no code field at all.
func TestDecisionsSendBearerPathAndTheApproverDispatchNames(t *testing.T) {
	code := "ABCD-1234"
	for _, tc := range []struct {
		name, wantPath, wantBody string
		decide                   func(*Client) (json.RawMessage, error)
	}{
		{"approve", "/v1/credential-requests/req-1/approve", `{"approver":"sjawhar"}`, func(c *Client) (json.RawMessage, error) {
			return c.Approve(context.Background(), "req-1", Decision{Approver: "sjawhar"})
		}},
		{"deny with code", "/v1/credential-requests/req-1/deny", `{"approver":"sjawhar","code":"ABCD-1234"}`, func(c *Client) (json.RawMessage, error) {
			return c.Deny(context.Background(), "req-1", Decision{Approver: "sjawhar", Code: &code})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, captured := serveCapturing(t, http.StatusOK, `{"ok":true}`)
			defer server.Close()
			if _, err := tc.decide(New(server.URL, tokenFile(t, "account-token"))); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if captured.Method != http.MethodPost || captured.Path != tc.wantPath {
				t.Fatalf("request = %s %s, want POST %s", captured.Method, captured.Path, tc.wantPath)
			}
			if captured.Auth != "Bearer account-token" {
				t.Fatalf("Authorization = %q, want Bearer account-token", captured.Auth)
			}
			if string(captured.Body) != tc.wantBody {
				t.Fatalf("body = %s, want %s", captured.Body, tc.wantBody)
			}
		})
	}
}

func TestMachineLookupSendsBearerAndPath(t *testing.T) {
	server, captured := serveCapturing(t, http.StatusOK, `{"login":"sjawhar"}`)
	defer server.Close()

	requestBody := json.RawMessage(`{"public_key":"ssh-ed25519 AAAA..."}`)
	if _, err := New(server.URL, tokenFile(t, "account-token")).MachineLookup(context.Background(), requestBody); err != nil {
		t.Fatalf("MachineLookup: %v", err)
	}
	if captured.Method != http.MethodPost || captured.Path != "/v1/machine-logins/lookup" {
		t.Fatalf("request = %s %s, want POST /v1/machine-logins/lookup", captured.Method, captured.Path)
	}
	if captured.Auth != "Bearer account-token" {
		t.Fatalf("Authorization = %q, want Bearer account-token", captured.Auth)
	}
	if string(captured.Body) != string(requestBody) {
		t.Fatalf("relayed body = %s, want byte-identical %s", captured.Body, requestBody)
	}
}

func TestGrantsSendsBearerAndApproverQuery(t *testing.T) {
	server, captured := serveCapturing(t, http.StatusOK, `{"grants":[]}`)
	defer server.Close()

	if _, err := New(server.URL, tokenFile(t, "account-token")).Grants(context.Background(), "sjawhar"); err != nil {
		t.Fatalf("Grants: %v", err)
	}
	if captured.Method != http.MethodGet || captured.Path != "/v1/grants?approver=sjawhar" {
		t.Fatalf("request = %s %s, want GET /v1/grants?approver=sjawhar", captured.Method, captured.Path)
	}
	if captured.Auth != "Bearer account-token" {
		t.Fatalf("Authorization = %q, want Bearer account-token", captured.Auth)
	}
}

func TestRevokeByApproverSendsBearerPathAndTheApprover(t *testing.T) {
	server, captured := serveCapturing(t, http.StatusOK, `{"ok":true}`)
	defer server.Close()

	if _, err := New(server.URL, tokenFile(t, "account-token")).RevokeByApprover(context.Background(), "grant-1", "sjawhar"); err != nil {
		t.Fatalf("RevokeByApprover: %v", err)
	}
	if captured.Method != http.MethodPost || captured.Path != "/v1/grants/grant-1/revoke-by-approver" {
		t.Fatalf("request = %s %s, want POST /v1/grants/grant-1/revoke-by-approver", captured.Method, captured.Path)
	}
	if captured.Auth != "Bearer account-token" {
		t.Fatalf("Authorization = %q, want Bearer account-token", captured.Auth)
	}
	if string(captured.Body) != `{"approver":"sjawhar"}` {
		t.Fatalf("body = %s, want {\"approver\":\"sjawhar\"}", captured.Body)
	}
}

// A broker refusal must come back typed, with its status, code and full body preserved, so
// Task 2's proxy handlers can forward the broker's exact verdict to the browser rather than
// flattening it into a generic error string.
func TestNonTwoXXResponseReturnsTypedErrorWithStatusCodeAndBody(t *testing.T) {
	responseBody := `{"code":"NOT_APPROVER","error":"only the record's approver may decide it"}`
	server, _ := serveCapturing(t, http.StatusForbidden, responseBody)
	defer server.Close()

	_, err := New(server.URL, tokenFile(t, "account-token")).Pending(context.Background(), "sjawhar")
	var brokerErr *Error
	if !errors.As(err, &brokerErr) {
		t.Fatalf("err = %v (%T), want *Error", err, err)
	}
	if brokerErr.Status != http.StatusForbidden {
		t.Fatalf("Status = %d, want %d", brokerErr.Status, http.StatusForbidden)
	}
	if brokerErr.Code != "NOT_APPROVER" {
		t.Fatalf("Code = %q, want NOT_APPROVER", brokerErr.Code)
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

	_, err := New(server.URL, tokenFile(t, "account-token")).Pending(context.Background(), "sjawhar")
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

	_, err := New(closed.URL, tokenFile(t, "account-token")).Pending(context.Background(), "sjawhar")
	if err == nil {
		t.Fatal("Pending against a closed listener: got nil error")
	}
	var brokerErr *Error
	if errors.As(err, &brokerErr) {
		t.Fatalf("err = %v, want a plain transport error, not *Error", err)
	}
}

// A projected token is rewritten in place by the kubelet, so the client sends what the file
// holds now: the copy it cached stands until the cache lapses, and the next call after that
// reads the file again.
func TestARewrittenTokenFileReachesTheBrokerOnceTheCacheLapses(t *testing.T) {
	server, captured := serveCapturing(t, http.StatusOK, `{"requests":[]}`)
	defer server.Close()
	path := tokenFile(t, "first-token")
	client := New(server.URL, path)

	if _, err := client.Pending(context.Background(), "sjawhar"); err != nil {
		t.Fatalf("the first call: %v", err)
	}
	if captured.Auth != "Bearer first-token" {
		t.Fatalf("Authorization = %q, want Bearer first-token", captured.Auth)
	}
	if err := os.WriteFile(path, []byte("second-token\n"), 0o600); err != nil {
		t.Fatalf("rewrite the token file: %v", err)
	}
	if _, err := client.Pending(context.Background(), "sjawhar"); err != nil {
		t.Fatalf("the call inside the cache window: %v", err)
	}
	if captured.Auth != "Bearer first-token" {
		t.Fatalf("Authorization = %q within %v of the read, want the cached Bearer first-token", captured.Auth, tokenCacheTTL)
	}

	client.tokenTTL = 0
	if _, err := client.Pending(context.Background(), "sjawhar"); err != nil {
		t.Fatalf("the call after the cache lapsed: %v", err)
	}
	if captured.Auth != "Bearer second-token" {
		t.Fatalf("Authorization = %q, want the rewritten Bearer second-token, trimmed", captured.Auth)
	}
}

// A token file Dispatch cannot read, or that holds nothing, fails the call naming the file and
// sends the broker nothing: an empty bearer would be refused there as an unauthenticated caller,
// hiding Dispatch's own misconfiguration behind the broker's verdict.
func TestAMissingOrEmptyTokenFileFailsNamingItAndSendsNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		path func(t *testing.T) string
	}{
		{"missing", func(t *testing.T) string { return filepath.Join(t.TempDir(), "nothing-here") }},
		{"empty", func(t *testing.T) string { return tokenFile(t, "  \n") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, captured := serveCapturing(t, http.StatusOK, `{"requests":[]}`)
			defer server.Close()
			path := tc.path(t)

			_, err := New(server.URL, path).Pending(context.Background(), "sjawhar")
			if err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("Pending with a %s token file: err = %v, want a refusal naming %s", tc.name, err, path)
			}
			var brokerErr *Error
			if errors.As(err, &brokerErr) {
				t.Fatalf("err = %v, want a plain error, not a broker verdict", err)
			}
			if captured.Method != "" {
				t.Fatalf("the broker received %s %s, want no request at all", captured.Method, captured.Path)
			}
		})
	}
}
