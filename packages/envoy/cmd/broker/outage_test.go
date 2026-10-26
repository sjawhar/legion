package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// TestABrokerOutageLongerThanALease reproduces dispatch INFRA-487, the 2026-10-26 broker cutover:
// no broker served for longer than a pod's enrollment lease, and the first sweep of the broker
// that came back ended every enrollment whose lease ran out while none was serving, so each pod's
// `agent-secrets renew` was refused ENROLLMENT_NOT_LIVE and exited 76, and the pod was refused
// every secret until it was relaunched. It runs the REAL broker binary twice on one database and
// the REAL agent-secrets binary, with one local OIDC issuer standing in for the cluster's token
// issuer the whole time, and a lease of 45 seconds, a sweep of one and no drain. Both brokers are
// reached at one address a TCP forwarder the test owns holds across the restart, as a Service's
// is: BROKER_PUBLIC_URL and AGENT_SECRETS_URL both name it, so a proof's htu is as good after the
// restart as before, and while no broker runs the forwarder refuses every connection at once. The
// outage runs from the first broker's exit until a lease, the three sweeps the broker tolerates
// between two records that it serves, and the renewer's longest retry wait have all passed.
//
// Four pods of one service, enrolled by its launcher and granted its agent-tier secret with no
// approver, prove one thing each. The survivor is the outage itself: keyed by `agent-secrets
// keygen` and enrolled into its key dir as the worker shim's pods are, it renews through
// `agent-secrets renew --pod-token-file` from before the outage to the end. Once the second broker
// listens its first fetch through the CLI is served, its renewer, which never exited, renews its
// lease to a whole one within a third of a lease, and the second broker logs that it gave back the
// time no broker was serving. The revoked pod, ended by its launcher, and the lapsed pod, whose
// lease the first broker's sweeper ended while that broker served, prove the time given back
// revives nothing that had ended: each is refused a fetch, PROOF_INVALID not live, and a renewal,
// ENROLLMENT_NOT_LIVE. The mismatch pod, whose lease ran out in the outage as the survivor's did,
// proves a lease given back renews for no one else: a renewal carrying another pod's token, or
// another service account's, is refused POD_IDENTITY_MISMATCH and moves its lease not at all. The
// broker's log line is read last, so that against a broker without the fix the test fails on the
// refusal the pods met rather than on a missing line.
func TestABrokerOutageLongerThanALease(t *testing.T) {
	databaseURL := storetest.URL(t)
	issuer := oidctest.New(t)
	podKey := issuer.PublishKey(t, "signing-key")
	const (
		podAudience  = "example-broker-pod"
		service      = "example-service"
		loginAccount = "system:serviceaccount:example-system:example-service-login"
		podAccount   = "system:serviceaccount:example:example-worker"
		otherAccount = "system:serviceaccount:example:example-other-worker"
		secretName   = "OUTAGE_TOKEN"
		secretValue  = "outage-token-value"
		// lease is BROKER_LEASE_SECONDS. A renewer renews once a third of it has passed, so a pod
		// that renews holds at least two thirds of it.
		lease = 45 * time.Second
		// notServing is the longest gap between two records that a broker serves which still
		// counts as one serving: three sweeps of BROKER_SWEEP_SECONDS=1.
		notServing = 3 * time.Second
		// renewerLongestWait is agent-secrets renew's longest wait between two failed attempts
		// (nextBackoff's cap), so a renewer whose broker is back renews within it.
		renewerLongestWait = 10 * time.Second
	)
	fakeSecretsFile := filepath.Join(t.TempDir(), "fake-secrets.json")
	fakeSecrets := `{"secrets": [
		{"name": "` + testSecretsPrefix + `outage-token", "kms_key_id": "` + testSecretsKMSKeyARN + `", "tags": {"owner": "` + service + `", "tier": "agent"}, "value": "` + secretValue + `"}
	]}`
	if err := os.WriteFile(fakeSecretsFile, []byte(fakeSecrets), 0o600); err != nil {
		t.Fatal(err)
	}
	brokerBinary := buildBroker(t)
	cli := buildAgentSecrets(t)
	// Started before either broker, so its cleanup runs after theirs and never waits on a
	// connection to a broker still running.
	front := startForwarder(t)
	env := []string{
		"BROKER_DATABASE_URL=" + databaseURL,
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=" + front.url,
		"BROKER_FAKE_SECRETS_FILE=" + fakeSecretsFile,
		"BROKER_K8S_OIDC_ISSUER=" + issuer.URL(),
		"BROKER_K8S_OIDC_AUDIENCE=" + podAudience,
		"BROKER_SERVICE_LOGINS=" + service + "=" + loginAccount,
		"BROKER_SERVICES=" + service + "=" + podAccount,
		"BROKER_LEASE_SECONDS=45",
		"BROKER_SWEEP_SECONDS=1",
		"BROKER_DRAIN_SECONDS=0",
	}
	serve := func() *brokerProcess {
		t.Helper()
		process := startBroker(t, brokerOptions{Binary: brokerBinary, Env: env})
		front.route(process.Addr)
		return process
	}
	first := serve()
	broker := podBroker{t: t, base: front.url, issuer: issuer, key: podKey, audience: podAudience}

	ctx := context.Background()
	db, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect to the brokers' database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close(ctx) })
	// leaseOf answers an enrollment's lease_expires_at and the database's now(), so every
	// comparison of the two is on Postgres's clock, the one the broker's liveness reads.
	leaseOf := func(enrollmentID string) (expires, now time.Time) {
		t.Helper()
		if err := db.QueryRow(ctx, `select lease_expires_at, now() from enrollments where id = $1`, enrollmentID).Scan(&expires, &now); err != nil {
			t.Fatalf("read the lease of enrollment %s: %v", enrollmentID, err)
		}
		return expires, now
	}
	podToken := func(uid, account string) string {
		claims := issuer.Claims(account, podAudience)
		claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": uid}}
		return issuer.Mint(t, podKey, claims)
	}

	t.Log("--- the service logs in with its token")
	launcher := broker.logIn(service, loginAccount)

	t.Log("--- the survivor: a key from keygen, enrolled by the launcher into its key dir, a renewer, and a fetch through the CLI")
	keyDir := filepath.Join(t.TempDir(), "survivor")
	keygen := exec.Command(cli, "keygen", "--out", keyDir)
	keygen.Env = agentSecretsEnv(front.url, keyDir)
	thumbprint, err := keygen.Output()
	if err != nil {
		t.Fatalf("agent-secrets keygen --out %s: %v", keyDir, err)
	}
	const survivorUID = "survivor-pod"
	survivorID := enrollKey(broker, launcher, survivorUID, strings.TrimSpace(string(thumbprint)), podToken(survivorUID, podAccount))
	writeWhole(t, filepath.Join(keyDir, "enrollment"), survivorID+"\n")
	tokenFile := filepath.Join(t.TempDir(), "token")
	writeWhole(t, tokenFile, podToken(survivorUID, podAccount))
	renew := exec.Command(cli, "renew", "--pod-token-file", tokenFile)
	renew.Env = agentSecretsEnv(front.url, keyDir)
	renewer := cmdtest.Start(t, "agent-secrets renew", renew)
	renewerRunning := func(when string) {
		t.Helper()
		select {
		case <-renewer.Exited:
			t.Fatalf("the survivor's renewer exited %s (%v):\n%s", when, renewer.Cmd.ProcessState, renewer.Output.String())
		default:
		}
	}
	fetch := func(when string) {
		t.Helper()
		started := time.Now()
		commandCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		command := exec.CommandContext(commandCtx, cli, secretName, "--", "sh", "-c", `printf %s "$`+secretName+`"`)
		command.Env = agentSecretsEnv(front.url, keyDir)
		var stdout, stderr strings.Builder
		command.Stdout, command.Stderr = &stdout, &stderr
		err := command.Run()
		took := time.Since(started)
		if err != nil || stdout.String() != secretValue {
			t.Fatalf("the survivor's fetch %s = %v, stdout %q, want %q; its stderr:\n%s\nits renewer's:\n%s",
				when, err, stdout.String(), secretValue, stderr.String(), renewer.Output.String())
		}
		t.Logf("the survivor's fetch %s was served in %s: %s", when, took.Round(time.Millisecond), strings.TrimSpace(stderr.String()))
	}
	fetch("before the outage")

	t.Log("--- the revoked pod: enrolled, then ended by its launcher")
	revoked := broker.enroll(launcher, "revoked-pod", podAccount)
	if status := deleteEnrollment(t, front.url, launcher, revoked.enrollmentID); status != http.StatusNoContent {
		t.Fatalf("the launcher's DELETE of the revoked pod's enrollment = %d, want 204", status)
	}

	t.Log("--- the lapsed pod: enrolled, its lease set in the past, then ended by the sweeper of a broker serving")
	lapsed := broker.enroll(launcher, "lapsed-pod", podAccount)
	if _, err := db.Exec(ctx, `update enrollments set lease_expires_at = now() - interval '1 second' where id = $1`, lapsed.enrollmentID); err != nil {
		t.Fatalf("set the lapsed pod's lease in the past: %v", err)
	}
	first.Await(t, "broker sweeper: ended an enrollment whose lease lapsed enrollment_id="+lapsed.enrollmentID)

	t.Log("--- the mismatch pod: enrolled, and served its secret once")
	mismatch := broker.enroll(launcher, "mismatch-pod", podAccount)
	reply := broker.ask(mismatch, secretName)
	if reply.State != "granted" || reply.GrantID == nil {
		t.Fatalf("the mismatch pod's request = %+v, want granted with a grant", reply)
	}
	if values := broker.values(mismatch, *reply.GrantID); values[secretName] != secretValue {
		t.Fatalf("the mismatch pod's grant values = %v, want %s=%s", values, secretName, secretValue)
	}

	// The margin: the broker is stopped while the survivor holds more than two thirds of its lease
	// but renewed at least five seconds ago, between two of its renewals. The time given back
	// restores the lease the survivor held at the first broker's last record that it served, at
	// most one sweep before the stop, so the restored survivor holds at least two thirds of a
	// lease, 30 s, less one beat, and its renewer, whose longest retry wait is 10 s, renews well
	// inside that. And the lease given back runs at least four seconds short of a whole one from
	// the restart, which is what tells the renewer's own renewal after the restart from it.
	t.Log("--- the outage: the broker is stopped between two of the survivor's renewals")
	for waited := time.Now(); ; {
		expires, now := leaseOf(survivorID)
		left := expires.Sub(now)
		if left > lease*2/3+time.Second && left <= lease-5*time.Second {
			t.Logf("the survivor holds %s of its lease at the stop", left.Round(time.Millisecond))
			break
		}
		if time.Since(waited) > lease {
			t.Fatalf("the survivor's renewer has not renewed its lease in a whole lease (it holds %s):\n%s", left, renewer.Output.String())
		}
		if left > lease-5*time.Second {
			time.Sleep(left - (lease - 5*time.Second) + 50*time.Millisecond)
		} else {
			time.Sleep(250 * time.Millisecond)
		}
	}
	front.route("")
	first.Terminate(t)
	if err := first.AwaitExit(t, 30*time.Second); err != nil {
		t.Fatalf("the first broker's exit = %v, want a clean one:\n%s", err, first.Log())
	}
	stopped := time.Now()
	outage := lease + notServing + renewerLongestWait
	t.Logf("the first broker exited; no broker serves for %s", outage)
	time.Sleep(time.Until(stopped.Add(outage)))
	renewerRunning("during the outage")

	t.Log("--- a second broker serves the same database at the same address")
	restarted := serve()
	restartedAt := time.Now()
	t.Logf("the second broker listens %s after the first exited", restartedAt.Sub(stopped).Round(time.Millisecond))
	var restartedDB time.Time
	if err := db.QueryRow(ctx, `select now()`).Scan(&restartedDB); err != nil {
		t.Fatal(err)
	}

	t.Log("--- (1) the survivor's first fetch through the CLI is served")
	fetch("after the restart")
	t.Logf("(1) served %s after the second broker listened", time.Since(restartedAt).Round(time.Millisecond))

	t.Log("--- (2) the survivor's renewer, which never exited, renews a whole lease within a third of one")
	for {
		expires, now := leaseOf(survivorID)
		if !expires.Before(restartedDB.Add(lease - time.Second)) {
			if left := expires.Sub(now); left < lease-2*time.Second || left > lease+time.Second {
				t.Fatalf("the survivor's renewed lease runs %s past now, want about %s", left, lease)
			}
			t.Logf("(2) renewed %s after the second broker listened, to %s past now", time.Since(restartedAt).Round(time.Millisecond), expires.Sub(now).Round(time.Millisecond))
			break
		}
		renewerRunning("after the restart")
		if time.Since(restartedAt) > lease/3 {
			t.Fatalf("the survivor's lease runs to %s, %s past the restart, %s after it: its renewer renewed nothing within a third of a lease:\n%s",
				expires, expires.Sub(restartedDB).Round(time.Millisecond), time.Since(restartedAt).Round(time.Millisecond), renewer.Output.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	renewerRunning("after it renewed")

	for _, ended := range []struct {
		name string
		pod  pod
	}{{"revoked", revoked}, {"lapsed", lapsed}} {
		t.Logf("--- (3) the %s pod stays ended: refused a fetch and a renewal", ended.name)
		request, err := record.Sign(ended.pod.key, front.url, []record.AuthorizationDetail{
			{Type: record.KindAgentSecret, Identifier: secretName, Actions: []string{"inject"}},
		}, ended.pod.runtimeID+" needs "+secretName, "", nil, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		status, answer := broker.signed(ended.pod, "/v1/requests", map[string]any{"request": request, "session_id": nil})
		if code, message := brokerRefusal(answer); status != http.StatusUnauthorized || code != "PROOF_INVALID" || !strings.Contains(message, "not live") {
			t.Fatalf("the %s pod's fetch = %d %s, want 401 PROOF_INVALID not live", ended.name, status, answer)
		}
		status, answer = broker.signed(ended.pod, "/v1/enrollments/"+ended.pod.enrollmentID+"/renew", nil)
		if code, _ := brokerRefusal(answer); status != http.StatusUnauthorized || code != "ENROLLMENT_NOT_LIVE" {
			t.Fatalf("the %s pod's renewal = %d %s, want 401 ENROLLMENT_NOT_LIVE", ended.name, status, answer)
		}
	}

	t.Log("--- (4) the mismatch pod's lease renews for no other pod and no other account")
	before, _ := leaseOf(mismatch.enrollmentID)
	for _, other := range []struct{ what, token string }{
		{"another pod's UID", podToken("another-pod", podAccount)},
		{"another service account", podToken(mismatch.runtimeID, otherAccount)},
	} {
		status, answer := broker.signed(mismatch, "/v1/enrollments/"+mismatch.enrollmentID+"/renew", map[string]any{"pod_token": other.token})
		if code, _ := brokerRefusal(answer); status != http.StatusForbidden || code != "POD_IDENTITY_MISMATCH" {
			t.Fatalf("the mismatch pod's renewal with a token naming %s = %d %s, want 403 POD_IDENTITY_MISMATCH", other.what, status, answer)
		}
	}
	if after, _ := leaseOf(mismatch.enrollmentID); !after.Equal(before) {
		t.Fatalf("the mismatch pod's lease moved from %s to %s on two refused renewals", before, after)
	}

	t.Log("--- (5) the second broker logged that it gave back the time no broker was serving")
	const gaveBack = "gave back the time no broker was serving"
	for _, line := range strings.Split(restarted.Await(t, gaveBack), "\n") {
		if strings.Contains(line, gaveBack) {
			t.Logf("(5) %s", line)
		}
	}
	renewerRunning("at the end")
}

// buildAgentSecrets builds cmd/agent-secrets into a temporary directory and returns its path, as
// buildBroker builds this command.
func buildAgentSecrets(t *testing.T) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "agent-secrets")
	build := exec.Command("go", "build", "-o", binPath, ".")
	build.Dir = filepath.Join("..", "agent-secrets")
	build.Env = append(os.Environ(), "GOTOOLCHAIN=go1.27.1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/agent-secrets: %v\n%s", err, out)
	}
	return binPath
}

// agentSecretsEnv is the environment an agent-secrets process of a pod runs with: the test's own,
// less every AGENT_SECRETS_* variable and every agent session id (requestSessionID's), which name
// the session running the test rather than the pod, plus the broker's address and the pod's key dir.
func agentSecretsEnv(brokerURL, keyDir string) []string {
	var env []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch {
		case strings.HasPrefix(name, "AGENT_SECRETS_"), name == "OMP_SESSION_ID", name == "ENVOY_SESSION_ID", name == "CLAUDE_CODE_SESSION_ID":
			continue
		}
		env = append(env, entry)
	}
	return append(env, "AGENT_SECRETS_URL="+brokerURL, "AGENT_SECRETS_KEY_DIR="+keyDir)
}

// writeWhole writes content to path whole and renames it into place, as the worker shim writes a
// pod's enrollment file, so no reader ever sees half of it.
func writeWhole(t *testing.T, path, content string) {
	t.Helper()
	partial := path + ".partial"
	if err := os.WriteFile(partial, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(partial, path); err != nil {
		t.Fatal(err)
	}
}

// enrollKey is podBroker.enroll for a key the pod made itself: by's credential enrolls the pod
// runtimeID for the key whose thumbprint is thumbprint, proving its account with token, and
// answers the enrollment's id.
func enrollKey(b podBroker, by launcher, runtimeID, thumbprint, token string) string {
	b.t.Helper()
	signed, err := proof.SignLauncher(by.key, by.credentialID, http.MethodPost, b.base+"/v1/enrollments", time.Now())
	if err != nil {
		b.t.Fatal(err)
	}
	status, answer := b.post("/v1/enrollments", map[string]string{"Proof": signed}, map[string]any{
		"kind": "pod", "runtime_id": runtimeID, "thumbprint": thumbprint, "pod_token": token,
	})
	var enrolled struct {
		EnrollmentID string `json:"enrollment_id"`
	}
	b.decode(answer, &enrolled)
	if status != http.StatusCreated || enrolled.EnrollmentID == "" {
		b.t.Fatalf("enroll %s = %d %s, want 201 with an enrollment_id", runtimeID, status, answer)
	}
	return enrolled.EnrollmentID
}

// deleteEnrollment has by's credential end the enrollment enrollmentID at the broker at base, as a
// launcher ends a pod it lets go, and answers the status.
func deleteEnrollment(t *testing.T, base string, by launcher, enrollmentID string) int {
	t.Helper()
	path := "/v1/enrollments/" + enrollmentID
	signed, err := proof.SignLauncher(by.key, by.credentialID, http.MethodDelete, base+path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodDelete, base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Proof", signed)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("DELETE %s: %v", path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("> DELETE %s\n< %d %s", path, response.StatusCode, strings.TrimSpace(string(answer)))
	return response.StatusCode
}

// brokerRefusal answers the code and message of the broker's error answer, empty for an answer
// that is not one.
func brokerRefusal(answer []byte) (code, message string) {
	var refusal struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(answer, &refusal)
	return refusal.Code, refusal.Error
}

// forwarder is the one address every broker process of a test is reached at, as a Service's
// address is in production: it holds a loopback port for the whole test and passes each
// connection it accepts on to the process it routes to, and while it routes to none it closes the
// connection at once, as a Service with no ready endpoint refuses one.
type forwarder struct {
	url     string
	mu      sync.Mutex
	backend string
	closed  bool
	open    map[net.Conn]struct{}
}

// startForwarder starts a forwarder routing to no process, and stops it when t ends, closing
// every connection it holds.
func startForwarder(t *testing.T) *forwarder {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &forwarder{url: "http://" + listener.Addr().String(), open: map[net.Conn]struct{}{}}
	var passing sync.WaitGroup
	passing.Add(1)
	go func() {
		defer passing.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			passing.Add(1)
			go func() {
				defer passing.Done()
				f.pass(conn)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		f.mu.Lock()
		f.closed = true
		for conn := range f.open {
			_ = conn.Close()
		}
		f.mu.Unlock()
		passing.Wait()
	})
	return f
}

// route sends every connection accepted from now on to addr, or refuses each one when addr is "".
func (f *forwarder) route(addr string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.backend = addr
}

// pass copies client to the process the forwarder routes to and back until either side closes,
// then closes both; with no process routed to, or none answering, it closes client at once.
func (f *forwarder) pass(client net.Conn) {
	defer client.Close()
	f.mu.Lock()
	backend := f.backend
	f.mu.Unlock()
	if backend == "" || !f.hold(client) {
		return
	}
	defer f.release(client)
	upstream, err := net.DialTimeout("tcp", backend, 5*time.Second)
	if err != nil {
		return
	}
	defer upstream.Close()
	if !f.hold(upstream) {
		return
	}
	defer f.release(upstream)
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		_, _ = io.Copy(upstream, client)
		_ = upstream.Close()
	}()
	_, _ = io.Copy(client, upstream)
	_ = client.Close()
	<-copied
}

// hold records conn as open, for the cleanup to close, and answers false once the forwarder has
// stopped.
func (f *forwarder) hold(conn net.Conn) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return false
	}
	f.open[conn] = struct{}{}
	return true
}

func (f *forwarder) release(conn net.Conn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.open, conn)
}
