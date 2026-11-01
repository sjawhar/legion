package shim_test

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shim"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// fakeAgentSecrets is `agent-secrets` as the shim runs it: keygen writes key.pem and prints a
// fixed thumbprint; renew records its invocation and either sleeps or exits the test-controlled
// code; anything else fails.
const fakeThumbprint = "NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs"

const fakeRenewRestartDelay = 5 * time.Second

func writeFakeAgentSecrets(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "agent-secrets")
	script := `#!/bin/sh
case "$1" in
  keygen)
    [ "$2" = "--out" ] || exit 64
    umask 077; printf 'fake key\n' >"$3/key.pem"; printf '%s\n' "` + fakeThumbprint + `"; exit 0 ;;
  renew)
    shift; printf 'renew %s %s\n' "$$" "$*" >>"$AGENT_SECRETS_KEY_DIR/renew.log"
    if [ "$AGENT_SECRETS_TEST_RENEW_EXIT" = 76 ]; then
      printf 'agent-secrets renew: enrollment %s permanently refused by broker: HTTP 403 POD_TOKEN_REQUIRED: fake refusal\n' "$(cat "$AGENT_SECRETS_KEY_DIR/enrollment")" >&2
    fi
    if [ -n "$AGENT_SECRETS_TEST_RENEW_EXIT" ]; then exit "$AGENT_SECRETS_TEST_RENEW_EXIT"; fi
    exec sleep 300 ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func agentSecretsConfig(t *testing.T, socket string, o *omp) (shim.Config, string, string) {
	t.Helper()
	dir := t.TempDir()
	keyDir := filepath.Join(dir, "keys")
	if err := os.Mkdir(keyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("eyJhbGciOiJSUzI1NiJ9.e30.sig\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config(t, socket, o)
	cfg.AgentSecrets = &shim.AgentSecrets{KeyDir: keyDir, TokenFile: tokenFile, Binary: writeFakeAgentSecrets(t, dir)}
	cfg.Env = append(cfg.Env, "AGENT_SECRETS_KEY_DIR="+keyDir, "AGENT_SECRETS_URL=http://broker.test")
	return cfg, keyDir, tokenFile
}

func TestTheHelloCarriesTheKeyThumbprintAndTheProjectedToken(t *testing.T) {
	path := socketPath(t)
	stub := listen(t, path)
	o := newOMP(t)
	cfg, keyDir, _ := agentSecretsConfig(t, path, o)
	r := run(t, cfg, newClock())
	peer := stub.accept(t)

	frame := peer.next(t)
	hello, ok := frame.(shimwire.Hello2)
	if !ok || hello.BootToken != bootToken || hello.AgentSecrets == nil {
		t.Fatalf("first frame %#v, want a hello2 with an identity", frame)
	}
	if hello.AgentSecrets.Thumbprint != fakeThumbprint || hello.AgentSecrets.PodToken != "eyJhbGciOiJSUzI1NiJ9.e30.sig" {
		t.Fatalf("identity %+v", *hello.AgentSecrets)
	}
	if _, err := os.Stat(filepath.Join(keyDir, "key.pem")); err != nil {
		t.Fatalf("keygen wrote no key before the hello: %v", err)
	}
	if o.started() {
		t.Fatal("OMP was spawned before the hello was acked")
	}
	peer.send(t, shimwire.HelloAck{})
	readyPID(t, peer.expectRaw(t, "fake_ready")) // the child, spawned on the ack
	r.stop(t)
}

func TestAHelloWithoutAgentSecretsIsAPlainHello2(t *testing.T) {
	path := socketPath(t)
	stub := listen(t, path)
	o := newOMP(t)
	r := run(t, config(t, path, o), newClock())
	peer := stub.accept(t)
	hello, ok := peer.next(t).(shimwire.Hello2)
	if !ok || hello.BootToken != bootToken || hello.AgentSecrets != nil {
		t.Fatalf("hello %#v, want a hello2 with no identity", hello)
	}
	peer.send(t, shimwire.HelloAck{})
	readyPID(t, peer.expectRaw(t, "fake_ready"))
	r.stop(t)
}

// TestTheEnrollmentFrameIsWrittenBesideTheKeyAndStartsTheRenewer: the renewer is started with the
// projected token file the runtime mounts, `agent-secrets renew --pod-token-file <file>`, so each
// renewal carries the pod's current token for the broker to check against the enrollment.
func TestTheEnrollmentFrameIsWrittenBesideTheKeyAndStartsTheRenewer(t *testing.T) {
	path := socketPath(t)
	stub := listen(t, path)
	o := newOMP(t)
	cfg, keyDir, tokenFile := agentSecretsConfig(t, path, o)
	cfg.Env = append(cfg.Env, "AGENT_SECRETS_TEST_RENEW_EXIT=1")
	clock := newClock()
	r := run(t, cfg, clock)
	peer := stub.accept(t)
	peer.open(t) // hello2 read, acked, the child's first frame read back

	peer.send(t, shimwire.AgentSecretsEnrollment{ID: "req-1", EnrollmentID: "enr-1"})
	peer.expect(t, shimwire.AgentSecretsEnrollmentResult{ID: "req-1", OK: true})
	got, err := os.ReadFile(filepath.Join(keyDir, "enrollment"))
	if err != nil || strings.TrimSpace(string(got)) != "enr-1" {
		t.Fatalf("enrollment file: %q, %v", got, err)
	}
	if info, _ := os.Stat(filepath.Join(keyDir, "enrollment")); info.Mode().Perm() != 0o600 {
		t.Fatalf("enrollment file mode %v, want 0600", info.Mode().Perm())
	}
	clock.next(t, fakeRenewRestartDelay) // fake renewer wrote renew.log, exited, and asked for its retry
	raw, err := os.ReadFile(filepath.Join(keyDir, "renew.log"))
	if err != nil {
		t.Fatal(err)
	}
	if fields := strings.Fields(string(raw)); len(fields) != 4 || fields[2] != "--pod-token-file" || fields[3] != tokenFile {
		t.Fatalf("renewer started as %q, want `renew --pod-token-file %s`", raw, tokenFile)
	}
	peer.send(t, shimwire.AgentSecretsEnrollment{ID: "req-2", EnrollmentID: "enr-1"})
	peer.expect(t, shimwire.AgentSecretsEnrollmentResult{ID: "req-2", OK: true})
	if frames := o.received(t); len(frames) != 0 {
		t.Fatalf("OMP received %v; the enrollment frames are the shim's alone", frameTypes(frames))
	}
	r.stop(t)
	if raw, _ := os.ReadFile(filepath.Join(keyDir, "renew.log")); strings.Count(string(raw), "renew ") != 1 {
		t.Fatalf("renew.log %q, want exactly one renewer", raw)
	}
}

func TestPermanentlyRefusedRenewerStopsWithoutRestarting(t *testing.T) {
	path := socketPath(t)
	stub := listen(t, path)
	o := newOMP(t)
	cfg, _, _ := agentSecretsConfig(t, path, o)
	cfg.Env = append(cfg.Env, "AGENT_SECRETS_TEST_RENEW_EXIT=76")
	clock := newClock()
	r := run(t, cfg, clock)
	peer := stub.accept(t)
	peer.open(t)
	peer.send(t, shimwire.AgentSecretsEnrollment{ID: "req-1", EnrollmentID: "enr-1"})
	peer.expect(t, shimwire.AgentSecretsEnrollmentResult{ID: "req-1", OK: true})
	r.log.await(t, "renewer permanently refused for enrollment enr-1 (exit 76): \"agent-secrets renew: enrollment enr-1 permanently refused by broker: HTTP 403 POD_TOKEN_REQUIRED: fake refusal\"; not restarting", 1)
	clock.idle(t)
	if r.log.count("renewer started") != 1 || r.log.count("renewer exited") != 0 {
		t.Fatalf("permanently refused renewer log:\n%s", r.log)
	}
	r.stop(t)
}

func TestTransientRenewerExitRestarts(t *testing.T) {
	path := socketPath(t)
	stub := listen(t, path)
	o := newOMP(t)
	cfg, _, _ := agentSecretsConfig(t, path, o)
	cfg.Env = append(cfg.Env, "AGENT_SECRETS_TEST_RENEW_EXIT=1")
	clock := newClock()
	r := run(t, cfg, clock)
	peer := stub.accept(t)
	peer.open(t)
	peer.send(t, shimwire.AgentSecretsEnrollment{ID: "req-1", EnrollmentID: "enr-1"})
	peer.expect(t, shimwire.AgentSecretsEnrollmentResult{ID: "req-1", OK: true})
	clock.next(t, fakeRenewRestartDelay).elapse()
	clock.next(t, fakeRenewRestartDelay) // second child exited and scheduled the next retry
	if r.log.count("renewer started") != 2 || r.log.count("restarting") != 2 {
		t.Fatalf("transient renewer log:\n%s", r.log)
	}
	r.stop(t)
}

func TestAnEnrollmentFrameOnAPaneWithoutAgentSecretsIsRefused(t *testing.T) {
	path := socketPath(t)
	stub := listen(t, path)
	o := newOMP(t)
	r := run(t, config(t, path, o), newClock())
	peer := stub.accept(t)
	peer.open(t)
	peer.send(t, shimwire.AgentSecretsEnrollment{ID: "req-1", EnrollmentID: "enr-1"})
	result := peer.next(t).(shimwire.AgentSecretsEnrollmentResult)
	if result.OK || !strings.Contains(result.Error, "--agent-secrets-key-dir") {
		t.Fatalf("result %+v, want a refusal naming the flag", result)
	}
	r.stop(t)
}

// The kubelet rewrites the projected token in place before it expires, so the daemon's request
// is answered with the token as the mount holds it now, not as it held it at the hello.
func TestATokenRequestIsAnsweredWithTheTokenTheMountHoldsNow(t *testing.T) {
	path := socketPath(t)
	stub := listen(t, path)
	o := newOMP(t)
	cfg, _, _ := agentSecretsConfig(t, path, o)
	r := run(t, cfg, newClock())
	peer := stub.accept(t)
	peer.open(t)

	rotated := "eyJhbGciOiJSUzI1NiJ9.eyJleHAiOjF9.rotated"
	if err := os.WriteFile(cfg.AgentSecrets.TokenFile, []byte(rotated+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	peer.send(t, shimwire.AgentSecretsToken{ID: "req-1"})
	peer.expect(t, shimwire.AgentSecretsTokenResult{ID: "req-1", OK: true, PodToken: rotated})
	if frames := o.received(t); len(frames) != 0 {
		t.Fatalf("OMP received %v; the token request is the shim's alone", frameTypes(frames))
	}
	r.stop(t)
}

func TestATokenRequestOnAPaneWithoutAgentSecretsIsRefused(t *testing.T) {
	path := socketPath(t)
	stub := listen(t, path)
	o := newOMP(t)
	r := run(t, config(t, path, o), newClock())
	peer := stub.accept(t)
	peer.open(t)
	peer.send(t, shimwire.AgentSecretsToken{ID: "req-1"})
	result := peer.next(t).(shimwire.AgentSecretsTokenResult)
	if result.OK || result.PodToken != "" || !strings.Contains(result.Error, "--agent-secrets-key-dir") {
		t.Fatalf("result %+v, want a refusal naming the flag", result)
	}
	r.stop(t)
}

func TestKeygenFailureEndsTheShimBeforeAnyDial(t *testing.T) {
	path := socketPath(t)
	stub := listen(t, path)
	o := newOMP(t)
	cfg, _, _ := agentSecretsConfig(t, path, o)
	if err := os.WriteFile(cfg.AgentSecrets.Binary, []byte("#!/bin/sh\necho 'keygen: disk full' >&2; exit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	r := run(t, cfg, newClock())
	// The shim ends on its own once keygen fails, from inside connect(), before any dial: wait
	// for that rather than cancelling, which would race the keygen subprocess and could end the
	// shim for the wrong reason (its own signalled stop) instead of the keygen failure.
	select {
	case <-r.done:
	case <-time.After(waitLimit):
		t.Fatalf("the shim never exited; log:\n%s", r.log)
	}
	if r.code != 1 || !strings.Contains(r.err(), "agent-secrets keygen") || !strings.Contains(r.err(), "disk full") {
		t.Fatalf("exit %d, err %q; want 1 naming keygen and its stderr", r.code, r.err())
	}
	// The shim never dialled: draining the listener's accept queue with a short deadline finds
	// nothing waiting.
	_ = stub.ln.(*net.UnixListener).SetDeadline(time.Now().Add(100 * time.Millisecond))
	if _, err := stub.ln.Accept(); err == nil {
		t.Fatal("the shim dialled before keygen succeeded")
	}
}
