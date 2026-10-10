package sandbox

import (
	"context"
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
// installation token's hour, at one Secret read per live role.
const gitHubRefreshInterval = time.Minute

// refreshGitHubCredentialsEvery runs refreshGitHubCredentials every interval until ctx ends: the
// informers' lifetime (start), so the walk never reads a pod store nothing feeds.
func (r *Runtime) refreshGitHubCredentialsEvery(ctx context.Context, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			r.refreshGitHubCredentials(ctx)
		}
	}
}

// refreshGitHubCredentials brings the role Secrets of every live pod to the current render of each
// role's GitHub credential, so the gh files its container projects (GHConfigDir) carry the token
// the daemon holds now: the kubelet rewrites the projection in place once the Secret changes. It
// walks the pod store for the pods of this project (every launcher role's container holds its
// App's gh files, the controller's included) that are Pending or Running and not being deleted —
// a pod waiting to be scheduled starts its launchers from the Secret as it is then — and, for each
// of the kind's roles, reads the role's Secret, skips one another Sandbox owns (the pod's Sandbox
// replaced since the pod was listed), renders the credential, and leaves a Secret whose hosts.yml
// is already the render untouched. A render that fails is logged and the role keeps its last token
// until the next tick; a write that conflicts is retried once on a fresh read. Nothing of the
// Secret but the two gh keys changes: the launcher token, whose hash binds the launcher
// (credentialFromSecret), and the annotations stay as they are.
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
		reading, cancel := call(ctx)
		defer cancel()
		return secrets.Get(reading, name, metav1.GetOptions{})
	}
	secret, err := read()
	if err != nil {
		failed(err)
		return
	}
	if !ownedBySandbox(secret.OwnerReferences, uid) {
		return
	}
	minting, cancel := call(ctx)
	rendered, err := r.gitHubCredential(minting, role)
	cancel()
	if err != nil {
		failed(err)
		return
	}
	if string(secret.Data[GitHubHostsKey]) == rendered.Hosts {
		return
	}
	for retried := false; ; retried = true {
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data[GitHubHostsKey], secret.Data[GitHubConfigKey] = []byte(rendered.Hosts), []byte(rendered.Config)
		updating, cancel := call(ctx)
		_, err = secrets.Update(updating, secret, metav1.UpdateOptions{})
		cancel()
		if err == nil {
			break
		}
		if !apierrors.IsConflict(err) || retried {
			failed(err)
			return
		}
		// Something else wrote the Secret since it was read — a launch binding it to its new pod
		// (bindLauncherSecrets) — so its write is kept and the two keys set over a fresh read.
		if secret, err = read(); err != nil {
			failed(err)
			return
		}
		if !ownedBySandbox(secret.OwnerReferences, uid) {
			return
		}
	}
	r.log.Info("sandbox runtime: github credential refreshed", "sandbox", sandbox, "role", role, "app", rendered.App, "expiresAt", rendered.ExpiresAt)
}
