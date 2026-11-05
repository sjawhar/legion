package procread

import (
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The three fixture profiles under testdata, each a root a reader is given: gvisor is this pod's
// own kernel (cgroup v1 at the mount root, no smaps_rollup, a tiny Shmem), microvm is a unified
// hierarchy with a child group and a real Shmem, and runcnode is a node's kubepods tree.
const (
	gvisor   = "gvisor"
	microvm  = "microvm"
	runcnode = "runcnode"
)

// profile is one fixture root, named the way a caller names "/": a directory holding proc/… and
// sys/fs/cgroup/… of its own.
func profile(name string) string { return filepath.Join("testdata", name) }

// PIDs are the numeric directories and nothing else: /proc holds meminfo and a dozen other files
// beside the processes, and the order is numeric rather than the directory's own, which sorts
// "137" before "42" and would have two passes disagree about the order they saw the same pod in.
func TestPIDsAreTheNumericDirectoriesOfProcAscending(t *testing.T) {
	pids, err := PIDs(profile(gvisor))
	if err != nil {
		t.Fatalf("PIDs(%s) = %v", profile(gvisor), err)
	}
	if want := []int{1, 42, 137}; !slices.Equal(pids, want) {
		t.Errorf("PIDs() = %v, want %v", pids, want)
	}
}

// A root with no /proc at all is the filesystem's own error, not an empty pod: a sampler told a
// root it cannot read must say so rather than report that nothing is running.
func TestPIDsRefusesARootWithNoProc(t *testing.T) {
	if _, err := PIDs(t.TempDir()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("PIDs(an empty directory) error = %v, want one that is fs.ErrNotExist", err)
	}
}

// ReadStat is the fixture's own line, including the one whose comm holds spaces and parentheses:
// the fields are counted after the last `)`, so "fun (ny) proc" costs the reading nothing.
func TestReadStatIsTheProcessesOwnLine(t *testing.T) {
	for _, test := range []struct {
		pid  int
		want Stat
	}{
		{1, Stat{
			PID: 1, Parent: 0, Comm: "legion",
			UserTicks: 4210, SystemTicks: 1308, ChildUserTicks: 12, ChildSystemTicks: 7,
			ResidentPages: 25638, StartTicks: 1337,
		}},
		{42, Stat{
			PID: 42, Parent: 1, Comm: "fun (ny) proc",
			UserTicks: 73, SystemTicks: 19,
			ResidentPages: 512, StartTicks: 9981,
		}},
		{137, Stat{
			PID: 137, Parent: 2, Comm: "kworker/0:2",
			SystemTicks: 3, StartTicks: 2042,
		}},
	} {
		stat, err := ReadStat(profile(gvisor), test.pid)
		if err != nil {
			t.Fatalf("ReadStat(%d) = %v", test.pid, err)
		}
		if stat != test.want {
			t.Errorf("ReadStat(%d) = %+v, want %+v", test.pid, stat, test.want)
		}
	}
}

// A process that ended between a pass's PIDs and its reads is the filesystem's "no such file",
// which the caller tells by fs.ErrNotExist and skips — never a failed pass.
func TestReadStatOfAProcessThatIsGoneIsNotExist(t *testing.T) {
	if _, err := ReadStat(profile(gvisor), 99999); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadStat(a pid that is gone) error = %v, want one that is fs.ErrNotExist", err)
	}
}

// ParseStat reads the fields it wants after the last `)` of the line. A comm is the first sixteen
// bytes of an executable's name and may hold anything but a NUL — spaces, parentheses, both — so a
// reading that split the whole line on spaces would take the state for a parent pid and the parent
// pid for a page count in exactly the case a resource sampler must not get wrong.
func TestParseStatCountsFieldsAfterTheLastParenthesis(t *testing.T) {
	for _, test := range []struct {
		name string
		line string
		want Stat
	}{
		{
			name: "an ordinary command name",
			line: "7 (omp) S 1 7 7 0 -1 4194304 500 0 0 0 99 11 2 1 20 0 9 0 4242 10000 303 0 0 0\n",
			want: Stat{
				PID: 7, Parent: 1, Comm: "omp",
				UserTicks: 99, SystemTicks: 11, ChildUserTicks: 2, ChildSystemTicks: 1,
				ResidentPages: 303, StartTicks: 4242,
			},
		},
		{
			name: "a command name holding spaces and parentheses",
			line: "8 (fun (ny) proc) R 7 8 8 0 -1 4194304 1 0 0 0 1 2 3 4 20 0 1 0 5151 20000 404 0 0 0\n",
			want: Stat{
				PID: 8, Parent: 7, Comm: "fun (ny) proc",
				UserTicks: 1, SystemTicks: 2, ChildUserTicks: 3, ChildSystemTicks: 4,
				ResidentPages: 404, StartTicks: 5151,
			},
		},
		{
			name: "a command name that is only parentheses",
			line: "9 (()) S 8 9 9 0 -1 4194304 1 0 0 0 0 0 0 0 20 0 1 0 6161 30000 505 0 0 0\n",
			want: Stat{PID: 9, Parent: 8, Comm: "()", ResidentPages: 505, StartTicks: 6161},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			stat, err := ParseStat(test.line)
			if err != nil {
				t.Fatalf("ParseStat() = %v", err)
			}
			if stat != test.want {
				t.Errorf("ParseStat() = %+v, want %+v", stat, test.want)
			}
		})
	}
}

// A line this cannot read is a refusal quoting the line, never a Stat of zeroes a sampler would
// publish as a process that uses no memory and no CPU.
func TestParseStatRefusesALineItCannotRead(t *testing.T) {
	for _, test := range []struct {
		name string
		line string
	}{
		{"no comm at all", "7 omp S 1 7 7 0 -1 4194304 500 0 0 0 99 11 2 1 20 0 9 0 4242 10000 303"},
		{"no pid before the comm", "x (omp) S 1 7 7 0 -1 4194304 500 0 0 0 99 11 2 1 20 0 9 0 4242 10000 303"},
		{"too few fields after the comm", "7 (omp) S 1 7 7 0 -1 4194304 500 0 0 0 99 11"},
		{"a parent pid that is no number", "7 (omp) S x 7 7 0 -1 4194304 500 0 0 0 99 11 2 1 20 0 9 0 4242 10000 303"},
		{"a utime that is no number", "7 (omp) S 1 7 7 0 -1 4194304 500 0 0 0 x 11 2 1 20 0 9 0 4242 10000 303"},
		{"an empty line", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			stat, err := ParseStat(test.line)
			if err == nil {
				t.Fatalf("ParseStat(%q) = %+v, want an error", test.line, stat)
			}
			if stat != (Stat{}) {
				t.Errorf("ParseStat(%q) = %+v beside its error, want the zero Stat", test.line, stat)
			}
		})
	}
}

// ClockTicks is the one conversion the caller holds: /proc counts a process's time in hundredths
// of a second, whatever the kernel, so the fixture's 4210 + 1308 ticks are 55.18 seconds.
func TestClockTicksTurnAFixturesTicksIntoSeconds(t *testing.T) {
	stat, err := ReadStat(profile(gvisor), 1)
	if err != nil {
		t.Fatalf("ReadStat(1) = %v", err)
	}
	seconds := float64(stat.UserTicks+stat.SystemTicks) / ClockTicks
	if math.Abs(seconds-55.18) > 1e-9 {
		t.Errorf("(utime+stime)/ClockTicks = %v, want 55.18", seconds)
	}
}

// cmdline is the argument vector with a NUL after each argument, so the last NUL is a terminator
// and not an empty final argument. A kernel thread has no argument vector at all: an empty list
// and no error, since the process is there and only its arguments are not.
func TestReadCmdlineSplitsOnNULAndDropsTheTrailingEmptyArgument(t *testing.T) {
	for _, test := range []struct {
		pid  int
		want []string
	}{
		{1, []string{"/opt/legion/bin/legion", "launcher", "--roles-dir", "/legion/roles"}},
		{42, []string{"omp", "--mode", "rpc"}},
		{137, nil},
	} {
		args, err := ReadCmdline(profile(gvisor), test.pid)
		if err != nil {
			t.Fatalf("ReadCmdline(%d) = %v", test.pid, err)
		}
		if !slices.Equal(args, test.want) {
			t.Errorf("ReadCmdline(%d) = %q, want %q", test.pid, args, test.want)
		}
	}
}

// ReadAnonymous sums the Anonymous line of every mapping, which smaps reports in kB: pid 1's
// mappings hold 0 + 20480 + 14476 + 0 kB. The sum is taken per mapping because this pod's gVisor
// writes neither smaps_rollup nor a RssAnon line in status, which the fixture keeps true of
// itself — a kernel that gained smaps_rollup would make this reading slower, never wrong.
func TestReadAnonymousSumsEveryMappingsAnonymousLine(t *testing.T) {
	for _, test := range []struct {
		pid  int
		want int64
	}{
		{1, 34956 * 1024},
		{42, 1152 * 1024},
	} {
		bytes, err := ReadAnonymous(profile(gvisor), test.pid)
		if err != nil {
			t.Fatalf("ReadAnonymous(%d) = %v", test.pid, err)
		}
		if bytes != test.want {
			t.Errorf("ReadAnonymous(%d) = %d, want %d", test.pid, bytes, test.want)
		}
	}
	rollup := filepath.Join(profile(gvisor), "proc", "1", "smaps_rollup")
	if _, err := os.Stat(rollup); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat %s = %v, want it absent: the gvisor fixture is a kernel that writes no rollup", rollup, err)
	}
}

// ReadProcCgroup hands the file over as it stands: under v1 it is one line per controller, and
// under this pod's gVisor every one of them names a directory that exists nowhere under the mount,
// which is why it is read for the record and never for a path to open.
func TestReadProcCgroupIsTheWholeFile(t *testing.T) {
	text, err := ReadProcCgroup(profile(gvisor), 1)
	if err != nil {
		t.Fatalf("ReadProcCgroup(1) = %v", err)
	}
	want := "12:memory:/e063798f1a4c4d2db0d6f3f6b8c9a1e2\n" +
		"11:cpuacct:/e063798f1a4c4d2db0d6f3f6b8c9a1e2\n" +
		"10:pids:/e063798f1a4c4d2db0d6f3f6b8c9a1e2\n"
	if text != want {
		t.Errorf("ReadProcCgroup(1) = %q, want %q", text, want)
	}
}

// meminfo reports kB and the sampler publishes bytes. Shmem is read beside AnonPages because no
// process's own anonymous total carries it: under gVisor it is 56 kB of nothing in particular,
// and on a microVM half a gigabyte of tmpfs that a reading of AnonPages alone would lose.
func TestReadMemInfoIsAnonPagesAndShmemInBytes(t *testing.T) {
	for _, test := range []struct {
		root string
		want MemInfo
	}{
		{gvisor, MemInfo{AnonPages: 743092 * 1024, Shmem: 56 * 1024}},
		{microvm, MemInfo{AnonPages: 2097152 * 1024, Shmem: 524288 * 1024}},
	} {
		info, err := ReadMemInfo(profile(test.root))
		if err != nil {
			t.Fatalf("ReadMemInfo(%s) = %v", test.root, err)
		}
		if info != test.want {
			t.Errorf("ReadMemInfo(%s) = %+v, want %+v", test.root, info, test.want)
		}
	}
	if _, err := ReadMemInfo(t.TempDir()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadMemInfo(a root with no /proc) error = %v, want one that is fs.ErrNotExist", err)
	}
}
