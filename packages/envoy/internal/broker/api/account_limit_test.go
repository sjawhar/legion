// account_limit_test.go drives the bucket every accountAuth route shares (api.AccountLimit) on the
// same real broker api_test.go mounts: it is spent before the caller's ServiceAccount token is
// verified, so a flood of forged tokens past it never reaches the verifier or the issuer's keys,
// and a listed caller's ordinary traffic across the class fits inside it.
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
