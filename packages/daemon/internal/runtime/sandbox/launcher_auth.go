package sandbox

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"slices"
	"sync"
	"time"

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
	// failed is when each Secret's latest read failed, until a read or a binding write of it
	// succeeds (credentialRetry).
	failed map[string]time.Time
	loads  singleflight.Group
}

// credentialRetry is how long after a launcher Secret's read failed a hello needing it is refused
// without another read. A launcher says hello again a second after a refusal, so a Secret the API
// does not serve costs one read in ten hellos: three hundred launchers whose reads fail ask for
// thirty a second, within the client's rate (clientQPS).
const credentialRetry = 10 * time.Second

// LauncherResolver rejects unknown pods from informer state before consulting credentials: a
// launcher's role must be one of the roles of the kind its pod's labels name (podKindOf), a
// workflow role of an issue pod or the controller of the project controller's, and its claim is the
// one that kind derives (podKind.claimToken). Tokens minted here are cached as hashes; after a
// restart a known role's Secret is read once however often its launcher says hello
// (launcherCredential), so a restart's reconnecting launchers cost one read each (LEGION-723) and
// unauthenticated hellos cannot turn into an unbounded stream of Kubernetes requests. A hello waits
// for that read up to apiTimeout.
func (r *Runtime) LauncherResolver() stream.LauncherResolver {
	return func(hello shimwire.LauncherHello) (stream.LauncherHandler, string) {
		role := claim.Role(hello.Role)
		view, err := r.view(hello.Sandbox)
		if err != nil || view.sandbox == nil || view.pod == nil || string(view.pod.UID) != hello.PodUID {
			return nil, "launcher pod is not the current controller-owned pod"
		}
		kind, err := podKindOf(view.sandbox.Labels)
		if err != nil || !slices.Contains(kind.roles(), role) {
			return nil, "launcher role is not one of its pod's roles"
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
		// Options.Project is the legion.dev/project label's value (its own doc comment), not
		// necessarily already a claim.ProjectToken: a deployment's own project name may carry
		// characters ProjectToken drops (its doc: "sjawhar/legion" names the same role topics
		// under either daemon), and go through it before naming a claim.
		project, err := claim.ProjectToken(r.project)
		if err != nil {
			return nil, "launcher credential does not name this pod's role"
		}
		token, err := kind.claimToken(project, credential.issue, role)
		if err != nil || SandboxName(token) != hello.Sandbox {
			return nil, "launcher credential does not name this pod's role"
		}
		return r.launchers.accept(token, hello), ""
	}
}

// launcherCredential is the credential of role's launcher in s: the cached one, or one read of the
// role's Secret, single-flight per Secret. The read outlives the hello that began it: it waits its
// turn on the client's rate limiter for as long as that takes (readLauncherCredential), while each
// hello waits for it until ctx ends and is then refused, so the launcher's next hello joins the
// same read or finds what it cached, never starting another. A Secret whose read failed is not
// read again for credentialRetry.
func (r *Runtime) launcherCredential(ctx context.Context, s *sandbox, role claim.Role) (launcherCredential, error) {
	name := roleSecretName(s.Name, role)
	if value, found := r.launcherAuth.cached(name, s.UID); found {
		return value, nil
	}
	r.launcherAuth.mu.Lock()
	failedAt, failed := r.launcherAuth.failed[name]
	r.launcherAuth.mu.Unlock()
	if retry := failedAt.Add(credentialRetry); failed && r.now().Before(retry) {
		return launcherCredential{}, fmt.Errorf("launcher Secret %s: its last read failed; it is read again from %s", name, retry.UTC().Format(time.RFC3339))
	}
	read := r.launcherAuth.loads.DoChan(name, func() (any, error) { return r.readLauncherCredential(s, name) })
	select {
	case result := <-read:
		if result.Err != nil {
			return launcherCredential{}, result.Err
		}
		return result.Val.(launcherCredential), nil
	case <-ctx.Done():
		return launcherCredential{}, ctx.Err()
	}
}

// cached is the credential cached as name's for the Sandbox with uid, if there is one.
func (c *launcherCredentials) cached(name string, uid types.UID) (launcherCredential, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	value, found := c.values[name]
	return value, found && value.sandboxUID == uid
}

// readLauncherCredential reads the launcher Secret name of s into the cache, or records when its
// read failed. It runs for the runtime's lifetime rather than any one hello's, so its wait on the
// client's rate limiter is bounded by nothing but its turn; the request itself is bounded by
// apiTimeout (clientConfigs).
func (r *Runtime) readLauncherCredential(s *sandbox, name string) (launcherCredential, error) {
	if value, found := r.launcherAuth.cached(name, s.UID); found {
		return value, nil
	}
	secret, err := r.kube.CoreV1().Secrets(r.namespace).Get(r.lifetime, name, metav1.GetOptions{})
	if err == nil && (!ownedBySandbox(secret.OwnerReferences, s.UID) || len(secret.Data[LauncherTokenFile]) == 0) {
		err = fmt.Errorf("launcher Secret %s is not a credential of Sandbox %s", name, s.Name)
	}
	r.launcherAuth.mu.Lock()
	defer r.launcherAuth.mu.Unlock()
	if err != nil {
		if r.launcherAuth.failed == nil {
			r.launcherAuth.failed = make(map[string]time.Time)
		}
		r.launcherAuth.failed[name] = r.now()
		return launcherCredential{}, err
	}
	delete(r.launcherAuth.failed, name)
	// A binding write that completed during this GET is newer than the read; do not undo it.
	if current, found := r.launcherAuth.values[name]; found && current.sandboxUID == s.UID {
		return current, nil
	}
	if r.launcherAuth.values == nil {
		r.launcherAuth.values = make(map[string]launcherCredential)
	}
	loaded := credentialFromSecret(s.UID, secret)
	r.launcherAuth.values[name] = loaded
	return loaded, nil
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
	delete(r.launcherAuth.failed, secret.Name)
}

// forgetLauncherCredentials drops the cached credential, and any failed read, of every launcher
// role of the Sandbox named name, once that Sandbox is deleted: whichever roles its pod ran, no
// other Sandbox's Secret has a name of its.
func (r *Runtime) forgetLauncherCredentials(name string) {
	r.launcherAuth.mu.Lock()
	defer r.launcherAuth.mu.Unlock()
	for _, role := range launcherRoles {
		delete(r.launcherAuth.values, roleSecretName(name, role))
		delete(r.launcherAuth.failed, roleSecretName(name, role))
	}
}
