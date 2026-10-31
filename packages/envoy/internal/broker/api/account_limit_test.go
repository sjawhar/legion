// account_limit_test.go drives the bucket every accountAuth route shares (api.AccountLimit) on the
// same real broker api_test.go mounts: it is spent before the caller's ServiceAccount token is
// verified, so a flood of forged tokens past it never reaches the verifier or the issuer's keys,
// and a listed caller's ordinary traffic across the class fits inside it. The approval page's own
// routes and GET /signin/status spend that same bucket, and each refuses in its own shape.
package api_test

import (
	"context"
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
	"github.com/sjawhar/envoy/internal/broker/ratelimit"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
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
