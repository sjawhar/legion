package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
)

const (
	sessionOperatorToken = "session-operator-token"
	sessionPodToken      = "eyJ.session-pod-token.sig"
)

// sessionDaemon is a stand-in for the daemon's two session routes: it records every request and
// answers what the test set, so a run's argv, its files and the request it makes are all the test
// reads.
type sessionDaemon struct {
	t         *testing.T
	port      int
	tokenFile string
	podToken  string

	status int
	answer any

	mu   sync.Mutex
	seen []sessionRequest
}

type sessionRequest struct {
	path          string
	authorization string
	body          []byte
}

func newSessionDaemon(t *testing.T) *sessionDaemon {
	t.Helper()
	d := &sessionDaemon{
		t: t, status: http.StatusOK,
		tokenFile: writeFile(t, "operator-token", sessionOperatorToken+"\n"),
		podToken:  writeFile(t, "pod-token", sessionPodToken+"\n"),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read the request body: %v", err)
		}
		d.mu.Lock()
		d.seen = append(d.seen, sessionRequest{path: r.URL.Path, authorization: r.Header.Get("Authorization"), body: body})
		d.mu.Unlock()
		encoded, err := json.Marshal(d.answer)
		if err != nil {
			t.Errorf("encode the answer: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(d.status)
		_, _ = w.Write(encoded)
	}))
	t.Cleanup(server.Close)
	d.port = server.Listener.Addr().(*net.TCPAddr).Port
	return d
}

func (d *sessionDaemon) requests() []sessionRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]sessionRequest(nil), d.seen...)
}

// session runs `legion session <args>` with the daemon's port and the operator token file added,
// and holds every run to the one rule all of them keep: neither the operator bearer nor the pod's
// own token is in either stream.
func (d *sessionDaemon) session(args ...string) (int, string, string) {
	d.t.Helper()
	argv := append([]string{"legion", "session", args[0], "--port", strconv.Itoa(d.port), "--operator-token-file", d.tokenFile}, args[1:]...)
	var out, errb bytes.Buffer
	code := run(context.Background(), argv, &out, &errb)
	for name, stream := range map[string]string{"stdout": out.String(), "stderr": errb.String()} {
		if strings.Contains(stream, sessionOperatorToken) || strings.Contains(stream, sessionPodToken) {
			d.t.Fatalf("legion session %v printed a secret on %s: %q", args, name, stream)
		}
	}
	return code, out.String(), errb.String()
}

// `legion session enroll` reads the pod's token from the file it names and sends it with the
// thumbprint and the pod's name; the token is in no argv. The bearer is the operator's, and the
// line it prints names the pod uid, the enrollment and the lease's expiry.
func TestSessionEnrollSendsTheTokenFromItsFile(t *testing.T) {
	d := newSessionDaemon(t)
	d.answer = api.SessionEnrollResponse{EnrollmentID: "enr-1", PodUID: "uid-ada", LeaseExpiresAt: "2026-11-04T21:00:00Z"}

	code, out, errb := d.session("enroll", "--pod", "k8s-session-ada", "--thumbprint", "tp-ada",
		"--pod-token-file", d.podToken, "--session", "ses_ada")
	if code != 0 || errb != "" {
		t.Fatalf("legion session enroll = %d, stderr %q", code, errb)
	}
	if want := "enrolled pod uid-ada as enr-1, lease expires 2026-11-04T21:00:00Z\n"; out != want {
		t.Fatalf("stdout = %q, want %q", out, want)
	}
	requests := d.requests()
	if len(requests) != 1 || requests[0].path != "/legion/v1/sessions/agent-secrets" {
		t.Fatalf("requests = %+v, want one to the enrollment route", requests)
	}
	if requests[0].authorization != "Bearer "+sessionOperatorToken {
		t.Fatalf("authorization = %q, want the operator bearer", requests[0].authorization)
	}
	var sent api.SessionEnrollRequest
	if err := json.Unmarshal(requests[0].body, &sent); err != nil {
		t.Fatal(err)
	}
	want := api.SessionEnrollRequest{Pod: "k8s-session-ada", Thumbprint: "tp-ada", PodToken: sessionPodToken, Session: "ses_ada"}
	if sent != want {
		t.Fatalf("sent %+v, want %+v", sent, want)
	}
}

// `legion session github-credential` sends the pod and the owner, prints the one line naming what
// was written, and under --json prints the daemon's answer as it served it.
func TestSessionGitHubCredentialPrintsWhatWasWritten(t *testing.T) {
	d := newSessionDaemon(t)
	d.answer = api.SessionCredentialResponse{
		SecretName: "k8s-session-ada-gh", PodUID: "uid-ada", Default: "sjawhar", App: "implement",
		Owners: []string{"acme", "sjawhar"}, ExpiresAt: "2026-11-04T21:00:00Z",
	}

	code, out, errb := d.session("github-credential", "--pod", "k8s-session-ada", "--pod-uid", "uid-ada", "--owner", "sjawhar")
	if code != 0 || errb != "" {
		t.Fatalf("legion session github-credential = %d, stderr %q", code, errb)
	}
	want := `wrote Secret k8s-session-ada-gh for pod uid-ada: app implement, default sjawhar, owners ["acme","sjawhar"], expires 2026-11-04T21:00:00Z` + "\n"
	if out != want {
		t.Fatalf("stdout = %q, want %q", out, want)
	}
	var sent api.SessionCredentialRequest
	if err := json.Unmarshal(d.requests()[0].body, &sent); err != nil {
		t.Fatal(err)
	}
	if (sent != api.SessionCredentialRequest{Pod: "k8s-session-ada", PodUID: "uid-ada", Owner: "sjawhar"}) {
		t.Fatalf("sent %+v", sent)
	}

	code, out, errb = d.session("github-credential", "--pod", "k8s-session-ada", "--owner", "sjawhar", "--json")
	if code != 0 || errb != "" {
		t.Fatalf("legion session github-credential --json = %d, stderr %q", code, errb)
	}
	var served api.SessionCredentialResponse
	if err := json.Unmarshal([]byte(out), &served); err != nil {
		t.Fatalf("--json printed %q: %v", out, err)
	}
	if served.SecretName != "k8s-session-ada-gh" {
		t.Fatalf("--json printed %+v", served)
	}
}

// A required flag left out, or given empty, is a usage error named by the flag, before anything is
// read or sent — the pod token's file among them, which is required and never a flag value.
func TestSessionRefusesAMissingFlagBeforeReachingTheDaemon(t *testing.T) {
	d := newSessionDaemon(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"enroll", "--thumbprint", "tp", "--pod-token-file", d.podToken}, "--pod is required"},
		{[]string{"enroll", "--pod", "p", "--pod-token-file", d.podToken}, "--thumbprint is required"},
		{[]string{"enroll", "--pod", "p", "--thumbprint", "tp"}, "--pod-token-file is required"},
		{[]string{"enroll", "--pod", "", "--thumbprint", "tp", "--pod-token-file", d.podToken}, "--pod is required"},
		{[]string{"github-credential", "--owner", "sjawhar"}, "--pod is required"},
		{[]string{"github-credential", "--pod", "p"}, "--owner is required"},
		{[]string{"github-credential", "--pod", "p", "--owner", "sjawhar", "stray"}, `unexpected argument "stray"`},
	} {
		code, out, errb := d.session(tc.args...)
		if code != 2 || out != "" || !strings.Contains(errb, tc.want) {
			t.Errorf("legion session %v = %d, stdout %q, stderr %q; want a usage error saying %q", tc.args, code, out, errb, tc.want)
		}
	}
	if n := len(d.requests()); n != 0 {
		t.Fatalf("%d requests reached the daemon, want none", n)
	}
}

// The operator's bearer is required too: `legion session` has no form that reaches the daemon
// without it.
func TestSessionRequiresTheOperatorTokenFile(t *testing.T) {
	d := newSessionDaemon(t)
	for _, args := range [][]string{
		{"legion", "session", "enroll", "--pod", "p", "--thumbprint", "tp", "--pod-token-file", d.podToken},
		{"legion", "session", "github-credential", "--pod", "p", "--owner", "sjawhar"},
	} {
		var out, errb bytes.Buffer
		code := run(context.Background(), append(args, "--port", strconv.Itoa(d.port)), &out, &errb)
		if code != 2 || !strings.Contains(errb.String(), "--operator-token-file is required") {
			t.Errorf("%v = %d, stderr %q; want the usage error", args, code, errb.String())
		}
	}
	if n := len(d.requests()); n != 0 {
		t.Fatalf("%d requests reached the daemon, want none", n)
	}
}

// A pod token file the group or others can read is refused by its path, with nothing of its
// contents said and nothing sent: a token anyone on the box can read is no proof of a pod.
func TestSessionEnrollRefusesAPodTokenFileOthersCanRead(t *testing.T) {
	d := newSessionDaemon(t)
	if err := os.Chmod(d.podToken, 0o644); err != nil {
		t.Fatal(err)
	}

	code, out, errb := d.session("enroll", "--pod", "p", "--thumbprint", "tp", "--pod-token-file", d.podToken)
	if code != 1 || out != "" || !strings.Contains(errb, d.podToken) {
		t.Fatalf("= %d, stdout %q, stderr %q; want the refusal naming the file", code, out, errb)
	}
	if n := len(d.requests()); n != 0 {
		t.Fatalf("%d requests reached the daemon, want none", n)
	}
}

// The daemon's refusal is the command's: its status and sentence are reported and the command
// fails, so a launcher script stops rather than starting a pod with no credential.
func TestSessionReportsTheDaemonsRefusal(t *testing.T) {
	d := newSessionDaemon(t)
	d.status = http.StatusNotFound
	d.answer = api.Failure{Code: "POD_NOT_FOUND", Error: "pod k8s-session-gone in namespace legion: no pod of that name in the deployment's namespace"}

	code, out, errb := d.session("github-credential", "--pod", "k8s-session-gone", "--owner", "sjawhar")
	if code != 1 || out != "" || !strings.Contains(errb, "no pod of that name") {
		t.Fatalf("= %d, stdout %q, stderr %q; want the daemon's refusal", code, out, errb)
	}
}

// `legion session` with no subcommand, an unknown one, or --help names the two it has.
func TestSessionNamesItsSubcommands(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{"legion", "session"}, 2},
		{[]string{"legion", "session", "--help"}, 0},
		{[]string{"legion", "session", "enrol"}, 2},
	} {
		var out, errb bytes.Buffer
		code := run(context.Background(), tc.args, &out, &errb)
		if code != tc.code || !strings.Contains(errb.String(), "usage: legion session enroll|github-credential [flags]") {
			t.Errorf("%v = %d, stderr %q; want %d and the usage line", tc.args, code, errb.String(), tc.code)
		}
	}
}
