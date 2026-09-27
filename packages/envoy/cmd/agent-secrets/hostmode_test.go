// packages/envoy/cmd/agent-secrets/hostmode_test.go
//
// These tests cover the Signer abstraction (buildSigner, fileSigner, helperSigner) and the three
// host-mode subcommands: register, whoami, and sign. Everything that doesn't exec a child runs
// in-process (this file is package main, same as main_test.go); register --exec must run in a
// subprocess, exactly as main_test.go's exec-form tests do, because it replaces the process
// image.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/helper"
	"github.com/sjawhar/envoy/internal/broker/proof"
)

// TestBuildSignerSelectsMode covers the Signer abstraction's mode selection: file mode when
// AGENT_SECRETS_KEY_DIR has a readable key.pem, helper mode when it doesn't but
// AGENT_SECRETS_HELPER_SOCK is set, and an error naming both variables when neither is usable.
func TestBuildSignerSelectsMode(t *testing.T) {
	t.Setenv("AGENT_SECRETS_KEY_DIR", "")
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", "")
	if _, _, err := buildSigner(); err == nil ||
		!strings.Contains(err.Error(), "AGENT_SECRETS_KEY_DIR") ||
		!strings.Contains(err.Error(), "AGENT_SECRETS_HELPER_SOCK") {
		t.Fatalf("no identity must name both variables: %v", err)
	}

	emptyDir := t.TempDir()
	t.Setenv("AGENT_SECRETS_KEY_DIR", emptyDir)
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", "/x/h.sock")
	_, signer, err := buildSigner()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := signer.(*helperSigner); !ok {
		t.Fatalf("a key dir without key.pem falls through to the helper: %T", signer)
	}

	t.Setenv("AGENT_SECRETS_KEY_DIR", newKeyDir(t))
	id, signer, err := buildSigner()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := signer.(*fileSigner); !ok {
		t.Fatalf("key.pem present means file mode: %T", signer)
	}
	if id != testEnrollmentID {
		t.Fatalf("enrollment id: got %q, want %q", id, testEnrollmentID)
	}
}

// TestBuildSignerNamesTheEnrollmentErrorDiagnostic covers the enrollment.error side-channel: when
// key.pem is present but the enrollment file the launcher writes on success is missing (an
// enroll that failed), buildSigner's error appends whatever the launcher recorded in
// enrollment.error, so a caller sees why enrollment failed rather than a bare "no such file";
// with no enrollment.error file, the message stays byte-identical to today's bare
// missing-file error.
func TestBuildSignerNamesTheEnrollmentErrorDiagnostic(t *testing.T) {
	dir := newKeyDir(t)
	if err := os.Remove(filepath.Join(dir, "enrollment")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_SECRETS_KEY_DIR", dir)
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", "")

	if _, _, err := buildSigner(); err == nil || strings.Contains(err.Error(), "enrollment.error") {
		t.Fatalf("no enrollment.error file must not be mentioned: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "enrollment.error"), []byte("broker 503 DATABASE: postgres unreachable\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := buildSigner()
	if err == nil || !strings.Contains(err.Error(), "enrollment.error") || !strings.Contains(err.Error(), "postgres unreachable") {
		t.Fatalf("a missing enrollment names the error file and its content: %v", err)
	}
}

// TestFileSignerMatchesWhatProofVerifyExpects round-trips a fileSigner's compact JWS through a
// real proof.Verifier: the same htm/htu/eid comparison the broker itself performs.
func TestFileSignerMatchesWhatProofVerifyExpects(t *testing.T) {
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	thumb, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	signer := &fileSigner{key: key, enrollmentID: "enr-round-trip"}
	const method, url = http.MethodGet, "https://broker.example/v1/enrollments/self"
	compact, err := signer.Sign(method, url)
	if err != nil {
		t.Fatal(err)
	}
	verifier := &proof.Verifier{
		Skew:   time.Minute,
		Lookup: func(ctx context.Context, enrollmentID string) (string, bool, error) { return thumb, true, nil },
		Replay: func(ctx context.Context, jti string, expires time.Time) (bool, error) { return true, nil },
	}
	id, err := verifier.Verify(context.Background(), compact, method, url, time.Now())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if id.EnrollmentID != "enr-round-trip" {
		t.Fatalf("verified enrollment id: got %q", id.EnrollmentID)
	}
}

// fakeHelper answers one canned line per connection and returns every decoded request line it
// received, so a test can assert what the client actually sent the helper; most callers ignore
// the second return value, ready with `sock, _ := fakeHelper(...)`.
// The pid-observing variant (fakeHelperWithPids, hostmode_linux_test.go) is Linux-only, since it
// pins the connecting pid with helper.PeerOf (SO_PEERPIDFD); this one only needs an ordinary unix
// socket, so it stays available on every platform this package's test binary is built for.
func fakeHelper(t *testing.T, canned helper.Response) (string, <-chan helper.Request) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "h.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	reqs := make(chan helper.Request, 8)
	go func() {
		for {
			conn, err := ln.AcceptUnix()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadBytes('\n')
			var req helper.Request
			_ = json.Unmarshal(line, &req)
			reqs <- req
			data, _ := json.Marshal(canned)
			_, _ = conn.Write(append(data, '\n'))
			conn.Close()
		}
	}()
	return sock, reqs
}

// TestHelperSignerAsksTheHelper covers both a granted proof and the helper's error code
// reaching the caller unwrapped.
func TestHelperSignerAsksTheHelper(t *testing.T) {
	sock, _ := fakeHelper(t, helper.Response{OK: true, Proof: "eyJ.fake.proof"})
	signer := &helperSigner{sock: sock}
	p, err := signer.Sign(http.MethodGet, "https://s/v1/enrollments/self")
	if err != nil || p != "eyJ.fake.proof" {
		t.Fatalf("sign: %q %v", p, err)
	}

	sock2, _ := fakeHelper(t, helper.Response{Code: helper.CodeNotASession, Error: "pid 5 is not inside a registered host session"})
	if _, err := (&helperSigner{sock: sock2}).Sign(http.MethodGet, "https://s/v1/enrollments/self"); err == nil ||
		!strings.Contains(err.Error(), helper.CodeNotASession) {
		t.Fatalf("the helper's code reaches the caller: %v", err)
	}
}

// TestHelperSignerSignsRequestObjects covers SignRequestObject: it must send Op "sign-request"
// with the requested secret names and reason (never audience — the helper signs against its own
// configured broker URL, not a value this peer supplies), return the helper's signed request
// object verbatim, and surface the helper's error code unwrapped exactly like Sign does.
func TestHelperSignerSignsRequestObjects(t *testing.T) {
	sock, reqs := fakeHelper(t, helper.Response{OK: true, RequestObject: "eyJ.fake.request"})
	signer := &helperSigner{sock: sock}
	ro, err := signer.SignRequestObject("https://secrets.test", []string{"DEEL_API_KEY", "OTHER_KEY"}, "why")
	if err != nil || ro != "eyJ.fake.request" {
		t.Fatalf("sign-request: %q %v", ro, err)
	}
	select {
	case req := <-reqs:
		if req.Op != "sign-request" || !reflect.DeepEqual(req.Secrets, []string{"DEEL_API_KEY", "OTHER_KEY"}) || req.Reason != "why" {
			t.Fatalf("request sent to the helper: %+v", req)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the fake never saw a request")
	}

	sock2, _ := fakeHelper(t, helper.Response{Code: helper.CodeNotASession, Error: "pid 5 is not inside a registered host session"})
	if _, err := (&helperSigner{sock: sock2}).SignRequestObject("https://secrets.test", []string{"DEEL_API_KEY"}, "why"); err == nil ||
		!strings.Contains(err.Error(), helper.CodeNotASession) {
		t.Fatalf("the helper's code reaches the caller: %v", err)
	}
}

// TestRegisterWithoutExecPrintsRuntimeEnrollmentState covers register's contract without
// --exec: "<runtime_id>\t<enrollment_id>\t<state>" on stdout, exit 0.
func TestRegisterWithoutExecPrintsRuntimeEnrollmentState(t *testing.T) {
	sock, _ := fakeHelper(t, helper.Response{OK: true, RuntimeID: "h:1:1", EnrollmentID: "enr-h", State: "enrolled"})
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", sock)
	var stdout, stderr bytes.Buffer
	code := cmdRegister(nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("register: exit %d, stderr %q", code, stderr.String())
	}
	if got, want := stdout.String(), "h:1:1\tenr-h\tenrolled\n"; got != want {
		t.Fatalf("register output: got %q, want %q", got, want)
	}
}

// TestRegisterWithoutExecExitsOneWhenWaitedButNotEnrolled covers "exits 1 if --wait was given
// and state isn't enrolled".
func TestRegisterWithoutExecExitsOneWhenWaitedButNotEnrolled(t *testing.T) {
	sock, _ := fakeHelper(t, helper.Response{OK: true, RuntimeID: "h:1:1", EnrollmentID: "enr-h", State: "enrolling", Error: "waiting on approver"})
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", sock)
	var stdout, stderr bytes.Buffer
	code := cmdRegister([]string{"--wait", "1"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("register --wait, not enrolled: exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}

// TestRegisterExecRunsTheCommandAsTheRegisteredPid lives in hostmode_linux_test.go (//go:build
// linux): it needs fakeHelperWithPids, which pins the connecting pid with helper.PeerOf
// (SO_PEERPIDFD, Linux-only).

// TestRegisterExecWithoutHelperWarnsAndStillExecs waits the client's 10 s connect patience; keep
// it — it is the launch-ordering case tmux-resurrect hits at boot.
func TestRegisterExecWithoutHelperWarnsAndStillExecs(t *testing.T) {
	cmd := exec.Command("go", "run", ".", "register", "--exec", "--", "sh", "-c", "echo ran")
	cmd.Env = append(os.Environ(), "AGENT_SECRETS_HELPER_SOCK="+filepath.Join(t.TempDir(), "absent.sock"))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != "ran" {
		t.Fatalf("the command must still run: %q %v", out, err)
	}
	if !strings.Contains(stderr.String(), "unreachable") {
		t.Fatalf("a warning names the unreachable helper: %q", stderr.String())
	}
}

// TestWhoamiAlwaysPrintsJSON proves whoami prints the broker's raw GET /v1/enrollments/self JSON
// body even without --json (scripts/agentbox's box-doctor and host-doctor both pipe bare
// `agent-secrets whoami` straight into jq): the same route and shape as self --json. Every field
// but lease_expires_at must match exactly; that one field is fakeBroker's own
// time.Now().Add(...), freshly computed on each of the two separate invocations, so it is
// compared as "present and parseable" rather than byte-equal.
func TestWhoamiAlwaysPrintsJSON(t *testing.T) {
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	keyDir := newKeyDir(t)

	selfOut, selfErr, selfCode := runAgentSecrets(t, binary, broker.URL, keyDir, nil, "self", "--json")
	whoamiOut, whoamiErr, whoamiCode := runAgentSecrets(t, binary, broker.URL, keyDir, nil, "whoami")

	if selfCode != 0 || whoamiCode != 0 {
		t.Fatalf("exit codes: self=%d (stderr %q) whoami=%d (stderr %q)", selfCode, selfErr, whoamiCode, whoamiErr)
	}
	self, whoami := oneJSONObject(t, selfOut), oneJSONObject(t, whoamiOut)
	if !mapsEqual(keySet(self), keySet(whoami)) {
		t.Fatalf("bare whoami must print self --json's exact key set: self=%v whoami=%v", keySet(self), keySet(whoami))
	}
	for k, v := range self {
		if k == "lease_expires_at" {
			continue
		}
		if !reflect.DeepEqual(whoami[k], v) {
			t.Fatalf("field %q: self=%v whoami=%v", k, v, whoami[k])
		}
	}
	if _, err := time.Parse(time.RFC3339, whoami["lease_expires_at"].(string)); err != nil {
		t.Fatalf("whoami lease_expires_at must parse as RFC3339: %v", err)
	}
}

// TestSignWithoutEnrollmentUsesTheBoxsOwnIdentity proves sign's default path signs a real,
// verifiable proof for the box's own enrollment id, without making a broker call.
func TestSignWithoutEnrollmentUsesTheBoxsOwnIdentity(t *testing.T) {
	keyDir := newKeyDir(t)
	t.Setenv("AGENT_SECRETS_KEY_DIR", keyDir)
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", "")

	var stdout, stderr bytes.Buffer
	const method, url = "GET", "https://broker.example/v1/enrollments/self"
	code := cmdSign([]string{"--method", method, "--url", url}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("sign: exit %d, stderr %q", code, stderr.String())
	}
	compact := strings.TrimSpace(stdout.String())

	key, err := loadKey(filepath.Join(keyDir, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	thumb, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	var seenEnrollment string
	verifier := &proof.Verifier{
		Skew: time.Minute,
		Lookup: func(ctx context.Context, enrollmentID string) (string, bool, error) {
			seenEnrollment = enrollmentID
			return thumb, true, nil
		},
		Replay: func(ctx context.Context, jti string, expires time.Time) (bool, error) { return true, nil },
	}
	if _, err := verifier.Verify(context.Background(), compact, method, url, time.Now()); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if seenEnrollment != testEnrollmentID {
		t.Fatalf("signed enrollment id: got %q, want %q", seenEnrollment, testEnrollmentID)
	}
}

// TestSignWithEnrollmentOverridesTheEnrollmentID proves --enrollment signs with this box's own
// key for a caller-supplied id instead of the box's own — the deliberate cross-enrollment
// substitution the broker must refuse.
func TestSignWithEnrollmentOverridesTheEnrollmentID(t *testing.T) {
	keyDir := newKeyDir(t)
	t.Setenv("AGENT_SECRETS_KEY_DIR", keyDir)
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", "")

	const otherEnrollment = "enr-other-box"
	var stdout, stderr bytes.Buffer
	const method, url = "GET", "https://broker.example/v1/enrollments/self"
	code := cmdSign([]string{"--method", method, "--url", url, "--enrollment", otherEnrollment}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("sign --enrollment: exit %d, stderr %q", code, stderr.String())
	}
	compact := strings.TrimSpace(stdout.String())

	key, err := loadKey(filepath.Join(keyDir, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	thumb, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	var seenEnrollment string
	verifier := &proof.Verifier{
		Skew: time.Minute,
		Lookup: func(ctx context.Context, enrollmentID string) (string, bool, error) {
			seenEnrollment = enrollmentID
			return thumb, true, nil
		},
		Replay: func(ctx context.Context, jti string, expires time.Time) (bool, error) { return true, nil },
	}
	if _, err := verifier.Verify(context.Background(), compact, method, url, time.Now()); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if seenEnrollment != otherEnrollment {
		t.Fatalf("--enrollment must substitute the signed id: got %q, want %q", seenEnrollment, otherEnrollment)
	}
}

// TestSignEnrollmentRefusedInHelperMode covers "--enrollment works only with a key dir; the
// helper signs only its own enrollment".
func TestSignEnrollmentRefusedInHelperMode(t *testing.T) {
	t.Setenv("AGENT_SECRETS_KEY_DIR", "")
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", "/x/h.sock")
	var stdout, stderr bytes.Buffer
	code := cmdSign([]string{"--method", "GET", "--url", "https://s/v1/enrollments/self", "--enrollment", "enr-x"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("--enrollment must be refused in helper mode")
	}
	if !strings.Contains(stderr.String(), "helper") {
		t.Fatalf("refusal must explain helper mode signs only its own enrollment: %q", stderr.String())
	}
}
