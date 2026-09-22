package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
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
	verifier, err := oidc.New(context.Background(), issuer.URL(), listenerTestAudience)
	if err != nil {
		t.Fatalf("oidc.New(%q, %q): %v", issuer.URL(), listenerTestAudience, err)
	}

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
			if tc.authorize != "" {
				request.Header.Set("Authorization", tc.authorize)
			}

			var configured *oidc.Verifier
			if tc.oidc {
				configured = verifier
			}
			var logged bytes.Buffer
			apiAuth(tc.token, configured, logging.NewWithWriter("test-machine", &logged), next).
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
	issuer := oidctest.New(t)
	verifier, err := oidc.New(context.Background(), issuer.URL(), listenerTestAudience)
	if err != nil {
		t.Fatalf("oidc.New(%q, %q): %v", issuer.URL(), listenerTestAudience, err)
	}

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
			var configured *oidc.Verifier
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
