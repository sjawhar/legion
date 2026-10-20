//go:build e2e

// The controller orphan sweep's live proof's driver (scripts/e2e/controller-sweep-kind.sh, which
// builds it with `-tags e2e` and owns the throwaway kind cluster and the Postgres it runs against).
// The daemon's loader reads a `controller: daemon` legion.yaml, boot's own factory builds the
// Sandbox runtime it configures over the daemon's Postgres store (controller_sweep_kind_test.go),
// and the runtime launches the project controller through Agent Sandbox's controller under gVisor.
// The claims the orphan sweep is told of are read from the store before that launch, as a boot
// reconciliation retried while the keeper launches the controller reads them, and the store holds
// the controller's claim launching, as the supervisor stores it before the launch begins. What the
// proof stands in for lies outside the sweep: the agent is a sleep stub (no model, Dispatch or
// NATS), the App tokens are placeholders, and no repository is mapped.
package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// controllerSweepLiveSandboxes is the Sandbox resource at the Agent Sandbox version the runtime
// speaks.
var controllerSweepLiveSandboxes = schema.GroupVersionResource{Group: "agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxes"}

// controllerSweepLiveSweeps is how many sweeps a check runs, each controllerSweepLivePause apart: a
// delete the fence refused because the runtime's informer had not yet seen the Sandbox's last write
// (deleteFenced) is decided again at the next sweep, so a sweep that deletes the Sandbox gets that
// many chances to.
const (
	controllerSweepLiveSweeps = 5
	controllerSweepLivePause  = 2 * time.Second
)

// controllerSweepLive is one run of the proof against one cluster and one store.
type controllerSweepLive struct {
	t         *testing.T
	ctx       context.Context
	rt        runtime.Runtime
	st        *store.Store
	project   string
	token     claim.Token
	bootToken string
	name      string
	volume    string
	sandboxes dynamic.ResourceInterface
	pvcs      typedcorev1.PersistentVolumeClaimInterface
	pods      typedcorev1.PodInterface
}

// TestTheControllerOrphanSweepLive launches the project controller and sweeps at grace 0 with the
// known set read before the launch. LEGION_E2E_EXPECT says which sweep the binary was built with:
// `kept` for this checkout's, which must keep the launched controller's Sandbox and delete it with
// its volume once the claim is retired, and `deleted` for a sweep built without the fix (the
// script's negative control), which must delete it.
func TestTheControllerOrphanSweepLive(t *testing.T) {
	env := map[string]string{}
	for _, name := range []string{"LEGION_E2E_KUBECONFIG", "LEGION_E2E_KUBE_CONTEXT", "LEGION_E2E_IMAGE", "LEGION_E2E_STREAM_HOST",
		"LEGION_E2E_WORK", "LEGION_E2E_STORE_DSN", "LEGION_E2E_EXPECT"} {
		if env[name] = os.Getenv(name); env[name] == "" {
			t.Fatalf("%s is unset; scripts/e2e/controller-sweep-kind.sh sets it", name)
		}
	}
	expect := env["LEGION_E2E_EXPECT"]
	if expect != "kept" && expect != "deleted" {
		t.Fatalf("LEGION_E2E_EXPECT is %q, want kept or deleted", expect)
	}
	// The proof creates and deletes Sandboxes, so it runs only on a kind cluster, the one its script
	// made, and never on whatever the operator's current context is.
	if !strings.HasPrefix(env["LEGION_E2E_KUBE_CONTEXT"], "kind-") {
		t.Fatalf("LEGION_E2E_KUBE_CONTEXT %q is no kind cluster's context (kind-<name>); the proof runs only on the cluster its script creates", env["LEGION_E2E_KUBE_CONTEXT"])
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	path := writeControllerSweepKindConfig(t, filepath.Join(env["LEGION_E2E_WORK"], "controller-sweep-"+expect), env["LEGION_E2E_STORE_DSN"],
		env["LEGION_E2E_STREAM_HOST"], env["LEGION_E2E_IMAGE"], env["LEGION_E2E_KUBECONFIG"], env["LEGION_E2E_KUBE_CONTEXT"])
	cfg, err := config.LoadForValidation(path, nil)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	k := *cfg.Runtime.Kubernetes
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := kubeClient(k)
	if err != nil {
		t.Fatal(err)
	}
	if err := sandbox.CheckInstalled(ctx, rc, agentSandbox); err != nil {
		t.Fatalf("the cluster check: %v", err)
	}
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		t.Fatal(err)
	}
	clients, err := kubernetes.NewForConfig(rc)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate the store: %v", err)
	}
	token := claim.ControllerToken(project)
	r := &controllerSweepLive{
		t: t, ctx: ctx, st: st, project: project, token: token, bootToken: "boot-" + randomSuffix(t),
		name: sandbox.SandboxName(token), volume: sandbox.IssueClaimName(token),
		sandboxes: dyn.Resource(controllerSweepLiveSandboxes).Namespace(k.Namespace),
		pvcs:      clients.CoreV1().PersistentVolumeClaims(k.Namespace), pods: clients.CoreV1().Pods(k.Namespace),
	}
	resolve := func(presented string) (claim.Token, uint64, bool, bool, error) {
		if presented != r.bootToken {
			return "", 0, false, false, nil
		}
		return token, 1, false, true, nil
	}
	listener, err := stream.Listen(ctx, "tcp://"+cfg.Bind+":0", resolve, stream.Options{RPCTimeout: cfg.WorkerRPCTimeout, Log: log})
	if err != nil {
		t.Fatalf("the worker stream: %v", err)
	}
	go func() {
		for range listener.Events() {
		}
	}()
	if r.rt, err = controllerSweepKindRuntime(ctx, cfg, listener, st, log); err != nil {
		t.Fatalf("the Sandbox runtime: %v", err)
	}

	// known is what each sweep is told of, read before the controller's claim is stored, so it
	// names no controller claim: what a boot reconciliation that read its claims before the keeper
	// launched the controller hands the sweep.
	known := r.known()
	r.check("controller-launch", func() { r.launch(known) })
	if expect == "deleted" {
		r.check("reverted-sweep-deletes-launched-controller", func() {
			sweep := r.sweepUntilGone(known)
			fmt.Printf("   the sweep without the fix deleted the launched controller's Sandbox at sweep %d, its claim stored %s and unretired\n", sweep, supervise.StateLaunching)
		})
		return
	}
	r.check("sweep-keeps-launched-controller", func() { r.sweepKeeps(known) })
	r.check("retired-controller-swept", func() {
		r.putClaim(supervise.StateRetired)
		sweep := r.sweepUntilGone(r.known())
		fmt.Printf("   sweep %d deleted the retired controller's Sandbox\n", sweep)
	})
}

// check runs one of the proof's checks between its `== name` and `ok name (<s>s)` lines.
func (r *controllerSweepLive) check(name string, body func()) {
	r.t.Helper()
	started := time.Now()
	fmt.Printf("== %s\n", name)
	body()
	fmt.Printf("ok %s (%ds)\n", name, int(time.Since(started).Seconds()))
}

// known is knownClaims of the project's claims as the store holds them now, as the daemon's
// supervisor reads them for a sweep.
func (r *controllerSweepLive) known() []runtime.Known {
	r.t.Helper()
	claims, err := r.st.Claims(r.ctx)
	if err != nil {
		r.t.Fatalf("read the claims: %v", err)
	}
	known := knownClaims(ofProject(claims, r.project))
	names := []string{}
	for _, k := range known {
		names = append(names, string(k.Claim))
	}
	fmt.Printf("   known, read from the store: %v\n", names)
	return known
}

// putClaim stores the controller's claim in state, and holds Store.ClaimUnretired to it.
func (r *controllerSweepLive) putClaim(state supervise.ClaimState) {
	r.t.Helper()
	if err := r.st.PutClaim(r.ctx, supervise.Claim{
		Token: r.token, Project: r.project, Role: claim.RoleController, Generation: 1, State: state,
		BootTokenHash: supervise.HashBootToken(r.bootToken),
	}); err != nil {
		r.t.Fatalf("store the controller's claim %s: %v", state, err)
	}
	unretired, err := r.st.ClaimUnretired(r.ctx, r.token)
	if err != nil || unretired != (state != supervise.StateRetired) {
		r.t.Fatalf("Store.ClaimUnretired(%s) = %t, %v with the claim stored %s", r.token, unretired, err, state)
	}
	fmt.Printf("   stored %s %s; Store.ClaimUnretired = %t\n", r.token, state, unretired)
}

// launch stores the controller's claim launching and launches it through the runtime, then holds
// the API server to a Sandbox of the controller's name owning its volume claim and the launched pod.
func (r *controllerSweepLive) launch(known []runtime.Known) {
	r.t.Helper()
	r.putClaim(supervise.StateLaunching)
	prompt := filepath.Join(r.t.TempDir(), "controller.md")
	if err := os.WriteFile(prompt, []byte("You are the controller.\n"), 0o600); err != nil {
		r.t.Fatal(err)
	}
	loc, err := r.rt.Spawn(r.ctx, controllerSweepKindSpec(r.project, r.bootToken, prompt))
	if err != nil {
		r.t.Fatalf("Spawn %s: %v", r.token, err)
	}
	s := r.sandbox()
	if s == nil {
		r.t.Fatalf("the controller's launch returned %+v, and the API server holds no Sandbox %s", loc.Sandbox, r.name)
	}
	pods, err := r.pods.List(r.ctx, metav1.ListOptions{})
	if err != nil {
		r.t.Fatal(err)
	}
	var pod *corev1.Pod
	for i := range pods.Items {
		if string(pods.Items[i].UID) == loc.Sandbox.PodUID {
			pod = &pods.Items[i]
		}
	}
	if pod == nil {
		r.t.Fatalf("no pod has the launched uid %s", loc.Sandbox.PodUID)
	}
	pvc := r.pvc()
	if pvc == nil {
		r.t.Fatalf("the controller's Sandbox %s owns no volume claim %s", r.name, r.volume)
	}
	fmt.Printf("   Sandbox %s uid %s, operatingMode %s\n", r.name, s.GetUID(), liveMode(s))
	fmt.Printf("   pod %s uid %s (the launch's), phase %s, runtimeClassName %s\n", pod.Name, pod.UID, pod.Status.Phase, liveString(pod.Spec.RuntimeClassName))
	fmt.Printf("   volume claim %s %s, owned by %s\n", pvc.Name, pvc.Status.Phase, liveOwners(pvc.OwnerReferences))
	if liveString(pod.Spec.RuntimeClassName) != "gvisor" {
		r.t.Fatalf("the controller's pod runs under %q, not gvisor", liveString(pod.Spec.RuntimeClassName))
	}
	for _, k := range known {
		if k.Claim == r.token {
			r.t.Fatalf("the known set read before the launch names the controller's claim %s", r.token)
		}
	}
}

// sweepKeeps runs controllerSweepLiveSweeps sweeps at grace 0 told known, and holds the API server
// to the launched controller's Sandbox, unchanged in uid and not terminating, and its volume claim
// after each.
func (r *controllerSweepLive) sweepKeeps(known []runtime.Known) {
	r.t.Helper()
	launched := r.sandbox()
	fmt.Printf("   the Sandbox settled at resourceVersion %s\n", r.settle())
	for sweep := 1; sweep <= controllerSweepLiveSweeps; sweep++ {
		if err := r.rt.ReconcileOrphans(r.ctx, known, 0); err != nil {
			r.t.Fatalf("sweep %d: ReconcileOrphans: %v", sweep, err)
		}
		s := r.sandbox()
		switch {
		case s == nil:
			r.t.Fatalf("sweep %d at grace 0 deleted the launched controller's Sandbox %s (uid %s)", sweep, r.name, launched.GetUID())
		case s.GetDeletionTimestamp() != nil:
			r.t.Fatalf("sweep %d at grace 0 set the launched controller's Sandbox %s (uid %s) terminating at %s", sweep, r.name, s.GetUID(), s.GetDeletionTimestamp())
		case s.GetUID() != launched.GetUID():
			r.t.Fatalf("sweep %d: Sandbox %s is uid %s, not the launch's %s", sweep, r.name, s.GetUID(), launched.GetUID())
		}
		pvc := r.pvc()
		if pvc == nil || pvc.DeletionTimestamp != nil {
			r.t.Fatalf("sweep %d took the launched controller's volume claim %s: %+v", sweep, r.volume, pvc)
		}
		fmt.Printf("   sweep %d of %d at grace 0: Sandbox %s uid %s resourceVersion %s, not terminating; volume claim %s %s\n",
			sweep, controllerSweepLiveSweeps, r.name, s.GetUID(), s.GetResourceVersion(), r.volume, pvc.Status.Phase)
		time.Sleep(controllerSweepLivePause)
	}
}

// sweepUntilGone sweeps at grace 0 told known until one deletes the controller's Sandbox, at most
// controllerSweepLiveSweeps times, then waits until the API server holds neither the Sandbox nor
// its volume claim, and returns the sweep that deleted it.
func (r *controllerSweepLive) sweepUntilGone(known []runtime.Known) int {
	r.t.Helper()
	launched := r.sandbox()
	if launched == nil {
		r.t.Fatalf("there is no Sandbox %s to sweep", r.name)
	}
	fmt.Printf("   the Sandbox settled at resourceVersion %s\n", r.settle())
	deleted := 0
	for sweep := 1; sweep <= controllerSweepLiveSweeps && deleted == 0; sweep++ {
		if err := r.rt.ReconcileOrphans(r.ctx, known, 0); err != nil {
			r.t.Fatalf("sweep %d: ReconcileOrphans: %v", sweep, err)
		}
		if s := r.sandbox(); s == nil || s.GetDeletionTimestamp() != nil || s.GetUID() != launched.GetUID() {
			deleted = sweep
			fmt.Printf("   sweep %d of %d at grace 0 deleted Sandbox %s (uid %s)\n", sweep, controllerSweepLiveSweeps, r.name, launched.GetUID())
			break
		}
		fmt.Printf("   sweep %d of %d at grace 0 kept Sandbox %s\n", sweep, controllerSweepLiveSweeps, r.name)
		time.Sleep(controllerSweepLivePause)
	}
	if deleted == 0 {
		r.t.Fatalf("%d sweeps at grace 0 kept Sandbox %s (uid %s)", controllerSweepLiveSweeps, r.name, launched.GetUID())
	}
	started := time.Now()
	for r.sandbox() != nil || r.pvc() != nil {
		if time.Since(started) > 5*time.Minute {
			r.t.Fatalf("Sandbox %s or volume claim %s is still there %s after the sweep deleted it", r.name, r.volume, time.Since(started).Round(time.Second))
		}
		time.Sleep(time.Second)
	}
	fmt.Printf("   the API server holds neither Sandbox %s nor volume claim %s, %s after the sweep\n", r.name, r.volume, time.Since(started).Round(time.Second))
	return deleted
}

// settle waits until the Sandbox's resourceVersion has not moved for five seconds, so the Agent
// Sandbox controller's status writes after the launch are done and the runtime's informer holds the
// Sandbox as the API server does, and returns that version.
func (r *controllerSweepLive) settle() string {
	r.t.Helper()
	version, since, started := "", time.Now(), time.Now()
	for {
		s := r.sandbox()
		if s == nil {
			r.t.Fatalf("Sandbox %s went before the sweep", r.name)
		}
		if s.GetResourceVersion() != version {
			version, since = s.GetResourceVersion(), time.Now()
		}
		if time.Since(since) >= 5*time.Second {
			return version
		}
		if time.Since(started) > 2*time.Minute {
			r.t.Fatalf("Sandbox %s is still being written two minutes after its launch (resourceVersion %s)", r.name, version)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// sandbox is the controller's Sandbox as the API server holds it now, or nil when it holds none.
func (r *controllerSweepLive) sandbox() *unstructured.Unstructured {
	r.t.Helper()
	s, err := r.sandboxes.Get(r.ctx, r.name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		r.t.Fatalf("read Sandbox %s: %v", r.name, err)
	}
	return s
}

// pvc is the controller's volume claim as the API server holds it now, or nil when it holds none.
func (r *controllerSweepLive) pvc() *corev1.PersistentVolumeClaim {
	r.t.Helper()
	pvc, err := r.pvcs.Get(r.ctx, r.volume, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		r.t.Fatalf("read volume claim %s: %v", r.volume, err)
	}
	return pvc
}

func liveMode(s *unstructured.Unstructured) string {
	mode, _, _ := unstructured.NestedString(s.Object, "spec", "operatingMode")
	return mode
}

func liveOwners(refs []metav1.OwnerReference) string {
	var owners []string
	for _, ref := range refs {
		owners = append(owners, ref.Kind+"/"+ref.Name)
	}
	return strings.Join(owners, ", ")
}
