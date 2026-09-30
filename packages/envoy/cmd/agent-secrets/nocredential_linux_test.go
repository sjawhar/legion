// packages/envoy/cmd/agent-secrets/nocredential_linux_test.go
//go:build linux

// A host session on a helper that holds no launcher credential has no broker identity: after a
// reboot or helper restart the helper enrolls no one until the operator logs the machine in, so
// identity sends its callers to their other backend, and every command that meets the helper's
// NO_CREDENTIAL answer says so in one line. Linux-only for the real helper's pidfd pinning.
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const notLoggedInNotice = "agent-secrets: this machine is not logged in to the secrets broker; not an agent session (run: agent-secrets launcher login)\n"

// runInSession runs `agent-secrets register --exec -- binary args...` against the helper at sock,
// so the command runs as a freshly registered host session, and returns its exit code and output.
func runInSession(t *testing.T, binary, sock string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(binary, append([]string{"register", "--exec", "--", binary}, args...)...)
	cmd.Env = append(os.Environ(), "AGENT_SECRETS_HELPER_SOCK="+sock, "AGENT_SECRETS_KEY_DIR=",
		"AGENT_SECRETS_URL=http://127.0.0.1:1", "XDG_RUNTIME_DIR="+t.TempDir(), "HOME="+t.TempDir())
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	_ = cmd.Run()
	return cmd.ProcessState.ExitCode(), out.String(), errOut.String()
}

// TestIdentityOnAHelperWithoutALauncherCredential: a registered session whose helper was never
// logged in is not an agent session. identity exits 1 with the notice, where a helper still
// enrolling with a credential gets exit 0 (TestIdentityWhileAHelperWithACredentialEnrolls).
func TestIdentityOnAHelperWithoutALauncherCredential(t *testing.T) {
	binary := buildAgentSecrets(t)
	sock := realHelper(t)
	code, stdout, stderr := runInSession(t, binary, sock, "identity")
	if code != 1 || stdout != "" || stderr != notLoggedInNotice {
		t.Fatalf("identity in a registered session on a helper with no credential: exit %d, stdout %q, stderr %q; want 1, nothing, %q", code, stdout, stderr, notLoggedInNotice)
	}
}

// TestRegisterWaitReturnsAtOnceWithoutALauncherCredential runs `register --wait 10 --exec` against
// the real helper while it holds no launcher credential: its enroll loop cannot succeed, so the
// helper answers at once instead of holding the launch for the full wait. The launch still execs,
// warning that the machine is not logged in, what the session's secrets calls do until it is, and
// the login command. The bound only rules out the 10 s wait.
func TestRegisterWaitReturnsAtOnceWithoutALauncherCredential(t *testing.T) {
	binary := buildAgentSecrets(t)
	sock := realHelper(t)
	cmd := exec.Command(binary, "register", "--wait", "10", "--exec", "--", "sh", "-c", "echo ran")
	cmd.Env = append(os.Environ(), "AGENT_SECRETS_HELPER_SOCK="+sock, "AGENT_SECRETS_KEY_DIR=")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	start := time.Now()
	out, err := cmd.Output()
	elapsed := time.Since(start)
	if err != nil || strings.TrimSpace(string(out)) != "ran" {
		t.Fatalf("the command must still run: %q %v (stderr %q)", out, err, stderr.String())
	}
	const warning = "agent-secrets register: this machine is not logged in to the secrets broker; launching anyway, and until it is (run: agent-secrets launcher login) this session's agent-secrets calls fail and secret-run uses secretsd\n"
	if stderr.String() != warning {
		t.Fatalf("stderr %q, want the no-credential warning %q", stderr.String(), warning)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("register --wait 10 took %s with no credential; it must not wait", elapsed)
	}
	t.Logf("register --wait 10 with no credential returned in %s", elapsed)
}

// TestIdentityWhileAHelperWithACredentialEnrolls: once the helper holds a launcher credential, a
// registered session it has not enrolled yet is an agent session (NOT_ENROLLED), and identity
// exits 0 silently. The broker here issues the machine login and refuses every enrollment, so the
// session stays unenrolled for the whole test.
func TestIdentityWhileAHelperWithACredentialEnrolls(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/launcher-credentials", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"pending_id":"pending-1","code":"ABCD-EFGH"}`))
	})
	mux.HandleFunc("GET /v1/launcher-credentials/pending-1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"state": "issued", "credential_id": "cred-1"})
	})
	mux.HandleFunc("POST /v1/enrollments", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"code":"DATABASE","error":"postgres unreachable"}`))
	})
	broker := httptest.NewServer(mux)
	defer broker.Close()

	binary := buildAgentSecrets(t)
	srv, sock := serveRealHelper(t, broker.URL)
	if _, err := srv.Broker.Login(context.Background(), srv.Hostname); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !srv.Broker.HasCredential() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !srv.Broker.HasCredential() {
		t.Fatal("the helper never installed the issued launcher credential")
	}
	code, stdout, stderr := runInSession(t, binary, sock, "identity")
	if code != 0 || stdout != "" || stderr != "" {
		t.Fatalf("identity in a registered, enrolling session on a logged-in helper: exit %d, stdout %q, stderr %q; want 0 and nothing", code, stdout, stderr)
	}
}

// TestCommandsOnAHelperWithoutALauncherCredentialPrintTheNotice: every command that asks the helper
// to sign, for a proof or for a request object, fails with identity's line rather than the
// helper's raw code.
func TestCommandsOnAHelperWithoutALauncherCredentialPrintTheNotice(t *testing.T) {
	binary := buildAgentSecrets(t)
	sock := realHelper(t)
	for _, args := range [][]string{
		{"SOME_KEY", "--reason", "test", "--", "true"},
		{"request", "SOME_KEY", "--reason", "test"},
		{"self"},
		{"sign", "--method", "GET", "--url", "https://broker.internal.example/v1/enrollments/self"},
	} {
		code, stdout, stderr := runInSession(t, binary, sock, args...)
		if code != 1 || stdout != "" || stderr != notLoggedInNotice {
			t.Errorf("agent-secrets %s: exit %d, stdout %q, stderr %q; want 1, nothing, %q", strings.Join(args, " "), code, stdout, stderr, notLoggedInNotice)
		}
	}
}
