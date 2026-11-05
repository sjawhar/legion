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
// second, what every second second and what every ten — the budget the costly read is held
// inside, and the two profiles of docs/sampler-metrics.md, the `/proc` profile of a gVisor pod
// and the cgroup profile of a pod that runs each command in a cgroup of its own.
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
	// smapsBudget is the sampler's own CPU one smaps window may spend. It is CPU and not wall
	// clock on purpose: a sampler sharing a core with a build must read fewer processes, not
	// the same number more slowly.
	smapsBudget = 100 * time.Millisecond
)

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
// internal/launcher's roleInitLaunch is, and both are procread's own readers everywhere else.
var (
	readAnonymous = procread.ReadAnonymous
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
	ticker := time.NewTicker(podInterval)
	defer ticker.Stop()
	// The first pass runs before the first tick, so a scrape that arrives a moment after the
	// sampler started is answered with readings rather than with an empty registry.
	for tick := int64(0); ; tick++ {
		collector.pass(tick)
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
	cgroups  bool
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

	// procErrors are the /proc reads this collector failed, beside the walk's own count.
	procErrors uint64

	// groups is the cgroup profile's memory of each command cgroup, including cgroups that are
	// gone: a removed cgroup keeps the last values read until the hour window drops it, which
	// is what docs/sampler-metrics.md promises whoever writes those cgroups.
	groups map[string]*groupReading

	logged map[ReadSource]string
}

// groupReading is one command cgroup as the passes have read it: the pair its series are charged
// to, the last reading of its files, the CPU total the last rise was taken from, the lifetime
// peak that reading held, the most it is known to have held within this scrape's window, and when
// it was last there to be read.
type groupReading struct {
	pair    Pair
	group   procread.Group
	charged float64
	peak    int64
	within  int64
	seen    time.Time
}

func newPodCollector(opts PodOptions) (*podCollector, error) {
	if err := opts.check(); err != nil {
		return nil, err
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	c := &podCollector{
		opts:        opts,
		layout:      procread.DetectLayout(opts.Root),
		cgroups:     cgroupTreePresent(opts.CgroupRoot),
		walk:        NewWalk(opts.Root, opts.Rules),
		pageSize:    int64(os.Getpagesize()),
		walkEvery:   ticksPer(walkInterval),
		smapsEvery:  ticksPer(smapsInterval),
		roles:       Roles{},
		volumesRead: opts.VolumesFile == "",
		reported:    map[string]bool{},
		groups:      map[string]*groupReading{},
		logged:      map[ReadSource]string{},
	}
	c.opts.Log(fmt.Sprintf("sampler: %s/%s: reading %s as the %s profile, cgroup layout %s",
		opts.Project, opts.Issue, opts.Root, c.profile(), c.layout))
	return c, nil
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

// ticksPer is how many ticks of the loop's own rate one of the slower rates stands for. A rate no
// slower than the tick runs on every one of them, which is what a test that shortens one rate and
// not another gets.
func ticksPer(interval time.Duration) int64 {
	if podInterval <= 0 || interval <= podInterval {
		return 1
	}
	return int64(interval / podInterval)
}

// pass is one tick of the loop: always the pod's totals and the sampler's own readings, every
// walkEvery ticks the process walk and the volumes, and every smapsEvery ticks the smaps window.
// Tick zero runs all three, so nothing waits ten seconds for its first reading.
func (c *podCollector) pass(tick int64) {
	c.readTotals()
	c.readSelf()
	if tick%c.walkEvery == 0 {
		c.readProcesses()
		c.readVolumes()
	}
	// The cgroup profile reads each command cgroup's memory.stat for its anonymous memory, so
	// the costly per-process read belongs to the /proc profile alone.
	if !c.cgroups && tick%c.smapsEvery == 0 {
		c.readSmaps()
	}
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
	} else if totals, err := procread.PodTotals(c.opts.Root, c.layout); err != nil {
		c.failed(SourceCgroup, err)
	} else {
		c.opts.Registry.SetPodMemoryBytes(totals.MemoryBytes)
		c.opts.Registry.SetPodCPUSeconds(totals.CPUSeconds)
	}

	info, err := procread.ReadMemInfo(c.opts.Root)
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

// readSelf is what the sampler costs the pod: its own CPU, the anonymous memory its exit rule is
// written against, and the resident memory that under gVisor counts the mapped legion binary. A
// reading that fails leaves the last one standing rather than serving a zero.
func (c *podCollector) readSelf() {
	stat, err := procread.ReadStat(c.opts.Root, selfPID())
	if err != nil {
		c.failed(SourceProc, err)
		return
	}
	c.opts.Registry.SetSelfCPUSeconds(seconds(stat.UserTicks + stat.SystemTicks))
	anonymous, err := readAnonymous(c.opts.Root, selfPID())
	if err != nil {
		c.failed(SourceProc, err)
		return
	}
	c.opts.Registry.SetSelfMemoryBytes(anonymous, int64(stat.ResidentPages)*c.pageSize)
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
// process first, within smapsBudget of the sampler's own CPU. What the window did not reach is
// carried into the next one and served as legion_sampler_smaps_deferred, and the carried
// processes go first there, so every process is reached within the windows the remainder needs
// rather than the largest being read again and again.
//
// The budget is the sampler's own CPU and never a share of the wall clock: /proc/self/stat is
// what the pod is charged for, and a sampler that was descheduled for 300 ms spent none of it.
func (c *podCollector) readSmaps() {
	order := c.smapsOrder()
	if len(order) == 0 {
		c.carried = nil
		c.opts.Registry.SetSmapsDeferred(0)
		return
	}
	began, measured := c.selfCPUSeconds()
	anonymous := map[Pair]int64{}
	read := 0
	for _, process := range order {
		bytes, err := readAnonymous(c.opts.Root, process.PID)
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
		// The budget is read after the process rather than before it, so every window
		// reaches at least one process and the pass always moves forward. A sampler that
		// cannot read its own CPU cannot measure the budget at all, and reads one process
		// a window rather than running unbounded.
		spent, ok := c.selfCPUSeconds()
		if !measured || !ok || spent-began >= smapsBudget.Seconds() {
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
	stat, err := procread.ReadStat(c.opts.Root, selfPID())
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
		if group.MemoryPeakBytes > reading.peak {
			reading.within = group.MemoryPeakBytes
		}
		reading.peak = group.MemoryPeakBytes
		// The CPU a cgroup reports is a total since it was made, and the registry is
		// charged the rise alone. A total below the one already charged is another cgroup
		// wearing a name that was used before, whose own total is the rise to charge.
		rise := group.CPUSeconds - reading.charged
		if group.CPUSeconds < reading.charged {
			rise = group.CPUSeconds
		}
		reading.charged = group.CPUSeconds
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
