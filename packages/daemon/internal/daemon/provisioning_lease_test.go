package daemon

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/config"
)

// provisioningTokenRequest is one installation token request a provisioning mint made: the
// installation it was addressed to, and the permissions its body named. permissions is nil for a
// request whose body named none, which is what the App's own full installation token asks for.
type provisioningTokenRequest struct {
	installation string
	permissions  map[string]string
}

// provisioningInstallation is one owner's installation on the stand-in: the id installation
// discovery gives for owner, and how that installation's token exchange answers a request, given
// the permissions its body named (nil for a request that named none).
type provisioningInstallation struct {
	id     string
	owner  string
	answer func(w http.ResponseWriter, permissions map[string]string)
}

// provisioningGitHub is a GitHub stand-in over the App endpoints a provisioning mint calls —
// installation discovery, the token exchange and the App's identity — with an installation of its
// own per owner, and a record of every token request's body, so a test reads what each mint asked
// for and not only what it got back. It is appmint_test.go's gitHubStandIn with that record
// added; that one stays as it is, since the boot tests' mints depend on its answers.
type provisioningGitHub struct {
	tokens *appauth.Manager

	mu       sync.Mutex
	requests []provisioningTokenRequest
}

// requestsTo is the token requests the stand-in recorded for one installation, in the order they
// arrived.
func (g *provisioningGitHub) requestsTo(installation string) []provisioningTokenRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	var requests []provisioningTokenRequest
	for _, request := range g.requests {
		if request.installation == installation {
			requests = append(requests, request)
		}
	}
	return requests
}

// provisioningStandIn serves installations and returns the stand-in, whose tokens is a real App
// token manager pointed at it, the implement App id 1 and the review App id 2.
func provisioningStandIn(t *testing.T, installations ...provisioningInstallation) *provisioningGitHub {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privatePEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	byID := make(map[string]provisioningInstallation, len(installations))
	for _, installation := range installations {
		byID[installation.id] = installation
	}
	github := &provisioningGitHub{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations":
			discovered := make([]string, 0, len(installations))
			for _, installation := range installations {
				discovered = append(discovered, fmt.Sprintf(`{"id":%s,"account":{"login":%q}}`, installation.id, installation.owner))
			}
			fmt.Fprintf(w, "[%s]", strings.Join(discovered, ","))
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/app/installations/"), "/access_tokens")
			installation, known := byID[id]
			if !known {
				t.Errorf("a token request for the unknown installation %q", id)
				http.NotFound(w, r)
				return
			}
			permissions := provisioningPermissions(t, r)
			github.mu.Lock()
			github.requests = append(github.requests, provisioningTokenRequest{installation: id, permissions: permissions})
			github.mu.Unlock()
			installation.answer(w, permissions)
		case r.URL.Path == "/app":
			fmt.Fprint(w, `{"slug":"legion-test"}`)
		case strings.HasPrefix(r.URL.Path, "/users/"):
			fmt.Fprint(w, `{"id":42}`)
		default:
			t.Errorf("unexpected GitHub request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	github.tokens = appauth.New(config.GitHubApps{
		Implement: config.GitHubApp{AppID: "1", PrivateKey: privatePEM},
		Review:    config.GitHubApp{AppID: "2", PrivateKey: privatePEM},
	}, appauth.Options{BaseURL: server.URL, HTTPClient: server.Client(), Stderr: io.Discard})
	return github
}

// provisioningPermissions is the permissions a token request's body named, nil where it named
// none.
func provisioningPermissions(t *testing.T, r *http.Request) map[string]string {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("read a token request's body: %v", err)
		return nil
	}
	if len(body) == 0 {
		return nil
	}
	var payload struct {
		Permissions map[string]string `json:"permissions"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Errorf("decode the token request body %q: %v", body, err)
	}
	return payload.Permissions
}

// provisioningMint answers a token request with token, as GitHub's own answer is shaped.
func provisioningMint(w http.ResponseWriter, token string) {
	fmt.Fprintf(w, `{"token":%q,"expires_at":%q}`, token, time.Now().Add(time.Hour).Format(time.RFC3339))
}

// provisioningRefusal is GitHub's 422 to a token request naming permissions the installation does
// not hold. It carries no rate-limit signal, so appauth reads it as the installation's
// permissions (ErrPermissionsRefused) rather than as GitHub's trouble.
func provisioningRefusal(w http.ResponseWriter) {
	w.WriteHeader(http.StatusUnprocessableEntity)
	fmt.Fprint(w, `{"message":"Validation Failed","errors":[{"resource":"Permission","code":"invalid"}]}`)
}

// The Sandbox runtime's provisioning token is a reading lease: implementTokens mints the implement
// App's installation token for the repository's owner narrowed to contents, pull requests, Actions
// and metadata at read, which is all a clone, a fetch and a repository cache do (INFRA-1254).
// Four installations, one per case, so no case's mints or cached leases reach another's:
//
//   - one that grants the reading set: the one request names it, and that token is served;
//   - one that refuses actions: appauth's own exchangeRead asks again without it, and the adapter
//     is served the narrowed token;
//   - one that refuses the reading set outright: the adapter falls back to the owner's own
//     installation token, the one fallback there is, and says so once for that owner;
//   - one in GitHub's trouble (a 500): the error is returned as it came, with no fallback mint and
//     nothing logged.
func TestTheProvisioningTokenIsAReadingLease(t *testing.T) {
	const (
		readingOwner = "acme-reading"
		narrowOwner  = "acme-no-actions"
		refusedOwner = "acme-no-reading"
		flakyOwner   = "acme-flaky"

		readingInstallation = "11"
		narrowInstallation  = "22"
		refusedInstallation = "33"
		flakyInstallation   = "44"
	)
	readingSet := map[string]string{"contents": "read", "pull_requests": "read", "actions": "read", "metadata": "read"}
	withoutActions := map[string]string{"contents": "read", "pull_requests": "read", "metadata": "read"}

	github := provisioningStandIn(t,
		provisioningInstallation{id: readingInstallation, owner: readingOwner, answer: func(w http.ResponseWriter, _ map[string]string) {
			provisioningMint(w, "reading-lease")
		}},
		// An installation whose account has not granted the App Actions: GitHub mints nothing
		// for a request that names actions, and the three other reads for one that does not.
		provisioningInstallation{id: narrowInstallation, owner: narrowOwner, answer: func(w http.ResponseWriter, permissions map[string]string) {
			if permissions["actions"] != "" {
				provisioningRefusal(w)
				return
			}
			provisioningMint(w, "narrowed-lease")
		}},
		// An installation that refuses any narrowing: every request naming permissions is
		// refused, and only the one naming none — the App's own full installation token — mints.
		provisioningInstallation{id: refusedInstallation, owner: refusedOwner, answer: func(w http.ResponseWriter, permissions map[string]string) {
			if permissions != nil {
				provisioningRefusal(w)
				return
			}
			provisioningMint(w, "own-installation-token")
		}},
		provisioningInstallation{id: flakyInstallation, owner: flakyOwner, answer: func(w http.ResponseWriter, _ map[string]string) {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"message":"Server Error"}`)
		}},
	)
	var logged syncBuffer
	// The adapter as sandboxRuntime builds it, with the identity serving map of a deployment that
	// configures no rename.
	adapter := newImplementTokens(github.tokens, func(owner string) string { return owner },
		slog.New(slog.NewJSONHandler(&logged, nil)))
	fallbacks := func() []map[string]any {
		t.Helper()
		return loggedLines(t, logged.String(), "refuses a read-only provisioning token")
	}
	ctx := context.Background()

	// The request carries the reading permissions, and that token is what a pod clones with.
	token, err := adapter.Token(ctx, readingOwner)
	if err != nil || token != "reading-lease" {
		t.Fatalf("the token for %s = %q, %v; want the reading lease", readingOwner, token, err)
	}
	requests := github.requestsTo(readingInstallation)
	if len(requests) != 1 {
		t.Fatalf("%d token requests for %s, want the one read mint: %+v", len(requests), readingOwner, requests)
	}
	if !maps.Equal(requests[0].permissions, readingSet) {
		t.Fatalf("the token request asked for %v, want %v", requests[0].permissions, readingSet)
	}

	// An installation that refuses actions: appauth asks again without it, and the adapter is
	// served the narrowed token rather than a failure.
	token, err = adapter.Token(ctx, narrowOwner)
	if err != nil || token != "narrowed-lease" {
		t.Fatalf("the token for %s = %q, %v; want the lease narrowed past actions", narrowOwner, token, err)
	}
	requests = github.requestsTo(narrowInstallation)
	if len(requests) != 2 {
		t.Fatalf("%d token requests for %s, want the read mint and its retry: %+v", len(requests), narrowOwner, requests)
	}
	if !maps.Equal(requests[0].permissions, readingSet) {
		t.Fatalf("the first token request asked for %v, want %v", requests[0].permissions, readingSet)
	}
	if !maps.Equal(requests[1].permissions, withoutActions) {
		t.Fatalf("the retried token request asked for %v, want %v: the reading set without actions", requests[1].permissions, withoutActions)
	}

	// An installation that refuses the reading set outright: the owner's own installation token
	// is the one fallback, said once.
	token, err = adapter.Token(ctx, refusedOwner)
	if err != nil || token != "own-installation-token" {
		t.Fatalf("the token for %s = %q, %v; want the owner's own installation token", refusedOwner, token, err)
	}
	requests = github.requestsTo(refusedInstallation)
	if len(requests) != 3 {
		t.Fatalf("%d token requests for %s, want the read mint, its retry and the fallback: %+v", len(requests), refusedOwner, requests)
	}
	if requests[2].permissions != nil {
		t.Fatalf("the fallback token request asked for %v, want a request naming no permissions", requests[2].permissions)
	}
	lines := fallbacks()
	if len(lines) != 1 {
		t.Fatalf("%d fallback lines, want exactly one: %s", len(lines), logged.String())
	}
	if lines[0]["owner"] != refusedOwner || lines[0]["level"] != "WARN" {
		t.Fatalf("the fallback line = %v, want a WARN naming %s", lines[0], refusedOwner)
	}

	// The same owner provisioned again says nothing more, for the life of the process. The lease
	// the first call cached is dropped first, so this call really mints again (appauth's Forget,
	// as TestForgetMakesTheNextTokenAndReadTokenMintAgain uses it): served from the cache it
	// would make no GitHub request at all and prove nothing about the once-per-owner rule.
	github.tokens.Forget(appauth.Implement, refusedOwner)
	token, err = adapter.Token(ctx, refusedOwner)
	if err != nil || token != "own-installation-token" {
		t.Fatalf("the second token for %s = %q, %v; want the owner's own installation token again", refusedOwner, token, err)
	}
	if again := github.requestsTo(refusedInstallation); len(again) != 6 {
		t.Fatalf("%d token requests for %s after Forget, want the three of each call: %+v", len(again), refusedOwner, again)
	}
	if lines := fallbacks(); len(lines) != 1 {
		t.Fatalf("%d fallback lines after the second mint for the same owner, want the first one alone: %s", len(lines), logged.String())
	}

	// GitHub's trouble is returned as it came: no fallback mint, and nothing logged.
	token, err = adapter.Token(ctx, flakyOwner)
	var transient *appauth.TransientError
	if !errors.As(err, &transient) || token != "" {
		t.Fatalf("the token for %s = %q, %v; want GitHub's trouble returned", flakyOwner, token, err)
	}
	if errors.Is(err, appauth.ErrPermissionsRefused) {
		t.Fatalf("a 500 = %v, want it never read as the permissions the installation holds", err)
	}
	requests = github.requestsTo(flakyInstallation)
	if len(requests) == 0 {
		t.Fatalf("no token request for %s, want the read mint that failed", flakyOwner)
	}
	for _, request := range requests {
		if request.permissions == nil {
			t.Fatalf("a token request for %s named no permissions, want no fallback to the owner's own token: %+v", flakyOwner, requests)
		}
	}
	if lines := fallbacks(); len(lines) != 1 {
		t.Fatalf("%d fallback lines after GitHub's trouble, want only the refused owner's: %s", len(lines), logged.String())
	}
}
