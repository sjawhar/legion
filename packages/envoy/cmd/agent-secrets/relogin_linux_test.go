// packages/envoy/cmd/agent-secrets/relogin_linux_test.go
//go:build linux

// The weekly re-login runs while the helper still holds the credential the last login issued. A
// re-login the operator denies, or lets expire, leaves that credential in place, and the helper
// keeps enrolling sessions with it, so login-status reports the credential the helper holds
// rather than the outcome of the most recent login. These tests drive the real CLI against a real
// helper and a real broker (brokertest.NewRig), as internal/broker/helper's contract test does.
// Linux-only for the real helper's pidfd pinning.
package main

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/helper"
	"github.com/sjawhar/envoy/internal/broker/proof"
)

// TestLoginStatusFollowsTheCredentialTheHelperHolds: an approved machine login, then a re-login
// the operator denies. The denied `launcher login` exits 1 naming the denial, while login-status
// reads issued and exits 0, since the helper still holds the first login's credential, and says on
// stderr that the most recent login was denied. Once the broker refuses that credential,
// login-status exits 1 saying so, and a re-login denied while the helper holds no credential
// reads denied and exits 1.
func TestLoginStatusFollowsTheCredentialTheHelperHolds(t *testing.T) {
	rig := brokertest.NewRig(t)
	binary := buildAgentSecrets(t)
	srv, sock := serveRealHelper(t, rig.URL)
	loginStatus := func() (string, string, int) {
		t.Helper()
		return runAgentSecrets(t, binary, rig.URL, t.TempDir(), []string{"AGENT_SECRETS_HELPER_SOCK=" + sock}, "launcher", "login-status")
	}
	const prefix = "agent-secrets launcher login-status: "

	// --- an approved login: the helper holds its credential ---
	if code, exit, stderr := launcherLogin(t, rig, binary, sock, "approve"); exit != 0 {
		t.Fatalf("approved launcher login (code %s): exit %d, stderr %q; want 0", code, exit, stderr)
	}
	if !srv.Broker.HasCredential() {
		t.Fatal("the approved login installed no launcher credential")
	}
	if stdout, stderr, exit := loginStatus(); exit != 0 || stdout != "issued\n" || stderr != "" {
		t.Fatalf("login-status after the approved login: exit %d, stdout %q, stderr %q; want 0, %q, nothing", exit, stdout, stderr, "issued\n")
	}

	// --- a re-login the operator denies: the first credential is still held ---
	denied, exit, stderr := launcherLogin(t, rig, binary, sock, "deny")
	if exit != 1 || stderr != "agent-secrets launcher login: denied\n" {
		t.Fatalf("denied launcher login (code %s): exit %d, stderr %q; want 1 naming the denial", denied, exit, stderr)
	}
	if !srv.Broker.HasCredential() {
		t.Fatal("a denied re-login must leave the earlier login's credential in place")
	}
	want := prefix + "the most recent machine login (code " + denied + ") was denied; the helper still holds the launcher credential an earlier login issued\n"
	if stdout, stderr, exit := loginStatus(); exit != 0 || stdout != "issued\n" || stderr != want {
		t.Fatalf("login-status after a denied re-login: exit %d, stdout %q, stderr %q; want 0, %q, %q", exit, stdout, stderr, "issued\n", want)
	}

	// --- the broker refuses the held credential: expire it by direct SQL, then enroll a box ---
	if _, err := rig.Store.Pool.Exec(context.Background(),
		`update launcher_credentials set expires_at = now() - interval '1 minute' where expires_at > now()`); err != nil {
		t.Fatalf("expire the launcher credential: %v", err)
	}
	boxKey, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := proof.Thumbprint(&boxKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := helper.Call(sock, helper.Request{Op: "enroll-box", RuntimeID: "box-" + t.Name(), Thumbprint: thumbprint}, 10*time.Second); err != nil || resp.OK {
		t.Fatalf("enroll-box with an expired credential: %+v %v; want the broker's refusal", resp, err)
	}
	if srv.Broker.HasCredential() {
		t.Fatal("the broker's refusal must clear the launcher credential")
	}
	want = prefix + "the broker refused this machine's launcher credential (expired, revoked, or a proof it could not verify, such as clock skew or an AGENT_SECRETS_URL mismatch); run: agent-secrets launcher login\n"
	if stdout, stderr, exit := loginStatus(); exit != 1 || stdout != "expired\n" || stderr != want {
		t.Fatalf("login-status after the broker refused the credential: exit %d, stdout %q, stderr %q; want 1, %q, %q", exit, stdout, stderr, "expired\n", want)
	}

	// --- a re-login denied while the helper holds no credential ---
	if code, exit, stderr := launcherLogin(t, rig, binary, sock, "deny"); exit != 1 {
		t.Fatalf("denied launcher login (code %s): exit %d, stderr %q; want 1", code, exit, stderr)
	}
	want = prefix + "the last machine login is denied; run: agent-secrets launcher login\n"
	if stdout, stderr, exit := loginStatus(); exit != 1 || stdout != "denied\n" || stderr != want {
		t.Fatalf("login-status after a denied login with no credential held: exit %d, stdout %q, stderr %q; want 1, %q, %q", exit, stdout, stderr, "denied\n", want)
	}
}

// launcherLogin runs `agent-secrets launcher login` against the helper at sock, reads the code it
// prints, decides that machine login on the broker as the operator does on the Dispatch page
// (look it up by the code, then approve or deny it with the seeded approver key), and returns the
// code with the command's exit status and stderr once its poll has read the decision.
func launcherLogin(t *testing.T, rig *brokertest.Rig, binary, sock, action string) (code string, exit int, stderr string) {
	t.Helper()
	return launcherLoginThen(t, rig, binary, sock, action, nil)
}
