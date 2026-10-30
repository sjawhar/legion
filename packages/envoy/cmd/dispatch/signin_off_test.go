package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/routes"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A deployment with no DISPATCH_SIGNIN_* setting — a sandbox, which has no sign-in client —
// boots, serves its agent-token API, and tells a browser that sign-in is not configured. One
// setting on its own is still half a sign-in, and refuses the boot naming the rest.
func TestABootWithNoSignInServesAgentsAndRefusesBrowserSignIn(t *testing.T) {
	boot, err := resolveBootConfig(envGetter(noSignInEnvironment(nil)))
	if err != nil || boot.SignInIssuer != "" {
		t.Fatalf("no sign-in setting: SignInIssuer = %q, err = %v, want a boot with no sign-in", boot.SignInIssuer, err)
	}

	handler := routerFor(t, boot, routes.AppContextOptions{Store: storetest.Open(t)})
	if response := whoamiAs(handler, "agent-token", ""); !answersSharedToken(response) {
		t.Errorf("GET /api/v1/whoami with the agent token answered %d %s, want 200 naming an agent", response.Code, response.Body.String())
	}

	signIn := httptest.NewRecorder()
	handler.ServeHTTP(signIn, httptest.NewRequest(http.MethodGet, "http://dispatch.test/auth/start", nil))
	if signIn.Code != http.StatusServiceUnavailable || errorCode(signIn) != "SIGNIN_UNCONFIGURED" {
		t.Errorf("GET /auth/start answered %d %s, want 503 SIGNIN_UNCONFIGURED", signIn.Code, signIn.Body.String())
	}

	_, err = resolveBootConfig(envGetter(noSignInEnvironment(map[string]string{"DISPATCH_SIGNIN_CLIENT_ID": "dispatch-client"})))
	const missing = "DISPATCH_SIGNIN_CLIENT_ID set, DISPATCH_SIGNIN_ISSUER, DISPATCH_SIGNIN_CLIENT_SECRET, DISPATCH_SIGNIN_GROUP or DISPATCH_SIGNIN_DOMAIN missing"
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Errorf("DISPATCH_SIGNIN_CLIENT_ID alone: err = %v, want a refusal naming %q", err, missing)
	}
}
