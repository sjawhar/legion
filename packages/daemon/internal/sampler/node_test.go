package sampler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// The tests below read a node the test wrote: a root of its own holding proc/… and
// sys/fs/cgroup/…, and the pods of that node in a fake clientset, as internal/runtime/sandbox's
// tests use one. No test reads the host's /proc, no test reaches an API server, and every number
// asserted on is one the test put there.
const (
	gradeNode      = "grade-us-west1-node-1"
	gradeNamespace = "grade"
	// buildPodUID and testPodUID are two pods of the node, which the fixture writes cgroup
	// paths for under either of the two container runtime drivers.
	buildPodUID = types.UID("6c2f1b1e-9a3d-4a6e-8f10-2b7b6a0c9d11")
	testPodUID  = types.UID("0f3c7a52-18b4-4d0e-9c21-5e8a44d7b602")
	// nodeSelfAnonymousKB is the anonymous memory the fixture's own sampler process holds.
	nodeSelfAnonymousKB = 2048
)

type nodeFixture struct {
	t        *testing.T
	root     string
	clock    *clock
	registry *Registry
	client   *kubefake.Clientset
}

// newNodeFixture is a grade node as node mode reads it: an empty root, a node registry on a clock
// the test moves, and an API server holding no pod yet.
func newNodeFixture(t *testing.T) *nodeFixture {
	t.Helper()
	at := newClock()
	usePID(t, samplerPID)
	return &nodeFixture{
		t:        t,
		root:     t.TempDir(),
		clock:    at,
		registry: NewNodeRegistry(at.now),
		client:   kubefake.NewClientset(),
	}
}

func (f *nodeFixture) options() NodeOptions {
	return NodeOptions{
		Root:     f.root,
		NodeName: gradeNode,
		Registry: f.registry,
		Client:   f.client,
		Now:      f.clock.now,
		Log:      func(line string) { f.t.Log(line) },
	}
}

func (f *nodeFixture) collector(opts NodeOptions) *nodeCollector {
	f.t.Helper()
	c, err := newNodeCollector(opts)
	if err != nil {
		f.t.Fatalf("the collector refused its options: %v", err)
	}
	return c
}

// list is one list of the node's pods, which the tests that drive the passes by hand make where
// the loop would have made it.
func (f *nodeFixture) list(c *nodeCollector) {
	f.t.Helper()
	if _, listed := c.list(f.t.Context()); !listed {
		f.t.Fatal("the list of this node's pods failed")
	}
}

func (f *nodeFixture) scrape() exposition { return scrape(f.t, f.registry) }

// write is one file of the fixture, under a directory made where it is not there yet.
func (f *nodeFixture) write(path, content string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatalf("could not make the directory of %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		f.t.Fatalf("could not write %s: %v", path, err)
	}
}

// nodeProc is one process of the node's /proc: what a pass reads of it, and the cgroup path the
// kernel wrote for it, which is what puts it in a pod or outside every pod.
type nodeProc struct {
	fakeProcess
	cgroup string
}

// writeProcesses is the fixture's /proc: these processes, the sampler's own beside them, and each
// process's cgroup file. The sampler's own process is always there, since every pass reports what
// the sampler costs and a test asserting on read errors must have those reads succeed.
func (f *nodeFixture) writeProcesses(processes ...nodeProc) {
	f.t.Helper()
	processes = append(processes, nodeProc{
		fakeProcess: fakeProcess{
			pid: samplerPID, parent: 1, start: 900, own: 3, pages: 400,
			args: []string{"/opt/legion/bin/legion", "sampler", "--node"},
		},
		cgroup: "/system.slice/legion-sampler.service",
	})
	plain := make([]fakeProcess, 0, len(processes))
	for _, process := range processes {
		plain = append(plain, process.fakeProcess)
	}
	writeProc(f.t, f.root, plain...)
	for _, process := range processes {
		f.write(filepath.Join(f.root, "proc", strconv.Itoa(process.pid), "cgroup"),
			fmt.Sprintf("0::%s\n", process.cgroup))
	}
	f.writeSmaps(samplerPID, nodeSelfAnonymousKB, 0o644)
}

// writeSmaps is one process's smaps holding this much anonymous memory. The grade pods' own are
// written unreadable: on a node the sampler may not read another user's smaps, and a test whose
// fixture allowed it would prove nothing about a sampler that never asks.
func (f *nodeFixture) writeSmaps(pid int, kB int64, mode os.FileMode) {
	f.t.Helper()
	path := filepath.Join(f.root, "proc", strconv.Itoa(pid), "smaps")
	f.write(path, fmt.Sprintf(
		"560f1a000000-560f1a200000 rw-p 00000000 00:00 0\nRss:             %7d kB\nAnonymous:       %7d kB\n",
		kB, kB))
	if err := os.Chmod(path, mode); err != nil {
		f.t.Fatalf("could not set the mode of %s: %v", path, err)
	}
}

// writePodCgroup is one pod's own cgroup as the kubelet leaves it on a node: the memory it holds
// and the CPU its processes have spent, in the files the unified hierarchy writes.
func (f *nodeFixture) writePodCgroup(path string, memory, microseconds int64) {
	f.t.Helper()
	dir := filepath.Join(append([]string{f.root, "sys", "fs", "cgroup"},
		strings.Split(strings.Trim(path, "/"), "/")...)...)
	f.write(filepath.Join(dir, "memory.current"), fmt.Sprintln(memory))
	f.write(filepath.Join(dir, "cpu.stat"),
		fmt.Sprintf("usage_usec %d\nuser_usec %d\nsystem_usec 0\n", microseconds, microseconds))
}

// systemdPod and cgroupfsPod are one pod's own cgroup path as each of the two container runtime
// drivers writes it: the systemd driver's slices, with the uid's dashes written as underscores,
// and the cgroupfs driver's plain directories.
func systemdPod(uid types.UID, qos string) string {
	return fmt.Sprintf("/kubepods.slice/kubepods-%s.slice/kubepods-%s-pod%s.slice",
		qos, qos, strings.ReplaceAll(string(uid), "-", "_"))
}

func cgroupfsPod(uid types.UID, qos string) string {
	return fmt.Sprintf("/kubepods/%s/pod%s", qos, uid)
}

// container is the cgroup path of one container of a pod, which is where a process of that pod
// sits: the sampler reads the pod from it and never the container.
func container(pod, id string) string {
	if strings.HasSuffix(pod, ".slice") {
		return pod + "/cri-containerd-" + id + ".scope"
	}
	return pod + "/" + id
}

// nodePodObject is one pod of this node as the API server reports it.
func nodePodObject(name string, uid types.UID, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: gradeNamespace, UID: uid, Labels: labels},
		Spec:       corev1.PodSpec{NodeName: gradeNode},
	}
}

// ownedBy is the pod with the owner reference the kubelet writes for a Job's pod.
func ownedBy(pod *corev1.Pod, job string) *corev1.Pod {
	pod.OwnerReferences = []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job}}
	return pod
}

func (f *nodeFixture) addPod(pod *corev1.Pod) {
	f.t.Helper()
	if _, err := f.client.CoreV1().Pods(pod.Namespace).Create(f.t.Context(), pod, metav1.CreateOptions{}); err != nil {
		f.t.Fatalf("could not add the pod %s: %v", pod.Name, err)
	}
}

func (f *nodeFixture) addJob(name string, labels map[string]string) {
	f.t.Helper()
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: gradeNamespace, Labels: labels}}
	if _, err := f.client.BatchV1().Jobs(gradeNamespace).Create(f.t.Context(), job, metav1.CreateOptions{}); err != nil {
		f.t.Fatalf("could not add the job %s: %v", name, err)
	}
}

// relabel is one pod the API server has changed, which the watch carries to a running sampler.
func (f *nodeFixture) relabel(name string, labels map[string]string) {
	f.t.Helper()
	pods := f.client.CoreV1().Pods(gradeNamespace)
	pod, err := pods.Get(f.t.Context(), name, metav1.GetOptions{})
	if err != nil {
		f.t.Fatalf("could not read the pod %s back: %v", name, err)
	}
	pod.Labels = labels
	if _, err := pods.Update(f.t.Context(), pod, metav1.UpdateOptions{}); err != nil {
		f.t.Fatalf("could not relabel the pod %s: %v", name, err)
	}
}

// useNodeRates shortens the loop's rates for a test, as internal/launcher's tests shorten
// testPGInterval: a hundred passes then cost what one pass costs on a node.
func useNodeRates(t *testing.T) {
	t.Helper()
	tick, walk, retry := nodeInterval, nodeWalkInterval, nodeRetry
	nodeInterval, nodeWalkInterval, nodeRetry = time.Millisecond, 2*time.Millisecond, time.Millisecond
	t.Cleanup(func() { nodeInterval, nodeWalkInterval, nodeRetry = tick, walk, retry })
}

// run is RunNode over this fixture at those rates, and the call that stops it. The test joins the
// loop before it ends, since the sampler's log and its registry are the test's own.
func (f *nodeFixture) run(opts NodeOptions) func() {
	f.t.Helper()
	useNodeRates(f.t)
	ctx, cancel := context.WithCancel(context.Background())
	ended := make(chan error, 1)
	go func() { ended <- RunNode(ctx, opts) }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		if err := <-ended; !errors.Is(err, context.Canceled) {
			f.t.Errorf("RunNode answered %v, want the context's own error", err)
		}
	}
	f.t.Cleanup(stop)
	return stop
}

// eventually waits for something the watcher's own goroutine does, and fails where it has not
// happened within two seconds. Every rate is a millisecond in these tests, so the wait is a
// thousand passes and not a wait a person notices.
func eventually(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s did not happen within two seconds", what)
}

// watching answers a channel closed once the sampler's watch of this node's pods is registered
// with the fake clientset. A test that changes a pod waits for it first: the fake replays
// nothing to a watch that began after the change, where an API server replays from the resource
// version the list answered with.
func (f *nodeFixture) watching() <-chan struct{} {
	f.t.Helper()
	started, once := make(chan struct{}), sync.Once{}
	f.client.PrependWatchReactor("pods", func(action k8stesting.Action) (bool, watch.Interface, error) {
		watcher, err := f.client.Tracker().Watch(corev1.SchemeGroupVersion.WithResource("pods"), action.GetNamespace())
		if err == nil {
			once.Do(func() { close(started) })
		}
		return true, watcher, err
	})
	return started
}

// awaitWatch waits for that channel, and fails the test where the sampler never watched.
func (f *nodeFixture) awaitWatch(started <-chan struct{}) {
	f.t.Helper()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		f.t.Fatal("the sampler did not watch this node's pods")
	}
}

// selectors are the field selectors the fake clientset recorded for every list or every watch of
// pods the sampler made.
func (f *nodeFixture) selectors(verb string) []string {
	var carried []string
	for _, action := range f.client.Actions() {
		if !action.Matches(verb, "pods") {
			continue
		}
		switch read := action.(type) {
		case k8stesting.ListActionImpl:
			carried = append(carried, read.GetListRestrictions().Fields.String())
		case k8stesting.WatchActionImpl:
			carried = append(carried, read.GetWatchRestrictions().Fields.String())
		}
	}
	return carried
}

// wantNoGradeSeries fails unless the exposition carries nothing at all about any grade pod, which
// is what a skipped pod is served.
func wantNoGradeSeries(t *testing.T, e exposition) {
	t.Helper()
	if strings.Contains(e.text, "grade_pod=") {
		t.Errorf("the sampler serves a pod it was told to skip:\n%s", e.text)
	}
}

// A node's sampler asks the API server for its own node's pods and nothing else: the field
// selector is on the list and on the watch that follows it, so a node of a thousand-pod cluster
// reads thirty pods and the API server does the filtering.
func TestTheNodesListAndWatchCarryItsOwnNodesFieldSelector(t *testing.T) {
	f := newNodeFixture(t)
	f.writeProcesses()
	stop := f.run(f.options())
	eventually(t, "the sampler lists and watches this node's pods", func() bool {
		return len(f.selectors("list")) > 0 && len(f.selectors("watch")) > 0
	})
	stop()

	want := "spec.nodeName=" + gradeNode
	for _, verb := range []string{"list", "watch"} {
		carried := f.selectors(verb)
		if len(carried) == 0 {
			t.Fatalf("the sampler made no %s of this node's pods", verb)
		}
		for _, got := range carried {
			if got != want {
				t.Errorf("a %s of pods carries the field selector %q, want %q", verb, got, want)
			}
		}
	}
}

// A Kueue workload's labels sit on the Job, and the pods it makes need not carry them: a pod
// whose own labels carry neither takes both from the Job that owns it, and a pod that carries its
// own keeps them whatever the Job says.
func TestAPodTakesTheQueueAndTheOwnerOfItsJobWhereItsOwnLabelsCarryNone(t *testing.T) {
	f := newNodeFixture(t)
	f.addJob("grade-run-7", map[string]string{queueLabel: "grade-queue", ownerLabel: "alice"})
	f.addJob("grade-run-8", map[string]string{queueLabel: "job-queue", ownerLabel: "the-job"})
	f.addPod(ownedBy(nodePodObject("grade-run-7-ab12", buildPodUID, nil), "grade-run-7"))
	f.addPod(ownedBy(nodePodObject("grade-run-8-cd34", testPodUID,
		map[string]string{queueLabel: "pod-queue", ownerLabel: "the-pod"}), "grade-run-8"))
	f.writeProcesses(
		nodeProc{
			fakeProcess: fakeProcess{pid: 100, parent: 1, start: 1000, own: 50, pages: 300,
				args: []string{"go", "build", "./..."}},
			cgroup: container(systemdPod(buildPodUID, "burstable"), "aa11"),
		},
		nodeProc{
			fakeProcess: fakeProcess{pid: 200, parent: 1, start: 1000, own: 25, pages: 100,
				args: []string{"pytest", "-q"}},
			cgroup: container(cgroupfsPod(testPodUID, "besteffort"), "bb22"),
		},
	)

	c := f.collector(f.options())
	f.list(c)
	c.pass(0)

	e := f.scrape()
	wantValue(t, e, 0.5, gradeCPUSecondsTotal,
		"grade-run-7-ab12", gradeNamespace, string(KindBuild), "grade-queue", "alice")
	wantValue(t, e, 0.25, gradeCPUSecondsTotal,
		"grade-run-8-cd34", gradeNamespace, string(KindTest), "pod-queue", "the-pod")
}

// The pod cgroup answers for the whole pod — every container of it and the kubelet's own pause —
// and its two files are what the loop reads every second.
func TestAPodsTotalsComeFromItsOwnCgroup(t *testing.T) {
	f := newNodeFixture(t)
	f.addPod(nodePodObject("grade-run-7-ab12", buildPodUID, map[string]string{
		queueLabel: "grade-queue", ownerLabel: "alice",
	}))
	pod := systemdPod(buildPodUID, "burstable")
	f.writeProcesses(nodeProc{
		fakeProcess: fakeProcess{pid: 100, parent: 1, start: 1000, own: 50, pages: 300,
			args: []string{"go", "build", "./..."}},
		cgroup: container(pod, "aa11"),
	})
	f.writePodCgroup(pod, 3<<30, 12_500_000)

	c := f.collector(f.options())
	f.list(c)
	c.pass(0)

	e := f.scrape()
	labels := []string{"grade-run-7-ab12", gradeNamespace, "grade-queue", "alice"}
	wantValue(t, e, 3<<30, gradePodMemoryBytes, labels...)
	wantValue(t, e, 3<<30, gradePodMemoryPeakBytes, labels...)
	wantValue(t, e, 12.5, gradePodCPUSecondsTotal, labels...)
}

// Three switches stop all collection about a pod, and each of them serves none of its series: the
// pod's own label, the excluded namespaces and the excluded pod names
// (dispatch://INFRA-1141/spec#b-r-switch). A namespace list the pod's namespace is not on is the
// fourth way a pod goes unserved.
func TestASkippedPodServesNoSeriesAtAll(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]string
		change func(*NodeOptions)
	}{
		{name: "the pod asks not to be reported", labels: map[string]string{detailLabel: detailOff}},
		{name: "its namespace is excluded", change: func(o *NodeOptions) {
			o.ExcludeNamespaces = []string{"kube-system", gradeNamespace}
		}},
		{name: "its name is excluded", change: func(o *NodeOptions) {
			o.ExcludePods = []string{"grade-run-7-ab12"}
		}},
		{name: "another namespace is the only one served", change: func(o *NodeOptions) {
			o.Namespaces = []string{"grade-harbor"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNodeFixture(t)
			f.addPod(nodePodObject("grade-run-7-ab12", buildPodUID, tc.labels))
			pod := systemdPod(buildPodUID, "burstable")
			f.writeProcesses(nodeProc{
				fakeProcess: fakeProcess{pid: 100, parent: 1, start: 1000, own: 50, pages: 300,
					args: []string{"go", "build", "./..."}},
				cgroup: container(pod, "aa11"),
			})
			f.writePodCgroup(pod, 3<<30, 12_500_000)

			opts := f.options()
			if tc.change != nil {
				tc.change(&opts)
			}
			c := f.collector(opts)
			f.list(c)
			c.pass(0)

			wantNoGradeSeries(t, f.scrape())
		})
	}
}

// A pod that is labelled off while it runs loses the series it already had: the watch carries the
// change, and the registry forgets the pod rather than serving a reading that stops moving.
func TestAPodLabelledOffWhileRunningLosesItsSeries(t *testing.T) {
	f := newNodeFixture(t)
	f.addPod(nodePodObject("grade-run-7-ab12", buildPodUID, map[string]string{
		queueLabel: "grade-queue", ownerLabel: "alice",
	}))
	pod := systemdPod(buildPodUID, "burstable")
	f.writeProcesses(nodeProc{
		fakeProcess: fakeProcess{pid: 100, parent: 1, start: 1000, own: 50, pages: 300,
			args: []string{"go", "build", "./..."}},
		cgroup: container(pod, "aa11"),
	})
	f.writePodCgroup(pod, 3<<30, 12_500_000)

	started := f.watching()
	stop := f.run(f.options())
	defer stop()
	eventually(t, "the pod's series are served", func() bool {
		return strings.Contains(scrape(t, f.registry).text, "grade-run-7-ab12")
	})
	f.awaitWatch(started)
	f.relabel("grade-run-7-ab12", map[string]string{
		queueLabel: "grade-queue", ownerLabel: "alice", detailLabel: detailOff,
	})
	eventually(t, "the pod's series leave the output", func() bool {
		return !strings.Contains(scrape(t, f.registry).text, "grade-run-7-ab12")
	})
}

// Which container runtime driver a node runs is the kubelet's configuration and not something a
// sampler is told, so a process is read into its pod under both: the systemd driver's slices,
// whose uid is written with underscores, and the cgroupfs driver's directories.
func TestAProcessIsReadIntoItsPodUnderBothCgroupDrivers(t *testing.T) {
	f := newNodeFixture(t)
	f.addPod(nodePodObject("grade-systemd", buildPodUID, map[string]string{ownerLabel: "alice"}))
	f.addPod(nodePodObject("grade-cgroupfs", testPodUID, map[string]string{ownerLabel: "bob"}))
	systemd, cgroupfs := systemdPod(buildPodUID, "burstable"), cgroupfsPod(testPodUID, "besteffort")
	f.writeProcesses(
		nodeProc{
			fakeProcess: fakeProcess{pid: 100, parent: 1, start: 1000, own: 50, pages: 300,
				args: []string{"go", "build", "./..."}},
			cgroup: container(systemd, "aa11"),
		},
		nodeProc{
			fakeProcess: fakeProcess{pid: 200, parent: 1, start: 1000, own: 25, pages: 100,
				args: []string{"go", "build", "./..."}},
			cgroup: container(cgroupfs, "bb22"),
		},
	)
	f.writePodCgroup(systemd, 1<<30, 2_000_000)
	f.writePodCgroup(cgroupfs, 2<<30, 4_000_000)

	c := f.collector(f.options())
	f.list(c)
	c.pass(0)

	e := f.scrape()
	page := int64(os.Getpagesize())
	wantValue(t, e, 0.5, gradeCPUSecondsTotal, "grade-systemd", gradeNamespace, string(KindBuild), "", "alice")
	wantValue(t, e, float64(300*page), gradeMemoryResidentBytes,
		"grade-systemd", gradeNamespace, string(KindBuild), "", "alice")
	wantValue(t, e, 2, gradePodCPUSecondsTotal, "grade-systemd", gradeNamespace, "", "alice")
	wantValue(t, e, 0.25, gradeCPUSecondsTotal, "grade-cgroupfs", gradeNamespace, string(KindBuild), "", "bob")
	wantValue(t, e, float64(100*page), gradeMemoryResidentBytes,
		"grade-cgroupfs", gradeNamespace, string(KindBuild), "", "bob")
	wantValue(t, e, 4, gradePodCPUSecondsTotal, "grade-cgroupfs", gradeNamespace, "", "bob")
}

// Node mode's vocabulary is three words: a build is `build`, a test run is `test`, and everything
// else a pod does — a shell, a language server, the container's own init — is `other`. A process
// a build started is the build's, as it is in an issue pod.
func TestAPodsWorkIsBuildTestOrOther(t *testing.T) {
	f := newNodeFixture(t)
	f.addPod(nodePodObject("grade-run-7-ab12", buildPodUID, map[string]string{ownerLabel: "alice"}))
	pod := container(systemdPod(buildPodUID, "burstable"), "aa11")
	f.writeProcesses(
		nodeProc{fakeProcess: fakeProcess{pid: 100, parent: 1, start: 1000, own: 1, pages: 10,
			args: []string{"/sbin/docker-init", "--"}}, cgroup: pod},
		nodeProc{fakeProcess: fakeProcess{pid: 101, parent: 100, start: 1000, own: 20, pages: 100,
			args: []string{"go", "build", "./..."}}, cgroup: pod},
		nodeProc{fakeProcess: fakeProcess{pid: 102, parent: 101, start: 1000, own: 30, pages: 200,
			args: []string{"/usr/lib/go/pkg/tool/linux_amd64/compile", "-o", "a.out"}}, cgroup: pod},
		nodeProc{fakeProcess: fakeProcess{pid: 103, parent: 100, start: 1000, own: 40, pages: 300,
			args: []string{"pytest", "-q"}}, cgroup: pod},
		nodeProc{fakeProcess: fakeProcess{pid: 104, parent: 100, start: 1000, own: 5, pages: 50,
			args: []string{"bash", "-lc", "sleep 60"}}, cgroup: pod},
	)

	c := f.collector(f.options())
	f.list(c)
	c.pass(0)

	e := f.scrape()
	labels := func(kind Kind) []string {
		return []string{"grade-run-7-ab12", gradeNamespace, string(kind), "", "alice"}
	}
	// The build and the compile it started are one kind, the test run another, and the
	// container's init with the shell it holds are what is left.
	wantValue(t, e, 0.5, gradeCPUSecondsTotal, labels(KindBuild)...)
	wantValue(t, e, 0.4, gradeCPUSecondsTotal, labels(KindTest)...)
	wantValue(t, e, 0.06, gradeCPUSecondsTotal, labels(KindOther)...)
	page := int64(os.Getpagesize())
	wantValue(t, e, float64(300*page), gradeMemoryResidentBytes, labels(KindBuild)...)
	wantValue(t, e, float64(300*page), gradeMemoryResidentBytes, labels(KindTest)...)
	wantValue(t, e, float64(60*page), gradeMemoryResidentBytes, labels(KindOther)...)
}

// Node mode reads no grade pod's smaps. Another user's is refused without rights the read-only
// setup does not give, and a node's sampler has every process of every pod to read, so what it
// serves is resident memory and no anonymous memory at all. The only smaps it reads is its own.
func TestNodeModeReadsNoGradePodsSmaps(t *testing.T) {
	f := newNodeFixture(t)
	f.addPod(nodePodObject("grade-run-7-ab12", buildPodUID, map[string]string{ownerLabel: "alice"}))
	pod := container(systemdPod(buildPodUID, "burstable"), "aa11")
	f.writeProcesses(
		nodeProc{fakeProcess: fakeProcess{pid: 100, parent: 1, start: 1000, own: 50, pages: 300,
			args: []string{"go", "build", "./..."}}, cgroup: pod},
		nodeProc{fakeProcess: fakeProcess{pid: 101, parent: 100, start: 1000, own: 10, pages: 100,
			args: []string{"pytest", "-q"}}, cgroup: pod},
	)
	// Each grade process has an smaps no other user may read, as a node's has.
	f.writeSmaps(100, 1<<20, 0o000)
	f.writeSmaps(101, 1<<20, 0o000)
	read := useAnonymousReader(t)

	c := f.collector(f.options())
	f.list(c)
	c.pass(0)
	c.pass(2)

	for _, pid := range *read {
		if pid != samplerPID {
			t.Errorf("the sampler read the smaps of process %d, and node mode reads none", pid)
		}
	}
	if !slices.Contains(*read, samplerPID) {
		t.Error("the sampler read no smaps at all, not even its own")
	}
	e := f.scrape()
	// The pass did read both processes, so the reading that is missing is the smaps one and
	// not the whole pod: their resident memory, which /proc/<pid>/stat holds, is served.
	page := int64(os.Getpagesize())
	wantValue(t, e, float64(300*page), gradeMemoryResidentBytes,
		"grade-run-7-ab12", gradeNamespace, string(KindBuild), "", "alice")
	wantValue(t, e, float64(100*page), gradeMemoryResidentBytes,
		"grade-run-7-ab12", gradeNamespace, string(KindTest), "", "alice")
	if got := e.value(t, readErrorsTotal, string(SourceProc)); got != 0 {
		t.Errorf("%g reads of /proc failed, want none at all:\n%s", got, e.text)
	}
}

// useAnonymousReader records every process whose anonymous memory was read, and answers a number
// of its own, so a test says which smaps the sampler asked for whatever the fixture holds.
func useAnonymousReader(t *testing.T) *[]int {
	t.Helper()
	before := readAnonymous
	read := &[]int{}
	readAnonymous = func(_ string, pid int) (int64, error) {
		*read = append(*read, pid)
		return 32 << 20, nil
	}
	t.Cleanup(func() { readAnonymous = before })
	return read
}

// The pod uid is read out of a cgroup path whichever driver wrote it, and a path naming no pod —
// the kubelet's own services, the sampler's — names none.
func TestThePodOfACgroupPath(t *testing.T) {
	root := "/host"
	mount := filepath.Join(root, "sys", "fs", "cgroup")
	for _, tc := range []struct {
		name, text string
		uid        types.UID
		dir        string
	}{
		{
			name: "the systemd driver's burstable pod",
			text: "0::/kubepods.slice/kubepods-burstable.slice/" +
				"kubepods-burstable-pod6c2f1b1e_9a3d_4a6e_8f10_2b7b6a0c9d11.slice/cri-containerd-aa11.scope\n",
			uid: buildPodUID,
			dir: filepath.Join(mount, "kubepods.slice", "kubepods-burstable.slice",
				"kubepods-burstable-pod6c2f1b1e_9a3d_4a6e_8f10_2b7b6a0c9d11.slice"),
		},
		{
			name: "the systemd driver's guaranteed pod, which has no quality-of-service slice",
			text: "0::/kubepods.slice/kubepods-pod6c2f1b1e_9a3d_4a6e_8f10_2b7b6a0c9d11.slice/" +
				"cri-containerd-aa11.scope\n",
			uid: buildPodUID,
			dir: filepath.Join(mount, "kubepods.slice",
				"kubepods-pod6c2f1b1e_9a3d_4a6e_8f10_2b7b6a0c9d11.slice"),
		},
		{
			name: "the cgroupfs driver",
			text: "0::/kubepods/besteffort/pod6c2f1b1e-9a3d-4a6e-8f10-2b7b6a0c9d11/aa11\n",
			uid:  buildPodUID,
			dir:  filepath.Join(mount, "kubepods", "besteffort", "pod6c2f1b1e-9a3d-4a6e-8f10-2b7b6a0c9d11"),
		},
		{
			name: "a cgroup v1 file, one line for each controller",
			text: "12:memory:/kubepods/burstable/pod6c2f1b1e-9a3d-4a6e-8f10-2b7b6a0c9d11/aa11\n" +
				"11:cpuacct:/kubepods/burstable/pod6c2f1b1e-9a3d-4a6e-8f10-2b7b6a0c9d11/aa11\n",
			uid: buildPodUID,
			dir: filepath.Join(mount, "kubepods", "burstable", "pod6c2f1b1e-9a3d-4a6e-8f10-2b7b6a0c9d11"),
		},
		{name: "a process of no pod", text: "0::/system.slice/kubelet.service\n"},
		{name: "the root cgroup", text: "0::/\n"},
		{name: "nothing at all", text: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uid, dir := podCgroup(root, tc.text)
			if uid != tc.uid || dir != tc.dir {
				t.Errorf("podCgroup read %q and %q, want %q and %q", uid, dir, tc.uid, tc.dir)
			}
		})
	}
}

// The options a node's sampler could not run with are refused by name, as an issue pod's are.
func TestNodeOptionsAreRefusedByName(t *testing.T) {
	f := newNodeFixture(t)
	for _, tc := range []struct {
		name, want string
		change     func(*NodeOptions)
	}{
		{name: "no root", want: "root", change: func(o *NodeOptions) { o.Root = "" }},
		{name: "no node name", want: "node name", change: func(o *NodeOptions) { o.NodeName = "" }},
		{name: "no registry", want: "registry", change: func(o *NodeOptions) { o.Registry = nil }},
		{name: "no client", want: "kubernetes client", change: func(o *NodeOptions) { o.Client = nil }},
		{name: "no log", want: "log", change: func(o *NodeOptions) { o.Log = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := f.options()
			tc.change(&opts)
			err := opts.check()
			if err == nil {
				t.Fatalf("options with %s were accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal is %q, want it to name %q", err, tc.want)
			}
		})
	}
}
