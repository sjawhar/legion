// packages/envoy/internal/broker/helper/peer_test.go
//go:build linux

package helper

import (
	"bufio"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func sleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd
}

func TestDescendsFromRealProcesses(t *testing.T) {
	child := sleeper(t)
	self := os.Getpid()
	if !DescendsFrom(child.Process.Pid, self) {
		t.Fatal("a child must descend from the test process")
	}
	if !DescendsFrom(self, self) {
		t.Fatal("a process descends from itself")
	}
	if DescendsFrom(self, child.Process.Pid) {
		t.Fatal("a parent does not descend from its child")
	}
	if DescendsFrom(1, self) {
		t.Fatal("pid 1 descends from nothing")
	}
}

func TestStartTicksNamesOneIncarnation(t *testing.T) {
	child := sleeper(t)
	ticks, err := StartTicks(child.Process.Pid)
	if err != nil || ticks == 0 {
		t.Fatalf("start ticks: %d %v", ticks, err)
	}
	if _, err := StartTicks(1 << 22); err == nil {
		t.Fatal("a pid that does not exist must error")
	}
}

func TestPinnedPeerReportsExit(t *testing.T) {
	child := sleeper(t)
	peer, err := PinPID(child.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if got := peer.PID(); got != child.Process.Pid {
		t.Fatalf("pinned pid %d, want %d", got, child.Process.Pid)
	}
	exited := make(chan bool, 1)
	go func() { exited <- peer.WaitExit(nil) }()
	_ = child.Process.Kill()
	_ = child.Wait()
	select {
	case ok := <-exited:
		if !ok {
			t.Fatal("WaitExit returned false for an exited process")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("WaitExit did not observe the exit within 3 s")
	}
	if got := peer.PID(); got != 0 {
		t.Fatalf("a dead pinned process must read as pid 0, got %d", got)
	}
}

func TestWaitExitStops(t *testing.T) {
	child := sleeper(t)
	peer, err := PinPID(child.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	stop := make(chan struct{})
	done := make(chan bool, 1)
	go func() { done <- peer.WaitExit(stop) }()
	close(stop)
	select {
	case ok := <-done:
		if ok {
			t.Fatal("WaitExit must return false when stopped")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("WaitExit ignored stop")
	}
}

func TestPeerOfUnixConnIsTheDialer(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "h.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	client, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := ln.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	peer, err := PeerOf(server)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if got := peer.PID(); got != os.Getpid() {
		t.Fatalf("peer pid %d, want this process %d", got, os.Getpid())
	}
}

func TestCallRoundTrip(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "h.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.AcceptUnix()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = bufio.NewReader(conn).ReadBytes('\n')
		_, _ = conn.Write([]byte(`{"ok":true,"proof":"p","enrollment_id":"e"}` + "\n"))
	}()
	resp, err := Call(sock, Request{Op: "sign", Method: "GET", URL: "https://x/v1/enrollments/self"}, 2*time.Second)
	if err != nil || !resp.OK || resp.Proof != "p" || resp.EnrollmentID != "e" {
		t.Fatalf("round trip: %+v %v", resp, err)
	}
	if _, err := Call(filepath.Join(t.TempDir(), "absent.sock"), Request{Op: "sessions"}, time.Second); err == nil {
		t.Fatal("an absent socket must error")
	}
}
