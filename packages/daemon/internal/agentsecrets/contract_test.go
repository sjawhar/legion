// contract_test.go drives Client against the real broker (packages/envoy's own httptest server,
// mounted through its brokertest.NewRig) instead of client_test.go's fakeBroker: this is the
// byte-compatibility proof for jws.go and the token login, judged by the broker's own
// record.VerifyRequestObject, its Kubernetes token verifier against the rig's local OIDC issuer, and
// its proof.Verifier, not by a reimplementation of the wire contract by inspection. It skips without
// BROKER_TEST_DATABASE_URL (brokertest.NewRig's own skip), except on GitHub Actions, where the Tests
// workflow's test job sets it and a skip would hide that it stopped (newBrokerRig).
//
// This is the only file in this module that imports anything under github.com/sjawhar/envoy/...:
// that import is test-only, never leaking into the daemon's production build. It imports
// packages/envoy/brokertest, a thin public facade over the internal rig, because Go's
// internal-package rule refuses this module an import under github.com/sjawhar/envoy/internal/...
package agentsecrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/envoy/brokertest"
)

// newBrokerRig is brokertest.NewRig, failing rather than skipping on GitHub Actions when
// BROKER_TEST_DATABASE_URL is unset: the Tests workflow's test job names its Postgres service
// there, and a silent skip would let the contract tests stop running unnoticed.
func newBrokerRig(t *testing.T) *brokertest.Rig {
	t.Helper()
	if os.Getenv("BROKER_TEST_DATABASE_URL") == "" && os.Getenv("GITHUB_ACTIONS") == "true" {
		t.Fatal("BROKER_TEST_DATABASE_URL is unset on GitHub Actions: the Tests workflow's test job names its Postgres service")
	}
	return brokertest.NewRig(t)
}

// daemonTokenFile writes a fresh ServiceAccount token for account, living lifetime, where the
// kubelet would project it, and returns its path.
func daemonTokenFile(t *testing.T, rig *brokertest.Rig, account string, lifetime time.Duration) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(rig.MintAccountToken(t, account, lifetime)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// count answers one count(*) query against the rig's broker database.
func count(t *testing.T, rig *brokertest.Rig, query string) int {
	t.Helper()
	var n int
	if err := rig.Store.Pool.QueryRow(context.Background(), query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// enrollPod enrolls a fresh pod under c, answering its enrollment.
func enrollPod(t *testing.T, rig *brokertest.Rig, c *Client, podUID string) Enrollment {
	t.Helper()
	e, err := c.Enroll(context.Background(), PodEnrollment{PodUID: podUID, Thumbprint: "tp-" + podUID, PodToken: rig.MintPodToken(t, podUID)})
	if err != nil || e.ID == "" {
		t.Fatalf("Enroll %s = %+v, %v", podUID, e, err)
	}
	return e
}

// TestContractARestartedDaemonLogsInWithItsTokenAndAsksNoOne is a daemon's restart against the real
// broker: the first boot logs in with its ServiceAccount token and enrolls a pod; the restart (a
// new Client, as a new process, with the kubelet's next token) logs in again and enrolls a pod with
// no machine login opened at any point, so nothing waits for a person. The broker holds the
// account's one credential, which the restart's key now signs for: it ends the pod the first boot
// enrolled, and the first boot's key is refused.
func TestContractARestartedDaemonLogsInWithItsTokenAndAsksNoOne(t *testing.T) {
	rig := newBrokerRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	first := &Client{URL: rig.URL, TokenFile: daemonTokenFile(t, rig, brokertest.DaemonAccount, time.Hour)}
	if err := first.Start(ctx); err != nil {
		t.Fatalf("first boot's Start: %v", err)
	}
	earlier := enrollPod(t, rig, first, "pod-uid-1")
	cancel()

	ctx2, cancel2 := context.WithCancel(context.Background())
	t.Cleanup(cancel2)
	restarted := &Client{URL: rig.URL, TokenFile: daemonTokenFile(t, rig, brokertest.DaemonAccount, time.Hour)}
	if err := restarted.Start(ctx2); err != nil {
		t.Fatalf("restart's Start: %v", err)
	}
	if got := restarted.LoginStatus().State; got != "issued" {
		t.Fatalf("after the restart: LoginStatus = %q, want issued", got)
	}
	enrollPod(t, rig, restarted, "pod-uid-2")
	if err := restarted.Revoke(ctx2, earlier.ID); err != nil {
		t.Fatalf("the restart ends the pod the first boot enrolled: %v", err)
	}
	if _, err := first.Enroll(ctx2, PodEnrollment{PodUID: "pod-uid-3", Thumbprint: "tp-pod-uid-3", PodToken: rig.MintPodToken(t, "pod-uid-3")}); !isNoCredential(err, "") {
		t.Fatalf("the first boot's key after the restart = %v, want the broker's refusal", err)
	}
	if n := count(t, rig, `select count(*) from credential_requests`); n != 0 {
		t.Fatalf("the broker holds %d machine-login records, want none: no person approves a token login", n)
	}
	if n := count(t, rig, `select count(*) from launcher_credentials`); n != 1 {
		t.Fatalf("the broker holds %d launcher credentials, want the account's one", n)
	}
}

// TestContractARefusedCredentialLogsInAgain: once the broker refuses the daemon's credential (here
// SQL-expired, as a credential the daemon could not renew in time), the next enrollment answers
// NO_MACHINE_CREDENTIAL and the login loop logs in again with the token, after which a pod enrolls,
// with no restart and no person.
func TestContractARefusedCredentialLogsInAgain(t *testing.T) {
	rig := newBrokerRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c := &Client{URL: rig.URL, TokenFile: daemonTokenFile(t, rig, brokertest.DaemonAccount, time.Hour)}
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.Store.Pool.Exec(ctx, `update launcher_credentials set expires_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Enroll(ctx, PodEnrollment{PodUID: "pod-uid-1", Thumbprint: "tp-1", PodToken: rig.MintPodToken(t, "pod-uid-1")}); !isNoCredential(err, "") {
		t.Fatalf("Enroll with an expired credential = %v, want NO_MACHINE_CREDENTIAL", err)
	}
	eventually(t, func() bool {
		_, err := c.Enroll(ctx, PodEnrollment{PodUID: "pod-uid-2", Thumbprint: "tp-2", PodToken: rig.MintPodToken(t, "pod-uid-2")})
		return err == nil
	}, "the login loop to log in again and a pod to enroll")
	if n := count(t, rig, `select count(*) from credential_requests`); n != 0 {
		t.Fatalf("the broker holds %d machine-login records, want none", n)
	}
}

// TestContractARenewalKeepsTheCredentialAndItsPods: a renewal proves the key the credential is
// already bound to, so the real broker keeps the credential's id and key: a pod enrolled before the
// renewal is still the daemon's to end after it, a proof signed before the renewal still
// authenticates, and the broker holds one credential throughout.
func TestContractARenewalKeepsTheCredentialAndItsPods(t *testing.T) {
	rig := newBrokerRig(t)
	ctx := context.Background()
	c := &Client{URL: rig.URL, TokenFile: daemonTokenFile(t, rig, brokertest.DaemonAccount, time.Hour)}
	first, err := c.Login(ctx)
	if err != nil {
		t.Fatal(err)
	}
	before := c.cred.Load()
	enrolled := enrollPod(t, rig, c, "pod-uid-1")
	time.Sleep(10 * time.Millisecond)
	// The kubelet's next token, which outlives the first, so the renewed credential expires later.
	c.TokenFile = daemonTokenFile(t, rig, brokertest.DaemonAccount, time.Hour+30*time.Second)
	renewed, err := c.Login(ctx)
	if err != nil {
		t.Fatal(err)
	}
	after := c.cred.Load()
	if after.id != before.id || after.key != before.key || !renewed.After(first) {
		t.Fatalf("renewal moved the credential from %s to %s (same key %v), expiry %v to %v; want the same credential, later", before.id, after.id, after.key == before.key, first, renewed)
	}
	c.cred.Store(before)
	if err := c.Revoke(ctx, enrolled.ID); err != nil {
		t.Fatalf("a proof under the credential as it was before the renewal: %v", err)
	}
	if n := count(t, rig, `select count(*) from launcher_credentials`); n != 1 {
		t.Fatalf("the broker holds %d launcher credentials, want the account's one", n)
	}
}

// TestContractOnlyTheListedAccountWithALiveTokenLogsIn: a token for an account the broker does not
// list is refused ACCOUNT_NOT_LISTED and an expired token TOKEN_EXPIRED, each a failed login and no
// credential; once BROKER_SERVICE_LOGINS drops the daemon's own account, its live credential stops
// authenticating and its next login is refused.
func TestContractOnlyTheListedAccountWithALiveTokenLogsIn(t *testing.T) {
	rig := newBrokerRig(t)
	ctx := context.Background()
	for _, c := range []struct {
		name, account string
		lifetime      time.Duration
		code          string
	}{
		{"an unlisted account", "system:serviceaccount:legion-system:someone-else", time.Hour, "ACCOUNT_NOT_LISTED"},
		{"an expired token", brokertest.DaemonAccount, -time.Minute, "TOKEN_EXPIRED"},
	} {
		client := &Client{URL: rig.URL, TokenFile: daemonTokenFile(t, rig, c.account, c.lifetime)}
		_, err := client.Login(ctx)
		var api *APIError
		if !errors.As(err, &api) || api.Code != c.code || client.LoginStatus().State != "failed" {
			t.Fatalf("%s: Login = %v, state %q; want %s and failed", c.name, err, client.LoginStatus().State, c.code)
		}
	}

	c := &Client{URL: rig.URL, TokenFile: daemonTokenFile(t, rig, brokertest.DaemonAccount, time.Hour)}
	if _, err := c.Login(ctx); err != nil {
		t.Fatal(err)
	}
	enrollPod(t, rig, c, "pod-uid-1")
	rig.DropServiceLogin(brokertest.DaemonAccount)
	if _, err := c.Enroll(ctx, PodEnrollment{PodUID: "pod-uid-2", Thumbprint: "tp-2", PodToken: rig.MintPodToken(t, "pod-uid-2")}); !isNoCredential(err, "") {
		t.Fatalf("Enroll once the account is unlisted = %v, want the broker's refusal", err)
	}
	_, err := c.Login(ctx)
	var api *APIError
	if !errors.As(err, &api) || api.Code != "ACCOUNT_NOT_LISTED" {
		t.Fatalf("Login once the account is unlisted = %v, want ACCOUNT_NOT_LISTED", err)
	}
}
