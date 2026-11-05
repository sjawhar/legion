//go:build linux

package launcher

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/procread"
)

// What the parent reads the role's processes under, and what the role's own process holds for it
// to read. procRoot is the root procread is given in a pod — the container's own "/", since the
// launcher accounts for the processes of the container it is PID 1 of. grandchildHold is the
// anonymous memory the role's process holds, as a string a shell builds itself, which no file
// backs; grandchildFloor is the least the parent must read back for it once that process says it
// holds the string, the hold less a quarter, so a kernel that accounts those pages a little
// differently than the shell asked for still proves the read is of real memory and not of
// zeroes. grandchildLife is how long that process stays: far longer than the walk below takes,
// and short enough that one the test somehow failed to end is gone before the package's next
// test. grandchildSearch is what the walk and the wait for that word are given.
const (
	procRoot         = "/"
	grandchildHold   = 4 << 20
	grandchildFloor  = grandchildHold * 3 / 4
	grandchildLife   = 20 * time.Second
	grandchildSearch = 10 * time.Second
)

// grandchildScript is the role's own process, the shim role-init starts inside the role's
// namespaces: a shell that builds a string of grandchildHold bytes by doubling one — a shell
// variable is the shell's own heap, so every page of it is anonymous memory the kernel accounts
// to the process — then says so on the output the launcher gives every role, so the parent reads
// what it holds rather than what it was still building, and then stays for grandchildLife while
// the parent finds and reads it. Its $0 is the test's marker, so the word it says carries it
// too. It takes the hold and that lifetime, in seconds, in that order.
const grandchildScript = `hold=x
while [ ${#hold} -lt %d ]; do hold=$hold$hold; done
echo "$0 holds"
sleep %d`

// A role's processes are the launcher's to account for, and the launcher is outside every
// namespace they run in: it is the parent of the role's init, which is the init of the role's own
// user, mount, PID and IPC namespaces, and every process of the role is that init's child or
// below it. This is the read the resource sampler of INFRA-1208 stands on, and it is proved here
// before the sampler is written: a role generation of the production shape (newRoleProcess, the
// clone flags roleNamespaceAttr gives every role) whose shim holds memory of its own, and, from
// the parent alone, the walk the sampler makes over the container's /proc — procread.PIDs,
// procread.ReadStat and the parent map they build — finding that grandchild as a descendant of
// the role's init and reading its stat, its argument vector and its anonymous memory across the
// four namespaces. Nothing inside the role is asked what pid it has: a pid the role reported
// would be its own PID namespace's, which names no process of the container's /proc.
func TestARolesGrandchildIsReadThroughProcFromOutsideItsNamespaces(t *testing.T) {
	requireRoleNamespaces(t)
	scratch := namespaceScratch(t)
	marker := "legion-sampler-grandchild-" + strconv.Itoa(os.Getpid())
	script := fmt.Sprintf(grandchildScript, grandchildHold, int(grandchildLife/time.Second))
	cfg := RoleInitConfig{
		RolesDir: filepath.Join(scratch, "roles"), Role: "sampled", Generation: 1,
		Home: filepath.Join(scratch, "home"), HomeMounts: filepath.Join(scratch, "home-mounts"),
		FixedRoot: filepath.Join(scratch, "fixed"),
		// The marker is the shell's own $0, so the argument vector the parent reads back holds it
		// as an argument of its own rather than inside the script.
		Argv: []string{"/bin/sh", "-c", script, marker},
	}
	// The layout one generation needs, as the image's own check builds it (CheckRoleNamespaces):
	// the role's gh, private and state directories under the roles directory and under the fixed
	// root they are bound back at, an empty home, and a home mounts directory carrying no mount.
	dirs := []string{cfg.Home, cfg.HomeMounts}
	for _, name := range []string{RoleGHDir, RolePrivateDir, RoleStateDir} {
		dirs = append(dirs, filepath.Join(cfg.RolesDir, cfg.Role, name), filepath.Join(cfg.FixedRoot, name))
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	useRoleLayout(t, cfg.Home, cfg.HomeMounts, cfg.FixedRoot)
	output := &lockedBuffer{}
	role, err := newRoleProcess(cfg, os.Environ(), output, output)
	if err != nil {
		t.Fatal(err)
	}
	// The kill below ends the role's PID namespace, and the kernel ends every process in it with
	// its init; the wait delay keeps one that still held the role's output pipe from holding the
	// test with it.
	role.cmd.WaitDelay = time.Second
	if err := role.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = role.cmd.Process.Kill()
		_ = role.cmd.Wait()
		_ = role.status.Close()
	})

	rolePID := role.cmd.Process.Pid
	var grandchild int
	for deadline := time.Now().Add(grandchildSearch); ; {
		if grandchild = roleGrandchild(t, rolePID, marker); grandchild != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no descendant of the role's init %d carries %q among its arguments after %s\n%s",
				rolePID, marker, grandchildSearch, output.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The grandchild is found before it holds its string, so the parent waits for the word the
	// role's own output carries — role-init gives its shim the writers the launcher gave it — and
	// then reads what the grandchild holds rather than what it was still building.
	held := marker + " holds"
	for deadline := time.Now().Add(grandchildSearch); !strings.Contains(output.String(), held); {
		if time.Now().After(deadline) {
			t.Fatalf("the role's grandchild %d did not say %q within %s\n%s", grandchild, held, grandchildSearch, output.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	stat, err := procread.ReadStat(procRoot, grandchild)
	if err != nil {
		t.Fatalf("the parent could not read the stat of the role's grandchild %d: %v\n%s", grandchild, err, output.String())
	}
	if stat.PID != grandchild {
		t.Errorf("the stat of the role's grandchild %d reports pid %d, want the pid the parent read it by", grandchild, stat.PID)
	}
	if stat.Parent != rolePID {
		t.Errorf("the role's grandchild %d reports parent %d, want the role's init %d: the chain the parent follows is the kernel's, not the role's",
			grandchild, stat.Parent, rolePID)
	}
	args, err := procread.ReadCmdline(procRoot, grandchild)
	if err != nil {
		t.Fatalf("the parent could not read the arguments of the role's grandchild %d: %v\n%s", grandchild, err, output.String())
	}
	if !slices.Contains(args, marker) {
		t.Errorf("the arguments of the role's grandchild %d are %q, want the marker %q among them", grandchild, args, marker)
	}
	anonymous, err := procread.ReadAnonymous(procRoot, grandchild)
	if err != nil {
		t.Fatalf("the parent could not read the anonymous memory of the role's grandchild %d: %v\n%s", grandchild, err, output.String())
	}
	if anonymous < grandchildFloor {
		t.Errorf("the role's grandchild %d holds %d bytes of anonymous memory, want at least %d of the %d it says it holds\n%s",
			grandchild, anonymous, int64(grandchildFloor), int64(grandchildHold), output.String())
	}
	t.Logf("the parent read the role's grandchild %d across its namespaces: parent %d, %d arguments, %d bytes of anonymous memory",
		grandchild, stat.Parent, len(args), anonymous)
}

// roleGrandchild is one pass of the walk the sampler makes, from the parent's side alone: every
// process of the container's /proc (procread.PIDs), the parent of each (procread.ReadStat), and,
// from the map those answers build, the one process under rolePID whose arguments
// (procread.ReadCmdline) hold marker. It answers 0 while no such process is there, since a role's
// init is started before its shim exists.
//
// A process that ended between the walk and a read is skipped, as a sampler's pass skips it, and
// so is one this process may not read. Neither can be the role's own grandchild: the parent
// started it, and the role's user namespaces map it back to the parent's own uid. Anything else
// is a failure, since a pass must read what /proc gives it.
func roleGrandchild(t *testing.T, rolePID int, marker string) int {
	t.Helper()
	pids, err := procread.PIDs(procRoot)
	if err != nil {
		t.Fatalf("the parent could not walk the container's processes: %v", err)
	}
	parents := make(map[int]int, len(pids))
	for _, pid := range pids {
		stat, err := procread.ReadStat(procRoot, pid)
		switch {
		case unreadable(err):
			continue
		case err != nil:
			t.Fatalf("the parent could not read the stat of process %d: %v", pid, err)
		}
		parents[pid] = stat.Parent
	}
	for _, pid := range pids {
		if !descendsFrom(parents, pid, rolePID) {
			continue
		}
		args, err := procread.ReadCmdline(procRoot, pid)
		switch {
		case unreadable(err):
			continue
		case err != nil:
			t.Fatalf("the parent could not read the arguments of process %d: %v", pid, err)
		}
		if slices.Contains(args, marker) {
			return pid
		}
	}
	return 0
}

// unreadable reports whether a read of /proc failed for a process the reader cannot account for:
// one that ended while the pass walked, and one belonging to a user this process may not read.
func unreadable(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission)
}

// descendsFrom reports whether pid is below ancestor in the parent map, which is how a sampler
// attributes a process to the role-init that heads its namespaces. The climb ends at a pid the
// map does not hold — the process ended mid-pass, or the climb reached pid 0 — and after as many
// steps as the map has entries, so a map a process ended under cannot loop it.
func descendsFrom(parents map[int]int, pid, ancestor int) bool {
	for range len(parents) {
		parent, held := parents[pid]
		if !held {
			return false
		}
		if parent == ancestor {
			return true
		}
		pid = parent
	}
	return false
}
