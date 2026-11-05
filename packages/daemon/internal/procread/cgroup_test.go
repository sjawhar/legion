package procread

import (
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

// sameTotals reports the two as equal, the seconds within a float's own slack: the value comes
// out of a division the test cannot write as a literal and get back bit for bit.
func sameTotals(got, want Totals) bool {
	return got.MemoryBytes == want.MemoryBytes && math.Abs(got.CPUSeconds-want.CPUSeconds) < 1e-9
}

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

// The pod's own totals come from the files its hierarchy keeps them in, each out of the unit that
// file counts in: v1's cpuacct.usage is nanoseconds, v2's cpu.stat usage_usec is microseconds. A
// root with no cgroup filesystem is a zero Totals and no error, since there is nothing there to
// read and nothing wrong with that.
func TestPodTotalsReadTheHierarchysOwnFiles(t *testing.T) {
	for _, test := range []struct {
		root   string
		layout Layout
		want   Totals
	}{
		{profile(gvisor), LayoutV1, Totals{MemoryBytes: 798289920, CPUSeconds: 123.456789012}},
		{profile(microvm), LayoutV2, Totals{MemoryBytes: 1610612736, CPUSeconds: 91.234567}},
		{profile(runcnode), LayoutV2, Totals{MemoryBytes: 8589934592, CPUSeconds: 987.654321}},
		{profile(gvisor), LayoutNone, Totals{}},
		{t.TempDir(), LayoutV2, Totals{}},
	} {
		totals, err := PodTotals(test.root, test.layout)
		if err != nil {
			t.Fatalf("PodTotals(%s, %s) = %v", test.root, test.layout, err)
		}
		if !sameTotals(totals, test.want) {
			t.Errorf("PodTotals(%s, %s) = %+v, want %+v", test.root, test.layout, totals, test.want)
		}
	}
}

// A file that is there and holds something that is no number is this package reading a kernel
// wrongly, not a kernel counting less: the refusal names the file, so whoever reads the log knows
// which one to look at.
func TestPodTotalsRefusesAFileItCannotParse(t *testing.T) {
	root := t.TempDir()
	usage := filepath.Join(root, "sys", "fs", "cgroup", "memory", "memory.usage_in_bytes")
	writeFile(t, usage, "lots\n")
	totals, err := PodTotals(root, LayoutV1)
	if err == nil {
		t.Fatalf("PodTotals(a root whose memory.usage_in_bytes is \"lots\") = %+v, want an error", totals)
	}
	if !strings.Contains(err.Error(), usage) {
		t.Errorf("PodTotals() error = %v, want it to name %s", err, usage)
	}
}

// One group's reading is the five files cgroup v2 keeps its accounting in. memory.peak is a
// recent kernel's file: the burstable group has none, and its peak is zero rather than a refusal,
// because a kernel that does not report a number has reported nothing, not a failure.
func TestReadGroupIsOneDirectorysOwnAccounting(t *testing.T) {
	for _, test := range []struct {
		dir  string
		want Group
	}{
		{
			filepath.Join(mount(microvm), "legion.slice"),
			Group{
				CPUSeconds: 12.345678, MemoryBytes: 268435456, MemoryPeakBytes: 402653184,
				AnonBytes: 201326592, ShmemBytes: 16777216, OOMKills: 2,
			},
		},
		{
			filepath.Join(mount(runcnode), "kubepods.slice", "kubepods-besteffort.slice"),
			Group{
				CPUSeconds: 234.567890, MemoryBytes: 2147483648, MemoryPeakBytes: 3221225472,
				AnonBytes: 1610612736, ShmemBytes: 268435456, OOMKills: 1,
			},
		},
		{
			filepath.Join(mount(runcnode), "kubepods.slice", "kubepods-burstable.slice"),
			Group{
				CPUSeconds: 45.678901, MemoryBytes: 1073741824,
				AnonBytes: 805306368, ShmemBytes: 67108864,
			},
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
