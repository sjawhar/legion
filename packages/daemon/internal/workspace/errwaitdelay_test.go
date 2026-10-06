package workspace

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

// procgroup.Err is execRunner's one classification of a command's error: nil for an ordinary exit
// or for exec.ErrWaitDelay (a clean exit whose I/O draining outlived WaitDelay), either way read
// from cmd.ProcessState.ExitCode(). A fake git that exits 0 while a backgrounded child keeps its
// stdout pipe open proves that wiring end to end: RunChecked must succeed with ExitCode 0, not
// report the command's own clean exit as a failure.
func TestRunCheckedSucceedsWhenACommandExitsZeroWithAChildStillHoldingItsOutputOpen(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "sleep.pid")
	fakeGit := filepath.Join(dir, "git")
	script := "#!/bin/sh\necho done\nsleep 3 & echo $! >" + pidFile + "\nexit 0\n"
	if err := os.WriteFile(fakeGit, []byte(script), 0o700); err != nil {
		t.Fatalf("write the fake git: %v", err)
	}
	t.Cleanup(func() { killPIDFile(t, pidFile) })

	run := NewRunner(5*time.Second, map[string]string{"git": fakeGit})

	result, err := RunChecked(context.Background(), run, []string{"git"}, nil, "")
	if err != nil {
		t.Fatalf("RunChecked = %v, want no error: the command exited 0 on its own", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
}

// killPIDFile kills the process named by the PID pidFile holds, left running past a test's own
// assertions by design (a backgrounded sleep standing in for a command's own lingering child).
func killPIDFile(t *testing.T, pidFile string) {
	t.Helper()
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
