package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
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
	for _, want := range []string{string(rootToken), "ses_unimported", IssueClaimName(workerToken), "legion sessions import", "legion claims forget-session"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("replacement refusal %q does not name %q", err, want)
		}
	}
	if s := g.sandbox(SandboxName(workerToken)); s == nil || s.UID != "uid-sandbox-superseded" {
		t.Fatalf("Sandbox after refused replacement = %+v, want the legacy Sandbox kept", s)
	}
}

func TestReplacementReapsOnlyDatabaseBackedOrEmptySessions(t *testing.T) {
	for _, tc := range []struct {
		name      string
		sql       bool
		claimsErr error
		claim     supervise.Claim
	}{
		{
			name:      "the retiring Sandbox uses the session database",
			sql:       true,
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
			if tc.sql {
				s := g.sandbox(SandboxName(workerToken))
				s.Spec.PodTemplate.Spec.Containers = append(s.Spec.PodTemplate.Spec.Containers, corev1.Container{Name: "roles", Env: []corev1.EnvVar{{Name: "OMP_SESSION_STORAGE", Value: "sql"}}})
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
