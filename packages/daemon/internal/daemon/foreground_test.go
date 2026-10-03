package daemon

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ignoredBy is the SigIgn mask of a process that reads its own /proc/self/status, run as wrap has
// it run: inside a probe attempt's script, or bare, as a later child such as the controller's Oh My
// Pi runs.
func ignoredBy(t *testing.T, wrap func(string) string) uint64 {
	t.Helper()
	out, err := exec.Command("sh", "-c", wrap("exec sed -n 's/^SigIgn:[[:space:]]*//p' /proc/self/status")).Output()
	if err != nil {
		t.Fatalf("read a child's SigIgn: %v", err)
	}
	mask, err := strconv.ParseUint(strings.TrimSpace(string(out)), 16, 64)
	if err != nil {
		t.Fatalf("parse SigIgn %q: %v", out, err)
	}
	return mask
}

func bit(sig syscall.Signal) uint64 { return 1 << (uint(sig) - 1) }

func bare(command string) string { return command }

func requireProcStatus(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/proc/self/status"); err != nil {
		t.Skip("no /proc/self/status to read a child's signal dispositions from")
	}
}

// The attempt's SIGTTOU immunity ends with it: a child started after release - the controller's
// Oh My Pi - is stopped by SIGTTOU again rather than inheriting SIG_IGN, which signal.Reset alone
// leaves in place.
func TestATerminalAttemptLeavesLaterChildrenSIGTTOUsDefault(t *testing.T) {
	requireProcStatus(t)
	before := ignoredBy(t, bare)
	if before&bit(syscall.SIGTTOU) != 0 {
		t.Skip("this test process was started with SIGTTOU ignored")
	}
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	job := &terminalJob{fd: int(devNull.Fd()), owner: syscall.Getpgrp()}
	attempt := exec.Command("true")
	attempt.SysProcAttr = &syscall.SysProcAttr{}
	job.attach(attempt)
	if during := ignoredBy(t, bare); during&bit(syscall.SIGTTOU) == 0 {
		t.Fatalf("while the attempt holds the terminal, SigIgn = %#x: SIGTTOU is not ignored", during)
	}
	job.release()
	if after := ignoredBy(t, bare); after != before {
		t.Fatalf("after release a child's SigIgn = %#x, want %#x as before the attempt", after, before)
	}
}

// At the terminal the attempt ignores SIGTSTP, so a Ctrl-Z cannot stop the terminal's foreground
// job while this process waits in the background; an attempt run as the daemon's are does not.
func TestATerminalAttemptIgnoresSIGTSTP(t *testing.T) {
	requireProcStatus(t)
	var background *terminalJob
	if mask := ignoredBy(t, background.script); mask&bit(syscall.SIGTSTP) != 0 {
		t.Skip("this test process was started with SIGTSTP ignored")
	}
	job := &terminalJob{}
	if mask := ignoredBy(t, job.script); mask&bit(syscall.SIGTSTP) == 0 {
		t.Fatalf("a terminal attempt's SigIgn = %#x: SIGTSTP is not ignored", mask)
	}
	if mask := ignoredBy(t, bare); mask&bit(syscall.SIGTSTP) != 0 {
		t.Fatalf("this process's own SIGTSTP changed: a child's SigIgn = %#x", mask)
	}
}

// A terminal attempt the operator's Ctrl-C ended is an interrupt whether it died of SIGINT or, as
// Oh My Pi does, caught it and exited 130; any other end is judged as an answer, and an attempt
// away from a terminal is never an interrupt.
func TestATerminalAttemptEndedByCtrlCIsInterrupted(t *testing.T) {
	ended := func(script string) *os.ProcessState {
		t.Helper()
		cmd := exec.Command("sh", "-c", script)
		_ = cmd.Run()
		return cmd.ProcessState
	}
	job := &terminalJob{}
	for _, tc := range []struct {
		name   string
		script string
		want   bool
	}{
		{"died of SIGINT", "kill -INT $$", true},
		{"caught it and exited 130", "exit 130", true},
		{"exited 1", "exit 1", false},
		{"died of SIGTERM", "kill -TERM $$", false},
		{"exited 0", "exit 0", false},
	} {
		if got := job.interrupted(ended(tc.script)); got != tc.want {
			t.Errorf("%s: interrupted = %v, want %v", tc.name, got, tc.want)
		}
	}
	var background *terminalJob
	if background.interrupted(ended("exit 130")) {
		t.Error("an attempt away from a terminal that exited 130 counts as interrupted")
	}
}

// At the terminal a Ctrl-C ends the attempt as interrupted even when the launch answers it and
// exits 0, as Oh My Pi does when the SIGINT lands late in `omp models`: the attempt's shell records
// the signal rather than reading it from the launch's status. Without a Ctrl-C the launch's own
// status comes through.
func TestATerminalAttemptRecordsACtrlCTheLaunchExitsZeroOn(t *testing.T) {
	job := &terminalJob{}
	// The launch ignores SIGINT and finishes normally, exit 0: the case no exit status carries.
	launch := `exec sh -c 'trap "" INT; sleep 1; exit 0'`
	run := func(ctrlC bool) *os.ProcessState {
		t.Helper()
		cmd := exec.Command("sh", "-c", job.script(launch))
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if ctrlC {
			time.Sleep(300 * time.Millisecond)
			// The terminal's Ctrl-C reaches the whole foreground process group.
			if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGINT); err != nil {
				t.Fatal(err)
			}
		}
		_ = cmd.Wait()
		return cmd.ProcessState
	}
	if state := run(true); state.ExitCode() != 130 || !job.interrupted(state) {
		t.Fatalf("a Ctrl-C the launch answered with exit 0: exit %d, interrupted %v; want 130 and interrupted", state.ExitCode(), job.interrupted(state))
	}
	if state := run(false); state.ExitCode() != 0 || job.interrupted(state) {
		t.Fatalf("no Ctrl-C: exit %d, interrupted %v; want the launch's 0 and not interrupted", state.ExitCode(), job.interrupted(state))
	}
}

type chunks []string

func (c *chunks) Write(p []byte) (int, error) {
	*c = append(*c, string(p))
	return len(p), nil
}

// The terminal sees the attempt's stderr as it arrives, without the load probe's report.
func TestTheTerminalCopyDropsTheProbesReportAndShowsAPromptAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name   string
		writes []string
		want   []string
	}{
		{"a prompt with no newline", []string{"Enter PIN: "}, []string{"Enter PIN: "}},
		{"the report dropped whole, the rest kept", []string{"warn\nLEGION_PLUGIN_LOADED=yes\nLEGION_PLUGIN_LOADED_FROM=file:///x\ndone\n"}, []string{"warn\ndone\n"}},
		{"a marker split across writes", []string{"LEGION_PLU", "GIN_LOADED=no\n", "ok\n"}, []string{"ok\n"}},
		{"a line that only starts like one", []string{"LEGION_P", "ROFILE=x\n"}, []string{"LEGION_PROFILE=x\n"}},
		{"the report right after a prompt", []string{"Enter PIN: ", "LEGION_PLUGIN_LOADED=yes\nLEGION_PLUGIN_LOADED_FROM=file:///x\n"}, []string{"Enter PIN: "}},
		{"a false start inside a marker", []string{"LEGION_PLLEGION_PLUGIN_LOADED=no\nok\n"}, []string{"LEGION_PLok\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got chunks
			filter := &markerFilter{w: &got}
			for _, write := range tc.writes {
				if n, err := filter.Write([]byte(write)); n != len(write) || err != nil {
					t.Fatalf("Write(%q) = %d, %v; want %d, nil", write, n, err, len(write))
				}
			}
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("the terminal got %q, want %q", got, tc.want)
			}
		})
	}
}
