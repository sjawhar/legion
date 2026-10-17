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

// The refresher brings a live pod's role Secrets to the current render of each role's credential
// for the default owner its Secret records, and touches nothing else, the controller's included: a
// Secret whose owners' hosts.yml are already the render is not written, one another Sandbox owns is
// skipped, one that records no default is logged and left, and a rewrite changes the stale owner's
// two gh keys alone — the other owner's, the default, the launcher token and the annotations the
// binding wrote stay — and is logged naming the sandbox, the role, its App, the lease's expiry and
// the owner it rewrote.
func TestTheRefresherRewritesOnlyTheOwnersWhoseGhFilesAreStale(t *testing.T) {
	logs := &lockedLog{}
	g := newRig(t, nil, withOptions(func(o *Options) {
		o.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}))
	g.spawn(rootSpec(t))
	g.spawn(controllerSpec(t))
	issue, controller := SandboxName(rootToken), SandboxName(controllerToken)
	stale, foreign := roleSecretName(issue, claim.RoleTester), roleSecretName(issue, claim.RoleReviewer)
	unrecorded := roleSecretName(issue, claim.RolePlanner)
	editSecret(g, stale, func(s *corev1.Secret) { s.Data[GitHubHostsKey("acme")] = []byte(ghconfig.Hosts("ghs_stale_lease")) })
	editSecret(g, foreign, func(s *corev1.Secret) {
		s.Data[GitHubHostsKey("acme")] = []byte(ghconfig.Hosts("ghs_stale_lease"))
		s.OwnerReferences[0].UID = "uid-another-sandbox"
	})
	editSecret(g, unrecorded, func(s *corev1.Secret) {
		s.Data[GitHubHostsKey("acme")] = []byte(ghconfig.Hosts("ghs_stale_lease"))
		delete(s.Data, GitHubDefaultKey)
	})
	before := g.secret(stale).DeepCopy()
	g.clearActions()

	g.r.refreshGitHubCredentials(g.ctx)

	if updated := secretUpdates(g.writes()); !slices.Equal(updated, []string{stale}) {
		t.Fatalf("the refresher updated %v, want %s alone", updated, stale)
	}
	after := g.secret(stale)
	if token, err := ghconfig.TokenFromHosts(after.Data[GitHubHostsKey("acme")]); err != nil || token != staticReadToken(claim.RoleTester, "acme") {
		t.Errorf("the tester's acme hosts.yml holds %q (%v), want %q", token, err, staticReadToken(claim.RoleTester, "acme"))
	}
	if string(after.Data[GitHubConfigKey("acme")]) != ghconfig.Config {
		t.Errorf("the tester's acme config.yml is %q, want %q", after.Data[GitHubConfigKey("acme")], ghconfig.Config)
	}
	for _, key := range []string{LauncherTokenFile, GitHubDefaultKey, GitHubHostsKey("sjawhar"), GitHubConfigKey("sjawhar")} {
		if !slices.Equal(after.Data[key], before.Data[key]) {
			t.Errorf("the refresher changed the tester's %s", key)
		}
	}
	if len(after.Data) != 6 {
		t.Errorf("the tester's Secret holds %v, want the launcher token, the default and two owners' two gh files", slices.Sorted(maps.Keys(after.Data)))
	}
	if !maps.Equal(after.Annotations, before.Annotations) || before.Annotations[launcherPodUIDAnnotation] == "" || before.Annotations[annotationIssue] != testTree {
		t.Errorf("the refresher left annotations %v, want the binding's %v kept", after.Annotations, before.Annotations)
	}
	if kept := g.secret(foreign); string(kept.Data[GitHubHostsKey("acme")]) != ghconfig.Hosts("ghs_stale_lease") {
		t.Errorf("the refresher rewrote %s, which another Sandbox owns", foreign)
	}
	if kept := g.secret(unrecorded); string(kept.Data[GitHubHostsKey("acme")]) != ghconfig.Hosts("ghs_stale_lease") {
		t.Errorf("the refresher rewrote %s, which records no default owner", unrecorded)
	}
	if secret := g.secret(roleSecretName(controller, claim.RoleController)); len(secret.Data) != 6 || string(secret.Data[GitHubDefaultKey]) != "sjawhar" {
		t.Errorf("the controller's Secret holds %v, want the launcher token, the default sjawhar and the review App's gh files of both owners",
			slices.Sorted(maps.Keys(secret.Data)))
	}
	var refreshed, failed []string
	for _, line := range strings.Split(logs.String(), "\n") {
		switch {
		case strings.Contains(line, "github credential refreshed"):
			refreshed = append(refreshed, line)
		case strings.Contains(line, "github credential refresh failed"):
			failed = append(failed, line)
		}
	}
	if len(refreshed) != 1 {
		t.Fatalf("the runtime logged %d refreshed credentials %q, want the tester's alone", len(refreshed), refreshed)
	}
	for _, want := range []string{"sandbox=" + issue, "role=" + string(claim.RoleTester), "app=" + string(appauth.Review), "expiresAt=2026-10-08T13:00:00", "owners=[acme]"} {
		if !strings.Contains(refreshed[0], want) {
			t.Errorf("the runtime logged %q, want %q in it", refreshed[0], want)
		}
	}
	if len(failed) != 1 || !strings.Contains(failed[0], "role="+string(claim.RolePlanner)) || !strings.Contains(failed[0], GitHubDefaultKey) {
		t.Errorf("the runtime logged failures %q, want the planner's alone, naming %s", failed, GitHubDefaultKey)
	}
}

// A lease the daemon re-minted reaches every role of every live pod on the next tick, the
// controller's included: each role's Secret is rewritten to its own App's new tokens, every owner's.
// A role whose render fails keeps its last files, logged, while its siblings are refreshed, and so
// does one whose render names another default owner than its Secret records.
func TestTheRefresherFollowsAReMintedLeaseIntoEveryRolesSecret(t *testing.T) {
	var lease atomic.Pointer[string]
	first, second := "first", "second"
	lease.Store(&first)
	var refuse atomic.Bool
	logs := &lockedLog{}
	g := newRig(t, nil, withOptions(func(o *Options) {
		o.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
		o.GitHubCredential = func(_ context.Context, role claim.Role, owner string) (ghconfig.Bundle, error) {
			if refuse.Load() {
				switch role {
				case claim.RoleMerger:
					return ghconfig.Bundle{}, errors.New("no installation token: GitHub is down")
				case claim.RolePlanner:
					owner = "acme"
				}
			}
			if owner == "" {
				owner = "sjawhar"
			}
			app := string(appauth.AppRoleFor(role))
			return ghconfig.Bundle{Default: owner, Owners: map[string]ghconfig.Rendered{
				"acme": ghconfig.Render("ghs_"+app+"_"+*lease.Load()+"_read", app, staticCredentialExpiry),
				owner:  ghconfig.Render("ghs_"+app+"_"+*lease.Load(), app, staticCredentialExpiry),
			}}, nil
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
		if role != claim.RoleMerger && role != claim.RolePlanner {
			want = append(want, roleSecretName(issue, role))
		}
	}
	want = append(want, roleSecretName(SandboxName(controllerToken), claim.RoleController))
	if updated := secretUpdates(g.writes()); !slices.Equal(slices.Sorted(slices.Values(updated)), slices.Sorted(slices.Values(want))) {
		t.Fatalf("the refresher updated %v, want every live role's Secret but the merger's and the planner's: %v", updated, want)
	}
	for _, role := range claim.Roles {
		secret := g.secret(roleSecretName(issue, role))
		lease := second
		if role == claim.RoleMerger || role == claim.RolePlanner {
			lease = first
		}
		app := string(appauth.AppRoleFor(role))
		for owner, expected := range map[string]string{"sjawhar": "ghs_" + app + "_" + lease, "acme": "ghs_" + app + "_" + lease + "_read"} {
			if token, err := ghconfig.TokenFromHosts(secret.Data[GitHubHostsKey(owner)]); err != nil || token != expected {
				t.Errorf("%s's %s hosts.yml holds %q (%v), want %q", role, owner, token, err, expected)
			}
		}
	}
	secret := g.secret(roleSecretName(SandboxName(controllerToken), claim.RoleController))
	if token, err := ghconfig.TokenFromHosts(secret.Data[GitHubHostsKey("sjawhar")]); err != nil || token != "ghs_review_second" {
		t.Errorf("the controller's hosts.yml holds %q (%v), want %q", token, err, "ghs_review_second")
	}
	logged := logs.String()
	if !strings.Contains(logged, "github credential refresh failed") || !strings.Contains(logged, "role="+string(claim.RoleMerger)) {
		t.Errorf("the runtime logged %q, want the merger's failed refresh named", logged)
	}
	if !strings.Contains(logged, "default owner acme is not the Secret's sjawhar") {
		t.Errorf("the runtime logged %q, want the planner's render refused for its default", logged)
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
	editSecret(g, stale, func(s *corev1.Secret) { s.Data[GitHubHostsKey("sjawhar")] = []byte(ghconfig.Hosts("ghs_stale_lease")) })
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
	if token, err := ghconfig.TokenFromHosts(after.Data[GitHubHostsKey("sjawhar")]); err != nil || token != staticCredentialToken(claim.RoleArchitect) {
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
			editSecret(g, stale, func(s *corev1.Secret) { s.Data[GitHubHostsKey("sjawhar")] = []byte(ghconfig.Hosts("ghs_stale_lease")) })
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

// leaseTokens is a provisioning token source whose lease the test re-mints: each token is the
// lease's name and the owner's.
type leaseTokens struct{ lease atomic.Pointer[string] }

func (l *leaseTokens) Token(_ context.Context, owner string) (string, error) {
	return "ghs_" + *l.lease.Load() + "_" + owner, nil
}

// A re-minted lease reaches the provisioning Secret of every issue pod whose workspace-fetch has yet
// to finish, on the same tick as the role Secrets: the fetch reads the token's file anew before
// every transfer from GitHub, so one that waits long on its repository's cache presents the
// current lease. Only the token is rewritten; the cache's bearer token beside it stays. A pod whose
// fetch has finished keeps its Secret as it is: nothing in it reads the token again.
func TestTheRefresherKeepsAFetchingPodsProvisioningTokenFresh(t *testing.T) {
	tokens := &leaseTokens{}
	first, second := "first", "second"
	tokens.lease.Store(&first)
	g := newRig(t, nil, withOptions(func(o *Options) { o.Tokens = tokens }))
	other, err := claim.NewToken(testProject, "LEGION-210", claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	g.spawn(rootSpec(t))
	g.spawn(testSpec(t, other, claim.RoleArchitect, "LEGION-210"))
	inFetch, pastFetch := SandboxName(rootToken), SandboxName(other)
	owner := rootSpec(t).Repository.Owner()
	for _, name := range []string{inFetch, pastFetch} {
		if got := string(g.secret(secretName(name)).Data[provisionTokenKey]); got != "ghs_first_"+owner {
			t.Fatalf("%s's provisioning Secret holds %q at launch, want the first lease's", name, got)
		}
	}
	bearer := string(g.secret(secretName(inFetch)).Data[cacheTokenKey])
	g.update(g.pod(inFetch), fetching)
	g.update(g.pod(pastFetch), func(p *corev1.Pod) {
		p.Status = corev1.PodStatus{Phase: corev1.PodPending, InitContainerStatuses: []corev1.ContainerStatus{
			{Name: fetchContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed"}}},
			{Name: initContainer, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		}}
	})
	g.eventually("the store to hold both pods Pending", func() bool {
		for _, name := range []string{inFetch, pastFetch} {
			if pod := g.r.storedPod(name); pod == nil || pod.Status.Phase != corev1.PodPending {
				return false
			}
		}
		return true
	})
	tokens.lease.Store(&second)
	g.clearActions()

	g.r.refreshGitHubCredentials(g.ctx)

	if updated := secretUpdates(g.writes()); !slices.Equal(updated, []string{secretName(inFetch)}) {
		t.Fatalf("the refresher updated %v, want %s alone", updated, secretName(inFetch))
	}
	refreshed := g.secret(secretName(inFetch))
	if got := string(refreshed.Data[provisionTokenKey]); got != "ghs_second_"+owner || string(refreshed.Data[cacheTokenKey]) != bearer || len(refreshed.Data) != 2 {
		t.Errorf("the fetching pod's provisioning Secret holds %q and the cache token %t, want the second lease's and the cache's token kept", got, string(refreshed.Data[cacheTokenKey]) == bearer)
	}
	if got := string(g.secret(secretName(pastFetch)).Data[provisionTokenKey]); got != "ghs_first_"+owner {
		t.Errorf("the pod past its fetch holds %q, want its Secret left as it was", got)
	}
}
