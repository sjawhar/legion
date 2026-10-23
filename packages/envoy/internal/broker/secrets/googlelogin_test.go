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

	got, err := (secrets.GoogleLogin{TokenURL: server.URL, HTTP: server.Client()}).Mint(t.Context(), value)
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

	_, err := (secrets.GoogleLogin{TokenURL: server.URL, HTTP: server.Client()}).Mint(t.Context(), value)
	if err == nil || !strings.Contains(err.Error(), "invalid_rapt") {
		t.Fatalf("Mint error = %v, want invalid_rapt", err)
	}
	for _, credential := range []string{value, "client-secret", "refresh-token", "access-token"} {
		if strings.Contains(err.Error(), credential) {
			t.Fatalf("Mint error leaked %q: %q", credential, err)
		}
	}
}
