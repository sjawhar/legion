package secrets_test

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/secrets"
)

type mintedInstallation struct {
	InstallationID int64     `json:"installation_id"`
	Token          string    `json:"token"`
	ExpiresAt      time.Time `json:"expires_at"`
}

type mintedGitHubApp struct {
	Installations map[string]mintedInstallation `json:"installations"`
}

func githubAppValue(t *testing.T, appID string, key *rsa.PrivateKey) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]string{
		"app_id":      appID,
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})),
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func verifyAppJWT(t *testing.T, authorization string, public *rsa.PublicKey, appID string, now time.Time) {
	t.Helper()
	jwt := strings.TrimPrefix(authorization, "Bearer ")
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("authorization has %d JWT parts, want 3", len(parts))
	}
	decode := func(part string, destination any) {
		t.Helper()
		bytes, err := base64.RawURLEncoding.DecodeString(part)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(bytes, destination); err != nil {
			t.Fatal(err)
		}
	}
	var header struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
	}
	decode(parts[0], &header)
	if header.Algorithm != "RS256" || header.Type != "JWT" {
		t.Fatalf("JWT header = %+v, want RS256 JWT", header)
	}
	var claims struct {
		IssuedAt  int64  `json:"iat"`
		ExpiresAt int64  `json:"exp"`
		Issuer    string `json:"iss"`
	}
	decode(parts[1], &claims)
	if claims.IssuedAt != now.Add(-time.Minute).Unix() || claims.ExpiresAt != now.Add(9*time.Minute).Unix() || claims.Issuer != appID {
		t.Fatalf("JWT claims = %+v, want iat=%d exp=%d iss=%q", claims, now.Add(-time.Minute).Unix(), now.Add(9*time.Minute).Unix(), appID)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(public, crypto.SHA256, digest[:], signature); err != nil {
		t.Fatalf("JWT signature: %v", err)
	}
}

func TestGitHubAppMintSignsJWTAndFollowsPagination(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	const appID = "123456"
	var posts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		verifyAppJWT(t, r.Header.Get("Authorization"), &key.PublicKey, appID, now)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations" && r.URL.Query().Get("page") == "1":
			installations := make([]map[string]any, 100)
			for index := range installations {
				installations[index] = map[string]any{"id": index + 1, "account": map[string]string{"login": fmt.Sprintf("owner-%03d", index+1)}}
			}
			_ = json.NewEncoder(w).Encode(installations)
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations" && r.URL.Query().Get("page") == "2":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 101, "account": map[string]string{"login": "second-owner"}},
				{"id": 102, "account": map[string]string{"login": "third-owner"}},
			})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/app/installations/") && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			posts++
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/app/installations/"), "/access_tokens")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token":      "installation-token-" + id,
				"expires_at": now.Add(time.Hour).Format(time.RFC3339),
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	value, err := (secrets.GitHubApp{Base: server.URL, HTTP: server.Client(), Now: func() time.Time { return now }}).Mint(t.Context(), githubAppValue(t, appID, key))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	var got mintedGitHubApp
	if err := json.Unmarshal([]byte(value), &got); err != nil {
		t.Fatalf("Mint output: %v", err)
	}
	if len(got.Installations) != 102 || posts != 102 {
		t.Fatalf("Mint returned %d installations after %d token requests, want 102", len(got.Installations), posts)
	}
	for owner, wantID := range map[string]int64{"owner-001": 1, "second-owner": 101, "third-owner": 102} {
		installation, ok := got.Installations[owner]
		if !ok || installation.InstallationID != wantID || installation.Token != "installation-token-"+strconv.FormatInt(wantID, 10) || !installation.ExpiresAt.Equal(now.Add(time.Hour)) {
			t.Errorf("installation %q = %+v, want id=%d minted token and expiry", owner, installation, wantID)
		}
	}
}

func TestGitHubAppMintRefusesAppWithoutInstallations(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations" {
			t.Fatalf("unexpected request %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]any{})
	}))
	defer server.Close()

	_, err = (secrets.GitHubApp{Base: server.URL, HTTP: server.Client()}).Mint(t.Context(), githubAppValue(t, "123456", key))
	if err == nil || !strings.Contains(err.Error(), "github app 123456 has no installation") {
		t.Fatalf("Mint error = %v, want no-installation error", err)
	}
}

// TestGitHubAppMintSkipsSuspendedInstallations: GitHub refuses a token for an installation the
// App is suspended on, so Mint leaves that owner out rather than failing every other owner, and
// an App suspended everywhere has no installation to mint for.
func TestGitHubAppMintSkipsSuspendedInstallations(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		installations []map[string]any
		want          []string
	}{
		"one suspended": {
			installations: []map[string]any{
				{"id": 1, "account": map[string]string{"login": "active-owner"}, "suspended_at": nil},
				{"id": 2, "account": map[string]string{"login": "suspended-owner"}, "suspended_at": "2026-10-01T00:00:00Z"},
			},
			want: []string{"active-owner"},
		},
		"all suspended": {
			installations: []map[string]any{
				{"id": 2, "account": map[string]string{"login": "suspended-owner"}, "suspended_at": "2026-10-01T00:00:00Z"},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
					_ = json.NewEncoder(w).Encode(tc.installations)
				case r.Method == http.MethodPost && r.URL.Path == "/app/installations/1/access_tokens":
					w.WriteHeader(http.StatusCreated)
					_ = json.NewEncoder(w).Encode(map[string]string{"token": "installation-token-1", "expires_at": "2026-10-23T13:00:00Z"})
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.Error(w, "suspended", http.StatusForbidden)
				}
			}))
			defer server.Close()

			value, err := (secrets.GitHubApp{Base: server.URL, HTTP: server.Client()}).Mint(t.Context(), githubAppValue(t, "123456", key))
			if len(tc.want) == 0 {
				if err == nil || !strings.Contains(err.Error(), "github app 123456 has no installation") {
					t.Fatalf("Mint = %q, %v; want no-installation error", value, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Mint: %v", err)
			}
			var got mintedGitHubApp
			if err := json.Unmarshal([]byte(value), &got); err != nil {
				t.Fatalf("Mint output: %v", err)
			}
			if len(got.Installations) != len(tc.want) {
				t.Fatalf("Mint installations = %v, want %v", got.Installations, tc.want)
			}
			for _, owner := range tc.want {
				if _, ok := got.Installations[owner]; !ok {
					t.Fatalf("Mint installations = %v, want %q", got.Installations, owner)
				}
			}
		})
	}
}

func TestGitHubAppMintNeverLeaksKeyOnFailure(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	value := githubAppValue(t, "123456", key)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "credentials rejected", http.StatusUnauthorized)
	}))
	defer server.Close()

	_, err = (secrets.GitHubApp{Base: server.URL, HTTP: server.Client()}).Mint(t.Context(), value)
	if err == nil {
		t.Fatal("Mint succeeded after a GitHub 401")
	}
	if strings.Contains(err.Error(), value) || strings.Contains(err.Error(), "PRIVATE KEY") {
		t.Fatalf("Mint error leaked the App key: %q", err)
	}
}

func TestGitHubAppMintRejectsCorruptKeyWithoutLeakingIt(t *testing.T) {
	value := `{"app_id":"123456","private_key":"-----BEGIN RSA PRIVATE KEY-----\nprivate-material\n-----END RSA PRIVATE KEY-----"}`
	_, err := (secrets.GitHubApp{}).Mint(t.Context(), value)
	if err == nil || strings.Contains(err.Error(), "private-material") || strings.Contains(err.Error(), "PRIVATE KEY") {
		t.Fatalf("Mint corrupt key = %v, want a parse error without key material", err)
	}
}

func TestGitHubAppMintAcceptsPKCS8Key(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	value, err := json.Marshal(map[string]string{"app_id": "123456", "private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}))})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 7, "account": map[string]string{"login": "owner"}}})
		case "/app/installations/7/access_tokens":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "token-7", "expires_at": "2026-10-23T13:00:00Z"})
		default:
			t.Fatalf("unexpected request %s", r.URL.Path)
		}
	}))
	defer server.Close()
	if _, err := (secrets.GitHubApp{Base: server.URL, HTTP: server.Client()}).Mint(t.Context(), string(value)); err != nil {
		t.Fatalf("Mint PKCS8 = %v", err)
	}
}
