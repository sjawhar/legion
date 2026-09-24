package sandbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
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
	defer g.mu.Unlock()
	g.log = log
	reason := map[int32]string{0: "Completed", 1: "Error"}[exit]
	g.finish = func(p *corev1.Pod) {
		p.Spec.NodeName = "ip-10-1-40-7"
		p.Status = corev1.PodStatus{Phase: phase, ContainerStatuses: []corev1.ContainerStatus{{
			Name: probeContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exit, Reason: reason}},
		}}}
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
		Contract: 3, StateDir: t.TempDir(), Budget: 10 * time.Second,
		Retry:     bootprobe.Retry{Initial: time.Millisecond, Max: time.Millisecond, Attempts: 2},
		APIServer: "https://A1B2C3.gr7.us-west-2.eks.amazonaws.com",
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

// The probe is a Sandbox named for the project and the image, running the image's Go `legion
// probe-image` with the daemon's contract on the Legion pool; it passes on the OK line confirming
// that contract, and it deletes its Sandbox when it is done.
func TestProbeImagePassesOnTheOKLineConfirmingTheContract(t *testing.T) {
	g := newProbeRig(t, nil)
	g.succeeds("[legion] OMP pi.agents probe failed transiently\n" + bootprobe.OKLine("/opt/omp/bin/omp", 3) + "\n")

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
		{"another contract", func(g *probeRig) { g.succeeds(bootprobe.OKLine("/opt/omp/bin/omp", 2)) },
			[]string{"confirmed Go daemon API contract 2, this daemon requires 3"}},
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
			if _, statErr := os.Stat(filepath.Join(p.StateDir, "image-probes", testDigestHex+".json")); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("a refused image left a pass cache: %v", statErr)
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
			g.succeeds(bootprobe.OKLine("/opt/omp/bin/omp", 3))
			g.with(func() {
				g.logErr = apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, probeSandboxName, errors.New("no pods/log"))
			})
		}, true, "read probe pod " + probeSandboxName + "'s log"},
		{"the log failing on the server", func(g *probeRig) {
			g.succeeds(bootprobe.OKLine("/opt/omp/bin/omp", 3))
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
		g.succeeds(bootprobe.OKLine("/opt/omp/bin/omp", 3))

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

// A pass is remembered per image digest, contract, and placement — the cluster, the namespace, and
// the scheduling it was proven on: a daemon that boots again with all of them unchanged launches no
// probe, and a change to any one of them probes again. A devbox daemon may keep one state directory
// for a kind cluster and for production, and a pass on one proves nothing on the other. Each case
// starts from the first pass, so it differs from the cache in exactly one key, and dropping that
// key's comparison fails it.
func TestProbeImageRemembersAPassPerDigestContractAndPlacement(t *testing.T) {
	p := probeOptions(t)
	cache := filepath.Join(p.StateDir, "image-probes", testDigestHex+".json")
	g := newProbeRig(t, nil)
	g.succeeds(bootprobe.OKLine("/opt/omp/bin/omp", 3))
	if err := g.probe(p); err != nil {
		t.Fatalf("the first probe = %v", err)
	}
	var entry map[string]any
	raw, err := os.ReadFile(cache)
	if err != nil {
		t.Fatalf("read the pass cache: %v", err)
	}
	if err := json.Unmarshal(raw, &entry); err != nil {
		t.Fatalf("decode the pass cache: %v", err)
	}
	if entry["digest"] != "sha256:"+testDigestHex || entry["goDaemonApiVersion"] != float64(3) {
		t.Errorf("the pass cache = %s, want this digest at contract 3", raw)
	}

	again := newProbeRig(t, nil)
	if err := again.probe(p); err != nil {
		t.Fatalf("a probe with the pass cached = %v", err)
	}
	if n := again.creates.Load(); n != 0 {
		t.Errorf("a cached pass still created %d probe Sandboxes", n)
	}

	for name, rerun := range map[string]func() (*probeRig, ImageProbe){
		"another contract": func() (*probeRig, ImageProbe) {
			q := p
			q.Contract = 4
			return newProbeRig(t, nil).succeeds(bootprobe.OKLine("/opt/omp/bin/omp", 4)), q
		},
		"another placement": func() (*probeRig, ImageProbe) {
			return newProbeRig(t, nil, withOptions(func(o *Options) {
				o.Scheduling.NodeSelector = map[string]string{"topology.kubernetes.io/zone": "us-west-2a"}
			})).succeeds(bootprobe.OKLine("/opt/omp/bin/omp", 3)), p
		},
		"another cluster": func() (*probeRig, ImageProbe) {
			q := p
			q.APIServer = "https://127.0.0.1:40357"
			return newProbeRig(t, nil).succeeds(bootprobe.OKLine("/opt/omp/bin/omp", 3)), q
		},
		// The rig's controller stand-in serves only its own namespace, so this probe never gets a
		// pod: that it created a Sandbox at all is the cache refusing a pass from another namespace.
		"another namespace": func() (*probeRig, ImageProbe) {
			q := p
			q.Budget = unfinishedBudget
			return newProbeRig(t, nil, withOptions(func(o *Options) { o.Namespace = "legion-staging" })), q
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(cache, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			g, q := rerun()
			err := g.probe(q)
			if n := g.creates.Load(); n == 0 {
				t.Fatalf("reused the pass cached under another key: no probe Sandbox created (err %v)", err)
			}
			if name != "another namespace" && err != nil {
				t.Errorf("ProbeImage = %v, want the probe run again and passing", err)
			}
		})
	}
}

// A cache file that cannot be read, does not decode, or names another image is no pass: the probe
// runs again, and says why it ignored the file.
func TestProbeImageIgnoresACacheItCannotTrust(t *testing.T) {
	for name, contents := range map[string]string{
		"not JSON":      "{not json",
		"another field": `{"digest":"sha256:` + testDigestHex + `","goDaemonApiVersion":3,"placement":"x","probedAt":"2026-09-23T12:00:00Z","daemonApiVersion":8}`,
		"another image": `{"digest":"sha256:` + strings.Repeat("a", 64) + `","goDaemonApiVersion":3,"placement":"x","probedAt":"2026-09-23T12:00:00Z"}`,
	} {
		t.Run(name, func(t *testing.T) {
			p := probeOptions(t)
			cache := filepath.Join(p.StateDir, "image-probes", testDigestHex+".json")
			if err := os.MkdirAll(filepath.Dir(cache), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cache, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			var logged bytes.Buffer
			g := newProbeRig(t, nil, withOptions(func(o *Options) { o.Log = slogTo(&logged) }))
			g.succeeds(bootprobe.OKLine("/opt/omp/bin/omp", 3))

			if err := g.probe(p); err != nil {
				t.Fatalf("ProbeImage = %v", err)
			}
			if n := g.creates.Load(); n != 1 {
				t.Errorf("created %d probe Sandboxes, want the probe run again", n)
			}
			if !strings.Contains(logged.String(), "ignoring the image probe cache") || !strings.Contains(logged.String(), cache) {
				t.Errorf("the log does not say the cache %s was ignored:\n%s", cache, logged.String())
			}
		})
	}
}

// A pass names the cluster it was proven on, so a probe that cannot name its API server refuses to
// run rather than record a pass any cluster would reuse.
func TestProbeImageRefusesWithoutItsAPIServer(t *testing.T) {
	g := newProbeRig(t, nil)
	p := probeOptions(t)
	p.APIServer = ""

	err := g.probe(p)

	wantContains(t, err, "an API server")
	if n := g.creates.Load(); n != 0 {
		t.Errorf("created %d probe Sandboxes without an API server to record", n)
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
	u, err := encodeProbe(r.probeManifest(probeSandboxName, 3, small, time.Date(2026, 9, 23, 12, 5, 30, 0, time.UTC)))
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
