// packages/envoy/cmd/broker/main_test.go
//
// main() calls fatal on every failure, which calls os.Exit — so it cannot be driven directly by a
// test. refusePortZeroPublicURLInProduction is main's own port-0 refusal outside a local run,
// extracted so this test can drive it without exiting the test process.
package main

import (
	"bufio"
	"encoding/json"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/config"
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

// TestTheRequestMachineCarriesTheServiceAccounts pins the one place the broker hands
// BROKER_SERVICES' accounts to the request decisions: the Machine newRequestMachine builds from a
// config holds the config's ServiceAccounts, without which no pod is ever a service's.
func TestTheRequestMachineCarriesTheServiceAccounts(t *testing.T) {
	cfg, err := config.Load(func(name string) string {
		return map[string]string{
			"BROKER_DATABASE_URL":        "postgres://unused",
			"BROKER_PUBLIC_URL":          "https://secrets.internal.example",
			"BROKER_UI_TOKEN":            "ui-token",
			"BROKER_SECRETS_PREFIX":      "example/agent-secrets/",
			"BROKER_SECRETS_KMS_KEY_ARN": "arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab",
			"BROKER_SERVICES":            "example-service=system:serviceaccount:example:example-sa",
		}[name]
	})
	if err != nil {
		t.Fatal(err)
	}
	m := newRequestMachine(cfg, nil, nil, nil, nil)
	if want := map[string]string{"example-service": "system:serviceaccount:example:example-sa"}; !maps.Equal(m.ServiceAccounts, want) {
		t.Fatalf("request Machine's ServiceAccounts = %v, want %v", m.ServiceAccounts, want)
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

// rdsTestHost is an Amazon RDS cluster endpoint's form. Nothing resolves it, so a broker that
// reached the network for it would fail on the lookup, never on the refusal a test wants.
const rdsTestHost = "example-cluster.cluster-abcdefghijkl.us-west-2.rds.amazonaws.com"

// noAWSEnv strips the AWS SDK's configuration from a broker run: no region, no credentials, no
// shared files and no instance metadata, so a run that reaches for AWS says so instead of using
// the machine's own.
func noAWSEnv(t *testing.T) []string {
	t.Helper()
	empty := filepath.Join(t.TempDir(), "aws-config")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{
		"AWS_REGION=", "AWS_DEFAULT_REGION=", "AWS_PROFILE=", "AWS_ACCESS_KEY_ID=", "AWS_SECRET_ACCESS_KEY=",
		"AWS_SESSION_TOKEN=", "AWS_CONFIG_FILE=" + empty, "AWS_SHARED_CREDENTIALS_FILE=" + empty,
		"AWS_EC2_METADATA_DISABLED=true", "AWS_CONTAINER_CREDENTIALS_FULL_URI=", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI=",
		"AWS_WEB_IDENTITY_TOKEN_FILE=",
	}
}

// buildBroker builds cmd/broker into a temporary directory and returns its path.
func buildBroker(t *testing.T) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "broker")
	build := exec.Command("go", "build", "-o", binPath, ".")
	build.Env = append(os.Environ(), "GOTOOLCHAIN=go1.26.8")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/broker: %v\n%s", err, out)
	}
	return binPath
}

// runRefusedBroker runs the broker binary with env and returns its output, failing t unless it
// exits non-zero without ever logging "broker listening".
func runRefusedBroker(t *testing.T, binPath string, env ...string) string {
	t.Helper()
	cmd := exec.Command(binPath)
	cmd.Env = append(append(append(os.Environ(), productionEnv...), noAWSEnv(t)...), env...)
	out, err := cmd.CombinedOutput()
	exitErr, isExit := err.(*exec.ExitError)
	if err == nil || !isExit || exitErr.ExitCode() == 0 {
		t.Fatalf("broker: want a nonzero exit, got err=%v output=%s", err, out)
	}
	if strings.Contains(string(out), "broker listening") {
		t.Fatalf(`broker refused to boot but logged "broker listening" first: %s`, out)
	}
	return string(out)
}

// TestMainRefusesAnIAMURLThatDoesNotVerifyItsHost drives the real binary with an IAM-form
// BROKER_DATABASE_URL (a user, no password, an RDS endpoint) that asks only sslmode=require,
// which encrypts and verifies nothing: it refuses at startup naming the host, before it reaches
// for AWS or the database.
func TestMainRefusesAnIAMURLThatDoesNotVerifyItsHost(t *testing.T) {
	out := runRefusedBroker(t, buildBroker(t),
		"BROKER_DATABASE_URL=postgres://agent_secrets_broker@"+rdsTestHost+":5432/agent_secrets?sslmode=require",
		"BROKER_PUBLIC_URL=https://secrets.internal.example",
	)
	for _, want := range []string{"BROKER_DATABASE_URL signs in to " + rdsTestHost + " by RDS IAM token", "its sslmode is not verify-full"} {
		if !strings.Contains(out, want) {
			t.Fatalf("broker refused, but its output does not say %q: %s", want, out)
		}
	}
}

// TestMainMintsTokensBeforeOpeningTheDatabase drives the real binary with a verified IAM-form
// URL and no AWS region: it refuses naming the region, which proves the token minter is built
// from the AWS config before the database is opened. A broker that opened the database first
// would fail on the unresolvable host instead.
func TestMainMintsTokensBeforeOpeningTheDatabase(t *testing.T) {
	out := runRefusedBroker(t, buildBroker(t),
		"BROKER_DATABASE_URL=postgres://agent_secrets_broker@"+rdsTestHost+":5432/agent_secrets?sslmode=verify-full&sslrootcert=../../docker/rds-global-bundle.pem",
		"BROKER_PUBLIC_URL=https://secrets.internal.example",
	)
	if !strings.Contains(out, "needs an AWS region") {
		t.Fatalf("broker refused, but not for the missing AWS region the token minter needs: %s", out)
	}
}

// TestMainSignsInToALocalPasswordlessURLAsGiven drives the real binary with a passwordless URL to
// a local Postgres, with no AWS configuration at all: it is not an RDS endpoint, so the broker
// mints no token and signs in as the URL says (here with the password libpq's passfile holds), and
// boots. A broker that took it for IAM would refuse for the missing AWS region.
func TestMainSignsInToALocalPasswordlessURLAsGiven(t *testing.T) {
	databaseURL := storetest.URL(t)
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	password, _ := parsed.User.Password()
	parsed.User = url.User(parsed.User.Username())
	passfile := filepath.Join(t.TempDir(), "pgpass")
	if err := os.WriteFile(passfile, []byte("*:*:*:"+parsed.User.Username()+":"+password+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeSecretsFile := filepath.Join(t.TempDir(), "fake-secrets.json")
	if err := os.WriteFile(fakeSecretsFile, []byte(`{"secrets": []}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(buildBroker(t))
	cmd.Env = append(append(append(os.Environ(), productionEnv...), noAWSEnv(t)...),
		"BROKER_DATABASE_URL="+parsed.String(),
		"PGPASSFILE="+passfile,
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
	addr := waitForBoundAddress(t, stderr)
	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("GET http://%s/healthz: %v", addr, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET http://%s/healthz: status %d, want 200", addr, resp.StatusCode)
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
// still booting to serve the rest; and one owned by example-service, which BROKER_SERVICES
// registers, and asserts the binary serves it: the policy loader main builds carries the
// configured services.
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
		{"name": "example/agent-secrets/untagged-key", "kms_key_id": "` + key + `", "tags": {"owner": "sjawhar", "tier": "agent"}, "value": "nope"},
		{"name": "example/agent-secrets/service-key", "kms_key_id": "` + key + `", "tags": {"owner": "example-service", "tier": "agent"}, "value": "service"}
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
		"BROKER_SERVICES=example-service=system:serviceaccount:example:example-sa",
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
	if strings.Contains(boot.String(), "name=example/agent-secrets/service-key") {
		t.Fatalf("boot log refuses the secret example-service owns, which BROKER_SERVICES registers:\n%s", boot.String())
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

	reread, err := http.Post("http://"+addr+"/v1/secrets/SERVICE_KEY/reread", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /v1/secrets/SERVICE_KEY/reread: %v", err)
	}
	defer reread.Body.Close()
	var answer struct {
		Served bool   `json:"served"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(reread.Body).Decode(&answer); err != nil || reread.StatusCode != http.StatusOK || !answer.Served {
		t.Fatalf("reread SERVICE_KEY = status %d, %+v, %v; want served, its owner a service BROKER_SERVICES registers", reread.StatusCode, answer, err)
	}
}
