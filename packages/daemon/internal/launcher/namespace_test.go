package launcher

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// This helper runs the real launcher as PID 1, without a Docker daemon or a model process.
func TestLauncherPIDOneHelper(t *testing.T) {
	if os.Getenv("LEGION_LAUNCHER_NAMESPACE_HELPER") != "1" {
		return
	}
	if os.Getpid() != 1 {
		t.Fatal("launcher namespace helper is not PID 1")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	go func() { _, _ = io.Copy(io.Discard, os.Stdin); cancel() }()
	grace, err := time.ParseDuration(os.Getenv("LEGION_LAUNCHER_TEST_GRACE"))
	if err != nil {
		t.Fatal(err)
	}
	err = Run(ctx, Config{
		Connect: os.Getenv("LEGION_LAUNCHER_TEST_ADDRESS"), PrivateDir: os.Getenv("LEGION_LAUNCHER_TEST_PRIVATE"),
		Token: "launcher-token", Sandbox: "legion-legion-legion-208", Role: "tester", PodUID: "pod-1", Stderr: os.Stderr,
		StopGrace: grace,
	})
	if err != nil && ctx.Err() == nil {
		t.Fatal(err)
	}
}

func namespaceRig(t *testing.T, grace time.Duration) *rig {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("PID namespace launcher proof requires Linux")
	}
	prefix := []string{"-n", "unshare", "--pid", "--fork", "--mount-proc", "--kill-child", "--setuid", strconv.Itoa(os.Getuid()), "--setgid", strconv.Itoa(os.Getgid())}
	probe := exec.Command("sudo", append(append([]string{}, prefix...), "/bin/true")...)
	if output, err := probe.CombinedOutput(); err != nil {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatalf("PID namespace prerequisite: %v: %s", err, output)
		}
		t.Skipf("PID namespace prerequisite: %v: %s", err, output)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if err := listener.(*net.TCPListener).SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatal(err)
	}
	g := &rig{t: t, listener: listener, private: t.TempDir(), result: make(chan error, 1)}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	args := append(append([]string{}, prefix...), "/usr/bin/env", "-i", "PATH=/usr/bin:/bin", "GOMAXPROCS=1",
		"LEGION_LAUNCHER_NAMESPACE_HELPER=1", "LEGION_LAUNCHER_TEST_ADDRESS=tcp://"+listener.Addr().String(),
		"LEGION_LAUNCHER_TEST_PRIVATE="+g.private, "LEGION_LAUNCHER_TEST_GRACE="+grace.String(), self, "-test.run=^TestLauncherPIDOneHelper$")
	command := exec.Command("sudo", args...)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	g.cancel = func() { _ = stdin.Close() }
	go func() { g.result <- command.Wait() }()
	t.Cleanup(func() {
		g.cancel()
		select {
		case err := <-g.result:
			if err != nil {
				t.Errorf("PID 1 launcher exited: %v\n%s", err, output.String())
			}
		case <-time.After(10 * time.Second):
			_ = command.Process.Kill()
			<-g.result
			t.Error("PID 1 launcher did not stop after its context ended")
		}
	})
	g.accept()
	return g
}

func TestPIDOneLauncherCleansDetachedDescendants(t *testing.T) {
	for _, end := range []string{"stop", "natural exit"} {
		t.Run(end, func(t *testing.T) {
			g := namespaceRig(t, 2*time.Second)
			marker := filepath.Join(t.TempDir(), "detached")
			script := `/usr/bin/setsid /bin/sh -c 'printf "%s ready\n" "$$" > "$DETACHED_MARKER"; exec /bin/sleep 600' &
while [ ! -s "$DETACHED_MARKER" ]; do /bin/sleep 0.01; done
`
			if end == "stop" {
				script += "wait\n"
			}
			if result := g.start(shimwire.LauncherStart{ID: "start-1", Generation: 1, Argv: []string{"/bin/sh", "-c", script}, Env: []string{"DETACHED_MARKER=" + marker}}); !result.OK {
				t.Fatalf("start: %+v", result)
			}
			pid, _ := published(t, marker)
			if end == "stop" {
				g.stop(1)
			} else {
				for g.latest.LastExit == nil || g.latest.LastExit.Generation != 1 {
					g.next()
				}
			}
			resultFile := filepath.Join(t.TempDir(), "result")
			inspect := `if kill -0 "$DETACHED_PID" 2>/dev/null; then printf '1 survived\n'; else printf '1 gone\n'; fi > "$RESULT"`
			if result := g.start(shimwire.LauncherStart{ID: "start-2", Generation: 2, Argv: []string{"/bin/sh", "-c", inspect}, Env: []string{"DETACHED_PID=" + strconv.Itoa(pid), "RESULT=" + resultFile}}); !result.OK {
				t.Fatalf("next generation: %+v", result)
			}
			if _, result := published(t, resultFile); strings.TrimSpace(result) != "gone" {
				t.Fatalf("detached process %d %s into the next generation after %s", pid, result, end)
			}
		})
	}
}

func TestPIDOneLauncherReapsOrphansWhileRoleStaysResident(t *testing.T) {
	g := namespaceRig(t, 2*time.Second)
	marker := filepath.Join(t.TempDir(), "orphan")
	resultFile := filepath.Join(t.TempDir(), "reaped")
	script := `/bin/sh -c '/usr/bin/setsid /bin/sh -c '\''/bin/sleep 0.1; printf "%s ready\n" "$$" > "$ORPHAN"; exit 0'\'' &' 
while [ ! -s "$ORPHAN" ]; do /bin/sleep 0.01; done
read pid unused < "$ORPHAN"
for n in $(/usr/bin/seq 1 500); do
  if ! kill -0 "$pid" 2>/dev/null; then printf '1 gone\n' > "$RESULT"; exec /bin/sleep 600; fi
  /bin/sleep 0.01
done
printf '1 zombie\n' > "$RESULT"
exec /bin/sleep 600
`
	if result := g.start(shimwire.LauncherStart{ID: "resident", Generation: 1, Argv: []string{"/bin/sh", "-c", script},
		Env: []string{"ORPHAN=" + marker, "RESULT=" + resultFile}}); !result.OK {
		t.Fatalf("start: %+v", result)
	}
	if _, result := published(t, resultFile); strings.TrimSpace(result) != "gone" {
		t.Fatalf("resident role left an adopted orphan: %s", result)
	}
	g.stop(1)
}

func TestPIDOneLauncherUsesConfiguredShutdownGrace(t *testing.T) {
	for _, tc := range []struct {
		name     string
		grace    time.Duration
		graceful bool
	}{
		{"graceful", 2500 * time.Millisecond, true},
		{"deadline", 50 * time.Millisecond, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := namespaceRig(t, tc.grace)
			ready := filepath.Join(t.TempDir(), "ready")
			finished := filepath.Join(t.TempDir(), "finished")
			script := `trap '/bin/sleep 1.2; printf done > "$FINISHED"; exit 0' TERM
printf '1 ready\n' > "$READY"
while :; do /bin/sleep 600 & wait; done
`
			if result := g.start(shimwire.LauncherStart{ID: "start", Generation: 1, Argv: []string{"/bin/sh", "-c", script},
				Env: []string{"READY=" + ready, "FINISHED=" + finished}}); !result.OK {
				t.Fatalf("start: %+v", result)
			}
			published(t, ready)
			g.cancel()
			select {
			case err := <-g.result:
				g.result <- err
				if err != nil {
					t.Fatalf("launcher exit: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("launcher exceeded its shutdown budget")
			}
			body, err := os.ReadFile(finished)
			if tc.graceful {
				if err != nil || string(body) != "done" {
					t.Fatalf("configured grace was cut short: %q, %v", body, err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatalf("deadline did not stop the slow shutdown: %q, %v", body, err)
			}
		})
	}
}
