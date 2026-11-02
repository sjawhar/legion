// packages/envoy/cmd/agent-secrets/vm_proxy_test.go
//
// A VM's identity token logs in as that VM for up to an hour, so it must reach no one but the
// metadata server. These tests run the real binary, a fresh process that reads HTTP_PROXY itself,
// and point the metadata host at a name that is not loopback, which a proxy-honouring client would
// send through the proxy.
package main

import (
	"net"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// unresolvableMetadataHost is a metadata host that is not loopback and resolves nowhere (.invalid
// is reserved), so a client that skips the proxy fails at its own DNS lookup and one that honours
// HTTP_PROXY dials the proxy with it.
const unresolvableMetadataHost = "metadata.agent-secrets-test.invalid"

// countingProxy listens as an HTTP proxy would and counts every connection made to it, closing each
// at once. It answers the proxy URL and the count.
func countingProxy(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	var dials atomic.Int32
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			dials.Add(1)
			conn.Close()
		}
	}()
	return "http://" + listener.Addr().String(), &dials
}

// proxiedVMEnv is the environment of a VM whose shell sets a proxy for every scheme, with the
// metadata host at unresolvableMetadataHost and the broker at broker.
func proxiedVMEnv(proxy, broker string, extra ...string) []string {
	env := append(os.Environ(),
		"HTTP_PROXY="+proxy, "http_proxy="+proxy, "HTTPS_PROXY="+proxy, "https_proxy="+proxy,
		"ALL_PROXY="+proxy, "all_proxy="+proxy, "NO_PROXY=", "no_proxy=",
		"GCE_METADATA_HOST="+unresolvableMetadataHost,
		"AGENT_SECRETS_URL="+broker, "AGENT_SECRETS_AUDIENCE="+testVMAudience,
	)
	return append(env, extra...)
}

// TestEnrollVMNeverSendsTheMetadataRequestThroughAProxy: `enroll --vm` on a VM whose environment
// sets HTTP_PROXY asks the metadata server directly: the proxy is never dialled, and the command
// fails on the metadata host it could not reach itself.
func TestEnrollVMNeverSendsTheMetadataRequestThroughAProxy(t *testing.T) {
	binary := buildAgentSecrets(t)
	proxy, dials := countingProxy(t)
	broker, _ := serveVMBroker(t, "never-reached")
	cmd := exec.Command(binary, "enroll", "--vm", "--service", testVMService, "--out", t.TempDir())
	cmd.Env = proxiedVMEnv(proxy, broker.URL)
	out, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		t.Fatalf("enroll --vm with no reachable metadata server = %v, want exit 1:\n%s", err, out)
	}
	if !strings.Contains(string(out), "the metadata server at http://"+unresolvableMetadataHost) {
		t.Fatalf("enroll --vm output = %q, want it to name the metadata server it could not reach", out)
	}
	if n := dials.Load(); n != 0 {
		t.Fatalf("enroll --vm dialled the proxy %d times; the metadata request must never go through one", n)
	}
}

// TestRenewVMNeverSendsTheMetadataRequestThroughAProxy: `renew --vm`, which fetches a fresh token
// before every renewal, never dials the proxy either: it reports the token it could not read and
// retries, until it is stopped.
func TestRenewVMNeverSendsTheMetadataRequestThroughAProxy(t *testing.T) {
	binary := buildAgentSecrets(t)
	proxy, dials := countingProxy(t)
	broker, _ := serveVMBroker(t, testEnrollmentID)
	cmd := exec.Command(binary, "renew", "--vm")
	cmd.Env = proxiedVMEnv(proxy, broker.URL, "AGENT_SECRETS_KEY_DIR="+newKeyDir(t))
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Long enough for the first fetch and the retry after its 2 s backoff.
	time.Sleep(3 * time.Second)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("renew --vm stopped with %v:\n%s", err, out.String())
	}
	if n := strings.Count(out.String(), "cannot read its VM identity token"); n < 2 {
		t.Fatalf("renew --vm reported %d unreadable tokens, want at least 2 (the first fetch and its retry):\n%s", n, out.String())
	}
	if n := dials.Load(); n != 0 {
		t.Fatalf("renew --vm dialled the proxy %d times; the metadata request must never go through one", n)
	}
}
