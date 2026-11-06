// packages/envoy/internal/broker/helper/peerfacts.go
//go:build linux

package helper

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sjawhar/envoy/internal/broker/record"
)

// atCLKTCK is the auxiliary vector's AT_CLKTCK (getauxval(3)): the kernel's USER_HZ, the unit of
// /proc/<pid>/stat's starttime field. Go's runtime exposes no getauxval, so clockTicks reads it
// from /proc/self/auxv, where the kernel writes the same pairs. It is 100 on this fleet
// (`getconf CLK_TCK` on the devbox, 7.0.0-1017-azure linux/amd64, answers 100), and reading it
// rather than assuming it keeps a host whose kernel says otherwise from dating every process
// start wrong.
const atCLKTCK = 17

// fallbackUserHZ is the USER_HZ a reading of /proc/self/auxv without an AT_CLKTCK entry falls
// back to: the value every Linux architecture this module runs on uses.
const fallbackUserHZ = 100

// maxPeerPathBytes is the longest path a read here accepts: PATH_MAX, the longest Linux resolves,
// plus room for the " (deleted)" suffix the kernel appends to an executable whose file is gone.
const maxPeerPathBytes = 4096 + len(" (deleted)")

// Facts are the kernel's answers about a pinned process, with the instant they were read. They
// are answers about that process, not statements by it: a process writes its own argv, comm and
// environment, and a reader of those reads what the process chose to say. It does choose some of
// what the kernel then reports — it execs the binary /proc/<pid>/exe resolves and chdirs to the
// directory /proc/<pid>/cwd names — but it cannot tell the kernel to answer one thing while
// doing another, and it cannot answer for a process it is not.
type Facts struct {
	// The pid the handle named when these facts were read.
	PID int
	// /proc/<pid>/exe: the binary the kernel resolved for this process, verbatim, including the
	// " (deleted)" suffix the kernel appends when that file has since been replaced or removed.
	Executable string
	// /proc/<pid>/cwd: the directory the process is working in, verbatim.
	WorkingDir string
	// The unified hierarchy's line of /proc/<pid>/cgroup ("0::" and the path) with its newline
	// removed, or, on a host that gives the process no unified hierarchy, the file's first line.
	// The path is the one the host's cgroup manager placed the process in: under systemd, the
	// scope or service it runs in.
	Cgroup string
	// When the kernel says the process started: /proc/<pid>/stat's starttime, in USER_HZ ticks
	// since boot, added to the boot instant /proc/stat's btime names, truncated to the second
	// btime is written in.
	StartedAt time.Time
	// When these facts were read: the instant taken once the last of them had been read through
	// the pinned handle.
	ReadAt time.Time
}

// Claim is these facts as a request object's peer claim, the shape the broker stores and an
// approval page renders. Each instant becomes whole Unix seconds; a start time is already no
// finer than the second /proc/stat's btime names.
func (f Facts) Claim() record.PeerFacts {
	return record.PeerFacts{
		PID:        f.PID,
		Executable: f.Executable,
		WorkingDir: f.WorkingDir,
		Cgroup:     f.Cgroup,
		StartedAt:  f.StartedAt.Unix(),
		ReadAt:     f.ReadAt.Unix(),
	}
}

// errPeerGone is every refusal that means the handle no longer names a live process, so there is
// nothing the kernel can still be asked about it.
var errPeerGone = errors.New("the pinned process is gone")

// Facts reads the kernel's answers about the pinned process — its executable, its working
// directory, its start time and its cgroup — through the pidfd this Peer holds, and stamps the
// reading with now(), taken once the last read has returned.
//
// Everything it reads, it reads through two handles and never through a path it rebuilds. The
// kernel offers no way to open a process's /proc entries from a pidfd itself: there is no
// openat(2) on a pidfd, and this kernel answers PIDFD_GET_INFO with ENOTTY (measured on
// 7.0.0-1017-azure), so a pid number has to appear once, to open the directory. Taking that
// number from anywhere but the handle, or re-using it for a second lookup, is what would be
// wrong, and this is why:
//
//   - The pin does not reserve the pid number (Peer's doc comment). The kernel is free to give
//     that number to a new process as soon as the pinned one is reaped, so a second path built
//     from the number can resolve to a different process than the one the handle pinned — the
//     race the pin exists to close, re-opened at the last step. A reading of
//     /proc/<pid>/exe by number is a reading of whatever process holds that number at that
//     moment, which is not a question anyone asked.
//   - So the number comes from the handle (Peer.PID reads it out of /proc/self/fdinfo/<pidfd>,
//     where the kernel writes -1, and so 0 here, once the process has been reaped), /proc/<pid>
//     is opened once as a directory, and every value is then read through that one descriptor
//     with openat and readlinkat. The descriptor does not follow a reused pid: once its process
//     is gone, every read through it answers ESRCH (measured on 7.0.0-1017-azure: readlinkat
//     "exe" and openat "cgroup" against a reaped child both answered "no such process"; the
//     kernel has invalidated a dead task's /proc directory this way since cgroup-era procfs, and
//     TestAProcDirectoryDescriptorNeverFollowsAReusedPid holds this kernel to it).
//   - The handle is read again after the open and again after the reads, and a pid that no
//     longer matches refuses the whole reading. That is what rules out a reuse that happened
//     before the open: a pid number can be reused only after its process is reaped, and a reaped
//     process makes Peer.PID answer 0 from then on, so a handle that still names the same pid
//     after the open had not been reaped when the open happened.
func (p *Peer) Facts(now func() time.Time) (Facts, error) {
	pid := p.PID()
	if pid == 0 {
		return Facts{}, errPeerGone
	}
	dirfd, err := unix.Open("/proc/"+strconv.Itoa(pid), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return Facts{}, fmt.Errorf("open /proc/%d: %w", pid, err)
	}
	defer unix.Close(dirfd)
	if again := p.PID(); again != pid {
		// The handle stopped naming pid while /proc/<pid> was being opened, so that directory may
		// already belong to another process. Nothing read through it would be about the peer.
		return Facts{}, errPeerGone
	}
	exe, err := readLinkAt(dirfd, "exe")
	if err != nil {
		return Facts{}, err
	}
	cwd, err := readLinkAt(dirfd, "cwd")
	if err != nil {
		return Facts{}, err
	}
	cgroup, err := readFileAt(dirfd, "cgroup")
	if err != nil {
		return Facts{}, err
	}
	stat, err := readFileAt(dirfd, "stat")
	if err != nil {
		return Facts{}, err
	}
	started, err := startedAt(stat)
	if err != nil {
		return Facts{}, err
	}
	readAt := now()
	if again := p.PID(); again != pid {
		return Facts{}, errPeerGone
	}
	return Facts{
		PID:        pid,
		Executable: exe,
		WorkingDir: cwd,
		Cgroup:     unifiedCgroupLine(cgroup),
		StartedAt:  started,
		ReadAt:     readAt,
	}, nil
}

// readLinkAt resolves one of the pinned process's /proc symlinks through the directory
// descriptor. A link that fills the buffer is refused rather than truncated: a truncated path
// names a directory that is not the one the process is in.
func readLinkAt(dirfd int, name string) (string, error) {
	buf := make([]byte, maxPeerPathBytes+1)
	n, err := unix.Readlinkat(dirfd, name, buf)
	switch {
	case err != nil:
		return "", fmt.Errorf("read /proc/<pid>/%s: %w", name, err)
	case n <= 0:
		return "", fmt.Errorf("read /proc/<pid>/%s: empty", name)
	case n > maxPeerPathBytes:
		return "", fmt.Errorf("read /proc/<pid>/%s: over %d bytes", name, maxPeerPathBytes)
	}
	return string(buf[:n]), nil
}

// readFileAt reads one of the pinned process's small /proc files through the directory
// descriptor. These files report a size of 0, so the read runs to EOF; a read that ends any
// other way is an error, never a partial value, since half a cgroup line reads like a whole one.
func readFileAt(dirfd int, name string) (string, error) {
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", fmt.Errorf("open /proc/<pid>/%s: %w", name, err)
	}
	f := os.NewFile(uintptr(fd), "/proc/<pid>/"+name)
	defer f.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := f.Read(buf)
		sb.Write(buf[:n])
		switch {
		case errors.Is(err, io.EOF):
			if sb.Len() == 0 {
				return "", fmt.Errorf("read /proc/<pid>/%s: empty", name)
			}
			return sb.String(), nil
		case err != nil:
			return "", fmt.Errorf("read /proc/<pid>/%s: %w", name, err)
		case sb.Len() > maxPeerPathBytes:
			return "", fmt.Errorf("read /proc/<pid>/%s: over %d bytes", name, maxPeerPathBytes)
		}
	}
}

// unifiedCgroupLine is the cgroup v2 line of a /proc/<pid>/cgroup file ("0::" and the path),
// which is the whole file on a unified host. A host that gives the process no unified hierarchy
// (cgroup v1 only) has no such line, and its first line stands in: one line, so an approval page
// renders one value and no newline reaches the record.
func unifiedCgroupLine(cgroup string) string {
	lines := strings.Split(strings.TrimRight(cgroup, "\n"), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "0::") {
			return line
		}
	}
	return lines[0]
}

// startedAt turns a /proc/<pid>/stat line into the instant the kernel says the process started:
// its starttime field, in the USER_HZ ticks clockTicks reads from the auxiliary vector, added to
// the boot instant /proc/stat's btime names.
func startedAt(stat string) (time.Time, error) {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 || i+2 > len(stat) {
		return time.Time{}, errors.New("read /proc/<pid>/stat: no comm")
	}
	fields := strings.Fields(stat[i+2:])
	if len(fields) < 20 {
		return time.Time{}, fmt.Errorf("read /proc/<pid>/stat: %d fields", len(fields))
	}
	ticks, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("read /proc/<pid>/stat: starttime: %w", err)
	}
	boot, err := bootTime()
	if err != nil {
		return time.Time{}, err
	}
	hz, err := clockTicks()
	if err != nil {
		return time.Time{}, err
	}
	return boot.Add(time.Duration(ticks) * time.Second / time.Duration(hz)), nil
}

// clockTicks is the kernel's USER_HZ, from AT_CLKTCK in /proc/self/auxv: pairs of native-endian
// unsigned longs, type then value, ending at a type of 0 (AT_NULL). A kernel that writes no
// AT_CLKTCK entry leaves fallbackUserHZ.
func clockTicks() (uint64, error) {
	auxv, err := os.ReadFile("/proc/self/auxv")
	if err != nil {
		return 0, fmt.Errorf("read /proc/self/auxv: %w", err)
	}
	for i := 0; i+16 <= len(auxv); i += 16 {
		key := binary.NativeEndian.Uint64(auxv[i : i+8])
		value := binary.NativeEndian.Uint64(auxv[i+8 : i+16])
		switch {
		case key == 0:
			return fallbackUserHZ, nil
		case key != atCLKTCK:
			continue
		case value == 0:
			return 0, errors.New("read /proc/self/auxv: AT_CLKTCK is 0")
		}
		return value, nil
	}
	return fallbackUserHZ, nil
}

// bootTime is the instant the kernel booted, as /proc/stat's btime line names it in whole
// seconds since the epoch.
func bootTime() (time.Time, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return time.Time{}, err
	}
	defer f.Close()
	// The interrupt lines of /proc/stat are far longer than a default scanner token on a host
	// with many IRQs, and btime sits after them.
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		rest, ok := strings.CutPrefix(scanner.Text(), "btime ")
		if !ok {
			continue
		}
		seconds, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("read /proc/stat: btime: %w", err)
		}
		return time.Unix(seconds, 0), nil
	}
	if err := scanner.Err(); err != nil {
		return time.Time{}, fmt.Errorf("read /proc/stat: %w", err)
	}
	return time.Time{}, errors.New("read /proc/stat: no btime")
}
