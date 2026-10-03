package modeltoken

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

func (f *fakeCognito) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func testConfig(t *testing.T, endpoint string, now *time.Time) Config {
	t.Helper()
	dir := t.TempDir()
	serviceAccountToken := filepath.Join(dir, "token")
	if err := os.WriteFile(serviceAccountToken, []byte("pod-service-account-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return Config{
		Region: "example-region-1", ClientID: "client-id", Username: "machine-user",
		ServiceAccountTokenFile: serviceAccountToken,
		CacheFile:               filepath.Join(dir, "state", "model-token"),
		Endpoint:                endpoint,
		Now:                     func() time.Time { return *now },
	}
}

func TestTokenSignsInWithThePodsTokenAndCachesOnlyTheAccessToken(t *testing.T) {
	cognito := &fakeCognito{t: t}
	server := httptest.NewServer(cognito)
	defer server.Close()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	cfg := testConfig(t, server.URL, &now)
	if err := os.MkdirAll(filepath.Dir(cfg.CacheFile), 0o700); err != nil {
		t.Fatal(err)
	}

	token, err := Token(context.Background(), cfg)
	if err != nil || token != "access-1" {
		t.Fatalf("Token = %q, %v; want access-1", token, err)
	}
	info, err := os.Stat(cfg.CacheFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("cache mode = %o, want 600", info.Mode().Perm())
	}
	cached, err := os.ReadFile(cfg.CacheFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, kept := range []string{"refresh-never-kept", "id-never-kept", "pod-service-account-token"} {
		if strings.Contains(string(cached), kept) {
			t.Fatalf("cache %s holds %q; only the access token may be kept", cached, kept)
		}
	}
	if entries, _ := os.ReadDir(filepath.Dir(cfg.CacheFile)); len(entries) != 1 {
		t.Fatalf("state directory holds %d entries, want the cache alone (no temporary file left)", len(entries))
	}
}

func TestTokenUsesTheCacheUntilShortlyBeforeExpiryThenSignsInAgain(t *testing.T) {
	cognito := &fakeCognito{t: t}
	server := httptest.NewServer(cognito)
	defer server.Close()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	cfg := testConfig(t, server.URL, &now)
	if err := os.MkdirAll(filepath.Dir(cfg.CacheFile), 0o700); err != nil {
		t.Fatal(err)
	}

	if token, err := Token(context.Background(), cfg); err != nil || token != "access-1" {
		t.Fatalf("first Token = %q, %v", token, err)
	}
	calls := cognito.callCount()
	now = now.Add(30 * time.Minute)
	if token, err := Token(context.Background(), cfg); err != nil || token != "access-1" {
		t.Fatalf("cached Token = %q, %v; want access-1", token, err)
	}
	if cognito.callCount() != calls {
		t.Fatalf("a cached unexpired token called Cognito %d more times", cognito.callCount()-calls)
	}
	now = now.Add(26 * time.Minute) // 4 minutes before expiry
	if token, err := Token(context.Background(), cfg); err != nil || token != "access-2" {
		t.Fatalf("near-expiry Token = %q, %v; want a fresh sign-in's access-2", token, err)
	}
}

func TestTokenRefusesAFailedSignInWithoutATokenAndKeepsTheCache(t *testing.T) {
	cognito := &fakeCognito{t: t, refuse: true}
	server := httptest.NewServer(cognito)
	defer server.Close()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	cfg := testConfig(t, server.URL, &now)
	if err := os.MkdirAll(filepath.Dir(cfg.CacheFile), 0o700); err != nil {
		t.Fatal(err)
	}
	stale := `{"accessToken":"expired","expiresAt":"2026-10-03T11:00:00Z"}`
	if err := os.WriteFile(cfg.CacheFile, []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}

	token, err := Token(context.Background(), cfg)
	if err == nil || token != "" || !strings.Contains(err.Error(), "NotAuthorizedException") {
		t.Fatalf("Token = %q, %v; want no token and Cognito's refusal", token, err)
	}
	if body, _ := os.ReadFile(cfg.CacheFile); string(body) != stale {
		t.Fatalf("a refused sign-in rewrote the cache: %s", body)
	}
}

func TestConfigRefusesAMissingSetting(t *testing.T) {
	now := time.Now()
	cfg := testConfig(t, "", &now)
	cfg.Username = ""
	if _, err := Token(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "username") {
		t.Fatalf("Token = %v, want the missing username named", err)
	}
}

// The image probe's pod has no state volume: a token that cannot be cached is still returned,
// with the cache failure named, so the probe can prove the route.
func TestTokenReturnsAnUncachableTokenWithItsCacheError(t *testing.T) {
	server := httptest.NewServer(&fakeCognito{t: t})
	defer server.Close()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	cfg := testConfig(t, server.URL, &now) // the cache's directory is never created

	token, err := Token(context.Background(), cfg)
	var cacheErr *CacheError
	if token != "access-1" || !errors.As(err, &cacheErr) {
		t.Fatalf("Token = %q, %v; want access-1 with a *CacheError", token, err)
	}
}
