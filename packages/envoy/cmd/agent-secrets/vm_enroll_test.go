// packages/envoy/cmd/agent-secrets/vm_enroll_test.go
//
// `enroll --vm` and `renew --vm` against a fake metadata server and a fake broker: what the VM
// asks the metadata server for, what it sends the broker, what it writes, and what it says when
// either refuses. The real broker end of it is TestAVMReadsItsServicesSecretWithNoApproval
// (vm_rig_test.go), which uses no fake but the metadata server a real VM would have.
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
	"sync"
	"testing"
	"time"
)

const (
	testVMAccount    = "example-fleet@example-project.iam.gserviceaccount.com"
	testVMProject    = "example-project"
	testVMZone       = "example-zone-a"
	testVMInstance   = "example-vm-1"
	testVMInstanceID = "1000000000000000001"
	testVMAudience   = "example-vm-audience"
	testVMService    = "example-fleet"
	testVMRuntimeID  = testVMAccount + "@" + testVMProject + "/" + testVMZone + "/" + testVMInstanceID
)

// fakeVMToken is a JWT shaped like the one Google's metadata server mints with format=full. It is
// not signed by anything: the client reads its claims and never verifies them, and every broker
// in this file is a fake.
func fakeVMToken(t *testing.T, mark string) string {
	t.Helper()
	claims := map[string]any{
		"iss":   "https://accounts.google.com",
		"aud":   testVMAudience,
		"email": testVMAccount,
		"mark":  mark,
		"google": map[string]any{
			"compute_engine": map[string]any{
				"project_id":    testVMProject,
				"zone":          testVMZone,
				"instance_name": testVMInstance,
				"instance_id":   testVMInstanceID,
			},
		},
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc.EncodeToString(payload) + ".c2ln"
}

// serveMetadata starts a fake metadata server on GCE_METADATA_HOST that serves the token next
// returns, after asserting the request is the one a Google VM makes: the identity path, the
// Metadata-Flavor header, the audience, and format=full, without which the token would carry no
// compute-engine claims at all. It answers how many tokens it has served.
func serveMetadata(t *testing.T, next func() string) func() int {
	t.Helper()
	var mu sync.Mutex
	served := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != metadataIdentityPath {
			t.Errorf("metadata path = %q, want %q", r.URL.Path, metadataIdentityPath)
		}
		if got := r.Header.Get("Metadata-Flavor"); got != "Google" {
			t.Errorf("Metadata-Flavor = %q, want Google", got)
		}
		if got := r.URL.Query().Get("audience"); got != testVMAudience {
			t.Errorf("audience = %q, want %q", got, testVMAudience)
		}
		if got := r.URL.Query().Get("format"); got != "full" {
			t.Errorf("format = %q, want full (without it the token carries no compute-engine claims)", got)
		}
		mu.Lock()
		served++
		mu.Unlock()
		io.WriteString(w, next()+"\n")
	}))
	t.Cleanup(srv.Close)
	t.Setenv(metadataHostVar, strings.TrimPrefix(srv.URL, "http://"))
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return served
	}
}

// vmCall is one call a fake broker took: its path and its decoded JSON body.
type vmCall struct {
	path  string
	proof string
	body  map[string]string
}

// serveVMBroker is a broker that takes the token login, the enrollment and a renewal, recording
// each. It answers a thumbprint it has already enrolled 200 with the same enrollment id and,
// like the real broker, without extending its lease, so a re-run has to renew.
func serveVMBroker(t *testing.T, enrollmentID string) (*httptest.Server, func() []vmCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []vmCall
	enrolled := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := map[string]string{}
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		calls = append(calls, vmCall{path: r.URL.Path, proof: r.Header.Get("Proof"), body: body})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/launcher-credentials/token":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"credential_id": "credential-for-the-vm",
				"expires_at":    time.Now().Add(time.Hour),
			})
		case "/v1/enrollments":
			mu.Lock()
			live := enrolled[body["thumbprint"]]
			enrolled[body["thumbprint"]] = true
			mu.Unlock()
			if live {
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusCreated)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"enrollment_id":    enrollmentID,
				"lease_expires_at": time.Now().Add(time.Hour),
				"slot":             nil,
			})
		case "/v1/enrollments/" + enrollmentID + "/renew":
			_ = json.NewEncoder(w).Encode(map[string]any{"lease_expires_at": time.Now().Add(time.Hour)})
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"code": "NOT_FOUND", "error": r.URL.Path})
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []vmCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]vmCall(nil), calls...)
	}
}

// TestEnrolAVMFromItsMetadataToken: the whole of a VM's enrollment. It logs in with the token the
// metadata server served, enrols kind vm under the runtime id the token's own claims name,
// carrying the token again and a launcher proof, writes key.pem and enrollment into --out, and
// prints the enrollment id alone. Run again on the same directory it reuses key.pem, so the
// enrollment the broker holds live for that key is the one it gets back.
func TestEnrolAVMFromItsMetadataToken(t *testing.T) {
	token := fakeVMToken(t, "first")
	serveMetadata(t, func() string { return token })
	broker, calls := serveVMBroker(t, "enrollment-of-the-vm")
	out := t.TempDir()
	t.Setenv("AGENT_SECRETS_URL", broker.URL)
	t.Setenv("AGENT_SECRETS_KEY_DIR", out)
	t.Setenv(vmAudienceVar, testVMAudience)

	var stdout, stderr bytes.Buffer
	if code := cmdEnroll([]string{"--vm", "--service", testVMService}, &stdout, &stderr); code != 0 {
		t.Fatalf("enroll --vm: exit %d, stderr %q", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "enrollment-of-the-vm" {
		t.Fatalf("stdout = %q, want the enrollment id alone", stdout.String())
	}
	got := calls()
	if len(got) != 2 || got[0].path != "/v1/launcher-credentials/token" || got[1].path != "/v1/enrollments" {
		t.Fatalf("broker calls = %+v, want the token login then the enrollment", got)
	}
	if got[0].body["token"] != token {
		t.Fatalf("token login sent %q, want the metadata server's token", got[0].body["token"])
	}
	if got[0].body["request"] == "" {
		t.Fatal("token login sent no signed request object")
	}
	enrol := got[1]
	if enrol.proof == "" {
		t.Fatal("the enrollment carried no Proof header")
	}
	if enrol.body["kind"] != "vm" {
		t.Fatalf("enrollment kind = %q, want vm", enrol.body["kind"])
	}
	if enrol.body["runtime_id"] != testVMRuntimeID {
		t.Fatalf("runtime_id = %q, want %q", enrol.body["runtime_id"], testVMRuntimeID)
	}
	if enrol.body["vm_token"] != token {
		t.Fatalf("enrollment vm_token = %q, want the metadata server's token", enrol.body["vm_token"])
	}
	if enrol.body["thumbprint"] == "" {
		t.Fatal("the enrollment named no thumbprint")
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
	id, err := readTrimmed(filepath.Join(out, enrollmentFile))
	if err != nil || id != "enrollment-of-the-vm" {
		t.Fatalf("enrollment file = %q (%v), want the enrollment id", id, err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := cmdEnroll([]string{"--vm", "--service", testVMService, "--out", out}, &stdout, &stderr); code != 0 {
		t.Fatalf("second enroll --vm: exit %d, stderr %q", code, stderr.String())
	}
	again, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(keyPEM, again) {
		t.Fatal("re-running enroll --vm wrote a new key.pem, so the live enrollment of the first key would be lost")
	}
	if got := strings.TrimSpace(stdout.String()); got != "enrollment-of-the-vm" {
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
	if renew.path != "/v1/enrollments/enrollment-of-the-vm/renew" {
		t.Fatalf("the second run's last call was %q, want the renewal of the enrollment it already holds", renew.path)
	}
	if renew.body["vm_token"] != token {
		t.Fatalf("renewal vm_token = %q, want the metadata server's token", renew.body["vm_token"])
	}
	if renew.proof == "" {
		t.Fatal("the renewal carried no session Proof header")
	}
	if id, err := readTrimmed(filepath.Join(out, enrollmentFile)); err != nil || id != "enrollment-of-the-vm" {
		t.Fatalf("enrollment file after the second run = %q (%v)", id, err)
	}
}

// TestTwoConcurrentVMEnrollmentsOnOneDirAgree: the lane re-runs `enroll --vm` over one directory
// whenever it needs a secret, so two runs can overlap. The flock on <out>/enroll.lock makes the
// second wait for the first, so both answer the same enrollment id over one key.pem instead of
// racing over the files and the VM's one launcher credential.
func TestTwoConcurrentVMEnrollmentsOnOneDirAgree(t *testing.T) {
	token := fakeVMToken(t, "concurrent")
	serveMetadata(t, func() string { return token })
	broker, _ := serveVMBroker(t, "enrollment-of-the-vm")
	out := t.TempDir()
	t.Setenv("AGENT_SECRETS_URL", broker.URL)
	t.Setenv(vmAudienceVar, testVMAudience)

	var wg sync.WaitGroup
	ids := make([]string, 2)
	codes := make([]int, 2)
	errs := make([]string, 2)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var stdout, stderr bytes.Buffer
			codes[i] = cmdEnroll([]string{"--vm", "--service", testVMService, "--out", out}, &stdout, &stderr)
			ids[i] = strings.TrimSpace(stdout.String())
			errs[i] = stderr.String()
		}()
	}
	wg.Wait()
	for i := range ids {
		if codes[i] != 0 {
			t.Fatalf("run %d: exit %d, stderr %q", i, codes[i], errs[i])
		}
		if ids[i] != "enrollment-of-the-vm" {
			t.Fatalf("run %d printed %q, want the enrollment id alone", i, ids[i])
		}
	}
}

// TestNoMetadataServerExitsOneAndNamesIt: a VM whose metadata server cannot be reached exits 1
// and says so, naming the address it tried and the variable that chose it, rather than failing
// later as a broker refusal that never happened.
func TestNoMetadataServerExitsOneAndNamesIt(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	host := strings.TrimPrefix(dead.URL, "http://")
	dead.Close()
	broker, calls := serveVMBroker(t, "never-reached")
	t.Setenv(metadataHostVar, host)
	t.Setenv("AGENT_SECRETS_URL", broker.URL)
	t.Setenv(vmAudienceVar, testVMAudience)

	var stdout, stderr bytes.Buffer
	code := cmdEnroll([]string{"--vm", "--service", testVMService, "--out", t.TempDir()}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("enroll --vm with no metadata server: exit %d, want 1 (stderr %q)", code, stderr.String())
	}
	for _, want := range []string{"metadata server", host, metadataHostVar} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr %q does not name %q", stderr.String(), want)
		}
	}
	if n := len(calls()); n != 0 {
		t.Fatalf("the broker was asked %d times, want none: nothing is sent without a token", n)
	}
}

// TestARefusedVMLoginExitsAndNamesTheIdentity: a refusal names which VM was refused — account,
// project, zone, instance name and id — with the HTTP status, the broker's code and message, and
// the broker asked, and never the token itself.
func TestARefusedVMLoginExitsAndNamesTheIdentity(t *testing.T) {
	token := fakeVMToken(t, "refused")
	serveMetadata(t, func() string { return token })
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"code":  "ACCOUNT_NOT_LISTED",
			"error": "the account is bound to another service",
		})
	}))
	defer broker.Close()
	out := t.TempDir()
	t.Setenv("AGENT_SECRETS_URL", broker.URL)
	t.Setenv(vmAudienceVar, testVMAudience)

	var stdout, stderr bytes.Buffer
	if code := cmdEnroll([]string{"--vm", "--service", testVMService, "--out", out}, &stdout, &stderr); code != 1 {
		t.Fatalf("a refused login: exit %d, want 1 (stderr %q)", code, stderr.String())
	}
	for _, want := range []string{
		testVMAccount, testVMProject, testVMZone, testVMInstance, testVMInstanceID,
		"HTTP 403", "ACCOUNT_NOT_LISTED", "the account is bound to another service",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr %q does not name %q", stderr.String(), want)
		}
	}
	if strings.Contains(stderr.String(), token) {
		t.Fatal("the refusal printed the identity token")
	}
	if exists(filepath.Join(out, enrollmentFile)) {
		t.Fatal("a refused login wrote an enrollment file")
	}
}

// TestRenewReadsAFreshMetadataTokenEachTime: a VM's renewal carries the token the metadata server
// served at that moment, as a pod's carries the one its projected file held, so a renewal is
// never refused for a token that expired while the renewer ran.
func TestRenewReadsAFreshMetadataTokenEachTime(t *testing.T) {
	var mu sync.Mutex
	minted := 0
	tokens := map[int]string{}
	serveMetadata(t, func() string {
		mu.Lock()
		defer mu.Unlock()
		minted++
		tokens[minted] = fakeVMToken(t, "token-"+string(rune('a'+minted)))
		return tokens[minted]
	})
	var bodies []string
	first := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		n := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			first <- struct{}{}
			_ = json.NewEncoder(w).Encode(map[string]any{"lease_expires_at": time.Now().Add(time.Hour)})
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": "ENROLLMENT_NOT_LIVE", "error": "enrollment is not live"})
	}))
	defer srv.Close()
	client, signer, enrollmentID, _ := renewalSession(t, srv.URL)
	waits, after := steppedWaits(1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan int, 1)
	go func() {
		result <- renewLoop(ctx, client, signer, enrollmentID, vmRenewToken(ctx, testVMAudience), io.Discard, after)
	}()
	receive(t, first, "first renewal")
	receive(t, waits, "wait for the next renewal") <- time.Now()
	if code := receive(t, result, "renew loop exit"); code != exitRenewRefused {
		t.Fatalf("renew loop exit = %d, want %d", code, exitRenewRefused)
	}
	mu.Lock()
	got := append([]string(nil), bodies...)
	want := []string{`{"vm_token":"` + tokens[1] + `"}`, `{"vm_token":"` + tokens[2] + `"}`}
	mu.Unlock()
	if len(got) != len(want) {
		t.Fatalf("renew bodies %q, want %q", got, want)
	}
	for i := range want {
		if strings.TrimSpace(got[i]) != want[i] {
			t.Fatalf("renew body %d = %q, want the token minted for it, %q", i, got[i], want[i])
		}
	}
}

// TestVMUsageRefusals: every way of naming the VM forms wrongly is exit 2, and says what is
// missing or which two flags cannot be given together.
func TestVMUsageRefusals(t *testing.T) {
	token := fakeVMToken(t, "usage")
	serveMetadata(t, func() string { return token })
	broker, _ := serveVMBroker(t, "never-reached")
	for _, tc := range []struct {
		name string
		args []string
		env  map[string]string
		want string
		cmd  func([]string, io.Writer, io.Writer) int
	}{
		{"enroll with no audience", []string{"--vm", "--service", testVMService}, map[string]string{vmAudienceVar: ""}, vmAudienceVar, cmdEnroll},
		{"enroll with no service", []string{"--vm"}, nil, "--service", cmdEnroll},
		{"enroll with no key dir", []string{"--vm", "--service", testVMService}, map[string]string{"AGENT_SECRETS_KEY_DIR": ""}, "--out", cmdEnroll},
		{"enroll with no broker URL", []string{"--vm", "--service", testVMService}, map[string]string{"AGENT_SECRETS_URL": ""}, brokerURLVar, cmdEnroll},
		{"--vm with --helper", []string{"--vm", "--helper", "--service", testVMService}, nil, "mutually exclusive", cmdEnroll},
		{"renew --vm with --pod-token-file", []string{"--vm", "--pod-token-file", "/dev/null"}, nil, "mutually exclusive", cmdRenew},
		{"renew --vm with no audience", []string{"--vm"}, map[string]string{vmAudienceVar: ""}, vmAudienceVar, cmdRenew},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_SECRETS_URL", broker.URL)
			t.Setenv("AGENT_SECRETS_KEY_DIR", t.TempDir())
			t.Setenv(vmAudienceVar, testVMAudience)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			var stdout, stderr bytes.Buffer
			if code := tc.cmd(tc.args, &stdout, &stderr); code != exitUsageError {
				t.Fatalf("exit %d, want %d (stderr %q)", code, exitUsageError, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("stderr %q does not name %q", stderr.String(), tc.want)
			}
		})
	}
}

// TestAVMTokenWithoutTheComputeEngineClaimsIsNamed: a token minted without format=full names no
// project, zone or instance, so nothing can be enrolled from it; the refusal says which claims
// are missing and that format=full is what carries them.
func TestAVMTokenWithoutTheComputeEngineClaimsIsNamed(t *testing.T) {
	payload, err := json.Marshal(map[string]any{"email": testVMAccount, "aud": testVMAudience})
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding
	short := enc.EncodeToString([]byte(`{"alg":"RS256"}`)) + "." + enc.EncodeToString(payload) + ".c2ln"
	serveMetadata(t, func() string { return short })
	broker, calls := serveVMBroker(t, "never-reached")
	t.Setenv("AGENT_SECRETS_URL", broker.URL)
	t.Setenv(vmAudienceVar, testVMAudience)
	var stdout, stderr bytes.Buffer
	if code := cmdEnroll([]string{"--vm", "--service", testVMService, "--out", t.TempDir()}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1 (stderr %q)", code, stderr.String())
	}
	for _, want := range []string{"project_id", "zone", "instance_name", "instance_id", "format=full"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr %q does not name %q", stderr.String(), want)
		}
	}
	if n := len(calls()); n != 0 {
		t.Fatalf("the broker was asked %d times, want none", n)
	}
}
