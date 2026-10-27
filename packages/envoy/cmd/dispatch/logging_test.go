package main

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A boot that cannot go on exits with 1, and the reason it logged reaches stderr first. Log lines
// are written by a goroutine of their own (logging.Handler), which os.Exit does not wait for, so
// an exit that did not flush them could end the process with its reason still buffered. This boot
// fails late, after it has connected, migrated and logged other lines, on a default project the
// database does not hold.
func TestABootFailureWritesItsReasonBeforeTheProcessExits(t *testing.T) {
	database := storetest.Open(t)
	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{
		Env: map[string]string{"DISPATCH_DEFAULT_PROJECT": "NOSUCHPROJECT"},
	})
	process.WaitExit(t, "a boot naming a default project the database does not hold")
	if code := process.Cmd.ProcessState.ExitCode(); code != 1 {
		t.Errorf("the refused boot exited %d, want 1", code)
	}
	want := `level=ERROR msg="dispatch: validate default project" error="DISPATCH_DEFAULT_PROJECT \"NOSUCHPROJECT\" does not exist"`
	if output := process.Output.String(); !strings.Contains(output, want) {
		t.Errorf("the refused boot's output lacks its reason %s:\n%s", want, output)
	}
}
