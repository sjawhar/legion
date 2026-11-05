package procread

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Layout is the shape of the cgroup filesystem under a root, which decides which files hold a
// pod's totals and whether a per-group reading is there to be had at all.
type Layout int

const (
	// LayoutNone is a root with no cgroup filesystem this package can read. Its totals are zero.
	LayoutNone Layout = iota
	// LayoutV1 is the legacy hierarchy, one directory per controller at the mount root
	// (memory/, cpuacct/), which is what this pod's gVisor kernel presents.
	LayoutV1
	// LayoutV2 is the unified hierarchy, every controller's files in one tree, which a microVM
	// and a node's own kubepods tree present.
	LayoutV2
)

// String is the layout's name as a label value and a log line write it.
func (l Layout) String() string {
	switch l {
	case LayoutNone:
		return "none"
	case LayoutV1:
		return "v1"
	case LayoutV2:
		return "v2"
	}
	return fmt.Sprintf("Layout(%d)", int(l))
}

// DetectLayout is the cgroup shape under root, read from the mount itself: LayoutV2 where
// <root>/sys/fs/cgroup/cgroup.controllers exists, since only the unified hierarchy writes that
// file; else LayoutV1 where <root>/sys/fs/cgroup/memory/memory.usage_in_bytes exists, the legacy
// hierarchy's own memory controller; else LayoutNone, which is no failure — a root may simply hold
// no cgroup filesystem, and the sampler then reports what /proc alone tells it.
//
// It never reads /proc/self/cgroup. That file names the group this process is in as its own
// kernel sees it, and in this pod's gVisor it names a directory that exists nowhere under the
// mount (INFRA-1208, measured 2026-11-05), so a layout read from it would send every later read
// at a path that cannot be opened.
func DetectLayout(root string) Layout {
	mount := filepath.Join(root, "sys", "fs", "cgroup")
	if exists(filepath.Join(mount, "cgroup.controllers")) {
		return LayoutV2
	}
	if exists(filepath.Join(mount, "memory", "memory.usage_in_bytes")) {
		return LayoutV1
	}
	return LayoutNone
}

// exists is whether path can be stat'd, which for a cgroup file is whether the controller that
// writes it is mounted here.
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Totals is the whole pod's own accounting, read at the cgroup mount root: the memory it holds
// and the CPU time it has spent since it started, both converted out of the unit the file reports
// them in.
type Totals struct {
	MemoryBytes int64
	CPUSeconds  float64
}

// PodTotals is the pod's totals at the mount root of root, read the way layout's hierarchy keeps
// them: under v1, memory/memory.usage_in_bytes and cpuacct/cpuacct.usage, which counts
// nanoseconds; under v2, memory.current and cpu.stat's usage_usec, which counts microseconds.
// LayoutNone is a zero Totals and no error, since a root with no cgroup filesystem has nothing to
// disagree with and nothing to report.
//
// A file the mount does not hold leaves its own field zero rather than failing the pass: a kernel
// may mount the memory controller and not cpuacct, and half a reading beats none. A file that is
// there and holds something that is no number is a refusal naming it, because that is a kernel
// this package has read wrongly, not a kernel that counts less.
func PodTotals(root string, layout Layout) (Totals, error) {
	mount := filepath.Join(root, "sys", "fs", "cgroup")
	var totals Totals
	switch layout {
	case LayoutV1:
		memory, err := readInt(filepath.Join(mount, "memory", "memory.usage_in_bytes"))
		if err != nil {
			return Totals{}, err
		}
		nanoseconds, err := readInt(filepath.Join(mount, "cpuacct", "cpuacct.usage"))
		if err != nil {
			return Totals{}, err
		}
		totals = Totals{MemoryBytes: memory, CPUSeconds: float64(nanoseconds) / 1e9}
	case LayoutV2:
		memory, err := readInt(filepath.Join(mount, "memory.current"))
		if err != nil {
			return Totals{}, err
		}
		cpu, err := readKeys(filepath.Join(mount, "cpu.stat"), "usage_usec")
		if err != nil {
			return Totals{}, err
		}
		totals = Totals{MemoryBytes: memory, CPUSeconds: float64(cpu["usage_usec"]) / 1e6}
	}
	return totals, nil
}

// Group is one cgroup v2 directory's own accounting: the CPU time its processes have spent,
// the memory it holds now and the most it has ever held, that memory's anonymous and shared-memory
// parts, and how many processes the OOM killer has taken from it. A file the directory does not
// hold leaves its field zero — memory.peak is a recent kernel's, memory.events is the memory
// controller's — so a zero here is "not reported", never "reported as none".
type Group struct {
	CPUSeconds                   float64
	MemoryBytes, MemoryPeakBytes int64
	AnonBytes, ShmemBytes        int64
	OOMKills                     int64
}

// ReadGroup is one cgroup v2 directory's Group, dir named absolutely: cpu.stat's usage_usec,
// memory.current, memory.peak, memory.stat's anon and shmem, and memory.events' oom_kill. A file
// that does not exist, and a key a file does not carry, leave their field zero and are no error:
// a group can be removed between two passes, and a kernel writes the files of the controllers it
// has. A file that exists and cannot be parsed is an error naming the file.
func ReadGroup(dir string) (Group, error) {
	var group Group
	cpu, err := readKeys(filepath.Join(dir, "cpu.stat"), "usage_usec")
	if err != nil {
		return Group{}, err
	}
	group.CPUSeconds = float64(cpu["usage_usec"]) / 1e6
	if group.MemoryBytes, err = readInt(filepath.Join(dir, "memory.current")); err != nil {
		return Group{}, err
	}
	if group.MemoryPeakBytes, err = readInt(filepath.Join(dir, "memory.peak")); err != nil {
		return Group{}, err
	}
	memory, err := readKeys(filepath.Join(dir, "memory.stat"), "anon", "shmem")
	if err != nil {
		return Group{}, err
	}
	group.AnonBytes, group.ShmemBytes = memory["anon"], memory["shmem"]
	events, err := readKeys(filepath.Join(dir, "memory.events"), "oom_kill")
	if err != nil {
		return Group{}, err
	}
	group.OOMKills = events["oom_kill"]
	return group, nil
}

// ChildGroups are the names — not the paths — of dir's child groups, which is every entry of dir
// that is a directory, since a cgroup directory's own files sit beside them. A dir that does not
// exist is an empty list and no error: a cgroup is removed the moment the last process in it
// ends, so a tree walked twice is a tree whose branches disappear under the walker.
func ChildGroups(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names, nil
}

// readInt is the one number path holds, and zero for a path that does not exist: a cgroup file
// belongs to a controller, and a controller this kernel did not mount wrote nothing to read. A
// file that is there and holds something else is a refusal naming it and quoting what it held.
func readInt(path string) (int64, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	text := strings.TrimSpace(string(raw))
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is no number", path, text)
	}
	return value, nil
}

// readKeys are the values keys name in path, a cgroup file of "<key> <value>" lines (cpu.stat,
// memory.stat, memory.events). A file that does not exist, and a key the file does not carry,
// answer zero, for the same reason readInt does; a key whose value is no number is a refusal
// naming the file and the key. Only the asked-for keys are parsed: memory.stat carries forty
// lines, and a kernel's new one is no business of a reader that wants two of them.
func readKeys(path string, keys ...string) (map[string]int64, error) {
	values := make(map[string]int64, len(keys))
	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return values, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	lines := bufio.NewScanner(file)
	for lines.Scan() {
		key, value, found := strings.Cut(strings.TrimSpace(lines.Text()), " ")
		if !found || !slices.Contains(keys, key) {
			continue
		}
		number, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s: %s is %q, no number", path, key, strings.TrimSpace(value))
		}
		values[key] = number
	}
	if err := lines.Err(); err != nil {
		return nil, err
	}
	return values, nil
}
