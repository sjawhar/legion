package sandbox

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// gitHubRefreshInterval is how often refreshGitHubCredentials walks the pod store. The daemon's
// function (Options.GitHubCredential) holds each App's lease and re-mints it as the lease nears its
// expiry, so most ticks render the hosts.yml the Secret already holds and write nothing; a minute
// keeps the time a role's gh runs on a token the daemon has already replaced short against an
// installation token's hour, at one Secret read per live role. A runtime reads it once, as it starts
// (start); a test that must see a tick inside its run shortens it before starting one.
var gitHubRefreshInterval = time.Minute

// refreshGitHubCredentialsEvery runs refreshGitHubCredentials, then refreshCacheTokens, then
// readCacheHealth, every interval until ctx ends: the informers' lifetime (start), so the walk never
// reads a store nothing feeds.
func (r *Runtime) refreshGitHubCredentialsEvery(ctx context.Context, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			r.refreshGitHubCredentials(ctx)
			r.refreshCacheTokens(ctx)
			r.readCacheHealth(ctx)
		}
	}
}

// refreshGitHubCredentials brings the role Secrets of every live pod to the current render of each
// role's GitHub credential, so the gh files its container projects (GHConfigDir) carry the tokens
// the daemon holds now: the kubelet rewrites the projection in place once the Secret changes. It
// walks the pod store for the pods of this project (every launcher role's container holds its
// App's gh bundle, the controller's included; a repository cache's pod names no kind and holds
// none, so it is passed over, its token refreshCacheTokens') that are Pending or Running and not
// being deleted —
// a pod waiting to be scheduled starts its launchers from the Secret as it is then — and, for each
// of the kind's roles, reads the role's Secret, skips one another Sandbox owns (the pod's Sandbox
// replaced since the pod was listed), renders the credential for the default owner the Secret
// records (GitHubDefaultKey), and rewrites only the owners whose hosts.yml is not the render. A
// Secret that records no default (one an older runtime wrote, whose pod the next launch replaces),
// a render that fails, or one whose default is not the Secret's is logged and the role keeps its
// last tokens until the next tick, and a Secret whose default is empty (a controller whose project
// had no GitHub owner when its pod started, whose gh directory is an empty one) has nothing to
// refresh until its next pod; a write that conflicts is retried once on a fresh read. Nothing
// of the Secret but the rewritten owners' gh keys changes: the launcher token, whose hash binds the
// launcher (credentialFromSecret), the default, the annotations, and the keys of an owner the
// render no longer holds, which the pod's projection may still name, stay as they are. An issue
// pod whose workspace-fetch has yet to finish has its provisioning Secret refreshed too
// (refreshProvisioningToken).
func (r *Runtime) refreshGitHubCredentials(ctx context.Context) {
	for _, obj := range r.pods.GetStore().List() {
		pod := obj.(*corev1.Pod)
		kind, err := podKindOf(pod.Labels)
		if err != nil {
			continue
		}
		if phase := pod.Status.Phase; (phase != corev1.PodPending && phase != corev1.PodRunning) || pod.DeletionTimestamp != nil {
			continue
		}
		owner := metav1.GetControllerOf(pod)
		if owner == nil {
			continue
		}
		for _, role := range kind.roles() {
			r.refreshGitHubCredential(ctx, pod.Name, owner.UID, role)
		}
		if _, issue := kind.(issuePod); issue && fetchUnfinished(pod) {
			r.refreshProvisioningToken(ctx, pod, owner.UID)
		}
	}
}

// fetchUnfinished is whether pod's workspace-fetch has yet to finish: the pod is Pending, since a
// Running pod's init containers have all finished, and its fetch container has not exited
// successfully — it is waiting to start, running, or about to run again after a failed attempt.
func fetchUnfinished(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodPending {
		return false
	}
	status := initContainerStatus(pod, fetchContainer)
	return status == nil || status.State.Terminated == nil || status.State.Terminated.ExitCode != 0
}

// refreshProvisioningToken brings the provisioning Secret of pod, owned by the Sandbox with uid, to
// the implement App's current lease for the owner of the repository its fetch names
// (fetchRepository): the fetch reads the token's file anew before every transfer from GitHub
// (workspace.Fetch), and the kubelet rewrites the projected file within its sync period, so a fetch
// that waits long on its repository's cache never presents an expired token. Only the token is
// written; the cache's token beside it stays. A Secret another Sandbox owns, or one that already
// holds the lease, is left; a mint or write that fails is logged and tried at the next tick, and a
// write that conflicts is retried once on a fresh read.
func (r *Runtime) refreshProvisioningToken(ctx context.Context, pod *corev1.Pod, uid types.UID) {
	repo, ok := fetchRepository(pod)
	if !ok {
		return
	}
	name := secretName(pod.Name)
	failed := func(err error) {
		r.log.Warn("sandbox runtime: provisioning token refresh failed", "sandbox", pod.Name, "repo", repo.String(), "err", err)
	}
	minting, cancel := call(ctx)
	token, err := r.tokens.Token(minting, repo.Owner())
	cancel()
	if err != nil {
		failed(err)
		return
	}
	secrets := r.kube.CoreV1().Secrets(r.namespace)
	for retried := false; ; retried = true {
		secret, err := secrets.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			failed(err)
			return
		}
		if !ownedBySandbox(secret.OwnerReferences, uid) || string(secret.Data[provisionTokenKey]) == token {
			return
		}
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data[provisionTokenKey] = []byte(token)
		_, err = secrets.Update(ctx, secret, metav1.UpdateOptions{})
		if err == nil {
			r.log.Info("sandbox runtime: provisioning token refreshed", "sandbox", pod.Name, "repo", repo.String())
			return
		}
		if !apierrors.IsConflict(err) || retried {
			failed(err)
			return
		}
	}
}

// refreshGitHubCredential is one role's refresh (refreshGitHubCredentials): the role's Secret in the
// pod named sandbox, owned by the Sandbox with uid.
func (r *Runtime) refreshGitHubCredential(ctx context.Context, sandbox string, uid types.UID, role claim.Role) {
	name := roleSecretName(sandbox, role)
	secrets := r.kube.CoreV1().Secrets(r.namespace)
	failed := func(err error) {
		r.log.Warn("sandbox runtime: github credential refresh failed", "sandbox", sandbox, "role", role, "err", err)
	}
	read := func() (*corev1.Secret, error) {
		return secrets.Get(ctx, name, metav1.GetOptions{})
	}
	secret, err := read()
	if err != nil {
		failed(err)
		return
	}
	if !ownedBySandbox(secret.OwnerReferences, uid) {
		return
	}
	recorded, ok := secret.Data[GitHubDefaultKey]
	if !ok {
		failed(fmt.Errorf("Secret %s records no %s", name, GitHubDefaultKey))
		return
	}
	owner := string(recorded)
	if owner == "" {
		return
	}
	minting, cancel := call(ctx)
	bundle, err := r.gitHubCredential(minting, role, owner)
	cancel()
	if err != nil {
		failed(err)
		return
	}
	if bundle.Default != owner {
		failed(fmt.Errorf("the credential's default owner %s is not the Secret's %s", bundle.Default, owner))
		return
	}
	var changed []string
	for _, o := range bundle.OwnerNames() {
		if string(secret.Data[GitHubHostsKey(o)]) != bundle.Owners[o].Hosts {
			changed = append(changed, o)
		}
	}
	if len(changed) == 0 {
		return
	}
	for retried := false; ; retried = true {
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		for _, o := range changed {
			rendered := bundle.Owners[o]
			secret.Data[GitHubHostsKey(o)], secret.Data[GitHubConfigKey(o)] = []byte(rendered.Hosts), []byte(rendered.Config)
		}
		_, err = secrets.Update(ctx, secret, metav1.UpdateOptions{})
		if err == nil {
			break
		}
		if !apierrors.IsConflict(err) || retried {
			failed(err)
			return
		}
		// Something else wrote the Secret since it was read — a launch binding it to its new pod
		// (bindLauncherSecrets) — so its write is kept and the changed owners' keys set over a
		// fresh read.
		if secret, err = read(); err != nil {
			failed(err)
			return
		}
		if !ownedBySandbox(secret.OwnerReferences, uid) {
			return
		}
	}
	written := bundle.Owners[owner]
	r.log.Info("sandbox runtime: github credential refreshed", "sandbox", sandbox, "role", role, "app", written.App,
		"expiresAt", written.ExpiresAt, "owners", changed)
}
