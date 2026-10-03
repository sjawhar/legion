package modeltoken

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// childConfig is the Config a test hands a fresh process of this test binary (TestMain), whose
// Token result it prints as JSON: the TLS roots and proxy settings crypto/x509 and net/http read
// from the environment are loaded once per process, so only a fresh process proves what a pod's
// command reads.
const childConfig = "MODELTOKEN_TEST_CHILD_CONFIG"

type childResult struct{ Token, Err string }

func TestMain(m *testing.M) {
	if encoded := os.Getenv(childConfig); encoded != "" {
		var cfg Config
		if err := json.Unmarshal([]byte(encoded), &cfg); err != nil {
			panic(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		token, err := Token(ctx, cfg)
		cancel()
		result := childResult{Token: token}
		if err != nil {
			result.Err = err.Error()
		}
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			panic(err)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// tokenInFreshProcess runs Token for cfg in a new process of this test binary with env added.
func tokenInFreshProcess(t *testing.T, cfg Config, env ...string) childResult {
	t.Helper()
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0])
	child.Env = append(append(os.Environ(), env...), childConfig+"="+string(encoded))
	out, err := child.Output()
	if err != nil {
		t.Fatalf("child: %v", err)
	}
	var result childResult
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("child printed %q: %v", out, err)
	}
	return result
}

// fakeCognito answers the two calls of Cognito's custom authentication: InitiateAuth with
// CUSTOM_AUTH, then RespondToAuthChallenge with the pod's service-account token as the answer.
// accessToken, when set, is the access token every sign-in returns in place of access-<n>.
type fakeCognito struct {
	t               *testing.T
	mu              sync.Mutex
	signIn          int
	refuse          bool
	refusalCode     string
	challengeName   string
	accessToken     string
	responsePadding int
	calls           atomic.Int32
}

func (f *fakeCognito) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Errorf("decode request: %v", err)
	}
	target := strings.TrimPrefix(r.Header.Get("X-Amz-Target"), "AWSCognitoIdentityProviderService.")
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
		challengeName := "CUSTOM_CHALLENGE"
		if f.challengeName != "" {
			challengeName = f.challengeName
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ChallengeName": challengeName, "Session": "session-1",
			"ChallengeParameters": map[string]string{"USERNAME": "machine-user"},
		})
	case "RespondToAuthChallenge":
		responses, _ := body["ChallengeResponses"].(map[string]any)
		if body["ChallengeName"] != "CUSTOM_CHALLENGE" || body["Session"] != "session-1" ||
			responses["USERNAME"] != "machine-user" || responses["ANSWER"] != "pod-service-account-token" {
			f.t.Errorf("RespondToAuthChallenge = %#v, want the pod's token as the custom-challenge answer", body)
		}
		if f.refuse {
			code := "NotAuthorizedException"
			if f.refusalCode != "" {
				code = f.refusalCode
			}
			w.Header().Set("X-Amzn-ErrorType", code)
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"__type": code, "message": "Incorrect username or password."})
			return
		}
		f.mu.Lock()
		f.signIn++
		token := fmt.Sprintf("access-%d", f.signIn)
		f.mu.Unlock()
		if f.accessToken != "" {
			token = f.accessToken
		}
		response, _ := json.Marshal(map[string]any{"AuthenticationResult": map[string]any{
			"AccessToken": token, "RefreshToken": "refresh-never-kept", "IdToken": "id-never-kept",
			"ExpiresIn": 3600, "TokenType": "Bearer",
		}})
		_, _ = w.Write(response)
		if f.responsePadding > 0 {
			_, _ = w.Write([]byte(strings.Repeat(" ", f.responsePadding)))
		}
	default:
		f.t.Errorf("unexpected Cognito call %q", target)
		w.WriteHeader(http.StatusBadRequest)
	}
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
	server := httptest.NewServer(&fakeCognito{t: t})
	defer server.Close()
	cfg := testConfig(t, server.URL)

	for _, want := range []string{"access-1", "access-2"} {
		if token, err := Token(context.Background(), cfg); err != nil || token != want {
			t.Fatalf("Token = %q, %v; want %s", token, err, want)
		}
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

// Error codes and challenge names belong to the endpoint, so they must not echo the pod token into
// the error a caller receives.
func TestTokenHidesEndpointControlledErrorCodeAndChallengeName(t *testing.T) {
	for _, tc := range []struct {
		name          string
		refuse        bool
		refusalCode   string
		challengeName string
	}{
		{name: "refusal error code", refuse: true, refusalCode: "pod-service-account-token"},
		{name: "challenge name", challengeName: "pod-service-account-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cognito := &fakeCognito{
				t: t, refuse: tc.refuse, refusalCode: tc.refusalCode, challengeName: tc.challengeName,
			}
			server := httptest.NewServer(cognito)
			defer server.Close()

			token, err := Token(context.Background(), testConfig(t, server.URL))
			if token != "" || err == nil {
				t.Fatalf("Token = %q, %v; want no token and a refusal", token, err)
			}
			if strings.Contains(err.Error(), "pod-service-account-token") {
				t.Fatalf("Token error %q repeats the endpoint's token echo", err)
			}
		})
	}
}

// A configured service-account token pointer is authoritative: a blank file is a configuration
// error naming that flag, before the command contacts Cognito.
func TestTokenRefusesAnEmptyServiceAccountTokenFile(t *testing.T) {
	cognito := &fakeCognito{t: t}
	server := httptest.NewServer(cognito)
	defer server.Close()
	cfg := testConfig(t, server.URL)
	if err := os.WriteFile(cfg.ServiceAccountTokenFile, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}

	token, err := Token(context.Background(), cfg)
	if token != "" || err == nil || !strings.Contains(err.Error(), "--service-account-token-file") ||
		!strings.Contains(err.Error(), cfg.ServiceAccountTokenFile) {
		t.Fatalf("Token = %q, %v; want the named empty token-file refusal", token, err)
	}
	if cognito.calls.Load() != 0 {
		t.Fatalf("Cognito got %d requests, want none", cognito.calls.Load())
	}
}

// Oh My Pi trims the command's whole output and sends it as a header: an access token that is
// blank or spans lines is refused rather than printed.
func TestTokenRefusesAnAccessTokenThatIsNotOneLine(t *testing.T) {
	for name, accessToken := range map[string]string{
		"whitespace only": "   ",
		"two lines":       "good\nX-Injected: 1",
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(&fakeCognito{t: t, accessToken: accessToken})
			defer server.Close()

			if token, err := Token(context.Background(), testConfig(t, server.URL)); err == nil || token != "" {
				t.Fatalf("Token = %q, %v; want no token and the refusal", token, err)
			}
		})
	}
}

// An endpoint response longer than maxResponseBytes is refused before it fills the pod's memory,
// whether the excess bytes are part of the JSON answer or trailing padding after a valid answer.
func TestTokenRefusesAResponseLongerThanTheCap(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cognito *fakeCognito
	}{
		{name: "oversized access token", cognito: &fakeCognito{accessToken: strings.Repeat("a", 16*maxResponseBytes)}},
		{name: "valid answer plus padding", cognito: &fakeCognito{responsePadding: 2 * maxResponseBytes}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.cognito.t = t
			server := httptest.NewServer(tc.cognito)
			defer server.Close()

			token, err := Token(context.Background(), testConfig(t, server.URL))
			if token != "" || err == nil {
				t.Fatalf("Token = %q, %v; want no token and a refusal", token, err)
			}
			if !strings.Contains(err.Error(), "longer than 64 KiB") {
				t.Fatalf("Token error %q; want the fixed response-cap refusal", err)
			}
		})
	}
}

// A 302 is an endpoint answer, never a second destination for the sign-in.
func TestTokenFollowsNoRedirect(t *testing.T) {
	var elsewhere atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere.Add(1) }))
	defer target.Close()
	cognito := &fakeCognito{t: t}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.Header.Get("X-Amz-Target"), ".RespondToAuthChallenge") {
			http.Redirect(w, r, target.URL, http.StatusFound)
			return
		}
		cognito.ServeHTTP(w, r)
	}))
	defer server.Close()

	if token, err := Token(context.Background(), testConfig(t, server.URL)); err == nil || token != "" {
		t.Fatalf("Token = %q, %v; want no token", token, err)
	}
	if elsewhere.Load() != 0 {
		t.Fatalf("the redirect target got %d requests, want none", elsewhere.Load())
	}
}

// A certificate named by SSL_CERT_FILE or SSL_CERT_DIR, which a workspace's .env can set, is not a
// root the sign-in trusts.
func TestTokenTrustsNoTLSRootFromTheEnvironment(t *testing.T) {
	cognito := &fakeCognito{t: t}
	server := httptest.NewTLSServer(cognito)
	defer server.Close()
	dir := t.TempDir()
	certificate := filepath.Join(dir, "endpoint.pem")
	encoded := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(certificate, encoded, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, variable := range []string{"SSL_CERT_FILE=" + certificate, "SSL_CERT_DIR=" + dir} {
		t.Run(strings.SplitN(variable, "=", 2)[0], func(t *testing.T) {
			result := tokenInFreshProcess(t, testConfig(t, server.URL), variable)
			if result.Token != "" || result.Err == "" {
				t.Fatalf("Token = %q, %q; want the endpoint's certificate refused", result.Token, result.Err)
			}
		})
	}
	if cognito.calls.Load() != 0 {
		t.Fatalf("the endpoint got %d requests over an untrusted certificate, want none", cognito.calls.Load())
	}
}

// HTTPS_PROXY and HTTP_PROXY, which a workspace's .env can set, never carry the sign-in.
func TestTokenTakesNoProxyFromTheEnvironment(t *testing.T) {
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { proxied.Add(1) }))
	defer proxy.Close()

	// 192.0.2.1 is TEST-NET-1: unreachable directly, and not a loopback address, which net/http's
	// environment proxy would skip.
	result := tokenInFreshProcess(t, testConfig(t, "https://192.0.2.1"), "HTTPS_PROXY="+proxy.URL, "HTTP_PROXY="+proxy.URL)
	if result.Token != "" || result.Err == "" {
		t.Fatalf("Token = %q, %q; want the sign-in to fail on an unreachable endpoint", result.Token, result.Err)
	}
	if proxied.Load() != 0 {
		t.Fatalf("the environment's proxy got %d requests, want none", proxied.Load())
	}
}
