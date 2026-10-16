// Package sandbox runs each issue in one Agent Sandbox (agents.x-k8s.io/v1beta1 Sandbox,
// kubernetes-sigs/agent-sandbox v1.0.3). Six role containers share its workspace and network;
// each has a private launcher that starts and stops only its own worker-shim child. The project
// controller (`controller: daemon`) runs in a Sandbox of its own whose pod has the controller's
// launcher alone. The two are the runtime's two kinds of pod (podKind): one supervision path runs
// both, and each kind builds the facets of its pod the other does not.
//
// Process Suspend leaves the shared Sandbox running, and so does Release of a claim on an issue.
// The durable issue close first stops every stored role, then sets its Sandbox Suspended and waits
// for the pod to disappear, retaining the volume and sessions until linger cleanup. Re-admission
// resumes the Sandbox, while an individual role's recovery in a healthy pod does not replace that
// pod. The controller's Sandbox is the controller's claim's, and goes with its Release or, once
// the claim is retired, with the orphan sweep.
//
// The Sandbox structs are Legion's own (types.go): importing sigs.k8s.io/agent-sandbox would move
// grpc, otel, and controller-runtime for every module under go.work.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
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
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

var _ runtime.Runtime = (*Runtime)(nil)

// apiTimeout bounds one call the runtime makes: each request of its requests' Kubernetes client,
// from the moment the client's rate limiter lets it go (requestConfig), and any call its clients do
// not bound whole (call).
const apiTimeout = 30 * time.Second

// recheckInterval is how often a wait re-reads what it waits on even when no informer event
// arrived, since a wait's condition can include an API read that no event announces.
const recheckInterval = 500 * time.Millisecond

// Runtime is the Agent Sandbox runtime. Build one with New.
type Runtime struct {
	namespace, project, image, storageClass string
	// probeRun tells this runtime's image probe Sandbox from every other creator's (probeName): ""
	// for the daemon's runtime, whose probe is one per project and image, and a suffix drawn for
	// each run by a Prober (newProber), so a probe-boot and a booting daemon never take each other's
	// probe for a leftover of their own.
	probeRun                       string
	volumeSize                     resource.Quantity
	scheduling                     Scheduling
	resources                      map[claim.Role]corev1.ResourceRequirements
	streamURL, daemonURL, envoyURL string
	dispatchURL, dispatchToken     string
	natsURLs                       []string
	tools                          Tools
	pod                            Pod
	providerKeys                   map[string]string
	providersSecrets               []string
	natsUser                       string
	sessionDSNKey                  string
	agentSecrets                   *AgentSecrets
	agent                          []string
	bootTimeout                    time.Duration
	busyBackoff                    time.Duration
	provisionBound                 time.Duration
	terminationGrace               time.Duration
	probeInterval                  time.Duration
	adoptTimeout                   time.Duration
	tokens                         ProvisionTokens
	gitHubCredential               runtime.GitHubCredential
	store                          Store
	conns                          runtime.Conns
	launchers                      *launchers
	launcherAuth                   launcherCredentials
	now                            func() time.Time
	log                            *slog.Logger

	// lifetime is the informers' context; a read no single caller waits for
	// (readLauncherCredential) ends with it.
	lifetime  context.Context
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
	// podTurns serializes the relaunches, issue suspension, release and orphan deletion of one pod,
	// keyed by its Sandbox's name (lockPod). Nothing serializes the pods of one tree against each
	// other: each issue's pod provisions its own clone on its own volume.
	podTurns map[string]chan struct{}
}

// New builds the runtime from opts, starts its Sandbox and pod informers for ctx's lifetime, and
// returns once both stores have synced. A list or watch that fails before then is a refusal
// naming the failure: a runtime that cannot see its Sandboxes can verify nothing.
func New(ctx context.Context, rc *rest.Config, opts Options) (*Runtime, error) {
	r, err := configure(opts)
	if err != nil {
		return nil, err
	}
	c, err := clients(rc)
	if err != nil {
		return nil, err
	}
	if err := r.start(ctx, c); err != nil {
		return nil, err
	}
	return r, nil
}

// configure checks opts and fills their defaults, touching no cluster: everything the runtime reads
// for any work (configureProber), then supervision's own requirements (checkSupervision).
func configure(opts Options) (*Runtime, error) {
	r, err := configureProber(opts)
	if err != nil {
		return nil, err
	}
	if err := checkSupervision(opts); err != nil {
		return nil, err
	}
	return r, nil
}

// checkSupervision refuses opts that lack what only supervision reads — launching, relaunching,
// adopting and sweeping a claim's pod: the provisioning token source, the roles' gh-files
// function, the store, the connection directory, and a worker stream address a pod can dial. A
// runtime that runs the image probe alone (newProber) reads none of them.
func checkSupervision(opts Options) error {
	refuse := func(format string, args ...any) error { return fmt.Errorf("sandbox runtime: "+format, args...) }
	switch {
	case opts.Tokens == nil:
		return refuse("no provisioning token source")
	case opts.GitHubCredential == nil:
		return refuse("no github credential function for the roles' gh files")
	case opts.Store == nil:
		return refuse("no store")
	case opts.Conns == nil:
		return refuse("no connection directory")
	}
	host, ok := strings.CutPrefix(opts.StreamURL, "tcp://")
	if _, _, err := net.SplitHostPort(host); !ok || err != nil {
		return refuse("stream URL %q is not tcp://host:port, which a pod can dial", opts.StreamURL)
	}
	return nil
}

// configureProber checks what every runtime reads, the image probe's own included, and fills the
// defaults, touching no cluster. A runtime it builds alone runs the image probe and nothing else
// (newProber); configure adds supervision's requirements to it.
func configureProber(opts Options) (*Runtime, error) {
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
		return refuse("no storage class for the issue volume (the cluster has no default class to fall back on)")
	case opts.IssueVolume.Sign() <= 0:
		return refuse("no issue volume size: %s is not a positive quantity", opts.IssueVolume.String())
	case opts.BootTimeout <= 0 || opts.TerminationGrace <= 0 || opts.ProbeInterval <= 0 || opts.AdoptTimeout <= 0:
		return refuse("the boot timeout, termination grace, probe interval, and adoption timeout must be positive")
	case opts.BusyBackoff <= 0:
		return refuse("the busy backoff %s is not positive", opts.BusyBackoff)
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
	for _, tool := range []struct{ name, path string }{
		{"gh", opts.Tools.GH}, {"git", opts.Tools.Git}, {"jj", opts.Tools.JJ}, {"legion", opts.Tools.Legion},
		{"agent-secrets", opts.Tools.AgentSecrets},
	} {
		if !filepath.IsAbs(tool.path) {
			return refuse("the image's %s path %q is not absolute", tool.name, tool.path)
		}
	}
	// Every container of every pod the runtime builds takes its role's entry of Resources — the
	// init containers the launching role's, the image probe's the controller's — so a role without
	// a reservation would run with no requests or limits at all, a silently BestEffort pod.
	for _, role := range launcherRoles {
		if err := checkReservation(role, opts.Resources); err != nil {
			return refuse("%v", err)
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
	if err := CheckPod(opts.Pod, opts.ProviderKeys, opts.Tools, opts.LaunchSecrets, opts.ProvidersSecrets, opts.SessionDSNKey); err != nil {
		return refuse("%v", err)
	}
	for _, name := range opts.ProvidersSecrets {
		if !slices.Contains(opts.LaunchSecrets, name) {
			return refuse("providers secret %s is not a launch secret (%s)", name, strings.Join(opts.LaunchSecrets, ", "))
		}
	}
	r := &Runtime{
		namespace: opts.Namespace, project: opts.Project, image: opts.Image, storageClass: opts.StorageClass,
		volumeSize: opts.IssueVolume, scheduling: opts.Scheduling, resources: opts.Resources,
		streamURL: opts.StreamURL, daemonURL: opts.DaemonURL, envoyURL: opts.EnvoyURL, dispatchURL: opts.DispatchURL,
		dispatchToken: opts.DispatchToken, natsURLs: opts.NATSURLs, tools: opts.Tools, agentSecrets: opts.AgentSecrets,
		pod: opts.Pod, providerKeys: opts.ProviderKeys, providersSecrets: slices.Sorted(slices.Values(opts.ProvidersSecrets)), natsUser: opts.NATSUser,
		sessionDSNKey: opts.SessionDSNKey,
		bootTimeout:   opts.BootTimeout, busyBackoff: opts.BusyBackoff, provisionBound: workspace.FetchTimeout, terminationGrace: opts.TerminationGrace,
		probeInterval: opts.ProbeInterval, adoptTimeout: opts.AdoptTimeout, agent: opts.Agent,
		tokens: opts.Tokens, gitHubCredential: opts.GitHubCredential, store: opts.Store, conns: opts.Conns, now: opts.Now, log: opts.Log,
		changed: make(chan struct{}), watch: map[claim.Token]runtime.Locator{}, podTurns: map[string]chan struct{}{},
		launchers: newLaunchers(),
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

// checkReservation is why resources' entry for role is not a reservation, or nil when it is one:
// cpu and memory each requested as a positive quantity equal to its limit, the kubelet's Guaranteed
// rule for the one container; ephemeral-storage requested as a positive quantity no greater than
// its positive limit — QoS reads cpu and memory alone, so the disk bound need not be one value as
// both: it is the node-disk isolation between pods of unrelated trees sharing a node, a limit the
// kubelet evicts the offending pod alone for passing, over a request the scheduler fits to the
// node's allocatable disk; and no other resource named, since the daemon sizes nothing else
// (daemon/kubernetes.go, roleRequirements) and an extended resource here would be one no operator
// configured. A missing role is the first fault: its containers would carry the zero requirements.
func checkReservation(role claim.Role, resources map[claim.Role]corev1.ResourceRequirements) error {
	requirements, ok := resources[role]
	if !ok {
		return fmt.Errorf("resources: no reservation for role %s", role)
	}
	for _, list := range []struct {
		name string
		list corev1.ResourceList
	}{{"requests", requirements.Requests}, {"limits", requirements.Limits}} {
		for _, name := range slices.Sorted(maps.Keys(list.list)) {
			if name != corev1.ResourceCPU && name != corev1.ResourceMemory && name != corev1.ResourceEphemeralStorage {
				return fmt.Errorf("resources: role %s names %s in its %s; a reservation is cpu, memory and ephemeral-storage alone", role, name, list.name)
			}
		}
	}
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		request, limit := requirements.Requests[name], requirements.Limits[name]
		switch {
		case request.Sign() <= 0 || limit.Sign() <= 0:
			return fmt.Errorf("resources: role %s reserves no %s (request %s, limit %s); a reservation is a positive quantity as both", role, name, request.String(), limit.String())
		case request.Cmp(limit) != 0:
			return fmt.Errorf("resources: role %s requests %s %s but is limited to %s; a reservation is one value as both", role, name, request.String(), limit.String())
		}
	}
	request, limit := requirements.Requests[corev1.ResourceEphemeralStorage], requirements.Limits[corev1.ResourceEphemeralStorage]
	switch {
	case request.Sign() <= 0 || limit.Sign() <= 0:
		return fmt.Errorf("resources: role %s bounds no ephemeral-storage (request %s, limit %s); the bound is a positive request under a positive limit", role, request.String(), limit.String())
	case request.Cmp(limit) > 0:
		return fmt.Errorf("resources: role %s requests ephemeral-storage %s past its limit %s; the request may not exceed the limit", role, request.String(), limit.String())
	}
	return nil
}

// start runs both informers (startInformers) and then the GitHub credential refresher
// (refreshGitHubCredentials) on ctx, the informers' lifetime, so it stops with the informers whose
// pod store it walks.
func (r *Runtime) start(ctx context.Context, c runtimeClients) error {
	if err := r.startInformers(ctx, c); err != nil {
		return err
	}
	go r.refreshGitHubCredentialsEvery(ctx, gitHubRefreshInterval)
	return nil
}

// startInformers runs both informers over c's informers' clients, selected on the project label,
// until ctx ends, and waits for both stores to sync; every other request of the runtime goes
// through c's requests' clients, each read and write of them sent again while the API server
// answers a launch's overloaded (patientDynamic, patientKube).
func (r *Runtime) startInformers(ctx context.Context, c runtimeClients) error {
	r.dyn, r.kube, r.lifetime = patientDynamic{c.requests.dyn, r}, patientKube{c.requests.kube, r}, ctx
	sandboxes := c.informers.dyn.Resource(sandboxGVR).Namespace(r.namespace)
	pods := c.informers.kube.CoreV1().Pods(r.namespace)
	r.sandboxFeed.resource, r.podFeed.resource = "Sandbox", "pod"
	r.sandboxes = r.informer(&r.sandboxFeed, c.informers.dyn, &unstructured.Unstructured{},
		func(ctx context.Context, o metav1.ListOptions) (k8sruntime.Object, error) {
			return sandboxes.List(ctx, o)
		},
		func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) {
			return sandboxes.Watch(ctx, o)
		})
	r.pods = r.informer(&r.podFeed, c.informers.kube, &corev1.Pod{},
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

// call is ctx bounded for one call no client of the runtime bounds whole: a GitHub App token mint;
// a pod log Stream, which client-go bounds by the client's timeout per attempt and not across its
// Retry-After retries (rest/request.go, Request.Stream), so a log read fails fast rather than hold
// its caller; and a failed launch's suspension of its Sandbox (suspendFailedLaunch), which waits an
// overloaded server out for no longer than this.
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
// true, fails, or budget passes; what names the wait in the timeout. done is handed ctx bounded by
// budget, and that one bound is the wait's: a request done makes, a launch's waiting out an
// overloaded server included (resend), ends with the wait rather than up to a boot timeout past it.
func (r *Runtime) await(ctx context.Context, budget time.Duration, what string, done func(context.Context) (bool, error)) error {
	bounded, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	recheck := time.NewTicker(recheckInterval)
	defer recheck.Stop()
	for {
		r.mu.Lock()
		changed := r.changed
		r.mu.Unlock()
		ok, err := done(bounded)
		if err != nil || ok {
			return err
		}
		select {
		case <-bounded.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
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

// imageDrift is the image a container of p — a role's or an init container — runs that is not
// image, the one this runtime launches every container with (r.image), and whether there is one.
// A pod is built whole from one image, so one container's drift is the pod's: it was launched by
// another release of the daemon, and its agents call this daemon as that release — for a route it
// no longer serves, a variable it no longer sets — until the pod is replaced. relaunch replaces
// such a pod at the next launch of any of its roles, as it does one holding a moved address
// (movedInPod), and ReconcileOrphans warns of each one it finds. A nil pod holds no drift: it is
// replaced for being absent.
func imageDrift(p *corev1.Pod, image string) (string, bool) {
	if p == nil {
		return "", false
	}
	for _, containers := range [][]corev1.Container{p.Spec.InitContainers, p.Spec.Containers} {
		for _, c := range containers {
			if c.Image != image {
				return c.Image, true
			}
		}
	}
	return "", false
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

// locatorFor is one role process's address in an issue pod: role's container.
func (r *Runtime) locatorFor(token claim.Token, role claim.Role, uid types.UID, generation uint64) runtime.Locator {
	podUID := string(uid)
	return runtime.Locator{
		Runtime:     runtime.RuntimeSandbox,
		Claim:       token,
		Incarnation: runtime.SandboxIncarnation(podUID, generation),
		Sandbox: &runtime.SandboxLocator{
			Namespace: r.namespace, Name: SandboxName(token), PodUID: podUID, Container: string(role), Generation: generation,
		},
	}
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
// issue's own volume, which goes with the issue's Sandbox.
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
	if state, connected := r.launchers.state(loc.Claim, loc.Sandbox.PodUID); connected && (state.Child == nil || state.Child.Generation != loc.Sandbox.Generation) {
		r.forgetIf(loc)
		return nil
	}
	stopping, cancel := context.WithTimeout(ctx, r.bootTimeout+r.terminationGrace)
	defer cancel()
	if err := r.launchers.stop(stopping, loc.Claim, loc.Sandbox.PodUID, r.stopFrame(loc.Sandbox.Generation)); err != nil {
		return fmt.Errorf("suspend %s: %w", loc.Claim, err)
	}
	r.forgetIf(loc)
	return nil
}

// stopFrame stops generation's child within the runtime's stop grace. Its id is the generation's,
// so a stop the launcher already answered is answered again rather than run twice.
func (r *Runtime) stopFrame(generation uint64) shimwire.LauncherStop {
	return shimwire.LauncherStop{
		ID: "stop-" + strconv.FormatUint(generation, 10), Generation: generation,
		GraceMs: int(math.Ceil(r.terminationGrace.Seconds() * 1000)),
	}
}

// setMode sets the Sandbox's operating mode, fenced to the Sandbox read (a JSON patch whose first
// operation tests the uid): a Sandbox replaced in between is never written.
func (r *Runtime) setMode(ctx context.Context, s *sandbox, mode string) error {
	_, err := r.patch(ctx, s, jsonPatchOp{Op: "add", Path: "/spec/operatingMode", Value: mode})
	return err
}

// Release is a claim-scoped operation: it ends at most the recorded role process and forgets the
// claim from this runtime. It never suspends or deletes an issue Sandbox, even when this runtime
// sees no sibling role: the issue's Sandbox, its Secrets and its volume are the tree cleanup's
// alone (CleanupTree), which runs once every stored claim of the tree has retired. The project
// controller's Sandbox belongs to the controller's claim and nothing else, so releasing that claim
// deletes it (releaseControllerSandbox).
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
	return r.releaseControllerSandbox(ctx, k.Claim)
}

// releaseControllerSandbox deletes token's Sandbox when the store holds it as this project's
// controller pod (podKindOf), which only the controller's claim names; its Secrets and volume go
// with it through their owner references. Any other Sandbox, an issue's above all, is left as it
// is, and the decision costs no API read and takes no turn. One the store does not hold is the
// orphan sweep's once the claim has retired. The delete runs under the pod's launch turn and is
// fenced to the Sandbox the store showed (deleteFenced): a relaunch of the controller that holds the
// turn meanwhile writes the Sandbox (recordAddresses), so the delete conflicts and leaves the
// Sandbox to the orphan sweep, which judges it again on the claims known then, and a relaunch that
// begins after waits for the delete and creates the Sandbox afresh.
func (r *Runtime) releaseControllerSandbox(ctx context.Context, token claim.Token) error {
	name := SandboxName(token)
	s, err := r.storedSandbox(name)
	if err != nil {
		return fmt.Errorf("release %s: %w", token, err)
	}
	if s == nil || s.Labels[labelProject] != r.project {
		return nil
	}
	kind, err := podKindOf(s.Labels)
	if _, controller := kind.(controllerPod); err != nil || !controller {
		return nil
	}
	release, err := r.lockPod(ctx, name)
	if err != nil {
		return fmt.Errorf("release %s: %w", token, err)
	}
	defer release()
	fence := metav1.Preconditions{UID: &s.UID, ResourceVersion: &s.ResourceVersion}
	if err := r.deleteFenced(ctx, name, fence, "sandbox runtime: deleted a released claim's sandbox", "claim", token); err != nil {
		return fmt.Errorf("release %s: %w", token, err)
	}
	return nil
}

// AdoptWorkingCopy has the agent's shim set its working copy's author (the shared `jj metaedit
// --update-author`, run in the pod's own workspace on the issue's volume) over the claim's
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

// ReconcileOrphans deletes the project's Sandboxes nothing owns, once older than grace. An issue
// Sandbox is owned by its tree's lifecycle: it is an orphan once its tree's cleanup confirmed or
// when it has no lifecycle, which a launch whose create reached the API after its tree's cleanup
// listed it can leave. A Sandbox of a tree whose lifecycle is open or releasing is its cleanup's
// alone (CleanupTree), whatever claims are known: a launch passes its claim's lifecycle check
// before it creates a Sandbox, so what this runtime launches is never swept. The project
// controller's Sandbox is owned by the controller's claim: it is an orphan once no claim of known
// is that claim. known is every claim the daemon has not retired, a suspended one included
// (knownClaims), so a retired controller's Sandbox goes and a suspended one's, which holds its
// session, stays. Every delete runs under the pod's launch turn, so a launch that begins meanwhile
// waits for the delete and then creates the Sandbox afresh, and is fenced to the Sandbox as the
// decision read it, its uid and resourceVersion (deleteFenced): an issue Sandbox's tree is read
// again under the turn, and a Sandbox written since the informer's snapshot — relabelled for a new
// tree by a re-admitted issue's launch (ensureSandbox), which keeps its uid, volume and sessions,
// or set Running — is kept as a conflict for the next sweep to judge on what it holds then.
// Every start writes the Sandbox its generation's address record
// (recordAddresses), so a controller relaunched into it since makes the delete a conflict, and the
// next sweep judges it on the claims known then. The image probe's Sandbox (labelled
// legion.dev/probe) is no claim's and never an orphan: the probe deletes it, and its shutdown time
// has the controller delete it otherwise (probe.go). known's located claims join the watch, unless
// it already holds a newer incarnation of the claim, and are evaluated at once. Nothing here lists
// Secrets: each goes with its Sandbox. Each Sandbox whose pod runs an image other than the
// runtime's (imageDrift) is warned of, named with both images, at the boot reconciliation and at
// every sweep until a relaunch of one of its roles replaces the pod (warnImageDrift): a release
// whose daemon stops serving a route, or stops setting a variable, that the previous image's agents
// call or read leaves such a pod's agents calling this daemon as the older release, so it is pinned
// only once every tree is drained or every pod relaunched, and the boot log is where an operator
// reads which pods are still to go. Only a warning: refusing the pod would stop it being observed.
func (r *Runtime) ReconcileOrphans(ctx context.Context, known []runtime.Known, grace time.Duration) error {
	var errs []error
	// controllers are the Sandboxes of the known controller claims, which the sweep keeps.
	controllers := map[string]bool{}
	for _, k := range known {
		if role, ok := k.Claim.Role(); ok && role == claim.RoleController {
			controllers[SandboxName(k.Claim)] = true
		}
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
		if u.GetLabels()[labelProbe] != "" || u.GetDeletionTimestamp() != nil {
			continue
		}
		r.warnImageDrift(u)
		if age := r.now().Sub(u.GetCreationTimestamp().Time); age < grace {
			continue
		}
		if err := r.sweep(ctx, u, controllers); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// warnImageDrift logs, for the Sandbox u, the pod it owns in the store when a container of it runs
// an image other than the runtime's (imageDrift): the Sandbox's name, the image found and the
// configured one, as ReconcileOrphans says. A same-named pod u does not own is an earlier
// Sandbox's, on its way out, and is nothing of u's to warn about.
func (r *Runtime) warnImageDrift(u *unstructured.Unstructured) {
	pod := r.storedPod(u.GetName())
	if !ownedBy(pod, u.GetUID()) {
		return
	}
	if other, drifted := imageDrift(pod, r.image); drifted {
		r.log.Warn(fmt.Sprintf("sandbox runtime: %s runs image %s, the configured image is %s; its agents call this daemon as the older release until the pod is relaunched", u.GetName(), other, r.image),
			"sandbox", u.GetName(), "pod", pod.UID)
	}
}

// sweep deletes u, the Sandbox as the informer held it, unless something owns it
// (ReconcileOrphans), by its kind (podKindOf): the controller's pod is kept while it is one of
// controllers, the Sandboxes of the known controller claims; an issue pod while its tree's lifecycle
// is live, or while its issue's Sandbox belongs to the tree it is labelled with
// (Store.IssueSandboxHome): a carried child's Sandbox the old tree's cleanup handed over
// (handOverSandbox), while the child waits for the admission that opens its tree or lingers after
// its own close. One whose child left the line, or whose linger expired, belongs nowhere and is
// deleted, as every other orphan is, with the line that names it. A live tree is checked before any
// turn is taken, so a Sandbox a concurrent launch of the same issue is still creating or waiting on
// is never serialized behind that launch: the common case, where the tree stays live, costs no turn
// at all. Only once the tree looks closed does the pod's launch turn apply, held from a second,
// authoritative read through the delete (lockPod), so a launch that begins meanwhile still waits for
// the delete and then creates the Sandbox afresh. A Sandbox whose labels name no kind, or whose
// issue and tree labels are no keys, is kept and reported: what cannot be told apart from a live
// tree's is never deleted.
func (r *Runtime) sweep(ctx context.Context, u *unstructured.Unstructured, controllers map[string]bool) error {
	kind, err := podKindOf(u.GetLabels())
	if err != nil {
		return fmt.Errorf("reconcile orphans: Sandbox %s: %v; kept", u.GetName(), err)
	}
	name, uid := u.GetName(), u.GetUID()
	switch kind.(type) {
	case controllerPod:
		if controllers[name] {
			return nil
		}
		release, err := r.lockPod(ctx, name)
		if err != nil {
			return err
		}
		defer release()
		version := u.GetResourceVersion()
		fence := metav1.Preconditions{UID: &uid, ResourceVersion: &version}
		if err := r.deleteFenced(ctx, name, fence, "sandbox runtime: deleted an orphaned sandbox", "role", claim.RoleController); err != nil {
			return fmt.Errorf("reconcile orphans: %w", err)
		}
		return nil
	}
	issue, tree := u.GetLabels()[labelIssue], u.GetLabels()[labelTree]
	if !claim.IsIssueKey(issue) || !claim.IsIssueKey(tree) {
		return fmt.Errorf("reconcile orphans: Sandbox %s names no issue and tree (labels %s=%q, %s=%q); kept", name, labelIssue, issue, labelTree, tree)
	}
	if live, err := r.store.TreeLive(ctx, r.project, tree); err != nil {
		return fmt.Errorf("reconcile orphans: %w", err)
	} else if live {
		return nil
	}
	release, err := r.lockPod(ctx, name)
	if err != nil {
		return err
	}
	defer release()
	live, err := r.store.TreeLive(ctx, r.project, tree)
	if err != nil {
		return fmt.Errorf("reconcile orphans: %w", err)
	}
	if live {
		return nil
	}
	if home, err := r.store.IssueSandboxHome(ctx, r.project, issue); err != nil {
		return fmt.Errorf("reconcile orphans: %w", err)
	} else if home == tree {
		return nil
	}
	// Fenced to the uid and resourceVersion the decision read: a re-admission of this issue that
	// landed between the informer's snapshot and this turn relabelled the same Sandbox for its new
	// tree (ensureSandbox) and may already run a pod in it; the uid alone would not tell.
	version := u.GetResourceVersion()
	fence := metav1.Preconditions{UID: &uid, ResourceVersion: &version}
	if err := r.deleteFenced(ctx, name, fence, "sandbox runtime: deleted an orphaned sandbox", "tree", tree); err != nil {
		return fmt.Errorf("reconcile orphans: %w", err)
	}
	return nil
}

// deleteFenced deletes the Sandbox name once, fenced by fence, and forgets its launchers'
// credentials; msg and attrs log the delete. Its caller holds the pod's launch turn. One already
// gone is gone, and one the fence refuses, replaced or written since, is kept and logged with
// attrs, since its owner's next decision is the orphan sweep's: neither is an error. Deletion
// propagates in the foreground, as the tree cleanup's does (deleteSandbox): the Sandbox owns its
// PVC and its role Secrets by owner reference, and the issue's next Sandbox names the same PVC
// (IssueClaimName), so a background delete, which drops the Sandbox at once and collects the PVC
// after, would let a re-admission inside that window create a Sandbox asking for a Terminating
// PVC. In the foreground the Sandbox stays, Terminating, until both are gone, so a launch that
// waits out a Sandbox being deleted (ensureSandbox) waits for its volume too.
func (r *Runtime) deleteFenced(ctx context.Context, name string, fence metav1.Preconditions, msg string, attrs ...any) error {
	policy := metav1.DeletePropagationForeground
	err := r.sandboxClient().Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy, Preconditions: &fence})
	switch {
	case err == nil:
		r.forgetLauncherCredentials(name)
		r.log.Info(msg, append([]any{"sandbox", name, "uid", *fence.UID}, attrs...)...)
	case apierrors.IsNotFound(err):
		r.forgetLauncherCredentials(name)
	case apierrors.IsConflict(err):
		r.log.Info("sandbox runtime: kept a sandbox written since its delete was decided; the orphan sweep decides it again",
			append([]any{"sandbox", name, "uid", *fence.UID, "error", err}, attrs...)...)
	default:
		return fmt.Errorf("delete sandbox %s: %w", name, err)
	}
	return nil
}
