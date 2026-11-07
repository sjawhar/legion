package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/broker/config"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
)

// TestEachKindReachesItsOwnMinter: the broker's minters send a github-app secret's value to the
// GitHub App minter, a google-login secret's to the Google login minter and a
// google-service-account secret's to the service account minter, each against an endpoint of its
// own, so wiring a kind to another kind's minter, which would refuse the value, fails here.
func TestEachKindReachesItsOwnMinter(t *testing.T) {
	_, pemText := githubapptest.Key(t)
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
	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects/-/serviceAccounts/dev-aditya@theorem-agent-identities.iam.gserviceaccount.com:generateAccessToken" {
			t.Errorf("IAM endpoint asked %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": "service-account-access-token"})
	}))
	defer iam.Close()
	minters := kindMinters(
		secrets.GitHubApp{Base: github.URL, HTTP: github.Client()},
		secrets.GoogleLogin{TokenURL: google.URL, HTTP: google.Client()},
		secrets.AzureLogin{},
		&secrets.GoogleServiceAccount{BaseURL: iam.URL, HTTP: iam.Client(), Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "broker-own-token"})},
	)

	app, err := json.Marshal(map[string]string{"app_id": "123456", "private_key": pemText})
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
	account := `{"service_account":"dev-aditya@theorem-agent-identities.iam.gserviceaccount.com"}`
	minted, err = minters[policy.KindGoogleServiceAccount](context.Background(), secrets.MintRequest{Value: account})
	if err != nil || minted != "service-account-access-token" {
		t.Fatalf("google-service-account minter = %q, %v; want the IAM endpoint's access token", minted, err)
	}
}

// TestKindMinters: the map kindMinters answers holds one minter for every kind the policy serves
// and nothing else, so a kind added to policy.Kinds with no minter behind it, or a minter left
// under a key no kind names, fails here rather than when a session asks for that secret.
func TestKindMinters(t *testing.T) {
	minters := kindMinters(secrets.GitHubApp{}, secrets.GoogleLogin{}, secrets.AzureLogin{}, &secrets.GoogleServiceAccount{})
	for _, kind := range policy.Kinds {
		if minters[kind] == nil {
			t.Errorf("kindMinters has no minter for kind %q, which the policy serves", kind)
		}
	}
	for kind := range minters {
		if !policy.ValidKind(kind) {
			t.Errorf("kindMinters holds a minter under %q, which is no kind the policy serves", kind)
		}
	}
	if len(minters) != len(policy.Kinds) {
		t.Errorf("kindMinters has %d minters for %d kinds: %v", len(minters), len(policy.Kinds), minters)
	}
}

func TestRequestMachineReleasesAzureLoginFromTheConfiguredFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "azure-token")
	const token = "eyJhbGciOiJSUzI1NiJ9.eyJleHAiOjQxMDI0NDQ4MDB9.c2lnbmF0dXJl"
	if err := os.WriteFile(file, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	machine := newRequestMachine(config.Config{AzureTokenFile: file, AzureLoginSecret: "AZURE_LOGIN"}, nil, nil, nil, nil)
	for _, kind := range policy.Kinds {
		if machine.Minters[kind] == nil {
			t.Fatalf("request machine has no minter for %s", kind)
		}
	}
	mint := machine.Minters["azure-login"]
	if mint == nil {
		t.Fatal("request machine has no Azure login minter")
	}
	got, err := mint(t.Context(), secrets.MintRequest{Name: "AZURE_LOGIN", HumanTier: true, Value: `{"tenant_id":"tenant","client_id":"client"}`})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	var value map[string]string
	if json.Unmarshal([]byte(got), &value) != nil || len(value) != 4 || value["tenant_id"] != "tenant" || value["client_id"] != "client" || value["federated_token"] != token || value["expires_at"] != "2100-01-01T00:00:00Z" {
		t.Fatal("request machine did not release the Azure login from its configured token file")
	}
}
