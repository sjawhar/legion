// packages/envoy/cmd/broker/main_test.go
//
// main() calls fatal on every failure, which calls os.Exit — so it cannot be driven directly by a
// test. refusePortZeroPublicURLInProduction is main's own port-0 refusal outside a local run,
// extracted so this test can drive it without exiting the test process.
package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"maps"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/airlock"
	"github.com/sjawhar/envoy/internal/broker/airlockproxy"
	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/config"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/cmdtest"
)

// testSecretsPrefix and testSecretsKMSKeyARN are the name prefix every agent secret sits under
// and the key every one of them is encrypted with, as productionEnv configures every broker
// these tests start (BROKER_SECRETS_PREFIX, BROKER_SECRETS_KMS_KEY_ARN). A fake secrets file a
// test writes names its secrets under this prefix and on this key, through these constants and
// not a literal of its own (multiprocess_test.go's fakeSecretsJSON): the policy loader refuses a
// secret under another prefix or on another key, which shows up as nothing but a secret never
// served, and one constant per fact is what leaves no second value to drift from this one.
const (
	testSecretsPrefix    = "example/agent-secrets/"
	testSecretsKMSKeyARN = "arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"
)

// testSignInIssuer is the provider every broker these tests start signs people in at. Nothing
// dials it: the approval page reads a provider's discovery document at a person's first sign-in,
// so a broker nobody signs in to never reaches for it. approval_page_test.go's fixture overrides
// these settings with a local issuer that answers.
const testSignInIssuer = "https://signin.example"

// signInClientSecretFile writes the sign-in client's secret into a file of t's own and answers
// its path, the file BROKER_SIGNIN_CLIENT_SECRET_FILE names.
func signInClientSecretFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "signin-client-secret")
	if err := os.WriteFile(path, []byte(brokertest.SignInClientSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// testMachineLoginURL is the page a person types a machine login's code into, as every broker
// these tests start that serves the device grant names it (BROKER_MACHINE_LOGIN_URL). It is not
// in productionEnv: the setting is optional, and the rows that need the device grant ask for it
// by name, so the broker every other row starts is the one that serves none.
const testMachineLoginURL = "https://dispatch.example/credentials/machine"

// productionEnv is the configuration every broker needs besides its addresses and database: the
// namespace it serves, the key every secret there is on, and the four settings a person signs
// in to its approval page with, which it refuses to start without.
func productionEnv(t *testing.T) []string {
	t.Helper()
	return []string{
		"BROKER_SECRETS_PREFIX=" + testSecretsPrefix,
		"BROKER_SECRETS_KMS_KEY_ARN=" + testSecretsKMSKeyARN,
		"BROKER_SIGNIN_ISSUER=" + testSignInIssuer,
		"BROKER_SIGNIN_CLIENT_ID=" + brokertest.SignInClientID,
		"BROKER_SIGNIN_CLIENT_SECRET_FILE=" + signInClientSecretFile(t),
		"BROKER_SIGNIN_DOMAIN=" + brokertest.SignInDomain,
	}
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

// airlockTestSettings is the seven settings of the airlock's local stack, each as a Config the
// refusal below reads and as the environment a real broker is started with. One row per
// setting: the refusal has to name whichever one it found, since an operator reads from it
// which setting to take out.
func airlockTestSettings() []struct {
	name string
	cfg  func(*config.Config)
	env  string
} {
	return []struct {
		name string
		cfg  func(*config.Config)
		env  string
	}{
		{"BROKER_AIRLOCK_TEST_GITHUB_API_BASE", func(c *config.Config) { c.AirlockTestGitHubAPIBase = "http://127.0.0.1:18080" }, "http://127.0.0.1:18080"},
		{"BROKER_AIRLOCK_TEST_GOOGLE_TOKEN_URL", func(c *config.Config) { c.AirlockTestGoogleTokenURL = "http://127.0.0.1:18081/token" }, "http://127.0.0.1:18081/token"},
		{"BROKER_AIRLOCK_TEST_DIAL_ALLOW", func(c *config.Config) { c.AirlockTestDialAllow = []string{"s.airlock.test:443"} }, "s.airlock.test:443"},
		{"BROKER_AIRLOCK_TEST_UPSTREAM_CA_FILE", func(c *config.Config) { c.AirlockTestUpstreamCAFile = "/run/airlock/ca.crt" }, "/run/airlock/ca.crt"},
		{"BROKER_AIRLOCK_TEST_RESOLVE", func(c *config.Config) {
			c.AirlockTestResolve = map[string]netip.Addr{"s.airlock.test": netip.MustParseAddr("127.0.0.1")}
		}, "s.airlock.test=127.0.0.1"},
		{"BROKER_AIRLOCK_TEST_KEEP_VALUES", func(c *config.Config) { c.AirlockTestKeepValues = true }, "true"},
		{"BROKER_AIRLOCK_TEST_NO_KIND", func(c *config.Config) { c.AirlockTestNoKind = true }, "true"},
	}
}

// TestEachAirlockTestSettingIsRefusedInProduction: every one of the seven points the broker at
// a stand-in, lets it dial what no production proxy may, or turns a part of the airlock off, so
// each on its own refuses a start that is not a local run, naming itself.
func TestEachAirlockTestSettingIsRefusedInProduction(t *testing.T) {
	for _, setting := range airlockTestSettings() {
		var cfg config.Config
		setting.cfg(&cfg)
		err := refuseAirlockTestSettingsInProduction(cfg, "")
		if err == nil {
			t.Errorf("%s set with no BROKER_FAKE_SECRETS_FILE: want an error, got nil", setting.name)
			continue
		}
		if !strings.Contains(err.Error(), setting.name) || !strings.Contains(err.Error(), "BROKER_FAKE_SECRETS_FILE is unset") {
			t.Errorf("%s set: error = %q; want it to name the setting and the local-run file", setting.name, err)
		}
	}
}

// TestEveryAirlockTestSettingIsFineForALocalRun: all seven at once, with the fake secrets file
// that makes a start a local run, are accepted - and a production start naming none of them is
// accepted too, since this refusal must not stand in the way of an ordinary boot.
func TestEveryAirlockTestSettingIsFineForALocalRun(t *testing.T) {
	var cfg config.Config
	for _, setting := range airlockTestSettings() {
		setting.cfg(&cfg)
	}
	if err := refuseAirlockTestSettingsInProduction(cfg, "/dev/fake-secrets.json"); err != nil {
		t.Fatalf("all seven with BROKER_FAKE_SECRETS_FILE set: want nil, got %v", err)
	}
	if err := refuseAirlockTestSettingsInProduction(config.Config{}, ""); err != nil {
		t.Fatalf("a production start naming none of them: want nil, got %v", err)
	}
}

// TestMainRefusesAnAirlockTestSettingInProduction drives the REAL compiled binary with each of
// the seven, as the port-0 row above drives it: the helper's rows prove it is correct, and this
// proves the broker runs it at boot, before it migrates the database or binds a socket. The
// database it is given is unreachable, so a broker that read these settings later would fail on
// the host instead of on the setting.
func TestMainRefusesAnAirlockTestSettingInProduction(t *testing.T) {
	binary := buildBroker(t)
	for _, setting := range airlockTestSettings() {
		t.Run(setting.name, func(t *testing.T) {
			out := runRefusedBroker(t, binary,
				"BROKER_DATABASE_URL=postgres://nonexistent-host-this-test-must-never-reach/db",
				"BROKER_PUBLIC_URL=https://secrets.internal.example",
				setting.name+"="+setting.env,
			)
			if !strings.Contains(out, setting.name) || !strings.Contains(out, "local stack") {
				t.Fatalf("broker refused, but its output does not name %s as a local-stack setting: %s", setting.name, out)
			}
		})
	}
}

// TestTheRequestMachineCarriesTheServicesByAccount pins the one place the broker hands
// BROKER_SERVICES' accounts to the request decisions: the Machine newRequestMachine builds from a
// config holds the config's ServicesByAccount, keyed by account so one service may have several,
// without which no pod is ever a service's.
func TestTheRequestMachineCarriesTheServicesByAccount(t *testing.T) {
	secretFile := signInClientSecretFile(t)
	cfg, err := config.Load(func(name string) string {
		return map[string]string{
			"BROKER_DATABASE_URL":              "postgres://unused",
			"BROKER_PUBLIC_URL":                "https://secrets.internal.example",
			"BROKER_MACHINE_LOGIN_URL":         "https://dispatch.example/credentials/machine",
			"BROKER_SERVICES":                  "example-service=system:serviceaccount:example:example-sa example-service=system:serviceaccount:example:another-sa",
			"BROKER_SECRETS_PREFIX":            "example/agent-secrets/",
			"BROKER_SECRETS_KMS_KEY_ARN":       "arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab",
			"BROKER_SIGNIN_ISSUER":             testSignInIssuer,
			"BROKER_SIGNIN_CLIENT_ID":          brokertest.SignInClientID,
			"BROKER_SIGNIN_CLIENT_SECRET_FILE": secretFile,
			"BROKER_SIGNIN_DOMAIN":             brokertest.SignInDomain,
		}[name]
	})
	if err != nil {
		t.Fatal(err)
	}
	m := newRequestMachine(cfg, nil, nil, nil, nil, airlock.Rules{}, "")
	want := map[string]string{
		"system:serviceaccount:example:example-sa": "example-service",
		"system:serviceaccount:example:another-sa": "example-service",
	}
	if !maps.Equal(m.ServicesByAccount, want) {
		t.Fatalf("request Machine's ServicesByAccount = %v, want %v", m.ServicesByAccount, want)
	}
	for _, kind := range policy.Kinds {
		if m.Minters[kind] == nil {
			t.Errorf("request Machine has no minter for kind %q, which the policy serves", kind)
		}
	}
	if len(m.Minters) != len(policy.Kinds) {
		t.Errorf("request Machine has %d minters for %d kinds: %v", len(m.Minters), len(policy.Kinds), m.Minters)
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
	cmd := exec.Command(buildBroker(t))
	cmd.Env = append(append(os.Environ(), productionEnv(t)...),
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
	build.Env = append(os.Environ(), "GOTOOLCHAIN=go1.27.3")
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
	cmd.Env = append(append(append(os.Environ(), productionEnv(t)...), noAWSEnv(t)...), env...)
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

// signInLine matches the one line the broker logs at boot, before it reaches for AWS or the
// database, naming how it signs in to its database: method rds-iam or password.
func signInLine(method string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} INFO database sign-in method=` + method + `$`)
}

// TestMainMintsTokensBeforeOpeningTheDatabase drives the real binary with a verified IAM-form
// URL and no AWS region: it refuses naming the region, which proves the token minter is built
// from the AWS config before the database is opened. A broker that opened the database first
// would fail on the unresolvable host instead. Before the refusal it logs that it signs in by
// RDS IAM token, and never by password.
func TestMainMintsTokensBeforeOpeningTheDatabase(t *testing.T) {
	out := runRefusedBroker(t, buildBroker(t),
		"BROKER_DATABASE_URL=postgres://agent_secrets_broker@"+rdsTestHost+":5432/agent_secrets?sslmode=verify-full&sslrootcert=../../docker/rds-global-bundle.pem",
		"BROKER_PUBLIC_URL=https://secrets.internal.example",
	)
	if !strings.Contains(out, "needs an AWS region") {
		t.Fatalf("broker refused, but not for the missing AWS region the token minter needs: %s", out)
	}
	if !signInLine("rds-iam").MatchString(out) || signInLine("password").MatchString(out) {
		t.Fatalf("broker output has no line matching %s, or names the password method too:\n%s", signInLine("rds-iam"), out)
	}
}

// TestMainSignsInToALocalPasswordlessURLAsGiven drives the real binary with a passwordless URL to
// a local Postgres, with no AWS configuration at all: it is not an RDS endpoint, so the broker
// mints no token and signs in as the URL says (here with the password libpq's passfile holds), and
// boots. A broker that took it for IAM would refuse for the missing AWS region. Its boot log says
// it signs in by password, the one a passfile supplies from outside the URL, and never by token.
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
	broker := startBroker(t, brokerOptions{Env: append(noAWSEnv(t),
		"BROKER_DATABASE_URL="+parsed.String(),
		"PGPASSFILE="+passfile,
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE="+fakeSecretsFile,
	)})
	addr, boot := broker.Addr, broker.Boot
	requireHealthy(t, addr)
	if !signInLine("password").MatchString(boot) || signInLine("rds-iam").MatchString(boot) {
		t.Fatalf("boot log has no line matching %s, or names the rds-iam method too:\n%s", signInLine("password"), boot)
	}
}

// listenTimeout bounds how long startBroker waits for a broker's "broker listening" line: long
// enough for a cold binary to migrate an empty database, short enough that a broker which will
// never bind fails its row rather than its package's deadline.
const listenTimeout = 10 * time.Second

// stopTimeout bounds how long a broker gets to exit after the SIGTERM its test's cleanup sends,
// before it is killed outright. It covers the whole stop budget the process logs at boot — its
// drain plus its shutdown — so a broker that honours that budget is never killed mid-drain, and
// one that hangs leaves no stray process behind holding a port and a database connection.
const stopTimeout = 90 * time.Second

// brokerOptions varies how startBroker starts a broker.
type brokerOptions struct {
	// Env is what the broker is started with on top of the test process's own environment and
	// productionEnv: its addresses, its database, and whatever else the row configures.
	Env []string
	// Binary is the broker to start, buildBroker's own when empty. A file that starts several
	// processes builds once and names the result here rather than paying a `go build` per start.
	Binary string
	// NoWaitForListening returns from startBroker as soon as the child is running, before it has
	// logged "broker listening" — before it has migrated, even. A row about two processes racing
	// to migrate one database needs both of them started before either has finished, which a
	// start that waits for that line cannot give it; such a caller calls AwaitListening itself.
	NoWaitForListening bool
}

// brokerProcess is a broker binary a test started and is driving: cmdtest's process — its whole
// log, its SIGTERM and the code it exited with, as cmd/dispatch's and cmd/listener's own
// shutdown rows drive theirs — plus the one thing that is the broker's alone, the address it
// logged that it bound.
type brokerProcess struct {
	*cmdtest.Process
	// Addr is the address its "broker listening" line named, and Boot its log up to that line.
	// Both stay empty until AwaitListening has answered, which startBroker does itself unless
	// NoWaitForListening was asked for.
	Addr string
	Boot string
}

// startBroker starts the broker binary with productionEnv and options.Env, waits for its
// "broker listening" line unless options.NoWaitForListening says not to, and stops it with
// SIGTERM when t ends, killing it if it has not gone within stopTimeout.
func startBroker(t *testing.T, options brokerOptions) *brokerProcess {
	t.Helper()
	binary := options.Binary
	if binary == "" {
		binary = buildBroker(t)
	}
	cmd := exec.Command(binary)
	cmd.Env = append(append(os.Environ(), productionEnv(t)...), options.Env...)
	p := &brokerProcess{Process: cmdtest.Start(t, "broker", cmd)}
	// Cleanups run last-registered-first, so this one — registered after cmdtest.Start's own —
	// asks the broker to stop and waits its whole stop budget out before cmdtest's cleanup kills
	// the child. The signal's own error is dropped because a process that has already exited is
	// nothing for a teardown to fail on.
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-p.Exited:
		case <-time.After(stopTimeout):
		}
	})
	if !options.NoWaitForListening {
		p.AwaitListening(t, listenTimeout)
	}
	return p
}

// Log is everything the broker has logged so far, safe to read while it is still running. Its
// output goes to a buffer os/exec's own copier drains to the end, which is what lets Log answer
// mid-run and what keeps a chatty child running at all: a child whose pipe nobody reads blocks in
// its own write once the pipe's buffer fills.
func (p *brokerProcess) Log() string {
	return p.Output.String()
}

// awaitTimeout bounds how long Await waits for the line it asks for.
const awaitTimeout = 10 * time.Second

// Await answers everything the broker has logged once a line carrying want has arrived. The
// binary logs the rest of its boot — the state of its token exchange — after the bind line
// startBroker synchronises on, so a test that reads one of those later lines asks Await for it
// rather than reading Boot. It fails t if that line has not come within awaitTimeout.
func (p *brokerProcess) Await(t *testing.T, want string) string {
	t.Helper()
	for deadline := time.Now().Add(awaitTimeout); ; time.Sleep(listeningPoll) {
		if text := p.Log(); strings.Contains(text, want) {
			return text
		}
		if time.Now().After(deadline) {
			t.Fatalf("the broker never logged a line carrying %q:\n%s", want, p.Log())
		}
	}
}

// listeningPoll is how often AwaitListening looks for the broker's listening line. The line is
// logged synchronously, only after Listen has already succeeded — the bind fix's whole point —
// so this is how soon a test reads the bind, never how soon the broker makes it.
const listeningPoll = 20 * time.Millisecond

// AwaitListening waits up to within for the broker's "broker listening" line and records the
// address it names in Addr and the log up to it in Boot. It fails t when the broker exits without
// ever logging that line, and when within passes with no such line.
func (p *brokerProcess) AwaitListening(t *testing.T, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if addr, boot, ok := listeningIn(p.Log()); ok {
			p.Addr, p.Boot = addr, boot
			return
		}
		select {
		case <-p.Exited:
			// Read the log once more rather than judging an exited broker by the read taken
			// before the exit: everything it wrote is in the buffer by the time Exited closes, so
			// a broker that logged its address and exited straight after is read here, where it
			// did bind, instead of being failed for the exit.
			if addr, boot, ok := listeningIn(p.Log()); ok {
				p.Addr, p.Boot = addr, boot
				return
			}
			t.Fatalf("the broker exited without ever logging %q:\n%s", "broker listening", p.Log())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %q line within %s:\n%s", "broker listening", within, p.Log())
		}
		time.Sleep(listeningPoll)
	}
}

// listeningIn finds the broker's first "broker listening" line in log and answers the address it
// names and the log up to the end of that line, which is the boot log: everything the broker
// logged before it was serving.
//
// Only a line a newline closes counts. This reads a buffer the child's output is still being
// copied into, and half a listening line would hand addrLogPattern half an address — 127.0.0.1:4
// for a broker bound to port 41337 — and call it the one it bound.
func listeningIn(log string) (addr, boot string, ok bool) {
	complete := strings.LastIndexByte(log, '\n') + 1
	for offset := 0; offset < complete; {
		end := offset + strings.IndexByte(log[offset:], '\n') + 1
		line := log[offset:end]
		if strings.Contains(line, "broker listening") {
			if m := addrLogPattern.FindStringSubmatch(line); m != nil {
				return m[1], log[:end], true
			}
		}
		offset = end
	}
	return "", "", false
}

// rereadRateLine is what one of a broker's "broker reread rate" lines says: the replica count
// the process counted at a beat of its serving clock, and the shared reread bucket it derived
// from that count (api.RereadRate.SetReplicas). A process logs one at its first beat and again
// whenever the count it sees changes, so the LAST such line is the sizing it serves at now.
type rereadRateLine struct {
	Replicas int
	Every    time.Duration
	Burst    int
}

// rereadRatePattern reads a whole "broker reread rate" line: the message and the three values
// that follow it, in the order api.RereadRate logs them.
//
// It matches the message and the first key together because the warning a count above
// api.MaxSupportedReplicas logs opens with the same words and carries replicas too, and what
// tells the two apart is that the warning's message goes on ("above the replica count this
// sizing supports") where this one ends. The optional quote covers both handlers a broker can
// be started with: cmd/broker logs through slog's default handler, which writes the message
// bare, and a TextHandler writes it as msg="broker reread rate".
//
// every is whatever the handler wrote a time.Duration as (2.25s), which time.ParseDuration
// reads back.
var rereadRatePattern = regexp.MustCompile(`broker reread rate"? replicas=(\d+) every=(\S+) burst=(\d+)`)

// rereadRateIn finds the last complete "broker reread rate" line in log and answers what it
// says. Only a line a newline closes counts, for listeningIn's own reason: this reads a buffer
// the child's output is still being copied into, and half a line would hand back half a
// duration — 2.2s for a broker sized at 2.25s — as the rate it derived.
func rereadRateIn(log string) (rate rereadRateLine, ok bool) {
	complete := strings.LastIndexByte(log, '\n') + 1
	for offset := 0; offset < complete; {
		end := offset + strings.IndexByte(log[offset:], '\n') + 1
		line := log[offset:end]
		offset = end
		m := rereadRatePattern.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		replicas, replicasErr := strconv.Atoi(m[1])
		every, everyErr := time.ParseDuration(m[2])
		burst, burstErr := strconv.Atoi(m[3])
		if replicasErr != nil || everyErr != nil || burstErr != nil {
			continue
		}
		rate, ok = rereadRateLine{Replicas: replicas, Every: every, Burst: burst}, true
	}
	return rate, ok
}

// AwaitExit waits up to within for the broker to exit and answers what its exit was: nil for a
// clean one, an *exec.ExitError naming the code otherwise, which is what Wait itself answers. It
// fails t when the broker is still running after within. The bound is its caller's rather than
// cmdtest.WaitExit's fixed thirty seconds, because a broker's stop budget is its whole drain plus
// its shutdown, which the drain row gives ninety.
func (p *brokerProcess) AwaitExit(t *testing.T, within time.Duration) error {
	t.Helper()
	select {
	case <-p.Exited:
		if p.Cmd.ProcessState.Success() {
			return nil
		}
		return &exec.ExitError{ProcessState: p.Cmd.ProcessState}
	case <-time.After(within):
		t.Fatalf("the broker at %s was still running %s after it was asked to stop:\n%s", p.Addr, within, p.Log())
		return nil
	}
}

// requireHealthy fails t unless GET /healthz at addr answers 200.
func requireHealthy(t *testing.T, addr string) {
	t.Helper()
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

	broker := startBroker(t, brokerOptions{Env: []string{
		"BROKER_DATABASE_URL=" + databaseURL,
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE=" + fakeSecretsFile,
		"BROKER_SERVICES=example-service=system:serviceaccount:example:example-sa",
	}})
	addr, boot := broker.Addr, broker.Boot
	refused := regexp.MustCompile(`(?m)^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} ERROR agent secret policy refused name=example/agent-secrets/untagged-key reason=owner-tag-malformed$`)
	if !refused.MatchString(boot) {
		t.Fatalf("boot log has no refusal line matching %s:\n%s", refused, boot)
	}
	if strings.Contains(boot, "name=example/agent-secrets/service-key") {
		t.Fatalf("boot log refuses the secret example-service owns, which BROKER_SERVICES registers:\n%s", boot)
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

	requireHealthy(t, addr)

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

// TestFakeSecretsFileServesUnderTheGCPStore pins that the local stand-in serves its secrets with the
// GCP store selected: the file's kms_key_id names the agent-secrets key as that store configures
// it (BROKER_SECRETS_KMS_KEY_NAME), and its names sit under the GCP prefix.
func TestFakeSecretsFileServesUnderTheGCPStore(t *testing.T) {
	const (
		prefix = "projects/000000000000/secrets/agent-secrets-"
		key    = "projects/example-project/locations/global/keyRings/agent-secrets/cryptoKeys/agent-secrets"
	)
	fakeSecretsFile := filepath.Join(t.TempDir(), "fake-secrets.json")
	if err := os.WriteFile(fakeSecretsFile, []byte(`{"secrets": [
		{"name": "`+prefix+`demo-key", "kms_key_id": "`+key+`", "tags": {"owner": "shared", "tier": "agent"}, "value": "demo"}
	]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	broker := startBroker(t, brokerOptions{Env: []string{
		"BROKER_DATABASE_URL=" + storetest.URL(t),
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE=" + fakeSecretsFile,
		"BROKER_SECRETS_STORE=gcp-secret-manager",
		"BROKER_SECRETS_PREFIX=" + prefix,
		"BROKER_SECRETS_KMS_KEY_ARN=",
		"BROKER_SECRETS_KMS_KEY_NAME=" + key,
	}})
	addr, boot := broker.Addr, broker.Boot
	if strings.Contains(boot, "agent secret policy refused") {
		t.Fatalf("boot log refuses a fake secret on the configured key:\n%s", boot)
	}
	reread, err := http.Post("http://"+addr+"/v1/secrets/DEMO_KEY/reread", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /v1/secrets/DEMO_KEY/reread: %v", err)
	}
	defer reread.Body.Close()
	var answer struct {
		Served bool   `json:"served"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(reread.Body).Decode(&answer); err != nil || reread.StatusCode != http.StatusOK || !answer.Served {
		t.Fatalf("reread DEMO_KEY = status %d, %+v, %v; want served under the GCP store's key", reread.StatusCode, answer, err)
	}
}

// airlockRulesFile writes rules into a file of t's own and answers its path, the file
// BROKER_AIRLOCK_RULES_FILE names.
func airlockRulesFile(t *testing.T, rules string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "airlock.json")
	if err := os.WriteFile(path, []byte(rules), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// airlockCAFiles writes a self-signed certificate authority and its key into files of t's own
// and answers the two paths, the files BROKER_AIRLOCK_CA_CERT_FILE and
// BROKER_AIRLOCK_CA_KEY_FILE name. Self-signed, since the boot reads the certificate as a
// certificate and trusts nothing about who signed it; the key is beside it because the proxy
// port signs each bound host's certificate with it and refuses to start without it.
func airlockCAFiles(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "broker airlock test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile = filepath.Join(dir, "airlock-ca.crt")
	keyFile = filepath.Join(dir, "airlock-ca.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

// writeTempFile writes content into a file named name under a directory of t's own.
func writeTempFile(t *testing.T, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// airlockEnv is what a broker with an airlock is started with besides its rules file: the proxy
// the release names, the certificate authority it hands over and the key behind it, without
// which config.Load refuses a broker that reads rules it could not proxy, and an address of its
// own for the proxy port, since a fixed one would be a port two rows of this package could not
// both hold.
func airlockEnv(t *testing.T) []string {
	t.Helper()
	certFile, keyFile := airlockCAFiles(t)
	return []string{
		"BROKER_AIRLOCK_PROXY_URL=http://127.0.0.1:13381",
		"BROKER_AIRLOCK_CA_CERT_FILE=" + certFile,
		"BROKER_AIRLOCK_CA_KEY_FILE=" + keyFile,
		"BROKER_AIRLOCK_LISTEN_ADDR=127.0.0.1:0",
	}
}

// TestMainRefusesAnAirlockRulesFileItCannotServe drives the real binary with each kind of rules
// file it must refuse — one whose entry the airlock will not validate, one whose entry names a
// secret by a name no session can ask for, one that is not JSON, and a path that is not there —
// and asserts it exits non-zero naming the file and what is wrong with it, without ever
// binding. The database it is given is unreachable, so a broker that read the file after
// opening the store would fail on the host instead of on the file.
func TestMainRefusesAnAirlockRulesFileItCannotServe(t *testing.T) {
	binary := buildBroker(t)
	missing := filepath.Join(t.TempDir(), "absent.json")
	for name, c := range map[string]struct {
		path string
		want []string
	}{
		// The broker logs a refusal as slog's quoted error=…, so a quotation mark inside it
		// comes out escaped; these rows look for the parts around it.
		"an entry the airlock refuses": {
			path: airlockRulesFile(t, `{"secrets": [{"name": "DEEL_API_KEY", "hosts": []}]}`),
			want: []string{"secrets[0]", "DEEL_API_KEY", "no host"},
		},
		// An entry whose name is not of the policy's form matches no release, so the file
		// would hold back nothing of the secret a reviewer read it as covering.
		"an entry name in lower case": {
			path: airlockRulesFile(t, `{"secrets": [{"name": "deel_api_key", "hosts": ["api.deel.com"]}]}`),
			want: []string{"secrets[0]", "deel_api_key", "not an agent secret name", "holds nothing back"},
		},
		"an entry name written with hyphens": {
			path: airlockRulesFile(t, `{"secrets": [{"name": "DEEL-API-KEY", "hosts": ["api.deel.com"]}]}`),
			want: []string{"secrets[0]", "DEEL-API-KEY", "not an agent secret name", "holds nothing back"},
		},
		"a file that is not JSON": {
			path: airlockRulesFile(t, "these are not the rules you are looking for\n"),
			want: []string{"not JSON", "byte offset 2, line 1, column 2"},
		},
		"a path that cannot be read": {
			path: missing,
			want: []string{missing, "no such file"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			out := runRefusedBroker(t, binary, append(airlockEnv(t),
				"BROKER_DATABASE_URL=postgres://nonexistent-host-this-test-must-never-reach/db",
				"BROKER_PUBLIC_URL=https://secrets.internal.example",
				"BROKER_AIRLOCK_RULES_FILE="+c.path,
			)...)
			if !strings.Contains(out, "BROKER_AIRLOCK_RULES_FILE") {
				t.Fatalf("broker refused, but its output does not name BROKER_AIRLOCK_RULES_FILE: %s", out)
			}
			for _, want := range c.want {
				if !strings.Contains(out, want) {
					t.Fatalf("broker refused, but its output does not say %q: %s", want, out)
				}
			}
		})
	}
}

// TestMainStartsWithAnAirlockRulesFile: a file the airlock parses and validates is no obstacle to
// the boot, and the broker goes on to serve.
func TestMainStartsWithAnAirlockRulesFile(t *testing.T) {
	fakeSecretsFile := filepath.Join(t.TempDir(), "fake-secrets.json")
	if err := os.WriteFile(fakeSecretsFile, []byte(`{"secrets": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	broker := startBroker(t, brokerOptions{Env: append(airlockEnv(t),
		"BROKER_DATABASE_URL="+storetest.URL(t),
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE="+fakeSecretsFile,
		"BROKER_AIRLOCK_RULES_FILE="+airlockRulesFile(t, `{"secrets": [
			{"name": "DEEL_API_KEY", "hosts": ["api.deel.com"], "allow": [{"method": "GET", "pathPrefix": "/rest/"}]}
		]}`),
	)})
	requireHealthy(t, broker.Addr)
}

// TestMainRefusesAnAirlockCertificateItCannotRead drives the real binary with each kind of
// certificate file it must refuse — a path that is not there, a file that is not PEM, and a PEM
// block that is a key rather than a certificate — and asserts it exits non-zero naming the
// variables, without ever binding. The proxy signs every bound host's certificate with this
// pair and every release hands the command the certificate of it, so the pod that mounted the
// wrong file has to find out, not the commands it releases to.
func TestMainRefusesAnAirlockCertificateItCannotRead(t *testing.T) {
	binary := buildBroker(t)
	missing := filepath.Join(t.TempDir(), "absent.crt")
	// A key that matches nothing here: what each row is about is the certificate file beside
	// it, and the authority reads both.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	_, keyFile := airlockCAFiles(t)
	for name, c := range map[string]struct {
		path string
		want string
	}{
		"a path that cannot be read": {path: missing, want: "no such file"},
		"a file that is not PEM":     {path: writeTempFile(t, "plain.crt", []byte("not a certificate\n")), want: "PEM data in certificate input"},
		"a PEM block that is a key":  {path: writeTempFile(t, "key.crt", pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})), want: "did find a private key"},
	} {
		t.Run(name, func(t *testing.T) {
			out := runRefusedBroker(t, binary,
				"BROKER_DATABASE_URL=postgres://nonexistent-host-this-test-must-never-reach/db",
				"BROKER_PUBLIC_URL=https://secrets.internal.example",
				"BROKER_AIRLOCK_RULES_FILE="+airlockRulesFile(t, `{"secrets": [{"name": "DEEL_API_KEY", "hosts": ["api.deel.com"]}]}`),
				"BROKER_AIRLOCK_PROXY_URL=http://127.0.0.1:13381",
				"BROKER_AIRLOCK_CA_CERT_FILE="+c.path,
				"BROKER_AIRLOCK_CA_KEY_FILE="+keyFile,
			)
			if !strings.Contains(out, "BROKER_AIRLOCK_CA_CERT_FILE") || !strings.Contains(out, c.want) {
				t.Fatalf("broker refused, but its output does not name BROKER_AIRLOCK_CA_CERT_FILE and %q: %s", c.want, out)
			}
		})
	}
}

// TestTheReleasesCertificateIsTheAuthoritysOwnAndCarriesNoKey: a release hands the command the
// one certificate it trusts the proxy by, and that has to be the certificate this process signs
// each bound host with - not the bytes of the file it was read from. A certificate and its key
// in one PEM file is a shape an operator really mounts, and one the authority accepts, so a
// reader that answered the file would put the authority's private key in every release, with
// which a command could sign a certificate for any host that every other airlock command trusts.
func TestTheReleasesCertificateIsTheAuthoritysOwnAndCarriesNoKey(t *testing.T) {
	certFile, keyFile := airlockCAFiles(t)
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	combined := writeTempFile(t, "combined.pem", append(append([]byte{}, certPEM...), keyPEM...))

	ca, err := airlockproxy.NewAuthority(combined, keyFile)
	if err != nil {
		t.Fatalf("the authority on a file holding the certificate and the key: %v; the row is about a file the boot accepts", err)
	}
	released := airlockCACertificatePEM(ca)
	if strings.Contains(released, "PRIVATE KEY") {
		t.Fatalf("the release carries a private key:\n%s", released)
	}
	block, rest := pem.Decode([]byte(released))
	if block == nil || block.Type != "CERTIFICATE" || len(rest) != 0 {
		t.Fatalf("the release carries %q and %d bytes after it; want one PEM certificate and nothing else", released, len(rest))
	}
	if !bytes.Equal(block.Bytes, ca.Certificate().Raw) {
		t.Fatal("the release's certificate is not the one the proxy signs with")
	}
	// And a deployment with no airlock releases no certificate at all.
	if released := airlockCACertificatePEM(nil); released != "" {
		t.Fatalf("a broker with no airlock released %q; want nothing", released)
	}
}
