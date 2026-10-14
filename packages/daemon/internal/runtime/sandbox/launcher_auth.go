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
	podUID, issue string
	hash          [sha256.Size]byte
}

// launcherSecret is what the runtime holds of one role's launcher Secret for one Sandbox: the
// credential a read or a binding of the Secret gave it (held), or else when its latest read failed.
type launcherSecret struct {
	credential launcherCredential
	held       bool
	failedAt   time.Time
}

// launcherCredentials are the launcher Secrets the runtime holds, by name and then by the UID of
// the Sandbox each was read or bound for, so a Sandbox's entry is never another's of the same
// name; and the reads of them in flight, one per Secret of a Sandbox (launcherCredential).
type launcherCredentials struct {
	mu      sync.Mutex
	secrets map[string]map[types.UID]launcherSecret
	reads   singleflight.Group
}

// credentialRetry is how long after a launcher Secret's read failed a hello needing it is refused
// without another read. A launcher says hello again a second after a refusal, so a Secret the API
// does not serve costs one read in ten hellos.
const credentialRetry = 10 * time.Second

// LauncherResolver rejects unknown pods from informer state before consulting credentials: a
// launcher's role must be one of the roles of the kind its pod's labels name (podKindOf), a
// workflow role of an issue pod or the controller of the project controller's, and its claim is the
// one that kind derives (podKind.claimToken). Tokens minted here are cached as hashes; after a
// restart a known role's Secret is read once however often its launcher says hello
// (launcherCredential), so a restart's reconnecting launchers cost one read each and
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

// launcherCredential is the credential of role's launcher in s: the one held, or one read of the
// role's Secret, single-flight per Secret of a Sandbox. The read outlives the hello that began it:
// it waits its turn on the client's rate limiter for as long as that takes
// (readLauncherCredential), while each hello waits for it until ctx ends and is then refused, so
// the launcher's next hello joins the same read or finds what it stored, never starting another. A
// Secret whose read failed is not read again for that Sandbox for credentialRetry.
func (r *Runtime) launcherCredential(ctx context.Context, s *sandbox, role claim.Role) (launcherCredential, error) {
	name := roleSecretName(s.Name, role)
	credential, held, readFrom := r.launcherAuth.lookup(name, s.UID)
	if held {
		return credential, nil
	}
	if r.now().Before(readFrom) {
		return launcherCredential{}, fmt.Errorf("launcher Secret %s: its last read failed; it is read again from %s", name, readFrom.UTC().Format(time.RFC3339))
	}
	read := r.launcherAuth.reads.DoChan(name+"/"+string(s.UID), func() (any, error) { return r.readLauncherCredential(s, name) })
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

// readLauncherCredential reads the launcher Secret name of s and stores its credential, or records
// and logs that the read failed. It runs for the runtime's lifetime rather than any one hello's, so
// its wait on the client's rate limiter is bounded by nothing but its turn; the request itself is
// bounded by apiTimeout (requestConfig).
func (r *Runtime) readLauncherCredential(s *sandbox, name string) (launcherCredential, error) {
	if credential, held, _ := r.launcherAuth.lookup(name, s.UID); held {
		return credential, nil
	}
	secret, err := r.kube.CoreV1().Secrets(r.namespace).Get(r.lifetime, name, metav1.GetOptions{})
	if err == nil && (!ownedBySandbox(secret.OwnerReferences, s.UID) || len(secret.Data[LauncherTokenFile]) == 0) {
		err = fmt.Errorf("launcher Secret %s is not a credential of Sandbox %s", name, s.Name)
	}
	if err != nil {
		r.launcherAuth.fail(name, s.UID, r.now())
		r.log.Warn("sandbox runtime: a launcher Secret could not be read; its launcher's hellos are refused until it is read again",
			"secret", name, "sandbox", s.Name, "readAgainAfter", credentialRetry, "err", err)
		return launcherCredential{}, err
	}
	return r.launcherAuth.store(name, s.UID, credentialFromSecret(secret)), nil
}

func credentialFromSecret(secret *corev1.Secret) launcherCredential {
	return launcherCredential{podUID: secret.Annotations[launcherPodUIDAnnotation],
		issue: secret.Annotations[annotationIssue], hash: sha256.Sum256(secret.Data[LauncherTokenFile])}
}

// lookup is the credential held as name's for the Sandbox with uid, if one is (held), and otherwise
// when a read of it may next begin: credentialRetry after that Sandbox's latest failed read, the
// zero time when none failed.
func (c *launcherCredentials) lookup(name string, uid types.UID) (credential launcherCredential, held bool, readFrom time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	secret := c.secrets[name][uid]
	switch {
	case secret.held:
		return secret.credential, true, time.Time{}
	case secret.failedAt.IsZero():
		return launcherCredential{}, false, time.Time{}
	}
	return launcherCredential{}, false, secret.failedAt.Add(credentialRetry)
}

// bind holds credential, a binding write's or a binding check's, as name's for the Sandbox with
// uid, in place of whatever was held. A binding is made under a launch of that Sandbox's pod, which
// proves it holds the name now, so every other Sandbox's entry of the name, a deleted one's, goes.
func (c *launcherCredentials) bind(name string, uid types.UID, credential launcherCredential) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.secrets == nil {
		c.secrets = make(map[string]map[types.UID]launcherSecret)
	}
	c.secrets[name] = map[types.UID]launcherSecret{uid: {credential: credential, held: true}}
}

// store holds credential, a read's, as name's for the Sandbox with uid, unless that Sandbox holds
// one already: a binding that completed during the read is newer than it. It is the credential the
// Sandbox holds.
func (c *launcherCredentials) store(name string, uid types.UID, credential launcherCredential) launcherCredential {
	c.mu.Lock()
	defer c.mu.Unlock()
	if held := c.secrets[name][uid]; held.held {
		return held.credential
	}
	c.put(name, uid, launcherSecret{credential: credential, held: true})
	return credential
}

// fail records that a read of name for the Sandbox with uid failed at at, unless that Sandbox
// holds a credential.
func (c *launcherCredentials) fail(name string, uid types.UID, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.secrets[name][uid].held {
		return
	}
	c.put(name, uid, launcherSecret{failedAt: at})
}

// forget drops whatever is held of name, for every Sandbox. A read that lands afterwards for a
// Sandbox since deleted writes only that Sandbox's entry, which nothing looks up again, until the
// name's next bind or forget drops it.
func (c *launcherCredentials) forget(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.secrets, name)
}

// put holds secret as name's for the Sandbox with uid; its caller holds c.mu.
func (c *launcherCredentials) put(name string, uid types.UID, secret launcherSecret) {
	if c.secrets == nil {
		c.secrets = make(map[string]map[types.UID]launcherSecret)
	}
	if c.secrets[name] == nil {
		c.secrets[name] = make(map[types.UID]launcherSecret)
	}
	c.secrets[name][uid] = secret
}

// cacheLauncherCredential holds secret's credential, read or written as bound to its pod, for the
// Sandbox s.
func (r *Runtime) cacheLauncherCredential(s *sandbox, secret *corev1.Secret) {
	r.launcherAuth.bind(secret.Name, s.UID, credentialFromSecret(secret))
}

// forgetLauncherCredentials drops what is held of every launcher role's Secret of the Sandbox named
// name, once that Sandbox is deleted: whichever roles its pod ran, no other Sandbox's Secret has a
// name of its.
func (r *Runtime) forgetLauncherCredentials(name string) {
	for _, role := range launcherRoles {
		r.launcherAuth.forget(roleSecretName(name, role))
	}
}
