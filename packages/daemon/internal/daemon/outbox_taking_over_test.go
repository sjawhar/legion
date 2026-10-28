package daemon

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// takingOverRuntime is a process runtime with no issue Sandbox to suspend that took the project's
// claims over from another runtime, as the tmux runtime does under runtime.tmux.take_over.
type takingOverRuntime struct {
	*fake.Runtime
	from string
}

func (r takingOverRuntime) TakesOver() string { return r.from }

// An issue close's suspend effect on a runtime that took the project over from Kubernetes finishes,
// since that runtime cannot reach a Sandbox, but says so, naming the Sandbox the operator still has
// to delete. Three LINUX issues (LINUX-446, LINUX-402, LINUX-440) closed in the 17 minutes before
// their Kubernetes daemon stopped; the tmux daemon that took the project over finished their
// suspend effects as silent no-ops, and each kept its Sandbox and its volume. A runtime that took
// nothing over finishes the effect silently, as before: it never had a Sandbox.
func TestAnIssueCloseOnATakingOverRuntimeNamesTheSandboxItCannotRelease(t *testing.T) {
	for name, tc := range map[string]struct {
		from string
		want bool
	}{
		"taking over kubernetes": {from: runtime.RuntimeSandbox, want: true},
		"taking nothing over":    {from: "", want: false},
	} {
		t.Run(name, func(t *testing.T) {
			sup, _ := newOutboxSupervisor(t, "linux", t.TempDir())
			sup.deps.Runtime = takingOverRuntime{Runtime: fake.NewRuntime(), from: tc.from}
			logs := &syncBuffer{}
			runner := &outbox{supervisor: sup, project: "linux", dispatchProject: "LINUX", log: slog.New(slog.NewTextHandler(logs, nil))}
			row := mustOutboxRow(t, "LINUX-446", record.IssueSuspend{Tree: "LINUX-354", Generation: 1, TreeGeneration: 1}, time.Now())
			if err := runner.execute(context.Background(), row); err != nil {
				t.Fatalf("execute = %v, want the close finished", err)
			}
			got := logs.String()
			named := strings.Contains(got, "sandbox=legion-linux-linux-446") && strings.Contains(got, "released nothing there")
			if named != tc.want {
				t.Fatalf("warning naming legion-linux-linux-446 = %v, want %v; log:\n%s", named, tc.want, got)
			}
		})
	}
}
