package appauth

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
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
)

func testAppKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	return key, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}))
}

func decodeJWTPart(t *testing.T, part string, into any) {
	t.Helper()
	decoded, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		t.Fatalf("decode JWT part: %v", err)
	}
	if err := json.Unmarshal(decoded, into); err != nil {
		t.Fatalf("decode JWT JSON: %v", err)
	}
}

func verifyAppJWT(t *testing.T, token string, publicKey *rsa.PublicKey, now time.Time, appID string) {
	t.Helper()
	var header struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
	}
	var claims struct {
		Issuer  string `json:"iss"`
		Issued  int64  `json:"iat"`
		Expires int64  `json:"exp"`
	}
	parts := splitJWT(t, token)
	decodeJWTPart(t, parts[0], &header)
	decodeJWTPart(t, parts[1], &claims)
	if header.Algorithm != "RS256" || header.Type != "JWT" {
		t.Fatalf("JWT header = %+v, want RS256 JWT", header)
	}
	if claims.Issuer != appID {
		t.Fatalf("JWT issuer = %q, want %q", claims.Issuer, appID)
	}
	if claims.Issued != now.Unix()-60 || claims.Expires != now.Unix()+600 {
		t.Fatalf("JWT claims = %+v, want iat %d and exp %d", claims, now.Unix()-60, now.Unix()+600)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode JWT signature: %v", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(publicKey, crypto.SHA256, digest[:], signature); err != nil {
		t.Fatalf("verify JWT signature: %v", err)
	}
}

func splitJWT(t *testing.T, token string) []string {
	t.Helper()
	var parts []string
	for _, part := range split(token, '.') {
		parts = append(parts, part)
	}
	if len(parts) != 3 {
		t.Fatalf("JWT has %d parts, want 3", len(parts))
	}
	return parts
}

func split(value string, sep byte) []string {
	start := 0
	parts := make([]string, 0, 3)
	for i := 0; i < len(value); i++ {
		if value[i] == sep {
			parts = append(parts, value[start:i])
			start = i + 1
		}
	}
	return append(parts, value[start:])
}

func TestGenerateJWTSignsShippedClaims(t *testing.T) {
	key, privatePEM := testAppKey(t)
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

	token, err := GenerateJWT("12345", privatePEM, now)
	if err != nil {
		t.Fatalf("GenerateJWT: %v", err)
	}

	verifyAppJWT(t, token, &key.PublicKey, now, "12345")
}

func TestAppRoleForEveryClaimRole(t *testing.T) {
	for _, tc := range []struct {
		role claim.Role
		want AppRole
	}{
		{claim.RoleArchitect, Review},
		{claim.RolePlanner, Review},
		{claim.RoleImplementer, Implement},
		{claim.RoleTester, Review},
		{claim.RoleReviewer, Review},
		{claim.RoleMerger, Implement},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			if got := AppRoleFor(tc.role); got != tc.want {
				t.Errorf("AppRoleFor(%q) = %q, want %q", tc.role, got, tc.want)
			}
		})
	}
}

func TestTokensMintsRefreshesAndCachesIdentity(t *testing.T) {
	key, privatePEM := testAppKey(t)
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	exchanges, discoveries, identities := 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jwt := r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/app/installations":
			verifyAppJWT(t, jwt[len("Bearer "):], &key.PublicKey, now, "12345")
			mu.Lock()
			discoveries++
			mu.Unlock()
			fmt.Fprint(w, `[{"id":77,"account":{"login":"Sjawhar"}}]`)
		case "/app/installations/77/access_tokens":
			if r.Method != http.MethodPost {
				t.Fatalf("method = %s, want POST", r.Method)
			}
			verifyAppJWT(t, jwt[len("Bearer "):], &key.PublicKey, now, "12345")
			mu.Lock()
			exchanges++
			exchange := exchanges
			mu.Unlock()
			fmt.Fprintf(w, `{"token":"installation-%d","expires_at":%q}`, exchange, now.Add(time.Hour).Format(time.RFC3339))
		case "/app":
			verifyAppJWT(t, jwt[len("Bearer "):], &key.PublicKey, now, "12345")
			fmt.Fprint(w, `{"slug":"legion-implementer"}`)
		case "/users/legion-implementer[bot]":
			mu.Lock()
			identities++
			mu.Unlock()
			if got := r.Header.Get("Authorization"); got != "token installation-1" {
				t.Fatalf("bot identity authorization = %q, want installation token", got)
			}
			fmt.Fprint(w, `{"id":271566630}`)
		default:
			t.Fatalf("unexpected GitHub path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	tokens := New(config.GitHubApps{
		Implement: config.GitHubApp{AppID: "12345", PrivateKey: privatePEM},
		Review:    config.GitHubApp{AppID: "67890", PrivateKey: privatePEM},
	}, Options{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now }})

	first, err := tokens.Token(context.Background(), Implement, "sjawhar")
	if err != nil {
		t.Fatalf("first token: %v", err)
	}
	if first.Token != "installation-1" || first.Identity.Name != "legion-implementer[bot]" || first.Identity.Email != "271566630+legion-implementer[bot]@users.noreply.github.com" {
		t.Fatalf("first lease = %+v", first)
	}
	if _, err := tokens.Token(context.Background(), Implement, "SJAWHAR"); err != nil {
		t.Fatalf("cached token: %v", err)
	}

	now = now.Add(55*time.Minute + time.Second)
	refreshed, err := tokens.Token(context.Background(), Implement, "sjawhar")
	if err != nil {
		t.Fatalf("refreshed token: %v", err)
	}
	if refreshed.Token != "installation-2" {
		t.Fatalf("refreshed token = %q, want installation-2", refreshed.Token)
	}
	mu.Lock()
	defer mu.Unlock()
	if exchanges != 2 || discoveries != 1 || identities != 1 {
		t.Fatalf("exchanges=%d discoveries=%d identities=%d, want 2, 1, 1", exchanges, discoveries, identities)
	}
}

func TestTokensDeduplicatesConcurrentRequests(t *testing.T) {
	key, privatePEM := testAppKey(t)
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	exchangeStarted := make(chan struct{})
	releaseExchange := make(chan struct{})
	var mu sync.Mutex
	exchanges := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jwt := r.Header.Get("Authorization")
		if r.URL.Path == "/app/installations/77/access_tokens" {
			verifyAppJWT(t, jwt[len("Bearer "):], &key.PublicKey, now, "12345")
			mu.Lock()
			exchanges++
			mu.Unlock()
			close(exchangeStarted)
			<-releaseExchange
			fmt.Fprintf(w, `{"token":"installation","expires_at":%q}`, now.Add(time.Hour).Format(time.RFC3339))
			return
		}
		switch r.URL.Path {
		case "/app":
			verifyAppJWT(t, jwt[len("Bearer "):], &key.PublicKey, now, "12345")
			fmt.Fprint(w, `{"slug":"legion-implementer"}`)
		case "/users/legion-implementer[bot]":
			fmt.Fprint(w, `{"id":271566630}`)
		default:
			t.Fatalf("unexpected GitHub path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	tokens := New(config.GitHubApps{
		Implement: config.GitHubApp{AppID: "12345", PrivateKey: privatePEM, Installations: map[string]string{"sjawhar": "77"}},
		Review:    config.GitHubApp{AppID: "67890", PrivateKey: privatePEM},
	}, Options{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now }})

	results := make(chan Lease, 2)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			lease, err := tokens.Token(context.Background(), Implement, "sjawhar")
			if err != nil {
				errs <- err
				return
			}
			results <- lease
		}()
	}
	<-exchangeStarted
	close(releaseExchange)
	first, second := <-results, <-results
	select {
	case err := <-errs:
		t.Fatalf("Token: %v", err)
	default:
	}
	if first.Token != "installation" || second.Token != first.Token {
		t.Fatalf("leases = %+v and %+v, want one installation token", first, second)
	}
	mu.Lock()
	defer mu.Unlock()
	if exchanges != 1 {
		t.Fatalf("token exchanges = %d, want 1", exchanges)
	}
}

// GitHub's trouble is transient and its answers are not: a request GitHub never answered or whose
// body stopped mid-read, a 5xx, or a rate limit (a 429, or a 403 that says it is one) may pass when
// it is made again, and a boot waits it out; any other status, a body that is not the JSON asked
// for, or an owner the App is not installed on is refused at once. The boot's retry reads nothing
// else.
func TestGitHubsTroubleIsTransientAndItsAnswersAreNot(t *testing.T) {
	_, privatePEM := testAppKey(t)
	status := func(code int, header ...string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			for i := 0; i+1 < len(header); i += 2 {
				w.Header().Set(header[i], header[i+1])
			}
			http.Error(w, "{}", code)
		}
	}
	installed := func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `[{"id":77,"account":{"login":"acme"}}]`) }
	for _, tc := range []struct {
		name      string
		discovery http.HandlerFunc
		exchange  http.HandlerFunc // nil answers with a token
		transient bool
	}{
		{name: "discovery 502", discovery: status(http.StatusBadGateway), transient: true},
		{name: "discovery 429", discovery: status(http.StatusTooManyRequests), transient: true},
		{name: "discovery 403 out of rate limit", discovery: status(http.StatusForbidden, "X-RateLimit-Remaining", "0"), transient: true},
		{name: "discovery 403 with retry-after", discovery: status(http.StatusForbidden, "Retry-After", "60"), transient: true},
		{name: "discovery 403", discovery: status(http.StatusForbidden, "X-RateLimit-Remaining", "4999")},
		{name: "discovery 401", discovery: status(http.StatusUnauthorized)},
		{name: "discovery never answers", discovery: func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }, transient: true},
		{name: "discovery body cut off mid-read", discovery: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "1000")
			fmt.Fprint(w, `[{"id":77,`)
			w.(http.Flusher).Flush()
			conn, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			conn.Close()
		}, transient: true},
		{name: "discovery body not JSON", discovery: func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `<html>`) }},
		{name: "not installed on the owner", discovery: func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `[{"id":77,"account":{"login":"someone-else"}}]`)
		}},
		{name: "exchange 500", discovery: installed, exchange: status(http.StatusInternalServerError), transient: true},
		{name: "exchange 422", discovery: installed, exchange: status(http.StatusUnprocessableEntity)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/app/installations":
					tc.discovery(w, r)
				case "/app/installations/77/access_tokens":
					if tc.exchange != nil {
						tc.exchange(w, r)
						return
					}
					fmt.Fprintf(w, `{"token":"installation-1","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
				default:
					t.Errorf("unexpected GitHub path %s", r.URL.Path)
				}
			}))
			defer server.Close()
			tokens := New(config.GitHubApps{Implement: config.GitHubApp{AppID: "12345", PrivateKey: privatePEM}},
				Options{BaseURL: server.URL, HTTPClient: server.Client()})
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()

			_, err := tokens.Token(ctx, Implement, "acme")
			if err == nil {
				t.Fatal("Token succeeded, want a failure")
			}
			var transient *TransientError
			if got := errors.As(err, &transient); got != tc.transient {
				t.Fatalf("Token = %v, transient %v, want transient %v", err, got, tc.transient)
			}
		})
	}
}
