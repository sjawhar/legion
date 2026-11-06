// account_limit_test.go drives the bucket every accountAuth route shares (api.AccountLimit) on the
// same real broker api_test.go mounts: it is spent before the caller's ServiceAccount token is
// verified, so a flood of forged tokens past it never reaches the verifier or the issuer's keys,
// and a listed caller's ordinary traffic across the class fits inside it. That bucket is keyed on
// one address, so the file also drives what bounds a flood spread over many addresses, which every
// address's own bucket admits in full: the key cache the verifier reads through
// (internal/oidc.Keys), whose MinRefresh costs the issuer one read of its key set per window
// however many tokens naming keys it never published arrive. The approval page's own routes and
// GET /signin/status spend that same bucket, and each refuses in its own shape.
package api_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/approval"
	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

// countingAccountTokens is the test server's own account verifier, counting every token it is
// asked to verify: the work a request the bucket refuses must never reach.
type countingAccountTokens struct {
	enroll.PodVerifier
	verified atomic.Int64
}

func (c *countingAccountTokens) VerifyAccount(ctx context.Context, raw string) (enroll.AccountClaims, error) {
	c.verified.Add(1)
	return c.PodVerifier.VerifyAccount(ctx, raw)
}

// countVerifications wraps the account verifier newTestServerWith wires, for adjust to call.
func countVerifications(d *api.Deps) *countingAccountTokens {
	counting := &countingAccountTokens{PodVerifier: d.AccountTokens}
	d.AccountTokens = counting
	return counting
}

// accountCall is one request on an accountAuth route: GET /v1/pending for the approver, or, with
// a form, POST /v1/oauth/introspect, presenting bearer and arriving through the trusted proxy
// from address. It answers the status, the headers and the body: a refusal's Retry-After is one
// testServer.req does not give back.
func accountCall(t *testing.T, ts *testServer, introspect bool, bearer, address string) (int, http.Header, []byte) {
	t.Helper()
	method, path, body := http.MethodGet, "/v1/pending?approver="+url.QueryEscape(testApprover), io.Reader(nil)
	if introspect {
		method, path, body = http.MethodPost, "/v1/oauth/introspect", strings.NewReader(url.Values{"token": {"example-unknown-token"}}.Encode())
	}
	request, err := http.NewRequest(method, ts.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if introspect {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set("X-Forwarded-For", address)
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, answer
}

// TestAForgedAccountTokenFloodStopsReachingTheVerifier: tokens that name the listed account, the
// issuer and the audience, but that a key the issuer never published signed, are each refused 401
// while their address's bucket lasts — every one costing a verification and a fetch of the
// issuer's keys — and 429 RATE_LIMITED with Retry-After once it is empty, on every route of the
// class alike, reaching neither the verifier nor the issuer again. The bucket is keyed on the
// caller's own address behind the trusted proxy, not on the proxy's, so the listed caller arriving
// through the same proxy from its own address is still served, on the relay route and on
// introspection, each spending one request of its bucket and no more.
func TestAForgedAccountTokenFloodStopsReachingTheVerifier(t *testing.T) {
	var counting *countingAccountTokens
	ts := newTestServerWith(t, func(d *api.Deps) {
		d.AccountLimit = &ratelimit.Limit{Every: time.Hour, Burst: 2}
		d.TrustedProxyHeader = "X-Forwarded-For"
		counting = countVerifications(d)
	})
	forged := ts.podIssuer.Mint(t, oidctest.NewKey(t, "forger"), ts.podIssuer.Claims(dispatchAccount, podAudience))
	const flooder = "198.51.100.1"

	fetchesBefore := ts.podIssuer.JWKSFetches()
	for i, introspect := range []bool{false, true} {
		if status, _, body := accountCall(t, ts, introspect, forged, flooder); status != http.StatusUnauthorized || decode[wireError](t, body).Code != "ACCOUNT_TOKEN_INVALID" {
			t.Fatalf("forged token %d, inside the bucket = %d %s, want 401 ACCOUNT_TOKEN_INVALID", i, status, body)
		}
	}
	if got := counting.verified.Load(); got != 2 {
		t.Fatalf("verifications inside the bucket = %d, want 2", got)
	}
	fetchesAdmitted := ts.podIssuer.JWKSFetches()
	if fetchesAdmitted == fetchesBefore {
		t.Fatalf("the issuer's keys were fetched %d times for the admitted forgeries, want a refetch: the cost this bucket bounds", fetchesAdmitted-fetchesBefore)
	}

	for i := range 10 {
		status, headers, body := accountCall(t, ts, i%2 == 1, forged, flooder)
		if status != http.StatusTooManyRequests || headers.Get("Retry-After") != "3600" || decode[wireError](t, body).Code != "RATE_LIMITED" {
			t.Fatalf("forged token %d past the bucket = %d Retry-After=%q %s, want 429 RATE_LIMITED with Retry-After 3600", i, status, headers.Get("Retry-After"), body)
		}
	}
	if got := counting.verified.Load(); got != 2 {
		t.Fatalf("verifications after ten refused forgeries = %d, want still 2: a refused request must not reach the verifier", got)
	}
	if got := ts.podIssuer.JWKSFetches(); got != fetchesAdmitted {
		t.Fatalf("the issuer's keys were fetched %d more times for ten refused forgeries, want none", got-fetchesAdmitted)
	}

	const listed = "198.51.100.2"
	if status, _, body := accountCall(t, ts, false, ts.uiToken, listed); status != http.StatusOK {
		t.Fatalf("the listed caller's GET /v1/pending from its own address after the flood = %d %s, want 200", status, body)
	}
	if status, _, body := accountCall(t, ts, true, ts.uiToken, listed); status != http.StatusOK {
		t.Fatalf("the listed caller's introspection, its bucket's second request = %d %s, want 200: introspection spends one request, not two", status, body)
	}
	if got := counting.verified.Load(); got != 4 {
		t.Fatalf("verifications after the listed caller's two requests = %d, want 4", got)
	}
}

// TestAListedCallersOrdinaryTrafficIsNotRefused: at the default size (api.DefaultAccountLimit),
// the listed caller sending a whole burst at once from one address, relaying a person's page and
// introspecting in turn, is refused nothing, and every request is verified.
func TestAListedCallersOrdinaryTrafficIsNotRefused(t *testing.T) {
	var counting *countingAccountTokens
	ts := newTestServerWith(t, func(d *api.Deps) { counting = countVerifications(d) })
	sent := api.DefaultAccountLimit.Burst
	for i := range sent {
		if status, _, body := accountCall(t, ts, i%2 == 1, ts.uiToken, "198.51.100.3"); status != http.StatusOK {
			t.Fatalf("the listed caller's request %d of %d = %d %s, want 200", i, sent, status, body)
		}
	}
	if got := counting.verified.Load(); got != int64(sent) {
		t.Fatalf("verifications = %d, want %d: one for every request", got, sent)
	}
}

// floodTokens is how many forged tokens a flood below sends, one from each source address. It is
// well past the burst any one address's bucket holds at the tight size the per-address test uses,
// and past floodKeys, so neither the bucket nor a single key id can be what bounds the reads
// counted; sixty loopback requests take a small fraction of oidc.DefaultKeysMinRefresh.
const floodTokens = 60

// floodKeys is how many distinct key ids a flood's tokens name, none of them ever published. More
// than one, so what the count below proves is the refresh window the key cache opens once for the
// whole set rather than anything remembered per key id.
const floodKeys = 4

// floodAddress is the source address the i-th token of a flood arrives from, through the trusted
// proxy header: one address per token, so every one has a bucket of its own and reaches the
// verifier. They are documentation addresses (RFC 5737), as the rest of this file's are.
func floodAddress(i int) string { return fmt.Sprintf("198.51.100.%d", i+1) }

// listedAddress is where the listed caller's own request arrives from, outside the range
// floodAddress hands out, so the flood spends nothing of that caller's bucket.
const listedAddress = "198.51.100.200"

// forgedFlood mints count tokens that name the listed account, the issuer and the audience but
// that the issuer never signed: each is signed by one of floodKeys keys it never published and
// carries a jti of its own, so the verifier meets a stream of distinct tokens naming several
// unpublished key ids rather than one token replayed.
func forgedFlood(t *testing.T, issuer *oidctest.Issuer, count int) []string {
	t.Helper()
	keys := make([]*oidctest.Key, floodKeys)
	for i := range keys {
		keys[i] = oidctest.NewKey(t, fmt.Sprintf("never-published-%d", i))
	}
	flood := make([]string, count)
	for i := range flood {
		claims := issuer.Claims(dispatchAccount, podAudience)
		claims["jti"] = fmt.Sprintf("forgery-%d", i)
		flood[i] = issuer.Mint(t, keys[i%len(keys)], claims)
	}
	return flood
}

// TestAForgedAccountTokenFloodFromManyAddressesCostsOneFetchPerMinute: the bucket above bounds one
// address, and a flood spread over many defeats it — each address gets its own burst, so every
// forgery passes the limit and is verified. What bounds the issuer's cost then is the key cache
// the account verifier reads through (internal/oidc.Keys): sixty tokens signed by keys the issuer
// never published, every one from a different address through the trusted proxy header, are each
// refused 401 ACCOUNT_TOKEN_INVALID — so the flood really did reach the verification this counts —
// and together cost the issuer one read of its key set, not one per request.
func TestAForgedAccountTokenFloodFromManyAddressesCostsOneFetchPerMinute(t *testing.T) {
	var counting *countingAccountTokens
	ts := newTestServerWith(t, func(d *api.Deps) {
		d.TrustedProxyHeader = "X-Forwarded-For"
		counting = countVerifications(d)
	})
	// The listed caller's own valid token first: it leaves the issuer's key set cached and fresh
	// and no miss window open, so every read counted after it is one the flood itself cost.
	if status, _, body := accountCall(t, ts, false, ts.uiToken, listedAddress); status != http.StatusOK {
		t.Fatalf("the listed caller's GET /v1/pending = %d %s, want 200: the key set has to be cached before the flood", status, body)
	}
	verifiedBefore, fetchesBefore := counting.verified.Load(), ts.podIssuer.JWKSFetches()

	flood := forgedFlood(t, ts.podIssuer, floodTokens)
	started := time.Now()
	for i, forged := range flood {
		if status, _, body := accountCall(t, ts, i%2 == 1, forged, floodAddress(i)); status != http.StatusUnauthorized || decode[wireError](t, body).Code != "ACCOUNT_TOKEN_INVALID" {
			t.Fatalf("forged token %d from %s = %d %s, want 401 ACCOUNT_TOKEN_INVALID", i, floodAddress(i), status, body)
		}
	}
	elapsed := time.Since(started)

	if got := counting.verified.Load() - verifiedBefore; got != floodTokens {
		t.Fatalf("verifications for the flood = %d, want %d: one address per token, so every address's own bucket admits its one request and every forgery reaches the verifier", got, floodTokens)
	}
	// One read, and the arithmetic is this. The set the request above cached is younger than
	// oidc.DefaultKeysLifetime (fifteen minutes), so no verification here re-reads the issuer for
	// staleness. Each forgery names a key id that set does not hold, which is a miss, and
	// oidc.DefaultKeysMinRefresh (a minute) lets a miss prompt a read once per minute: the first
	// forgery opens that window and costs the one read, and the other floodTokens-1 arrive inside
	// the same window — sixty loopback requests take milliseconds, and the elapsed time is in the
	// failure below if it ever does not — and cost nothing. 1 + 0 = 1. Without the window it would
	// be floodTokens, which is what the sibling test below measures.
	if got := ts.podIssuer.JWKSFetches() - fetchesBefore; got != 1 {
		t.Fatalf("the issuer's key set was read %d times for %d forged tokens from %d addresses in %s, want exactly 1: oidc.DefaultKeysMinRefresh is %s, so the first miss costs one read and the rest fall inside its window", got, floodTokens, floodTokens, elapsed, oidc.DefaultKeysMinRefresh)
	}
}

// TestAForgedAccountTokenFloodCostsAFetchEachWithoutTheKeyCachesMinimumRefresh is the negative
// control for the bound above, on the same routes and the same flood: a broker whose account
// verifier reads through a key cache with no minimum refresh (MinRefresh 0, every miss free to
// read again, which is what go-oidc's own RemoteKeySet does) pays the issuer one key-set read for
// every forged token. The one read the test above counts is that bound working, not an issuer
// nobody asked.
func TestAForgedAccountTokenFloodCostsAFetchEachWithoutTheKeyCachesMinimumRefresh(t *testing.T) {
	issuer := oidctest.New(t)
	published := issuer.PublishKey(t, "signing-key")
	keys, err := oidc.NewKeys(context.Background(), issuer.URL())
	if err != nil {
		t.Fatalf("oidc.NewKeys: %v", err)
	}
	keys.MinRefresh = 0

	var counting *countingAccountTokens
	ts := newTestServerWith(t, func(d *api.Deps) {
		d.TrustedProxyHeader = "X-Forwarded-For"
		d.AccountTokens = enroll.K8sPodVerifier{Verifier: oidc.NewWithKeys(issuer.URL(), podAudience, keys)}
		counting = countVerifications(d)
	})

	// A valid token of this issuer first, as above: it caches the set, so the reads counted below
	// are the misses' own and not a cold cache's.
	now := time.Now()
	valid := issuer.Claims(dispatchAccount, podAudience)
	valid["iat"], valid["exp"] = now.Add(-5*time.Second).Unix(), now.Add(time.Hour).Unix()
	if status, _, body := accountCall(t, ts, false, issuer.Mint(t, published, valid), listedAddress); status != http.StatusOK {
		t.Fatalf("the listed caller's GET /v1/pending = %d %s, want 200: the key set has to be cached before the flood", status, body)
	}
	verifiedBefore, fetchesBefore := counting.verified.Load(), issuer.JWKSFetches()

	for i, forged := range forgedFlood(t, issuer, floodTokens) {
		if status, _, body := accountCall(t, ts, i%2 == 1, forged, floodAddress(i)); status != http.StatusUnauthorized || decode[wireError](t, body).Code != "ACCOUNT_TOKEN_INVALID" {
			t.Fatalf("forged token %d from %s = %d %s, want 401 ACCOUNT_TOKEN_INVALID", i, floodAddress(i), status, body)
		}
	}
	if got := counting.verified.Load() - verifiedBefore; got != floodTokens {
		t.Fatalf("verifications for the flood = %d, want %d: every forgery has to reach the verifier for its reads to be counted", got, floodTokens)
	}
	if got := issuer.JWKSFetches() - fetchesBefore; got != floodTokens {
		t.Fatalf("the issuer's key set was read %d times for %d forged tokens against a cache with no minimum refresh, want %d: one for every token, since every miss reopens the window at once", got, floodTokens, floodTokens)
	}
}

// browserCall is one request a browser makes on a route of the approval page, arriving through
// the trusted proxy from address with no credential at all. It follows no redirect, since the
// sign-in a page starts inside the bucket leads to the provider, and it answers the status, the
// headers and the body: a refusal's Retry-After and the page's own headers are what this file
// reads.
func browserCall(t *testing.T, ts *testServer, path, address string) (int, http.Header, []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Forwarded-For", address)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, answer
}

// TestAFloodedPageAnswersThePagesOwn429AndNamesNoToken: the approval page's routes spend the
// bucket the routes a ServiceAccount token authenticates spend, and past it a browser gets the
// page's own refusal — 429 with Retry-After, under the headers every answer of a page route
// carries, saying in the page's HTML that too many requests came from this address and naming
// neither a ServiceAccount nor a token it never sent. GET /signin/status, being JSON, is refused
// in JSON, and names the address rather than a token too. The page's stylesheet is refused at no
// point, since it spends nothing, and inside the burst no route of either kind is refused.
func TestAFloodedPageAnswersThePagesOwn429AndNamesNoToken(t *testing.T) {
	ts := newTestServerWith(t, func(d *api.Deps) {
		d.AccountLimit = &ratelimit.Limit{Every: time.Hour, Burst: 3}
		d.TrustedProxyHeader = "X-Forwarded-For"
	})
	const (
		flooder = "198.51.100.4"
		// A record id's shape, which no record of this broker has: the refusal comes before the
		// page reads anything, and inside the bucket an unknown id starts a sign-in like any
		// other, so what the page does with it is beside the point here.
		recordID   = "c92c8c55d333d24204fcb453596cf1c784ea2755e6fc963c1176314c3e2bee9f"
		stylesheet = "/approve/assets/approve.css"
	)
	page := "/approve/" + recordID
	spenders := []string{page, "/signin/callback", "/signin/status"}

	for _, path := range spenders {
		if status, _, body := browserCall(t, ts, path, flooder); status == http.StatusTooManyRequests {
			t.Fatalf("GET %s inside the bucket = %d %s, want anything but a refusal", path, status, body)
		}
	}
	if status, _, body := browserCall(t, ts, stylesheet, flooder); status != http.StatusOK {
		t.Fatalf("the stylesheet inside the bucket = %d %s, want 200", status, body)
	}

	for _, path := range spenders[:2] {
		status, headers, body := browserCall(t, ts, path, flooder)
		if status != http.StatusTooManyRequests || headers.Get("Retry-After") != "3600" {
			t.Fatalf("GET %s past the bucket = %d Retry-After=%q %s, want 429 with Retry-After 3600",
				path, status, headers.Get("Retry-After"), body)
		}
		for header, want := range map[string]string{
			"Content-Security-Policy": approval.ContentSecurityPolicy,
			"X-Frame-Options":         "DENY",
			"Referrer-Policy":         "same-origin",
			"Cache-Control":           "no-store",
			"X-Content-Type-Options":  "nosniff",
			"Content-Type":            "text/html; charset=utf-8",
		} {
			if got := headers.Get(header); got != want {
				t.Errorf("GET %s past the bucket: %s = %q, want %q", path, header, got, want)
			}
		}
		text := string(body)
		if !strings.Contains(text, "<html") || !strings.Contains(text, "Too many requests") {
			t.Errorf("GET %s past the bucket answered:\n%s\nwant the page's own refusal page", path, text)
		}
		for _, word := range []string{"serviceaccount", "token"} {
			if strings.Contains(strings.ToLower(text), word) {
				t.Errorf("GET %s past the bucket names %q, which this caller never sent:\n%s", path, word, text)
			}
		}
	}

	status, headers, body := browserCall(t, ts, "/signin/status", flooder)
	if status != http.StatusTooManyRequests || headers.Get("Retry-After") != "3600" || decode[wireError](t, body).Code != "RATE_LIMITED" {
		t.Fatalf("GET /signin/status past the bucket = %d Retry-After=%q %s, want 429 RATE_LIMITED with Retry-After 3600",
			status, headers.Get("Retry-After"), body)
	}
	for _, word := range []string{"serviceaccount", "token"} {
		if strings.Contains(strings.ToLower(string(body)), word) {
			t.Errorf("GET /signin/status past the bucket names %q, which no caller of it sends: %s", word, body)
		}
	}
	if status, _, body := browserCall(t, ts, stylesheet, flooder); status != http.StatusOK {
		t.Fatalf("the stylesheet past the bucket = %d %s, want 200: it spends nothing", status, body)
	}
}
