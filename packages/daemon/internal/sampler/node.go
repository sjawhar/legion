package sampler

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/sjawhar/legion/daemon/internal/procread"
)

// This file is node mode: the loop the DaemonSet of a grade node runs, which reports every pod of
// its own node rather than one pod's roles (dispatch://INFRA-1208/spec#b-a-node). It has two
// sources where the issue pod's collector has one — the API server, which says which pods are on
// this node and what each is labelled, and the node's /proc and cgroup filesystem, which say what
// each pod is spending — and it joins them by the pod uid the kernel writes into a process's
// cgroup path.
//
// What it serves is the node-mode table of docs/sampler-metrics.md: each pod's `build`, `test`
// and `other` CPU and resident memory, and the pod cgroup's own totals. There is no anonymous
// memory here, because node mode reads no `smaps`: another user's is refused without rights this
// read-only setup does not give, and a node's sampler would have every process of every pod to
// read.
//
// Nothing here opens a path of its own either: every read is given NodeOptions.Root and every
// call to the API server goes through NodeOptions.Client, so a test drives the whole loop over a
// directory it wrote and a fake clientset.

// The rates, each a package var rather than a constant so that a test runs a hundred passes in
// the time a node spends on one, as pod.go's rates are.
var (
	// nodeInterval is the loop's own tick: every pod cgroup's totals, which are two small
	// files a pod (dispatch://INFRA-1208/spec#b-a-node: "cgroup totals every second").
	nodeInterval = time.Second
	// nodeWalkInterval is how often every process of the node is read ("processes at least
	// every 2 seconds").
	nodeWalkInterval = 2 * time.Second
	// nodeRetry is how long the watcher waits after a list it could not make, so a sampler
	// whose API server is refusing it asks once a second and not as fast as it can.
	nodeRetry = time.Second
)

// The labels node mode reads from a pod, and the one value that stops it reporting the pod at all.
const (
	// queueLabel is the Kueue queue a grade pod was admitted through, and ownerLabel who the
	// pod is charged to. Each is read from the pod, and from its owning Job where the pod
	// carries none.
	queueLabel = "kueue.x-k8s.io/queue-name"
	ownerLabel = "owner"
	// detailLabel set to detailOff is a pod asking not to be reported, which the sampler
	// obeys whatever its namespace lists say (dispatch://INFRA-1141/spec#b-r-switch).
	detailLabel = "monitoring.theorem.dev/detail"
	detailOff   = "off"
	// nodeField is the field selector the API server filters the pod list and the watch by, so
	// a node's sampler is sent its own node's pods and never the cluster's.
	nodeField = "spec.nodeName"
	// jobKind is the owner reference node mode follows for the two labels.
	jobKind = "Job"
)

// cgroupMount is the cgroup filesystem under a root, which is where a pod's own cgroup directory
// is read: procread names the same path from the other side (DetectLayout), and this is the one
// place node mode builds a path of the filesystem rather than being handed one.
func cgroupMount(root string) string { return filepath.Join(root, "sys", "fs", "cgroup") }

// NodeOptions is everything a node's sampler reads and everything it writes to. Every
// collaborator is injected — the root each procread call is given, the clientset, the registry
// the readings land in, the clock and the log — so a test drives the whole loop over a directory
// of its own and the fake clientset.
type NodeOptions struct {
	// Root is the root every procread call is given: "/" on a node, where the DaemonSet runs
	// with the host's process namespace and the host's /proc and /sys/fs/cgroup mounted
	// read-only, and a fixture directory in a test.
	Root string
	// NodeName is the node this sampler runs on, which the DaemonSet reads from the downward
	// API. It is the value of the spec.nodeName field selector and nothing else.
	NodeName string
	// Namespaces are the namespaces served, and empty is every namespace.
	Namespaces []string
	// ExcludeNamespaces and ExcludePods are the two lists that stop all collection about a
	// pod, whatever Namespaces says (dispatch://INFRA-1141/spec#b-r-switch).
	ExcludeNamespaces []string
	ExcludePods       []string
	// Rules are the deployment's kind rules, which decide what a process's work is counted
	// as. Node mode serves their `build` and `test` answers and counts everything else as
	// `other`.
	Rules Rules
	// Registry is where every reading goes, and what the HTTP handler serves. It is a node
	// registry (NewNodeRegistry).
	Registry *Registry
	// Client is the API server this node's pods are listed and watched through: the in-cluster
	// client of NodeClient in a pod, and the fake clientset in a test.
	Client kubernetes.Interface
	// Now is the clock the collector is given, as PodOptions.Now is, and what a test hands
	// the registry beside it. A nil clock is the wall clock.
	Now func() time.Time
	// Log takes one line about a read that failed or about the node the sampler reads.
	Log func(string)
}

// check refuses options the collector could not run with, in the shape PodOptions.check uses:
// the field, lower case, and what it must be.
func (opts NodeOptions) check() error {
	switch {
	case opts.Root == "":
		return errors.New("sampler: root is required")
	case opts.NodeName == "":
		return errors.New("sampler: node name is required")
	case opts.Registry == nil:
		return errors.New("sampler: registry is required")
	case opts.Client == nil:
		return errors.New("sampler: kubernetes client is required")
	case opts.Log == nil:
		return errors.New("sampler: log is required")
	}
	return nil
}

// NodeClient is the API server as a node's sampler reaches it: the in-cluster credentials of the
// DaemonSet's service account, which is the half of internal/daemon/kubernetes.go's kubeClient
// that needs no kubeconfig — a DaemonSet has none, and its token and the cluster's CA are the
// files the kubelet mounts into every pod.
func NodeClient() (kubernetes.Interface, error) {
	rc, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("the sampler runs as a DaemonSet, so it reads in-cluster credentials: %w", err)
	}
	client, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("the sampler's kubernetes client: %w", err)
	}
	return client, nil
}

// RunNode reads this node until its context ends. It returns ctx.Err(), as RunPod does, and
// nothing short of the context stops it: a read that fails is counted as
// legion_sampler_read_errors_total under its own source and the next pass runs anyway.
//
// Two goroutines run: this one, which reads /proc and the cgroup filesystem at the rates above,
// and the watcher, which holds the pod set the readings are charged to. The watcher is joined
// before this returns, so a sampler that has ended has stopped writing to the registry and to
// the log.
func RunNode(ctx context.Context, opts NodeOptions) error {
	collector, err := newNodeCollector(opts)
	if err != nil {
		return err
	}
	following, stop := context.WithCancel(ctx)
	listed, ended := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(ended)
		collector.follow(following, listed)
	}()
	defer func() {
		stop()
		<-ended
	}()
	// The first pass waits for the first list, since a pass over a node's /proc with no pod
	// set would charge every process it reads to nobody and serve nothing at all.
	select {
	case <-listed:
	case <-ctx.Done():
		return ctx.Err()
	}
	ticker := time.NewTicker(nodeInterval)
	defer ticker.Stop()
	for tick := int64(0); ; tick++ {
		collector.pass(tick)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// nodeCollector is one RunNode's state: the pod set the watcher holds, what the last pass read of
// each process, and the last message logged for each source, so a read that fails every second is
// reported once and not sixty times a minute.
//
// The mutex guards what the two goroutines share — the pod set, and the logged messages beside
// it. Everything else here belongs to the sampling loop alone.
type nodeCollector struct {
	opts     NodeOptions
	pageSize int64
	// walkEvery is the process walk's rate in ticks of nodeInterval.
	walkEvery int64

	mu     sync.Mutex
	pods   map[types.UID]nodePod
	logged map[ReadSource]string

	// last is the reading each process of the node left behind, which is what charges the
	// rise alone and what the CPU a parent reaps is spread over.
	last map[int]*nodeProcess
}

// nodePod is one pod of this node as the collector holds it: the labels every series of it
// carries, and its own cgroup directory, which the first process of it a pass reads gives. A pod
// no process of which has been read yet has no directory and no totals, which is the pod the
// kubelet has only just made.
type nodePod struct {
	pod    GradePod
	cgroup string
}

// nodeProcess is what the passes remember of one process of the node: the walk's own reading,
// and the pod and pod cgroup its own cgroup path names, read once — a process never leaves the
// cgroup it was made in.
//
// The reading is tree.go's, and the pair it holds is the pod uid and the kind this process's work
// was last counted as, so that the CPU a parent reaps for a child that has ended is spread by the
// one rule both modes follow (chargeReaped). An issue pod charges a role and a kind; a node
// charges a pod and a kind, and the uid stands where the role does.
type nodeProcess struct {
	reading
	uid    types.UID
	cgroup string
}

func newNodeCollector(opts NodeOptions) (*nodeCollector, error) {
	if err := opts.check(); err != nil {
		return nil, err
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	c := &nodeCollector{
		opts:      opts,
		pageSize:  int64(os.Getpagesize()),
		walkEvery: nodeTicks(nodeWalkInterval),
		pods:      map[types.UID]nodePod{},
		logged:    map[ReadSource]string{},
		last:      map[int]*nodeProcess{},
	}
	c.opts.Log(fmt.Sprintf("sampler: node %s: reading %s for the pods of this node", opts.NodeName, opts.Root))
	return c, nil
}

// nodeTicks is how many ticks of this loop's own rate one of the slower rates stands for, as
// pod.go's ticksPer is for the issue pod's loop. A rate no slower than the tick runs on every one
// of them, which is what a test that shortens one rate and not the other gets.
func nodeTicks(interval time.Duration) int64 {
	if nodeInterval <= 0 || interval <= nodeInterval {
		return 1
	}
	return int64(interval / nodeInterval)
}

// pass is one tick of the loop: every walkEvery ticks the process walk, then every pod's cgroup
// totals and the sampler's own readings. Tick zero walks, so the first totals are read with the
// cgroup directories that walk learned rather than a tick later.
func (c *nodeCollector) pass(tick int64) {
	pods := c.snapshot()
	if tick%c.walkEvery == 0 {
		c.readProcesses(pods)
	}
	c.readTotals(pods)
	c.readSelf()
}

// snapshot is the pod set as it stands for this pass. The pass works on a copy of its own, so the
// watcher goes on taking pods in and out while a walk of a node's whole /proc runs.
func (c *nodeCollector) snapshot() map[types.UID]nodePod {
	c.mu.Lock()
	defer c.mu.Unlock()
	return maps.Clone(c.pods)
}

// learn keeps the cgroup directory a pass read for a pod, so the totals of the seconds between
// two walks are read without walking /proc again.
func (c *nodeCollector) learn(uid types.UID, dir string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if pod, held := c.pods[uid]; held && pod.cgroup == "" {
		pod.cgroup = dir
		c.pods[uid] = pod
	}
}

// readProcesses is one walk of the node: every process's stat, the pod each belongs to, and the
// CPU and resident memory each pod's kinds hold. A process of no pod this sampler serves — the
// kubelet, the container runtime, a pod of an excluded namespace — is read and charged to nobody,
// since only its stat says whether the pid is still the process the last pass read.
func (c *nodeCollector) readProcesses(pods map[types.UID]nodePod) {
	pids, err := procread.PIDs(c.opts.Root)
	if err != nil {
		c.failed(SourceProc, err)
		return
	}
	stats := make(map[int]procread.Stat, len(pids))
	live := make([]int, 0, len(pids))
	for _, pid := range pids {
		stat, err := procread.ReadStat(c.opts.Root, pid)
		switch {
		case unreadable(err):
			continue
		case err != nil:
			c.failed(SourceProc, err)
			continue
		}
		stats[pid] = stat
		live = append(live, pid)
	}
	reaped := c.forget(stats)
	c.seed(live, stats)

	kinds := make(map[int]Kind, len(live))
	cpu := make(map[Pair]float64, len(live))
	resident := make(map[Pair]int64, len(live))
	for _, pid := range live {
		process, stat := c.last[pid], stats[pid]
		var pair Pair
		if pod, served := pods[process.uid]; served {
			if pod.cgroup == "" && process.cgroup != "" {
				pod.cgroup = process.cgroup
				pods[process.uid] = pod
				c.learn(process.uid, process.cgroup)
			}
			pair = Pair{Role: string(process.uid), Kind: c.kindOf(pid, stats, kinds)}
			cpu[pair] += seconds(rise(stat.UserTicks+stat.SystemTicks, process.own))
			chargeReaped(cpu, pair,
				rise(stat.ChildUserTicks+stat.ChildSystemTicks, process.reaped), reaped[pid])
			resident[pair] += int64(stat.ResidentPages) * c.pageSize
		}
		process.own = stat.UserTicks + stat.SystemTicks
		process.reaped = stat.ChildUserTicks + stat.ChildSystemTicks
		process.parent = stat.Parent
		process.pair = pair
	}
	for pair, spent := range cpu {
		c.publish(types.UID(pair.Role), func(pod GradePod) {
			c.opts.Registry.AddGradeCPUSeconds(pod, pair.Kind, spent)
		})
	}
	for pair, bytes := range resident {
		c.publish(types.UID(pair.Role), func(pod GradePod) {
			c.opts.Registry.SetGradeResidentBytes(pod, pair.Kind, bytes)
		})
	}
}

// publish hands the registry one pod's reading, under the lock the pod set is kept by and only
// where the pod is still served. The watcher forgets a pod's series under that same lock, so a
// reading taken a moment before a pod was dropped cannot put its series back afterwards, where
// nothing would take them out again until the hour window — and nothing at all would take out
// the pod's totals, which that window does not reach.
//
// The labels are the pod set's own and never the pass's copy, so a reading of a pod whose labels
// have just moved is served under the labels it carries now.
func (c *nodeCollector) publish(uid types.UID, write func(GradePod)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if pod, served := c.pods[uid]; served {
		write(pod.pod)
	}
}

// forget drops the reading of every process that is no longer there — one that ended, and one
// whose pid the kernel has since handed to another process, which its starting moment tells — and
// answers those readings by the parent that is reaping for them, each list in pid order so that
// two passes over the same /proc charge the same amounts. It is Walk.forget over this mode's own
// readings, and it bounds what the collector holds the same way.
func (c *nodeCollector) forget(stats map[int]procread.Stat) map[int][]*reading {
	var gone []*nodeProcess
	for pid, process := range c.last {
		if stat, alive := stats[pid]; alive && stat.StartTicks == process.start {
			continue
		}
		gone = append(gone, process)
		delete(c.last, pid)
	}
	slices.SortFunc(gone, func(a, b *nodeProcess) int { return cmp.Compare(a.pid, b.pid) })
	reaped := make(map[int][]*reading, len(gone))
	for _, process := range gone {
		reaped[process.parent] = append(reaped[process.parent], &process.reading)
	}
	return reaped
}

// seed gives a process the passes have not read before a reading of its own: the pod its cgroup
// path names, that pod's own cgroup directory, and — for a process of any pod of this node — the
// kind its command line gives. A process in no pod's cgroup has no command line read at all,
// since a node runs hundreds of processes of its own that no series will ever carry.
//
// Both reads happen once, on the pass that first saw the process: a process keeps the cgroup it
// was made in and the program it runs, so a second read could only cost the pass the time it
// takes.
func (c *nodeCollector) seed(live []int, stats map[int]procread.Stat) {
	for _, pid := range live {
		if _, held := c.last[pid]; held {
			continue
		}
		process := &nodeProcess{reading: reading{pid: pid, start: stats[pid].StartTicks}}
		c.last[pid] = process
		text, err := procread.ReadProcCgroup(c.opts.Root, pid)
		if err != nil {
			if !unreadable(err) {
				c.failed(SourceProc, err)
			}
			continue
		}
		uid, dir := podCgroup(c.opts.Root, text)
		if uid == "" {
			continue
		}
		process.uid, process.cgroup = uid, dir
		args, err := procread.ReadCmdline(c.opts.Root, pid)
		if err != nil && !unreadable(err) {
			c.failed(SourceProc, err)
		}
		if kind, named := c.opts.Rules.Kind(args); named {
			process.rule = kind
		}
	}
}

// kindOf is what one process's work is counted as in node mode's three words: the kind its own
// command line gives, else the kind of the nearest ancestor within the same pod that has one,
// else `other`. A node's sampler knows nothing of roles, so everything that is neither a build
// nor a test is `other` (docs/sampler-metrics.md, "The kinds").
//
// The climb stops at the pod's edge: above a pod's own processes stand the container runtime and
// the kubelet, whose work is nobody's. What each process answers is kept, as the issue pod's walk
// keeps its own climb, so a pass climbs each chain once however many processes stand on it.
func (c *nodeCollector) kindOf(pid int, stats map[int]procread.Stat, known map[int]Kind) Kind {
	chain := make([]int, 0, 8)
	answer := KindOther
	for range len(stats) + 1 {
		if kind, held := known[pid]; held {
			answer = kind
			break
		}
		process, read := c.last[pid]
		if !read {
			break
		}
		chain = append(chain, pid)
		if process.rule != "" {
			answer = nodeKind(process.rule)
			break
		}
		parent, held := c.last[stats[pid].Parent]
		if !held || parent.uid != process.uid {
			break
		}
		pid = stats[pid].Parent
	}
	for _, pid := range chain {
		known[pid] = answer
	}
	return answer
}

// nodeKind is a rule's answer in node mode's vocabulary. A rule may answer `tool` as well as
// `build` and `test`, and a deployment may write one that does; on a node that is `other`, since
// the three words the node-mode table carries are all a reader of it knows.
func nodeKind(kind Kind) Kind {
	if kind == KindBuild || kind == KindTest {
		return kind
	}
	return KindOther
}

// podCgroup is the pod a process belongs to and that pod's own cgroup directory, read from the
// text of <root>/proc/<pid>/cgroup. Both container runtime drivers are accepted, since which one
// a node runs is the kubelet's configuration and not something a sampler is told:
//
//	systemd    /kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod<uid>.slice/<container>.scope
//	cgroupfs   /kubepods/burstable/pod<uid>/<container>
//
// The systemd driver writes the uid with its dashes as underscores, which is undone here, and a
// guaranteed pod has no quality-of-service segment under either driver. A process in no pod's
// cgroup — the kubelet, the container runtime, the sampler itself — answers an empty uid.
func podCgroup(root, text string) (types.UID, string) {
	for line := range strings.SplitSeq(strings.TrimSpace(text), "\n") {
		// A line is hierarchy:controllers:path, under v2 one line and under v1 one for each
		// controller, and the path is the same pod's whichever line it is read from.
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		segments := strings.Split(strings.Trim(parts[2], "/"), "/")
		for i, segment := range segments {
			uid := podUID(segment)
			if uid == "" {
				continue
			}
			dir := filepath.Join(append([]string{cgroupMount(root)}, segments[:i+1]...)...)
			return types.UID(uid), dir
		}
	}
	return "", ""
}

// podUID is the pod uid one segment of a cgroup path names, and empty where the segment names no
// pod. The systemd driver's segment is `kubepods-<qos>-pod<uid>.slice`, or `kubepods-pod<uid>`
// for a guaranteed pod, with the uid's dashes written as underscores; the cgroupfs driver's is
// `pod<uid>`.
func podUID(segment string) string {
	name := strings.TrimSuffix(segment, ".slice")
	if cut := strings.LastIndex(name, "-pod"); cut >= 0 {
		name = name[cut+len("-"):]
	}
	uid, named := strings.CutPrefix(name, "pod")
	if !named || uid == "" {
		return ""
	}
	return strings.ReplaceAll(uid, "_", "-")
}

// readTotals is every served pod's own cgroup: the memory it holds and the CPU it has spent,
// which is the reading every second buys. A pod no process of which has been read yet has no
// cgroup directory to read, and is served its kinds alone until the next walk finds one.
func (c *nodeCollector) readTotals(pods map[types.UID]nodePod) {
	for uid, pod := range pods {
		if pod.cgroup == "" {
			continue
		}
		group, err := procread.ReadGroup(pod.cgroup)
		if err != nil {
			c.failed(SourceCgroup, err)
			continue
		}
		c.publish(uid, func(pod GradePod) {
			c.opts.Registry.SetGradePodMemoryBytes(pod, group.MemoryBytes)
			c.opts.Registry.SetGradePodCPUSeconds(pod, group.CPUSeconds)
		})
	}
}

// readSelf is what the sampler costs the node: its own CPU, the anonymous memory its exit rule is
// written against, and its resident memory. Both modes serve these three series
// (docs/sampler-metrics.md, "The series the sampler serves about itself"), and this is the smaps
// node mode does read — the sampler's own, which no permission stands in the way of. A reading
// that fails leaves the last one standing rather than serving a zero.
func (c *nodeCollector) readSelf() {
	stat, err := procread.ReadStat(c.opts.Root, selfPID())
	if err != nil {
		c.failed(SourceProc, err)
		return
	}
	c.opts.Registry.SetSelfCPUSeconds(seconds(stat.UserTicks + stat.SystemTicks))
	anonymous, err := readAnonymous(c.opts.Root, selfPID())
	if err != nil {
		c.failed(SourceProc, err)
		return
	}
	c.opts.Registry.SetSelfMemoryBytes(anonymous, int64(stat.ResidentPages)*c.pageSize)
}

// follow is the pod set: one list of this node's pods, then a watch from the version that list
// answered with, again and again until the context ends. A watch that ends is no failure — the
// API server closes one every few minutes — and the list that follows it is how a pod that
// started or ended meanwhile is picked up without a restart.
//
// listed is closed after the first list, successful or not, so the sampling loop starts once the
// pod set is as good as the API server will make it and not before.
func (c *nodeCollector) follow(ctx context.Context, listed chan<- struct{}) {
	for first := true; ctx.Err() == nil; {
		version, ok := c.list(ctx)
		if first {
			close(listed)
			first = false
		}
		if !ok {
			c.pause(ctx, nodeRetry)
			continue
		}
		c.watch(ctx, version)
	}
}

// list is the pods of this node, as the API server answers the field selector, and the pod set
// they make. A list that fails is counted as legion_sampler_read_errors_total{source="kubernetes"}
// and the loop goes on with the pod set it had, since a sampler that forgot every pod over one
// refused request would report nothing about a node that is still working.
//
// One list covers every namespace, whatever NodeOptions.Namespaces says: which namespaces are
// served is a rule about what the sampler reports (served) and not about what it may read, and
// one list and one watch of this node's pods cost the API server less than one of each for every
// namespace of a list that can be changed.
func (c *nodeCollector) list(ctx context.Context) (string, bool) {
	list, err := c.opts.Client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, c.listOptions())
	if err != nil {
		// A request the sampler's own end cut short is no read error of the node's.
		if ctx.Err() == nil {
			c.failed(SourceKubernetes, err)
		}
		return "", false
	}
	pods := make(map[types.UID]nodePod, len(list.Items))
	for i := range list.Items {
		pod := &list.Items[i]
		if !c.served(pod) {
			continue
		}
		pods[pod.UID] = nodePod{pod: c.gradePod(ctx, pod)}
	}
	c.replace(pods)
	return list.ResourceVersion, true
}

// listOptions are what every list and every watch of this node's pods carries: the field selector
// that leaves the cluster's other nodes to the samplers running on them.
func (c *nodeCollector) listOptions() metav1.ListOptions {
	return metav1.ListOptions{FieldSelector: fields.OneTermEqualSelector(nodeField, c.opts.NodeName).String()}
}

// watch follows this node's pods from the version a list answered with until the watch ends or
// the context does. A pod that starts on the node gains its series here and a pod that ends loses
// them, so nothing waits for the next list.
func (c *nodeCollector) watch(ctx context.Context, version string) {
	opts := c.listOptions()
	opts.ResourceVersion = version
	events, err := c.opts.Client.CoreV1().Pods(metav1.NamespaceAll).Watch(ctx, opts)
	if err != nil {
		if ctx.Err() == nil {
			c.failed(SourceKubernetes, err)
			c.pause(ctx, nodeRetry)
		}
		return
	}
	defer events.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case event, open := <-events.ResultChan():
			if !open {
				return
			}
			pod, isPod := event.Object.(*corev1.Pod)
			if !isPod {
				// An error event, and anything else no pod: end the watch, and the
				// list that follows it is what makes the pod set whole again.
				if event.Type == watch.Error {
					c.failed(SourceKubernetes, fmt.Errorf("the watch of %s's pods ended in an error", c.opts.NodeName))
				}
				return
			}
			switch event.Type {
			case watch.Added, watch.Modified:
				c.update(ctx, pod)
			case watch.Deleted:
				c.drop(pod.UID)
			}
		}
	}
}

// pause waits, and no longer than the context: a sampler whose context has ended waits for
// nothing.
func (c *nodeCollector) pause(ctx context.Context, wait time.Duration) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// replace is the pod set one list answered with. Every pod the list does not name has ended on
// this node, and every pod whose labels have moved was served under labels that are now wrong:
// both lose the series they had, which with the hour window over each pod's kinds is what bounds
// a node's registry.
func (c *nodeCollector) replace(pods map[types.UID]nodePod) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for uid, held := range c.pods {
		fresh, kept := pods[uid]
		if kept {
			// The cgroup directory is the kernel's and not the API server's: a pod
			// the list names again is the pod whose processes were read before it.
			fresh.cgroup = held.cgroup
			pods[uid] = fresh
		}
		if !kept || fresh.pod != held.pod {
			c.opts.Registry.ForgetGradePod(held.pod)
		}
	}
	c.pods = pods
}

// update is one pod the watch reported: served from now on under the labels it carries now, or
// dropped where this is the pod asking not to be reported, a namespace that is not served, or a
// name on the excluded list — a pod that gains the label while running loses the series it had.
func (c *nodeCollector) update(ctx context.Context, pod *corev1.Pod) {
	if !c.served(pod) {
		c.drop(pod.UID)
		return
	}
	// The Job behind the pod is read before the lock is taken: that is a call to the API
	// server, and the sampling loop publishes under this lock every second.
	grade := c.gradePod(ctx, pod)
	c.mu.Lock()
	defer c.mu.Unlock()
	held, known := c.pods[pod.UID]
	c.pods[pod.UID] = nodePod{pod: grade, cgroup: held.cgroup}
	if known && held.pod != grade {
		c.opts.Registry.ForgetGradePod(held.pod)
	}
}

// drop takes a pod out of the set and its series out of the registry.
func (c *nodeCollector) drop(uid types.UID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if held, known := c.pods[uid]; known {
		delete(c.pods, uid)
		c.opts.Registry.ForgetGradePod(held.pod)
	}
}

// served is whether this pod is reported at all: not one whose own label asks to be left alone,
// not one of an excluded namespace, not one whose name is on the excluded list, and, where the
// sampler was given a namespace list, one of those namespaces.
func (c *nodeCollector) served(pod *corev1.Pod) bool {
	switch {
	case pod.Labels[detailLabel] == detailOff:
		return false
	case slices.Contains(c.opts.ExcludeNamespaces, pod.Namespace):
		return false
	case slices.Contains(c.opts.ExcludePods, pod.Name):
		return false
	case len(c.opts.Namespaces) > 0 && !slices.Contains(c.opts.Namespaces, pod.Namespace):
		return false
	}
	return true
}

// gradePod is one pod as every series about it is labelled: its name and namespace, and the queue
// it was admitted through and the owner it is charged to, from its own labels where it carries
// them and from the Job that owns it where it does not — a Kueue workload's labels sit on the Job
// and the pods it makes need not carry them.
func (c *nodeCollector) gradePod(ctx context.Context, pod *corev1.Pod) GradePod {
	grade := GradePod{
		Pod:       pod.Name,
		Namespace: pod.Namespace,
		Queue:     pod.Labels[queueLabel],
		Owner:     pod.Labels[ownerLabel],
	}
	if grade.Queue != "" && grade.Owner != "" {
		return grade
	}
	labels, read := c.jobLabels(ctx, pod)
	if !read {
		return grade
	}
	if grade.Queue == "" {
		grade.Queue = labels[queueLabel]
	}
	if grade.Owner == "" {
		grade.Owner = labels[ownerLabel]
	}
	return grade
}

// jobLabels are the labels of the Job that owns this pod, and whether there was one to read. A
// Job that cannot be read is counted and the pod served under the labels it carries itself, since
// a missing queue is a worse answer than no answer at all only if it stops the pod being reported.
func (c *nodeCollector) jobLabels(ctx context.Context, pod *corev1.Pod) (map[string]string, bool) {
	for _, owner := range pod.OwnerReferences {
		if owner.Kind != jobKind {
			continue
		}
		job, err := c.opts.Client.BatchV1().Jobs(pod.Namespace).Get(ctx, owner.Name, metav1.GetOptions{})
		if err != nil {
			if ctx.Err() == nil {
				c.failed(SourceKubernetes, err)
			}
			return nil, false
		}
		return job.Labels, true
	}
	return nil, false
}

// failed counts one read that failed under its own source and logs it once, as the issue pod's
// collector does. The loop goes on: every reading here is independent of every other, and a node
// whose API server is refusing it still reports what its /proc says.
func (c *nodeCollector) failed(source ReadSource, err error) {
	c.opts.Registry.AddReadError(source)
	message := fmt.Sprintf("sampler: %s read failed: %v", source, err)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.logged[source] == message {
		return
	}
	c.logged[source] = message
	c.opts.Log(message)
}
