package sandbox

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
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
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
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

// linesHolding are the lines of logged that hold text.
func linesHolding(logged, text string) []string {
	var held []string
	for _, line := range strings.Split(logged, "\n") {
		if strings.Contains(line, text) {
			held = append(held, line)
		}
	}
	return held
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
	for _, want := range []string{"sandbox=" + issue, "role=" + string(claim.RoleTester), "app=" + string(appauth.Review), "expiresAt=2026-10-08T13:00:00", "dirs=[acme]"} {
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
		o.GitHubCredential = func(_ context.Context, role claim.Role, repo ghrepo.Repository) (ghconfig.Bundle, error) {
			owner := strings.ToLower(repo.Owner())
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
			return ghconfig.Bundle{Default: owner, Dirs: map[string]ghconfig.Rendered{
				"acme": ghconfig.Render("ghs_"+app+"_"+*lease.Load()+"_read", app, staticCredentialExpiry),
				owner:  ghconfig.Render("ghs_"+app+"_"+*lease.Load(), app, staticCredentialExpiry),
			}}, nil
		}
		// The controller's bundle comes from the one function that answers by owner, over the same
		// lease: its launch asks for no owner and its refresh for the one its Secret records.
		o.ControllerCredential = func(ctx context.Context, defaultOwner string) (ghconfig.Bundle, error) {
			bundle, err := o.GitHubCredential(ctx, claim.RoleController, ghrepo.Repository{})
			if err != nil || defaultOwner == "" {
				return bundle, err
			}
			bundle.Default = defaultOwner
			return bundle, nil
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

// A controller's bundle is asked for by owner at its launch and at every refresh
// (Options.ControllerCredential), so a project that gains an owner sorting before the one its
// Secret records still refreshes it: the controller works no repository, and a bundle asked for
// by repository takes its default from the first owner the project serves, which would be
// another owner than the one the pod's GH_CONFIG_DIR was fixed at and so refused at every tick
// until something replaced the pod, its tokens expiring within the hour.
func TestAControllerIsRefreshedForTheOwnerItsSecretRecords(t *testing.T) {
	var gained atomic.Bool
	logs := &lockedLog{}
	app := string(appauth.AppRoleFor(claim.RoleController))
	// The project served sjawhar alone when the controller's pod was launched, and has since
	// gained acme, which sorts before it.
	bundle := func(dflt string) ghconfig.Bundle {
		rendered := ghconfig.Bundle{Default: dflt, Dirs: map[string]ghconfig.Rendered{
			"sjawhar": ghconfig.Render("ghs_review_read_sjawhar", app, staticCredentialExpiry),
		}}
		if gained.Load() {
			rendered.Dirs["acme"] = ghconfig.Render("ghs_review_read_acme", app, staticCredentialExpiry)
		}
		return rendered
	}
	first := func() string {
		if gained.Load() {
			return "acme"
		}
		return "sjawhar"
	}
	g := newRig(t, nil, withOptions(func(o *Options) {
		o.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
		o.GitHubCredential = func(_ context.Context, _ claim.Role, repo ghrepo.Repository) (ghconfig.Bundle, error) {
			owner := strings.ToLower(repo.Owner())
			if owner == "" {
				owner = first()
			}
			return bundle(owner), nil
		}
		o.ControllerCredential = func(_ context.Context, defaultOwner string) (ghconfig.Bundle, error) {
			if defaultOwner == "" {
				defaultOwner = first()
			}
			return bundle(defaultOwner), nil
		}
	}))
	g.spawn(controllerSpec(t))
	name := roleSecretName(SandboxName(controllerToken), claim.RoleController)
	if recorded := string(g.secret(name).Data[GitHubDefaultKey]); recorded != "sjawhar" {
		t.Fatalf("the controller's Secret records the default %q, want the one owner its launch served, sjawhar", recorded)
	}
	gained.Store(true)
	if asked, _ := g.r.gitHubCredential(g.ctx, claim.RoleController, ghrepo.Repository{}); asked.Default != "acme" {
		t.Fatalf("a bundle asked for by repository defaults to %q, want the project's new first owner acme, which the Secret does not record", asked.Default)
	}
	g.clearActions()

	g.r.refreshGitHubCredentials(g.ctx)

	if updated := secretUpdates(g.writes()); !slices.Equal(updated, []string{name}) {
		t.Fatalf("the refresher updated %v, want the controller's Secret %s", updated, name)
	}
	secret := g.secret(name)
	if string(secret.Data[GitHubDefaultKey]) != "sjawhar" {
		t.Errorf("the refresher wrote the default %q, want the recorded sjawhar untouched", secret.Data[GitHubDefaultKey])
	}
	if token, err := ghconfig.TokenFromHosts(secret.Data[GitHubHostsKey("acme")]); err != nil || token != "ghs_review_read_acme" {
		t.Errorf("the controller's acme hosts.yml holds %q (%v), want the new owner's read token", token, err)
	}
	if logged := logs.String(); strings.Contains(logged, "github credential refresh failed") {
		t.Errorf("the runtime logged %q, want the controller refreshed rather than refused", logged)
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
// lease's name and the owner's. Every mint is counted under the lower-cased repository the
// walk's own memo keys by, so a test reads what a tick cost, and the repository refuse names is
// refused, so a test reads what a tick does when GitHub mints nothing.
type leaseTokens struct {
	lease  atomic.Pointer[string]
	refuse atomic.Pointer[string]
	mu     sync.Mutex
	mints  map[string]int
}

func (l *leaseTokens) Token(_ context.Context, repo ghrepo.Repository) (string, error) {
	l.mu.Lock()
	if l.mints == nil {
		l.mints = map[string]int{}
	}
	l.mints[strings.ToLower(repo.String())]++
	l.mu.Unlock()
	if refused := l.refuse.Load(); refused != nil && strings.EqualFold(*refused, repo.String()) {
		return "", errors.New("the implement App's installation does not reach " + repo.String())
	}
	return "ghs_" + *l.lease.Load() + "_" + repo.Owner(), nil
}

// minted is how often repo's token has been asked for.
func (l *leaseTokens) minted(repo ghrepo.Repository) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.mints[strings.ToLower(repo.String())]
}

// fetched is a pod's status once workspace-fetch has exited successfully: Pending still, with
// workspace-init running, which is a pod past its fetch.
func fetched(p *corev1.Pod) {
	p.Status = corev1.PodStatus{Phase: corev1.PodPending, InitContainerStatuses: []corev1.ContainerStatus{
		{Name: fetchContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed"}}},
		{Name: initContainer, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
	}}
}

// spawnIssue launches the architect of the issue named key on g — an issue of the test tree with a
// pod of its own — and returns that pod's name.
func spawnIssue(t *testing.T, g *rig, key string) string {
	t.Helper()
	token, err := claim.NewToken(testProject, key, claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	g.spawn(testSpec(t, token, claim.RoleArchitect, key))
	return SandboxName(token)
}

// provisioned is the token the provisioning Secret of the pod named name holds.
func provisioned(g *rig, name string) string {
	g.t.Helper()
	secret := g.secret(secretName(name))
	if secret == nil {
		g.t.Fatalf("no provisioning Secret %s", secretName(name))
	}
	return string(secret.Data[provisionTokenKey])
}

// refreshTick is the refresher's tick over the Secrets: the walk of the pod store and then the
// repository caches, the pair refreshGitHubCredentialsEvery runs, since a cache's Secret is the
// second call's.
func refreshTick(g *rig) {
	g.r.refreshGitHubCredentials(g.ctx)
	g.r.refreshCacheTokens(g.ctx)
}

// A re-minted lease reaches the provisioning Secret of every live issue pod on one tick, whatever
// its workspace-fetch has already done — a pod's init containers can run again at any point in its
// life, and the fetch reads the token's file anew before every transfer from GitHub, so a boot
// token aged past the hour GitHub gives one would leave the pod unable to fetch its repository
// again — and the repository cache's Secret follows on the same tick. Only the token is rewritten;
// the cache's bearer token beside it stays. A pod no launcher runs in any more, one the walk can
// judge nothing by, and a Secret another Sandbox owns are left as they are. The lease is minted
// once for a repository however many of its pods the walk holds, and a repository whose mint
// fails is logged once and left to the next tick.
func TestTheRefresherKeepsEveryLivePodsProvisioningTokenFresh(t *testing.T) {
	t.Run("every live pod and the repository cache follow the lease", func(t *testing.T) {
		tokens := &leaseTokens{}
		first, second := "first", "second"
		tokens.lease.Store(&first)
		logs := &logBuffer{}
		g := newRig(t, nil, withLog(logs), withOptions(func(o *Options) { o.Tokens = tokens }))
		repo := rootSpec(t).Repository
		owner, cache := repo.Owner(), CacheSandboxName(testProject, repo)
		running := spawnIssue(t, g, "LEGION-301")
		pastFetch := spawnIssue(t, g, "LEGION-302")
		inFetch := spawnIssue(t, g, "LEGION-303")
		deleting := spawnIssue(t, g, "LEGION-304")
		succeeded := spawnIssue(t, g, "LEGION-305")
		failed := spawnIssue(t, g, "LEGION-306")
		unlabelled := spawnIssue(t, g, "LEGION-307")
		unowned := spawnIssue(t, g, "LEGION-308")
		unparsed := spawnIssue(t, g, "LEGION-309")
		foreign := spawnIssue(t, g, "LEGION-310")
		live := []string{running, pastFetch, inFetch}
		// skipped are the pods whose provisioning Secret the walk never reads; left is every pod
		// whose Secret stays as it was, which is those and the one another Sandbox owns; minting
		// is every pod whose Secret the walk reads.
		skipped := []string{deleting, succeeded, failed, unlabelled, unowned, unparsed}
		left := append(slices.Clone(skipped), foreign)
		minting := append(slices.Clone(live), foreign)
		for _, name := range append(slices.Clone(live), left...) {
			if got := provisioned(g, name); got != "ghs_first_"+owner {
				t.Fatalf("%s's provisioning Secret holds %q at launch, want the first lease's", name, got)
			}
		}
		bearer := string(g.secret(secretName(inFetch)).Data[cacheTokenKey])
		g.update(g.pod(pastFetch), fetched)
		g.update(g.pod(inFetch), fetching)
		g.update(g.pod(deleting), func(p *corev1.Pod) { now := metav1.Now(); p.DeletionTimestamp = &now })
		g.update(g.pod(succeeded), func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded })
		g.update(g.pod(failed), func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed })
		g.update(g.pod(unlabelled), func(p *corev1.Pod) { delete(p.Labels, labelIssue) })
		g.update(g.pod(unowned), func(p *corev1.Pod) { p.OwnerReferences = nil })
		g.update(g.pod(unparsed), func(p *corev1.Pod) {
			for i, c := range p.Spec.InitContainers {
				if at := slices.Index(c.Command, "--repo"); c.Name == fetchContainer && at >= 0 {
					p.Spec.InitContainers[i].Command[at+1] = repo.Name()
				}
			}
		})
		editSecret(g, secretName(foreign), func(s *corev1.Secret) { s.OwnerReferences[0].UID = "uid-another-sandbox" })
		g.eventually("the store to hold every pod as the test left it", func() bool {
			for name, held := range map[string]func(*corev1.Pod) bool{
				pastFetch:  func(p *corev1.Pod) bool { return p.Status.Phase == corev1.PodPending },
				inFetch:    func(p *corev1.Pod) bool { return p.Status.Phase == corev1.PodPending },
				deleting:   func(p *corev1.Pod) bool { return p.DeletionTimestamp != nil },
				succeeded:  func(p *corev1.Pod) bool { return p.Status.Phase == corev1.PodSucceeded },
				failed:     func(p *corev1.Pod) bool { return p.Status.Phase == corev1.PodFailed },
				unlabelled: func(p *corev1.Pod) bool { _, err := podKindOf(p.Labels); return err != nil },
				unowned:    func(p *corev1.Pod) bool { return metav1.GetControllerOf(p) == nil },
				unparsed:   func(p *corev1.Pod) bool { _, named := fetchRepository(p); return !named },
			} {
				if pod := g.r.storedPod(name); pod == nil || !held(pod) {
					return false
				}
			}
			return true
		})
		tokens.lease.Store(&second)
		g.clearActions()

		refreshTick(g)

		want := []string{secretName(running), secretName(pastFetch), secretName(inFetch), cacheSecretName(cache)}
		if updated := secretUpdates(g.writes()); !slices.Equal(slices.Sorted(slices.Values(updated)), slices.Sorted(slices.Values(want))) {
			t.Fatalf("the tick updated %v, want every live pod's provisioning Secret and the repository cache's: %v", updated, want)
		}
		for _, name := range live {
			secret := g.secret(secretName(name))
			if got := string(secret.Data[provisionTokenKey]); got != "ghs_second_"+owner || string(secret.Data[cacheTokenKey]) != bearer || len(secret.Data) != 2 {
				t.Errorf("%s's provisioning Secret holds %q beside the cache token %q, want the second lease's and the bearer token %q kept",
					name, got, secret.Data[cacheTokenKey], bearer)
			}
		}
		if got := string(g.secret(cacheSecretName(cache)).Data[provisionTokenKey]); got != "ghs_second_"+owner {
			t.Errorf("the repository cache's Secret holds %q, want the second lease's", got)
		}
		for _, name := range left {
			if got := provisioned(g, name); got != "ghs_first_"+owner {
				t.Errorf("%s's provisioning Secret holds %q, want it left as it was", name, got)
			}
		}
		logged := logs.String()
		if n := strings.Count(logged, "sandbox runtime: provisioning token refreshed"); n != len(live) {
			t.Errorf("the tick logged %d provisioning refreshes, want one for each of %v: %s", n, live, logged)
		}
		for _, quiet := range []string{"sandbox runtime: provisioning token mint failed", "sandbox runtime: provisioning token refresh failed"} {
			if strings.Contains(logged, quiet) {
				t.Errorf("the tick logged %q, want none: %s", quiet, logged)
			}
		}
		for _, line := range strings.Split(logged, "\n") {
			if !strings.Contains(line, "provisioning token") {
				continue
			}
			for _, quiet := range []string{unparsed, foreign} {
				if strings.Contains(line, quiet) {
					t.Errorf("the tick logged %q of %s, want it passed over in silence", line, quiet)
				}
			}
		}

		// The same tick again with the lease where it is: the walk mints once for the repository
		// however many of its pods it holds — the mint refreshCacheTokens makes is the repository
		// cache's own (writeCacheSecret) — reads each live pod's provisioning Secret once, and
		// writes nothing at all.
		g.clearActions()
		minted := tokens.minted(repo)
		g.r.refreshGitHubCredentials(g.ctx)
		if got := tokens.minted(repo) - minted; got != 1 {
			t.Errorf("the walk minted %s's token %d times over the %d pods whose Secret it reads, want once", repo, got, len(minting))
		}
		g.r.refreshCacheTokens(g.ctx)
		if updated := secretUpdates(g.writes()); len(updated) != 0 {
			t.Errorf("the tick updated %v while every Secret held the lease", updated)
		}
		reads := map[string]int{}
		for _, request := range g.requests() {
			if request.verb == "get" && request.resource == "secrets" {
				reads[request.name]++
			}
		}
		for _, name := range minting {
			if reads[secretName(name)] != 1 {
				t.Errorf("the tick read %s's provisioning Secret %d times, want once", name, reads[secretName(name)])
			}
		}
		for _, name := range skipped {
			if reads[secretName(name)] != 0 {
				t.Errorf("the tick read %s's provisioning Secret %d times, want none", name, reads[secretName(name)])
			}
		}
	})

	t.Run("a repository whose mint fails is logged once and tried again", func(t *testing.T) {
		tokens := &leaseTokens{}
		first, second := "first", "second"
		tokens.lease.Store(&first)
		logs := &logBuffer{}
		g := newRig(t, nil, withLog(logs), withOptions(func(o *Options) { o.Tokens = tokens }))
		repo := rootSpec(t).Repository
		owner, cache := repo.Owner(), CacheSandboxName(testProject, repo)
		one, two := spawnIssue(t, g, "LEGION-311"), spawnIssue(t, g, "LEGION-312")
		refused := repo.String()
		tokens.lease.Store(&second)
		tokens.refuse.Store(&refused)
		g.clearActions()
		minted := tokens.minted(repo)

		g.r.refreshGitHubCredentials(g.ctx)

		if got := tokens.minted(repo) - minted; got != 1 {
			t.Errorf("the walk minted %s's token %d times over its two pods, want once", repo, got)
		}
		if updated := secretUpdates(g.writes()); len(updated) != 0 {
			t.Fatalf("the walk updated %v after the mint failed", updated)
		}
		for _, name := range []string{one, two} {
			if got := provisioned(g, name); got != "ghs_first_"+owner {
				t.Errorf("%s's provisioning Secret holds %q, want it left as it was", name, got)
			}
		}
		failures := linesHolding(logs.String(), "sandbox runtime: provisioning token mint failed")
		if len(failures) != 1 || !strings.Contains(failures[0], "repo="+repo.String()) || !strings.Contains(failures[0], "err=") || !strings.Contains(failures[0], "level=WARN") {
			t.Errorf("the walk logged %q, want one warning naming %s and the error", failures, repo)
		}

		tokens.refuse.Store(nil)
		g.clearActions()

		refreshTick(g)

		want := []string{secretName(one), secretName(two), cacheSecretName(cache)}
		if updated := secretUpdates(g.writes()); !slices.Equal(slices.Sorted(slices.Values(updated)), slices.Sorted(slices.Values(want))) {
			t.Fatalf("the next tick updated %v, want both pods' provisioning Secrets and the cache's %v", updated, want)
		}
		for _, name := range []string{one, two} {
			if got := provisioned(g, name); got != "ghs_second_"+owner {
				t.Errorf("%s's provisioning Secret holds %q after the mint worked again, want the second lease's", name, got)
			}
		}
	})

	t.Run("a Secret the API refuses leaves the tick's other pods written", func(t *testing.T) {
		tokens := &leaseTokens{}
		first, second := "first", "second"
		tokens.lease.Store(&first)
		logs := &logBuffer{}
		g := newRig(t, nil, withLog(logs), withOptions(func(o *Options) { o.Tokens = tokens }))
		owner := rootSpec(t).Repository.Owner()
		unreadable := spawnIssue(t, g, "LEGION-313")
		unwritable := spawnIssue(t, g, "LEGION-314")
		written := spawnIssue(t, g, "LEGION-315")
		g.kube.PrependReactor("get", "secrets", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
			if a.(k8stesting.GetAction).GetName() == secretName(unreadable) {
				return true, nil, errors.New("the API server is unreachable")
			}
			return false, nil, nil
		})
		g.kube.PrependReactor("update", "secrets", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
			if a.(k8stesting.UpdateAction).GetObject().(*corev1.Secret).Name == secretName(unwritable) {
				return true, nil, errors.New("the API server is busy")
			}
			return false, nil, nil
		})
		tokens.lease.Store(&second)
		g.clearActions()

		refreshTick(g)

		if got := provisioned(g, written); got != "ghs_second_"+owner {
			t.Errorf("the pod beside the two refused Secrets holds %q, want the second lease's", got)
		}
		for _, name := range []string{unreadable, unwritable} {
			if got := provisioned(g, name); got != "ghs_first_"+owner {
				t.Errorf("%s's provisioning Secret holds %q, want the refused write left it as it was", name, got)
			}
		}
		failures := linesHolding(logs.String(), "sandbox runtime: provisioning token refresh failed")
		if len(failures) != 2 {
			t.Fatalf("the tick logged %q, want one failure for each refused Secret", failures)
		}
		for _, name := range []string{unreadable, unwritable} {
			if !slices.ContainsFunc(failures, func(line string) bool { return strings.Contains(line, "sandbox="+name) }) {
				t.Errorf("the tick logged %q, want %s's refusal named", failures, name)
			}
		}
	})

	t.Run("a write that conflicts is retried once on a fresh read", func(t *testing.T) {
		tokens := &leaseTokens{}
		first, second := "first", "second"
		tokens.lease.Store(&first)
		g := newRig(t, nil, withOptions(func(o *Options) { o.Tokens = tokens }))
		repo := rootSpec(t).Repository
		owner, cache := repo.Owner(), CacheSandboxName(testProject, repo)
		pod := spawnIssue(t, g, "LEGION-316")
		var conflicted atomic.Bool
		g.kube.PrependReactor("update", "secrets", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
			secret := a.(k8stesting.UpdateAction).GetObject().(*corev1.Secret)
			if secret.Name == secretName(pod) && conflicted.CompareAndSwap(false, true) {
				editSecret(g, secretName(pod), func(s *corev1.Secret) { s.Data[cacheTokenKey] = []byte("written-between") })
				return true, nil, apierrors.NewConflict(corev1.Resource("secrets"), secretName(pod), errors.New("the object has been modified"))
			}
			return false, nil, nil
		})
		tokens.lease.Store(&second)
		g.clearActions()

		refreshTick(g)

		// The rig records a request once the conflict reactor has let it through, so the refused
		// write is the reactor's own evidence and the retry the one recorded update.
		if updated := secretUpdates(g.writes()); !conflicted.Load() || !slices.Equal(updated, []string{secretName(pod), cacheSecretName(cache)}) {
			t.Fatalf("the tick's recorded updates were %v after a conflict served: %t; want one retry of %s and the cache's",
				updated, conflicted.Load(), secretName(pod))
		}
		after := g.secret(secretName(pod))
		if got := string(after.Data[provisionTokenKey]); got != "ghs_second_"+owner {
			t.Errorf("the retried write left %q, want the second lease's", got)
		}
		if string(after.Data[cacheTokenKey]) != "written-between" {
			t.Errorf("the retry wrote over the cache token written between read and write: %q", after.Data[cacheTokenKey])
		}
	})
}

// recordedSmoke is the repository every test spec records (testSpec), and servingSmoke the one
// GitHub holds it under after the transfer: the pair a deployment configures over that move,
// and the only repository the project serves.
var (
	recordedSmoke = ghrepo.MustParse("sjawhar/legion-smoke")
	servingSmoke  = ghrepo.MustParse("cybermonklabs/legion-smoke")
	smokeRename   = ghrepo.MustRename("sjawhar/legion-smoke", "cybermonklabs/legion-smoke")
)

// namedLeases is the daemon's lease source as appauth.GitHubCredential asks it: each token names
// the App, the owner the lease was minted for and whether it writes, so a Secret's hosts.yml says
// which installation its token came from, and every mint is recorded, so a test reads which
// installations GitHub was asked about at all.
type namedLeases struct {
	mu     sync.Mutex
	minted []string
}

func (l *namedLeases) lease(role appauth.AppRole, owner, kind string) (appauth.Lease, error) {
	l.mu.Lock()
	l.minted = append(l.minted, owner+"'s "+kind+" lease")
	l.mu.Unlock()
	return appauth.Lease{Token: leaseNamed(role, owner, kind), ExpiresAt: staticCredentialExpiry}, nil
}

func (l *namedLeases) Token(_ context.Context, role appauth.AppRole, repo ghrepo.Repository) (appauth.Lease, error) {
	return l.lease(role, strings.ToLower(repo.Owner()), "write")
}

func (l *namedLeases) ReadToken(_ context.Context, role appauth.AppRole, owner string, _ []string) (appauth.Lease, error) {
	return l.lease(role, strings.ToLower(owner), "read")
}

// asked is every lease GitHub was asked for, deduplicated and sorted.
func (l *namedLeases) asked() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Compact(slices.Sorted(slices.Values(l.minted)))
}

// leaseNamed is the token namedLeases mints for an App, an owner and a kind of lease.
func leaseNamed(role appauth.AppRole, owner, kind string) string {
	return "ghs_" + string(role) + "_" + kind + "_" + owner
}

// An issue recorded on the repository as it was keeps writing the repository as GitHub holds it
// now across a refresh, pod after pod. The refresher reads the repository from the pod itself
// (fetchRepository, the --repo its workspace-fetch names) and asks the credential by it, so the
// configured pair is resolved for that one repository: the sjawhar directory the pod's
// GH_CONFIG_DIR names — fixed at its launch, which no transfer moves — holds the write lease
// minted for cybermonklabs, and so does cybermonklabs' own directory, the two of them holding
// that one lease. A refresh that asked by the owner its Secret records instead would mint
// sjawhar's own token and leave it there, which is every push of the issue's trees refused.
func TestTheRefresherKeepsARenamedIssuesWriteLeaseInItsOwnDirectory(t *testing.T) {
	leases := &namedLeases{}
	served := func() []ghrepo.Repository { return []ghrepo.Repository{servingSmoke} }
	renames := ghrepo.MustRenames(smokeRename)
	left := appauth.NewLeftOut()
	g := newRig(t, nil, withOptions(func(o *Options) {
		o.GitHubCredential = appauth.GitHubCredential(leases, served, renames, left, nil)
		o.ControllerCredential = appauth.ControllerCredential(leases, served, renames, left, nil)
		o.GitHubDirs = func(repo ghrepo.Repository, roles []appauth.AppRole) []string {
			return appauth.GitHubDirs(served, renames, left, roles, repo)
		}
	}))
	g.spawn(rootSpec(t))
	issue := SandboxName(rootToken)
	role := claim.RoleArchitect
	app := appauth.AppRoleFor(role)
	name := roleSecretName(issue, role)
	write := leaseNamed(app, "cybermonklabs", "write")
	if repo, named := fetchRepository(g.pod(issue)); !named || repo.String() != recordedSmoke.String() {
		t.Fatalf("the pod's fetch names %s (%t), want the repository the issue is recorded on, %s", repo, named, recordedSmoke)
	}

	// One tick after the launch changes nothing: the refresh renders the bundle the launch
	// wrote, so the lease minted for cybermonklabs stays in sjawhar's directory.
	g.clearActions()
	g.r.refreshGitHubCredentials(g.ctx)
	if updated := secretUpdates(g.writes()); len(updated) != 0 {
		t.Fatalf("the tick after the launch updated %v, want the launch's own render left as it is", updated)
	}
	if token, err := ghconfig.TokenFromHosts(g.secret(name).Data[GitHubHostsKey("sjawhar")]); err != nil || token != write {
		t.Fatalf("the sjawhar directory holds %q (%v) one tick after the launch, want %q", token, err, write)
	}

	// A pod created before this change holds, in that same sjawhar directory, the token the
	// owner rule minted: the sjawhar installation's own, which cannot push a repository that
	// installation no longer covers. The directory's name authorises nothing, so the tick
	// writes the lease of the owner serving the repository now over it, and nothing else of the
	// Secret moves — the other owner's files, the recorded default, the launcher token and the
	// annotations the binding wrote all stay.
	editSecret(g, name, func(s *corev1.Secret) {
		s.Data[GitHubHostsKey("sjawhar")] = []byte(ghconfig.Hosts(leaseNamed(app, "sjawhar", "write")))
	})
	before := g.secret(name).DeepCopy()
	g.clearActions()

	g.r.refreshGitHubCredentials(g.ctx)

	if updated := secretUpdates(g.writes()); !slices.Equal(updated, []string{name}) {
		t.Fatalf("the tick updated %v, want %s alone: the token the old owner rule left cannot push the repository", updated, name)
	}
	after := g.secret(name)
	for dir, want := range map[string]string{"sjawhar": write, "cybermonklabs": write} {
		if token, err := ghconfig.TokenFromHosts(after.Data[GitHubHostsKey(dir)]); err != nil || token != want {
			t.Errorf("the %s directory holds %q (%v), want %q", dir, token, err, want)
		}
	}
	for _, key := range []string{LauncherTokenFile, GitHubDefaultKey, GitHubHostsKey("cybermonklabs"), GitHubConfigKey("cybermonklabs")} {
		if !slices.Equal(after.Data[key], before.Data[key]) {
			t.Errorf("the refresh changed %s", key)
		}
	}
	if got := string(after.Data[GitHubDefaultKey]); got != "sjawhar" || len(after.Data) != 6 {
		t.Errorf("the Secret records the default %q and holds %v, want sjawhar still and both owners' two gh files beside the launcher token",
			got, slices.Sorted(maps.Keys(after.Data)))
	}
	if !maps.Equal(after.Annotations, before.Annotations) {
		t.Errorf("the refresh left annotations %v, want the binding's %v kept", after.Annotations, before.Annotations)
	}
	if asked := leases.asked(); !slices.Equal(asked, []string{"cybermonklabs's write lease"}) {
		t.Errorf("GitHub was asked for %v over the launch and both ticks, want cybermonklabs' write lease alone: the two directories hold it, and the installation the repository left is asked for nothing", asked)
	}
}
