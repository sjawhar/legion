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
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

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
// names every secret it puts in the command's environment and the store they came from, never a
// value, and it is written before the command runs - a reader of the command's own log sees the
// source above its first line.
func TestExecFormNamesItsSourceBeforeItExecs(t *testing.T) {
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)

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
	if !strings.Contains(first, "GRANT_ME") || !strings.Contains(first, fakeSourceText(broker)) {
		t.Fatalf("the first stderr line is %q; want it to name GRANT_ME and %s", redactSecrets(first), fakeSourceText(broker))
	}
	if strings.Contains(first, "topsecretvalue123") {
		t.Fatalf("the source line carried the secret's value: %q", redactSecrets(first))
	}
	if !strings.Contains(rest, "the-child-ran") {
		t.Fatalf("the child's own line is %q, want it after the source line", redactSecrets(rest))
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
