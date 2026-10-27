package githubapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/ratelimit"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// limitGitHub is a GitHub App API that records every request it is sent ("METHOD path
// Authorization"): one repository (acme/web) and the installation covering it, token mints, the
// App's installation list, and any other path answered as a read. A request to limitPath is
// answered as GitHub answers an exhausted rate limit: 403, X-RateLimit-Remaining: 0 and a
// Retry-After of a minute.
type limitGitHub struct {
	server *httptest.Server

	mu             sync.Mutex
	installationID int64
	mints          int
	limitPath      string
	requests       []string
	// mintsAwaited and mintsArrived, set by holdMints, hold each mint's answer until that many
	// mints have arrived.
	mintsAwaited int
	mintsArrived chan struct{}
}

func startLimitGitHub(t *testing.T) *limitGitHub {
	t.Helper()
	fake := &limitGitHub{installationID: 7}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *limitGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
	limited := r.URL.Path == f.limitPath
	installationID := f.installationID
	var held chan struct{}
	if !limited && strings.HasSuffix(r.URL.Path, "/access_tokens") {
		f.mints++
		if f.mintsArrived != nil {
			held = f.mintsArrived
			if f.mintsAwaited--; f.mintsAwaited == 0 {
				close(f.mintsArrived)
				f.mintsArrived = nil
			}
		}
	}
	mints := f.mints
	f.mu.Unlock()
	if held != nil {
		// A client that sends fewer mints than awaited is answered anyway, a little later.
		select {
		case <-held:
		case <-time.After(2 * time.Second):
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if limited {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/web/installation":
		_ = json.NewEncoder(w).Encode(map[string]any{"id": installationID, "app_slug": "dispatch-test", "permissions": map[string]string{"contents": "read"}})
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/app/installations/") && strings.HasSuffix(r.URL.Path, "/access_tokens"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/app/installations/"), "/access_tokens")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": fmt.Sprintf("ghs_%s_%d", id, mints), "expires_at": time.Now().Add(time.Hour)})
	case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": installationID, "app_slug": "dispatch-test", "account": map[string]string{"login": "acme"}}})
	default:
		fmt.Fprint(w, `{"state":"open"}`)
	}
}

// limit makes every later request to path answer an exhausted rate limit; "" stops it.
func (f *limitGitHub) limit(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.limitPath = path
}

// holdMints holds each later mint's answer until n mints have arrived, so n callers that each
// found no token mint at once.
func (f *limitGitHub) holdMints(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mintsAwaited = n
	f.mintsArrived = make(chan struct{})
}

// moveInstallation makes acme/web's installation lookup answer id.
func (f *limitGitHub) moveInstallation(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installationID = id
}

// count is how many requests GitHub was sent whose line holds every part.
func (f *limitGitHub) count(parts ...string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, request := range f.requests {
		matched := true
		for _, part := range parts {
			matched = matched && strings.Contains(request, part)
		}
		if matched {
			n++
		}
	}
	return n
}

func (f *limitGitHub) newClient(t *testing.T, options ...Option) *Client {
	t.Helper()
	_, pemText := testKeyPEM(t)
	client, err := New(&auth.AppConfig{ClientID: "Iv1.limits", PEM: pemText}, f.server.URL, options...)
	if err != nil || client == nil {
		t.Fatalf("new client: %v", err)
	}
	return client
}

// twoProcesses is two clients on one database, each through a pool of its own, as two Dispatch
// processes hold them, both calling one GitHub.
func twoProcesses(t *testing.T, fake *limitGitHub) (database *store.Store, a, b *Client) {
	t.Helper()
	database = storetest.Open(t)
	a = fake.newClient(t, WithLimits(ratelimit.New(database.Pool)))
	b = fake.newClient(t, WithLimits(ratelimit.New(storetest.SecondProcess(t, database).Pool)))
	return database, a, b
}

// passLimits moves every recorded limit's end into the past, as waiting GitHub's wait out would.
func passLimits(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `update github_rate_limits set until = now() - interval '1 second'`); err != nil {
		t.Fatalf("move the recorded limits back: %v", err)
	}
}

// requireHeld requires err to be a limit recorded by the other process: no request was sent.
func requireHeld(t *testing.T, err error, what string) {
	t.Helper()
	limited, ok := AsRateLimit(err)
	if !ok || !limited.Held || limited.Wait <= 0 {
		t.Fatalf("%s: err = %v, want the recorded limit, held without a request", what, err)
	}
}

// GitHub budgets an installation, not a process. A limit one process meets reading under an
// installation's token holds the other process's reads under that installation, with no request,
// and leaves the App's own calls alone; once the limit's end has passed, the other reads again.
func TestAnInstallationsLimitMetByOneProcessHoldsTheOthersReads(t *testing.T) {
	fake := startLimitGitHub(t)
	database, a, b := twoProcesses(t, fake)
	ctx := context.Background()
	tokenA, err := a.Token(ctx, 7)
	if err != nil {
		t.Fatalf("A's token: %v", err)
	}
	tokenB, err := b.Token(ctx, 7)
	if err != nil {
		t.Fatalf("B's token: %v", err)
	}

	fake.limit("/repos/acme/web/pulls/1")
	if _, status, _, err := a.Read(ctx, tokenA, "/repos/acme/web/pulls/1"); err != nil || status != http.StatusForbidden {
		t.Fatalf("A's limited read: status %d, %v; want GitHub's 403", status, err)
	}
	_, _, _, err = b.Read(ctx, tokenB, "/repos/acme/web/issues/2")
	requireHeld(t, err, "B's read under the same installation")
	_, _, _, err = b.GraphQL(ctx, tokenB, "query { viewer { login } }", nil)
	requireHeld(t, err, "B's GraphQL query under the same installation")
	if got := fake.count("Bearer " + tokenB); got != 0 {
		t.Fatalf("GitHub got %d requests under B's token inside the installation's limit, want none", got)
	}
	if _, err := b.ListInstallations(ctx); err != nil {
		t.Fatalf("B's App call inside an installation's limit: %v, want GitHub's answer", err)
	}

	fake.limit("")
	passLimits(t, database)
	if _, status, _, err := b.Read(ctx, tokenB, "/repos/acme/web/issues/2"); err != nil || status != http.StatusOK {
		t.Fatalf("B's read once the limit passed: status %d, %v; want GitHub's 200", status, err)
	}
	if got := fake.count("GET /repos/acme/web/issues/2 Bearer " + tokenB); got != 1 {
		t.Fatalf("GitHub got B's read %d times once the limit passed, want once", got)
	}
}

// A token stays under its installation's limit until it expires, whatever mints after it: a
// caller still holding the token from before another caller refreshed the installation's (an
// architecture import mid-sync) is held by the installation's limit, with no request.
func TestATokenMintedBeforeARefreshStaysUnderItsInstallationsLimit(t *testing.T) {
	fake := startLimitGitHub(t)
	_, a, b := twoProcesses(t, fake)
	clock := &testClock{now: time.Now()}
	a.now = clock.Now
	ctx := context.Background()
	held, err := a.Token(ctx, 7)
	if err != nil {
		t.Fatalf("A's first token: %v", err)
	}
	// Inside tokenExpirySlack of the first token's expiry, so the next caller mints anew, while
	// the first token is still valid at GitHub.
	clock.advance(time.Hour - tokenExpirySlack + time.Minute)
	refreshed, err := a.Token(ctx, 7)
	if err != nil || refreshed == held {
		t.Fatalf("A's refreshed token %q (first %q), %v; want a new token", refreshed, held, err)
	}
	tokenB, err := b.Token(ctx, 7)
	if err != nil {
		t.Fatalf("B's token: %v", err)
	}

	fake.limit("/repos/acme/web/pulls/1")
	if _, status, _, err := b.Read(ctx, tokenB, "/repos/acme/web/pulls/1"); err != nil || status != http.StatusForbidden {
		t.Fatalf("B's limited read: status %d, %v; want GitHub's 403", status, err)
	}
	_, _, _, err = a.Read(ctx, held, "/repos/acme/web/issues/2")
	requireHeld(t, err, "A's read under the token minted before the refresh")
	_, _, _, err = a.Read(ctx, refreshed, "/repos/acme/web/issues/2")
	requireHeld(t, err, "A's read under the refreshed token")
	if got := fake.count("Bearer " + held); got != 0 {
		t.Fatalf("GitHub got %d requests under the token minted before the refresh inside the installation's limit, want none", got)
	}
}

// Two callers that find no token for an installation at once both mint, and each token is under
// the installation's limit: the second mint to land does not drop the first's scope.
func TestTwoConcurrentFirstMintsForOneInstallationAreBothUnderItsLimit(t *testing.T) {
	fake := startLimitGitHub(t)
	_, a, b := twoProcesses(t, fake)
	ctx := context.Background()
	fake.holdMints(2)
	tokens := make([]string, 2)
	errs := make([]error, 2)
	var minting sync.WaitGroup
	for i := range tokens {
		minting.Go(func() { tokens[i], errs[i] = a.Token(ctx, 7) })
	}
	minting.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("A's concurrent mint %d: %v", i, err)
		}
	}
	if got := fake.count("POST /app/installations/7/access_tokens"); got != 2 {
		t.Fatalf("mints that reached GitHub = %d, want both callers' (tokens %q)", got, tokens)
	}
	tokenB, err := b.Token(ctx, 7)
	if err != nil {
		t.Fatalf("B's token: %v", err)
	}

	fake.limit("/repos/acme/web/pulls/1")
	if _, status, _, err := b.Read(ctx, tokenB, "/repos/acme/web/pulls/1"); err != nil || status != http.StatusForbidden {
		t.Fatalf("B's limited read: status %d, %v; want GitHub's 403", status, err)
	}
	for _, token := range tokens {
		_, _, _, err = a.Read(ctx, token, "/repos/acme/web/issues/2")
		requireHeld(t, err, "A's read under the token "+token)
		if got := fake.count("Bearer " + token); got != 0 {
			t.Fatalf("GitHub got %d requests under %s inside the installation's limit, want none", got, token)
		}
	}
}

// The client remembers a token's installation only until the token expires: the next mint forgets
// every expired token, of any installation, so a long-running process holds no more than the
// tokens still valid.
func TestAnExpiredTokensInstallationIsForgottenAtTheNextMint(t *testing.T) {
	fake := startLimitGitHub(t)
	client := fake.newClient(t)
	clock := &testClock{now: time.Now()}
	client.now = clock.Now
	ctx := context.Background()
	var expired []string
	for _, installation := range []int64{7, 9} {
		token, err := client.Token(ctx, installation)
		if err != nil {
			t.Fatalf("token for installation %d: %v", installation, err)
		}
		expired = append(expired, token)
	}
	clock.advance(time.Hour + time.Minute)
	current, err := client.Token(ctx, 7)
	if err != nil {
		t.Fatalf("token past the first ones' expiry: %v", err)
	}
	for _, token := range expired {
		if scope := client.tokenScope(token); scope != "" {
			t.Fatalf("expired token %s still answers scope %q", token, scope)
		}
	}
	if scope := client.tokenScope(current); scope != installationScope(7) {
		t.Fatalf("current token's scope = %q, want %q", scope, installationScope(7))
	}
}

// A limit one process meets on the App's own calls (installation lookups, token mints, the
// installation list) holds the other process's App calls, with no request. A rate-limited mint is
// the typed *RateLimitError, so every caller tells it from a refusal.
func TestTheAppsLimitMetByOneProcessHoldsTheOthersAppCalls(t *testing.T) {
	fake := startLimitGitHub(t)
	database, a, b := twoProcesses(t, fake)
	ctx := context.Background()
	tokenB, err := b.Token(ctx, 7)
	if err != nil {
		t.Fatalf("B's token: %v", err)
	}

	fake.limit("/app/installations/9/access_tokens")
	_, err = a.Token(ctx, 9)
	if limited, ok := AsRateLimit(err); !ok || limited.Held || limited.Status != http.StatusForbidden || limited.Wait != time.Minute {
		t.Fatalf("A's limited mint: err = %v, want GitHub's own 403 as a *RateLimitError of a minute", err)
	}
	requests := fake.count()
	_, err = b.Token(ctx, 9)
	requireHeld(t, err, "B's token mint")
	_, err = b.ListInstallations(ctx)
	requireHeld(t, err, "B's installation list")
	_, err = b.Installation(ctx, "acme", "web")
	requireHeld(t, err, "B's installation lookup")
	if got := fake.count(); got != requests {
		t.Fatalf("GitHub got %d more requests inside the App's limit, want none", got-requests)
	}
	if _, status, _, err := b.Read(ctx, tokenB, "/repos/acme/web/issues/2"); err != nil || status != http.StatusOK {
		t.Fatalf("B's read under an installation's token inside the App's limit: status %d, %v; want GitHub's 200", status, err)
	}

	fake.limit("")
	passLimits(t, database)
	if _, err := b.Token(ctx, 9); err != nil {
		t.Fatalf("B's token mint once the limit passed: %v", err)
	}
	if installations, err := b.ListInstallations(ctx); err != nil || len(installations) != 1 {
		t.Fatalf("B's installation list once the limit passed: %v, %v", installations, err)
	}
}

// The webhook redelivery sweep's own backoff doubles to an hour under its own scope; the App's
// token mints obey only GitHub's own wait, so a strike against redelivery never holds them.
func TestARedeliveryStrikeDoesNotHoldATokenMint(t *testing.T) {
	fake := startLimitGitHub(t)
	database := storetest.Open(t)
	limits := ratelimit.New(database.Pool)
	ctx := context.Background()
	for range 7 {
		if _, err := limits.Strike(ctx, "redelivery", time.Minute); err != nil {
			t.Fatalf("strike: %v", err)
		}
	}
	client := fake.newClient(t, WithLimits(limits))
	if _, err := client.Token(ctx, 7); err != nil {
		t.Fatalf("token mint under a redelivery backoff: %v", err)
	}
	if got := fake.count("POST /app/installations/7/access_tokens"); got != 1 {
		t.Fatalf("token mints that reached GitHub = %d, want 1", got)
	}
}

// A shared limit that cannot be read leaves the request to GitHub, whose own answer still limits
// it: the database failing never stops every GitHub call.
func TestAnUnreadableLimitLetsTheRequestGo(t *testing.T) {
	fake := startLimitGitHub(t)
	gone := storetest.SecondProcess(t, storetest.Open(t))
	gone.Pool.Close()
	client := fake.newClient(t, WithLimits(ratelimit.New(gone.Pool)))
	if _, err := client.Token(context.Background(), 7); err != nil {
		t.Fatalf("token mint with the limits unreadable: %v", err)
	}
	if got := fake.count("POST /app/installations/7/access_tokens"); got != 1 {
		t.Fatalf("token mints that reached GitHub = %d, want 1", got)
	}
}

// testClock is a clock a test moves by hand.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// A repository's installation is remembered for installationsCacheTTL and no longer: an App moved
// to another installation is found by every process within ten minutes, not only once a token for
// the old installation fails to mint (it still mints here: the old installation exists).
func TestARepositorysInstallationIsLookedUpAgainAfterTheTTL(t *testing.T) {
	fake := startLimitGitHub(t)
	client := fake.newClient(t)
	clock := &testClock{now: time.Now()}
	client.now = clock.Now
	ctx := context.Background()

	token, err := client.RepositoryToken(ctx, "acme", "web")
	if err != nil || !strings.HasPrefix(token, "ghs_7_") {
		t.Fatalf("first token %q, %v; want installation 7's", token, err)
	}
	fake.moveInstallation(8)
	clock.advance(installationsCacheTTL - time.Minute)
	if token, err := client.RepositoryToken(ctx, "acme", "web"); err != nil || !strings.HasPrefix(token, "ghs_7_") {
		t.Fatalf("token inside the TTL %q, %v; want installation 7's, from the remembered installation", token, err)
	}
	if got := fake.count("GET /repos/acme/web/installation"); got != 1 {
		t.Fatalf("installation lookups inside the TTL = %d, want 1", got)
	}

	clock.advance(2 * time.Minute)
	if token, err := client.RepositoryToken(ctx, "acme", "web"); err != nil || !strings.HasPrefix(token, "ghs_8_") {
		t.Fatalf("token past the TTL %q, %v; want installation 8's, looked up again", token, err)
	}
	if got := fake.count("GET /repos/acme/web/installation"); got != 2 {
		t.Fatalf("installation lookups past the TTL = %d, want 2", got)
	}
}
