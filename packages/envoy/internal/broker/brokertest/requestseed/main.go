// Command requestseed puts pending secret requests on a running broker for Dispatch's browser
// suite (packages/dispatch/e2e/broker-approval.e2e.ts), as pods would: a service logs in with
// its ServiceAccount token, enrolls pods that prove their accounts with projected tokens, and each
// pod asks for a human-tier secret, through brokertest.Pods, the client cmd/broker's tests drive
// the binary with. The tokens come from the dev issuer's POST /dev/mint (cmd/dev-oidc-issuer), the
// issuer that broker trusts. It lives under internal/ so no release builds or ships it.
//
//	requestseed ask --broker <url> --issuer <url> --state <file> --secret <name> [--count <n>]
//	requestseed state --broker <url> --state <file>
//	requestseed withdraw --broker <url> --state <file>
//
// ask enrolls n pods (1 unless given), each asking for the secret once, so each request waits on
// a record of its own; it writes each pod's key and enrollment beside its request into the state
// file, readable by its owner alone, and prints {"requests": [{request_id, record_id,
// approve_url}]}. state prints {"requests": [{request_id, state, grant_id}]}, each read by the pod
// that made it. withdraw cancels each of the file's requests that is still pending, so a row
// leaves the approver's pending list as it found it. Every exchange with the broker is logged to
// stderr, without its credentials; any answer outside the protocol ends the command, 1.
package main

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/brokertest"
)

// The accounts and audience the browser suite's broker is configured with
// (packages/dispatch/e2e/run-broker.sh): the service whose token login enrolls the pods, the
// account every pod runs as, and the audience the broker takes their tokens for.
const (
	podAudience     = "example-broker-pod"
	launcherService = "example-launcher"
	launcherAccount = "system:serviceaccount:example-system:example-launcher"
	podAccount      = "system:serviceaccount:example:example-worker"
)

const exitUsageError = 2

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// seededPod is one pod as the state file keeps it: its runtime id, its enrollment, its key (DER,
// SEC 1), and the request it made.
type seededPod struct {
	RuntimeID    string `json:"runtime_id"`
	EnrollmentID string `json:"enrollment_id"`
	Key          []byte `json:"key"`
	RequestID    string `json:"request_id"`
}

// seeded is a state file's content.
type seeded struct {
	Pods []seededPod `json:"pods"`
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "requestseed: want ask, state or withdraw")
		return exitUsageError
	}
	fs := flag.NewFlagSet("requestseed "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	broker := fs.String("broker", "", "the broker's public `<url>`")
	stateFile := fs.String("state", "", "the `<file>` ask writes and state and withdraw read")
	issuer := fs.String("issuer", "", "ask: the dev issuer's `<url>`, whose /dev/mint mints the tokens")
	secret := fs.String("secret", "", "ask: the `<name>` of the secret each pod asks for")
	count := fs.Int("count", 1, "ask: how many pods ask, `<n>`")
	if err := fs.Parse(args[1:]); err != nil {
		return exitUsageError
	}
	if *broker == "" || *stateFile == "" || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "requestseed: want --broker and --state, and no other argument")
		return exitUsageError
	}
	t := &command{stderr: stderr}
	switch args[0] {
	case "ask":
		if *issuer == "" || *secret == "" || *count < 1 {
			fmt.Fprintln(stderr, "requestseed ask: want --issuer, --secret and a --count of at least 1")
			return exitUsageError
		}
		pods := brokertest.Pods{T: t, Base: *broker, Token: devIssuerTokens(t, *issuer)}
		return t.print(stdout, ask(t, pods, *stateFile, *secret, *count))
	case "state":
		pods := brokertest.Pods{T: t, Base: *broker}
		return t.print(stdout, states(t, pods, t.read(*stateFile)))
	case "withdraw":
		pods := brokertest.Pods{T: t, Base: *broker}
		withdraw(t, pods, t.read(*stateFile))
		return 0
	}
	fmt.Fprintf(stderr, "requestseed: unknown command %q: want ask, state or withdraw\n", args[0])
	return exitUsageError
}

// seededRequest is one request ask made, as it prints it.
type seededRequest struct {
	RequestID  string `json:"request_id"`
	RecordID   string `json:"record_id"`
	ApproveURL string `json:"approve_url"`
}

// ask logs the service in, enrolls count pods, has each ask for secret, writes them to stateFile,
// and answers each pending request with its record and page.
func ask(t *command, pods brokertest.Pods, stateFile, secret string, count int) any {
	launcher := pods.LogIn(launcherService, launcherAccount)
	var file seeded
	var requests []seededRequest
	for range count {
		p := pods.Enroll(launcher, uuid.NewString(), podAccount)
		reply := pods.Ask(p, secret)
		if reply.State != "pending" || reply.RecordID == nil || reply.ApproveURL == "" {
			t.Fatalf("%s's request for %s = %+v, want pending on a record with its approve_url", p.RuntimeID, secret, reply)
		}
		key, err := x509.MarshalECPrivateKey(p.Key)
		if err != nil {
			t.Fatalf("encode %s's key: %v", p.RuntimeID, err)
		}
		file.Pods = append(file.Pods, seededPod{RuntimeID: p.RuntimeID, EnrollmentID: p.EnrollmentID, Key: key, RequestID: reply.RequestID})
		requests = append(requests, seededRequest{RequestID: reply.RequestID, RecordID: *reply.RecordID, ApproveURL: reply.ApproveURL})
	}
	content, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("encode %s: %v", stateFile, err)
	}
	if err := os.WriteFile(stateFile, content, 0o600); err != nil {
		t.Fatalf("write %s: %v", stateFile, err)
	}
	return map[string]any{"requests": requests}
}

// requestState is one request's state, as state prints it.
type requestState struct {
	RequestID string  `json:"request_id"`
	State     string  `json:"state"`
	GrantID   *string `json:"grant_id"`
}

// states reads each seeded pod's request as that pod.
func states(t *command, pods brokertest.Pods, file seeded) any {
	var answer []requestState
	for _, s := range file.Pods {
		reply := pods.State(t.pod(s), s.RequestID)
		answer = append(answer, requestState{RequestID: s.RequestID, State: reply.State, GrantID: reply.GrantID})
	}
	return map[string]any{"requests": answer}
}

// withdraw cancels each seeded request that is still pending, as the pod that made it.
func withdraw(t *command, pods brokertest.Pods, file seeded) {
	for _, s := range file.Pods {
		p := t.pod(s)
		if pods.State(p, s.RequestID).State == "pending" {
			pods.Cancel(p, s.RequestID)
		}
	}
}

// devIssuerTokens is a Pods' Token minted by the dev issuer at issuer through its POST /dev/mint,
// for a random pod when podUID is empty.
func devIssuerTokens(t *command, issuer string) func(account, podUID string) string {
	return func(account, podUID string) string {
		body, err := json.Marshal(map[string]string{"subject": account, "audience": podAudience, "pod_uid": podUID})
		if err != nil {
			t.Fatalf("encode the mint request: %v", err)
		}
		response, err := http.Post(issuer+"/dev/mint", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("POST %s/dev/mint: %v", issuer, err)
		}
		defer response.Body.Close()
		answer, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatalf("read the answer to POST %s/dev/mint: %v", issuer, err)
		}
		var minted struct {
			Token string `json:"token"`
		}
		if response.StatusCode != http.StatusOK || json.Unmarshal(answer, &minted) != nil || minted.Token == "" {
			// The answer is printed only when it carries no token.
			t.Fatalf("POST %s/dev/mint for %s = %d %s, want 200 with a token", issuer, account, response.StatusCode, bytes.TrimSpace(answer))
		}
		return minted.Token
	}
}

// command is brokertest.TB for this command rather than a test: a log line goes to stderr, and a
// failure ends the process, 1, as it ends a test.
type command struct {
	stderr io.Writer
}

func (*command) Helper() {}

func (c *command) Logf(format string, args ...any) {
	fmt.Fprintf(c.stderr, format+"\n", args...)
}

func (c *command) Fatalf(format string, args ...any) {
	fmt.Fprintf(c.stderr, "requestseed: "+format+"\n", args...)
	os.Exit(1)
}

// read reads the state file ask wrote.
func (c *command) read(stateFile string) seeded {
	content, err := os.ReadFile(stateFile)
	if err != nil {
		c.Fatalf("%v", err)
	}
	var file seeded
	if err := json.Unmarshal(content, &file); err != nil {
		c.Fatalf("%s: %v", stateFile, err)
	}
	if len(file.Pods) == 0 {
		c.Fatalf("%s names no pod", stateFile)
	}
	return file
}

// pod is the seeded pod s, its key decoded.
func (c *command) pod(s seededPod) brokertest.Pod {
	key, err := x509.ParseECPrivateKey(s.Key)
	if err != nil {
		c.Fatalf("decode %s's key: %v", s.RuntimeID, err)
	}
	return brokertest.Pod{RuntimeID: s.RuntimeID, Key: key, EnrollmentID: s.EnrollmentID}
}

// print writes answer to stdout as JSON.
func (c *command) print(stdout io.Writer, answer any) int {
	if err := json.NewEncoder(stdout).Encode(answer); err != nil {
		c.Fatalf("write the answer: %v", err)
	}
	return 0
}
