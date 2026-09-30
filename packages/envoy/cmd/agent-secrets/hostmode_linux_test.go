// packages/envoy/cmd/agent-secrets/hostmode_linux_test.go
//go:build linux

// fakeHelperWithPids and TestRegisterExecRunsTheCommandAsTheRegisteredPid need helper.PeerOf
// (SO_PEERPIDFD), which is Linux-only (internal/broker/helper/peer.go carries the same
// //go:build linux); everything else in hostmode_test.go that never needs to observe a
// connecting pid stays untagged so a darwin build of this package's test binary isn't broken by
// a helper detail no darwin client build ever touches.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/helper"
)

// fakeHelperWithPids answers one canned line per connection and records the peer pid of each
// request — the pidfd pin only Linux provides.
func fakeHelperWithPids(t *testing.T, canned helper.Response) (string, <-chan int) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "h.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	pids := make(chan int, 8)
	go func() {
		for {
			conn, err := ln.AcceptUnix()
			if err != nil {
				return
			}
			peer, err := helper.PeerOf(conn)
			if err == nil {
				pids <- peer.PID()
				peer.Close()
			}
			_, _ = bufio.NewReader(conn).ReadBytes('\n')
			data, _ := json.Marshal(canned)
			_, _ = conn.Write(append(data, '\n'))
			conn.Close()
		}
	}()
	return sock, pids
}

// TestRegisterExecRunsTheCommandAsTheRegisteredPid must run in a subprocess: --exec replaces the
// process image.
func TestRegisterExecRunsTheCommandAsTheRegisteredPid(t *testing.T) {
	sock, pids := fakeHelperWithPids(t, helper.Response{OK: true, State: "enrolling", RuntimeID: "h:1:1"})
	cmd := exec.Command("go", "run", ".", "register", "--exec", "--", "sh", "-c", "echo $$")
	cmd.Env = append(os.Environ(), "AGENT_SECRETS_HELPER_SOCK="+sock)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("register --exec: %v", err)
	}
	printed, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("sh must print its pid, got %q", out)
	}
	select {
	case registered := <-pids:
		if registered != printed {
			t.Fatalf("the pid the helper pinned (%d) must be the pid the command runs as (%d)", registered, printed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the helper saw no register")
	}
}

// realHelper serves a real helper.Server — the daemon's own registry, pidfd pinning and sign op —
// on a socket, with a broker that can never enroll anyone (no launcher credential, nothing
// listening), so every session it registers stays unenrolled.
func realHelper(t *testing.T) string {
	t.Helper()
	_, sock := serveRealHelper(t, "http://127.0.0.1:1")
	return sock
}

// serveRealHelper serves a real helper.Server against the broker at brokerURL, with an operator
// file so a test can log it in (Broker.Login). It waits for every session to be retired, and the
// server to stop, before the test's temporary directories go.
func serveRealHelper(t *testing.T, brokerURL string) (*helper.Server, string) {
	t.Helper()
	dir := t.TempDir()
	operator := filepath.Join(dir, "operator")
	if err := os.WriteFile(operator, []byte("sjawhar\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := &helper.Server{
		Registry: helper.NewRegistry(filepath.Join(dir, "sessions.json")),
		Broker:   &helper.Broker{URL: brokerURL, OperatorFile: operator, HTTP: http.DefaultClient},
		Hostname: "testhost",
		PeerOf:   helper.PeerOf,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		MinRenew: 50 * time.Millisecond,
	}
	sock := filepath.Join(dir, "helper.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() { defer close(served); _ = srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		deadline := time.Now().Add(10 * time.Second)
		for len(srv.Registry.List()) > 0 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
		<-served
	})
	return srv, sock
}

func helperSessions(t *testing.T, sock string) []helper.SessionInfo {
	t.Helper()
	resp, err := helper.Call(sock, helper.Request{Op: "sessions"}, 5*time.Second)
	if err != nil || !resp.OK {
		t.Fatalf("sessions: %+v %v", resp, err)
	}
	return resp.Sessions
}

// TestIdentityAsksARealHelperWithoutRegistering runs identity against the real helper from a
// process no session registered: it answers 1 (NOT_A_SESSION) and leaves the helper's sessions
// exactly as they were — identity must never become a registration, which is what `register`
// from an unregistered process is. nocredential_linux_test.go covers identity inside a
// registered session.
func TestIdentityAsksARealHelperWithoutRegistering(t *testing.T) {
	binary := buildAgentSecrets(t)
	sock := realHelper(t)
	env := append(os.Environ(), "AGENT_SECRETS_HELPER_SOCK="+sock, "AGENT_SECRETS_KEY_DIR=", "XDG_RUNTIME_DIR="+t.TempDir(), "HOME="+t.TempDir())

	before := helperSessions(t, sock)
	cmd := exec.Command(binary, "identity")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if code := cmd.ProcessState.ExitCode(); code != 1 {
		t.Fatalf("identity from an unregistered process: exit %d (%v), output %q; want 1", code, err, out)
	}
	if after := helperSessions(t, sock); len(after) != len(before) {
		t.Fatalf("identity changed the helper's sessions: before %+v, after %+v", before, after)
	}
}
