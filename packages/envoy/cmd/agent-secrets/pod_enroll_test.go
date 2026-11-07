//go:build unix

// packages/envoy/cmd/agent-secrets/pod_enroll_test.go
//
// `enroll --pod` against a projected token file and a fake broker: what the pod reads, what it
// sends the broker, what it writes, and what it says when the file, the token or the broker
// refuses it. The broker here is servePodBroker (pod_broker_test.go), which records the token
// login, the enrollment and the renewal this mode makes.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
)

const (
	testPodAccount = "system:serviceaccount:legion:k8s-session"
	testPodUID     = "11111111-2222-3333-4444-555555555555"
	testPodService = "k8s-session"
)

// fakePodToken is a JWT shaped like the projected ServiceAccount token a kubelet mounts for the
// agent-secrets audience. It is not signed by anything: the client reads its claims and never
// verifies them, and every broker in this file is a fake.
func fakePodToken(t *testing.T, subject, podUID string) string {
	t.Helper()
	claims := map[string]any{
		"iss": "https://kubernetes.example/cluster",
		"aud": []string{"agent-secrets"},
		"sub": subject,
		"exp": time.Now().Add(time.Hour).Unix(),
	}
	if podUID != "" {
		claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": podUID}}
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc.EncodeToString(payload) + ".c2ln"
}

// writePodToken writes token to a file in its own directory and answers the path, as the kubelet's
// projection mounts one.
func writePodToken(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestEnrolAPodFromItsProjectedToken: the whole of a pod's enrollment. It logs in with the token
// its projected file holds, enrols kind pod under the pod UID that token's own claims name,
// carrying the token again and a launcher proof, writes key.pem and enrollment into --out, prints
// the enrollment id alone on stdout and names the renewer on stderr. Run again on the same
// directory it reuses key.pem, so the enrollment the broker holds live for that key is the one it
// gets back, and it renews that lease.
func TestEnrolAPodFromItsProjectedToken(t *testing.T) {
	token := fakePodToken(t, testPodAccount, testPodUID)
	tokenFile := writePodToken(t, token)
	broker, calls := servePodBroker(t, "enrollment-of-the-pod")
	out := t.TempDir()
	t.Setenv("AGENT_SECRETS_URL", broker.URL)
	t.Setenv("AGENT_SECRETS_KEY_DIR", out)

	var stdout, stderr bytes.Buffer
	if code := cmdEnroll([]string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile}, &stdout, &stderr); code != 0 {
		t.Fatalf("enroll --pod: exit %d, stderr %q", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "enrollment-of-the-pod" {
		t.Fatalf("stdout = %q, want the enrollment id alone", stdout.String())
	}
	// The key directory is part of the command because renew reads AGENT_SECRETS_KEY_DIR and
	// never --out. Only the shape of the line is read here, never the paths in it: both are
	// shell-quoted, so a temporary directory holding a space or an apostrophe is correctly
	// printed unlike itself. pod_renew_command_test.go runs the printed line from exactly such
	// a directory, which is what proves it names the right one.
	for _, want := range []string{"keep this lease alive with: AGENT_SECRETS_KEY_DIR=", "agent-secrets renew --pod-token-file "} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr %q does not name the next command's %q", stderr.String(), want)
		}
	}
	if strings.Contains(stderr.String(), token) {
		t.Fatal("the pod's token reached stderr")
	}
	got := calls()
	if len(got) != 2 || got[0].path != "/v1/launcher-credentials/token" || got[1].path != "/v1/enrollments" {
		t.Fatalf("broker calls = %+v, want the token login then the enrollment", got)
	}
	if got[0].body["token"] != token {
		t.Fatalf("token login sent %q, want the projected token", got[0].body["token"])
	}
	// The signed request object is what the real broker verifies and reads --service from, so it
	// is verified here rather than merely counted (record.VerifyRequestObject, as main_test.go's
	// fake broker does for a secret request).
	login, err := record.VerifyRequestObject(got[0].body["request"], broker.URL, time.Minute, time.Now())
	if err != nil {
		t.Fatalf("record.VerifyRequestObject(the pod's login request): %v", err)
	}
	if len(login.Details) != 1 || login.Details[0].Type != "launcher_credential" {
		t.Fatalf("login request details = %+v, want exactly one launcher_credential detail", login.Details)
	}
	if login.Details[0].Service != testPodService {
		t.Fatalf("login request service = %q, want --service's %q", login.Details[0].Service, testPodService)
	}
	if login.Details[0].Identifier != testPodUID {
		t.Fatalf("login request identifier = %q, want the pod UID %q", login.Details[0].Identifier, testPodUID)
	}
	enrol := got[1]
	if enrol.proof == "" {
		t.Fatal("the enrollment carried no Proof header")
	}
	if enrol.body["kind"] != "pod" {
		t.Fatalf("enrollment kind = %q, want pod", enrol.body["kind"])
	}
	if enrol.body["runtime_id"] != testPodUID {
		t.Fatalf("runtime_id = %q, want the pod UID the token names, %q", enrol.body["runtime_id"], testPodUID)
	}
	if enrol.body["pod_token"] != token {
		t.Fatalf("enrollment pod_token = %q, want the projected token", enrol.body["pod_token"])
	}
	// The thumbprint must be the key.pem this run wrote, and the enrollment must be signed by the
	// login's own key, not by the session key: a broker with the real verifier refuses either
	// mistake, so both are checked here with that verifier.
	sessionKey, err := loadKey(filepath.Join(out, keyFile))
	if err != nil {
		t.Fatalf("read the session key the run wrote: %v", err)
	}
	sessionThumbprint, err := proof.Thumbprint(&sessionKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if enrol.body["thumbprint"] != sessionThumbprint {
		t.Fatalf("enrollment thumbprint = %q, want key.pem's %q", enrol.body["thumbprint"], sessionThumbprint)
	}
	verifier := &proof.Verifier{
		Skew: time.Minute,
		Lookup: func(_ context.Context, id string) (string, bool, error) {
			return sessionThumbprint, id == "enrollment-of-the-pod", nil
		},
		LookupLauncher: func(_ context.Context, id string) (string, bool, error) {
			return login.Thumbprint, id == "credential-for-the-pod", nil
		},
		Replay: func(context.Context, string, time.Time) (bool, error) { return true, nil },
	}
	subject, err := verifier.Verify(context.Background(), enrol.proof, http.MethodPost, broker.URL+"/v1/enrollments", time.Now())
	if err != nil {
		t.Fatalf("verify the enrollment's launcher proof: %v", err)
	}
	if subject.LauncherID != "credential-for-the-pod" {
		t.Fatalf("the enrollment's proof names launcher %q, want the credential the login issued", subject.LauncherID)
	}

	keyPath := filepath.Join(out, keyFile)
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read key.pem: %v", err)
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key.pem mode = %v, want 0600", info.Mode().Perm())
	}
	if id, err := readTrimmed(filepath.Join(out, enrollmentFile)); err != nil || id != "enrollment-of-the-pod" {
		t.Fatalf("enrollment file = %q (%v), want the enrollment id", id, err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := cmdEnroll([]string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--out", out}, &stdout, &stderr); code != 0 {
		t.Fatalf("second enroll --pod: exit %d, stderr %q", code, stderr.String())
	}
	again, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(keyPEM, again) {
		t.Fatal("re-running enroll --pod wrote a new key.pem, so the live enrollment of the first key would be lost")
	}
	if got := strings.TrimSpace(stdout.String()); got != "enrollment-of-the-pod" {
		t.Fatalf("second run stdout = %q, want the same enrollment id alone", stdout.String())
	}
	after := calls()
	if len(after) != 5 {
		t.Fatalf("broker calls after the second run = %d, want 5 (login, enrollment, renewal)", len(after))
	}
	if tp := after[3].body["thumbprint"]; tp != enrol.body["thumbprint"] {
		t.Fatalf("second enrollment thumbprint = %q, want the first key's %q", tp, enrol.body["thumbprint"])
	}
	renew := after[4]
	if renew.path != "/v1/enrollments/enrollment-of-the-pod/renew" {
		t.Fatalf("the second run's last call was %q, want the renewal of the enrollment it already holds", renew.path)
	}
	if renew.body["pod_token"] != token {
		t.Fatalf("renewal pod_token = %q, want the projected token", renew.body["pod_token"])
	}
	// The renewal is the session key's own proof, naming the enrollment; the launcher key signs
	// nothing after the enrollment.
	renewal, err := verifier.Verify(context.Background(), renew.proof, http.MethodPost, broker.URL+"/v1/enrollments/enrollment-of-the-pod/renew", time.Now())
	if err != nil {
		t.Fatalf("verify the renewal's session proof: %v", err)
	}
	if renewal.EnrollmentID != "enrollment-of-the-pod" {
		t.Fatalf("the renewal's proof names enrollment %q, want the one it holds", renewal.EnrollmentID)
	}
}

// TestPodUsageRefusals: every way of naming the pod form wrongly is exit 2, and says what is
// missing or which two flags cannot be given together. With neither mode the refusal names both,
// so a pod that forgot --pod is told the mode exists.
func TestPodUsageRefusals(t *testing.T) {
	tokenFile := writePodToken(t, fakePodToken(t, testPodAccount, testPodUID))
	broker, calls := servePodBroker(t, "never-reached")
	for _, tc := range []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"no mode at all", nil, nil, "--pod"},
		{"no mode at all names the helper", nil, nil, "--helper is required"},
		{"--pod with --helper", []string{"--pod", "--helper", "--service", testPodService}, nil, "mutually exclusive"},
		{"--pod with no service", []string{"--pod", "--pod-token-file", tokenFile}, nil, "--service"},
		{"--pod with no token file", []string{"--pod", "--service", testPodService, "--pod-token-file", ""}, nil, "--pod-token-file"},
		{"--pod with no key dir", []string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile}, map[string]string{"AGENT_SECRETS_KEY_DIR": ""}, "--out"},
		{"--pod with no broker URL", []string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile}, map[string]string{"AGENT_SECRETS_URL": ""}, brokerURLVar},
		// --helper's three, each refused by name rather than silently ignored, as --helper
		// itself is beside --pod.
		{"--pod with --runtime-id", []string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--runtime-id", "pod-1"}, nil, "--runtime-id is --helper's"},
		{"--pod with --thumbprint", []string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--thumbprint", "tp"}, nil, "--thumbprint is --helper's"},
		{"--pod with --session-id", []string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--session-id", "s-1"}, nil, "--session-id is --helper's"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_SECRETS_URL", broker.URL)
			t.Setenv("AGENT_SECRETS_KEY_DIR", t.TempDir())
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			var stdout, stderr bytes.Buffer
			if code := cmdEnroll(tc.args, &stdout, &stderr); code != exitUsageError {
				t.Fatalf("exit %d, want %d (stderr %q)", code, exitUsageError, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("stderr %q does not name %q", stderr.String(), tc.want)
			}
		})
	}
	if n := len(calls()); n != 0 {
		t.Fatalf("the broker was asked %d times, want none: a usage refusal calls nothing", n)
	}
}

// TestAPodTokenFileThatCannotBeReadIsNamed: a pod whose projected token file is missing or blank
// exits 1 naming the file, and asks the broker nothing — a blank file is never sent as a token.
func TestAPodTokenFileThatCannotBeReadIsNamed(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	blank := writePodToken(t, "")
	for name, path := range map[string]string{"a missing file": missing, "an empty file": blank} {
		t.Run(name, func(t *testing.T) {
			broker, calls := servePodBroker(t, "never-reached")
			t.Setenv("AGENT_SECRETS_URL", broker.URL)
			var stdout, stderr bytes.Buffer
			code := cmdEnroll([]string{"--pod", "--service", testPodService, "--pod-token-file", path, "--out", t.TempDir()}, &stdout, &stderr)
			if code != 1 {
				t.Fatalf("exit %d, want 1 (stderr %q)", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), path) {
				t.Fatalf("stderr %q does not name the token file %q", stderr.String(), path)
			}
			if n := len(calls()); n != 0 {
				t.Fatalf("the broker was asked %d times, want none", n)
			}
		})
	}
}

// TestAPodTokenBoundToNoPodIsNamed: a token `kubectl create token` mints names no pod, so it
// enrols nothing — the broker lets a login enrol only the pod its own token is bound to. The
// refusal says so before anything is asked of the broker, rather than arriving as a 403 the
// operator has to decode.
func TestAPodTokenBoundToNoPodIsNamed(t *testing.T) {
	tokenFile := writePodToken(t, fakePodToken(t, testPodAccount, ""))
	broker, calls := servePodBroker(t, "never-reached")
	t.Setenv("AGENT_SECRETS_URL", broker.URL)
	var stdout, stderr bytes.Buffer
	if code := cmdEnroll([]string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--out", t.TempDir()}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1 (stderr %q)", code, stderr.String())
	}
	for _, want := range []string{"bound to no pod", "kubernetes.io.pod.uid", "kubectl create token"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr %q does not say %q", stderr.String(), want)
		}
	}
	if n := len(calls()); n != 0 {
		t.Fatalf("the broker was asked %d times, want none", n)
	}
}

// TestNotAJWTInThePodTokenFileIsNamed: a file holding something that is not a JWT at all is named
// as such, not sent to the broker to be refused there.
func TestNotAJWTInThePodTokenFileIsNamed(t *testing.T) {
	tokenFile := writePodToken(t, "not-a-jwt")
	broker, calls := servePodBroker(t, "never-reached")
	t.Setenv("AGENT_SECRETS_URL", broker.URL)
	var stdout, stderr bytes.Buffer
	if code := cmdEnroll([]string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--out", t.TempDir()}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1 (stderr %q)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "is not a JWT") {
		t.Fatalf("stderr %q does not say the token is not a JWT", stderr.String())
	}
	if n := len(calls()); n != 0 {
		t.Fatalf("the broker was asked %d times, want none", n)
	}
}

// TestARefusedPodLoginExitsAndNamesTheIdentity: a refusal names which pod was refused — its
// ServiceAccount and its UID — with the HTTP status, the broker's code and message, and never the
// token itself. POD_SELF_ENROLL_ONLY is what a login bound to another pod, or to none, is
// answered.
func TestARefusedPodLoginExitsAndNamesTheIdentity(t *testing.T) {
	token := fakePodToken(t, testPodAccount, testPodUID)
	tokenFile := writePodToken(t, token)
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"code": "ACCOUNT_NOT_LISTED", "error": "this ServiceAccount logs in as no service",
		})
	}))
	defer broker.Close()
	t.Setenv("AGENT_SECRETS_URL", broker.URL)
	var stdout, stderr bytes.Buffer
	if code := cmdEnroll([]string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--out", t.TempDir()}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1 (stderr %q)", code, stderr.String())
	}
	for _, want := range []string{testPodAccount, testPodUID, "403", "ACCOUNT_NOT_LISTED", "could not log in"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr %q does not name %q", stderr.String(), want)
		}
	}
	if strings.Contains(stderr.String(), token) {
		t.Fatal("the refusal printed the pod's token")
	}
}

// TestARefusedEnrolmentLeavesTheRefusalBesideTheKey: a pod whose enrolment the broker refuses
// keeps its key — a new key while the old enrollment is live is ALREADY_ENROLLED — and that
// leaves the one state a later call cannot read on its own: key.pem with no enrollment, which
// selectSigner reaches on the key alone and then finds no id for. So the refusal is written
// into enrollment.error beside the key, and a later `agent-secrets` call says what the broker
// said. A re-run that succeeds takes that file away again.
func TestARefusedEnrolmentLeavesTheRefusalBesideTheKey(t *testing.T) {
	token := fakePodToken(t, testPodAccount, testPodUID)
	tokenFile := writePodToken(t, token)
	out := t.TempDir()
	refuse := true
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/launcher-credentials/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"credential_id": "credential-for-the-pod", "expires_at": time.Now().Add(time.Hour),
			})
			return
		}
		if refuse {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"code": "POD_SELF_ENROLL_ONLY", "error": "this machine login enrols its own pod alone",
			})
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"enrollment_id": "enrollment-of-the-pod", "lease_expires_at": time.Now().Add(time.Hour), "slot": nil,
		})
	}))
	defer broker.Close()
	t.Setenv("AGENT_SECRETS_URL", broker.URL)
	t.Setenv("AGENT_SECRETS_KEY_DIR", out)
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", "")

	var stdout, stderr bytes.Buffer
	if code := cmdEnroll([]string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--out", out}, &stdout, &stderr); code != 1 {
		t.Fatalf("the refused enrolment = exit %d, want 1 (stderr %q)", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(out, keyFile)); err != nil {
		t.Fatalf("the key after a refused enrolment: %v; it is kept on purpose", err)
	}
	recorded, err := readTrimmed(filepath.Join(out, enrollmentErrorFile))
	if err != nil {
		t.Fatalf("read %s after a refused enrolment: %v", enrollmentErrorFile, err)
	}
	for _, want := range []string{"POD_SELF_ENROLL_ONLY", "403", testPodUID} {
		if !strings.Contains(recorded, want) {
			t.Fatalf("%s holds %q, which does not name %q", enrollmentErrorFile, recorded, want)
		}
	}
	if strings.Contains(recorded, token) {
		t.Fatalf("%s holds the pod's token", enrollmentErrorFile)
	}

	// What a later call now says: the broker's refusal, not a missing file.
	if _, _, err := buildSigner(); err == nil ||
		!strings.Contains(err.Error(), enrollmentErrorFile) || !strings.Contains(err.Error(), "POD_SELF_ENROLL_ONLY") {
		t.Fatalf("a later call after a refused enrolment = %v; want it to name %s and what the broker said", err, enrollmentErrorFile)
	}

	refuse = false
	stdout.Reset()
	stderr.Reset()
	if code := cmdEnroll([]string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--out", out}, &stdout, &stderr); code != 0 {
		t.Fatalf("the run that succeeds = exit %d, stderr %q", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(out, enrollmentErrorFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s after a run that succeeded: %v; want it gone", enrollmentErrorFile, err)
	}
	if _, _, err := buildSigner(); err != nil {
		t.Fatalf("a later call after the run that succeeded: %v", err)
	}
}

// TestTwoRunsOnOneDirectoryTakeTurns: a pod can run `enroll --pod` more than once over one key
// directory, and without the lock the second logs in again — moving the launcher credential the
// first is still enrolling under — and the two race over key.pem and enrollment. The lock is the
// only thing holding that off, and nothing else in this package exercises it. Four runs at once
// on one directory must leave one key, one enrollment id, and must not have made four logins
// race: each run either made the enrollment or got that same one back.
func TestTwoRunsOnOneDirectoryTakeTurns(t *testing.T) {
	tokenFile := writePodToken(t, fakePodToken(t, testPodAccount, testPodUID))
	broker, calls := servePodBroker(t, "enrollment-of-the-pod")
	out := filepath.Join(t.TempDir(), "keys")
	t.Setenv("AGENT_SECRETS_URL", broker.URL)

	const runs = 4
	codes := make([]int, runs)
	outs := make([]strings.Builder, runs)
	errs := make([]strings.Builder, runs)
	start := make(chan struct{})
	var wait sync.WaitGroup
	for i := range runs {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			codes[i] = cmdEnrollPod(testPodService, tokenFile, out, &outs[i], &errs[i])
		}()
	}
	close(start)
	wait.Wait()

	for i := range runs {
		if codes[i] != 0 {
			t.Fatalf("run %d = exit %d, stderr %q; every run on one directory must succeed in its turn", i, codes[i], errs[i].String())
		}
		if got := strings.TrimSpace(outs[i].String()); got != "enrollment-of-the-pod" {
			t.Fatalf("run %d printed %q, want the one enrollment id", i, got)
		}
	}
	if id, err := readTrimmed(filepath.Join(out, enrollmentFile)); err != nil || id != "enrollment-of-the-pod" {
		t.Fatalf("the enrollment file = %q (%v), want the one id", id, err)
	}
	// One key, so the enrollment every run got back is the one that key holds: the first run
	// wrote it and the rest reused it rather than writing their own.
	thumbprints := map[string]bool{}
	for _, call := range calls() {
		if call.path == "/v1/enrollments" {
			thumbprints[call.body["thumbprint"]] = true
		}
	}
	if len(thumbprints) != 1 {
		t.Fatalf("the runs enrolled %d keys, want one: the lock did not serialize them", len(thumbprints))
	}
}
