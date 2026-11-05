package sampler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/procread"
)

// This file is the issue pod's collector: the loop that reads what one pod's kernel already
// counts and hands every reading to the series registry. It owns the rates — what is read each
// second, what every second second and what every ten, each read placed by what it costs the
// pod rather than by what it is of — the one cap every one of those reads is held inside, and
// the two profiles of docs/sampler-metrics.md, the `/proc` profile of a gVisor pod and the
// cgroup profile of a pod that runs each command in a cgroup of its own.
//
// Nothing here opens a path of its own: every read is given PodOptions.Root or a path one of the
// launcher's two files names, so a test drives a whole pod's worth of passes over a directory it
// wrote, with no real clock, no real /proc and no sleep.

// The rates, each a package var rather than a constant so that a test runs a hundred passes in
// the time a pod spends on one (internal/launcher/testpg.go:42 is the same pattern). The slower
// rates are counted in ticks of podInterval, so shortening all of them keeps the passes in step.
//
// They are split by what a reading costs the pod rather than by what it is of: the pod's CPU is
// cheap and is read every second, the pod's memory is dear and is read every two
// (dispatch://INFRA-1208/spec#a3).
var (
	// podInterval is the loop's own tick: the pod's CPU total and the sampler's own two
	// readings, which are the cheap reads. The pod's CPU — the cgroup's cpuacct.usage under
	// the legacy hierarchy, its cpu.stat under the unified one — costs 0.00 ms to 0.50 ms in
	// this pod, the sampler's own stat 0.00 ms and its own smaps 0.50 ms (INFRA-1208,
	// measured 2026-11-05). Nothing here is ever deferred.
	podInterval = time.Second
	// totalsInterval is how often the pod's memory totals are read: the pod cgroup's
	// memory.usage_in_bytes (memory.current under the unified hierarchy) and /proc/meminfo,
	// which feed legion_sampler_pod_memory_bytes, legion_sampler_pod_process_memory_bytes
	// and legion_sampler_pod_shared_memory_bytes.
	//
	// Those two are the dear reads of a gVisor pod, where the kernel answers a read of the
	// pod's memory by accounting for the pod's memory there and then rather than by reading
	// a file: 17 ms to 20 ms for memory.usage_in_bytes and 12 ms to 20 ms for /proc/meminfo
	// (INFRA-1208, measured 2026-11-05). Two seconds is the rate the cap's own rule pays for
	// them at (dispatch://INFRA-1208/spec#a3), and they are the first reading to give way
	// where the window cannot pay (readsTotals).
	totalsInterval = 2 * time.Second
	// walkInterval is how often every process's stat and every volume are read
	// (dispatch://INFRA-1208/spec#b-a-rates: "each process's `stat` and each volume at least
	// every 2 seconds").
	walkInterval = 2 * time.Second
	// smapsInterval is the window of the smaps pass, the costly read: 2.45 ms a process, so it
	// runs rarely and inside a budget.
	smapsInterval = 10 * time.Second
	// smapsBudget is the most of the sampler's own CPU one smaps window may spend. What it
	// may spend is the window's whole allowance less the rest of that window's work
	// (smapsLeft), and never more than this. It is CPU and not wall clock on purpose: a
	// sampler sharing a core with a build must read fewer processes, not the same number
	// more slowly.
	smapsBudget = 100 * time.Millisecond
)

// samplerCap is the whole of the sampler's own cost: 3% of one CPU over any minute, whether or
// not the pod's own CPUs are saturated (dispatch://INFRA-1208/spec#a3). It is one cap and not
// two, so there is one number here and nothing to decide at runtime about what the pod is
// doing. It is the requirement, where the rates above and the allowance below are the means,
// and it is what the two cost tests hold the sampler to.
const samplerCap = 0.03

// windowShare is what a window budgets against: five sixths of the cap, which leaves 0.5% of
// one CPU a minute for the readings the floors force when the allowance is already spent — one
// walk of every ten seconds and one reading of the pod's memory every four.
//
// It is below the cap and not at it because the cap is the measurement and the allowance is
// only what the collector can hold a window to: a saturated pod spends the allowance and then
// the forced walk on top of it, and what keeps the minute inside the cap is that headroom and
// the overspend the next window carries (nextWindowCPU). This pod held 2.00% to 2.53% of one
// CPU over every rolling minute of a `go build -a` that saturated both its CPUs, against this
// allowance of 2.5% — an allowance at the cap would have spent the cap and the forced walk
// would have taken the minute past it.
//
// It is no lower because the rates are what the cap now pays for: at this allowance the same
// pod idle held 0.95% to 1.37% and deferred not one reading, where a window held to much less
// would start deferring the dear reads on a pod that is merely busy (INFRA-1208, measured
// 2026-11-05).
const windowShare = 0.025

// windowAllowance is the sampler's own CPU for one smaps window: windowShare of one CPU over
// smapsInterval, which is 250 ms of every ten seconds. It is read off the rate rather than
// written down a second time, so a test that shortens the window shortens what the window may
// spend with it.
//
// The cap is held over the smaps window, which is the longest of the rates, and over every pass
// of that window: what the window has spent decides whether the memory totals are read on their
// own tick (readsTotals), whether the next walk pass runs at all (readsWalk) and what the smaps
// pass may spend (smapsLeft).
func windowAllowance() float64 { return smapsInterval.Seconds() * windowShare }

// totalsSlowed is how many of the memory totals' own ticks apart they stand once the window's
// allowance is spent: every second one of them, which is the 4 seconds the cap's rule gives them
// against the 2 seconds of their standing rate (dispatch://INFRA-1208/spec#a3). It is a count of
// ticks rather than a rate of its own, so a test that shortens the loop's rates shortens this
// with them, and it is well inside the ten seconds nothing the sampler reads waits longer than.
const totalsSlowed = 2

// volumeFloor is the used bytes a volume must reach before its series is served, and never again
// after that: an `emptyDir` holds a few blocks of nothing in particular, and a series for each of
// them would cost every scrape and tell a reader nothing. A volume that passes the mark and later
// falls back under it keeps its series, since a series that flickers is worse than a small number
// (dispatch://INFRA-1208/artifact/plan-md#b-t-pod).
const volumeFloor = 1 << 20

// selfPID is the sampler's own process id, and a package var so that a test points the sampler's
// readings of itself at a process of its fixture rather than at the process running the test.
var selfPID = os.Getpid

// Every file the collector reads it reads through one of these, which are package vars as
// internal/launcher's roleInitLaunch is and procread's own readers everywhere but a test. Two
// of them are readings a test cannot write down — the cost of a smaps read is the kernel's, and
// a statfs answers for the filesystem the test's own directory sits on and not for the bytes
// the test wants to see — and the five together are the seam the cap is tested through: a test
// gives every read a cost of its own and drives a minute of passes over a pod of hundreds of
// processes, which is what the cap has to hold against and what no fixture's own files cost
// (pod_test.go's useCostlyReads). The walk reads each process's stat through readStat too, so
// what a pass of a busy pod spends is charged where it is spent.
var (
	readStat      = func(reader *procread.Reader, pid int) (procread.Stat, error) { return reader.Stat(pid) }
	readAnonymous = func(reader *procread.Reader, pid int) (int64, error) { return reader.Anonymous(pid) }
	readMemInfo   = func(reader *procread.Reader) (procread.MemInfo, error) { return reader.MemInfo() }
	readPodCPU    = func(reader *procread.Reader) (float64, error) { return reader.PodCPUSeconds() }
	readPodMemory = func(reader *procread.Reader) (int64, error) { return reader.PodMemoryBytes() }
	usedBytes     = procread.UsedBytes
)

// Roles is the launcher's roles file: each role's role-init process id, which is the root of that
// role's process subtree. The sampler reads the file again on every pass, so a generation that
// starts or ends between two passes is picked up without a restart, and a role between
// generations holds a pid that names no process, which the walk passes over.
//
// The type is exported because the launcher writes this file and the daemon reads it back: both
// sides render the same shape, and this is it.
type Roles map[string]int

// Volume is one entry of the pod's volumes file: the volume as the pod spec declares it and the
// paths this container sees it mounted at. A volume several roles mount holds one entry with one
// path for each mount, and is measured once — the first path that can be read — since every mount
// of it is the same filesystem and a sum over them would report it as many times as it is
// mounted.
//
// Medium is `memory` or `disk` as the pod spec declares it and never what /proc/mounts shows,
// where a disk emptyDir is a tmpfs too. Role is the role that owns the volume, and empty for one
// no single role owns, which is served as `shared`.
type Volume struct {
	Name   string   `json:"name"`
	Paths  []string `json:"paths"`
	Medium string   `json:"medium"`
	Role   string   `json:"role"`
}

// owner is the role label the volume's series carries.
func (v Volume) owner() string {
	if v.Role == "" {
		return RoleShared
	}
	return v.Role
}

// PodOptions is everything the issue pod's collector reads and everything it writes to. Every
// collaborator is injected — the root each procread call is given, the two files the launcher
// writes, the registry the readings land in, the clock and the log — so a test drives the whole
// loop over a directory of its own.
type PodOptions struct {
	// Root is the root every procread call is given: "/" in a pod, and a fixture directory
	// holding proc/… and sys/fs/cgroup/… in a test.
	Root string
	// Project and Issue are the pod's legion.dev/project and legion.dev/issue label values,
	// which the registry carries on every series and the log line below names the pod by.
	Project, Issue string
	// RolesFile is the launcher's roles file, JSON of role name to role-init process id
	// ({"implementer": 1234}), read again on every pass.
	RolesFile string
	// VolumesFile is the pod's volumes file, a JSON list of Volume, read once at the start.
	VolumesFile string
	// CgroupRoot is the per-command cgroup tree of dispatch://INFRA-1208/spec#b-a-cgroup: the
	// pod's own cgroup, holding one child cgroup named <role>-<id> for each command. Where it
	// is empty, or names a directory that is not there, the sampler reads the /proc profile.
	CgroupRoot string
	// Rules are the deployment's kind rules, which decide what a process's work is counted as.
	Rules Rules
	// Registry is where every reading goes, and what the HTTP handler serves.
	Registry *Registry
	// Now is the clock the collector keeps its own windows by. A nil clock is the wall clock.
	Now func() time.Time
	// Log takes one line about a read that failed or about the profile the sampler read.
	Log func(string)
	// Passed, where it is set, is called at the end of every pass the collector completes. It
	// is what GET /healthz reports, so that answer says a pod has been read and not that the
	// collector has been running.
	Passed func()
}

// check refuses options the collector could not run with, in the shape internal/repocache's own
// Options.check uses: the field, lower case, and what it must be.
func (opts PodOptions) check() error {
	switch {
	case opts.Root == "":
		return errors.New("sampler: root is required")
	case opts.RolesFile == "":
		return errors.New("sampler: roles file is required")
	case opts.Registry == nil:
		return errors.New("sampler: registry is required")
	case opts.Log == nil:
		return errors.New("sampler: log is required")
	}
	return nil
}

// RunPod reads this pod until its context ends. It returns ctx.Err(), as the repository's other
// loops do (internal/launcher's Run), and nothing short of the context stops it: a read that
// fails is counted as legion_sampler_read_errors_total under its own source and the next pass
// runs anyway, because a sampler that exits on a file it could not read reports nothing at all
// about a pod that is still running.
func RunPod(ctx context.Context, opts PodOptions) error {
	collector, err := newPodCollector(opts)
	if err != nil {
		return err
	}
	// The descriptors this collector kept open are released with the loop that read through
	// them, whatever ends it.
	defer collector.close()
	ticker := time.NewTicker(podInterval)
	defer ticker.Stop()
	// The first pass runs before the first tick, so a scrape that arrives a moment after the
	// sampler started is answered with readings rather than with an empty registry.
	for tick := int64(0); ; tick++ {
		collector.pass(tick)
		if opts.Passed != nil {
			opts.Passed()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// podCollector is one RunPod's state between passes: what it reads from, what each pass left
// behind for the next one, and the last message it logged for each source, so a file that fails
// to be read every second is reported once and not sixty times a minute.
type podCollector struct {
	opts   PodOptions
	layout procread.Layout
	// cgroups is the cgroup profile: CgroupRoot names a directory that is there.
	cgroups bool
	// reader is every file this collector reads again and again, kept open: the pod's
	// meminfo, the two cgroup files of its layout, and the stat and smaps of each process
	// the walk below is holding. The walk reads its own stats through it, so one reader
	// holds one descriptor of each file however many of this collector's readings want it.
	reader   *procread.Reader
	walk     *Walk
	pageSize int64
	// totalsEvery, walkEvery and smapsEvery are those three rates in ticks of podInterval,
	// and totalsFloor is how many ticks the memory totals may be made to wait: totalsSlowed
	// of their own ticks, which is the 4 seconds they slow to under the cap.
	totalsEvery, walkEvery, smapsEvery int64
	totalsFloor                        int64

	// roles is the last roles file that could be read, which stands until another one can be.
	roles Roles
	// volumes is the volumes file, read once; volumesRead is whether that read has succeeded,
	// since a file that was not there at the start is tried again rather than given up on.
	volumes     []Volume
	volumesRead bool
	// reported are the volumes that have passed volumeFloor, by name, which is what keeps a
	// volume's series from flickering once it has been served.
	reported map[string]bool

	// processes is the last walk's processes, largest resident first, which is what the smaps
	// pass reads; carried are the process ids the last smaps window did not reach, in the
	// order it would have reached them, so the next window takes them up where it left off
	// rather than reading the largest again.
	processes []Process
	carried   []int

	// windowCPU is the sampler's own CPU when this smaps window began, which is the moment
	// the window before it stopped reading, and windowMeasured whether that reading could be
	// taken at all. What the sampler has spent since is the rest of this window's pass work,
	// and what the smaps pass may spend is the window's allowance less that (smapsLeft).
	windowCPU      float64
	windowMeasured bool

	// walkDeferred and totalsDeferred are how many walk passes and how many readings of the
	// pod's memory totals this collector's own cap has deferred, which it serves as
	// legion_sampler_walk_deferred and legion_sampler_totals_deferred. lastWalk and
	// lastTotals are the ticks each of them last ran on, which is what holds the floor under
	// the deferring: the walk waits no longer than a window and the memory totals no longer
	// than totalsFloor (readsWalk, readsTotals). walkCost is what the last walk pass spent,
	// which is what says whether this window can pay for another (chargeWalk).
	walkDeferred, totalsDeferred uint64
	lastWalk, lastTotals         int64
	walkCost                     float64

	// procErrors are the /proc reads this collector failed, beside the walk's own count.
	procErrors uint64

	// groups is the cgroup profile's memory of each command cgroup, including cgroups that are
	// gone: a removed cgroup keeps the last values read until the hour window drops it, which
	// is what docs/sampler-metrics.md promises whoever writes those cgroups.
	groups map[string]*groupReading

	logged map[ReadSource]string
}

// groupReading is one command cgroup as the passes have read it: the pair its series are charged
// to, the last reading of its files, the most it is known to have held within this scrape's
// window, and when it was last there to be read. The next pass's rise is taken from that last
// reading itself — the CPU already charged is its CPUSeconds and the peak already seen its
// MemoryPeakBytes — so neither is held a second time beside it.
type groupReading struct {
	pair   Pair
	group  procread.Group
	within int64
	seen   time.Time
}

func newPodCollector(opts PodOptions) (*podCollector, error) {
	if err := opts.check(); err != nil {
		return nil, err
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	layout := procread.DetectLayout(opts.Root)
	reader := procread.NewReader(opts.Root, layout)
	c := &podCollector{
		opts:        opts,
		layout:      layout,
		cgroups:     cgroupTreePresent(opts.CgroupRoot),
		reader:      reader,
		walk:        NewWalk(opts.Root, opts.Rules, reader),
		pageSize:    int64(os.Getpagesize()),
		totalsEvery: ticksPer(totalsInterval, podInterval),
		walkEvery:   ticksPer(walkInterval, podInterval),
		smapsEvery:  ticksPer(smapsInterval, podInterval),
		roles:       Roles{},
		volumesRead: opts.VolumesFile == "",
		reported:    map[string]bool{},
		groups:      map[string]*groupReading{},
		logged:      map[ReadSource]string{},
	}
	c.totalsFloor = totalsSlowed * c.totalsEvery
	// Both readings stand their own floor behind the first tick, so tick zero reads
	// everything however much the sampler's own first reading cost (readsWalk, readsTotals).
	c.lastWalk, c.lastTotals = -c.smapsEvery, -c.totalsFloor
	// The first window begins where the collector does, so the walk and the totals of its
	// first ten seconds stand inside the allowance as every later window's do.
	c.windowCPU, c.windowMeasured = c.selfCPUSeconds()
	c.opts.Log(fmt.Sprintf("sampler: %s/%s: reading %s as the %s profile, cgroup layout %s",
		opts.Project, opts.Issue, opts.Root, c.profile(), c.layout))
	return c, nil
}

// close releases every descriptor this collector kept open. A collector whose loop has ended
// reads nothing more, and a reader that was not closed would hold a descriptor of each process
// of the pod for as long as the process that made it runs.
func (c *podCollector) close() {
	if err := c.reader.Close(); err != nil {
		c.opts.Log("sampler: the files it had kept open could not all be released: " + err.Error())
	}
}

// profile is the name of the profile this collector reads, for the one log line that names it.
func (c *podCollector) profile() string {
	if c.cgroups {
		return "cgroup"
	}
	return "/proc"
}

// cgroupTreePresent is whether the per-command cgroup tree is there to read. A pod that declares
// no tree, and one whose tree the kernel does not write, is the /proc profile.
func cgroupTreePresent(root string) bool {
	if root == "" {
		return false
	}
	info, err := os.Stat(root)
	return err == nil && info.IsDir()
}

// ticksPer is how many ticks of a loop's own rate one of its slower rates stands for. A rate no
// slower than the tick runs on every one of them, which is what a test that shortens one rate
// and not another gets. Both collectors count their slower rates this way, each in ticks of its
// own loop: podInterval here and nodeInterval in node mode.
func ticksPer(interval, tick time.Duration) int64 {
	if tick <= 0 || interval <= tick {
		return 1
	}
	return int64(interval / tick)
}

// pass is one tick of the loop: the sampler's own readings and the pod's CPU total always, then
// the pod's memory totals, the process walk and the volumes at whatever rate the window's
// allowance can pay for, and every smapsEvery ticks the smaps window. Tick zero runs all of
// them, so nothing waits ten seconds for its first reading.
//
// What the allowance will not pay for gives way, and never the cap: the walk and the volumes
// wait for a later pass as often as the window needs (readsWalk), the pod's memory totals fall
// from their own two-second rate to one reading every four seconds (readsTotals), and the smaps
// pass reads what those two leave (smapsLeft).
// Under all three is one floor: a window whose allowance is spent still runs one walk and one
// volume read before it ends and never leaves the memory totals longer than totalsFloor, so
// nothing the sampler serves goes staler than the ten seconds of a window.
//
// The sampler's own readings and the pod's CPU are not among what waits. The sampler's two are
// files of its own, read through the descriptors it holds on them, and the pod's CPU is one
// small cgroup file: all three together cost about a millisecond in this pod, against the tens
// of milliseconds its two memory readings cost (totalsInterval). The first of them is the CPU
// every budget here is measured against: the pass takes that reading once and makes every
// decision below from it, rather than reading the same file again for each.
//
// The smaps window closes on its own tick whether or not that tick deferred anything, since a
// window that never closes is a window whose allowance never begins again. A window with
// nothing left to spend reads no process there and says so (readSmaps).
func (c *podCollector) pass(tick int64) {
	spent, measured := readSelf(c.reader, c.pageSize, c.opts.Registry, c.failed)
	left := c.windowLeft(spent, measured)
	c.readCPUTotal()
	if tick%c.totalsEvery == 0 && c.readsTotals(tick, left) {
		c.readMemoryTotals()
	}
	if tick%c.walkEvery == 0 && c.readsWalk(tick, left) {
		c.readProcesses()
		c.readVolumes()
		c.chargeWalk(spent, measured)
	}
	// The cgroup profile reads each command cgroup's memory.stat for its anonymous memory, so
	// the costly per-process read belongs to the /proc profile alone.
	if !c.cgroups && tick%c.smapsEvery == 0 {
		c.readSmaps()
	}
}

// windowLeft is what this window has left of the sampler's own allowance, which is 3% of one
// CPU (dispatch://INFRA-1208/spec#a3) measured from /proc/self/stat as every budget here is,
// taken against the reading this pass began with.
//
// A sampler that cannot read its own CPU measures nothing and defers nothing: the walk is the
// reading the pod's series are made of, and a budget that cannot be measured is no reason to
// stop taking it.
func (c *podCollector) windowLeft(spent float64, measured bool) float64 {
	if !measured || !c.windowMeasured {
		return windowAllowance()
	}
	return windowAllowance() - (spent - c.windowCPU)
}

// readsTotals is whether this totals tick reads the pod's memory: every one of them while the
// window has something left to spend, and every second one once the allowance is gone, which is
// the two-second rate giving way at four seconds and never the cap. Each tick that gives way is
// counted as legion_sampler_totals_deferred, the series an operator reads the slowdown off.
//
// The memory totals are the dearest reading the sampler makes on a gVisor pod and the first to
// give way, because they are the one reading whose rate the cap's rule allows to halve
// (dispatch://INFRA-1208/spec#a3). They are weighed against the window's allowance alone and
// have no budget of their own inside it: one would hold them under the rate the rule fixes for
// them, since the five readings of a ten-second window are most of what an idle pod costs the
// sampler at all, which was 95 ms to 140 ms of the 250 ms a window has (INFRA-1208, measured
// 2026-11-05).
//
// Four seconds is far inside the ten the walk's floor allows, so nothing the memory totals feed
// goes staler than a window either.
func (c *podCollector) readsTotals(tick int64, left float64) bool {
	if left > 0 || tick-c.lastTotals >= c.totalsFloor {
		c.lastTotals = tick
		return true
	}
	c.totalsDeferred++
	c.opts.Registry.SetTotalsDeferred(c.totalsDeferred)
	return false
}

// readsWalk is whether this walk tick runs its walk and its volumes: the window must have
// enough of the allowance left to pay for them — what the last walk pass cost, measured the way
// every budget here is (chargeWalk) — and a window that has not defers this pass and as many
// after it as the window needs.
//
// The walk is what a busy pod costs a sampler: a build makes thousands of short-lived
// processes, each one a stat to open, and under gVisor a syscall under contention costs several
// times what it costs on an idle pod. Where the readings of a window have spent the allowance,
// the resolution is what gives way and never the cap, and how often that happens is
// legion_sampler_walk_deferred, which is where an operator reads that this pod is busy enough
// to cost the sampler its cap.
//
// The one thing the deferring may not do is let a window pass with no walk in it. A walk that
// has not run for a whole window runs whatever the allowance says, so every process's stat and
// every volume is read at least once each ten seconds and no series the walk feeds goes staler
// than that. That floor and the memory totals' own four seconds are the only things the cap
// gives way to.
func (c *podCollector) readsWalk(tick int64, left float64) bool {
	if (left > 0 && left >= c.walkCost) || tick-c.lastWalk >= c.smapsEvery {
		c.lastWalk = tick
		return true
	}
	c.walkDeferred++
	c.opts.Registry.SetWalkDeferred(c.walkDeferred)
	return false
}

// chargeWalk is what the walk pass that has just run cost: the sampler's own CPU from the
// reading this pass began with to one taken now. That is what the next walk tick's decision is
// made against, and it is a whole walk tick's cost — the pod's CPU, and the memory totals where
// this tick read them, stand in it beside the walk's own reads — since a walk tick is what the
// decision is about.
//
// It is one reading of a file the collector holds open, taken on a walk tick alone, and it
// stands in for the reading the walk's own rule used to take on every one of them.
func (c *podCollector) chargeWalk(spent float64, measured bool) {
	after, ok := c.selfCPUSeconds()
	if measured && ok {
		c.walkCost = after - spent
	}
}

// readCPUTotal is the CPU the pod's cgroup has spent, the cheap half of its totals and the one
// the loop reads every second: under the cgroup profile the pod's own cgroup answers for it, and
// otherwise the cgroup filesystem's mount root does. It is never deferred — 0.00 ms to 0.50 ms
// in this pod, against the tens of milliseconds a reading of the pod's memory costs there
// (totalsInterval) — so the series a dashboard reads a pod's CPU off moves every second
// however busy the pod is.
func (c *podCollector) readCPUTotal() {
	var cpu float64
	var err error
	if c.cgroups {
		cpu, err = procread.GroupCPUSeconds(c.opts.CgroupRoot)
	} else {
		cpu, err = readPodCPU(c.reader)
	}
	if err != nil {
		c.failed(SourceCgroup, err)
		return
	}
	c.opts.Registry.SetPodCPUSeconds(cpu)
}

// readMemoryTotals is the memory the pod holds and the memory its processes hold: the pod
// cgroup's own memory and /proc/meminfo, the two dear readings of a gVisor pod, which the loop
// takes every two seconds and every four while the window's allowance is spent (readsTotals).
func (c *podCollector) readMemoryTotals() {
	var memory int64
	var err error
	if c.cgroups {
		memory, err = procread.GroupMemoryBytes(c.opts.CgroupRoot)
	} else {
		memory, err = readPodMemory(c.reader)
	}
	if err != nil {
		c.failed(SourceCgroup, err)
	} else {
		c.opts.Registry.SetPodMemoryBytes(memory)
	}

	info, err := readMemInfo(c.reader)
	if err != nil {
		c.failed(SourceProc, err)
		return
	}
	// AnonPages, plus Shmem on a kernel that keeps the two apart (docs/sampler-metrics.md, "The
	// /proc profile"). The unified hierarchy is that kernel: a microVM and a node present
	// cgroup v2 and account a tmpfs to Shmem alone, while the gVisor pod that presents cgroup
	// v1 at the mount root already holds its tmpfs pages in AnonPages — 743,092 kB of AnonPages
	// beside 56 kB of Shmem, measured in this pod on 2026-11-05 — so adding them there would
	// count the same pages twice.
	process := info.AnonPages
	if c.layout == procread.LayoutV2 {
		process += info.Shmem
	}
	c.opts.Registry.SetPodProcessMemoryBytes(process)
	c.opts.Registry.SetPodSharedMemoryBytes(info.Shmem)
}

// readSelf is what the sampler costs the pod or the node it reads: its own CPU, the anonymous
// memory its exit rule is written against, and the resident memory that under gVisor counts the
// mapped legion binary. Both modes serve these three series (docs/sampler-metrics.md, "The
// series the sampler serves about itself") and both read them the same way, so this is one body
// given each collector's own reader, page size, registry and count of what failed. A reading
// that fails leaves the last one standing rather than serving a zero.
//
// It answers the CPU it read and whether it could be read, because that reading is the one
// every budget of the pass below it is measured against: the pass takes it here, once, and
// decides what it can afford to read from what this answers (podCollector.pass).
//
// It is also the one read of /proc/self/smaps the sampler makes each second: the memory guard
// takes the reading this publishes rather than a second one of its own (service.overLimit).
// Both files are the sampler's own, read every second of its life, so both are read through the
// descriptors the reader keeps on them.
func readSelf(reader *procread.Reader, pageSize int64, registry *Registry, failed func(ReadSource, error)) (float64, bool) {
	stat, err := readStat(reader, selfPID())
	if err != nil {
		failed(SourceProc, err)
		return 0, false
	}
	cpu := seconds(stat.UserTicks + stat.SystemTicks)
	registry.SetSelfCPUSeconds(cpu)
	anonymous, err := readAnonymous(reader, selfPID())
	if err != nil {
		failed(SourceProc, err)
		return cpu, true
	}
	registry.SetSelfMemoryBytes(anonymous, int64(stat.ResidentPages)*pageSize)
	return cpu, true
}

// readProcesses is one pass of the process walk: the roles file as it stands now, every process's
// stat, and then the numbers of whichever profile this pod is. The walk runs under both profiles,
// because the cgroup profile reads a command's kind from the processes in its cgroup and only the
// walk knows what a process is doing.
func (c *podCollector) readProcesses() {
	c.readRoles()
	pass, err := c.walk.Next(c.roles)
	if err != nil {
		c.failed(SourceProc, err)
		return
	}
	c.publishProcErrors()
	c.processes = pass.Processes
	if c.cgroups {
		c.readCgroups(pass)
		return
	}
	for pair, spent := range pass.CPUSeconds {
		c.opts.Registry.AddCPUSeconds(pair, spent)
	}
	resident := map[Pair]int64{}
	for _, process := range pass.Processes {
		resident[process.Pair] += process.ResidentBytes
	}
	for pair, bytes := range resident {
		c.opts.Registry.SetResidentBytes(pair, bytes)
	}
}

// readRoles takes the roles file as it stands for this pass. A file that is missing or half
// written — a reader can catch the launcher's rename — leaves the last good map in place and
// raises a read error, since the alternative is to report every role's processes as shared for as
// long as the file is unreadable.
func (c *podCollector) readRoles() {
	raw, err := os.ReadFile(c.opts.RolesFile)
	if err != nil {
		c.failed(SourceProc, err)
		return
	}
	var roles Roles
	if err := json.Unmarshal(raw, &roles); err != nil {
		c.failed(SourceProc, fmt.Errorf("%s: %w", c.opts.RolesFile, err))
		return
	}
	c.roles = roles
}

// readVolumes is each volume's used bytes, one statfs for each volume however many roles mount
// it. The volumes file is read once, and again on a later pass where that read failed, since a
// file written a moment after the sampler started is no reason to serve no volume for the pod's
// whole life.
func (c *podCollector) readVolumes() {
	if !c.volumesRead {
		volumes, err := readVolumesFile(c.opts.VolumesFile)
		if err != nil {
			c.failed(SourceVolume, err)
			return
		}
		c.volumes, c.volumesRead = volumes, true
	}
	for _, volume := range c.volumes {
		used, err := c.volumeUsed(volume)
		if err != nil {
			c.failed(SourceVolume, err)
			continue
		}
		if !c.reported[volume.Name] {
			if used < volumeFloor {
				continue
			}
			c.reported[volume.Name] = true
		}
		c.opts.Registry.SetVolumeBytes(volume.Name, volume.Medium, volume.owner(), used)
	}
}

// readVolumesFile is the pod's volumes file: the JSON list the launcher writes.
func readVolumesFile(path string) ([]Volume, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var volumes []Volume
	if err := json.Unmarshal(raw, &volumes); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return volumes, nil
}

// volumeUsed is the bytes used in one volume: the first of its paths that can be read, since each
// path is another role's mount of the same filesystem and reading two of them would answer twice.
// A volume none of whose paths can be read is the error of the first one, which is the one the
// pod spec names first.
func (c *podCollector) volumeUsed(volume Volume) (int64, error) {
	var first error
	for _, path := range volume.Paths {
		used, err := usedBytes(path)
		if err == nil {
			return used, nil
		}
		if first == nil {
			first = err
		}
	}
	if first == nil {
		return 0, fmt.Errorf("volume %s names no path to read", volume.Name)
	}
	return 0, first
}

// readSmaps is one window of the smaps pass: each process's anonymous memory, largest resident
// process first, within what this window has left of the sampler's own allowance (smapsLeft).
// What the window did not reach is carried into the next one and served as
// legion_sampler_smaps_deferred, and the carried processes go first there, so every process is
// reached within the windows the remainder needs rather than the largest being read again and
// again.
//
// The budget is the sampler's own CPU and never a share of the wall clock: /proc/self/stat is
// what the pod is charged for, and a sampler that was descheduled for 300 ms spent none of it.
//
// The window closes here, with the reading this pass ends on: what the next window measures its
// own work against is the CPU the sampler had spent when it stopped reading.
func (c *podCollector) readSmaps() {
	order := c.smapsOrder()
	began, measured := c.selfCPUSeconds()
	budget := c.smapsLeft(began, measured)
	anonymous := map[Pair]int64{}
	read := 0
	for budget > 0 && read < len(order) {
		process := order[read]
		bytes, err := readAnonymous(c.reader, process.PID)
		read++
		switch {
		case unreadable(err):
			// A process that ended between the walk and this read, and one this
			// process may not read, are both the ordinary course of a pass.
		case err != nil:
			c.failed(SourceProc, err)
		default:
			anonymous[process.Pair] += bytes
		}
		// The budget is read after the process rather than before it, so a window with
		// anything at all to spend reaches at least one process and the pass always moves
		// forward. A sampler that cannot read its own CPU cannot measure the budget at
		// all, and reads one process a window rather than running unbounded.
		spent, ok := c.selfCPUSeconds()
		if !measured || !ok || spent-began >= budget {
			break
		}
	}
	// Only the pairs this window reached are set: a pair no process of which was read keeps the
	// value of the window that last read one, which is a reading of its memory from ten or
	// twenty seconds ago rather than a zero it never held.
	for pair, bytes := range anonymous {
		c.opts.Registry.SetAnonymousBytes(pair, bytes)
	}
	c.carried = make([]int, 0, len(order)-read)
	for _, process := range order[read:] {
		c.carried = append(c.carried, process.PID)
	}
	c.opts.Registry.SetSmapsDeferred(len(c.carried))
	spent, measured := c.selfCPUSeconds()
	c.windowCPU, c.windowMeasured = c.nextWindowCPU(spent, measured), measured
}

// nextWindowCPU is where the next window's allowance is measured from: what the sampler had
// spent when this window stopped reading, less whatever this window spent over its allowance.
//
// A window that went over takes the overspend out of the next one, because the cap is over a
// minute and not over one window of it (docs/sampler-metrics.md). The floor is the one reading
// rate that never gives way, so a window whose forced walk cost more than the allowance is paid
// for by the window after it, which reads nothing it does not have to. Without that, a pod
// saturated for minutes spends its whole allowance again every ten seconds and the overspend
// of each window is forgiven: this one held 3.49% of one CPU over the worst minute of a
// `go build -a ./...` and 3.03% over the whole of it, against a cap of 3% (INFRA-1208, measured
// 2026-11-05).
//
// The debt is never more than one window's allowance. The cap is a minute's, and a sampler that
// fell behind once must read at the floor until it has paid and not for the rest of the pod's
// life.
//
// A window that spent less than its allowance carries no credit the other way: an idle minute
// does not buy a busy one the right to read twice as much.
func (c *podCollector) nextWindowCPU(spent float64, measured bool) float64 {
	if !measured || !c.windowMeasured {
		return spent
	}
	allowance := windowAllowance()
	return max(min(spent, c.windowCPU+allowance), spent-allowance)
}

// smapsLeft is what this window may spend reading smaps: the sampler's whole allowance for a
// window less the pass work the window has already spent, and never more than smapsBudget.
//
// The cap is the requirement and the rates are the means (dispatch://INFRA-1208/spec#a3: 3% of
// one CPU over any minute). Holding the costly read alone inside a budget holds nothing: the
// per-second and the per-two-second work is most of what a sampler over a pod of hundreds of
// processes spends, and this one spent 3.6% of one CPU over a live run of 319 s with its smaps
// budget honoured (INFRA-1208, measured 2026-11-05). So the walk and the memory totals are
// measured the same way the smaps read is, from /proc/self/stat, and smaps spends what they
// leave.
//
// A window whose other work spent the whole allowance reads no process at all and defers every
// one of them, which legion_sampler_smaps_deferred reports: the operator's signal that the
// sampler is holding its cap against a pod busy enough to cost it one.
//
// A sampler that cannot read its own CPU measures nothing and falls back to the ceiling, which
// the reading loop's own rule then cuts to one process a window.
func (c *podCollector) smapsLeft(began float64, measured bool) float64 {
	if !measured || !c.windowMeasured {
		return smapsBudget.Seconds()
	}
	return min(smapsBudget.Seconds(), windowAllowance()-(began-c.windowCPU))
}

// smapsOrder is the processes of this window: the ones the last window did not reach first, in
// the order it left them, and then the rest largest resident memory first, which is the walk's
// own order. A carried process that has ended since is passed over, since the walk no longer
// reports it.
func (c *podCollector) smapsOrder() []Process {
	if len(c.carried) == 0 {
		return c.processes
	}
	live := make(map[int]Process, len(c.processes))
	for _, process := range c.processes {
		live[process.PID] = process
	}
	carried := make(map[int]bool, len(c.carried))
	order := make([]Process, 0, len(c.processes))
	for _, pid := range c.carried {
		process, running := live[pid]
		if !running || carried[pid] {
			continue
		}
		carried[pid] = true
		order = append(order, process)
	}
	for _, process := range c.processes {
		if !carried[process.PID] {
			order = append(order, process)
		}
	}
	return order
}

// selfCPUSeconds is the CPU the sampler's own process has spent, which is what the smaps budget
// is measured against, and whether it could be read at all.
func (c *podCollector) selfCPUSeconds() (float64, bool) {
	stat, err := readStat(c.reader, selfPID())
	if err != nil {
		c.failed(SourceProc, err)
		return 0, false
	}
	return seconds(stat.UserTicks + stat.SystemTicks), true
}

// readCgroups is the cgroup profile's pass: every command cgroup under CgroupRoot, each giving
// its role from the first segment of its name, its kind from the first build or test process in
// it, its CPU from cpu.stat and its memory from memory.current, memory.peak and memory.stat.
//
// A cgroup that disappears between two passes keeps the last values read, and goes on standing in
// its pair's totals until the hour window would have dropped the pair anyway. That is what
// docs/sampler-metrics.md tells whoever writes those cgroups: keep an ended command's cgroup
// until the sampler has read it once more, and the final numbers are reported; remove it at once
// and the last ones read stand in their place.
func (c *podCollector) readCgroups(pass Pass) {
	names, err := procread.ChildGroups(c.opts.CgroupRoot)
	if err != nil {
		c.failed(SourceCgroup, err)
	}
	kinds := make(map[int]Kind, len(pass.Processes))
	for _, process := range pass.Processes {
		kinds[process.PID] = process.Pair.Kind
	}
	// A cgroup this pass does not read holds what it held: the peak it is known to have
	// reached within this scrape's window is its last memory reading and nothing above it.
	for _, reading := range c.groups {
		reading.within = reading.group.MemoryBytes
	}
	now := c.opts.Now()
	for _, name := range names {
		dir := filepath.Join(c.opts.CgroupRoot, name)
		group, err := procread.ReadGroup(dir)
		if err != nil {
			c.failed(SourceCgroup, err)
			continue
		}
		reading := c.groups[name]
		if reading == nil {
			reading = &groupReading{}
			c.groups[name] = reading
		}
		reading.pair = Pair{Role: c.groupRole(name), Kind: c.groupKind(dir, kinds, reading.pair.Kind)}
		// memory.peak is the most the cgroup has ever held. Where it has risen since the
		// last pass, that much was held since the last pass, which is what the registry's
		// peak series is for; where it has not, the reading of this moment is all this
		// window knows the cgroup to have held.
		reading.within = group.MemoryBytes
		if group.MemoryPeakBytes > reading.group.MemoryPeakBytes {
			reading.within = group.MemoryPeakBytes
		}
		// The CPU a cgroup reports is a total since it was made, and the registry is
		// charged the rise alone. A total below the one already charged is another cgroup
		// wearing a name that was used before, whose own total is the rise to charge.
		rise := group.CPUSeconds - reading.group.CPUSeconds
		if group.CPUSeconds < reading.group.CPUSeconds {
			rise = group.CPUSeconds
		}
		reading.group = group
		reading.seen = now
		c.opts.Registry.AddCPUSeconds(reading.pair, rise)
	}
	c.chargeCgroupMemory(now)
}

// chargeCgroupMemory sums what every cgroup holds into its pair, over the cgroups that are there
// and the ones that have gone since, and drops a cgroup the hour window has passed over: a pair
// whose series have left the registry has no use for the last reading of a cgroup that ended an
// hour ago, and that is what bounds this map.
func (c *podCollector) chargeCgroupMemory(now time.Time) {
	resident := map[Pair]int64{}
	peak := map[Pair]int64{}
	anonymous := map[Pair]int64{}
	for name, reading := range c.groups {
		if now.Sub(reading.seen) > activeWindow {
			delete(c.groups, name)
			continue
		}
		resident[reading.pair] += reading.group.MemoryBytes
		peak[reading.pair] += reading.within
		anonymous[reading.pair] += reading.group.AnonBytes
	}
	for pair, bytes := range resident {
		// The peak goes in first and the reading of this moment second: the registry keeps
		// the most it was handed since the last scrape beside the last value handed to it,
		// so this serves the memory the pair holds now and the most it held in between.
		if high := peak[pair]; high > bytes {
			c.opts.Registry.SetResidentBytes(pair, high)
		}
		c.opts.Registry.SetResidentBytes(pair, bytes)
	}
	for pair, bytes := range anonymous {
		c.opts.Registry.SetAnonymousBytes(pair, bytes)
	}
}

// groupRole is the role a command cgroup's series are charged to: the first segment of its name.
// A name whose first segment is no role of this pod is `shared` — the same role a process in no
// role's subtree is counted under — so a tree that turns out to be named otherwise than
// docs/sampler-metrics.md says degrades into one series rather than inventing a role for each
// cgroup it meets.
func (c *podCollector) groupRole(name string) string {
	role, _, _ := strings.Cut(name, "-")
	if _, known := c.roles[role]; !known {
		return RoleShared
	}
	return role
}

// groupKind is what a command cgroup's work is counted as: the kind of the first build or test
// process in it, and `tool` where it holds neither. The processes are the cgroup's own
// membership and their kinds are the walk's, which read each command line once.
//
// A cgroup holding nobody keeps the kind the last pass gave it, and so does one whose membership
// cannot be read. That is the cgroup of a command whose processes have ended, kept for the
// sampler's last read of it: its final numbers belong where every reading before them went, and
// not under `tool` because the command that earned them has exited.
func (c *podCollector) groupKind(dir string, kinds map[int]Kind, last Kind) Kind {
	pids, err := groupProcesses(dir)
	if err != nil {
		c.failed(SourceCgroup, err)
		pids = nil
	}
	for _, pid := range pids {
		switch kinds[pid] {
		case KindBuild:
			return KindBuild
		case KindTest:
			return KindTest
		}
	}
	if len(pids) == 0 && last != "" {
		return last
	}
	return KindTool
}

// groupProcesses are the process ids one cgroup holds. It is the one read of the cgroup
// filesystem this package makes for itself: internal/procread reads what a cgroup accounts for,
// and which processes are in it is not one of those files. A cgroup that has been removed holds
// nobody, which is no failure — a command ends and its cgroup goes with it.
func groupProcesses(dir string) ([]int, error) {
	path := filepath.Join(dir, "cgroup.procs")
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(string(raw))
	pids := make([]int, 0, len(fields))
	for _, field := range fields {
		pid, err := strconv.Atoi(field)
		if err != nil {
			return nil, fmt.Errorf("%s: %q is no process id", path, field)
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

// failed counts one read that failed under its own source and logs it once. The loop goes on:
// every reading here is independent of every other, and a pod whose volumes cannot be read still
// reports its CPU.
//
// A /proc failure is counted beside the walk's own, which the walk keeps itself, so that the two
// counts are one number that only rises; the other sources are counted one failure at a time.
func (c *podCollector) failed(source ReadSource, err error) {
	if source == SourceProc {
		c.procErrors++
		c.publishProcErrors()
	} else {
		c.opts.Registry.AddReadError(source)
	}
	message := fmt.Sprintf("sampler: %s read failed: %v", source, err)
	if c.logged[source] == message {
		return
	}
	c.logged[source] = message
	c.opts.Log(message)
}

// publishProcErrors serves the /proc reads that failed: the walk's own count, which it keeps
// between passes, and this collector's.
func (c *podCollector) publishProcErrors() {
	c.opts.Registry.SetReadErrors(SourceProc, c.procErrors+c.walk.ReadErrors())
}
