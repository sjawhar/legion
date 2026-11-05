package procread

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// mount is a fixture profile's cgroup mount root, where a pod's own totals sit.
func mount(name string) string { return filepath.Join(profile(name), "sys", "fs", "cgroup") }

// writeFile is one cgroup file in a tree a test builds for itself, with its parents.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("make %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// sameSeconds reports the two as equal within a float's own slack: the value comes out of a
// division the test cannot write as a literal and get back bit for bit.
func sameSeconds(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

// sameGroup reports the two as equal, the seconds within the same slack.
func sameGroup(got, want Group) bool {
	seconds := math.Abs(got.CPUSeconds-want.CPUSeconds) < 1e-9
	got.CPUSeconds, want.CPUSeconds = 0, 0
	return seconds && got == want
}

// The layout is read from the mount itself and from nothing else: cgroup.controllers is the
// unified hierarchy's own file, memory/memory.usage_in_bytes the legacy one's, and a root with
// neither is LayoutNone rather than a failure. The gvisor profile also carries a
// /proc/1/cgroup naming a group that exists nowhere under its mount — exactly what this pod's
// kernel writes — so a detection that read that file instead would answer v2 here and then open
// nothing.
func TestDetectLayoutReadsTheMountAndNotProcSelfCgroup(t *testing.T) {
	for _, test := range []struct {
		root string
		want Layout
	}{
		{profile(gvisor), LayoutV1},
		{profile(microvm), LayoutV2},
		{profile(runcnode), LayoutV2},
		{t.TempDir(), LayoutNone},
	} {
		if layout := DetectLayout(test.root); layout != test.want {
			t.Errorf("DetectLayout(%s) = %s, want %s", test.root, layout, test.want)
		}
	}
}

// A layout names itself in a log line and a label value.
func TestLayoutStringNamesTheHierarchy(t *testing.T) {
	for _, test := range []struct {
		layout Layout
		want   string
	}{
		{LayoutNone, "none"},
		{LayoutV1, "v1"},
		{LayoutV2, "v2"},
		{Layout(7), "Layout(7)"},
	} {
		if name := test.layout.String(); name != test.want {
			t.Errorf("Layout(%d).String() = %q, want %q", int(test.layout), name, test.want)
		}
	}
}

// The pod's own totals come from the files its hierarchy keeps them in, each read apart from the
// other and each out of the unit that file counts in: v1's cpuacct.usage is nanoseconds, v2's
// cpu.stat usage_usec is microseconds. A mount whose files are not there is zero and no error,
// since a kernel may mount one controller and not another and half a reading beats none.
//
// There is one reader of them, the Reader of reader.go, because one open is the whole cost of
// these reads under gVisor and a second reader would be a second parser to keep true
// (INFRA-1208 round 1).
func TestThePodsTotalsAreReadFromTheHierarchysOwnFiles(t *testing.T) {
	for _, test := range []struct {
		root    string
		layout  Layout
		memory  int64
		seconds float64
	}{
		{profile(gvisor), LayoutV1, 798289920, 123.456789012},
		{profile(microvm), LayoutV2, 1610612736, 91.234567},
		{profile(runcnode), LayoutV2, 8589934592, 987.654321},
		{t.TempDir(), LayoutV2, 0, 0},
	} {
		reader := newReaderOf(t, test.root, test.layout)
		seconds, err := reader.PodCPUSeconds()
		if err != nil {
			t.Fatalf("Reader.PodCPUSeconds(%s, %s) = %v", test.root, test.layout, err)
		}
		if !sameSeconds(seconds, test.seconds) {
			t.Errorf("Reader.PodCPUSeconds(%s, %s) = %g, want %g",
				test.root, test.layout, seconds, test.seconds)
		}
		memory, err := reader.PodMemoryBytes()
		if err != nil {
			t.Fatalf("Reader.PodMemoryBytes(%s, %s) = %v", test.root, test.layout, err)
		}
		if memory != test.memory {
			t.Errorf("Reader.PodMemoryBytes(%s, %s) = %d, want %d",
				test.root, test.layout, memory, test.memory)
		}
	}
}

// A root with no cgroup filesystem has no pod totals to read, and says so: ErrNoLayout, which is
// neither a reading nor a read that failed. A zero here would be served as a pod that holds no
// memory and has spent no CPU, where the collector must serve no pod total at all
// (internal/sampler/pod.go's noTotals, INFRA-1208 round 1).
func TestThePodsTotalsUnderNoCgroupFilesystemAreNoLayout(t *testing.T) {
	reader := newReaderOf(t, profile(gvisor), LayoutNone)
	seconds, err := reader.PodCPUSeconds()
	if !errors.Is(err, ErrNoLayout) {
		t.Errorf("Reader.PodCPUSeconds(LayoutNone) = %g, %v; want ErrNoLayout", seconds, err)
	}
	if seconds != 0 {
		t.Errorf("Reader.PodCPUSeconds(LayoutNone) = %g beside its error, want no reading", seconds)
	}
	memory, err := reader.PodMemoryBytes()
	if !errors.Is(err, ErrNoLayout) {
		t.Errorf("Reader.PodMemoryBytes(LayoutNone) = %d, %v; want ErrNoLayout", memory, err)
	}
	if memory != 0 {
		t.Errorf("Reader.PodMemoryBytes(LayoutNone) = %d beside its error, want no reading", memory)
	}
}

// A file that is there and holds something that is no number is this package reading a kernel
// wrongly, not a kernel counting less: the refusal names the file, so whoever reads the log knows
// which one to look at.
func TestThePodsTotalsRefuseAFileTheyCannotParse(t *testing.T) {
	root := t.TempDir()
	usage := filepath.Join(root, "sys", "fs", "cgroup", "memory", "memory.usage_in_bytes")
	writeFile(t, usage, "lots\n")
	cpuacct := filepath.Join(root, "sys", "fs", "cgroup", "cpuacct", "cpuacct.usage")
	writeFile(t, cpuacct, "plenty\n")
	reader := newReaderOf(t, root, LayoutV1)
	memory, err := reader.PodMemoryBytes()
	if err == nil {
		t.Fatalf("Reader.PodMemoryBytes(a memory.usage_in_bytes of \"lots\") = %d, want an error", memory)
	}
	if !strings.Contains(err.Error(), usage) {
		t.Errorf("Reader.PodMemoryBytes() error = %v, want it to name %s", err, usage)
	}
	seconds, err := reader.PodCPUSeconds()
	if err == nil {
		t.Fatalf("Reader.PodCPUSeconds(a cpuacct.usage of \"plenty\") = %g, want an error", seconds)
	}
	if !strings.Contains(err.Error(), cpuacct) {
		t.Errorf("Reader.PodCPUSeconds() error = %v, want it to name %s", err, cpuacct)
	}
}

// One group's reading is the four numbers of cgroup v2 a caller of this reads: its CPU, the
// memory it holds, the most it has held and that memory's anonymous part. memory.peak is a
// recent kernel's file: the burstable group has none, and its peak is zero rather than a refusal,
// because a kernel that does not report a number has reported nothing, not a failure.
//
// The groups below hold a memory.events and a shmem line their reading leaves alone: a field no
// series carries is a file opened for every pod every second (INFRA-1208 round 1), so the fixture
// keeps them and the Group has no room for them.
func TestReadGroupIsOneDirectorysOwnAccounting(t *testing.T) {
	for _, test := range []struct {
		dir  string
		want Group
	}{
		{
			filepath.Join(mount(microvm), "legion.slice"),
			Group{
				CPUSeconds: 12.345678, MemoryBytes: 268435456, MemoryPeakBytes: 402653184,
				AnonBytes: 201326592,
			},
		},
		{
			filepath.Join(mount(runcnode), "kubepods.slice", "kubepods-besteffort.slice"),
			Group{
				CPUSeconds: 234.567890, MemoryBytes: 2147483648, MemoryPeakBytes: 3221225472,
				AnonBytes: 1610612736,
			},
		},
		{
			filepath.Join(mount(runcnode), "kubepods.slice", "kubepods-burstable.slice"),
			Group{CPUSeconds: 45.678901, MemoryBytes: 1073741824, AnonBytes: 805306368},
		},
	} {
		group, err := ReadGroup(test.dir)
		if err != nil {
			t.Fatalf("ReadGroup(%s) = %v", test.dir, err)
		}
		if !sameGroup(group, test.want) {
			t.Errorf("ReadGroup(%s) = %+v, want %+v", test.dir, group, test.want)
		}
	}
}

// A group removed between the pass that listed it and the pass that reads it is a zero Group and
// no error: the processes it held are gone, which is what a sampler reports, and a pass that
// failed over it would lose every other group of that tick too.
func TestReadGroupOfAGroupThatIsGoneIsZero(t *testing.T) {
	group, err := ReadGroup(filepath.Join(mount(microvm), "gone.slice"))
	if err != nil {
		t.Fatalf("ReadGroup(a group that is gone) = %v", err)
	}
	if group != (Group{}) {
		t.Errorf("ReadGroup(a group that is gone) = %+v, want the zero Group", group)
	}
}

// A file that exists and cannot be parsed is an error naming that file, whether the number stands
// alone (memory.current) or under a key (cpu.stat's usage_usec).
func TestReadGroupRefusesAFileItCannotParse(t *testing.T) {
	for _, test := range []struct {
		name    string
		file    string
		content string
	}{
		{"a memory.current that is no number", "memory.current", "nearly all of it\n"},
		{"a cpu.stat whose usage_usec is no number", "cpu.stat", "usage_usec plenty\nuser_usec 1\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, test.file)
			writeFile(t, path, test.content)
			group, err := ReadGroup(dir)
			if err == nil {
				t.Fatalf("ReadGroup(%s) = %+v, want an error", test.file, group)
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("ReadGroup() error = %v, want it to name %s", err, path)
			}
		})
	}
}

// A group's children are the directory names under it, which is every entry that is a directory:
// a cgroup keeps its own files beside its children, and cgroup.procs is not a group.
func TestChildGroupsAreTheDirectoryNamesUnderADirectory(t *testing.T) {
	kubepods := filepath.Join(mount(runcnode), "kubepods.slice")
	names, err := ChildGroups(kubepods)
	if err != nil {
		t.Fatalf("ChildGroups(%s) = %v", kubepods, err)
	}
	want := []string{"kubepods-besteffort.slice", "kubepods-burstable.slice"}
	if !slices.Equal(names, want) {
		t.Errorf("ChildGroups(%s) = %q, want %q", kubepods, names, want)
	}
}

// A cgroup is removed the moment the last process in it ends, so a tree is walked while its
// branches disappear: both a child that was there on the previous pass and the parent the walk
// started from answer an empty list and no error rather than ending the tick.
func TestChildGroupsSurvivesAGroupRemovedBetweenTwoPasses(t *testing.T) {
	root := filepath.Join(t.TempDir(), "legion.slice")
	for _, child := range []string{"planner", "implementer"} {
		writeFile(t, filepath.Join(root, child, "memory.current"), "1024\n")
	}
	names, err := ChildGroups(root)
	if err != nil {
		t.Fatalf("ChildGroups(the first pass) = %v", err)
	}
	if want := []string{"implementer", "planner"}; !slices.Equal(names, want) {
		t.Fatalf("ChildGroups(the first pass) = %q, want %q", names, want)
	}
	if err := os.RemoveAll(filepath.Join(root, "planner")); err != nil {
		t.Fatalf("remove the planner's group: %v", err)
	}
	names, err = ChildGroups(root)
	if err != nil {
		t.Fatalf("ChildGroups(after a child was removed) = %v", err)
	}
	if want := []string{"implementer"}; !slices.Equal(names, want) {
		t.Fatalf("ChildGroups(after a child was removed) = %q, want %q", names, want)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("remove the whole group: %v", err)
	}
	names, err = ChildGroups(root)
	if err != nil {
		t.Fatalf("ChildGroups(after the group itself was removed) = %v", err)
	}
	if len(names) != 0 {
		t.Errorf("ChildGroups(after the group itself was removed) = %q, want none", names)
	}
}

// GroupProcesses is who is in a cgroup, which is the one thing a group's own files say that is
// not a number: cgroup.procs, one process id a line. A sampler reads it to learn what the work in
// a command's cgroup is (internal/sampler/pod.go's groupKind), and it is read here because every
// read of the cgroup filesystem belongs to this package (INFRA-1208 round 1).
func TestGroupProcessesAreTheProcessIdsOfACgroup(t *testing.T) {
	kubepods := filepath.Join(mount(runcnode), "kubepods.slice")
	pids, err := GroupProcesses(kubepods)
	if err != nil {
		t.Fatalf("GroupProcesses(%s) = %v", kubepods, err)
	}
	if want := []int{4011, 4012, 4087}; !slices.Equal(pids, want) {
		t.Errorf("GroupProcesses(%s) = %v, want %v", kubepods, pids, want)
	}

	// A command ends and its cgroup goes with it: a group that is not there holds nobody,
	// which is no failure of the pass that was about to read it.
	gone := filepath.Join(t.TempDir(), "implementer-7")
	pids, err = GroupProcesses(gone)
	if err != nil {
		t.Fatalf("GroupProcesses(a group that is gone) = %v", err)
	}
	if len(pids) != 0 {
		t.Errorf("GroupProcesses(a group that is gone) = %v, want nobody", pids)
	}

	// An empty cgroup.procs is a group every process of which has ended, which the caller
	// tells apart from a group it could not read.
	empty := t.TempDir()
	writeFile(t, filepath.Join(empty, "cgroup.procs"), "")
	if pids, err := GroupProcesses(empty); err != nil || len(pids) != 0 {
		t.Errorf("GroupProcesses(an empty cgroup.procs) = %v, %v; want nobody", pids, err)
	}

	// A line that is no process id is a refusal naming the file: that is this package reading
	// a kernel wrongly, and a sampler that passed over it would charge a command's work to
	// whatever the last pass thought was in its cgroup.
	odd := t.TempDir()
	path := filepath.Join(odd, "cgroup.procs")
	writeFile(t, path, "4011\nnobody\n")
	pids, err = GroupProcesses(odd)
	if err == nil {
		t.Fatalf("GroupProcesses(a cgroup.procs holding \"nobody\") = %v, want a refusal", pids)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("the refusal is %q, want it to name %s", err, path)
	}
}
