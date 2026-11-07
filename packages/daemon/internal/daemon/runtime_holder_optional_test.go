package daemon

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// holderOptionalRuntime is a runtime that answers every optional interface the daemon asks
// `deps.Runtime` for by a type assertion. The Sandbox runtime answers all three
// (runtime/sandbox: SuspendIssue, BundleWorkspace, podimage.go's
// IssuePodRunsConfiguredImage), so a holder in front of it must answer them too.
type holderOptionalRuntime struct {
	*fake.Runtime
}

func (r *holderOptionalRuntime) SuspendIssue(context.Context, string, string, func(context.Context) (bool, error)) error {
	return nil
}

func (r *holderOptionalRuntime) BundleWorkspace(context.Context, string, claim.Role) (runtime.WorkspaceBundle, error) {
	return runtime.WorkspaceBundle{}, nil
}

func (r *holderOptionalRuntime) IssuePodRunsConfiguredImage(string) bool { return true }

// TestTheRuntimeHolderAnswersEveryOptionalInterfaceItsRuntimeDoes: `deps.Runtime` is the runtime
// holder (daemon.go: `rt := newRuntimeHolder(...)`, then `supervise.Deps{Runtime: rt}`), and
// three places ask it for an interface the core runtime.Runtime does not carry:
//
//   - outbox.go:507, `record.IssueSuspend`, runtime.IssueSuspender — a closed issue's Sandbox,
//     pod and disk are deleted through SuspendIssue. The miss returns nil, so the row is marked
//     done and the pod and its disk are never deleted.
//   - outbox.go:1397 and :1410, runtime.IssueSuspender then runtime.WorkspaceBundler — the
//     unpushed work of a released issue's workspace. The first miss returns nil as "a runtime
//     with no shared issue resources", so the release saves nothing.
//   - workflow.go:857, issuePodImages — attachIdleRelease, which logs "idle release: the runtime
//     reads no pod image, so no pod is released" and leaves the idle tick off.
//
// Two of the three fail silently, which is why only the idle rig caught it. A holder that
// forwards the core interface alone passes every other test in this package and takes those
// three behaviours out of every Kubernetes deployment, so the rule is asserted on the holder
// itself rather than on any one caller: whatever the runtime inside it answers, the holder
// answers.
func TestTheRuntimeHolderAnswersEveryOptionalInterfaceItsRuntimeDoes(t *testing.T) {
	inner := &holderOptionalRuntime{Runtime: fake.NewRuntime()}

	// The inner runtime answers all three: if this fails the fixture is wrong, not the holder.
	if _, ok := runtime.Runtime(inner).(runtime.IssueSuspender); !ok {
		t.Fatal("the fixture runtime is no IssueSuspender")
	}
	if _, ok := runtime.Runtime(inner).(runtime.WorkspaceBundler); !ok {
		t.Fatal("the fixture runtime is no WorkspaceBundler")
	}
	if _, ok := runtime.Runtime(inner).(issuePodImages); !ok {
		t.Fatal("the fixture runtime reads no pod image")
	}

	held := runtime.Runtime(newRuntimeHolder(inner, func() {}, nil,
		func(cfg config.Config) (runtime.Runtime, context.CancelFunc, error) {
			return inner, func() {}, nil
		}))

	if _, ok := held.(runtime.IssueSuspender); !ok {
		t.Error("the runtime holder is no IssueSuspender, so outbox.go:507 returns nil and a closed " +
			"issue's pod, disk and Sandbox are never deleted")
	}
	if _, ok := held.(runtime.WorkspaceBundler); !ok {
		t.Error("the runtime holder is no WorkspaceBundler, so outbox.go:1410 cancels the release " +
			"save and the unpushed work of a released workspace is never bundled")
	}
	if _, ok := held.(issuePodImages); !ok {
		t.Error("the runtime holder reads no pod image, so attachIdleRelease leaves the idle tick " +
			"off and no pod is ever released (TestIdleReleaseAndWake's own failure)")
	}
}
