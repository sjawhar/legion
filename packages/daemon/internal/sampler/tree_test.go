package sampler

import (
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// fakeProcess is one process of a synthetic /proc: what the walk reads of it, and the two ways a
// read of it can fail. The counters are ticks, as /proc writes them, and pages, as /proc counts
// memory. A test writes the same process again with other counters to make a second pass.
type fakeProcess struct {
	pid, parent int
	args        []string
	own, reaped uint64 // utime and cutime; stime and cstime are left at zero
	start       uint64
	pages       uint64
	// gone is a process whose directory the pass lists and whose stat has already been taken
	// away, which is a process that ended between the two reads; unparsable is a stat line
	// no reader can make sense of.
	gone, unparsable bool
}

// writeProc is a /proc holding exactly these processes, under a root of the test's own: one
// directory per process with the stat line the kernel writes and the NUL-separated cmdline. It
// clears what stood there first, so a second call is the next pass over a tree that has moved on.
// No test reads the host's /proc: every number below is one the test wrote.
func writeProc(t *testing.T, root string, processes ...fakeProcess) {
	t.Helper()
	dir := filepath.Join(root, "proc")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("could not clear the fixture's /proc: %v", err)
	}
	for _, process := range processes {
		pidDir := filepath.Join(dir, strconv.Itoa(process.pid))
		if err := os.MkdirAll(pidDir, 0o755); err != nil {
			t.Fatalf("could not write process %d: %v", process.pid, err)
		}
		cmdline := strings.Join(process.args, "\x00")
		if len(process.args) > 0 {
			cmdline += "\x00"
		}
		if err := os.WriteFile(filepath.Join(pidDir, "cmdline"), []byte(cmdline), 0o644); err != nil {
			t.Fatalf("could not write the cmdline of process %d: %v", process.pid, err)
		}
		if process.gone {
			continue
		}
		line := statLine(process)
		if process.unparsable {
			line = "nothing a reader of /proc can parse\n"
		}
		if err := os.WriteFile(filepath.Join(pidDir, "stat"), []byte(line), 0o644); err != nil {
			t.Fatalf("could not write the stat of process %d: %v", process.pid, err)
		}
	}
}

// statLine is the line /proc/<pid>/stat holds, with every field this package does not read left
// at a plausible constant: the pid, the program name in parentheses as the kernel writes it (its
// first fifteen bytes), the state, the parent, nine fields nothing here reads, utime, stime,
// cutime and cstime, four more, then starttime, vsize and the resident pages.
func statLine(process fakeProcess) string {
	comm := "kthread"
	if len(process.args) > 0 {
		comm = path.Base(process.args[0])
	}
	if len(comm) > 15 {
		comm = comm[:15]
	}
	fields := []string{"S", strconv.Itoa(process.parent)}
	fields = append(fields, slices.Repeat([]string{"0"}, 9)...)
	fields = append(fields, strconv.FormatUint(process.own, 10), "0", strconv.FormatUint(process.reaped, 10), "0")
	fields = append(fields, "20", "0", "1", "0")
	fields = append(fields, strconv.FormatUint(process.start, 10), "4194304", strconv.FormatUint(process.pages, 10))
	return fmt.Sprintf("%d (%s) %s\n", process.pid, comm, strings.Join(fields, " "))
}

// next is one pass over the fixture, which must never fail: a pass fails only where /proc itself
// cannot be listed.
func next(t *testing.T, walk *Walk, roles map[string]int) Pass {
	t.Helper()
	pass, err := walk.Next(roles)
	if err != nil {
		t.Fatalf("the pass failed: %v", err)
	}
	return pass
}

// pairOf is the role and kind the pass gave this process, and a failure where the pass did not
// report it at all.
func pairOf(t *testing.T, pass Pass, pid int) Pair {
	t.Helper()
	for _, process := range pass.Processes {
		if process.PID == pid {
			return process.Pair
		}
	}
	t.Fatalf("the pass did not report process %d", pid)
	return Pair{}
}

// wantCPU fails unless the pass charged this pair these seconds. The comparison allows the last
// bits of a division, since a reaped rise is split in proportion and not in whole ticks.
func wantCPU(t *testing.T, pass Pass, pair Pair, want float64) {
	t.Helper()
	if got := pass.CPUSeconds[pair]; math.Abs(got-want) > 1e-9 {
		t.Errorf("the pass charged %v %g seconds, want %g", pair, got, want)
	}
}

// rolesTree is an issue pod's container as the walk reads it: the launcher at process 1 with the
// sampler and the test Postgres beside it, a build somebody left behind through `kubectl exec`,
// and two role generations, each a role-init with its shim, its harness and the work that harness
// started. Every process carries the counters the test gives it.
func rolesTree() []fakeProcess {
	return []fakeProcess{
		{pid: 1, parent: 0, args: []string{"/opt/legion/bin/legion", "launcher", "--roles-dir", "/var/run/legion/roles"}},
		{pid: 2, parent: 1, args: []string{"/opt/legion/bin/legion", "sampler", "--listen", "0.0.0.0:9464"}},
		{pid: 3, parent: 1, args: []string{"postgres", "-D", "/var/lib/postgresql/data"}},
		{pid: 4, parent: 1, args: []string{"go", "build", "./..."}},
		{pid: 5, parent: 4, args: []string{"/usr/lib/go/pkg/tool/linux_amd64/compile", "-o", "a.out"}},
		{pid: 10, parent: 1, args: []string{"/opt/legion/bin/legion", "role-init", "--role", "implementer"}},
		{pid: 11, parent: 10, args: []string{"/opt/legion/bin/legion", "worker-shim", "--role", "implementer"}},
		{pid: 12, parent: 11, args: []string{"omp", "--mode", "rpc"}},
		{pid: 13, parent: 12, args: []string{"bash", "-lc", "sleep 1"}},
		{pid: 14, parent: 13, args: []string{"go", "build", "./..."}},
		{pid: 15, parent: 14, args: []string{"/usr/lib/go/pkg/tool/linux_amd64/link", "-o", "a.out"}},
		{pid: 16, parent: 12, args: []string{"gopls", "-remote=auto"}},
		{pid: 17, parent: 12, args: []string{"bash", "-c", "go test ./... || go build ./..."}},
		{pid: 18, parent: 17, args: []string{"/usr/lib/go/pkg/tool/linux_amd64/link", "-o", "pkg.test"}},
		{pid: 20, parent: 1, args: []string{"/opt/legion/bin/legion", "role-init", "--role", "reviewer"}},
		{pid: 21, parent: 20, args: []string{"claude", "-p"}},
		{pid: 22, parent: 21, args: []string{"rg", "--json", "needle"}},
	}
}

// rolesOf is the roles file's own answer for that tree: each role's role-init process id.
func rolesOf() map[string]int { return map[string]int{"implementer": 10, "reviewer": 20} }

// A role owns the subtree of its role-init, whatever namespaces stand in the way, and a process
// outside every one of those subtrees is shared — the launcher that reaps for the container, the
// sampler, the test Postgres. The kind is the role's own harness where the tree says so, the
// first kind a rule gives walking up to the role's root otherwise, and the work a build or a test
// started carries that work's kind however far below it the process sits.
func TestAPassGivesEveryProcessItsRoleAndItsKind(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, rolesTree()...)
	pass := next(t, NewWalk(root, Rules{}), rolesOf())

	for _, test := range []struct {
		pid  int
		want Pair
		why  string
	}{
		{1, Pair{RoleShared, KindShared}, "the launcher is in no role's subtree"},
		{2, Pair{RoleShared, KindShared}, "the sampler is its own shared process"},
		{3, Pair{RoleShared, KindShared}, "the test Postgres is shared"},
		{4, Pair{RoleShared, KindBuild}, "a stray build a person left behind is still a build"},
		{5, Pair{RoleShared, KindBuild}, "and so is the compiler that build started"},
		{10, Pair{"implementer", KindAgent}, "the role-init process is the role's own"},
		{11, Pair{"implementer", KindAgent}, "and so is the worker-shim it runs"},
		{12, Pair{"implementer", KindAgent}, "and the harness the shim runs"},
		{13, Pair{"implementer", KindTool}, "a shell the harness started that no rule names"},
		{14, Pair{"implementer", KindBuild}, "the build that shell started"},
		{15, Pair{"implementer", KindBuild}, "and the compiler the build started"},
		{16, Pair{"implementer", KindTool}, "a language server is a tool"},
		{17, Pair{"implementer", KindTest}, "a shell told to test and then build is a test run"},
		{18, Pair{"implementer", KindTest}, "and the linker that test run started"},
		{20, Pair{"reviewer", KindAgent}, "the second role's init"},
		{21, Pair{"reviewer", KindAgent}, "the Claude harness of that role"},
		{22, Pair{"reviewer", KindTool}, "and what that harness ran"},
	} {
		if got := pairOf(t, pass, test.pid); got != test.want {
			t.Errorf("process %d is %v, want %v: %s", test.pid, got, test.want, test.why)
		}
	}

	// Each process is reported under the parent /proc gave it, which is the tree the roles
	// above were read from and what a reaped child's CPU is charged through.
	parents := make(map[int]int, len(pass.Processes))
	for _, process := range pass.Processes {
		parents[process.PID] = process.Parent
	}
	for _, process := range rolesTree() {
		if parents[process.pid] != process.parent {
			t.Errorf("process %d is reported under parent %d, want %d",
				process.pid, parents[process.pid], process.parent)
		}
	}
}

// The pass reports the processes largest resident memory first, so a reader spending a budget on
// something costlier than a stat spends it on the processes worth reading, and two processes of
// one size come in pid order so that two passes agree.
func TestAPassReportsTheLargestResidentMemoryFirst(t *testing.T) {
	root := t.TempDir()
	page := int64(os.Getpagesize())
	writeProc(t, root,
		fakeProcess{pid: 1, args: []string{"legion", "launcher"}, pages: 300},
		fakeProcess{pid: 2, parent: 1, args: []string{"omp"}, pages: 25000},
		fakeProcess{pid: 3, parent: 1, args: []string{"gopls"}, pages: 300},
		fakeProcess{pid: 4, parent: 1, args: []string{"go", "build", "./..."}, pages: 4000},
	)
	pass := next(t, NewWalk(root, Rules{}), nil)

	var order []int
	for _, process := range pass.Processes {
		order = append(order, process.PID)
	}
	if want := []int{2, 4, 1, 3}; !slices.Equal(order, want) {
		t.Errorf("the pass reported %v, want %v", order, want)
	}
	if got, want := pass.Processes[0].ResidentBytes, 25000*page; got != want {
		t.Errorf("the largest process holds %d bytes, want %d", got, want)
	}
}

// A process the walk has not read before contributes the whole CPU /proc already reports for it,
// so a sampler that started after the pod did carries what ran before it rather than stepping its
// counters (dispatch://INFRA-1208/spec#b-a-rates). Every pass after that charges the rise alone.
func TestTheFirstPassChargesTheLifetimeAndTheNextChargesTheRise(t *testing.T) {
	root := t.TempDir()
	tree := []fakeProcess{
		{pid: 1, args: []string{"legion", "launcher"}, own: 100},
		{pid: 10, parent: 1, args: []string{"legion", "role-init", "--role", "implementer"}, own: 200},
		{pid: 12, parent: 10, args: []string{"omp", "--mode", "rpc"}, own: 0},
		{pid: 14, parent: 12, args: []string{"go", "build", "./..."}, own: 300},
	}
	writeProc(t, root, tree...)
	walk := NewWalk(root, Rules{})

	first := next(t, walk, rolesOf())
	wantCPU(t, first, Pair{RoleShared, KindShared}, 1)
	wantCPU(t, first, Pair{"implementer", KindAgent}, 2)
	wantCPU(t, first, Pair{"implementer", KindBuild}, 3)

	tree[0].own, tree[1].own, tree[3].own = 110, 205, 600
	writeProc(t, root, tree...)
	second := next(t, walk, rolesOf())
	wantCPU(t, second, Pair{RoleShared, KindShared}, 0.10)
	wantCPU(t, second, Pair{"implementer", KindAgent}, 0.05)
	wantCPU(t, second, Pair{"implementer", KindBuild}, 3)
}

// A pid the kernel handed to another process is another process: its starting moment differs from
// the one the walk recorded, so its counters are read as a new process's lifetime and never as a
// rise over the counters of the process that wore the number before it.
func TestAReusedPIDStartsAgainRatherThanRising(t *testing.T) {
	root := t.TempDir()
	launcher := fakeProcess{pid: 1, args: []string{"legion", "launcher"}, start: 10}
	writeProc(t, root,
		launcher,
		fakeProcess{pid: 7, parent: 1, args: []string{"go", "build", "./..."}, own: 5000, start: 4000},
	)
	walk := NewWalk(root, Rules{})
	first := next(t, walk, nil)
	wantCPU(t, first, Pair{RoleShared, KindBuild}, 50)

	writeProc(t, root,
		launcher,
		fakeProcess{pid: 7, parent: 1, args: []string{"gopls"}, own: 3, start: 9000},
	)
	second := next(t, walk, nil)
	if got := pairOf(t, second, 7); got != (Pair{RoleShared, KindTool}) {
		t.Errorf("the reused pid is %v, want the new process's own kind", got)
	}
	wantCPU(t, second, Pair{RoleShared, KindTool}, 0.03)
}

// The CPU a parent reaps for a child the walk saw belongs to that child's kind, and only the part
// above what the walk already charged it: the child was charged the CPU it had spent on every
// pass that read it, so charging the whole rise would count its life twice.
func TestAParentChargesAReapedChildsOwnKind(t *testing.T) {
	root := t.TempDir()
	tree := []fakeProcess{
		{pid: 1, args: []string{"legion", "launcher"}},
		{pid: 10, parent: 1, args: []string{"legion", "role-init", "--role", "implementer"}},
		{pid: 12, parent: 10, args: []string{"omp", "--mode", "rpc"}},
		{pid: 14, parent: 12, args: []string{"go", "build", "./..."}, own: 50},
	}
	writeProc(t, root, tree...)
	walk := NewWalk(root, Rules{})
	wantCPU(t, next(t, walk, rolesOf()), Pair{"implementer", KindBuild}, 0.50)

	// The build ended at 80 ticks, 30 of them after the pass that last read it, and the
	// harness reaped all 80.
	tree[2].reaped = 80
	writeProc(t, root, tree[:3]...)
	second := next(t, walk, rolesOf())
	wantCPU(t, second, Pair{"implementer", KindBuild}, 0.30)
	wantCPU(t, second, Pair{"implementer", KindTool}, 0)
	wantCPU(t, second, Pair{"implementer", KindAgent}, 0)
}

// Two children the walk saw ending between one pass and the next share what their parent reaped
// in proportion to the CPU each was last read with, since the kernel says nothing about which of
// them a rise came from.
func TestTwoReapedChildrenShareTheRiseInProportion(t *testing.T) {
	root := t.TempDir()
	tree := []fakeProcess{
		{pid: 1, args: []string{"legion", "launcher"}},
		{pid: 10, parent: 1, args: []string{"legion", "role-init", "--role", "implementer"}},
		{pid: 12, parent: 10, args: []string{"omp", "--mode", "rpc"}},
		{pid: 14, parent: 12, args: []string{"go", "build", "./..."}, own: 30},
		{pid: 16, parent: 12, args: []string{"go", "test", "./..."}, own: 10},
	}
	writeProc(t, root, tree...)
	walk := NewWalk(root, Rules{})
	next(t, walk, rolesOf())

	// Both ended; the harness reaped 60 ticks, 20 above the 40 the walk had charged them.
	tree[2].reaped = 60
	writeProc(t, root, tree[:3]...)
	second := next(t, walk, rolesOf())
	wantCPU(t, second, Pair{"implementer", KindBuild}, 0.15)
	wantCPU(t, second, Pair{"implementer", KindTest}, 0.05)
}

// A rise with no child the walk ever saw is the whole life of a child that began and ended
// between two passes. It goes to the parent's own kind — and to tool where the parent is the
// role's harness, which spends no second of its own on the compile it started
// (dispatch://INFRA-1208/spec#b-a-kinds).
func TestCPUFromAChildNoReadSawGoesToTheParentsKind(t *testing.T) {
	root := t.TempDir()
	tree := []fakeProcess{
		{pid: 1, args: []string{"legion", "launcher"}},
		{pid: 10, parent: 1, args: []string{"legion", "role-init", "--role", "implementer"}},
		{pid: 12, parent: 10, args: []string{"omp", "--mode", "rpc"}},
		{pid: 13, parent: 12, args: []string{"bash", "-lc", "make -j4"}},
	}
	writeProc(t, root, tree...)
	walk := NewWalk(root, Rules{})
	next(t, walk, rolesOf())

	tree[0].reaped = 25 // the launcher reaped what a `kubectl exec` left behind
	tree[2].reaped = 70 // the harness reaped a tool that lived between two passes
	tree[3].reaped = 40 // the shell reaped a compiler of its own
	writeProc(t, root, tree...)
	second := next(t, walk, rolesOf())
	wantCPU(t, second, Pair{RoleShared, KindShared}, 0.25)
	wantCPU(t, second, Pair{"implementer", KindBuild}, 0.40)
	wantCPU(t, second, Pair{"implementer", KindTool}, 0.70)
	wantCPU(t, second, Pair{"implementer", KindAgent}, 0)
}

// The roles file is read again on each pass, so a generation that ends between two passes gives
// its processes up to shared and one that starts is picked up at once, with no restart.
func TestEachPassTakesTheRolesFileAsItStandsNow(t *testing.T) {
	root := t.TempDir()
	launcher := fakeProcess{pid: 1, args: []string{"legion", "launcher"}}
	harness := fakeProcess{pid: 12, parent: 10, args: []string{"omp", "--mode", "rpc"}}
	writeProc(t, root, launcher,
		fakeProcess{pid: 10, parent: 1, args: []string{"legion", "role-init", "--role", "implementer"}},
		harness)
	walk := NewWalk(root, Rules{})
	if got := pairOf(t, next(t, walk, map[string]int{"implementer": 10}), 12); got != (Pair{"implementer", KindAgent}) {
		t.Errorf("the harness of the running generation is %v, want the role's agent", got)
	}

	// The generation ended: its init is gone, the launcher has the orphan, and the roles file
	// names no pid for the role.
	harness.parent = 1
	writeProc(t, root, launcher, harness)
	if got := pairOf(t, next(t, walk, map[string]int{"implementer": 0}), 12); got != (Pair{RoleShared, KindShared}) {
		t.Errorf("the orphan of an ended generation is %v, want shared", got)
	}

	// The next generation starts, and the pass that follows it reads the new pid.
	writeProc(t, root, launcher, harness,
		fakeProcess{pid: 30, parent: 1, args: []string{"legion", "role-init", "--role", "implementer"}},
		fakeProcess{pid: 31, parent: 30, args: []string{"omp", "--mode", "rpc"}})
	if got := pairOf(t, next(t, walk, map[string]int{"implementer": 30}), 31); got != (Pair{"implementer", KindAgent}) {
		t.Errorf("the harness of the new generation is %v, want the role's agent", got)
	}
}

// A process that ends between the pass's listing and its read is no failure: the pass passes over
// it and counts no error. A read that failed for any other reason is counted, since the sampler
// serves that count as legion_sampler_read_errors_total.
func TestAProcessThatEndsMidPassIsSkippedAndOtherFailuresAreCounted(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root,
		fakeProcess{pid: 1, args: []string{"legion", "launcher"}},
		fakeProcess{pid: 42, parent: 1, args: []string{"go", "build", "./..."}, gone: true},
	)
	walk := NewWalk(root, Rules{})
	pass := next(t, walk, nil)
	for _, process := range pass.Processes {
		if process.PID == 42 {
			t.Errorf("the pass reported process 42, which ended before it was read")
		}
	}
	if got := walk.ReadErrors(); got != 0 {
		t.Errorf("the walk counted %d read errors, want none for a process that ended", got)
	}

	writeProc(t, root,
		fakeProcess{pid: 1, args: []string{"legion", "launcher"}},
		fakeProcess{pid: 43, parent: 1, args: []string{"gopls"}, unparsable: true},
	)
	if pass := next(t, walk, nil); len(pass.Processes) != 1 {
		t.Errorf("the pass reported %d processes, want the one it could read", len(pass.Processes))
	}
	if got := walk.ReadErrors(); got != 1 {
		t.Errorf("the walk counted %d read errors, want the one stat it could not parse", got)
	}
}

// The walk forgets a process once it is gone, so what it holds between passes is bounded by the
// processes that exist and not by every process the pod has ever run.
func TestTheWalkForgetsAProcessOnceItIsGone(t *testing.T) {
	root := t.TempDir()
	launcher := fakeProcess{pid: 1, args: []string{"legion", "launcher"}}
	short := make([]fakeProcess, 0, 51)
	short = append(short, launcher)
	for pid := 2; pid <= 51; pid++ {
		short = append(short, fakeProcess{pid: pid, parent: 1, args: []string{"gopls"}, own: 1})
	}
	walk := NewWalk(root, Rules{})
	writeProc(t, root, short...)
	next(t, walk, nil)
	if len(walk.last) != len(short) {
		t.Fatalf("the walk holds %d readings, want one of each of the %d processes", len(walk.last), len(short))
	}

	writeProc(t, root, launcher)
	next(t, walk, nil)
	if len(walk.last) != 1 {
		t.Errorf("the walk holds %d readings after fifty processes ended, want the one still there", len(walk.last))
	}
}
