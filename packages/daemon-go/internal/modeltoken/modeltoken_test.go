package modeltoken

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeCognito answers the two calls of Cognito's custom authentication: InitiateAuth with
// CUSTOM_AUTH, then RespondToAuthChallenge with the pod's service-account token as the answer.
type fakeCognito struct {
	t      *testing.T
	mu     sync.Mutex
	calls  []string
	signIn int
	refuse bool
}

func (f *fakeCognito) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Errorf("decode request: %v", err)
	}
	target := strings.TrimPrefix(r.Header.Get("X-Amz-Target"), "AWSCognitoIdentityProviderService.")
	f.mu.Lock()
	f.calls = append(f.calls, target)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	if body["ClientId"] != "client-id" {
		f.t.Errorf("%s ClientId = %#v", target, body["ClientId"])
	}
	switch target {
	case "InitiateAuth":
		parameters, _ := body["AuthParameters"].(map[string]any)
		if body["AuthFlow"] != "CUSTOM_AUTH" || parameters["USERNAME"] != "machine-user" || len(parameters) != 1 {
			f.t.Errorf("InitiateAuth = %#v, want CUSTOM_AUTH with USERNAME only", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ChallengeName": "CUSTOM_CHALLENGE", "Session": "session-1",
			"ChallengeParameters": map[string]string{"USERNAME": "machine-user"},
		})
	case "RespondToAuthChallenge":
		responses, _ := body["ChallengeResponses"].(map[string]any)
		if body["ChallengeName"] != "CUSTOM_CHALLENGE" || body["Session"] != "session-1" ||
			responses["USERNAME"] != "machine-user" || responses["ANSWER"] != "pod-service-account-token" {
			f.t.Errorf("RespondToAuthChallenge = %#v, want the pod's token as the custom-challenge answer", body)
		}
		if f.refuse {
			w.Header().Set("X-Amzn-ErrorType", "NotAuthorizedException")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"__type": "NotAuthorizedException", "message": "Incorrect username or password."})
			return
		}
		f.mu.Lock()
		f.signIn++
		token := fmt.Sprintf("access-%d", f.signIn)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"AuthenticationResult": map[string]any{
			"AccessToken": token, "RefreshToken": "refresh-never-kept", "IdToken": "id-never-kept",
			"ExpiresIn": 3600, "TokenType": "Bearer",
		}})
	default:
		f.t.Errorf("unexpected Cognito call %q", target)
		w.WriteHeader(http.StatusBadRequest)
	}
}

func (f *fakeCognito) signIns() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.signIn
}

func testConfig(t *testing.T, endpoint string) Config {
	t.Helper()
	serviceAccountToken := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(serviceAccountToken, []byte("pod-service-account-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return Config{
		Region: "example-region-1", ClientID: "client-id", Username: "machine-user",
		ServiceAccountTokenFile: serviceAccountToken,
		Endpoint:                endpoint,
	}
}

// The fake checks that the custom challenge's ANSWER is the pod's service-account token; Token
// returns the access token alone, never the refresh or ID token Cognito also returns.
func TestTokenAnswersTheChallengeWithThePodsTokenAndReturnsOnlyTheAccessToken(t *testing.T) {
	server := httptest.NewServer(&fakeCognito{t: t})
	defer server.Close()
	token, err := Token(context.Background(), testConfig(t, server.URL))
	if err != nil || token != "access-1" {
		t.Fatalf("Token = %q, %v; want access-1", token, err)
	}
}

// Oh My Pi re-runs the command only after a 401, so every run must sign in afresh rather than hand
// back a token a gateway just refused.
func TestTokenSignsInOnEveryRun(t *testing.T) {
	cognito := &fakeCognito{t: t}
	server := httptest.NewServer(cognito)
	defer server.Close()
	cfg := testConfig(t, server.URL)

	for _, want := range []string{"access-1", "access-2"} {
		if token, err := Token(context.Background(), cfg); err != nil || token != want {
			t.Fatalf("Token = %q, %v; want %s", token, err, want)
		}
	}
	if cognito.signIns() != 2 {
		t.Fatalf("two runs made %d sign-ins, want 2", cognito.signIns())
	}
}

func TestTokenRefusesAFailedSignInWithoutAToken(t *testing.T) {
	server := httptest.NewServer(&fakeCognito{t: t, refuse: true})
	defer server.Close()

	token, err := Token(context.Background(), testConfig(t, server.URL))
	if err == nil || token != "" || !strings.Contains(err.Error(), "NotAuthorizedException") {
		t.Fatalf("Token = %q, %v; want no token and Cognito's refusal", token, err)
	}
}

func TestConfigRefusesAMissingSetting(t *testing.T) {
	cfg := testConfig(t, "")
	cfg.Username = ""
	if _, err := Token(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "username") {
		t.Fatalf("Token = %v, want the missing username named", err)
	}
}
