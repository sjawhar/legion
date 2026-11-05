package procread

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
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

// ErrNoLayout is the answer to a reading of the pod's own totals under a root with no cgroup
// filesystem this package can read (LayoutNone): there is no file to read them from, which is no
// failure of the kernel's and no zero either. A collector tells it from a read that failed by
// errors.Is and serves no pod total at all for that pass, because a pod whose cgroup files this
// kernel does not write has no total to serve rather than a total of none.
var ErrNoLayout = errors.New("procread: no cgroup filesystem under this root")

// cgroupMount is the cgroup filesystem under a root, which is the directory a pod's own totals
// and every group under it are read from. Every path of that filesystem this package builds
// starts here, so a reader of a fixture root and a reader of "/" name the same files.
func cgroupMount(root string) string { return filepath.Join(root, "sys", "fs", "cgroup") }

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
	mount := cgroupMount(root)
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

// Group is one cgroup v2 directory's own accounting: the CPU time its processes have spent, the
// memory it holds now, the most it has ever held and that memory's anonymous part. A file the
// directory does not hold leaves its field zero — memory.peak is a recent kernel's — so a zero
// here is "not reported", never "reported as none".
//
// What is here is what a caller reads. The cgroup v2 files hold more than this — the processes
// the OOM killer took (memory.events), the shared-memory part of the reading (memory.stat's
// shmem) — and a field no series carries is a file opened and parsed for every pod every second
// (INFRA-1208 round 1), so neither is read.
type Group struct {
	CPUSeconds                   float64
	MemoryBytes, MemoryPeakBytes int64
	AnonBytes                    int64
}

// ReadGroup is one cgroup v2 directory's Group, dir named absolutely: cpu.stat's usage_usec,
// memory.current, memory.peak and memory.stat's anon. A file that does not exist, and a key a
// file does not carry, leave their field zero and are no error: a group can be removed between
// two passes, and a kernel writes the files of the controllers it has. A file that exists and
// cannot be parsed is an error naming the file.
//
// A caller that wants one of these numbers alone reads it alone: node mode's pod totals are
// GroupCPUSeconds and GroupMemoryBytes, which are two files where this is four.
func ReadGroup(dir string) (Group, error) {
	var group Group
	var err error
	if group.CPUSeconds, err = GroupCPUSeconds(dir); err != nil {
		return Group{}, err
	}
	if group.MemoryBytes, err = GroupMemoryBytes(dir); err != nil {
		return Group{}, err
	}
	if group.MemoryPeakBytes, err = readInt(filepath.Join(dir, "memory.peak")); err != nil {
		return Group{}, err
	}
	if group.AnonBytes, err = readKey(filepath.Join(dir, "memory.stat"), "anon"); err != nil {
		return Group{}, err
	}
	return group, nil
}

// GroupCPUSeconds is one cgroup v2 directory's CPU alone: cpu.stat's usage_usec, which counts
// microseconds. It is the reading a sampler takes every second, apart from the memory beside it
// in the same directory: in a gVisor pod a read of a cgroup's CPU is 0.00 ms to 0.50 ms where a
// read of its memory is 17 ms to 20 ms, since that kernel answers the memory by accounting for it
// there and then (INFRA-1208, measured 2026-11-05). So a sampler reads the CPU every second and
// the memory at half that rate (internal/sampler/pod.go's rates).
func GroupCPUSeconds(dir string) (float64, error) {
	microseconds, err := readKey(filepath.Join(dir, "cpu.stat"), "usage_usec")
	if err != nil {
		return 0, err
	}
	return float64(microseconds) / 1e6, nil
}

// GroupMemoryBytes is one cgroup v2 directory's memory alone: memory.current, the memory it holds
// now.
func GroupMemoryBytes(dir string) (int64, error) {
	return readInt(filepath.Join(dir, "memory.current"))
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

// GroupProcesses are the process ids one cgroup holds: the lines of its cgroup.procs, which is
// the one file of the cgroup filesystem that says who is in a group rather than what the group
// has spent. A group that has been removed holds nobody, which is no failure — a command ends
// and its cgroup goes with it — and a line that is no process id is a refusal naming the file.
func GroupProcesses(dir string) ([]int, error) {
	path := filepath.Join(dir, "cgroup.procs")
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	fields := bytes.Fields(raw)
	pids := make([]int, 0, len(fields))
	for _, field := range fields {
		pid, err := strconv.Atoi(string(field))
		if err != nil {
			return nil, fmt.Errorf("%s: %q is no process id", path, field)
		}
		pids = append(pids, pid)
	}
	return pids, nil
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
	return parseInt(raw, path)
}

// parseInt is the number a cgroup file of one number holds (memory.current, memory.peak,
// cpuacct.usage), named for the refusal. It is this package's one reader of that shape: the file
// read by path comes through here and so does the one read through a descriptor a Reader keeps
// (Reader.number), so the two cannot answer differently.
func parseInt(raw []byte, path string) (int64, error) {
	text := string(bytes.TrimSpace(raw))
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is no number", path, text)
	}
	return value, nil
}

// readKey is the value key names in path, a cgroup file of "<key> <value>" lines (cpu.stat,
// memory.stat). A file that does not exist, and a key the file does not carry, answer zero, for
// the same reason readInt does.
func readKey(path, key string) (int64, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return parseKey(raw, key, path)
}

// parseKey is the value one key names in a cgroup file of "<key> <value>" lines, zero for a key
// the file does not carry, and a refusal naming the file and the key for a value that is no
// number. It is this package's one reader of that shape, as parseInt is of the other, and it is
// what both the read by path and Reader.key answer from.
//
// Only the asked-for key is parsed and no string is made of any other line: memory.stat carries
// forty lines, read for every pod of a node every second, and a kernel's new line is no business
// of a reader that wants one of them.
func parseKey(raw []byte, key, path string) (int64, error) {
	for line := range bytes.Lines(raw) {
		name, value, found := bytes.Cut(bytes.TrimSpace(line), []byte(" "))
		if !found || string(name) != key {
			continue
		}
		number, err := strconv.ParseInt(string(bytes.TrimSpace(value)), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s: %s is %q, no number", path, key, bytes.TrimSpace(value))
		}
		return number, nil
	}
	return 0, nil
}
