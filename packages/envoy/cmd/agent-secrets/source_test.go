// packages/envoy/cmd/agent-secrets/source_test.go
//
// Every answer and every refusal about a secret names where it came from. The unit tests hold
// what a source carries for each store the broker's settings can describe, and the command tests
// run the built binary against the fake broker to hold that the injection form names its source
// before it execs, that a refusal names it, and that a broker whose settings cannot be read still
// leaves every line naming the broker itself.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/helper"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// TestASourceNamesTheBrokerAndTheStoreItsSettingsDescribe: a source carries the broker's host and
// what the broker says its store is - the kind, and the account or project holding the secrets,
// which is what tells two deployments' stores apart. A broker from before the `store` field holds
// them in AWS Secrets Manager.
func TestASourceNamesTheBrokerAndTheStoreItsSettingsDescribe(t *testing.T) {
	for _, row := range []struct {
		name     string
		base     string
		settings Settings
		want     []string
		absent   []string
	}{
		{
			name:     "gcp",
			base:     "https://secrets.internal.example/",
			settings: Settings{Store: secrets.StoreGCP, GCPProject: "000000000000"},
			want:     []string{"secrets.internal.example", "GCP Secret Manager", "000000000000"},
			absent:   []string{"AWS"},
		},
		{
			name:     "aws",
			base:     "http://127.0.0.1:13380",
			settings: Settings{Store: secrets.StoreAWS, AWSAccountID: "111122223333", AWSRegion: "us-east-1"},
			want:     []string{"127.0.0.1:13380", "AWS Secrets Manager", "111122223333", "us-east-1"},
			absent:   []string{"GCP"},
		},
		{
			name:     "a broker from before the store field",
			base:     "http://127.0.0.1:13380",
			settings: Settings{AWSAccountID: "111122223333"},
			want:     []string{"127.0.0.1:13380", "AWS Secrets Manager", "111122223333"},
		},
		{
			name:     "a store this release does not know",
			base:     "http://127.0.0.1:13380",
			settings: Settings{Store: "vault"},
			want:     []string{"127.0.0.1:13380", "vault"},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			got := brokerSource(row.base).withSettings(row.settings).String()
			for _, want := range row.want {
				if !strings.Contains(got, want) {
					t.Fatalf("source %q does not name %q", got, want)
				}
			}
			for _, absent := range row.absent {
				if strings.Contains(got, absent) {
					t.Fatalf("source %q names %q, which is another store", got, absent)
				}
			}
		})
	}
}

// TestASourceWithNoSettingsNamesTheBrokerItself: a broker that cannot be asked for its store
// (an older one, or one that did not answer) still leaves a source naming the broker, which is
// what tells this CLI's answer from another secrets CLI's for the same name; the store forms say
// so in place of a store rather than guessing one.
func TestASourceWithNoSettingsNamesTheBrokerItself(t *testing.T) {
	src := brokerSource("https://secrets.internal.example")
	if !strings.Contains(src.String(), "secrets.internal.example") {
		t.Fatalf("source = %q, want the broker's host", src)
	}
	if !strings.Contains(src.asked(), src.String()) {
		t.Fatalf("a refusal's %q does not name the source %q", src.asked(), src)
	}
	if !strings.Contains(src.storeName(), "secrets.internal.example") {
		t.Fatalf("store name = %q, want it to name the broker whose store it is", src.storeName())
	}
	// A URL no parser reads is named as it was given: the line still says which value this
	// process was pointed at.
	if got := brokerSource("not a url").String(); !strings.Contains(got, "not a url") {
		t.Fatalf("source = %q, want the configured value named", got)
	}
}

// TestExecFormNamesItsSourceBeforeItExecs: the one line the injection form writes before it execs
// names every secret it puts in the command's environment, the broker AGENT_SECRETS_URL names and
// the kind of its store, never a value, and it is written before the command runs - a reader of
// the command's own log sees the source above its first line. The line is quoted wherever that
// output is, so for every store kind it names no host, account, region or project.
func TestExecFormNamesItsSourceBeforeItExecs(t *testing.T) {
	binary := buildAgentSecrets(t)
	keyDir := newKeyDir(t)
	const prefix = "agent-secrets: GRANT_ME from the broker AGENT_SECRETS_URL names"
	for _, row := range []struct {
		name     string
		settings *Settings // nil: a broker too old to serve GET /v1/settings
		line     string
	}{
		{"gcp", &fakeSettings, prefix + " (GCP Secret Manager)\n"},
		{"aws", &Settings{Store: secrets.StoreAWS, AWSAccountID: "111122223333", AWSRegion: "us-east-1"}, prefix + " (AWS Secrets Manager)\n"},
		{"a broker from before the store field", &Settings{AWSAccountID: "111122223333", AWSRegion: "us-east-1"}, prefix + " (AWS Secrets Manager)\n"},
		{"a store this release does not know", &Settings{Store: "vault"}, prefix + " (vault)\n"},
		{"no settings route", nil, prefix + "\n"},
	} {
		t.Run(row.name, func(t *testing.T) {
			broker, counters := fakeBroker(t)
			defer broker.Close()
			if row.settings == nil {
				counters.hideSettings()
			} else {
				counters.serveSettings(*row.settings)
			}
			cmd := exec.Command(binary, "GRANT_ME", "--", "sh", "-c", "echo the-child-ran >&2")
			cmd.Env = append(os.Environ(), "AGENT_SECRETS_URL="+broker.URL, "AGENT_SECRETS_KEY_DIR="+keyDir)
			pipe, err := cmd.StderrPipe()
			if err != nil {
				t.Fatalf("stderr pipe: %v", err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatalf("start agent-secrets: %v", err)
			}
			stderr := bufio.NewReader(pipe)
			first, err := stderr.ReadString('\n')
			if err != nil {
				t.Fatalf("read the first stderr line: %v (%q)", err, redactSecrets(first))
			}
			rest, _ := stderr.ReadString('\n')
			if err := cmd.Wait(); err != nil {
				t.Fatalf("agent-secrets GRANT_ME -- sh: %v (stderr %q)", err, redactSecrets(first+rest))
			}
			if first != row.line {
				t.Fatalf("the first stderr line is %q; want %q", redactSecrets(first), row.line)
			}
			host := strings.TrimPrefix(broker.URL, "http://")
			for _, absent := range []string{"topsecretvalue123", host, "127.0.0.1", "000000000000", "111122223333", "us-east-1"} {
				if strings.Contains(first, absent) {
					t.Fatalf("the injection line %q names %q", redactSecrets(first), absent)
				}
			}
			if !strings.Contains(rest, "the-child-ran") {
				t.Fatalf("the child's own line is %q, want it after the source line", redactSecrets(rest))
			}
		})
	}
}

// TestNoFormPrintsACredentialTheBrokerURLCarries: a user name, password or query in
// AGENT_SECRETS_URL reaches no output of any form - a refusal from a broker that answered, a
// broker that was never reached, a URL no parser reads, the secret forms' own refusals, and a
// line naming a source - while a line about an unreached broker still names the address it
// tried, with the credential masked.
func TestNoFormPrintsACredentialTheBrokerURLCarries(t *testing.T) {
	binary := buildAgentSecrets(t)
	keyDir := newKeyDir(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	answering := "http://tokuser9f3:hunter2pw@" + strings.TrimPrefix(broker.URL, "http://")
	const unreached = "http://tokuser9f3:hunter2pw@127.0.0.1:1"
	injection := []string{"GRANT_ME", "--", "sh", "-c", "echo the-child-ran"}
	for _, row := range []struct {
		name, base string
		args       []string
		want       string // what the output must still name
	}{
		{"self, unreached", unreached, []string{"self"}, "GET http://xxxxx@127.0.0.1:1/v1/enrollments/self (AGENT_SECRETS_URL names http://xxxxx@127.0.0.1:1)"},
		{"request, unreached", unreached, []string{"request", "GRANT_ME"}, "POST http://xxxxx@127.0.0.1:1/v1/requests"},
		{"request --json, unreached", unreached, []string{"request", "--json", "GRANT_ME"}, "POST http://xxxxx@127.0.0.1:1/v1/requests"},
		{"status, unreached", unreached, []string{"status", "req-approved"}, "GET http://xxxxx@127.0.0.1:1/v1/requests/req-approved"},
		{"exec, unreached", unreached, injection, "POST http://xxxxx@127.0.0.1:1/v1/requests"},
		{"secret list, unreached", unreached, []string{"secret", "list"}, "http://xxxxx@127.0.0.1:1"},
		{"self, answered", answering, []string{"self"}, "source: the broker at 127.0.0.1:"},
		{"status, answered", answering, []string{"status", "req-approved"}, "source: the broker at 127.0.0.1:"},
		{"exec, refused by the broker", answering, injection, "asked the broker at 127.0.0.1:"},
		{"a port no parser reads", "http://tokuser9f3:hunter2pw@127.0.0.1:badport", []string{"self"}, "http://xxxxx@127.0.0.1:badport"},
		{"a plain-http broker on the network, secret list", "http://tokuser9f3:hunter2pw@example.internal", []string{"secret", "list"}, `"http://xxxxx@example.internal" must be https`},
		{"a token in the query", "http://127.0.0.1:1/?token=hunter2pw", []string{"self"}, "http://127.0.0.1:1/?xxxxx"},
	} {
		t.Run(row.name, func(t *testing.T) {
			stdout, stderr, _ := runAgentSecrets(t, binary, row.base, keyDir, nil, row.args...)
			output := stdout + stderr
			for _, secret := range []string{"tokuser9f3", "hunter2pw"} {
				if strings.Contains(output, secret) {
					t.Fatalf("output names %q: stdout %q, stderr %q", secret, stdout, stderr)
				}
			}
			if !strings.Contains(output, row.want) {
				t.Fatalf("stdout %q, stderr %q; want them to name %q", stdout, stderr, row.want)
			}
		})
	}
}

// TestWhatAScriptReadsIsUnchanged pins the bytes a script parses: --json stdout is the broker's
// body verbatim, and each outcome keeps its exit code, whatever the human-read lines say.
func TestWhatAScriptReadsIsUnchanged(t *testing.T) {
	binary := buildAgentSecrets(t)
	keyDir := newKeyDir(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	for _, row := range []struct {
		name   string
		base   string
		args   []string
		exit   int
		stdout string
	}{
		{"request --json", broker.URL, []string{"request", "--json", "GRANT_ME"}, 0,
			`{"approver":null,"grant_id":"grant-granted","record_id":null,"request_id":"req-granted","secrets":[{"decision":"automatic","name":"GRANT_ME"}],"state":"granted"}` + "\n"},
		{"request --json, no trailing newline", broker.URL, []string{"request", "--json", "NONEWLINE_ME"}, 0,
			`{"request_id":"req-nonewline","state":"granted","secrets":[{"name":"NONEWLINE_ME","decision":"automatic"}],"grant_id":"grant-nonewline","record_id":null,"approver":null}`},
		{"status --json", broker.URL, []string{"status", "--json", "req-approved"}, 0, approvedStatusJSON},
		{"request --json, denied", broker.URL, []string{"request", "--json", "DENY_ME"}, 77,
			`{"approver":null,"grant_id":null,"record_id":null,"request_id":"req-denied","secrets":[{"decision":"deny","name":"DENY_ME"}],"state":"denied"}` + "\n"},
		{"request --json, refused", broker.URL, []string{"request", "--json", "NO_SUCH_NAME"}, 1, ""},
		{"request --json, unreached", "http://127.0.0.1:1", []string{"request", "--json", "GRANT_ME"}, 1, ""},
		{"exec, granted", broker.URL, []string{"GRANT_ME", "--", "sh", "-c", "echo the-child-ran"}, 0, "the-child-ran\n"},
		{"exec, denied", broker.URL, []string{"DENY_ME", "--", "sh", "-c", "echo the-child-ran"}, 77, ""},
		{"exec, nothing released", broker.URL, []string{"UNRELEASED_ME", "--", "sh", "-c", "echo the-child-ran"}, 1, ""},
		{"exec, still pending", broker.URL, []string{"PENDING_ME", "--wait", "200ms", "--", "sh", "-c", "echo the-child-ran"}, 75, ""},
		{"exec, refused", broker.URL, []string{"NO_SUCH_NAME", "--", "sh", "-c", "echo the-child-ran"}, 1, ""},
		{"exec, unreached", "http://127.0.0.1:1", []string{"GRANT_ME", "--", "sh", "-c", "echo the-child-ran"}, 1, ""},
		{"exec, the child's own exit", broker.URL, []string{"GRANT_ME", "--", "sh", "-c", "exit 9"}, 9, ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			stdout, stderr, exit := runAgentSecrets(t, binary, row.base, keyDir, nil, row.args...)
			if exit != row.exit || stdout != row.stdout {
				t.Fatalf("exit %d, stdout %q (stderr %q); want %d, %q", exit, redactSecrets(stdout), redactSecrets(stderr), row.exit, row.stdout)
			}
		})
	}
}

// TestExecFormRefusalsNameWhereItLooked: a request the broker decides against, and a grant that
// releases no value, each say which broker and store answered, so a refusal is never read as
// another secrets CLI's answer for the same name.
func TestExecFormRefusalsNameWhereItLooked(t *testing.T) {
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)

	for _, row := range []struct {
		name string
		exit int
	}{
		{"DENY_ME", exitDenied},
		{"UNRELEASED_ME", 1},
	} {
		t.Run(row.name, func(t *testing.T) {
			stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil,
				row.name, "--", "sh", "-c", "echo the-child-ran")
			if exit != row.exit || strings.Contains(stdout, "the-child-ran") {
				t.Fatalf("exit = %d, stdout %q; want %d and no child", exit, redactSecrets(stdout), row.exit)
			}
			if !strings.Contains(stderr, fakeSourceText(broker)) {
				t.Fatalf("stderr = %q; want the refusal to name %s", redactSecrets(stderr), fakeSourceText(broker))
			}
		})
	}
}

// TestStatusFormsNameTheirSource: self and status report the broker and store this session's
// secrets come from, beside what they already report.
func TestStatusFormsNameTheirSource(t *testing.T) {
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)

	for _, args := range [][]string{{"self"}, {"status", "req-approved"}, {"request", "GRANT_ME"}} {
		stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil, args...)
		if exit != 0 {
			t.Fatalf("%v: exit %d, stderr %q", args, exit, stderr)
		}
		if !strings.Contains(stdout, "source: "+fakeSourceText(broker)) {
			t.Fatalf("%v printed %q; want a source line naming %s", args, stdout, fakeSourceText(broker))
		}
	}
}

// TestAnAnswerFromABrokerWithNoSettingsRouteStillNamesTheBroker: a broker too old to serve
// GET /v1/settings leaves every line naming the broker alone - the command still runs, and
// nothing guesses a store.
func TestAnAnswerFromABrokerWithNoSettingsRouteStillNamesTheBroker(t *testing.T) {
	binary := buildAgentSecrets(t)
	broker, counters := fakeBroker(t)
	defer broker.Close()
	counters.hideSettings()
	keyDir := newKeyDir(t)

	host := brokerSource(broker.URL).String()
	stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil, "self")
	if exit != 0 || !strings.Contains(stdout, "source: "+host+"\n") {
		t.Fatalf("self against a broker with no settings route: exit %d, stdout %q, stderr %q; want a source line naming %s alone", exit, stdout, stderr, host)
	}
	if strings.Contains(stdout, "Secrets Manager") {
		t.Fatalf("self named a store the broker never described: %q", stdout)
	}

	cmd := exec.Command(binary, "GRANT_ME", "--", "sh", "-c", "echo the-child-ran")
	cmd.Env = append(os.Environ(), "AGENT_SECRETS_URL="+broker.URL, "AGENT_SECRETS_KEY_DIR="+keyDir)
	out, err := cmd.Output()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Fatalf("run agent-secrets GRANT_ME: %v", err)
	}
	if err != nil || !strings.Contains(string(out), "the-child-ran") {
		t.Fatalf("the child did not run against a broker with no settings route: %v, stdout %q", err, redactSecrets(string(out)))
	}
}

// TestABrokerThatCannotBeReachedSaysSo: a call that never reached the broker is not a refusal the
// broker gave. Its message classifies the failure (a host that does not resolve, a refused
// connection), names the address it tried and the variable that named it, so a session that
// inherited a stale AGENT_SECRETS_URL reads a stale address rather than a broker that is down.
func TestABrokerThatCannotBeReachedSaysSo(t *testing.T) {
	binary := buildAgentSecrets(t)
	keyDir := newKeyDir(t)

	// broker.invalid is reserved (RFC 2606) and resolves nowhere; 127.0.0.1:1 answers nothing.
	for _, row := range []struct {
		name, base, what string
		args             []string
	}{
		{"a host that does not resolve", "https://broker.invalid", "does not resolve", []string{"self"}},
		{"a host that does not resolve, injecting", "https://broker.invalid", "does not resolve", []string{"GRANT_ME", "--", "sh", "-c", "echo the-child-ran"}},
		{"nothing listening", "http://127.0.0.1:1", "the connection was refused", []string{"self"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			stdout, stderr, exit := runAgentSecrets(t, binary, row.base, keyDir, nil, row.args...)
			if exit == 0 || strings.Contains(stdout, "the-child-ran") {
				t.Fatalf("exit = %d, stdout %q; want a refusal and no command run", exit, stdout)
			}
			for _, want := range []string{row.what, brokerHost(row.base), brokerURLVar, row.base, "/v1/"} {
				if !strings.Contains(stderr, want) {
					t.Fatalf("stderr = %q; want it to name %q", stderr, want)
				}
			}
			if strings.Contains(stderr, "asked the broker") {
				t.Fatalf("stderr = %q; a broker that was never reached refused nothing", stderr)
			}
		})
	}
}

// TestEveryUnreachableFailureIsClassified keeps a failed name lookup distinct from a broker that
// timed out, a dial that made no connection distinct from a peer that answered and then failed the
// exchange, and an error none of those covers worded as no more than a failed request. Each class
// sends a reader to a different layer: DNS, routing or a listener, TLS or a proxy.
func TestEveryUnreachableFailureIsClassified(t *testing.T) {
	const base = "https://secrets.internal.example"
	const answered = "answered but the exchange failed"
	dial := func(errno syscall.Errno) error {
		return &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", errno)}
	}
	for _, row := range []struct {
		name    string
		err     error
		want    string
		notWant string
	}{
		{"no such host", &net.DNSError{Err: "no such host", Name: "secrets.internal.example", IsNotFound: true}, "does not resolve", ""},
		{"DNS timeout", &net.DNSError{Err: "i/o timeout", Name: "secrets.internal.example", IsTimeout: true}, "name resolution failed", ""},
		{"DNS server failure", &net.DNSError{Err: "server misbehaving", Name: "secrets.internal.example", IsTemporary: true}, "name resolution failed", ""},
		{"refused", dial(syscall.ECONNREFUSED), "the connection was refused", answered},
		{"network unreachable", dial(syscall.ENETUNREACH), "nothing answered at secrets.internal.example: no connection could be made", answered},
		{"host unreachable", dial(syscall.EHOSTUNREACH), "nothing answered at secrets.internal.example: no connection could be made", answered},
		{"any other dial failure", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("socket: too many open files")}, "no connection could be made", answered},
		{"timed out", context.DeadlineExceeded, "did not answer in time", answered},
		{"an HTTP answer to HTTPS", tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}, answered, ""},
		{"an untrusted certificate", &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, answered, ""},
		{"EOF after the request", io.EOF, answered, ""},
		{"a reset after the request", &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}, answered, ""},
		{"an error none of these covers", errors.New("something unforeseen"), "the request to the broker at secrets.internal.example failed", "answered"},
	} {
		t.Run(row.name, func(t *testing.T) {
			err := unreachable(base, http.MethodGet, base+"/v1/enrollments/self", &url.Error{Op: "Get", URL: base, Err: row.err})
			var unreached *unreachableError
			if !errors.As(err, &unreached) {
				t.Fatalf("unreachable(%v) = %v, want an unreachableError", row.err, err)
			}
			for _, want := range []string{row.want, "secrets.internal.example", brokerURLVar, "/v1/enrollments/self"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("message %q does not name %q", err, want)
				}
			}
			if row.notWant != "" && strings.Contains(err.Error(), row.notWant) {
				t.Fatalf("message %q says %q, which this failure is not", err, row.notWant)
			}
		})
	}
}

// TestAHelperFailureNamesTheHelperNotTheBroker: requesting a secret first asks the local helper
// to sign, so an unavailable helper or a helper's own refusal must name its socket. A broker was
// never asked, and adding its source trailer lies about the request path.
func TestAHelperFailureNamesTheHelperNotTheBroker(t *testing.T) {
	binary := buildAgentSecrets(t)
	socket, _ := fakeHelper(t, helper.Response{
		Code:  helper.CodeNotASession,
		Error: "this process is not a registered session",
	})
	for _, row := range []struct {
		name, socket, want string
	}{
		{"helper refusal", socket, "refused: NOT_A_SESSION: this process is not a registered session"},
		{"dead helper", filepath.Join(t.TempDir(), "missing.sock"), "did not answer"},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, stderr, exit := runAgentSecrets(
				t,
				binary,
				"https://broker.invalid",
				t.TempDir(),
				[]string{"AGENT_SECRETS_HELPER_SOCK=" + row.socket},
				"request",
				"DEMO_API_KEY",
			)
			if exit == 0 {
				t.Fatal("request succeeded with no helper")
			}
			for _, want := range []string{row.socket, row.want} {
				if !strings.Contains(stderr, want) {
					t.Fatalf("stderr = %q; want it to name %q", stderr, want)
				}
			}
			for _, notWant := range []string{"asked the broker", brokerURLVar} {
				if strings.Contains(stderr, notWant) {
					t.Fatalf("stderr = %q; the helper failure must not name %q", stderr, notWant)
				}
			}
		})
	}
}
