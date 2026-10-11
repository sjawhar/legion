package workspace

import (
	"context"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// hangingServer accepts an HTTPS connection and never writes a byte of response: git's
// git-remote-https helper sits forever waiting for a reply unless something outside curl's own
// retry logic ends it.
func hangingServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(func() {
		// Close alone blocks until every in-flight handler returns, which a connection this
		// server never answers would never do on its own: sever it first.
		server.CloseClientConnections()
		server.Close()
	})
	return server
}

// trustHangingServerCA makes the fetch's own clone (which reads no config file, GIT_CONFIG_GLOBAL
// and GIT_CONFIG_NOSYSTEM both pinned) trust server's self-signed certificate: GIT_SSL_CAINFO is
// an environment variable git's http transport reads directly, independent of any config file.
func trustHangingServerCA(t *testing.T, server *httptest.Server) {
	t.Helper()
	caFile := filepath.Join(t.TempDir(), "server-ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(caFile, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSL_CAINFO", caFile)
}

// shortTimeoutRunner retargets the fetch's clone at a local server and cuts Fetch's real
// FetchTimeout (30 minutes, never practical to wait out) down to timeout, so the test proves
// execRunner.Run's own process-group kill without waiting real minutes: the production Fetch
// through NewRunner, against a local TLS server that accepts a connection and never answers, with
// the timeout cut to a few seconds.
type shortTimeoutRunner struct {
	remote  string
	timeout time.Duration
	run     Runner
}

func (r *shortTimeoutRunner) Timeout() time.Duration { return r.run.Timeout() }

func (r *shortTimeoutRunner) Run(ctx context.Context, command Command) (Result, error) {
	if commandWith(command.Argv, "git", "clone", "--bare", "--quiet", "https://github.com/acme/widgets") {
		rewritten := append([]string(nil), command.Argv...)
		rewritten[4] = r.remote
		command.Argv = rewritten
		command.Timeout = r.timeout
	}
	return r.run.Run(ctx, command)
}

// git's https transport spawns git-remote-https, a helper holding the same stdout/stderr pipes as
// the git process exec.CommandContext starts directly: killing only that direct child when the
// deadline fires leaves the helper, now reparented, holding the pipe open, and Wait (so Run, so
// Fetch) never returns. Against a server that accepts the connection and never answers, with the
// clone's timeout cut to 3 s, Fetch must still return within a few seconds of that deadline, with
// an error naming the timeout, and no git process of this test still running.
func TestFetchKillsGitsWholeProcessTreeWhenItsTimeoutFires(t *testing.T) {
	run := newLocalRunner(t)
	server := hangingServer(t)
	trustHangingServerCA(t, server)
	req := fetchRequest(t)
	timeout := 3 * time.Second

	start := time.Now()
	_, err := Fetch(context.Background(), &shortTimeoutRunner{remote: server.URL + "/widgets.git", timeout: timeout, run: run}, req)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Fetch: no error, want the clone's own timeout to end it")
	}
	if !strings.Contains(err.Error(), "command timed out") {
		t.Errorf("Fetch error = %q, want it to name the timeout (\"command timed out\")", err)
	}
	if elapsed > timeout+3*time.Second {
		t.Errorf("Fetch took %s to return, want within a few seconds of its %s timeout", elapsed, timeout)
	}

	if leftover := gitProcessesUnder(t, server.URL); len(leftover) != 0 {
		t.Errorf("git processes still running after Fetch returned: %v", leftover)
	}
}

// gitProcessesUnder scans /proc for a process whose command line names marker (server.URL, the
// one local address the fetch's clone and git-remote-https both reach): a git or git-remote-https
// process orphaned by a timeout that killed only its parent would still show up this way. A
// request's own CredentialDir never appears in any git argv — the credential helper reaches git
// through GIT_CONFIG_VALUE_1 (clone.go), not a path — so scanning for it finds nothing whether or
// not a process leaked.
func gitProcessesUnder(t *testing.T, marker string) []string {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Skipf("/proc unavailable: %v", err)
	}
	var found []string
	for _, entry := range entries {
		cmdline, err := os.ReadFile("/proc/" + entry.Name() + "/cmdline")
		if err != nil {
			continue
		}
		args := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
		joined := strings.Join(args, " ")
		if strings.Contains(joined, marker) {
			found = append(found, fmt.Sprintf("pid %s: %s", entry.Name(), joined))
		}
	}
	return found
}
