package metrics

import (
	"io"
	"math"
	"net/http/httptest"
	"runtime"
	runtimemetrics "runtime/metrics"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// scrape serves one scrape of registry and returns the body.
func scrape(t *testing.T, registry *Registry) string {
	t.Helper()
	response := httptest.NewRecorder()
	registry.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
	if got := response.Header().Get("Content-Type"); got != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want the text exposition format's", got)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// A labelled gauge family and a counter family read at scrape time are written as the text format
// writes them: HELP and TYPE once, then one line per series, its labels in the order given, and
// each value read when the scrape came, not when the family was registered. They are written after
// the gauges and gauge functions a registry already served, which stay as they were.
func TestGaugeAndCounterFamiliesAreReadAtScrapeTime(t *testing.T) {
	registry := New()
	registry.NewGauge("test_held", "A set gauge").Set(2)
	registry.NewGaugeFunc("test_pending", "A gauge function", func() int64 { return 9 })
	inFlight := 3.0
	registry.NewGaugeVecFunc("test_in_flight", "Units held now", func() []Sample {
		return []Sample{
			{Labels: [][2]string{{"class", "read"}}, Value: inFlight},
			{Labels: [][2]string{{"class", "write"}}, Value: 0.25},
		}
	})
	registry.NewCounterVecFunc("test_refused_total", "Requests refused", func() []Sample {
		return []Sample{{Labels: [][2]string{{"class", "read"}, {"reason", "queue_full"}}, Value: 7}}
	})
	registry.NewCounterVecFunc("test_committed_total", "Events committed", func() []Sample {
		return []Sample{{Value: 12345678}}
	})
	inFlight = 5
	want := `# HELP test_held A set gauge
# TYPE test_held gauge
test_held 2
# HELP test_pending A gauge function
# TYPE test_pending gauge
test_pending 9
# HELP test_in_flight Units held now
# TYPE test_in_flight gauge
test_in_flight{class="read"} 5
test_in_flight{class="write"} 0.25
# HELP test_refused_total Requests refused
# TYPE test_refused_total counter
test_refused_total{class="read",reason="queue_full"} 7
# HELP test_committed_total Events committed
# TYPE test_committed_total counter
test_committed_total 12345678
`
	if got := scrape(t, registry); got != want {
		t.Fatalf("scrape =\n%s\nwant\n%s", got, want)
	}
}

// A histogram given as cumulative buckets is written with a _bucket line per bound, le last among
// its labels, then the +Inf bucket, which is the count, then _sum and _count. A count read below
// the last bucket (counters read one at a time) is written as that bucket, so +Inf never falls
// below the bucket before it.
func TestHistogramFamilyWritesCumulativeBucketsInfSumAndCount(t *testing.T) {
	registry := New()
	registry.NewHistogramVecFunc("test_acquire_seconds", "Acquire waits", func() []HistogramSample {
		return []HistogramSample{
			{
				Labels: [][2]string{{"pool", "requests"}}, Bounds: []float64{0.001, 0.5, 1, 30},
				Cumulative: []uint64{1, 3, 3, 3}, Count: 4, Sum: 31.25,
			},
			{
				Labels: [][2]string{{"pool", "rooms"}}, Bounds: []float64{0.001, 0.5, 1, 30},
				Cumulative: []uint64{2, 2, 2, 2}, Count: 1, Sum: 0.0015,
			},
		}
	})
	registry.NewHistogramVecFunc("test_pause_seconds", "Pauses", func() []HistogramSample {
		return []HistogramSample{{Bounds: []float64{1e-05, 0.001}, Cumulative: []uint64{0, 2}, Count: 2, Sum: 0.0012}}
	})
	want := `# HELP test_acquire_seconds Acquire waits
# TYPE test_acquire_seconds histogram
test_acquire_seconds_bucket{pool="requests",le="0.001"} 1
test_acquire_seconds_bucket{pool="requests",le="0.5"} 3
test_acquire_seconds_bucket{pool="requests",le="1"} 3
test_acquire_seconds_bucket{pool="requests",le="30"} 3
test_acquire_seconds_bucket{pool="requests",le="+Inf"} 4
test_acquire_seconds_sum{pool="requests"} 31.25
test_acquire_seconds_count{pool="requests"} 4
test_acquire_seconds_bucket{pool="rooms",le="0.001"} 2
test_acquire_seconds_bucket{pool="rooms",le="0.5"} 2
test_acquire_seconds_bucket{pool="rooms",le="1"} 2
test_acquire_seconds_bucket{pool="rooms",le="30"} 2
test_acquire_seconds_bucket{pool="rooms",le="+Inf"} 2
test_acquire_seconds_sum{pool="rooms"} 0.0015
test_acquire_seconds_count{pool="rooms"} 2
# HELP test_pause_seconds Pauses
# TYPE test_pause_seconds histogram
test_pause_seconds_bucket{le="1e-05"} 0
test_pause_seconds_bucket{le="0.001"} 2
test_pause_seconds_bucket{le="+Inf"} 2
test_pause_seconds_sum 0.0012
test_pause_seconds_count 2
`
	if got := scrape(t, registry); got != want {
		t.Fatalf("scrape =\n%s\nwant\n%s", got, want)
	}
}

// A scrape runs each OnScrape hook once, before it writes any family, so the families read at
// scrape time all report the reading the hook took for that scrape.
func TestOnScrapeRunsOnceAtTheStartOfEachScrape(t *testing.T) {
	registry := New()
	var scrapes, reading int64
	registry.OnScrape(func() {
		scrapes++
		reading = scrapes * 10
	})
	registry.NewGaugeFunc("test_first", "Read first", func() int64 { return reading })
	registry.NewGaugeVecFunc("test_second", "Read second", func() []Sample {
		return []Sample{{Value: float64(reading)}}
	})
	for n := int64(1); n <= 2; n++ {
		want := strings.ReplaceAll(`# HELP test_first Read first
# TYPE test_first gauge
test_first N
# HELP test_second Read second
# TYPE test_second gauge
test_second N
`, "N", strconv.FormatInt(n*10, 10))
		if got := scrape(t, registry); got != want {
			t.Fatalf("scrape %d =\n%s\nwant\n%s", n, got, want)
		}
		if scrapes != n {
			t.Fatalf("after scrape %d the hook ran %d times, want once a scrape", n, scrapes)
		}
	}
}

// A runtime histogram's buckets are each counted under the first bound at or above their upper
// edge, so no cumulative bucket counts an observation above its bound; a bucket with no finite
// upper edge counts in +Inf alone; and the sum is estimated from each bucket's midpoint, or its
// finite edge where the other is infinite.
func TestRuntimeHistogramCountsEachBucketUnderTheBoundAtOrAboveItsUpperEdge(t *testing.T) {
	histogram := &runtimemetrics.Float64Histogram{
		Buckets: []float64{math.Inf(-1), 0, 0.001, 0.002, 0.01, math.Inf(1)},
		Counts:  []uint64{1, 2, 3, 4, 5},
	}
	sample := RuntimeHistogram(histogram, []float64{0.001, 0.005, 0.01}, [2]string{"kind", "gc"})
	if want := []uint64{3, 6, 10}; !slices.Equal(sample.Cumulative, want) {
		t.Errorf("Cumulative = %v, want %v", sample.Cumulative, want)
	}
	if sample.Count != 15 {
		t.Errorf("Count = %d, want 15, the runtime histogram's every observation", sample.Count)
	}
	wantSum := 1*0.0 + 2*0.0005 + 3*0.0015 + 4*0.006 + 5*0.01
	if math.Abs(sample.Sum-wantSum) > 1e-12 {
		t.Errorf("Sum = %v, want %v", sample.Sum, wantSum)
	}
	if len(sample.Labels) != 1 || sample.Labels[0] != [2]string{"kind", "gc"} {
		t.Errorf("Labels = %v, want kind=gc", sample.Labels)
	}
}

// The garbage collector's pauses, read from runtime/metrics after a collection, load into a
// histogram whose buckets only grow and never exceed its count, and a scrape writes it whole.
func TestRuntimeHistogramLoadsTheGarbageCollectorsPauses(t *testing.T) {
	runtime.GC()
	read := []runtimemetrics.Sample{{Name: "/gc/pauses:seconds"}}
	runtimemetrics.Read(read)
	if read[0].Value.Kind() != runtimemetrics.KindFloat64Histogram {
		t.Fatalf("/gc/pauses:seconds is of kind %v, want a float64 histogram", read[0].Value.Kind())
	}
	bounds := []float64{1e-05, 0.0001, 0.001, 0.01, 0.1, 1}
	sample := RuntimeHistogram(read[0].Value.Float64Histogram(), bounds)
	if sample.Count == 0 {
		t.Fatal("no garbage collector pause counted after runtime.GC")
	}
	for i := 1; i < len(sample.Cumulative); i++ {
		if sample.Cumulative[i] < sample.Cumulative[i-1] {
			t.Fatalf("Cumulative = %v, which falls", sample.Cumulative)
		}
	}
	if last := sample.Cumulative[len(sample.Cumulative)-1]; last > sample.Count {
		t.Fatalf("last bucket %d exceeds the count %d", last, sample.Count)
	}
	registry := New()
	registry.NewHistogramVecFunc("test_gc_pause_seconds", "GC pauses", func() []HistogramSample { return []HistogramSample{sample} })
	body := scrape(t, registry)
	for _, line := range []string{`test_gc_pause_seconds_bucket{le="1e-05"} `, `test_gc_pause_seconds_bucket{le="+Inf"} `, "test_gc_pause_seconds_sum ", "test_gc_pause_seconds_count "} {
		if !strings.Contains(body, "\n"+line) {
			t.Errorf("scrape lacks a line starting %q:\n%s", line, body)
		}
	}
}
