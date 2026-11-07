// account_google_test.go drives the other kind of caller BROKER_API_ACCOUNTS lists: a machine
// holding a Google identity token rather than a Kubernetes projected one, which is the Envoy
// listener on a VM. It is the same authority and the same refusals as a Kubernetes caller's, on
// the same routes; what differs is which verifier judges the token and which claim the broker
// looks the caller up by.
package api_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const (
	// listenerGoogleAccount is the Google service account BROKER_API_ACCOUNTS names as the
	// caller "listener"; strangerGoogleAccount is one of the same fleet's shape that it does
	// not name. No real account appears: this repository is public.
	listenerGoogleAccount = "example-listener@example-project.iam.gserviceaccount.com"
	strangerGoogleAccount = "example-stranger@example-project.iam.gserviceaccount.com"
	// machineAudience is BROKER_GOOGLE_OIDC_AUDIENCE here: the audience every machine of the
	// deployment already holds an identity token for, and so the one a listed caller's token
	// must carry.
	machineAudience = "https://broker.example/machines"
)

// googleCallers is a broker whose BROKER_API_ACCOUNTS lists one Google account as the listener
// caller, beside the Kubernetes dispatch caller the rig already lists: the test server, the
// issuer standing in for Google and the key it signs with.
type googleCallers struct {
	*testServer
	issuer *oidctest.Issuer
	key    *oidctest.Key
}

// newGoogleCallerServer wires that broker. Google's issuer is one of its own, distinct from the
// cluster's, which is what lets the broker send each token to the verifier that issued it.
func newGoogleCallerServer(t *testing.T) *googleCallers {
	t.Helper()
	google := oidctest.New(t)
	key := google.PublishKey(t, "google-key")
	verifier, err := oidc.New(context.Background(), google.URL(), machineAudience)
	if err != nil {
		t.Fatalf("oidc.New for Google: %v", err)
	}
	ts := newTestServerWith(t, func(d *api.Deps) {
		d.GoogleAPIAccounts = map[string]api.Account{listenerGoogleAccount: brokertest.Caller(t, "listener")}
		d.GoogleAccountTokens = api.GoogleTokens{Verifier: verifier}
	})
	return &googleCallers{testServer: ts, issuer: google, key: key}
}

// token is a Google identity token as a machine's metadata server hands one out: the account in
// both sub and email, the audience the broker holds it to, and whatever lifetime and
// email_verified the row under test asks for.
func (g *googleCallers) token(t *testing.T, account, audience string, verified any, issued, expires time.Time) string {
	t.Helper()
	claims := g.issuer.Claims(account, audience)
	claims["email"] = account
	claims["email_verified"] = verified
	claims["iat"] = issued.Unix()
	claims["nbf"] = issued.Unix()
	claims["exp"] = expires.Unix()
	return g.issuer.Mint(t, g.key, claims)
}

// live is the token a listed listener presents on a good day: minted seconds ago, for the
// broker's audience, naming a verified account.
func (g *googleCallers) live(t *testing.T, account string) string {
	t.Helper()
	now := time.Now()
	return g.token(t, account, machineAudience, true, now.Add(-5*time.Second), now.Add(time.Hour))
}

// introspect reads a token back as a listed caller does: the caller's own identity token on the
// Authorization header, the token to read in the form.
func (g *googleCallers) introspect(t *testing.T, bearer string) (int, []byte) {
	t.Helper()
	return g.form(t, "/v1/oauth/introspect", bearer, url.Values{"token": {"a-token-nobody-issued"}})
}

// form posts a form to path with bearer on the Authorization header.
func (g *googleCallers) form(t *testing.T, path, bearer string, values url.Values) (int, []byte) {
	t.Helper()
	headers := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	if bearer != "" {
		headers["Authorization"] = "Bearer " + bearer
	}
	return g.raw(t, http.MethodPost, path, headers, strings.NewReader(values.Encode()))
}

// raw issues one request whose body is sent as given rather than marshalled to JSON, which is
// what a form-encoded route takes; testServer.req is the JSON one every other test uses.
func (ts *testServer) raw(t *testing.T, method, path string, headers map[string]string, body io.Reader) (int, []byte) {
	t.Helper()
	request, err := http.NewRequest(method, ts.URL+path, body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	return response.StatusCode, answer
}

// TestAGoogleCallerIsAnsweredOnTheRoutesItsNameCarries: a machine holding a Google identity
// token for a listed account reaches introspection, the one route the listener caller's name
// carries, and is refused the relay routes 403 ACCOUNT_LACKS_POWER exactly as a Kubernetes
// listener is — the kind of token says who the caller is, never what it may do.
func TestAGoogleCallerIsAnsweredOnTheRoutesItsNameCarries(t *testing.T) {
	g := newGoogleCallerServer(t)
	token := g.live(t, listenerGoogleAccount)

	status, body := g.introspect(t, token)
	if status != http.StatusOK || !strings.Contains(string(body), `"active":false`) {
		t.Fatalf("introspection as the Google listener = %d %s, want 200 and an inactive token: a listed Google caller reaches the route its name carries", status, body)
	}

	status, body = g.raw(t, http.MethodGet, "/v1/pending?approver="+url.QueryEscape(testApprover),
		map[string]string{"Authorization": "Bearer " + token}, nil)
	if refusal := decode[wireError](t, body); status != http.StatusForbidden || refusal.Code != "ACCOUNT_LACKS_POWER" {
		t.Fatalf("GET /v1/pending as the Google listener = %d %s, want 403 ACCOUNT_LACKS_POWER: the listener relays no person's decision, whatever it holds", status, body)
	}
}

// TestTheKubernetesCallerStillCallsBesideAGoogleOne: a broker that lists both kinds judges each
// token by its own issuer, so the dispatch caller's projected token keeps working unchanged.
func TestTheKubernetesCallerStillCallsBesideAGoogleOne(t *testing.T) {
	g := newGoogleCallerServer(t)
	if status, body := g.introspect(t, g.uiToken); status != http.StatusOK {
		t.Fatalf("introspection as the Kubernetes dispatch caller = %d %s, want 200", status, body)
	}
}

// TestEveryGoogleCallerRefusalKeepsItsMeaning walks the taxonomy accountCaller already had and
// pins what each code means for a Google token: an account the setting does not name is the
// broker's list refusing a sound credential (403), and every other row is the credential itself
// (401).
func TestEveryGoogleCallerRefusalKeepsItsMeaning(t *testing.T) {
	g := newGoogleCallerServer(t)
	now := time.Now()
	for _, row := range []struct {
		name, token string
		status      int
		code        string
	}{
		{
			name:   "an account BROKER_API_ACCOUNTS does not name",
			token:  g.live(t, strangerGoogleAccount),
			status: http.StatusForbidden, code: "ACCOUNT_NOT_LISTED",
		},
		{
			name:   "a token that has expired",
			token:  g.token(t, listenerGoogleAccount, machineAudience, true, now.Add(-2*time.Hour), now.Add(-time.Hour)),
			status: http.StatusUnauthorized, code: "ACCOUNT_TOKEN_EXPIRED",
		},
		{
			name:   "a token minted for another audience",
			token:  g.token(t, listenerGoogleAccount, "https://somewhere.example/else", true, now.Add(-5*time.Second), now.Add(time.Hour)),
			status: http.StatusUnauthorized, code: "ACCOUNT_TOKEN_INVALID",
		},
		{
			name:   "a token that is not a token at all",
			token:  "not-a-jwt",
			status: http.StatusUnauthorized, code: "ACCOUNT_TOKEN_INVALID",
		},
		{
			name:   "a token whose email_verified is not true",
			token:  g.token(t, listenerGoogleAccount, machineAudience, false, now.Add(-5*time.Second), now.Add(time.Hour)),
			status: http.StatusUnauthorized, code: "ACCOUNT_TOKEN_INVALID",
		},
		{
			name:   "a token that lives longer than an hour from its issue",
			token:  g.token(t, listenerGoogleAccount, machineAudience, true, now.Add(-5*time.Second), now.Add(4*time.Hour)),
			status: http.StatusUnauthorized, code: "ACCOUNT_TOKEN_TOO_LONG_LIVED",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			status, body := g.introspect(t, row.token)
			refusal := decode[wireError](t, body)
			if status != row.status || refusal.Code != row.code {
				t.Fatalf("introspection with %s = %d %s, want %d %s", row.name, status, body, row.status, row.code)
			}
			if strings.Contains(refusal.Error, row.token) {
				t.Errorf("the refusal repeats the caller's token: %q", refusal.Error)
			}
		})
	}
}

// TestWithNoIssuerAtAllEveryAccountCallerIsOff: a broker told about neither a cluster nor Google
// authenticates no caller on these routes, which is the one refusal that is about the broker and
// not about the token.
func TestWithNoIssuerAtAllEveryAccountCallerIsOff(t *testing.T) {
	ts := newTestServerWith(t, func(d *api.Deps) {
		d.AccountTokens = nil
		d.GoogleAccountTokens = nil
	})
	status, body := ts.raw(t, http.MethodPost, "/v1/oauth/introspect",
		map[string]string{"Authorization": "Bearer anything", "Content-Type": "application/x-www-form-urlencoded"},
		strings.NewReader("token=a-token-nobody-issued"))
	if refusal := decode[wireError](t, body); status != http.StatusUnauthorized || refusal.Code != "ACCOUNT_TOKENS_OFF" {
		t.Fatalf("introspection on a broker that trusts no issuer = %d %s, want 401 ACCOUNT_TOKENS_OFF", status, body)
	}
}
