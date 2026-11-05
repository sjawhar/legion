// Package procread is what the kernel already counts, read: the processes under /proc and the
// accounting files of the cgroup filesystem, as plain functions over a root directory. Every
// reader is told the root to read under — the sampler passes "/", a test passes a directory of its
// own holding proc/… and sys/fs/cgroup/… — so nothing here reaches for a path of its own or
// imports another Legion package. A second reader of the same files (INFRA-1115) takes these
// functions as they stand.
//
// The functions hold no state between calls. Beside them is one type that does: a Reader
// (reader.go) keeps the descriptors of the files a sampler reads again and again and answers
// the same readings through them, which under gVisor is what the reading costs (INFRA-1208).
//
// What a number means is the caller's. This package answers resident pages, not bytes, because
// only the caller knows the page size of the kernel it read (os.Getpagesize), and ticks, not
// seconds, because the same caller holds ClockTicks. It converts only what the kernel itself
// reports in a unit of its own: a kB line of /proc becomes bytes, and a cgroup's microseconds or
// nanoseconds become seconds, since those units are the file's and not the reading.
//
// Every reader answers a typed value and an error, and none of them panics. An absence whose shape
// is known is a zero rather than a failure — a cgroup file a controller never mounted did not
// write, a cgroup directory the previous pass saw and this one does not — and anything else is the
// error the filesystem gave, naming the file it gave it for.
//
// An absence a caller must tell apart from a reading is neither: it is one of this package's own
// errors, which a caller reads with errors.Is and answers for. There are two — ErrNoLayout, a
// root with no cgroup filesystem to read a pod's totals from, and ErrCgroupV1, a pod cgroup of
// the legacy hierarchy — and both say "there is no reading here", never "the reading is none".
package procread

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// ClockTicks is /proc's fixed USER_HZ: the kernel reports process times in hundredths of a second.
const ClockTicks = 100

// Stat is one process's /proc/<pid>/stat, in the kernel's own units: ticks of ClockTicks for the
// four time fields and for StartTicks (the boot-relative moment the process began), and pages for
// ResidentPages. Comm is field 2 as the kernel wrote it, without its parentheses — the first
// sixteen bytes of the executable's name, which may itself hold spaces and parentheses, which is
// why ParseStat counts the fields it wants after the last `)` and never by splitting the line.
type Stat struct {
	PID, Parent                                              int
	Comm                                                     string
	UserTicks, SystemTicks, ChildUserTicks, ChildSystemTicks uint64
	ResidentPages                                            uint64
	StartTicks                                               uint64
}

// PIDs are the processes <root>/proc holds, ascending: every entry of that directory that is a
// directory whose name is a number. A thread is not among them, since the kernel keeps those under
// a process's own task/ directory, and neither is meminfo or any other file /proc carries beside
// the processes. The order is numeric rather than the directory's own, which sorts "10" before
// "2", so two passes over the same /proc report the same processes in the same order.
func PIDs(root string) ([]int, error) {
	entries, err := os.ReadDir(filepath.Join(root, "proc"))
	if err != nil {
		return nil, err
	}
	pids := make([]int, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		pids = append(pids, pid)
	}
	slices.Sort(pids)
	return pids, nil
}

// ReadStat is <root>/proc/<pid>/stat, parsed. A process that ended between a pass's PIDs and this
// read is the filesystem's own "no such file or directory", which a caller tells by fs.ErrNotExist
// and skips; a line this cannot parse is a refusal naming the file.
func ReadStat(root string, pid int) (Stat, error) {
	path := filepath.Join(root, "proc", strconv.Itoa(pid), "stat")
	line, err := os.ReadFile(path)
	if err != nil {
		return Stat{}, err
	}
	stat, err := ParseStat(string(line))
	if err != nil {
		return Stat{}, fmt.Errorf("%s: %w", path, err)
	}
	return stat, nil
}

// ParseStat is one /proc/<pid>/stat line: the pid before the comm, the comm between the first `(`
// and the last `)`, and parent (field 4), utime (14), stime (15), cutime (16), cstime (17),
// starttime (22) and rss in pages (24) counted after that last `)`. Field 2 is the executable's
// name in parentheses and may hold spaces and parentheses of its own — a process named
// "(fun (ny) proc)" is a line strings.Fields reads as ten fields too many — so the count starts at
// field 3 over the remainder of the line, as internal/runtime/tmux's own stat readers do.
func ParseStat(line string) (Stat, error) {
	open, end := strings.IndexByte(line, '('), strings.LastIndexByte(line, ')')
	if open < 0 || end < open {
		return Stat{}, fmt.Errorf("malformed /proc/<pid>/stat line (no comm): %s", strings.TrimSpace(line))
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line[:open]))
	if err != nil {
		return Stat{}, fmt.Errorf("malformed /proc/<pid>/stat line (no pid): %s", strings.TrimSpace(line))
	}
	// Every index below is a field's own number less three: fields[0] is field 3, the state.
	const (
		parent    = 4 - 3
		utime     = 14 - 3
		stime     = 15 - 3
		cutime    = 16 - 3
		cstime    = 17 - 3
		starttime = 22 - 3
		rss       = 24 - 3
	)
	fields := strings.Fields(line[end+1:])
	if len(fields) <= rss {
		return Stat{}, fmt.Errorf("malformed /proc/<pid>/stat line (%d fields after the comm, want %d): %s",
			len(fields), rss+1, strings.TrimSpace(line))
	}
	stat := Stat{PID: pid, Comm: line[open+1 : end]}
	parentPID, err := strconv.Atoi(fields[parent])
	if err != nil {
		return Stat{}, fmt.Errorf("malformed /proc/<pid>/stat line (no parent pid): %s", strings.TrimSpace(line))
	}
	stat.Parent = parentPID
	for _, field := range []struct {
		into  *uint64
		index int
		name  string
	}{
		{&stat.UserTicks, utime, "utime"},
		{&stat.SystemTicks, stime, "stime"},
		{&stat.ChildUserTicks, cutime, "cutime"},
		{&stat.ChildSystemTicks, cstime, "cstime"},
		{&stat.StartTicks, starttime, "starttime"},
		{&stat.ResidentPages, rss, "rss"},
	} {
		value, err := strconv.ParseUint(fields[field.index], 10, 64)
		if err != nil {
			return Stat{}, fmt.Errorf("malformed /proc/<pid>/stat line (no %s field): %s",
				field.name, strings.TrimSpace(line))
		}
		*field.into = value
	}
	return stat, nil
}

// ReadCmdline is <root>/proc/<pid>/cmdline as the arguments the kernel holds: the file is the
// argument vector with a NUL after each argument, so the split's trailing empty string is that
// last NUL and not an argument of its own. A kernel thread's cmdline is empty, and the answer is
// an empty list and no error: the process is there, and it has no argument vector to read.
func ReadCmdline(root string, pid int) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(root, "proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	args := strings.Split(string(raw), "\x00")
	if args[len(args)-1] == "" {
		args = args[:len(args)-1]
	}
	return args, nil
}

// ReadAnonymous is the bytes of anonymous memory <root>/proc/<pid>/smaps accounts to the process:
// the sum of the `Anonymous:` line of every mapping, which the file reports in kB. It is what a
// process holds that no file backs, the number a pod is killed over, and it is summed from the
// per-mapping file because this pod's gVisor kernel writes neither /proc/<pid>/smaps_rollup nor a
// RssAnon line in /proc/<pid>/status (INFRA-1208, measured 2026-11-05).
//
// Every line goes through addAnonymous, which is this package's one reader of a mapping, so what
// this answers and what a Reader's kept descriptor answers (Reader.Anonymous) cannot differ: a
// bare `Anonymous:` line ends the sum with what was read, and a line holding something that is no
// number is a refusal naming the file.
func ReadAnonymous(root string, pid int) (int64, error) {
	path := filepath.Join(root, "proc", strconv.Itoa(pid), "smaps")
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	// Every line is handed over as the scanner's own bytes and never as a string of its own:
	// the smaps of a large process runs to thousands of lines, this is the costliest read the
	// sampler makes, and a string for each line would be an allocation for each line.
	var total int64
	lines := bufio.NewScanner(file)
	for lines.Scan() {
		switch err := addAnonymous(&total, lines.Bytes(), path); {
		case err == nil:
		case errors.Is(err, errTruncated):
			return total, nil
		default:
			return 0, err
		}
	}
	if err := lines.Err(); err != nil {
		return 0, err
	}
	return total, nil
}

// addAnonymous adds what one smaps line accounts to the process, and nothing for a line that is
// no Anonymous line. It is the whole of this package's reading of that file, called from the read
// by path above and from the read through a descriptor a Reader keeps (Reader.anonymousOf).
//
// A line that is an Anonymous line and holds no size at all is the end of a read the kernel
// truncated — a process unmaps and exits while its smaps is being written, and gVisor was
// measured writing a bare "Anonymous:" for such a mapping (INFRA-1208, 2026-11-05) — so it ends
// the sum with what the pass had read rather than refusing: that process's reading is a little
// low for one pass, where a refusal would raise legion_sampler_read_errors_total for a kernel
// that did nothing wrong. A line holding something that is no number is still a refusal naming
// the file.
func addAnonymous(total *int64, line []byte, path string) error {
	rest, found := bytes.CutPrefix(line, []byte("Anonymous:"))
	if !found {
		return nil
	}
	if len(bytes.TrimSpace(rest)) == 0 {
		return errTruncated
	}
	size, err := kilobytes(rest)
	if err != nil {
		return fmt.Errorf("%s: malformed Anonymous line: %s", path, bytes.TrimSpace(line))
	}
	*total += size
	return nil
}

// errTruncated is how addAnonymous says that the smaps it is reading ended mid-line, which its
// callers answer with the total they have and no error.
var errTruncated = errors.New("procread: the smaps read ended mid-line")

// ReadProcCgroup is the whole text of <root>/proc/<pid>/cgroup, the controllers a process is in
// and the path it holds in each: under v2 one line names the group, under v1 one line per
// controller does. It is the file as it stands, for a caller that wants the text itself; the pod
// a Kubernetes node's process belongs to is read out of it by PodCgroup below.
//
// A layout is never read from this file. This pod's gVisor writes a path that exists nowhere on
// the filesystem (INFRA-1208, measured 2026-11-05), which is why DetectLayout reads the mount.
func ReadProcCgroup(root string, pid int) (string, error) {
	raw, err := os.ReadFile(filepath.Join(root, "proc", strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// ErrCgroupV1 is how PodCgroup refuses a pod whose cgroup path the legacy hierarchy wrote. Such a
// path names no directory of its own under the mount: the v1 hierarchy keeps one tree for each
// controller (memory/…, cpuacct/…), and this package reads a group's accounting from the unified
// hierarchy's files alone (ReadGroup, GroupCPUSeconds, GroupMemoryBytes). Answering the path
// anyway would hand a caller a directory whose files do not exist, which reads as a pod spending
// nothing at all, so the reading is refused and the caller counts a read error for it.
var ErrCgroupV1 = errors.New("procread: a pod cgroup of the legacy hierarchy, which holds no group accounting this reads")

// PodCgroup is the Kubernetes pod a process belongs to and that pod's own cgroup directory, read
// from <root>/proc/<pid>/cgroup. Both container runtime drivers are accepted, since which one a
// node runs is the kubelet's configuration and not something a sampler is told:
//
//	systemd    /kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod<uid>.slice/<container>.scope
//	cgroupfs   /kubepods/burstable/pod<uid>/<container>
//
// The systemd driver writes the uid with its dashes as underscores, which is undone here, and a
// guaranteed pod has no quality-of-service segment under either driver. A process in no pod's
// cgroup — the kubelet, the container runtime, the sampler itself — answers an empty uid, an empty
// directory and no error.
//
// A unified line (`0::<path>`) is the reading. A pod named by a legacy line alone is ErrCgroupV1:
// a host running both hierarchies writes a line for each, and the unified one is taken wherever
// the file carries it.
func PodCgroup(root string, pid int) (string, string, error) {
	path := filepath.Join(root, "proc", strconv.Itoa(pid), "cgroup")
	text, err := ReadProcCgroup(root, pid)
	if err != nil {
		return "", "", err
	}
	return parsePodCgroup(root, path, text)
}

// parsePodCgroup is PodCgroup over the text of one cgroup file, path named for the refusal.
func parsePodCgroup(root, path, text string) (string, string, error) {
	var legacy string
	for line := range strings.SplitSeq(strings.TrimSpace(text), "\n") {
		// A line is hierarchy:controllers:path. The unified hierarchy writes one line,
		// numbered 0 and naming no controller; the legacy one writes a line for each
		// controller, whose path stands under that controller's own tree.
		hierarchy, rest, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		controllers, group, found := strings.Cut(rest, ":")
		if !found {
			continue
		}
		segments := strings.Split(strings.Trim(group, "/"), "/")
		for i, segment := range segments {
			uid := podUID(segment)
			if uid == "" {
				continue
			}
			if hierarchy != "0" || controllers != "" {
				if legacy == "" {
					legacy = line
				}
				break
			}
			dir := filepath.Join(append([]string{cgroupMount(root)}, segments[:i+1]...)...)
			return uid, dir, nil
		}
	}
	if legacy != "" {
		return "", "", fmt.Errorf("%s: %w: %s", path, ErrCgroupV1, legacy)
	}
	return "", "", nil
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

// MemInfo is the two /proc/meminfo lines a resource sampler reads, in bytes: AnonPages, the
// anonymous memory of every process together, and Shmem, the shared memory a tmpfs and a POSIX
// shared segment hold, which no process's own anonymous total carries. The file reports both in
// kB. A line the file does not carry stays zero.
type MemInfo struct{ AnonPages, Shmem int64 }

// parseMemInfo is the AnonPages and the Shmem of a /proc/meminfo, in bytes, path named for the
// refusal. It is this package's one reader of that file: a Reader answers from it through the
// descriptor it keeps on the file (Reader.MemInfo), and nothing else reads it.
func parseMemInfo(raw []byte, path string) (MemInfo, error) {
	var info MemInfo
	// The line a pass is on is taken once and tried against both prefixes, rather than once
	// for each of them, and as bytes rather than as a string: a meminfo is fifty lines, read
	// for the two of them.
	wanted := []struct {
		into   *int64
		prefix []byte
	}{
		{&info.AnonPages, []byte("AnonPages:")},
		{&info.Shmem, []byte("Shmem:")},
	}
	for line := range bytes.Lines(raw) {
		for _, want := range wanted {
			rest, found := bytes.CutPrefix(line, want.prefix)
			if !found {
				continue
			}
			size, err := kilobytes(rest)
			if err != nil {
				return MemInfo{}, fmt.Errorf("%s: malformed %s line: %s",
					path, bytes.TrimSuffix(want.prefix, []byte(":")), bytes.TrimSpace(line))
			}
			*want.into = size
		}
	}
	return info, nil
}

// kilobytes is the bytes of a "   743092 kB" value, the shape /proc reports a size in: the first
// field as a count of 1024-byte units. The unit itself is not read — every size /proc writes this
// way is kB — only that a number stands where one must.
//
// It takes the bytes of the line and reads the digits itself rather than handing a string to
// strconv, so that a line costs no allocation at all: its callers run over every mapping of every
// process the sampler reads, inside a budget of 100 ms of CPU.
func kilobytes(value []byte) (int64, error) {
	digits := bytes.TrimLeft(value, " \t")
	if end := bytes.IndexAny(digits, " \t"); end >= 0 {
		digits = digits[:end]
	}
	// Fifteen digits of kB is a petabyte, past anything a kernel reports and short of the
	// multiplication below overflowing an int64, which a longer run of digits could.
	if len(digits) == 0 || len(digits) > 15 {
		return 0, fmt.Errorf("no value")
	}
	var kb int64
	for _, digit := range digits {
		if digit < '0' || digit > '9' {
			return 0, fmt.Errorf("%q is no number", digits)
		}
		kb = kb*10 + int64(digit-'0')
	}
	return kb * 1024, nil
}
