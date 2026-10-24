package secrets_test

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// testAppKey is the one App key every test in this package signs with: generating a 2048-bit key
// costs more than any of these tests does.
var testAppKey = sync.OnceValues(func() (*rsa.PrivateKey, error) { return rsa.GenerateKey(rand.Reader, 2048) })

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

// mintFor is a release of value for owners, the github-owners tag's lowercase logins.
func mintFor(value string, owners ...string) secrets.MintRequest {
	return secrets.MintRequest{Value: value, GitHubOwners: owners}
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
	key, err := testAppKey()
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

	owners := []string{"second-owner", "third-owner"}
	for index := 1; index <= 100; index++ {
		owners = append(owners, fmt.Sprintf("owner-%03d", index))
	}
	value, err := (secrets.GitHubApp{Base: server.URL, HTTP: server.Client(), Now: func() time.Time { return now }}).Mint(t.Context(), mintFor(githubAppValue(t, appID, key), owners...))
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
	key, err := testAppKey()
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

	_, err = (secrets.GitHubApp{Base: server.URL, HTTP: server.Client()}).Mint(t.Context(), mintFor(githubAppValue(t, "123456", key), "acme"))
	if err == nil || !strings.Contains(err.Error(), "github app 123456 has no installation") {
		t.Fatalf("Mint error = %v, want no-installation error", err)
	}
}

// TestGitHubAppMintSkipsSuspendedInstallations: GitHub refuses a token for an installation the
// App is suspended on, so Mint leaves that owner out rather than failing every other owner, and
// an App suspended everywhere has no installation to mint for.
func TestGitHubAppMintSkipsSuspendedInstallations(t *testing.T) {
	key, err := testAppKey()
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

			value, err := (secrets.GitHubApp{Base: server.URL, HTTP: server.Client()}).Mint(t.Context(), mintFor(githubAppValue(t, "123456", key), "active-owner", "suspended-owner"))
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
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	value := githubAppValue(t, "123456", key)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "credentials rejected", http.StatusUnauthorized)
	}))
	defer server.Close()

	_, err = (secrets.GitHubApp{Base: server.URL, HTTP: server.Client()}).Mint(t.Context(), mintFor(value, "acme"))
	if err == nil {
		t.Fatal("Mint succeeded after a GitHub 401")
	}
	if strings.Contains(err.Error(), value) || strings.Contains(err.Error(), "PRIVATE KEY") {
		t.Fatalf("Mint error leaked the App key: %q", err)
	}
}

func TestGitHubAppMintRejectsCorruptKeyWithoutLeakingIt(t *testing.T) {
	value := `{"app_id":"123456","private_key":"-----BEGIN RSA PRIVATE KEY-----\nprivate-material\n-----END RSA PRIVATE KEY-----"}`
	_, err := (secrets.GitHubApp{}).Mint(t.Context(), mintFor(value, "acme"))
	if err == nil || strings.Contains(err.Error(), "private-material") || strings.Contains(err.Error(), "PRIVATE KEY") {
		t.Fatalf("Mint corrupt key = %v, want a parse error without key material", err)
	}
}

func TestGitHubAppMintAcceptsPKCS8Key(t *testing.T) {
	key, err := testAppKey()
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
	if _, err := (secrets.GitHubApp{Base: server.URL, HTTP: server.Client()}).Mint(t.Context(), mintFor(string(value), "owner")); err != nil {
		t.Fatalf("Mint PKCS8 = %v", err)
	}
}

// TestGitHubAppMintNamesTheClassOfEachFailure: every failure wraps the class the broker's log
// names, so an operator can tell a bad stored key from GitHub refusing from GitHub being down.
func TestGitHubAppMintNamesTheClassOfEachFailure(t *testing.T) {
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	answer := func(status int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "no", status)
		}))
	}
	refused, down := answer(http.StatusUnauthorized), answer(http.StatusBadGateway)
	defer refused.Close()
	defer down.Close()
	for name, tc := range map[string]struct {
		app   secrets.GitHubApp
		value string
		class string
	}{
		"stored value is not JSON": {secrets.GitHubApp{}, "not json", "stored-value"},
		"stored key is corrupt":    {secrets.GitHubApp{}, `{"app_id":"1","private_key":"x"}`, "stored-value"},
		"GitHub answers 401":       {secrets.GitHubApp{Base: refused.URL, HTTP: refused.Client()}, githubAppValue(t, "1", key), "refused"},
		"GitHub answers 502":       {secrets.GitHubApp{Base: down.URL, HTTP: down.Client()}, githubAppValue(t, "1", key), "upstream"},
		"GitHub cannot be reached": {secrets.GitHubApp{Base: "http://127.0.0.1:1"}, githubAppValue(t, "1", key), "upstream"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := tc.app.Mint(t.Context(), mintFor(tc.value, "acme"))
			if got := secrets.MintErrorClass(err); err == nil || got != tc.class {
				t.Fatalf("Mint = %v (class %s); want class %s", err, got, tc.class)
			}
		})
	}
}

// TestGitHubAppMintRefusesAnAnswerPastItsCap: an upstream answer is read to 1 MiB and no further,
// so a GitHub that streams without end costs the broker a bounded read.
func TestGitHubAppMintRefusesAnAnswerPastItsCap(t *testing.T) {
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("["))
		_, _ = w.Write(bytes.Repeat([]byte(" "), 2<<20))
	}))
	defer server.Close()
	_, err = (secrets.GitHubApp{Base: server.URL, HTTP: server.Client()}).Mint(t.Context(), mintFor(githubAppValue(t, "1", key), "acme"))
	if !errors.Is(err, secrets.ErrMintUpstream) || !strings.Contains(err.Error(), "more than 1048576 bytes") {
		t.Fatalf("Mint against an endless answer = %v; want ErrMintUpstream naming the cap", err)
	}
}

// TestGitHubAppMintLeavesOutAnAccountItsOwnersDoNotList: a public App can be installed on any
// account, so Mint asks GitHub for a token only for an installation on an account github-owners
// lists, matched without regard to case, and leaves out every other installation without a
// request for it. An App installed only on accounts the list does not name mints nothing.
func TestGitHubAppMintLeavesOutAnAccountItsOwnersDoNotList(t *testing.T) {
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	var asked []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 1, "account": map[string]string{"login": "Acme-Org"}},
				{"id": 2, "account": map[string]string{"login": "outsider"}},
				{"id": 3, "account": map[string]string{"login": "ada"}},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			asked = append(asked, r.URL.Path)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "token-" + r.URL.Path, "expires_at": "2026-10-23T13:00:00Z"})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	app := secrets.GitHubApp{Base: server.URL, HTTP: server.Client()}

	value, err := app.Mint(t.Context(), mintFor(githubAppValue(t, "123456", key), "acme-org", "ada"))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	var got mintedGitHubApp
	if err := json.Unmarshal([]byte(value), &got); err != nil {
		t.Fatalf("Mint output: %v", err)
	}
	if _, ok := got.Installations["outsider"]; ok || len(got.Installations) != 2 {
		t.Fatalf("Mint installations = %v, want Acme-Org and ada only", got.Installations)
	}
	if _, ok := got.Installations["Acme-Org"]; !ok {
		t.Fatalf("Mint installations = %v, want Acme-Org matched without regard to case", got.Installations)
	}
	if len(asked) != 2 || slices.Contains(asked, "/app/installations/2/access_tokens") {
		t.Fatalf("token requests = %v, want none for the outsider's installation", asked)
	}

	_, err = app.Mint(t.Context(), mintFor(githubAppValue(t, "123456", key), "nobody-installed"))
	if secrets.MintErrorClass(err) != "refused" || !strings.Contains(err.Error(), "github-owners") {
		t.Fatalf("Mint with no listed installation = %v, want refused naming github-owners", err)
	}
	_, err = app.Mint(t.Context(), mintFor(githubAppValue(t, "123456", key)))
	if secrets.MintErrorClass(err) != "refused" {
		t.Fatalf("Mint with no github-owners = %v, want refused", err)
	}
}

// TestGitHubAppMintSkipsAnInstallationThatNamesNoAccount: an installation whose account carries
// neither a login nor a slug names no account github-owners can list, so it is left out like any
// unlisted installation, and the listed owners still mint: one outsider's malformed installation
// never fails the whole release.
func TestGitHubAppMintSkipsAnInstallationThatNamesNoAccount(t *testing.T) {
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	var asked []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 1, "account": map[string]string{}},
				{"id": 2, "account": map[string]string{"login": "ada"}},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			asked = append(asked, r.URL.Path)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "token-" + r.URL.Path, "expires_at": "2026-10-23T13:00:00Z"})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	value, err := (secrets.GitHubApp{Base: server.URL, HTTP: server.Client()}).Mint(t.Context(), mintFor(githubAppValue(t, "123456", key), "ada"))
	if err != nil {
		t.Fatalf("Mint beside an installation that names no account: %v", err)
	}
	var got mintedGitHubApp
	if err := json.Unmarshal([]byte(value), &got); err != nil {
		t.Fatalf("Mint output: %v", err)
	}
	if _, ok := got.Installations["ada"]; !ok || len(got.Installations) != 1 {
		t.Fatalf("Mint installations = %v, want ada alone", got.Installations)
	}
	if !slices.Equal(asked, []string{"/app/installations/2/access_tokens"}) {
		t.Fatalf("token requests = %v, want ada's alone", asked)
	}
}

// TestGitHubAppMintSkipsAnInstallationWithNoID: an installation record whose id is missing, null,
// zero or negative names no installation a token can be asked for, so it is left out, and the
// listed owner's valid installation still mints: no token request is sent for installation 0.
func TestGitHubAppMintSkipsAnInstallationWithNoID(t *testing.T) {
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	var asked []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			_, _ = w.Write([]byte(`[{"account":{"login":"ada"}},{"id":null,"account":{"login":"ada"}},{"id":0,"account":{"login":"ada"}},{"id":-3,"account":{"login":"ada"}},{"id":2,"account":{"login":"ada"}}]`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			asked = append(asked, r.URL.Path)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "token-" + r.URL.Path, "expires_at": "2026-10-23T13:00:00Z"})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	value, err := (secrets.GitHubApp{Base: server.URL, HTTP: server.Client()}).Mint(t.Context(), mintFor(githubAppValue(t, "123456", key), "ada"))
	if err != nil {
		t.Fatalf("Mint beside installations with no id: %v", err)
	}
	var got mintedGitHubApp
	if err := json.Unmarshal([]byte(value), &got); err != nil {
		t.Fatalf("Mint output: %v", err)
	}
	if installation, ok := got.Installations["ada"]; !ok || installation.InstallationID != 2 || len(got.Installations) != 1 {
		t.Fatalf("Mint installations = %v, want ada's installation 2 alone", got.Installations)
	}
	if !slices.Equal(asked, []string{"/app/installations/2/access_tokens"}) {
		t.Fatalf("token requests = %v, want installation 2's alone", asked)
	}
}
