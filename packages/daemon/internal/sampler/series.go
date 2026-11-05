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
//
// There is one series model and not one for each mode: every reading the registry holds is keyed
// by its owner — the pod the 200-series cap counts it against — and the label values its series
// carry, and which metric each reading is served as comes from the mode's own row of modeSeries.
// So a series added to the contract is added in one setter and one row, where before it was two
// of each (dispatch://INFRA-1208, the review of pull request 2786).

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
	totalsDeferred
	walkDeferred
	metricCount
)

// noMetric is a reading the mode does not serve: node mode reads no smaps, so its work holds no
// anonymous memory, and no grade pod has a volume or a /proc/meminfo of its own. A reading whose
// metric is this one is held in the registry and written nowhere, which is what keeps a mode's
// exposition to the mode's own table.
const noMetric metricID = metricCount

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

// maxLabels is the most label values a series of this contract carries, which is the five of a
// grade pod's kind and of a volume. The values are held in an array of that width rather than a
// slice, so that one of them is a map key and costs a reading no allocation at all.
const maxLabels = 5

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
	totalsDeferred: {
		name: "legion_sampler_totals_deferred", typ: gaugeType, modes: bothModes,
		help: "Memory-total reads the sampler's own cap has deferred",
	},
	walkDeferred: {
		name: "legion_sampler_walk_deferred", typ: gaugeType, modes: bothModes,
		help: "Walk passes the sampler's own cap has deferred",
	},
}

// workMetrics is which metric each reading of one owner's work of one kind is served as in one
// mode: an issue pod's role and kind pair, or a grade pod's kind. A reading the mode does not
// serve is noMetric — node mode reads no smaps and serves no anonymous memory — and no
// collector of that mode ever takes one.
type workMetrics struct {
	cpuSeconds               metricID
	anonymous, anonymousPeak metricID
	resident, residentPeak   metricID
}

// totalMetrics is the same for the cgroup totals of one owner: the issue pod's own, and each
// grade pod's. A node reads no /proc/meminfo of another pod, so neither the memory its processes
// hold nor its shared memory is served there.
type totalMetrics struct {
	memory, memoryPeak         metricID
	cpuSeconds                 metricID
	processMemory, processPeak metricID
	sharedMemory               metricID
}

// modeSeries is the whole of what one mode serves about the pods it reads: which metric each
// reading of an owner's work, of an owner's totals and of a volume is written as. A registry
// holds its own row, so the exposition asks the row and never the mode.
type modeSeries struct {
	work       workMetrics
	totals     totalMetrics
	volumeUsed metricID
}

// served is each mode's row. The two of them are the one place a reading of the registry is tied
// to the name it goes out under.
var served = map[Mode]modeSeries{
	ModePod: {
		work: workMetrics{
			cpuSeconds:    cpuSecondsTotal,
			anonymous:     memoryAnonymousBytes,
			anonymousPeak: memoryAnonymousPeakBytes,
			resident:      memoryResidentBytes,
			residentPeak:  memoryResidentPeakBytes,
		},
		totals: totalMetrics{
			memory:        podMemoryBytes,
			memoryPeak:    podMemoryPeakBytes,
			cpuSeconds:    podCPUSecondsTotal,
			processMemory: podProcessMemoryBytes,
			processPeak:   podProcessMemoryPeakBytes,
			sharedMemory:  podSharedMemoryBytes,
		},
		volumeUsed: volumeUsedBytes,
	},
	ModeNode: {
		work: workMetrics{
			cpuSeconds:    gradeCPUSecondsTotal,
			anonymous:     noMetric,
			anonymousPeak: noMetric,
			resident:      gradeMemoryResidentBytes,
			residentPeak:  gradeMemoryResidentPeakBytes,
		},
		totals: totalMetrics{
			memory:        gradePodMemoryBytes,
			memoryPeak:    gradePodMemoryPeakBytes,
			cpuSeconds:    gradePodCPUSecondsTotal,
			processMemory: noMetric,
			processPeak:   noMetric,
			sharedMemory:  noMetric,
		},
		volumeUsed: noMetric,
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

// set is a reading of this moment from a reader that knows of nothing higher, so the reading
// itself is the most the series is known to have held since the last scrape.
func (g *gauge) set(bytes float64) { g.observe(bytes, bytes) }

// observe is a reading of this moment and the most the reader knows the series to have held
// since the last scrape, which a cgroup's memory.peak answers for and a reading of a moment
// cannot. Both are taken in one call because a reader that set them in two left the peak
// standing as the reading of this moment in between (Registry.SetResidentPeakBytes).
func (g *gauge) observe(bytes, peak float64) {
	g.value = bytes
	if high := max(bytes, peak); !g.read || high > g.peak {
		g.peak = high
	}
	g.read = true
}

func (g *gauge) resetPeak() { g.peak = g.value }

// workSeries is what one owner's work of one kind holds, and when a process of it last ran,
// which is what the hour window reads. One type serves both modes: a node writes no anonymous
// memory into it and a gauge nothing has read is served nowhere, which is how the same body
// answers a four-label pair of an issue pod and a five-label kind of a grade pod.
type workSeries struct {
	cpuSeconds counter
	anonymous  gauge
	resident   gauge
	lastActive time.Time
}

// totalSeries is one owner's own cgroup accounting: the issue pod's, and each grade pod's. Each
// reading is served only once something has read it, since a pod whose cgroup files this kernel
// does not write has no zero to report, and a node reads neither of the last two at all.
type totalSeries struct {
	memory        gauge
	cpuSeconds    counter
	processMemory gauge
	sharedMemory  gauge
}

// shape is which of the three kinds of series a key names, and so which of the registry's maps
// a reading lands in and which row of its mode decides the metric. The zero shape is a reading
// of the other mode, which every setter holds and nothing serves.
type shape uint8

const (
	shapeNone shape = iota
	shapeWork
	shapeTotals
	shapeVolume
)

// Series names one set of series the registry keeps: the mode that serves them, which of the
// three shapes they are, the pod the 200-series cap counts them against, and the label values
// each of them carries, in the order its metric declares them.
//
// It is one type for both modes, and the mode's own labels go into it where the series is named
// (Registry.Pair and the four beside it). A Series of the other mode is the zero value, which
// every setter below holds and nothing serves: a reading handed to the wrong mode's name reaches
// no collector, whatever a caller does.
type Series struct {
	mode   Mode
	shape  shape
	owner  string
	labels [maxLabels]string
}

// Registry is every series the sampler serves: the value a reader last gave it, the most a
// memory series has held since the last scrape, and when an owner's work last had a process of
// its own. One registry serves one mode, and the exposition writes the metrics of that mode
// alone.
//
// Every method takes the registry's lock, so the collector's loop and the HTTP handler may run
// at once, which they do: the loop writes each second and a scrape arrives whenever it arrives.
type Registry struct {
	mode Mode
	// served is this mode's row of the table above: which metric each reading it holds is
	// written as. It is read once, here, so nothing below asks the mode what it serves.
	served  modeSeries
	project string
	issue   string
	// now is the clock the hour window and each key's last activity are read from, so a test
	// drives the window without sleeping.
	now func() time.Time

	mu       sync.Mutex
	work     map[Series]*workSeries
	totals   map[Series]*totalSeries
	volumes  map[Series]*gauge
	self     selfSeries
	errors   map[ReadSource]*counter
	restarts counter
	// deferred is the processes the last smaps window carried, deferredWalks the walk passes
	// the collector's own cap has deferred since it started, and deferredTotals the readings
	// of the pod's memory totals it has deferred, each a count that only rises.
	deferred       float64
	deferredWalks  float64
	deferredTotals float64
}

// selfSeries is what the sampler reports about itself. Its memory has no peak of its own: the
// number that matters is the reading it exits on, which is the last one. read is when that
// memory reading was taken, which is what lets the memory guard use it rather than reading the
// same file of the same process again.
type selfSeries struct {
	cpuSeconds counter
	anonymous  float64
	resident   float64
	read       time.Time
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
		served:  served[mode],
		project: project,
		issue:   issue,
		now:     now,
		work:    map[Series]*workSeries{},
		totals:  map[Series]*totalSeries{},
		volumes: map[Series]*gauge{},
		errors:  errors,
	}
}

// Pair is the series of one role's work of one kind in an issue pod, which carries the pod's
// project and issue beside the two. It counts against the sampler's own pod, which is the one
// the cap is held over there.
func (r *Registry) Pair(role string, kind Kind) Series {
	return r.series(ModePod, shapeWork, "", r.project, r.issue, role, string(kind))
}

// Volume is the series of one volume, under the medium the pod spec declares and the role that
// owns it. A volume is read once however many roles mount it.
func (r *Registry) Volume(name, medium, role string) Series {
	return r.series(ModePod, shapeVolume, "", r.project, r.issue, name, medium, role)
}

// Totals is the issue pod's own cgroup readings and the memory its processes hold.
func (r *Registry) Totals() Series {
	return r.series(ModePod, shapeTotals, "", r.project, r.issue)
}

// GradeKind is the series of one grade pod's work of one kind in node mode, which carries the
// pod's own four labels beside the kind. It counts against that pod and not against the node, so
// a node serving thirty pods is not capped as though the thirty were one.
func (r *Registry) GradeKind(pod GradePod, kind Kind) Series {
	return r.series(ModeNode, shapeWork, gradeKey(pod),
		pod.Pod, pod.Namespace, string(kind), pod.Queue, pod.Owner)
}

// GradeTotals is one grade pod's own cgroup readings.
func (r *Registry) GradeTotals(pod GradePod) Series {
	return r.series(ModeNode, shapeTotals, gradeKey(pod),
		pod.Pod, pod.Namespace, pod.Queue, pod.Owner)
}

// series is one key of this registry's own mode, and the zero Series for a name of the other
// mode's: node mode serves no volume and an issue pod no grade pod, whatever a caller does.
func (r *Registry) series(mode Mode, shape shape, owner string, labels ...string) Series {
	if mode != r.mode {
		return Series{}
	}
	key := Series{mode: mode, shape: shape, owner: owner}
	copy(key.labels[:], labels)
	return key
}

// AddCPUSeconds charges one owner's work of one kind the CPU one pass found, which is the rise
// since the pass before and never a total (Pass.CPUSeconds). A pass that found the work and no
// CPU charges zero, which is how work that is running and idle stays in the hour window.
func (r *Registry) AddCPUSeconds(key Series, seconds float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if series := r.workOf(key); series != nil {
		series.cpuSeconds.add(seconds)
	}
}

// SetAnonymousBytes is the anonymous memory one owner's work of one kind holds now, summed over
// its processes. The smaps budget decides how often it is read, so this moves less often than
// the resident reading beside it, and node mode never reads it at all.
func (r *Registry) SetAnonymousBytes(key Series, bytes int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if series := r.workOf(key); series != nil {
		series.anonymous.set(float64(bytes))
	}
}

// SetResidentBytes is the resident memory one owner's work of one kind holds now.
func (r *Registry) SetResidentBytes(key Series, bytes int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if series := r.workOf(key); series != nil {
		series.resident.set(float64(bytes))
	}
}

// SetResidentPeakBytes is the resident memory one owner's work of one kind holds now and the
// most a reader knows it to have held since the last scrape, which is more than the reading of
// this moment where a cgroup's memory.peak says so (podCollector.chargeCgroupMemory).
//
// The two are set in one call because they are one reading. Set in two, a scrape that arrived
// between them served the peak as the memory the pair holds now and reset the peak to it, so
// the window after it counted that peak a second time (INFRA-1208, the review of pull request
// 2786).
func (r *Registry) SetResidentPeakBytes(key Series, bytes, peak int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if series := r.workOf(key); series != nil {
		series.resident.observe(float64(bytes), float64(peak))
	}
}

// ZeroMemoryExcept sets the memory of every pair this registry holds to zero, except the pairs
// this map names: the ones a walk has just found a process of. A pair whose processes have all
// ended holds no memory, and a collector that went on serving the last reading it took served
// the memory of a build that ended a minute ago as memory held now, so the per-kind parts
// stopped summing to the pod's totals until the hour window dropped the pair
// (INFRA-1208, the review of pull request 2786).
//
// What it does not touch is the pair's activity, its CPU counter or its peak. The pair stays in
// the output for its hour with the counter it stopped at, which is what Prometheus reads a
// counter that stops as, and the peak stands until the next scrape reads it and resets it
// (resetPeaks). A reading nothing has ever taken is left unread rather than made a zero, as
// every other series the sampler has no reading for is.
func (r *Registry) ZeroMemoryExcept(live map[Series]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, series := range r.work {
		if live[key] {
			continue
		}
		if series.anonymous.read {
			series.anonymous.set(0)
		}
		if series.resident.read {
			series.resident.set(0)
		}
	}
}

// workOf is that work's series, made if this is the first reading of it, with its activity moved
// to now: every reading of it is a process of it having run. A key of the other mode, and one
// naming another shape, answers nil and is held.
func (r *Registry) workOf(key Series) *workSeries {
	if key.mode != r.mode || key.shape != shapeWork {
		return nil
	}
	series := r.work[key]
	if series == nil {
		series = &workSeries{}
		r.work[key] = series
	}
	series.lastActive = r.now()
	return series
}

// SetVolumeBytes is the bytes used in one volume.
func (r *Registry) SetVolumeBytes(key Series, bytes int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if key.mode != r.mode || key.shape != shapeVolume {
		return
	}
	used := r.volumes[key]
	if used == nil {
		used = &gauge{}
		r.volumes[key] = used
	}
	used.set(float64(bytes))
}

// SetTotalMemoryBytes is the memory one owner's own cgroup holds.
func (r *Registry) SetTotalMemoryBytes(key Series, bytes int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if series := r.totalOf(key); series != nil {
		series.memory.set(float64(bytes))
	}
}

// SetTotalCPUSeconds is the CPU one owner's own cgroup has spent since it started, which is a
// total and not a rise.
func (r *Registry) SetTotalCPUSeconds(key Series, seconds float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if series := r.totalOf(key); series != nil {
		series.cpuSeconds.set(seconds)
	}
}

// SetProcessMemoryBytes is the memory the pod's processes hold: AnonPages, plus Shmem on a
// kernel that keeps the two apart.
func (r *Registry) SetProcessMemoryBytes(key Series, bytes int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if series := r.totalOf(key); series != nil {
		series.processMemory.set(float64(bytes))
	}
}

// SetSharedMemoryBytes is the pod's Shmem.
func (r *Registry) SetSharedMemoryBytes(key Series, bytes int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if series := r.totalOf(key); series != nil {
		series.sharedMemory.set(float64(bytes))
	}
}

// totalOf is that owner's totals, made if this is the first reading of them. The hour window
// does not reach them: an issue pod's stand for as long as the sampler runs, and a grade pod's
// until the API server says the pod has ended (ForgetOwner).
func (r *Registry) totalOf(key Series) *totalSeries {
	if key.mode != r.mode || key.shape != shapeTotals {
		return nil
	}
	series := r.totals[key]
	if series == nil {
		series = &totalSeries{}
		r.totals[key] = series
	}
	return series
}

// ForgetOwner drops every series of an owner that is gone — its totals, and every kind of its
// work — which is what bounds a node's registry: the hour window takes a pod's kinds, and this
// takes the whole of the pod the moment the API server says it has ended.
func (r *Registry) ForgetOwner(key Series) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if key.mode != r.mode {
		return
	}
	delete(r.totals, key)
	for held := range r.work {
		if held.owner == key.owner {
			delete(r.work, held)
		}
	}
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
	r.self.read = r.now()
}

// SelfAnonymousBytes is the anonymous memory the last SetSelfMemoryBytes published, and whether
// that reading was taken within the last `within`. It is what the memory guard reads instead of
// taking a second reading of its own: both collectors read /proc/self/smaps every second, that
// file is the costliest read the sampler makes, and reading it twice a second for one number
// serves nobody. A reading older than the caller allows is no answer at all, so a guard whose
// collector has stopped publishing reads the file itself rather than deciding on a stale number.
func (r *Registry) SelfAnonymousBytes(within time.Duration) (int64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.self.read.IsZero() || r.now().Sub(r.self.read) > within {
		return 0, false
	}
	return int64(r.self.anonymous), true
}

// AddReadError counts one read of this source that failed. Every read the sampler makes is
// counted here, the process walk's among them, so the number only rises and no reader keeps a
// count of its own to publish.
func (r *Registry) AddReadError(source ReadSource) {
	r.mu.Lock()
	defer r.mu.Unlock()
	errors := r.errors[source]
	if errors == nil {
		errors = &counter{read: true}
		r.errors[source] = errors
	}
	errors.add(1)
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

// SetTotalsDeferred is how many readings of the pod's memory totals the sampler's own cap has
// deferred from their two-second rate, a count that only rises, as the walk's below.
func (r *Registry) SetTotalsDeferred(count uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deferredTotals = float64(count)
}

// SetWalkDeferred is how many walk passes the sampler's own cap has deferred, a count that only
// rises: the collector keeps it and hands over the whole of it.
func (r *Registry) SetWalkDeferred(count uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deferredWalks = float64(count)
}

// sample is one series of one scrape: the metric, its label values in that metric's own order,
// and the number served.
type sample struct {
	metric metricID
	labels [maxLabels]string
	value  float64
}

// The tiers the cap drops in, lowest first: every role and kind pair, and in node mode every
// grade pod's kind, goes before any volume.
const (
	tierWork = iota
	tierVolume
)

// group is what the cap drops as one: the series of one owner's work of one kind, or of one
// volume. A pair's five series are kept or dropped together, since half a pair's memory is worse
// to read than none of it.
type group struct {
	// owner is the pod the cap counts this group against: the sampler's own pod in an issue
	// pod, and the grade pod in node mode, so a node serving thirty pods is not capped as
	// though the thirty were one.
	owner string
	// tier and last are the drop order: the lowest tier first and, within it, the work whose
	// process ran longest ago. The labels break a tie, so two scrapes of one state drop the
	// same groups.
	tier    int
	last    time.Time
	labels  [maxLabels]string
	samples []sample
}

// add holds one series of this group, and holds nothing for a metric this mode does not serve.
func (g *group) add(id metricID, value float64) {
	if id == noMetric {
		return
	}
	g.samples = append(g.samples, sample{metric: id, labels: g.labels, value: value})
}

// Write serves every series the registry holds, in the Prometheus text format, and resets every
// peak: the peak a scrape reads is the most that was held since the scrape before it, whoever
// scraped (dispatch://INFRA-1141/spec#b-r-peaks). The reset happens whether or not the write
// itself succeeds, because a reader that read half an exposition has still read the peaks in it.
//
// It also applies the hour window, dropping work no process has run in for an hour, and the
// 200-series cap, counting what the cap removed as legion_sampler_series_dropped.
func (r *Registry) Write(w io.Writer) error {
	_, err := w.Write(r.exposition())
	return err
}

// exposition is the bytes Write writes, for the handler that serves them: it answers a whole
// body so that the answer carries its length and goes down the connection as one write, rather
// than as the chunks net/http frames a body of unknown length into. A scrape of this pod's
// sampler is its syscalls and almost nothing else — 1.3 ms over a connection of its own, of
// which rendering these bytes is 0.04 ms (INFRA-1208, measured 2026-11-05) — so the one thing
// worth saving on the way out is a syscall.
//
// Everything the scrape decides happens here, whether or not the bytes ever reach a reader.
func (r *Registry) exposition() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	r.expire(now)

	fixed := newFixedSamples()
	r.addSelf(fixed)
	r.addTotals(fixed)
	kept, dropped := applyCap(r.droppable(), fixed.counts)
	fixed.samples[fixed.droppedAt].value = float64(dropped)
	for _, group := range kept {
		fixed.samples = append(fixed.samples, group.samples...)
	}

	body := render(fixed.samples)
	r.resetPeaks()
	return body
}

// expire drops every key no process of which has run within the hour window. It is what bounds
// the registry: a pod that has run a hundred build commands holds the pairs of the last hour and
// nothing older, and a node holds each of its pods' kinds on the same rule.
func (r *Registry) expire(now time.Time) {
	for key, series := range r.work {
		if now.Sub(series.lastActive) > activeWindow {
			delete(r.work, key)
		}
	}
}

// resetPeaks takes every peak down to the reading beside it, which is what makes a peak the most
// held since the last scrape rather than since the sampler started.
func (r *Registry) resetPeaks() {
	for _, series := range r.work {
		series.anonymous.resetPeak()
		series.resident.resetPeak()
	}
	for _, series := range r.totals {
		series.memory.resetPeak()
		series.processMemory.resetPeak()
	}
}

// fixedSamples are the series of one scrape that the cap never drops: the samples themselves,
// how many of them stand against each pod's cap, and where the dropped count sits, since that
// number is only known once the cap has run over everything else.
type fixedSamples struct {
	samples   []sample
	counts    map[string]int
	droppedAt int
}

func newFixedSamples() *fixedSamples {
	return &fixedSamples{counts: map[string]int{}, droppedAt: -1}
}

// add holds one series that the cap never drops, counted against the pod named by owner. A
// metric the registry's mode does not serve is passed over, which is how a grade pod's totals
// hold no reading of a /proc/meminfo no node reads.
func (s *fixedSamples) add(owner string, id metricID, value float64, labels ...string) {
	if id == noMetric {
		return
	}
	one := sample{metric: id, value: value}
	copy(one.labels[:], labels)
	s.samples = append(s.samples, one)
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
	s.add("", totalsDeferred, r.deferredTotals)
	s.add("", walkDeferred, r.deferredWalks)
}

// addTotals holds the cgroup totals of every owner, which the cap never drops either: an issue
// pod's own readings, or, in node mode, each grade pod's. A total no read has answered yet is
// left out rather than served as a zero.
func (r *Registry) addTotals(s *fixedSamples) {
	ids := r.served.totals
	for key, series := range r.totals {
		labels := key.labels[:]
		if series.memory.read {
			s.add(key.owner, ids.memory, series.memory.value, labels...)
			s.add(key.owner, ids.memoryPeak, series.memory.peak, labels...)
		}
		if series.cpuSeconds.read {
			s.add(key.owner, ids.cpuSeconds, series.cpuSeconds.value, labels...)
		}
		if series.processMemory.read {
			s.add(key.owner, ids.processMemory, series.processMemory.value, labels...)
			s.add(key.owner, ids.processPeak, series.processMemory.peak, labels...)
		}
		if series.sharedMemory.read {
			s.add(key.owner, ids.sharedMemory, series.sharedMemory.value, labels...)
		}
	}
}

// droppable are the groups the cap may drop, each with the moment its processes last ran.
func (r *Registry) droppable() []group {
	groups := make([]group, 0, len(r.work)+len(r.volumes))
	ids := r.served.work
	for key, series := range r.work {
		g := group{owner: key.owner, tier: tierWork, last: series.lastActive, labels: key.labels}
		if series.cpuSeconds.read {
			g.add(ids.cpuSeconds, series.cpuSeconds.value)
		}
		if series.anonymous.read {
			g.add(ids.anonymous, series.anonymous.value)
			g.add(ids.anonymousPeak, series.anonymous.peak)
		}
		if series.resident.read {
			g.add(ids.resident, series.resident.value)
			g.add(ids.residentPeak, series.resident.peak)
		}
		groups = append(groups, g)
	}
	// A volume has no activity of its own: it is dropped after every pair, and between
	// volumes by name, so the same volume goes first every time.
	for key, used := range r.volumes {
		g := group{owner: key.owner, tier: tierVolume, labels: key.labels}
		g.add(r.served.volumeUsed, used.value)
		groups = append(groups, g)
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
		return slices.Compare(x.labels[:], y.labels[:])
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
		slices.SortFunc(series, func(a, b sample) int { return slices.Compare(a.labels[:], b.labels[:]) })
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
// The metric's own row says how many of them it carries, so a key wider than the metric leaves
// its last values unwritten rather than inventing a label name for them.
func appendSeries(body []byte, m *metric, s sample) []byte {
	body = append(body, m.name...)
	for i, name := range m.labels {
		if i == 0 {
			body = append(body, '{')
		} else {
			body = append(body, ',')
		}
		body = append(body, name...)
		body = append(body, '=', '"')
		body = appendEscaped(body, s.labels[i])
		body = append(body, '"')
	}
	if len(m.labels) > 0 {
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
