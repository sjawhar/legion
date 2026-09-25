package daemon

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// ignoredBy is the SigIgn mask of the process `sh -c script` ends in, which reads its own
// /proc/self/status: a probe attempt's script, or the empty one a later child such as the
// controller's Oh My Pi stands for.
func ignoredBy(t *testing.T, script string) uint64 {
	t.Helper()
	out, err := exec.Command("sh", "-c", script+"exec sed -n 's/^SigIgn:[[:space:]]*//p' /proc/self/status").Output()
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
	before := ignoredBy(t, "")
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
	if during := ignoredBy(t, ""); during&bit(syscall.SIGTTOU) == 0 {
		t.Fatalf("while the attempt holds the terminal, SigIgn = %#x: SIGTTOU is not ignored", during)
	}
	job.release()
	if after := ignoredBy(t, ""); after != before {
		t.Fatalf("after release a child's SigIgn = %#x, want %#x as before the attempt", after, before)
	}
}

// At the terminal the attempt ignores SIGTSTP, so a Ctrl-Z cannot stop the terminal's foreground
// job while this process waits in the background; an attempt run as the daemon's are does not.
func TestATerminalAttemptIgnoresSIGTSTP(t *testing.T) {
	requireProcStatus(t)
	var background *terminalJob
	if mask := ignoredBy(t, background.script("")); mask&bit(syscall.SIGTSTP) != 0 {
		t.Skip("this test process was started with SIGTSTP ignored")
	}
	job := &terminalJob{}
	if mask := ignoredBy(t, job.script("")); mask&bit(syscall.SIGTSTP) == 0 {
		t.Fatalf("a terminal attempt's SigIgn = %#x: SIGTSTP is not ignored", mask)
	}
	if mask := ignoredBy(t, ""); mask&bit(syscall.SIGTSTP) != 0 {
		t.Fatalf("this process's own SIGTSTP changed: a child's SigIgn = %#x", mask)
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
