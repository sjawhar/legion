// packages/envoy/cmd/agent-secrets/pod_enroll_test.go
//
// `enroll --pod` against a projected token file and a fake broker: what the pod reads, what it
// sends the broker, what it writes, and what it says when the file, the token or the broker
// refuses it. The broker here is serveVMBroker (vm_enroll_test.go): a pod's enrollment uses the
// same two routes a VM's does, so the same fake records both.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	broker, calls := serveVMBroker(t, "enrollment-of-the-pod")
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
	for _, want := range []string{"renew --pod-token-file", tokenFile} {
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
			return login.Thumbprint, id == "credential-for-the-vm", nil
		},
		Replay: func(context.Context, string, time.Time) (bool, error) { return true, nil },
	}
	subject, err := verifier.Verify(context.Background(), enrol.proof, http.MethodPost, broker.URL+"/v1/enrollments", time.Now())
	if err != nil {
		t.Fatalf("verify the enrollment's launcher proof: %v", err)
	}
	if subject.LauncherID != "credential-for-the-vm" {
		t.Fatalf("the enrollment's proof names launcher %q, want the credential the login issued", subject.LauncherID)
	}
	if enrol.body["vm_token"] != "" {
		t.Fatalf("the enrollment carried a vm_token %q, want none", enrol.body["vm_token"])
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
// missing or which two flags cannot be given together. With none of the three modes the refusal
// names all three, so a pod that forgot --pod is told the mode exists.
func TestPodUsageRefusals(t *testing.T) {
	tokenFile := writePodToken(t, fakePodToken(t, testPodAccount, testPodUID))
	broker, calls := serveVMBroker(t, "never-reached")
	for _, tc := range []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"no mode at all", nil, nil, "--pod"},
		{"no mode at all names the helper", nil, nil, "--helper is required"},
		{"--pod with --helper", []string{"--pod", "--helper", "--service", testPodService}, nil, "mutually exclusive"},
		{"--pod with --vm", []string{"--pod", "--vm", "--service", testPodService}, nil, "mutually exclusive"},
		{"--pod with no service", []string{"--pod", "--pod-token-file", tokenFile}, nil, "--service"},
		{"--pod with no token file", []string{"--pod", "--service", testPodService, "--pod-token-file", ""}, nil, "--pod-token-file"},
		{"--pod with no key dir", []string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile}, map[string]string{"AGENT_SECRETS_KEY_DIR": ""}, "--out"},
		{"--pod with no broker URL", []string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile}, map[string]string{"AGENT_SECRETS_URL": ""}, brokerURLVar},
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
			broker, calls := serveVMBroker(t, "never-reached")
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
	broker, calls := serveVMBroker(t, "never-reached")
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
	broker, calls := serveVMBroker(t, "never-reached")
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
