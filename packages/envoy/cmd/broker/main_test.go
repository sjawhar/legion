// packages/envoy/cmd/broker/main_test.go
//
// main() calls fatal on every failure, which calls os.Exit — so it cannot be driven directly by a
// test. refusePortZeroPublicURLInProduction is main's own port-0 refusal outside a local run,
// extracted so this test can drive it without exiting the test process.
package main

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/store/storetest"
)

// productionEnv is the configuration every broker needs besides its addresses and database: the
// namespace it serves and the key every secret there is on.
var productionEnv = []string{
	"BROKER_UI_TOKEN=test-token-0123456789abcdef0123456789abcdef",
	"BROKER_SECRETS_PREFIX=example/agent-secrets/",
	"BROKER_SECRETS_KMS_KEY_ARN=arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab",
}

func TestPortZeroPublicURLRefusedInProduction(t *testing.T) {
	err := refusePortZeroPublicURLInProduction("http://127.0.0.1:0", "")
	if err == nil {
		t.Fatal("BROKER_PUBLIC_URL port 0 with no BROKER_FAKE_SECRETS_FILE: want an error, got nil")
	}
	const want = `BROKER_PUBLIC_URL "http://127.0.0.1:0": port 0 is never dialable in production (BROKER_FAKE_SECRETS_FILE is unset); only a local run may use it as dev-broker.sh's derive-from-bind convention`
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

func TestPortZeroPublicURLIsFineForALocalRun(t *testing.T) {
	if err := refusePortZeroPublicURLInProduction("http://127.0.0.1:0", "/dev/fake-secrets.json"); err != nil {
		t.Fatalf("BROKER_PUBLIC_URL port 0 with BROKER_FAKE_SECRETS_FILE set: want nil, got %v", err)
	}
}

func TestNonZeroPortPublicURLIsFineInProduction(t *testing.T) {
	if err := refusePortZeroPublicURLInProduction("https://broker.invalid", ""); err != nil {
		t.Fatalf("a real BROKER_PUBLIC_URL: want nil, got %v", err)
	}
}

// TestMainRefusesPortZeroPublicURLInProduction drives the REAL compiled binary's main(), not
// refusePortZeroPublicURLInProduction directly: the helper's own tests above prove only that it is
// correct, and with this guard's only call site moved to run after st.Migrate, the first Secrets
// Manager read, and net.Listen, a misconfigured production boot would migrate the production
// database and bind a socket before ever refusing. This builds cmd/broker once, execs it with
// BROKER_PUBLIC_URL=http://127.0.0.1:0 and no BROKER_FAKE_SECRETS_FILE (plus an unreachable
// BROKER_DATABASE_URL and just enough other required BROKER_* variables for config.Load to succeed
// — the refusal must run before store.Open, so no real Postgres or AWS credential, and no
// successful bind, is ever needed), and asserts the process exits non-zero naming the port-0
// refusal on stderr and never logs "broker listening": a regression that runs the guard after
// Listen would still refuse eventually, but only after already printing that line and binding a
// real socket.
func TestMainRefusesPortZeroPublicURLInProduction(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "broker")
	build := exec.Command("go", "build", "-o", binPath, ".")
	build.Env = append(os.Environ(), "GOTOOLCHAIN=go1.26.8")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/broker: %v\n%s", err, out)
	}

	cmd := exec.Command(binPath)
	cmd.Env = append(append(os.Environ(), productionEnv...),
		"BROKER_DATABASE_URL=postgres://nonexistent-host-this-test-must-never-reach/db",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
	)
	out, err := cmd.CombinedOutput()
	exitErr, isExit := err.(*exec.ExitError)
	if err == nil || !isExit || exitErr.ExitCode() == 0 {
		t.Fatalf("broker BROKER_PUBLIC_URL=http://127.0.0.1:0 in production: want a nonzero exit, got err=%v output=%s", err, out)
	}
	const wantSubstring = `port 0 is never dialable in production`
	if !strings.Contains(string(out), wantSubstring) {
		t.Fatalf("broker refused to boot (exit %d) but its output didn't name the reason: %s\nwant it to contain: %s",
			exitErr.ExitCode(), out, wantSubstring)
	}
	if strings.Contains(string(out), "broker listening") {
		t.Fatalf(`broker refused to boot but still logged "broker listening" first — the guard ran after binding: %s`, out)
	}
}

// addrLogPattern extracts the value of a slog key=value pair named addr. A bare host:port never
// contains a space, so slog's TextHandler (which quotes only values that do) never quotes it.
var addrLogPattern = regexp.MustCompile(`addr=(\S+)`)

// waitForBoundAddress scans the broker's stderr for its "broker listening" log line — the bind
// fix's whole point: a real bind is reported once, synchronously, only after Listen has already
// succeeded — and returns the addr it names. Fails the test if the process exits or 10s pass
// without that line ever appearing.
func waitForBoundAddress(t *testing.T, stderr io.Reader) string {
	t.Helper()
	scanner := bufio.NewScanner(stderr)
	done := make(chan string, 1)
	go func() {
		defer close(done)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.Contains(line, "broker listening") {
				continue
			}
			if m := addrLogPattern.FindStringSubmatch(line); m != nil {
				done <- m[1]
				return
			}
		}
	}()
	select {
	case addr, ok := <-done:
		if !ok || addr == "" {
			t.Fatal(`broker exited without ever logging "broker listening"`)
		}
		return addr
	case <-time.After(10 * time.Second):
		t.Fatal(`timed out waiting for the "broker listening" log line`)
	}
	return ""
}

// TestMainLogsRealBoundAddress drives the REAL compiled binary with BROKER_LISTEN_ADDR=127.0.0.1:0
// (dev-broker.sh's own setting after the bind fix) and BROKER_PUBLIC_URL=http://127.0.0.1:0
// (dev-broker.sh's "derive my public URL from whatever I actually bind" convention, see
// cmd/broker/main.go's own comment beside its url.Parse check). It asserts the "broker listening"
// log line names a real, nonzero port on 127.0.0.1 — never the configured placeholder — and then
// proves that exact logged address is the one actually serving, by getting /healthz there and
// requiring 200. Before this fix, a losing instance in a port collision could print "ready" while
// a completely different, already-running instance answered its healthz check; binding
// synchronously before logging anything means the address this test reads can only ever name this
// process's own listener.
// It also holds a fake Secrets Manager with one secret whose owner tag names nobody, and asserts
// the real binary refuses it at boot with the exact line the deployment's alarm filters on, while
// still booting to serve the rest.
func TestMainLogsRealBoundAddress(t *testing.T) {
	databaseURL := storetest.URL(t)

	binPath := filepath.Join(t.TempDir(), "broker")
	build := exec.Command("go", "build", "-o", binPath, ".")
	build.Env = append(os.Environ(), "GOTOOLCHAIN=go1.26.8")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/broker: %v\n%s", err, out)
	}

	fakeSecretsFile := filepath.Join(t.TempDir(), "fake-secrets.json")
	const key = "arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"
	const fakeSecrets = `{"secrets": [
		{"name": "example/agent-secrets/demo-key", "kms_key_id": "` + key + `", "tags": {"owner": "shared", "tier": "agent"}, "value": "demo"},
		{"name": "example/agent-secrets/untagged-key", "kms_key_id": "` + key + `", "tags": {"owner": "sjawhar", "tier": "agent"}, "value": "nope"}
	]}`
	if err := os.WriteFile(fakeSecretsFile, []byte(fakeSecrets), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(binPath)
	cmd.Env = append(append(os.Environ(), productionEnv...),
		"BROKER_DATABASE_URL="+databaseURL,
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE="+fakeSecretsFile,
	)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start broker: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
	})

	var boot strings.Builder
	addr := waitForBoundAddress(t, io.TeeReader(stderr, &boot))
	refused := regexp.MustCompile(`(?m)^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} ERROR agent secret policy refused name=example/agent-secrets/untagged-key reason=owner-tag-malformed$`)
	if !refused.MatchString(boot.String()) {
		t.Fatalf("boot log has no refusal line matching %s:\n%s", refused, boot.String())
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("logged address %q did not parse as host:port: %v", addr, err)
	}
	if host != "127.0.0.1" {
		t.Fatalf("logged address %q: want host 127.0.0.1, got %q", addr, host)
	}
	if port == "" || port == "0" {
		t.Fatalf("logged address %q: want a real, nonzero port, got %q", addr, port)
	}

	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("GET http://%s/healthz: %v", addr, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET http://%s/healthz: status %d, want 200", addr, resp.StatusCode)
	}
}
