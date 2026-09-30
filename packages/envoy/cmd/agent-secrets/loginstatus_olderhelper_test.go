// packages/envoy/cmd/agent-secrets/loginstatus_olderhelper_test.go

package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/helper"
)

// TestLoginStatusDoesNotClaimAnUnapprovedLoginFromAnOlderHelper: a helper from before
// login_refused existed answers a credential the broker refused with exactly this reply —
// "expired", no login_refused — and such a helper keeps running after the client is upgraded,
// since the installer restarts it only when no session is registered. The client cannot tell
// that reply from a login nobody approved in time, so it must not tell the operator the login
// was never approved.
func TestLoginStatusDoesNotClaimAnUnapprovedLoginFromAnOlderHelper(t *testing.T) {
	binary := buildAgentSecrets(t)
	sock := filepath.Join(t.TempDir(), "h.sock")
	fakeHelperAt(t, sock, helper.Response{OK: true, LoginState: "expired"})
	stdout, stderr, exit := runAgentSecrets(t, binary, "http://unused", t.TempDir(),
		[]string{"AGENT_SECRETS_HELPER_SOCK=" + sock}, "launcher", "login-status")
	if exit != 1 || stdout != "expired\n" {
		t.Fatalf("exit %d, stdout %q; want 1, %q", exit, stdout, "expired\n")
	}
	if strings.Contains(stderr, "before it was approved") {
		t.Fatalf("stderr %q claims the login was never approved; an older helper sends this same reply for a refused credential", stderr)
	}
	if !strings.Contains(stderr, "run: agent-secrets launcher login") {
		t.Fatalf("stderr %q does not name the remedy", stderr)
	}
}
