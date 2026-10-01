// packages/envoy/cmd/agent-secrets/relogin_refused_linux_test.go
//go:build linux

package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/helper"
	"github.com/sjawhar/envoy/internal/broker/proof"
)

// TestADeniedReLoginSaysDeniedWhenTheOldCredentialIsRefusedMeanwhile: an approved machine login,
// then a re-login that is still waiting for approval when the broker refuses the first login's
// credential, then the operator denies the re-login. The person who ran `launcher login` must be
// told it was denied: the refusal is about the earlier credential, and this login's own outcome is
// the denial. Only its message is at stake (it exits 1 either way); "expired" would send them
// looking for a timeout that never happened.
func TestADeniedReLoginSaysDeniedWhenTheOldCredentialIsRefusedMeanwhile(t *testing.T) {
	rig := brokertest.NewRig(t)
	binary := buildAgentSecrets(t)
	srv, sock := serveRealHelper(t, rig.URL)

	if code, exit, stderr := launcherLoginThen(t, rig, binary, sock, "approve", nil); exit != 0 {
		t.Fatalf("approved launcher login (code %s): exit %d, stderr %q; want 0", code, exit, stderr)
	}
	refuseTheHeldCredential := func() {
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
	code, exit, stderr := launcherLoginThen(t, rig, binary, sock, "deny", refuseTheHeldCredential)
	if exit != 1 || stderr != "agent-secrets launcher login: denied\n" {
		t.Fatalf("re-login denied after the old credential was refused (code %s): exit %d, stderr %q; want 1, %q",
			code, exit, stderr, "agent-secrets launcher login: denied\n")
	}
}

// launcherLoginThen is launcherLogin with a hook: it runs `agent-secrets launcher login`, reads the
// code, runs before (when non-nil) while that login is still pending, then decides the login on
// the broker and returns the command's exit status and stderr once its poll has read the decision.
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
	go func() {
		for lines.Scan() {
		}
	}()
	if before != nil {
		before()
	}

	status, body := rig.UI(t, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": code})
	if status != http.StatusOK {
		t.Fatalf("POST /v1/machine-logins/lookup = %d: %s", status, body)
	}
	var looked struct {
		RecordID   string `json:"record_id"`
		Challenges struct {
			Approve string `json:"approve"`
			Deny    string `json:"deny"`
		} `json:"challenges"`
	}
	if err := json.Unmarshal(body, &looked); err != nil {
		t.Fatalf("decode lookup: %v (body: %s)", err, body)
	}
	encoded := looked.Challenges.Approve
	if action == "deny" {
		encoded = looked.Challenges.Deny
	}
	challenge, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(challenge) == 0 {
		t.Fatalf("decode the %s challenge %q: %v", action, encoded, err)
	}
	assertion := rig.Approver.Assert(t, brokertest.RPID, brokertest.Origin, challenge)
	status, body = rig.UI(t, http.MethodPost, "/v1/credential-requests/"+looked.RecordID+"/"+action,
		map[string]any{"assertion": json.RawMessage(assertion), "code": code})
	if status != http.StatusOK {
		t.Fatalf("%s machine login = %d: %s", action, status, body)
	}

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
