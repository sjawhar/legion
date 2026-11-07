package testpg

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// cloneChildVariable marks the run of this test binary that does the cloning, so the parent run
// reads one child's whole stderr rather than its own, which holds whatever else the package's
// tests logged.
const cloneChildVariable = "LEGION_TESTPG_CLONE_CHILD"

// Every clone of a test process copies the one template that process migrated: the whole point of
// Clone over Database plus Migrate. The child run clones twice and its stderr must hold one
// `testpg: migrated template` line, the line prepare logs; a second line is a second migration
// run, which is the regression this guards — a template rebuilt per test, or per clone.
func TestTwoClonesInOneProcessBuildOneTemplate(t *testing.T) {
	if os.Getenv("LEGION_TEST_PG_DSN") == "" {
		t.Skip(SkipMessage)
	}
	if os.Getenv(cloneChildVariable) != "" {
		first, second := Clone(t, "legion_testpg_clone_test"), Clone(t, "legion_testpg_clone_test")
		if first == second {
			t.Fatalf("both clones are the same database: %s", first)
		}
		return
	}

	child := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.count=1")
	child.Env = append(os.Environ(), cloneChildVariable+"=1")
	var stdout, stderr bytes.Buffer
	child.Stdout, child.Stderr = &stdout, &stderr
	if err := child.Run(); err != nil {
		t.Fatalf("the cloning run of this test binary failed: %v\n%s%s", err, stdout.String(), stderr.String())
	}
	if builds := strings.Count(stderr.String(), "testpg: migrated template"); builds != 1 {
		t.Fatalf("two clones in one process built %d templates, want 1; the run's stderr was:\n%s", builds, stderr.String())
	}
}
