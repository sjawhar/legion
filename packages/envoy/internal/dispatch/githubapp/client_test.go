package githubapp

import (
	"context"
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
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
)

func testKeyPEM(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate fixture key: %v", err)
	}
	encoded := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return key, string(encoded)
}

// fakeGitHub is an httptest GitHub App API: one installation per seeded
// repository, token minting, and the repository read the minted token proves.
type fakeGitHub struct {
	t *testing.T
	// installations maps "owner/repo" to its Contents permission; a missing
	// repo answers 404.
	installations  map[string]string
	installationID int64
	tokenMints     int
	tokenExpiresAt time.Time
	publicKey      *rsa.PublicKey
	clientID       string
}

func (f *fakeGitHub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, r *http.Request) {
		f.verifyAppJWT(r)
		contents, ok := f.installations[r.PathValue("owner")+"/"+r.PathValue("repo")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		response := map[string]any{
			"id":          f.installationID,
			"app_slug":    "dispatch-test",
			"permissions": map[string]string{"contents": contents},
		}
		if contents == "" {
			response["permissions"] = map[string]string{}
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			f.t.Errorf("encode installation: %v", err)
		}
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		f.verifyAppJWT(r)
		f.tokenMints++
		w.WriteHeader(http.StatusCreated)
		if err := json.NewEncoder(w).Encode(map[string]any{
			"token":      fmt.Sprintf("ghs_fake_%d_%d", f.installationID, f.tokenMints),
			"expires_at": f.tokenExpiresAt.Format(time.RFC3339),
		}); err != nil {
			f.t.Errorf("encode token: %v", err)
		}
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ghs_fake_") {
			f.t.Errorf("repository read used %q, want an installation token", r.Header.Get("Authorization"))
		}
		if _, ok := f.installations[r.PathValue("owner")+"/"+r.PathValue("repo")]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"full_name":%q}`, r.PathValue("owner")+"/"+r.PathValue("repo"))
	})
	return mux
}

// verifyAppJWT checks the Authorization JWT verifies against the fixture
// public key and carries the claims GitHub requires.
func (f *fakeGitHub) verifyAppJWT(r *http.Request) {
	f.t.Helper()
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		f.t.Errorf("app endpoint called with Authorization %q, want a bearer JWT", r.Header.Get("Authorization"))
		return
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		f.t.Errorf("JWT has %d segments, want 3", len(parts))
		return
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		f.t.Errorf("decode JWT header: %v", err)
		return
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil || header.Alg != "RS256" || header.Typ != "JWT" {
		f.t.Errorf("JWT header %s, want RS256/JWT", headerJSON)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		f.t.Errorf("decode JWT signature: %v", err)
		return
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(f.publicKey, crypto.SHA256, digest[:], signature); err != nil {
		f.t.Errorf("JWT signature does not verify against the fixture key: %v", err)
	}
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		f.t.Errorf("decode JWT claims: %v", err)
		return
	}
	var claims struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		f.t.Errorf("parse JWT claims: %v", err)
		return
	}
	if claims.Iss != f.clientID {
		f.t.Errorf("JWT iss %q, want client ID %q", claims.Iss, f.clientID)
	}
	now := time.Now().Unix()
	if claims.Iat > now-30 {
		f.t.Errorf("JWT iat %d is not backdated (now %d)", claims.Iat, now)
	}
	if claims.Exp <= now || claims.Exp > now+10*60 {
		f.t.Errorf("JWT exp %d outside (now, now+10m] (now %d)", claims.Exp, now)
	}
}

func newTestClient(t *testing.T, fake *fakeGitHub) *Client {
	t.Helper()
	key, pemText := testKeyPEM(t)
	fake.publicKey = &key.PublicKey
	fake.clientID = "Iv1.testclient"
	server := httptest.NewServer(fake.handler())
	t.Cleanup(server.Close)
	client, err := New(&auth.AppConfig{ClientID: "Iv1.testclient", ClientSecret: "secret", PEM: pemText}, server.URL)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if client == nil {
		t.Fatal("new client: got nil for a configured app")
	}
	return client
}

func TestCheckSourceVerifiesJWTAndResolvesInstallation(t *testing.T) {
	fake := &fakeGitHub{
		t:              t,
		installations:  map[string]string{"legion/arch": "write"},
		installationID: 4242,
		tokenExpiresAt: time.Now().Add(time.Hour),
	}
	client := newTestClient(t, fake)
	source, err := client.CheckSource(context.Background(), "legion", "arch")
	if err != nil {
		t.Fatalf("check source: %v", err)
	}
	if source.InstallationID != 4242 || source.AppSlug != "dispatch-test" {
		t.Fatalf("source: got %+v", source)
	}
}

func TestInstallationMissingIsErrNoInstallation(t *testing.T) {
	fake := &fakeGitHub{t: t, installations: map[string]string{}, tokenExpiresAt: time.Now().Add(time.Hour)}
	client := newTestClient(t, fake)
	_, err := client.CheckSource(context.Background(), "legion", "missing")
	if !errors.Is(err, ErrNoInstallation) {
		t.Fatalf("missing installation: got %v, want ErrNoInstallation", err)
	}
	if !strings.Contains(err.Error(), "legion/missing") {
		t.Fatalf("error does not name the repository: %v", err)
	}
}

func TestContentsPermissionGatesTheCheck(t *testing.T) {
	for _, test := range []struct {
		contents string
		wantErr  bool
	}{
		{contents: "read"},
		{contents: "write"},
		{contents: "none", wantErr: true},
		{contents: "", wantErr: true},
	} {
		t.Run("contents="+test.contents, func(t *testing.T) {
			fake := &fakeGitHub{
				t:              t,
				installations:  map[string]string{"legion/arch": test.contents},
				installationID: 7,
				tokenExpiresAt: time.Now().Add(time.Hour),
			}
			client := newTestClient(t, fake)
			_, err := client.CheckSource(context.Background(), "legion", "arch")
			if test.wantErr {
				if !errors.Is(err, ErrNoContentsRead) {
					t.Fatalf("contents %q: got %v, want ErrNoContentsRead", test.contents, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("contents %q: %v", test.contents, err)
			}
		})
	}
}

func TestTokenCachesUntilNearExpiry(t *testing.T) {
	fake := &fakeGitHub{
		t:              t,
		installations:  map[string]string{"legion/arch": "read"},
		installationID: 9,
		tokenExpiresAt: time.Now().Add(time.Hour),
	}
	client := newTestClient(t, fake)
	first, err := client.Token(context.Background(), 9)
	if err != nil {
		t.Fatalf("first token: %v", err)
	}
	second, err := client.Token(context.Background(), 9)
	if err != nil {
		t.Fatalf("second token: %v", err)
	}
	if fake.tokenMints != 1 {
		t.Fatalf("token mints: got %d, want 1 (second call served from cache)", fake.tokenMints)
	}
	if first != second {
		t.Fatalf("cached token changed: %q then %q", first, second)
	}

	// Within five minutes of expiry the cache no longer serves the token: age the
	// cached entry rather than the clock so the re-mint's JWT stays verifiable.
	client.mu.Lock()
	client.tokens[9] = cachedToken{token: first, expires: client.now().Add(4 * time.Minute)}
	client.mu.Unlock()
	if _, err := client.Token(context.Background(), 9); err != nil {
		t.Fatalf("re-mint near expiry: %v", err)
	}
	if fake.tokenMints != 2 {
		t.Fatalf("token mints after expiry: got %d, want 2", fake.tokenMints)
	}
}

func TestNilClientAnswersErrNoAppKey(t *testing.T) {
	client, err := New(nil, "")
	if err != nil || client != nil {
		t.Fatalf("New(nil): got (%v, %v), want (nil, nil)", client, err)
	}
	client, err = New(&auth.AppConfig{ClientID: "Iv1.x", ClientSecret: "s"}, "")
	if err != nil || client != nil {
		t.Fatalf("New(no PEM): got (%v, %v), want (nil, nil)", client, err)
	}
	if _, err := client.CheckSource(context.Background(), "legion", "arch"); !errors.Is(err, ErrNoAppKey) {
		t.Fatalf("nil client check: got %v, want ErrNoAppKey", err)
	}
}

func TestNewRejectsMalformedKey(t *testing.T) {
	if _, err := New(&auth.AppConfig{ClientID: "Iv1.x", ClientSecret: "s", PEM: "not a key"}, ""); err == nil {
		t.Fatal("malformed PEM accepted")
	}
}
