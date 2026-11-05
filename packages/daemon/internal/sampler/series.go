package sampler

import (
	"cmp"
	"io"
	"maps"
	"math"
	"slices"
	"strconv"
	"sync"
	"time"
)

// This file is the series registry and the exposition: the one place that decides what the
// sampler serves, under which labels and at which moment a reading leaves the output. The
// contract it carries is written for a reader in docs/sampler-metrics.md, and a test reads that
// document and compares it with the table below, so neither can move without the other.

// ContentType is the media type of what Write writes: the Prometheus text format, version 0.0.4,
// which is what a Managed Prometheus collector scrapes. The sampler writes that format itself —
// the daemon module has no metrics library, and packages/envoy/internal/metrics is another
// module's internal package — in the shape that file already uses.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// activeWindow is how long a pair of labels outlives its processes: a role and kind pair, and a
// grade pod's kind, is served while a process of it ran within the last hour and leaves the
// output after that (dispatch://INFRA-1208/spec#b-a-rates). Prometheus reads a counter that
// stops and returns correctly, so the window keeps the series set small and costs a reader
// nothing.
const activeWindow = time.Hour

// seriesCap is the most series the sampler serves for one pod: the sampler's own pod in an issue
// pod, one grade pod in node mode. An issue pod as the daemon builds it today is 171 series at
// its widest, so the cap binds only on a pod with more volumes than Legion declares.
const seriesCap = 200

// Mode is which of the two series sets a registry serves. It is a bit so that a metric can name
// the modes it belongs to, since the sampler's own series are served in both.
type Mode uint8

const (
	// ModePod is the sampler of one issue pod: every role and kind pair of that pod, its
	// volumes and its totals, each carrying the pod's project and issue.
	ModePod Mode = 1 << iota
	// ModeNode is `legion sampler --node`: one set of series for each grade pod of the node,
	// and no volumes, since a node's sampler is told nothing about any pod's mounts.
	ModeNode
)

// bothModes are the sampler's own series, which every mode serves.
const bothModes = ModePod | ModeNode

// ReadSource is what a read that failed was a read of, and the value of
// legion_sampler_read_errors_total's `source` label. Each source is served from the start, at
// zero until something fails, so a rate over it answers on a sampler that has never failed.
type ReadSource string

const (
	// SourceProc is a read of /proc: a process's stat, cmdline or smaps.
	SourceProc ReadSource = "proc"
	// SourceCgroup is a read of the cgroup filesystem: the pod's totals or a group's files.
	SourceCgroup ReadSource = "cgroup"
	// SourceVolume is a statfs of a volume's mount path.
	SourceVolume ReadSource = "volume"
	// SourceKubernetes is a call to the API server, which only node mode makes.
	SourceKubernetes ReadSource = "kubernetes"
)

// readSources are every source served, which is all of them whether or not one has failed.
var readSources = []ReadSource{SourceCgroup, SourceKubernetes, SourceProc, SourceVolume}

// GradePod is one pod of node mode, as every series about it is labelled: its name and
// namespace, the Kueue queue it was admitted through and the owner it is charged to, each read
// from the pod's labels or its owning Job's (dispatch://INFRA-1208/spec#b-a-node).
type GradePod struct {
	Pod       string
	Namespace string
	Queue     string
	Owner     string
}

// metricID names one metric of the contract. It is the index of that metric's row in metrics, so
// the exposition walks the rows in this order and two scrapes of the same state write the same
// bytes.
type metricID int

const (
	cpuSecondsTotal metricID = iota
	memoryAnonymousBytes
	memoryAnonymousPeakBytes
	memoryResidentBytes
	memoryResidentPeakBytes
	volumeUsedBytes
	podMemoryBytes
	podMemoryPeakBytes
	podCPUSecondsTotal
	podProcessMemoryBytes
	podProcessMemoryPeakBytes
	podSharedMemoryBytes
	gradeCPUSecondsTotal
	gradeMemoryResidentBytes
	gradeMemoryResidentPeakBytes
	gradePodMemoryBytes
	gradePodMemoryPeakBytes
	gradePodCPUSecondsTotal
	selfCPUSecondsTotal
	selfMemoryAnonymousBytes
	selfMemoryResidentBytes
	readErrorsTotal
	restartsTotal
	seriesDropped
	smapsDeferred
	metricCount
)

// metric is one row of the contract: the name a collector sees, the type it is read as, the help
// line the exposition carries, the labels every series of it holds in the order they are
// written, and the modes that serve it.
type metric struct {
	name   string
	typ    string
	help   string
	labels []string
	modes  Mode
}

const (
	counterType = "counter"
	gaugeType   = "gauge"
)

// podLabels are on every series an issue pod's sampler serves about the pod: the project and the
// issue, which the daemon passes on the command line from the pod's legion.dev/project and
// legion.dev/issue label values. There is no `pod` label here or anywhere else, since Managed
// Prometheus adds that itself.
var podLabels = []string{"project", "issue"}

// gradeLabels are the labels of a grade pod, and gradeKindLabels those of one kind of its work.
var (
	gradeLabels     = []string{"grade_pod", "grade_namespace", "queue", "owner"}
	gradeKindLabels = []string{"grade_pod", "grade_namespace", "kind", "queue", "owner"}
)

// metrics is the contract: every series name the sampler serves, with its type, its labels and
// the modes it belongs to. docs/sampler-metrics.md carries the same table for a person, and
// TestTheServedSetIsTheDocumentedSet reads that file and compares it with this, so a metric
// added here and nowhere else fails the build's tests rather than reaching a dashboard the
// document does not describe.
var metrics = [metricCount]metric{
	cpuSecondsTotal: {
		name: "legion_sampler_cpu_seconds_total", typ: counterType, modes: ModePod,
		labels: []string{"project", "issue", "role", "kind"},
		help:   "CPU seconds this role has spent on work of this kind",
	},
	memoryAnonymousBytes: {
		name: "legion_sampler_memory_anonymous_bytes", typ: gaugeType, modes: ModePod,
		labels: []string{"project", "issue", "role", "kind"},
		help:   "Anonymous memory this role's work of this kind holds now",
	},
	memoryAnonymousPeakBytes: {
		name: "legion_sampler_memory_anonymous_peak_bytes", typ: gaugeType, modes: ModePod,
		labels: []string{"project", "issue", "role", "kind"},
		help:   "The most anonymous memory this role's work of this kind has held since the last scrape",
	},
	memoryResidentBytes: {
		name: "legion_sampler_memory_resident_bytes", typ: gaugeType, modes: ModePod,
		labels: []string{"project", "issue", "role", "kind"},
		help:   "Resident memory this role's work of this kind holds now",
	},
	memoryResidentPeakBytes: {
		name: "legion_sampler_memory_resident_peak_bytes", typ: gaugeType, modes: ModePod,
		labels: []string{"project", "issue", "role", "kind"},
		help:   "The most resident memory this role's work of this kind has held since the last scrape",
	},
	volumeUsedBytes: {
		name: "legion_sampler_volume_used_bytes", typ: gaugeType, modes: ModePod,
		labels: []string{"project", "issue", "volume", "medium", "role"},
		help:   "Bytes used in this volume",
	},
	podMemoryBytes: {
		name: "legion_sampler_pod_memory_bytes", typ: gaugeType, modes: ModePod,
		labels: podLabels,
		help:   "Memory the pod's cgroup holds now",
	},
	podMemoryPeakBytes: {
		name: "legion_sampler_pod_memory_peak_bytes", typ: gaugeType, modes: ModePod,
		labels: podLabels,
		help:   "The most memory the pod's cgroup has held since the last scrape",
	},
	podCPUSecondsTotal: {
		name: "legion_sampler_pod_cpu_seconds_total", typ: counterType, modes: ModePod,
		labels: podLabels,
		help:   "CPU seconds the pod's cgroup has spent",
	},
	podProcessMemoryBytes: {
		name: "legion_sampler_pod_process_memory_bytes", typ: gaugeType, modes: ModePod,
		labels: podLabels,
		help:   "Memory the pod's processes hold now: AnonPages, and Shmem on a kernel that keeps them apart",
	},
	podProcessMemoryPeakBytes: {
		name: "legion_sampler_pod_process_memory_peak_bytes", typ: gaugeType, modes: ModePod,
		labels: podLabels,
		help:   "The most memory the pod's processes have held since the last scrape",
	},
	podSharedMemoryBytes: {
		name: "legion_sampler_pod_shared_memory_bytes", typ: gaugeType, modes: ModePod,
		labels: podLabels,
		help:   "Shared memory the pod holds now, the Shmem of its /proc/meminfo",
	},
	gradeCPUSecondsTotal: {
		name: "legion_sampler_grade_cpu_seconds_total", typ: counterType, modes: ModeNode,
		labels: gradeKindLabels,
		help:   "CPU seconds this pod has spent on work of this kind",
	},
	gradeMemoryResidentBytes: {
		name: "legion_sampler_grade_memory_resident_bytes", typ: gaugeType, modes: ModeNode,
		labels: gradeKindLabels,
		help:   "Resident memory this pod's work of this kind holds now",
	},
	gradeMemoryResidentPeakBytes: {
		name: "legion_sampler_grade_memory_resident_peak_bytes", typ: gaugeType, modes: ModeNode,
		labels: gradeKindLabels,
		help:   "The most resident memory this pod's work of this kind has held since the last scrape",
	},
	gradePodMemoryBytes: {
		name: "legion_sampler_grade_pod_memory_bytes", typ: gaugeType, modes: ModeNode,
		labels: gradeLabels,
		help:   "Memory this pod's cgroup holds now",
	},
	gradePodMemoryPeakBytes: {
		name: "legion_sampler_grade_pod_memory_peak_bytes", typ: gaugeType, modes: ModeNode,
		labels: gradeLabels,
		help:   "The most memory this pod's cgroup has held since the last scrape",
	},
	gradePodCPUSecondsTotal: {
		name: "legion_sampler_grade_pod_cpu_seconds_total", typ: counterType, modes: ModeNode,
		labels: gradeLabels,
		help:   "CPU seconds this pod's cgroup has spent",
	},
	selfCPUSecondsTotal: {
		name: "legion_sampler_self_cpu_seconds_total", typ: counterType, modes: bothModes,
		help: "CPU seconds the sampler itself has spent",
	},
	selfMemoryAnonymousBytes: {
		name: "legion_sampler_self_memory_anonymous_bytes", typ: gaugeType, modes: bothModes,
		help: "Anonymous memory the sampler itself holds, the reading it exits on",
	},
	selfMemoryResidentBytes: {
		name: "legion_sampler_self_memory_resident_bytes", typ: gaugeType, modes: bothModes,
		help: "Resident memory the sampler itself holds, which under gVisor counts the mapped legion binary",
	},
	readErrorsTotal: {
		name: "legion_sampler_read_errors_total", typ: counterType, modes: bothModes,
		labels: []string{"source"},
		help:   "Reads of this source that failed",
	},
	restartsTotal: {
		name: "legion_sampler_restarts_total", typ: counterType, modes: bothModes,
		help: "Times the launcher has started the sampler again",
	},
	seriesDropped: {
		name: "legion_sampler_series_dropped", typ: gaugeType, modes: bothModes,
		help: "Series the 200-series cap removed at the last scrape",
	},
	smapsDeferred: {
		name: "legion_sampler_smaps_deferred", typ: gaugeType, modes: bothModes,
		help: "Processes the smaps budget carried into the next window",
	},
}

// counter is a number that only rises. A reader hands it either the rise since the last pass
// (add, which is what the process walk answers) or the whole count the kernel reports (set,
// which is what a cgroup file and /proc hold), and in both cases what is served never goes
// backwards within one sampler's life: a cgroup that was removed and made again, or a counter
// read while a file was being written, would otherwise serve a fall that Prometheus reads as a
// restart and turns into a spike.
type counter struct {
	value float64
	read  bool
}

func (c *counter) add(seconds float64) {
	if seconds > 0 {
		c.value += seconds
	}
	c.read = true
}

func (c *counter) set(total float64) {
	if total > c.value {
		c.value = total
	}
	c.read = true
}

// gauge is a reading of a moment, with the most it has held since the last scrape. Write resets
// the peak to the reading beside it, so the next scrape answers the most that was held in its
// own window and never a number from before it.
type gauge struct {
	value float64
	peak  float64
	read  bool
}

func (g *gauge) set(bytes float64) {
	g.value = bytes
	if !g.read || bytes > g.peak {
		g.peak = bytes
	}
	g.read = true
}

func (g *gauge) resetPeak() { g.peak = g.value }

// pairSeries is what one role and kind pair holds, and when a process of it last ran, which is
// what the hour window reads.
type pairSeries struct {
	cpuSeconds counter
	anonymous  gauge
	resident   gauge
	lastActive time.Time
}

// podSeries is the pod's own accounting: its cgroup's memory and CPU, and the memory its
// processes hold, each served only once something has read it, since a pod whose cgroup files
// this kernel does not write has no zero to report.
type podSeries struct {
	memory        gauge
	cpuSeconds    counter
	processMemory gauge
	sharedMemory  gauge
}

// gradeSeries is one grade pod of node mode: its cgroup's totals and one set of series for each
// kind of work its processes do.
type gradeSeries struct {
	memory     gauge
	cpuSeconds counter
	kinds      map[Kind]*gradeKindSeries
}

// gradeKindSeries is one kind of one grade pod's work. Node mode reads no smaps, so it has
// resident memory and no anonymous memory.
type gradeKindSeries struct {
	cpuSeconds counter
	resident   gauge
	lastActive time.Time
}

// volumeKey is one volume as its series is labelled: the volume's name, the medium the pod spec
// declares for it, and the role that owns it or `shared`.
type volumeKey struct {
	name   string
	medium string
	role   string
}

// Registry is every series the sampler serves: the value a reader last gave it, the most a
// memory series has held since the last scrape, and when a pair of labels last had a process of
// its own. One registry serves one mode, and the exposition writes the metrics of that mode
// alone, so a reading handed to the wrong mode's method is never served under a name that mode's
// collector does not know.
//
// Every method takes the registry's lock, so the collector's loop and the HTTP handler may run
// at once, which they do: the loop writes each second and a scrape arrives whenever it arrives.
type Registry struct {
	mode    Mode
	project string
	issue   string
	// now is the clock the hour window and each pair's last activity are read from, so a test
	// drives the window without sleeping.
	now func() time.Time

	mu       sync.Mutex
	pairs    map[Pair]*pairSeries
	volumes  map[volumeKey]*gauge
	grades   map[GradePod]*gradeSeries
	pod      podSeries
	self     selfSeries
	errors   map[ReadSource]*counter
	restarts counter
	deferred float64
}

// selfSeries is what the sampler reports about itself. Its memory has no peak of its own: the
// number that matters is the reading it exits on, which is the last one.
type selfSeries struct {
	cpuSeconds counter
	anonymous  float64
	resident   float64
}

// NewPodRegistry is an issue pod's registry, whose series all carry this project and this issue.
// A nil clock is the wall clock.
func NewPodRegistry(project, issue string, now func() time.Time) *Registry {
	return newRegistry(ModePod, project, issue, now)
}

// NewNodeRegistry is node mode's registry, which serves one set of series for each grade pod of
// the node and carries no project or issue of its own. A nil clock is the wall clock.
func NewNodeRegistry(now func() time.Time) *Registry {
	return newRegistry(ModeNode, "", "", now)
}

func newRegistry(mode Mode, project, issue string, now func() time.Time) *Registry {
	if now == nil {
		now = time.Now
	}
	errors := make(map[ReadSource]*counter, len(readSources))
	for _, source := range readSources {
		errors[source] = &counter{read: true}
	}
	return &Registry{
		mode:    mode,
		project: project,
		issue:   issue,
		now:     now,
		pairs:   map[Pair]*pairSeries{},
		volumes: map[volumeKey]*gauge{},
		grades:  map[GradePod]*gradeSeries{},
		errors:  errors,
	}
}

// AddCPUSeconds charges a role and kind pair the CPU one pass found, which is the rise since the
// pass before and never a total (Pass.CPUSeconds). A pass that found the pair and no CPU charges
// zero, which is how a pair that is running and idle stays in the hour window.
func (r *Registry) AddCPUSeconds(pair Pair, seconds float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	series := r.pair(pair)
	series.cpuSeconds.add(seconds)
}

// SetAnonymousBytes is the anonymous memory a role's work of one kind holds now, summed over its
// processes. The smaps budget decides how often it is read, so this moves less often than the
// resident reading beside it.
func (r *Registry) SetAnonymousBytes(pair Pair, bytes int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pair(pair).anonymous.set(float64(bytes))
}

// SetResidentBytes is the resident memory a role's work of one kind holds now.
func (r *Registry) SetResidentBytes(pair Pair, bytes int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pair(pair).resident.set(float64(bytes))
}

// pair is the pair's series, made if this is the first reading of it, with its activity moved to
// now: every reading of a pair is a process of that pair having run.
func (r *Registry) pair(pair Pair) *pairSeries {
	series := r.pairs[pair]
	if series == nil {
		series = &pairSeries{}
		r.pairs[pair] = series
	}
	series.lastActive = r.now()
	return series
}

// SetVolumeBytes is the bytes used in one volume, under the medium the pod spec declares and the
// role that owns it. A volume is read once however many roles mount it.
func (r *Registry) SetVolumeBytes(name, medium, role string, bytes int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := volumeKey{name: name, medium: medium, role: role}
	used := r.volumes[key]
	if used == nil {
		used = &gauge{}
		r.volumes[key] = used
	}
	used.set(float64(bytes))
}

// SetPodMemoryBytes is the memory the pod's own cgroup holds.
func (r *Registry) SetPodMemoryBytes(bytes int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pod.memory.set(float64(bytes))
}

// SetPodCPUSeconds is the CPU the pod's own cgroup has spent since it started, which is a total
// and not a rise.
func (r *Registry) SetPodCPUSeconds(seconds float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pod.cpuSeconds.set(seconds)
}

// SetPodProcessMemoryBytes is the memory the pod's processes hold: AnonPages, plus Shmem on a
// kernel that keeps the two apart.
func (r *Registry) SetPodProcessMemoryBytes(bytes int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pod.processMemory.set(float64(bytes))
}

// SetPodSharedMemoryBytes is the pod's Shmem.
func (r *Registry) SetPodSharedMemoryBytes(bytes int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pod.sharedMemory.set(float64(bytes))
}

// AddGradeCPUSeconds charges one grade pod's kind of work the CPU one pass found, as
// AddCPUSeconds does for a role and kind pair.
func (r *Registry) AddGradeCPUSeconds(pod GradePod, kind Kind, seconds float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gradeKind(pod, kind).cpuSeconds.add(seconds)
}

// SetGradeResidentBytes is the resident memory one grade pod's work of one kind holds now.
func (r *Registry) SetGradeResidentBytes(pod GradePod, kind Kind, bytes int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gradeKind(pod, kind).resident.set(float64(bytes))
}

// SetGradePodMemoryBytes is the memory one grade pod's cgroup holds.
func (r *Registry) SetGradePodMemoryBytes(pod GradePod, bytes int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.grade(pod).memory.set(float64(bytes))
}

// SetGradePodCPUSeconds is the CPU one grade pod's cgroup has spent since it started.
func (r *Registry) SetGradePodCPUSeconds(pod GradePod, seconds float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.grade(pod).cpuSeconds.set(seconds)
}

// ForgetGradePod drops every series of a pod that is gone, which is what bounds a node's
// registry: the hour window takes a pod's kinds, and this takes its totals the moment the API
// server says the pod has ended.
func (r *Registry) ForgetGradePod(pod GradePod) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.grades, pod)
}

func (r *Registry) grade(pod GradePod) *gradeSeries {
	series := r.grades[pod]
	if series == nil {
		series = &gradeSeries{kinds: map[Kind]*gradeKindSeries{}}
		r.grades[pod] = series
	}
	return series
}

func (r *Registry) gradeKind(pod GradePod, kind Kind) *gradeKindSeries {
	grade := r.grade(pod)
	series := grade.kinds[kind]
	if series == nil {
		series = &gradeKindSeries{}
		grade.kinds[kind] = series
	}
	series.lastActive = r.now()
	return series
}

// SetSelfCPUSeconds is the CPU the sampler's own process has spent since it started.
func (r *Registry) SetSelfCPUSeconds(seconds float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.self.cpuSeconds.set(seconds)
}

// SetSelfMemoryBytes is what the sampler's own process holds: the anonymous memory it exits on,
// and the resident memory that under gVisor counts the mapped legion binary.
func (r *Registry) SetSelfMemoryBytes(anonymous, resident int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.self.anonymous = float64(anonymous)
	r.self.resident = float64(resident)
}

// AddReadError counts one read of this source that failed.
func (r *Registry) AddReadError(source ReadSource) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.readErrors(source).add(1)
}

// SetReadErrors is the whole count of this source's failed reads, for a reader that keeps its
// own (the process walk's ReadErrors).
func (r *Registry) SetReadErrors(source ReadSource, total uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.readErrors(source).set(float64(total))
}

func (r *Registry) readErrors(source ReadSource) *counter {
	errors := r.errors[source]
	if errors == nil {
		errors = &counter{read: true}
		r.errors[source] = errors
	}
	return errors
}

// SetRestarts is how often the launcher has started the sampler again, which the launcher counts
// and hands over on the command line.
func (r *Registry) SetRestarts(count uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.restarts.set(float64(count))
}

// SetSmapsDeferred is how many processes the smaps budget carried into the next window.
func (r *Registry) SetSmapsDeferred(count int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deferred = float64(count)
}

// sample is one series of one scrape: the metric, its label values in that metric's own order,
// and the number served.
type sample struct {
	metric metricID
	labels []string
	value  float64
}

// The tiers the cap drops in, lowest first: every role and kind pair, and in node mode every
// grade pod's kind, goes before any volume.
const (
	tierPair = iota
	tierVolume
)

// group is what the cap drops as one: the series of one role and kind pair, of one volume, or of
// one kind of one grade pod's work. A pair's five series are kept or dropped together, since
// half a pair's memory is worse to read than none of it.
type group struct {
	// owner is the pod the cap counts this group against: the sampler's own pod in an issue
	// pod, and the grade pod in node mode, so a node serving thirty pods is not capped as
	// though the thirty were one.
	owner string
	// tier and last are the drop order: the lowest tier first and, within it, the pair whose
	// process ran longest ago. key breaks a tie, so two scrapes of one state drop the same
	// groups.
	tier    int
	last    time.Time
	key     string
	samples []sample
}

// Write serves every series the registry holds, in the Prometheus text format, and resets every
// peak: the peak a scrape reads is the most that was held since the scrape before it, whoever
// scraped (dispatch://INFRA-1141/spec#b-r-peaks). The reset happens whether or not the write
// itself succeeds, because a reader that read half an exposition has still read the peaks in it.
//
// It also applies the hour window, dropping a pair no process has run in for an hour, and the
// 200-series cap, counting what the cap removed as legion_sampler_series_dropped.
func (r *Registry) Write(w io.Writer) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	r.expire(now)

	fixed := newFixedSamples(r.mode)
	r.addSelf(fixed)
	r.addTotals(fixed)
	kept, dropped := applyCap(r.droppable(), fixed.counts)
	fixed.samples[fixed.droppedAt].value = float64(dropped)
	for _, group := range kept {
		fixed.samples = append(fixed.samples, group.samples...)
	}

	body := render(fixed.samples)
	r.resetPeaks()
	_, err := w.Write(body)
	return err
}

// expire drops every pair of labels no process of which has run within the hour window. It is
// what bounds the registry: a pod that has run a hundred build commands holds the pairs of the
// last hour and nothing older.
func (r *Registry) expire(now time.Time) {
	for pair, series := range r.pairs {
		if now.Sub(series.lastActive) > activeWindow {
			delete(r.pairs, pair)
		}
	}
	for _, grade := range r.grades {
		for kind, series := range grade.kinds {
			if now.Sub(series.lastActive) > activeWindow {
				delete(grade.kinds, kind)
			}
		}
	}
}

// resetPeaks takes every peak down to the reading beside it, which is what makes a peak the most
// held since the last scrape rather than since the sampler started.
func (r *Registry) resetPeaks() {
	for _, series := range r.pairs {
		series.anonymous.resetPeak()
		series.resident.resetPeak()
	}
	for _, grade := range r.grades {
		grade.memory.resetPeak()
		for _, series := range grade.kinds {
			series.resident.resetPeak()
		}
	}
	r.pod.memory.resetPeak()
	r.pod.processMemory.resetPeak()
}

// fixedSamples are the series of one scrape that the cap never drops: the samples themselves,
// how many of them stand against each pod's cap, and where the dropped count sits, since that
// number is only known once the cap has run over everything else.
type fixedSamples struct {
	mode      Mode
	samples   []sample
	counts    map[string]int
	droppedAt int
}

func newFixedSamples(mode Mode) *fixedSamples {
	return &fixedSamples{mode: mode, counts: map[string]int{}, droppedAt: -1}
}

// add holds one series that the cap never drops, counted against the pod named by owner. A
// metric the registry's mode does not serve is passed over, so a reading handed to the wrong
// mode's method reaches no collector.
func (s *fixedSamples) add(owner string, id metricID, value float64, labels ...string) {
	if metrics[id].modes&s.mode == 0 {
		return
	}
	s.samples = append(s.samples, sample{metric: id, labels: labels, value: value})
	s.counts[owner]++
}

// addSelf holds the series the sampler reports about itself, which both modes serve and the cap
// never drops. In an issue pod they count against that pod's 200; in node mode they belong to
// the node's sampler and to no grade pod.
func (r *Registry) addSelf(s *fixedSamples) {
	s.add("", selfCPUSecondsTotal, r.self.cpuSeconds.value)
	s.add("", selfMemoryAnonymousBytes, r.self.anonymous)
	s.add("", selfMemoryResidentBytes, r.self.resident)
	for _, source := range slices.Sorted(maps.Keys(r.errors)) {
		s.add("", readErrorsTotal, r.errors[source].value, string(source))
	}
	s.add("", restartsTotal, r.restarts.value)
	s.droppedAt = len(s.samples)
	s.add("", seriesDropped, 0)
	s.add("", smapsDeferred, r.deferred)
}

// addTotals holds the pod totals, which the cap never drops either: an issue pod's own cgroup
// readings, or, in node mode, each grade pod's. A total no read has answered yet is left out
// rather than served as a zero.
func (r *Registry) addTotals(s *fixedSamples) {
	if r.pod.memory.read {
		s.add("", podMemoryBytes, r.pod.memory.value, r.project, r.issue)
		s.add("", podMemoryPeakBytes, r.pod.memory.peak, r.project, r.issue)
	}
	if r.pod.cpuSeconds.read {
		s.add("", podCPUSecondsTotal, r.pod.cpuSeconds.value, r.project, r.issue)
	}
	if r.pod.processMemory.read {
		s.add("", podProcessMemoryBytes, r.pod.processMemory.value, r.project, r.issue)
		s.add("", podProcessMemoryPeakBytes, r.pod.processMemory.peak, r.project, r.issue)
	}
	if r.pod.sharedMemory.read {
		s.add("", podSharedMemoryBytes, r.pod.sharedMemory.value, r.project, r.issue)
	}
	for pod, grade := range r.grades {
		owner := gradeKey(pod)
		labels := []string{pod.Pod, pod.Namespace, pod.Queue, pod.Owner}
		if grade.memory.read {
			s.add(owner, gradePodMemoryBytes, grade.memory.value, labels...)
			s.add(owner, gradePodMemoryPeakBytes, grade.memory.peak, labels...)
		}
		if grade.cpuSeconds.read {
			s.add(owner, gradePodCPUSecondsTotal, grade.cpuSeconds.value, labels...)
		}
	}
}

// droppable are the groups the cap may drop, each with the moment its processes last ran.
func (r *Registry) droppable() []group {
	groups := make([]group, 0, len(r.pairs)+len(r.volumes)+len(r.grades))
	if r.mode == ModePod {
		for pair, series := range r.pairs {
			labels := []string{r.project, r.issue, pair.Role, string(pair.Kind)}
			g := group{tier: tierPair, last: series.lastActive, key: pair.Role + "\x00" + string(pair.Kind)}
			if series.cpuSeconds.read {
				g.samples = append(g.samples, sample{cpuSecondsTotal, labels, series.cpuSeconds.value})
			}
			if series.anonymous.read {
				g.samples = append(g.samples,
					sample{memoryAnonymousBytes, labels, series.anonymous.value},
					sample{memoryAnonymousPeakBytes, labels, series.anonymous.peak})
			}
			if series.resident.read {
				g.samples = append(g.samples,
					sample{memoryResidentBytes, labels, series.resident.value},
					sample{memoryResidentPeakBytes, labels, series.resident.peak})
			}
			groups = append(groups, g)
		}
		// A volume has no activity of its own: it is dropped after every pair, and
		// between volumes by name, so the same volume goes first every time.
		for key, used := range r.volumes {
			labels := []string{r.project, r.issue, key.name, key.medium, key.role}
			groups = append(groups, group{
				tier:    tierVolume,
				key:     key.name + "\x00" + key.medium + "\x00" + key.role,
				samples: []sample{{volumeUsedBytes, labels, used.value}},
			})
		}
		return groups
	}
	for pod, grade := range r.grades {
		owner := gradeKey(pod)
		for kind, series := range grade.kinds {
			labels := []string{pod.Pod, pod.Namespace, string(kind), pod.Queue, pod.Owner}
			g := group{owner: owner, tier: tierPair, last: series.lastActive, key: string(kind)}
			if series.cpuSeconds.read {
				g.samples = append(g.samples, sample{gradeCPUSecondsTotal, labels, series.cpuSeconds.value})
			}
			if series.resident.read {
				g.samples = append(g.samples,
					sample{gradeMemoryResidentBytes, labels, series.resident.value},
					sample{gradeMemoryResidentPeakBytes, labels, series.resident.peak})
			}
			groups = append(groups, g)
		}
	}
	return groups
}

// applyCap drops groups until no pod is over the cap, in the order the contract fixes: the
// sampler's own series and the pod totals are never dropped, then role and kind pairs with the
// oldest activity first, then volumes. It answers the groups that survive and how many series it
// removed.
func applyCap(groups []group, fixed map[string]int) ([]group, int) {
	held := make(map[string]int, len(fixed))
	for owner, count := range fixed {
		held[owner] = count
	}
	for _, g := range groups {
		held[g.owner] += len(g.samples)
	}
	order := make([]int, len(groups))
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(a, b int) int {
		x, y := &groups[a], &groups[b]
		if c := cmp.Compare(x.tier, y.tier); c != 0 {
			return c
		}
		if c := x.last.Compare(y.last); c != 0 {
			return c
		}
		return cmp.Compare(x.key, y.key)
	})
	dropped := 0
	drop := make([]bool, len(groups))
	for _, i := range order {
		g := &groups[i]
		if held[g.owner] <= seriesCap {
			continue
		}
		held[g.owner] -= len(g.samples)
		dropped += len(g.samples)
		drop[i] = true
	}
	if dropped == 0 {
		return groups, 0
	}
	kept := groups[:0]
	for i, g := range groups {
		if !drop[i] {
			kept = append(kept, g)
		}
	}
	return kept, dropped
}

// gradeKey is one grade pod as the cap counts series against it.
func gradeKey(pod GradePod) string {
	return pod.Namespace + "\x00" + pod.Pod
}

// render is the exposition: each metric that has a series, in the order of the contract's table,
// with one # HELP and one # TYPE line before its series and the series sorted by their labels,
// so two scrapes of one state are the same bytes.
func render(samples []sample) []byte {
	var byMetric [metricCount][]sample
	for _, s := range samples {
		byMetric[s.metric] = append(byMetric[s.metric], s)
	}
	body := make([]byte, 0, 128*len(samples))
	for id := range metricCount {
		series := byMetric[id]
		if len(series) == 0 {
			continue
		}
		m := &metrics[id]
		slices.SortFunc(series, func(a, b sample) int { return slices.Compare(a.labels, b.labels) })
		body = append(body, "# HELP "...)
		body = append(body, m.name...)
		body = append(body, ' ')
		body = append(body, m.help...)
		body = append(body, "\n# TYPE "...)
		body = append(body, m.name...)
		body = append(body, ' ')
		body = append(body, m.typ...)
		body = append(body, '\n')
		for _, s := range series {
			body = appendSeries(body, m, s)
		}
	}
	return body
}

// appendSeries writes one series: its name, its labels in the metric's own order, and its value.
func appendSeries(body []byte, m *metric, s sample) []byte {
	body = append(body, m.name...)
	for i, value := range s.labels {
		if i == 0 {
			body = append(body, '{')
		} else {
			body = append(body, ',')
		}
		body = append(body, m.labels[i]...)
		body = append(body, '=', '"')
		body = appendEscaped(body, value)
		body = append(body, '"')
	}
	if len(s.labels) > 0 {
		body = append(body, '}')
	}
	body = append(body, ' ')
	body = appendValue(body, s.value)
	return append(body, '\n')
}

// appendEscaped writes a label value as the text format reads one: a backslash, a quote and a
// newline each escaped, every other byte as it stands. Nothing the sampler labels with should
// hold any of the three, and a volume or a pod named with one must not break the exposition for
// every other series.
func appendEscaped(body []byte, value string) []byte {
	for i := range len(value) {
		switch c := value[i]; c {
		case '\\':
			body = append(body, '\\', '\\')
		case '"':
			body = append(body, '\\', '"')
		case '\n':
			body = append(body, '\\', 'n')
		default:
			body = append(body, c)
		}
	}
	return body
}

// appendValue writes a number as Prometheus reads it: a whole number as its digits, which every
// byte count is, and anything else at the shortest form that reads back the same.
func appendValue(body []byte, value float64) []byte {
	if whole := math.Trunc(value); whole == value && math.Abs(value) < 1<<53 {
		return strconv.AppendInt(body, int64(whole), 10)
	}
	return strconv.AppendFloat(body, value, 'g', -1, 64)
}
