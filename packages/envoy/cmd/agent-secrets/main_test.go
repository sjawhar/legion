// packages/envoy/cmd/agent-secrets/main_test.go
//
// These tests build the real agent-secrets binary and run it as a subprocess against a fake
// broker: the "NAME... -- <command>" form ends in syscall.Exec, which replaces the calling
// process image, so it cannot be exercised in-process the way an ordinary function call can.
package main

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/proof"
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
// final observable outcome.
type brokerCounters struct {
	createRequest int32
	getRequest    int32
}

// fakeBroker serves just enough of the AGENTC-393 contract for the exec-form and --json tests:
// POST /v1/requests routes on the requested secret name to a canned granted/pending/denied/
// proxy-only/no-trailing-newline response, GET /v1/requests/{id} answers the pending case's own
// request id with the same never-resolving pending state (and 404s any other id, since a
// granted-immediately response must never be polled), POST /v1/grants/{id}/values releases one
// canned value (or, for the proxy-only case, none at all), and GET /v1/enrollments/self echoes
// testEnrollmentID. It does not verify the Proof header or launcher bearer at all —
// proof.Verifier's own behavior is covered by internal/broker/proof and internal/broker/api's
// test suites, not this package's.
func fakeBroker(t *testing.T) (*httptest.Server, *brokerCounters) {
	t.Helper()
	counters := &brokerCounters{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/requests", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&counters.createRequest, 1)
		var body struct {
			Secrets []string `json:"secrets"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Secrets) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch name := body.Secrets[0]; name {
		case "GRANT_ME":
			writeJSON(w, map[string]any{
				"request_id": "req-granted", "state": "granted",
				"secrets":  []map[string]string{{"name": name, "decision": "automatic", "delivery": "inject"}},
				"grant_id": "grant-granted", "ask": nil,
			})
		case "PENDING_ME":
			writeJSON(w, map[string]any{
				"request_id": "req-pending", "state": "pending",
				"secrets":  []map[string]string{{"name": name, "decision": "approval", "delivery": "inject"}},
				"grant_id": nil, "ask": "dispatch://AGENTC-1/ask/1",
			})
		case "DENY_ME":
			writeJSON(w, map[string]any{
				"request_id": "req-denied", "state": "denied",
				"secrets":  []map[string]string{{"name": name, "decision": "deny", "delivery": "inject"}},
				"grant_id": nil, "ask": nil,
			})
		case "PROXY_ME":
			writeJSON(w, map[string]any{
				"request_id": "req-proxy", "state": "granted",
				"secrets":  []map[string]string{{"name": name, "decision": "automatic", "delivery": "proxy"}},
				"grant_id": "grant-proxy", "ask": nil,
			})
		case "NONEWLINE_ME":
			// Written with http.ResponseWriter.Write directly, with NO trailing newline, unlike
			// every other case (which goes through writeJSON's json.Encoder, always "\n"
			// terminated) — this is the fixture for the writeVerbatim byte-exact test.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"request_id":"req-nonewline","state":"granted","secrets":[{"name":"NONEWLINE_ME","decision":"automatic","delivery":"inject"}],"grant_id":"grant-nonewline","ask":null}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	mux.HandleFunc("GET /v1/requests/{id}", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&counters.getRequest, 1)
		if r.PathValue("id") != "req-pending" {
			// A request the fake granted immediately must never be polled; failing loudly here
			// (rather than serving it) is the reuse-transparency test's proof that cmdExec does
			// not enter its pending-wait loop for an already-granted response.
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]any{"state": "pending", "grant_id": nil, "decided_at": nil, "decision": nil})
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
	return httptest.NewServer(mux), counters
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
// Critical finding, fixed in requests/machine.go): running the exec form twice for the same
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
	want := map[string]bool{"request_id": true, "state": true, "secrets": true, "grant_id": true, "ask": true}
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

	const wantExact = `{"request_id":"req-nonewline","state":"granted","secrets":[{"name":"NONEWLINE_ME","decision":"automatic","delivery":"inject"}],"grant_id":"grant-nonewline","ask":null}`
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
