// packages/envoy/cmd/agent-secrets/vm_rig_test.go
//
// The whole of a lane VM's path against the real broker (brokertest.NewRig: real handlers, real
// Postgres, the Google issuer stood up by WithVMFleet), driving the real binary: the metadata
// server mints the VM's identity token, `enroll --vm` logs in and enrols with it, and the exec
// form then reads the VM's service's secret with no person approving. Skips without
// BROKER_TEST_DATABASE_URL, as every rig test does.
package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/brokertest"
)

// TestAVMEnrolsAndReadsItsServicesSecretWithNoApproval is the acceptance of the whole client
// path: nothing is installed on the VM, nobody approves anything, and the value the broker holds
// for the fleet's secret reaches the VM's own command's environment.
func TestAVMEnrolsAndReadsItsServicesSecretWithNoApproval(t *testing.T) {
	rig := brokertest.NewRig(t, brokertest.WithVMFleet())
	binary := buildAgentSecrets(t)

	// The VM's metadata server: every ask mints a fresh token, as a real one does, so the
	// enrollment and each renewal carry a token that is live at that moment.
	metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" || r.URL.Query().Get("format") != "full" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if got := r.URL.Query().Get("audience"); got != brokertest.VMAudience {
			t.Errorf("audience = %q, want %q", got, brokertest.VMAudience)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Write([]byte(rig.MintVMToken(t, brokertest.VMIdentity{})))
	}))
	defer metadata.Close()

	keyDir := t.TempDir()
	env := []string{
		"GCE_METADATA_HOST=" + strings.TrimPrefix(metadata.URL, "http://"),
		"AGENT_SECRETS_AUDIENCE=" + brokertest.VMAudience,
		"AGENT_SECRETS_HELPER_SOCK=",
	}
	stdout, stderr, exit := runAgentSecrets(t, binary, rig.URL, keyDir, env,
		"enroll", "--vm", "--service", brokertest.VMService)
	if exit != 0 {
		t.Fatalf("enroll --vm: exit %d\nstdout %q\nstderr %q", exit, stdout, stderr)
	}
	if strings.TrimSpace(stdout) == "" {
		t.Fatalf("enroll --vm printed no enrollment id (stderr %q)", stderr)
	}

	stdout, stderr, exit = runAgentSecrets(t, binary, rig.URL, keyDir, env,
		"--wait", "30s", "--reason", "test", brokertest.VMSecret, "--", "printenv", brokertest.VMSecret)
	if exit != 0 {
		t.Fatalf("read %s: exit %d\nstdout %q\nstderr %q", brokertest.VMSecret, exit, stdout, stderr)
	}
	if strings.TrimSpace(stdout) != brokertest.VMSecretValue {
		t.Fatalf("%s = %q, want %q (stderr %q)", brokertest.VMSecret, stdout, brokertest.VMSecretValue, stderr)
	}
}
