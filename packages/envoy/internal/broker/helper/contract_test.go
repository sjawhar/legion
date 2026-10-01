// packages/envoy/internal/broker/helper/contract_test.go
//go:build linux

package helper

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/proof"
)

// syncBuffer is bytes.Buffer plus a mutex: slog's own handler serializes its Write calls, but a
// test reading the buffer's text back out (String) is a second, unsynchronized accessor racing
// those writes from the Server's own background goroutines (enrollLoop, renewLoop) — a real race
// -race catches, not a flaky artifact to suppress.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// contractRig is a real Server + Broker (this package's own production types) pointed at a real
// broker (brokertest.Rig) over a real unix socket — the lesson test's own rig, distinct from
// server_test.go's newRig/startRig, which point the same Server/Broker types at a hand-rolled
// fake. Only this file (and brokertest itself) drives a genuine end-to-end broker.
type contractRig struct {
	broker       *brokertest.Rig
	srv          *Server
	sock         string
	sessionsPath string
	logBuf       *syncBuffer
	cancel       context.CancelFunc
}

// newContractRig mounts a real broker (brokertest.NewRig) and serves this package's own Server
// on a real unix socket, its Broker pointed at that real broker's URL and OperatorFile. Every
// socket/registry/operator artifact lives under its own TempDir, never under $HOME — the caller
// is expected to have set HOME to a separate, otherwise-untouched TempDir of its own so it can
// later assert nothing wrote there.
func newContractRig(t *testing.T) *contractRig {
	t.Helper()
	broker := brokertest.NewRig(t)
	artifacts := t.TempDir()
	logBuf := &syncBuffer{}
	sessionsPath := filepath.Join(artifacts, "sessions.json")
	cr := &contractRig{broker: broker, sessionsPath: sessionsPath, logBuf: logBuf}
	cr.srv = &Server{
		Registry: NewRegistry(sessionsPath),
		Broker:   &Broker{URL: broker.URL, OperatorFile: broker.OperatorFile, HTTP: http.DefaultClient},
		Hostname: "contract-test-host",
		PeerOf:   PeerOf,
		Log:      slog.New(slog.NewTextHandler(logBuf, nil)),
		MinRenew: 50 * time.Millisecond,
	}
	cr.sock = filepath.Join(artifacts, "helper.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: cr.sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cr.cancel = cancel
	served := make(chan struct{})
	t.Cleanup(func() {
		for _, sess := range cr.srv.Registry.List() {
			if cr.srv.Registry.Remove(sess) && sess.peer != nil {
				sess.peer.Close()
			}
		}
		cr.srv.Registry.saveMu.Lock()
		cancel()
		<-served
	})
	cr.srv.Recover(ctx)
	go func() { defer close(served); _ = cr.srv.Serve(ctx, ln) }()
	return cr
}

func (cr *contractRig) call(t *testing.T, req Request) Response {
	t.Helper()
	resp, err := Call(cr.sock, req, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// waitForIssued blocks until the background login poller (pollLogin's 2s->10s backoff) installs
// the machine credential, or fails the test — longer than the package's own waitFor, since a
// real broker round trip plus that backoff can outrun a 2 s deadline.
func waitForIssued(t *testing.T, b *Broker) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if b.LoginStatus().State == "issued" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("login never reached issued: %+v", b.LoginStatus())
}

// --- wire-shape mirrors of the shared broker contract
// (dispatch://AGENTC-393/artifact/plan-overview-md), for decoding the real broker's responses ---

type contractSelfResponse struct {
	EnrollmentID string `json:"enrollment_id"`
	Kind         string `json:"kind"`
	Operator     string `json:"operator"`
}

func contractDecode[T any](t *testing.T, body []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, body)
	}
	return v
}

// TestContractLoginApprovalEnrollSignAndExpiry drives helper.Broker/Server against a real broker
// (brokertest.NewRig) end to end: Broker.Login -> a human approves the pending machine login
// through the real UI routes, by its typed code and the operator's login -> LoginStatus reaches
// issued -> EnrollBox mints a live enrollment -> a session register (the existing path) enrolls
// kind host -> sign produces a proof the real broker's own /v1/enrollments/self accepts -> the
// launcher credential is expired by direct SQL -> the next EnrollBox names the login command
// again. It also proves the machine key, the session key, and every proof this test produced
// never touch disk (neither an arbitrary $HOME nor the registry's own persisted sessions.json
// state file) or a log line.
func TestContractLoginApprovalEnrollSignAndExpiry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cr := newContractRig(t)
	b := cr.srv.Broker

	// --- Login (machine side) ---
	code, err := b.Login(context.Background(), cr.srv.Hostname)
	if err != nil {
		t.Fatalf("Broker.Login: %v", err)
	}
	if code == "" {
		t.Fatal("Login returned no confirmation code")
	}

	// --- approve (human/operator side, over the rig's own UI-bearer HTTP calls) ---
	credentialID := cr.broker.DecideMachineLogin(t, code, true)

	// --- LoginStatus reaches issued ---
	waitForIssued(t, b)
	cred := b.cred.Load()
	if cred == nil || cred.id != credentialID {
		t.Fatalf("installed credential id %v, want %s", cred, credentialID)
	}

	// --- EnrollBox: a live enrollment ---
	boxKey, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	boxThumbprint, err := proof.Thumbprint(&boxKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	boxEnrollmentID, _, err := b.EnrollBox(context.Background(), "box-"+t.Name(), boxThumbprint, nil)
	if err != nil {
		t.Fatalf("EnrollBox: %v", err)
	}
	if boxEnrollmentID == "" {
		t.Fatal("EnrollBox returned no enrollment id")
	}

	// --- a session register (the existing path) enrolls kind host ---
	reg := cr.call(t, Request{Op: "register", WaitSeconds: 10})
	if !reg.OK || reg.State != "enrolled" || reg.EnrollmentID == "" || reg.Operator != cr.broker.Operator {
		t.Fatalf("register: %+v", reg)
	}
	want := "contract-test-host:" + strconv.Itoa(os.Getpid()) + ":"
	if len(reg.RuntimeID) <= len(want) || reg.RuntimeID[:len(want)] != want {
		t.Fatalf("runtime_id %q must be <hostname>:<pid>:<ticks>", reg.RuntimeID)
	}

	// --- sign produces a proof the real broker's /v1/enrollments/self accepts ---
	signed := cr.call(t, Request{Op: "sign", Method: http.MethodGet, URL: cr.broker.URL + "/v1/enrollments/self"})
	if !signed.OK || signed.Proof == "" || signed.EnrollmentID != reg.EnrollmentID {
		t.Fatalf("sign: %+v", signed)
	}
	status, body := cr.broker.Req(t, http.MethodGet, "/v1/enrollments/self", map[string]string{"Proof": signed.Proof}, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/enrollments/self (real broker) = %d: %s", status, body)
	}
	self := contractDecode[contractSelfResponse](t, body)
	if self.EnrollmentID != reg.EnrollmentID || self.Kind != "host" || self.Operator != cr.broker.Operator {
		t.Fatalf("self = %+v, want enrollment_id=%s kind=host operator=%s", self, reg.EnrollmentID, cr.broker.Operator)
	}

	// --- expire the credential by direct SQL; the broker needs no restart: expiry is read from
	// the row on every AuthenticateLauncher call, never cached in memory ---
	if _, err := cr.broker.Store.Pool.Exec(context.Background(),
		`update launcher_credentials set expires_at = now() - interval '1 minute' where id = $1::uuid`, credentialID); err != nil {
		t.Fatalf("expire launcher credential: %v", err)
	}

	// --- the next EnrollBox names the login command ---
	secondKey, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	secondThumbprint, err := proof.Thumbprint(&secondKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	refused := cr.call(t, Request{Op: "enroll-box", RuntimeID: "box-2-" + t.Name(), Thumbprint: secondThumbprint})
	if refused.OK || refused.Code != CodeEnrollFailed || !strings.Contains(refused.Error, "agent-secrets launcher login") {
		t.Fatalf("enroll-box after expiry: %+v, want %s naming the login command", refused, CodeEnrollFailed)
	}
	if b.cred.Load() != nil {
		t.Fatal("an expired credential (401 LAUNCHER_INVALID) must clear the in-memory credential")
	}
	if status := cr.call(t, Request{Op: "login-status"}); !status.OK || status.LoginState != "expired" || !status.LoginRefused {
		t.Fatalf("login-status after the broker refused the expired credential: %+v, want expired and refused", status)
	}

	// --- neither the machine key, the session key, nor any proof this test produced ever
	// touches disk or a log line; the credential id itself is not secret, so it is never
	// asserted absent ---
	sess := cr.srv.Registry.Get(os.Getpid())
	if sess == nil {
		t.Fatal("the registered session must still be resolvable by this test's own pid")
	}
	secrets := [][]byte{cred.key.D.Bytes(), sess.Key.D.Bytes(), boxKey.D.Bytes(), secondKey.D.Bytes()}
	logText := cr.logBuf.String()
	for _, raw := range secrets {
		for _, encoded := range []string{hex.EncodeToString(raw), base64.StdEncoding.EncodeToString(raw)} {
			if strings.Contains(logText, encoded) {
				t.Fatalf("a private key's raw bytes appear in the log: %q", encoded)
			}
		}
	}
	for _, token := range []string{code, signed.Proof} {
		if token != "" && strings.Contains(logText, token) {
			t.Fatalf("a bearer-shaped token appears in the log verbatim: %q", token)
		}
	}
	if strings.Contains(logText, "Bearer") {
		t.Fatal("the log must never carry a bearer-shaped token (this design has none)")
	}

	// --- the persisted session-registry state file (sessions.json) must never carry key
	// material either: Record has no key field today, but a future regression that serialized
	// one in would land here, not just in the log. register already triggered a Save once this
	// session enrolled; call it again for a deterministic read of the current state. ---
	if err := cr.srv.Registry.Save(); err != nil {
		t.Fatalf("save session state file: %v", err)
	}
	stateBytes, err := os.ReadFile(cr.sessionsPath)
	if err != nil {
		t.Fatalf("read session state file: %v", err)
	}
	stateText := string(stateBytes)
	for _, raw := range secrets {
		for _, encoded := range []string{hex.EncodeToString(raw), base64.StdEncoding.EncodeToString(raw)} {
			if strings.Contains(stateText, encoded) {
				t.Fatalf("a private key's raw bytes appear in the persisted session state file: %q", encoded)
			}
		}
	}

	files := 0
	if err := filepath.WalkDir(home, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if files != 0 {
		t.Fatalf("nothing must ever write under HOME; found %d file(s)", files)
	}
}
