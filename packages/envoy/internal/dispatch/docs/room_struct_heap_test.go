package docs

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// This file holds the harness every heap measurement of this package runs under (measuredAlone):
// a test that reads the heap a room or a copy holds runs again in a process of its own, where a
// collection costs what that process holds rather than what a whole run of this package holds by
// the time it reaches that test.
//
// It once held the prices of a struct, a split and a byte of content, each measured from frames
// a browser can send. One price replaced the six (bytesOfHeapAStateByte), and that price is of a
// room's whole encoded state, not of a frame, so what measures it is a room's own heap against
// its state (TestAResidentRoomsHeapPerByteOfItsState) and nothing here.

// heapPriceChild is the environment variable naming the test a process was started to measure
// alone (measuredAlone).
const heapPriceChild = "DISPATCH_HEAP_PRICE_CHILD"

// measurementReportingMargin is how long before the run's own deadline (testing.T.Deadline, the
// -timeout the test binary was started with) the process a measurement runs in is killed, so this
// process has that long to report what it printed and to fail. Without it the binary's timeout
// panic would end this process while the measurement runs, leaving that process running with
// nothing waiting for it: it holds the pipe it prints to, a copy of this binary's heap and, under
// the race detector, its shadow memory, for as long as it goes on.
const measurementReportingMargin = time.Minute

// measurementOutputWait is how long Wait waits for the measurement's output to end once the
// process itself has gone (exec.Cmd.WaitDelay), so a process it left behind holding the write end
// of that pipe cannot keep this test waiting for it.
const measurementOutputWait = 10 * time.Second

// measuredAloneOutput runs the calling test, or subtest, again in a process of its own and
// reports what that process printed and whether this is that process, the one to measure in; the
// caller returns when it is not. A reading its caller needs where the readings meet - the
// heaviest of shapes measured one process each - is read out of that output
// (TestAResidentRoomsHeapPerByteOfItsState). A collection costs as much as the heap the process
// holds, and a whole run of this package holds a gigabyte or more by the time these tests run:
// on this pod the three of them took 108, 75 and
// 16 s inside the package's run, against 2.5, 1.4 and 0.2 s in a process of their own, with the
// same readings. The process runs the one test of that exact name, each level of it matched whole.
// The test that started the process logs what the process printed and the process's peak resident
// memory, and fails when it failed. The peak is the kernel's for the process, which folds in the
// peak of the memory the process was started from (Go starts it sharing this process's memory
// until its exec), so it reads no less than this process's own peak at the start, and bounds the
// process's own from above. The process outlives neither this test nor this process: it is killed,
// as a group, once this test ends and in any case a minute before the run's own deadline
// (measurementReportingMargin), and its output is waited for no longer than
// measurementOutputWait past it.
func measuredAloneOutput(t *testing.T) (string, bool) {
	t.Helper()
	if os.Getenv(heapPriceChild) == t.Name() {
		return "", true
	}
	levels := strings.Split(t.Name(), "/")
	for i, level := range levels {
		levels[i] = "^" + regexp.QuoteMeta(level) + "$"
	}
	measuring, stopMeasuring := context.WithCancel(context.Background())
	defer stopMeasuring()
	if deadline, ok := t.Deadline(); ok {
		measuring, stopMeasuring = context.WithDeadline(measuring, deadline.Add(-measurementReportingMargin))
		defer stopMeasuring()
	}
	child := exec.CommandContext(measuring, os.Args[0], "-test.run="+strings.Join(levels, "/"), "-test.count=1", "-test.v")
	child.Env = append(os.Environ(), heapPriceChild+"="+t.Name())
	// The process is its own group, and the kill is the group's, so whatever it started goes with
	// it; a kill that finds the group gone (ESRCH) is the process having just exited, which Wait
	// reports as it does any other.
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	child.Cancel = func() error {
		if err := syscall.Kill(-child.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return nil
	}
	child.WaitDelay = measurementOutputWait
	output, err := child.CombinedOutput()
	t.Logf("measured in a process of its own:\n%s", output)
	if child.ProcessState != nil {
		if usage, ok := child.ProcessState.SysUsage().(*syscall.Rusage); ok {
			t.Logf("the process's peak resident memory, at least this process's own: %d KiB", usage.Maxrss)
		}
	}
	if err != nil {
		// The kill this deadline makes reaches the caller as "signal: killed", which says nothing
		// of the deadline: the run's own -timeout, less measurementReportingMargin, ran out while
		// the measurement was still going.
		if errors.Is(measuring.Err(), context.DeadlineExceeded) {
			t.Fatalf("the measurement in a process of its own was killed %s before the test binary's deadline (measurementReportingMargin), with the measurement still running: this looks like a misconfigured run; raise -timeout",
				measurementReportingMargin)
		}
		t.Fatalf("the measurement in a process of its own failed: %v", err)
	}
	return string(output), false
}

// measuredAlone is measuredAloneOutput for a measurement whose caller wants nothing of what the
// process printed beyond the log and the failure it already reports.
func measuredAlone(t *testing.T) bool {
	t.Helper()
	_, here := measuredAloneOutput(t)
	return here
}
