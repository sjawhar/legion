package stacktest

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

const statusEnv = "ENVOY_STACKTEST_STATUS"

// TestUnderReportsChildStatus makes each child outcome observable from the test binary that owns
// it. A stack overflow ends that binary, so the cases must run out of process. Each verdict is
// matched on the child's own unindented line, so a copy quoted inside a message cannot stand in
// for it.
func TestUnderReportsChildStatus(t *testing.T) {
	for _, test := range []struct {
		name   string
		fails  bool
		output string
	}{
		{name: "skip", output: "\n--- SKIP: TestUnderChildStatus ("},
		{name: "pass", output: "\n--- PASS: TestUnderChildStatus ("},
		{name: "pass with a skipped subtest", output: "\n--- PASS: TestUnderChildStatus ("},
		{name: "fail", fails: true, output: "under a 1048576-byte stack cap"},
		{name: "stack exhaustion", fails: true, output: "fatal error: stack overflow"},
		{name: "stack exhaustion after long output", fails: true, output: "fatal error: stack overflow"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			child := exec.Command(os.Args[0], "-test.run", "^TestUnderChildStatus$", "-test.v", "-test.count=1")
			child.Env = append(os.Environ(), statusEnv+"="+test.name)
			output, err := child.CombinedOutput()
			if (err != nil) != test.fails {
				t.Fatalf("child error = %v, want failure %t\n%s", err, test.fails, output)
			}
			if !strings.Contains(string(output), test.output) {
				t.Fatalf("child output does not contain %q:\n%s", test.output, output)
			}
		})
	}
}

func TestUnderChildStatus(t *testing.T) {
	switch os.Getenv(statusEnv) {
	case "":
		t.Skip("child-status helper")
	case "skip":
		Under(t, 1<<20, func(t *testing.T) { t.Skip("database unavailable") })
	case "pass":
		Under(t, 1<<20, func(*testing.T) {})
	case "pass with a skipped subtest":
		Under(t, 1<<20, func(t *testing.T) {
			t.Run("skips", func(t *testing.T) { t.Skip("database unavailable") })
			t.Run("passes", func(*testing.T) {})
		})
	case "fail":
		Under(t, 1<<20, func(t *testing.T) { t.Error("expected child failure") })
	case "stack exhaustion":
		Under(t, 1<<20, func(*testing.T) { exhaustStack() })
	case "stack exhaustion after long output":
		// A child that logs before it dies, as a room logging every update it cannot read does,
		// still shows how it died.
		Under(t, 1<<20, func(*testing.T) {
			fmt.Fprint(os.Stderr, strings.Repeat("ERROR a log line written before the overflow\n", 400))
			exhaustStack()
		})
	default:
		t.Fatalf("unknown child status %q", os.Getenv(statusEnv))
	}
}

//go:noinline
func exhaustStack() {
	var frame [4096]byte
	runtime.KeepAlive(frame)
	exhaustStack()
}
