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
	machineFile  string
	logBuf       *syncBuffer
	cancel       context.CancelFunc
}

// newContractRig mounts a real broker (brokertest.NewRig, with options) and serves a helper
// against it (startContractHelper) with a machine file of its own.
func newContractRig(t *testing.T, options ...brokertest.Option) *contractRig {
	t.Helper()
	return startContractHelper(t, brokertest.NewRig(t, options...), filepath.Join(t.TempDir(), "agent-secrets", "machine.json"), http.DefaultClient)
}

// startContractHelper serves this package's own Server on a real unix socket, its Broker pointed
// at broker's URL and OperatorFile, keeping its login at machineFile and sending its broker calls
// through client, both logging to logBuf through one logger, and starts it as
// cmd/agent-secrets-helper does: the Broker resumes any login machineFile holds, then the Server
// recovers its sessions and listens. Every registry/operator artifact lives under its own TempDir
// and the socket under a short temporary directory of its own, never under $HOME — the caller is
// expected to have set HOME to a separate, otherwise-untouched TempDir of its own so it can later
// assert nothing wrote there.
func startContractHelper(t *testing.T, broker *brokertest.Rig, machineFile string, client *http.Client) *contractRig {
	t.Helper()
	artifacts := t.TempDir()
	logBuf := &syncBuffer{}
	sessionsPath := filepath.Join(artifacts, "sessions.json")
	cr := &contractRig{broker: broker, sessionsPath: sessionsPath, machineFile: machineFile, logBuf: logBuf}
	log := slog.New(slog.NewTextHandler(logBuf, nil))
	cr.srv = &Server{
		Registry: NewRegistry(sessionsPath),
		Broker:   &Broker{URL: broker.URL, OperatorFile: broker.OperatorFile, MachineFile: machineFile, HTTP: client, Log: log},
		Hostname: "contract-test-host",
		PeerOf:   PeerOf,
		Log:      log,
		MinRenew: 50 * time.Millisecond,
	}
	cr.sock = filepath.Join(shortTempDir(t), "helper.sock")
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
		cr.srv.Registry.Close()
		cancel()
		<-served
		cr.srv.Broker.Close()
	})
	if err := cr.srv.Broker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	cr.srv.Recover(ctx)
	go func() { defer close(served); _ = cr.srv.Serve(ctx, ln) }()
	return cr
}

// login logs cr's helper in, has the rig's operator approve it, and waits for the credential.
func (cr *contractRig) login(t *testing.T) string {
	t.Helper()
	code, err := cr.srv.Broker.Login(context.Background(), cr.srv.Hostname)
	if err != nil {
		t.Fatalf("Broker.Login: %v", err)
	}
	credentialID := cr.broker.DecideMachineLogin(t, code, true)
	waitForIssued(t, cr.srv.Broker)
	return credentialID
}

func (cr *contractRig) call(t *testing.T, req Request) Response {
	t.Helper()
	resp, err := Call(cr.sock, req, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// waitForIssued blocks until the background login poller (pollLogin, at the broker's 5 s
// interval) installs the machine credential, or fails the test — longer than the package's own
// waitFor, since a real broker round trip plus that interval can outrun a 5 s deadline.
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

// --- wire-shape mirrors of the broker's HTTP API, for decoding the real broker's responses ---

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
// issued -> EnrollSessionKey mints a live enrollment -> a session register (the existing path)
// enrolls kind session -> sign produces a proof the real broker's own /v1/enrollments/self
// accepts -> the launcher credential is expired by direct SQL -> the next EnrollSessionKey names the login command
// again. It also proves the machine key, the session key, and every proof this test produced
// never reach a log line or the registry's own persisted sessions.json state file, and that
// nothing is written under $HOME but the machine file the Broker names (here outside it).
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

	// --- EnrollSessionKey: a live enrollment ---
	boxKey, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	boxThumbprint, err := proof.Thumbprint(&boxKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	boxEnrollmentID, _, err := b.EnrollSessionKey(context.Background(), "box-"+t.Name(), boxThumbprint, nil)
	if err != nil {
		t.Fatalf("EnrollSessionKey: %v", err)
	}
	if boxEnrollmentID == "" {
		t.Fatal("EnrollSessionKey returned no enrollment id")
	}

	// --- a session register (the existing path) enrolls kind session ---
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
	if self.EnrollmentID != reg.EnrollmentID || self.Kind != "session" || self.Operator != cr.broker.Operator {
		t.Fatalf("self = %+v, want enrollment_id=%s kind=session operator=%s", self, reg.EnrollmentID, cr.broker.Operator)
	}

	// --- expire the credential by direct SQL; the broker needs no restart: expiry is read from
	// the row on every AuthenticateLauncher call, never cached in memory ---
	if _, err := cr.broker.Store.Pool.Exec(context.Background(),
		`update launcher_credentials set expires_at = now() - interval '1 minute' where id = $1::uuid`, credentialID); err != nil {
		t.Fatalf("expire launcher credential: %v", err)
	}

	// --- the next EnrollSessionKey names the login command ---
	secondKey, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	secondThumbprint, err := proof.Thumbprint(&secondKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	refused := cr.call(t, Request{Op: "enroll-session-key", RuntimeID: "box-2-" + t.Name(), Thumbprint: secondThumbprint})
	if refused.OK || refused.Code != CodeEnrollFailed || !strings.Contains(refused.Error, "agent-secrets machine login") {
		t.Fatalf("enroll-session-key after expiry: %+v, want %s naming the login command", refused, CodeEnrollFailed)
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

// TestContractARefusedLauncherCredentialIsAnErrorAndTheHelperKeepsServing reproduces a machine
// login dying under a running helper, against a real broker: the credential passes its expiry
// on the broker (direct SQL, as BROKER_LAUNCHER_CREDENTIAL_SECONDS elapsing would) and a session
// registers after it. The helper keeps serving — register, sessions and login-status all answer —
// but says at ERROR that the broker refused the credential and that the session cannot enroll and
// why, where a minute-by-minute stream of retry warnings once hid it for hours.
func TestContractARefusedLauncherCredentialIsAnErrorAndTheHelperKeepsServing(t *testing.T) {
	cr := newContractRig(t)
	b := cr.srv.Broker
	code, err := b.Login(context.Background(), cr.srv.Hostname)
	if err != nil {
		t.Fatalf("Broker.Login: %v", err)
	}
	credentialID := cr.broker.DecideMachineLogin(t, code, true)
	waitForIssued(t, b)
	if _, err := cr.broker.Store.Pool.Exec(context.Background(),
		`update launcher_credentials set expires_at = now() - interval '1 minute' where id = $1::uuid`, credentialID); err != nil {
		t.Fatalf("expire launcher credential: %v", err)
	}

	if reg := cr.call(t, Request{Op: "register"}); !reg.OK || reg.EnrollmentID != "" {
		t.Fatalf("register after the credential expired: %+v; want OK and not enrolled", reg)
	}
	const cannotEnroll = `level=ERROR msg="session cannot enroll: the helper holds no launcher credential; run: agent-secrets machine login, and have a human approve it"`
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(cr.logBuf.String(), cannotEnroll) {
		if time.Now().After(deadline) {
			t.Fatalf("no ERROR says the session cannot enroll; log:\n%s", cr.logBuf.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	log := cr.logBuf.String()
	refused := `level=ERROR msg="the broker refused the launcher credential (expired or revoked, or a proof it could not verify, such as clock skew or an AGENT_SECRETS_URL mismatch); cleared: no session can enroll until a human approves a new machine login (run: agent-secrets machine login)" credential_id=` + credentialID + " code=LAUNCHER_INVALID"
	why := `why="the broker refused the launcher credential (expired or revoked, or a proof it could not verify, such as clock skew or an AGENT_SECRETS_URL mismatch)"`
	if !strings.Contains(log, refused) || !strings.Contains(log, why) {
		t.Fatalf("log:\n%s\nwant %s, and the session's error naming %s", log, refused, why)
	}
	if strings.Contains(log, `level=WARN msg="enroll failed; retrying"`) {
		t.Fatalf("a missing credential is an ERROR, never a retry warning; log:\n%s", log)
	}

	if sessions := cr.call(t, Request{Op: "sessions"}); !sessions.OK || len(sessions.Sessions) != 1 || sessions.Sessions[0].State != "enrolling" {
		t.Fatalf("sessions after the refusal: %+v; want the one session, still enrolling", sessions)
	}
	if status := cr.call(t, Request{Op: "login-status"}); !status.OK || status.LoginState != "expired" || !status.LoginRefused || status.CredentialDropped != dropRefused || status.CredentialHeld {
		t.Fatalf("login-status after the refusal: %+v; want expired, refused by the broker, none held", status)
	}
}

// TestContractLoginStatusNamesWhenTheLauncherCredentialExpires: the real broker's issued poll
// carries the minted credential's expiry, and login-status hands it on, so a human sees the
// deadline for the next machine login before the credential lapses.
func TestContractLoginStatusNamesWhenTheLauncherCredentialExpires(t *testing.T) {
	cr := newContractRig(t)
	b := cr.srv.Broker
	code, err := b.Login(context.Background(), cr.srv.Hostname)
	if err != nil {
		t.Fatalf("Broker.Login: %v", err)
	}
	credentialID := cr.broker.DecideMachineLogin(t, code, true)
	waitForIssued(t, b)
	var minted time.Time
	if err := cr.broker.Store.Pool.QueryRow(context.Background(), `select expires_at from launcher_credentials where id = $1::uuid`, credentialID).Scan(&minted); err != nil {
		t.Fatalf("read the minted credential's expiry: %v", err)
	}
	status := cr.call(t, Request{Op: "login-status"})
	if want := minted.UTC().Format(time.RFC3339); !status.OK || !status.CredentialHeld || status.CredentialExpiresAt != want {
		t.Fatalf("login-status: %+v; want the credential held, expiring at %s", status, want)
	}
}

// TestContractRevokingTheMachineLoginEndsItsSessionsAndTheHelperDropsIt: the person who approved
// the machine login a running helper holds revokes it, as Dispatch's machine-login page does (the
// broker's UI route). Nothing tells the helper, and nothing needs to: the revoke ends the helper's
// session, so the session's next renew is refused; revoking that lapsed enrollment through the
// launcher route is refused 401 LAUNCHER_INVALID, so the helper drops the credential, says at
// ERROR that the broker refused it, and login-status reports no credential held.
func TestContractRevokingTheMachineLoginEndsItsSessionsAndTheHelperDropsIt(t *testing.T) {
	cr := newContractRig(t, brokertest.WithLease(3*time.Second))
	b := cr.srv.Broker
	code, err := b.Login(context.Background(), cr.srv.Hostname)
	if err != nil {
		t.Fatalf("Broker.Login: %v", err)
	}
	credentialID := cr.broker.DecideMachineLogin(t, code, true)
	waitForIssued(t, b)
	reg := cr.call(t, Request{Op: "register", WaitSeconds: 10})
	if !reg.OK || reg.State != "enrolled" || reg.EnrollmentID == "" {
		t.Fatalf("register: %+v, want the session enrolled", reg)
	}

	status, body := cr.broker.UI(t, http.MethodPost, "/v1/launcher-credentials/"+credentialID+"/revoke-by-approver",
		map[string]any{"approver": cr.broker.Operator})
	if status != http.StatusOK {
		t.Fatalf("revoke the machine login = %d: %s", status, body)
	}

	refused := `level=ERROR msg="the broker refused the launcher credential (expired or revoked, or a proof it could not verify, such as clock skew or an AGENT_SECRETS_URL mismatch); cleared: no session can enroll until a human approves a new machine login (run: agent-secrets machine login)" credential_id=` + credentialID + " code=LAUNCHER_INVALID"
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(cr.logBuf.String(), refused) {
		if time.Now().After(deadline) {
			t.Fatalf("the helper never logged its credential refused; log:\n%s", cr.logBuf.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	log := cr.logBuf.String()
	if !strings.Contains(log, `msg="renew refused; revoking the lapsed enrollment before enrolling again"`) ||
		!strings.Contains(log, "code=ENROLLMENT_NOT_LIVE enrollment_id="+reg.EnrollmentID) {
		t.Fatalf("log:\n%s\nwant the session's renew refused ENROLLMENT_NOT_LIVE for %s", log, reg.EnrollmentID)
	}
	if status := cr.call(t, Request{Op: "login-status"}); !status.OK || status.CredentialHeld || !status.LoginRefused || status.CredentialDropped != dropRefused || status.LoginState != "expired" {
		t.Fatalf("login-status after the revoke: %+v; want expired, refused by the broker, none held", status)
	}
	if sessions := cr.call(t, Request{Op: "sessions"}); !sessions.OK || len(sessions.Sessions) != 1 || sessions.Sessions[0].State != "enrolling" {
		t.Fatalf("sessions after the revoke: %+v; want the one session, no longer enrolled", sessions)
	}
}

// TestContractSignLauncherProvesTheOperatorRoutes: a launcher proof sign-launcher signs for the
// operator's shell is one the real broker's operator routes accept. GET /v1/operator/machines
// answers this machine's own login, the held credential on this helper's host; and since the proof
// names its method and URL, the broker refuses one replayed against another route.
func TestContractSignLauncherProvesTheOperatorRoutes(t *testing.T) {
	cr := newContractRig(t)
	b := cr.srv.Broker
	code, err := b.Login(context.Background(), cr.srv.Hostname)
	if err != nil {
		t.Fatalf("Broker.Login: %v", err)
	}
	credentialID := cr.broker.DecideMachineLogin(t, code, true)
	waitForIssued(t, b)

	signed := cr.call(t, Request{Op: "sign-launcher", Method: http.MethodGet, URL: cr.broker.URL + "/v1/operator/machines"})
	if !signed.OK || signed.Proof == "" {
		t.Fatalf("sign-launcher: %+v", signed)
	}
	status, body := cr.broker.Req(t, http.MethodGet, "/v1/operator/machines", map[string]string{"Proof": signed.Proof}, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/operator/machines (real broker) = %d: %s", status, body)
	}
	machines := contractDecode[struct {
		Credentials []struct {
			CredentialID string `json:"credential_id"`
			Host         string `json:"host"`
		} `json:"credentials"`
	}](t, body)
	if len(machines.Credentials) != 1 || machines.Credentials[0].CredentialID != credentialID || machines.Credentials[0].Host != cr.srv.Hostname {
		t.Fatalf("machines = %s, want this machine's login %s on %s alone", body, credentialID, cr.srv.Hostname)
	}

	grants := cr.call(t, Request{Op: "sign-launcher", Method: http.MethodGet, URL: cr.broker.URL + "/v1/operator/grants"})
	if status, body := cr.broker.Req(t, http.MethodGet, "/v1/operator/machines", map[string]string{"Proof": grants.Proof}, nil); status != http.StatusUnauthorized {
		t.Fatalf("a proof for /v1/operator/grants sent to /v1/operator/machines = %d: %s, want 401", status, body)
	}
}

// TestContractASessionIDNamedAtRegisterReachesTheApprovalPage is the live proof for the session
// a credential request came from: against a real broker, a session registered with
// `--session-id` (what the launcher names) enrolls under that id, and the credential request it
// then makes is listed to its approver with the id in the `session.enrollment` field Dispatch's
// approval page reads. A second session on the same broker, registered naming no id, is listed
// beside it with both ids null — a page that says no session is named, rather than one that
// names the wrong session.
func TestContractASessionIDNamedAtRegisterReachesTheApprovalPage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cr := newContractRig(t)
	code, err := cr.srv.Broker.Login(context.Background(), cr.srv.Hostname)
	if err != nil {
		t.Fatalf("Broker.Login: %v", err)
	}
	cr.broker.DecideMachineLogin(t, code, true)
	waitForIssued(t, cr.srv.Broker)

	// Two sessions of their own, neither inside the other: this test process registers nothing,
	// so each child process is a session root, as two launched agents on one machine are.
	named, anonymous := sleeper(t), sleeper(t)
	const sessionID = "01a13dca-e979-7264-af32-f654ef57a6f5"

	cr.actAs(named.Process.Pid)
	reg := cr.call(t, Request{Op: "register", WaitSeconds: 10, SessionID: new(sessionID)})
	if !reg.OK || reg.State != "enrolled" || reg.SessionID != sessionID {
		t.Fatalf("register: %+v", reg)
	}
	if got := cr.enrollmentSessionID(t, reg.EnrollmentID); got == nil || *got != sessionID {
		t.Fatalf("enrollments.session_id = %v, want %s", got, sessionID)
	}
	record := cr.requestSecret(t, "TEST_SECRET", "the live proof's own request")
	if got := cr.pendingSession(t, record); got.Enrollment == nil || *got.Enrollment != sessionID {
		t.Fatalf("the pending record's session.enrollment = %v, want %s", got.Enrollment, sessionID)
	}
	// The record page a person opens from that row reads the same field; its body is the
	// evidence for what the page shows.
	status, body := cr.broker.UI(t, http.MethodGet, "/v1/credential-requests/"+record, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/credential-requests/%s = %d: %s", record, status, body)
	}
	if got := contractDecode[struct {
		Session contractRecordSession `json:"session"`
	}](t, body).Session; got.Enrollment == nil || *got.Enrollment != sessionID {
		t.Fatalf("the record page's session.enrollment = %v, want %s", got.Enrollment, sessionID)
	}
	t.Logf("the record Dispatch's approval page reads: %s", body)

	// A session whose launcher names none: it registers, enrolls and gets its request listed,
	// with no id anywhere — the page says no session is named rather than naming a wrong one.
	cr.actAs(anonymous.Process.Pid)
	second := cr.call(t, Request{Op: "register", WaitSeconds: 10})
	if !second.OK || second.State != "enrolled" || second.SessionID != "" || second.EnrollmentID == reg.EnrollmentID {
		t.Fatalf("register with no session id: %+v", second)
	}
	if got := cr.enrollmentSessionID(t, second.EnrollmentID); got != nil {
		t.Fatalf("an enrollment whose launcher named no session must store none, got %q", *got)
	}
	secondRecord := cr.requestSecret(t, "TEST_SECRET", "the live proof's second request")
	if secondRecord == record {
		t.Fatal("the second session's request must be its own record")
	}
	if got := cr.pendingSession(t, secondRecord); got.Enrollment != nil || got.Request != nil {
		t.Fatalf("a request from a session with no id must name none: %+v", got)
	}
}

// actAs makes every later socket call present pid as its peer, as that process calling
// agent-secrets would: the contract rig's own peer is this test process, which registers nothing
// here so that each child is a session root of its own.
func (cr *contractRig) actAs(pid int) {
	cr.srv.PeerOf = func(*net.UnixConn) (*Peer, error) { return PinPID(pid) }
}

// enrollmentSessionID is the broker's own enrollments.session_id for id, the column the approval
// page's join reads as the session that enrolled.
func (cr *contractRig) enrollmentSessionID(t *testing.T, id string) *string {
	t.Helper()
	var sessionID *string
	if err := cr.broker.Store.Pool.QueryRow(context.Background(),
		`select session_id from enrollments where id = $1::uuid`, id).Scan(&sessionID); err != nil {
		t.Fatalf("read enrollment %s: %v", id, err)
	}
	return sessionID
}

// requestSecret makes the calling session's credential request for name through the helper (the
// request object and the call's proof are both the helper's, as the CLI makes them) and returns
// the record id its approver decides on. The request body names no session_id of its own, so the
// record's `session` carries only what the enrollment stated.
func (cr *contractRig) requestSecret(t *testing.T, name, reason string) string {
	t.Helper()
	signed := cr.call(t, Request{Op: "sign-request", Secrets: []string{name}, Reason: reason})
	if !signed.OK || signed.RequestObject == "" {
		t.Fatalf("sign-request: %+v", signed)
	}
	call := cr.call(t, Request{Op: "sign", Method: http.MethodPost, URL: cr.broker.URL + "/v1/requests"})
	if !call.OK || call.Proof == "" {
		t.Fatalf("sign: %+v", call)
	}
	status, body := cr.broker.Req(t, http.MethodPost, "/v1/requests", map[string]string{"Proof": call.Proof},
		map[string]any{"request": signed.RequestObject, "session_id": nil})
	if status != http.StatusOK {
		t.Fatalf("POST /v1/requests = %d: %s", status, body)
	}
	created := contractDecode[struct {
		State    string  `json:"state"`
		RecordID *string `json:"record_id"`
	}](t, body)
	if created.State != "pending" || created.RecordID == nil {
		t.Fatalf("create = %+v, want a pending record", created)
	}
	return *created.RecordID
}

// pendingSession is the `session` field GET /v1/pending carries for recordID: the two ids
// Dispatch's approval page renders, read from the broker's own UI route.
func (cr *contractRig) pendingSession(t *testing.T, recordID string) contractRecordSession {
	t.Helper()
	status, body := cr.broker.UI(t, http.MethodGet, "/v1/pending?approver="+cr.broker.Operator, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/pending = %d: %s", status, body)
	}
	for _, entry := range contractDecode[struct {
		Pending []struct {
			RecordID string                `json:"record_id"`
			Session  contractRecordSession `json:"session"`
		} `json:"pending"`
	}](t, body).Pending {
		if entry.RecordID == recordID {
			return entry.Session
		}
	}
	t.Fatalf("GET /v1/pending = %s, want record %s listed", body, recordID)
	return contractRecordSession{}
}

// contractRecordSession mirrors the broker's recordSessionResp, the shape both the pending list
// and the record page carry.
type contractRecordSession struct {
	Request    *string `json:"request"`
	Enrollment *string `json:"enrollment"`
}
