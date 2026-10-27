package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// issuerDownAudience is the audience the process verifies pods' service-account tokens for.
const issuerDownAudience = "dispatch"

// discoveryRetryWindow is how long a test waits for a process to discover an issuer that has begun
// to answer: the background loop's longest wait between attempts (thirty seconds) and one attempt's
// bound (ten seconds), with a margin.
const discoveryRetryWindow = 45 * time.Second

// A process started while both its sign-in issuer and its service-account token issuer are down
// starts and serves: /healthz answers 200 with `signin` and `service_tokens` false, a shared agent
// token is served, a pod's JWT is answered 503 OIDC_ISSUER_UNAVAILABLE (never 401: the token was
// not judged), and /auth/start 503 SIGNIN_UNAVAILABLE. Once the issuers answer, the process
// discovers both in the background with no restart: /healthz reports both true, the JWT is served,
// and a person signs in. Then a Google outage at the token endpoint does not sign that person out:
// past the hour their page and API requests are answered 503 with the same cookie, and 200 again
// once the issuer answers. Before INFRA-555 the boot exited when either discovery failed.
func TestAProcessStartedWhileTheSignInIssuerIsDownServesAgentTokensAndSignsInLater(t *testing.T) {
	database := storetest.Open(t)
	signInIssuer := oidctest.New(t)
	signInIssuer.EnableCodeFlow(signInIssuer.PublishKey(t, "signing-key"), cookieClientID, cookieClientSecret)
	signInIssuer.FailDiscovery(http.StatusServiceUnavailable)
	podIssuer := oidctest.New(t)
	podKey := podIssuer.PublishKey(t, "pod-key")
	podIssuer.FailDiscovery(http.StatusServiceUnavailable)
	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{
		SigningKey: "issuer-down-signing-key",
		Env: map[string]string{
			"DISPATCH_IDENTITY":             "cookie",
			"DISPATCH_SIGNIN_ISSUER":        signInIssuer.URL(),
			"DISPATCH_SIGNIN_CLIENT_ID":     cookieClientID,
			"DISPATCH_SIGNIN_CLIENT_SECRET": cookieClientSecret,
			"DISPATCH_SIGNIN_GROUP":         cookieGroup,
			"DISPATCH_INSECURE_COOKIE":      "1",
			"DISPATCH_OIDC_ISSUER":          podIssuer.URL(),
			"DISPATCH_OIDC_AUDIENCE":        issuerDownAudience,
		},
	})
	process.waitHealthy(t)

	if status, health := process.health(t); status != http.StatusOK || !health.OK || health.SignIn == nil || *health.SignIn || health.ServiceTokens == nil || *health.ServiceTokens {
		t.Fatalf("/healthz while both issuers are down = %d %+v, want 200 ok with signin and service_tokens false", status, health)
	}
	if status, body := bearerWhoami(t, process, "shutdown-test-token"); status != http.StatusOK || !strings.Contains(body, `"kind":"agent"`) {
		t.Fatalf("the shared agent token while both issuers are down: %d %s, want 200 as an agent", status, body)
	}
	const podSubject = "system:serviceaccount:legion:legion-worker"
	podToken := podIssuer.Mint(t, podKey, podIssuer.Claims(podSubject, issuerDownAudience))
	if status, body := bearerWhoami(t, process, podToken); status != http.StatusServiceUnavailable || errorCodeOf(body) != "OIDC_ISSUER_UNAVAILABLE" {
		t.Fatalf("a pod's JWT while its issuer is down: %d %s, want 503 OIDC_ISSUER_UNAVAILABLE", status, body)
	}
	started := browserGet(t, process.url("/auth/start"), nil)
	body, _ := io.ReadAll(started.Body)
	_ = started.Body.Close()
	if started.StatusCode != http.StatusServiceUnavailable || errorCodeOf(string(body)) != "SIGNIN_UNAVAILABLE" {
		t.Fatalf("/auth/start while the sign-in issuer is down: %d %s, want 503 SIGNIN_UNAVAILABLE", started.StatusCode, body)
	}
	process.waitForLog(t, 30*time.Second, `msg="oidc: discover the issuer`, "issuer="+signInIssuer.URL())

	signInIssuer.FailDiscovery(0)
	podIssuer.FailDiscovery(0)
	process.waitForHealth(t, discoveryRetryWindow, "signin and service_tokens true", func(health healthBody) bool {
		return health.OK && health.SignIn != nil && *health.SignIn && health.ServiceTokens != nil && *health.ServiceTokens
	})
	if status, body := bearerWhoami(t, process, podToken); status != http.StatusOK || !strings.Contains(body, podSubject) {
		t.Fatalf("a pod's JWT once its issuer answers: %d %s, want 200 naming %s", status, body, podSubject)
	}
	session := signInThrough(t, process, signInIssuer)
	if answer, status := whoami(t, process, session); status != http.StatusOK || !strings.Contains(answer, cookiePerson) {
		t.Fatalf("whoami after the sign-in: %d %s, want %s", status, answer, cookiePerson)
	}

	// The token endpoint goes down and the person's confirmation lapses: the page route and the API
	// answer 503 with the same cookie, which is not ended, and 200 once the issuer answers.
	if _, err := database.Pool.Exec(context.Background(), `update people set confirmed_at = now() - interval '2 hours' where email = $1`, cookiePerson); err != nil {
		t.Fatalf("date the confirmation two hours back: %v", err)
	}
	signInIssuer.FailToken(http.StatusServiceUnavailable, "")
	if answer, status := whoami(t, process, session); status != http.StatusServiceUnavailable || errorCodeOf(answer) != "SIGNIN_UNAVAILABLE" {
		t.Fatalf("/auth/whoami past the hour while the token endpoint answers 503: %d %s, want 503 SIGNIN_UNAVAILABLE", status, answer)
	}
	if answer, status := cookieGet(t, process, "/api/v1/whoami", session); status != http.StatusServiceUnavailable || errorCodeOf(answer) != "SIGNIN_UNAVAILABLE" {
		t.Fatalf("/api/v1/whoami past the hour while the token endpoint answers 503: %d %s, want 503 SIGNIN_UNAVAILABLE", status, answer)
	}
	signInIssuer.FailToken(0, "")
	if answer, status := whoami(t, process, session); status != http.StatusOK || !strings.Contains(answer, cookiePerson) {
		t.Fatalf("/auth/whoami with the same cookie once the issuer answers: %d %s, want 200 naming %s", status, answer, cookiePerson)
	}
	if answer, status := cookieGet(t, process, "/api/v1/whoami", session); status != http.StatusOK || !strings.Contains(answer, cookiePerson) {
		t.Fatalf("/api/v1/whoami with the same cookie once the issuer answers: %d %s, want 200 naming %s", status, answer, cookiePerson)
	}
}

// bearerWhoami asks the process's API who token's bearer is, and answers the status and body.
func bearerWhoami(t *testing.T, p *dispatchProcess, token string) (int, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, p.url("/api/v1/whoami"), nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := (&http.Client{Timeout: 20 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("GET /api/v1/whoami: %v", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(body)
}

// cookieGet is GET path on the process from a browser holding the session cookie, answering its
// body and status.
func cookieGet(t *testing.T, p *dispatchProcess, path string, session *http.Cookie) (string, int) {
	t.Helper()
	response := browserGet(t, p.url(path), session)
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return strings.TrimSpace(string(body)), response.StatusCode
}

// errorCodeOf is the code of a JSON error body, empty when it names none.
func errorCodeOf(body string) string {
	var answer struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal([]byte(body), &answer)
	return answer.Code
}

// ───── shared with multiprocess_test.go ─────────────────────────────────────

// The sign-in client the fake issuer serves, and the group it admits.
const (
	cookieClientID     = "dispatch-multiprocess-client"
	cookieClientSecret = "dispatch-multiprocess-secret"
	cookieGroup        = "dispatch-members"
	cookiePerson       = "alice@example.test"
)

// signInThrough signs cookiePerson in on the process, the whole code flow a browser walks:
// /auth/start for the issuer's authorization URL and the state cookie, the issuer's own sign-in,
// then /auth/callback, which answers the session cookie. It returns that cookie.
func signInThrough(t *testing.T, p *dispatchProcess, issuer *oidctest.Issuer) *http.Cookie {
	t.Helper()
	issuer.SignInAs(map[string]any{
		"sub":              "alice-subject",
		"cognito:username": "ExampleIdP_" + cookiePerson,
		"cognito:groups":   []string{cookieGroup},
		"identities":       []map[string]any{{"providerName": "ExampleIdP"}},
	})
	started := browserGet(t, p.url("/auth/start"), nil)
	defer started.Body.Close()
	if started.StatusCode != http.StatusFound {
		t.Fatalf("/auth/start answered %d, want a redirect to the issuer", started.StatusCode)
	}
	var state *http.Cookie
	for _, cookie := range started.Cookies() {
		if cookie.Name == "dispatch_oauth_state" {
			state = cookie
		}
	}
	if state == nil {
		t.Fatalf("/auth/start set no sign-in state cookie: %v", started.Cookies())
	}
	code, returned := issuer.Authorize(t, started.Header.Get("Location"))
	callback := browserGet(t, p.url("/auth/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(returned)), state)
	defer callback.Body.Close()
	for _, cookie := range callback.Cookies() {
		if cookie.Name == "dsession" && cookie.Value != "" {
			return cookie
		}
	}
	body, _ := io.ReadAll(callback.Body)
	t.Fatalf("/auth/callback answered %d and set no session cookie: %s", callback.StatusCode, body)
	return nil
}

// browserGet is one GET a browser makes during a sign-in: it sends cookie when there is one and
// never follows the redirect, since the redirect's own response is where the cookie is set.
func browserGet(t *testing.T, target string, cookie *http.Cookie) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("build %s: %v", target, err)
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	return response
}

// whoami asks the process who the session cookie names, and answers its body and status.
func whoami(t *testing.T, p *dispatchProcess, session *http.Cookie) (string, int) {
	t.Helper()
	return cookieGet(t, p, "/auth/whoami", session)
}
