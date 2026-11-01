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
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/helper"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/redact"
)

// TestASourceNamesTheBrokerAndTheStoreItsSettingsDescribe: as a session's forms print it, a
// source names the broker AGENT_SECRETS_URL names and the kind of store its settings describe,
// and no host, account, region or project; the secret forms' store name carries the account or
// project, which tells the person which one they write to. A broker from before the `store`
// field holds them in AWS Secrets Manager.
func TestASourceNamesTheBrokerAndTheStoreItsSettingsDescribe(t *testing.T) {
	for _, row := range []struct {
		name     string
		base     string
		settings Settings
		source   string
		store    string
	}{
		{"gcp", "https://secrets.internal.example/", Settings{Store: secrets.StoreGCP, GCPProject: "000000000000"},
			"the broker AGENT_SECRETS_URL names (GCP Secret Manager)", "GCP Secret Manager project 000000000000"},
		{"aws", "http://127.0.0.1:13380", Settings{Store: secrets.StoreAWS, AWSAccountID: "111122223333", AWSRegion: "us-east-1"},
			"the broker AGENT_SECRETS_URL names (AWS Secrets Manager)", "AWS Secrets Manager account 111122223333 in us-east-1"},
		{"a broker from before the store field", "http://127.0.0.1:13380", Settings{AWSAccountID: "111122223333"},
			"the broker AGENT_SECRETS_URL names (AWS Secrets Manager)", "AWS Secrets Manager account 111122223333"},
		{"a store this release does not know", "http://127.0.0.1:13380", Settings{Store: "vault"},
			"the broker AGENT_SECRETS_URL names (vault)", "vault"},
	} {
		t.Run(row.name, func(t *testing.T) {
			src := brokerSource(row.base).withSettings(row.settings)
			if got := src.String(); got != row.source {
				t.Fatalf("source %q, want %q", got, row.source)
			}
			if got := src.asked(); got != "; asked "+row.source {
				t.Fatalf("a refusal's trailer %q, want it to name %q", got, row.source)
			}
			if got := src.storeName(); got != row.store {
				t.Fatalf("store name %q, want %q", got, row.store)
			}
		})
	}
}

// TestASourceWithNoSettingsNamesTheBrokerItself: a broker that cannot be asked for its store
// (an older one, or one that did not answer) still leaves a source naming the broker, which is
// what tells this CLI's answer from another secrets CLI's for the same name; the secret forms say
// which broker by host in place of a store rather than guessing one.
func TestASourceWithNoSettingsNamesTheBrokerItself(t *testing.T) {
	src := brokerSource("https://secrets.internal.example")
	if got := src.String(); got != "the broker AGENT_SECRETS_URL names" {
		t.Fatalf("source = %q, want the broker AGENT_SECRETS_URL names alone", got)
	}
	if !strings.Contains(src.storeName(), "secrets.internal.example") {
		t.Fatalf("store name = %q, want it to name the broker whose store it is", src.storeName())
	}
	// A value that is no plain http(s) URL is not printed: the allow-list proves nothing about
	// what it holds.
	if got := brokerSource("not a url").brokerAt(); !strings.Contains(got, redact.NotPrinted) || strings.Contains(got, "not a url") {
		t.Fatalf("broker = %q, want %q", got, redact.NotPrinted)
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

// TestNoFormPrintsACredentialTheBrokerURLCarries: AGENT_SECRETS_URL prints only when redact.URL
// proves it a plain http(s) URL, and as redact.NotPrinted otherwise - in a refusal from a broker
// that answered, a broker that was never reached, a URL no parser reads, a scheme-less URL, the
// secret forms' own refusals, and a line naming a source - and for such a URL the transport's
// cause prints as its class alone. A clean URL's messages read as before. No value forges a line
// with a control byte, raw or escaped.
func TestNoFormPrintsACredentialTheBrokerURLCarries(t *testing.T) {
	binary := buildAgentSecrets(t)
	keyDir := newKeyDir(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	answering := "http://tokuser9f3:hunter2pw@" + strings.TrimPrefix(broker.URL, "http://")
	const unreached = "http://tokuser9f3:hunter2pw@127.0.0.1:1"
	const badEscape = "http://tokuser9f3:hunter2pw%zz@127.0.0.1:1"
	const np = redact.NotPrinted
	names := "(AGENT_SECRETS_URL names " + np + ")"
	injection := []string{"GRANT_ME", "--", "sh", "-c", "echo the-child-ran"}
	for _, row := range []struct {
		name, base string
		args       []string
		want       string // what the output must still name
	}{
		{"self, a clean URL unreached", "http://127.0.0.1:1", []string{"self"},
			"nothing answered at 127.0.0.1:1: the connection was refused: GET http://127.0.0.1:1/v1/enrollments/self (AGENT_SECRETS_URL names http://127.0.0.1:1): dial tcp 127.0.0.1:1: connect: connection refused"},
		{"self, unreached", unreached, []string{"self"}, "nothing answered at " + np + ": the connection was refused: GET " + np + " " + names + ": connection refused"},
		{"request, unreached", unreached, []string{"request", "GRANT_ME"}, "POST " + np + " " + names + ": connection refused"},
		{"request --json, unreached", unreached, []string{"request", "--json", "GRANT_ME"}, "POST " + np + " " + names},
		{"status, unreached", unreached, []string{"status", "req-approved"}, "GET " + np + " " + names},
		{"exec, unreached", unreached, injection, "POST " + np + " " + names},
		{"secret list, unreached", unreached, []string{"secret", "list"}, np},
		{"self, answered", answering, []string{"self"}, "source: the broker AGENT_SECRETS_URL names (GCP Secret Manager)"},
		{"status, answered", answering, []string{"status", "req-approved"}, "source: the broker AGENT_SECRETS_URL names (GCP Secret Manager)"},
		{"exec, refused by the broker", answering, injection, "asked the broker AGENT_SECRETS_URL names"},
		{"a port no parser reads", "http://tokuser9f3:hunter2pw@127.0.0.1:badport", []string{"self"}, "the broker's address is not a valid URL: GET " + np},
		{"a bad escape in the password", badEscape, []string{"self"}, "the broker's address is not a valid URL: GET " + np},
		{"a bad escape in the password, injecting", badEscape, injection, "the broker's address is not a valid URL"},
		{"a scheme-less URL", "tokuser9f3:hunter2pw@127.0.0.1:1", []string{"self"}, "GET " + np + " " + names + ": the request failed"},
		{"a plain-http broker on the network, secret list", "http://tokuser9f3:hunter2pw@example.internal", []string{"secret", "list"}, `AGENT_SECRETS_URL "` + np + `" must be https`},
		{"a token in the query", "http://127.0.0.1:1/?token=hunter2pw", []string{"self"}, names + ": connection refused"},
		{"one slash after the scheme", "http:/tokuser9f3:hunter2pw@127.0.0.1:1", []string{"self"}, names},
		{"no colon after the scheme", "http//tokuser9f3:hunter2pw@127.0.0.1:1", []string{"self"}, names},
		{"a token holding a slash", "http://AbCtok9f3/dEfgh+ij=@127.0.0.1:1", []string{"self"}, names + ": name resolution failed"},
		{"a short token holding a slash", "http://ab/cd==@127.0.0.1:1", []string{"self"}, names},
		{"a password beginning with digits", "http://tokuser9f3:1234/hunter2pw@127.0.0.1:1", []string{"self"}, names},
		{"an '@' in a query value", "http://127.0.0.1:1/?password=hunter@2pwSECRET", []string{"self"}, names},
		{"an '@' in one of several query values", "http://127.0.0.1:1/?a=1&token=qtok@77SECRET&b=2", []string{"self"}, names},
		{"an '@' in the fragment", "http://127.0.0.1:1/#frag@9zSECRET", []string{"self"}, names},
		{"a percent-encoded '@'", "http://tokuser9f3:hunter2pw%40127.0.0.1:1", []string{"self"}, names},
		{"a literal '@' and an escaped one", "http://ada@corp.example:hunter2pw%40127.0.0.1:1", []string{"self"}, names},
		{"a double-escaped '@'", "http://tokuser9f3:hunter2pw%2540127.0.0.1:1", []string{"self"}, names},
		{"a short token head before a '?'", "http://Ab?cdefghijklSECRET@127.0.0.1:1", []string{"self"}, names},
		{"an escaped newline forging a line", "http://tokuser9f3:hunter2pw%40127.0.0.1:1%0Aagent-secrets: GRANT_ME from FORGED", []string{"self"}, names},
		{"a raw newline forging a line", "http://127.0.0.1:1\nagent-secrets: GRANT_ME from FORGED", []string{"self"}, names},
	} {
		t.Run(row.name, func(t *testing.T) {
			stdout, stderr, _ := runAgentSecrets(t, binary, row.base, keyDir, nil, row.args...)
			output := stdout + stderr
			for _, secret := range []string{"tokuser9f3", "hunter2pw", "%zz", "AbCtok9f3", "dEfgh", "1234/", "2pwSECRET", "77SECRET", "9zSECRET", "klSECRET",
				"qtok", "ada@corp", "2540", "cd==", "lookup Ab", "host ab", "FORGED"} {
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

// TestAnIDThatWouldNotNameOnePathSegmentIsRefusedByName: against a broker that verifies the
// proof as the real one does (proof.Verifier, with the htu rebuilt from the decoded path), a
// request id url.PathEscape would change is refused before any call, naming the id and not the
// broker's address, while an ordinary id still verifies and is answered.
func TestAnIDThatWouldNotNameOnePathSegmentIsRefusedByName(t *testing.T) {
	binary := buildAgentSecrets(t)
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDir := writeKeyDir(t, key, testEnrollmentID)
	verifier := &proof.Verifier{Skew: time.Minute,
		Lookup: func(_ context.Context, id string) (string, bool, error) {
			return thumbprint, id == testEnrollmentID, nil
		},
		Replay: func(context.Context, string, time.Time) (bool, error) { return true, nil }}
	var publicURL string
	var reached, verified atomic.Int32
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/requests/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		reached.Add(1)
		if _, err := verifier.Verify(r.Context(), r.Header.Get("Proof"), r.Method, publicURL+r.URL.Path, time.Now()); err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"PROOF_INVALID","error":"the proof does not verify"}`))
			return
		}
		verified.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(approvedStatusJSON))
	}))
	defer broker.Close()
	publicURL = broker.URL
	for _, id := range []string{"req%zz", "a b", "a/b", "req%25zz"} {
		_, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil, "status", id)
		want := "agent-secrets status: " + strconv.Quote(id) + " is not a request id\n"
		if exit == 0 || stderr != want {
			t.Errorf("status %q: exit %d, stderr %q; want exactly %q, with no trailer since nothing was sent", id, exit, stderr, want)
		}
	}
	if n := reached.Load(); n != 0 {
		t.Fatalf("the broker was called %d times for ids the client must refuse", n)
	}
	for _, id := range []string{"0f9a6c1e-2b7d-4c3a-9e5f-8a1b2c3d4e5f", "req-approved"} {
		_, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil, "status", id)
		if exit != 0 {
			t.Errorf("status %q: exit %d, stderr %q; want the broker's answer", id, exit, stderr)
		}
	}
	if n := verified.Load(); n != 2 {
		t.Fatalf("%d proofs verified, want 2: an ordinary id must sign the path the broker rebuilds", n)
	}
}

// TestAPeerThatBrokeTheExchangeIsNamed: with a clean AGENT_SECRETS_URL, a real http server asked
// over https and a real server that closes the connection each read as a peer that answered and
// broke the exchange, with net/http's own cause.
func TestAPeerThatBrokeTheExchangeIsNamed(t *testing.T) {
	binary := buildAgentSecrets(t)
	keyDir := newKeyDir(t)
	plain := httptest.NewServer(http.NotFoundHandler())
	defer plain.Close()
	closing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			conn.Close()
		}
	}))
	defer closing.Close()
	for _, row := range []struct{ base, cause string }{
		{"https://" + strings.TrimPrefix(plain.URL, "http://"), "http: server gave HTTP response to HTTPS client"},
		{closing.URL, "EOF"},
	} {
		_, stderr, _ := runAgentSecrets(t, binary, row.base, keyDir, nil, "self")
		want := "answered but the exchange failed: GET " + row.base + "/v1/enrollments/self (AGENT_SECRETS_URL names " + row.base + "): " + row.cause + "\n"
		if !strings.HasSuffix(stderr, want) {
			t.Errorf("AGENT_SECRETS_URL=%s: stderr %q, want it to end %q", row.base, stderr, want)
		}
	}
}

// TestARedirectsTargetIsNotPrinted: a clean broker URL that redirects (a proxy's sign-in, say)
// to a URL with a query, to another path, or through a malformed Location, and then fails,
// prints the failure's class alone: net/http's error names the redirect's target.
func TestARedirectsTargetIsNotPrinted(t *testing.T) {
	binary := buildAgentSecrets(t)
	keyDir := newKeyDir(t)
	for _, row := range []struct{ location, class string }{
		{"http://127.0.0.1:1/signin?state=qtok77SECRET", "connection refused"},
		{"/magic/AbCtok9f3SECRET", "the request failed"},
		{"http://127.0.0.1:1/signin%zz?state=qtok77SECRET", "the request failed"},
	} {
		redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header()["Location"] = []string{row.location}
			w.WriteHeader(http.StatusFound)
		}))
		_, stderr, _ := runAgentSecrets(t, binary, redirector.URL, keyDir, nil, "self")
		redirector.Close()
		if strings.Contains(stderr, "qtok77SECRET") || strings.Contains(stderr, "AbCtok9f3SECRET") || strings.Contains(stderr, "/signin") || strings.Contains(stderr, "/magic") {
			t.Errorf("Location %q: stderr %q names the redirect's target", row.location, stderr)
		}
		if want := "(AGENT_SECRETS_URL names " + redirector.URL + "): " + row.class; !strings.Contains(stderr, want) {
			t.Errorf("Location %q: stderr %q, want it to name %q", row.location, stderr, want)
		}
	}
}

// TestNoSessionLineNamesAHostOrAStoreID: every line a session's forms print about where an
// answer came from - the injection line, a denial, a refusal, a pending request, a grant that
// released no value, and the source: line of request, status and self - names the broker
// AGENT_SECRETS_URL names and the store's kind, and no host, account, region or project, for
// each store kind the broker's settings can describe; each outcome keeps its exit code.
//
// One line is excepted, and only for the exact string the fake broker answered with: a pending
// request's waiting line names approve_url, the record's page on the broker, because a person
// must open that page to decide the request (A12). Its host is the broker's, which
// AGENT_SECRETS_URL already names, so the page tells a reader nothing the command's own
// configuration does not. Everything the invariant forbids is still forbidden, the host anywhere
// else in the output included: the exception removes that one URL and then checks the list.
func TestNoSessionLineNamesAHostOrAStoreID(t *testing.T) {
	binary := buildAgentSecrets(t)
	keyDir := newKeyDir(t)
	run := func(name string) []string {
		return []string{name, "--wait", "200ms", "--", "sh", "-c", "echo the-child-ran"}
	}
	forms := []struct {
		args []string
		exit int
	}{
		{[]string{"self"}, 0},
		{[]string{"status", "req-approved"}, 0},
		{[]string{"request", "GRANT_ME"}, 0},
		{[]string{"request", "PENDING_ME"}, exitPending},
		{[]string{"request", "DENY_ME"}, exitDenied},
		{[]string{"request", "NO_SUCH_NAME"}, 1},
		{[]string{"cancel", "req-unknown"}, 1},
		{run("GRANT_ME"), 0},
		{run("DENY_ME"), exitDenied},
		{run("UNRELEASED_ME"), 1},
		{run("PENDING_ME"), exitPending},
		{run("NO_SUCH_NAME"), 1},
	}
	for _, kind := range []struct {
		name     string
		settings *Settings // nil: a broker too old to serve GET /v1/settings
		source   string
	}{
		{"gcp", &fakeSettings, "the broker AGENT_SECRETS_URL names (GCP Secret Manager)"},
		{"aws", &Settings{Store: secrets.StoreAWS, AWSAccountID: "111122223333", AWSRegion: "us-east-1"}, "the broker AGENT_SECRETS_URL names (AWS Secrets Manager)"},
		{"a store this release does not know", &Settings{Store: "vault"}, "the broker AGENT_SECRETS_URL names (vault)"},
		{"no settings route", nil, "the broker AGENT_SECRETS_URL names"},
	} {
		broker, counters := fakeBroker(t)
		defer broker.Close()
		if kind.settings == nil {
			counters.hideSettings()
		} else {
			counters.serveSettings(*kind.settings)
		}
		host := strings.TrimPrefix(broker.URL, "http://")
		for _, form := range forms {
			t.Run(kind.name+"/"+strings.Join(form.args, " "), func(t *testing.T) {
				stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil, form.args...)
				output := stdout + stderr
				if exit != form.exit {
					t.Fatalf("exit %d, want %d (stdout %q, stderr %q)", exit, form.exit, redactSecrets(stdout), redactSecrets(stderr))
				}
				if !strings.Contains(output, kind.source) {
					t.Fatalf("stdout %q, stderr %q; want them to name %q", redactSecrets(stdout), redactSecrets(stderr), kind.source)
				}
				// The one exception, and only the exact URL this broker answered with: a
				// pending request's waiting line names the page a person must open to decide
				// it. Taking that string out and then checking the list is what keeps the
				// exception narrow — the host anywhere else, 127.0.0.1 outside that URL, the
				// account, the region and "the broker at" are all still refused below.
				if page := approveURLOf(form.args, broker.URL); page != "" {
					output = strings.ReplaceAll(output, page, "")
				}
				for _, absent := range []string{host, "127.0.0.1", "000000000000", "111122223333", "us-east-1", "the broker at"} {
					if strings.Contains(output, absent) {
						t.Fatalf("stdout %q, stderr %q name %q", redactSecrets(stdout), redactSecrets(stderr), absent)
					}
				}
			})
		}
	}
}

// approveURLOf is the approve_url fakeBroker answers a form with, and "" for a form that reaches
// no pending request or one the fake broker answers as an older broker would. It reads the
// fixtures rather than matching on text, so the exception covers the exact string the broker
// returned and nothing that merely looks like it.
func approveURLOf(args []string, brokerURL string) string {
	for _, arg := range args {
		p, ok := fakePending[arg]
		if ok && p.page {
			return brokerURL + "/approve/" + p.recordID
		}
	}
	return ""
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
		{"an HTTP answer to HTTPS, as net/http reports it", http.ErrSchemeMismatch, answered, ""},
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
