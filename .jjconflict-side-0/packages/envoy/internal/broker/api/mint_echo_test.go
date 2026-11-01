package api_test

import (
	"bufio"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// TestAMintFailureEchoingTheRefreshTokenReachesNeitherTheSessionNorTheLog: the real Google login
// minter, against a token endpoint that echoes the refresh token in a status line Go's HTTP client
// refuses, answers the session 502 MINT_FAILED, and neither that answer nor the broker's mint
// failure line quotes the refresh token.
func TestAMintFailureEchoingTheRefreshTokenReachesNeitherTheSessionNorTheLog(t *testing.T) {
	const canary = "refresh-token-canary-77d2"
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				request, err := http.ReadRequest(bufio.NewReader(conn))
				if err != nil || request.ParseForm() != nil {
					return
				}
				_, _ = conn.Write([]byte("HTTP/1.1 " + request.PostForm.Get("refresh_token") + " OK\r\nContent-Length: 0\r\n\r\n"))
			}()
		}
	}()
	logged := policytest.CaptureLog(t)
	ts := newTestServerWith(t, func(d *api.Deps) {
		d.Machine.Minters = map[string]requests.Minter{
			policy.KindGoogleLogin: secrets.GoogleLogin{TokenURL: "http://" + listener.Addr().String()}.Mint,
		}
	})
	value := `{"type":"authorized_user","client_id":"client-id","client_secret":"client-secret","refresh_token":"` + canary + `"}`
	ts.Secrets.Put(policytest.KindSecret("ECHOED_LOGIN", testApprover, policy.TierAgent, policy.KindGoogleLogin, value))
	if status, body := ts.req(t, http.MethodPost, "/v1/secrets/ECHOED_LOGIN/reread", nil, nil); status != http.StatusOK {
		t.Fatalf("reread ECHOED_LOGIN = %d %s", status, body)
	}
	enrollmentID, sessionKey := ts.newSessionEnrollment(t, "session-"+t.Name(), testApprover)
	_, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": signAgentSecretRequest(t, sessionKey, ts.URL, "read login", "ECHOED_LOGIN"), "session_id": nil})
	created := decode[wireCreateRequestResponse](t, body)
	if created.GrantID == nil {
		t.Fatalf("POST /v1/requests = %s, want an automatic grant", body)
	}
	status, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/grants/"+*created.GrantID+"/values", nil)
	if status != http.StatusBadGateway || decode[wireError](t, body).Code != "MINT_FAILED" {
		t.Fatalf("POST values = %d %s, want 502 MINT_FAILED", status, body)
	}
	if strings.Contains(string(body), canary) {
		t.Fatal("the session's MINT_FAILED answer quotes the refresh token")
	}
	if !strings.Contains(logged.String(), requests.MintFailedMessage) {
		t.Fatalf("no %q line was logged", requests.MintFailedMessage)
	}
	if strings.Contains(logged.String(), canary) {
		t.Fatal("the broker's log quotes the refresh token")
	}
}
