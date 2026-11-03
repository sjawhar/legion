package shim

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

// An adoption command that exits on its own did not time out, however long adoptionWaitDelay then
// holds the run open for a child still holding its output pipes: its exit is its answer. In the
// first case the budget is 100 ms short of adoptionWaitDelay, so the run always returns past it,
// while the command still has 900 ms to exit under load; a run that read a timeout from the
// deadline having passed would report jj's own exit 3 as a timeout. In the second the command
// exits 0 well inside its budget; a run that took exec.ErrWaitDelay for a failure would report
// that success as an error.
func TestRunAdoptionCommandReportsTheCommandsOwnExitWhenAChildHoldsItsPipesPastIt(t *testing.T) {
	for name, testCase := range map[string]struct {
		exit    int
		budget  time.Duration
		wantErr string
	}{
		"an exit 3, the pipes held past the budget": {3, adoptionWaitDelay - 100*time.Millisecond, "Command failed (exit 3)"},
		"an exit 0 inside the budget":               {0, time.Minute, ""},
	} {
		t.Run(name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "lingering.pid")
			t.Cleanup(func() { killPIDFile(pidFile) })
			ctx, cancel := context.WithTimeout(context.Background(), testCase.budget)
			defer cancel()
			script := `echo answered; echo refused >&2; sleep 30 & echo $! >"$1"; exit ` + strconv.Itoa(testCase.exit)

			started := time.Now()
			out, err := runAdoptionCommand(ctx, testCase.budget, started, []string{"PATH=" + os.Getenv("PATH")}, []string{"sh", "-c", script, "sh", pidFile})
			elapsed := time.Since(started)

			if elapsed < adoptionWaitDelay {
				t.Fatalf("the run returned after %s, inside adoptionWaitDelay: nothing held the pipe, so this run proves nothing", elapsed)
			}
			if testCase.wantErr == "" {
				if err != nil || out != "answered" {
					t.Errorf("runAdoptionCommand = %q, %v after %s; want \"answered\" and no error: the command exited 0 on its own", out, err, elapsed)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), testCase.wantErr) || strings.Contains(err.Error(), "timed out") {
				t.Errorf("runAdoptionCommand = %q, %v after %s; want an error naming %q and no timeout: the command exited on its own inside its %s budget",
					out, err, elapsed, testCase.wantErr, testCase.budget)
			}
		})
	}
}

// The adoption's two commands share one budget, so a command can find it already spent before it
// starts: that is the budget running out, reported as a timeout, never a failure to run.
func TestRunAdoptionCommandReportsABudgetSpentBeforeItStartedAsATimeout(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	_, err := runAdoptionCommand(ctx, time.Second, time.Now(), []string{"PATH=" + os.Getenv("PATH")}, []string{"sh", "-c", "exit 0"})

	if err == nil || !strings.HasPrefix(err.Error(), "Command timed out after 1 s") {
		t.Errorf("runAdoptionCommand under a spent budget = %v, want \"Command timed out after 1 s …\"", err)
	}
}

// killPIDFile kills the process named by the PID pidFile holds, left running past a test's own
// assertions by design (a backgrounded sleep standing in for a child holding a command's pipes).
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
