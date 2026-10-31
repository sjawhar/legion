// Package metrics provides minimal Prometheus-compatible metrics without
// third-party dependencies. It supports counters (with labels), histograms
// (with labels and configurable buckets), gauges, and gauge functions
// evaluated at scrape time, and families whose series are all read at scrape
// time from counts kept elsewhere: labelled gauges and counters, and
// histograms given as cumulative buckets, one of them a histogram the
// runtime/metrics package reads (RuntimeHistogram).
package metrics

import (
	"fmt"
	"io"
	"math"
	"net/http"
	runtimemetrics "runtime/metrics"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// TopicPrefix returns the first two dot-separated segments of a topic.
// E.g. "notifications.github.acme.widgets.issue.42.comment" → "notifications.github".
func TopicPrefix(topic string) string {
	parts := strings.SplitN(topic, ".", 3)
	if len(parts) >= 2 {
		return parts[0] + "." + parts[1]
	}
	return topic
}

// DefaultBuckets are the standard histogram buckets for delivery duration.
var DefaultBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// Registry collects metrics and serves them in Prometheus text exposition format.
type Registry struct {
	mu             sync.RWMutex
	scrapeHooks    []func()
	counters       []*Counter
	histograms     []*Histogram
	gauges         []*Gauge
	gaugeFuncs     []*GaugeFunc
	sampleFuncs    []*sampleFunc
	histogramFuncs []*histogramFunc
}

// OnScrape registers fn to run once at the start of each scrape, before any family is written,
// so families read at scrape time can share one reading that fn takes, rather than each taking
// its own.
func (r *Registry) OnScrape(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scrapeHooks = append(r.scrapeHooks, fn)
}

// New creates an empty metrics registry.
func New() *Registry { return &Registry{} }

// ---------------------------------------------------------------------------
// Counter
// ---------------------------------------------------------------------------

// Counter is a monotonically increasing metric with optional labels.
type Counter struct {
	name   string
	help   string
	mu     sync.Mutex
	series map[string]*counterEntry
}

type counterEntry struct {
	formatted string // pre-rendered label string, e.g. `{source="agent"}`
	value     atomic.Int64
}

// NewCounter creates, registers, and returns a new counter.
func (r *Registry) NewCounter(name, help string) *Counter {
	c := &Counter{name: name, help: help, series: make(map[string]*counterEntry)}
	r.mu.Lock()
	r.counters = append(r.counters, c)
	r.mu.Unlock()
	return c
}

// Inc increments the counter for the given label set by 1.
func (c *Counter) Inc(labels ...[2]string) {
	key := labelKey(labels)
	c.mu.Lock()
	e, ok := c.series[key]
	if !ok {
		e = &counterEntry{formatted: labelFormat(labels)}
		c.series[key] = e
	}
	c.mu.Unlock()
	e.value.Add(1)
}

// ---------------------------------------------------------------------------
// Histogram
// ---------------------------------------------------------------------------

// Histogram records the distribution of observed values in pre-defined buckets.
type Histogram struct {
	name   string
	help   string
	bounds []float64
	mu     sync.Mutex
	series map[string]*histEntry
}

type histEntry struct {
	formatted string // pre-rendered label string
	buckets   []*atomic.Int64
	count     atomic.Int64
	sumBits   atomic.Uint64 // float64 stored as bits for lock-free CAS
}

// NewHistogram creates, registers, and returns a new histogram.
func (r *Registry) NewHistogram(name, help string, buckets []float64) *Histogram {
	b := make([]float64, len(buckets))
	copy(b, buckets)
	sort.Float64s(b)
	h := &Histogram{name: name, help: help, bounds: b, series: make(map[string]*histEntry)}
	r.mu.Lock()
	r.histograms = append(r.histograms, h)
	r.mu.Unlock()
	return h
}

// Observe records a value in the histogram for the given label set.
func (h *Histogram) Observe(value float64, labels ...[2]string) {
	key := labelKey(labels)
	h.mu.Lock()
	e, ok := h.series[key]
	if !ok {
		bc := make([]*atomic.Int64, len(h.bounds))
		for i := range bc {
			bc[i] = &atomic.Int64{}
		}
		e = &histEntry{formatted: labelFormat(labels), buckets: bc}
		h.series[key] = e
	}
	h.mu.Unlock()

	// Increment only the first matching bucket (non-cumulative storage).
	for i, bound := range h.bounds {
		if value <= bound {
			e.buckets[i].Add(1)
			break
		}
	}
	e.count.Add(1)
	// CAS loop for atomic float64 addition.
	for {
		old := e.sumBits.Load()
		if e.sumBits.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+value)) {
			break
		}
	}
}

// ---------------------------------------------------------------------------
// Gauge
// ---------------------------------------------------------------------------

// Gauge is a metric that can increase and decrease.
type Gauge struct {
	name  string
	help  string
	value atomic.Int64
}

// NewGauge creates, registers, and returns a new gauge.
func (r *Registry) NewGauge(name, help string) *Gauge {
	g := &Gauge{name: name, help: help}
	r.mu.Lock()
	r.gauges = append(r.gauges, g)
	r.mu.Unlock()
	return g
}

// Set stores v as the current gauge value.
func (g *Gauge) Set(v int64) { g.value.Store(v) }

// Inc adds 1 to the gauge.
func (g *Gauge) Inc() { g.value.Add(1) }

// Dec subtracts 1 from the gauge.
func (g *Gauge) Dec() { g.value.Add(-1) }

// ---------------------------------------------------------------------------
// GaugeFunc
// ---------------------------------------------------------------------------

// GaugeFunc evaluates fn at scrape time to produce a gauge value.
type GaugeFunc struct {
	name string
	help string
	fn   func() int64
}

// NewGaugeFunc registers a gauge whose value is computed by fn on each scrape.
func (r *Registry) NewGaugeFunc(name, help string, fn func() int64) {
	r.mu.Lock()
	r.gaugeFuncs = append(r.gaugeFuncs, &GaugeFunc{name: name, help: help, fn: fn})
	r.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Families read at scrape time
// ---------------------------------------------------------------------------

// Sample is one series of a family read at scrape time (NewGaugeVecFunc, NewCounterVecFunc): its
// labels, none for a family of one unlabelled series, and its value.
type Sample struct {
	Labels [][2]string
	Value  float64
}

// sampleFunc is a gauge or counter family whose series samples reports on each scrape.
type sampleFunc struct {
	name, help, typ string
	samples         func() []Sample
}

// NewGaugeVecFunc registers a gauge family whose series fn reports on each scrape, one Sample per
// label set.
func (r *Registry) NewGaugeVecFunc(name, help string, fn func() []Sample) {
	r.addSampleFunc(&sampleFunc{name: name, help: help, typ: "gauge", samples: fn})
}

// NewCounterVecFunc registers a counter family whose series fn reports on each scrape, one Sample
// per label set: counts something else keeps, which must only ever grow.
func (r *Registry) NewCounterVecFunc(name, help string, fn func() []Sample) {
	r.addSampleFunc(&sampleFunc{name: name, help: help, typ: "counter", samples: fn})
}

func (r *Registry) addSampleFunc(f *sampleFunc) {
	r.mu.Lock()
	r.sampleFuncs = append(r.sampleFuncs, f)
	r.mu.Unlock()
}

// HistogramSample is one series of a histogram read at scrape time (NewHistogramVecFunc): its
// labels; the upper bounds of its buckets, increasing; Cumulative[i], how many observations were
// at most Bounds[i]; and the count and sum of every observation, those above the last bound
// included. A Count below the last cumulative bucket, which counters read one at a time can give,
// is written as that bucket, so the +Inf bucket never falls below the one before it.
type HistogramSample struct {
	Labels     [][2]string
	Bounds     []float64
	Cumulative []uint64
	Count      uint64
	Sum        float64
}

// histogramFunc is a histogram family whose series samples reports on each scrape.
type histogramFunc struct {
	name, help string
	samples    func() []HistogramSample
}

// NewHistogramVecFunc registers a histogram family whose series fn reports on each scrape, from
// buckets something else counts.
func (r *Registry) NewHistogramVecFunc(name, help string, fn func() []HistogramSample) {
	r.mu.Lock()
	r.histogramFuncs = append(r.histogramFuncs, &histogramFunc{name: name, help: help, samples: fn})
	r.mu.Unlock()
}

// RuntimeHistogram is h, a histogram the runtime/metrics package reads, as a HistogramSample with
// the buckets bounds names, labelled labels. The runtime's own buckets are far finer than a scrape
// needs (hundreds for a duration), so each is counted under the first of bounds at or above its
// upper edge: every observation it holds is below that edge, so a cumulative bucket never counts
// an observation above its bound, and one that straddles a bound counts in the bucket above it. A
// runtime bucket with no finite upper edge counts in the +Inf bucket alone. The runtime keeps no
// sum, so Sum is an estimate: each runtime bucket's count times its midpoint, or times its finite
// edge where the other is infinite.
func RuntimeHistogram(h *runtimemetrics.Float64Histogram, bounds []float64, labels ...[2]string) HistogramSample {
	sample := HistogramSample{Labels: labels, Bounds: bounds, Cumulative: make([]uint64, len(bounds))}
	for i, count := range h.Counts {
		if count == 0 {
			continue
		}
		low, high := h.Buckets[i], h.Buckets[i+1]
		sample.Count += count
		if j := sort.SearchFloat64s(bounds, high); j < len(bounds) {
			sample.Cumulative[j] += count
		}
		switch {
		case math.IsInf(low, -1) && math.IsInf(high, 1):
		case math.IsInf(low, -1):
			sample.Sum += float64(count) * high
		case math.IsInf(high, 1):
			sample.Sum += float64(count) * low
		default:
			sample.Sum += float64(count) * (low + high) / 2
		}
	}
	for i := 1; i < len(sample.Cumulative); i++ {
		sample.Cumulative[i] += sample.Cumulative[i-1]
	}
	return sample
}

// ---------------------------------------------------------------------------
// Timer
// ---------------------------------------------------------------------------

// Timer measures elapsed time for histogram observations.
type Timer struct{ start time.Time }

// NewTimer starts a new timer.
func NewTimer() Timer { return Timer{start: time.Now()} }

// ObserveDuration records the elapsed seconds since the timer was created.
func (t Timer) ObserveDuration(h *Histogram, labels ...[2]string) {
	h.Observe(time.Since(t.start).Seconds(), labels...)
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

// Handler returns an http.Handler that writes all registered metrics in
// Prometheus text exposition format (text/plain; version=0.0.4).
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.mu.RLock()
		defer r.mu.RUnlock()

		for _, hook := range r.scrapeHooks {
			hook()
		}

		for _, c := range r.counters {
			writeType(w, c.name, c.help, "counter")
			c.mu.Lock()
			for _, key := range sortedKeys(c.series) {
				e := c.series[key]
				fmt.Fprintf(w, "%s%s %d\n", c.name, e.formatted, e.value.Load())
			}
			c.mu.Unlock()
		}

		for _, h := range r.histograms {
			writeType(w, h.name, h.help, "histogram")
			h.mu.Lock()
			for _, key := range sortedKeys(h.series) {
				e := h.series[key]
				inner := stripBraces(e.formatted)
				var cum int64
				for i, bound := range h.bounds {
					cum += e.buckets[i].Load()
					writeBucket(w, h.name, inner, bound, cum)
				}
				total := e.count.Load()
				sum := math.Float64frombits(e.sumBits.Load())
				writeInfBucket(w, h.name, inner, total)
				writeSumCount(w, h.name, inner, sum, total)
			}
			h.mu.Unlock()
		}

		for _, g := range r.gauges {
			writeType(w, g.name, g.help, "gauge")
			fmt.Fprintf(w, "%s %d\n", g.name, g.value.Load())
		}

		for _, gf := range r.gaugeFuncs {
			writeType(w, gf.name, gf.help, "gauge")
			fmt.Fprintf(w, "%s %d\n", gf.name, gf.fn())
		}

		for _, f := range r.sampleFuncs {
			writeType(w, f.name, f.help, f.typ)
			for _, sample := range f.samples() {
				fmt.Fprintf(w, "%s%s %s\n", f.name, labelFormat(sample.Labels), formatValue(sample.Value))
			}
		}

		for _, f := range r.histogramFuncs {
			writeType(w, f.name, f.help, "histogram")
			for _, sample := range f.samples() {
				writeHistogramSample(w, f.name, sample)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

func writeType(w http.ResponseWriter, name, help, typ string) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	fmt.Fprintf(w, "# TYPE %s %s\n", name, typ)
}

func writeBucket(w http.ResponseWriter, name, inner string, le float64, cum int64) {
	if inner != "" {
		fmt.Fprintf(w, "%s_bucket{%s,le=\"%g\"} %d\n", name, inner, le, cum)
	} else {
		fmt.Fprintf(w, "%s_bucket{le=\"%g\"} %d\n", name, le, cum)
	}
}

func writeInfBucket(w http.ResponseWriter, name, inner string, total int64) {
	if inner != "" {
		fmt.Fprintf(w, "%s_bucket{%s,le=\"+Inf\"} %d\n", name, inner, total)
	} else {
		fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n", name, total)
	}
}

func writeSumCount(w http.ResponseWriter, name, inner string, sum float64, count int64) {
	if inner != "" {
		fmt.Fprintf(w, "%s_sum{%s} %g\n", name, inner, sum)
		fmt.Fprintf(w, "%s_count{%s} %d\n", name, inner, count)
	} else {
		fmt.Fprintf(w, "%s_sum %g\n", name, sum)
		fmt.Fprintf(w, "%s_count %d\n", name, count)
	}
}

// writeHistogramSample writes one series of a histogram read at scrape time: a cumulative bucket
// per bound, the +Inf bucket, the sum and the count.
func writeHistogramSample(w io.Writer, name string, sample HistogramSample) {
	labels := labelFormat(sample.Labels)
	inner := stripBraces(labels)
	prefix := ""
	if inner != "" {
		prefix = inner + ","
	}
	var last uint64
	for i, bound := range sample.Bounds {
		last = sample.Cumulative[i]
		fmt.Fprintf(w, "%s_bucket{%sle=\"%s\"} %d\n", name, prefix, formatValue(bound), last)
	}
	count := max(sample.Count, last)
	fmt.Fprintf(w, "%s_bucket{%sle=\"+Inf\"} %d\n", name, prefix, count)
	fmt.Fprintf(w, "%s_sum%s %s\n", name, labels, formatValue(sample.Sum))
	fmt.Fprintf(w, "%s_count%s %d\n", name, labels, count)
}

// formatValue writes v as the text format takes a value: a whole number below 2^53 as an integer,
// and any other number - infinities and NaN included, which Go spells as Prometheus does - in Go's
// shortest form.
func formatValue(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1<<53 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func labelKey(labels [][2]string) string {
	if len(labels) == 0 {
		return ""
	}
	var b strings.Builder
	for i, kv := range labels {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(kv[0])
		b.WriteByte('=')
		b.WriteString(kv[1])
	}
	return b.String()
}

func labelFormat(labels [][2]string) string {
	if len(labels) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, kv := range labels {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(kv[0])
		b.WriteString(`="`)
		b.WriteString(kv[1])
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func stripBraces(s string) string {
	if len(s) >= 2 && s[0] == '{' && s[len(s)-1] == '}' {
		return s[1 : len(s)-1]
	}
	return s
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
