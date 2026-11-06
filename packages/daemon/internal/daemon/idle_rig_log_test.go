package daemon

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// logFileChildEnv tells the re-executed test binary to run the helper below, which is skipped in
// an ordinary run. The helper has to fail, and a test that fails fails its parent, so it runs in
// a binary of its own.
const logFileChildEnv = "LEGION_TEST_IDLE_RIG_LOG_CHILD"

// TestTheIdleRigLogHelperFails is the child of the test below: it takes the rig's daemon log and
// then fails, so that the parent can read what the test binary left in its TMPDIR. It runs only
// when the parent re-executes this binary.
func TestTheIdleRigLogHelperFails(t *testing.T) {
	if os.Getenv(logFileChildEnv) != "1" {
		t.Skip(logFileChildEnv + " is not set: this test is the child of TestAFailedTestLeavesNoDaemonLog")
	}
	rig := &idleWakeRig{t: t}
	rig.logFile()
	t.Error("the child fails on purpose: what is measured is what its cleanup leaves behind")
}

// A test run that ends leaves no file in the temporary directory it was given, whether its tests
// passed or failed (dispatch://INFRA-1346/spec#b-a1: "whether its tests pass or fail", and "the
// six removals after m.Run become removals a finished run makes whatever its tests concluded";
// the two endings R1 exempts are a run killed by a signal and a test that panics, and a test that
// merely fails is neither). The idle rig's daemon log is the site R1 names: it is a quarter of a
// megabyte, and a run in a Legion issue pod writes it onto the issue's own volume, where every
// red round leaves another copy.
//
// The child runs with a TMPDIR of its own, so the directory this test reads holds nothing but
// what the child left, and t.TempDir removes it whatever this test concludes.
func TestAFailedTestLeavesNoDaemonLog(t *testing.T) {
	dir := t.TempDir()
	child := exec.Command(os.Args[0], "-test.run", "^TestTheIdleRigLogHelperFails$", "-test.v")
	child.Env = append(os.Environ(), logFileChildEnv+"=1", "TMPDIR="+dir, "GOTMPDIR="+dir)
	out, err := child.CombinedOutput()
	if err == nil {
		t.Fatalf("the child was meant to fail; it passed:\n%s", out)
	}
	if !strings.Contains(string(out), "the daemon's log:") {
		t.Fatalf("the child never took a daemon log:\n%s", out)
	}
	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		names := make([]string, 0, len(left))
		for _, entry := range left {
			names = append(names, entry.Name())
		}
		t.Fatalf("a run whose test failed left %d file(s) in the temporary directory it was given: %s",
			len(left), strings.Join(names, ", "))
	}
}
