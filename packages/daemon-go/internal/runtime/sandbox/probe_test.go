package sandbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
)

// The probe Sandbox testImage gets in testProject: the project, then the digest's first 12 hex.
const (
	probeSandboxName = "legion-probe-legion-1d10089a0000"
	testDigestHex    = "1d10089a0000000000000000000000000000000000000000000000000000beef"
)

var sandboxesResource = schema.GroupResource{Group: "agents.x-k8s.io", Resource: "sandboxes"}

// probeRig is a rig whose controller stand-in leaves the pods it creates Pending, with a stand-in
// for the probe's container finishing (finish, applied once to each probe pod), for the garbage
// collector (a pod whose owning Sandbox is gone is deleted), and for the pod's log and the API's
// answers to the probe's own requests.
type probeRig struct {
	*rig
	creates atomic.Int32

	mu          sync.Mutex
	finish      func(*corev1.Pod)
	log         string
	logErr      error
	createErr   error
	finishedUID map[types.UID]bool
}

func newProbeRig(t *testing.T, objects []k8sruntime.Object, options ...rigOption) *probeRig {
	t.Helper()
	g := &probeRig{rig: newRig(t, objects, options...), finishedUID: map[types.UID]bool{}}
	g.autoStart.Store(false)
	g.kube.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		if action.GetSubresource() != "log" {
			return false, nil, nil
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.logErr != nil {
			return true, nil, g.logErr
		}
		return true, &k8sruntime.Unknown{Raw: []byte(g.log)}, nil
	})
	g.dyn.PrependReactor("create", "sandboxes", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		g.creates.Add(1)
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.createErr != nil {
			return true, nil, g.createErr
		}
		return false, nil, nil
	})
	go g.containerAndCollector()
	return g
}

// with edits what the stand-ins answer.
func (g *probeRig) with(edit func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	edit()
}

// succeeds finishes the probe's container with exit 0 and log; fails with exit 1 and log.
func (g *probeRig) succeeds(log string) *probeRig { return g.ends(corev1.PodSucceeded, 0, log) }
func (g *probeRig) fails(log string) *probeRig    { return g.ends(corev1.PodFailed, 1, log) }

func (g *probeRig) ends(phase corev1.PodPhase, exit int32, log string) *probeRig {
	g.mu.Lock()
	g.log = log
	g.mu.Unlock()
	reason := map[int32]string{0: "Completed", 1: "Error"}[exit]
	return g.finishes(func(p *corev1.Pod) {
		p.Status = corev1.PodStatus{Phase: phase, ContainerStatuses: []corev1.ContainerStatus{{
			Name: probeContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exit, Reason: reason}},
		}}}
	})
}

// finishes ends the probe's pod as finish writes its status, on a node.
func (g *probeRig) finishes(finish func(*corev1.Pod)) *probeRig {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.finish = func(p *corev1.Pod) {
		p.Spec.NodeName = "ip-10-1-40-7"
		finish(p)
	}
	return g
}

// waits leaves the probe's container waiting for reason.
func (g *probeRig) waits(reason string) *probeRig {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.finish = func(p *corev1.Pod) {
		p.Spec.NodeName = "ip-10-1-40-7"
		p.Status = corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
			Name: probeContainer, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}},
		}}}
	}
	return g
}

func (g *probeRig) containerAndCollector() {
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-g.ctx.Done():
			return
		case <-tick.C:
		}
		pod := g.pod(probeSandboxName)
		if pod == nil {
			continue
		}
		if owner := g.sandbox(probeSandboxName); owner == nil || !ownedBy(pod, owner.UID) {
			if len(pod.OwnerReferences) > 0 {
				_ = g.kube.Tracker().Delete(podsGVR, testNamespace, pod.Name)
			}
			continue
		}
		g.mu.Lock()
		finish, done := g.finish, g.finishedUID[pod.UID]
		if finish != nil && !done {
			g.finishedUID[pod.UID] = true
		}
		g.mu.Unlock()
		if finish != nil && !done {
			g.update(pod, finish)
		}
	}
}

// probeOptions allow two attempts, so a definitive verdict shows as one attempt and a transient one
// as two. Each attempt's budget is long enough that a pod the stand-ins answer at once is judged
// within it on a loaded machine too; an attempt ends as soon as the pod answers, so it costs a
// passing test nothing. A test whose attempt must run out sets unfinishedBudget.
func probeOptions(t *testing.T) ImageProbe {
	return ImageProbe{
		Contract: 3, Budget: 10 * time.Second,
		Retry: bootprobe.Retry{Initial: time.Millisecond, Max: time.Millisecond, Attempts: 2},
	}
}

// unfinishedBudget is the attempt budget of a test in which no pod ever answers, so the attempt
// runs out.
const unfinishedBudget = 300 * time.Millisecond

func (g *probeRig) probe(p ImageProbe) error {
	g.t.Helper()
	return g.r.ProbeImage(g.ctx, p)
}

func wantContains(t *testing.T, err error, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want one saying %q", err, want)
		}
	}
}

// okLine is the OK line an image prints when every probe passed, confirming contract, the
// prompt-named agents' models resolved.
func okLine(contract int) string {
	return bootprobe.OKLine("/opt/omp/bin/omp", contract, bootprobe.AgentModelsResolved)
}

// The probe is a Sandbox named for the project and the image, running the image's Go `legion
// probe-image` with the daemon's contract on the Legion pool; it passes on the OK line confirming
// that contract — which names no model: Legion's probe makes no model round trip — and it deletes
// its Sandbox when it is done.
func TestProbeImagePassesOnTheOKLineConfirmingTheContract(t *testing.T) {
	g := newProbeRig(t, nil)
	g.succeeds("[legion] OMP pi.agents probe failed transiently\n" + okLine(3) + "\n")

	if err := g.probe(probeOptions(t)); err != nil {
		t.Fatalf("ProbeImage = %v, want a pass", err)
	}

	if n := g.creates.Load(); n != 1 {
		t.Errorf("created the probe Sandbox %d times, want once", n)
	}
	var created *action
	for _, a := range g.writes() {
		if a.verb == "create" && a.resource == "sandboxes" {
			created = &a
		}
	}
	if created == nil || created.name != probeSandboxName {
		t.Fatalf("created %+v, want the Sandbox %s", created, probeSandboxName)
	}
	g.eventually("the probe Sandbox to be deleted", func() bool { return g.sandbox(probeSandboxName) == nil })
}

// A pod that ran is judged by its log: only a Succeeded pod whose OK line confirms this daemon's
// contract passes, and every other ending is an answer no retry changes — so the probe runs once.
func TestProbeImageRefusesWhatTheProbePodAnswered(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		setup func(*probeRig)
		want  []string
	}{
		{"a Failed pod", func(g *probeRig) {
			g.fails("legion probe-image: pi-legion-envoy at /home/legion/.omp/… speaks Go daemon API contract 2; this daemon requires 3")
		}, []string{"worker image sha256:" + testDigestHex + " failed its probe", "pod " + probeSandboxName + " Failed", "exit code 1", "speaks Go daemon API contract 2"}},
		{"no OK line", func(g *probeRig) { g.succeeds("hello") },
			[]string{"Succeeded without printing probe-image: OK", "hello"}},
		{"the TypeScript CLI's OK line", func(g *probeRig) {
			g.succeeds("probe-image: OK (/opt/omp/bin/omp) session-storage=probed daemon-api-version=8")
		}, []string{"without confirming Go daemon API contract 3 (its legion CLI predates the check)"}},
		{"a CLI that predates the session-storage probe", func(g *probeRig) { g.succeeds("probe-image: OK (/opt/omp/bin/omp)") },
			[]string{"without confirming Go daemon API contract 3 (its legion CLI predates the check)"}},
		{"another contract", func(g *probeRig) { g.succeeds(okLine(2)) },
			[]string{"confirmed Go daemon API contract 2, this daemon requires 3"}},
		{"a CLI that predates the agent-model check", func(g *probeRig) {
			g.succeeds("probe-image: OK (/opt/omp/bin/omp) session-storage=probed go-daemon-api-version=3")
		}, []string{"without resolving the prompt-named agents' models: the worker image predates the agent-model check (LEGION-270)"}},
		{"a CLI that predates a flag the probe passes", func(g *probeRig) {
			g.fails("flag provided but not defined: -role-references\nUsage of legion probe-image:")
		}, []string{"Failed (container probe terminated", "its legion CLI has no -role-references, a flag this daemon's probe passes, so the worker image predates this daemon"}},
		{"a build-time probe's result", func(g *probeRig) { g.succeeds(bootprobe.OKLine("/opt/omp/bin/omp", 3, bootprobe.AgentModelsSkipped)) },
			[]string{"with the agents' models skipped: a build-time probe's result reached boot"}},
		{"an image name the kubelet cannot use", func(g *probeRig) { g.waits("InvalidImageName") },
			[]string{"container probe waiting: InvalidImageName"}},
		{"an image the node may never pull", func(g *probeRig) { g.waits("ErrImageNeverPull") },
			[]string{"container probe waiting: ErrImageNeverPull"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			g := newProbeRig(t, nil)
			testCase.setup(g)
			p := probeOptions(t)

			err := g.probe(p)

			wantContains(t, err, testCase.want...)
			if n := g.creates.Load(); n != 1 {
				t.Errorf("ran the probe %d times for a definitive answer, want once", n)
			}
			g.eventually("the probe Sandbox to be deleted", func() bool { return g.sandbox(probeSandboxName) == nil })
		})
	}
}

// A pod that never finished within the budget, or a Sandbox that never got one, says nothing
// about the image: the probe runs again, and gives up only when its retry does.
func TestProbeImageRetriesAProbeThatNeverFinished(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		setup func(*probeRig)
		want  []string
	}{
		{"an image still being pulled", func(g *probeRig) { g.waits("ImagePullBackOff") },
			[]string{"probe pod " + probeSandboxName + " still Pending after 300ms (container probe waiting: ImagePullBackOff)"}},
		{"no pod at all", func(g *probeRig) { g.hold.Store(true) },
			[]string{"probe sandbox " + probeSandboxName + " has no pod after 300ms"}},
		// A pod whose probe outlasts the attempt: what it logged so far is quoted.
		{"a pod still probing", func(g *probeRig) {
			g.with(func() {
				g.log = "probe-image: pi.agents answered\nprobe-image: the load probe is waiting"
			})
			g.finishes(func(p *corev1.Pod) {
				p.Status = corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
					Name: probeContainer, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				}}}
			})
		}, []string{"probe pod " + probeSandboxName + " still Running after 300ms", "log tail: probe-image: pi.agents answered",
			"the load probe is waiting"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			g := newProbeRig(t, nil)
			testCase.setup(g)

			p := probeOptions(t)
			p.Budget = unfinishedBudget

			err := g.probe(p)

			wantContains(t, err, append([]string{"the worker image probe never completed within its retry budget (2 attempts)"}, testCase.want...)...)
			if n := g.creates.Load(); n != 2 {
				t.Errorf("ran the probe %d times, want the retry's 2", n)
			}
			g.eventually("the probe Sandbox to be deleted", func() bool { return g.sandbox(probeSandboxName) == nil })
		})
	}
}

// A pod the kubelet failed for reasons of its own — evicted under node pressure, stopped by a node
// shutdown, refused at node admission — never ran the image to its end, so it says nothing about
// the image: the probe runs again. Only a pod whose probe container exited on its own is the
// image's answer (TestProbeImageRefusesWhatTheProbePodAnswered).
func TestProbeImageRetriesAPodTheKubeletFailed(t *testing.T) {
	killed := []corev1.ContainerStatus{{Name: probeContainer, State: corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "Error"},
	}}}
	for _, testCase := range []struct {
		name   string
		status corev1.PodStatus
		want   string
	}{
		{"evicted under node pressure", corev1.PodStatus{
			Phase: corev1.PodFailed, Reason: "Evicted", Message: "The node was low on resource: memory.",
			ContainerStatuses: killed,
		}, "Evicted: The node was low on resource: memory."},
		{"stopped by a node shutdown", corev1.PodStatus{
			Phase: corev1.PodFailed, Reason: "Terminated", Message: "Pod was terminated in response to imminent node shutdown.",
			Conditions:        []corev1.PodCondition{{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue, Reason: "TerminationByKubelet"}},
			ContainerStatuses: killed,
		}, "Terminated: Pod was terminated in response to imminent node shutdown."},
		{"marked a disruption target with no reason", corev1.PodStatus{
			Phase:             corev1.PodFailed,
			Conditions:        []corev1.PodCondition{{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue, Reason: "DeletionByTaintManager"}},
			ContainerStatuses: killed,
		}, "disruption target (DeletionByTaintManager)"},
		{"refused at node admission", corev1.PodStatus{
			Phase: corev1.PodFailed, Reason: "OutOfcpu", Message: "Pod was rejected: Node didn't have enough resource: cpu",
		}, "OutOfcpu: Pod was rejected"},
		{"failed before its container ran", corev1.PodStatus{Phase: corev1.PodFailed}, "its container probe never ran to an exit"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			g := newProbeRig(t, nil)
			status := testCase.status
			g.finishes(func(p *corev1.Pod) { p.Status = status })

			err := g.probe(probeOptions(t))

			wantContains(t, err, "never completed within its retry budget (2 attempts)", "pod "+probeSandboxName+" Failed", testCase.want)
			if n := g.creates.Load(); n != 2 {
				t.Errorf("ran the probe %d times, want the retry's 2", n)
			}
			g.eventually("the probe Sandbox to be deleted", func() bool { return g.sandbox(probeSandboxName) == nil })
		})
	}
}

// The API refusing what the probe sent — RBAC, credentials, a manifest it rejects, a namespace
// that does not exist — refuses it again, so the probe stops naming it; anything else is the
// cluster's moment, retried.
func TestProbeImageClassifiesTheAPIsAnswers(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		setup      func(*probeRig)
		definitive bool
		want       string
	}{
		{"create forbidden", func(g *probeRig) {
			g.with(func() {
				g.createErr = apierrors.NewForbidden(sandboxesResource, probeSandboxName, errors.New("no RBAC"))
			})
		}, true, "create probe sandbox " + probeSandboxName},
		{"create in a namespace that does not exist", func(g *probeRig) {
			g.with(func() {
				g.createErr = apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, testNamespace)
			})
		}, true, "not found"},
		{"create invalid", func(g *probeRig) {
			g.with(func() {
				g.createErr = apierrors.NewInvalid(schema.GroupKind{Group: "agents.x-k8s.io", Kind: "Sandbox"}, probeSandboxName, nil)
			})
		}, true, "is invalid"},
		{"create failing on the server", func(g *probeRig) {
			g.with(func() { g.createErr = apierrors.NewInternalError(errors.New("etcd leader changed")) })
		}, false, "etcd leader changed"},
		{"the log forbidden", func(g *probeRig) {
			g.succeeds(okLine(3))
			g.with(func() {
				g.logErr = apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, probeSandboxName, errors.New("no pods/log"))
			})
		}, true, "read probe pod " + probeSandboxName + "'s log"},
		{"the log failing on the server", func(g *probeRig) {
			g.succeeds(okLine(3))
			g.with(func() { g.logErr = apierrors.NewServiceUnavailable("kubelet unreachable") })
		}, false, "kubelet unreachable"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			g := newProbeRig(t, nil)
			testCase.setup(g)

			err := g.probe(probeOptions(t))

			attempts := int32(2)
			if testCase.definitive {
				attempts = 1
				wantContains(t, err, "failed its probe", testCase.want)
			} else {
				wantContains(t, err, "never completed within its retry budget (2 attempts)", testCase.want)
			}
			if n := g.creates.Load(); n != attempts {
				t.Errorf("ran the probe %d times, want %d", n, attempts)
			}
		})
	}
}

// A probe Sandbox of this project and image left by a crashed boot is replaced; one carrying
// another project's label, or none, is not this daemon's to delete, and the probe says so.
func TestProbeImageReplacesOnlyItsOwnProjectsLeftover(t *testing.T) {
	t.Run("this project's", func(t *testing.T) {
		labels := map[string]string{labelProject: testProject, labelProbe: "image"}
		g := newProbeRig(t, []k8sruntime.Object{
			sandboxObject(t, probeSandboxName, "uid-leftover", modeRunning, labels),
			podObject(probeSandboxName, "uid-leftover-pod", "uid-leftover", labels, corev1.PodStatus{Phase: corev1.PodSucceeded}),
		})
		g.succeeds(okLine(3))

		if err := g.probe(probeOptions(t)); err != nil {
			t.Fatalf("ProbeImage = %v, want the leftover replaced and a pass", err)
		}
		var sequence []string
		for _, a := range g.writes() {
			if a.resource == "sandboxes" {
				sequence = append(sequence, a.verb)
			}
		}
		if got := strings.Join(sequence, " "); got != "create delete create delete" {
			t.Errorf("sandbox writes = %q, want the refused create, the leftover's delete, the probe's create, and its delete", got)
		}
	})
	for name, labels := range map[string]map[string]string{
		"another project's": {labelProject: "widgets", labelProbe: "image"},
		"an unlabelled one": nil,
	} {
		t.Run(name, func(t *testing.T) {
			// No pod: the fakes' watches ignore label selectors, so a pod of another project's
			// Sandbox would reach the runtime's store, which a real API server never sends it.
			g := newProbeRig(t, []k8sruntime.Object{sandboxObject(t, probeSandboxName, "uid-foreign", modeRunning, labels)})
			g.hold.Store(true)

			err := g.probe(probeOptions(t))

			owner := "no Legion project"
			if labels != nil {
				owner = "project widgets"
			}
			wantContains(t, err, "probe sandbox "+probeSandboxName+" already exists and belongs to "+owner+", not "+testProject+"; not deleting it")
			if s := g.sandbox(probeSandboxName); s == nil || s.UID != "uid-foreign" {
				t.Errorf("the other Sandbox is %+v, want it untouched", s)
			}
		})
	}
}

// Every boot probes the image: no pass is remembered, since the probe pod's spec cannot show the
// operator's ConfigMap and Secret contents, which decide whether the agents' models resolve. A
// second boot on the same image creates its own probe pod, and refuses when the operator's
// configuration stopped resolving an agent's model in between.
func TestEveryBootProbesTheImage(t *testing.T) {
	g := newProbeRig(t, nil)
	g.succeeds(okLine(3))
	if err := g.probe(probeOptions(t)); err != nil {
		t.Fatalf("the first boot's probe = %v, want a pass", err)
	}
	g.eventually("the first probe Sandbox to be deleted", func() bool { return g.sandbox(probeSandboxName) == nil })
	g.fails("legion probe-image: Oh My Pi, loading the plugin from /opt/legion/pi-legion-envoy with discovery off, as a pod does, " +
		"cannot run task agent oracle (dispatched by roles/core/planner.md) on its model @oracle: role oracle is not configured.")

	err := g.probe(probeOptions(t))

	wantContains(t, err, "task agent oracle", "role oracle is not configured")
	if n := g.creates.Load(); n != 2 {
		t.Errorf("created the probe Sandbox %d times over two boots, want once each", n)
	}
}

// A providers Secret the kubelet cannot mount leaves the probe pod Pending, never Failed: a
// FailedMount event saying the Secret, or one of the keys provider_keys asks of it, is missing is
// the answer, a refusal naming the Secret, the keys, and the kubelet's message, given once rather
// than retried to the budget. Any other FailedMount on the volume is one the kubelet retries (its
// Secret watch not yet synced, kubernetes/kubernetes#99475), and the attempt runs to its budget.
func TestProbeImageRefusesAProvidersSecretThePodCannotMount(t *testing.T) {
	mount := `MountVolume.SetUp failed for volume "providers" : `
	for _, testCase := range []struct {
		name, message string
		refused       bool
	}{
		{"the Secret missing", mount + `secret "` + ProvidersSecretName(testProject) + `" not found`, true},
		{"a key missing", mount + "references non-existent secret key: ANTHROPIC_API_KEY", true},
		{"the kubelet's secret cache not synced", mount + "failed to sync secret cache: timed out waiting for the condition", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			g := newProbeRig(t, nil, withOptions(func(o *Options) { o.ProviderKeys = map[string]string{"ANTHROPIC_API_KEY": "anthropic"} }))
			g.waits("ContainerCreating")
			if _, err := g.kube.CoreV1().Events(testNamespace).Create(g.ctx, &corev1.Event{
				ObjectMeta:     metav1.ObjectMeta{Name: probeSandboxName + ".mount", Namespace: testNamespace},
				InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: probeSandboxName, Namespace: testNamespace},
				Reason:         "FailedMount", Type: corev1.EventTypeWarning, Message: testCase.message,
			}, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			p := probeOptions(t)
			p.Budget = unfinishedBudget

			err := g.probe(p)

			if testCase.refused {
				wantContains(t, err, "cannot mount the providers Secret "+ProvidersSecretName(testProject), "(anthropic)", testCase.message)
				if n := g.creates.Load(); n != 1 {
					t.Errorf("ran the probe %d times for a Secret the pod cannot mount, want once", n)
				}
				return
			}
			wantContains(t, err, "the worker image probe never completed within its retry budget (2 attempts)")
			if err != nil && strings.Contains(err.Error(), "cannot mount the providers Secret") {
				t.Errorf("ProbeImage = %v, refused a mount failure the kubelet retries", err)
			}
			if n := g.creates.Load(); n != 2 {
				t.Errorf("ran the probe %d times, want the retry's 2", n)
			}
		})
	}
}

// The probe runs as a worker runs: with provider keys configured it exports the providers Secret's
// keys as the worker's shim does, and given the daemon's role prompts' references it resolves
// those; with neither, its command carries neither flag.
func TestTheProbeRunsAsAWorkerRuns(t *testing.T) {
	const references = `{"LEGION_PROMPT_AGENTS":{"oracle":["roles/core/planner.md"]}}`
	for _, testCase := range []struct {
		name       string
		keys       map[string]string
		references string
		want       []string
	}{
		{"neither", nil, "", nil},
		{"provider keys", map[string]string{"ANTHROPIC_API_KEY": "anthropic"}, "", []string{"--provider-env-dir", ProvidersDir}},
		{"role references", nil, references, []string{"--role-references", references}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			opts := goldenOptions()
			opts.ProviderKeys = testCase.keys
			r, err := configure(opts)
			if err != nil {
				t.Fatal(err)
			}
			command := r.probeManifest(probeSandboxName, ImageProbe{Contract: 3, RoleReferences: testCase.references}, time.Now()).Spec.PodTemplate.Spec.Containers[0].Command
			base := []string{opts.Tools.Legion, "probe-image", "--go-daemon-api-version", "3", "--plugin-root", legionPlugin, "--pod-safety"}
			if want := append(base, testCase.want...); !slices.Equal(command, want) {
				t.Errorf("the probe's command = %q, want %q", command, want)
			}
		})
	}
}

// The probe's name is scoped to the project and the image, and fits a DNS label however long the
// project is.
func TestProbeNameIsTheProjectAndTheImage(t *testing.T) {
	if got := probeName(testProject, testDigestHex); got != probeSandboxName {
		t.Errorf("probeName = %q, want %q", got, probeSandboxName)
	}
	long := probeName(strings.Repeat("widgets", 12), testDigestHex)
	if len(long) > maxNameLength || !strings.HasPrefix(long, "legion-probe-widgetswidgets") || !strings.HasSuffix(long, "-1d10089a0000") {
		t.Errorf("probeName of a long project = %q (%d characters), want a DNS label ending in the digest", long, len(long))
	}
}

// The probe container is told the operator's variables as written, as a worker's container is,
// although the kubelet expands `$(NAME)` and `$$` in a container's env values (kubeletLiteral).
func TestTheProbeIsToldTheOperatorsVariablesAsWritten(t *testing.T) {
	opts := goldenOptions()
	literal := "/etc/legion-operator/$(HOME)/a$$b.yml"
	opts.Pod.Env = map[string]string{"PI_CONFIG_FILES": literal}
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	env := envOf(r.probeManifest(probeSandboxName, ImageProbe{Contract: 3}, time.Now()).Spec.PodTemplate.Spec.Containers[0])
	if seen := kubeExpand(env["PI_CONFIG_FILES"], env); seen != literal {
		t.Errorf("the probe process is told PI_CONFIG_FILES = %q, want %q", seen, literal)
	}
}

// The golden pins every byte of the probe Sandbox, and every field is one the v1.0.3 CRD declares
// with that type: the lifecycle fields the worker Sandboxes never set included.
func TestProbeManifestGoldenAndSchema(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	small := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("1Gi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
	}
	u, err := encodeProbe(r.probeManifest(probeSandboxName, ImageProbe{Contract: 3, Resources: small}, time.Date(2026, 9, 23, 12, 5, 30, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	manifest := wire(t, u.Object)
	if found := schemaViolations(manifest, sandboxSchema(t), ""); len(found) > 0 {
		t.Fatalf("the probe manifest has fields the CRD does not declare as sent:\n%s", strings.Join(found, "\n"))
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(manifest); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "golden", "probe.json")
	if *updateGolden {
		if err := os.WriteFile(path, buffer.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run `go test ./internal/runtime/sandbox/ -update`): %v", err)
	}
	if !bytes.Equal(buffer.Bytes(), want) {
		t.Fatalf("%s is stale\n got: %s\nwant: %s", path, buffer.Bytes(), want)
	}
}

// slogTo is a logger that records everything the runtime says into w.
func slogTo(w io.Writer) *slog.Logger { return slog.New(slog.NewTextHandler(w, nil)) }
