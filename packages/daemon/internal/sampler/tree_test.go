package sampler

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/procread"
	"github.com/sjawhar/legion/daemon/internal/samplerwire"
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
// directory per process with the stat line the kernel writes and the NUL-separated cmdline. A
// second call is the next pass over a tree that has moved on — a process it does not name is
// taken away, and one it names again is written where it stands.
//
// The files of a process that is still there are rewritten and never replaced, because that is
// what the kernel does to them: a reader holding the descriptor of /proc/<pid>/stat reads what
// the kernel has written into it since, and a fixture that unlinked the file and wrote another
// in its place would hand that reader the line of a pass that is over (procread.Reader).
//
// No test reads the host's /proc: every number below is one the test wrote.
func writeProc(t *testing.T, root string, processes ...fakeProcess) {
	t.Helper()
	dir := filepath.Join(root, "proc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("could not write the fixture's /proc: %v", err)
	}
	live := make(map[string]bool, len(processes))
	for _, process := range processes {
		live[strconv.Itoa(process.pid)] = true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("could not read the fixture's /proc: %v", err)
	}
	for _, entry := range entries {
		// Only the processes are taken away: /proc holds meminfo beside them, which a
		// pass of its own writes.
		if _, numeric := strconv.Atoi(entry.Name()); numeric != nil || live[entry.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			t.Fatalf("could not take process %s away: %v", entry.Name(), err)
		}
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
		stat := filepath.Join(pidDir, "stat")
		if process.gone {
			if err := os.Remove(stat); err != nil && !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("could not take the stat of process %d away: %v", process.pid, err)
			}
			continue
		}
		line := statLine(process)
		if process.unparsable {
			line = "nothing a reader of /proc can parse\n"
		}
		if err := os.WriteFile(stat, []byte(line), 0o644); err != nil {
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

// testWalk is a walk of the fixture's own /proc with the attribution an issue pod's collector
// gives it, and the reads it reported as failed: a collector counts those as
// legion_sampler_read_errors_total{source="proc"}, and a test of the walk alone holds them here.
type testWalk struct {
	*Walk
	attribution *podAttribution
	failures    []error
}

// newWalk is that walk, reading through a reader of its own whose descriptors the test releases
// when it ends — the reader a collector hands its walk.
func newWalk(t *testing.T, root string, rules samplerwire.Rules) *testWalk {
	t.Helper()
	reader := procread.NewReader(root, procread.DetectLayout(root))
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Errorf("the walk's reader could not be closed: %v", err)
		}
	})
	walk := &testWalk{}
	walk.attribution = newPodAttribution(root, rules, walk.failed)
	walk.Walk = NewWalk(root, reader, walk.attribution, walk.failed)
	return walk
}

func (w *testWalk) failed(_ ReadSource, err error) { w.failures = append(w.failures, err) }

// next is one pass over the fixture, with the roles file as it stands for that pass, which must
// never fail: a pass fails only where /proc itself cannot be listed.
func next(t *testing.T, walk *testWalk, roles map[string]int) Pass {
	t.Helper()
	walk.attribution.roles = roles
	pass, err := walk.Next()
	if err != nil {
		t.Fatalf("the pass failed: %v", err)
	}
	return pass
}

// settled is the second pass over a tree that has not moved: the pass that has read every
// process's command line, and so the one whose kinds are the kinds docs/sampler-metrics.md
// describes. The pass that first sees a process reads no command line of it and hands it its
// nearest ancestor's kind (Walk.Next), which the two tests below are about and the rest take as
// read.
func settled(t *testing.T, walk *testWalk, roles map[string]int) Pass {
	t.Helper()
	next(t, walk, roles)
	return next(t, walk, roles)
}

// sighted is this tree as the pass that first sees it finds it: every counter at zero. A test
// whose numbers are a settled pod's writes this for that pass and the tree itself for the pass
// it asserts on, which then charges the whole of each counter as the rise it is.
func sighted(processes []fakeProcess) []fakeProcess {
	sighting := slices.Clone(processes)
	for i := range sighting {
		sighting[i].own, sighting[i].reaped = 0, 0
	}
	return sighting
}

// cmdlineReads records the pid of every process a pass reads a command line for, in the order it
// read them, and answers the list. It is what says that the pass which first sees a process
// reads none of it: the kinds say what a pass decided, and this says what it cost.
func cmdlineReads(t *testing.T) *[]int {
	t.Helper()
	before := readCmdline
	read := &[]int{}
	readCmdline = func(root string, pid int) ([]string, error) {
		*read = append(*read, pid)
		return before(root, pid)
	}
	t.Cleanup(func() { readCmdline = before })
	return read
}

// workOf is the owner and kind the pass gave this process, and a failure where the pass did not
// report it at all.
func workOf(t *testing.T, pass Pass, pid int) Work {
	t.Helper()
	for _, process := range pass.Processes {
		if process.PID == pid {
			return process.Work
		}
	}
	t.Fatalf("the pass did not report process %d", pid)
	return Work{}
}

// wantCPU fails unless the pass charged this work these seconds. The comparison allows the last
// bits of a division, since a reaped rise is split in proportion and not in whole ticks.
func wantCPU(t *testing.T, pass Pass, work Work, want float64) {
	t.Helper()
	if got := pass.CPUSeconds[work]; math.Abs(got-want) > 1e-9 {
		t.Errorf("the pass charged %v %g seconds, want %g", work, got, want)
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
//
// These are the kinds of a settled pod, which is the second pass over a tree: the pass that
// first sees a process reads no command line of it, and the two tests below this file's rule
// tests say what that pass gives instead.
func TestAPassGivesEveryProcessItsRoleAndItsKind(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, rolesTree()...)
	pass := settled(t, newWalk(t, root, samplerwire.Rules{}), rolesOf())

	for _, test := range []struct {
		pid  int
		want Work
		why  string
	}{
		{1, Work{RoleShared, samplerwire.KindShared}, "the launcher is in no role's subtree"},
		{2, Work{RoleShared, samplerwire.KindShared}, "the sampler is its own shared process"},
		{3, Work{RoleShared, samplerwire.KindShared}, "the test Postgres is shared"},
		{4, Work{RoleShared, samplerwire.KindBuild}, "a stray build a person left behind is still a build"},
		{5, Work{RoleShared, samplerwire.KindBuild}, "and so is the compiler that build started"},
		{10, Work{"implementer", samplerwire.KindAgent}, "the role-init process is the role's own"},
		{11, Work{"implementer", samplerwire.KindAgent}, "and so is the worker-shim it runs"},
		{12, Work{"implementer", samplerwire.KindAgent}, "and the harness the shim runs"},
		{13, Work{"implementer", samplerwire.KindTool}, "a shell the harness started that no rule names"},
		{14, Work{"implementer", samplerwire.KindBuild}, "the build that shell started"},
		{15, Work{"implementer", samplerwire.KindBuild}, "and the compiler the build started"},
		{16, Work{"implementer", samplerwire.KindTool}, "a language server is a tool"},
		{17, Work{"implementer", samplerwire.KindTest}, "a shell told to test and then build is a test run"},
		{18, Work{"implementer", samplerwire.KindTest}, "and the linker that test run started"},
		{20, Work{"reviewer", samplerwire.KindAgent}, "the second role's init"},
		{21, Work{"reviewer", samplerwire.KindAgent}, "the Claude harness of that role"},
		{22, Work{"reviewer", samplerwire.KindTool}, "and what that harness ran"},
	} {
		if got := workOf(t, pass, test.pid); got != test.want {
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

// promptTree is this issue's own pod as its /proc held it: a role whose harness is an `omp`
// carrying the whole system prompt as one argument, and under it a shell and the Python that
// shell started. The prompt is prose, and like every agent's prompt it names the toolchain the
// role is told to run — which is the one thing about this tree that matters.
func promptTree() []fakeProcess {
	prompt := "You are Legion's implementer for INFRA-1208. Build the daemon with " +
		"go build ./cmd/legion before you hand the work on, and read " +
		"docs/sampler-metrics.md, which is the contract its series answer to."
	return []fakeProcess{
		{pid: 1, parent: 0, args: []string{"/opt/legion/bin/legion", "launcher"}},
		{pid: 10, parent: 1, args: []string{"/opt/legion/bin/legion", "role-init", "--role", "implementer"}},
		{pid: 12, parent: 10, own: 100, args: []string{
			"/opt/omp/bin/omp", "--extension", "@sjawhar/pi-legion", "--mode", "rpc",
			"--append-system-prompt", prompt,
		}},
		{pid: 13, parent: 12, own: 200, args: []string{"bash", "-l"}},
		{pid: 14, parent: 13, own: 300, args: []string{"python3", "hold.py"}},
	}
}

// An agent's own command line says nothing about the work its children do. The harness of a role
// carries the whole system prompt as one argument, that prose names the toolchain the role is
// told to run, and a sampler that read those words as a command counted every process of the
// role as a build: `legion_sampler_cpu_seconds_total{kind="build"}` rose 802.96 s against a build
// that spent 390.2 s, and `kind="tool"` never moved at all (INFRA-1208, measured in this issue's
// own pod on 2026-11-05).
//
// So the prompt matches no rule, and the climb that looks for one stops at the harness without
// taking its kind: the harness is `agent`, what it started is `tool` until a rule says otherwise
// (dispatch://INFRA-1208/spec#b-a-kinds), and the build counter of a role that built nothing
// stays at zero.
func TestAHarnessPromptNamingAToolchainIsNoBuild(t *testing.T) {
	root := t.TempDir()
	walk := newWalk(t, root, samplerwire.Rules{})
	// The first pass of a walk's life charges nothing, so the tree it finds has spent
	// nothing yet and the pass after it charges the whole of each counter as the rise it is
	// (Walk.Next).
	writeProc(t, root, sighted(promptTree())...)
	next(t, walk, rolesOf())
	writeProc(t, root, promptTree()...)
	pass := next(t, walk, rolesOf())

	for _, test := range []struct {
		pid  int
		want Work
		why  string
	}{
		{12, Work{"implementer", samplerwire.KindAgent}, "the harness is the role's own machinery"},
		{13, Work{"implementer", samplerwire.KindTool}, "a shell under the harness that no rule names"},
		{14, Work{"implementer", samplerwire.KindTool}, "and what that shell started"},
	} {
		if got := workOf(t, pass, test.pid); got != test.want {
			t.Errorf("process %d is %v, want %v: %s", test.pid, got, test.want, test.why)
		}
	}
	wantCPU(t, pass, Work{"implementer", samplerwire.KindBuild}, 0)
	wantCPU(t, pass, Work{"implementer", samplerwire.KindTool}, 5)
	wantCPU(t, pass, Work{"implementer", samplerwire.KindAgent}, 1)
}

// A real build under the same harness is still a build, and it is the only one: the words of the
// harness's prompt count for nothing, and the seconds the build and its compiler spend are the
// whole of what the build counter holds.
func TestOnlyTheRealBuildUnderAPromptingHarnessIsABuild(t *testing.T) {
	root := t.TempDir()
	tree := append(promptTree(),
		fakeProcess{pid: 40, parent: 12, own: 400, args: []string{"bash", "-c", "go build ./..."}},
		fakeProcess{pid: 41, parent: 40, own: 500, args: []string{"/usr/lib/go/pkg/tool/linux_amd64/compile", "-o", "a.out"}},
	)
	walk := newWalk(t, root, samplerwire.Rules{})
	// The pass that first sees these processes reads no command line of any of them, so the
	// tree it finds has spent nothing yet and the pass that reads them charges the whole of
	// what it finds.
	writeProc(t, root, sighted(tree)...)
	next(t, walk, rolesOf())
	writeProc(t, root, tree...)
	pass := next(t, walk, rolesOf())

	for _, test := range []struct {
		pid  int
		want Work
		why  string
	}{
		{13, Work{"implementer", samplerwire.KindTool}, "the shell beside the build is no part of it"},
		{14, Work{"implementer", samplerwire.KindTool}, "nor is what that shell started"},
		{40, Work{"implementer", samplerwire.KindBuild}, "a shell told to build is a build"},
		{41, Work{"implementer", samplerwire.KindBuild}, "and so is the compiler it started"},
	} {
		if got := workOf(t, pass, test.pid); got != test.want {
			t.Errorf("process %d is %v, want %v: %s", test.pid, got, test.want, test.why)
		}
	}
	wantCPU(t, pass, Work{"implementer", samplerwire.KindBuild}, 9)
	wantCPU(t, pass, Work{"implementer", samplerwire.KindTool}, 5)
}

// A shell copied to another name is not a shell this engine knows, so the command its -c takes is
// one word and no rule reads a toolchain out of it. `burn` was a copy of the pod's own shell
// spinning on the CPU for twenty seconds, and the sampler charged every one of them to `build`
// (INFRA-1208, measured 2026-11-05); it is a tool, which is what a child of the harness that no
// rule names is.
func TestAShellCopiedToAnotherNameUnderTheHarnessIsATool(t *testing.T) {
	root := t.TempDir()
	tree := append(promptTree(),
		fakeProcess{pid: 30, parent: 12, own: 1940, args: []string{"./burn", "-c", "while :; do :; done"}},
	)
	walk := newWalk(t, root, samplerwire.Rules{})
	// The first pass of a walk's life charges nothing, so the burn loop's seconds are the
	// rise the pass after it charges (Walk.Next).
	writeProc(t, root, sighted(tree)...)
	next(t, walk, rolesOf())
	writeProc(t, root, tree...)
	pass := next(t, walk, rolesOf())

	if got, want := workOf(t, pass, 30), (Work{"implementer", samplerwire.KindTool}); got != want {
		t.Errorf("the burn loop is %v, want %v", got, want)
	}
	wantCPU(t, pass, Work{"implementer", samplerwire.KindBuild}, 0)
	wantCPU(t, pass, Work{"implementer", samplerwire.KindTool}, 24.4)
}

// harnessTree is the three processes a role's work hangs under, none of them spending anything:
// the launcher that is the container's process 1, the role's init, and the harness it runs.
func harnessTree() []fakeProcess {
	return []fakeProcess{
		{pid: 1, args: []string{"/opt/legion/bin/legion", "launcher"}},
		{pid: 10, parent: 1, args: []string{"/opt/legion/bin/legion", "role-init", "--role", "implementer"}},
		{pid: 12, parent: 10, args: []string{"omp", "--mode", "rpc"}},
	}
}

// A process the walk sees on one pass only is never read for its command line at all. A build
// makes thousands of processes that are new at one pass and gone by the next, and under this
// pod's gVisor the open of a cmdline costs what the open of the stat beside it costs: the read
// is the sampler's largest avoidable cost over a busy pod, and for a process that will not be
// there to charge again it buys nothing (INFRA-1208, measured 2026-11-05).
//
// So the pass that first sees a process gives it the kind of its nearest ancestor that has one,
// exactly as it gives that kind to a process whose own command line matches no rule. Here that
// ancestor is the harness, so the `go build` of one pass is charged `tool` and the build counter
// of the role never moves.
func TestAProcessOfOnePassIsNeverReadForItsCommandLine(t *testing.T) {
	root := t.TempDir()
	above := harnessTree()
	walk := newWalk(t, root, samplerwire.Rules{})
	read := cmdlineReads(t)

	writeProc(t, root, above...)
	next(t, walk, rolesOf())
	if len(*read) != 0 {
		t.Errorf("the pass that first saw the tree read %v for a command line, want none of them", *read)
	}

	// A build appears, spends three seconds and is gone before the next pass, which is what
	// the harness's reap of its whole life says.
	compile := fakeProcess{pid: 50, parent: 12, own: 300, args: []string{"go", "build", "./..."}}
	writeProc(t, root, append(slices.Clone(above), compile)...)
	appeared := next(t, walk, rolesOf())
	if got, want := workOf(t, appeared, 50), (Work{"implementer", samplerwire.KindTool}); got != want {
		t.Errorf("the build on the pass that first saw it is %v, want %v", got, want)
	}
	wantCPU(t, appeared, Work{"implementer", samplerwire.KindTool}, 3)
	wantCPU(t, appeared, Work{"implementer", samplerwire.KindBuild}, 0)
	if want := []int{1, 10, 12}; !slices.Equal(*read, want) {
		t.Errorf("the pass read %v for a command line, want the %v it had seen before", *read, want)
	}

	above[2].reaped = 300
	writeProc(t, root, above...)
	ended := next(t, walk, rolesOf())
	wantCPU(t, ended, Work{"implementer", samplerwire.KindBuild}, 0)
	wantCPU(t, ended, Work{"implementer", samplerwire.KindTool}, 0)
	if want := []int{1, 10, 12}; !slices.Equal(*read, want) {
		t.Errorf("the command lines read are %v: process 50 lived one pass and %v was read for it", *read, want)
	}
}

// A process that is still there on the next pass is read then, and the kind its own command line
// gives it is the kind it carries from that pass on. What the pass that first saw it charged
// stays where it was charged: the kind a pass gives a process is the kind that pass charges it
// to, and the reading that corrects it corrects nothing already counted.
func TestAProcessThatSurvivesIsReadOnTheNextPassAndTakesItsOwnRule(t *testing.T) {
	root := t.TempDir()
	above := harnessTree()
	walk := newWalk(t, root, samplerwire.Rules{})
	read := cmdlineReads(t)

	writeProc(t, root, above...)
	next(t, walk, rolesOf())

	build := fakeProcess{pid: 50, parent: 12, own: 300, args: []string{"go", "build", "./..."}}
	writeProc(t, root, append(slices.Clone(above), build)...)
	appeared := next(t, walk, rolesOf())
	wantCPU(t, appeared, Work{"implementer", samplerwire.KindTool}, 3)
	wantCPU(t, appeared, Work{"implementer", samplerwire.KindBuild}, 0)

	// The build is still running on the next pass, which reads its command line: the two
	// seconds it has spent since are a build's, and so is every second it spends after them.
	build.own = 500
	writeProc(t, root, append(slices.Clone(above), build)...)
	settled := next(t, walk, rolesOf())
	if got, want := workOf(t, settled, 50), (Work{"implementer", samplerwire.KindBuild}); got != want {
		t.Errorf("the build on the pass that read it is %v, want %v", got, want)
	}
	wantCPU(t, settled, Work{"implementer", samplerwire.KindBuild}, 2)
	wantCPU(t, settled, Work{"implementer", samplerwire.KindTool}, 0)
	if want := []int{1, 10, 12, 50}; !slices.Equal(*read, want) {
		t.Errorf("the command lines read are %v, want %v: each on the pass after the one that first saw it", *read, want)
	}

	build.own = 800
	writeProc(t, root, append(slices.Clone(above), build)...)
	wantCPU(t, next(t, walk, rolesOf()), Work{"implementer", samplerwire.KindBuild}, 3)
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
	pass := next(t, newWalk(t, root, samplerwire.Rules{}), nil)

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

// The first pass of a walk's life charges nothing at all, and every pass after it charges the
// rise alone: the CPU /proc already holds for a process is work this walk never watched, and
// nothing in those two counters says which kind of work it was (Walk.Next). So every counter of
// a sampler starts at zero and a restart is a counter reset, which Prometheus reads correctly.
func TestTheFirstPassChargesNothingAndTheNextChargesTheRise(t *testing.T) {
	root := t.TempDir()
	tree := []fakeProcess{
		{pid: 1, args: []string{"legion", "launcher"}, own: 100},
		{pid: 10, parent: 1, args: []string{"legion", "role-init", "--role", "implementer"}, own: 200},
		{pid: 12, parent: 10, args: []string{"omp", "--mode", "rpc"}, own: 0},
		{pid: 14, parent: 12, args: []string{"go", "build", "./..."}, own: 300},
	}
	writeProc(t, root, tree...)
	walk := newWalk(t, root, samplerwire.Rules{})

	// Every process of the tree is reported, with the role and the kind the pass gives it,
	// and the six seconds they had spent before this walk began are charged to nobody.
	first := next(t, walk, rolesOf())
	if len(first.Processes) != len(tree) {
		t.Errorf("the first pass reported %d processes, want the %d of the tree", len(first.Processes), len(tree))
	}
	for _, work := range []Work{
		{RoleShared, samplerwire.KindShared},
		{"implementer", samplerwire.KindAgent},
		{"implementer", samplerwire.KindTool},
		{"implementer", samplerwire.KindBuild},
	} {
		wantCPU(t, first, work, 0)
	}

	tree[0].own, tree[1].own, tree[3].own = 110, 205, 600
	writeProc(t, root, tree...)
	second := next(t, walk, rolesOf())
	wantCPU(t, second, Work{RoleShared, samplerwire.KindShared}, 0.10)
	wantCPU(t, second, Work{"implementer", samplerwire.KindAgent}, 0.05)
	wantCPU(t, second, Work{"implementer", samplerwire.KindBuild}, 3)
}

// A sampler that restarts charges no part of the role's history to the wrong kind. The pod runs
// on while the sampler is replaced — the 64 MiB exit, a crash, an OOM kill — and the walk of the
// new sampler meets a `/proc` holding every second the role has spent: the harness's reaped
// cutime + cstime, which is every build and test it has ever waited for, and the own CPU of
// whatever is running now.
//
// A fresh walk holds no reading of any child, so charging the harness's reaped rise made all of
// it look like the CPU of a child no read saw and sent it to `tool` in one pass, while a live
// build's own lifetime went to the kind of its nearest ancestor, which under a harness is `tool`
// as well: the role below had spent 2.00 on a build and 3.00 on a test with `tool` at zero, and
// the first pass of the new walk charged the whole 5.00 to `tool` (INFRA-1208, the review of
// pull request 2786). Neither counter says which kind the work was, so the first pass of the
// new walk charges neither, and what the role spends after the restart is charged where it
// belongs.
func TestARestartChargesNoneOfTheRolesHistoryToAnotherKind(t *testing.T) {
	root := t.TempDir()
	above := harnessTree()
	build := fakeProcess{pid: 50, parent: 12, args: []string{"go", "build", "./..."}, own: 200}
	ended := fakeProcess{pid: 51, parent: 12, args: []string{"go", "test", "./..."}, own: 300}

	// The sampler before the restart watched both commands and charged every second of them
	// where it belonged.
	walk := newWalk(t, root, samplerwire.Rules{})
	writeProc(t, root, sighted(append(slices.Clone(above), build, ended))...)
	next(t, walk, rolesOf())
	writeProc(t, root, append(slices.Clone(above), build, ended)...)
	watched := next(t, walk, rolesOf())
	wantCPU(t, watched, Work{"implementer", samplerwire.KindBuild}, 2)
	wantCPU(t, watched, Work{"implementer", samplerwire.KindTest}, 3)
	wantCPU(t, watched, Work{"implementer", samplerwire.KindTool}, 0)

	// The test run ends and the harness reaps its three seconds, which the walk has charged
	// already; the build runs on.
	above[2].reaped = 300
	writeProc(t, root, append(slices.Clone(above), build)...)
	reaped := next(t, walk, rolesOf())
	wantCPU(t, reaped, Work{"implementer", samplerwire.KindTool}, 0)
	wantCPU(t, reaped, Work{"implementer", samplerwire.KindTest}, 0)

	// The sampler is replaced: a walk of its own over the same /proc, where the harness still
	// reports the three seconds it reaped and the build its own two.
	restarted := newWalk(t, root, samplerwire.Rules{})
	after := next(t, restarted, rolesOf())
	for _, work := range []Work{
		{"implementer", samplerwire.KindTool},
		{"implementer", samplerwire.KindBuild},
		{"implementer", samplerwire.KindTest},
		{"implementer", samplerwire.KindAgent},
		{RoleShared, samplerwire.KindShared},
	} {
		wantCPU(t, after, work, 0)
	}

	// What the role spends after the restart is charged to the kind the new walk reads, and
	// the harness's next reap answers for the build the new walk did watch.
	build.own = 500
	writeProc(t, root, append(slices.Clone(above), build)...)
	spent := next(t, restarted, rolesOf())
	wantCPU(t, spent, Work{"implementer", samplerwire.KindBuild}, 3)
	wantCPU(t, spent, Work{"implementer", samplerwire.KindTool}, 0)

	above[2].reaped = 800
	writeProc(t, root, above...)
	last := next(t, restarted, rolesOf())
	wantCPU(t, last, Work{"implementer", samplerwire.KindBuild}, 0)
	wantCPU(t, last, Work{"implementer", samplerwire.KindTool}, 0)
}

// A pid the kernel handed to another process is another process: its starting moment differs from
// the one the walk recorded, so its counters are read as a new process's lifetime and never as a
// rise over the counters of the process that wore the number before it.
func TestAReusedPIDStartsAgainRatherThanRising(t *testing.T) {
	root := t.TempDir()
	launcher := fakeProcess{pid: 1, args: []string{"legion", "launcher"}, start: 10}
	build := fakeProcess{pid: 7, parent: 1, args: []string{"go", "build", "./..."}, start: 4000}
	walk := newWalk(t, root, samplerwire.Rules{})
	writeProc(t, root, launcher, build)
	next(t, walk, nil)

	build.own = 5000
	writeProc(t, root, launcher, build)
	wantCPU(t, next(t, walk, nil), Work{RoleShared, samplerwire.KindBuild}, 50)

	// The kernel hands the number on. The pass that meets the new process charges the three
	// ticks it has spent and nothing of the fifty seconds the build spent under the same pid,
	// and gives it the kind of the launcher above it until a pass reads its command line.
	writeProc(t, root,
		launcher,
		fakeProcess{pid: 7, parent: 1, args: []string{"gopls"}, own: 3, start: 9000},
	)
	second := next(t, walk, nil)
	if got := workOf(t, second, 7); got != (Work{RoleShared, samplerwire.KindShared}) {
		t.Errorf("the reused pid is %v, want the kind of the process above it", got)
	}
	wantCPU(t, second, Work{RoleShared, samplerwire.KindShared}, 0.03)

	third := next(t, walk, nil)
	if got := workOf(t, third, 7); got != (Work{RoleShared, samplerwire.KindTool}) {
		t.Errorf("the reused pid is %v once read, want the new process's own kind", got)
	}
	wantCPU(t, third, Work{RoleShared, samplerwire.KindTool}, 0)
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
	walk := newWalk(t, root, samplerwire.Rules{})
	// The build's own kind is read on the pass after the one that first saw it, so the tree
	// that pass finds has spent nothing yet and the 50 ticks are the rise the next one
	// charges — to `build`, which is the kind the reap below is answered under.
	writeProc(t, root, sighted(tree)...)
	next(t, walk, rolesOf())
	writeProc(t, root, tree...)
	wantCPU(t, next(t, walk, rolesOf()), Work{"implementer", samplerwire.KindBuild}, 0.50)

	// The build ended at 80 ticks, 30 of them after the pass that last read it, and the
	// harness reaped all 80.
	tree[2].reaped = 80
	writeProc(t, root, tree[:3]...)
	second := next(t, walk, rolesOf())
	wantCPU(t, second, Work{"implementer", samplerwire.KindBuild}, 0.30)
	wantCPU(t, second, Work{"implementer", samplerwire.KindTool}, 0)
	wantCPU(t, second, Work{"implementer", samplerwire.KindAgent}, 0)
}

// A build is two processes and not one: the shell the agent started,
// `bash -c 'time go build …'`, and the `go` it runs, which reaps every compile of the build.
// Both of them end between one pass and the next, and the harness above them reaps the shell's
// whole accounting — the shell's own CPU, the `go`, and every compile the `go` reaped — in that
// one rise.
//
// The pod measured on 2026-11-05 charged all of it a second time. The walk's record of the shell
// held the shell's own two counters as the last pass read them, and both were nothing: the shell
// spends no CPU and had reaped nothing until the moment it ended. So almost the whole rise looked
// like CPU no series had counted, and `legion_sampler_cpu_seconds_total{kind="build"}` went from
// 402.20 s to 805.87 s in the one pass after a build that spent 403.63 s.
//
// What a reap answers for is everything the walk has already charged below the child that
// vanished, which is what reading.charged keeps: the shell's own CPU plus that, and the remainder
// alone is new.
func TestABuildReapedWithItsShellIsChargedOnce(t *testing.T) {
	root := t.TempDir()
	// The ticks below are the real run's shape in miniature: the shell spends almost nothing,
	// the `go` spends its own CPU and reaps a compile at a time, and the whole subtree spends
	// 237 ticks before the two of them end together.
	above := []fakeProcess{
		{pid: 1, args: []string{"/opt/legion/bin/legion", "launcher"}},
		{pid: 10, parent: 1, args: []string{"/opt/legion/bin/legion", "role-init", "--role", "implementer"}},
		{pid: 12, parent: 10, args: []string{"omp", "--mode", "rpc"}},
	}
	shell := fakeProcess{pid: 50, parent: 12, own: 1, args: []string{"bash", "-c", "time go build -a -o /dev/null ./..."}}
	driver := fakeProcess{pid: 51, parent: 50, own: 40, args: []string{"go", "build", "-a", "-o", "/dev/null", "./..."}}
	compile := fakeProcess{pid: 60, parent: 51, own: 60, args: []string{"/usr/lib/go/pkg/tool/linux_amd64/compile", "-o", "a.out"}}
	build := Work{"implementer", samplerwire.KindBuild}
	walk := newWalk(t, root, samplerwire.Rules{})
	var charged float64

	// The pass that first sees the three of them reads no command line, so it finds a build
	// that has spent nothing and the pass after it charges the whole 101 ticks as the rise
	// they are.
	writeProc(t, root, sighted(append(slices.Clone(above), shell, driver, compile))...)
	next(t, walk, rolesOf())
	writeProc(t, root, append(slices.Clone(above), shell, driver, compile)...)
	first := next(t, walk, rolesOf())
	wantCPU(t, first, build, 1.01)
	charged += first.CPUSeconds[build]

	// The first compile ended at 70 ticks, ten of them after the pass that read it, and the
	// `go` reaped it and started another. Those ten ticks and both processes' own rises are
	// charged through the `go`, which is where the walk records them.
	driver.own, driver.reaped = 100, 70
	later := fakeProcess{pid: 61, parent: 51, own: 50, args: []string{"/usr/lib/go/pkg/tool/linux_amd64/compile", "-o", "b.out"}}
	writeProc(t, root, append(slices.Clone(above), shell, driver, later)...)
	second := next(t, walk, rolesOf())
	wantCPU(t, second, build, 1.20)
	charged += second.CPUSeconds[build]

	// The build ends: the second compile at 55 ticks, the `go` at 110 of its own, the shell at
	// 2, and the harness reaps the 237 ticks of the whole subtree at once. Of those, 221 are
	// the shell's own CPU and what the walk has charged below it, so 16 are new.
	above[2].reaped = 237
	writeProc(t, root, above...)
	third := next(t, walk, rolesOf())
	wantCPU(t, third, build, 0.16)
	wantCPU(t, third, Work{"implementer", samplerwire.KindTool}, 0)
	wantCPU(t, third, Work{"implementer", samplerwire.KindAgent}, 0)
	charged += third.CPUSeconds[build]

	// The three passes together charged the CPU the tree really spent: the shell's 2 ticks,
	// the `go`'s 110 and the two compiles' 70 and 55.
	if want := float64(2+110+70+55) / 100; math.Abs(charged-want) > 1e-9 {
		t.Errorf("the three passes charged the build %g seconds, want the %g the tree spent", charged, want)
	}
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
	walk := newWalk(t, root, samplerwire.Rules{})
	// Both children are read on the pass after the one that first saw them, and the kind each
	// is read with is the kind its share of the reap below is charged to.
	writeProc(t, root, sighted(tree)...)
	next(t, walk, rolesOf())
	writeProc(t, root, tree...)
	next(t, walk, rolesOf())

	// Both ended; the harness reaped 60 ticks, 20 above the 40 the walk had charged them.
	tree[2].reaped = 60
	writeProc(t, root, tree[:3]...)
	second := next(t, walk, rolesOf())
	wantCPU(t, second, Work{"implementer", samplerwire.KindBuild}, 0.15)
	wantCPU(t, second, Work{"implementer", samplerwire.KindTest}, 0.05)
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
	walk := newWalk(t, root, samplerwire.Rules{})
	next(t, walk, rolesOf())

	tree[0].reaped = 25 // the launcher reaped what a `kubectl exec` left behind
	tree[2].reaped = 70 // the harness reaped a tool that lived between two passes
	tree[3].reaped = 40 // the shell reaped a compiler of its own
	writeProc(t, root, tree...)
	second := next(t, walk, rolesOf())
	wantCPU(t, second, Work{RoleShared, samplerwire.KindShared}, 0.25)
	wantCPU(t, second, Work{"implementer", samplerwire.KindBuild}, 0.40)
	wantCPU(t, second, Work{"implementer", samplerwire.KindTool}, 0.70)
	wantCPU(t, second, Work{"implementer", samplerwire.KindAgent}, 0)
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
	walk := newWalk(t, root, samplerwire.Rules{})
	if got := workOf(t, next(t, walk, map[string]int{"implementer": 10}), 12); got != (Work{"implementer", samplerwire.KindAgent}) {
		t.Errorf("the harness of the running generation is %v, want the role's agent", got)
	}

	// The generation ended: its init is gone, the launcher has the orphan, and the roles file
	// names no pid for the role.
	harness.parent = 1
	writeProc(t, root, launcher, harness)
	if got := workOf(t, next(t, walk, map[string]int{"implementer": 0}), 12); got != (Work{RoleShared, samplerwire.KindShared}) {
		t.Errorf("the orphan of an ended generation is %v, want shared", got)
	}

	// The next generation starts, and the pass that follows it reads the new pid.
	writeProc(t, root, launcher, harness,
		fakeProcess{pid: 30, parent: 1, args: []string{"legion", "role-init", "--role", "implementer"}},
		fakeProcess{pid: 31, parent: 30, args: []string{"omp", "--mode", "rpc"}})
	if got := workOf(t, next(t, walk, map[string]int{"implementer": 30}), 31); got != (Work{"implementer", samplerwire.KindAgent}) {
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
	walk := newWalk(t, root, samplerwire.Rules{})
	pass := next(t, walk, nil)
	for _, process := range pass.Processes {
		if process.PID == 42 {
			t.Errorf("the pass reported process 42, which ended before it was read")
		}
	}
	if got := len(walk.failures); got != 0 {
		t.Errorf("the walk counted %d read errors, want none for a process that ended", got)
	}

	writeProc(t, root,
		fakeProcess{pid: 1, args: []string{"legion", "launcher"}},
		fakeProcess{pid: 43, parent: 1, args: []string{"gopls"}, unparsable: true},
	)
	if pass := next(t, walk, nil); len(pass.Processes) != 1 {
		t.Errorf("the pass reported %d processes, want the one it could read", len(pass.Processes))
	}
	if got := len(walk.failures); got != 1 {
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
	walk := newWalk(t, root, samplerwire.Rules{})
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
