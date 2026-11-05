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
// second, what every second second and what every ten — the budget every one of those reads is
// held inside, and the two profiles of docs/sampler-metrics.md, the `/proc` profile of a gVisor
// pod and the cgroup profile of a pod that runs each command in a cgroup of its own.
//
// Nothing here opens a path of its own: every read is given PodOptions.Root or a path one of the
// launcher's two files names, so a test drives a whole pod's worth of passes over a directory it
// wrote, with no real clock, no real /proc and no sleep.

// The rates, each a package var rather than a constant so that a test runs a hundred passes in
// the time a pod spends on one (internal/launcher/testpg.go:42 is the same pattern). The slower
// rates are counted in ticks of podInterval, so shortening all four keeps the passes in step.
var (
	// podInterval is the loop's own tick: the pod's totals, its process memory and the
	// sampler's own readings, which are three small files and cost nothing.
	podInterval = time.Second
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

// samplerShare is the whole of the sampler's own cost: 2% of one CPU
// (dispatch://INFRA-1208/spec#b-a-rates), which is the requirement, where the rates above are
// the means. The cap is held over the smaps window, which is the longest of them, and over
// every pass of that window: what the window has spent decides what the smaps pass may spend
// (smapsLeft) and whether the next walk pass runs at all (deferWalk).
const samplerShare = 0.02

// windowAllowance is the sampler's own CPU for one smaps window: samplerShare of one CPU over
// smapsInterval, which is 200 ms of every ten seconds. It is read off the rate rather than
// written down a second time, so a test that shortens the window shortens what the window may
// spend with it.
func windowAllowance() float64 { return smapsInterval.Seconds() * samplerShare }

// volumeFloor is the used bytes a volume must reach before its series is served, and never again
// after that: an `emptyDir` holds a few blocks of nothing in particular, and a series for each of
// them would cost every scrape and tell a reader nothing. A volume that passes the mark and later
// falls back under it keeps its series, since a series that flickers is worse than a small number
// (dispatch://INFRA-1208/artifact/plan-md#b-t-pod).
const volumeFloor = 1 << 20

// selfPID is the sampler's own process id, and a package var so that a test points the sampler's
// readings of itself at a process of its fixture rather than at the process running the test.
var selfPID = os.Getpid

// readAnonymous and usedBytes are the two readings a test cannot write down: the cost of a smaps
// read is the kernel's, and a statfs answers for the filesystem the test's own directory sits on
// and not for the bytes the test wants to see. Both are package vars for that reason, as
// internal/launcher's roleInitLaunch is, and both are procread's own readers everywhere else —
// the smaps read through the collector's own reader, which keeps that file's descriptor.
var (
	readAnonymous = func(reader *procread.Reader, pid int) (int64, error) { return reader.Anonymous(pid) }
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
	// walkEvery and smapsEvery are those two rates in ticks of podInterval.
	walkEvery, smapsEvery int64

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

	// walkDeferred is how many walk passes this collector's own cap has deferred, which it
	// serves as legion_sampler_walk_deferred, and deferredLast whether the last walk tick was
	// one of them: a deferral is never two in a row (deferWalk).
	walkDeferred uint64
	deferredLast bool

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
		walkEvery:   ticksPer(walkInterval, podInterval),
		smapsEvery:  ticksPer(smapsInterval, podInterval),
		roles:       Roles{},
		volumesRead: opts.VolumesFile == "",
		reported:    map[string]bool{},
		groups:      map[string]*groupReading{},
		logged:      map[ReadSource]string{},
	}
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

// pass is one tick of the loop: always the sampler's own readings, the pod's totals unless this
// tick's work is deferred, every walkEvery ticks the process walk and the volumes, and every
// smapsEvery ticks the smaps window. Tick zero runs all of them, so nothing waits ten seconds
// for its first reading.
//
// A walk tick whose window has already spent the sampler's allowance reads nothing but the
// sampler itself: the walk, the volumes and the pod totals wait for the tick after it
// (deferWalk). The sampler's own readings are not among what waits — they are two files of its
// own, read through the descriptors it holds on them, and the first is the CPU every budget
// here is measured against.
//
// The smaps window closes on its own tick whether or not that tick was deferred, since a window
// that never closes is a window whose allowance never begins again. A window with nothing left
// to spend reads no process there and says so (readSmaps).
func (c *podCollector) pass(tick int64) {
	walking := tick%c.walkEvery == 0
	deferring := walking && c.deferWalk()
	if !deferring {
		c.readTotals()
	}
	readSelf(c.reader, c.pageSize, c.opts.Registry, c.failed)
	if walking && !deferring {
		c.readProcesses()
		c.readVolumes()
	}
	// The cgroup profile reads each command cgroup's memory.stat for its anonymous memory, so
	// the costly per-process read belongs to the /proc profile alone.
	if !c.cgroups && tick%c.smapsEvery == 0 {
		c.readSmaps()
	}
}

// deferWalk is whether this walk pass waits for the pass after it: the window's work has already
// spent the sampler's whole allowance for a window, which is 2% of one CPU
// (dispatch://INFRA-1208/spec#b-a-rates), measured from /proc/self/stat as every budget here is.
//
// The cap holds whatever the pod runs, and the walk is what a busy pod costs a sampler: a build
// makes thousands of short-lived processes, each one a stat to open, and under gVisor a syscall
// under contention costs several times what it costs on an idle pod. Where the readings of a
// window have spent the allowance, the resolution is what gives way and never the cap.
//
// A deferral is never two in a row. So a process's stat is read at least every two walk
// intervals — four seconds while the pod is saturated, two otherwise — and the sampler reports
// a pod it reads half as often rather than one it has stopped reading. How often that happens is
// legion_sampler_walk_deferred, which is where an operator reads that this pod is busy enough to
// cost the sampler its cap.
//
// A sampler that cannot read its own CPU measures nothing and defers nothing: the walk is the
// reading the pod's series are made of, and a budget that cannot be measured is no reason to
// stop taking it.
func (c *podCollector) deferWalk() bool {
	if c.deferredLast {
		c.deferredLast = false
		return false
	}
	spent, measured := c.selfCPUSeconds()
	if !measured || !c.windowMeasured || spent-c.windowCPU < windowAllowance() {
		return false
	}
	c.deferredLast = true
	c.walkDeferred++
	c.opts.Registry.SetWalkDeferred(c.walkDeferred)
	return true
}

// readTotals is the pod's own accounting and the memory its processes hold, which is the reading
// every second buys: under the cgroup profile the pod's cgroup answers for the totals, and
// otherwise the cgroup filesystem's mount root does.
func (c *podCollector) readTotals() {
	if c.cgroups {
		group, err := procread.ReadGroup(c.opts.CgroupRoot)
		if err != nil {
			c.failed(SourceCgroup, err)
		} else {
			c.opts.Registry.SetPodMemoryBytes(group.MemoryBytes)
			c.opts.Registry.SetPodCPUSeconds(group.CPUSeconds)
		}
	} else if totals, err := c.reader.PodTotals(); err != nil {
		c.failed(SourceCgroup, err)
	} else {
		c.opts.Registry.SetPodMemoryBytes(totals.MemoryBytes)
		c.opts.Registry.SetPodCPUSeconds(totals.CPUSeconds)
	}

	info, err := c.reader.MemInfo()
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
// It is also the one read of /proc/self/smaps the sampler makes each second: the memory guard
// takes the reading this publishes rather than a second one of its own (service.overLimit).
// Both files are the sampler's own, read every second of its life, so both are read through the
// descriptors the reader keeps on them.
func readSelf(reader *procread.Reader, pageSize int64, registry *Registry, failed func(ReadSource, error)) {
	stat, err := reader.Stat(selfPID())
	if err != nil {
		failed(SourceProc, err)
		return
	}
	registry.SetSelfCPUSeconds(seconds(stat.UserTicks + stat.SystemTicks))
	anonymous, err := readAnonymous(reader, selfPID())
	if err != nil {
		failed(SourceProc, err)
		return
	}
	registry.SetSelfMemoryBytes(anonymous, int64(stat.ResidentPages)*pageSize)
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
	c.windowCPU, c.windowMeasured = c.selfCPUSeconds()
}

// smapsLeft is what this window may spend reading smaps: the sampler's whole allowance for a
// window less the pass work the window has already spent, and never more than smapsBudget.
//
// The cap is the requirement and the rates are the means
// (dispatch://INFRA-1208/spec#b-a-rates: 2% of one CPU). Holding the costly read alone inside a
// budget holds nothing: the per-second and the per-two-second work is most of what a sampler
// over a pod of hundreds of processes spends, and this one spent 3.6% of one CPU over a live
// run of 319 s with its smaps budget honoured (INFRA-1208, measured 2026-11-05). So the walk
// and the totals are measured the same way the smaps read is, from /proc/self/stat, and smaps
// spends what they leave.
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
	stat, err := c.reader.Stat(selfPID())
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
