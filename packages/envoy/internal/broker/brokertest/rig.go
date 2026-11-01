// packages/envoy/internal/broker/brokertest/rig.go

// Package brokertest mounts the real broker (api.Register) — real Postgres store, real
// enroll/machine-login/request services — on an httptest.Server, for any package that needs to
// drive a genuine broker end to end rather than a hand-rolled fake. It is the extraction
// api_test.go's own newTestServer names as its reference pattern. Every consumer of a full
// real-broker rig imports this package instead of duplicating the wiring.
package brokertest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/exchange"
	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/oauth"
	"github.com/sjawhar/envoy/internal/broker/oauthstore"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// podAudience is the fixed audience every Rig's pod verifier is constructed for, and the value
// MintPodToken and MintAccountToken sign tokens against — arbitrary, since a Rig both mints and
// verifies them itself; chosen only to read clearly in a failure message.
const podAudience = "legion-broker-pod"

// exchangeAudience is the audience every Rig's token exchange takes a pod's subject token for,
// the one MintSubjectToken signs: a second audience of the same issuer, as a deployment gives an
// exchanging pod a token beside the one it enrols with.
const exchangeAudience = "legion-broker-exchange"

// githubAudience is the audience every Rig's token exchange takes a workflow run's token for.
// The Rig's own issuer stands in for GitHub's, as it stands in for the cluster's: a test mints
// both, and nothing here reaches the real one.
const githubAudience = "legion-broker-github"

// APIAccount is the ServiceAccount every Rig's BROKER_API_ACCOUNTS names, as the caller
// "dispatch": the identity its UI helper presents on the routes Dispatch's server calls.
const APIAccount = "system:serviceaccount:example-ns:example-dispatch"

// APICaller is the name that account calls as, the one the broker logs when it acts.
const APICaller = "dispatch"

// ListenerAccount is the second ServiceAccount every Rig's BROKER_API_ACCOUNTS names, as the
// caller "listener": the Envoy listener, which reads one of the broker's access tokens back
// through introspection and relays no person's decision. A Rig lists it so a test can drive a
// listed caller reaching past its own routes, which is 403 ACCOUNT_LACKS_POWER.
const ListenerAccount = "system:serviceaccount:example-ns:example-listener"

// ListenerCaller is the name that second account calls as.
const ListenerCaller = "listener"

// The one account every Rig's BROKER_EXCHANGE_ACCOUNTS lists and what a pod running as it may
// write: ExchangeSession is the session name the entry fixes, so a token minted for that pod
// carries it and a request naming any other is refused.
const (
	ExchangeAccount = "system:serviceaccount:example-ns:example-worker"
	ExchangeProject = "EXAMPLE"
	ExchangePerson  = "ada@example.com"
	ExchangeSession = "example-worker"
)

// The one workflow every Rig's BROKER_EXCHANGE_WORKFLOWS lists and what a run of it may write. It
// fixes no session, so a run names its own.
const (
	ExchangeRepository = "example-org/example-repo"
	ExchangeWorkflow   = ".github/workflows/release.yaml"
	ExchangeRef        = "refs/heads/main"
)

// Rig is a live broker HTTP server (real handlers, real Postgres) plus a direct handle to its
// store — enough to drive a machine login, a session enrollment, and an agent-secret request
// end to end over genuine HTTP, and to reach into the store directly for what the API itself has
// no route to do (e.g. forcing a launcher credential's expiry). A human decision is a call to one
// of the routes Dispatch's server calls, with UIToken as the caller's credential and Operator
// named in the body as the approver.
type Rig struct {
	// URL is the broker's own httptest.Server base URL: every proof's htu and every request
	// object's aud must check against this one value.
	URL string
	// Operator is the approver, named by email as Dispatch names a signed-in person, also the
	// owner of the fixture secrets below and the operator of the sessions a test enrolls.
	Operator string
	// OperatorFile already holds Operator, trimmed — ready to hand to a helper.Broker as its
	// OperatorFile field.
	OperatorFile string
	// UIToken is the ServiceAccount token this Rig's APIAccount presents on the routes Dispatch's
	// server calls (UI). It is minted once, for an hour, so a test may send it from a goroutine
	// of its own where failing t is not allowed.
	UIToken string
	// ListenerToken is ListenerAccount's own token, minted the same way: a listed caller that may
	// introspect and nothing else, for a test driving what a caller's powers refuse.
	ListenerToken string
	Store         *store.Store
	// Secrets is the secrets.Local standing in for Secrets Manager, which a test writes to as a
	// person would, before asking the broker to reread a secret.
	Secrets *secrets.Local
	// podIssuer and podKey back every token this Rig mints — a pod's enrolment token, a service
	// account's login token, and a subject token for the exchange: a local OIDC issuer this Rig's
	// own verifier (wired into its enroll.Service and its api.Deps by NewRig) trusts, and the
	// signing key they are minted under.
	podIssuer *oidctest.Issuer
	podKey    *oidctest.Key
	// serviceLogins is the broker's live BROKER_SERVICE_LOGINS: DaemonAccount, as legion-daemon.
	serviceLogins map[string]string
	// vmIssuer and vmKey stand in for Google's issuer and the key it signs a VM's identity token
	// with: nil unless WithVMFleet wired the broker to trust them.
	vmIssuer *oidctest.Issuer
	vmKey    *oidctest.Key
}

// Option changes how NewRig wires its broker.
type Option func(*rigConfig)

type rigConfig struct {
	lease    time.Duration
	maxConns int
	minters  map[string]requests.Minter
	vmFleet  bool
}

// The one VM fleet WithVMFleet binds: VMAccount is its Google service account, bound to service
// VMService in project VMProject for instances named VMInstances, and VMAudience the audience its
// identity tokens carry. VMSecret is an agent-tier secret VMService owns, holding VMSecretValue.
const (
	VMAccount     = "example-fleet@example-project.iam.gserviceaccount.com"
	VMService     = "example-fleet"
	VMProject     = "example-project"
	VMInstances   = "example-vm-*"
	VMAudience    = "example-vm-audience"
	VMSecret      = "TEST_VM_SECRET"
	VMSecretValue = "test-vm-secret-v1"
)

// WithVMFleet makes the broker trust a local issuer standing in for Google's, for VMAudience, as
// cmd/broker/main.go does when BROKER_GOOGLE_OIDC_AUDIENCE is set, with VMAccount bound to
// VMService, VMProject and VMInstances (BROKER_VM_SERVICES, BROKER_VM_PROJECTS,
// BROKER_VM_INSTANCES), and adds VMSecret, which VMService owns. MintVMToken mints the fleet's
// identity tokens.
func WithVMFleet() Option {
	return func(c *rigConfig) { c.vmFleet = true }
}

// WithLease sets the enrollment lease the broker grants, an hour by default, for a test whose
// launcher must renew a session within it.
func WithLease(lease time.Duration) Option {
	return func(c *rigConfig) { c.lease = lease }
}

// WithPoolMaxConns caps the broker's database pool at n connections (pgx's pool_max_conns), for a
// test that starves it; pgx's default otherwise.
func WithPoolMaxConns(n int) Option {
	return func(c *rigConfig) { c.maxConns = n }
}

// WithMinters gives the broker a minter for each kind minters names, as cmd/broker/main.go's
// kindMinters does, so a test releases a kind secret through a minter aimed at its own stub
// upstream; none by default.
func WithMinters(minters map[string]requests.Minter) Option {
	return func(c *rigConfig) { c.minters = minters }
}

// NewRig holds two secrets the person ada@example.com owns: TEST_SECRET, human-tier, which a
// request from any session makes an approval request to her, and TEST_AUTO_SECRET, agent-tier,
// which her own sessions get at once. It wires a real pod verifier (a local OIDC issuer trusted by
// an enroll.K8sPodVerifier, mirroring cmd/broker/main.go's own wiring), which is also what
// authenticates the listed caller of the routes Dispatch's server calls, and mounts api.Register
// on an httptest.Server — wired exactly as cmd/broker/main.go and api_test.go's newTestServer
// wire it. It skips t when BROKER_TEST_DATABASE_URL is unset (storetest.Open's own contract).
func NewRig(t *testing.T, options ...Option) *Rig {
	t.Helper()
	config := rigConfig{lease: time.Hour}
	for _, option := range options {
		option(&config)
	}
	var params []string
	if config.maxConns > 0 {
		params = append(params, "pool_max_conns="+strconv.Itoa(config.maxConns))
	}
	st := storetest.Open(t, params...)
	operator := "ada@example.com"
	fixtures := []secrets.LocalSecret{
		policytest.Secret("TEST_SECRET", operator, policy.TierHuman, "test-secret-v1"),
		policytest.Secret("TEST_AUTO_SECRET", operator, policy.TierAgent, "test-auto-secret-v1"),
	}
	var services []string
	if config.vmFleet {
		fixtures = append(fixtures, policytest.Secret(VMSecret, VMService, policy.TierAgent, VMSecretValue))
		services = append(services, VMService)
	}
	local := secrets.NewLocal(fixtures...)
	cur := policytest.Current(t, local, services...)

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	issuer := oidctest.New(t)
	podKey := issuer.PublishKey(t, "signing-key")
	podVerifier, err := oidc.New(context.Background(), issuer.URL(), podAudience)
	if err != nil {
		t.Fatalf("oidc.New: %v", err)
	}

	serviceLogins := map[string]string{DaemonAccount: "legion-daemon"}
	enr := &enroll.Service{Store: st, Lease: config.lease, Pod: enroll.K8sPodVerifier{Verifier: podVerifier}, ServiceLogins: serviceLogins}
	enr.Chain = enroll.NewChainVerifier(st, srv.URL, time.Minute)
	var vmIssuer *oidctest.Issuer
	var vmKey *oidctest.Key
	if config.vmFleet {
		vmIssuer = oidctest.New(t)
		vmKey = vmIssuer.PublishKey(t, "vm-signing-key")
		vmVerifier, err := oidc.New(context.Background(), vmIssuer.URL(), VMAudience)
		if err != nil {
			t.Fatalf("oidc.New(the VM issuer): %v", err)
		}
		enr.VM = enroll.GoogleVMVerifier{Verifier: vmVerifier}
		enr.VMServicesByAccount = map[string]string{VMAccount: VMService}
		enr.VMProjects = map[string]string{VMAccount: VMProject}
		enr.VMInstancePatterns = map[string]string{VMAccount: VMInstances}
	}

	reqMachine := &requests.Machine{
		Store: st, Policy: cur, Secrets: policytest.Store(local), PendingTTL: 12 * time.Hour,
		Audience: srv.URL, Skew: time.Minute, Replay: enr.Replay, Minters: config.minters,
		VMServicesByAccount: enr.VMServicesByAccount, VMProjects: enr.VMProjects, VMInstancePatterns: enr.VMInstancePatterns,
	}
	reqMachine.Chain = requests.NewChainVerifier(st, srv.URL, time.Minute)
	provider := NewOAuthProvider(t, st, issuer)
	mach := &machine.Service{
		Store: st, Enroll: enr, Policy: cur,
		Audience: srv.URL, Skew: time.Minute, PendingTTL: 15 * time.Minute, CredentialLifetime: 7 * 24 * time.Hour,
		OAuth: provider, Replay: enr.Replay,
	}

	api.Register(mux, api.Deps{
		PublicURL: srv.URL,
		APIAccounts: map[string]api.Account{
			APIAccount:      Caller(t, APICaller),
			ListenerAccount: Caller(t, ListenerCaller),
		},
		// The one verifier serves both, as the broker's own wiring does: a pod enrolling and a
		// listed caller present tokens of the same issuer for the same audience.
		AccountTokens: enroll.K8sPodVerifier{Verifier: podVerifier},
		Enroll:        enr, Machine: reqMachine, MachineLogin: mach,
		OAuth: provider, DeviceLogin: mach.LoginDevice, TokenHooks: mach.TokenHooks(),
		Proof:  &proof.Verifier{Skew: time.Minute, Lookup: enr.Lookup, LookupLauncher: enr.AuthenticateLauncher, Replay: enr.Replay},
		Policy: cur, SecretsStore: secrets.StoreAWS, SecretsPrefix: policytest.Prefix, SecretsKMSKey: policytest.KeyARN,
	})

	operatorFile := filepath.Join(t.TempDir(), "operator")
	if err := os.WriteFile(operatorFile, []byte(operator), 0o600); err != nil {
		t.Fatal(err)
	}

	rig := &Rig{
		URL: srv.URL, Operator: operator, OperatorFile: operatorFile,
		Store:     st,
		Secrets:   local,
		podIssuer: issuer, podKey: podKey,
		serviceLogins: serviceLogins,
		vmIssuer:      vmIssuer, vmKey: vmKey,
	}
	rig.UIToken = rig.MintAccountToken(t, APIAccount, time.Hour)
	rig.ListenerToken = rig.MintAccountToken(t, ListenerAccount, time.Hour)
	return rig
}

// Caller is the account a caller name carries, for an api.Deps a test wires itself as well as for
// the Rig's own: the broker's boot reads BROKER_API_ACCOUNTS this way, and a name its table does
// not know fails t here exactly as it would refuse that boot.
func Caller(t *testing.T, name string) api.Account {
	t.Helper()
	account, known := api.AccountFor(name)
	if !known {
		t.Fatalf("api.AccountFor(%q): the broker knows no caller of that name", name)
	}
	return account
}

// MachineLoginURL is the rig broker's machine-login page, the device grant's verification_uri.
const MachineLoginURL = "https://dispatch.example/credentials/machine"

// NewOAuthProvider builds the machine login's OAuth provider on st's database as
// cmd/broker/main.go does: device codes live as long as a pending machine login, access tokens
// fifteen minutes, refresh tokens as long as a launcher credential, and verification_uri is
// MachineLoginURL. Its token exchange reads issuer for both kinds of subject token — a pod's, for
// exchangeAudience, and a workflow run's, for githubAudience — and lists the one account and the
// one workflow the Rig's constants name, so an exchange test has an identity to be and an unlisted
// one to be refused as.
func NewOAuthProvider(t *testing.T, st *store.Store, issuer *oidctest.Issuer) *oauth.Provider {
	t.Helper()
	oauthStore := oauthstore.New(st.Pool)
	oauthStore.Reused = enroll.RevokeReusedCredential
	secret, err := oauthStore.Secret(context.Background())
	if err != nil {
		t.Fatalf("OAuth secret: %v", err)
	}
	policy, err := exchange.NewSet(
		[]exchange.AccountEntry{{Account: ExchangeAccount, Project: ExchangeProject, Person: ExchangePerson, Session: ExchangeSession}},
		[]exchange.WorkflowEntry{{Repository: ExchangeRepository, Workflow: ExchangeWorkflow, Ref: ExchangeRef, Project: ExchangeProject, Person: ExchangePerson}},
	)
	if err != nil {
		t.Fatalf("exchange.NewSet: %v", err)
	}
	keys, err := oidc.NewKeys(context.Background(), issuer.URL())
	if err != nil {
		t.Fatalf("oidc.NewKeys: %v", err)
	}
	provider, err := oauth.New(oauth.Config{
		Secret: secret, DeviceLifetime: 15 * time.Minute, AccessLifetime: 15 * time.Minute,
		RefreshLifetime: 7 * 24 * time.Hour, VerificationURL: MachineLoginURL, ProofSkew: time.Minute,
		Exchange: oauth.ExchangeConfig{
			Policy:     policy,
			Kubernetes: oauth.IssuerConfig{Issuer: issuer.URL(), Audience: exchangeAudience, Keys: keys},
			GitHub:     oauth.IssuerConfig{Issuer: issuer.URL(), Audience: githubAudience, Keys: keys},
		},
	}, oauthStore)
	if err != nil {
		t.Fatalf("oauth.New: %v", err)
	}
	return provider
}

// request issues method against baseURL+path through client, with an optional body (marshaled to
// JSON) and headers, and answers the status and the body bytes. It asserts no status itself: a
// caller does that, so a mismatch names the actual body.
//
// It is the one request body in this package: Rig.Req and Client.Req differ in the broker they
// name, the client they go out on, and at — the process a failure is about, " (at host:port)"
// for a Client pinned to one of two processes serving one database, and empty for a Rig, which
// has one broker and so nothing to tell apart.
func request(
	t *testing.T,
	client *http.Client,
	baseURL, method, path string,
	headers map[string]string,
	body any,
	at string,
) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, baseURL+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s%s: %v", method, path, at, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body of %s %s%s: %v", method, path, at, err)
	}
	return resp.StatusCode, respBody
}

// Req issues method against path on the real broker with an optional body (marshaled to JSON)
// and headers, returning the decoded status and body bytes. It never asserts a status itself: a
// caller does that so a mismatch names the actual body.
func (r *Rig) Req(t *testing.T, method, path string, headers map[string]string, body any) (int, []byte) {
	t.Helper()
	return request(t, http.DefaultClient, r.URL, method, path, headers, body, "")
}

// UI is Req as the listed caller of the routes Dispatch's server calls: it presents UIToken, the
// ServiceAccount token this Rig minted for APIAccount, which is the credential Dispatch's own
// server presents in production.
func (r *Rig) UI(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	return r.Req(t, method, path, map[string]string{"Authorization": "Bearer " + r.UIToken}, body)
}

// uiCall is the UI-route caller a machine-login decision goes out through: Rig.UI or Client.UI,
// which differ in the broker they reach and the bearer they carry.
type uiCall func(t *testing.T, method, path string, body any) (int, []byte)

// decideMachineLogin decides the pending machine login whose confirmation code is code, through
// ui, as the operator does on Dispatch's machine-login page: it looks the login up by that code,
// then approves or denies it naming approver as the approver. It returns the launcher credential
// an approval minted, "" for a denial, and fails t on any other answer. at names the process a
// failure is about, as request's does.
func decideMachineLogin(t *testing.T, ui uiCall, approver, code string, approve bool, at string) (credentialID string) {
	t.Helper()
	status, body := ui(t, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": code})
	if status != http.StatusOK {
		t.Fatalf("POST /v1/machine-logins/lookup%s = %d: %s", at, status, body)
	}
	var looked struct {
		RecordID string `json:"record_id"`
		State    string `json:"state"`
	}
	if err := json.Unmarshal(body, &looked); err != nil || looked.State != "pending" || looked.RecordID == "" {
		t.Fatalf("POST /v1/machine-logins/lookup%s answered %s (%v), want a pending record", at, body, err)
	}
	action, want := "deny", "denied"
	if approve {
		action, want = "approve", "approved"
	}
	status, body = ui(t, http.MethodPost, "/v1/credential-requests/"+looked.RecordID+"/"+action,
		map[string]any{"approver": approver, "code": code})
	if status != http.StatusOK {
		t.Fatalf("%s %s's machine login%s = %d: %s", action, approver, at, status, body)
	}
	var decided struct {
		State        string  `json:"state"`
		CredentialID *string `json:"credential_id"`
	}
	if err := json.Unmarshal(body, &decided); err != nil || decided.State != want ||
		approve != (decided.CredentialID != nil && *decided.CredentialID != "") {
		t.Fatalf("%s %s's machine login%s answered %s (%v), want state %s and a credential id only for an approval",
			action, approver, at, body, err, want)
	}
	if decided.CredentialID == nil {
		return ""
	}
	return *decided.CredentialID
}

// DecideMachineLogin decides the pending machine login whose confirmation code is code, naming
// this Rig's Operator as the approver.
func (r *Rig) DecideMachineLogin(t *testing.T, code string, approve bool) (credentialID string) {
	t.Helper()
	return decideMachineLogin(t, r.UI, r.Operator, code, approve, "")
}

// MintPodToken mints a projected service-account token bound to podUID — the shape
// enroll.K8sPodVerifier.Verify reads — signed by this Rig's own local OIDC issuer, which the
// Rig's enroll.Service trusts as its PodVerifier (wired in NewRig). Mirrors
// internal/broker/enroll/enroll_test.go's own mintPodToken helper.
func (r *Rig) MintPodToken(t *testing.T, podUID string) string {
	t.Helper()
	claims := r.podIssuer.Claims("system:serviceaccount:legion:worker", podAudience)
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	var merged map[string]any
	if err := json.Unmarshal(raw, &merged); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	merged["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": podUID}}
	return r.podIssuer.Mint(t, r.podKey, merged)
}

// DaemonAccount is the ServiceAccount every Rig's BROKER_SERVICE_LOGINS lets log in as
// legion-daemon with its token (POST /v1/launcher-credentials/token), as a Legion daemon's own
// account does.
const DaemonAccount = "system:serviceaccount:legion-system:legion-daemon-test"

// VMIdentity is the VM a token MintVMToken mints names. Each field left zero takes the fleet's
// default: VMAccount, VMProject, zone example-zone-a, instance example-vm-1 (which VMInstances
// matches), instance id 1000000000000000001, VMAudience, and a lifetime of an hour, as Google's are.
type VMIdentity struct {
	Account, Project, Zone, Instance, InstanceID, Audience string
	Lifetime                                               time.Duration
}

// MintVMToken mints a Compute Engine VM's identity token in Google's full format (email,
// email_verified, and the google.compute_engine claims) for vm, signed by the issuer WithVMFleet
// stands up in Google's place: what a VM's metadata server answers for
// /instance/service-accounts/default/identity?audience=...&format=full. It expires vm.Lifetime from
// now and was issued an hour before that, as Google's tokens live an hour; a negative lifetime mints
// one already expired. It fails t on a Rig built without WithVMFleet.
func (r *Rig) MintVMToken(t *testing.T, vm VMIdentity) string {
	t.Helper()
	if r.vmIssuer == nil {
		t.Fatal("MintVMToken needs a Rig built WithVMFleet")
	}
	or := func(value, fallback string) string {
		if value == "" {
			return fallback
		}
		return value
	}
	account := or(vm.Account, VMAccount)
	lifetime := vm.Lifetime
	if lifetime == 0 {
		lifetime = time.Hour
	}
	claims := r.vmIssuer.Claims("100000000000000000001", or(vm.Audience, VMAudience))
	expires := time.Now().Add(lifetime)
	claims["iat"] = expires.Add(-time.Hour).Unix()
	claims["nbf"] = claims["iat"]
	claims["exp"] = expires.Unix()
	claims["email"] = account
	claims["email_verified"] = true
	claims["google"] = map[string]any{"compute_engine": map[string]any{
		"project_id":    or(vm.Project, VMProject),
		"zone":          or(vm.Zone, "example-zone-a"),
		"instance_name": or(vm.Instance, "example-vm-1"),
		"instance_id":   or(vm.InstanceID, "1000000000000000001"),
	}}
	return r.vmIssuer.Mint(t, r.vmKey, claims)
}

// MintAccountToken mints a projected ServiceAccount token for account, expiring lifetime from now
// (a negative lifetime mints one already expired), signed by this Rig's own local OIDC issuer for
// its audience: what a daemon's token file holds. Bound to no pod, as `kubectl create token` mints.
func (r *Rig) MintAccountToken(t *testing.T, account string, lifetime time.Duration) string {
	t.Helper()
	claims := r.podIssuer.Claims(account, podAudience)
	now := time.Now()
	claims["iat"] = now.Add(min(lifetime, 0) - 5*time.Second).Unix()
	claims["exp"] = now.Add(lifetime).Unix()
	return r.podIssuer.Mint(t, r.podKey, claims)
}

// MintSubjectToken mints a pod's projected ServiceAccount token for account, bound to podUID and
// carrying this Rig's exchange audience: the subject token POST /v1/oauth/token takes for the
// Kubernetes token type. A podUID of "" mints one bound to no pod, which the exchange refuses.
func (r *Rig) MintSubjectToken(t *testing.T, account, podUID string) string {
	t.Helper()
	claims := r.podIssuer.Claims(account, exchangeAudience)
	if podUID != "" {
		claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": podUID}}
	}
	return r.podIssuer.Mint(t, r.podKey, claims)
}

// MintWorkflowToken mints a continuous-integration workflow run's token as GitHub mints one —
// the repository, the workflow file's ref, the git ref and the run id the exchange decides by —
// signed by this Rig's own issuer for its GitHub audience. The claims name the listed workflow
// unless a caller gives another repository, workflow or ref.
func (r *Rig) MintWorkflowToken(t *testing.T, repository, workflow, ref, runID string) string {
	t.Helper()
	claims := r.podIssuer.Claims("repo:"+repository+":ref:"+ref, githubAudience)
	workflowRef := repository + "/" + workflow + "@" + ref
	claims["repository"] = repository
	claims["workflow_ref"] = workflowRef
	claims["job_workflow_ref"] = workflowRef
	claims["ref"] = ref
	claims["run_id"] = runID
	return r.podIssuer.Mint(t, r.podKey, claims)
}

// DropServiceLogin takes account off this Rig's BROKER_SERVICE_LOGINS, as an operator removing its
// line and redeploying the broker does.
func (r *Rig) DropServiceLogin(account string) {
	delete(r.serviceLogins, account)
}
