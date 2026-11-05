package sampler

import (
	"cmp"
	"errors"
	"io/fs"
	"os"
	"path"
	"slices"
	"sync/atomic"

	"github.com/sjawhar/legion/daemon/internal/procread"
	"github.com/sjawhar/legion/daemon/internal/samplerwire"
)

// This file is the process walk, which is one body and not two: the pass itself — every
// process's stat, the readings it keeps between passes, the pid the kernel reused, the CPU a
// parent reaped and the reads that failed — belongs to Walk, and everything that differs
// between an issue pod and a node belongs to an Attribution. So a fix to reaping, to pid reuse
// or to charging is made once, where before node mode kept a copy of all four
// (dispatch://INFRA-1208, the review of pull request 2786).

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

// processesSeen is every new process the walks of this process have read, counted once each, at
// the stat read that first sees one (sight). It is what the sampler serves as
// legion_sampler_processes_seen_total, the rate the cap of dispatch://INFRA-1208/spec#b-a-rates
// is held at: 3% of one CPU over any minute in which this counter rises by at most 20 a second,
// and above that rate a cost that grows with it.
//
// It stands here, beside the sighting, and not in a registry a pass hands a number to, because
// it is the sampler process's own count and carries no label to tell two of anything apart. One
// sampler runs one walk — an issue pod's over its container, a node's over the node's whole
// process table — and both modes serve the one counter, so there is one number here and no
// reading for a collector of either mode to carry. A test reads the rise between two scrapes
// rather than the count itself, as a reader of the series does (pod_test.go's churn minute).
var processesSeen atomic.Uint64

// Work is what a pass counts a process's work under: the owner the work belongs to and the kind
// of work it is. The owner is a role in an issue pod and the uid of a pod on a node, which is
// the one thing the two modes do not agree on, and an empty owner is work this mode charges to
// nobody — a node's kubelet, its container runtime, a pod this sampler does not serve.
//
// It is the whole of what a walk says about a process: never a program, an argument or a command
// line, which is dispatch://INFRA-1208/spec#b-a-noargs.
type Work struct {
	Owner string
	Kind  samplerwire.Kind
}

// Process is one process of a pass, as a series reader needs it: who it is, who reaped for it,
// what it is counted as, and the memory it holds.
type Process struct {
	PID, Parent int
	Work        Work
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
	// /proc agree. A process of no owner stands here too, since the pass read it.
	Processes []Process
	// CPUSeconds is the CPU charged in this pass alone, not a total: the caller holds the
	// counter and this is what to add to it. Every owner and kind the pass found a process of
	// stands here, at zero where it spent nothing, together with the one of a child reaped
	// since the last pass, whose process is already gone.
	CPUSeconds map[Work]float64
	// ResidentBytes is the resident memory each owner's work of one kind holds now, summed
	// over the processes of this pass. It is a reading of this moment and not a rise.
	ResidentBytes map[Work]int64
}

// Attribution is the half of a pass that differs between the two modes: what to read of a
// process the walk has not read before, and which owner and kind to count its work under. One
// of these is an issue pod's — the role whose role-init heads the process's subtree, and the
// kinds of dispatch://INFRA-1208/spec#b-a-kinds — and one is a node's, where the owner is the
// pod whose cgroup the process sits in and the kinds are `build`, `test` and `other`.
//
// Everything else is the walk's: the pass, the readings it keeps between passes, the pid the
// kernel reused, the reaped-CPU ledger and the reads it reports as failed.
type Attribution interface {
	// Seen is the policy's own reading of a process, which the walk makes on the first two
	// passes that find it and never again: first is the pass that first saw it, holding the
	// stat that pass has just read, and the call after it is the pass that found the process
	// still there.
	//
	// Which of the two reads what is the policy's: an issue pod's reads a command line on the
	// second of them, because a build makes thousands of processes that are gone before the
	// next pass and reading each one costs the pod more than the kind it would give (Next),
	// and a node's reads the cgroup path that says which pod a process belongs to on the
	// first, because a process it cannot place is charged to nobody at all.
	Seen(pid int, stat procread.Stat, first bool)
	// Attribute is the owner and kind of every process of one pass. A process it answers no
	// entry for, and one whose owner is empty, is read and charged to nobody.
	Attribute(live []int, stats map[int]procread.Stat) map[int]Work
	// Forget drops what the policy holds about a process that is gone, which the walk calls
	// for every reading it drops itself: what a policy holds is bounded by the processes
	// that exist, as what the walk holds is.
	Forget(pid int)
}

// Walk is one /proc read again and again: an issue pod's container, which holds every role's
// processes, and a node's whole process table. It holds the last reading of each process so that
// a pass can charge the rise alone, and forgets a process once it is gone, so what it holds is
// bounded by the processes that exist. One walk serves every pass of one /proc, from one
// goroutine: a second walk over the same /proc would charge every process's whole lifetime a
// second time.
//
// Every stat it reads is read through the collector's own reader, which keeps the descriptor of
// each process it is holding and is told to forget a process when this walk forgets it, so a
// pass over a pod costs the reads and not the opens (procread.Reader).
type Walk struct {
	root        string
	reader      *procread.Reader
	attribution Attribution
	// failed takes one read of /proc that failed for a reason a pass does not expect. It is
	// the collector's own, which counts the failure as legion_sampler_read_errors_total under
	// its source and logs it once, so the walk keeps no count of its own for a collector to
	// add to its own and publish.
	failed func(ReadSource, error)
	last   map[int]*reading
	// walked is whether a pass of this walk has run. The first pass charges nothing at all:
	// what /proc holds for a process this walk has never watched is work of before the
	// sampler, and nothing in it says which kind of work it was (Next, sight).
	walked bool
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
	// ticks. Both are what /proc already held for a process the walk's first pass found, so
	// that pass charges none of a lifetime it never watched, and zero for a process that
	// appeared since, whose whole lifetime is CPU spent between two passes of this walk
	// (Next, "across a restart").
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
	// parent and work are where the last pass found this process and what it counted it as,
	// which is what the CPU its parent reaps for it is charged to once it has ended.
	parent int
	work   Work
	// sightings is how many passes have found this process, held at two: the attribution
	// policy reads a process on the first two passes that find it and never again (Seen).
	sightings uint8
}

// NewWalk is a walk of the processes under root — "/" in a pod and on a node, where the
// launcher's own container holds every role's processes, and a directory of its own in a test —
// read through this reader, the one its collector reads everything else of the same pod through,
// attributed by this policy, and reporting what it could not read to this collector.
func NewWalk(root string, reader *procread.Reader, attribution Attribution, failed func(ReadSource, error)) *Walk {
	return &Walk{
		root:        root,
		reader:      reader,
		attribution: attribution,
		failed:      failed,
		last:        make(map[int]*reading),
	}
}

// Next is one pass: every process of <root>/proc read once, each given the owner and the kind
// its mode's policy makes of it, and the CPU and the resident memory each of those spent since
// the pass before.
//
// The policy is given whatever it attributes by before the pass: an issue pod's collector hands
// it the roles file as it stands at this moment, which it reads again for every pass, so a
// generation that started or ended between two passes is picked up without a restart; a node's
// hands it the pod set its watch holds.
//
// A process that ends while the pass walks and a process this one may not read are skipped rather
// than failing the pass: a walk of a tree that is changing under it meets both in the ordinary
// course. Any other failed read is reported to the collector and that process passed over. Only a
// /proc that cannot be listed at all is an error, since a pass that read nothing must say so
// rather than report that nothing is running.
//
// An issue pod reads a command line on a process's second sighting and not its first. The pass
// that first sees a process reads its stat and nothing else, and gives it the kind of its nearest
// ancestor that has one, exactly as it gives that kind to a process whose own command line
// matches no rule; the next pass that still finds the process reads the command line and the kind
// a rule gives it stands from then on. A process that lived and died between two passes is never
// read for one at all.
//
// That is where a busy pod's sampler spends: a build makes thousands of compile processes, each
// new at nearly every pass and most of them gone before the next one, and under gVisor the open
// of a cmdline costs as much as the open of the stat beside it. The read buys nothing for those
// processes — a compile under `go build` takes `build` from the `go` that started it either way
// — and what it would correct, it corrects on the next pass (INFRA-1208, measured 2026-11-05).
//
// A role's harness is KindAgent from the pass that first sees it all the same: that is read from
// the Comm of its stat, which the walk already reads (agentPrograms).
//
// Across a restart a walk charges only what it has watched. The first pass of a walk's life
// records what /proc already holds for every process it finds — the process's own utime + stime
// and the cutime + cstime of everything it has reaped — as CPU already charged, so that pass
// charges nothing and every counter of the sampler starts at zero. A restart is then a counter
// reset, which Prometheus reads correctly, and the pod's own CPU total is read from its cgroup
// and keeps its history whatever the sampler did.
//
// Charging those two counters instead put a role's history under the wrong kind. A fresh walk
// holds no reading of any child, so a harness's whole reaped lifetime looked like the CPU of a
// child no read saw and went to KindTool at once (chargeReaped, unseenChildWork): every build
// and test the role had reaped before the restart — the 64 MiB exit, a crash, an OOM kill —
// moved into `tool` in one pass. A live process's own lifetime went the same way, to the kind
// of its nearest ancestor, since the pass that first sees a process reads no command line for
// it. Neither counter says which kind the work was, so neither is charged (INFRA-1208, the
// review of pull request 2786).
func (w *Walk) Next() (Pass, error) {
	pids, err := procread.PIDs(w.root)
	if err != nil {
		return Pass{}, err
	}
	stats := make(map[int]procread.Stat, len(pids))
	live := make([]int, 0, len(pids))
	for _, pid := range pids {
		stat, err := readStat(w.reader, pid)
		switch {
		case unreadable(err):
			continue
		case err != nil:
			w.failed(SourceProc, err)
			continue
		}
		stats[pid] = stat
		live = append(live, pid)
	}

	reaped := w.forget(stats)
	w.sight(live, stats)
	attributed := w.attribution.Attribute(live, stats)

	pass := Pass{
		Processes:     make([]Process, 0, len(live)),
		CPUSeconds:    make(map[Work]float64, len(live)),
		ResidentBytes: make(map[Work]int64, len(live)),
	}
	pageSize := int64(os.Getpagesize())
	charges := ledger{walk: w, cpu: pass.CPUSeconds, stats: stats}
	for _, pid := range live {
		stat, entry, work := stats[pid], w.last[pid], attributed[pid]
		resident := int64(stat.ResidentPages) * pageSize
		// A process no owner of this mode claims is read and charged to nobody: its
		// reading is kept all the same, since only that says whether the pid is still the
		// process the last pass read.
		if work.Owner != "" {
			charges.charge(work, rise(stat.UserTicks+stat.SystemTicks, entry.own), stat.Parent)
			charges.chargeReaped(work, pid,
				rise(stat.ChildUserTicks+stat.ChildSystemTicks, entry.reaped), reaped[pid])
			pass.ResidentBytes[work] += resident
		}
		pass.Processes = append(pass.Processes, Process{
			PID:           pid,
			Parent:        stat.Parent,
			Work:          work,
			ResidentBytes: resident,
		})
		entry.own = stat.UserTicks + stat.SystemTicks
		entry.reaped = stat.ChildUserTicks + stat.ChildSystemTicks
		entry.parent = stat.Parent
		entry.work = work
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
// two passes over the same /proc charge the same amounts. Dropping them is what bounds a
// collector: it holds a reading of the processes that exist and of nothing else.
//
// The reader and the attribution policy are told about every process dropped here, and that is
// what bounds what each of them holds: a collector keeps the descriptors of the processes it is
// holding readings of and of no others. The descriptor of a pid the kernel has handed on is
// dropped twice over, since the reader refuses to answer for a process through a descriptor
// opened on another one.
func (w *Walk) forget(stats map[int]procread.Stat) map[int][]*reading {
	var gone []*reading
	for pid, entry := range w.last {
		if stat, alive := stats[pid]; alive && stat.StartTicks == entry.start {
			continue
		}
		gone = append(gone, entry)
		delete(w.last, pid)
		w.reader.Forget(pid)
		w.attribution.Forget(pid)
	}
	slices.SortFunc(gone, func(a, b *reading) int { return cmp.Compare(a.pid, b.pid) })
	reaped := make(map[int][]*reading, len(gone))
	for _, entry := range gone {
		reaped[entry.parent] = append(reaped[entry.parent], entry)
	}
	return reaped
}

// sight gives a process the walk has not read before a reading of its own and shows it to the
// attribution policy, which it shows a process again on the pass after that one and never again:
// the first sighting is the stat the pass has just read, and the second is the policy's chance to
// read what only a process that lived longer than one pass is worth reading (Attribution.Seen).
//
// Every process this reading is made for is one more in processesSeen: a process is new to the
// walk at the stat read that first sees it, whatever it turns out to cost after that, and a
// process that lived and died between two passes was never new to any pass and is counted
// nowhere. The pass that first sees the pod — the walk's own first — counts the processes it
// inherited there, as it charges none of their CPU.
//
// On the first pass of a walk's life a new reading holds the counters /proc answered with rather
// than zero, so that pass charges nothing of a lifetime the walk never watched; on every pass
// after it a new reading starts at zero, since a process that appeared since the last pass spent
// its whole lifetime between two passes of this walk (Next, "across a restart").
func (w *Walk) sight(live []int, stats map[int]procread.Stat) {
	for _, pid := range live {
		entry, held := w.last[pid]
		if !held {
			stat := stats[pid]
			entry = &reading{pid: pid, start: stat.StartTicks}
			if !w.walked {
				entry.own = stat.UserTicks + stat.SystemTicks
				entry.reaped = stat.ChildUserTicks + stat.ChildSystemTicks
			}
			w.last[pid] = entry
			processesSeen.Add(1)
		}
		if entry.sightings >= 2 {
			continue
		}
		entry.sightings++
		w.attribution.Seen(pid, stats[pid], entry.sightings == 1)
	}
	w.walked = true
}

// climb is the one bounded walk up the process tree this package makes: from a process through
// its parents, taking the first answer one of them gives and `none` where the chain runs out.
// Every attribution policy climbs this way — a process's role, the kind of the work that started
// it, the pod it belongs to — and each of them answers for one step alone.
//
// step is that step: the answer this process settles the climb with, and the process to go on to,
// which is below zero where this one settles it. What each process answers is kept in known, so a
// pass climbs each chain once however many processes stand on it.
//
// The climb takes at most limit steps, which every caller gives as the processes of the pass, so
// a /proc whose parents somehow lead in a circle costs a pass one walk of it and not a hang.
func climb[T comparable](pid, limit int, none T, known map[int]T, step func(int) (T, int)) T {
	chain := make([]int, 0, 8)
	answer := none
	for range limit {
		if held, read := known[pid]; read {
			answer = held
			break
		}
		settled, next := step(pid)
		if next < 0 {
			answer = settled
			break
		}
		chain = append(chain, pid)
		pid = next
	}
	for _, pid := range chain {
		known[pid] = answer
	}
	return answer
}

// ruleKind is the one read of a process's command line a pass makes: the kind a rule gives the
// program and arguments it holds, empty where no rule names them, and those arguments, for a
// policy that must look at the program itself too. Both policies read it through the package var
// above, so a test says which processes a pass read one for whichever mode it drives.
//
// A process whose cmdline cannot be read is no failure of the pass: it is still somebody's parent
// and still spending CPU, and a read that failed for a reason a pass does not expect is reported
// as a /proc read error. A failed read is not tried again, since a file this process may not read
// now is one it may not read in two seconds either.
func ruleKind(root string, pid int, rules samplerwire.Rules, failed func(ReadSource, error)) (samplerwire.Kind, []string) {
	args, err := readCmdline(root, pid)
	if err != nil && !unreadable(err) {
		failed(SourceProc, err)
	}
	kind, _ := rules.Kind(args)
	return kind, args
}

// podAttribution is an issue pod's half of a pass: the role whose role-init heads a process's
// subtree, and the kind of work that process is doing (dispatch://INFRA-1208/spec#b-a-kinds).
type podAttribution struct {
	root   string
	rules  samplerwire.Rules
	failed func(ReadSource, error)
	// roles is each role's role-init process id, as the launcher's roles file holds it at the
	// moment this pass began. The collector reads that file again for every pass and sets it
	// here, so a generation that started or ended between two passes is picked up without a
	// restart. A role whose pid is not positive names no process — which is what the file
	// holds for a role between generations — and is passed over.
	roles samplerwire.Roles
	// seen is what the policy has read of each process that exists: the kind its own command
	// line gives, empty where no rule names it, and whether the program it runs is one of
	// Legion's own.
	seen map[int]*sighting
}

// sighting is what an issue pod's policy reads of one process, once: its rule and whether it is
// a role's own machinery. Neither can change, since a process never changes the program it runs.
type sighting struct {
	rule    samplerwire.Kind
	harness bool
}

func newPodAttribution(root string, rules samplerwire.Rules, failed func(ReadSource, error)) *podAttribution {
	return &podAttribution{root: root, rules: rules, failed: failed, seen: map[int]*sighting{}}
}

// Seen reads the Comm of a process's stat on the pass that first sees it, which says whether it
// runs one of Legion's own programs, and its command line on the pass after that. Which pass
// reads which is Walk.Next's rule, and the reason is there.
func (p *podAttribution) Seen(pid int, stat procread.Stat, first bool) {
	if first {
		p.seen[pid] = &sighting{harness: slices.Contains(agentPrograms, stat.Comm)}
		return
	}
	entry := p.seen[pid]
	if entry == nil {
		return
	}
	kind, args := ruleKind(p.root, pid, p.rules, p.failed)
	if kind != "" {
		entry.rule = kind
	}
	if len(args) > 0 && !entry.harness {
		entry.harness = slices.Contains(agentPrograms, path.Base(args[0]))
	}
}

// Forget drops a process that is gone.
func (p *podAttribution) Forget(pid int) { delete(p.seen, pid) }

// Attribute is the role and the kind of every process the pass read.
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
func (p *podAttribution) Attribute(live []int, stats map[int]procread.Stat) map[int]Work {
	inits := make(map[int]string, len(p.roles))
	for role, pid := range p.roles {
		if pid > 0 {
			inits[pid] = role
		}
	}
	roles := make(map[int]string, len(live))
	kinds := make(map[int]samplerwire.Kind, len(live))
	attributed := make(map[int]Work, len(live))
	for _, pid := range live {
		work := Work{
			Owner: p.role(pid, stats, inits, roles),
			Kind:  p.ruled(pid, stats, inits, roles, kinds),
		}
		switch {
		case work.Owner == RoleShared:
			if work.Kind == "" {
				work.Kind = samplerwire.KindShared
			}
		case p.agent(pid, inits):
			work.Kind = samplerwire.KindAgent
		case work.Kind == "":
			work.Kind = samplerwire.KindTool
		}
		attributed[pid] = work
	}
	return attributed
}

// role is the role this process belongs to, and the role of every process on the way up to the
// role-init that answers for it. A chain that leaves the processes the pass read — the climb
// reached process 0, or a process that ended before the pass read it — belongs to no role.
func (p *podAttribution) role(pid int, stats map[int]procread.Stat, inits, known map[int]string) string {
	return climb(pid, len(stats)+1, RoleShared, known, func(pid int) (string, int) {
		if _, read := stats[pid]; !read {
			return RoleShared, -1
		}
		if role, heads := inits[pid]; heads {
			return role, -1
		}
		return RoleShared, stats[pid].Parent
	})
}

// agent is whether this process is a role's own machinery rather than work the role started: its
// role-init, and a process running one of Legion's own programs, which is what sighting.harness
// holds. It is what KindAgent counts and where a kind climb stops.
func (p *podAttribution) agent(pid int, inits map[int]string) bool {
	entry, read := p.seen[pid]
	return inits[pid] != "" || (read && entry.harness)
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
// The policy reads no command line here: a process's rule was read on the pass after the one that
// first saw it, and a process no pass has read one for yet has none, which is what makes the
// climb hand it its nearest ancestor's kind (Walk.Next).
func (p *podAttribution) ruled(pid int, stats map[int]procread.Stat, inits, roles map[int]string, known map[int]samplerwire.Kind) samplerwire.Kind {
	return climb(pid, len(stats)+1, "", known, func(pid int) (samplerwire.Kind, int) {
		entry, read := p.seen[pid]
		if !read || p.agent(pid, inits) {
			return "", -1
		}
		if entry.rule != "" {
			return entry.rule, -1
		}
		parent := stats[pid].Parent
		if p.role(parent, stats, inits, roles) != p.role(pid, stats, inits, roles) {
			return "", -1
		}
		return "", parent
	})
}

// ledger is what one pass charges: the CPU each owner's kind spent, and the record each charge
// leaves behind on the processes it was spent below. Both rules are one for both modes — a
// process's own rise, and chargeReaped's spread of what a parent reaped — since what a reap
// answers for has nothing to do with who the work belongs to.
type ledger struct {
	walk  *Walk
	cpu   map[Work]float64
	stats map[int]procread.Stat
}

// charge counts these ticks to this owner's kind and records them below, so that it stands in the
// pass even where it spent nothing: a reader of a pass is told about everything it found.
func (l ledger) charge(work Work, ticks uint64, below int) {
	l.cpu[work] += seconds(ticks)
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
func (l ledger) record(ticks uint64, below int) {
	if ticks == 0 {
		return
	}
	for range len(l.stats) + 1 {
		entry, known := l.walk.last[below]
		if !known {
			return
		}
		entry.charged += ticks
		stat, alive := l.stats[below]
		if !alive {
			return
		}
		below = stat.Parent
	}
}

// chargeReaped spreads the CPU a parent reaped since the last pass — the rise of its cutime +
// cstime, which is what the kernel folds into a parent when it waits for a child — over the
// owners and kinds it belongs to.
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
func (l ledger) chargeReaped(parent Work, pid int, ticks uint64, children []*reading) {
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
		l.cpu[unseenChildWork(parent)] += seconds(left)
	case counted == 0:
		each := seconds(left) / float64(len(children))
		for _, child := range children {
			l.cpu[child.work] += each
		}
	default:
		for _, child := range children {
			l.cpu[child.work] += seconds(left) * float64(child.own+child.charged) / float64(counted)
		}
	}
	// The remainder is the reaped subtree's and is charged below the process that reaped it,
	// which is what answers for it when that process is itself reaped.
	l.record(left, pid)
}

// unseenChildWork is what the CPU of a child no read saw is counted as: its parent's owner and
// kind, except that a harness process is charged KindTool, since an agent does not spend its own
// seconds on the compile it started — the kind is the work, and the work was the child's.
func unseenChildWork(parent Work) Work {
	if parent.Kind == samplerwire.KindAgent {
		return Work{Owner: parent.Owner, Kind: samplerwire.KindTool}
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
