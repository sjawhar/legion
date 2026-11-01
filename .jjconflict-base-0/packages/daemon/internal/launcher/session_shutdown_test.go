package launcher

import (
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

const launcherTermDialerEnv = "LEGION_LAUNCHER_TEST_TERM_DIALER"

// TestLauncherTermDialer is a child that does what an agent does when its container is told to stop:
// it keeps working through its grace, and the work it finishes then is written to its session. Once
// it is ready it waits for SIGTERM, then opens a fresh connection to its generation's session socket
// and records whether anything answered, as an agent's last write after its pool went idle would.
func TestLauncherTermDialer(t *testing.T) {
	if os.Getenv(launcherTermDialerEnv) != "1" {
		return
	}
	terms := make(chan os.Signal, 1)
	signal.Notify(terms, syscall.SIGTERM)
	if err := os.WriteFile(os.Getenv("LEGION_LAUNCHER_READY"), []byte("ready"), 0o600); err != nil {
		os.Exit(2)
	}
	<-terms
	seen := "served"
	conn, err := net.Dial("unix", os.Getenv("LEGION_LAUNCHER_SOCKET"))
	if err != nil {
		seen = "refused: " + err.Error()
	} else {
		_ = conn.Close()
	}
	// Written aside and renamed, so the test never reads it half-written.
	resultFile := os.Getenv("LEGION_LAUNCHER_RESULT")
	if err := os.WriteFile(resultFile+".tmp", []byte(seen), 0o600); err != nil {
		os.Exit(3)
	}
	if err := os.Rename(resultFile+".tmp", resultFile); err != nil {
		os.Exit(4)
	}
	os.Exit(0)
}

// A generation's session database is the generation's until its child exits, however the stop
// arrives: the daemon's stop command, or the container's own SIGTERM (the launcher's context
// ending), which is what a pod deletion, an eviction or a node drain sends. The agent spends its
// grace finishing and recording its work, and every write in that grace that needs a connection must
// still find the proxy serving; the old postgres store, which dials the database directly, never
// lost its last writes this way (INFRA-271).
func TestASessionDatabaseServesItsChildThroughTheChildsShutdownGrace(t *testing.T) {
	for _, how := range []string{"daemon stop", "container shutdown"} {
		t.Run(how, func(t *testing.T) {
			dialer := &sessionDialer{addr: idleUpstream(t)}
			g := newSessionRig(t, Config{SessionDatabase: testDatabase, SessionDialer: dialer.build, StopGrace: 5 * time.Second})
			self, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			scratch := t.TempDir()
			ready, result := filepath.Join(scratch, "ready"), filepath.Join(scratch, "result")
			start := shimwire.LauncherStart{
				ID: "start-1", Generation: 1, Argv: []string{self, "-test.run=^TestLauncherTermDialer$"},
				Env: []string{launcherTermDialerEnv + "=1", "LEGION_LAUNCHER_READY=" + ready, "LEGION_LAUNCHER_RESULT=" + result,
					"LEGION_LAUNCHER_SOCKET=" + filepath.Join(g.generationDir(1), ompsessions.SocketName)},
			}
			if got := g.start(start); !got.OK {
				t.Fatalf("start result = %#v", got)
			}
			waitForFile(t, ready)
			if how == "daemon stop" {
				g.stop(1)
			} else {
				g.cancel()
			}
			waitForFile(t, result)
			body, err := os.ReadFile(result)
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != "served" {
				t.Fatalf("after the %s, the child's shutdown-time connection to its session socket was %s; want it served until the child exits", how, body)
			}
		})
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}
