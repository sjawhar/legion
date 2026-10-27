package tmux

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A tmux client that exits on its own did not time out, however long pipeDrainDelay then holds
// runTmux open for something still holding its output pipes (a server that took the client's
// stdio over the socket): its exit is its answer. In the first case the timeout is 100 ms short of
// pipeDrainDelay, so the run always returns past it, while the client still has 900 ms to exit
// under load; a runTmux that read a timeout from the deadline having passed would report the
// client's own exit 3 as a timeout. In the second the client exits 0 well inside its timeout; a
// runTmux that took exec.ErrWaitDelay for a failure would report that success as an error.
func TestRunTmuxReportsTheClientsOwnExitWhenSomethingHoldsItsPipesPastIt(t *testing.T) {
	for name, testCase := range map[string]struct {
		exit    int
		timeout time.Duration
	}{
		"an exit 3, the pipes held past the timeout": {3, pipeDrainDelay - 100*time.Millisecond},
		"an exit 0 inside the timeout":               {0, time.Minute},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "lingering.pid")
			t.Cleanup(func() { killPIDFile(pidFile) })
			client := fakeTmuxClient(t, dir, "echo answered; sleep 30 & echo $! >"+pidFile+"; exit "+strconv.Itoa(testCase.exit))
			r := &Runtime{tmuxPath: client, commandTimeout: testCase.timeout, environ: []string{"PATH=" + os.Getenv("PATH")}}

			started := time.Now()
			res, err := r.runTmux(context.Background(), []string{"tmux", "-L", "legion-test", "list-panes"})
			elapsed := time.Since(started)

			if err != nil {
				t.Fatalf("runTmux = %v after %s, want the client's own exit %d", err, elapsed, testCase.exit)
			}
			if elapsed < pipeDrainDelay {
				t.Fatalf("runTmux returned after %s, inside pipeDrainDelay: nothing held the pipe, so this run proves nothing", elapsed)
			}
			if res.timedOut != 0 || res.exitCode != testCase.exit || strings.TrimSpace(res.stdout) != "answered" {
				t.Errorf("runTmux = exit %d, timed out %s, stdout %q after %s; want exit %d, no timeout, and the answer: the client exited on its own inside its %s timeout",
					res.exitCode, res.timedOut, res.stdout, elapsed, testCase.exit, testCase.timeout)
			}
		})
	}
}

// A client its timeout kills timed out, with that timeout, and is a result, not an error.
func TestRunTmuxReportsAClientItsTimeoutKilledAsTimedOut(t *testing.T) {
	client := fakeTmuxClient(t, t.TempDir(), "exec sleep 30")
	r := &Runtime{tmuxPath: client, commandTimeout: time.Second, environ: []string{"PATH=" + os.Getenv("PATH")}}

	res, err := r.runTmux(context.Background(), []string{"tmux", "-L", "legion-test", "list-panes"})

	if err != nil || res.timedOut != time.Second || res.exitCode != -1 {
		t.Errorf("runTmux = exit %d, timed out %s, error %v; want exit -1, timed out 1s, no error", res.exitCode, res.timedOut, err)
	}
}

// fakeTmuxClient writes an executable that runs body under sh in place of a tmux client.
func fakeTmuxClient(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "tmux")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatalf("write the fake tmux client: %v", err)
	}
	return path
}

// killPIDFile kills the process named by the PID pidFile holds, left running past a test's own
// assertions by design (a backgrounded sleep standing in for what holds a client's pipes).
func killPIDFile(pidFile string) {
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
