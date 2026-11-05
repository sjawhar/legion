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

	"github.com/sjawhar/legion/daemon/internal/procgroup"
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

	result, err := RunChecked(context.Background(), run, []string{"git"}, nil, dir)
	if err != nil {
		t.Fatalf("RunChecked = %v, want no error: the command exited 0 on its own", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
}

// A command that exits on its own inside its Timeout did not time out, however long
// procgroup.WaitDelay then holds Run open for a child still holding its output pipe. The Timeout
// here is 100 ms short of WaitDelay, so every such run returns past its deadline, while git still
// has 900 ms to exit under load: a Run that read TimedOut from the deadline having passed would
// report this git's own exit 128 as a timeout, and a clone that failed transiently inside the
// last WaitDelay of its budget, git-remote-https still holding the pipe, would read as timed out,
// which transientCloneFailure never retries.
func TestRunReportsTheCommandsOwnFailureNotATimeoutWhenAChildHoldsItsOutputPastTheDeadline(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "sleep.pid")
	fakeGit := filepath.Join(dir, "git")
	script := "#!/bin/sh\necho 'fatal: unable to access' >&2\nsleep 30 & echo $! >" + pidFile + "\nexit 128\n"
	if err := os.WriteFile(fakeGit, []byte(script), 0o700); err != nil {
		t.Fatalf("write the fake git: %v", err)
	}
	t.Cleanup(func() { killPIDFile(t, pidFile) })
	timeout := procgroup.WaitDelay - 100*time.Millisecond
	run := NewRunner(timeout, map[string]string{"git": fakeGit})

	started := time.Now()
	result, err := run.Run(context.Background(), Command{Dir: dir, Argv: []string{"git"}, Timeout: timeout})
	elapsed := time.Since(started)

	if err != nil {
		t.Fatalf("Run = %v, want the command's own exit", err)
	}
	if elapsed < timeout {
		t.Fatalf("Run returned after %s, inside its %s Timeout: nothing held the pipe past the deadline, so this run proves nothing", elapsed, timeout)
	}
	if result.TimedOut || result.ExitCode != 128 {
		t.Errorf("Run = exit %d, TimedOut %t after %s; want exit 128, not timed out: git failed on its own inside its %s Timeout", result.ExitCode, result.TimedOut, elapsed, timeout)
	}
}

// A command its caller's context ended was not cut off by its own Timeout: TimedOut is the
// Timeout's kill alone. A Run that read a timeout from the kill alone would call a clone its
// caller stopped a timeout, which transientCloneFailure never retries.
func TestRunDoesNotReportACommandItsCallerStoppedAsTimedOut(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	fakeGit := filepath.Join(dir, "git")
	script := "#!/bin/sh\n: >" + started + "\nexec sleep 30\n"
	if err := os.WriteFile(fakeGit, []byte(script), 0o700); err != nil {
		t.Fatalf("write the fake git: %v", err)
	}
	run := NewRunner(time.Minute, map[string]string{"git": fakeGit})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go cancelOnceExists(ctx, cancel, started)

	result, err := run.Run(ctx, Command{Dir: dir, Argv: []string{"git"}, Timeout: time.Minute})

	if err != nil {
		t.Fatalf("Run = %v, want the killed command's own exit", err)
	}
	if result.TimedOut {
		t.Errorf("Run = exit %d, TimedOut true; want not timed out: the caller's context ended the command inside its minute", result.ExitCode)
	}
}

// A RunCommand command that exits on its own did not time out, however long commandWaitDelay then
// holds the run open for a child still holding its output pipes: its exit is its answer. In the
// first case the budget is 100 ms short of commandWaitDelay, so the run always returns past it,
// while the command still has 900 ms to exit under load; a run that read a timeout from the
// deadline having passed would report jj's own exit 3 as a timeout. In the second the command
// exits 0 well inside its budget; a run that took exec.ErrWaitDelay for a failure would report
// that success as an error. (Ported with RunCommand's own semantics from the shim's
// runAdoptionCommand, which the adoption now reaches through RunCommand.)
func TestRunCommandReportsTheCommandsOwnExitWhenAChildHoldsItsPipesPastIt(t *testing.T) {
	for name, testCase := range map[string]struct {
		exit    int
		budget  time.Duration
		wantErr string
	}{
		"an exit 3, the pipes held past the budget": {3, commandWaitDelay - 100*time.Millisecond, "Command failed (exit 3)"},
		"an exit 0 inside the budget":               {0, time.Minute, ""},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "lingering.pid")
			t.Cleanup(func() { killPIDFile(t, pidFile) })
			ctx, cancel := context.WithTimeout(context.Background(), testCase.budget)
			defer cancel()
			script := `echo answered; echo refused >&2; sleep 30 & echo $! >"$1"; exit ` + strconv.Itoa(testCase.exit)

			started := time.Now()
			out, err := RunCommand(ctx, testCase.budget, started, []string{"PATH=" + os.Getenv("PATH")}, dir, nil, []string{"sh", "-c", script, "sh", pidFile})
			elapsed := time.Since(started)

			if elapsed < commandWaitDelay {
				t.Fatalf("the run returned after %s, inside commandWaitDelay: nothing held the pipe, so this run proves nothing", elapsed)
			}
			if testCase.wantErr == "" {
				if err != nil || out != "answered" {
					t.Errorf("RunCommand = %q, %v after %s; want \"answered\" and no error: the command exited 0 on its own", out, err, elapsed)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), testCase.wantErr) || strings.Contains(err.Error(), "timed out") {
				t.Errorf("RunCommand = %q, %v after %s; want an error naming %q and no timeout: the command exited on its own inside its %s budget",
					out, err, elapsed, testCase.wantErr, testCase.budget)
			}
		})
	}
}

// The commands of one save, one restore, or one adoption share a single budget, so a command can
// find it already spent before it starts: that is the budget running out, reported as a timeout,
// never a failure to run.
func TestRunCommandReportsABudgetSpentBeforeItStartedAsATimeout(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	_, err := RunCommand(ctx, time.Second, time.Now(), []string{"PATH=" + os.Getenv("PATH")}, t.TempDir(), nil, []string{"sh", "-c", "exit 0"})

	if err == nil || !strings.HasPrefix(err.Error(), "Command timed out after 1 s") {
		t.Errorf("RunCommand under a spent budget = %v, want \"Command timed out after 1 s …\"", err)
	}
}

// A command RunCommand's caller stopped was not cut off by the budget: the restore runs under the
// launcher's own context (internal/launcher), so a launcher stopped mid-restore must read the
// command's own exit, never a timeout naming a budget that had not run out. The cancel lands once
// the command runs, never on one not yet started.
func TestRunCommandDoesNotReportACommandItsCallerStoppedAsATimeout(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "started")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go cancelOnceExists(ctx, cancel, marker)

	_, err := RunCommand(ctx, time.Minute, time.Now(), []string{"PATH=" + os.Getenv("PATH")}, dir, nil, []string{"sh", "-c", `: >"$1"; exec sleep 30`, "sh", marker})

	if err == nil || strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "Command failed (exit -1)") {
		t.Errorf("RunCommand = %v, want \"Command failed (exit -1)\": the caller's context ended the command inside its minute", err)
	}
}

// cancelOnceExists calls cancel once path exists, which the command under test creates once it
// runs, so the cancel lands on a running command and never on one not yet started.
func cancelOnceExists(ctx context.Context, cancel context.CancelFunc, path string) {
	for ctx.Err() == nil {
		if _, err := os.Stat(path); err == nil {
			cancel()
			return
		}
		time.Sleep(10 * time.Millisecond)
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
