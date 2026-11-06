package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// claimsForReap is the runtime's durable claim record for a reaping test. The
// retiring Sandbox's pod template, not claim metadata, identifies SQL-backed
// sessions.
type claimsForReap struct {
	*fakeStore
	claims []supervise.Claim
	err    error
}

func (s *claimsForReap) Claims(context.Context) ([]supervise.Claim, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.claims, nil
}

// legacySessionTemplate turns a current controller Sandbox into the
// pre-cutover volume-store shape: its template has neither durable database
// marker that a post-cutover runtime writes.
func legacySessionTemplate(t *testing.T, g *rig, name string) {
	t.Helper()
	s := g.sandbox(name)
	s.Spec.PodTemplate.Spec.Volumes, s.Spec.PodTemplate.Spec.Containers = nil, nil
	object, err := encodeSandbox(*s)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.dyn.Tracker().Update(sandboxGVR, object, testNamespace); err != nil {
		t.Fatal(err)
	}
	g.eventually("the legacy pod template to be observed", func() bool {
		current, err := g.r.storedSandbox(name)
		return err == nil && current != nil &&
			sessionStoreInPod(&current.Spec.PodTemplate.Spec) == sessionStoreName(ompsessions.StoreVolume, "", nil)
	})
}

func TestReplacementRefusesToReapAVolumeWithALegacySession(t *testing.T) {
	store := &claimsForReap{
		fakeStore: newFakeStore(),
		claims: []supervise.Claim{{
			Token: rootToken, Project: testProject, Tree: testTree, Issue: testTree,
			Session: "ses_unimported", SessionFile: "/sessions/ses_unimported.jsonl",
		}},
	}
	g := supersededIssue(t, withOptions(func(o *Options) { o.Store = store }))
	g.launcher(workerToken)

	_, err := g.r.Spawn(g.ctx, workerSpec(t))
	if err == nil {
		t.Fatal("replacement succeeded, want it to refuse reaping the legacy session volume")
	}
	for _, want := range []string{string(rootToken), "ses_unimported", IssueClaimName(workerToken),
		"run legion sessions import first", "then run legion claims forget-session"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("replacement refusal %q does not name %q", err, want)
		}
	}
	if s := g.sandbox(SandboxName(workerToken)); s == nil || s.UID != "uid-sandbox-superseded" {
		t.Fatalf("Sandbox after refused replacement = %+v, want the legacy Sandbox kept", s)
	}
}

// Forgetting a legacy session is the explicit acknowledgement that lets its
// volume reap. Importing it first preserves its contents in the session table;
// the runtime needs only the cleared durable claim to proceed.
func TestReplacementReapsAfterALegacySessionIsForgotten(t *testing.T) {
	store := &claimsForReap{
		fakeStore: newFakeStore(),
		claims: []supervise.Claim{{
			Token: rootToken, Project: testProject, Tree: testTree, Issue: testTree,
			Session: "ses_imported", SessionFile: "/sessions/ses_imported.jsonl",
		}},
	}
	g := supersededIssue(t, withOptions(func(o *Options) { o.Store = store }))
	g.launcher(workerToken)
	if _, err := g.r.Spawn(g.ctx, workerSpec(t)); err == nil {
		t.Fatal("replacement succeeded before the session was forgotten")
	}

	// This is the persisted claim transition RequestForgetSession makes after
	// the operator has imported the session and stopped the claim.
	store.claims[0].Session, store.claims[0].SessionFile = "", ""
	if _, err := g.r.Spawn(g.ctx, workerSpec(t)); err != nil {
		t.Fatalf("replacement after the session was forgotten: %v", err)
	}
	if s := g.sandbox(SandboxName(workerToken)); s == nil || s.UID == "uid-sandbox-superseded" {
		t.Fatalf("Sandbox after the session was forgotten = %+v, want its replacement", s)
	}
}

func TestReplacementReapsOnlyDatabaseBackedOrEmptySessions(t *testing.T) {
	for _, tc := range []struct {
		name      string
		database  func(*sandbox)
		claimsErr error
		claim     supervise.Claim
	}{
		{
			name: "the retiring Sandbox has the PostgreSQL providers projection",
			database: func(s *sandbox) {
				s.Spec.PodTemplate.Spec.Volumes = append(s.Spec.PodTemplate.Spec.Volumes, corev1.Volume{
					Name: providersVolume,
					VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
						Items: []corev1.KeyToPath{{Key: testSessionDSNKey, Path: ompsessions.DSNFileName}},
					}},
				})
			},
			claimsErr: errors.New("claims are unavailable"),
			claim: supervise.Claim{
				Token: rootToken, Project: testProject, Tree: testTree, Issue: testTree,
				Session: "ses_imported", SessionFile: "/sessions/ses_imported.jsonl",
			},
		},
		{
			name: "the retiring Sandbox has Cloud SQL launcher flags",
			database: func(s *sandbox) {
				s.Spec.PodTemplate.Spec.Containers = append(s.Spec.PodTemplate.Spec.Containers, corev1.Container{
					Name: "roles",
					Command: []string{"legion", "launcher",
						"--session-instance", "example:us-central1:legion",
						"--session-database", "legion_sessions",
						"--session-user", "legion@example.iam"},
				})
			},
			claimsErr: errors.New("claims are unavailable"),
			claim: supervise.Claim{
				Token: rootToken, Project: testProject, Tree: testTree, Issue: testTree,
				Session: "ses_imported", SessionFile: "/sessions/ses_imported.jsonl",
			},
		},
		{
			name: "the claim records no session",
			claim: supervise.Claim{
				Token: rootToken, Project: testProject, Tree: testTree, Issue: testTree,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &claimsForReap{fakeStore: newFakeStore(), claims: []supervise.Claim{tc.claim}, err: tc.claimsErr}
			g := supersededIssue(t, withOptions(func(o *Options) { o.Store = store }))
			if tc.database != nil {
				s := g.sandbox(SandboxName(workerToken))
				tc.database(s)
				object, err := encodeSandbox(*s)
				if err != nil {
					t.Fatal(err)
				}
				if err := g.dyn.Tracker().Update(sandboxGVR, object, testNamespace); err != nil {
					t.Fatal(err)
				}
			}

			g.spawn(workerSpec(t))

			if s := g.sandbox(SandboxName(workerToken)); s == nil || s.UID == "uid-sandbox-superseded" {
				t.Fatalf("Sandbox after replacement = %+v, want a replacement after %s", s, tc.name)
			}
		})
	}
}

// A stopped carried child is re-pointed to its new tree before the old tree's
// cleanup removes the still-labelled legacy Sandbox. The claim's session still
// belongs to that old Sandbox until the guard permits its reap.
func TestTreeCleanupRefusesToReapARetreedLegacySession(t *testing.T) {
	store := newFakeStore()
	store.claims = []supervise.Claim{{
		Token: childToken, Project: testProject, Tree: childIssue, Issue: childIssue,
		Session: "ses_carried", SessionFile: "/sessions/ses_carried.jsonl",
	}}
	name := SandboxName(childToken)
	object := withSupersededDisk(t, sandboxObject(t, name, "uid-sandbox-carried", modeSuspended,
		map[string]string{labelProject: testProject, labelTree: testTree, labelIssue: childIssue}))
	g := newRig(t, []k8sruntime.Object{object}, withoutController(), withOptions(func(o *Options) { o.Store = store }))

	err := g.r.CleanupTree(g.ctx, testTree)

	if err == nil || !strings.Contains(err.Error(), "refuse to reap volume") || !strings.Contains(err.Error(), "ses_carried") {
		t.Fatalf("cleanup = %v, want the carried session-volume refusal", err)
	}
	if s := g.sandbox(name); s == nil || s.UID != "uid-sandbox-carried" {
		t.Fatalf("Sandbox after refused cleanup = %+v, want the carried legacy Sandbox kept", s)
	}
}

func TestSessionVolumeReapGuardCoversEveryDeletionPath(t *testing.T) {
	legacy := func(token claim.Token, tree, issue string) supervise.Claim {
		return supervise.Claim{
			Token: token, Project: testProject, Tree: tree, Issue: issue,
			Session: "ses_unimported", SessionFile: "/sessions/ses_unimported.jsonl",
		}
	}
	refusal := func(t *testing.T, err error, g *rig, name string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "refuse to reap volume") {
			t.Fatalf("reap = %v, want the session-volume refusal", err)
		}
		if s := g.sandbox(name); s == nil {
			t.Fatal("the refusal still deleted the Sandbox")
		}
	}
	legacyIssue := func(t *testing.T, store *fakeStore) (*rig, string) {
		t.Helper()
		name := SandboxName(rootToken)
		object := withSupersededDisk(t, sandboxObject(t, name, "uid-sandbox-superseded", modeSuspended, claimLabels(claim.RoleArchitect)))
		g := newRig(t, []k8sruntime.Object{object}, withoutController(), withOptions(func(o *Options) { o.Store = store }))
		return g, name
	}

	t.Run("tree cleanup", func(t *testing.T) {
		store := newFakeStore()
		store.claims = []supervise.Claim{legacy(rootToken, testTree, testTree)}
		g, name := legacyIssue(t, store)
		refusal(t, g.r.CleanupTree(g.ctx, testTree), g, name)
	})

	t.Run("issue close", func(t *testing.T) {
		store := newFakeStore()
		store.claims = []supervise.Claim{legacy(rootToken, testTree, testTree)}
		g, name := legacyIssue(t, store)
		refusal(t, g.r.SuspendIssue(g.ctx, testTree, testTree, func(context.Context) (bool, error) { return true, nil }), g, name)
	})

	t.Run("issue orphan sweep", func(t *testing.T) {
		store := newFakeStore()
		store.claims = []supervise.Claim{legacy(rootToken, testTree, testTree)}
		store.close(testTree)
		g, name := legacyIssue(t, store)
		refusal(t, g.r.ReconcileOrphans(g.ctx, nil, 0), g, name)
	})

	controllerGuard := func(t *testing.T, reap func(*rig, runtime.Locator) error) {
		t.Helper()
		g := newRig(t, nil)
		loc := g.spawn(controllerSpec(t))
		legacySessionTemplate(t, g, loc.Sandbox.Name)
		g.store.claims = []supervise.Claim{legacy(controllerToken, "", "")}
		refusal(t, reap(g, loc), g, loc.Sandbox.Name)
	}
	t.Run("controller release", func(t *testing.T) {
		controllerGuard(t, func(g *rig, loc runtime.Locator) error {
			return g.r.Release(g.ctx, runtime.Known{Claim: controllerToken, Locator: &loc})
		})
	})
	t.Run("controller orphan sweep", func(t *testing.T) {
		controllerGuard(t, func(g *rig, _ runtime.Locator) error {
			return g.r.ReconcileOrphans(g.ctx, nil, 0)
		})
	})
}

// A controller reaping refusal must leave its process running: the operator
// can then suspend it, forget the lost session, and retry the release.
func TestControllerReapRefusalLeavesTheControllerRecoverable(t *testing.T) {
	g := newRig(t, nil)
	loc := g.spawn(controllerSpec(t))
	legacySessionTemplate(t, g, loc.Sandbox.Name)
	g.connectRunning(controllerToken, loc.Sandbox.Generation)
	g.store.claims = []supervise.Claim{{
		Token: controllerToken, Project: testProject, Role: claim.RoleController,
		Session: "ses_controller_file", SessionFile: controllerSession,
	}}

	err := g.r.Release(g.ctx, runtime.Known{Claim: controllerToken, Locator: &loc})

	if err == nil || !strings.Contains(err.Error(), "refuse to reap volume") {
		t.Fatalf("controller release = %v, want the session-volume refusal", err)
	}
	for _, frame := range g.sent(controllerToken) {
		if _, stopped := frame.(shimwire.LauncherStop); stopped {
			t.Fatalf("refused controller release sent stop %T, want its process left running", frame)
		}
	}

	if err := g.r.Suspend(g.ctx, loc); err != nil {
		t.Fatalf("suspend the refused controller: %v", err)
	}
	g.store.claims[0].Session, g.store.claims[0].SessionFile = "", ""
	if err := g.r.Release(g.ctx, runtime.Known{Claim: controllerToken}); err != nil {
		t.Fatalf("release the controller after its session was forgotten: %v", err)
	}
	if s := g.sandbox(loc.Sandbox.Name); s != nil {
		t.Fatalf("controller Sandbox after recovery = %+v, want it deleted", s)
	}
}
