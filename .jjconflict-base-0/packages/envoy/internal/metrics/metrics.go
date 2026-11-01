// Package metrics provides minimal Prometheus-compatible metrics without
// third-party dependencies. It supports counters (with labels), histograms
// (with labels and configurable buckets), gauges, and gauge functions
// evaluated at scrape time.
package metrics

import (
	"fmt"
	"math"
	"net/http"
	"sort"
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
	mu         sync.RWMutex
	counters   []*Counter
	histograms []*Histogram
	gauges     []*Gauge
	gaugeFuncs []*GaugeFunc
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
