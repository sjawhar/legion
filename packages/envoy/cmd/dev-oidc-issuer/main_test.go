package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const (
	testAudience = "legion-broker-dev"
	testAccount  = "system:serviceaccount:example-ns:example-worker"
)

// lockedBuffer is the command's stderr, read by the test while the command still writes it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

var listening = regexp.MustCompile(`issuer listening url=(\S+)`)

// awaitListening waits for the command to log its URL on stderr and answers it.
func awaitListening(t *testing.T, stderr *lockedBuffer) string {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if match := listening.FindStringSubmatch(stderr.String()); match != nil {
			return match[1]
		}
	}
	t.Fatalf("the issuer never logged its URL:\n%s", stderr.String())
	return ""
}

// TestTheTokenFileVerifiesAgainstTheIssuerAndIsRewrittenBeforeItLapses runs the issuer with a
// lifetime of seconds: the file it writes before it reports its URL holds a token the broker's own
// verifier accepts for the account and audience asked for, bound to a pod and living exactly the
// lifetime, and four fifths of the way through that life the file holds a new token for the same
// pod, as a kubelet rewrites a projected token.
func TestTheTokenFileVerifiesAgainstTheIssuerAndIsRewrittenBeforeItLapses(t *testing.T) {
	// Long enough that the rewrite at four fifths of the life has real slack on a loaded
	// machine: a shorter one let the first token lapse between the rewriter waking and the
	// test's next read, which the read below would have called a token that does not verify.
	const lifetime = 6 * time.Second
	file := filepath.Join(t.TempDir(), "token")
	var stderr lockedBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- serve(ctx, []tokenSpec{{audience: testAudience, account: testAccount, file: file}}, codeFlow{}, lifetime, &stderr)
	}()
	t.Cleanup(func() {
		cancel()
		if code := <-done; code != 0 {
			t.Errorf("serve returned %d after its context ended, want 0\n%s", code, stderr.String())
		}
	})

	issuerURL := awaitListening(t, &stderr)
	verifier, err := oidc.New(context.Background(), issuerURL, testAudience)
	if err != nil {
		t.Fatalf("oidc.New(%s): %v", issuerURL, err)
	}
	readRaw := func() string {
		t.Helper()
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read the token file the issuer reported written: %v", err)
		}
		return strings.TrimSpace(string(raw))
	}
	verify := func(token string) enroll.AccountClaims {
		t.Helper()
		claims, err := enroll.K8sPodVerifier{Verifier: verifier}.VerifyAccount(context.Background(), token)
		if err != nil {
			t.Fatalf("the token in %s does not verify against the issuer it names: %v", file, err)
		}
		return claims
	}

	first := readRaw()
	claims := verify(first)
	if claims.Subject != testAccount || claims.PodUID == "" || claims.Expiry.Sub(claims.IssuedAt) != lifetime {
		t.Fatalf("the token names subject %q, pod %q and a life of %s, want %s, a pod, and %s",
			claims.Subject, claims.PodUID, claims.Expiry.Sub(claims.IssuedAt), testAccount, lifetime)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("the token file's mode is %v, want 0600: it is a credential", info.Mode().Perm())
	}

	for deadline := time.Now().Add(3 * lifetime); ; time.Sleep(20 * time.Millisecond) {
		// Only the rewritten token is verified: the first one is allowed to lapse while this
		// loop waits, and verifying it again would make a slow machine look like a broken
		// issuer.
		second := readRaw()
		if second != first {
			rewritten := verify(second)
			if rewritten.PodUID != claims.PodUID || rewritten.Subject != testAccount {
				t.Fatalf("the rewritten token names %s's pod %q, want %s's pod %q kept across rewrites", rewritten.Subject, rewritten.PodUID, testAccount, claims.PodUID)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the token file still holds its first token %s after it was minted, want it rewritten before it lapses", 3*lifetime)
		}
	}
}

// TestRunRefusesATokenItCannotMint pins the usage refusals: a --token that names no file, and one
// whose subject is not a ServiceAccount, each exit 2 naming what is wrong, before any issuer runs.
func TestRunRefusesATokenItCannotMint(t *testing.T) {
	for _, tc := range []struct{ value, says string }{
		{"legion-broker-dev=" + testAccount, "want <audience>=<service account>=<file>"},
		{"legion-broker-dev=ada@example.com=/tmp/token", `service account "ada@example.com" is not system:serviceaccount:<namespace>:<name>`},
	} {
		var stderr lockedBuffer
		if code := run(context.Background(), []string{"--token", tc.value}, &stderr); code != exitUsageError || !strings.Contains(stderr.String(), tc.says) {
			t.Errorf("--token %s: exit %d, stderr %q; want %d saying %q", tc.value, code, stderr.String(), exitUsageError, tc.says)
		}
	}
}

const (
	testClientID       = "dispatch-dev"
	testClientSecret   = "dev-client-secret"
	brokerClientID     = "broker-dev"
	brokerClientSecret = "broker-dev-client-secret"
	testRedirect       = "http://127.0.0.1:1/auth/callback"
	testDomain         = "d.example"
)

// noRedirects hands back each redirect rather than following it, so a test reads where the
// issuer sends the browser.
var noRedirects = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// startRun runs the command on args until the test ends, and answers the URL it logs.
func startRun(t *testing.T, args ...string) string {
	t.Helper()
	var stderr lockedBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- run(ctx, args, &stderr) }()
	t.Cleanup(func() {
		cancel()
		if code := <-done; code != 0 {
			t.Errorf("run returned %d after its context ended, want 0\n%s", code, stderr.String())
		}
	})
	return awaitListening(t, &stderr)
}

// discoveryEndpoints reads the issuer's discovery document and answers the authorization and
// token endpoints it names, failing the test unless they are the issuer's own.
func discoveryEndpoints(t *testing.T, issuerURL string) (authorization, token string) {
	t.Helper()
	response, err := http.Get(issuerURL + "/.well-known/openid-configuration")
	if err != nil {
		t.Fatal(err)
	}
	var discovery struct {
		AuthorizationEndpoint string `json:"authorization_endpoint"`
		TokenEndpoint         string `json:"token_endpoint"`
	}
	err = json.NewDecoder(response.Body).Decode(&discovery)
	response.Body.Close()
	if err != nil || discovery.AuthorizationEndpoint != issuerURL+"/authorize" || discovery.TokenEndpoint != issuerURL+"/token" {
		t.Fatalf("discovery = %+v (%v), want the authorization and token endpoints at %s", discovery, err, issuerURL)
	}
	return discovery.AuthorizationEndpoint, discovery.TokenEndpoint
}

// secretFile writes content to a file of t's own and answers its path.
func secretFile(t *testing.T, content string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "client-secret")
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

// authorizeURL is the authorization request the confidential client clientID sends the browser
// to.
func authorizeURL(endpoint, clientID, state, nonce string) string {
	return endpoint + "?" + url.Values{
		"client_id":     {clientID},
		"response_type": {"code"},
		"scope":         {"openid email"},
		"redirect_uri":  {testRedirect},
		"state":         {state},
		"nonce":         {nonce},
	}.Encode()
}

// authorize follows authURL as a browser does and answers the code the issuer redirects back to
// testRedirect with, failing unless it redirects there with state.
func authorize(t *testing.T, authURL, state string) string {
	t.Helper()
	response, err := noRedirects.Get(authURL)
	if err != nil {
		t.Fatalf("GET %s: %v", authURL, err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	location, err := url.Parse(response.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	query := location.Query()
	location.RawQuery = ""
	if response.StatusCode != http.StatusFound || location.String() != testRedirect || query.Get("state") != state || query.Get("code") == "" {
		t.Fatalf("GET %s: status %d to %q (%s), want 302 to %s with state %s and a code", authURL, response.StatusCode, response.Header.Get("Location"), body, testRedirect, state)
	}
	return query.Get("code")
}

// exchangeCode posts code to the token endpoint as clientID authenticating with secret, and
// answers the status and body.
func exchangeCode(t *testing.T, endpoint, clientID, code, secret string) (int, []byte) {
	t.Helper()
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {testRedirect}}
	request, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.SetBasicAuth(clientID, secret)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("POST %s: %v", endpoint, err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, body
}

// idTokenClaims is the payload of the compact JWT raw, whose signature the caller has verified.
func idTokenClaims(t *testing.T, raw string) map[string]any {
	t.Helper()
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		t.Fatalf("the ID token %q is not a compact JWT", raw)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}

// TestTheCodeFlowSignsInTheDefaultPersonUntilAnotherIsQueued drives the command through its flags:
// discovery names the authorization and token endpoints; an authorization with none queued signs
// in --sign-in-as's person, whose ID token, issued for the client's secret, carries the claims
// Dispatch's domain rule and its sign-in pool group rule read; a person queued at
// /_dev/sign-in-as is the next authorization's, once; and a code exchanged with another secret is
// refused. The last sign-in goes through Dispatch's own client and both its rules, which name the
// default person.
func TestTheCodeFlowSignsInTheDefaultPersonUntilAnotherIsQueued(t *testing.T) {
	ctx := context.Background()
	// The trailing newline is the file's, not the secret's.
	issuerURL := startRun(t, "--code-flow", testClientID+"="+secretFile(t, testClientSecret+"\n"), "--sign-in-as", "Alice@D.example", "--sign-in-group", "dispatch-members")

	authorizeEndpoint, tokenEndpoint := discoveryEndpoints(t, issuerURL)
	verifier, err := oidc.New(ctx, issuerURL, testClientID)
	if err != nil {
		t.Fatal(err)
	}
	signIn := func(nonce string) map[string]any {
		t.Helper()
		code := authorize(t, authorizeURL(authorizeEndpoint, testClientID, "state-"+nonce, nonce), "state-"+nonce)
		status, body := exchangeCode(t, tokenEndpoint, testClientID, code, testClientSecret)
		var tokens struct {
			IDToken string `json:"id_token"`
		}
		if err := json.Unmarshal(body, &tokens); status != http.StatusOK || err != nil {
			t.Fatalf("token exchange: status %d, body %s, want 200 with an ID token", status, body)
		}
		verified, err := verifier.Verify(ctx, tokens.IDToken)
		if err != nil || verified.Nonce != nonce {
			t.Fatalf("the ID token verifies %v with nonce %q, want verified with nonce %q", err, verified.Nonce, nonce)
		}
		return idTokenClaims(t, tokens.IDToken)
	}
	alice := map[string]any{"sub": "alice@d.example", "email": "alice@d.example", "email_verified": true, "hd": testDomain}
	bob := map[string]any{"sub": "bob-account", "email": "bob@d.example", "email_verified": true, "hd": testDomain}
	wantPerson := func(got, want map[string]any) {
		t.Helper()
		for claim, value := range want {
			if got[claim] != value {
				t.Errorf("the ID token's %s is %#v, want %#v (claims %v)", claim, got[claim], value, got)
			}
		}
	}

	wantPerson(signIn("nonce-1"), alice)

	queue := func(body string) int {
		t.Helper()
		response, err := http.Post(issuerURL+"/_dev/sign-in-as", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		return response.StatusCode
	}
	if status := queue(`["not", "claims"]`); status != http.StatusBadRequest {
		t.Fatalf("POST /_dev/sign-in-as with a JSON array: status %d, want 400", status)
	}
	queued, _ := json.Marshal(bob)
	if status := queue(string(queued)); status != http.StatusNoContent {
		t.Fatalf("POST /_dev/sign-in-as: status %d, want 204", status)
	}
	wantPerson(signIn("nonce-2"), bob)

	code := authorize(t, authorizeURL(authorizeEndpoint, testClientID, "state-wrong", "nonce-wrong"), "state-wrong")
	if status, body := exchangeCode(t, tokenEndpoint, testClientID, code, "not-the-secret"); status != http.StatusUnauthorized || !strings.Contains(string(body), "invalid_client") {
		t.Fatalf("token exchange with another secret: status %d, body %s, want 401 invalid_client", status, body)
	}

	flow := oidc.NewCodeFlow(issuerURL, testClientID, testClientSecret, testDomain)
	authURL, err := flow.AuthURL(ctx, testRedirect, "state-3", "nonce-3")
	if err != nil {
		t.Fatal(err)
	}
	session, err := flow.Exchange(ctx, testRedirect, authorize(t, authURL, "state-3"), "nonce-3")
	if err != nil {
		t.Fatalf("Dispatch's code flow: %v", err)
	}
	email, err := identity.DomainRule{Domain: testDomain}.Person(session.Claims)
	if err != nil || email != "alice@d.example" || session.Claims.Subject != "alice@d.example" {
		t.Fatalf("Dispatch's domain rule names %q (%v) for subject %q, want alice@d.example, the default once bob's turn is spent", email, err, session.Claims.Subject)
	}
	if email, err := (identity.GroupRule{Group: "dispatch-members"}).Person(session.Claims); err != nil || email != "alice@d.example" {
		t.Fatalf("Dispatch's group rule names %q (%v), want alice@d.example, a member of dispatch-members", email, err)
	}
	if _, err := (identity.GroupRule{Group: "other-group"}).Person(session.Claims); !errors.Is(err, identity.ErrNotMember) {
		t.Fatalf("Dispatch's group rule for another group: %v, want ErrNotMember", err)
	}
}

// TestTheSignInControlRefusesACallerOffLoopback sends /_dev/sign-in-as a request from an address
// that is not loopback: it is refused 403 and queues nobody, so the issuer, which has no default
// person, still refuses the next authorization.
func TestTheSignInControlRefusesACallerOffLoopback(t *testing.T) {
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), testClientID, testClientSecret)
	request := httptest.NewRequest(http.MethodPost, "/_dev/sign-in-as", strings.NewReader(`{"sub":"mallory","email":"mallory@d.example","email_verified":true,"hd":"d.example"}`))
	request.RemoteAddr = "192.0.2.1:40000"
	recorder := httptest.NewRecorder()
	signInAsHandler(issuer).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("POST /_dev/sign-in-as from %s: status %d, want 403", request.RemoteAddr, recorder.Code)
	}
	response, err := noRedirects.Get(authorizeURL(issuer.URL()+"/authorize", testClientID, "state", "nonce"))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("the authorization after a refused sign-in: status %d, want 400, nobody queued", response.StatusCode)
	}
}

// TestOneIssuerServesTwoClients gives --code-flow twice, Dispatch's client and the secrets
// broker's, as Dispatch's browser suite runs both against one issuer: the one issuer process
// serves both, each completes an authorization-code exchange with its own secret, and the ID
// token each is issued names its own client as the audience and the default person. A code
// issued to one client is not the other's to exchange, and neither authenticates with the
// other's secret.
func TestOneIssuerServesTwoClients(t *testing.T) {
	ctx := context.Background()
	issuerURL := startRun(t,
		"--code-flow", testClientID+"="+secretFile(t, testClientSecret),
		"--code-flow", brokerClientID+"="+secretFile(t, brokerClientSecret),
		"--sign-in-as", "alice@d.example")
	authorizeEndpoint, tokenEndpoint := discoveryEndpoints(t, issuerURL)
	signIn := func(clientID, secret, nonce string) map[string]any {
		t.Helper()
		state := "state-" + nonce
		code := authorize(t, authorizeURL(authorizeEndpoint, clientID, state, nonce), state)
		status, body := exchangeCode(t, tokenEndpoint, clientID, code, secret)
		var tokens struct {
			IDToken string `json:"id_token"`
		}
		if err := json.Unmarshal(body, &tokens); status != http.StatusOK || err != nil {
			t.Fatalf("%s's token exchange: status %d, body %s, want 200 with an ID token", clientID, status, body)
		}
		verifier, err := oidc.New(ctx, issuerURL, clientID)
		if err != nil {
			t.Fatal(err)
		}
		verified, err := verifier.Verify(ctx, tokens.IDToken)
		if err != nil || verified.Nonce != nonce {
			t.Fatalf("%s's ID token verifies %v with nonce %q, want verified with nonce %q", clientID, err, verified.Nonce, nonce)
		}
		return idTokenClaims(t, tokens.IDToken)
	}

	for _, client := range []struct{ clientID, secret string }{
		{testClientID, testClientSecret},
		{brokerClientID, brokerClientSecret},
	} {
		claims := signIn(client.clientID, client.secret, "nonce-"+client.clientID)
		if claims["aud"] != client.clientID || claims["email"] != "alice@d.example" || claims["hd"] != testDomain {
			t.Errorf("%s's ID token: aud %#v, email %#v, hd %#v, want %q, alice@d.example and %s", client.clientID, claims["aud"], claims["email"], claims["hd"], client.clientID, testDomain)
		}
	}

	code := authorize(t, authorizeURL(authorizeEndpoint, testClientID, "state-crossed", "nonce-crossed"), "state-crossed")
	if status, body := exchangeCode(t, tokenEndpoint, brokerClientID, code, brokerClientSecret); status != http.StatusBadRequest || !strings.Contains(string(body), "invalid_grant") {
		t.Errorf("the broker exchanging Dispatch's code: status %d, body %s, want 400 invalid_grant", status, body)
	}
	if status, body := exchangeCode(t, tokenEndpoint, brokerClientID, code, testClientSecret); status != http.StatusUnauthorized || !strings.Contains(string(body), "invalid_client") {
		t.Errorf("the broker authenticating with Dispatch's secret: status %d, body %s, want 401 invalid_client", status, body)
	}
}

// TestRunRefusesACodeFlowItCannotServe pins the usage refusals of --code-flow and --sign-in-as,
// each exit 2 naming what is wrong before any issuer runs: a value with no secret file, a secret
// file that cannot be read or holds only whitespace, one client id named twice, --sign-in-as with
// no code flow to sign in at, and a --sign-in-as that is not an email address.
func TestRunRefusesACodeFlowItCannotServe(t *testing.T) {
	secret := secretFile(t, testClientSecret)
	for _, tc := range []struct {
		args []string
		says string
	}{
		{[]string{"--code-flow", testClientID}, "want <client id>=<client secret file>"},
		{[]string{"--code-flow", testClientID + "=" + filepath.Join(t.TempDir(), "missing")}, "read the client secret"},
		{[]string{"--code-flow", testClientID + "=" + secretFile(t, " \n")}, "is empty"},
		{[]string{"--code-flow", testClientID + "=" + secret, "--code-flow", testClientID + "=" + secret}, "--code-flow names client " + testClientID + " twice"},
		{[]string{"--sign-in-as", "alice@d.example"}, "--sign-in-as needs --code-flow"},
		{[]string{"--code-flow", testClientID + "=" + secret, "--sign-in-as", "alice"}, `--sign-in-as "alice" is not an email address`},
		{[]string{"--code-flow", testClientID + "=" + secret, "--sign-in-group", "dispatch-members"}, "--sign-in-group needs --sign-in-as"},
	} {
		var stderr lockedBuffer
		if code := run(context.Background(), tc.args, &stderr); code != exitUsageError || !strings.Contains(stderr.String(), tc.says) {
			t.Errorf("%q: exit %d, stderr %q; want %d saying %q", tc.args, code, stderr.String(), exitUsageError, tc.says)
		}
	}
}
