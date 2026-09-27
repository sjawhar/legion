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
// final observable outcome.
type brokerCounters struct {
	createRequest int32
	getRequest    int32
}

// fakeBroker serves just enough of the AGENTC-393 contract (v9) for the exec-form and --json
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

// TestRequestSignsARequestObject pins contract v9's core wire-shape change: POST /v1/requests
// posts a signed request object (record.Sign) instead of plain top-level "secrets"/"reason"/
// "issue" fields. The fake broker asserts the body is exactly {"request": <jws>, "session_id":
// null}, verifies the JWS with record.VerifyRequestObject against its own URL as audience, and
// checks the request object's authorization_details name exactly the argv secret names with the
// reason carried inside the signed object rather than as a top-level field.
func TestRequestSignsARequestObject(t *testing.T) {
	binary := buildAgentSecrets(t)
	keyDir := newKeyDir(t)

	var gotBody map[string]any
	var gotObj record.RequestObject
	var verifyErr error
	var audience string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/requests", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		compact, _ := gotBody["request"].(string)
		gotObj, verifyErr = record.VerifyRequestObject(compact, audience, time.Minute, time.Now())
		writeJSON(w, map[string]any{
			"request_id": "req-signed", "state": "granted",
			"secrets":  []map[string]string{{"name": "GRANT_ME", "decision": "automatic", "delivery": "inject"}},
			"grant_id": "grant-signed", "record_id": nil,
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	audience = srv.URL

	stdout, stderr, exit := runAgentSecrets(t, binary, srv.URL, keyDir, nil,
		"request", "GRANT_ME", "--reason", "need it for the build")
	if exit != 0 {
		t.Fatalf("exit = %d, want 0: stdout=%q stderr=%q", exit, stdout, stderr)
	}
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

// TestLauncherSubcommandIsGone pins that "launcher login" is refused outright: Plan B's
// helper-socket command replaces it, and this CLI holds no bearer launcher token to offer.
func TestLauncherSubcommandIsGone(t *testing.T) {
	binary := buildAgentSecrets(t)
	keyDir := newKeyDir(t)

	stdout, stderr, exit := runAgentSecrets(t, binary, "http://unused.invalid", keyDir, nil,
		"launcher", "login", "--operator", "sjawhar", "--host", "devbox", "--out", filepath.Join(t.TempDir(), "token"))
	if exit != exitUsageError {
		t.Fatalf("agent-secrets launcher login: exit = %d, want %d (usage error): stdout=%q stderr=%q", exit, exitUsageError, stdout, stderr)
	}
	if !strings.Contains(stderr, "Plan B") {
		t.Fatalf("stderr = %q, want it to name Plan B's helper-socket replacement", stderr)
	}
}

// TestEnrollWithLauncherTokenFileIsGone pins that "enroll --launcher-token-file ..." no longer
// enrolls anything: with no "enroll" case left in run()'s dispatch table, it falls through to
// the exec form, which requires a "--" command separator this invocation never supplies, so it
// refuses as a usage error rather than silently trying to run "--launcher-token-file" as a
// secret name.
func TestEnrollWithLauncherTokenFileIsGone(t *testing.T) {
	binary := buildAgentSecrets(t)
	keyDir := newKeyDir(t)
	tokenFile := filepath.Join(t.TempDir(), "launcher-token")
	if err := os.WriteFile(tokenFile, []byte("launcher-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, exit := runAgentSecrets(t, binary, "http://unused.invalid", keyDir, nil,
		"enroll", "--launcher-token-file", tokenFile, "--kind", "box", "--runtime-id", "box-1",
		"--operator", "sjawhar", "--thumbprint", "tp-1")
	if exit != exitUsageError {
		t.Fatalf("agent-secrets enroll: exit = %d, want %d (usage error, no enroll subcommand remains): stdout=%q stderr=%q", exit, exitUsageError, stdout, stderr)
	}
}
