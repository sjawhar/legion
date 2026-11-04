package sandbox

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

const (
	sessionPodName = "k8s-session-ada"
	sessionPodUID  = "uid-k8s-session-ada"
)

// sessionPodObject is an operator's session pod: no legion.dev/project label, and no Sandbox over
// it. podObject's owner reference is set only when ownerUID is given, so this pod has none.
func sessionPodObject(name, uid string, status corev1.PodStatus) *corev1.Pod {
	return podObject(name, uid, "", nil, status)
}

// sessionBundle is a gh bundle for owner as the session routes mint one: the implement App's
// write token for owner, its read token for acme.
func sessionBundle(t *testing.T, owner string) ghconfig.Bundle {
	t.Helper()
	bundle, err := staticCredential(t.Context(), appauth.SessionRole, owner)
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

// A session pod is one the operator launched in the daemon's namespace with no project label: it
// resolves to its uid. A name the namespace does not hold is ErrNoSuchPod, and one of Legion's
// own pods is ErrPodNotSession, so no session route can be aimed at a claim's pod.
func TestSessionPodResolvesOnlyAPodThatIsNoClaimsOwn(t *testing.T) {
	g := newRig(t, []k8sruntime.Object{
		sessionPodObject(sessionPodName, sessionPodUID, runningStatus()),
		podObject("legion-legion-legion-208", "uid-legion-pod", "", map[string]string{labelProject: testProject}, runningStatus()),
		podObject("legion-widgets-widgets-1", "uid-other-project", "", map[string]string{labelProject: "widgets"}, runningStatus()),
	})

	uid, err := g.r.SessionPod(g.ctx, sessionPodName)
	if err != nil || uid != sessionPodUID {
		t.Fatalf("SessionPod(%s) = %q, %v, want %s", sessionPodName, uid, err, sessionPodUID)
	}
	for name, want := range map[string]error{
		"k8s-session-gone":         runtime.ErrNoSuchPod,
		"legion-legion-legion-208": runtime.ErrPodNotSession,
		"legion-widgets-widgets-1": runtime.ErrPodNotSession,
	} {
		if _, err := g.r.SessionPod(g.ctx, name); !errors.Is(err, want) {
			t.Errorf("SessionPod(%s) = %v, want %v", name, err, want)
		}
	}
}

// The gh Secret a session pod gets carries the same keys a role Secret does — every owner's
// hosts.yml and config.yml and the default owner — and is owned by the pod, so Kubernetes deletes
// it with the pod and no session credential outlives the session. A second call rewrites it in
// place rather than making a second Secret.
func TestWriteSessionGitHubCredentialWritesASecretThePodOwns(t *testing.T) {
	g := newRig(t, []k8sruntime.Object{sessionPodObject(sessionPodName, sessionPodUID, runningStatus())})
	bundle := sessionBundle(t, "sjawhar")

	name, err := g.r.WriteSessionGitHubCredential(g.ctx, sessionPodName, sessionPodUID, bundle)
	if err != nil || name != sessionPodName+"-gh" {
		t.Fatalf("WriteSessionGitHubCredential = %q, %v, want %s-gh", name, err, sessionPodName)
	}
	secret := g.secret(name)
	if secret == nil {
		t.Fatalf("no Secret %s was written", name)
	}
	want := []string{GitHubConfigKey("acme"), GitHubConfigKey("sjawhar"), GitHubDefaultKey, GitHubHostsKey("acme"), GitHubHostsKey("sjawhar")}
	if got := slices.Sorted(maps.Keys(secret.Data)); !slices.Equal(got, want) {
		t.Fatalf("Secret holds %v, want %v", got, want)
	}
	if string(secret.Data[GitHubDefaultKey]) != "sjawhar" {
		t.Errorf("default owner = %q, want sjawhar", secret.Data[GitHubDefaultKey])
	}
	if token, err := ghconfig.TokenFromHosts(secret.Data[GitHubHostsKey("sjawhar")]); err != nil ||
		token != staticCredentialToken(appauth.SessionRole) {
		t.Errorf("the default owner's token = %q (%v), want the implement App's write token", token, err)
	}
	if token, err := ghconfig.TokenFromHosts(secret.Data[GitHubHostsKey("acme")]); err != nil ||
		token != staticReadToken(appauth.SessionRole, "acme") {
		t.Errorf("the other owner's token = %q (%v), want the implement App's read token", token, err)
	}
	owners := secret.OwnerReferences
	if len(owners) != 1 || owners[0].Kind != "Pod" || owners[0].Name != sessionPodName || owners[0].UID != types.UID(sessionPodUID) {
		t.Fatalf("owner references = %+v, want the session pod alone", owners)
	}
	if secret.Labels[labelSessionPod] != sessionPodName || secret.Labels[labelProject] != testProject {
		t.Errorf("labels = %v, want the pod and the project", secret.Labels)
	}

	// A second call for the same pod rewrites the one Secret.
	if _, err := g.r.WriteSessionGitHubCredential(g.ctx, sessionPodName, sessionPodUID, bundle); err != nil {
		t.Fatalf("a second write: %v", err)
	}
	if secret := g.secret(name); len(secret.OwnerReferences) != 1 {
		t.Fatalf("owner references after a second write = %+v", secret.OwnerReferences)
	}
}

// A Secret of the name that a pod of the same name before this one owns is refused, never taken
// over: its bytes belong to the pod that still projects them.
func TestWriteSessionGitHubCredentialRefusesASecretAnotherPodOwns(t *testing.T) {
	g := newRig(t, []k8sruntime.Object{sessionPodObject(sessionPodName, sessionPodUID, runningStatus())})
	bundle := sessionBundle(t, "sjawhar")
	if _, err := g.r.WriteSessionGitHubCredential(g.ctx, sessionPodName, "uid-the-pod-before", bundle); err != nil {
		t.Fatal(err)
	}

	_, err := g.r.WriteSessionGitHubCredential(g.ctx, sessionPodName, sessionPodUID, bundle)
	if err == nil || !strings.Contains(err.Error(), "is not owned by session pod") {
		t.Fatalf("the second write = %v, want it refused by ownership", err)
	}
	if token, _ := ghconfig.TokenFromHosts(g.secret(sessionPodName + "-gh").Data[GitHubHostsKey("sjawhar")]); token == "" {
		t.Fatal("the refused write emptied the other pod's Secret")
	}
}

// A bundle with no default owner names no directory for GH_CONFIG_DIR, so nothing is written.
func TestWriteSessionGitHubCredentialRefusesABundleWithNoDefault(t *testing.T) {
	g := newRig(t, []k8sruntime.Object{sessionPodObject(sessionPodName, sessionPodUID, runningStatus())})
	if _, err := g.r.WriteSessionGitHubCredential(g.ctx, sessionPodName, sessionPodUID, ghconfig.Bundle{}); err == nil {
		t.Fatal("an empty bundle was written")
	}
	if g.secret(sessionPodName+"-gh") != nil {
		t.Fatal("an empty bundle wrote a Secret")
	}
}

// The minute tick rewrites a session pod's gh Secret exactly as it rewrites a role's: only the
// owners whose hosts.yml is stale, and nothing else of the Secret.
func TestTheRefresherRewritesAStaleSessionBundle(t *testing.T) {
	g := newRig(t, []k8sruntime.Object{sessionPodObject(sessionPodName, sessionPodUID, runningStatus())})
	name, err := g.r.WriteSessionGitHubCredential(g.ctx, sessionPodName, sessionPodUID, sessionBundle(t, "sjawhar"))
	if err != nil {
		t.Fatal(err)
	}
	editSecret(g, name, func(s *corev1.Secret) { s.Data[GitHubHostsKey("acme")] = []byte(ghconfig.Hosts("ghs_stale_lease")) })
	before := g.secret(name).DeepCopy()
	g.clearActions()

	g.r.refreshSessionCredentials(g.ctx)

	if updated := secretUpdates(g.writes()); !slices.Equal(updated, []string{name}) {
		t.Fatalf("the refresher updated %v, want %s alone", updated, name)
	}
	after := g.secret(name)
	if token, err := ghconfig.TokenFromHosts(after.Data[GitHubHostsKey("acme")]); err != nil ||
		token != staticReadToken(appauth.SessionRole, "acme") {
		t.Errorf("acme's hosts.yml holds %q (%v), want the current read token", token, err)
	}
	for _, key := range []string{GitHubDefaultKey, GitHubHostsKey("sjawhar"), GitHubConfigKey("sjawhar")} {
		if !slices.Equal(after.Data[key], before.Data[key]) {
			t.Errorf("the refresher changed %s", key)
		}
	}

	// A second tick with nothing stale writes nothing.
	g.clearActions()
	g.r.refreshSessionCredentials(g.ctx)
	if updated := secretUpdates(g.writes()); len(updated) != 0 {
		t.Fatalf("a tick with nothing stale updated %v", updated)
	}
}

// A session stops being refreshed the moment its pod is gone, has ended, is being deleted, or has
// been replaced by another pod of the same name: each is dropped from the registry, so the next
// tick reads neither its pod nor its Secret. The Secret itself needs no deleting — the pod owns
// it, and Kubernetes takes it away with the pod.
func TestTheRefresherStopsOnceTheSessionPodIsGone(t *testing.T) {
	for label, gone := range map[string]func(g *rig){
		"deleted": func(g *rig) { deletePod(g, sessionPodName) },
		"ended": func(g *rig) {
			replacePod(g, sessionPodObject(sessionPodName, sessionPodUID, corev1.PodStatus{Phase: corev1.PodSucceeded}))
		},
		"being deleted": func(g *rig) {
			pod := sessionPodObject(sessionPodName, sessionPodUID, runningStatus())
			now := metav1.Now()
			pod.DeletionTimestamp = &now
			pod.Finalizers = []string{"legion.dev/test"}
			replacePod(g, pod)
		},
		"replaced": func(g *rig) {
			replacePod(g, sessionPodObject(sessionPodName, "uid-the-next-pod-of-that-name", runningStatus()))
		},
	} {
		t.Run(label, func(t *testing.T) {
			logs := &lockedLog{}
			g := newRig(t, []k8sruntime.Object{sessionPodObject(sessionPodName, sessionPodUID, runningStatus())},
				withOptions(func(o *Options) {
					o.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
				}))
			name, err := g.r.WriteSessionGitHubCredential(g.ctx, sessionPodName, sessionPodUID, sessionBundle(t, "sjawhar"))
			if err != nil {
				t.Fatal(err)
			}
			editSecret(g, name, func(s *corev1.Secret) { s.Data[GitHubHostsKey("acme")] = []byte(ghconfig.Hosts("ghs_stale_lease")) })
			gone(g)

			g.r.refreshSessionCredentials(g.ctx)

			if updated := secretUpdates(g.writes()); slices.Contains(updated, name) {
				t.Fatalf("the refresher rewrote %s for a pod that is %s", name, label)
			}
			if held := g.r.registeredSessions(); len(held) != 0 {
				t.Fatalf("registry = %v, want the session forgotten", held)
			}
			if !strings.Contains(logs.String(), "session no longer served") {
				t.Fatalf("logs do not say the session stopped being served: %s", logs.String())
			}
			g.clearActions()
			g.r.refreshSessionCredentials(g.ctx)
			if reads := g.writes(); len(reads) != 0 {
				t.Fatalf("a tick after the session was forgotten wrote %v", reads)
			}
		})
	}
}

// A read that merely failed forgets nothing: the pod may well be there, and the next tick asks
// again. Here the Secret is gone while the pod is not, which is the one read whose absence does
// end the refresh — nothing is left to rewrite.
func TestTheRefresherStopsOnceTheSessionSecretIsGone(t *testing.T) {
	g := newRig(t, []k8sruntime.Object{sessionPodObject(sessionPodName, sessionPodUID, runningStatus())})
	name, err := g.r.WriteSessionGitHubCredential(g.ctx, sessionPodName, sessionPodUID, sessionBundle(t, "sjawhar"))
	if err != nil {
		t.Fatal(err)
	}
	if err := g.kube.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("secrets"), testNamespace, name); err != nil {
		t.Fatal(err)
	}

	g.r.refreshSessionCredentials(g.ctx)

	if held := g.r.registeredSessions(); len(held) != 0 {
		t.Fatalf("registry = %v, want the session forgotten once its Secret is gone", held)
	}
}

// The refresher mints the same App for a session as the route that wrote its Secret did: one name
// decides both (appauth.SessionRole), so a session never finds the other App's token in its
// directory after a tick.
func TestTheSessionRefreshMintsTheSameAppAsTheRoute(t *testing.T) {
	if appauth.AppRoleFor(appauth.SessionRole) != appauth.Implement {
		t.Fatalf("a session's App = %s, want implement: a session writes to the owner it names",
			appauth.AppRoleFor(appauth.SessionRole))
	}
	if appauth.SessionRole == claim.RoleController {
		t.Fatal("a session's role is the controller's, which writes to no owner")
	}
}

// recordingRevoker is Options.RevokeEnrollment: it records each enrollment it was asked to end,
// and fails every call while err is set.
type recordingRevoker struct {
	err  error
	mu   sync.Mutex
	ends []string
}

func (r *recordingRevoker) revoke(_ context.Context, enrollment string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.ends = append(r.ends, enrollment)
	return nil
}

func (r *recordingRevoker) revoked() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.ends)
}

// A session pod's broker enrollment is revoked once the pod is gone, as a claim's is when its
// process is let go: the broker proves a renewal with the pod's projected token alone and never
// asks whether the pod still exists, so an enrollment nobody revokes would outlive its pod. A pod
// that asked for an enrollment and no gh bundle is swept all the same.
func TestTheSweepRevokesAGoneSessionPodsEnrollment(t *testing.T) {
	revoker := &recordingRevoker{}
	g := newRig(t, []k8sruntime.Object{sessionPodObject(sessionPodName, sessionPodUID, runningStatus())},
		withOptions(func(o *Options) { o.RevokeEnrollment = revoker.revoke }))
	g.r.RecordSessionEnrollment(sessionPodName, sessionPodUID, "enr-ada")

	g.r.refreshSessionCredentials(g.ctx)
	if ended := revoker.revoked(); len(ended) != 0 {
		t.Fatalf("revoked %v while the pod runs, want none", ended)
	}

	deletePod(g, sessionPodName)
	g.r.refreshSessionCredentials(g.ctx)

	if ended := revoker.revoked(); !slices.Equal(ended, []string{"enr-ada"}) {
		t.Fatalf("revoked %v, want enr-ada once its pod is gone", ended)
	}
	if held := g.r.registeredSessions(); len(held) != 0 {
		t.Fatalf("registry = %v, want the session forgotten once it is revoked", held)
	}
}

// A revocation that fails keeps the registration, so the next tick asks the broker again rather
// than leaving an enrollment nobody ends.
func TestASessionEnrollmentIsRevokedAgainAfterAFailedRevocation(t *testing.T) {
	revoker := &recordingRevoker{err: errors.New("the broker is out of reach")}
	g := newRig(t, []k8sruntime.Object{sessionPodObject(sessionPodName, sessionPodUID, runningStatus())},
		withOptions(func(o *Options) { o.RevokeEnrollment = revoker.revoke }))
	g.r.RecordSessionEnrollment(sessionPodName, sessionPodUID, "enr-ada")
	deletePod(g, sessionPodName)

	g.r.refreshSessionCredentials(g.ctx)
	if held := g.r.registeredSessions(); len(held) != 1 {
		t.Fatalf("registry = %v, want the session kept for another attempt", held)
	}

	revoker.mu.Lock()
	revoker.err = nil
	revoker.mu.Unlock()
	g.r.refreshSessionCredentials(g.ctx)
	if ended := revoker.revoked(); !slices.Equal(ended, []string{"enr-ada"}) {
		t.Fatalf("revoked %v, want enr-ada on the retry", ended)
	}
	if held := g.r.registeredSessions(); len(held) != 0 {
		t.Fatalf("registry = %v, want the session forgotten", held)
	}
}

// The route checks the owner against the project's before it mints, and so does every refresh:
// a project whose mapping loses its last repository under that owner stops minting write tokens
// for it rather than keeping one session on them for as long as it runs.
func TestTheSweepStopsRefreshingAnOwnerTheProjectNoLongerServes(t *testing.T) {
	logs := &lockedLog{}
	g := newRig(t, []k8sruntime.Object{sessionPodObject(sessionPodName, sessionPodUID, runningStatus())},
		withOptions(func(o *Options) {
			o.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
		}))
	name, err := g.r.WriteSessionGitHubCredential(g.ctx, sessionPodName, sessionPodUID, sessionBundle(t, "sjawhar"))
	if err != nil {
		t.Fatal(err)
	}
	editSecret(g, name, func(s *corev1.Secret) { s.Data[GitHubHostsKey("acme")] = []byte(ghconfig.Hosts("ghs_stale_lease")) })
	g.r.gitHubOwners = func() []string { return []string{"acme"} }
	g.clearActions()

	g.r.refreshSessionCredentials(g.ctx)

	if updated := secretUpdates(g.writes()); len(updated) != 0 {
		t.Fatalf("the sweep updated %v for an owner the project no longer serves", updated)
	}
	if held := g.r.registeredSessions(); len(held) != 0 {
		t.Fatalf("registry = %v, want the gh registration dropped", held)
	}
	if !strings.Contains(logs.String(), "its owner is no longer one of the project's") {
		t.Fatalf("logs do not name the owner that left the project: %s", logs.String())
	}
}

// A pod labelled with the empty project is Legion's own too: the label is read by its presence,
// so an empty value is no way past the refusal.
func TestAPodLabelledWithTheEmptyProjectIsNoSessionPod(t *testing.T) {
	g := newRig(t, []k8sruntime.Object{
		podObject(sessionPodName, sessionPodUID, "", map[string]string{labelProject: ""}, runningStatus()),
	})
	if _, err := g.r.SessionPod(g.ctx, sessionPodName); !errors.Is(err, runtime.ErrPodNotSession) {
		t.Fatalf("SessionPod of a pod labelled with the empty project = %v, want %v", err, runtime.ErrPodNotSession)
	}
}

// A pod whose name would make a Secret name longer than Kubernetes allows is refused by name and
// by the limit, rather than failing in the API server with the write.
func TestWriteSessionGitHubCredentialRefusesAPodNameTooLongForASecret(t *testing.T) {
	long := strings.Repeat("a", maxSecretName)
	g := newRig(t, []k8sruntime.Object{sessionPodObject(long, "uid-long", runningStatus())})
	_, err := g.r.WriteSessionGitHubCredential(g.ctx, long, "uid-long", sessionBundle(t, "sjawhar"))
	if err == nil || !strings.Contains(err.Error(), "longer than a Secret name's") {
		t.Fatalf("the write = %v, want it refused by the Secret name's length", err)
	}
}

// A pod name past a label value's 63 characters still gets its Secret: the pod's name is a label
// for an operator to find it by, bounded like every other (labelValue), and the owner reference
// is what binds the Secret to the pod.
func TestASessionSecretsPodLabelIsBounded(t *testing.T) {
	long := strings.Repeat("a", 32) + "." + strings.Repeat("b", 32)
	g := newRig(t, []k8sruntime.Object{sessionPodObject(long, "uid-long-label", runningStatus())})
	name, err := g.r.WriteSessionGitHubCredential(g.ctx, long, "uid-long-label", sessionBundle(t, "sjawhar"))
	if err != nil {
		t.Fatalf("the write of a 65-character pod name: %v", err)
	}
	label := g.secret(name).Labels[labelSessionPod]
	if label != labelValue(long) || len(label) > 63 {
		t.Fatalf("label = %q (%d characters), want labelValue's bounded form", label, len(label))
	}
}

// deletePod removes the pod named name from the tracker.
func deletePod(g *rig, name string) {
	g.t.Helper()
	if err := g.kube.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), testNamespace, name); err != nil {
		g.t.Fatal(err)
	}
}

// replacePod writes pod over whatever the tracker holds under its name.
func replacePod(g *rig, pod *corev1.Pod) {
	g.t.Helper()
	if err := g.kube.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), pod, testNamespace); err != nil {
		g.t.Fatal(err)
	}
}
