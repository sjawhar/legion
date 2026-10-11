package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// migratedStore is a migrated store on a database of its own: the daemon's authorization of an
// issue's close, which SuspendIssue runs under its pod's launch turn.
func migratedStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("LEGION_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LEGION_TEST_PG_DSN is unset, so there is no Postgres to authorize the issue's close against")
	}
	ctx := context.Background()
	base, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse LEGION_TEST_PG_DSN: %v", err)
	}
	adminURL := *base
	adminURL.Path = "/postgres"
	admin, err := pgxpool.New(ctx, adminURL.String())
	if err != nil {
		t.Fatalf("connect to the admin database: %v", err)
	}
	t.Cleanup(admin.Close)
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	name := "legion_sandbox_test_" + hex.EncodeToString(suffix[:])
	if _, err := admin.Exec(ctx, "create database "+name); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "drop database "+name+" with (force)"); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})
	testURL := *base
	testURL.Path = "/" + name
	st, err := store.Open(ctx, testURL.String())
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(st.Close)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

// authorizeClose is the daemon's authorization of close, the store's check (store.IssueSuspension).
func authorizeClose(st *store.Store, close store.IssueClose) func(context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		return st.IssueSuspension(ctx, testProject, close, record.OutOfWorkflow)
	}
}

func TestIssueSuspensionReadmissionResumesTheRetainedSession(t *testing.T) {
	st := migratedStore(t)
	g := newRig(t, nil)
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
	old := g.spawn(spec)
	name := SandboxName(rootToken)
	uid := g.sandbox(name).UID
	authorize := authorizeClose(st, store.IssueClose{Issue: testTree, Tree: testTree, IssueGeneration: 1, TreeGeneration: 1, Row: 1})
	stored := supervise.Claim{Token: rootToken, Project: testProject, Tree: testTree, TreeEpoch: 1, Issue: testTree, Role: claim.RoleArchitect,
		Generation: 1, State: supervise.StateWorking, Locator: &old, Session: "retained-session", SessionFile: resumeSession}
	if err := st.PutClaim(g.ctx, stored); err != nil {
		t.Fatal(err)
	}
	// A stored role still working keeps the close a wait, independently of runtime membership.
	if err := g.r.SuspendIssue(g.ctx, testTree, testTree, false, authorize); !errors.Is(err, wait.ErrWaiting) || !strings.Contains(err.Error(), "stored claim") {
		t.Fatalf("issue suspension with a remaining role process = %v, want the stored claim's wait", err)
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
	if err := g.r.SuspendIssue(g.ctx, testTree, testTree, false, authorize); err != nil {
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
	if err := g.r.SuspendIssue(context.Background(), testTree, testTree, false, authorize); err != nil {
		t.Fatalf("stale close: %v", err)
	}
	if obs, err := g.r.Probe(g.ctx, loc); err != nil || obs.Kind != runtime.Alive {
		t.Fatalf("stale close stopped the re-admitted role: %s, %v", obs.Kind, err)
	}
}

// A child's close never touches its parent's pod: the parent runs on a Sandbox and volume of its
// own. A child parked (backlog, icebox, triage) has its Sandbox Suspended and kept, its volume and
// the sessions on it with it, for a later todo to resume. A child closed as done releases it: once
// its claims have retired, the Sandbox is deleted with foreground propagation, fenced to the object
// as read, and awaited until the API no longer has it, so garbage collection takes the volume and
// Secrets it owns; the restricted runtime sends no PVC request itself. A retried close finds the
// Sandbox gone and finishes.
func TestChildIssueSuspensionKeepsItsParentRunning(t *testing.T) {
	for _, tc := range []struct {
		name    string
		release bool
		state   supervise.ClaimState
	}{
		{name: "a parked child keeps its Sandbox", state: supervise.StateSuspended},
		{name: "a done child releases its Sandbox and volume", release: true, state: supervise.StateRetired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := migratedStore(t)
			g := newRig(t, nil)
			deletes := map[string]metav1.DeleteOptions{}
			g.dyn.PrependReactor("delete", "sandboxes", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
				delete := action.(k8stesting.DeleteAction)
				deletes[delete.GetName()] = delete.GetDeleteOptions()
				return false, nil, nil
			})
			if _, err := st.OpenTreeLifecycle(g.ctx, testProject, testTree, treelifecycle.AuthorityWorkflow); err != nil {
				t.Fatal(err)
			}
			if _, err := st.Pool().Exec(g.ctx, `insert into issues
				(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
				values ($1, $1, 'LEGION', 'root', 'implementing', 7, 'in_progress', 'A', 1),
					($2, $1, 'LEGION', 'child', 'done', 2, 'done', 'B', 1)`, testTree, childIssue); err != nil {
				t.Fatal(err)
			}
			parent := g.spawn(rootSpec(t))
			g.issueLaunchers(childToken)
			child := g.spawn(childSpec(t))
			if err := g.r.Suspend(g.ctx, child); err != nil {
				t.Fatal(err)
			}
			// The child's one stored claim is where the close's stops left it: suspended with its
			// session for a parked child, retired with none for a done one.
			stored := supervise.Claim{Token: childToken, Project: testProject, Tree: testTree, TreeEpoch: 1, Issue: childIssue, Role: claim.RoleImplementer,
				Generation: 1, State: tc.state}
			if !tc.release {
				stored.Session, stored.SessionFile = "child-session", resumeSession
			}
			if err := st.PutClaim(g.ctx, stored); err != nil {
				t.Fatal(err)
			}
			name := SandboxName(childToken)
			uid := g.sandbox(name).UID
			g.clearActions()
			authorize := authorizeClose(st, store.IssueClose{Issue: childIssue, Tree: testTree, IssueGeneration: 2, TreeGeneration: 7, Row: 1, Release: tc.release})

			if err := g.r.SuspendIssue(g.ctx, childIssue, testTree, tc.release, authorize); err != nil {
				t.Fatal(err)
			}

			if obs, err := g.r.Probe(g.ctx, parent); err != nil || obs.Kind != runtime.Alive {
				t.Fatalf("child close affected its parent: %s, %v", obs.Kind, err)
			}
			if got := g.sandbox(SandboxName(rootToken)); got == nil || got.mode() != modeRunning {
				t.Fatalf("parent Sandbox after the child's close = %+v, want Running", got)
			}
			if !tc.release {
				if got := g.sandbox(name); got == nil || got.mode() != modeSuspended || got.UID != uid {
					t.Fatalf("parked child Sandbox = %+v, want the same Sandbox Suspended", got)
				}
				if len(deletes) != 0 {
					t.Fatalf("a parked child's close deleted %v, want nothing deleted", deletes)
				}
				return
			}
			if got := g.sandbox(name); got != nil {
				t.Fatalf("done child Sandbox = %+v, want it gone", got)
			}
			got, deleted := deletes[name]
			if !deleted || got.PropagationPolicy == nil || *got.PropagationPolicy != metav1.DeletePropagationForeground {
				t.Fatalf("child Sandbox delete options = %+v (deleted %t), want foreground propagation", got, deleted)
			}
			if got.Preconditions == nil || got.Preconditions.UID == nil || *got.Preconditions.UID != uid || got.Preconditions.ResourceVersion == nil {
				t.Fatalf("child Sandbox delete preconditions = %+v, want the Sandbox's uid %s and its resourceVersion", got.Preconditions, uid)
			}
			if _, other := deletes[SandboxName(rootToken)]; other || len(deletes) != 1 {
				t.Fatalf("deletes = %v, want the child's Sandbox alone", deletes)
			}
			for _, a := range g.writes() {
				if a.resource == "persistentvolumeclaims" {
					t.Fatalf("the restricted runtime sent a PVC request: %s", a)
				}
			}
			// A retry after the delete — the outbox row run again — finds no Sandbox and finishes.
			if err := g.r.SuspendIssue(g.ctx, childIssue, testTree, true, authorize); err != nil {
				t.Fatalf("retried release = %v, want it finished on the Sandbox already gone", err)
			}
			if len(deletes) != 1 {
				t.Fatalf("the retried release deleted again: %v", deletes)
			}
		})
	}
}
