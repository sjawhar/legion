package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// idleRigSaveWatch is the release's runtime with one thing added: it reads the state of the claim
// whose shim is being asked, at the moment it is asked. Only a shim can build the bundle — it
// runs the `jj` of the workspace it already works in — and a shim dies with its role's process,
// so the one thing the save's place in the release has to hold is that the process is still
// there when the save runs.
type idleRigSaveWatch struct {
	runtime.Runtime
	bundler   runtime.WorkspaceBundler
	suspender runtime.IssueSuspender
	images    issuePodImages
	observe   func()
}

func (w *idleRigSaveWatch) BundleWorkspace(ctx context.Context, issue string, role claim.Role) (runtime.WorkspaceBundle, error) {
	w.observe()
	return w.bundler.BundleWorkspace(ctx, issue, role)
}

func (w *idleRigSaveWatch) SuspendIssue(ctx context.Context, issue, tree string, authorize func(context.Context) (bool, error)) error {
	return w.suspender.SuspendIssue(ctx, issue, tree, authorize)
}

func (w *idleRigSaveWatch) IssuePodRunsConfiguredImage(issue string) bool {
	return w.images.IssuePodRunsConfiguredImage(issue)
}

// A release saves the workspace's unpushed work while the role whose shim builds it is still
// running, and only then stops it. The save is a row of its own (record.IssueBundle) ahead of the
// release's `release` rows, because the runtime asks the role's shim and refuses a claim with no
// process — "holds no role process to bundle it" (sandbox.Runtime.BundleWorkspace). With the save
// in the teardown behind those stops, as it was, every release of every pod was cancelled
// bundle_save_failed and no pod was ever released.
func TestTheReleaseSavesTheWorkspaceWhileTheRolesProcessIsStillRunning(t *testing.T) {
	rig := newIdleRig(t)
	ctx := context.Background()

	inner := rig.sup.deps.Runtime
	bundler, ok := inner.(runtime.WorkspaceBundler)
	if !ok {
		t.Fatal("the rig's runtime bundles no workspace")
	}
	suspender, ok := inner.(runtime.IssueSuspender)
	if !ok {
		t.Fatal("the rig's runtime suspends no issue")
	}
	images, ok := inner.(issuePodImages)
	if !ok {
		t.Fatal("the rig's runtime reads no pod image")
	}
	var saves int
	var atSave supervise.Claim
	rig.sup.deps.Runtime = &idleRigSaveWatch{Runtime: inner, bundler: bundler, suspender: suspender, images: images,
		observe: func() {
			saves++
			atSave = rig.machine.Claim()
		}}

	rig.w.releaseIdlePods(ctx)
	if got := rig.rows(t); len(got) != 3 || got[0] != "issue_bundle" {
		t.Fatalf("queued rows = %v, want the save first", got)
	}
	rig.drain(t)

	if saves != 1 {
		t.Fatalf("the release asked for the bundle %d times, want exactly once", saves)
	}
	if atSave.State == supervise.StateSuspended || atSave.Locator == nil {
		t.Fatalf("at the save the claim was %s by %q with locator %v; want its role process still running",
			atSave.State, atSave.SuspendedBy, atSave.Locator)
	}
	if got := rig.machine.Claim(); got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByReleased {
		t.Fatalf("after the release the claim = %s by %s, want suspended by released", got.State, got.SuspendedBy)
	}
	if !rig.bundled(t) {
		t.Fatal("the release stored no bundle")
	}
}

// The workflow engine and the daemon address the pod tables of the idle release under one
// project. The engine writes `listener_reports` (workflow's listenerWake) and the daemon reads it
// (workflowRuntime.listenerLive), and the two spellings of a project are not the same string: the
// key is what an issue's row holds ("LEGION"), the claim token's is what `claims.project` and
// every table keyed on a claim hold ("legion"). With the engine on the key and the daemon on the
// token, as it was, the gate R7 reads never opened and no pod was ever released.
//
// This writes through the engine and reads through the daemon's own store, which is the only
// shape that catches a divergence: every test that picks one spelling and uses it on both sides
// passes either way, which is how it was missed.
func TestTheEngineAndTheDaemonReadThePodTablesUnderOneProject(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	st := outboxTreeStore(t, pool, "LEGION-208")
	token, err := claim.ProjectToken("LEGION")
	if err != nil {
		t.Fatal(err)
	}
	if token == "LEGION" {
		t.Fatal("the project token and the project key are the same string, so this test proves nothing")
	}
	cfg := engineConfig(idleRigProjectConfig(), "legion-reviewer[bot]", Build{})
	// The engine is handed the store's own view of the project, which derives that one spelling
	// (store.Store.PodRecordsOf): the engine carries no second project of its own.
	if cfg.Pods, err = st.PodRecordsOf(cfg.Project); err != nil {
		t.Fatal(err)
	}
	engine := workflow.New(record.NewStore(), cfg, quietLogger())

	at := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := intake.ApplyFact(ctx, pool, "wake", "listener-ready-1",
		intake.ListenerWake{Kind: intake.ListenerWakeReady, At: at, Report: "listener-ready-1"}, engine); err != nil {
		t.Fatalf("apply the listener's ready report: %v", err)
	}

	live, read, found, err := st.ListenerLive(ctx, token, at.Add(time.Minute), listenerReadyWindow)
	if err != nil {
		t.Fatalf("read the listener's report: %v", err)
	}
	if !found || !live || !read.Equal(at) {
		t.Fatalf("the daemon read the listener's report as live=%t found=%t at=%s, want the report the engine wrote at %s",
			live, found, read, at)
	}
	if _, _, underTheKey, err := st.ListenerLive(ctx, cfg.Project, at.Add(time.Minute), listenerReadyWindow); err != nil || underTheKey {
		t.Fatalf("the report is filed under the project key %q too (%v); the pod tables are keyed on the claim token alone", cfg.Project, err)
	}
}

// idleRigProjectConfig is the smallest configuration engineConfig reads: the project whose two
// spellings this test is about.
func idleRigProjectConfig() config.Config {
	return config.Config{Project: "LEGION"}
}
