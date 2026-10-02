package launcher

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

const launcherChildEnv = "LEGION_LAUNCHER_TEST_CHILD"

func TestLauncherChild(t *testing.T) {
	if os.Getenv(launcherChildEnv) != "1" {
		return
	}
	marker := os.Getenv("LEGION_LAUNCHER_MARKER")
	if marker == "" {
		os.Exit(2)
	}
	child := exec.Command("/bin/sh", "-c", "exec sleep 600")
	if err := child.Start(); err != nil {
		os.Exit(3)
	}
	if err := os.WriteFile(marker, []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
		os.Exit(4)
	}
	select {}
}

func TestRunStartsOneChildAndStopsItsProcessGroup(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	marker := filepath.Join(t.TempDir(), "descendant-pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- Run(ctx, Config{
			Connect:       "tcp://" + listener.Addr().String(),
			Token:         "launcher-token",
			Sandbox:       "legion-legion-legion-208",
			Role:          "tester",
			PodUID:        "pod-1",
			BootTokenFile: filepath.Join(t.TempDir(), "boot-token"),
		})
	}()
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)
	writer := shimwire.NewWriter(conn)
	next := func() shimwire.Frame {
		t.Helper()
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		frame, err := shimwire.Decode(line)
		if err != nil {
			t.Fatal(err)
		}
		return frame
	}
	if hello, ok := next().(shimwire.LauncherHello); !ok || hello.Token != "launcher-token" || hello.Role != "tester" || hello.PodUID != "pod-1" || hello.LauncherID == "" {
		t.Fatalf("hello = %#v", hello)
	}
	if err := writer.WriteFrame(shimwire.LauncherHelloAck{}); err != nil {
		t.Fatal(err)
	}
	if state, ok := next().(shimwire.LauncherState); !ok || state.Child != nil || state.LastExit != nil {
		t.Fatalf("initial state = %#v", state)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	start := shimwire.LauncherStart{
		ID: "start-1", Generation: 7, BootToken: "boot-7",
		Argv: []string{self, "-test.run=^TestLauncherChild$"},
		Env:  []string{launcherChildEnv + "=1", "LEGION_LAUNCHER_MARKER=" + marker},
	}
	if err := writer.WriteFrame(start); err != nil {
		t.Fatal(err)
	}
	if got, ok := next().(shimwire.LauncherStartResult); !ok || !got.OK || got.ID != start.ID || got.RunningGeneration != start.Generation {
		t.Fatalf("start result = %#v", got)
	}
	var descendant int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(marker)
		if err == nil {
			descendant, err = strconv.Atoi(strings.TrimSpace(string(body)))
			if err == nil {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if descendant == 0 {
		t.Fatal("child did not publish its descendant PID")
	}
	if err := writer.WriteFrame(shimwire.LauncherStop{ID: "stop-1", Generation: 7, GraceMs: 1000}); err != nil {
		t.Fatal(err)
	}
	stopResult := next()
	if _, ok := stopResult.(shimwire.LauncherState); ok {
		stopResult = next()
	}
	if got, ok := stopResult.(shimwire.LauncherStopResult); !ok || !got.OK || got.ID != "stop-1" {
		t.Fatalf("stop result = %#v", got)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(descendant, 0); errors.Is(err, syscall.ESRCH) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := syscall.Kill(descendant, 0); err == nil {
		t.Fatalf("launcher left descendant %d running", descendant)
	}
	cancel()
	select {
	case err := <-result:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("launcher did not leave after its context ended")
	}
}
