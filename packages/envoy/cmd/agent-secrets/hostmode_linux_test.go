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
	"encoding/json"
	"net"
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
