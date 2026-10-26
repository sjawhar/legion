package secrets_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/secrets"
)

func TestGoogleLoginMintExchangesRefreshToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "refresh-token" || r.Form.Get("client_id") != "client-id" || r.Form.Get("client_secret") != "client-secret" {
			t.Fatalf("refresh request form = %v", r.Form)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "access-token"})
	}))
	defer server.Close()
	value := `{"type":"authorized_user","client_id":"client-id","client_secret":"client-secret","refresh_token":"refresh-token"}`

	got, err := (secrets.GoogleLogin{TokenURL: server.URL, HTTP: server.Client()}).Mint(t.Context(), secrets.MintRequest{Value: value})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if got != "access-token" {
		t.Fatalf("Mint = %q, want bare access token", got)
	}
}

func TestGoogleLoginMintNamesInvalidRaptWithoutLeakingCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_subtype": "invalid_rapt"})
	}))
	defer server.Close()
	value := `{"type":"authorized_user","client_id":"client-id","client_secret":"client-secret","refresh_token":"refresh-token"}`

	_, err := (secrets.GoogleLogin{TokenURL: server.URL, HTTP: server.Client()}).Mint(t.Context(), secrets.MintRequest{Value: value})
	if err == nil || !strings.Contains(err.Error(), "invalid_rapt") || secrets.MintErrorClass(err) != "refused" {
		t.Fatalf("Mint error = %v (class %s), want invalid_rapt, refused", err, secrets.MintErrorClass(err))
	}
	for _, credential := range []string{value, "client-secret", "refresh-token", "access-token"} {
		if strings.Contains(err.Error(), credential) {
			t.Fatalf("Mint error leaked %q: %q", credential, err)
		}
	}
}

// TestGoogleLoginMintNamesAGoogleOutageAsUpstream: a 5xx from Google is the upstream's failure,
// not a login that needs a new sign-in, so the broker's log points its operator at Google.
func TestGoogleLoginMintNamesAGoogleOutageAsUpstream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	value := `{"type":"authorized_user","client_id":"client-id","client_secret":"client-secret","refresh_token":"refresh-token"}`
	_, err := (secrets.GoogleLogin{TokenURL: server.URL, HTTP: server.Client()}).Mint(t.Context(), secrets.MintRequest{Value: value})
	if secrets.MintErrorClass(err) != "upstream" || !strings.Contains(err.Error(), "503") {
		t.Fatalf("Mint against a Google 503 = %v (class %s), want upstream naming 503", err, secrets.MintErrorClass(err))
	}
	if _, err := (secrets.GoogleLogin{}).Mint(t.Context(), secrets.MintRequest{Value: `{"type":"service_account"}`}); secrets.MintErrorClass(err) != "stored-value" {
		t.Fatalf("Mint of a service-account value = %v, want stored-value", err)
	}
}

// TestGoogleLoginMintNamesARateLimitAsUpstream: a rate limit is not the login's owner's to fix, so
// Google's rate-limit answers are upstream, never refused: a 429, and a 403 naming the
// rateLimitExceeded reason Google's usage limits give. Google sends no X-RateLimit-Remaining, so
// the status and the body are what tell it. A 4xx that is no rate limit stays refused.
func TestGoogleLoginMintNamesARateLimitAsUpstream(t *testing.T) {
	value := `{"type":"authorized_user","client_id":"client-id","client_secret":"client-secret","refresh_token":"refresh-token"}`
	for _, tc := range []struct {
		name   string
		status int
		body   string
		class  string
	}{
		{"a 429", http.StatusTooManyRequests, `{"error":{"code":429,"message":"Quota exceeded","status":"RESOURCE_EXHAUSTED"}}`, "upstream"},
		{"a 403 rateLimitExceeded", http.StatusForbidden, `{"error":{"code":403,"errors":[{"domain":"usageLimits","reason":"rateLimitExceeded"}]}}`, "upstream"},
		{"a 403 that is no rate limit", http.StatusForbidden, `{"error":"access_denied"}`, "refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			_, err := (secrets.GoogleLogin{TokenURL: server.URL, HTTP: server.Client()}).Mint(t.Context(), secrets.MintRequest{Value: value})
			if got := secrets.MintErrorClass(err); got != tc.class || strings.Contains(err.Error(), "a rate limit") != (tc.class == "upstream") {
				t.Fatalf("Mint against Google's %s = %v (class %s), want class %s", tc.name, err, got, tc.class)
			}
		})
	}
}
