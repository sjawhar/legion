package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/sjawhar/envoy/internal/oidc"
)

const (
	listenerTestAudience = "envoy"
	listenerTestSubject  = "system:serviceaccount:legion:legion-worker"
)

// listenerIssuer is a local OIDC issuer for the listener's own auth tests: an
// httptest server answering /.well-known/openid-configuration and /keys for one
// RSA key it mints tokens with. internal/oidc has an equivalent helper, but it
// is test-only code in that package and so is not importable here.
type listenerIssuer struct {
	server *httptest.Server
	kid    string
	priv   *rsa.PrivateKey
}

func newListenerIssuer(t *testing.T) *listenerIssuer {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	issuer := &listenerIssuer{kid: "listener-test-key", priv: priv}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                issuer.server.URL,
			"authorization_endpoint":                issuer.server.URL + "/auth",
			"token_endpoint":                        issuer.server.URL + "/token",
			"jwks_uri":                              issuer.server.URL + "/keys",
			"id_token_signing_alg_values_supported": []string{string(jose.RS256)},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key:       priv.Public(),
			KeyID:     issuer.kid,
			Algorithm: string(jose.RS256),
			Use:       "sig",
		}}})
	})
	issuer.server = httptest.NewServer(mux)
	t.Cleanup(issuer.server.Close)
	return issuer
}

func (li *listenerIssuer) url() string { return li.server.URL }

// claims is a valid projected service-account token's claim set, for a test to
// spoil one claim at a time.
func (li *listenerIssuer) claims() map[string]any {
	now := time.Now()
	return map[string]any{
		"iss": li.url(),
		"sub": listenerTestSubject,
		"aud": []string{listenerTestAudience},
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(time.Hour).Unix(),
	}
}

func (li *listenerIssuer) mint(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: li.priv, KeyID: li.kid}},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	signed, err := signer.Sign(payload)
	if err != nil {
		t.Fatalf("sign claims: %v", err)
	}
	raw, err := signed.CompactSerialize()
	if err != nil {
		t.Fatalf("serialize token: %v", err)
	}
	return raw
}

func TestAPIAuth(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	issuer := newListenerIssuer(t)
	verifier, err := oidc.New(context.Background(), issuer.url(), listenerTestAudience)
	if err != nil {
		t.Fatalf("oidc.New(%q, %q): %v", issuer.url(), listenerTestAudience, err)
	}

	serviceToken := issuer.mint(t, issuer.claims())

	otherAudience := issuer.claims()
	otherAudience["aud"] = []string{"dispatch"}
	otherAudienceToken := issuer.mint(t, otherAudience)

	const unauthorized = "{\"error\":\"unauthorized\"}\n"

	cases := []struct {
		name       string
		token      string
		oidc       bool
		path       string
		authorize  string
		wantStatus int
		wantBody   string
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
			apiAuth(tc.token, configured, next).ServeHTTP(recorder, request)

			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, tc.wantStatus)
			}
			if body := recorder.Body.String(); body != tc.wantBody {
				t.Fatalf("body = %q, want %q", body, tc.wantBody)
			}
		})
	}
}

func TestValidateListenerAPIAuth(t *testing.T) {
	issuer := newListenerIssuer(t)
	verifier, err := oidc.New(context.Background(), issuer.url(), listenerTestAudience)
	if err != nil {
		t.Fatalf("oidc.New(%q, %q): %v", issuer.url(), listenerTestAudience, err)
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
