// packages/envoy/cmd/agent-secrets/main_test.go
//
// These tests build the real agent-secrets binary and run it as a subprocess against a fake
// broker: the "NAME... -- <command>" form ends in syscall.Exec, which replaces the calling
// process image, so it cannot be exercised in-process the way an ordinary function call can.
package main

import (
	"bufio"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/helper"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
)

// testEnrollmentID is the enrollment id these tests' key dir fixture "issues" (writes into the
// enrollment file), and the id fakeBroker's GET /v1/enrollments/self echoes back — proof that
// self round-trips whatever id was on disk rather than a hardcoded one.
const testEnrollmentID = "enr-fake-1"

// buildAgentSecrets compiles this package's binary into the test's temporary directory.
func buildAgentSecrets(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "agent-secrets")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build agent-secrets: %v\n%s", err, out)
	}
	return binary
}

// brokerCounters records how many times fakeBroker served each route, so a test can assert on
// call counts (e.g. "no second ask", "never entered the polling loop") instead of only on the
// final observable outcome, plus (guarded by mu) the last session_id POST /v1/requests recorded.
type brokerCounters struct {
	createRequest int32
	getRequest    int32

	mu            sync.Mutex
	lastSessionID string
}

func (c *brokerCounters) sessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastSessionID
}

// fakeBroker serves just enough of the shared broker contract
// (dispatch://AGENTC-393/artifact/plan-overview-md) for the exec-form and --json
// tests: POST /v1/requests decodes the signed request object CreateRequest posts (verifying it
// with record.VerifyRequestObject against the fake's own URL as audience — a real, non-stubbed
// check, since the wire shape under test IS that signed object) and routes on its first
// authorization_detail's identifier to a canned granted/pending/denied/proxy-only/no-trailing-
// newline response, GET /v1/requests/{id} answers the pending case's own request id with the
// same never-resolving pending state (and 404s any other id, since a granted-or-denied-
// immediately response must never be polled), POST /v1/grants/{id}/values releases one canned
// value (or, for the proxy-only case, none at all), and GET /v1/enrollments/self echoes
// testEnrollmentID. It does not verify the outer Proof header at all — proof.Verifier's own
// behavior is covered by internal/broker/proof and internal/broker/api's test suites, not this
// package's.
func fakeBroker(t *testing.T) (*httptest.Server, *brokerCounters) {
	t.Helper()
	counters := &brokerCounters{}
	var audience string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/requests", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&counters.createRequest, 1)
		var body struct {
			Request   string  `json:"request"`
			SessionID *string `json:"session_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Request == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if body.SessionID != nil {
			counters.mu.Lock()
			counters.lastSessionID = *body.SessionID
			counters.mu.Unlock()
		}
		obj, err := record.VerifyRequestObject(body.Request, audience, time.Minute, time.Now())
		if err != nil || len(obj.Details) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch name := obj.Details[0].Identifier; name {
		case "GRANT_ME":
			writeJSON(w, map[string]any{
				"request_id": "req-granted", "state": "granted",
				"secrets":  []map[string]string{{"name": name, "decision": "automatic", "delivery": "inject"}},
				"grant_id": "grant-granted", "record_id": nil,
			})
		case "PENDING_ME":
			writeJSON(w, map[string]any{
				"request_id": "req-pending", "state": "pending",
				"secrets":  []map[string]string{{"name": name, "decision": "approval", "delivery": "inject"}},
				"grant_id": nil, "record_id": "rec-pending-1",
			})
		case "DENY_ME":
			writeJSON(w, map[string]any{
				"request_id": "req-denied", "state": "denied",
				"secrets":  []map[string]string{{"name": name, "decision": "deny", "delivery": "inject"}},
				"grant_id": nil, "record_id": nil,
			})
		case "PROXY_ME":
			writeJSON(w, map[string]any{
				"request_id": "req-proxy", "state": "granted",
				"secrets":  []map[string]string{{"name": name, "decision": "automatic", "delivery": "proxy"}},
				"grant_id": "grant-proxy", "record_id": nil,
			})
		case "NONEWLINE_ME":
			// Written with http.ResponseWriter.Write directly, with NO trailing newline, unlike
			// every other case (which goes through writeJSON's json.Encoder, always "\n"
			// terminated) — this is the fixture for the writeVerbatim byte-exact test.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"request_id":"req-nonewline","state":"granted","secrets":[{"name":"NONEWLINE_ME","decision":"automatic","delivery":"inject"}],"grant_id":"grant-nonewline","record_id":null}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	mux.HandleFunc("GET /v1/requests/{id}", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&counters.getRequest, 1)
		if r.PathValue("id") != "req-pending" {
			// A request the fake decided immediately (granted or denied) must never be polled;
			// failing loudly here (rather than serving it) is the reuse-transparency test's proof
			// that cmdExec does not enter its pending-wait loop for an already-decided response.
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]any{"state": "pending", "grant_id": nil, "record_id": "rec-pending-1", "decided_at": nil, "decision": nil})
	})
	mux.HandleFunc("POST /v1/grants/grant-granted/values", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"values":     map[string]string{"GRANT_ME": "topsecretvalue123"},
			"expires_at": time.Now().Add(time.Hour),
			"proxy_only": []string{},
		})
	})
	mux.HandleFunc("POST /v1/grants/grant-proxy/values", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"values":     map[string]string{},
			"expires_at": time.Now().Add(time.Hour),
			"proxy_only": []string{"PROXY_ME"},
		})
	})
	mux.HandleFunc("POST /v1/grants/grant-nonewline/values", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"values":     map[string]string{"NONEWLINE_ME": "irrelevant"},
			"expires_at": time.Now().Add(time.Hour),
			"proxy_only": []string{},
		})
	})
	mux.HandleFunc("GET /v1/enrollments/self", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"enrollment_id": testEnrollmentID, "kind": "box", "operator": "sjawhar",
			"lease_expires_at": time.Now().Add(time.Hour), "grants": []any{},
		})
	})
	srv := httptest.NewServer(mux)
	audience = srv.URL
	return srv, counters
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// newKeyDir writes a real ECDSA key.pem (proof.Sign needs a genuine key to sign with, even
// though fakeBroker never verifies the signature) and an enrollment file naming
// testEnrollmentID — the two files AGENT_SECRETS_KEY_DIR holds for every session-authenticated
// subcommand.
func newKeyDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	key, err := proof.NewKey()
	if err != nil {
		t.Fatalf("proof.NewKey: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(filepath.Join(dir, "key.pem"), pemBytes, 0o600); err != nil {
		t.Fatalf("write key.pem: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "enrollment"), []byte(testEnrollmentID+"\n"), 0o600); err != nil {
		t.Fatalf("write enrollment: %v", err)
	}
	return dir
}

// runAgentSecrets runs the built binary against broker and keyDir, returning its separate
// stdout/stderr and exit code (0 for a clean exit). extraEnv, when non-nil, is appended after the
// fixed AGENT_SECRETS_* variables, so a test can set an inherited variable the CLI must not let
// leak into (or shadow) a granted secret.
func runAgentSecrets(t *testing.T, binary, broker, keyDir string, extraEnv []string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Env = append(append(os.Environ(), "AGENT_SECRETS_URL="+broker, "AGENT_SECRETS_KEY_DIR="+keyDir), extraEnv...)
	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	if err == nil {
		return out.String(), errOut.String(), 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return out.String(), errOut.String(), exitErr.ExitCode()
	}
	t.Fatalf("run agent-secrets %v: %v\nstdout: %s\nstderr: %s", args, err, out.String(), errOut.String())
	return "", "", -1
}

// oneJSONObject parses stdout as exactly one JSON object (one line, nothing else) and returns
// its top-level key set, for asserting --json prints "the contract's response body verbatim ...
// one JSON object on stdout and nothing else".
func oneJSONObject(t *testing.T, stdout string) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout has %d lines, want exactly 1: %q", len(lines), stdout)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &obj); err != nil {
		t.Fatalf("stdout is not one JSON object: %v: %q", err, stdout)
	}
	return obj
}

func keySet(m map[string]any) map[string]bool {
	keys := make(map[string]bool, len(m))
	for k := range m {
		keys[k] = true
	}
	return keys
}

func mapsEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// redactSecrets replaces the granted value these tests' fake broker hands out with a placeholder
// before it is used in a t.Fatalf diagnostic. Comparisons and control flow always use the real
// value; this project's rule against a secret ever reaching logs, audit, or error messages
// carries no test-only carve-out, so nothing this suite prints on failure may contain one either,
// even a fake, fixture-only value.
func redactSecrets(s string) string {
	return strings.ReplaceAll(s, "topsecretvalue123", "[REDACTED]")
}

func TestExecFormGrantRunsChildWithValueInEnvironment(t *testing.T) {
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)

	stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil,
		"GRANT_ME", "--", "sh", "-c", `echo -n "$GRANT_ME" | sha256sum`)
	if exit != 0 {
		t.Fatalf("exit = %d, want 0: stdout=%q stderr=%q", exit, redactSecrets(stdout), redactSecrets(stderr))
	}
	sum := sha256.Sum256([]byte("topsecretvalue123"))
	want := hex.EncodeToString(sum[:])
	fields := strings.Fields(stdout)
	if len(fields) == 0 || fields[0] != want {
		t.Fatalf("child sha256 = %q, want %q (stdout=%q)", redactSecrets(strings.Join(fields, " ")), want, redactSecrets(stdout))
	}
	if strings.Contains(stdout, "topsecretvalue123") {
		t.Fatalf("raw secret value leaked into stdout: %q", redactSecrets(stdout))
	}
}

func TestExecFormPendingExitsSeventyFiveWithNoChild(t *testing.T) {
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)

	stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil,
		"PENDING_ME", "--wait", "200ms", "--", "sh", "-c", "echo ran-the-child")
	if exit != exitPending {
		t.Fatalf("exit = %d, want %d (pending): stdout=%q stderr=%q", exit, exitPending, stdout, stderr)
	}
	if strings.Contains(stdout, "ran-the-child") {
		t.Fatalf("child ran while request was still pending: stdout=%q", stdout)
	}
	if !strings.Contains(stderr, "agent-secrets status req-pending") {
		t.Fatalf("stderr = %q, want the request id and the status command to check it", stderr)
	}
}

func TestExecFormDeniedExitsSeventySevenWithNoChild(t *testing.T) {
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)

	stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil,
		"DENY_ME", "--", "sh", "-c", "echo ran-the-child")
	if exit != exitDenied {
		t.Fatalf("exit = %d, want %d (denied): stdout=%q stderr=%q", exit, exitDenied, stdout, stderr)
	}
	if strings.Contains(stdout, "ran-the-child") {
		t.Fatalf("child ran for a denied request: stdout=%q", stdout)
	}
	if !strings.Contains(stderr, "req-denied was denied") {
		t.Fatalf("stderr = %q, want it to name the denied request", stderr)
	}
}

func TestExecFormRefusesWhenNoNameIsGiven(t *testing.T) {
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)

	stdout, _, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil, "--", "sh", "-c", "echo ran-the-child")
	if exit == 0 {
		t.Fatalf("exit = 0, want a nonzero usage error when no NAME is given")
	}
	if strings.Contains(stdout, "ran-the-child") {
		t.Fatalf("child ran with no NAME given: stdout=%q", stdout)
	}
}

// TestTopLevelHelpExitsZero pins that a bare top-level "--help" or "-h" — no subcommand — exits
// 0 and prints usage(), the conventional exit code for an explicit help request; only the
// zero-argument form (no NAME, no --help) remains a usage error exiting 2.
func TestTopLevelHelpExitsZero(t *testing.T) {
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)

	for _, flag := range []string{"--help", "-h", "help"} {
		stdout, _, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil, flag)
		if exit != 0 {
			t.Fatalf("agent-secrets %s: exit = %d, want 0", flag, exit)
		}
		if !strings.Contains(stdout, "usage:") {
			t.Fatalf("agent-secrets %s: stdout = %q, want it to contain %q", flag, stdout, "usage:")
		}
	}

	if _, _, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil); exit != exitUsageError {
		t.Fatalf("agent-secrets with no arguments: exit = %d, want %d", exit, exitUsageError)
	}
}

// TestExecFormRefusesToRunWhenAGrantedNameIsProxyOnly is the regression for the review's
// Important finding 3: a granted request whose delivery is "proxy" (or otherwise missing from
// the grant's values) must never exec — a proxy-only secret has no value for the CLI to release
// into the child's environment at all, and silently execing without it would be a silent partial
// grant.
func TestExecFormRefusesToRunWhenAGrantedNameIsProxyOnly(t *testing.T) {
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)

	stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil,
		"PROXY_ME", "--", "sh", "-c", "echo ran-the-child")
	if exit == 0 {
		t.Fatalf("exit = 0, want a nonzero refusal for a proxy-only grant: stdout=%q stderr=%q", stdout, stderr)
	}
	if exit == exitPending || exit == exitDenied {
		t.Fatalf("exit = %d, want a plain operational refusal (not 75/76/77): stdout=%q stderr=%q", exit, stdout, stderr)
	}
	if strings.Contains(stdout, "ran-the-child") {
		t.Fatalf("child ran despite a proxy-only (unreleased) secret: stdout=%q", stdout)
	}
	if !strings.Contains(stderr, "PROXY_ME") {
		t.Fatalf("stderr should name the missing secret PROXY_ME: %q", stderr)
	}
}

// TestExecFormDoesNotLetInheritedEnvShadowAGrantedValue is the regression for the review's
// Important finding 1: an inherited environment variable sharing a granted secret's exact name
// must not survive into the child's environment alongside (and potentially before) the
// broker-released value.
func TestExecFormDoesNotLetInheritedEnvShadowAGrantedValue(t *testing.T) {
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)

	stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir,
		[]string{"GRANT_ME=attacker-controlled-stale-value"},
		"GRANT_ME", "--", "sh", "-c", `echo -n "$GRANT_ME" | sha256sum`)
	if exit != 0 {
		t.Fatalf("exit = %d, want 0: stdout=%q stderr=%q", exit, redactSecrets(stdout), redactSecrets(stderr))
	}
	sum := sha256.Sum256([]byte("topsecretvalue123"))
	want := hex.EncodeToString(sum[:])
	fields := strings.Fields(stdout)
	if len(fields) == 0 || fields[0] != want {
		t.Fatalf("child GRANT_ME sha256 = %q, want %q (granted value, not the shadowing inherited one): stdout=%q", redactSecrets(strings.Join(fields, " ")), want, redactSecrets(stdout))
	}
}

// TestExecFormSecondInvocationReusesGrantTransparently proves cmd/agent-secrets needs no
// client-side change to benefit from Machine.Create's server-side grant reuse (the review's
// Critical finding; requests/machine.go): running the exec form twice for the same
// already-granted secret name must both times grant immediately, and — critically — must never
// poll GET /v1/requests/{id} (proof that neither invocation ever entered the pending-wait loop,
// whether the broker minted the grant fresh or handed back a reused one).
func TestExecFormSecondInvocationReusesGrantTransparently(t *testing.T) {
	binary := buildAgentSecrets(t)
	broker, counters := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)

	for i := 0; i < 2; i++ {
		stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil,
			"GRANT_ME", "--", "sh", "-c", `echo -n "$GRANT_ME" | sha256sum`)
		if exit != 0 {
			t.Fatalf("invocation %d: exit = %d, want 0: stdout=%q stderr=%q", i, exit, redactSecrets(stdout), redactSecrets(stderr))
		}
	}
	if got := atomic.LoadInt32(&counters.createRequest); got != 2 {
		t.Fatalf("POST /v1/requests called %d times, want 2 (one per invocation)", got)
	}
	if got := atomic.LoadInt32(&counters.getRequest); got != 0 {
		t.Fatalf("GET /v1/requests/{id} called %d times, want 0 (an immediately granted request must never be polled)", got)
	}
}

// TestExecFormNestedCallKeepsTheSessionIdentity proves a command run under the exec form belongs
// to the same session: its own `agent-secrets` call (a skill that nests a request, gh's token
// helper under `agent-secrets K -- omp`) reaches the broker as that session instead of failing
// with "AGENT_SECRETS_URL is required".
func TestExecFormNestedCallKeepsTheSessionIdentity(t *testing.T) {
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)

	// No default key dir or helper socket to fall back to: the inner call can reach the broker
	// only through the variables the outer one kept for it.
	env := []string{"BIN=" + binary, "XDG_RUNTIME_DIR=" + t.TempDir(), "AGENT_SECRETS_HELPER_SOCK="}
	stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, env,
		"GRANT_ME", "--", "sh", "-c", `"$BIN" NONEWLINE_ME -- sh -c "printf %s \"\$NONEWLINE_ME\""`)
	if exit != 0 {
		t.Fatalf("exit = %d, want 0: stdout=%q stderr=%q", exit, redactSecrets(stdout), redactSecrets(stderr))
	}
	if stdout != "irrelevant" {
		t.Fatalf("the nested call's child printed %q, want the value the broker released to it", redactSecrets(stdout))
	}
}

// TestBuildChildEnvKeepsOnlyTheSessionIdentity pins which of this CLI's variables the exec'd
// child inherits: exactly the three that say which session it is (the broker's URL, the helper
// socket, the box's key dir), never any other AGENT_SECRETS_* setting, and never an inherited
// copy of a released name beside the released value.
func TestBuildChildEnvKeepsOnlyTheSessionIdentity(t *testing.T) {
	environ := []string{
		"PATH=/usr/bin",
		"AGENT_SECRETS_URL=https://broker.internal.example",
		"AGENT_SECRETS_HELPER_SOCK=/run/user/1000/agent-secrets/helper.sock",
		"AGENT_SECRETS_KEY_DIR=/run/user/1000/agent-secrets",
		"AGENT_SECRETS_WAIT=5m",
		"AGENT_SECRETS_ENROLL_WAIT=3s",
		"AGENT_SECRETS_APPROVE_URL=https://dispatch.internal.example",
		"GRANT_ME=inherited-stale-value",
		"NOT_AN_ASSIGNMENT",
	}
	got := buildChildEnv(environ, map[string]string{"GRANT_ME": "released"})
	want := []string{
		"PATH=/usr/bin",
		"AGENT_SECRETS_URL=https://broker.internal.example",
		"AGENT_SECRETS_HELPER_SOCK=/run/user/1000/agent-secrets/helper.sock",
		"AGENT_SECRETS_KEY_DIR=/run/user/1000/agent-secrets",
		"GRANT_ME=released",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("child env:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// runExecForm runs the exec form of GRANT_ME against broker from keyDir with no helper to fall
// back to (the runtime dir is empty and AGENT_SECRETS_HELPER_SOCK is empty), returning its exit
// code, stderr and how long it took. extraEnv comes last, so it can override any of that.
func runExecForm(t *testing.T, binary, broker, keyDir string, extraEnv ...string) (int, string, time.Duration) {
	t.Helper()
	env := append([]string{"AGENT_SECRETS_HELPER_SOCK=", "XDG_RUNTIME_DIR=" + t.TempDir()}, extraEnv...)
	start := time.Now()
	_, stderr, exit := runAgentSecrets(t, binary, broker, keyDir, env, "GRANT_ME", "--", "sh", "-c", "exit 0")
	return exit, stderr, time.Since(start)
}

// writePendingMarker writes the launcher's enrollment.pending into dir (created if absent), aged
// by age.
func writePendingMarker(t *testing.T, dir string, age time.Duration) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "enrollment.pending")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	then := time.Now().Add(-age)
	if err := os.Chtimes(marker, then, then); err != nil {
		t.Fatal(err)
	}
	return marker
}

// TestEnrollWaitRidesOutTheLaunchersSetup is the box's enrollment race: omp starts its MCP
// servers, and they call agent-secrets, while the launcher is still generating the box's key and
// enrolling it. While the launcher's enrollment.pending marker is there, a call waits for key.pem
// and enrollment to appear instead of failing at once.
func TestEnrollWaitRidesOutTheLaunchersSetup(t *testing.T) {
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	fixture := newKeyDir(t)
	keyDir := t.TempDir()
	marker := writePendingMarker(t, keyDir, 0)

	setupDone := make(chan error, 1)
	go func() {
		time.Sleep(time.Second)
		data, err := os.ReadFile(filepath.Join(fixture, "key.pem"))
		if err == nil {
			err = os.WriteFile(filepath.Join(keyDir, "key.pem"), data, 0o600)
		}
		time.Sleep(time.Second)
		if err == nil {
			err = os.WriteFile(filepath.Join(keyDir, "enrollment"), []byte(testEnrollmentID+"\n"), 0o600)
		}
		if err == nil {
			err = os.Remove(marker)
		}
		setupDone <- err
	}()

	exit, stderr, elapsed := runExecForm(t, binary, broker.URL, keyDir)
	if err := <-setupDone; err != nil {
		t.Fatal(err)
	}
	if exit != 0 {
		t.Fatalf("exit = %d after %s, want 0 once the launcher finished: stderr=%q", exit, elapsed, stderr)
	}
	if elapsed < 2*time.Second {
		t.Fatalf("finished in %s, before the launcher wrote the enrollment", elapsed)
	}
}

// TestEnrollWaitGivesUpAtItsBound pins the wait's end: a marker nothing ever follows (a launcher
// stuck without removing it) holds a call for AGENT_SECRETS_ENROLL_WAIT at most, and the call then
// fails with the error it would have failed with at once.
func TestEnrollWaitGivesUpAtItsBound(t *testing.T) {
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	keyDir := t.TempDir()
	writePendingMarker(t, keyDir, 0)

	exit, stderr, elapsed := runExecForm(t, binary, broker.URL, keyDir, "AGENT_SECRETS_ENROLL_WAIT=1s")
	if exit == 0 || !strings.Contains(stderr, "no session identity") {
		t.Fatalf("exit = %d, stderr = %q, want the no-identity failure", exit, stderr)
	}
	if elapsed < 900*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("gave up after %s, want about the 1s bound", elapsed)
	}
}

// TestEnrollWaitNeedsAFreshMarker pins that only the launcher's live marker makes a call wait: an
// empty key dir (a host session's, a pod's), a marker older than the launcher's own setup bound
// (a launcher killed outright), and a failed enrollment the launcher has already reported
// (enrollment.error, marker removed) all fail at once, as they did before the wait existed.
func TestEnrollWaitNeedsAFreshMarker(t *testing.T) {
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()

	stale := t.TempDir()
	writePendingMarker(t, stale, 200*time.Second)
	failed := newKeyDir(t)
	if err := os.Remove(filepath.Join(failed, "enrollment")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(failed, "enrollment.error"), []byte("broker 503 DATABASE: postgres unreachable\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, keyDir, want string
	}{
		{"empty key dir", t.TempDir(), "no session identity"},
		{"stale marker", stale, "no session identity"},
		{"reported failure", failed, "postgres unreachable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exit, stderr, elapsed := runExecForm(t, binary, broker.URL, tc.keyDir)
			if exit == 0 || !strings.Contains(stderr, tc.want) {
				t.Fatalf("exit = %d, stderr = %q, want a failure naming %q", exit, stderr, tc.want)
			}
			if elapsed > 10*time.Second {
				t.Fatalf("took %s; with no fresh marker the call must not wait", elapsed)
			}
		})
	}
}

// TestEnrollWaitRefusesAnInvalidBound pins that a malformed AGENT_SECRETS_ENROLL_WAIT is an
// error naming the variable, never a silent fall back to the default.
func TestEnrollWaitRefusesAnInvalidBound(t *testing.T) {
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	for _, bad := range []string{"soon", "-1s"} {
		exit, stderr, _ := runExecForm(t, binary, broker.URL, newKeyDir(t), "AGENT_SECRETS_ENROLL_WAIT="+bad)
		if exit == 0 || !strings.Contains(stderr, "AGENT_SECRETS_ENROLL_WAIT") {
			t.Fatalf("AGENT_SECRETS_ENROLL_WAIT=%s: exit = %d, stderr = %q, want a refusal naming the variable", bad, exit, stderr)
		}
	}
}

func TestRequestJSONPrintsExactlyOneContractObject(t *testing.T) {
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)

	stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil, "request", "GRANT_ME", "--json")
	if exit != 0 {
		t.Fatalf("exit = %d, want 0: stdout=%q stderr=%q", exit, stdout, stderr)
	}
	obj := oneJSONObject(t, stdout)
	want := map[string]bool{"request_id": true, "state": true, "secrets": true, "grant_id": true, "record_id": true}
	if got := keySet(obj); !mapsEqual(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	if obj["request_id"] != "req-granted" || obj["state"] != "granted" {
		t.Fatalf("request/state = %v/%v, want req-granted/granted", obj["request_id"], obj["state"])
	}
}

// TestRequestJSONIsByteIdenticalToTheBrokerResponse is the regression for the review's Important
// finding 2: --json must print the broker's exact bytes, never appending a newline (or anything
// else) the response didn't already end with.
func TestRequestJSONIsByteIdenticalToTheBrokerResponse(t *testing.T) {
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)

	const wantExact = `{"request_id":"req-nonewline","state":"granted","secrets":[{"name":"NONEWLINE_ME","decision":"automatic","delivery":"inject"}],"grant_id":"grant-nonewline","record_id":null}`
	stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil, "request", "NONEWLINE_ME", "--json")
	if exit != 0 {
		t.Fatalf("exit = %d, want 0: stdout=%q stderr=%q", exit, stdout, stderr)
	}
	if stdout != wantExact {
		t.Fatalf("stdout = %q, want byte-identical to the broker's exact response %q", stdout, wantExact)
	}
}

func TestSelfJSONPrintsExactlyOneContractObjectNamingTheIssuedEnrollment(t *testing.T) {
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)

	stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil, "self", "--json")
	if exit != 0 {
		t.Fatalf("exit = %d, want 0: stdout=%q stderr=%q", exit, stdout, stderr)
	}
	obj := oneJSONObject(t, stdout)
	want := map[string]bool{"enrollment_id": true, "kind": true, "operator": true, "lease_expires_at": true, "grants": true}
	if got := keySet(obj); !mapsEqual(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	if obj["enrollment_id"] != testEnrollmentID {
		t.Fatalf("enrollment_id = %v, want %q (the id the fake issued)", obj["enrollment_id"], testEnrollmentID)
	}
}

// TestRequestSignsARequestObject pins the shared broker contract's request shape: POST /v1/requests
// posts a signed request object (record.Sign, via Signer.SignRequestObject) instead of plain
// top-level "secrets"/"reason"/"issue" fields. The fake broker asserts the body is exactly
// {"request": <jws>, "session_id": null}, verifies the JWS with record.VerifyRequestObject
// against its own URL as audience, and checks the request object's authorization_details name
// exactly the argv secret names with the reason carried inside the signed object rather than as
// a top-level field.
func TestRequestSignsARequestObject(t *testing.T) {
	binary := buildAgentSecrets(t)
	keyDir := newKeyDir(t)

	var mu sync.Mutex
	var gotBody map[string]any
	var gotObj record.RequestObject
	var verifyErr error
	var audience string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/requests", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		aud := audience
		mu.Unlock()
		compact, _ := body["request"].(string)
		obj, verr := record.VerifyRequestObject(compact, aud, time.Minute, time.Now())
		mu.Lock()
		gotBody, gotObj, verifyErr = body, obj, verr
		mu.Unlock()
		writeJSON(w, map[string]any{
			"request_id": "req-signed", "state": "granted",
			"secrets":  []map[string]string{{"name": "GRANT_ME", "decision": "automatic", "delivery": "inject"}},
			"grant_id": "grant-signed", "record_id": nil,
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mu.Lock()
	audience = srv.URL
	mu.Unlock()

	// OMP_SESSION_ID is cleared: this test's own process may run inside an omp session, whose id
	// the client would otherwise forward as session_id (the behavior
	// TestRequestAndExecFormSendOMPSessionIDAsSessionID pins).
	stdout, stderr, exit := runAgentSecrets(t, binary, srv.URL, keyDir, []string{"OMP_SESSION_ID="},
		"request", "GRANT_ME", "--reason", "need it for the build")
	if exit != 0 {
		t.Fatalf("exit = %d, want 0: stdout=%q stderr=%q", exit, stdout, stderr)
	}
	mu.Lock()
	defer mu.Unlock()
	sessionID, present := gotBody["session_id"]
	if !present || sessionID != nil {
		t.Fatalf(`request body["session_id"] = %v (present=%v), want a present null`, sessionID, present)
	}
	if verifyErr != nil {
		t.Fatalf("record.VerifyRequestObject(request): %v", verifyErr)
	}
	if len(gotObj.Details) != 1 || gotObj.Details[0].Type != "agent_secret" || gotObj.Details[0].Identifier != "GRANT_ME" {
		t.Fatalf("authorization_details = %+v, want exactly one agent_secret detail naming GRANT_ME", gotObj.Details)
	}
	if gotObj.Reason != "need it for the build" {
		t.Fatalf("reason = %q, want the --reason text", gotObj.Reason)
	}
}

// fakeLoginHelper serves just the launcher-login socket ops (login, login-status) a bare unix
// listener needs to drive cmdLauncher's own poll loop, letting each test's states slice fully
// drive the poll loop through however many pending answers it wants before a terminal state.
func fakeLoginHelper(t *testing.T, code string, states []string) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "h.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var calls int32
	go func() {
		for {
			conn, err := ln.AcceptUnix()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadBytes('\n')
			var req helper.Request
			_ = json.Unmarshal(line, &req)
			var resp helper.Response
			switch req.Op {
			case "login":
				resp = helper.Response{OK: true, Code: code, LoginState: "pending"}
			case "login-status":
				idx := int(atomic.AddInt32(&calls, 1)) - 1
				state := states[len(states)-1]
				if idx < len(states) {
					state = states[idx]
				}
				resp = helper.Response{OK: true, Code: code, LoginState: state}
			}
			data, _ := json.Marshal(resp)
			_, _ = conn.Write(append(data, '\n'))
			conn.Close()
		}
	}()
	return sock
}

// TestLauncherLoginPrintsTheCodeAndWaits pins the socket-based launcher login: it prints the
// confirmation code and, since AGENT_SECRETS_APPROVE_URL is unset, the generic Dispatch-page
// line, then polls login-status through two pending answers before exiting 0 on "issued".
func TestLauncherLoginPrintsTheCodeAndWaits(t *testing.T) {
	t.Setenv("AGENT_SECRETS_APPROVE_URL", "")
	binary := buildAgentSecrets(t)
	sock := fakeLoginHelper(t, "KQ7M-X4PZ", []string{"pending", "pending", "issued"})
	stdout, stderr, exit := runAgentSecrets(t, binary, "http://unused", t.TempDir(),
		[]string{"AGENT_SECRETS_HELPER_SOCK=" + sock}, "launcher", "login")
	if exit != 0 {
		t.Fatalf("exit = %d, want 0: stdout=%q stderr=%q", exit, stdout, stderr)
	}
	want := "machine login code: KQ7M-X4PZ\nenter it on the Dispatch credential page\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

// TestLauncherLoginPrintsTheDispatchURLWhenApproveURLIsSet pins the other half of the two-line
// contract: with AGENT_SECRETS_APPROVE_URL set, the second line names it instead of the generic
// "Dispatch credential page" fallback.
func TestLauncherLoginPrintsTheDispatchURLWhenApproveURLIsSet(t *testing.T) {
	binary := buildAgentSecrets(t)
	sock := fakeLoginHelper(t, "KQ7M-X4PZ", []string{"issued"})
	stdout, stderr, exit := runAgentSecrets(t, binary, "http://unused", t.TempDir(),
		[]string{"AGENT_SECRETS_HELPER_SOCK=" + sock, "AGENT_SECRETS_APPROVE_URL=https://dispatch.example/"},
		"launcher", "login")
	if exit != 0 {
		t.Fatalf("exit = %d, want 0: stdout=%q stderr=%q", exit, stdout, stderr)
	}
	want := "machine login code: KQ7M-X4PZ\nenter it at https://dispatch.example/credentials/machine — approve only if the code matches this terminal\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

// TestLauncherLoginExitsOneOnDenied pins the terminal-failure half of the poll loop: a
// login-status answer of "denied" exits 1 and names the state, never retrying past it.
func TestLauncherLoginExitsOneOnDenied(t *testing.T) {
	binary := buildAgentSecrets(t)
	sock := fakeLoginHelper(t, "KQ7M-X4PZ", []string{"denied"})
	_, stderr, exit := runAgentSecrets(t, binary, "http://unused", t.TempDir(),
		[]string{"AGENT_SECRETS_HELPER_SOCK=" + sock}, "launcher", "login")
	if exit != 1 {
		t.Fatalf("exit = %d, want 1: stderr=%q", exit, stderr)
	}
	if !strings.Contains(stderr, "denied") {
		t.Fatalf("stderr = %q, want it to name the denied state", stderr)
	}
}

// TestLauncherLoginStatusExitsZeroOnlyWhileACredentialIsHeld pins login-status's read-only,
// single-shot contract (AGENTC-834): it prints a bare state on stdout and its exit code is a
// liveness probe — 0, printing "issued", while the helper holds a launcher credential, and 1 for
// every login state with none, "none" when login was never run (empty LoginState) — with a single
// helper call, never login's mint-a-fresh-key-and-poll side effect. A re-login still pending or
// expired unapproved beside a held credential prints "issued" and names that login on stderr; a
// helper from before credential_held sends "issued" alone exactly while it holds one. Every state
// with no credential and no login in flight (never logged in, denied, or expired, which is also
// what a credential the broker rejected becomes) says on stderr to run the login again, and a
// login in flight outranks a refused credential.
func TestLauncherLoginStatusExitsZeroOnlyWhileACredentialIsHeld(t *testing.T) {
	binary := buildAgentSecrets(t)
	const held = "the helper still holds the launcher credential an earlier login issued"
	for _, tc := range []struct {
		name   string
		resp   helper.Response
		want   string
		exit   int
		remedy bool
		notice string
	}{
		{name: "issued", resp: helper.Response{LoginState: "issued"}, want: "issued\n"},
		{name: "pending", resp: helper.Response{LoginState: "pending"}, want: "pending\n", exit: 1},
		{name: "denied", resp: helper.Response{LoginState: "denied"}, want: "denied\n", exit: 1, remedy: true},
		{name: "expired", resp: helper.Response{LoginState: "expired"}, want: "expired\n", exit: 1, remedy: true},
		{name: "none", resp: helper.Response{}, want: "none\n", exit: 1, remedy: true},
		{name: "pending beside a refused credential", resp: helper.Response{LoginState: "pending", LoginRefused: true}, want: "pending\n", exit: 1},
		{name: "pending beside a held credential", resp: helper.Response{LoginState: "pending", CredentialHeld: true}, want: "issued\n",
			notice: "a machine login is waiting for approval (code KQ7M-X4PZ); " + held},
		{name: "expired beside a held credential", resp: helper.Response{LoginState: "expired", CredentialHeld: true}, want: "issued\n",
			notice: "the most recent machine login (code KQ7M-X4PZ) expired before anyone approved it; " + held},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.resp.OK, tc.resp.Code = true, "KQ7M-X4PZ"
			sock, reqs := fakeHelper(t, tc.resp)
			stdout, stderr, exit := runAgentSecrets(t, binary, "http://unused", t.TempDir(),
				[]string{"AGENT_SECRETS_HELPER_SOCK=" + sock}, "launcher", "login-status")
			if exit != tc.exit || stdout != tc.want {
				t.Fatalf("exit = %d stdout = %q, want exit %d stdout %q (stderr=%q)", exit, stdout, tc.exit, tc.want, stderr)
			}
			if got := strings.Contains(stderr, "run: agent-secrets launcher login"); got != tc.remedy {
				t.Fatalf("stderr = %q, want the login remedy: %v", stderr, tc.remedy)
			}
			if tc.notice != "" && stderr != "agent-secrets launcher login-status: "+tc.notice+"\n" {
				t.Fatalf("stderr = %q, want %q", stderr, tc.notice)
			}
			if req := <-reqs; req.Op != "login-status" || len(reqs) != 0 {
				t.Fatalf("helper calls: first %q, %d more; want one login-status", req.Op, len(reqs))
			}
		})
	}
}

// TestEnrollHelperPrintsEnrollmentID pins enroll --helper's box contract: it asks the local
// agent-secrets-helper over its unix socket (never the broker directly), writes the returned
// enrollment id into AGENT_SECRETS_KEY_DIR/enrollment exactly as file mode's own buildSigner
// reads it back, and prints the enrollment id alone on stdout — the contract scripts/agentbox
// parses.
func TestEnrollHelperPrintsEnrollmentID(t *testing.T) {
	binary := buildAgentSecrets(t)
	sock, _ := fakeHelper(t, helper.Response{OK: true, EnrollmentID: "enr-box-1", LeaseExpires: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)})
	keyDir := t.TempDir()
	stdout, stderr, exit := runAgentSecrets(t, binary, "http://unused", keyDir,
		[]string{"AGENT_SECRETS_HELPER_SOCK=" + sock},
		"enroll", "--helper", "--kind", "box", "--runtime-id", "box-1", "--thumbprint", "tp-box")
	if exit != 0 {
		t.Fatalf("enroll --helper exit = %d: stdout=%q stderr=%q", exit, stdout, stderr)
	}
	if stdout != "enr-box-1\n" {
		t.Fatalf("stdout = %q, want the bare enrollment id", stdout)
	}
	data, err := os.ReadFile(filepath.Join(keyDir, "enrollment"))
	if err != nil || strings.TrimSpace(string(data)) != "enr-box-1" {
		t.Fatalf("enrollment file: err=%v content=%q", err, string(data))
	}
}

// TestUnenrollHelperSucceeds pins unenroll --helper's exit-code-only contract: it asks the local
// helper to revoke the enrollment over its unix socket and prints nothing on success.
func TestUnenrollHelperSucceeds(t *testing.T) {
	binary := buildAgentSecrets(t)
	sock, _ := fakeHelper(t, helper.Response{OK: true})
	stdout, stderr, exit := runAgentSecrets(t, binary, "http://unused", t.TempDir(),
		[]string{"AGENT_SECRETS_HELPER_SOCK=" + sock}, "unenroll", "--helper", "--enrollment", "enr-box-1")
	if exit != 0 {
		t.Fatalf("unenroll --helper exit = %d: stdout=%q stderr=%q", exit, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want nothing on success", stdout)
	}
}

// TestEnrollHelperFlagValidation pins enroll/unenroll's remaining validation now that contract
// v9 dropped launcher bearer tokens entirely: --helper is the only enrollment path left, so it
// is required (not merely one of two mutually exclusive options), and enroll --helper still
// supports only --kind box.
func TestEnrollHelperFlagValidation(t *testing.T) {
	binary := buildAgentSecrets(t)
	keyDir := t.TempDir()

	if _, stderr, exit := runAgentSecrets(t, binary, "http://unused", keyDir, nil,
		"enroll", "--kind", "box", "--runtime-id", "box-1", "--thumbprint", "tp-box"); exit != exitUsageError {
		t.Fatalf("enroll without --helper exit = %d, want a usage error: %s", exit, stderr)
	}
	if _, stderr, exit := runAgentSecrets(t, binary, "http://unused", keyDir, nil,
		"enroll", "--helper", "--kind", "host", "--runtime-id", "box-1", "--thumbprint", "tp-box"); exit != exitUsageError {
		t.Fatalf("enroll --helper --kind host exit = %d, want a usage error naming box: %s", exit, stderr)
	}
	if _, stderr, exit := runAgentSecrets(t, binary, "http://unused", keyDir, nil,
		"unenroll", "--enrollment", "enr-1"); exit != exitUsageError {
		t.Fatalf("unenroll without --helper exit = %d, want a usage error: %s", exit, stderr)
	}
}

// TestRequestAndExecFormSendOMPSessionIDAsSessionID is the regression for the review's finding
// I3: POST /v1/requests carries session_id so the broker can wake this host session directly
// once its request is decided (host enrollments carry no session_id of their own) — both call
// sites, cmdRequest and cmdExec, must forward OMP_SESSION_ID rather than leaving it "".
func TestRequestAndExecFormSendOMPSessionIDAsSessionID(t *testing.T) {
	binary := buildAgentSecrets(t)
	broker, counters := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)

	stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, []string{"OMP_SESSION_ID=sess-request-123"}, "request", "GRANT_ME")
	if exit != 0 {
		t.Fatalf("exit = %d, want 0: stdout=%q stderr=%q", exit, stdout, stderr)
	}
	if got := counters.sessionID(); got != "sess-request-123" {
		t.Fatalf("request form session_id = %q, want %q", got, "sess-request-123")
	}

	stdout, stderr, exit = runAgentSecrets(t, binary, broker.URL, keyDir, []string{"OMP_SESSION_ID=sess-exec-456"}, "GRANT_ME", "--", "true")
	if exit != 0 {
		t.Fatalf("exit = %d, want 0: stdout=%q stderr=%q", exit, stdout, stderr)
	}
	if got := counters.sessionID(); got != "sess-exec-456" {
		t.Fatalf("exec form session_id = %q, want %q", got, "sess-exec-456")
	}
}
