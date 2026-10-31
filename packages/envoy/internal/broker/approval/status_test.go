package approval_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// signInStatus is GET /signin/status's answer, as a caller with no credential reads it.
type signInStatus struct {
	ClientCheck            *string    `json:"client_check"`
	ClientCheckedAt        *time.Time `json:"client_checked_at"`
	LastSignInAt           *time.Time `json:"last_sign_in_at"`
	LastFailedSignInAt     *time.Time `json:"last_failed_sign_in_at"`
	LastFailedSignInReason *string    `json:"last_failed_sign_in_reason"`
	LastDecisionAt         *time.Time `json:"last_decision_at"`
}

// signInStatus fetches the route as a production check does - a plain client with no cookie, no
// token and no header of its own - and answers its body as read and as parsed, failing t on
// anything but a 200 of JSON.
func (r *rig) signInStatus(t *testing.T) (signInStatus, string) {
	t.Helper()
	res, err := r.srv.Client().Get(r.url + "/signin/status")
	if err != nil {
		t.Fatalf("GET /signin/status: %v", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("GET /signin/status with no credential = %d %s:\n%s", res.StatusCode, res.Header.Get("Content-Type"), body)
	}
	var status signInStatus
	if err := json.Unmarshal(body, &status); err != nil {
		t.Fatalf("GET /signin/status answered %q: %v", body, err)
	}
	return status, string(body)
}

// TestSignInStatusReportsWhatHappenedOnThePage drives the public route through everything that
// writes it: before anything has happened every fact is null, a sign-in that completes sets its
// own time and nothing else, a decision sets the decision's time, and a sign-in that fails sets
// a coded reason. No answer carries the person's login or the record they decided.
func TestSignInStatusReportsWhatHappenedOnThePage(t *testing.T) {
	r := newRig(t)
	enrollmentID, key := r.benSession(t, "ben-box")
	p := r.request(t, enrollmentID, key, "deploy", nil, "", "TEST_SECRET")

	status, body := r.signInStatus(t)
	if status != (signInStatus{}) {
		t.Fatalf("GET /signin/status before anything happened = %s, want every fact null", body)
	}
	for _, want := range []string{`"client_check":null`, `"last_sign_in_at":null`, `"last_failed_sign_in_reason":null`} {
		if !strings.Contains(body, want) {
			t.Errorf("the status names no %s: %s", want, body)
		}
	}

	before := time.Now().Add(-time.Second)
	b := r.browser(t)
	b.SignIn(t, p.recordID, person(ada))
	status, body = r.signInStatus(t)
	switch {
	case status.LastSignInAt == nil || status.LastSignInAt.Before(before):
		t.Fatalf("after ada signed in the status = %s, want the sign-in's own time", body)
	case status.LastFailedSignInAt != nil || status.LastFailedSignInReason != nil || status.LastDecisionAt != nil:
		t.Fatalf("ada's sign-in wrote a failure or a decision too: %s", body)
	}
	signedIn := *status.LastSignInAt

	b.Decide(t, p.recordID, true)
	status, body = r.signInStatus(t)
	switch {
	case status.LastDecisionAt == nil || status.LastDecisionAt.Before(signedIn):
		t.Fatalf("after ada's decision the status = %s, want the decision's own time", body)
	case !status.LastSignInAt.Equal(signedIn):
		t.Fatalf("the decision moved the last sign-in's time: %s", body)
	}

	// A failure now: a code the provider will not exchange. The facts already written stay.
	b2 := r.browser(t)
	callback := b2.Authorize(t, b2.StartSignIn(t, "/approve/"+p.recordID), person(ada))
	u, err := url.Parse(callback)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("code", "forged-code")
	u.RawQuery = q.Encode()
	if res := b2.Get(t, u.String()); res.Status != http.StatusForbidden {
		t.Fatalf("the callback with a forged code = %d, want 403:\n%s", res.Status, res.Body)
	}
	status, body = r.signInStatus(t)
	switch {
	case status.LastFailedSignInReason == nil || *status.LastFailedSignInReason != "invalid_grant":
		t.Fatalf("after the refused exchange the status = %s, want the reason invalid_grant", body)
	case status.LastFailedSignInAt == nil:
		t.Fatalf("the failed sign-in has no time: %s", body)
	case !status.LastSignInAt.Equal(signedIn) || status.LastDecisionAt == nil:
		t.Fatalf("the failure lost an earlier fact: %s", body)
	}

	// Nothing in the answer names a person, a record or a secret.
	for _, forbidden := range []string{ada, ben, p.recordID, p.requestID, "TEST_SECRET", "forged-code"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("the public status carries %q: %s", forbidden, body)
		}
	}
}

// TestSignInStatusWritesAFailedSignInWithACodeFromTheClosedSet drives each failure the codes name through
// the real callback and pins the word written for it. A failure none of them names writes
// nothing at all rather than a word meaning "something else": the last named failure stands.
func TestSignInStatusWritesAFailedSignInWithACodeFromTheClosedSet(t *testing.T) {
	for _, c := range []struct {
		name string
		// spoil changes the claims of every ID token the provider mints, before the sign-in.
		spoil func(claims map[string]any)
		// claims is what the person signs in as, person(ada) when nil.
		claims map[string]any
		// sign re-arms the provider's client with a key it does not publish.
		foreignKey bool
		want       string
	}{
		{name: "a wrong issuer", spoil: func(c map[string]any) { c["iss"] = "https://issuer.example.com" }, want: "bad_issuer"},
		{name: "a wrong audience", spoil: func(c map[string]any) { c["aud"] = "another-client" }, want: "bad_audience"},
		{name: "a wrong nonce", spoil: func(c map[string]any) { c["nonce"] = "another-nonce" }, want: "bad_nonce"},
		{name: "a signature by a key the provider does not serve", foreignKey: true, want: "bad_signature"},
		{name: "an account of another domain", claims: map[string]any{"sub": "sub-" + ada, "email": ada, "email_verified": true, "hd": "other.example"}, want: "not_in_domain"},
		{name: "an unverified email", claims: map[string]any{"sub": "sub-" + ada, "email": ada, "email_verified": false, "hd": domain}, want: "email_not_verified"},
		{name: "an ID token naming no email", claims: map[string]any{"sub": "sub-" + ada, "email_verified": true, "hd": domain}, want: ""},
		{name: "an expired ID token", spoil: func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }, want: ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			enrollmentID, key := r.benSession(t, "ben-box")
			p := r.request(t, enrollmentID, key, "deploy", nil, "", "TEST_SECRET")
			// One named failure first, so a case that must write nothing is seen not to: the
			// reason afterwards is still this one.
			r.failSignIn(t, p.recordID, nil, false, func(u *url.URL) {
				q := u.Query()
				q.Set("code", "forged-code")
				u.RawQuery = q.Encode()
			})
			if status, body := r.signInStatus(t); status.LastFailedSignInReason == nil || *status.LastFailedSignInReason != "invalid_grant" {
				t.Fatalf("the first refused sign-in = %s, want invalid_grant", body)
			}

			if c.spoil != nil {
				r.issuer.SpoilIDTokens(c.spoil)
				t.Cleanup(func() { r.issuer.SpoilIDTokens(nil) })
			}
			claims := c.claims
			if claims == nil {
				claims = person(ada)
			}
			r.failSignIn(t, p.recordID, claims, c.foreignKey, nil)

			status, body := r.signInStatus(t)
			want := c.want
			if want == "" {
				want = "invalid_grant" // unchanged: the failure this case makes has no code
			}
			if status.LastFailedSignInReason == nil || *status.LastFailedSignInReason != want {
				t.Fatalf("after %s the reason = %s, want %q", c.name, body, want)
			}
			if r.sessions(t) != 0 {
				t.Fatalf("a refused sign-in made a session")
			}
		})
	}
}

// failSignIn takes one sign-in at the provider as claims and sends its callback back to the
// broker, which must refuse it. foreignKey re-arms the provider's client with a key of the same
// id that it does not publish, so its ID tokens do not verify; spoilCallback, when given,
// changes the callback URL before the browser follows it.
func (r *rig) failSignIn(t *testing.T, recordID string, claims map[string]any, foreignKey bool, spoilCallback func(*url.URL)) {
	t.Helper()
	if claims == nil {
		claims = person(ada)
	}
	b := r.browser(t)
	authURL := b.StartSignIn(t, "/approve/"+recordID)
	if foreignKey {
		// Under the published key's own id, so the key set resolves and only the signature
		// fails - oidctest.Key.KeyID's one shape.
		r.issuer.EnableCodeFlow(oidctest.NewKey(t, "signing-key"), clientID, brokertest.SignInClientSecret)
	}
	callback := b.Authorize(t, authURL, claims)
	u, err := url.Parse(callback)
	if err != nil {
		t.Fatal(err)
	}
	if spoilCallback != nil {
		spoilCallback(u)
	}
	if res := b.Get(t, u.String()); res.Status != http.StatusForbidden {
		t.Fatalf("the refused callback = %d, want 403:\n%s", res.Status, res.Body)
	}
}

// TestSignInStatusWritesIssuerUnreachableWhenTheProviderCannotBeReached: a callback is a sign-in
// holding a code, so a provider that cannot be read there is an exchange that could not be made.
func TestSignInStatusWritesIssuerUnreachableWhenTheProviderCannotBeReached(t *testing.T) {
	r := newRig(t)
	enrollmentID, key := r.benSession(t, "ben-box")
	p := r.request(t, enrollmentID, key, "deploy", nil, "", "TEST_SECRET")

	// The sign-in in progress is written by hand, not started on the page: the page reads the
	// provider's discovery document when it starts one, and a read that has succeeded is kept
	// for the life of the process, so a sign-in started here would leave the callback with a
	// provider it could still reach.
	r.issuer.FailDiscovery(http.StatusInternalServerError)
	pending := map[string]string{
		"state": "a-state", "nonce": "a-nonce", "verifier": strings.Repeat("v", 43),
		"next": "/approve/" + p.recordID,
	}
	res := r.getWith(t, "/signin/callback?"+url.Values{"code": {"a-code"}, "state": {pending["state"]}}.Encode(),
		&http.Cookie{Name: "__Host-broker-signin", Value: encodePending(t, pending)})
	if res.Status != http.StatusServiceUnavailable {
		t.Fatalf("the callback with the provider down = %d, want 503:\n%s", res.Status, res.Body)
	}
	status, body := r.signInStatus(t)
	if status.LastFailedSignInReason == nil || *status.LastFailedSignInReason != "issuer_unreachable" {
		t.Fatalf("the status after a callback the provider could not answer = %s, want issuer_unreachable", body)
	}
}
