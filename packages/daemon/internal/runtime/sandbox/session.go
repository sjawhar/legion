package sandbox

import (
	"context"
	"fmt"
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A session pod is one an operator launches by hand in this runtime's namespace, outside every
// claim: it carries no legion.dev/project label, this runtime neither built nor supervises it, and
// nothing of the workflow knows it exists. What it gets here is what the daemon alone holds — the
// identity of a pod the daemon can resolve, a gh bundle written into a Secret of the pod's own,
// and the sweep that ends both once the pod is gone (api.SessionPods, internal/api/session.go).
// Nothing of it reaches a claim's pod: a pod that carries the project label is refused by name.

// labelSessionPod marks a Secret this runtime wrote for a session pod, with the pod's name as a
// label value (labelValue, which bounds it), so an operator can find it in the namespace. The
// sweep does not select on it: the Secret is owned by its pod, and the registry below is what it
// walks.
const labelSessionPod = "legion.dev/session-pod"

// sessionGHSuffix makes a session pod's gh Secret name out of the pod's: one Secret per pod, named
// after it so whatever launches the pod can name it in the pod's own volume before the Secret
// exists, and in none of Legion's own shapes (a claim's Secrets end `-boot` and a cache's
// `-token`). maxSecretName is a Secret name's length, a DNS subdomain's.
const (
	sessionGHSuffix = "-gh"
	maxSecretName   = 253
)

func sessionGHSecretName(pod string) string { return pod + sessionGHSuffix }

// sessionRegistration is one session pod as the sweep holds it: the pod uid everything of it is
// bound to, which also tells a pod of the same name apart from the one that was served; gh,
// whether this daemon wrote that pod a gh Secret it keeps fresh; and enrollment, the broker
// enrollment to revoke once the pod is gone, "" for a pod that asked for none.
//
// The gh Secret's owner is not here: the Secret records it (GitHubDefaultKey) and the refresh
// reads it from there, as a role's refresh reads a role Secret's, so the registry and the Secret
// can never disagree about whose token the pod holds.
type sessionRegistration struct {
	uid        types.UID
	gh         bool
	enrollment string
}

// SessionPod is the uid of the session pod named name in this runtime's namespace. The daemon
// resolves a session pod here and never takes a uid from a request: runtime.ErrNoSuchPod when the
// namespace holds no pod of that name, and runtime.ErrPodNotSession when the one it holds carries
// a legion.dev/project label — a claim's pod, the project controller's, a repository cache's, or
// another Legion project's in the same namespace — so no session route can be aimed at one. The
// label is read by its presence, not its value: a pod labelled with the empty project is Legion's
// own too.
func (r *Runtime) SessionPod(ctx context.Context, name string) (string, error) {
	read, cancel := call(ctx)
	defer cancel()
	pod, err := r.kube.CoreV1().Pods(r.namespace).Get(read, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return "", fmt.Errorf("pod %s in namespace %s: %w", name, r.namespace, runtime.ErrNoSuchPod)
	case err != nil:
		return "", fmt.Errorf("read pod %s in namespace %s: %w", name, r.namespace, err)
	}
	if _, labelled := pod.Labels[labelProject]; labelled {
		return "", fmt.Errorf("pod %s %w", name, runtime.ErrPodNotSession)
	}
	return string(pod.UID), nil
}

// RecordSessionEnrollment records the broker enrollment made for the session pod named name, so
// the sweep revokes it once the pod is gone, as a claim's machine revokes its process's when it
// lets the process go (internal/supervise's letGo). The broker proves a renewal with the pod's
// projected token alone and never asks whether the pod still exists, so an enrollment nobody
// revokes outlives its pod; this is what keeps a session's from outliving one any longer than a
// worker's does.
//
// It only writes memory, so it cannot fail. A second enrollment of the same pod replaces the id
// the sweep will revoke: the broker answers a re-enrollment of the same key with the live
// enrollment, and a fresh key's enrollment is the one to end.
func (r *Runtime) RecordSessionEnrollment(name, uid, enrollment string) {
	r.updateSession(name, types.UID(uid), func(held *sessionRegistration) { held.enrollment = enrollment })
}

// WriteSessionGitHubCredential writes bundle into the session pod's own gh Secret and registers
// that Secret with the minute sweep (refreshSessionCredentials), answering the Secret's name.
//
// The Secret is owned by the pod, so Kubernetes deletes it with the pod and no credential of a
// session outlives it; a second call for the same pod rewrites it in place, and one for a Secret
// another pod of the same name owns is refused rather than taken over. The pod projects it as its
// gh directories exactly as a role's container projects its role Secret: GitHubHostsKey(owner) as
// <owner>/hosts.yml, GitHubConfigKey(owner) as <owner>/config.yml, and GitHubDefaultKey naming the
// directory its GH_CONFIG_DIR points at.
//
// It runs under the pod's own session turn, which the refresh takes too, so a write that changes
// which owner the session writes to and a refresh computed against the owner before it can never
// interleave into a Secret holding two write tokens.
func (r *Runtime) WriteSessionGitHubCredential(ctx context.Context, name, uid string, bundle ghconfig.Bundle) (string, error) {
	if err := bundle.Validate(); err != nil {
		return "", err
	}
	secretName := sessionGHSecretName(name)
	if len(secretName) > maxSecretName {
		return "", fmt.Errorf("session pod %s: its gh Secret's name %s is longer than a Secret name's %d characters", name, secretName, maxSecretName)
	}
	podUID := types.UID(uid)
	write, cancel := call(ctx)
	defer cancel()
	done, err := r.takeTurn(write, r.sessionTurns, name)
	if err != nil {
		return "", err
	}
	defer done()
	want := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            secretName,
			Namespace:       r.namespace,
			Labels:          map[string]string{labelProject: r.project, labelSessionPod: labelValue(name)},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: name, UID: podUID}},
		},
		Data: sessionSecretData(bundle),
	}
	secrets := r.kube.CoreV1().Secrets(r.namespace)
	_, err = secrets.Create(write, want, metav1.CreateOptions{})
	switch {
	case err == nil:
		r.rememberSessionGH(name, podUID)
		r.log.Info("sandbox runtime: session github credential written", "pod", name, "secret", secretName,
			"default", bundle.Default, "owners", bundle.OwnerNames())
		return secretName, nil
	case !apierrors.IsAlreadyExists(err):
		return "", fmt.Errorf("create Secret %s for session pod %s: %w", secretName, name, err)
	}
	existing, err := secrets.Get(write, secretName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("read Secret %s for session pod %s: %w", secretName, name, err)
	}
	if !ownedBySandbox(existing.OwnerReferences, podUID) {
		return "", fmt.Errorf("Secret %s is not owned by session pod %s: a pod of that name was replaced, so delete it and ask again", secretName, name)
	}
	existing.Labels = want.Labels
	existing.Data = want.Data
	if _, err := secrets.Update(write, existing, metav1.UpdateOptions{}); err != nil {
		return "", fmt.Errorf("rewrite Secret %s for session pod %s: %w", secretName, name, err)
	}
	r.rememberSessionGH(name, podUID)
	r.log.Info("sandbox runtime: session github credential rewritten", "pod", name, "secret", secretName,
		"default", bundle.Default, "owners", bundle.OwnerNames())
	return secretName, nil
}

// sessionSecretData is bundle as the gh keys of a Secret, the same keys a role Secret carries:
// every owner's hosts.yml and config.yml, and the default owner.
func sessionSecretData(bundle ghconfig.Bundle) map[string][]byte {
	data := map[string][]byte{GitHubDefaultKey: []byte(bundle.Default)}
	for owner, rendered := range bundle.Owners {
		data[GitHubHostsKey(owner)] = []byte(rendered.Hosts)
		data[GitHubConfigKey(owner)] = []byte(rendered.Config)
	}
	return data
}

// rememberSessionGH records that name's gh Secret is this daemon's to keep fresh.
func (r *Runtime) rememberSessionGH(name string, uid types.UID) {
	r.updateSession(name, uid, func(held *sessionRegistration) { held.gh = true })
}

// updateSession applies edit to name's registration, starting a fresh one whenever the pod uid is
// not the one already held: a pod of the same name that replaced it carries nothing of its own.
func (r *Runtime) updateSession(name string, uid types.UID, edit func(*sessionRegistration)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessions == nil {
		r.sessions = map[string]sessionRegistration{}
	}
	held := r.sessions[name]
	if held.uid != uid {
		held = sessionRegistration{uid: uid}
	}
	edit(&held)
	r.sessions[name] = held
}

// forgetSession drops name's registration whole, leaving one another write has since replaced (a
// pod of the same name served again) alone.
func (r *Runtime) forgetSession(name string, uid types.UID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if held, ok := r.sessions[name]; ok && held.uid == uid {
		delete(r.sessions, name)
	}
}

// forgetSessionGH stops refreshing name's gh Secret and keeps the rest of its registration, so an
// enrollment still to be revoked is not forgotten with it. A registration with nothing left in it
// goes.
func (r *Runtime) forgetSessionGH(name string, uid types.UID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	held, ok := r.sessions[name]
	if !ok || held.uid != uid {
		return
	}
	held.gh = false
	if held.enrollment == "" {
		delete(r.sessions, name)
		return
	}
	r.sessions[name] = held
}

// registeredSessions is the session pods the sweep holds now, copied so the walk takes no lock
// while it calls the API server or the broker.
func (r *Runtime) registeredSessions() map[string]sessionRegistration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return maps.Clone(r.sessions)
}

// refreshSessionCredentials is the session pods' minute sweep, run beside the role Secrets'
// (refreshGitHubCredentialsEvery). Each pod is read first, and that read is where a session ends:
// a pod that is gone, that another pod of the same name has replaced, that is being deleted, or
// that has ended has its broker enrollment revoked and its registration dropped, and is never read
// again. The gh Secret needs no deleting — the pod owns it, so Kubernetes takes it away with the
// pod. A pod that is still there has its gh Secret brought to the current render of its bundle, so
// a session's gh runs on a token the daemon holds now rather than on one of an hour ago. A read
// that merely failed drops nothing and is tried again at the next tick.
//
// The registry lives for this daemon's life: a daemon that restarts refreshes and revokes nothing
// of a session it did not serve itself. That session's gh tokens then last until their hour is
// out, a fresh `legion session github-credential` registers it again, and its enrollment's lease
// ends when the pod's renewer stops with the pod.
func (r *Runtime) refreshSessionCredentials(ctx context.Context) {
	for name, held := range r.registeredSessions() {
		read, cancel := call(ctx)
		pod, err := r.kube.CoreV1().Pods(r.namespace).Get(read, name, metav1.GetOptions{})
		cancel()
		gone := ""
		switch {
		case apierrors.IsNotFound(err):
			gone = "its pod is gone"
		case err != nil:
			r.log.Warn("sandbox runtime: session pod read failed", "pod", name, "err", err)
			continue
		case pod.UID != held.uid:
			gone = "another pod of its name replaced it"
		case pod.DeletionTimestamp != nil:
			gone = "its pod is being deleted"
		case pod.Status.Phase != corev1.PodPending && pod.Status.Phase != corev1.PodRunning:
			gone = "its pod has ended"
		}
		if gone != "" {
			r.endSession(ctx, name, held, gone)
			continue
		}
		if held.gh {
			r.refreshSessionCredential(ctx, name, held)
		}
	}
}

// endSession is what a session pod's end costs the daemon: its broker enrollment revoked, and its
// registration dropped so nothing of it is read again. A revocation that fails keeps the
// registration, so the next tick tries again; the broker takes a revocation of an enrollment it no
// longer has as done, so a retry never sticks on one that is already gone.
func (r *Runtime) endSession(ctx context.Context, name string, held sessionRegistration, why string) {
	if held.enrollment != "" && r.revokeEnrollment != nil {
		revoke, cancel := call(ctx)
		err := r.revokeEnrollment(revoke, held.enrollment)
		cancel()
		if err != nil {
			r.log.Warn("sandbox runtime: revoking a session pod's broker enrollment failed; trying again at the next tick",
				"pod", name, "why", why, "err", err)
			return
		}
		r.log.Info("sandbox runtime: revoked a session pod's broker enrollment", "pod", name, "why", why)
	}
	r.forgetSession(name, held.uid)
	r.log.Info("sandbox runtime: session no longer served", "pod", name, "why", why)
}

// refreshSessionCredential is one session pod's gh refresh: its Secret, read and rewritten exactly
// as a role's is (refreshGitHubCredential), for the default owner the Secret itself records.
//
// It runs under the pod's session turn, so the Secret it reads is the one the write before it
// left. The owner is checked against the owners the daemon serves now (appauth.GitHubOwners, which
// is what Options.GitHubOwners answers): a project whose mapping loses its last repository under
// that owner refuses a fresh request for it, and this stops minting for it too, rather than
// keeping one session on write tokens for an owner the project no longer serves.
func (r *Runtime) refreshSessionCredential(ctx context.Context, name string, held sessionRegistration) {
	secretName := sessionGHSecretName(name)
	failed := func(err error) {
		r.log.Warn("sandbox runtime: session github credential refresh failed", "pod", name, "err", err)
	}
	turn, cancel := call(ctx)
	done, err := r.takeTurn(turn, r.sessionTurns, name)
	cancel()
	if err != nil {
		failed(err)
		return
	}
	defer done()
	secret, err := r.kube.CoreV1().Secrets(r.namespace).Get(ctx, secretName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		r.forgetSessionGH(name, held.uid)
		r.log.Info("sandbox runtime: session github credential no longer refreshed: its Secret is gone", "pod", name, "secret", secretName)
		return
	case err != nil:
		failed(err)
		return
	}
	if !ownedBySandbox(secret.OwnerReferences, held.uid) {
		r.forgetSessionGH(name, held.uid)
		return
	}
	owner := string(secret.Data[GitHubDefaultKey])
	if owner == "" {
		failed(fmt.Errorf("Secret %s records no %s", secretName, GitHubDefaultKey))
		return
	}
	if !slices.Contains(r.gitHubOwners(), owner) {
		r.forgetSessionGH(name, held.uid)
		r.log.Warn("sandbox runtime: session github credential no longer refreshed: its owner is no longer one of the project's",
			"pod", name, "secret", secretName, "owner", owner)
		return
	}
	minting, cancel := call(ctx)
	bundle, err := r.gitHubCredential(minting, appauth.SessionRole, owner)
	cancel()
	if err != nil {
		failed(err)
		return
	}
	if bundle.Default != owner {
		failed(fmt.Errorf("the credential's default owner %s is not the Secret's %s", bundle.Default, owner))
		return
	}
	changed := changedGitHubOwners(secret, bundle)
	if len(changed) == 0 {
		return
	}
	written, err := r.writeGitHubOwners(ctx, secretName, held.uid, secret, bundle, changed)
	switch {
	case err != nil:
		failed(err)
		return
	case !written:
		r.forgetSessionGH(name, held.uid)
		return
	}
	lease := bundle.Owners[owner]
	r.log.Info("sandbox runtime: session github credential refreshed", "pod", name, "secret", secretName,
		"app", lease.App, "expiresAt", lease.ExpiresAt, "owners", changed)
}
