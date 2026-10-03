package sandbox

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

func TestIssueSuspensionReadmissionResumesTheRetainedSession(t *testing.T) {
	st := resourceStore(t)
	g := newRig(t, nil)
	g.r.SetIssueResourceStore(st)
	if _, err := st.OpenTreeLifecycle(g.ctx, testProject, testTree, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool().Exec(g.ctx, `insert into issues
		(key, tree, project, title, phase, generation, status, rank, linger_until, last_dispatch_seq)
		values ($1, $1, 'LEGION', 'root', 'done', 1, 'done', 'A', now() + interval '1 hour', 1)`, testTree); err != nil {
		t.Fatal(err)
	}
	g.issueLaunchers(rootToken)
	spec := rootSpec(t)
	spec.TreeEpoch = 1
	old := g.spawn(spec)
	name := SandboxName(rootToken)
	uid := g.sandbox(name).UID
	key := runtime.IssueResourceKey{Issue: testTree, Tree: testTree, IssueGeneration: 1, TreeGeneration: 1, StopRow: 1}
	stored := supervise.Claim{Token: rootToken, Project: testProject, Tree: testTree, TreeEpoch: 1, Issue: testTree, Role: claim.RoleArchitect,
		Generation: 1, State: supervise.StateWorking, Locator: &old, Session: "retained-session", SessionFile: resumeSession}
	if err := st.PutClaim(g.ctx, stored); err != nil {
		t.Fatal(err)
	}
	// A stored role still working keeps the close pending, independently of runtime membership.
	if err := g.r.SuspendIssue(g.ctx, key); err == nil || !strings.Contains(err.Error(), "stored claim") {
		t.Fatalf("issue suspension with a remaining role process = %v", err)
	}
	if g.sandbox(name).mode() != modeRunning {
		t.Fatal("suspension stopped a role that still runs")
	}
	if err := g.r.Suspend(g.ctx, old); err != nil {
		t.Fatal(err)
	}
	stored.State, stored.Locator = supervise.StateSuspended, nil
	if err := st.PutClaim(g.ctx, stored); err != nil {
		t.Fatal(err)
	}
	if err := g.r.SuspendIssue(g.ctx, key); err != nil {
		t.Fatal(err)
	}
	if got := g.sandbox(name); got == nil || got.mode() != modeSuspended || got.UID != uid {
		t.Fatalf("closed issue Sandbox = %+v, want the same Sandbox Suspended", got)
	}
	if g.pod(name) != nil {
		t.Fatal("issue suspension returned while its pod remained")
	}
	if _, err := st.Pool().Exec(g.ctx, `update issues set generation = 2, phase = 'admitted', status = 'todo', linger_until = null where key = $1`, testTree); err != nil {
		t.Fatal(err)
	}
	spec.Generation = 2
	spec.ResumeSessionFile = stored.SessionFile
	loc, err := g.r.Resume(g.ctx, &old, spec)
	if err != nil {
		t.Fatal(err)
	}
	if loc.Sandbox.PodUID == old.Sandbox.PodUID || g.sandbox(name).UID != uid || g.sandbox(name).mode() != modeRunning {
		t.Fatalf("re-admission did not resume the retained Sandbox in a new pod: old=%+v new=%+v sandbox=%+v", old, loc, g.sandbox(name))
	}
	var resumed string
	for _, frame := range g.sent(rootToken) {
		if start, ok := frame.(shimwire.LauncherStart); ok && start.Generation == 2 {
			resumed = start.ResumeFile
		}
	}
	if resumed != stored.SessionFile {
		t.Fatalf("resumed session = %q, want %q", resumed, stored.SessionFile)
	}
	if err := g.r.SuspendIssue(context.Background(), key); err != nil {
		t.Fatalf("stale close: %v", err)
	}
	if obs, err := g.r.Probe(g.ctx, loc); err != nil || obs.Kind != runtime.Alive {
		t.Fatalf("stale close stopped the re-admitted role: %s, %v", obs.Kind, err)
	}
}

func TestChildIssueSuspensionKeepsItsParentRunning(t *testing.T) {
	st := resourceStore(t)
	g := newRig(t, nil)
	g.r.SetIssueResourceStore(st)
	if _, err := st.OpenTreeLifecycle(g.ctx, testProject, testTree, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool().Exec(g.ctx, `insert into issues
		(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ($1, $1, 'LEGION', 'root', 'implementing', 7, 'in_progress', 'A', 1),
			($2, $1, 'LEGION', 'child', 'done', 2, 'done', 'B', 1)`, testTree, childIssue); err != nil {
		t.Fatal(err)
	}
	parentSpec := rootSpec(t)
	parentSpec.TreeEpoch = 1
	parent := g.spawn(parentSpec)
	g.issueLaunchers(childToken)
	spec := childSpec(t)
	spec.TreeEpoch = 1
	child := g.spawn(spec)
	if err := g.r.Suspend(g.ctx, child); err != nil {
		t.Fatal(err)
	}
	if err := g.r.SuspendIssue(g.ctx, runtime.IssueResourceKey{
		Issue: childIssue, Tree: testTree, IssueGeneration: 2, TreeGeneration: 7, StopRow: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if got := g.sandbox(SandboxName(childToken)); got == nil || got.mode() != modeSuspended {
		t.Fatalf("closed child Sandbox = %+v, want Suspended", got)
	}
	if obs, err := g.r.Probe(g.ctx, parent); err != nil || obs.Kind != runtime.Alive {
		t.Fatalf("child close affected its parent: %s, %v", obs.Kind, err)
	}
}
