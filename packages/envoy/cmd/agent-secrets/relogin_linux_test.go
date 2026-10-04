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
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/helper"
	"github.com/sjawhar/envoy/internal/broker/proof"
)

// TestLoginStatusFollowsTheCredentialTheHelperHolds: an approved machine login, then a re-login
// the operator denies. The denied `launcher login` exits 1 naming the denial, while login-status
// reads issued and exits 0, since the helper still holds the first login's credential, and says on
// stderr that the most recent login was denied. While the credential is held, login-status ends
// with the expiry the broker minted it with, a week out, and a box the helper enrolls with it
// reads its grants through `agent-secrets self` and an agent-tier secret's value through the client's
// grant values. Once the broker refuses that credential, login-status exits 1 saying so, and a
// re-login denied while the helper holds no credential reads denied and exits 1.
func TestLoginStatusFollowsTheCredentialTheHelperHolds(t *testing.T) {
	rig := brokertest.NewRig(t)
	binary := buildAgentSecrets(t)
	srv, sock := serveRealHelper(t, rig.URL, rig.Operator)
	loginStatus := func() (string, string, int) {
		t.Helper()
		return runAgentSecrets(t, binary, rig.URL, t.TempDir(), []string{"AGENT_SECRETS_HELPER_SOCK=" + sock}, "launcher", "login-status")
	}
	const prefix = "agent-secrets launcher login-status: "

	// --- an approved login: the helper holds its credential ---
	if code, exit, stderr := launcherLoginThen(t, rig, binary, sock, "approve", nil); exit != 0 {
		t.Fatalf("approved launcher login (code %s): exit %d, stderr %q; want 0", code, exit, stderr)
	}
	if !srv.Broker.HasCredential() {
		t.Fatal("the approved login installed no launcher credential")
	}
	var minted time.Time
	if err := rig.Store.Pool.QueryRow(context.Background(), `select expires_at from launcher_credentials where expires_at > now()`).Scan(&minted); err != nil {
		t.Fatalf("read the minted credential's expiry: %v", err)
	}
	// A week less the seconds the test has run so far.
	expiry := prefix + "the launcher credential expires at " + minted.UTC().Format(time.RFC3339) + " (in 6d23h"
	const expiryEnd = "m); the broker has no renewal, so a new machine login a human approves must replace it before then\n"
	if stdout, stderr, exit := loginStatus(); exit != 0 || stdout != "issued\n" || !strings.HasPrefix(stderr, expiry) || !strings.HasSuffix(stderr, expiryEnd) || strings.Count(stderr, "\n") != 1 {
		t.Fatalf("login-status after the approved login: exit %d, stdout %q, stderr %q; want 0, %q, %q…%q", exit, stdout, stderr, "issued\n", expiry, expiryEnd)
	}

	// --- a box the helper enrolls: self lists its grant, and its agent-tier secret comes back ---
	boxKey, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := proof.Thumbprint(&boxKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	enrolled, err := helper.Call(sock, helper.Request{Op: "enroll-box", RuntimeID: "self-box-" + t.Name(), Thumbprint: thumbprint}, 10*time.Second)
	if err != nil || !enrolled.OK {
		t.Fatalf("enroll-box with the approved login's credential: %+v %v", enrolled, err)
	}
	keyDir := writeKeyDir(t, boxKey, enrolled.EnrollmentID)
	stdout, stderr, exit := runAgentSecrets(t, binary, rig.URL, keyDir, nil, "request", "TEST_AUTO_SECRET", "--json")
	var granted RequestResult
	if err := json.Unmarshal([]byte(stdout), &granted); exit != 0 || err != nil || granted.State != "granted" || granted.GrantID == nil {
		t.Fatalf("request TEST_AUTO_SECRET from the box: exit %d, stdout %q, stderr %q; want it granted at once", exit, stdout, stderr)
	}
	wantGrant := "grant: " + *granted.GrantID + " (request " + granted.RequestID + ", expires "
	if stdout, stderr, exit := runAgentSecrets(t, binary, rig.URL, keyDir, nil, "self"); exit != 0 || !strings.Contains(stdout, wantGrant) {
		t.Fatalf("self after the grant: exit %d, stdout %q, stderr %q; want a line starting %q", exit, stdout, stderr, wantGrant)
	}
	values, err := newClient(rig.URL).GrantValues(context.Background(), &fileSigner{key: boxKey, enrollmentID: enrolled.EnrollmentID}, *granted.GrantID)
	if err != nil || values.Values["TEST_AUTO_SECRET"] != "test-auto-secret-v1" {
		t.Fatalf("grant values of the agent-tier secret: %+v %v; want TEST_AUTO_SECRET released", values, err)
	}

	// --- a re-login the operator denies: the first credential is still held ---
	denied, exit, stderr := launcherLoginThen(t, rig, binary, sock, "deny", nil)
	if exit != 1 || stderr != "agent-secrets launcher login: denied\n" {
		t.Fatalf("denied launcher login (code %s): exit %d, stderr %q; want 1 naming the denial", denied, exit, stderr)
	}
	if !srv.Broker.HasCredential() {
		t.Fatal("a denied re-login must leave the earlier login's credential in place")
	}
	want := prefix + "the most recent machine login (code " + denied + ") was denied; the helper still holds the launcher credential an earlier login issued\n" + expiry
	if stdout, stderr, exit := loginStatus(); exit != 0 || stdout != "issued\n" || !strings.HasPrefix(stderr, want) || !strings.HasSuffix(stderr, expiryEnd) || strings.Count(stderr, "\n") != 2 {
		t.Fatalf("login-status after a denied re-login: exit %d, stdout %q, stderr %q; want 0, %q, %q…%q", exit, stdout, stderr, "issued\n", want, expiryEnd)
	}

	// --- the broker refuses the held credential ---
	refuseTheHeldCredential(t, rig, srv, sock)
	want = prefix + "the broker refused the launcher credential (expired or revoked, or a proof it could not verify, such as clock skew or an AGENT_SECRETS_URL mismatch); run: agent-secrets launcher login\n"
	if stdout, stderr, exit := loginStatus(); exit != 1 || stdout != "expired\n" || stderr != want {
		t.Fatalf("login-status after the broker refused the credential: exit %d, stdout %q, stderr %q; want 1, %q, %q", exit, stdout, stderr, "expired\n", want)
	}

	// --- a re-login denied while the helper holds no credential ---
	if code, exit, stderr := launcherLoginThen(t, rig, binary, sock, "deny", nil); exit != 1 {
		t.Fatalf("denied launcher login (code %s): exit %d, stderr %q; want 1", code, exit, stderr)
	}
	want = prefix + "the most recent machine login was denied; run: agent-secrets launcher login\n"
	if stdout, stderr, exit := loginStatus(); exit != 1 || stdout != "denied\n" || stderr != want {
		t.Fatalf("login-status after a denied login with no credential held: exit %d, stdout %q, stderr %q; want 1, %q, %q", exit, stdout, stderr, "denied\n", want)
	}
}

// launcherLoginThen runs `agent-secrets launcher login` against the helper at sock and reads the
// code it prints. It runs before (when non-nil) while that login is still pending, then decides
// the login on the broker as the operator does on Dispatch's machine-login page
// (brokertest.Rig.DecideMachineLogin; action is "approve" or "deny"), and returns the code with
// the command's exit status and stderr once its poll has read the decision.
func launcherLoginThen(t *testing.T, rig *brokertest.Rig, binary, sock, action string, before func()) (code string, exit int, stderr string) {
	t.Helper()
	cmd := exec.Command(binary, "launcher", "login")
	cmd.Env = append(os.Environ(), "AGENT_SECRETS_HELPER_SOCK="+sock, "AGENT_SECRETS_URL="+rig.URL, "AGENT_SECRETS_KEY_DIR=", "AGENT_SECRETS_APPROVE_URL=")
	var errOut strings.Builder
	cmd.Stderr = &errOut
	out, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	cmd.Stdout = w
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	var waitErr error
	waited := make(chan struct{})
	go func() { waitErr = cmd.Wait(); close(waited) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-waited })
	lines := bufio.NewScanner(out)
	if !lines.Scan() {
		<-waited
		t.Fatalf("launcher login printed no code; stderr %q", errOut.String())
	}
	code, ok := strings.CutPrefix(lines.Text(), "machine login code: ")
	if !ok {
		t.Fatalf("launcher login's first line is %q, want the machine login code", lines.Text())
	}
	go func() { // the rest of stdout, so the command never blocks writing it
		for lines.Scan() {
		}
	}()
	if before != nil {
		before()
	}

	rig.DecideMachineLogin(t, code, action == "approve")

	select {
	case <-waited:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		<-waited
		t.Fatalf("launcher login never read the %s decision; stderr %q", action, errOut.String())
	}
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		t.Fatalf("launcher login: %v", waitErr)
	}
	return code, cmd.ProcessState.ExitCode(), errOut.String()
}

// refuseTheHeldCredential makes the broker refuse the launcher credential srv's helper holds:
// it expires every live credential by direct SQL, then asks the helper at sock to enroll a box,
// which the broker answers 401 LAUNCHER_INVALID, and requires that the helper cleared it.
func refuseTheHeldCredential(t *testing.T, rig *brokertest.Rig, srv *helper.Server, sock string) {
	t.Helper()
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
}
