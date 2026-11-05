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
	// A cgroup file holding something that is no number, a roles file that is not there, and
	// a volume mounted nowhere this sampler can see.
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
		{1, 2, 2, 1, "a tick between two walks reads the totals alone"},
		{2, 4, 3, 2, "the next walk reads the roles file and the volume again"},
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
// The stat line is written where it stands, as the kernel writes it, so the collector reads the
// charge back through the descriptor it holds on that file.
func useCostlyAnonymousReader(t *testing.T, f *podFixture, costTicks, pages uint64) *[]int {
	t.Helper()
	before := readAnonymous
	read := &[]int{}
	spent := uint64(0)
	readAnonymous = func(_ *procread.Reader, pid int) (int64, error) {
		*read = append(*read, pid)
		spent += costTicks
		line := statLine(fakeProcess{
			pid: selfPID(), parent: 1, start: 900, own: spent, pages: pages,
			args: []string{"/opt/legion/bin/legion", "sampler"},
		})
		path := filepath.Join(f.root, "proc", strconv.Itoa(selfPID()), "stat")
		if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
			t.Errorf("could not charge the sampler's own CPU: %v", err)
		}
		return 1024, nil
	}
	t.Cleanup(func() { readAnonymous = before })
	return read
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

// The cap is the whole sampler's and not the smaps pass's alone: 2% of one CPU over a
// ten-second window is 200 ms, the rest of the window's work is measured from /proc/self/stat
// as the smaps read is, and smaps spends what that work leaves — at most the 100 ms it may
// spend, and nothing at all where the window's other work has already spent the allowance.
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
	// Every smaps read costs the sampler three ticks of /proc's clock, 30 ms, and so does
	// each tick's own pass work: the sampler's anonymous memory is read through the same
	// reader, which is the one thing here that spends CPU.
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
		// The pass reads the sampler's own anonymous memory before the window opens, and
		// that read is part of the window's other work rather than of its smaps pass.
		return calls[1:]
	}

	// A window with room spends up to the 100 ms ceiling and no more, whatever the 200 ms
	// leaves: one tick of other work here, so the ceiling is what binds.
	windows := map[int64][]int{0: window(0)}
	if got, want := windows[0], []int{1, samplerPID, 100, 101}; !slices.Equal(got, want) {
		t.Errorf("a window with room read %v, want %v", got, want)
	}

	// Ten ticks of pass work spend 300 ms, past the 200 ms the whole sampler has for one
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

	// A window with its allowance again takes the deferred processes up where the last window
	// that read anything left off, so every process is still reached within the windows the
	// remainder needs.
	windows[20] = window(20)
	if got, want := windows[20], []int{102, 103, 104, 105}; !slices.Equal(got, want) {
		t.Errorf("the window after it read %v, want the processes carried first, %v", got, want)
	}
	windows[30], windows[40] = window(30), window(40)

	reached := map[int]bool{}
	for _, window := range windows {
		for _, pid := range window {
			reached[pid] = true
		}
	}
	for _, process := range budgetTree() {
		if !reached[process.pid] {
			t.Errorf("process %d was never read in the five windows thirteen processes need", process.pid)
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

// saturated is a fixture whose every tick spends more than a whole window's allowance: each
// read of the sampler's own anonymous memory charges it 21 ticks of /proc's clock, 210 ms
// against the 200 ms a ten-second window has. It answers the paths each volume pass read, which
// is what says whether a walk tick ran at all.
//
// That is this pod under a build. The sampler's own reads cost 1.4% of one CPU idle and 3.0%
// while `go build -a ./...` saturates both CPUs, because a gVisor syscall under contention
// costs several times what it costs beside it (INFRA-1208, measured 2026-11-05).
func saturated(t *testing.T, f *podFixture) *[]string {
	t.Helper()
	f.writeProcesses(743092, 56, budgetTree()...)
	f.writeV1Totals(1<<28, 1_000_000_000)
	f.writeRoles(map[string]int{})
	f.writeVolumes([]Volume{{Name: "scratch", Paths: []string{"/pod/scratch"}, Medium: "memory"}})
	volumes := useVolumeReader(t, map[string]int64{"/pod/scratch": 4 << 20})
	useCostlyAnonymousReader(t, f, 21, 1200)
	return volumes
}

// The cap is the whole sampler's over the whole of a window, and the walk is what a busy pod
// costs it most: a window whose work has already spent the allowance defers the next walk pass
// — the volumes and the pod totals with it — to the pass after that one, and counts it as
// legion_sampler_walk_deferred.
func TestAWindowThatSpentTheAllowanceDefersTheNextWalkPass(t *testing.T) {
	f := newPodFixture(t)
	volumes := saturated(t, f)
	page := float64(os.Getpagesize())
	shared := []string{podProject, podIssue, RoleShared, string(KindShared)}

	// The first pass of the first window has the whole allowance and reads everything: the
	// thirteen processes of the tree hold 9,100 pages between them.
	c := f.collector()
	c.pass(0)
	e := f.scrape()
	wantValue(t, e, 0, walkDeferred)
	wantValue(t, e, 9100*page, memoryResidentBytes, shared...)
	wantValue(t, e, 1, podCPUSecondsTotal, podProject, podIssue)
	if want := []string{"/pod/scratch"}; !slices.Equal(*volumes, want) {
		t.Fatalf("the first pass read the volumes at %v, want %v", *volumes, want)
	}

	// The tick between the two walks reads the totals and the sampler's own three files, and
	// that is the window's whole allowance spent.
	c.pass(1)

	// A process appears and the pod's cgroup spends another second, and this window has
	// already spent its 200 ms: the walk pass that would read either of them waits.
	f.appear(fakeProcess{pid: 777, parent: 1, args: []string{"gopls"}, own: 50, start: 900, pages: 1000})
	f.writeV1Totals(1<<28, 2_000_000_000)
	c.pass(2)

	e = f.scrape()
	wantValue(t, e, 1, walkDeferred)
	wantValue(t, e, 9100*page, memoryResidentBytes, shared...)
	wantValue(t, e, 1, podCPUSecondsTotal, podProject, podIssue)
	if want := []string{"/pod/scratch"}; !slices.Equal(*volumes, want) {
		t.Errorf("the deferred pass read the volumes at %v, want the one read of the pass before it", *volumes)
	}

	// The pass after it reads all three, and the deferral is counted once and not again.
	c.pass(4)
	e = f.scrape()
	wantValue(t, e, 1, walkDeferred)
	wantValue(t, e, 10100*page, memoryResidentBytes, shared...)
	wantValue(t, e, 2, podCPUSecondsTotal, podProject, podIssue)
	if want := []string{"/pod/scratch", "/pod/scratch"}; !slices.Equal(*volumes, want) {
		t.Errorf("the pass after the deferred one read the volumes at %v, want %v", *volumes, want)
	}
}

// A deferral is never two walk passes in a row. Where every window is over the allowance, which
// is a pod saturated for minutes, the walk runs on every other walk pass: a process's stat is
// read every four seconds rather than every two, and a sampler holding its cap reports a pod
// half as often rather than stopping.
func TestAWalkPassIsNeverDeferredTwiceInARow(t *testing.T) {
	f := newPodFixture(t)
	volumes := saturated(t, f)

	c := f.collector()
	var ran []int64
	for tick := int64(0); tick < 16; tick++ {
		read := len(*volumes)
		c.pass(tick)
		if len(*volumes) > read {
			ran = append(ran, tick)
		}
	}

	if want := []int64{0, 4, 8, 12}; !slices.Equal(ran, want) {
		t.Errorf("the walk ran at ticks %v, want every other walk tick, %v", ran, want)
	}
	wantValue(t, f.scrape(), 4, walkDeferred)
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
	pod, walk, smaps := podInterval, walkInterval, smapsInterval
	podInterval, walkInterval, smapsInterval = tick, 2*tick, 10*tick
	t.Cleanup(func() { podInterval, walkInterval, smapsInterval = pod, walk, smaps })
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
