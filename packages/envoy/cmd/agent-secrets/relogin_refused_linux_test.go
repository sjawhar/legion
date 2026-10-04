// packages/envoy/cmd/agent-secrets/relogin_refused_linux_test.go
//go:build linux

package main

import (
	"testing"

	"github.com/sjawhar/envoy/internal/broker/brokertest"
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
	srv, sock := serveRealHelper(t, rig.URL, rig.Operator)

	if code, exit, stderr := launcherLoginThen(t, rig, binary, sock, "approve", nil); exit != 0 {
		t.Fatalf("approved launcher login (code %s): exit %d, stderr %q; want 0", code, exit, stderr)
	}
	code, exit, stderr := launcherLoginThen(t, rig, binary, sock, "deny", func() { refuseTheHeldCredential(t, rig, srv, sock) })
	if exit != 1 || stderr != "agent-secrets launcher login: denied\n" {
		t.Fatalf("re-login denied after the old credential was refused (code %s): exit %d, stderr %q; want 1, %q",
			code, exit, stderr, "agent-secrets launcher login: denied\n")
	}
}
