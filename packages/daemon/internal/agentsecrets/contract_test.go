// contract_test.go drives Client against the real broker (packages/envoy's own httptest server,
// mounted through its brokertest.NewRig) instead of client_test.go's fakeBroker: this is the
// byte-compatibility proof for jws.go, judged by the broker's own real record.VerifyRequestObject
// and proof.Verifier, not by a reimplementation of the wire contract by inspection. It skips
// without BROKER_TEST_DATABASE_URL (brokertest.NewRig's own skip), except on GitHub Actions, where
// the Tests workflow's test job sets it and a skip would hide that it stopped (newBrokerRig).
//
// This is the only file in this module that imports anything under github.com/sjawhar/envoy/...:
// that import is test-only, never leaking into the daemon's production build.
//
// The rig itself lives at packages/envoy/internal/broker/brokertest, duplicated verbatim from
// Plan B's Task 4 (a sibling branch stacked on the same Plan A rig, not yet merged) — a
// deliberate, coordinated duplication rather than an accident, since Plan B and Plan C are
// independent siblings and neither should block on the other landing first. Whichever of the two
// PRs merges into main second must drop its own copy of that internal package in favor of the
// one main already carries.
//
// This file imports packages/envoy/brokertest instead of that internal package directly: Go's
// internal-package rule refuses to let github.com/sjawhar/legion/daemon (this module) import
// anything under github.com/sjawhar/envoy/internal/..., workspace-linked or not, so
// packages/envoy/brokertest is a thin public facade added specifically to cross that boundary
// (see its own doc comment). That facade is new, not part of Plan B's duplicated rig, and stays
// regardless of which plan's copy of the internal rig ultimately wins.
package agentsecrets

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/brokertest"
)

// newBrokerRig is brokertest.NewRig, failing rather than skipping on GitHub Actions when
// BROKER_TEST_DATABASE_URL is unset: the Tests workflow's test job names its Postgres service
// there, and a silent skip would let the contract tests stop running unnoticed.
func newBrokerRig(t *testing.T, options ...brokertest.Option) *brokertest.Rig {
	t.Helper()
	if os.Getenv("BROKER_TEST_DATABASE_URL") == "" && os.Getenv("GITHUB_ACTIONS") == "true" {
		t.Fatal("BROKER_TEST_DATABASE_URL is unset on GitHub Actions: the Tests workflow's test job names its Postgres service")
	}
	return brokertest.NewRig(t, options...)
}

// wireMachineLoginRecord is the small slice of POST /v1/machine-logins/lookup's response this
// test actually reads.
type wireMachineLoginRecord struct {
	RecordID string `json:"record_id"`
}

// approveMachineLogin drives the human-approval half of a machine login through the real UI
// routes exactly as packages/envoy/internal/broker/api/api_test.go's own
// TestMachineLoginApprovalMintsAKeyBoundLauncherCredentialForEnrollment does: look up the pending
// record by its confirmation code, then approve it with the same code and the operator's login,
// the body Dispatch's server sends.
func approveMachineLogin(t *testing.T, rig *brokertest.Rig, code string) {
	t.Helper()
	status, body := rig.UI(t, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": code})
	if status != http.StatusOK {
		t.Fatalf("POST /v1/machine-logins/lookup = %d: %s", status, body)
	}
	var looked wireMachineLoginRecord
	if err := json.Unmarshal(body, &looked); err != nil || looked.RecordID == "" {
		t.Fatalf("decode machine-login lookup: %v (body: %s)", err, body)
	}
	status, body = rig.UI(t, http.MethodPost, "/v1/credential-requests/"+looked.RecordID+"/approve",
		map[string]any{"approver": rig.Operator, "code": code})
	if status != http.StatusOK {
		t.Fatalf("approve machine login = %d: %s", status, body)
	}
}

// expireLauncherCredential SQL-expires the one launcher credential this test's single Login
// minted — a fresh Rig never carries another launcher_credentials row, so this is the row's own
// identity check rather than a lookup by key_thumbprint.
func expireLauncherCredential(t *testing.T, rig *brokertest.Rig) {
	t.Helper()
	tag, err := rig.Store.Pool.Exec(context.Background(),
		`update launcher_credentials set expires_at = now() - interval '1 hour'`)
	if err != nil {
		t.Fatalf("expire launcher credential: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("expire launcher credential: affected %d rows, want exactly 1 (a fresh rig mints exactly one)", tag.RowsAffected())
	}
}

// TestContractMachineLoginEnrollRevokeExpireReenroll is Task 3's whole flow, driven against a
// real broker: Login mints a key and asks for a launcher credential; approving it through the
// real UI routes (the operator's login and the typed code, real record.VerifyRequestObject
// verification) moves LoginStatus to issued; Enroll registers a pod authenticated by a real k8s pod
// token against a real local OIDC issuer (brokertest.Rig's own PodVerifier, wired in NewRig);
// Revoke ends it and is idempotent; SQL-expiring the launcher credential and enrolling again proves
// doProof's automatic re-login: the broker answers 401 LAUNCHER_INVALID, the client starts a fresh
// Login, and Enroll surfaces NO_MACHINE_CREDENTIAL naming that fresh code. That second login is
// also approved and driven to issued before the test returns, so no poll goroutine outlives it.
func TestContractMachineLoginEnrollRevokeExpireReenroll(t *testing.T) {
	withFastPolling(t)
	rig := newBrokerRig(t)
	ctx := context.Background()
	client := &Client{URL: rig.URL}

	code, err := client.Login(ctx)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	approveMachineLogin(t, rig, code)
	eventually(t, func() bool { return client.LoginStatus().State == "issued" },
		"LoginStatus reaches issued after approval")

	podToken := rig.MintPodToken(t, "pod-uid-1")
	enr, err := client.Enroll(ctx, PodEnrollment{PodUID: "pod-uid-1", Thumbprint: "tp-pod-1", PodToken: podToken})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if enr.ID == "" {
		t.Fatalf("Enroll returned an empty enrollment id")
	}

	if err := client.Revoke(ctx, enr.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if err := client.Revoke(ctx, enr.ID); err != nil {
		t.Fatalf("Revoke (idempotent retry of an already-revoked enrollment): %v", err)
	}

	expireLauncherCredential(t, rig)

	podToken2 := rig.MintPodToken(t, "pod-uid-2")
	_, err = client.Enroll(ctx, PodEnrollment{PodUID: "pod-uid-2", Thumbprint: "tp-pod-2", PodToken: podToken2})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "NO_MACHINE_CREDENTIAL" {
		t.Fatalf("Enroll (after the launcher credential expired) = %v, want *APIError{Code: NO_MACHINE_CREDENTIAL}", err)
	}
	newState := client.LoginStatus()
	if newState.State != "pending" {
		t.Fatalf("LoginStatus (after the automatic re-login) = %+v, want State=pending", newState)
	}
	if newState.Code == code {
		t.Fatalf("LoginStatus (after the automatic re-login) reused the original code %q, want a fresh one", code)
	}

	// Resolve the second login before returning: an unresolved pending login's poll goroutine
	// would otherwise keep running past this test (context.Background(), no terminal state),
	// racing withFastPolling's own t.Cleanup restore of the shared backoff variables — the exact
	// hazard client_test.go's TestExpiredCredentialTriggersAFreshLoginWithANewKey documents and
	// guards against the same way.
	approveMachineLogin(t, rig, newState.Code)
	eventually(t, func() bool { return client.LoginStatus().State == "issued" },
		"LoginStatus reaches issued after the second approval")
}

// TestContractADeniedBootLoginIsRetriedByTheNextEnrollment drives the retry against the real
// broker: the boot login is denied through the real UI deny route, the next Enroll starts a fresh
// login with a new code (answering NO_MACHINE_CREDENTIAL that names it), and once that code is
// approved the pod enrolls, with no restart.
func TestContractADeniedBootLoginIsRetriedByTheNextEnrollment(t *testing.T) {
	withFastPolling(t)
	withLoginRetry(t, 0, 0)
	withUnansweredRetry(t, 0, 0)
	rig := newBrokerRig(t)
	ctx := context.Background()
	client := &Client{URL: rig.URL}

	code, err := client.Login(ctx)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	status, body := rig.UI(t, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": code})
	var looked wireMachineLoginRecord
	if err := json.Unmarshal(body, &looked); status != http.StatusOK || err != nil || looked.RecordID == "" {
		t.Fatalf("POST /v1/machine-logins/lookup = %d %s (%v)", status, body, err)
	}
	if status, body := rig.UI(t, http.MethodPost, "/v1/credential-requests/"+looked.RecordID+"/deny",
		map[string]any{"approver": rig.Operator, "code": code}); status != http.StatusOK {
		t.Fatalf("deny the boot login = %d: %s", status, body)
	}
	eventually(t, func() bool { return client.LoginStatus().State == "denied" }, "the boot login to read denied")

	podToken := rig.MintPodToken(t, "pod-uid-1")
	pod := PodEnrollment{PodUID: "pod-uid-1", Thumbprint: "tp-pod-1", PodToken: podToken}
	_, err = client.Enroll(ctx, pod)
	var apiErr *APIError
	retried := client.LoginStatus()
	if !errors.As(err, &apiErr) || apiErr.Code != "NO_MACHINE_CREDENTIAL" || retried.State != "pending" || retried.Code == code {
		t.Fatalf("Enroll after the denial = %v, login %+v; want NO_MACHINE_CREDENTIAL and a fresh pending code, not %q", err, retried, code)
	}
	approveMachineLogin(t, rig, retried.Code)
	eventually(t, func() bool { return client.LoginStatus().State == "issued" }, "the retried login to read issued")
	enr, err := client.Enroll(ctx, pod)
	if err != nil || enr.ID == "" {
		t.Fatalf("Enroll after the retried login was approved = %+v, %v; want an enrollment", enr, err)
	}
}

// memoryKeeper is a Keeper that outlives the Clients sharing it, as the daemon's database outlives
// its process: a second Client over the same memoryKeeper is the same daemon after a restart.
type memoryKeeper struct {
	mu   sync.Mutex
	kept *Kept
}

func (k *memoryKeeper) Load(context.Context) (Kept, bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.kept == nil {
		return Kept{}, false, nil
	}
	return *k.kept, true, nil
}

func (k *memoryKeeper) Save(_ context.Context, kept Kept) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.kept = &kept
	return nil
}

func (k *memoryKeeper) Forget(_ context.Context, credentialID string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.kept != nil && k.kept.CredentialID == credentialID {
		k.kept = nil
	}
	return nil
}

// brokerLogins counts the machine logins the broker has opened, approved or not.
func brokerLogins(t *testing.T, rig *brokertest.Rig) int {
	t.Helper()
	var n int
	if err := rig.Store.Pool.QueryRow(context.Background(),
		`select count(*) from credential_requests where kind='launcher_credential'`).Scan(&n); err != nil {
		t.Fatalf("count machine logins: %v", err)
	}
	return n
}

// brokerKey reads the thumbprint of the key the broker holds for the rig's one launcher credential.
func brokerKey(t *testing.T, rig *brokertest.Rig) string {
	t.Helper()
	var tp string
	if err := rig.Store.Pool.QueryRow(context.Background(), `select key_thumbprint from launcher_credentials`).Scan(&tp); err != nil {
		t.Fatalf("read the launcher credential's key: %v", err)
	}
	return tp
}

// keptKey answers the thumbprints of the key keeper holds and of its next key ("" for none).
func keptKey(t *testing.T, keeper *memoryKeeper) (current, next string) {
	t.Helper()
	kept, ok, _ := keeper.Load(context.Background())
	if !ok {
		t.Fatal("the keeper holds no credential")
	}
	thumb := func(der []byte) string {
		key, err := parseKey(der)
		if err != nil {
			t.Fatalf("parse a kept key: %v", err)
		}
		tp, err := thumbprint(&key.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		return tp
	}
	current = thumb(kept.PrivateKey)
	if kept.NextPrivateKey != nil {
		next = thumb(kept.NextPrivateKey)
	}
	return current, next
}

// approvedClient starts a Client over keeper whose boot login a person approves, as a daemon's first
// boot does, and answers it with the cancel of the context that bounds its renewal loop.
func approvedClient(t *testing.T, rig *brokertest.Rig, keeper *memoryKeeper) (*Client, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c := &Client{URL: rig.URL, Keeper: keeper}
	code, err := c.Start(ctx)
	if err != nil || code == "" {
		t.Fatalf("first boot: Start = %q, %v; want a machine login's code", code, err)
	}
	approveMachineLogin(t, rig, code)
	eventually(t, func() bool { return c.LoginStatus().State == "issued" }, "the boot login to be issued")
	eventually(t, func() bool { _, ok, _ := keeper.Load(context.Background()); return ok }, "the issued credential to be kept")
	return c, cancel
}

// restart starts a new Client over keeper, as the daemon's next process over the same database.
func restart(t *testing.T, rig *brokertest.Rig, keeper *memoryKeeper) (*Client, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c := &Client{URL: rig.URL, Keeper: keeper}
	code, err := c.Start(ctx)
	if err != nil {
		t.Fatalf("after the restart: Start: %v", err)
	}
	return c, code
}

// TestContractARestartedClientResumesItsCredential is the restart itself, against the real broker:
// a daemon's boot login is approved and enrolls a pod; the daemon restarts (a new Client over the
// same Keeper, as a new process over the same database) and enrolls its next pod with the
// credential it kept, opening no machine login, so the broker still holds the one a person
// approved. Its boot renewal moves the credential to a new key, which it keeps.
func TestContractARestartedClientResumesItsCredential(t *testing.T) {
	withFastPolling(t)
	rig := newBrokerRig(t)
	keeper := &memoryKeeper{}
	first, stop := approvedClient(t, rig, keeper)
	if _, err := first.Enroll(context.Background(), PodEnrollment{PodUID: "pod-uid-1", Thumbprint: "tp-pod-1", PodToken: rig.MintPodToken(t, "pod-uid-1")}); err != nil {
		t.Fatalf("Enroll before the restart: %v", err)
	}
	approvedKey := brokerKey(t, rig)
	stop()

	second, code := restart(t, rig, keeper)
	if code != "" {
		t.Fatalf("after the restart: Start = %q; want the kept credential resumed, no machine login", code)
	}
	if got := second.LoginStatus().State; got != "issued" {
		t.Fatalf("after the restart: LoginStatus = %q, want issued", got)
	}
	if _, err := second.Enroll(context.Background(), PodEnrollment{PodUID: "pod-uid-2", Thumbprint: "tp-pod-2", PodToken: rig.MintPodToken(t, "pod-uid-2")}); err != nil {
		t.Fatalf("Enroll after the restart: %v", err)
	}
	if n := brokerLogins(t, rig); n != 1 {
		t.Fatalf("the broker holds %d machine logins, want the 1 a person approved before the restart", n)
	}
	eventually(t, func() bool { return brokerKey(t, rig) != approvedKey }, "the boot renewal to move the credential to a new key")
	eventually(t, func() bool { current, next := keptKey(t, keeper); return current == brokerKey(t, rig) && next == "" },
		"the keeper to hold the key the broker now holds, and no next key")
}

// TestContractTheCredentialOutlivesSeveralLifetimes: against a broker whose credentials last three
// seconds, a daemon that keeps renewing still enrolls pods ten seconds after its one approved login,
// on a key that has changed with every renewal, and the broker holds that one login alone.
func TestContractTheCredentialOutlivesSeveralLifetimes(t *testing.T) {
	withFastPolling(t)
	rig := newBrokerRig(t, brokertest.WithCredentialLifetime(3*time.Second))
	keeper := &memoryKeeper{}
	c, _ := approvedClient(t, rig, keeper)
	approvedKey := brokerKey(t, rig)
	time.Sleep(10 * time.Second)
	if _, err := c.Enroll(context.Background(), PodEnrollment{PodUID: "pod-uid-1", Thumbprint: "tp-pod-1", PodToken: rig.MintPodToken(t, "pod-uid-1")}); err != nil {
		t.Fatalf("Enroll three lifetimes after the approval: %v", err)
	}
	if n := brokerLogins(t, rig); n != 1 {
		t.Fatalf("the broker holds %d machine logins, want the 1 a person approved", n)
	}
	var renewals int
	if err := rig.Store.Pool.QueryRow(context.Background(), `select count(*) from audit where kind='launcher_credential.renewed'`).Scan(&renewals); err != nil {
		t.Fatal(err)
	}
	if renewals < 4 || brokerKey(t, rig) == approvedKey {
		t.Fatalf("%d renewals, key changed %v; want at least four renewals in ten seconds of three-second lifetimes, each onto a new key", renewals, brokerKey(t, rig) != approvedKey)
	}
}

// TestContractARestartTakesUpARenewalWhoseAnswerWasLost: the broker moved the credential to the key
// a renewal asked for, but the daemon stopped before it read the answer, so its Keeper holds the old
// key and, beside it, the new one. The restarted daemon asks the broker with the new key, takes it
// up, renews from it, and enrolls a pod, opening no machine login.
func TestContractARestartTakesUpARenewalWhoseAnswerWasLost(t *testing.T) {
	withFastPolling(t)
	rig := newBrokerRig(t)
	keeper := &memoryKeeper{}
	_, stop := approvedClient(t, rig, keeper)
	stop()
	next := mustTestKey(t)
	nextDER, err := x509.MarshalPKCS8PrivateKey(next)
	if err != nil {
		t.Fatal(err)
	}
	nextTP, err := thumbprint(&next.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rig.Store.Pool.Exec(context.Background(), `update launcher_credentials set key_thumbprint=$1`, nextTP); err != nil {
		t.Fatalf("move the broker's credential to the next key: %v", err)
	}
	keeper.mu.Lock()
	keeper.kept.NextPrivateKey = nextDER
	keeper.mu.Unlock()

	c, code := restart(t, rig, keeper)
	if code != "" {
		t.Fatalf("after the restart: Start = %q; want the kept credential resumed, no machine login", code)
	}
	eventually(t, func() bool {
		current, pending := keptKey(t, keeper)
		tp := brokerKey(t, rig)
		return tp != nextTP && current == tp && pending == ""
	}, "the restarted daemon to take up the next key and renew from it")
	if _, err := c.Enroll(context.Background(), PodEnrollment{PodUID: "pod-uid-1", Thumbprint: "tp-pod-1", PodToken: rig.MintPodToken(t, "pod-uid-1")}); err != nil {
		t.Fatalf("Enroll after taking up the next key: %v", err)
	}
	if n := brokerLogins(t, rig); n != 1 {
		t.Fatalf("the broker holds %d machine logins, want the 1 a person approved", n)
	}
}

// TestContractABootWithARevokedCredentialStartsOneLogin: someone revokes the daemon's login on
// Dispatch's machine-login page while it is stopped. The restarted daemon takes the kept credential
// back, its boot renewal is refused, it logs at ERROR the credential to revoke, and it opens exactly
// one machine login, which a person approves before the pod enrolls.
func TestContractABootWithARevokedCredentialStartsOneLogin(t *testing.T) {
	withFastPolling(t)
	rig := newBrokerRig(t)
	keeper := &memoryKeeper{}
	_, stop := approvedClient(t, rig, keeper)
	stop()
	kept, _, _ := keeper.Load(context.Background())
	if status, body := rig.UI(t, http.MethodPost, "/v1/launcher-credentials/"+kept.CredentialID+"/revoke-by-approver",
		map[string]any{"approver": "bob@example.com"}); status != http.StatusOK {
		t.Fatalf("revoke the daemon's login = %d: %s", status, body)
	}

	var logged syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c := &Client{URL: rig.URL, Keeper: keeper, Log: slog.New(slog.NewTextHandler(&logged, nil))}
	if _, err := c.Start(ctx); err != nil {
		t.Fatalf("after the restart: Start: %v", err)
	}
	eventually(t, func() bool { return c.LoginStatus().State == "pending" }, "the refused boot renewal to open a machine login")
	if out := logged.String(); !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "credential_id="+kept.CredentialID) || !strings.Contains(out, "machine-login page") {
		t.Fatalf("log = %s, want an ERROR naming the refused credential %s and the machine-login page", out, kept.CredentialID)
	}
	if n := brokerLogins(t, rig); n != 2 {
		t.Fatalf("the broker holds %d machine logins, want the revoked one and one new one", n)
	}
	approveMachineLogin(t, rig, c.LoginStatus().Code)
	eventually(t, func() bool { return c.LoginStatus().State == "issued" }, "the new login to be issued")
	if _, err := c.Enroll(context.Background(), PodEnrollment{PodUID: "pod-uid-1", Thumbprint: "tp-pod-1", PodToken: rig.MintPodToken(t, "pod-uid-1")}); err != nil {
		t.Fatalf("Enroll after the new login: %v", err)
	}
	if n := brokerLogins(t, rig); n != 2 {
		t.Fatalf("the broker holds %d machine logins, want 2", n)
	}
}

// TestContractAnEnrollmentTakesUpTheNextKeyTheBrokerHolds: with a renewal's answer unknown, the
// broker holds the next key, and an enrollment comes before the renewal loop has asked which key it
// holds (as when the broker cannot answer that question). The enrollment's first proof, signed with
// the old key, is refused; it retries with the next key, enrolls the pod, and the daemon takes that
// key up and keeps it, opening no machine login.
func TestContractAnEnrollmentTakesUpTheNextKeyTheBrokerHolds(t *testing.T) {
	withFastPolling(t)
	rig := newBrokerRig(t)
	keeper := &memoryKeeper{}
	_, stop := approvedClient(t, rig, keeper)
	stop()
	next := mustTestKey(t)
	nextDER, err := x509.MarshalPKCS8PrivateKey(next)
	if err != nil {
		t.Fatal(err)
	}
	nextTP, err := thumbprint(&next.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rig.Store.Pool.Exec(context.Background(), `update launcher_credentials set key_thumbprint=$1`, nextTP); err != nil {
		t.Fatalf("move the broker's credential to the next key: %v", err)
	}
	keeper.mu.Lock()
	keeper.kept.NextPrivateKey = nextDER
	keeper.mu.Unlock()

	c := &Client{URL: rig.URL, Keeper: keeper}
	if !c.resume(context.Background()) {
		t.Fatal("resume took nothing back")
	}
	if _, err := c.Enroll(context.Background(), PodEnrollment{PodUID: "pod-uid-1", Thumbprint: "tp-pod-1", PodToken: rig.MintPodToken(t, "pod-uid-1")}); err != nil {
		t.Fatalf("Enroll with the broker holding the next key: %v", err)
	}
	if current, pending := keptKey(t, keeper); current != nextTP || pending != "" {
		t.Fatalf("kept key %s with next %q, want the next key %s taken up and no next key", current, pending, nextTP)
	}
	if got := c.LoginStatus().State; got != "issued" || brokerLogins(t, rig) != 1 {
		t.Fatalf("LoginStatus %q, %d machine logins; want issued and the one a person approved", got, brokerLogins(t, rig))
	}
}
