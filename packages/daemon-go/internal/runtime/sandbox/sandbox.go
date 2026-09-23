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
	"net"
	"path/filepath"
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
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

var _ runtime.Runtime = (*Runtime)(nil)

// defaultTreeVolume is the tree volume's size when Options leaves it zero.
var defaultTreeVolume = resource.MustParse("20Gi")

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
	dispatchURL                             string
	natsURLs                                []string
	tools                                   Tools
	agent                                   []string
	bootTimeout                             time.Duration
	bootIntervals                           int
	terminationGrace                        time.Duration
	probeInterval                           time.Duration
	adoptTimeout                            time.Duration
	tokens                                  ProvisionTokens
	conns                                   runtime.Conns
	now                                     func() time.Time
	log                                     *slog.Logger

	dyn       dynamic.Interface
	kube      kubernetes.Interface
	sandboxes cache.SharedIndexInformer
	pods      cache.SharedIndexInformer

	mu sync.Mutex
	// changed is closed and replaced on every informer event, waking every wait.
	changed chan struct{}
	// watch is every claim's recorded incarnation (the watch of the interfaces block): filled by
	// Spawn's and Resume's results and by ReconcileOrphans' located claims; an entry leaves on
	// Suspend, on Release, and once a Gone or NotRecordedProcess for it is delivered.
	watch map[claim.Token]runtime.Locator
	// observer is the running Observe, nil when none runs.
	observer *observer
	// trees serializes the launches of one tree's pods (relaunch.go, awaitTreeInitialized).
	trees map[string]chan struct{}
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
	return r, nil
}

// configure checks opts and fills their defaults, touching no cluster.
func configure(opts Options) (*Runtime, error) {
	refuse := func(format string, args ...any) (*Runtime, error) {
		return nil, fmt.Errorf("sandbox runtime: "+format, args...)
	}
	switch {
	case opts.Namespace == "":
		return refuse("no namespace")
	case opts.Project == "":
		return refuse("no project")
	case !strings.Contains(opts.Image, "@sha256:"):
		return refuse("image %q is not pinned by digest (…@sha256:…)", opts.Image)
	case opts.StorageClass == "":
		return refuse("no storage class for the tree volume (the cluster has no default class to fall back on)")
	case opts.BootTimeout <= 0 || opts.TerminationGrace <= 0 || opts.ProbeInterval <= 0 || opts.AdoptTimeout <= 0:
		return refuse("the boot timeout, termination grace, probe interval, and adoption timeout must be positive")
	case opts.BootIntervals <= 0:
		return refuse("the registration deadline must be a positive number of boot intervals")
	case opts.Tokens == nil:
		return refuse("no provisioning token source")
	case opts.Conns == nil:
		return refuse("no connection directory")
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
	} {
		if !filepath.IsAbs(tool.path) {
			return refuse("the image's %s path %q is not absolute", tool.name, tool.path)
		}
	}
	r := &Runtime{
		namespace: opts.Namespace, project: opts.Project, image: opts.Image, storageClass: opts.StorageClass,
		treeVolume: opts.TreeVolume, scheduling: opts.Scheduling, resources: opts.Resources,
		streamURL: opts.StreamURL, daemonURL: opts.DaemonURL, envoyURL: opts.EnvoyURL, dispatchURL: opts.DispatchURL,
		natsURLs: opts.NATSURLs, tools: opts.Tools, agent: opts.Agent,
		bootTimeout: opts.BootTimeout, bootIntervals: opts.BootIntervals, terminationGrace: opts.TerminationGrace,
		probeInterval: opts.ProbeInterval, adoptTimeout: opts.AdoptTimeout,
		tokens: opts.Tokens, conns: opts.Conns, now: opts.Now, log: opts.Log,
		changed: make(chan struct{}), watch: map[claim.Token]runtime.Locator{}, trees: map[string]chan struct{}{},
	}
	if r.treeVolume.IsZero() {
		r.treeVolume = defaultTreeVolume
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
	selectProject := func(o *metav1.ListOptions) { o.LabelSelector = labelProject + "=" + r.project }
	r.sandboxes = dynamicinformer.NewFilteredDynamicInformer(dyn, sandboxGVR, r.namespace, 0, cache.Indexers{}, selectProject).Informer()
	r.pods = coreinformers.NewFilteredPodInformer(kube, r.namespace, 0, cache.Indexers{}, selectProject)
	failed := make(chan error, 1)
	for _, informer := range []cache.SharedIndexInformer{r.sandboxes, r.pods} {
		if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj any) { r.notify(obj) },
			UpdateFunc: func(_, obj any) { r.notify(obj) },
			DeleteFunc: func(obj any) { r.notify(obj) },
		}); err != nil {
			return fmt.Errorf("sandbox runtime: %w", err)
		}
		if err := informer.SetWatchErrorHandler(func(_ *cache.Reflector, err error) {
			r.log.Warn("sandbox runtime: list or watch failed", "err", err)
			select {
			case failed <- err:
			default:
			}
		}); err != nil {
			return fmt.Errorf("sandbox runtime: %w", err)
		}
	}
	running, stop := context.WithCancel(ctx)
	go r.sandboxes.RunWithContext(running)
	go r.pods.RunWithContext(running)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for !r.synced() {
		select {
		case <-ctx.Done():
			stop()
			return fmt.Errorf("sandbox runtime: the informers did not sync: %w", ctx.Err())
		case err := <-failed:
			stop()
			return fmt.Errorf("sandbox runtime: listing the namespace %s's Sandboxes and pods failed before the stores synced: %w",
				r.namespace, err)
		case <-tick.C:
		}
	}
	context.AfterFunc(ctx, stop)
	return nil
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

// locatorFor is the locator of the claim's pod at uid.
func (r *Runtime) locatorFor(token claim.Token, uid types.UID) runtime.Locator {
	return runtime.Locator{
		Runtime: runtime.RuntimeSandbox, Claim: token, Incarnation: string(uid),
		Sandbox: &runtime.SandboxLocator{Namespace: r.namespace, Name: SandboxName(token)},
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

// ControllerLaunch is "operator": a cluster has no terminal for the daemon to open the
// controller in, so the operator starts it on their own machine (`legion controller start`).
func (r *Runtime) ControllerLaunch() runtime.ControllerLaunch {
	return runtime.ControllerLaunchOperator
}

// Suspend stops loc's process and keeps the claim's Sandbox, its Secret, and the tree volume for
// a later Resume (decision 3b): a shutdown frame when the claim has a live connection, a wait of
// up to the termination grace for the process to end itself, then `operatingMode: Suspended`, in
// which the controller deletes the pod.
//
// A locator that is not the claim's current pod is already stopped (decision 3d): a pod that is
// the watch's newer incarnation of the claim — a relaunch by the daemon — is left alone, while a
// pod recorded nowhere, one the controller recreated from the claim's template after a hand
// deletion, is suspended all the same, so it cannot keep running on a valid token.
func (r *Runtime) Suspend(ctx context.Context, loc runtime.Locator) error {
	if err := r.checkLocator(loc); err != nil {
		return err
	}
	if !r.synced() {
		return fmt.Errorf("suspend %s: the stores have not synced, so its process cannot be verified", loc.Claim)
	}
	s, err := r.storedSandbox(loc.Sandbox.Name)
	if err != nil {
		return fmt.Errorf("suspend %s: %w", loc.Claim, err)
	}
	if s == nil {
		r.log.Info("sandbox runtime: nothing to suspend; the claim's sandbox is absent", "claim", loc.Claim)
		r.forgetIf(loc)
		return nil
	}
	pod := r.storedPod(loc.Sandbox.Name)
	if !ownedBy(pod, s.UID) {
		pod = nil
	}
	newer, hasNewer := r.recorded(loc.Claim)
	hasNewer = hasNewer && newer.Incarnation != loc.Incarnation
	switch {
	case pod != nil && string(pod.UID) == loc.Incarnation:
		if err := r.stopGracefully(ctx, loc, s); err != nil {
			return fmt.Errorf("suspend %s: %w", loc.Claim, err)
		}
	case pod != nil && hasNewer && string(pod.UID) == newer.Incarnation, pod == nil && hasNewer:
		r.log.Info("sandbox runtime: not suspending a newer incarnation of the claim; the recorded one is already stopped",
			"claim", loc.Claim, "recorded", loc.Incarnation, "newer", newer.Incarnation)
		return nil
	case pod != nil:
		r.log.Warn("sandbox runtime: suspending a pod recorded by no locator; the recorded one is already stopped",
			"claim", loc.Claim, "recorded", loc.Incarnation, "pod", pod.UID)
		if err := r.setMode(ctx, s, modeSuspended); err != nil {
			return fmt.Errorf("suspend %s: %w", loc.Claim, err)
		}
	default:
		r.log.Info("sandbox runtime: the recorded process is already stopped", "claim", loc.Claim, "recorded", loc.Incarnation)
		if s.mode() != modeSuspended {
			if err := r.setMode(ctx, s, modeSuspended); err != nil {
				return fmt.Errorf("suspend %s: %w", loc.Claim, err)
			}
		}
	}
	r.forgetIf(loc)
	return nil
}

// stopGracefully sends the claim's connection a shutdown frame while its pod still runs, waits up
// to the termination grace for the pod to end, and suspends the Sandbox.
func (r *Runtime) stopGracefully(ctx context.Context, loc runtime.Locator, s *sandbox) error {
	r.shutdown(ctx, loc.Claim, r.terminationGrace)
	return r.setMode(ctx, s, modeSuspended)
}

// shutdown asks the claim's live agent to end its own process and waits, up to grace, for its pod
// to stop running. The destructive step that follows is the caller's, so a frame that could not
// be sent, or a process still running at the grace, is logged, never an error.
func (r *Runtime) shutdown(ctx context.Context, token claim.Token, grace time.Duration) {
	name := SandboxName(token)
	running := func() bool {
		pod := r.storedPod(name)
		return pod != nil && !terminal(pod)
	}
	conn, ok := r.conns.Conn(token)
	if !ok || grace <= 0 || !running() {
		return
	}
	if err := conn.Shutdown(ctx); err != nil {
		r.log.Warn("sandbox runtime: shutdown frame not sent", "claim", token, "err", err)
		return
	}
	if err := r.await(ctx, grace, "the pod to stop after its shutdown frame", func() (bool, error) { return !running(), nil }); err != nil {
		r.log.Warn("sandbox runtime: the pod did not stop within its grace", "claim", token, "err", err)
	}
}

// setMode sets the Sandbox's operating mode, fenced to the Sandbox read (a JSON patch whose first
// operation tests the uid): a Sandbox replaced in between is never written.
func (r *Runtime) setMode(ctx context.Context, s *sandbox, mode string) error {
	return r.patch(ctx, s, jsonPatchOp{Op: "add", Path: "/spec/operatingMode", Value: mode})
}

// Release ends the claim (decision 3c): a shutdown frame when the claim has a live connection,
// a wait of up to grace, then the Sandbox deleted by name, whatever loc says — the claim is over.
// The Secret and a root's tree volume go with the Sandbox through their owner references. A
// Sandbox already absent is released.
func (r *Runtime) Release(ctx context.Context, c claim.Token, loc *runtime.Locator, grace time.Duration) error {
	if loc != nil {
		if err := r.checkLocator(*loc); err != nil {
			return err
		}
	}
	r.shutdown(ctx, c, grace)
	name := SandboxName(c)
	deleting, cancel := call(ctx)
	defer cancel()
	background := metav1.DeletePropagationBackground
	err := r.sandboxClient().Delete(deleting, name, metav1.DeleteOptions{PropagationPolicy: &background})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("release %s: delete sandbox %s: %w", c, name, err)
	}
	r.forget(c)
	return nil
}

// AdoptWorkingCopy has the agent's shim set its working copy's author (the shared `jj metaedit
// --update-author`, run in the pod's own workspace on the tree volume) over the claim's
// connection. The recorded process must still be the claim's running pod: the connection is its.
func (r *Runtime) AdoptWorkingCopy(ctx context.Context, loc runtime.Locator, id runtime.GitIdentity) error {
	if err := r.checkLocator(loc); err != nil {
		return err
	}
	view, err := r.view(ctx, loc.Sandbox.Name)
	if err != nil {
		return fmt.Errorf("adopt %s's working copy: %w", loc.Claim, err)
	}
	if view.pod == nil || string(view.pod.UID) != loc.Incarnation || terminal(view.pod) {
		return fmt.Errorf("adopt %s's working copy: the recorded process %s is not the claim's running pod", loc.Claim, loc.Incarnation)
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
// The located ones join the watch and are evaluated at once. Nothing here lists Secrets: each goes
// with its Sandbox.
func (r *Runtime) ReconcileOrphans(ctx context.Context, known []runtime.Known, grace time.Duration) error {
	if !r.synced() {
		return errors.New("reconcile orphans: the stores have not synced")
	}
	var errs []error
	names := map[string]bool{}
	for _, k := range known {
		names[SandboxName(k.Claim)] = true
		if k.Locator == nil {
			continue
		}
		if err := r.checkLocator(*k.Locator); err != nil {
			errs = append(errs, fmt.Errorf("reconcile orphans: %w", err))
			continue
		}
		r.join(*k.Locator)
	}
	for _, obj := range r.sandboxes.GetStore().List() {
		u := obj.(*unstructured.Unstructured)
		if names[u.GetName()] || u.GetDeletionTimestamp() != nil {
			continue
		}
		if age := r.now().Sub(u.GetCreationTimestamp().Time); age < grace {
			continue
		}
		deleting, cancel := call(ctx)
		uid := u.GetUID()
		err := r.sandboxClient().Delete(deleting, u.GetName(), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
		cancel()
		switch {
		case err == nil:
			r.log.Info("sandbox runtime: deleted an orphaned sandbox", "sandbox", u.GetName(), "uid", uid)
		case apierrors.IsNotFound(err) || apierrors.IsConflict(err):
		default:
			errs = append(errs, fmt.Errorf("reconcile orphans: delete sandbox %s: %w", u.GetName(), err))
		}
	}
	return errors.Join(errs...)
}
