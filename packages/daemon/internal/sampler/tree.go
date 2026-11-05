package sampler

import (
	"cmp"
	"errors"
	"io/fs"
	"os"
	"path"
	"slices"

	"github.com/sjawhar/legion/daemon/internal/procread"
)

// RoleShared is the role of a process in no role's subtree: the launcher that is the container's
// process 1, the sampler itself, the test Postgres, and whatever a `kubectl exec` left behind for
// the launcher to reap. dispatch://INFRA-1208/spec#b-a-sampler gives those "role and kind
// `shared`", and the split must hold everything the pod's totals hold, so a process no role
// claims is counted here rather than dropped.
const RoleShared = "shared"

// agentPrograms are Legion's own programs in a role: the `legion` of its worker-shim and its
// claude-bridge, and the harness either of them runs. A process of a role running one of these is
// the role's own machinery rather than work the role started, which is what KindAgent counts, and
// no rule can tell that — the same program outside every role's subtree is a shared process and
// not an agent, so what decides is where it sits and not what it is called.
//
// A process is read against this list twice: by the Comm of its stat, which the pass that first
// sees it already holds, and by the program of its command line on the pass that reads one. The
// two differ where a program renames itself — this pod's `omp daemon broker` reads Comm
// `omp daemon brok`, the fifteen bytes the kernel keeps, and command line `/opt/omp/bin/omp`
// (INFRA-1208, measured 2026-11-05) — and a process either of them names is the role's own
// machinery from the pass that named it.
var agentPrograms = []string{"legion", "omp", "claude"}

// readCmdline is the one read of a process's command line a walk makes. It is a package var, as
// this package's readAnonymous is, so that a test can say which processes a pass read one for:
// which pass reads which command line is the whole of the rule Next describes.
var readCmdline = procread.ReadCmdline

// Pair is a role and a kind of work, the two labels every CPU and memory series carries. It is
// the whole of what a walk says about a process: never a program, an argument or a command line,
// which is dispatch://INFRA-1208/spec#b-a-noargs.
type Pair struct {
	Role string
	Kind Kind
}

// Process is one process of a pass, as a series reader needs it: who it is, who reaped for it,
// what it is counted as, and the memory it holds.
type Process struct {
	PID, Parent int
	Pair        Pair
	// ResidentBytes is the kernel's resident set, in bytes of this kernel's pages: /proc
	// counts pages and only this side knows how big one is.
	ResidentBytes int64
}

// Pass is what one walk found.
type Pass struct {
	// Processes are the processes the pass read, the largest resident memory first, so a
	// reader spending a budget on something costlier than a stat — the 10-second smaps pass of
	// dispatch://INFRA-1208/spec#b-a-rates, at 2.45 ms a process — spends it on the processes
	// worth reading. Two processes of the same size come in pid order, so two passes over one
	// /proc agree.
	Processes []Process
	// CPUSeconds is the CPU charged in this pass alone, not a total: the caller holds the
	// counter and this is what to add to it. Every pair the pass found a process of stands
	// here, at zero where it spent nothing, together with the pair of a child reaped since the
	// last pass, whose process is already gone.
	CPUSeconds map[Pair]float64
}

// Walk is the process table of one container, read again and again. It holds the last reading of
// each process so that a pass can charge the rise alone, and forgets a process once it is gone,
// so what it holds is bounded by the processes that exist. One walk serves every pass of one
// /proc, from one goroutine: a second walk over the same /proc would charge every process's whole
// lifetime a second time.
//
// Every stat it reads is read through the collector's own reader, which keeps the descriptor of
// each process it is holding and is told to forget a process when this walk forgets it, so a
// pass over a pod costs the reads and not the opens (procread.Reader).
type Walk struct {
	root   string
	rules  Rules
	reader *procread.Reader
	last   map[int]*reading
	errors uint64
}

// reading is what the walk remembers of one process between two passes: enough to tell a rise
// from a whole lifetime, a pid the kernel reused from the process the walk read, and what to
// charge the CPU a parent reaps for it once it is gone.
type reading struct {
	pid int
	// start is the process's boot-relative starting moment. A pid whose start differs from
	// the one recorded is another process wearing an old number, not the one that was read.
	start uint64
	// own and reaped are utime + stime and cutime + cstime as the last pass read them, in
	// ticks. Both are zero for a process no pass has read, which is what makes its first pass
	// charge the lifetime /proc already reports (dispatch://INFRA-1208/spec#b-a-rates: the
	// counters are seeded from what /proc holds, so a restarted sampler is continuous rather
	// than stepped).
	own, reaped uint64
	// charged is the ticks every pass has charged for the processes below this one, alive and
	// dead alike: each process's own rise and each remainder a reap left, recorded against
	// every ancestor the pass knew of it. It is what answers the rise a parent reports when
	// this process is reaped, since the two counters above are this process's alone and say
	// nothing of the subtree the kernel folds into that rise — a `bash -c 'time go build …'`
	// that spends nothing itself and reaps a `go` worth 400 s of compiles reads as own 0 and
	// reaped 0 until the moment both of them end (INFRA-1208, measured 2026-11-05).
	//
	// It holds the charges of a descendant that outlives this process too, which the kernel
	// then folds into whoever adopts that descendant and never into this process's reap. The
	// reap is answered with more than it holds, which charges nothing — rise never goes below
	// zero — so an orphaned subtree costs this process's own last seconds and never a second
	// count of anybody's.
	charged uint64
	// parent and pair are where the last pass found this process and what it counted it as,
	// which is what the CPU its parent reaps for it is charged to once it has ended.
	parent int
	pair   Pair
	// rule is the kind this process's own command line gives, empty where no rule names it,
	// and harness reports whether the program is one of Legion's own. read is whether the
	// command line has been read at all: it is read on the pass after the one that first saw
	// the process, and once, since a process never changes the program it runs (Next).
	rule    Kind
	harness bool
	read    bool
}

// NewWalk is a walk of the processes under root — "/" in a pod, where the launcher's own
// container holds every role's processes, and a directory of its own in a test — whose kinds
// these rules decide and whose stat is read through this reader, the one its collector reads
// everything else of the same pod through.
func NewWalk(root string, rules Rules, reader *procread.Reader) *Walk {
	return &Walk{root: root, rules: rules, reader: reader, last: make(map[int]*reading)}
}

// ReadErrors is how many reads of /proc have failed for a reason other than the two a pass
// expects, since this walk began: the count dispatch://INFRA-1208/spec#b-a-rates serves as
// legion_sampler_read_errors_total. A process that ended mid-pass and a process this one may not
// read are not among them, since neither is a fault to report.
func (w *Walk) ReadErrors() uint64 { return w.errors }

// Next is one pass: every process of <root>/proc read once, each given its role and its kind, and
// the CPU each pair spent since the pass before.
//
// roles is each role's role-init process id, as the launcher's roles file holds it at this
// moment. The caller reads that file again for every pass and hands the answer here, so a
// generation that started or ended between two passes is picked up without a restart. A role
// whose pid is not positive names no process — which is what the file holds for a role between
// generations — and is passed over.
//
// A process that ends while the pass walks and a process this one may not read are skipped rather
// than failing the pass: a walk of a tree that is changing under it meets both in the ordinary
// course. Any other failed read is counted (ReadErrors) and that process passed over. Only a
// /proc that cannot be listed at all is an error, since a pass that read nothing must say so
// rather than report that nothing is running.
//
// A command line is read on a process's second sighting and not its first. The pass that first
// sees a process reads its stat and nothing else, and gives it the kind of its nearest ancestor
// that has one, exactly as it gives that kind to a process whose own command line matches no
// rule; the next pass that still finds the process reads the command line and the kind a rule
// gives it stands from then on. A process that lived and died between two passes is never read
// for one at all.
//
// That is where a busy pod's sampler spends: a build makes thousands of compile processes, each
// new at nearly every pass and most of them gone before the next one, and under gVisor the open
// of a cmdline costs as much as the open of the stat beside it. The read buys nothing for those
// processes — a compile under `go build` takes `build` from the `go` that started it either way
// — and what it would correct, it corrects on the next pass (INFRA-1208, measured 2026-11-05).
//
// A role's harness is KindAgent from the pass that first sees it all the same: that is read from
// the Comm of its stat, which the walk already reads (agentPrograms).
func (w *Walk) Next(roles map[string]int) (Pass, error) {
	pids, err := procread.PIDs(w.root)
	if err != nil {
		return Pass{}, err
	}
	stats := make(map[int]procread.Stat, len(pids))
	live := make([]int, 0, len(pids))
	for _, pid := range pids {
		stat, err := w.reader.Stat(pid)
		switch {
		case unreadable(err):
			continue
		case err != nil:
			w.errors++
			continue
		}
		stats[pid] = stat
		live = append(live, pid)
	}

	reaped := forget(w.last, stats, itself, w.reader)
	w.seed(live, stats)
	pairs := w.attribute(live, stats, roles)

	pass := Pass{
		Processes:  make([]Process, 0, len(live)),
		CPUSeconds: make(map[Pair]float64, len(live)),
	}
	pageSize := int64(os.Getpagesize())
	charges := ledger[*reading]{cpu: pass.CPUSeconds, last: w.last, of: itself, stats: stats}
	for _, pid := range live {
		stat, entry, pair := stats[pid], w.last[pid], pairs[pid]
		charges.charge(pair, rise(stat.UserTicks+stat.SystemTicks, entry.own), stat.Parent)
		charges.chargeReaped(pair, pid,
			rise(stat.ChildUserTicks+stat.ChildSystemTicks, entry.reaped), reaped[pid])
		pass.Processes = append(pass.Processes, Process{
			PID:           pid,
			Parent:        stat.Parent,
			Pair:          pair,
			ResidentBytes: int64(stat.ResidentPages) * pageSize,
		})
		entry.own = stat.UserTicks + stat.SystemTicks
		entry.reaped = stat.ChildUserTicks + stat.ChildSystemTicks
		entry.parent = stat.Parent
		entry.pair = pair
	}
	slices.SortFunc(pass.Processes, func(a, b Process) int {
		if order := cmp.Compare(b.ResidentBytes, a.ResidentBytes); order != 0 {
			return order
		}
		return cmp.Compare(a.PID, b.PID)
	})
	return pass, nil
}

// itself is what takes the reading out of a walk's own map value, which is a reading already.
func itself(entry *reading) *reading { return entry }

// forget drops the reading of every process that is no longer there — one that ended, and one
// whose pid the kernel has since handed to another process, which its starting moment tells — and
// answers those readings by the parent that is reaping for them, each list in pid order so that
// two passes over the same /proc charge the same amounts. Dropping them is what bounds a
// collector: it holds a reading of the processes that exist and of nothing else.
//
// Both collectors keep readings of their own — the walk's is a reading, node mode's is a reading
// with the pod its process belongs to — so the map is the caller's own and `of` is what takes
// the reading out of one of its values.
//
// The reader is told about every process dropped here, and that is what bounds what it holds
// open: a collector keeps the descriptors of the processes it is holding readings of and of no
// others. The descriptor of a pid the kernel has handed on is dropped twice over, since the
// reader refuses to answer for a process through a descriptor opened on another one.
func forget[T any](last map[int]T, stats map[int]procread.Stat, of func(T) *reading, reader *procread.Reader) map[int][]*reading {
	var gone []*reading
	for pid, held := range last {
		entry := of(held)
		if stat, alive := stats[pid]; alive && stat.StartTicks == entry.start {
			continue
		}
		gone = append(gone, entry)
		delete(last, pid)
		reader.Forget(pid)
	}
	slices.SortFunc(gone, func(a, b *reading) int { return cmp.Compare(a.pid, b.pid) })
	reaped := make(map[int][]*reading, len(gone))
	for _, entry := range gone {
		reaped[entry.parent] = append(reaped[entry.parent], entry)
	}
	return reaped
}

// seed gives a process the walk has not read before a reading of its own, and reads the command
// line of a process it saw on an earlier pass and has not read one for yet. A process whose
// cmdline cannot be read is kept all the same, with no kind of its own: it is still somebody's
// parent and still spending CPU, and a tree missing a process in the middle would hand every
// process below it the wrong role. A read that failed is not tried again, since a file this
// process may not read now is one it may not read in two seconds either.
//
// Which pass reads which command line is Next's rule, and the reason is there.
func (w *Walk) seed(live []int, stats map[int]procread.Stat) {
	for _, pid := range live {
		entry, held := w.last[pid]
		if !held {
			w.last[pid] = &reading{
				pid:     pid,
				start:   stats[pid].StartTicks,
				harness: slices.Contains(agentPrograms, stats[pid].Comm),
			}
			continue
		}
		if entry.read {
			continue
		}
		entry.read = true
		args, err := readCmdline(w.root, pid)
		if err != nil && !unreadable(err) {
			w.errors++
		}
		if kind, named := w.rules.Kind(args); named {
			entry.rule = kind
		}
		if len(args) > 0 && !entry.harness {
			entry.harness = slices.Contains(agentPrograms, path.Base(args[0]))
		}
	}
}

// attribute is the role and the kind of every process the pass read.
//
// The role is the role whose role-init process is the process itself or one of its ancestors: a
// role's processes are its role-init's descendants, across the user, mount, PID and IPC
// namespaces that init heads, which internal/launcher's TestARolesGrandchildIsReadThroughProc…
// proves the container's /proc shows. A process under no role-init is RoleShared.
//
// The kind is KindAgent for a role's own harness — its role-init, its `legion` processes and the
// harness they run — because that is a place in the tree and not a command line. Otherwise it is
// the first kind a rule gives walking from the process up to the harness of its role, so that
// every process a build started is build and a child of an agent that no rule and no ancestor
// names is KindTool. A shared process takes the kind a rule gives it, climbing the same way, and
// otherwise KindShared: a stray build a person left behind is still a build, while the launcher,
// the sampler and the test Postgres are shared (dispatch://INFRA-1208/spec#b-a-sampler).
func (w *Walk) attribute(live []int, stats map[int]procread.Stat, roles map[string]int) map[int]Pair {
	inits := make(map[int]string, len(roles))
	for role, pid := range roles {
		if pid > 0 {
			inits[pid] = role
		}
	}
	roleOf := make(map[int]string, len(live))
	for _, pid := range live {
		role(pid, stats, inits, roleOf)
	}
	inherited := make(map[int]Kind, len(live))
	pairs := make(map[int]Pair, len(live))
	for _, pid := range live {
		pair := Pair{Role: roleOf[pid], Kind: w.ruled(pid, stats, roleOf, inits, inherited)}
		switch {
		case pair.Role == RoleShared:
			if pair.Kind == "" {
				pair.Kind = KindShared
			}
		case agentProcess(w.last[pid], pid, inits):
			pair.Kind = KindAgent
		case pair.Kind == "":
			pair.Kind = KindTool
		}
		pairs[pid] = pair
	}
	return pairs
}

// role is the role this process belongs to, and the role of every process on the way up to the
// role-init that answers for it, which known keeps: a pass climbs each chain once however many
// processes stand on it. A chain that leaves the processes the pass read — the climb reached
// process 0, or a process that ended before the pass read it — belongs to no role. The climb
// takes at most as many steps as there are processes, so a /proc whose parents somehow lead in a
// circle costs a pass one walk of it and not a hang.
func role(pid int, stats map[int]procread.Stat, inits map[int]string, known map[int]string) string {
	chain := make([]int, 0, 8)
	answer := RoleShared
	for range len(stats) + 1 {
		if role, held := known[pid]; held {
			answer = role
			break
		}
		if _, read := stats[pid]; !read {
			break
		}
		chain = append(chain, pid)
		if role, heads := inits[pid]; heads {
			answer = role
			break
		}
		pid = stats[pid].Parent
	}
	for _, pid := range chain {
		known[pid] = answer
	}
	return answer
}

// agentProcess is whether this process is a role's own machinery rather than work the role
// started: its role-init, and a process running one of Legion's own programs, which is what
// reading.harness holds. It is what KindAgent counts and where a kind climb stops.
func agentProcess(entry *reading, pid int, inits map[int]string) bool {
	return inits[pid] != "" || entry.harness
}

// ruled is the first kind a rule gives, taking this process's command line and then its
// ancestors' up to the harness of its role, and empty where no rule names any of them.
//
// The climb stops where the role changes, which is the top of the tree for a shared process,
// since a kind is inherited from the work that started a process and never from the launcher
// that stands above every role. It stops at a harness process too, and takes no kind from it: an
// agent's own command line is the harness's — the whole system prompt stands in it — and says
// nothing about the work its children do, so a process under the harness that no rule names is
// KindTool, which is dispatch://INFRA-1208/spec#b-a-kinds ("other children of omp are `tool`").
//
// What each process answers is kept, as the role climb keeps its own, and the walk reads no
// command line here: a process's rule was read on the pass after the one that first saw it, and
// a process no pass has read one for yet has none, which is what makes the climb hand it its
// nearest ancestor's kind (Next).
func (w *Walk) ruled(pid int, stats map[int]procread.Stat, roleOf, inits map[int]string, known map[int]Kind) Kind {
	chain := make([]int, 0, 8)
	var answer Kind
	for range len(stats) + 1 {
		if kind, held := known[pid]; held {
			answer = kind
			break
		}
		entry, read := w.last[pid]
		if !read {
			break
		}
		chain = append(chain, pid)
		if agentProcess(entry, pid, inits) {
			break
		}
		if entry.rule != "" {
			answer = entry.rule
			break
		}
		parent := stats[pid].Parent
		if roleOf[parent] != roleOf[pid] {
			break
		}
		pid = parent
	}
	for _, pid := range chain {
		known[pid] = answer
	}
	return answer
}

// ledger is what one pass charges: the CPU each pair spent, and the record each charge leaves
// behind on the processes it was spent below. Both collectors charge through one of these —
// their maps of readings differ, so the map is the caller's own and `of` is what takes the
// reading out of one of its values, as forget takes it — and both follow the same two rules, a
// process's own rise and chargeReaped's spread of what a parent reaped.
type ledger[T any] struct {
	cpu   map[Pair]float64
	last  map[int]T
	of    func(T) *reading
	stats map[int]procread.Stat
}

// charge counts these ticks to this pair and records them below, so that the pair stands in the
// pass even where it spent nothing: a reader of a pass is told about every pair it found.
func (l ledger[T]) charge(pair Pair, ticks uint64, below int) {
	l.cpu[pair] += seconds(ticks)
	l.record(ticks, below)
}

// record remembers ticks the pass has charged for a subtree against every process that subtree
// stands under: the nearest process the pass knows above the work — the parent of a process
// spending its own CPU, and the reaping process itself for the remainder a reap leaves — and
// that process's own ancestors.
//
// This is what a reap is answered with. A process's own two counters say nothing about the CPU
// of the processes below it that it has not reaped yet, so a middle process that dies in the
// same interval as the work it reaped would have its whole subtree charged a second time
// (INFRA-1208, measured 2026-11-05: a build of 403.63 s charged 805.87 s). What the walk has
// already charged for that subtree is kept here instead, and it is answered whole.
//
// The climb costs a pass one step for each level of the process tree, which in a pod is a few,
// and takes at most as many steps as there are processes, so a /proc whose parents somehow lead
// in a circle costs a pass one walk of it and not a hang.
func (l ledger[T]) record(ticks uint64, below int) {
	if ticks == 0 {
		return
	}
	for range len(l.stats) + 1 {
		held, known := l.last[below]
		if !known {
			return
		}
		l.of(held).charged += ticks
		stat, alive := l.stats[below]
		if !alive {
			return
		}
		below = stat.Parent
	}
}

// chargeReaped spreads the CPU a parent reaped since the last pass — the rise of its cutime +
// cstime, which is what the kernel folds into a parent when it waits for a child — over the pairs
// it belongs to.
//
// Of that rise the walk has already charged what it counted: every pass charged each child the
// CPU it had spent and everything it charged below that child, and a child's whole accounting,
// its own CPU and what it had itself reaped, is what the kernel adds to the parent. So only what
// stands above the child's own reading and the record of its subtree is CPU no series has counted
// yet, and charging the rise whole would count those lives twice. That remainder is theirs — the
// seconds each spent between its last reading and its end — split in proportion to what each was
// last counted with, since the kernel says nothing about which child a rise came from, and
// equally between them where none of them had any.
//
// A parent that reaped no child the walk ever saw rose by the whole life of a child that began
// and ended between two passes. That goes to the parent's own kind, or to KindTool where the
// parent is the role's harness, which is dispatch://INFRA-1208/spec#b-a-kinds: "CPU from a child
// no read saw goes to its parent's kind, or to `tool` when the parent is omp".
//
// Nothing is ever charged a negative amount: a counter that went backwards is no CPU at all.
func (l ledger[T]) chargeReaped(parent Pair, pid int, ticks uint64, children []*reading) {
	if ticks == 0 {
		return
	}
	var counted uint64
	for _, child := range children {
		counted += child.own + child.charged
	}
	left := rise(ticks, counted)
	switch {
	case left == 0:
		return
	case len(children) == 0:
		l.cpu[unseenChildPair(parent)] += seconds(left)
	case counted == 0:
		each := seconds(left) / float64(len(children))
		for _, child := range children {
			l.cpu[child.pair] += each
		}
	default:
		for _, child := range children {
			l.cpu[child.pair] += seconds(left) * float64(child.own+child.charged) / float64(counted)
		}
	}
	// The remainder is the reaped subtree's and is charged below the process that reaped it,
	// which is what answers for it when that process is itself reaped.
	l.record(left, pid)
}

// unseenChildPair is what the CPU of a child no read saw is counted as: its parent's pair, except
// that a harness process is charged KindTool, since an agent does not spend its own seconds on
// the compile it started — the kind is the work, and the work was the child's.
func unseenChildPair(parent Pair) Pair {
	if parent.Kind == KindAgent {
		return Pair{Role: parent.Role, Kind: KindTool}
	}
	return parent
}

// rise is how far a kernel counter moved since it was last read, and nothing where it moved
// backwards: a pid the kernel reused carries counters of its own, lower than the ones the walk
// recorded for the process that wore the number before it.
func rise(now, then uint64) uint64 {
	if now < then {
		return 0
	}
	return now - then
}

// seconds is ticks of /proc's fixed USER_HZ as the seconds a series carries.
func seconds(ticks uint64) float64 { return float64(ticks) / procread.ClockTicks }

// unreadable reports whether a read of /proc failed for a process a pass cannot account for and
// must pass over: one that ended while the pass walked, and one belonging to a user this process
// may not read.
func unreadable(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission)
}
