package runtime

import "errors"

// A session pod is one an operator launches by hand in the deployment's namespace, outside every
// claim: it runs no launcher, the daemon never spawns, stops or observes it, and nothing of the
// workflow knows it exists. The daemon serves it the two things only the daemon holds — a
// secrets-broker enrollment and a gh bundle — and names a pod it will not serve with one of these.
//
// ErrNoSuchPod is a pod of that name in the deployment's namespace that the daemon cannot find.
// ErrPodNotSession is one it found that is Legion's own (it carries the project label): a claim's
// pod and the project controller's are launched with their credentials and are no session's.
var (
	ErrNoSuchPod     = errors.New("no pod of that name in the deployment's namespace")
	ErrPodNotSession = errors.New("is a pod of this Legion project, not a session pod")
)
