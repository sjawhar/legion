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
var agentPrograms = []string{"legion", "omp", "claude"}

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
type Walk struct {
	root   string
	rules  Rules
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
	// parent and pair are where the last pass found this process and what it counted it as,
	// which is what the CPU its parent reaps for it is charged to once it has ended.
	parent int
	pair   Pair
	// rule is the kind this process's own command line gives, empty where no rule names it,
	// and harness reports whether the program is one of Legion's own. Both are read once, on
	// the pass that first saw the process: a process never changes the program it runs, so a
	// second read of its cmdline could only cost the pass the 0.07 ms it takes.
	rule    Kind
	harness bool
}

// NewWalk is a walk of the processes under root — "/" in a pod, where the launcher's own
// container holds every role's processes, and a directory of its own in a test — whose kinds
// these rules decide.
func NewWalk(root string, rules Rules) *Walk {
	return &Walk{root: root, rules: rules, last: make(map[int]*reading)}
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
func (w *Walk) Next(roles map[string]int) (Pass, error) {
	pids, err := procread.PIDs(w.root)
	if err != nil {
		return Pass{}, err
	}
	stats := make(map[int]procread.Stat, len(pids))
	live := make([]int, 0, len(pids))
	for _, pid := range pids {
		stat, err := procread.ReadStat(w.root, pid)
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

	reaped := w.forget(stats)
	w.seed(live, stats)
	pairs := w.attribute(live, stats, roles)

	pass := Pass{
		Processes:  make([]Process, 0, len(live)),
		CPUSeconds: make(map[Pair]float64, len(live)),
	}
	pageSize := int64(os.Getpagesize())
	for _, pid := range live {
		stat, entry, pair := stats[pid], w.last[pid], pairs[pid]
		pass.CPUSeconds[pair] += seconds(rise(stat.UserTicks+stat.SystemTicks, entry.own))
		chargeReaped(pass.CPUSeconds, pair,
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

// forget drops the reading of every process that is no longer there — one that ended, and one
// whose pid the kernel has since handed to another process, which its starting moment tells — and
// answers those readings by the parent that is reaping for them, each list in pid order so that
// two passes over the same /proc charge the same amounts. Dropping them is what bounds the walk:
// it holds a reading of the processes that exist and of nothing else.
func (w *Walk) forget(stats map[int]procread.Stat) map[int][]*reading {
	var gone []*reading
	for pid, entry := range w.last {
		if stat, alive := stats[pid]; alive && stat.StartTicks == entry.start {
			continue
		}
		gone = append(gone, entry)
		delete(w.last, pid)
	}
	slices.SortFunc(gone, func(a, b *reading) int { return cmp.Compare(a.pid, b.pid) })
	reaped := make(map[int][]*reading, len(gone))
	for _, entry := range gone {
		reaped[entry.parent] = append(reaped[entry.parent], entry)
	}
	return reaped
}

// seed gives a process the walk has not read before a reading of its own, holding what its
// command line says about it. A process whose cmdline cannot be read is kept all the same, with
// no kind of its own: it is still somebody's parent and still spending CPU, and a tree missing a
// process in the middle would hand every process below it the wrong role.
func (w *Walk) seed(live []int, stats map[int]procread.Stat) {
	for _, pid := range live {
		if _, held := w.last[pid]; held {
			continue
		}
		entry := &reading{pid: pid, start: stats[pid].StartTicks}
		args, err := procread.ReadCmdline(w.root, pid)
		if err != nil && !unreadable(err) {
			w.errors++
		}
		if kind, named := w.rules.Kind(args); named {
			entry.rule = kind
		}
		if len(args) > 0 {
			entry.harness = slices.Contains(agentPrograms, path.Base(args[0]))
		}
		w.last[pid] = entry
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
// the first kind a rule gives walking from the process up to the root of its role, so that every
// process a build started is build and a child of an agent that no rule and no ancestor names is
// KindTool. A shared process takes the kind a rule gives it, climbing the same way, and otherwise
// KindShared: a stray build a person left behind is still a build, while the launcher, the
// sampler and the test Postgres are shared (dispatch://INFRA-1208/spec#b-a-sampler).
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
		pair := Pair{Role: roleOf[pid], Kind: w.ruled(pid, stats, roleOf, inherited)}
		switch {
		case pair.Role == RoleShared:
			if pair.Kind == "" {
				pair.Kind = KindShared
			}
		case inits[pid] != "" || w.last[pid].harness:
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

// ruled is the first kind a rule gives, reading this process's command line and then its
// ancestors' up to the root of its role, and empty where no rule names any of them. The climb
// stops where the role changes, which is the role-init of a role's process and the top of the
// tree for a shared one, since a kind is inherited from the work that started a process and never
// from the launcher that stands above every role. What each process answers is kept, as the role
// climb keeps its own, and the walk reads no command line here: every process's rule was read on
// the pass that first saw it.
func (w *Walk) ruled(pid int, stats map[int]procread.Stat, roleOf map[int]string, known map[int]Kind) Kind {
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

// chargeReaped spreads the CPU a parent reaped since the last pass — the rise of its cutime +
// cstime, which is what the kernel folds into a parent when it waits for a child — over the pairs
// it belongs to.
//
// Of that rise the walk has already charged what it read: every pass charged each child the CPU
// it had spent, and a child's whole accounting, its own CPU and what it had itself reaped, is
// what the kernel adds to the parent. So only what stands above the last readings of the children
// the walk saw is CPU no series has counted yet, and charging the rise whole would count their
// lives twice. That remainder is theirs — the seconds each spent between its last reading and its
// end — split in proportion to the CPU each was last read with, since the kernel says nothing
// about which child a rise came from, and equally between them where none of them had any.
//
// A parent that reaped no child the walk ever saw rose by the whole life of a child that began
// and ended between two passes. That goes to the parent's own kind, or to KindTool where the
// parent is the role's harness, which is dispatch://INFRA-1208/spec#b-a-kinds: "CPU from a child
// no read saw goes to its parent's kind, or to `tool` when the parent is omp".
//
// Nothing is ever charged a negative amount: a counter that went backwards is no CPU at all.
func chargeReaped(cpu map[Pair]float64, parent Pair, ticks uint64, children []*reading) {
	if ticks == 0 {
		return
	}
	var counted uint64
	for _, child := range children {
		counted += child.own + child.reaped
	}
	left := rise(ticks, counted)
	switch {
	case left == 0:
		return
	case len(children) == 0:
		cpu[unseenChildPair(parent)] += seconds(left)
	case counted == 0:
		each := seconds(left) / float64(len(children))
		for _, child := range children {
			cpu[child.pair] += each
		}
	default:
		for _, child := range children {
			cpu[child.pair] += seconds(left) * float64(child.own+child.reaped) / float64(counted)
		}
	}
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
