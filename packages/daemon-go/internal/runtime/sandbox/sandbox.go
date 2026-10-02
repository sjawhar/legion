// Package sandbox is the runtime that runs every agent as the pod of an Agent Sandbox
// (agents.x-k8s.io/v1beta1 Sandbox, kubernetes-sigs/agent-sandbox v1.0.3): one Sandbox per claim,
// named for the claim's token, whose pod runs `legion workspace-init` on the tree volume and then
// `legion worker-shim` dialing the daemon's worker stream, with Oh My Pi under it.
//
// A claim's Sandbox outlives its processes. A process is the Sandbox's pod, and the pod's uid is
// the incarnation every locator records; the controller names the pod after its Sandbox and owns
// it, so the runtime reads both from two informers, label-selected on the project, and joins them
// by name. Every relaunch goes through `operatingMode: Suspended` — the only mode in which the
// controller deletes a pod — waits the old pod out, rewrites the claim's Secret, and patches
// `Running` (LEGION-208 Stage 4 plan, decisions 1–6).
//
// The Sandbox structs are Legion's own (types.go): importing sigs.k8s.io/agent-sandbox would move
// grpc, otel, and controller-runtime for every module under go.work.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/stream"
)

var _ runtime.Runtime = (*Runtime)(nil)

// apiTimeout bounds one API call the runtime makes outside a relaunch's own budget: a log or
// event read for an observation's detail, a patch, a delete.
const apiTimeout = 30 * time.Second

// recheckInterval is how often a wait re-reads what it waits on even when no informer event
// arrived, since a wait's condition can include an API read that no event announces.
const recheckInterval = 500 * time.Millisecond

// Runtime is the Agent Sandbox runtime. Build one with New.
type Runtime struct {
	namespace, project, image, storageClass string
	treeVolume                              resource.Quantity
	scheduling                              Scheduling
	resources                               map[claim.Role]corev1.ResourceRequirements
	streamURL, daemonURL, envoyURL          string
	dispatchURL, dispatchToken              string
	natsURLs                                []string
	tools                                   Tools
	pod                                     Pod
	providerKeys                            map[string]string
	providersSecrets                        []string
	natsUser                                string
	agentSecrets                            *AgentSecrets
	agent                                   []string
	bootTimeout                             time.Duration
	bootIntervals                           int
	terminationGrace                        time.Duration
	probeInterval                           time.Duration
	adoptTimeout                            time.Duration
	tokens                                  ProvisionTokens
	conns                                   runtime.Conns
	launchers                               *launchers
	now                                     func() time.Time
	log                                     *slog.Logger

	dyn       dynamic.Interface
	kube      kubernetes.Interface
	sandboxes cache.SharedIndexInformer
	pods      cache.SharedIndexInformer
	// sandboxFeed and podFeed are whether each informer's latest list or watch request succeeded.
	sandboxFeed, podFeed feed

	mu sync.Mutex
	// changed is closed and replaced on every informer event, waking every wait.
	changed chan struct{}
	// watch is every claim's recorded incarnation (the watch of the interfaces block): filled by
	// Spawn's and Resume's results and by ReconcileOrphans' located claims; an entry leaves on
	// Suspend, on Release, and once a Gone or NotRecordedProcess for it is delivered.
	watch map[claim.Token]runtime.Locator
	// observer is the running Observe, nil when none runs.
	observer *observer
	// issues serializes first creation and replacement of one issue pod. Tree initialization stays
	// separately serialized because child issue pods share the root's clone and PVC.
	issues map[string]chan struct{}
	// trees serializes the launches of one tree's pods (relaunch.go, awaitTreeInitialized).
	trees map[string]chan struct{}
	// launchedMu guards launched: the Sandbox names of the claims this runtime has launched and not
	// released, which the orphan sweep keeps whether or not its known set names them. The sweep
	// holds launchedMu from its check through its delete (deleteOrphan).
	launchedMu sync.Mutex
	launched   map[string]bool
	// resourceStore is injected by daemon boot before any claim launches. Unit rigs explicitly
	// skip the production layout census and resource capability with test-only options.
	resourceStore           *store.Store
	testLayoutCensusSkipped bool
}

// New builds the runtime from opts, starts its Sandbox and pod informers for ctx's lifetime, and
// returns once both stores have synced. A list or watch that fails before then is a refusal
// naming the failure: a runtime that cannot see its Sandboxes can verify nothing.
func New(ctx context.Context, rc *rest.Config, opts Options) (*Runtime, error) {
	r, err := configure(opts)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("sandbox runtime: dynamic client: %w", err)
	}
	kube, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("sandbox runtime: kubernetes client: %w", err)
	}
	if err := r.start(ctx, dyn, kube); err != nil {
		return nil, err
	}
	if !opts.SkipLegacyLayoutCensusForTest {
		if err := r.rejectLegacyIssueSandboxes(ctx); err != nil {
			return nil, err
		}
	}

	if listener, ok := opts.Conns.(*stream.Listener); ok {
		listener.SetLauncherResolver(r.LauncherResolver())
	}
	return r, nil
}

// SetIssueResourceStore injects the daemon's durable capability before supervision launches any
// role. It is deliberately not an Option: production boot owns the store; unit rigs use the
// explicit test-only layout-census skip.
func (r *Runtime) SetIssueResourceStore(resources *store.Store) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resourceStore = resources
}

// rejectLegacyIssueSandboxes is the Kubernetes half of the issue-pod layout fence. It runs before
// a launcher resolver is registered: a legacy claim/object pair must never be adopted, suspended
// or deleted as though it belonged to the new shared-pod layout. Image probes are explicit
// non-issue Sandboxes and are excluded.
func (r *Runtime) rejectLegacyIssueSandboxes(ctx context.Context) error {
	reading, cancel := call(ctx)
	defer cancel()
	list, err := r.sandboxClient().List(reading, metav1.ListOptions{LabelSelector: labelProject + "=" + r.project})
	if err != nil {
		return fmt.Errorf("sandbox runtime: census existing issue Sandboxes before layout migration: %w", err)
	}
	for _, object := range list.Items {
		if object.GetLabels()[labelProbe] != "" {
			continue
		}
		containers, found, err := unstructured.NestedSlice(object.Object, "spec", "podTemplate", "spec", "containers")
		if err != nil || !found {
			return fmt.Errorf("sandbox runtime: legacy issue Sandbox %s has no issue-pod container shape; migrate or remove it before enabling issue pods", object.GetName())
		}
		names := map[string]bool{}
		for _, raw := range containers {
			container, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("sandbox runtime: legacy issue Sandbox %s has an unreadable container shape; migrate or remove it before enabling issue pods", object.GetName())
			}
			name, _ := container["name"].(string)
			names[name] = true
		}
		for _, role := range claim.Roles {
			if !names[string(role)] {
				return fmt.Errorf("sandbox runtime: legacy issue Sandbox %s has no %s launcher container; migrate or remove it before enabling issue pods", object.GetName(), role)
			}
		}
	}
	return nil
}

// configure checks opts and fills their defaults, touching no cluster.
func configure(opts Options) (*Runtime, error) {
	refuse := func(format string, args ...any) (*Runtime, error) {
		return nil, fmt.Errorf("sandbox runtime: "+format, args...)
	}
	pool, poolSet := opts.Scheduling.NodeSelector[poolKey]
	switch {
	case opts.Namespace == "":
		return refuse("no namespace")
	case opts.Project == "":
		return refuse("no project")
	case !strings.Contains(opts.Image, "@sha256:"):
		return refuse("image %q is not pinned by digest (…@sha256:…)", opts.Image)
	case opts.StorageClass == "":
		return refuse("no storage class for the tree volume (the cluster has no default class to fall back on)")
	case opts.TreeVolume.Sign() <= 0:
		return refuse("no tree volume size: %s is not a positive quantity", opts.TreeVolume.String())
	case opts.BootTimeout <= 0 || opts.TerminationGrace <= 0 || opts.ProbeInterval <= 0 || opts.AdoptTimeout <= 0:
		return refuse("the boot timeout, termination grace, probe interval, and adoption timeout must be positive")
	case opts.BootIntervals <= 0:
		return refuse("the registration deadline must be a positive number of boot intervals")
	case opts.Tokens == nil:
		return refuse("no provisioning token source")
	case opts.Conns == nil:
		return refuse("no connection directory")
	case (opts.DispatchURL == "") != (opts.DispatchToken == ""):
		return refuse("the Dispatch URL and its bearer are configured together (URL %q, bearer given: %t)",
			opts.DispatchURL, opts.DispatchToken != "")
	case poolSet:
		return refuse("scheduling node selector sets %s=%q: %s is the runtime's, which puts every pod on the %s pool the cluster's admission policy requires",
			poolKey, pool, poolKey, poolValue)
	}
	if errs := validation.IsValidLabelValue(opts.Project); len(errs) > 0 {
		return refuse("project %q is not a label value: %s", opts.Project, strings.Join(errs, "; "))
	}
	host, ok := strings.CutPrefix(opts.StreamURL, "tcp://")
	if _, _, err := net.SplitHostPort(host); !ok || err != nil {
		return refuse("stream URL %q is not tcp://host:port, which a pod can dial", opts.StreamURL)
	}
	for _, tool := range []struct{ name, path string }{
		{"gh", opts.Tools.GH}, {"git", opts.Tools.Git}, {"jj", opts.Tools.JJ}, {"legion", opts.Tools.Legion},
		{"agent-secrets", opts.Tools.AgentSecrets},
	} {
		if !filepath.IsAbs(tool.path) {
			return refuse("the image's %s path %q is not absolute", tool.name, tool.path)
		}
	}
	if a := opts.AgentSecrets; a != nil {
		switch {
		case a.URL == "":
			return refuse("agent secrets: no broker URL")
		case a.Audience == "":
			return refuse("agent secrets: no token audience")
		case a.TokenExpiry < 10*time.Minute || a.TokenExpiry > time.Hour:
			return refuse("agent secrets: token expiry %s is not between %s and %s (the API server's floor and the cluster's admission cap)", a.TokenExpiry, 10*time.Minute, time.Hour)
		}
	}
	if err := CheckPod(opts.Pod, opts.ProviderKeys, opts.Tools, opts.LaunchSecrets, opts.ProvidersSecrets); err != nil {
		return refuse("%v", err)
	}
	for _, name := range opts.ProvidersSecrets {
		if !slices.Contains(opts.LaunchSecrets, name) {
			return refuse("providers secret %s is not a launch secret (%s)", name, strings.Join(opts.LaunchSecrets, ", "))
		}
	}
	r := &Runtime{
		namespace: opts.Namespace, project: opts.Project, image: opts.Image, storageClass: opts.StorageClass,
		treeVolume: opts.TreeVolume, scheduling: opts.Scheduling, resources: opts.Resources,
		streamURL: opts.StreamURL, daemonURL: opts.DaemonURL, envoyURL: opts.EnvoyURL, dispatchURL: opts.DispatchURL,
		dispatchToken: opts.DispatchToken, natsURLs: opts.NATSURLs, tools: opts.Tools, agentSecrets: opts.AgentSecrets,
		pod: opts.Pod, providerKeys: opts.ProviderKeys, providersSecrets: slices.Sorted(slices.Values(opts.ProvidersSecrets)), natsUser: opts.NATSUser,
		bootTimeout: opts.BootTimeout, bootIntervals: opts.BootIntervals, terminationGrace: opts.TerminationGrace,
		probeInterval: opts.ProbeInterval, adoptTimeout: opts.AdoptTimeout, agent: opts.Agent,
		tokens: opts.Tokens, conns: opts.Conns, now: opts.Now, log: opts.Log,
		changed: make(chan struct{}), watch: map[claim.Token]runtime.Locator{}, issues: map[string]chan struct{}{},
		trees: map[string]chan struct{}{}, launched: map[string]bool{}, launchers: newLaunchers(),
		testLayoutCensusSkipped: opts.SkipLegacyLayoutCensusForTest,
	}
	if len(r.agent) == 0 {
		r.agent = []string{defaultAgent}
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.log == nil {
		r.log = slog.Default()
	}
	return r, nil
}

// start runs both informers, selected on the project label, until ctx ends, and waits for both
// stores to sync.
func (r *Runtime) start(ctx context.Context, dyn dynamic.Interface, kube kubernetes.Interface) error {
	r.dyn, r.kube = dyn, kube
	sandboxes, pods := r.sandboxClient(), kube.CoreV1().Pods(r.namespace)
	r.sandboxFeed.resource, r.podFeed.resource = "Sandbox", "pod"
	r.sandboxes = r.informer(&r.sandboxFeed, dyn, &unstructured.Unstructured{},
		func(ctx context.Context, o metav1.ListOptions) (k8sruntime.Object, error) {
			return sandboxes.List(ctx, o)
		},
		func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) {
			return sandboxes.Watch(ctx, o)
		})
	r.pods = r.informer(&r.podFeed, kube, &corev1.Pod{},
		func(ctx context.Context, o metav1.ListOptions) (k8sruntime.Object, error) { return pods.List(ctx, o) },
		func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) { return pods.Watch(ctx, o) })
	for _, informer := range []cache.SharedIndexInformer{r.sandboxes, r.pods} {
		if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj any) { r.notify(obj) },
			UpdateFunc: func(_, obj any) { r.notify(obj) },
			DeleteFunc: func(obj any) { r.notify(obj) },
		}); err != nil {
			return fmt.Errorf("sandbox runtime: %w", err)
		}
	}
	running, stop := context.WithCancel(ctx)
	go r.sandboxes.RunWithContext(running)
	go r.pods.RunWithContext(running)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	// Whether the stores are being fed is the feeds' answer, not client-go's watch-error handler:
	// the reflector retries a refused list or watch itself and never calls that handler, so a boot
	// that waited on it sat until its own deadline with two stores nothing was filling. Each list
	// and watch records its outcome in its feed, and a feed that is failing here refuses the boot
	// naming the request that failed.
	for !r.synced() {
		select {
		case <-ctx.Done():
			stop()
			return fmt.Errorf("sandbox runtime: the informers did not sync: %w", ctx.Err())
		case <-tick.C:
			if err := errors.Join(r.sandboxFeed.check(), r.podFeed.check()); err != nil {
				stop()
				return fmt.Errorf("sandbox runtime: listing the namespace %s's Sandboxes and pods failed before the stores synced: %w",
					r.namespace, err)
			}
		}
	}
	context.AfterFunc(ctx, stop)
	return nil
}

// informer is a shared informer of the namespace's objects of the project, listed and watched
// through list and watch, with the outcome of every request recorded in f: client-go's reflector
// retries a refused watch itself without calling the watch-error handler, so only the requests
// themselves say whether the store is still being fed.
func (r *Runtime) informer(f *feed, client any, example k8sruntime.Object, list cache.ListWithContextFunc,
	watchFn cache.WatchFuncWithContext,
) cache.SharedIndexInformer {
	project := func(o metav1.ListOptions) metav1.ListOptions {
		o.LabelSelector = labelProject + "=" + r.project
		return o
	}
	lw := &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, o metav1.ListOptions) (k8sruntime.Object, error) {
			object, err := list(ctx, project(o))
			f.record(err, r.now())
			return object, err
		},
		WatchFuncWithContext: func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) {
			w, err := watchFn(ctx, project(o))
			f.record(err, r.now())
			return w, err
		},
	}
	return cache.NewSharedIndexInformerWithOptions(cache.ToListWatcherWithWatchListSemantics(lw, client), example,
		cache.SharedIndexInformerOptions{})
}

// feed is whether one informer's latest list or watch request to the API server succeeded. A
// store whose feed is failing holds what it last heard, which may no longer be so.
type feed struct {
	resource string
	mu       sync.Mutex
	failed   error
	since    time.Time
}

// record notes a request's outcome at now: a failure marks the feed failing from the first one in
// a row, and a success clears it.
func (f *feed) record(err error, now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		f.failed = nil
		return
	}
	if f.failed == nil {
		f.since = now
	}
	f.failed = err
}

// check is an error while the feed is failing.
func (f *feed) check() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failed == nil {
		return nil
	}
	return fmt.Errorf("the %s store may be stale: its list or watch has failed since %s: %w",
		f.resource, f.since.UTC().Format(time.RFC3339), f.failed)
}

// call is ctx bounded for one API request.
func call(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, apiTimeout)
}

func (r *Runtime) synced() bool { return r.sandboxes.HasSynced() && r.pods.HasSynced() }

// notify wakes every wait and marks every watched claim whose Sandbox or pod changed for the
// running Observe. A pod is named after its Sandbox, so one name finds both.
func (r *Runtime) notify(obj any) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	var name string
	switch o := obj.(type) {
	case *corev1.Pod:
		name = o.Name
	case *unstructured.Unstructured:
		name = o.GetName()
	default:
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	close(r.changed)
	r.changed = make(chan struct{})
	if r.observer == nil {
		return
	}
	for token, loc := range r.watch {
		if loc.Sandbox.Name == name {
			r.observer.mark(token)
		}
	}
}

// await re-checks done after every informer event, and every recheck interval, until it reports
// true, fails, or budget passes; what names the wait in the timeout.
func (r *Runtime) await(ctx context.Context, budget time.Duration, what string, done func() (bool, error)) error {
	deadline := time.NewTimer(budget)
	defer deadline.Stop()
	recheck := time.NewTicker(recheckInterval)
	defer recheck.Stop()
	for {
		r.mu.Lock()
		changed := r.changed
		r.mu.Unlock()
		ok, err := done()
		if err != nil || ok {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("timed out after %s waiting for %s", budget, what)
		case <-changed:
		case <-recheck.C:
		}
	}
}

// storedSandbox is the claim's Sandbox in the synced store, or nil.
func (r *Runtime) storedSandbox(name string) (*sandbox, error) {
	obj, ok, err := r.sandboxes.GetStore().GetByKey(r.namespace + "/" + name)
	if err != nil || !ok {
		return nil, err
	}
	return decodeSandbox(obj.(*unstructured.Unstructured))
}

// storedPod is the pod named name in the synced store, or nil.
func (r *Runtime) storedPod(name string) *corev1.Pod {
	obj, ok, err := r.pods.GetStore().GetByKey(r.namespace + "/" + name)
	if err != nil || !ok {
		return nil
	}
	return obj.(*corev1.Pod)
}

// ownedBy reports whether p is controller-owned by the Sandbox with uid: the one relationship
// that makes a pod the Sandbox's (sandbox_controller.go:1460). A same-named pod without it is not
// the Sandbox's process, whatever its name says.
func ownedBy(p *corev1.Pod, uid types.UID) bool {
	if p == nil {
		return false
	}
	owner := metav1.GetControllerOf(p)
	return owner != nil && owner.UID == uid
}

func terminal(p *corev1.Pod) bool {
	return p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed
}

func decodeSandbox(u *unstructured.Unstructured) (*sandbox, error) {
	var s sandbox
	if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &s); err != nil {
		return nil, fmt.Errorf("decode sandbox %s: %w", u.GetName(), err)
	}
	return &s, nil
}

func encodeSandbox(s sandbox) (*unstructured.Unstructured, error) {
	object, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(&s)
	if err != nil {
		return nil, fmt.Errorf("encode sandbox %s: %w", s.Name, err)
	}
	return &unstructured.Unstructured{Object: object}, nil
}

func (r *Runtime) sandboxClient() dynamic.ResourceInterface {
	return r.dyn.Resource(sandboxGVR).Namespace(r.namespace)
}

// locatorFor is one role process's address in an issue pod.
func (r *Runtime) locatorFor(token claim.Token, uid types.UID, generation uint64) runtime.Locator {
	container, ok := roleContainer(token)
	if !ok {
		panic(fmt.Sprintf("sandbox runtime: claim %s has no role container", token))
	}
	podUID := string(uid)
	return runtime.Locator{
		Runtime:     runtime.RuntimeSandbox,
		Claim:       token,
		Incarnation: runtime.SandboxIncarnation(podUID, generation),
		Sandbox: &runtime.SandboxLocator{
			Namespace: r.namespace, Name: SandboxName(token), PodUID: podUID, Container: container, Generation: generation,
		},
	}
}

func roleContainer(token claim.Token) (string, bool) {
	for _, role := range claim.Roles {
		if strings.HasSuffix(string(token), "-"+string(role)) {
			return string(role), true
		}
	}
	return "", false
}

// checkLocator refuses a locator this runtime did not mint.
func (r *Runtime) checkLocator(loc runtime.Locator) error {
	if err := loc.Validate(); err != nil {
		return err
	}
	switch {
	case loc.Runtime != runtime.RuntimeSandbox:
		return fmt.Errorf("sandbox runtime: locator %s is a %s locator", loc.Claim, loc.Runtime)
	case loc.Sandbox.Namespace != r.namespace:
		return fmt.Errorf("sandbox runtime: locator %s is in namespace %s, not %s", loc.Claim, loc.Sandbox.Namespace, r.namespace)
	case loc.Sandbox.Name != SandboxName(loc.Claim):
		return fmt.Errorf("sandbox runtime: locator %s names sandbox %s, not the claim's %s", loc.Claim, loc.Sandbox.Name, SandboxName(loc.Claim))
	}
	return nil
}

// ProvisionsWorkspaces is true: every pod's init containers provision its claim's workspace on the
// tree volume, and the tree volume goes with the tree's root claim.
func (r *Runtime) ProvisionsWorkspaces() bool { return true }

// Suspend ends only the recorded role process. It never changes the issue Sandbox operating mode:
// every other resident role shares that pod and stays reachable until its own explicit stop or
// issue-level lifecycle effect. A recorded process its pod or launcher shows already ended — the
// pod replaced or gone, or the launcher running no child or another generation — is stopped
// already, and nothing is sent.
func (r *Runtime) Suspend(ctx context.Context, loc runtime.Locator) error {
	if err := r.checkLocator(loc); err != nil {
		return err
	}
	s, err := r.storedSandbox(loc.Sandbox.Name)
	if err != nil {
		return fmt.Errorf("suspend %s: %w", loc.Claim, err)
	}
	if s == nil {
		r.forgetIf(loc)
		return nil
	}
	pod := r.storedPod(loc.Sandbox.Name)
	if !ownedBy(pod, s.UID) || string(pod.UID) != loc.Sandbox.PodUID || terminal(pod) {
		r.forgetIf(loc)
		return nil
	}
	if state, connected := r.launchers.state(loc.Claim); connected && (state.Child == nil || state.Child.Generation != loc.Sandbox.Generation) {
		r.forgetIf(loc)
		return nil
	}
	stop := shimwire.LauncherStop{
		ID: "stop-" + strconv.FormatUint(loc.Sandbox.Generation, 10), Generation: loc.Sandbox.Generation,
		GraceMs: int(math.Ceil(r.terminationGrace.Seconds() * 1000)),
	}
	stopping, cancel := context.WithTimeout(ctx, r.bootTimeout+r.terminationGrace)
	defer cancel()
	if err := r.launchers.stop(stopping, loc.Claim, stop); err != nil {
		return fmt.Errorf("suspend %s: %w", loc.Claim, err)
	}
	r.forgetIf(loc)
	return nil
}

// setMode sets the Sandbox's operating mode, fenced to the Sandbox read (a JSON patch whose first
// operation tests the uid): a Sandbox replaced in between is never written.
func (r *Runtime) setMode(ctx context.Context, s *sandbox, mode string) error {
	_, err := r.patch(ctx, s, jsonPatchOp{Op: "add", Path: "/spec/operatingMode", Value: mode})
	return err
}

// Release is a claim-scoped operation: it ends at most the recorded role process and forgets the
// claim from this runtime. It NEVER suspends or deletes the issue Sandbox, even if this runtime
// currently sees no sibling role. The durable issue-resource lifecycle, fenced against complete
// stored sibling claims and the issue's close/start generation, is the only owner of issue-pod,
// Secret and root-PVC deletion.
func (r *Runtime) Release(ctx context.Context, k runtime.Known) error {
	if err := k.Validate(); err != nil {
		return fmt.Errorf("sandbox runtime: release: %w", err)
	}
	if k.Locator != nil {
		if err := r.Suspend(ctx, *k.Locator); err != nil {
			return err
		}
	}
	r.forget(k.Claim)
	r.disown(k.Claim)
	return nil
}

// CleanupIssue is the sole whole-issue delete capability. Outbox calls it only after its complete
// persisted sibling-claim census says every claim of issue is retired for generation. BeginCleanup
// repeats the generation/cleanup fence transactionally, so a re-admission that recorded a newer
// resource generation turns an old close into a no-op. The root's tree PVC is deleted only after a
// Get confirms its root Sandbox is gone.
func (r *Runtime) CleanupIssue(ctx context.Context, project, issue, tree string, generation uint64) error {
	if project != r.project {
		return fmt.Errorf("cleanup issue %s: project %s is not runtime project %s", issue, project, r.project)
	}
	r.mu.Lock()
	resources := r.resourceStore
	r.mu.Unlock()
	if resources == nil {
		return errors.New("cleanup issue resources: no durable issue resource store")
	}
	resource, began, err := resources.BeginIssueCleanup(ctx, project, issue, generation)
	if err != nil {
		return err
	}
	if !began {
		return nil
	}
	deleting, cancel := call(ctx)
	sandbox, err := r.sandboxClient().Get(deleting, resource.Sandbox, metav1.GetOptions{})
	cancel()
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return fmt.Errorf("cleanup issue %s: read Sandbox %s: %w", issue, resource.Sandbox, err)
	default:
		uid := sandbox.GetUID()
		deleting, cancel = call(ctx)
		err = r.sandboxClient().Delete(deleting, resource.Sandbox, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
		cancel()
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("cleanup issue %s: delete Sandbox %s: %w", issue, resource.Sandbox, err)
		}
	}
	if err := r.awaitSandboxDeleted(ctx, resource.Sandbox); err != nil {
		return err
	}
	if err := resources.ConfirmIssueCleanup(ctx, project, issue, generation); err != nil {
		return err
	}
	if issue != tree {
		return nil
	}
	pvc := TreeClaimName(claim.Token("legion-" + project + "-" + tree + "-architect"))
	deleting, cancel = call(ctx)
	err = r.kube.CoreV1().PersistentVolumeClaims(r.namespace).Delete(deleting, pvc, metav1.DeleteOptions{})
	cancel()
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("cleanup root issue %s: delete tree PVC %s after Sandbox confirmation: %w", issue, pvc, err)
	}
	return r.awaitPVCDeleted(ctx, pvc)
}

func (r *Runtime) awaitSandboxDeleted(ctx context.Context, name string) error {
	deadline := time.NewTimer(r.bootTimeout)
	defer deadline.Stop()
	for {
		reading, cancel := call(ctx)
		_, err := r.sandboxClient().Get(reading, name, metav1.GetOptions{})
		cancel()
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("confirm Sandbox %s deletion: %w", name, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("confirm Sandbox %s deletion: timed out after %s", name, r.bootTimeout)
		case <-time.After(recheckInterval):
		}
	}
}

func (r *Runtime) awaitPVCDeleted(ctx context.Context, name string) error {
	deadline := time.NewTimer(r.bootTimeout)
	defer deadline.Stop()
	for {
		reading, cancel := call(ctx)
		_, err := r.kube.CoreV1().PersistentVolumeClaims(r.namespace).Get(reading, name, metav1.GetOptions{})
		cancel()
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("confirm tree PVC %s deletion: %w", name, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("confirm tree PVC %s deletion: timed out after %s", name, r.bootTimeout)
		case <-time.After(recheckInterval):
		}
	}
}

// AdoptWorkingCopy has the agent's shim set its working copy's author (the shared `jj metaedit
// --update-author`, run in the pod's own workspace on the tree volume) over the claim's
// connection. The recorded process must still be the claim's running pod: the connection is its.
func (r *Runtime) AdoptWorkingCopy(ctx context.Context, loc runtime.Locator, id runtime.GitIdentity) error {
	if err := r.checkLocator(loc); err != nil {
		return err
	}
	view, err := r.view(loc.Sandbox.Name)
	if err != nil {
		return fmt.Errorf("adopt %s's working copy: %w", loc.Claim, err)
	}
	if view.pod == nil || string(view.pod.UID) != loc.Sandbox.PodUID || terminal(view.pod) {
		return fmt.Errorf("adopt %s's recorded role process %s is not in its running issue pod", loc.Claim, loc.Incarnation)
	}
	conn, ok := r.conns.Conn(loc.Claim)
	if !ok {
		return fmt.Errorf("adopt %s's working copy: the claim has no connection to ask over", loc.Claim)
	}
	if err := conn.AdoptWorkingCopy(ctx, id, r.adoptTimeout); err != nil {
		return fmt.Errorf("adopt %s's working copy for %s <%s>: %w", loc.Claim, id.Name, id.Email, err)
	}
	return nil
}

// ReconcileOrphans deletes the project's Sandboxes that belong to no known claim, once older than
// grace — what a crash between creating a Sandbox and persisting its claim leaves behind, or what
// a claim retired without its release leaves. known is every claim the daemon has not retired, a
// suspended one included, since its Sandbox holds its session and, for a root, the tree volume.
// The daemon reads known before it calls this, and a retry after boot sweeps at a grace of 0
// while claims launch, so a Sandbox this runtime launched and has not released is never an
// orphan either, known or not: a claim launched after that read is missing from known. The image
// probe's Sandbox (labelled legion.dev/probe) is no claim's and never an orphan: the probe deletes
// it, and its shutdown time has the controller delete it otherwise (probe.go). The located ones
// join the watch, unless it already holds a newer incarnation of the claim, and are evaluated at
// once. Nothing here lists Secrets: each goes with its Sandbox.
func (r *Runtime) ReconcileOrphans(ctx context.Context, known []runtime.Known, grace time.Duration) error {
	var errs []error
	names := map[string]bool{}
	for _, k := range known {
		names[SandboxName(k.Claim)] = true
		if err := k.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("reconcile orphans: %w", err))
			continue
		}
		if k.Locator == nil {
			continue
		}
		if err := r.checkLocator(*k.Locator); err != nil {
			errs = append(errs, fmt.Errorf("reconcile orphans: %w", err))
			continue
		}
		r.adopt(*k.Locator)
	}
	for _, obj := range r.sandboxes.GetStore().List() {
		u := obj.(*unstructured.Unstructured)
		if names[u.GetName()] || u.GetLabels()[labelProbe] != "" || u.GetDeletionTimestamp() != nil {
			continue
		}
		if age := r.now().Sub(u.GetCreationTimestamp().Time); age < grace {
			continue
		}
		if err := r.deleteOrphan(ctx, u); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// own records that this runtime is launching the claim's Sandbox, before the Sandbox can exist,
// so the orphan sweep keeps it until the claim is released.
func (r *Runtime) own(token claim.Token) {
	r.launchedMu.Lock()
	defer r.launchedMu.Unlock()
	r.launched[SandboxName(token)] = true
}

// disown hands the claim's Sandbox back to the orphan sweep's known-claims rule.
func (r *Runtime) disown(token claim.Token) {
	r.launchedMu.Lock()
	defer r.launchedMu.Unlock()
	delete(r.launched, SandboxName(token))
}

// deleteOrphan deletes u, the Sandbox as the store held it, unless this runtime launched its
// claim and has not released it. launchedMu is held from the check through the delete, so a
// launch that begins meanwhile waits for the delete and then finds the Sandbox deleted, never
// losing the one it took up.
func (r *Runtime) deleteOrphan(ctx context.Context, u *unstructured.Unstructured) error {
	r.launchedMu.Lock()
	defer r.launchedMu.Unlock()
	if r.launched[u.GetName()] {
		return nil
	}
	deleting, cancel := call(ctx)
	defer cancel()
	uid := u.GetUID()
	err := r.sandboxClient().Delete(deleting, u.GetName(), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
	switch {
	case err == nil:
		r.log.Info("sandbox runtime: deleted an orphaned sandbox", "sandbox", u.GetName(), "uid", uid)
	case apierrors.IsNotFound(err) || apierrors.IsConflict(err):
	default:
		return fmt.Errorf("reconcile orphans: delete sandbox %s: %w", u.GetName(), err)
	}
	return nil
}
