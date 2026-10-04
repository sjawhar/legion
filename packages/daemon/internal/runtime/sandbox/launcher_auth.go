package sandbox

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"sync"

	"golang.org/x/sync/singleflight"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/stream"
)

type launcherCredential struct {
	sandboxUID    types.UID
	podUID, issue string
	hash          [sha256.Size]byte
}

type launcherCredentials struct {
	mu     sync.Mutex
	values map[string]launcherCredential
	loads  singleflight.Group
}

// LauncherResolver rejects unknown pods from informer state before consulting credentials.
// Tokens minted here are cached as hashes; after restart a known role's first read is single-flight,
// so unauthenticated hellos cannot turn into an unbounded stream of Kubernetes requests.
func (r *Runtime) LauncherResolver() stream.LauncherResolver {
	return func(hello shimwire.LauncherHello) (stream.LauncherHandler, string) {
		role := claim.Role(hello.Role)
		if !claim.IsRole(role) {
			return nil, "launcher role is not a Legion role"
		}
		view, err := r.view(hello.Sandbox)
		if err != nil || view.sandbox == nil || view.pod == nil || string(view.pod.UID) != hello.PodUID || view.sandbox.Labels[labelIssue] == "" {
			return nil, "launcher pod is not the current controller-owned issue pod"
		}
		ctx, cancel := context.WithTimeout(context.Background(), apiTimeout)
		defer cancel()
		credential, err := r.launcherCredential(ctx, view.sandbox, role)
		if err != nil {
			return nil, "launcher credential is unavailable"
		}
		hash := sha256.Sum256([]byte(hello.Token))
		if subtle.ConstantTimeCompare(credential.hash[:], hash[:]) != 1 {
			return nil, "launcher token is not current"
		}
		if credential.podUID != hello.PodUID {
			return nil, "launcher pod UID is not bound to its Secret epoch"
		}
		token, err := claim.NewToken(r.project, credential.issue, role)
		if err != nil || SandboxName(token) != hello.Sandbox {
			return nil, "launcher credential does not name this issue role"
		}
		return r.launchers.accept(token, hello), ""
	}
}

func (r *Runtime) launcherCredential(ctx context.Context, s *sandbox, role claim.Role) (launcherCredential, error) {
	name := roleSecretName(s.Name, role)
	cached := func() (launcherCredential, bool) {
		r.launcherAuth.mu.Lock()
		defer r.launcherAuth.mu.Unlock()
		value, found := r.launcherAuth.values[name]
		return value, found && value.sandboxUID == s.UID
	}
	if value, found := cached(); found {
		return value, nil
	}
	value, err, _ := r.launcherAuth.loads.Do(name, func() (any, error) {
		if value, found := cached(); found {
			return value, nil
		}
		secret, err := r.kube.CoreV1().Secrets(r.namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return launcherCredential{}, err
		}
		if !ownedBySandbox(secret.OwnerReferences, s.UID) || len(secret.Data[LauncherTokenFile]) == 0 {
			return launcherCredential{}, fmt.Errorf("launcher Secret %s is not a credential of Sandbox %s", name, s.Name)
		}
		loaded := credentialFromSecret(s.UID, secret)
		r.launcherAuth.mu.Lock()
		defer r.launcherAuth.mu.Unlock()
		// A binding write that completed during this GET is newer than the read; do not undo it.
		if current, found := r.launcherAuth.values[name]; found && current.sandboxUID == s.UID {
			return current, nil
		}
		if r.launcherAuth.values == nil {
			r.launcherAuth.values = make(map[string]launcherCredential)
		}
		r.launcherAuth.values[name] = loaded
		return loaded, nil
	})
	if err != nil {
		return launcherCredential{}, err
	}
	return value.(launcherCredential), nil
}

func credentialFromSecret(uid types.UID, secret *corev1.Secret) launcherCredential {
	return launcherCredential{sandboxUID: uid, podUID: secret.Annotations[launcherPodUIDAnnotation],
		issue: secret.Annotations[annotationIssue], hash: sha256.Sum256(secret.Data[LauncherTokenFile])}
}

func (r *Runtime) cacheLauncherCredential(s *sandbox, secret *corev1.Secret) {
	r.launcherAuth.mu.Lock()
	defer r.launcherAuth.mu.Unlock()
	if r.launcherAuth.values == nil {
		r.launcherAuth.values = make(map[string]launcherCredential)
	}
	r.launcherAuth.values[secret.Name] = credentialFromSecret(s.UID, secret)
}

func (r *Runtime) forgetLauncherCredentials(name string) {
	r.launcherAuth.mu.Lock()
	defer r.launcherAuth.mu.Unlock()
	for _, role := range claim.Roles {
		delete(r.launcherAuth.values, roleSecretName(name, role))
	}
}
