package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/oidc"
)

const (
	testAudience = "legion-broker-dev"
	testAccount  = "system:serviceaccount:example-ns:example-worker"
	testClient   = "legion-broker-dev-sign-in"
	testSecret   = "dev-sign-in-secret"
	// testRedirect is the sign-in's callback. Nothing listens there: the test reads the code and
	// state from the issuer's redirect, as the callback would, without following it.
	testRedirect = "http://127.0.0.1:1/approve/callback"
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
		done <- serveIssuer(ctx, issuerConfig{specs: []tokenSpec{{audience: testAudience, account: testAccount, file: file}}, lifetime: lifetime}, &stderr)
	}()
	t.Cleanup(func() {
		cancel()
		if code := <-done; code != 0 {
			t.Errorf("serveIssuer returned %d after its context ended, want 0\n%s", code, stderr.String())
		}
	})

	var issuerURL string
	for deadline := time.Now().Add(10 * time.Second); issuerURL == "" && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if match := listening.FindStringSubmatch(stderr.String()); match != nil {
			issuerURL = match[1]
		}
	}
	if issuerURL == "" {
		t.Fatalf("the issuer never logged its URL:\n%s", stderr.String())
	}
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

// start runs the command with args until the test ends, and returns the issuer URL it logs once
// it serves.
func start(t *testing.T, args ...string) string {
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
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if match := listening.FindStringSubmatch(stderr.String()); match != nil {
			return match[1]
		}
		select {
		case code := <-done:
			done <- code
			t.Fatalf("run %v exited %d before it served:\n%s", args, code, stderr.String())
		default:
		}
	}
	t.Fatalf("the issuer never logged its URL:\n%s", stderr.String())
	return ""
}

// signInArgs are the arguments of an issuer that signs people in for testClient, on a loopback
// port the system picks, followed by extra.
func signInArgs(t *testing.T, extra ...string) []string {
	t.Helper()
	secretFile := filepath.Join(t.TempDir(), "client-secret")
	if err := os.WriteFile(secretFile, []byte(testSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return append([]string{"--listen", "127.0.0.1:0", "--sign-in-client", testClient, "--sign-in-client-secret-file", secretFile}, extra...)
}

// discover reads the issuer's discovery as the secrets broker does, for testClient in domain mode.
func discover(t *testing.T, issuerURL, hostedDomain string) *oidc.CodeFlow {
	t.Helper()
	flow, err := oidc.DiscoverCodeFlow(context.Background(), issuerURL, testClient, testSecret, hostedDomain, 5*time.Second)
	if err != nil {
		t.Fatalf("DiscoverCodeFlow(%s): %v", issuerURL, err)
	}
	return flow
}

// newSignIn is a fresh online sign-in naming hint, empty for none.
func newSignIn(hint string) oidc.OnlineSignIn {
	return oidc.OnlineSignIn{State: uuid.NewString(), Nonce: uuid.NewString(), Verifier: oauth2.GenerateVerifier(), LoginHint: hint}
}

// authorize sends a browser to authURL and returns the issuer's status and, from its redirect, the
// code and the state, without following the redirect.
func authorize(t *testing.T, authURL string) (status int, code, state string) {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Get(authURL)
	if err != nil {
		t.Fatalf("GET %s: %v", authURL, err)
	}
	defer response.Body.Close()
	location, err := url.Parse(response.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse the authorization redirect %q: %v", response.Header.Get("Location"), err)
	}
	return response.StatusCode, location.Query().Get("code"), location.Query().Get("state")
}

// signInWith runs one online sign-in naming hint to its exchange, and returns the verified claims.
func signInWith(t *testing.T, flow *oidc.CodeFlow, hint string) oidc.Claims {
	t.Helper()
	signIn := newSignIn(hint)
	status, code, state := authorize(t, flow.OnlineAuthURL(testRedirect, signIn))
	if status != http.StatusFound || code == "" || state != signIn.State {
		t.Fatalf("authorize with hint %q: status %d, code %q, state %q; want 302 with a code and state %q", hint, status, code, state, signIn.State)
	}
	claims, err := flow.OnlineExchange(context.Background(), testRedirect, code, signIn)
	if err != nil {
		t.Fatalf("OnlineExchange for hint %q: %v", hint, err)
	}
	return claims
}

// post sends body to the issuer's path with contentType and returns the status and response body.
func post(t *testing.T, issuerURL, path, contentType, body string) (int, string) {
	t.Helper()
	response, err := http.Post(issuerURL+path, contentType, strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer response.Body.Close()
	var answer bytes.Buffer
	if _, err := answer.ReadFrom(response.Body); err != nil {
		t.Fatalf("read the answer to POST %s: %v", path, err)
	}
	return response.StatusCode, answer.String()
}

// TestASignInSignsInTheQueuedPersonElseTheLoginHint signs in through the broker's own online code
// flow: a sign-in naming a login_hint signs that address in, its case kept, with the domain's hd
// and a verified email; a person queued at /dev/sign-in-as, as JSON or as a form, is signed in
// instead by the next sign-in alone, in the order queued; and an address keeps its sub across
// sign-ins.
func TestASignInSignsInTheQueuedPersonElseTheLoginHint(t *testing.T) {
	issuerURL := start(t, signInArgs(t)...)
	flow := discover(t, issuerURL, "example.com")

	ada := signInWith(t, flow, "Ada@Example.com")
	if ada.Email != "Ada@Example.com" || !ada.EmailVerified || ada.HostedDomain != "example.com" || ada.Subject == "" {
		t.Fatalf("the hinted sign-in signed in %+v, want Ada@Example.com, verified, hd example.com, with a sub", ada)
	}
	if status, body := post(t, issuerURL, "/dev/sign-in-as", "application/json", `{"email": "grace@example.com"}`); status != http.StatusNoContent {
		t.Fatalf("POST /dev/sign-in-as as JSON: %d %s, want 204", status, body)
	}
	if status, body := post(t, issuerURL, "/dev/sign-in-as", "application/x-www-form-urlencoded", url.Values{"email": {"bob@example.com"}}.Encode()); status != http.StatusNoContent {
		t.Fatalf("POST /dev/sign-in-as as a form: %d %s, want 204", status, body)
	}
	for _, want := range []string{"grace@example.com", "bob@example.com", "Ada@Example.com"} {
		got := signInWith(t, flow, "Ada@Example.com")
		if got.Email != want || !got.EmailVerified || got.HostedDomain != "example.com" {
			t.Fatalf("a sign-in hinting Ada@Example.com signed in %+v, want %s, verified, hd example.com", got, want)
		}
		switch {
		case want == "Ada@Example.com" && got.Subject != ada.Subject:
			t.Fatalf("Ada's second sign-in has sub %q, want her first's %q", got.Subject, ada.Subject)
		case want != "Ada@Example.com" && got.Subject == ada.Subject:
			t.Fatalf("%s has Ada's sub %q", want, got.Subject)
		}
	}
	if status, _ := post(t, issuerURL, "/dev/sign-in-as", "application/json", `{}`); status != http.StatusBadRequest {
		t.Fatalf("POST /dev/sign-in-as naming no email: %d, want 400", status)
	}
}

// TestASignInWithNoOneToSignInIsRefused pins the last two choices: with no one queued and no
// login_hint, the authorization request is refused 400 unless --sign-in-default names a person, who
// is then signed in with the --sign-in-domain's hd.
func TestASignInWithNoOneToSignInIsRefused(t *testing.T) {
	flow := discover(t, start(t, signInArgs(t)...), "example.com")
	if status, code, _ := authorize(t, flow.OnlineAuthURL(testRedirect, newSignIn(""))); status != http.StatusBadRequest || code != "" {
		t.Fatalf("a sign-in naming no one: status %d, code %q; want 400 and no code", status, code)
	}

	flow = discover(t, start(t, signInArgs(t, "--sign-in-default", "dora@example.org", "--sign-in-domain", "example.org")...), "example.org")
	if got := signInWith(t, flow, ""); got.Email != "dora@example.org" || got.HostedDomain != "example.org" || !got.EmailVerified {
		t.Fatalf("a sign-in naming no one with a default signed in %+v, want dora@example.org, verified, hd example.org", got)
	}
}

// TestACodeIsExchangedOnlyWithItsVerifier holds the issuer to PKCE S256: the code of a sign-in
// whose request carried a challenge is refused with another verifier.
func TestACodeIsExchangedOnlyWithItsVerifier(t *testing.T) {
	flow := discover(t, start(t, signInArgs(t)...), "example.com")
	signIn := newSignIn("ada@example.com")
	status, code, _ := authorize(t, flow.OnlineAuthURL(testRedirect, signIn))
	if status != http.StatusFound || code == "" {
		t.Fatalf("authorize: status %d, code %q; want 302 with a code", status, code)
	}
	stolen := signIn
	stolen.Verifier = oauth2.GenerateVerifier()
	if claims, err := flow.OnlineExchange(context.Background(), testRedirect, code, stolen); err == nil {
		t.Fatalf("the code was exchanged with another verifier, signing in %+v", claims)
	}
}

// TestACodeExchangeAnswersNoRefreshToken reads the token endpoint's answer itself, since the online
// flow drops a refresh token: the issuer answers an ID token and none, as Google does for an online
// sign-in.
func TestACodeExchangeAnswersNoRefreshToken(t *testing.T) {
	issuerURL := start(t, signInArgs(t)...)
	flow := discover(t, issuerURL, "example.com")
	signIn := newSignIn("ada@example.com")
	_, code, _ := authorize(t, flow.OnlineAuthURL(testRedirect, signIn))
	request, err := http.NewRequest(http.MethodPost, issuerURL+"/token", strings.NewReader(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {testRedirect},
		"code_verifier": {signIn.Verifier},
	}.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.SetBasicAuth(testClient, testSecret)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("POST /token: %v", err)
	}
	defer response.Body.Close()
	var answer map[string]any
	if err := json.NewDecoder(response.Body).Decode(&answer); err != nil {
		t.Fatalf("decode the token answer: %v", err)
	}
	if _, hasRefresh := answer["refresh_token"]; response.StatusCode != http.StatusOK || answer["id_token"] == nil || hasRefresh {
		t.Fatalf("the code exchange answered %d with id_token %t and refresh_token %t; want 200, an ID token and no refresh token",
			response.StatusCode, answer["id_token"] != nil, hasRefresh)
	}
}

// TestMintAnswersAServiceAccountTokenTheIssuerVerifies mints at /dev/mint and verifies the answer
// with the broker's own pod-token verifier: the subject, audience, pod and lifetime asked for, a
// random pod and the hour when none is asked for, and a subject that is not a ServiceAccount or a
// lifetime past the hour refused 400.
func TestMintAnswersAServiceAccountTokenTheIssuerVerifies(t *testing.T) {
	issuerURL := start(t, "--listen", "127.0.0.1:0")
	verifier, err := oidc.New(context.Background(), issuerURL, testAudience)
	if err != nil {
		t.Fatalf("oidc.New(%s): %v", issuerURL, err)
	}
	mint := func(request map[string]any) enroll.AccountClaims {
		t.Helper()
		body, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		status, answer := post(t, issuerURL, "/dev/mint", "application/json", string(body))
		var minted struct {
			Token string `json:"token"`
		}
		if status != http.StatusOK || json.Unmarshal([]byte(answer), &minted) != nil || minted.Token == "" {
			t.Fatalf("POST /dev/mint %s: %d %s, want 200 with a token", body, status, answer)
		}
		claims, err := enroll.K8sPodVerifier{Verifier: verifier}.VerifyAccount(context.Background(), minted.Token)
		if err != nil {
			t.Fatalf("the minted token does not verify against the issuer for %s: %v", testAudience, err)
		}
		return claims
	}

	asked := mint(map[string]any{"subject": testAccount, "audience": testAudience, "pod_uid": "pod-uid-asked", "lifetime_seconds": 600})
	if asked.Subject != testAccount || asked.PodUID != "pod-uid-asked" || asked.Expiry.Sub(asked.IssuedAt) != 10*time.Minute {
		t.Fatalf("the minted token names %q, pod %q, a life of %s; want %s, pod-uid-asked, 10m0s", asked.Subject, asked.PodUID, asked.Expiry.Sub(asked.IssuedAt), testAccount)
	}
	unasked := mint(map[string]any{"subject": testAccount, "audience": testAudience})
	if unasked.PodUID == "" || unasked.PodUID == asked.PodUID || unasked.Expiry.Sub(unasked.IssuedAt) != tokenLifetime {
		t.Fatalf("a token minted with no pod or lifetime names pod %q and a life of %s, want a random pod and %s", unasked.PodUID, unasked.Expiry.Sub(unasked.IssuedAt), tokenLifetime)
	}

	for _, refused := range []string{
		`{"subject": "ada@example.com", "audience": "` + testAudience + `"}`,
		`{"subject": "system:serviceaccount:example-ns", "audience": "` + testAudience + `"}`,
		`{"subject": "` + testAccount + `"}`,
		`{"subject": "` + testAccount + `", "audience": "` + testAudience + `", "lifetime_seconds": ` + strconv.FormatInt(int64(tokenLifetime/time.Second)+1, 10) + `}`,
	} {
		if status, answer := post(t, issuerURL, "/dev/mint", "application/json", refused); status != http.StatusBadRequest {
			t.Errorf("POST /dev/mint %s: %d %s, want 400", refused, status, answer)
		}
	}
}

// TestListenServesOnTheAddressGiven runs the issuer on a port fixed before it starts and finds it
// there, the URL it logs naming that port.
func TestListenServesOnTheAddressGiven(t *testing.T) {
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := free.Addr().String()
	if err := free.Close(); err != nil {
		t.Fatal(err)
	}
	if issuerURL := start(t, "--listen", address); issuerURL != "http://"+address {
		t.Fatalf("the issuer logged %s, want http://%s", issuerURL, address)
	}
	if _, err := oidc.New(context.Background(), "http://"+address, testAudience); err != nil {
		t.Fatalf("the issuer's discovery at %s: %v", address, err)
	}
}

// TestRunRefusesAnIssuerItCannotServeSafely pins the usage refusals of the new flags, each exit 2
// before any issuer runs: a --listen host that is not loopback, and sign-in flags given in part.
func TestRunRefusesAnIssuerItCannotServeSafely(t *testing.T) {
	secretFile := filepath.Join(t.TempDir(), "client-secret")
	if err := os.WriteFile(secretFile, []byte(testSecret), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		says string
	}{
		{[]string{"--listen", "0.0.0.0:0"}, `host "0.0.0.0" is not a loopback address`},
		{[]string{"--listen", ":8080"}, `host "" is not a loopback address`},
		{[]string{"--listen", "issuer.internal.example:8080"}, `host "issuer.internal.example" is not a loopback address`},
		{[]string{"--sign-in-client", testClient}, "--sign-in-client and --sign-in-client-secret-file are given together or not at all"},
		{[]string{"--sign-in-client-secret-file", secretFile}, "--sign-in-client and --sign-in-client-secret-file are given together or not at all"},
		{[]string{"--sign-in-domain", "example.org"}, "--sign-in-domain and --sign-in-default need --sign-in-client"},
		{[]string{"--sign-in-default", "ada@example.com"}, "--sign-in-domain and --sign-in-default need --sign-in-client"},
		{[]string{"--sign-in-client", testClient, "--sign-in-client-secret-file", filepath.Join(t.TempDir(), "absent")}, "--sign-in-client-secret-file:"},
	} {
		var stderr lockedBuffer
		if code := run(context.Background(), tc.args, &stderr); code != exitUsageError || !strings.Contains(stderr.String(), tc.says) {
			t.Errorf("run %v: exit %d, stderr %q; want %d saying %q", tc.args, code, stderr.String(), exitUsageError, tc.says)
		}
	}
}
