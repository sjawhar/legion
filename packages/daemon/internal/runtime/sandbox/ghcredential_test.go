package sandbox

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
)

// secretUpdates are the Secret updates among writes, by name.
func secretUpdates(writes []action) []string {
	var names []string
	for _, w := range writes {
		if w.verb == "update" && w.resource == "secrets" {
			names = append(names, w.name)
		}
	}
	return names
}

// The refresher brings a live issue pod's role Secrets to the current render of each role's
// credential and touches nothing else: a Secret whose hosts.yml is already the render is not
// written, one another Sandbox owns is skipped, the controller's pod is never visited, and a
// rewrite changes the two gh keys alone — the launcher token and the annotations the binding
// wrote stay — and is logged naming the sandbox, the role, its App and the lease's expiry.
func TestTheRefresherRewritesOnlyTheRoleSecretsWhoseGhFilesAreStale(t *testing.T) {
	logs := &lockedLog{}
	g := newRig(t, nil, withOptions(func(o *Options) {
		o.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}))
	g.spawn(rootSpec(t))
	g.spawn(controllerSpec(t))
	issue, controller := SandboxName(rootToken), SandboxName(controllerToken)
	stale, foreign := roleSecretName(issue, claim.RoleTester), roleSecretName(issue, claim.RoleReviewer)
	editSecret(g, stale, func(s *corev1.Secret) { s.Data[GitHubHostsKey] = []byte(ghconfig.Hosts("ghs_stale_lease")) })
	editSecret(g, foreign, func(s *corev1.Secret) {
		s.Data[GitHubHostsKey] = []byte(ghconfig.Hosts("ghs_stale_lease"))
		s.OwnerReferences[0].UID = "uid-another-sandbox"
	})
	before := g.secret(stale).DeepCopy()
	g.clearActions()

	g.r.refreshGitHubCredentials(g.ctx)

	if updated := secretUpdates(g.writes()); !slices.Equal(updated, []string{stale}) {
		t.Fatalf("the refresher updated %v, want %s alone", updated, stale)
	}
	after := g.secret(stale)
	if token, err := ghconfig.TokenFromHosts(after.Data[GitHubHostsKey]); err != nil || token != staticCredentialToken(claim.RoleTester) {
		t.Errorf("the tester's hosts.yml holds %q (%v), want the review App's %q", token, err, staticCredentialToken(claim.RoleTester))
	}
	if string(after.Data[GitHubConfigKey]) != ghconfig.Config {
		t.Errorf("the tester's config.yml is %q, want %q", after.Data[GitHubConfigKey], ghconfig.Config)
	}
	if !slices.Equal(after.Data[LauncherTokenFile], before.Data[LauncherTokenFile]) || len(after.Data) != 3 {
		t.Errorf("the refresher changed the tester's launcher token or keys: %v, want %v with the gh keys rewritten",
			slices.Sorted(maps.Keys(after.Data)), slices.Sorted(maps.Keys(before.Data)))
	}
	if !maps.Equal(after.Annotations, before.Annotations) || before.Annotations[launcherPodUIDAnnotation] == "" || before.Annotations[annotationIssue] != testTree {
		t.Errorf("the refresher left annotations %v, want the binding's %v kept", after.Annotations, before.Annotations)
	}
	if kept := g.secret(foreign); string(kept.Data[GitHubHostsKey]) != ghconfig.Hosts("ghs_stale_lease") {
		t.Errorf("the refresher rewrote %s, which another Sandbox owns", foreign)
	}
	if secret := g.secret(roleSecretName(controller, claim.RoleController)); len(secret.Data) != 1 {
		t.Errorf("the controller's Secret holds %v, want the launcher token alone", slices.Sorted(maps.Keys(secret.Data)))
	}
	var refreshed []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "github credential refreshed") {
			refreshed = append(refreshed, line)
		}
	}
	if len(refreshed) != 1 {
		t.Fatalf("the runtime logged %d refreshed credentials %q, want the tester's alone", len(refreshed), refreshed)
	}
	for _, want := range []string{"sandbox=" + issue, "role=" + string(claim.RoleTester), "app=" + string(appauth.Review), "expiresAt=2026-10-08T13:00:00"} {
		if !strings.Contains(refreshed[0], want) {
			t.Errorf("the runtime logged %q, want %q in it", refreshed[0], want)
		}
	}
}

// A lease the daemon re-minted reaches every role of every live issue pod on the next tick: each
// role's Secret is rewritten to its own App's new token, the controller's never. A role whose
// render fails keeps its last files, logged, while its siblings are refreshed.
func TestTheRefresherFollowsAReMintedLeaseIntoEveryRolesSecret(t *testing.T) {
	var lease atomic.Pointer[string]
	first, second := "first", "second"
	lease.Store(&first)
	var refuse atomic.Bool
	logs := &lockedLog{}
	g := newRig(t, nil, withOptions(func(o *Options) {
		o.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
		o.GitHubCredential = func(_ context.Context, role claim.Role) (ghconfig.Rendered, error) {
			if role == claim.RoleMerger && refuse.Load() {
				return ghconfig.Rendered{}, errors.New("no installation token: GitHub is down")
			}
			app := appauth.AppRoleFor(role)
			return ghconfig.Render("ghs_"+string(app)+"_"+*lease.Load(), string(app), staticCredentialExpiry), nil
		}
	}))
	g.spawn(rootSpec(t))
	g.spawn(controllerSpec(t))
	issue := SandboxName(rootToken)
	g.clearActions()

	g.r.refreshGitHubCredentials(g.ctx)
	if updated := secretUpdates(g.writes()); len(updated) != 0 {
		t.Fatalf("the refresher updated %v while every Secret held the current render", updated)
	}

	lease.Store(&second)
	refuse.Store(true)
	g.r.refreshGitHubCredentials(g.ctx)
	var want []string
	for _, role := range claim.Roles {
		if role != claim.RoleMerger {
			want = append(want, roleSecretName(issue, role))
		}
	}
	if updated := secretUpdates(g.writes()); !slices.Equal(slices.Sorted(slices.Values(updated)), slices.Sorted(slices.Values(want))) {
		t.Fatalf("the refresher updated %v, want every issue role's Secret but the merger's: %v", updated, want)
	}
	for _, role := range claim.Roles {
		token, err := ghconfig.TokenFromHosts(g.secret(roleSecretName(issue, role)).Data[GitHubHostsKey])
		expected := "ghs_" + string(appauth.AppRoleFor(role)) + "_" + second
		if role == claim.RoleMerger {
			expected = "ghs_" + string(appauth.Implement) + "_" + first
		}
		if err != nil || token != expected {
			t.Errorf("%s's hosts.yml holds %q (%v), want %q", role, token, err, expected)
		}
	}
	if secret := g.secret(roleSecretName(SandboxName(controllerToken), claim.RoleController)); len(secret.Data) != 1 {
		t.Errorf("the controller's Secret holds %v, want the launcher token alone", slices.Sorted(maps.Keys(secret.Data)))
	}
	if logged := logs.String(); !strings.Contains(logged, "github credential refresh failed") || !strings.Contains(logged, "role="+string(claim.RoleMerger)) {
		t.Errorf("the runtime logged %q, want the merger's failed refresh named", logged)
	}
}

// A write the API server refuses as a conflict — a launch bound the Secret to its new pod between
// the refresher's read and its write — is retried once on a fresh read, so the binding's annotation
// and the new gh files both land.
func TestTheRefresherRetriesAConflictedWriteOnce(t *testing.T) {
	g := newRig(t, nil)
	g.spawn(rootSpec(t))
	issue := SandboxName(rootToken)
	stale := roleSecretName(issue, claim.RoleArchitect)
	editSecret(g, stale, func(s *corev1.Secret) { s.Data[GitHubHostsKey] = []byte(ghconfig.Hosts("ghs_stale_lease")) })
	var conflicted atomic.Bool
	g.kube.PrependReactor("update", "secrets", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		secret := a.(k8stesting.UpdateAction).GetObject().(*corev1.Secret)
		if secret.Name == stale && conflicted.CompareAndSwap(false, true) {
			editSecret(g, stale, func(s *corev1.Secret) { s.Annotations["legion.dev/test-bound"] = "between" })
			return true, nil, apierrors.NewConflict(corev1.Resource("secrets"), stale, errors.New("the object has been modified"))
		}
		return false, nil, nil
	})
	g.clearActions()

	g.r.refreshGitHubCredentials(g.ctx)

	// The rig records a request once the conflict reactor has let it through, so the refused write
	// is the reactor's own evidence and the retry the one recorded update.
	if updated := secretUpdates(g.writes()); !conflicted.Load() || !slices.Equal(updated, []string{stale}) {
		t.Fatalf("the refresher's recorded updates were %v after a conflict served: %t; want one retry of %s", updated, conflicted.Load(), stale)
	}
	after := g.secret(stale)
	if token, err := ghconfig.TokenFromHosts(after.Data[GitHubHostsKey]); err != nil || token != staticCredentialToken(claim.RoleArchitect) {
		t.Errorf("the architect's hosts.yml holds %q (%v), want %q", token, err, staticCredentialToken(claim.RoleArchitect))
	}
	if after.Annotations["legion.dev/test-bound"] != "between" {
		t.Errorf("the retry wrote over the annotation written between read and write: %v", after.Annotations)
	}
}

// A pod that is not live is not refreshed: one whose phase is past Running, or one being deleted,
// runs its launchers no more, so its Secrets are left as they are.
func TestTheRefresherLeavesAPodThatIsNotLiveAlone(t *testing.T) {
	for name, edit := range map[string]func(*corev1.Pod){
		"a succeeded pod":   func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded },
		"a failed pod":      func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed },
		"a terminating pod": func(p *corev1.Pod) { now := metav1.Now(); p.DeletionTimestamp = &now },
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, nil)
			g.spawn(rootSpec(t))
			issue := SandboxName(rootToken)
			stale := roleSecretName(issue, claim.RolePlanner)
			editSecret(g, stale, func(s *corev1.Secret) { s.Data[GitHubHostsKey] = []byte(ghconfig.Hosts("ghs_stale_lease")) })
			g.update(g.pod(issue), edit)
			g.eventually("the store to hold the pod as edited", func() bool {
				pod := g.r.storedPod(issue)
				return pod != nil && (pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil)
			})
			g.clearActions()
			g.r.refreshGitHubCredentials(g.ctx)
			if updated := secretUpdates(g.writes()); len(updated) != 0 {
				t.Fatalf("the refresher updated %v for %s", updated, name)
			}
		})
	}
}
