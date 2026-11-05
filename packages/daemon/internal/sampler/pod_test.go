package sampler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/procread"
)

// The tests below read a pod the test wrote: a root of its own holding proc/… and
// sys/fs/cgroup/…, the launcher's two files, and a registry with a clock the test moves. No test
// reads the host's /proc, no test sleeps for a reading, and every number asserted on is one the
// test put there.
const (
	podProject = "INFRA"
	podIssue   = "INFRA-1208"
	// samplerPID is the sampler's own process in the fixture's /proc, which is the process
	// rolesTree calls the sampler, so what the sampler reports about itself is a process of
	// the fixture and never the process running the test.
	samplerPID = 2
	// selfAnonymousKB is the anonymous memory the fixture's sampler holds.
	selfAnonymousKB = 4096
)

type podFixture struct {
	t           *testing.T
	root        string
	rolesFile   string
	volumesFile string
	cgroupRoot  string
	clock       *clock
	registry    *Registry
	log         []string
}

// newPodFixture is an issue pod as the collector reads it, with the roles file naming the two
// generations of rolesTree and nothing else written yet.
func newPodFixture(t *testing.T) *podFixture {
	t.Helper()
	at := newClock()
	f := &podFixture{
		t:        t,
		root:     t.TempDir(),
		clock:    at,
		registry: NewPodRegistry(podProject, podIssue, at.now),
	}
	f.rolesFile = filepath.Join(f.root, "roles.json")
	f.writeRoles(rolesOf())
	usePID(t, samplerPID)
	return f
}

// usePID points the sampler's readings of itself at a process of the fixture.
func usePID(t *testing.T, pid int) {
	t.Helper()
	before := selfPID
	selfPID = func() int { return pid }
	t.Cleanup(func() { selfPID = before })
}

func (f *podFixture) options() PodOptions {
	return PodOptions{
		Root:        f.root,
		Project:     podProject,
		Issue:       podIssue,
		RolesFile:   f.rolesFile,
		VolumesFile: f.volumesFile,
		CgroupRoot:  f.cgroupRoot,
		Registry:    f.registry,
		Now:         f.clock.now,
		Log:         func(line string) { f.log = append(f.log, line) },
	}
}

func (f *podFixture) collector() *podCollector {
	f.t.Helper()
	c, err := newPodCollector(f.options())
	if err != nil {
		f.t.Fatalf("the collector refused its options: %v", err)
	}
	// A collector driven by hand is closed where its loop would have closed it, so a test
	// binary running every one of these holds no pod's descriptors at the end of them.
	f.t.Cleanup(c.close)
	return c
}

func (f *podFixture) scrape() exposition { return scrape(f.t, f.registry) }

// write is one file of the fixture, under a directory made where it is not there yet.
func (f *podFixture) write(path, content string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatalf("could not make the directory of %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		f.t.Fatalf("could not write %s: %v", path, err)
	}
}

func (f *podFixture) writeJSON(path string, value any) {
	f.t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		f.t.Fatalf("could not render %s: %v", path, err)
	}
	f.write(path, string(raw))
}

// writeRoles is the launcher's roles file: each role's role-init process id at this moment.
func (f *podFixture) writeRoles(roles map[string]int) {
	f.t.Helper()
	f.writeJSON(f.rolesFile, roles)
}

// writeVolumes is the launcher's volumes file, written by the tests that have volumes: a fixture
// with none leaves PodOptions.VolumesFile empty, as a pod that declares none does.
func (f *podFixture) writeVolumes(volumes []Volume) {
	f.t.Helper()
	f.volumesFile = filepath.Join(f.root, "volumes.json")
	f.writeJSON(f.volumesFile, volumes)
}

// writeProcesses is the fixture's /proc: these processes, the meminfo beside them, and the
// sampler's own smaps, which every pass reads to report what the sampler holds. It writes all
// three together because writing the processes clears everything under proc/.
func (f *podFixture) writeProcesses(anonPages, shmem int64, processes ...fakeProcess) {
	f.t.Helper()
	writeProc(f.t, f.root, processes...)
	f.write(filepath.Join(f.root, "proc", "meminfo"), fmt.Sprintf(
		"MemTotal:       16777216 kB\nMemFree:         1048576 kB\nAnonPages:      %8d kB\nShmem:          %8d kB\n",
		anonPages, shmem))
	f.writeSmaps(samplerPID, selfAnonymousKB)
}

// writeSmaps is one process's smaps holding this much anonymous memory, spread over two mappings
// as the kernel writes one Anonymous line for each.
func (f *podFixture) writeSmaps(pid int, kB int64) {
	f.t.Helper()
	first := kB / 4
	f.write(filepath.Join(f.root, "proc", strconv.Itoa(pid), "smaps"), fmt.Sprintf(
		"560f1a000000-560f1a200000 r-xp 00000000 fe:01 1179703 /opt/legion/bin/legion\n"+
			"Rss:             %7d kB\nAnonymous:       %7d kB\n"+
			"7f9a80000000-7f9a80400000 rw-p 00000000 00:00 0\n"+
			"Rss:             %7d kB\nAnonymous:       %7d kB\n",
		first, first, kB-first, kB-first))
}

// writeV1Totals is the cgroup v1 files at the mount root, which is what this pod's gVisor kernel
// presents: the memory the pod holds, and the CPU it has spent in nanoseconds.
func (f *podFixture) writeV1Totals(memory, nanoseconds int64) {
	f.t.Helper()
	mount := filepath.Join(f.root, "sys", "fs", "cgroup")
	f.write(filepath.Join(mount, "memory", "memory.usage_in_bytes"), fmt.Sprintln(memory))
	f.write(filepath.Join(mount, "cpuacct", "cpuacct.usage"), fmt.Sprintln(nanoseconds))
}

// usePodCgroup is the cgroup profile's tree: the unified hierarchy at the mount root and the
// pod's own cgroup, which holds the pod's totals and the command cgroups below it.
func (f *podFixture) usePodCgroup(memory, microseconds int64) {
	f.t.Helper()
	mount := filepath.Join(f.root, "sys", "fs", "cgroup")
	f.write(filepath.Join(mount, "cgroup.controllers"), "cpuset cpu io memory pids\n")
	f.cgroupRoot = filepath.Join(mount, "legion.slice")
	f.write(filepath.Join(f.cgroupRoot, "memory.current"), fmt.Sprintln(memory))
	f.write(filepath.Join(f.cgroupRoot, "cpu.stat"),
		fmt.Sprintf("usage_usec %d\nuser_usec %d\nsystem_usec 0\n", microseconds, microseconds))
}

// commandGroup is one command's cgroup of the cgroup profile, named <role>-<id>, as whoever
// writes those cgroups would leave it.
type commandGroup struct {
	name                string
	microseconds        int64
	current, peak, anon int64
	pids                []int
}

func (f *podFixture) writeCommandGroup(group commandGroup) {
	f.t.Helper()
	dir := filepath.Join(f.cgroupRoot, group.name)
	f.write(filepath.Join(dir, "cpu.stat"), fmt.Sprintf("usage_usec %d\n", group.microseconds))
	f.write(filepath.Join(dir, "memory.current"), fmt.Sprintln(group.current))
	f.write(filepath.Join(dir, "memory.peak"), fmt.Sprintln(group.peak))
	f.write(filepath.Join(dir, "memory.stat"), fmt.Sprintf("anon %d\nfile 65536\nshmem 0\n", group.anon))
	ids := make([]string, 0, len(group.pids))
	for _, pid := range group.pids {
		ids = append(ids, strconv.Itoa(pid))
	}
	f.write(filepath.Join(dir, "cgroup.procs"), strings.Join(ids, "\n")+"\n")
}

// podTree is rolesTree with the CPU and the memory each process holds: ticks of /proc's own
// clock and pages, so a pass over it has numbers to charge and an order to read them in.
func podTree() []fakeProcess {
	counters := map[int]struct{ own, pages uint64 }{
		samplerPID: {own: 7, pages: 500},
		12:         {own: 20, pages: 200},
		14:         {own: 10, pages: 100},
		15:         {own: 5, pages: 50},
		21:         {own: 3, pages: 30},
	}
	tree := rolesTree()
	for i := range tree {
		tree[i].start = 900
		tree[i].own, tree[i].pages = counters[tree[i].pid].own, counters[tree[i].pid].pages
	}
	return tree
}

// useVolumeReader makes a reader of the test's own the statfs each volume reading takes, since
// no test can make a real filesystem report the bytes it wants to see: a statfs of a temporary
// directory answers for the mount the whole test tree sits on. It answers the bytes this map
// gives a path, fails for a path the map does not hold, and records every path it was asked for,
// so a test can say that a volume two roles mount is read once.
func useVolumeReader(t *testing.T, used map[string]int64) *[]string {
	t.Helper()
	before := usedBytes
	read := &[]string{}
	usedBytes = func(path string) (int64, error) {
		*read = append(*read, path)
		bytes, mounted := used[path]
		if !mounted {
			return 0, fmt.Errorf("statfs %s: no such file or directory", path)
		}
		return bytes, nil
	}
	t.Cleanup(func() { usedBytes = before })
	return read
}

// wantValue fails unless the exposition serves this number for this series. It allows the last
// bits of a sum of divisions, as the walk's own comparison does: a pair's CPU is a sum of ticks
// divided by the clock's own rate.
func wantValue(t *testing.T, e exposition, want float64, id metricID, labels ...string) {
	t.Helper()
	if got := e.value(t, id, labels...); math.Abs(got-want) > 1e-9 {
		t.Errorf("%s%v = %g, want %g", metrics[id].name, labels, got, want)
	}
}

// The /proc profile is this pod's own: the totals from the cgroup v1 files at the mount root, the
// process memory from meminfo, each role's CPU and resident memory from the process walk and its
// anonymous memory from each process's smaps, with the sampler's own cost beside them. Every
// series docs/sampler-metrics.md names for an issue pod is served from one pass of a settled
// pod — the pass after the one that first saw its processes, which is where each of them takes
// the kind its own command line gives (Walk.Next).
func TestTheProcProfileServesThePodItRead(t *testing.T) {
	f := newPodFixture(t)
	f.writeProcesses(743092, 56, sighted(podTree())...)
	f.writeV1Totals(268435456, 12_500_000_000)
	f.writeSmaps(12, 1024)
	f.writeSmaps(14, 2048)
	f.writeVolumes([]Volume{{
		Name:   "private-implementer",
		Paths:  []string{"/var/run/legion/roles/implementer/private"},
		Medium: "memory",
		Role:   "implementer",
	}})
	useVolumeReader(t, map[string]int64{"/var/run/legion/roles/implementer/private": 4 << 20})

	c := f.collector()
	c.pass(0)
	// The pod has spent what podTree holds since that first pass, and the pass that charges
	// it is the one that has read every process's command line.
	f.writeProcesses(743092, 56, podTree()...)
	c.pass(10)
	e := f.scrape()
	page := float64(os.Getpagesize())

	// The pod's own accounting, and the memory its processes hold. This kernel keeps its
	// tmpfs pages in AnonPages — 743,092 kB beside 56 kB of Shmem, as this pod reports — so
	// the two are not added: Shmem is served beside them and not inside them.
	wantValue(t, e, 268435456, podMemoryBytes, podProject, podIssue)
	wantValue(t, e, 268435456, podMemoryPeakBytes, podProject, podIssue)
	wantValue(t, e, 12.5, podCPUSecondsTotal, podProject, podIssue)
	wantValue(t, e, 743092*1024, podProcessMemoryBytes, podProject, podIssue)
	wantValue(t, e, 743092*1024, podProcessMemoryPeakBytes, podProject, podIssue)
	wantValue(t, e, 56*1024, podSharedMemoryBytes, podProject, podIssue)

	// A role's work of one kind: the CPU of every process of that pair, the resident memory
	// they hold together, and the anonymous memory of the ones the smaps pass reached.
	build := []string{podProject, podIssue, "implementer", string(KindBuild)}
	wantValue(t, e, 0.15, cpuSecondsTotal, build...)
	wantValue(t, e, 150*page, memoryResidentBytes, build...)
	wantValue(t, e, 150*page, memoryResidentPeakBytes, build...)
	wantValue(t, e, 2048*1024, memoryAnonymousBytes, build...)
	wantValue(t, e, 2048*1024, memoryAnonymousPeakBytes, build...)

	agent := []string{podProject, podIssue, "implementer", string(KindAgent)}
	wantValue(t, e, 0.2, cpuSecondsTotal, agent...)
	wantValue(t, e, 200*page, memoryResidentBytes, agent...)
	wantValue(t, e, 1024*1024, memoryAnonymousBytes, agent...)

	// A process in no role's subtree is served under role shared, which is where the sampler
	// itself and the launcher are counted.
	wantValue(t, e, 500*page, memoryResidentBytes, podProject, podIssue, RoleShared, string(KindShared))

	// What the sampler costs the pod it is reading.
	wantValue(t, e, 0.07, selfCPUSecondsTotal)
	wantValue(t, e, 500*page, selfMemoryResidentBytes)
	wantValue(t, e, selfAnonymousKB*1024, selfMemoryAnonymousBytes)

	wantValue(t, e, 4<<20, volumeUsedBytes, podProject, podIssue, "private-implementer", "memory", "implementer")

	// Nothing failed, and the budget reached every process there was to read.
	for _, source := range readSources {
		wantValue(t, e, 0, readErrorsTotal, string(source))
	}
	wantValue(t, e, 0, smapsDeferred)
}

// A pod with no cgroup filesystem to read serves no pod totals at all. The collector is given the
// layout its root holds, and a root holding none answers procread.ErrNoLayout for both halves of
// the totals: those series are left out rather than served as a zero, as every other series the
// sampler has no reading for is (series.go's addTotals), and nothing is counted as a read that
// failed, because nothing failed (INFRA-1208 round 1).
//
// What /proc alone tells it is served beside that: the memory the pod's processes hold and its
// shared memory come from /proc/meminfo, which such a pod still has.
func TestAPodWithNoCgroupFilesystemServesNoTotals(t *testing.T) {
	f := newPodFixture(t)
	f.writeProcesses(743092, 56, podTree()...)

	c := f.collector()
	if c.layout != procread.LayoutNone {
		t.Fatalf("the fixture's root holds the %s layout, want no cgroup filesystem at all", c.layout)
	}
	// Two passes, so that a totals tick and a walk tick have both run and neither has put a
	// zero in the registry.
	c.pass(0)
	c.pass(2)

	e := f.scrape()
	for _, id := range []metricID{podMemoryBytes, podMemoryPeakBytes, podCPUSecondsTotal} {
		if e.has(id, podProject, podIssue) {
			t.Errorf("%s is served for a pod with no cgroup filesystem:\n%s", metrics[id].name, e.text)
		}
	}
	wantValue(t, e, 743092*1024, podProcessMemoryBytes, podProject, podIssue)
	wantValue(t, e, 56*1024, podSharedMemoryBytes, podProject, podIssue)
	for _, source := range readSources {
		wantValue(t, e, 0, readErrorsTotal, string(source))
	}
	// Why there are no totals is in the line the collector logs when it starts, once, where an
	// operator reads it: the passes themselves say nothing at all about it.
	if !slices.ContainsFunc(f.log, func(line string) bool { return strings.Contains(line, "cgroup layout none") }) {
		t.Errorf("the collector's log does not name the layout it found: %v", f.log)
	}
}

// The cgroup profile reads the same series out of a pod that runs each command in a cgroup of its
// own: the command's own cpu.stat, memory.current, memory.peak and memory.stat, its role from the
// first segment of its cgroup's name and its kind from the first build or test process in it,
// with the pod's totals from the pod cgroup. A cgroup whose name names no role of this pod is
// served under role shared rather than inventing a role of its own.
func TestTheCgroupProfileServesEachCommandsOwnCgroup(t *testing.T) {
	f := newPodFixture(t)
	f.writeProcesses(2097152, 524288, podTree()...)
	f.usePodCgroup(1<<30, 9_000_000)

	// The pass that first sees the pod's processes reads no command line of any of them, so
	// the pass that reads the command cgroups is the one after it: a cgroup's kind is the
	// kind of the first build or test process in it, and the walk is where that comes from.
	c := f.collector()
	c.pass(0)
	f.writeCommandGroup(commandGroup{
		name: "implementer-7", microseconds: 2_000_000,
		current: 100 << 20, peak: 150 << 20, anon: 80 << 20, pids: []int{14, 15},
	})
	f.writeCommandGroup(commandGroup{
		name: "implementer-8", microseconds: 500_000,
		current: 10 << 20, peak: 10 << 20, anon: 8 << 20, pids: []int{16},
	})
	f.writeCommandGroup(commandGroup{
		name: "pytest-3", microseconds: 1_000_000,
		current: 20 << 20, peak: 20 << 20, anon: 16 << 20, pids: []int{17, 18},
	})
	c.pass(2)
	e := f.scrape()

	// The pod's totals come from the pod cgroup, and this kernel is the unified hierarchy,
	// which keeps a tmpfs out of AnonPages: there the two are added.
	wantValue(t, e, 1<<30, podMemoryBytes, podProject, podIssue)
	wantValue(t, e, 9, podCPUSecondsTotal, podProject, podIssue)
	wantValue(t, e, (2097152+524288)*1024, podProcessMemoryBytes, podProject, podIssue)
	wantValue(t, e, 524288*1024, podSharedMemoryBytes, podProject, podIssue)

	// The build cgroup: its CPU is the rise of its own cpu.stat, its resident memory what it
	// holds now, and its peak the memory.peak the kernel has recorded for it.
	build := []string{podProject, podIssue, "implementer", string(KindBuild)}
	wantValue(t, e, 2, cpuSecondsTotal, build...)
	wantValue(t, e, 100<<20, memoryResidentBytes, build...)
	wantValue(t, e, 150<<20, memoryResidentPeakBytes, build...)
	wantValue(t, e, 80<<20, memoryAnonymousBytes, build...)

	// A cgroup holding neither a build nor a test process is tool.
	tool := []string{podProject, podIssue, "implementer", string(KindTool)}
	wantValue(t, e, 0.5, cpuSecondsTotal, tool...)
	wantValue(t, e, 10<<20, memoryResidentBytes, tool...)

	// `pytest` is no role of this pod, so its cgroup is served under shared with the kind its
	// processes give it.
	shared := []string{podProject, podIssue, RoleShared, string(KindTest)}
	wantValue(t, e, 1, cpuSecondsTotal, shared...)
	wantValue(t, e, 20<<20, memoryResidentBytes, shared...)
	wantValue(t, e, 16<<20, memoryAnonymousBytes, shared...)

	// This profile reports what each command's cgroup holds, and a role's harness runs in no
	// command cgroup: it stands in the pod's totals and in no pair of its own.
	if e.has(cpuSecondsTotal, podProject, podIssue, "implementer", string(KindAgent)) {
		t.Errorf("a pair was served for processes in no command cgroup:\n%s", e.text)
	}
	// The costly per-process read belongs to the /proc profile: a command's anonymous memory
	// is its cgroup's memory.stat here.
	wantValue(t, e, 0, smapsDeferred)
}

// A cgroup that disappears between two passes keeps the last values read, which is what
// docs/sampler-metrics.md promises whoever writes those cgroups: nothing leaves the output when a
// command ends, and the numbers served are the last ones read.
func TestARemovedCgroupKeepsItsLastValues(t *testing.T) {
	f := newPodFixture(t)
	f.writeProcesses(2097152, 524288, podTree()...)
	f.usePodCgroup(1<<30, 9_000_000)

	c := f.collector()
	// The pass that first sees the pod's processes reads no command line of any of them: the
	// cgroup below is read by the pass that knows what the processes in it are doing.
	c.pass(0)
	f.writeCommandGroup(commandGroup{
		name: "implementer-7", microseconds: 2_000_000,
		current: 100 << 20, peak: 150 << 20, anon: 80 << 20, pids: []int{14, 15},
	})
	c.pass(2)
	if err := os.RemoveAll(filepath.Join(f.cgroupRoot, "implementer-7")); err != nil {
		t.Fatalf("could not remove the command's cgroup: %v", err)
	}
	f.clock.advance(2 * time.Second)
	c.pass(4)

	e := f.scrape()
	build := []string{podProject, podIssue, "implementer", string(KindBuild)}
	wantValue(t, e, 2, cpuSecondsTotal, build...)
	wantValue(t, e, 100<<20, memoryResidentBytes, build...)
	wantValue(t, e, 80<<20, memoryAnonymousBytes, build...)
	// A cgroup that is gone is no failure: it ended, which is what a command does.
	wantValue(t, e, 0, readErrorsTotal, string(SourceCgroup))
}

// A cgroup kept for one more read after its command ended reports that command's final numbers,
// under the pair every reading before them went to: the processes are gone, so the membership
// says nothing about what the cgroup was doing, and the kind it was read with stands. That is
// what docs/sampler-metrics.md asks whoever writes those cgroups to keep them for.
func TestACgroupKeptForOneMoreReadReportsTheFinalNumbers(t *testing.T) {
	f := newPodFixture(t)
	f.writeProcesses(2097152, 524288, podTree()...)
	f.usePodCgroup(1<<30, 9_000_000)

	c := f.collector()
	// The pass that first sees the pod's processes reads no command line of any of them: the
	// cgroup below is read by the pass that knows what the processes in it are doing.
	c.pass(0)
	f.writeCommandGroup(commandGroup{
		name: "implementer-7", microseconds: 2_000_000,
		current: 100 << 20, peak: 150 << 20, anon: 80 << 20, pids: []int{14, 15},
	})
	c.pass(2)
	// The build ends: its cgroup holds nobody, and the kernel's last numbers for it are the
	// ones it finished on.
	f.writeCommandGroup(commandGroup{
		name: "implementer-7", microseconds: 3_000_000,
		current: 130 << 20, peak: 160 << 20, anon: 110 << 20,
	})
	f.clock.advance(2 * time.Second)
	c.pass(4)

	e := f.scrape()
	build := []string{podProject, podIssue, "implementer", string(KindBuild)}
	wantValue(t, e, 3, cpuSecondsTotal, build...)
	wantValue(t, e, 130<<20, memoryResidentBytes, build...)
	wantValue(t, e, 160<<20, memoryResidentPeakBytes, build...)
	wantValue(t, e, 110<<20, memoryAnonymousBytes, build...)
	if e.has(cpuSecondsTotal, podProject, podIssue, "implementer", string(KindTool)) {
		t.Errorf("the command's final numbers moved to another kind:\n%s", e.text)
	}
}

// A volume several roles mount is one volume: it is read once, at the first path that can be
// read, and served under one set of labels. Reading each mount would report the same filesystem
// as many times as it is mounted.
func TestAVolumeTwoRolesMountIsReadOnce(t *testing.T) {
	f := newPodFixture(t)
	f.writeProcesses(743092, 56, podTree()...)
	f.writeV1Totals(1<<28, 1_000_000_000)
	f.writeVolumes([]Volume{{
		Name:   "workspace",
		Paths:  []string{"/pod/implementer/workspace", "/pod/reviewer/workspace"},
		Medium: "disk",
	}})
	read := useVolumeReader(t, map[string]int64{
		"/pod/implementer/workspace": 8 << 20,
		"/pod/reviewer/workspace":    8 << 20,
	})

	c := f.collector()
	c.pass(0)
	c.pass(2)

	if want := []string{"/pod/implementer/workspace", "/pod/implementer/workspace"}; !slices.Equal(*read, want) {
		t.Errorf("the volume was read at %v, want one read of the first path in each pass: %v", *read, want)
	}
	e := f.scrape()
	// A volume no single role owns is served under role shared.
	wantValue(t, e, 8<<20, volumeUsedBytes, podProject, podIssue, "workspace", "disk", RoleShared)
	served := 0
	for _, one := range e.series {
		if one.name == metrics[volumeUsedBytes].name {
			served++
		}
	}
	if served != 1 {
		t.Errorf("the pod serves %d volume series, want one for the one volume:\n%s", served, e.text)
	}
}

// A volume is served from the first pass in which it passes a mebibyte, and keeps its series
// after that: an emptyDir holding a few blocks of nothing is no series worth a scrape, and a
// volume whose bytes fall back under the mark would otherwise flicker in and out of the output.
func TestAVolumeIsServedOnceItPassesAMebibyteAndKeepsItsSeries(t *testing.T) {
	f := newPodFixture(t)
	f.writeProcesses(743092, 56, podTree()...)
	f.writeV1Totals(1<<28, 1_000_000_000)
	f.writeVolumes([]Volume{{Name: "scratch", Paths: []string{"/pod/scratch"}, Medium: "memory"}})
	used := map[string]int64{"/pod/scratch": 512 << 10}
	useVolumeReader(t, used)
	labels := []string{podProject, podIssue, "scratch", "memory", RoleShared}

	c := f.collector()
	c.pass(0)
	if e := f.scrape(); e.has(volumeUsedBytes, labels...) {
		t.Errorf("a volume holding 512 KiB is served:\n%s", e.text)
	}

	used["/pod/scratch"] = 4 << 20
	c.pass(2)
	wantValue(t, f.scrape(), 4<<20, volumeUsedBytes, labels...)

	used["/pod/scratch"] = 512 << 10
	c.pass(4)
	wantValue(t, f.scrape(), 512<<10, volumeUsedBytes, labels...)
}

// Every read that fails is counted under its own source and the pass goes on: the /proc count
// holds the walk's own failures beside the collector's, and a pod whose cgroup files cannot be
// read still reports the memory its processes hold.
func TestEveryReadErrorIsCountedUnderItsSourceAndThePassGoesOn(t *testing.T) {
	f := newPodFixture(t)
	tree := append(podTree(), fakeProcess{pid: 99, parent: 1, args: []string{"sleep"}, unparsable: true})
	f.writeProcesses(743092, 56, tree...)
	// A cgroup file holding something that is no number — the memory one, where the CPU
	// beside it is there to be read — a roles file that is not there, and a volume mounted
	// nowhere this sampler can see.
	f.writeV1Totals(1<<28, 1_000_000_000)
	f.write(filepath.Join(f.root, "sys", "fs", "cgroup", "memory", "memory.usage_in_bytes"), "not a number\n")
	if err := os.Remove(f.rolesFile); err != nil {
		t.Fatalf("could not take the roles file away: %v", err)
	}
	f.writeVolumes([]Volume{{Name: "scratch", Paths: []string{"/pod/scratch"}, Medium: "memory"}})
	useVolumeReader(t, nil)

	c := f.collector()
	for _, pass := range []struct {
		tick                 int64
		proc, cgroup, volume float64
		why                  string
	}{
		{0, 2, 1, 1, "the roles file and the unparsable process are both /proc"},
		{1, 2, 1, 1, "a tick between two memory totals reads the pod's CPU alone, which is there"},
		{2, 4, 2, 2, "the next walk reads the roles file and the volume again, and the totals tick the memory"},
	} {
		c.pass(pass.tick)
		e := f.scrape()
		wantValue(t, e, pass.proc, readErrorsTotal, string(SourceProc))
		wantValue(t, e, pass.cgroup, readErrorsTotal, string(SourceCgroup))
		wantValue(t, e, pass.volume, readErrorsTotal, string(SourceVolume))
		if t.Failed() {
			t.Fatalf("after the pass at tick %d: %s", pass.tick, pass.why)
		}
		// The loop reads on: what could be read is still served. With no roles file to
		// read, every process stands in no role's subtree, which is role shared — the
		// walk's own answer for a generation the launcher has not named — and the whole
		// pod's 45 ticks are charged there, since no process has had its command line
		// read on the pass that first saw it.
		wantValue(t, e, 743092*1024, podProcessMemoryBytes, podProject, podIssue)
		wantValue(t, e, 0.45, cpuSecondsTotal, podProject, podIssue, RoleShared, string(KindShared))
	}
	if len(f.log) == 0 {
		t.Error("nothing was logged about the reads that failed")
	}
}

// budgetTree is a pod with more processes than one smaps window can reach, each holding less
// memory than the one before it, so the order the budget is spent in is the order below.
func budgetTree() []fakeProcess {
	tree := []fakeProcess{
		{pid: 1, parent: 0, args: []string{"/opt/legion/bin/legion", "launcher"}, start: 900, pages: 1300},
		{pid: samplerPID, parent: 1, args: []string{"/opt/legion/bin/legion", "sampler"}, start: 900, pages: 1200},
	}
	for i := range 11 {
		tree = append(tree, fakeProcess{
			pid: 100 + i, parent: 1, args: []string{"sleep", "60"}, start: 900,
			pages: uint64(1100 - 100*i),
		})
	}
	return tree
}

// useCostlyAnonymousReader gives the smaps pass a reader whose cost the test decides: each read
// charges the sampler's own process another costTicks of CPU, written into the fixture's /proc,
// which is where the budget reads it back from. The budget is then decidable on a shared runner,
// where a share of the wall clock would not be.
//
// It charges the smaps read alone, which is what the two tests of the smaps budget below are
// about. A test of the whole sampler's cap charges every read the collector makes
// (useCostlyReads).
func useCostlyAnonymousReader(t *testing.T, f *podFixture, costTicks, pages uint64) *[]int {
	t.Helper()
	before := readAnonymous
	read := &[]int{}
	spent := uint64(0)
	readAnonymous = func(_ *procread.Reader, pid int) (int64, error) {
		*read = append(*read, pid)
		spent += costTicks
		chargeSelf(t, f, spent, pages)
		return 1024, nil
	}
	t.Cleanup(func() { readAnonymous = before })
	return read
}

// chargeSelf writes what a test's reads have cost into the sampler's own stat line, in ticks of
// /proc's own clock, which is where every budget of the collector reads its own spending back
// from.
//
// The line is written where it stands, as the kernel writes it, so the collector reads the
// charge back through the descriptor it holds on that file.
func chargeSelf(t *testing.T, f *podFixture, ticks, pages uint64) {
	t.Helper()
	line := statLine(fakeProcess{
		pid: selfPID(), parent: 1, start: 900, own: ticks, pages: pages,
		args: []string{"/opt/legion/bin/legion", "sampler"},
	})
	path := filepath.Join(f.root, "proc", strconv.Itoa(selfPID()), "stat")
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Errorf("could not charge the sampler's own CPU: %v", err)
	}
}

// The smaps pass spends at most 100 ms of the sampler's own CPU on each ten-second window: it
// stops at the budget, carries what it did not reach into the next window and reports how many,
// and takes the carried processes up first there, so every process is read within the windows the
// remainder needs rather than the largest being read again and again.
func TestTheSmapsBudgetStopsAndCarriesTheRestIntoTheNextWindow(t *testing.T) {
	f := newPodFixture(t)
	f.writeProcesses(743092, 56, budgetTree()...)
	f.writeV1Totals(1<<28, 1_000_000_000)
	f.writeRoles(map[string]int{})
	// Three ticks of /proc's clock is 30 ms, so four reads stand at 120 ms and the fourth is
	// the one that passes the 100 ms budget.
	read := useCostlyAnonymousReader(t, f, 3, 1200)

	c := f.collector()
	window := func(tick int64) []int {
		t.Helper()
		before := len(*read)
		c.pass(tick)
		calls := (*read)[before:]
		if len(calls) == 0 || calls[0] != samplerPID {
			t.Fatalf("the pass read %v first, want the sampler's own smaps", calls)
		}
		// Every pass reads the sampler's own anonymous memory before the window opens,
		// and that read is charged before the budget's own reading is taken.
		return calls[1:]
	}

	windows := [][]int{window(0), window(10), window(20), window(30)}
	for i, want := range [][]int{
		{1, samplerPID, 100, 101},
		{102, 103, 104, 105},
		{106, 107, 108, 109},
		{110, 1, samplerPID, 100},
	} {
		if !slices.Equal(windows[i], want) {
			t.Errorf("window %d read %v, want %v", i, windows[i], want)
		}
	}

	// Nine of the thirteen processes are carried into the next window, which is what the
	// deferred count reports.
	wantValue(t, f.scrape(), 9, smapsDeferred)

	reached := map[int]bool{}
	for _, window := range windows {
		for _, pid := range window {
			reached[pid] = true
		}
	}
	for _, process := range budgetTree() {
		if !reached[process.pid] {
			t.Errorf("process %d was never read in the four windows thirteen processes need", process.pid)
		}
	}
}

// The cap is the whole sampler's and not the smaps pass's alone: a ten-second window has 250 ms
// of the sampler's own CPU to spend, the rest of the window's work is measured from
// /proc/self/stat as the smaps read is, and smaps spends what that work leaves — at most the
// 100 ms it may spend, and nothing at all where the window's other work has already spent the
// allowance. A window that went over pays it out of the next one, which then reads fewer
// processes than a window with its whole allowance.
//
// Holding the costly read alone inside a budget holds nothing. The live run of 2026-11-05 kept
// its smaps budget to the letter and still spent 11.55 s of CPU over 319 s, 3.6% of one CPU,
// because the per-second and per-two-second work over a pod running hundreds of build processes
// was never budgeted at all (INFRA-1208).
func TestTheWholeSamplersWorkIsInsideTheCapAndSmapsSpendsWhatIsLeft(t *testing.T) {
	f := newPodFixture(t)
	f.writeProcesses(743092, 56, budgetTree()...)
	f.writeV1Totals(1<<28, 1_000_000_000)
	f.writeRoles(map[string]int{})
	// Every smaps read costs the sampler four ticks of /proc's clock, 40 ms, and so does
	// each tick's own pass work: the sampler's anonymous memory is read through the same
	// reader, which is the one thing here that spends CPU.
	read := useCostlyAnonymousReader(t, f, 4, 1200)

	c := f.collector()
	window := func(tick int64) []int {
		t.Helper()
		before := len(*read)
		c.pass(tick)
		calls := (*read)[before:]
		if len(calls) == 0 || calls[0] != samplerPID {
			t.Fatalf("the pass read %v first, want the sampler's own smaps", calls)
		}
		// The pass reads the sampler's own anonymous memory before the window opens, and
		// that read is part of the window's other work rather than of its smaps pass.
		return calls[1:]
	}

	// A window with room spends up to the 100 ms ceiling and no more, whatever the 250 ms
	// leaves: one tick of other work here, so the ceiling is what binds.
	windows := map[int64][]int{0: window(0)}
	if got, want := windows[0], []int{1, samplerPID, 100}; !slices.Equal(got, want) {
		t.Errorf("a window with room read %v, want %v", got, want)
	}

	// Ten ticks of pass work spend 400 ms, past the 250 ms the whole sampler has for one
	// window. The window that closes on them reads no process at all and defers every one of
	// the thirteen, which is the signal an operator reads.
	for tick := int64(1); tick < 10; tick++ {
		c.pass(tick)
	}
	windows[10] = window(10)
	if got := windows[10]; len(got) != 0 {
		t.Errorf("a window whose other work spent the allowance read %v, want no process at all", got)
	}
	wantValue(t, f.scrape(), float64(len(budgetTree())), smapsDeferred)

	// The window after it takes the deferred processes up where the last window that read
	// anything left off, inside what it has to spend: the window before went 150 ms past the
	// allowance, and a window pays for the one before it (nextWindowCPU), so this one reads
	// two processes where a window with its whole allowance read three. Every process is
	// still reached within the windows the remainder needs.
	windows[20] = window(20)
	if got, want := windows[20], []int{101, 102}; !slices.Equal(got, want) {
		t.Errorf("the window after it read %v, want the processes carried first, %v", got, want)
	}
	for tick := int64(30); tick <= 50; tick += 10 {
		windows[tick] = window(tick)
	}

	reached := map[int]bool{}
	for _, window := range windows {
		for _, pid := range window {
			reached[pid] = true
		}
	}
	for _, process := range budgetTree() {
		if !reached[process.pid] {
			t.Errorf("process %d was never read in the six windows thirteen processes need", process.pid)
		}
	}
}

// appear writes one process into the fixture's /proc without rewriting the tree around it.
// writeProcesses writes every stat again, the sampler's own among them, and the sampler's own
// is where useCostlyAnonymousReader holds the CPU it has charged: a test that has charged the
// sampler and then adds a process adds it this way.
func (f *podFixture) appear(process fakeProcess) {
	f.t.Helper()
	dir := filepath.Join(f.root, "proc", strconv.Itoa(process.pid))
	f.write(filepath.Join(dir, "cmdline"), strings.Join(process.args, "\x00")+"\x00")
	f.write(filepath.Join(dir, "stat"), statLine(process))
}

// costlyReads is the seam the whole sampler's cap is tested through: every read the collector
// makes — the walk's stat of each process, the pod's CPU, the pod's memory and its meminfo,
// each volume's statfs and each smaps — charges the sampler's own process a cost the test
// fixes, written into the fixture's /proc where every budget here reads its own spending back
// from, and every read is recorded.
//
// What the fixture's own files cost the machine running the test is no part of that charge. So
// what a cap is held against here is CPU the test put there and not CPU a neighbour on the
// runner spent, which is what makes the cap decidable on a shared runner.
type costlyReads struct {
	t     *testing.T
	f     *podFixture
	pages uint64
	// cost is what one read charges, and spent what every read so far has charged together.
	cost, spent time.Duration
	// stats and smaps are the processes those two reads were of, volumes the paths the
	// volume reads were of, cpu how many readings of the pod's CPU were taken and totals how
	// many of the two files the pod's memory totals are read from, each in the order the
	// passes made them.
	stats, smaps []int
	volumes      []string
	cpu, totals  int
}

// charge takes one more read's cost. /proc counts a process's CPU in hundredths of a second, so
// what the collector reads back is the whole ticks of what has been charged, which is the
// resolution a real sampler measures its own budget at too.
func (r *costlyReads) charge() {
	r.spent += r.cost
	chargeSelf(r.t, r.f, uint64(r.spent/(time.Second/procread.ClockTicks)), r.pages)
}

// useCostlyReads gives every read of the collector the same cost and holds what was read. The
// charge is taken before the read it is for, so a read of the sampler's own stat answers with
// its own cost already in it, as a real one does.
//
// It wraps whatever readers are installed when it is called, so a test that answers its volumes
// with a reader of its own (useVolumeReader) installs that one first.
func useCostlyReads(t *testing.T, f *podFixture, cost time.Duration, pages uint64) *costlyReads {
	t.Helper()
	reads := &costlyReads{t: t, f: f, pages: pages, cost: cost}
	stat, anonymous, info, memory, cpu, volume := readStat, readAnonymous, readMemInfo, readPodMemory, readPodCPU, usedBytes
	readStat = func(reader *procread.Reader, pid int) (procread.Stat, error) {
		reads.charge()
		reads.stats = append(reads.stats, pid)
		return stat(reader, pid)
	}
	readAnonymous = func(reader *procread.Reader, pid int) (int64, error) {
		reads.charge()
		reads.smaps = append(reads.smaps, pid)
		return anonymous(reader, pid)
	}
	readMemInfo = func(reader *procread.Reader) (procread.MemInfo, error) {
		reads.charge()
		reads.totals++
		return info(reader)
	}
	readPodMemory = func(reader *procread.Reader) (int64, error) {
		reads.charge()
		reads.totals++
		return memory(reader)
	}
	readPodCPU = func(reader *procread.Reader) (float64, error) {
		reads.charge()
		reads.cpu++
		return cpu(reader)
	}
	usedBytes = func(path string) (int64, error) {
		reads.charge()
		reads.volumes = append(reads.volumes, path)
		return volume(path)
	}
	t.Cleanup(func() {
		readStat, readAnonymous, readMemInfo = stat, anonymous, info
		readPodMemory, readPodCPU, usedBytes = memory, cpu, volume
	})
	return reads
}

// useCostlyTotals charges the two readings the pod's memory totals are made of and leaves the
// pod's CPU free, which is this pod's gVisor kernel writ small: a read of the pod's memory
// costs some 20 ms there — 17 ms to 20 ms for the cgroup's memory.usage_in_bytes and 12 ms to
// 20 ms for /proc/meminfo — where the cgroup's cpuacct.usage costs 0.50 ms, the sampler's own
// stat 0.00 ms and its own smaps 0.50 ms (INFRA-1208, measured 2026-11-05). The cost is charged
// per file, so one reading of the memory totals costs twice what this is given.
//
// It records the pod's CPU reads without charging for them, which is how a test says the cheap
// reading is taken every tick whatever the dear ones are doing.
func useCostlyTotals(t *testing.T, f *podFixture, cost time.Duration, pages uint64) *costlyReads {
	t.Helper()
	reads := &costlyReads{t: t, f: f, pages: pages, cost: cost}
	info, memory, cpu := readMemInfo, readPodMemory, readPodCPU
	readMemInfo = func(reader *procread.Reader) (procread.MemInfo, error) {
		reads.charge()
		reads.totals++
		return info(reader)
	}
	readPodMemory = func(reader *procread.Reader) (int64, error) {
		reads.charge()
		reads.totals++
		return memory(reader)
	}
	readPodCPU = func(reader *procread.Reader) (float64, error) {
		reads.cpu++
		return cpu(reader)
	}
	t.Cleanup(func() { readMemInfo, readPodMemory, readPodCPU = info, memory, cpu })
	return reads
}

// saturated is a fixture whose every tick spends more than a whole window's allowance: every
// read costs 110 ms of the sampler's own CPU, so the two files it reads of itself each tick
// stand at 220 ms against the 250 ms a ten-second window has, and the pod's CPU total takes it
// past.
//
// That is this pod under a build, writ large. The sampler's own reads cost 1.4% of one CPU
// idle and 3.2% to 3.8% while `go build -a ./...` saturated both CPUs, because a gVisor
// syscall under contention costs several times what it costs beside an idle pod (INFRA-1208,
// measured 2026-11-05).
func saturated(t *testing.T, f *podFixture) *costlyReads {
	t.Helper()
	f.writeProcesses(743092, 56, budgetTree()...)
	f.writeV1Totals(1<<28, 1_000_000_000)
	f.writeRoles(map[string]int{})
	f.writeVolumes([]Volume{{Name: "scratch", Paths: []string{"/pod/scratch"}, Medium: "memory"}})
	useVolumeReader(t, map[string]int64{"/pod/scratch": 4 << 20})
	return useCostlyReads(t, f, 110*time.Millisecond, 1200)
}

// The cap is the whole sampler's over the whole of a window, and the walk is what a busy pod
// costs it most: a window whose work has spent the allowance defers the next walk pass — the
// volumes with it — and the pass after that one, as often as the window needs, counting each
// as legion_sampler_walk_deferred. What stops the deferring is the floor and nothing else: a
// walk that has not run for a whole window runs whatever the allowance says.
//
// The pod's memory totals are not deferred with the walk. They are two files of their own and
// what they give up is their rate: a reading every fourth tick while the allowance is spent
// instead of their standing every second, each tick that waits counted as
// legion_sampler_totals_deferred. The pod's CPU is deferred by nothing at all.
func TestASaturatedWindowDefersTheWalkToTheFloorAndSlowsTheTotals(t *testing.T) {
	f := newPodFixture(t)
	reads := saturated(t, f)
	page := float64(os.Getpagesize())
	shared := []string{podProject, podIssue, RoleShared, string(KindShared)}

	// The first pass has the whole allowance and reads everything: the thirteen processes of
	// the tree hold 9,100 pages between them.
	c := f.collector()
	c.pass(0)
	e := f.scrape()
	wantValue(t, e, 0, walkDeferred)
	wantValue(t, e, 0, totalsDeferred)
	wantValue(t, e, 9100*page, memoryResidentBytes, shared...)
	wantValue(t, e, 1, podCPUSecondsTotal, podProject, podIssue)
	if want := []string{"/pod/scratch"}; !slices.Equal(reads.volumes, want) {
		t.Fatalf("the first pass read the volumes at %v, want %v", reads.volumes, want)
	}

	// A process appears and the pod's cgroup spends another second, and every tick from here
	// is over the allowance before it has read anything of the pod at all.
	f.appear(fakeProcess{pid: 777, parent: 1, args: []string{"gopls"}, own: 50, start: 900, pages: 1000})
	f.writeV1Totals(1<<28, 2_000_000_000)
	walks, totals := []int64{0}, []int64{0}
	for tick := int64(1); tick < 21; tick++ {
		walked, read := len(reads.volumes), reads.totals
		c.pass(tick)
		if len(reads.volumes) > walked {
			walks = append(walks, tick)
		}
		if reads.totals > read {
			totals = append(totals, tick)
		}
		// The walk waits out the whole of the first window, so what it would have read
		// waits with it, while the pod's CPU goes on being read every tick.
		if tick == 9 {
			e = f.scrape()
			wantValue(t, e, 4, walkDeferred)
			wantValue(t, e, 9100*page, memoryResidentBytes, shared...)
			wantValue(t, e, 2, podCPUSecondsTotal, podProject, podIssue)
		}
		// The floor: a window has gone by with no walk in it, so the pass at the tenth
		// tick runs one whatever the allowance says, and the process that appeared ten
		// seconds ago is read — with the kind of what started it, since this is the pass
		// that first saw it (Walk.Next).
		if tick == 10 {
			wantValue(t, f.scrape(), 10100*page, memoryResidentBytes, shared...)
		}
	}

	// The walk ran on the first pass and then once a window, which is the floor: a process
	// that appeared under a saturated pod is read within ten seconds and not sooner.
	if want := []int64{0, 10, 20}; !slices.Equal(walks, want) {
		t.Errorf("the walk ran at ticks %v, want the first pass and then the floor, %v", walks, want)
	}
	e = f.scrape()
	// Eight walk ticks waited, which is every one of them the floor did not force.
	wantValue(t, e, 8, walkDeferred)

	// The memory totals read every fourth tick and never less often: five of the eleven
	// totals ticks waited, and no two of them in a row.
	wantValue(t, e, 5, totalsDeferred)
	for i, tick := range totals[1:] {
		if gap := tick - totals[i]; gap > c.totalsFloor {
			t.Errorf("the memory totals were read at tick %d and not again until tick %d, %d ticks later",
				totals[i], tick, gap)
		}
	}
	// The pod's CPU is the cheap read, so every one of the twenty-one passes took it.
	if want := 21; reads.cpu != want {
		t.Errorf("the pod's CPU was read %d times over %d passes, want every one of them", reads.cpu, want)
	}
}

// The pod's totals are split by what they cost. The CPU is one small cgroup file and is read on
// every tick whatever the window has spent; the memory totals are two files a gVisor kernel
// answers by accounting for the pod's memory there and then, and they are read every second
// tick while the window can pay for them and every fourth once it cannot, each reading that
// waits counted as legion_sampler_totals_deferred (dispatch://INFRA-1208/spec#a3).
//
// Both cases below charge the two memory files and leave the CPU free, which is this pod writ
// small: 17 ms to 20 ms for the cgroup's memory.usage_in_bytes and 12 ms to 20 ms for
// /proc/meminfo, against 0.50 ms for its cpuacct.usage (INFRA-1208, measured 2026-11-05).
func TestTheCPUTotalIsEveryTickAndTheMemoryTotalsGiveWayToFour(t *testing.T) {
	for _, test := range []struct {
		name string
		// cost is what one of the two memory files charges, ticks the ticks the memory
		// totals were read on, and deferred how many readings waited.
		cost     time.Duration
		ticks    []int64
		deferred float64
	}{
		{
			// A reading the window can pay for: a tenth of a /proc tick a file is
			// below what the sampler's own stat reports at all, so the allowance
			// stands and the standing rate holds.
			name:  "a window that can pay for them",
			cost:  time.Millisecond,
			ticks: []int64{0, 2, 4, 6, 8, 10, 12, 14, 16, 18},
		},
		{
			// 130 ms a file is 260 ms a reading, past the 250 ms a ten-second window
			// has, so every reading but the first two leaves the window with nothing
			// and the floor is what runs them.
			name:     "a window whose allowance is spent",
			cost:     130 * time.Millisecond,
			ticks:    []int64{0, 2, 6, 10, 14, 18},
			deferred: 4,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newPodFixture(t)
			f.writeProcesses(743092, 56, budgetTree()...)
			f.writeV1Totals(1<<28, 1_000_000_000)
			f.writeRoles(map[string]int{})
			reads := useCostlyTotals(t, f, test.cost, 1200)

			c := f.collector()
			passes := 2 * c.smapsEvery
			ticks := []int64{}
			for tick := range passes {
				before := reads.totals
				c.pass(tick)
				if reads.totals > before {
					ticks = append(ticks, tick)
				}
			}

			if !slices.Equal(ticks, test.ticks) {
				t.Errorf("the memory totals were read at ticks %v, want %v", ticks, test.ticks)
			}
			wantValue(t, f.scrape(), test.deferred, totalsDeferred)
			// The CPU total is read on every pass of both, and it is the one reading
			// of the pod the cap never slows.
			if reads.cpu != int(passes) {
				t.Errorf("the pod's CPU was read %d times over %d passes, want every one of them",
					reads.cpu, passes)
			}
		})
	}
}

// What the sampler costs the pod it reads over a minute of a pod busy enough to make every
// read cost what a read costs there. This is the measurement that decides the cap, and both
// sides of it belong to the test and not to the machine it runs on, which is what makes it
// decidable on a runner shared with somebody else's build:
//
//   - every read is charged a cost the test fixes and the sampler reads back out of its own
//     /proc/self/stat (useCostlyReads), so the CPU held against the cap is CPU the test put
//     there, whatever the runner was doing meanwhile;
//
//   - the passes are driven by hand, so sixty of them are the sixty seconds of a sampler at
//     the rates docs/sampler-metrics.md gives, however long the test takes to drive them.
//
// The cost is 1 ms a read, which is about what a read through a descriptor the sampler already
// holds cost in this pod while `go build -a ./...` saturated both its CPUs; an open and a read
// of a path cost some fourteen times that (INFRA-1208, measured 2026-11-05). The cap over such
// a minute is 3% of one CPU, which is the cap over any minute, saturated or not
// (docs/sampler-metrics.md, and dispatch://INFRA-1208/spec#a3).
//
// What the sampler gives up to hold that cap is its reading rate: the walk and the volumes
// defer as often as the window needs, the memory totals fall from every second tick to every
// fourth, and the smaps pass takes what those leave. What it does not give up is the floor —
// every process's stat read in every ten-second window — the pod's CPU, read on every tick, or
// the smaps budget, which stays inside its 100 ms.
func TestTheWholeSamplerHoldsItsCapOverASaturatedMinute(t *testing.T) {
	const (
		costProcesses = 180
		readCost      = time.Millisecond
	)
	f := newPodFixture(t)
	tree := costTree(costProcesses)
	f.writeProcesses(743092, 56, tree...)
	for _, process := range tree {
		f.writeSmaps(process.pid, int64(process.pages)*4)
	}
	f.writeV1Totals(1<<28, 1_000_000_000)
	f.writeRoles(rolesOf())
	// Two volumes, each a directory that is there, so a volume pass takes the statfs a pod's
	// own volumes cost it beside the cost the fixture charges for it.
	f.writeVolumes([]Volume{
		{Name: "private-implementer", Paths: []string{f.root}, Medium: "memory", Role: "implementer"},
		{Name: "workspace", Paths: []string{f.root}, Medium: "disk"},
	})
	reads := useCostlyReads(t, f, readCost, 1200)

	c := f.collector()
	minute := int64(time.Minute / podInterval)
	stats := map[int64]map[int]bool{}
	smaps := map[int64]int{}
	walks, totals := 0, 0
	for tick := range minute {
		read, reached := len(reads.stats), len(reads.smaps)
		walked, counted := len(reads.volumes), reads.totals
		c.pass(tick)
		window := tick / c.smapsEvery
		if stats[window] == nil {
			stats[window] = map[int]bool{}
		}
		for _, pid := range reads.stats[read:] {
			stats[window][pid] = true
		}
		smaps[window] += len(reads.smaps) - reached
		if len(reads.volumes) > walked {
			walks++
		}
		if reads.totals > counted {
			totals++
		}
	}

	// What the minute cost, against the cap it is held to.
	stood := time.Duration(minute) * podInterval
	share := reads.spent.Seconds() / stood.Seconds()
	t.Logf("%d passes over %d processes spent %s of CPU for the %s they stand for: %.2f%% of one CPU, over %d walks and %d totals readings",
		minute, costProcesses, reads.spent, stood, 100*share, walks, totals)
	if share > samplerCap {
		t.Errorf("the sampler spent %.2f%% of one CPU over a saturated minute; the cap is %.0f%%",
			100*share, 100*samplerCap)
	}
	e := f.scrape()
	if served, allowed := e.value(t, selfCPUSecondsTotal), samplerCap*stood.Seconds(); served > allowed {
		t.Errorf("the sampler served %g s of its own CPU for the minute, past the %g s of the cap", served, allowed)
	}

	// The rate is what gave way, and the walk is what gave it: every walk tick either read or
	// waited, and each one that waited was counted.
	walkTicks := int(minute / c.walkEvery)
	totalsTicks := int(minute / c.totalsEvery)
	wantValue(t, e, float64(walkTicks-walks), walkDeferred)
	wantValue(t, e, float64(totalsTicks-totals), totalsDeferred)
	if walks == walkTicks {
		t.Error("no walk pass waited over a minute in which every read cost what a saturated pod's reads cost")
	}
	// What the minute did not give up is the rate the cap pays for: at a millisecond a file
	// the pod's memory costs 2 ms a reading, which every window could pay for, so all thirty
	// readings were taken and none was deferred. What gives way where they are dear instead
	// is their own rate (TestTheCPUTotalIsEveryTickAndTheMemoryTotalsGiveWayToFour).
	if totals != totalsTicks {
		t.Errorf("the memory totals were read %d times over the minute's %d totals ticks, and 2 ms a reading is inside every window's allowance",
			totals, totalsTicks)
	}
	// The pod's CPU was read on every one of the minute's ticks, whatever the allowance did.
	if reads.cpu != int(minute) {
		t.Errorf("the pod's CPU was read %d times over the minute's %d passes, want every one of them",
			reads.cpu, minute)
	}

	// The floor held under all of it: every process of the pod was read in every window.
	for window := range minute / c.smapsEvery {
		for _, process := range tree {
			if !stats[window][process.pid] {
				t.Errorf("process %d was not read in the window of ticks %d to %d, and nothing waits longer than a window",
					process.pid, window*c.smapsEvery, (window+1)*c.smapsEvery-1)
			}
		}
	}

	// So did the smaps budget: each process a window reached cost its own read and the
	// reading of the sampler's own CPU that follows it, and the last of them is the one that
	// crossed the line.
	for window, reached := range smaps {
		if spent := time.Duration(2*reached) * readCost; spent > smapsBudget+2*readCost {
			t.Errorf("the smaps pass of the window of ticks %d to %d spent %s, past the %s it may spend",
				window*c.smapsEvery, (window+1)*c.smapsEvery-1, spent, smapsBudget)
		}
	}
}

// The roles file is read again on every pass, so a generation that starts between two passes is
// picked up without a restart: its processes leave role shared for the role that owns them. A
// file caught half written — the launcher renames a new one over the old — leaves the last good
// map in place and raises a read error, rather than handing every role's processes to shared
// until a whole file arrives.
func TestTheRolesFileIsReadAgainOnEachPass(t *testing.T) {
	f := newPodFixture(t)
	f.writeProcesses(743092, 56, podTree()...)
	f.writeV1Totals(1<<28, 1_000_000_000)
	f.writeRoles(map[string]int{"implementer": 10})

	c := f.collector()
	c.pass(0)
	reviewer := []string{podProject, podIssue, "reviewer", string(KindAgent)}
	if e := f.scrape(); e.has(cpuSecondsTotal, reviewer...) {
		t.Errorf("a role the roles file does not name is served:\n%s", e.text)
	}

	// The reviewer's generation starts, and its harness spends another six ticks.
	tree := podTree()
	for i := range tree {
		if tree[i].pid == 21 {
			tree[i].own = 9
		}
	}
	f.writeProcesses(743092, 56, tree...)
	f.writeRoles(map[string]int{"implementer": 10, "reviewer": 20})
	c.pass(2)

	wantValue(t, f.scrape(), 0.06, cpuSecondsTotal, reviewer...)

	// A pass that catches the file half written charges the pairs the last whole one named.
	for i := range tree {
		if tree[i].pid == 21 {
			tree[i].own = 15
		}
	}
	f.writeProcesses(743092, 56, tree...)
	f.write(f.rolesFile, `{"implementer": 10, "revi`)
	c.pass(4)

	e := f.scrape()
	wantValue(t, e, 0.12, cpuSecondsTotal, reviewer...)
	wantValue(t, e, 1, readErrorsTotal, string(SourceProc))
}

// useRates shortens every rate of the loop together, so a test runs in milliseconds what a pod
// does in seconds and the passes keep the order they have in a pod.
func useRates(t *testing.T, tick time.Duration) {
	t.Helper()
	pod, totals, walk, smaps := podInterval, totalsInterval, walkInterval, smapsInterval
	podInterval, totalsInterval, walkInterval, smapsInterval = tick, 2*tick, 2*tick, 10*tick
	t.Cleanup(func() {
		podInterval, totalsInterval, walkInterval, smapsInterval = pod, totals, walk, smaps
	})
}

// waitUntil runs the condition until it holds, which is how a test waits on a loop of its own
// goroutine without sleeping for a fixed time.
func waitUntil(t *testing.T, what string, holds func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !holds() {
		if time.Now().After(deadline) {
			t.Fatalf("%s never happened", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// The loop reads on until its context ends, whatever fails in the meantime, and answers the
// context's own error when it does.
func TestRunPodReadsUntilItsContextEnds(t *testing.T) {
	useRates(t, time.Millisecond)
	f := newPodFixture(t)
	f.writeProcesses(743092, 56, podTree()...)
	f.writeV1Totals(1<<28, 12_500_000_000)
	f.writeVolumes([]Volume{{Name: "scratch", Paths: []string{"/pod/scratch"}, Medium: "memory"}})
	useVolumeReader(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ended := make(chan error, 1)
	go func() { ended <- RunPod(ctx, f.options()) }()

	waitUntil(t, "the loop served the pod's memory and counted three volume reads that failed", func() bool {
		e := f.scrape()
		return e.has(podMemoryBytes, podProject, podIssue) &&
			e.value(t, readErrorsTotal, string(SourceVolume)) >= 3
	})
	cancel()
	select {
	case err := <-ended:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("RunPod ended with %v, want %v", err, context.Canceled)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunPod did not return when its context ended")
	}
}

// Options the collector could not read with are refused before the loop starts, rather than
// serving a pod of zeroes.
func TestRunPodRefusesOptionsItCouldNotReadWith(t *testing.T) {
	whole := PodOptions{
		Root:      t.TempDir(),
		RolesFile: "/var/run/legion/roles.json",
		Registry:  NewPodRegistry(podProject, podIssue, nil),
		Log:       func(string) {},
	}
	for _, test := range []struct {
		name string
		take func(*PodOptions)
		want string
	}{
		{"no root", func(o *PodOptions) { o.Root = "" }, "sampler: root is required"},
		{"no roles file", func(o *PodOptions) { o.RolesFile = "" }, "sampler: roles file is required"},
		{"no registry", func(o *PodOptions) { o.Registry = nil }, "sampler: registry is required"},
		{"no log", func(o *PodOptions) { o.Log = nil }, "sampler: log is required"},
	} {
		t.Run(test.name, func(t *testing.T) {
			opts := whole
			test.take(&opts)
			err := RunPod(context.Background(), opts)
			if err == nil || err.Error() != test.want {
				t.Errorf("RunPod with %s = %v, want %q", test.name, err, test.want)
			}
		})
	}
}
