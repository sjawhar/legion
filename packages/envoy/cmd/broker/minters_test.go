package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// TestEachKindReachesItsOwnMinter: the broker's minters send a github-app secret's value to the
// GitHub App minter and a google-login secret's to the Google login minter, each against an
// endpoint of its own, so wiring a kind to the other kind's minter, which would refuse the value,
// fails here.
func TestEachKindReachesItsOwnMinter(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 7, "account": map[string]string{"login": "acme-org"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/7/access_tokens":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "github-installation-token", "expires_at": "2026-10-24T13:00:00Z"})
		default:
			t.Errorf("GitHub endpoint asked %s %s", r.Method, r.URL.Path)
		}
	}))
	defer github.Close()
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "google-access-token"})
	}))
	defer google.Close()
	minters := kindMinters(secrets.GitHubApp{Base: github.URL, HTTP: github.Client()}, secrets.GoogleLogin{TokenURL: google.URL, HTTP: google.Client()})

	app, err := json.Marshal(map[string]string{
		"app_id":      "123456",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})),
	})
	if err != nil {
		t.Fatal(err)
	}
	minted, err := minters[policy.KindGitHubApp](context.Background(), secrets.MintRequest{Value: string(app), GitHubOwners: []string{"acme-org"}})
	if err != nil || !strings.Contains(minted, "github-installation-token") {
		t.Fatalf("github-app minter = %q, %v; want the GitHub endpoint's installation token", minted, err)
	}
	login := `{"type":"authorized_user","client_id":"c","client_secret":"s","refresh_token":"r"}`
	minted, err = minters[policy.KindGoogleLogin](context.Background(), secrets.MintRequest{Value: login})
	if err != nil || minted != "google-access-token" {
		t.Fatalf("google-login minter = %q, %v; want the Google endpoint's access token", minted, err)
	}
}
