// packages/envoy/internal/broker/brokertest/client.go

package brokertest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net"
	"net/http"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
)

// clientTimeout bounds every request a Client makes. It sits past the broker's own request
// deadline (cmd/broker's requestDeadline, 45 seconds) so a handler that takes its whole budget —
// one waiting on a row another process holds, say — still answers this client rather than being
// turned into a transport error that names nothing.
const clientTimeout = 90 * time.Second

// deviceTokenTimeout bounds a device-code exchange. x/oauth2's DeviceAccessToken polls until the
// device code expires (fifteen minutes here) whenever the broker answers authorization_pending or
// slow_down, so an exchange that goes wrong in that direction would otherwise hang a row for the
// whole of it instead of failing with what the broker kept saying.
const deviceTokenTimeout = 60 * time.Second

// pollInterval is the interval a Client's device-code exchange polls on, overriding the one the
// device authorization answered: every code a test exchanges has already been approved, so the
// first poll is the only one, and a second of it is all the delay a row should pay.
const pollInterval = 1

// Client drives one broker over genuine HTTP: every URL it builds, signs and sends names
// PublicURL, while every connection it opens goes to one process's real bound address. It is what
// a Rig's own Req/UI pair is for an in-process broker, for the tests that drive broker processes
// instead, and for the one case a Rig cannot express at all — two processes serving one database,
// where a record signed once has to be acceptable to either.
type Client struct {
	// PublicURL is the broker's BROKER_PUBLIC_URL: the value every DPoP proof's htu, every
	// session or launcher proof's htu, and every signed request object's audience names. It is
	// never the address the connection goes to.
	PublicURL string
	// Addr is the host:port this client's connections actually go to, named in a failure so a
	// message about two processes says which one answered.
	Addr string
	// UIToken is the bearer this client puts on the human/operator-facing routes. It is the
	// package's own UIToken, which a Rig's broker takes; a caller driving a broker configured with
	// another bearer (cmd/broker's BROKER_UI_TOKEN) replaces it rather than this package growing a
	// second constant for that broker's token.
	UIToken string
	// DPoP wraps this client's OAuth connections in DPoP proofs signed by a machine's key, which
	// the device authorization, the device-code exchange and every refresh carry:
	// helper.NewDPoPTransport in every caller. It is a field a caller sets rather than an import
	// of internal/broker/helper because that package's own tests import this one, and Go allows
	// no package to import one whose tests import it back. A client whose OAuth steps are never
	// used needs none; one whose are fails with the name of the field to set.
	DPoP func(key *ecdsa.PrivateKey, base http.RoundTripper) http.RoundTripper

	// transport dials Addr whatever host a URL names; the DPoP transports an OAuth exchange needs
	// are layered over this one, so they are pinned to the same process.
	transport http.RoundTripper
	http      *http.Client
}

// NewClient drives a broker reachable at addr as though it were reachable at publicURL: its
// transport dials addr whatever host a URL names, so every URL it builds — and so every proof's
// htu and every request object's audience — can name publicURL, which need not resolve to
// anything.
//
// That indirection is the whole reason this type exists. The broker authenticates against the URL
// it believes it is published at, never the one the client dialled: api/server.go's authenticate
// verifies a proof against deps.PublicURL plus the request's path, and api/handlers_oauth.go's
// atPublicURL rewrites every OAuth request to BROKER_PUBLIC_URL before the provider checks its
// DPoP proof. In production the pods of one broker Deployment all carry the same
// BROKER_PUBLIC_URL, their Service's, and a load balancer sends each request to one pod or
// another — which is exactly what makes a device code, a refresh token or a signed record minted
// through one pod redeemable through the next. A client per process, each pinned to that
// process's own bound address and all of them naming the one public URL, is that load balancer.
// Giving each process a public URL of its own would instead make everything signed for one of
// them refused by the other, and would prove nothing about the deployment this test stands for.
func NewClient(t *testing.T, publicURL, addr string) *Client {
	t.Helper()
	transport := &http.Transport{
		// Proxy is deliberately nil rather than http.ProxyFromEnvironment: a proxy variable in
		// the environment would send these requests somewhere other than the one process this
		// client exists to be pinned to, and publicURL resolves nowhere, so the failure would be
		// a DNS error naming a host no test chose.
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, addr)
		},
	}
	t.Cleanup(transport.CloseIdleConnections)
	return &Client{
		PublicURL: publicURL, Addr: addr, UIToken: UIToken,
		transport: transport,
		http:      &http.Client{Transport: transport, Timeout: clientTimeout},
	}
}

// Req issues method against path on the broker this client is pinned to, with an optional body
// (marshaled to JSON) and headers, returning the status and the body bytes. Like Rig.Req it
// asserts no status itself: a caller does that, so a mismatch names the actual body.
func (c *Client) Req(t *testing.T, method, path string, headers map[string]string, body any) (int, []byte) {
	t.Helper()
	return request(t, c.http, c.PublicURL, method, path, headers, body, " (at "+c.Addr+")")
}

// UI is Req with the bearer every human/operator-facing route requires: the token Dispatch's own
// server presents, UIToken here.
func (c *Client) UI(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	return c.Req(t, method, path, map[string]string{"Authorization": "Bearer " + c.UIToken}, body)
}

// Session is Req carrying a session proof: key signing for enrollmentID over the method and the
// public URL's own path, which is what the broker reconstructs and verifies against.
func (c *Client) Session(t *testing.T, key *ecdsa.PrivateKey, enrollmentID, method, path string, body any) (int, []byte) {
	t.Helper()
	p, err := proof.Sign(key, enrollmentID, method, c.PublicURL+path, time.Now())
	if err != nil {
		t.Fatalf("proof.Sign: %v", err)
	}
	return c.Req(t, method, path, map[string]string{"Proof": p}, body)
}

// Launcher is Req carrying a launcher proof: the machine key that holds launcherID signing for it,
// which is how a launcher credential authenticates an enrollment.
func (c *Client) Launcher(t *testing.T, key *ecdsa.PrivateKey, launcherID, method, path string, body any) (int, []byte) {
	t.Helper()
	p, err := proof.SignLauncher(key, launcherID, method, c.PublicURL+path, time.Now())
	if err != nil {
		t.Fatalf("proof.SignLauncher: %v", err)
	}
	return c.Req(t, method, path, map[string]string{"Proof": p}, body)
}

// NewSigningKey is a fresh P-256 key: what a machine, its launcher credential and each of its
// sessions sign with, and the only key shape the broker's proofs and request objects take.
func NewSigningKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

// deviceClient is the OAuth client a machine logs in with: this broker's device authorization and
// token endpoints under its public URL, over a transport that both pins the connection to this
// client's process and signs a DPoP proof of key for every request, as the helper's own does.
func (c *Client) deviceClient(t *testing.T, key *ecdsa.PrivateKey) (*oauth2.Config, context.Context) {
	t.Helper()
	if c.DPoP == nil {
		t.Fatal("this Client has no DPoP: set it to helper.NewDPoPTransport before driving an OAuth step")
	}
	return &oauth2.Config{
			ClientID: oauthwire.ClientID, Scopes: []string{oauthwire.Scope},
			Endpoint: oauth2.Endpoint{
				DeviceAuthURL: c.PublicURL + "/v1/oauth/device_authorization",
				TokenURL:      c.PublicURL + "/v1/oauth/token",
				AuthStyle:     oauth2.AuthStyleInParams,
			},
		},
		context.WithValue(context.Background(), oauth2.HTTPClient,
			&http.Client{Transport: c.DPoP(key, c.transport), Timeout: clientTimeout})
}

// DeviceLogin asks this broker for a device code for a machine of person on host, as the helper's
// `machine login` does: a machine-login request object signed by key and naming person as the
// login hint, plus that key's thumbprint as dpop_jkt, so only the holder of the key can redeem the
// code the operator approves.
func (c *Client) DeviceLogin(t *testing.T, key *ecdsa.PrivateKey, person, host string) *oauth2.DeviceAuthResponse {
	t.Helper()
	jkt, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatalf("thumbprint: %v", err)
	}
	request, err := record.Sign(key, c.PublicURL, []record.AuthorizationDetail{
		{Type: "launcher_credential", Identifier: host},
	}, "", person, nil, time.Now())
	if err != nil {
		t.Fatalf("sign %s's machine-login request for %s: %v", person, host, err)
	}
	config, ctx := c.deviceClient(t, key)
	da, err := config.DeviceAuth(ctx, oauth2.SetAuthURLParam("request", request), oauth2.SetAuthURLParam("dpop_jkt", jkt))
	if err != nil {
		t.Fatalf("POST /v1/oauth/device_authorization (%s at %s): %v", person, c.Addr, err)
	}
	return da
}

// ApproveMachineLogin approves the pending machine login whose confirmation code is code, as the
// operator does on Dispatch's machine-login page — look the login up by the code the person typed,
// then approve that record naming person as the approver — and answers the launcher credential the
// approval minted.
func (c *Client) ApproveMachineLogin(t *testing.T, person, code string) (credentialID string) {
	t.Helper()
	return decideMachineLogin(t, c.UI, person, code, true, " (at "+c.Addr+")")
}

// DeviceToken exchanges an approved device code for the machine's token pair, over a DPoP
// transport signing with key exactly as the helper's own exchange does.
//
// It answers the exchange's error instead of failing t, unlike every other helper here, because
// the rows it exists for are about what the loser of a race is told: two processes exchanging one
// device code at the same moment, where exactly one wins and the other's refusal is the
// observation, not a reason to end the test. A caller that expects an exchange to succeed fails t
// on the error itself, naming it.
func (c *Client) DeviceToken(t *testing.T, key *ecdsa.PrivateKey, da *oauth2.DeviceAuthResponse) (*oauth2.Token, error) {
	t.Helper()
	config, ctx := c.deviceClient(t, key)
	ctx, cancel := context.WithTimeout(ctx, deviceTokenTimeout)
	defer cancel()
	// A copy, so polling faster than the broker asked for is this exchange's business and not a
	// mutation of the device authorization its caller still holds and may exchange again.
	poll := *da
	poll.Interval = pollInterval
	return config.DeviceAccessToken(ctx, &poll)
}

// Refresh redeems token's refresh token for a fresh pair, over the same DPoP transport, as the
// helper does once its access token has expired. The token it presents is a copy expired a minute
// ago, which is how x/oauth2 is told to refresh rather than hand the access token back. It answers
// the error for the reason DeviceToken does: the loser of a refresh race is what a row reads.
func (c *Client) Refresh(t *testing.T, key *ecdsa.PrivateKey, token *oauth2.Token) (*oauth2.Token, error) {
	t.Helper()
	stale := *token
	stale.Expiry = time.Now().Add(-time.Minute)
	config, ctx := c.deviceClient(t, key)
	return config.TokenSource(ctx, &stale).Token()
}

// EnrollSession enrolls a session under the launcher credential, proving the credential with the
// machine key that holds it, and answers the enrollment and the fresh key that session signs with.
// A credential that enrolls is a credential still live, which is what a row asserting a machine
// login survived a race checks.
func (c *Client) EnrollSession(t *testing.T, machineKey *ecdsa.PrivateKey, credentialID, runtimeID string) (enrollmentID string, sessionKey *ecdsa.PrivateKey) {
	t.Helper()
	sessionKey = NewSigningKey(t)
	thumbprint, err := proof.Thumbprint(&sessionKey.PublicKey)
	if err != nil {
		t.Fatalf("thumbprint: %v", err)
	}
	status, body := c.Launcher(t, machineKey, credentialID, http.MethodPost, "/v1/enrollments",
		map[string]any{"kind": "session", "runtime_id": runtimeID, "thumbprint": thumbprint})
	if status != http.StatusCreated {
		t.Fatalf("enroll a session under credential %s (at %s) = %d, want 201: %s", credentialID, c.Addr, status, body)
	}
	var enrolled struct {
		EnrollmentID string `json:"enrollment_id"`
	}
	if err := json.Unmarshal(body, &enrolled); err != nil || enrolled.EnrollmentID == "" {
		t.Fatalf("enroll a session under credential %s (at %s) answered %s (%v), want an enrollment id",
			credentialID, c.Addr, body, err)
	}
	return enrolled.EnrollmentID, sessionKey
}

// AgentSecretAnswer is what POST /v1/requests answered one session's ask for one name: the status
// and the body itself, so a failure message names what the broker actually said, and the fields a
// row reads the policy's own verdict from.
type AgentSecretAnswer struct {
	// Status and Body are the HTTP answer exactly as it came back.
	Status int
	Body   string
	// State is "granted", "pending" (a person must decide it) or "denied"; GrantID is the grant
	// an outright release named and is nil otherwise; Approver is who the policy asked instead
	// and is nil when it asked nobody. All three are zero unless Status was 200.
	State    string
	GrantID  *string
	Approver *string
}

// AskForAgentSecret opens an agent_secret request for one name from the session, signed by its own
// key, and answers what the broker said without asserting any of it.
//
// It exists beside RequestAgentSecret for the rows whose observation IS the verdict: a retag that
// narrows a secret turns what was an outright grant into an approval request to the secret's new
// owner, and a row proving the retag reached a second process has to read which of the two came
// back. RequestAgentSecret, which fails t on anything but a grant, could only report that
// something other than a grant happened.
func (c *Client) AskForAgentSecret(t *testing.T, key *ecdsa.PrivateKey, enrollmentID, reason, name string) AgentSecretAnswer {
	t.Helper()
	request, err := record.Sign(key, c.PublicURL, []record.AuthorizationDetail{
		{Type: "agent_secret", Identifier: name, Actions: []string{"inject"}},
	}, reason, "", nil, time.Now())
	if err != nil {
		t.Fatalf("sign the request for %s: %v", name, err)
	}
	status, body := c.Session(t, key, enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": request, "session_id": nil})
	answer := AgentSecretAnswer{Status: status, Body: string(body)}
	if status != http.StatusOK {
		return answer
	}
	var created struct {
		State    string  `json:"state"`
		GrantID  *string `json:"grant_id"`
		Approver *string `json:"approver"`
	}
	if err := json.Unmarshal(body, &created); err != nil || created.State == "" {
		t.Fatalf("POST /v1/requests (%s, at %s) answered %s (%v), want the state the policy decided it in",
			name, c.Addr, body, err)
	}
	answer.State, answer.GrantID, answer.Approver = created.State, created.GrantID, created.Approver
	return answer
}

// RequestAgentSecret opens an agent_secret request for one name from the session, signed by its
// own key, and answers the grant the broker gave it. It fails t unless the request was granted
// outright, with no approval asked for: its rows ask only for an agent-tier secret the session's
// own person owns, which the policy releases at once, so a pending answer means the fixture's
// secret is not the one the row believes it is.
func (c *Client) RequestAgentSecret(t *testing.T, key *ecdsa.PrivateKey, enrollmentID, reason, name string) (grantID string) {
	t.Helper()
	answer := c.AskForAgentSecret(t, key, enrollmentID, reason, name)
	if answer.Status != http.StatusOK || answer.State != "granted" || answer.GrantID == nil || *answer.GrantID == "" {
		t.Fatalf("POST /v1/requests (%s, at %s) = %d %s, want 200 with state granted and a grant id",
			name, c.Addr, answer.Status, answer.Body)
	}
	return *answer.GrantID
}

// GrantValues reads the values the grant releases to the session that holds it.
func (c *Client) GrantValues(t *testing.T, key *ecdsa.PrivateKey, enrollmentID, grantID string) map[string]string {
	t.Helper()
	status, body := c.Session(t, key, enrollmentID, http.MethodPost, "/v1/grants/"+grantID+"/values", nil)
	if status != http.StatusOK {
		t.Fatalf("POST /v1/grants/%s/values (at %s) = %d, want 200: %s", grantID, c.Addr, status, body)
	}
	var released struct {
		Values map[string]string `json:"values"`
	}
	if err := json.Unmarshal(body, &released); err != nil {
		t.Fatalf("POST /v1/grants/%s/values (at %s) answered %s (%v), want the grant's values", grantID, c.Addr, body, err)
	}
	return released.Values
}
