package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

const (
	testNamespace = "legion"
	testProject   = "legion"
	testImage     = "ghcr.io/sjawhar/legion-worker@sha256:1d10089a0000000000000000000000000000000000000000000000000000beef"
	testTree      = "LEGION-208"
)

var (
	rootToken   = claim.Token("legion-legion-legion-208-architect")
	workerToken = claim.Token("legion-legion-legion-208-tester")
	otherToken  = claim.Token("legion-legion-legion-208-reviewer")
	// childToken is a role of another issue of the same tree: its own issue pod.
	childToken = claim.Token("legion-legion-legion-209-implementer")
)

// testOptions are the options every test starts from: production's shape, with budgets short
// enough that a wait the test expects to end never slows the suite.
func testOptions() Options {
	return Options{
		Namespace:    testNamespace,
		Project:      testProject,
		Store:        newTreeStore(),
		Image:        testImage,
		StorageClass: "gp2",
		TreeVolume:   resource.MustParse("20Gi"),
		StreamURL:    "tcp://192.0.2.250:13371",
		DaemonURL:    "http://192.0.2.250:13370",
		EnvoyURL:     "http://192.0.2.250:9020",
		NATSURLs:     []string{"nats://192.0.2.250:4222"},
		Tools: Tools{
			GH: "/usr/local/bin/gh", Git: "/usr/bin/git", JJ: "/usr/local/bin/jj", Legion: "/opt/legion/bin/legion",
			AgentSecrets: "/opt/legion/bin/agent-secrets",
		},
		BootTimeout:      2 * time.Second,
		BootIntervals:    3,
		TerminationGrace: 200 * time.Millisecond,
		ProbeInterval:    time.Hour,
		AdoptTimeout:     time.Minute,
		Tokens:           staticTokens{},
		Conns:            fake.NewConns(),
		Log:              slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
}

// staticTokens mints the same installation token for every owner.
type staticTokens struct{}

func (staticTokens) Token(_ context.Context, owner string) (string, error) {
	return "ghs_provision_" + owner, nil
}

// treeStore is the runtime's durable state, in memory: every tree is live (its lifecycle open)
// unless closed says its cleanup confirmed, and a tree recorded sessions only when sessions says
// so.
type treeStore struct {
	mu               sync.Mutex
	closed, sessions map[string]bool
}

func newTreeStore() *treeStore {
	return &treeStore{closed: map[string]bool{}, sessions: map[string]bool{}}
}

func (s *treeStore) TreeHasSessions(_ context.Context, _, tree string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[tree], nil
}

func (s *treeStore) TreeLive(_ context.Context, _, tree string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed[tree], nil
}

// close records tree's cleanup as confirmed: what the tree left behind is then an orphan.
func (s *treeStore) close(tree string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed[tree] = true
}

// testSpec is a launch of the claim on the tree, with its prompt files under dir.
func testSpec(t *testing.T, token claim.Token, role claim.Role, issue string) runtime.SpawnSpec {
	t.Helper()
	dir := t.TempDir()
	rolePrompt := filepath.Join(dir, "role.md")
	instructions := filepath.Join(dir, "instructions.md")
	if err := os.WriteFile(rolePrompt, []byte("You are the "+string(role)+".\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(instructions, []byte("Deployment instructions.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return runtime.SpawnSpec{
		Claim: token, Project: "legion", Tree: testTree, Issue: issue, Role: role, Generation: 1,
		BootToken: "boot-" + string(token) + "-g1",
		Env:       map[string]string{"JJ_USER": "legion-implement[bot]", "JJ_EMAIL": "1+legion-implement[bot]@users.noreply.github.com"},
		Secrets:   map[string]string{"ENVOY_TOKEN": "envoy-bearer"},
		Prompt: runtime.PromptParts{
			RolePromptPaths: []string{rolePrompt}, Addressing: "Legion addressing: your role topic is `notifications.role." + string(token) + "`.",
			DeploymentInstructionsPath: instructions,
		},
		Repository: ghrepo.MustParse("sjawhar/legion-smoke"),
	}
}

func rootSpec(t *testing.T) runtime.SpawnSpec {
	return testSpec(t, rootToken, claim.RoleArchitect, testTree)
}

func workerSpec(t *testing.T) runtime.SpawnSpec {
	return testSpec(t, workerToken, claim.RoleTester, testTree)
}

func childSpec(t *testing.T) runtime.SpawnSpec {
	return testSpec(t, childToken, claim.RoleImplementer, "LEGION-209")
}

// action is one API request either fake saw, in the one order both saw them.
type action struct {
	verb, resource, subresource, name string
	patch                             string
	// object is what a create sent.
	object k8sruntime.Object
}

func (a action) String() string {
	s := a.verb + " " + a.resource
	if a.subresource != "" {
		s += "/" + a.subresource
	}
	return s + " " + a.name
}

// rig is a runtime over client-go's dynamic and typed fakes, with a stand-in for the Agent
// Sandbox controller: while a Sandbox is Running and holds no pod it creates one it owns, and
// while it is Suspended it deletes the pod it owns — the two behaviours of v1.0.3's reconcilePod
// the runtime depends on (sandbox_controller.go:1256-1287, 1380-1453).
type rig struct {
	t     *testing.T
	ctx   context.Context
	stop  context.CancelFunc
	r     *Runtime
	dyn   *dynamicfake.FakeDynamicClient
	kube  *kubefake.Clientset
	conns *fake.Conns
	now   atomic.Pointer[time.Time]
	// store is the runtime's in-memory durable state, unless an option handed it another.
	store *treeStore

	mu      sync.Mutex
	actions []action
	uids    atomic.Int64

	// hold stops the controller creating pods; autoStart makes a pod it creates scheduled on a
	// node, past workspace-init, and Running at once.
	hold      atomic.Bool
	autoStart atomic.Bool
	// suspendDelay is how long a pod the controller deletes stays terminating first.
	suspendDelay time.Duration
	// noController leaves every object as the test wrote it.
	noController bool
	// hooks, when set, sees the runtime's Sandbox gets and deletes (withSandboxHooks).
	hooks *sandboxHooks

	// commands are what each role's fake launcher was sent, in order; wanted are the roles whose
	// fake launcher connects again whenever their issue gets a new pod.
	commandsMu sync.Mutex
	commands   map[claim.Token][]shimwire.Frame
	wanted     map[claim.Token]bool
}

type rigOption func(*rig, *Options)

func withOptions(edit func(*Options)) rigOption { return func(_ *rig, o *Options) { edit(o) } }

// withoutController runs no controller stand-in: the objects stay as the test wrote them.
func withoutController() rigOption { return func(g *rig, _ *Options) { g.noController = true } }

// sandboxHooks runs beforeDelete as the runtime asks to delete a Sandbox and afterGet once a get
// of one returns, outside the fake client's lock, which the fake holds across a whole call.
type sandboxHooks struct {
	beforeDelete, afterGet func(name string)
}

// withSandboxHooks hands the runtime a dynamic client that runs h around its Sandbox calls.
func withSandboxHooks(h *sandboxHooks) rigOption { return func(g *rig, _ *Options) { g.hooks = h } }

// hookedDynamic embeds the fake itself, not dynamic.Interface, so the informers still see the
// fake's IsWatchListSemanticsUnSupported and list and watch rather than stream a watch-list.
type hookedDynamic struct {
	*dynamicfake.FakeDynamicClient
	hooks *sandboxHooks
}

func (d hookedDynamic) Resource(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	if gvr != sandboxGVR {
		return d.FakeDynamicClient.Resource(gvr)
	}
	return hookedResource{d.FakeDynamicClient.Resource(gvr), d.hooks}
}

type hookedResource struct {
	dynamic.NamespaceableResourceInterface
	hooks *sandboxHooks
}

func (h hookedResource) Namespace(ns string) dynamic.ResourceInterface {
	return hookedSandboxes{h.NamespaceableResourceInterface.Namespace(ns), h.hooks}
}

type hookedSandboxes struct {
	dynamic.ResourceInterface
	hooks *sandboxHooks
}

func (h hookedSandboxes) Delete(ctx context.Context, name string, opts metav1.DeleteOptions, subresources ...string) error {
	h.hooks.beforeDelete(name)
	return h.ResourceInterface.Delete(ctx, name, opts, subresources...)
}

func (h hookedSandboxes) Get(ctx context.Context, name string, opts metav1.GetOptions, subresources ...string) (*unstructured.Unstructured, error) {
	u, err := h.ResourceInterface.Get(ctx, name, opts, subresources...)
	h.hooks.afterGet(name)
	return u, err
}

func newRig(t *testing.T, objects []k8sruntime.Object, options ...rigOption) *rig {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	var dynamicObjects []*unstructured.Unstructured
	var kubeObjects []k8sruntime.Object
	for _, object := range objects {
		if u, ok := object.(*unstructured.Unstructured); ok {
			dynamicObjects = append(dynamicObjects, u)
		} else {
			kubeObjects = append(kubeObjects, object)
		}
	}
	g := &rig{
		t: t, ctx: ctx, stop: stop, conns: fake.NewConns(),
		dyn: newDynamic(t, dynamicObjects...), kube: kubefake.NewClientset(kubeObjects...),
	}
	g.autoStart.Store(true)
	start := rigNow
	g.now.Store(&start)
	g.dyn.PrependReactor("patch", "sandboxes", k8stesting.ObjectReaction(generationTracker{g.dyn.Tracker()}))
	for _, client := range []*k8stesting.Fake{&g.dyn.Fake, &g.kube.Fake} {
		client.PrependReactor("*", "*", g.record)
	}
	opts := testOptions()
	opts.Conns = g.conns
	opts.Now = func() time.Time { return *g.now.Load() }
	for _, option := range options {
		option(g, &opts)
	}
	g.store, _ = opts.Store.(*treeStore)
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	var dyn dynamic.Interface = g.dyn
	if g.hooks != nil {
		dyn = hookedDynamic{g.dyn, g.hooks}
	}
	if err := r.start(ctx, dyn, g.kube); err != nil {
		t.Fatal(err)
	}
	g.r = r
	if !g.noController {
		go g.control()
	}
	return g
}

// newDynamic is the dynamic fake holding objects, each under the resource an API server serves
// its kind at: the fake guesses "sandboxs" for a Sandbox it is handed at construction.
func newDynamic(t *testing.T, objects ...*unstructured.Unstructured) *dynamicfake.FakeDynamicClient {
	t.Helper()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(k8sruntime.NewScheme(), map[schema.GroupVersionResource]string{
		sandboxGVR: "SandboxList", crdGVR: "CustomResourceDefinitionList",
	})
	for _, object := range objects {
		gvr := sandboxGVR
		if object.GetKind() == "CustomResourceDefinition" {
			gvr = crdGVR
		}
		if err := dyn.Tracker().Create(gvr, object, object.GetNamespace()); err != nil {
			t.Fatal(err)
		}
	}
	return dyn
}

// generationTracker stores what a Sandbox patch leaves as the API server does for a CRD with a
// status subresource, as the Sandbox's is: with metadata.generation bumped, in the same write,
// hence one watch event. The fake's tracker leaves the generation alone.
type generationTracker struct{ k8stesting.ObjectTracker }

func (t generationTracker) Patch(gvr schema.GroupVersionResource, obj k8sruntime.Object, ns string, opts ...metav1.PatchOptions) error {
	if object, err := metaOf(obj); err == nil && gvr == sandboxGVR {
		object.SetGeneration(object.GetGeneration() + 1)
	}
	return t.ObjectTracker.Patch(gvr, obj, ns, opts...)
}

// advance moves the runtime's clock.
func (g *rig) advance(d time.Duration) {
	next := g.now.Load().Add(d)
	g.now.Store(&next)
}

// record logs every request, and gives what a create makes the uid and creation time an API
// server would: the fakes set neither.
func (g *rig) record(a k8stesting.Action) (bool, k8sruntime.Object, error) {
	entry := action{verb: a.GetVerb(), resource: a.GetResource().Resource, subresource: a.GetSubresource()}
	switch typed := a.(type) {
	case k8stesting.CreateAction:
		if object, err := metaOf(typed.GetObject()); err == nil {
			entry.name = object.GetName()
			if object.GetUID() == "" {
				object.SetUID(types.UID(fmt.Sprintf("uid-%s-%d", entry.resource, g.uids.Add(1))))
			}
			if created := object.GetCreationTimestamp(); created.IsZero() {
				object.SetCreationTimestamp(metav1.NewTime(*g.now.Load()))
			}
			entry.object = typed.GetObject().DeepCopyObject()
		}
	case k8stesting.PatchAction:
		entry.name, entry.patch = typed.GetName(), string(typed.GetPatch())
	case k8stesting.UpdateAction:
		if object, err := metaOf(typed.GetObject()); err == nil {
			entry.name = object.GetName()
		}
	case k8stesting.DeleteAction:
		entry.name = typed.GetName()
	case k8stesting.GetAction:
		entry.name = typed.GetName()
	}
	if entry.verb != "list" && entry.verb != "watch" {
		g.mu.Lock()
		g.actions = append(g.actions, entry)
		g.mu.Unlock()
	}
	return false, nil, nil
}

func metaOf(object k8sruntime.Object) (metav1.Object, error) {
	if u, ok := object.(*unstructured.Unstructured); ok {
		return u, nil
	}
	accessor, ok := object.(metav1.Object)
	if !ok {
		return nil, fmt.Errorf("%T has no metadata", object)
	}
	return accessor, nil
}

// writes are the requests that changed something, in order, as "verb resource name".
func (g *rig) writes() []action {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []action
	for _, a := range g.actions {
		if a.verb != "get" {
			out = append(out, a)
		}
	}
	return out
}

func (g *rig) clearActions() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.actions = nil
}

// control is the controller stand-in, reconciling every Sandbox every few milliseconds. Its own
// requests go through the tracker directly, so they never appear among the runtime's.
func (g *rig) control() {
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	terminating := map[types.UID]time.Time{}
	for {
		select {
		case <-g.ctx.Done():
			return
		case <-tick.C:
		}
		list, err := g.dyn.Tracker().List(sandboxGVR, sandboxGVR.GroupVersion().WithKind("Sandbox"), testNamespace)
		if err != nil {
			continue
		}
		items, _ := list.(*unstructured.UnstructuredList)
		if items == nil {
			continue
		}
		for i := range items.Items {
			s, err := decodeSandbox(&items.Items[i])
			if err != nil || s.DeletionTimestamp != nil {
				continue
			}
			pod := g.pod(s.Name)
			owned := ownedBy(pod, s.UID)
			switch {
			case s.mode() == modeSuspended && owned:
				started, ok := terminating[pod.UID]
				if !ok {
					terminating[pod.UID] = time.Now()
					g.update(pod, func(p *corev1.Pod) { now := metav1.Now(); p.DeletionTimestamp = &now })
					continue
				}
				if time.Since(started) >= g.suspendDelay {
					_ = g.kube.Tracker().Delete(podsGVR, testNamespace, pod.Name)
				}
			case s.mode() == modeRunning && pod == nil && !g.hold.Load():
				g.createPod(s)
			}
		}
	}
}

var podsGVR = corev1.SchemeGroupVersion.WithResource("pods")

func (g *rig) createPod(s *sandbox) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: s.Name, Namespace: testNamespace, Labels: s.Spec.PodTemplate.Metadata.Labels,
			UID:             types.UID(fmt.Sprintf("uid-pod-%d", g.uids.Add(1))),
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "agents.x-k8s.io/v1beta1", Kind: "Sandbox", Name: s.Name, UID: s.UID, Controller: new(true)}},
		},
		Spec:   s.Spec.PodTemplate.Spec,
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	if g.autoStart.Load() {
		pod.Spec.NodeName = "ip-192-0-2-7"
		pod.Status = runningStatus()
	}
	// A new pod's role launchers are new processes, bound to its exact UID. The runtime waits
	// for their first state instead of treating the previous pod's connection as current.
	g.commandsMu.Lock()
	var reconnect []claim.Token
	for token := range g.wanted {
		if SandboxName(token) == s.Name {
			reconnect = append(reconnect, token)
		}
	}
	g.commandsMu.Unlock()
	_ = g.kube.Tracker().Add(pod)
	for _, token := range reconnect {
		g.connect(token)
	}
}

func runningStatus() corev1.PodStatus {
	status := corev1.PodStatus{
		Phase: corev1.PodRunning,
		InitContainerStatuses: []corev1.ContainerStatus{{
			Name: initContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed"}},
		}},
	}
	for _, role := range claim.Roles {
		status.ContainerStatuses = append(status.ContainerStatuses, corev1.ContainerStatus{
			Name: string(role), State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		})
	}
	return status
}

// reportLauncher registers a connected launcher for token reporting state, without a connection
// to drive: the observation mapping reads only the reported state.
func (g *rig) reportLauncher(token claim.Token, state shimwire.LauncherState) {
	g.t.Helper()
	pod := g.pod(SandboxName(token))
	if pod == nil {
		g.t.Fatal("cannot report launcher state without its pod")
	}
	s := g.r.launchers.accept(token, shimwire.LauncherHello{LauncherID: "test-" + string(token), PodUID: string(pod.UID)})
	s.mu.Lock()
	s.state = state
	close(s.ready)
	s.mu.Unlock()
}

// pod is the pod named name in the tracker, or nil.
func (g *rig) pod(name string) *corev1.Pod {
	object, err := g.kube.Tracker().Get(podsGVR, testNamespace, name)
	if err != nil {
		return nil
	}
	return object.(*corev1.Pod)
}

// update rewrites a pod in the tracker.
func (g *rig) update(pod *corev1.Pod, edit func(*corev1.Pod)) {
	next := pod.DeepCopy()
	edit(next)
	if err := g.kube.Tracker().Update(podsGVR, next, testNamespace); err != nil && !apierrors.IsNotFound(err) {
		g.t.Errorf("update pod %s: %v", pod.Name, err)
	}
}

// sandbox is the Sandbox named name in the tracker, or nil.
func (g *rig) sandbox(name string) *sandbox {
	object, err := g.dyn.Tracker().Get(sandboxGVR, testNamespace, name)
	if err != nil {
		return nil
	}
	s, err := decodeSandbox(object.(*unstructured.Unstructured))
	if err != nil {
		g.t.Fatal(err)
	}
	return s
}

// secret is the Secret named name in the tracker, or nil.
func (g *rig) secret(name string) *corev1.Secret {
	object, err := g.kube.Tracker().Get(corev1.SchemeGroupVersion.WithResource("secrets"), testNamespace, name)
	if err != nil {
		return nil
	}
	return object.(*corev1.Secret)
}

// eventually fails the test unless ok holds within a second.
func (g *rig) eventually(what string, ok func() bool) {
	g.t.Helper()
	deadline := time.Now().Add(time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			g.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// spawn launches spec and fails the test on an error.
func (g *rig) spawn(spec runtime.SpawnSpec) runtime.Locator {
	g.t.Helper()
	g.launcher(spec.Claim)
	loc, err := g.r.Spawn(g.ctx, spec)
	if err != nil {
		g.t.Fatalf("spawn %s: %v", spec.Claim, err)
	}
	return loc
}

// launcher is the test rig's role-private launcher connection. It models only the launcher
// protocol — one child generation at a time, a stop of another generation refused — and records
// every command: child shims and their agent events remain the fake Conns this rig already owns.
func (g *rig) launcher(token claim.Token) {
	g.t.Helper()
	g.commandsMu.Lock()
	if g.wanted == nil {
		g.wanted = map[claim.Token]bool{}
	}
	g.wanted[token] = true
	g.commandsMu.Unlock()
	g.r.launchers.mu.Lock()
	_, exists := g.r.launchers.sessions[token]
	g.r.launchers.mu.Unlock()
	if exists || g.pod(SandboxName(token)) == nil {
		return
	}
	g.connect(token)
}

// connect is one fake launcher process's connection for token, replacing any earlier one.
func (g *rig) connect(token claim.Token) {
	server, client := net.Pipe()
	pod := g.pod(SandboxName(token))
	session := g.r.launchers.accept(token, shimwire.LauncherHello{LauncherID: "test-" + string(token), PodUID: string(pod.UID)})
	g.t.Cleanup(func() { _ = client.Close() })
	go session.ServeLauncher(server, bufio.NewReader(server), shimwire.NewWriter(server))
	go func() {
		writer := shimwire.NewWriter(client)
		_ = writer.WriteFrame(shimwire.LauncherState{})
		reader := bufio.NewReader(client)
		var running uint64
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				return
			}
			frame, err := shimwire.Decode(line)
			if err != nil {
				return
			}
			g.commandsMu.Lock()
			if g.commands == nil {
				g.commands = map[claim.Token][]shimwire.Frame{}
			}
			g.commands[token] = append(g.commands[token], frame)
			g.commandsMu.Unlock()
			switch command := frame.(type) {
			case shimwire.LauncherStart:
				if running != 0 {
					_ = writer.WriteFrame(shimwire.LauncherStartResult{ID: command.ID, Error: fmt.Sprintf("generation %d is still running", running)})
					continue
				}
				running = command.Generation
				_ = writer.WriteFrame(shimwire.LauncherStartResult{ID: command.ID, OK: true, RunningGeneration: command.Generation})
				_ = writer.WriteFrame(shimwire.LauncherState{Child: &shimwire.LauncherChild{Generation: command.Generation, PID: 42}})
			case shimwire.LauncherStop:
				if running != 0 && running != command.Generation {
					_ = writer.WriteFrame(shimwire.LauncherStopResult{ID: command.ID, Error: fmt.Sprintf("generation %d is running, not %d", running, command.Generation)})
					continue
				}
				stopped := running
				running = 0
				_ = writer.WriteFrame(shimwire.LauncherStopResult{ID: command.ID, OK: true})
				state := shimwire.LauncherState{}
				if stopped != 0 {
					state.LastExit = &shimwire.LauncherExit{Generation: stopped, Code: 143, Signal: "terminated"}
				}
				_ = writer.WriteFrame(state)
			}
		}
	}()
}

// issueLaunchers connects a fake launcher for every role of token's issue.
func (g *rig) issueLaunchers(token claim.Token) {
	g.t.Helper()
	issue, _, ok := token.Cut()
	if !ok {
		g.t.Fatalf("%s names no role", token)
	}
	for _, sibling := range claim.Roles {
		g.launcher(claim.Token(issue + "-" + string(sibling)))
	}
}

// sent is every command token's fake launcher received.
func (g *rig) sent(token claim.Token) []shimwire.Frame {
	g.commandsMu.Lock()
	defer g.commandsMu.Unlock()
	return slices.Clone(g.commands[token])
}

// sandboxObject is a Sandbox as the API would hold it, for a test's starting state.
func sandboxObject(t *testing.T, name, uid, mode string, labels map[string]string, conditions ...metav1.Condition) *unstructured.Unstructured {
	t.Helper()
	s := sandbox{
		TypeMeta: metav1.TypeMeta{APIVersion: "agents.x-k8s.io/v1beta1", Kind: "Sandbox"},
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: testNamespace, UID: types.UID(uid), Labels: labels, Generation: 2,
			CreationTimestamp: metav1.NewTime(time.Date(2026, 9, 23, 11, 0, 0, 0, time.UTC)),
		},
		Spec:   sandboxSpec{OperatingMode: mode, PodTemplate: podTemplate{Metadata: podMetadata{Labels: labels}}},
		Status: sandboxStatus{Conditions: conditions},
	}
	u, err := encodeSandbox(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// podObject is a pod the Sandbox with ownerUID owns ("" for none), as the API would hold it.
func podObject(name, uid, ownerUID string, labels map[string]string, status corev1.PodStatus) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, UID: types.UID(uid), Labels: labels},
		Status:     status,
	}
	if ownerUID != "" {
		pod.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "agents.x-k8s.io/v1beta1", Kind: "Sandbox", Name: name, UID: types.UID(ownerUID), Controller: new(true),
		}}
	}
	return pod
}

// claimLabels are the labels an issue's shared Sandbox carries.
func claimLabels(_ claim.Role) map[string]string {
	return map[string]string{labelProject: testProject, labelTree: testTree, labelIssue: testTree}
}

// sandboxLocator is a recorded role process locator in one issue pod.
func sandboxLocator(token claim.Token, uid string) runtime.Locator {
	_, role, ok := token.Cut()
	if !ok {
		panic("test token has no role")
	}
	container := string(role)
	return runtime.Locator{
		Runtime:     runtime.RuntimeSandbox,
		Claim:       token,
		Incarnation: runtime.SandboxIncarnation(uid, 1),
		Sandbox:     &runtime.SandboxLocator{Namespace: testNamespace, Name: SandboxName(token), PodUID: uid, Container: container, Generation: 1},
	}
}

// patchOps decodes a JSON patch body.
func patchOps(t *testing.T, body string) []map[string]any {
	t.Helper()
	var ops []map[string]any
	if err := json.Unmarshal([]byte(body), &ops); err != nil {
		t.Fatalf("patch %q: %v", body, err)
	}
	return ops
}

// modePatched reports whether a patch body sets operatingMode to mode.
func modePatched(t *testing.T, body, mode string) bool {
	for _, op := range patchOps(t, body) {
		if op["path"] == "/spec/operatingMode" && op["value"] == mode {
			return true
		}
	}
	return false
}

// withLaggingSandboxInformer delays every watch event the Sandbox informer receives, as a store
// that has not caught up with an API write: the pod informer, undelayed, then runs ahead of it.
func withLaggingSandboxInformer(delay time.Duration) rigOption {
	return func(g *rig, _ *Options) {
		g.dyn.PrependWatchReactor("sandboxes", func(a k8stesting.Action) (bool, watch.Interface, error) {
			var opts metav1.ListOptions
			if w, ok := a.(k8stesting.WatchActionImpl); ok {
				opts = w.ListOptions
			}
			upstream, err := g.dyn.Tracker().Watch(sandboxGVR, a.GetNamespace(), opts)
			if err != nil {
				return true, nil, err
			}
			events := make(chan watch.Event)
			go func() {
				defer close(events)
				for ev := range upstream.ResultChan() {
					time.Sleep(delay)
					events <- ev
				}
			}()
			return true, watch.NewProxyWatcher(events), nil
		})
	}
}
