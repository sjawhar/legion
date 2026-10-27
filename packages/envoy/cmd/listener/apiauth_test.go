package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/testnats"
)

const (
	listenerTestAudience = "envoy"
	listenerTestSubject  = "system:serviceaccount:legion:legion-worker"
)

func TestAPIAuth(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	issuer := oidctest.New(t)
	signing := issuer.PublishKey(t, "listener-test-key")
	verifier := discoveredAPIVerifier(t, issuer)

	serviceToken := issuer.Mint(t, signing, issuer.Claims(listenerTestSubject, listenerTestAudience))

	otherAudience := issuer.Claims(listenerTestSubject, listenerTestAudience)
	otherAudience["aud"] = []string{"dispatch"}
	otherAudienceToken := issuer.Mint(t, signing, otherAudience)

	const unauthorized = "{\"error\":\"unauthorized\"}\n"

	cases := []struct {
		name       string
		token      string
		oidc       bool
		path       string
		authorize  string
		wantStatus int
		wantBody   string
		// wantReason is the failure class the reject must log, empty when the
		// request must produce no rejection line at all.
		wantReason string
	}{
		{
			name:       "allows v1 requests when neither token nor verifier is configured",
			path:       "/v1/sessions",
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "rejects v1 requests without a bearer token",
			token:      "expected-token",
			path:       "/v1/sessions",
			wantStatus: http.StatusUnauthorized,
			wantBody:   unauthorized,
		},
		{
			name:       "rejects v1 requests with a wrong bearer token",
			token:      "expected-token",
			path:       "/v1/sessions",
			authorize:  "Bearer wrong-token",
			wantStatus: http.StatusUnauthorized,
			wantBody:   unauthorized,
		},
		{
			name:       "allows v1 requests with the configured bearer token",
			token:      "expected-token",
			path:       "/v1/sessions",
			authorize:  "Bearer expected-token",
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "allows health checks without the bearer token",
			token:      "expected-token",
			path:       "/healthz",
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "allows metrics without the bearer token",
			token:      "expected-token",
			path:       "/metrics",
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "allows webhook ingress without the bearer token",
			token:      "expected-token",
			path:       "/webhook/github",
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "does not exempt the unused plural webhook path",
			token:      "expected-token",
			path:       "/webhooks/github",
			wantStatus: http.StatusUnauthorized,
			wantBody:   unauthorized,
		},
		{
			name:       "allows the shared token while a verifier is configured",
			token:      "expected-token",
			oidc:       true,
			path:       "/v1/sessions",
			authorize:  "Bearer expected-token",
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "allows a verified service-account token beside the shared token",
			token:      "expected-token",
			oidc:       true,
			path:       "/v1/sessions",
			authorize:  "Bearer " + serviceToken,
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "allows a verified service-account token when no shared token is set",
			oidc:       true,
			path:       "/v1/sessions",
			authorize:  "Bearer " + serviceToken,
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "rejects a service-account token minted for another audience",
			token:      "expected-token",
			oidc:       true,
			path:       "/v1/sessions",
			authorize:  "Bearer " + otherAudienceToken,
			wantStatus: http.StatusUnauthorized,
			wantBody:   unauthorized,
			wantReason: "audience",
		},
		{
			name:       "rejects an unauthenticated request when only a verifier is configured",
			oidc:       true,
			path:       "/v1/sessions",
			wantStatus: http.StatusUnauthorized,
			wantBody:   unauthorized,
		},
		{
			name:       "rejects a garbage bearer when only a verifier is configured",
			oidc:       true,
			path:       "/v1/sessions",
			authorize:  "Bearer not-a-service-account-token",
			wantStatus: http.StatusUnauthorized,
			wantBody:   unauthorized,
		},
		{
			name:       "rejects an empty bearer when only a verifier is configured",
			oidc:       true,
			path:       "/v1/sessions",
			authorize:  "Bearer ",
			wantStatus: http.StatusUnauthorized,
			wantBody:   unauthorized,
		},
		{
			name:       "rejects a JWT-shaped bearer the verifier cannot verify",
			oidc:       true,
			path:       "/v1/sessions",
			authorize:  "Bearer not.a.jwt",
			wantStatus: http.StatusUnauthorized,
			wantBody:   unauthorized,
			wantReason: "malformed",
		},
		{
			name:       "allows health checks when only a verifier is configured",
			oidc:       true,
			path:       "/healthz",
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "allows webhook ingress when only a verifier is configured",
			oidc:       true,
			path:       "/webhook/github",
			wantStatus: http.StatusNoContent,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, tc.path, nil)
			request.Host = "127.0.0.1:9020"
			if tc.authorize != "" {
				request.Header.Set("Authorization", tc.authorize)
			}

			var configured *apiVerifier
			if tc.oidc {
				configured = verifier
			}
			var logged bytes.Buffer
			apiAuth(tc.token, configured, nil, logging.NewWithWriter("test-machine", &logged), next).
				ServeHTTP(recorder, request)

			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, tc.wantStatus)
			}
			if body := recorder.Body.String(); body != tc.wantBody {
				t.Fatalf("body = %q, want %q", body, tc.wantBody)
			}
			// The class belongs in the operator's log and nowhere else: telling an
			// unauthenticated caller which credential was refused tells it which one
			// the listener is configured for.
			if strings.Contains(recorder.Body.String(), tc.wantReason) && tc.wantReason != "" {
				t.Fatalf("the 401 body names the failure class: %q", recorder.Body.String())
			}
			assertRejectionLog(t, logged.String(), tc.wantReason, tc.authorize)
		})
	}
}

func TestAPIAuthValidatesV1BodyContentType(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	cases := []struct {
		name          string
		path          string
		contentType   string
		unknownLength bool
		wantStatus    int
	}{
		{
			name:        "allows JSON parameters",
			path:        "/v1/messages/publish",
			contentType: "application/json; charset=utf-8",
			wantStatus:  http.StatusNoContent,
		},
		{
			name:       "refuses missing content type",
			path:       "/v1/messages/publish",
			wantStatus: http.StatusUnsupportedMediaType,
		},
		{
			name:        "refuses non JSON content type",
			path:        "/v1/messages/publish",
			contentType: "text/plain",
			wantStatus:  http.StatusUnsupportedMediaType,
		},
		{
			// A chunked body reaches the handler with ContentLength -1, so the gate must not
			// read an unknown length as an empty body.
			name:          "refuses non JSON body of unknown length",
			path:          "/v1/messages/publish",
			contentType:   "text/plain",
			unknownLength: true,
			wantStatus:    http.StatusUnsupportedMediaType,
		},
		{
			// ParseMediaType still returns "application/json" beside its error here, so the
			// gate must refuse on the error rather than on the type alone.
			name:        "refuses malformed JSON content type",
			path:        "/v1/messages/publish",
			contentType: "application/json; charset",
			wantStatus:  http.StatusUnsupportedMediaType,
		},
		{
			name:        "leaves webhooks to their signature handler",
			path:        "/webhook/github",
			contentType: "text/plain",
			wantStatus:  http.StatusNoContent,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var requestBody io.Reader = strings.NewReader(`{}`)
			if tc.unknownLength {
				requestBody = io.NopCloser(requestBody)
			}
			request := httptest.NewRequest(http.MethodPost, tc.path, requestBody)
			request.Host = "127.0.0.1:9020"
			if tc.unknownLength && request.ContentLength != -1 {
				t.Fatalf("ContentLength = %d, want -1 for a body of unknown length", request.ContentLength)
			}
			if tc.contentType != "" {
				request.Header.Set("Content-Type", tc.contentType)
			}
			recorder := httptest.NewRecorder()

			apiAuth("", nil, nil, logging.NewWithWriter("test-machine", io.Discard), next).ServeHTTP(recorder, request)

			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			if tc.wantStatus == http.StatusUnsupportedMediaType {
				if got := recorder.Header().Get("Content-Type"); got != "application/json" {
					t.Fatalf("Content-Type = %q, want application/json", got)
				}
				if body := recorder.Body.String(); body != "{\"error\":\"Content-Type must be application/json\"}\n" {
					t.Fatalf("body = %q, want JSON Content-Type refusal", body)
				}
			}
		})
	}
}

// assertRejectionLog holds the listener to one rejection line per refused
// service-account token, naming the class and never repeating the bearer.
func assertRejectionLog(t *testing.T, logged, wantReason, authorize string) {
	t.Helper()
	const message = "listener: service-account token rejected"
	lines := 0
	for _, line := range strings.Split(strings.TrimSpace(logged), "\n") {
		if strings.Contains(line, message) {
			lines++
			if !strings.Contains(line, `"reason":"`+wantReason+`"`) {
				t.Fatalf("rejection line does not name reason %q: %s", wantReason, line)
			}
		}
	}
	want := 0
	if wantReason != "" {
		want = 1
	}
	if lines != want {
		t.Fatalf("logged %d rejection lines, want %d; log: %s", lines, want, logged)
	}
	if raw, ok := strings.CutPrefix(authorize, "Bearer "); ok && raw != "" && strings.Contains(logged, raw) {
		t.Fatalf("the log repeats the bearer it refused: %s", logged)
	}
}

func TestValidateListenerAPIAuth(t *testing.T) {
	verifier := newAPIVerifier("https://oidc.eks.example/id/CLUSTER", listenerTestAudience)

	cases := []struct {
		name                 string
		listenHost           string
		apiToken             string
		allowUnauthenticated string
		oidc                 bool
		wantError            bool
	}{
		{
			name:       "allows loopback IPv4 without a token",
			listenHost: "127.0.0.1",
		},
		{
			name:       "allows loopback IPv6 without a token",
			listenHost: "::1",
		},
		{
			name:       "allows localhost without a token",
			listenHost: "localhost",
		},
		{
			name:       "rejects wildcard bind without a token or a verifier",
			listenHost: "0.0.0.0",
			wantError:  true,
		},
		{
			name:       "allows non-loopback bind with a token",
			listenHost: "0.0.0.0",
			apiToken:   "expected-token",
		},
		{
			name:       "allows non-loopback bind with only a configured verifier",
			listenHost: "0.0.0.0",
			oidc:       true,
		},
		{
			name:                 "allows explicit unauthenticated non-loopback bind",
			listenHost:           "0.0.0.0",
			allowUnauthenticated: "1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var configured *apiVerifier
			if tc.oidc {
				configured = verifier
			}
			err := validateListenerAPIAuth(tc.listenHost, tc.apiToken, tc.allowUnauthenticated, configured)
			if (err != nil) != tc.wantError {
				t.Fatalf("validateListenerAPIAuth(%q, token set: %t, allow unauthenticated: %q, verifier set: %t) error = %v, want error: %t", tc.listenHost, tc.apiToken != "", tc.allowUnauthenticated, configured != nil, err, tc.wantError)
			}
		})
	}
}

func TestResolveListenerOIDCConfig(t *testing.T) {
	cases := []struct {
		name         string
		env          map[string]string
		wantIssuer   string
		wantAudience string
		wantMissing  string
	}{
		{
			name: "neither variable set leaves the verifier unconfigured",
			env:  map[string]string{},
		},
		{
			name: "both variables set configure the verifier",
			env: map[string]string{
				"ENVOY_OIDC_ISSUER":   "https://oidc.eks.example/id/CLUSTER",
				"ENVOY_OIDC_AUDIENCE": "envoy",
			},
			wantIssuer:   "https://oidc.eks.example/id/CLUSTER",
			wantAudience: "envoy",
		},
		{
			name:        "issuer without audience names the missing audience",
			env:         map[string]string{"ENVOY_OIDC_ISSUER": "https://oidc.eks.example/id/CLUSTER"},
			wantMissing: "ENVOY_OIDC_AUDIENCE",
		},
		{
			name:        "audience without issuer names the missing issuer",
			env:         map[string]string{"ENVOY_OIDC_AUDIENCE": "envoy"},
			wantMissing: "ENVOY_OIDC_ISSUER",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issuer, audience, err := resolveListenerOIDCConfig(func(key string) string { return tc.env[key] })
			if tc.wantMissing != "" {
				if err == nil {
					t.Fatalf("resolveListenerOIDCConfig(%v) error = nil, want one naming %s", tc.env, tc.wantMissing)
				}
				if !strings.Contains(err.Error(), tc.wantMissing) {
					t.Fatalf("resolveListenerOIDCConfig(%v) error = %q, want it to name %s", tc.env, err, tc.wantMissing)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveListenerOIDCConfig(%v) error = %v, want nil", tc.env, err)
			}
			if issuer != tc.wantIssuer || audience != tc.wantAudience {
				t.Fatalf("resolveListenerOIDCConfig(%v) = (%q, %q), want (%q, %q)", tc.env, issuer, audience, tc.wantIssuer, tc.wantAudience)
			}
		})
	}
}

func TestDescribeListenerAPIAuth(t *testing.T) {
	cases := []struct {
		name   string
		token  string
		issuer string
		want   string
	}{
		{
			name: "neither is disabled",
			want: "disabled",
		},
		{
			name:  "only the shared token",
			token: "expected-token",
			want:  "shared token",
		},
		{
			name:   "only the verifier names its issuer",
			issuer: "https://oidc.eks.example/id/CLUSTER",
			want:   "oidc https://oidc.eks.example/id/CLUSTER",
		},
		{
			name:   "both name both",
			token:  "expected-token",
			issuer: "https://oidc.eks.example/id/CLUSTER",
			want:   "shared token and oidc https://oidc.eks.example/id/CLUSTER",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := describeListenerAPIAuth(tc.token, tc.issuer); got != tc.want {
				t.Fatalf("describeListenerAPIAuth(token set: %t, %q) = %q, want %q", tc.token != "", tc.issuer, got, tc.want)
			}
		})
	}
}

func TestAPIAuthRefusesHostsAnUnauthenticatedListenerDoesNotServe(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	allowed := []string{"hub.internal.example", "192.0.2.10"}
	cases := []struct {
		name       string
		host       string
		path       string
		token      string
		wantStatus int
	}{
		{name: "refuses a rebinding host with its port", host: "rebind.attacker.example:9020", path: "/v1/messages/publish", wantStatus: http.StatusMisdirectedRequest},
		{name: "refuses a rebinding host without a port", host: "rebind.attacker.example", path: "/v1/sessions", wantStatus: http.StatusMisdirectedRequest},
		{name: "refuses a host that only ends in an allowed name", host: "evil-hub.internal.example:9020", path: "/v1/sessions", wantStatus: http.StatusMisdirectedRequest},
		{name: "refuses a non-ASCII letter that lower-cases to an allowed one", host: "HUB.\u0130NTERNAL.EXAMPLE", path: "/v1/sessions", wantStatus: http.StatusMisdirectedRequest},
		{name: "refuses an address that only starts like an allowed one", host: "192.0.2.100:9020", path: "/v1/sessions", wantStatus: http.StatusMisdirectedRequest},
		{name: "refuses an empty host", host: "", path: "/v1/sessions", wantStatus: http.StatusMisdirectedRequest},
		{name: "refuses a non-loopback address", host: "192.0.2.7:9020", path: "/v1/sessions", wantStatus: http.StatusMisdirectedRequest},
		{name: "allows 127.0.0.1 with its port", host: "127.0.0.1:9020", path: "/v1/messages/publish", wantStatus: http.StatusNoContent},
		{name: "allows bracketed ::1 with its port", host: "[::1]:9020", path: "/v1/sessions", wantStatus: http.StatusNoContent},
		{name: "allows bracketed ::1 without a port", host: "[::1]", path: "/v1/sessions", wantStatus: http.StatusNoContent},
		{name: "allows localhost in any case", host: "LocalHost:9020", path: "/v1/sessions", wantStatus: http.StatusNoContent},
		{name: "allows localhost with a trailing dot", host: "localhost.:9020", path: "/v1/sessions", wantStatus: http.StatusNoContent},
		{name: "allows a configured address", host: "192.0.2.10:9020", path: "/v1/sessions", wantStatus: http.StatusNoContent},
		{name: "allows a configured host in any case", host: "HUB.Internal.Example:9020", path: "/v1/messages/publish", wantStatus: http.StatusNoContent},
		{name: "allows a configured host with a trailing dot", host: "hub.internal.example.", path: "/v1/sessions", wantStatus: http.StatusNoContent},
		{name: "leaves health checks to any host", host: "rebind.attacker.example:9020", path: "/healthz", wantStatus: http.StatusNoContent},
		{name: "leaves webhooks to any host", host: "rebind.attacker.example:9020", path: "/webhook/github", wantStatus: http.StatusNoContent},
		{
			// A bearer a rebinding page cannot hold already guards an authenticated listener, which
			// clients reach by names it does not list.
			name: "leaves an authenticated listener to its bearer", host: "listener.internal.example:9020",
			path: "/v1/sessions", token: "expected-token", wantStatus: http.StatusNoContent,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(`{}`))
			request.Header.Set("Content-Type", "application/json")
			request.Host = tc.host
			if tc.token != "" {
				request.Header.Set("Authorization", "Bearer "+tc.token)
			}
			recorder := httptest.NewRecorder()
			var logged bytes.Buffer

			apiAuth(tc.token, nil, allowed, logging.NewWithWriter("test-machine", &logged), next).ServeHTTP(recorder, request)

			if recorder.Code != tc.wantStatus {
				t.Fatalf("Host %q: status = %d, want %d; body = %s", tc.host, recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			if tc.wantStatus == http.StatusMisdirectedRequest {
				if got := recorder.Header().Get("Content-Type"); got != "application/json" {
					t.Fatalf("Content-Type = %q, want application/json", got)
				}
				var body struct{ Error string }
				if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || !strings.Contains(body.Error, "ENVOY_ALLOWED_HOSTS") {
					t.Fatalf("body = %q (%v), want a JSON error naming ENVOY_ALLOWED_HOSTS", recorder.Body.String(), err)
				}
				if !strings.Contains(logged.String(), "listener: request for a host it does not serve") {
					t.Fatalf("log = %q, want the refusal line", logged.String())
				}
			} else if strings.Contains(logged.String(), "does not serve") {
				t.Fatalf("log = %q, want no refusal line for a served request", logged.String())
			}
		})
	}
}

// discoveredAPIVerifier is the /v1 verifier of a listener whose discovery of issuer has succeeded.
func discoveredAPIVerifier(t *testing.T, issuer *oidctest.Issuer) *apiVerifier {
	t.Helper()
	verifier := newAPIVerifier(issuer.URL(), listenerTestAudience)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	verifier.discover(ctx, logging.NewWithWriter("test-machine", io.Discard))
	if !verifier.ready() {
		t.Fatalf("discovery of %s did not succeed", issuer.URL())
	}
	return verifier
}

// Until the issuer's discovery succeeds, /v1 answers 503 "service starting" to every request,
// the shared token's included, and logs each refusal as the starting gates do; the exempt paths
// pass. Discovery is retried, and once the issuer answers it, both bearers are served.
func TestAPIAuthAnswersServiceStartingUntilDiscoverySucceeds(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	issuer := oidctest.New(t)
	signing := issuer.PublishKey(t, "listener-test-key")
	serviceToken := issuer.Mint(t, signing, issuer.Claims(listenerTestSubject, listenerTestAudience))
	issuer.FailDiscovery(http.StatusServiceUnavailable)
	verifier := newAPIVerifier(issuer.URL(), listenerTestAudience)
	var logged cmdtest.LockedBuffer
	logger := logging.NewWithWriter("test-machine", &logged)
	ctx, cancel := context.WithCancel(context.Background())
	discovered := make(chan struct{})
	go func() {
		defer close(discovered)
		verifier.discover(ctx, logger)
	}()
	t.Cleanup(func() {
		cancel()
		<-discovered
	})
	handler := apiAuth("expected-token", verifier, nil, logger, next)
	serve := func(path, bearer string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer "+bearer)
		handler.ServeHTTP(recorder, request)
		return recorder
	}

	waitFor(t, 10*time.Second, "a failed discovery to be logged", func() bool {
		return strings.Contains(logged.String(), "listener API oidc discovery failed; retrying")
	})
	for _, bearer := range []string{"expected-token", serviceToken} {
		if recorder := serve("/v1/sessions", bearer); recorder.Code != http.StatusServiceUnavailable || recorder.Body.String() != "{\"error\":\"service starting\"}\n" {
			t.Fatalf("/v1 before discovery = %d %q, want 503 service starting", recorder.Code, recorder.Body.String())
		}
	}
	if !strings.Contains(logged.String(), `"msg":"request refused while starting"`) || !strings.Contains(logged.String(), `"gate":"oidc"`) {
		t.Fatalf("the refusal was not logged:\n%s", logged.String())
	}
	if recorder := serve("/healthz", ""); recorder.Code != http.StatusNoContent {
		t.Fatalf("/healthz before discovery = %d, want the exempt path passed on", recorder.Code)
	}

	issuer.FailDiscovery(0)
	select {
	case <-discovered:
	case <-time.After(10 * time.Second):
		t.Fatalf("discovery did not succeed within 10 s of the issuer answering:\n%s", logged.String())
	}
	for _, bearer := range []string{"expected-token", serviceToken} {
		if recorder := serve("/v1/sessions", bearer); recorder.Code != http.StatusNoContent {
			t.Fatalf("/v1 after discovery = %d %q, want it served", recorder.Code, recorder.Body.String())
		}
	}
}

// An issuer that keeps failing discovery is read again 1 s after the first failure, then after a
// wait twice the last one, never more than 30 s apart.
func TestOIDCDiscoveryRetriesBackOffFromOneSecondToThirty(t *testing.T) {
	var waits []time.Duration
	for wait := oidcDiscoveryFirstRetry; len(waits) < 8; wait = nextDiscoveryRetry(wait) {
		waits = append(waits, wait)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second}
	if !slices.Equal(waits, want) {
		t.Fatalf("discovery retry waits = %v, want %v", waits, want)
	}
}

// A listener whose OIDC issuer does not answer discovery at start stays up and unready: /healthz
// answers 503 "starting" and /v1 503 "service starting" while it retries, though NATS is up and its
// stores are open; once the issuer answers, /healthz answers 200 and /v1 serves.
func TestAListenerWhoseOIDCIssuerFailsAtStartServesOnceItAnswers(t *testing.T) {
	issuer := oidctest.New(t)
	issuer.PublishKey(t, "listener-test-key")
	issuer.FailDiscovery(http.StatusServiceUnavailable)
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), testnats.URL(t), "box-oidc",
		"ENVOY_OIDC_ISSUER="+issuer.URL(), "ENVOY_OIDC_AUDIENCE="+listenerTestAudience)
	listener.WaitForOutput(t, "listener API oidc discovery failed; retrying", "envoy-listener ready (NATS connected)")

	if status, body := callListener(t, listener.port, http.MethodGet, "/healthz", ""); status != http.StatusServiceUnavailable || !strings.Contains(body, `"status":"starting"`) {
		t.Fatalf("/healthz while discovery fails = %d %s, want 503 starting", status, body)
	}
	if status, body := callListener(t, listener.port, http.MethodGet, "/v1/sessions", ""); status != http.StatusServiceUnavailable || !strings.Contains(body, "service starting") {
		t.Fatalf("/v1 while discovery fails = %d %s, want 503 service starting", status, body)
	}
	select {
	case <-listener.Exited:
		t.Fatalf("the listener exited while discovery failed:\n%s", listener.Output.String())
	default:
	}

	issuer.FailDiscovery(0)
	listener.waitHealthy(t)
	if status, body := callListener(t, listener.port, http.MethodGet, "/healthz", ""); status != http.StatusOK {
		t.Fatalf("/healthz once the issuer answers = %d %s, want 200", status, body)
	}
	if status, body := callListener(t, listener.port, http.MethodGet, "/v1/sessions", ""); status != http.StatusOK {
		t.Fatalf("/v1 once the issuer answers = %d %s, want 200", status, body)
	}
}
