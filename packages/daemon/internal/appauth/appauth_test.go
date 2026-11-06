package appauth

import (
	"bytes"
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
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
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
	if claims.Issued != now.Unix()-60 || claims.Expires != now.Unix()+540 {
		t.Fatalf("JWT claims = %+v, want iat %d and exp %d", claims, now.Unix()-60, now.Unix()+540)
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

// mintedFor is GitHub's answer to a token exchange narrowed to repos: the token, its expiry and
// the repositories GitHub says the token reaches, which every narrowed mint holds its answer to
// (narrowedTo).
func mintedFor(token string, expires time.Time, repos ...string) string {
	named := make([]string, 0, len(repos))
	for _, repo := range repos {
		named = append(named, fmt.Sprintf(`{"name":%q}`, repo))
	}
	return fmt.Sprintf(`{"token":%q,"expires_at":%q,"repository_selection":"selected","repositories":[%s]}`,
		token, expires.Format(time.RFC3339), strings.Join(named, ","))
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

// TestGenerateJWTExpiresInsideGitHubsCapWithMargin holds exp at most nine minutes past now and iat
// at or before now: GitHub refuses an exp more than ten minutes ahead of its clock, so a signer
// whose clock runs ahead of GitHub's fails at exactly ten minutes.
func TestGenerateJWTExpiresInsideGitHubsCapWithMargin(t *testing.T) {
	_, privatePEM := testAppKey(t)
	now := time.Date(2026, 10, 27, 15, 42, 0, 0, time.UTC)
	token, err := GenerateJWT("12345", privatePEM, now)
	if err != nil {
		t.Fatalf("GenerateJWT: %v", err)
	}
	var claims struct {
		Issued  int64 `json:"iat"`
		Expires int64 `json:"exp"`
	}
	decodeJWTPart(t, splitJWT(t, token)[1], &claims)
	if lead := time.Duration(claims.Expires-now.Unix()) * time.Second; lead > 9*time.Minute {
		t.Fatalf("exp is %s past now, want at most 9m0s (GitHub refuses more than 10m ahead of its clock)", lead)
	}
	if claims.Issued > now.Unix() {
		t.Fatalf("iat %d is after now %d", claims.Issued, now.Unix())
	}
}

func TestAppRoleForEveryRole(t *testing.T) {
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
		{claim.RoleController, Review},
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
			fmt.Fprint(w, mintedFor(fmt.Sprintf("installation-%d", exchange), now.Add(time.Hour), "legion"))
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

	first, err := tokens.Token(context.Background(), Implement, ghrepo.MustParse("sjawhar/legion"))
	if err != nil {
		t.Fatalf("first token: %v", err)
	}
	if first.Token != "installation-1" || first.Identity.Name != "legion-implementer[bot]" || first.Identity.Email != "271566630+legion-implementer[bot]@users.noreply.github.com" {
		t.Fatalf("first lease = %+v", first)
	}
	if _, err := tokens.Token(context.Background(), Implement, ghrepo.MustParse("SJAWHAR/legion")); err != nil {
		t.Fatalf("cached token: %v", err)
	}

	// The hour-long lease has fewer than refreshWindow's twenty minutes left: the next call mints.
	now = now.Add(40*time.Minute + time.Second)
	refreshed, err := tokens.Token(context.Background(), Implement, ghrepo.MustParse("sjawhar/legion"))
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

// An App removed from an owner after the daemon found its installation answers the next mint's
// exchange 404: the installation found is forgotten and looked for again, so the mint answers
// ErrNotInstalled, an answer about the owner, rather than an error a caller retries without end;
// and an App installed there again, under a new installation, mints on that one.
func TestAnInstallationRemovedAfterItWasFoundIsAnsweredNotInstalled(t *testing.T) {
	_, privatePEM := testAppKey(t)
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	installation, discoveries := "77", 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		current := installation
		mu.Unlock()
		switch r.URL.Path {
		case "/app/installations":
			mu.Lock()
			discoveries++
			mu.Unlock()
			if current == "" {
				fmt.Fprint(w, `[]`)
				return
			}
			fmt.Fprintf(w, `[{"id":%s,"account":{"login":"acme"}}]`, current)
		case "/app/installations/77/access_tokens", "/app/installations/88/access_tokens":
			if !strings.Contains(r.URL.Path, "/"+current+"/") || current == "" {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"message":"Not Found"}`)
				return
			}
			fmt.Fprint(w, mintedFor("installation-"+current, now.Add(time.Hour), "legion"))
		case "/app":
			fmt.Fprint(w, `{"slug":"legion-reviewer"}`)
		case "/users/legion-reviewer[bot]":
			fmt.Fprint(w, `{"id":271566631}`)
		default:
			t.Errorf("unexpected GitHub path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	tokens := New(config.GitHubApps{
		Implement: config.GitHubApp{AppID: "12345", PrivateKey: privatePEM},
		Review:    config.GitHubApp{AppID: "67890", PrivateKey: privatePEM},
	}, Options{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now }})
	if _, err := tokens.Token(context.Background(), Review, ghrepo.MustParse("acme/legion")); err != nil {
		t.Fatalf("first token: %v", err)
	}

	mu.Lock()
	installation = ""
	mu.Unlock()
	now = now.Add(time.Hour)
	_, err := tokens.Token(context.Background(), Review, ghrepo.MustParse("acme/legion"))
	if !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("a mint after the App was removed = %v, want ErrNotInstalled", err)
	}

	mu.Lock()
	installation = "88"
	mu.Unlock()
	lease, err := tokens.Token(context.Background(), Review, ghrepo.MustParse("acme/legion"))
	if err != nil || lease.Token != "installation-88" {
		t.Fatalf("a mint after the App was installed again = %+v, %v; want the new installation's token", lease, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if discoveries != 3 {
		t.Fatalf("installations were listed %d times, want 3: at the first mint, once the removed one answered 404, and once more for the reinstall", discoveries)
	}
}

// Forget drops both of an owner's cached leases for a role, the write token's and the read
// token's, in any case of the owner's name, so the next Token and ReadToken each mint again; a
// lease it did not drop is still served from the cache, and each lease records when it was minted.
func TestForgetMakesTheNextTokenAndReadTokenMintAgain(t *testing.T) {
	_, privatePEM := testAppKey(t)
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	exchanges := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations":
			fmt.Fprint(w, `[{"id":77,"account":{"login":"acme"}}]`)
		case "/app/installations/77/access_tokens":
			mu.Lock()
			exchanges++
			n := exchanges
			mu.Unlock()
			fmt.Fprint(w, mintedFor(fmt.Sprintf("installation-%d", n), now.Add(time.Hour), "legion"))
		case "/app":
			fmt.Fprint(w, `{"slug":"legion-reviewer"}`)
		case "/users/legion-reviewer[bot]":
			fmt.Fprint(w, `{"id":271566631}`)
		default:
			t.Errorf("unexpected GitHub path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	tokens := New(config.GitHubApps{
		Implement: config.GitHubApp{AppID: "12345", PrivateKey: privatePEM},
		Review:    config.GitHubApp{AppID: "67890", PrivateKey: privatePEM},
	}, Options{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now }})
	ctx := context.Background()
	mint := func(read bool) Lease {
		t.Helper()
		lease, err := tokens.Token(ctx, Review, ghrepo.MustParse("acme/legion"))
		if read {
			lease, err = tokens.ReadToken(ctx, Review, "acme", []string{"legion"})
		}
		if err != nil {
			t.Fatal(err)
		}
		return lease
	}
	write, read := mint(false), mint(true)
	if !write.Minted.Equal(now) || !read.Minted.Equal(now) {
		t.Fatalf("minted %s and %s, want both at %s", write.Minted, read.Minted, now)
	}
	if again := mint(false); again.Token != write.Token {
		t.Fatalf("a second Token = %s, want the cached %s", again.Token, write.Token)
	}
	tokens.Forget(Review, "ACME")
	if again := mint(false); again.Token == write.Token {
		t.Fatalf("Token after Forget = the dropped %s, want a fresh mint", again.Token)
	}
	if again := mint(true); again.Token == read.Token {
		t.Fatalf("ReadToken after Forget = the dropped %s, want a fresh mint", again.Token)
	}
	mu.Lock()
	defer mu.Unlock()
	if exchanges != 4 {
		t.Fatalf("%d exchanges, want 4: each lease once, then each again after Forget", exchanges)
	}
}

// ReadToken is the same App's installation token for an owner, narrowed to reading: its exchange
// posts the read permissions, it caches under its own key beside Token's (one mint each for one
// owner, a second ReadToken served from the cache), and it carries the same bot identity.
func TestReadTokenMintsANarrowedTokenCachedApartFromToken(t *testing.T) {
	key, privatePEM := testAppKey(t)
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jwt := r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/app/installations":
			fmt.Fprint(w, `[{"id":77,"account":{"login":"acme"}}]`)
		case "/app/installations/77/access_tokens":
			verifyAppJWT(t, jwt[len("Bearer "):], &key.PublicKey, now, "12345")
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			bodies = append(bodies, string(body))
			exchange := len(bodies)
			mu.Unlock()
			fmt.Fprint(w, mintedFor(fmt.Sprintf("installation-%d", exchange), now.Add(time.Hour), "legion"))
		case "/app":
			fmt.Fprint(w, `{"slug":"legion-implementer"}`)
		case "/users/legion-implementer[bot]":
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

	read, err := tokens.ReadToken(context.Background(), Implement, "acme", []string{"legion"})
	if err != nil {
		t.Fatalf("read token: %v", err)
	}
	write, err := tokens.Token(context.Background(), Implement, ghrepo.MustParse("Acme/legion"))
	if err != nil {
		t.Fatalf("write token: %v", err)
	}
	again, err := tokens.ReadToken(context.Background(), Implement, "ACME", []string{"Legion"})
	if err != nil {
		t.Fatalf("cached read token: %v", err)
	}
	if read.Token != "installation-1" || write.Token != "installation-2" || again.Token != "installation-1" {
		t.Fatalf("read=%q write=%q again=%q, want installation-1, installation-2, installation-1", read.Token, write.Token, again.Token)
	}
	if read.Identity != write.Identity || read.Identity.Name != "legion-implementer[bot]" {
		t.Fatalf("read identity = %+v, write identity = %+v, want the same bot", read.Identity, write.Identity)
	}
	mu.Lock()
	defer mu.Unlock()
	// json.Marshal of a map sorts its keys, so each body is this exact text: the one repository
	// the lease reaches, then the permissions — the four read ones a narrowed lease carries,
	// actions among them and nothing writable, and the implement App's write set.
	want := []string{
		`{"repositories":["legion"],"permissions":{"actions":"read","contents":"read","metadata":"read","pull_requests":"read"}}`,
		`{"repositories":["legion"],"permissions":{"actions":"write","checks":"read","contents":"write","metadata":"read","pull_requests":"write","statuses":"write","workflows":"write"}}`,
	}
	if len(bodies) != len(want) || bodies[0] != want[0] || bodies[1] != want[1] {
		t.Fatalf("exchange bodies = %q, want %q", bodies, want)
	}
}

// An App holds Actions only once each installation's account approves that grant, and GitHub
// answers 422 to a token request naming a permission the installation does not hold, minting
// nothing. The read token is then asked for again without actions: the lease is served with
// contents, pull requests and metadata, every owner's read token is served rather than the first
// such owner failing, and the daemon writes what that lease cannot read once per owner.
func TestReadTokenDropsActionsWhereTheInstallationDoesNotHoldThem(t *testing.T) {
	_, privatePEM := testAppKey(t)
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	bodies := map[string][]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations":
			fmt.Fprint(w, `[{"id":77,"account":{"login":"acme"}},{"id":88,"account":{"login":"sjawhar"}}]`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			installation := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/app/installations/"), "/access_tokens")
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read the exchange body: %v", err)
				return
			}
			mu.Lock()
			bodies[installation] = append(bodies[installation], string(body))
			exchanges := len(bodies[installation])
			mu.Unlock()
			// GitHub's own answer to a request naming a permission the installation does not
			// hold: no token at all, rather than a token without that permission.
			if strings.Contains(string(body), `"actions"`) {
				w.WriteHeader(http.StatusUnprocessableEntity)
				fmt.Fprint(w, `{"message":"The permissions requested are not granted to this installation."}`)
				return
			}
			fmt.Fprint(w, mintedFor(fmt.Sprintf("installation-%s-%d", installation, exchanges), now.Add(time.Hour), "legion"))
		case r.URL.Path == "/app":
			fmt.Fprint(w, `{"slug":"legion-reviewer"}`)
		case r.URL.Path == "/users/legion-reviewer[bot]":
			fmt.Fprint(w, `{"id":271566631}`)
		default:
			t.Errorf("unexpected GitHub path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	var stderr bytes.Buffer
	tokens := New(config.GitHubApps{
		Review: config.GitHubApp{AppID: "67890", PrivateKey: privatePEM},
	}, Options{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now }, Stderr: &stderr})
	defer tokens.Close()

	read, err := tokens.ReadToken(context.Background(), Review, "acme", []string{"legion"})
	if err != nil {
		t.Fatalf("read token for an installation that holds no actions: %v", err)
	}
	if read.Token != "installation-77-2" {
		t.Fatalf("read token = %q, want installation-77-2, the exchange that named no actions", read.Token)
	}
	// The second owner's read token is asked for directly, which is what a controller's bundle
	// does for it (ControllerCredential): one mint per owner, over the repositories that owner
	// serves, each refused its actions and served without them.
	second, err := tokens.ReadToken(context.Background(), Review, "sjawhar", []string{"legion"})
	if err != nil {
		t.Fatalf("read token for the second installation that holds no actions: %v", err)
	}
	if second.Token != "installation-88-2" {
		t.Fatalf("the second read token = %q, want installation-88-2, the exchange that named no actions", second.Token)
	}
	asked := []string{
		`{"repositories":["legion"],"permissions":{"actions":"read","contents":"read","metadata":"read","pull_requests":"read"}}`,
		`{"repositories":["legion"],"permissions":{"contents":"read","metadata":"read","pull_requests":"read"}}`,
	}
	mu.Lock()
	if !maps.EqualFunc(bodies, map[string][]string{"77": asked, "88": asked}, slices.Equal) {
		mu.Unlock()
		t.Fatalf("exchange bodies = %v, want each installation asked for %v", bodies, asked)
	}
	mu.Unlock()

	// Each owner's lease is cached, so the line is not written again for a lease served from the
	// cache, nor for the fresh mint a Forget makes the next read token do.
	tokens.Forget(Review, "acme")
	if _, err := tokens.ReadToken(context.Background(), Review, "acme", []string{"legion"}); err != nil {
		t.Fatalf("read token after Forget: %v", err)
	}
	for _, line := range []string{
		"appauth: the review App's installation on acme does not hold actions",
		"appauth: the review App's installation on sjawhar does not hold actions",
	} {
		if count := strings.Count(stderr.String(), line); count != 1 {
			t.Errorf("stderr holds %d of %q, want exactly one:\n%s", count, line, stderr.String())
		}
	}
	if !strings.Contains(stderr.String(), "grant the App Actions: Read") {
		t.Errorf("stderr does not say what fixes it:\n%s", stderr.String())
	}
}

// A 422 carrying GitHub's rate limit says nothing about what the installation holds: it is
// GitHub's trouble (TransientError), so the request is not asked again without actions, no lease
// is served and nothing is said about a missing grant. GitHub documents its limits on a 429 and a
// 403, so the three ways a 422 can still carry one are each checked.
func TestARateLimited422DoesNotDropActions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers map[string]string
		body    string
	}{
		{"the remaining header", map[string]string{"X-RateLimit-Remaining": "0"}, `{"message":"Validation failed"}`},
		{"a retry-after", map[string]string{"Retry-After": "60"}, `{"message":"Validation failed"}`},
		{"the secondary limit in the body", nil, `{"message":"You have exceeded a secondary rate limit."}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, privatePEM := testAppKey(t)
			now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
			var mu sync.Mutex
			exchanges := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/app/installations":
					fmt.Fprint(w, `[{"id":77,"account":{"login":"acme"}}]`)
				case strings.HasSuffix(r.URL.Path, "/access_tokens"):
					mu.Lock()
					exchanges++
					mu.Unlock()
					for name, value := range tc.headers {
						w.Header().Set(name, value)
					}
					w.WriteHeader(http.StatusUnprocessableEntity)
					fmt.Fprint(w, tc.body)
				default:
					t.Errorf("unexpected GitHub path %s", r.URL.Path)
				}
			}))
			defer server.Close()

			var stderr bytes.Buffer
			tokens := New(config.GitHubApps{Review: config.GitHubApp{AppID: "67890", PrivateKey: privatePEM}},
				Options{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now }, Stderr: &stderr})
			defer tokens.Close()

			lease, err := tokens.ReadToken(context.Background(), Review, "acme", []string{"legion"})
			var trouble *TransientError
			if lease.Token != "" || !errors.As(err, &trouble) {
				t.Fatalf("read token under a rate limit = %q, %v; want no lease and a TransientError", lease.Token, err)
			}
			if errors.Is(err, errNarrowingRefused) {
				t.Errorf("a rate limit = %v, want it never read as the permissions the installation holds", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if exchanges != 1 {
				t.Errorf("%d exchanges, want the one request: a rate limit is not asked again without actions", exchanges)
			}
			if stderr.Len() != 0 {
				t.Errorf("stderr = %q, want nothing said of a grant the answer says nothing about", stderr.String())
			}
		})
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
			fmt.Fprint(w, mintedFor("installation", now.Add(time.Hour), "legion"))
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
			lease, err := tokens.Token(context.Background(), Implement, ghrepo.MustParse("sjawhar/legion"))
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
	// secondaryLimit is GitHub's secondary rate limit answered with its message alone.
	secondaryLimit := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "4321")
		http.Error(w, `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`, http.StatusForbidden)
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
		{name: "discovery 403 for a secondary rate limit", discovery: secondaryLimit, transient: true},
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
		{name: "exchange 403 for a secondary rate limit", discovery: installed, exchange: secondaryLimit, transient: true},
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
					fmt.Fprint(w, mintedFor("installation-1", time.Now().Add(time.Hour), "legion"))
				default:
					t.Errorf("unexpected GitHub path %s", r.URL.Path)
				}
			}))
			defer server.Close()
			tokens := New(config.GitHubApps{Implement: config.GitHubApp{AppID: "12345", PrivateKey: privatePEM}},
				Options{BaseURL: server.URL, HTTPClient: server.Client()})
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()

			_, err := tokens.Token(ctx, Implement, ghrepo.MustParse("acme/legion"))
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

// keyModeGitHub is a stand-in GitHub for a mint the daemon signs its own JWT for: one
// installation for each owner it names, the App's slug and bot user, and every access_tokens body
// as GitHub parsed it, which is what a narrowing is read from rather than the text it was sent
// as. answer decides each exchange from the body it carried and how many came before it.
type keyModeGitHub struct {
	mu     sync.Mutex
	owners []string
	slug   string
	bodies []accessTokensBody
	answer func(exchange int, body accessTokensBody) (int, string)
}

func (g *keyModeGitHub) serve(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations":
			listed := make([]string, 0, len(g.owners))
			for i, owner := range g.owners {
				listed = append(listed, fmt.Sprintf(`{"id":%d,"account":{"login":%q}}`, 101+i, owner))
			}
			fmt.Fprintf(w, "[%s]", strings.Join(listed, ","))
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read the exchange body: %v", err)
				return
			}
			var body accessTokensBody
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("the exchange body is not the JSON an access_tokens request is: %v", err)
				return
			}
			g.mu.Lock()
			g.bodies = append(g.bodies, body)
			exchange := len(g.bodies)
			g.mu.Unlock()
			status, answer := g.answer(exchange, body)
			w.WriteHeader(status)
			fmt.Fprint(w, answer)
		case r.URL.Path == "/app":
			fmt.Fprintf(w, `{"slug":%q}`, g.slug)
		case r.URL.Path == "/users/"+g.slug+"[bot]":
			fmt.Fprint(w, `{"id":271566630}`)
		default:
			t.Errorf("unexpected GitHub path %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// exchanges is every access_tokens body GitHub was sent, in the order it got them.
func (g *keyModeGitHub) exchanges() []accessTokensBody {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.bodies)
}

// keyModeManager is a Manager holding a key for each App, minting against github, with its
// stderr kept rather than written to the test's own.
func keyModeManager(t *testing.T, github *keyModeGitHub, now *time.Time, stderr *bytes.Buffer) *Manager {
	t.Helper()
	_, privatePEM := testAppKey(t)
	server := github.serve(t)
	tokens := New(config.GitHubApps{
		Implement: config.GitHubApp{AppID: "12345", PrivateKey: privatePEM},
		Review:    config.GitHubApp{AppID: "67890", PrivateKey: privatePEM},
	}, Options{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return *now }, Stderr: stderr})
	t.Cleanup(tokens.Close)
	return tokens
}

// A write mint asks for the one repository its caller named and for exactly the permissions that
// App's roles use on it, each App's set its own: a token that leaks reaches that repository
// alone, and does there only what Legion does. The installation holds more — issues, projects,
// packages — and a token that asks for none of it cannot be made to use them.
func TestTokenAsksForTheIssuesRepositoryAndTheAppsWritePermissions(t *testing.T) {
	for _, tc := range []struct {
		role AppRole
		slug string
		want map[string]string
	}{
		{Implement, "legion-implementer", map[string]string{
			"actions": "write", "checks": "read", "contents": "write", "metadata": "read",
			"pull_requests": "write", "statuses": "write", "workflows": "write",
		}},
		{Review, "legion-reviewer", map[string]string{
			"actions": "write", "checks": "write", "contents": "write", "metadata": "read",
			"pull_requests": "write",
		}},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
			var stderr bytes.Buffer
			github := &keyModeGitHub{owners: []string{"sjawhar"}, slug: tc.slug}
			github.answer = func(exchange int, body accessTokensBody) (int, string) {
				return http.StatusOK, mintedFor(fmt.Sprintf("ghs_%d", exchange), now.Add(time.Hour), body.Repositories...)
			}
			tokens := keyModeManager(t, github, &now, &stderr)

			if _, err := tokens.Token(context.Background(), tc.role, ghrepo.MustParse("sjawhar/legion")); err != nil {
				t.Fatalf("Token: %v", err)
			}

			asked := github.exchanges()
			if len(asked) != 1 {
				t.Fatalf("%d exchanges, want the one mint", len(asked))
			}
			if !slices.Equal(asked[0].Repositories, []string{"legion"}) {
				t.Errorf("the request named repositories %q, want the issue's alone", asked[0].Repositories)
			}
			if !maps.Equal(asked[0].Permissions, tc.want) {
				t.Errorf("the %s App's write request named permissions %v, want exactly %v", tc.role, asked[0].Permissions, tc.want)
			}
		})
	}
}

// A read mint asks for every repository its caller named, each once and in one order whatever
// order they were given in, and for the four reads a role and a controller need of a repository
// that is not their own issue's: nothing writable, and nothing of the installation's breadth.
func TestReadTokenAsksForEveryRepositoryNamedAndTheFourReads(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	var stderr bytes.Buffer
	github := &keyModeGitHub{owners: []string{"acme"}, slug: "legion-reviewer"}
	github.answer = func(exchange int, body accessTokensBody) (int, string) {
		return http.StatusOK, mintedFor(fmt.Sprintf("ghs_%d", exchange), now.Add(time.Hour), body.Repositories...)
	}
	tokens := keyModeManager(t, github, &now, &stderr)

	if _, err := tokens.ReadToken(context.Background(), Review, "acme", []string{"nats-server", "legion", "nats-server"}); err != nil {
		t.Fatalf("ReadToken: %v", err)
	}

	asked := github.exchanges()
	if len(asked) != 1 {
		t.Fatalf("%d exchanges, want the one mint", len(asked))
	}
	if !slices.Equal(asked[0].Repositories, []string{"legion", "nats-server"}) {
		t.Errorf("the request named repositories %q, want the two the caller named, each once", asked[0].Repositories)
	}
	want := map[string]string{"actions": "read", "contents": "read", "metadata": "read", "pull_requests": "read"}
	if !maps.Equal(asked[0].Permissions, want) {
		t.Errorf("the read request named permissions %v, want exactly %v", asked[0].Permissions, want)
	}
}

// GitHub applies the body and refuses what it cannot grant, so an answer wider than the request
// means the narrowing was dropped or changed on the way: the lease would reach every repository
// of the installation, and it is refused rather than served. The refusal carries no token, and
// nothing is cached, so the next call mints again instead of serving what was refused.
func TestTokenRefusesATokenWiderThanTheRepositoriesAsked(t *testing.T) {
	expires := time.Date(2026, 11, 6, 13, 0, 0, 0, time.UTC).Format(time.RFC3339)
	for _, tc := range []struct {
		name   string
		answer string
	}{
		{"a selection of every repository", `{"token":"ghs_wide","expires_at":"` + expires + `","repository_selection":"all","repositories":[]}`},
		{"a repository the request did not name", `{"token":"ghs_wide","expires_at":"` + expires + `","repository_selection":"selected","repositories":[{"name":"legion"},{"name":"secrets"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
			var stderr bytes.Buffer
			github := &keyModeGitHub{owners: []string{"sjawhar"}, slug: "legion-implementer"}
			github.answer = func(int, accessTokensBody) (int, string) { return http.StatusOK, tc.answer }
			tokens := keyModeManager(t, github, &now, &stderr)

			lease, err := tokens.Token(context.Background(), Implement, ghrepo.MustParse("sjawhar/legion"))
			if err == nil || lease.Token != "" {
				t.Fatalf("a token wider than the request = %q, %v; want no lease and a refusal", lease.Token, err)
			}
			if strings.Contains(err.Error(), "ghs_wide") {
				t.Errorf("the refusal carries the token it refused: %v", err)
			}

			if _, err := tokens.Token(context.Background(), Implement, ghrepo.MustParse("sjawhar/legion")); err == nil {
				t.Fatal("the second call was served a lease the first refused")
			}
			if asked := github.exchanges(); len(asked) != 2 {
				t.Errorf("%d exchanges, want the refused lease cached nowhere and minted again", len(asked))
			}
		})
	}
}

// GitHub answers 422 both to a repository the installation does not reach and to a permission it
// does not hold, and says which in no way a caller may read, so a write mint reports the refusal
// naming both halves of what it asked for and an operator reads which it was. It is not the
// missing Actions grant a read mint asks again without: a write token minted without actions
// would take `gh run rerun` from every role in silence.
func TestA422ForARepositoryTheInstallationCannotReachIsTheNarrowingRefusal(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	var stderr bytes.Buffer
	github := &keyModeGitHub{owners: []string{"sjawhar"}, slug: "legion-implementer"}
	github.answer = func(int, accessTokensBody) (int, string) {
		return http.StatusUnprocessableEntity, `{"message":"There is at least one repository that does not exist or is not accessible to the parent installation."}`
	}
	tokens := keyModeManager(t, github, &now, &stderr)

	lease, err := tokens.Token(context.Background(), Implement, ghrepo.MustParse("sjawhar/elsewhere"))
	var trouble *TransientError
	if lease.Token != "" || !errors.Is(err, errNarrowingRefused) || errors.As(err, &trouble) {
		t.Fatalf("a 422 to a narrowed write mint = %q, %v; want no lease and the narrowing refusal", lease.Token, err)
	}
	if !strings.Contains(err.Error(), "elsewhere") {
		t.Errorf("the refusal does not name the repository asked for: %v", err)
	}
	for _, permission := range []string{"actions: write", "checks: read", "contents: write", "metadata: read", "pull_requests: write", "statuses: write", "workflows: write"} {
		if !strings.Contains(err.Error(), permission) {
			t.Errorf("the refusal does not name %q, half of what the request carried: %v", permission, err)
		}
	}
	if asked := github.exchanges(); len(asked) != 1 {
		t.Errorf("%d exchanges, want the one: a write mint is never asked again without actions", len(asked))
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing said of an Actions grant this answer says nothing about", stderr.String())
	}
}

// Two repositories of one role and owner are two leases: each is minted and served under its own
// scope, and neither is ever served to the caller that asked for the other, since a token for one
// repository is no token for another. Forget knows the owner alone, and drops every one of them,
// the write lease among them.
func TestTwoRepositoryScopesOfOneRoleAndOwnerAreCachedApart(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	var stderr bytes.Buffer
	github := &keyModeGitHub{owners: []string{"acme"}, slug: "legion-reviewer"}
	github.answer = func(exchange int, body accessTokensBody) (int, string) {
		return http.StatusOK, mintedFor(fmt.Sprintf("ghs_%d", exchange), now.Add(time.Hour), body.Repositories...)
	}
	tokens := keyModeManager(t, github, &now, &stderr)

	read := func(repo string) string {
		t.Helper()
		lease, err := tokens.ReadToken(context.Background(), Review, "acme", []string{repo})
		if err != nil {
			t.Fatalf("ReadToken for %s: %v", repo, err)
		}
		return lease.Token
	}
	write := func() string {
		t.Helper()
		lease, err := tokens.Token(context.Background(), Review, ghrepo.MustParse("acme/alpha"))
		if err != nil {
			t.Fatalf("Token: %v", err)
		}
		return lease.Token
	}

	alpha, beta, written := read("alpha"), read("beta"), write()
	if alpha == beta || alpha == written || beta == written {
		t.Fatalf("alpha=%q beta=%q write=%q, want three leases of their own", alpha, beta, written)
	}
	if read("alpha") != alpha || read("beta") != beta || write() != written {
		t.Error("a second call minted again where its own scope's lease was cached")
	}
	if asked := github.exchanges(); len(asked) != 3 {
		t.Fatalf("%d exchanges, want one for each of the three scopes", len(asked))
	}

	tokens.Forget(Review, "ACME")
	if read("alpha") == alpha || read("beta") == beta || write() == written {
		t.Error("Forget left a lease of this owner cached")
	}
	if asked := github.exchanges(); len(asked) != 6 {
		t.Errorf("%d exchanges, want each of the three scopes minted again after Forget", len(asked))
	}
}

// A token names the repositories it reaches, so a call that names none asks for a token there is
// no narrowing for: it is refused where it is made, and GitHub is not asked at all.
func TestAMintThatNamesNoRepositoryIsRefusedBeforeGitHub(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	var stderr bytes.Buffer
	github := &keyModeGitHub{owners: []string{"acme"}, slug: "legion-implementer"}
	github.answer = func(int, accessTokensBody) (int, string) {
		t.Error("GitHub was asked for a token no repository was named for")
		return http.StatusOK, ""
	}
	tokens := keyModeManager(t, github, &now, &stderr)

	if lease, err := tokens.Token(context.Background(), Implement, ghrepo.Repository{}); err == nil || lease.Token != "" {
		t.Errorf("Token for the zero repository = %q, %v; want no lease and a refusal", lease.Token, err)
	}
	if lease, err := tokens.ReadToken(context.Background(), Implement, "acme", nil); err == nil || lease.Token != "" {
		t.Errorf("ReadToken naming no repository = %q, %v; want no lease and a refusal", lease.Token, err)
	}
	if asked := github.exchanges(); len(asked) != 0 {
		t.Errorf("%d exchanges, want none", len(asked))
	}
}
