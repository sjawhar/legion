package sandbox

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
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
// walks the pod store for the pods of this project (every launcher role holds its App's gh
// bundle, the controller's included; a repository cache's pod names no kind and holds
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
// render no longer holds, which the pod's projection may still name, stay as they are. Every live
// issue pod's provisioning Secret is brought to the implement App's current lease too
// (refreshProvisioningToken), whatever its workspace-fetch has already done: the kubelet runs a
// pod's init containers afresh whenever it rebuilds the pod's sandbox, at any point in the pod's
// life (a relaunch that replaces the pod provisions it anew instead), and the fetch re-reads the
// token's file before every transfer, so a pod left on its boot token, aged past the hour GitHub
// gives an installation token, could not fetch its repository again. The lease is minted once per
// repository owner a tick, however many of that owner's pods the walk holds, and an owner whose
// mint fails is logged once and left to the next tick, where its pods are tried again.
func (r *Runtime) refreshGitHubCredentials(ctx context.Context) {
	// mints memoizes this tick's provisioning leases by lower-cased owner — nothing here knows of a
	// configured repository rename, which the daemon's adapter maps, so Acme and acme are one call —
	// and records a mint that failed as well as one that worked, so an owner is asked at most once a
	// tick whatever the answer. The walk is one goroutine, so the map needs no lock.
	type mint struct {
		token string
		ok    bool
	}
	mints := map[string]mint{}
	provisioning := func(owner string) (string, bool) {
		key := strings.ToLower(owner)
		if m, asked := mints[key]; asked {
			return m.token, m.ok
		}
		minting, cancel := call(ctx)
		token, err := r.tokens.Token(minting, owner)
		cancel()
		if err != nil {
			r.log.Warn("sandbox runtime: provisioning token mint failed", "owner", owner, "err", err)
		}
		mints[key] = mint{token: token, ok: err == nil}
		return token, err == nil
	}
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
		if _, issue := kind.(issuePod); !issue {
			continue
		}
		repo, named := fetchRepository(pod)
		if !named {
			continue
		}
		token, ok := provisioning(repo.Owner())
		if !ok {
			continue
		}
		r.refreshProvisioningToken(ctx, pod, owner.UID, repo, token)
	}
}

// refreshProvisioningToken writes token — the implement App's current lease for the owner of repo,
// the repository the fetch of pod names (fetchRepository), which the walk minted once for that
// owner this tick — into the provisioning Secret of pod, owned by the Sandbox with uid. The fetch
// reads the token's file anew before every transfer from GitHub (workspace.Fetch), and the kubelet
// rewrites the projected file within its sync period, so a fetch that starts long after the pod
// did, one the kubelet runs again after it rebuilds the pod's sandbox included, never presents an
// expired token. Only the token is written; the cache's token beside it stays. A Secret another
// Sandbox owns, or one that already holds the lease, is left; a write that fails is logged and
// tried at the next tick, and a write that conflicts is retried once on a fresh read.
func (r *Runtime) refreshProvisioningToken(ctx context.Context, pod *corev1.Pod, uid types.UID, repo ghrepo.Repository, token string) {
	name := secretName(pod.Name)
	failed := func(err error) {
		r.log.Warn("sandbox runtime: provisioning token refresh failed", "sandbox", pod.Name, "repo", repo.String(), "err", err)
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
