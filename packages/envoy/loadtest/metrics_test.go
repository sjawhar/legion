package main

import (
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const metricsText = `# HELP dispatch_db_pool_waiting Acquires of the pool in progress.
# TYPE dispatch_db_pool_waiting gauge
dispatch_db_pool_waiting{pool="requests"} 3
dispatch_db_pool_waiting{pool="rooms"} 0

dispatch_db_pool_acquire_seconds_bucket{pool="requests",le="0.001"} 10
dispatch_db_pool_acquire_seconds_bucket{le="+Inf", pool="requests"} 12 1730000000000
dispatch_http_refused_total{class="read",reason="queue_full"} 1.5e1
dispatch_doc_writer_queued 0
dispatch_label_escapes{path="a\"b\\c\nd"} 1
go_goroutines 42
process_cpu_seconds_total 1.25
`

func TestParseMetricsKeepsTheDispatchSeries(t *testing.T) {
	m, err := parseMetrics(strings.NewReader(metricsText), metricsPrefix)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{
		`dispatch_db_pool_waiting{pool="requests"}`:                           3,
		`dispatch_db_pool_waiting{pool="rooms"}`:                              0,
		`dispatch_db_pool_acquire_seconds_bucket{le="0.001",pool="requests"}`: 10,
		`dispatch_db_pool_acquire_seconds_bucket{le="+Inf",pool="requests"}`:  12,
		`dispatch_http_refused_total{class="read",reason="queue_full"}`:       15,
		`dispatch_doc_writer_queued`:                                          0,
		`dispatch_label_escapes{path="a\"b\\c\nd"}`:                           1,
	}
	if len(m) != len(want) {
		t.Fatalf("parsed %d series %v, want %d", len(m), m, len(want))
	}
	for k, v := range want {
		if got, ok := m[k]; !ok || got != v {
			t.Errorf("%s = %v (present %t), want %v", k, got, ok, v)
		}
	}
	name, labels := splitMetricKey(`dispatch_label_escapes{path="a\"b\\c\nd"}`)
	if name != "dispatch_label_escapes" || labels["path"] != "a\"b\\c\nd" {
		t.Errorf("splitMetricKey: %q %q", name, labels)
	}
	for _, bad := range []string{
		"dispatch_x{pool=\"requests\" 1\n",
		"dispatch_x{pool=requests} 1\n",
		"dispatch_x\n",
		"dispatch_x one\n",
		"dispatch_x 1 2 3\n",
	} {
		if _, err := parseMetrics(strings.NewReader(bad), metricsPrefix); err == nil {
			t.Errorf("%q parsed without an error", bad)
		}
	}
}

func TestScrapeMetricsFailsWithoutTheEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/ok/"):
			fmt.Fprint(w, metricsText)
		case strings.HasPrefix(r.URL.Path, "/other/"):
			fmt.Fprint(w, "go_goroutines 3\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	if m, err := scrapeMetrics(srv.URL + "/ok"); err != nil || m[`dispatch_doc_writer_queued`] != 0 || len(m) != 7 {
		t.Fatalf("scrapeMetrics = %v, %v", m, err)
	}
	for _, base := range []string{srv.URL + "/missing", srv.URL + "/other", "http://127.0.0.1:1"} {
		if _, err := scrapeMetrics(base); err == nil {
			t.Errorf("scrapeMetrics(%s) found no dispatch series and no error", base)
		}
	}
}

func TestHistogramPctIsTheUpperBoundOfThePercentilesBucket(t *testing.T) {
	inf := math.Inf(1)
	for _, c := range []struct {
		name    string
		buckets []histBucket
		want    float64
		found   bool
	}{
		// 1,000 acquires: 980 under 1 ms, 15 more under 5 ms, 5 more under 30 s. The 99th
		// percentile (rank 990) is in the 5 ms bucket.
		{"p99 in a middle bucket", []histBucket{{inf, 1000}, {0.001, 980}, {0.005, 995}, {30, 1000}}, 0.005, true},
		{"p99 in the first bucket", []histBucket{{0.001, 100}, {0.005, 100}, {inf, 100}}, 0.001, true},
		{"p99 past the last finite bound", []histBucket{{0.001, 10}, {30, 90}, {inf, 100}}, inf, true},
		{"no acquire", []histBucket{{0.001, 0}, {inf, 0}}, 0, false},
		{"no bucket", nil, 0, false},
	} {
		got, found := histogramPct(c.buckets, 99)
		if found != c.found || (found && got != c.want) {
			t.Errorf("%s: %v, %t; want %v, %t", c.name, got, found, c.want, c.found)
		}
	}
}

// metricsSample is an in-window sample at second at with the given series.
func metricsSample(at int, series map[string]float64) sample {
	return sample{At: time.Date(2026, 10, 28, 9, 0, at, 0, time.UTC), InWindow: true, Metrics: series}
}

func TestSummarizeMetricsTakesMaximaAndDeltas(t *testing.T) {
	bucket := func(pool, le string) string {
		return metricKey("dispatch_db_pool_acquire_seconds_bucket", map[string]string{"pool": pool, "le": le})
	}
	pool := func(name, p string) string { return metricKey(name, map[string]string{"pool": p}) }
	class := func(name, c string) string { return metricKey(name, map[string]string{"class": c}) }
	refused := func(c, reason string) string {
		return metricKey("dispatch_http_refused_total", map[string]string{"class": c, "reason": reason})
	}
	dropped := func(level string) string {
		return metricKey("dispatch_log_dropped_total", map[string]string{"level": level})
	}
	first := map[string]float64{
		bucket("requests", "0.001"): 100, bucket("requests", "0.005"): 100, bucket("requests", "30"): 100, bucket("requests", "+Inf"): 100,
		pool("dispatch_db_pool_acquire_seconds_count", "requests"): 100,
		pool("dispatch_db_pool_waiting", "requests"):               1, pool("dispatch_db_pool_in_use_conns", "requests"): 10,
		class("dispatch_http_queued", "read"): 0, class("dispatch_http_queued", "write"): 2,
		class("dispatch_http_in_flight", "read"): 5, class("dispatch_http_in_flight", "write"): 1,
		refused("read", "queue_full"): 3, refused("write", "queue_full"): 1, refused("read", "wait_exceeded"): 0,
		"dispatch_doc_writer_queued": 0, "dispatch_settlements_running": 1, "dispatch_settlements_waiting": 0,
		dropped("info"): 5, dropped("debug"): 0,
	}
	mid := map[string]float64{
		pool("dispatch_db_pool_waiting", "requests"): 40, pool("dispatch_db_pool_in_use_conns", "requests"): 64,
		class("dispatch_http_queued", "write"): 9, "dispatch_doc_writer_queued": 4, "dispatch_settlements_waiting": 3,
	}
	// From first to last: 1,000 acquires, 980 under 1 ms and 15 more under 5 ms; the background
	// pool opened during the window, and all of its 10 acquires waited past 30 s.
	last := map[string]float64{
		bucket("requests", "0.001"): 1080, bucket("requests", "0.005"): 1095, bucket("requests", "30"): 1100, bucket("requests", "+Inf"): 1100,
		pool("dispatch_db_pool_acquire_seconds_count", "requests"): 1100,
		bucket("background", "0.001"):                              0, bucket("background", "30"): 0, bucket("background", "+Inf"): 10,
		pool("dispatch_db_pool_acquire_seconds_count", "background"): 10,
		pool("dispatch_db_pool_waiting", "requests"):                 0, pool("dispatch_db_pool_in_use_conns", "requests"): 12,
		class("dispatch_http_queued", "read"): 1, class("dispatch_http_queued", "write"): 0,
		class("dispatch_http_in_flight", "read"): 7, class("dispatch_http_in_flight", "write"): 1,
		refused("read", "queue_full"): 10, refused("write", "queue_full"): 4, refused("read", "wait_exceeded"): 2,
		"dispatch_doc_writer_queued": 1, "dispatch_settlements_running": 2, "dispatch_settlements_waiting": 0,
		dropped("info"): 9, dropped("debug"): 1,
	}
	failed := sample{At: time.Date(2026, 10, 28, 9, 0, 7, 0, time.UTC), InWindow: true, FailedReads: []string{readMetrics}}
	outside := metricsSample(59, map[string]float64{pool("dispatch_db_pool_waiting", "requests"): 999})
	outside.InWindow = false
	m := summarizeMetrics([]sample{metricsSample(0, first), metricsSample(5, mid), failed, metricsSample(10, last), outside}, 500)

	if m.Scraped != 3 || m.Skipped != 1 {
		t.Fatalf("scraped %d skipped %d, want 3 and 1", m.Scraped, m.Skipped)
	}
	req := m.Pools["requests"]
	if req.WaitingMax == nil || *req.WaitingMax != 40 || req.InUseMax == nil || *req.InUseMax != 64 {
		t.Errorf("requests pool waiting %v in use %v, want maxima 40 and 64", req.WaitingMax, req.InUseMax)
	}
	if req.AcquireP99Ms == nil || *req.AcquireP99Ms != 5 || req.AcquireP99AboveMs != nil {
		t.Errorf("requests pool acquire p99 %v (above %v), want the 5 ms bucket", req.AcquireP99Ms, req.AcquireP99AboveMs)
	}
	if req.Checkouts == nil || *req.Checkouts != 1000 {
		t.Errorf("requests pool checkouts %v, want 1000", req.Checkouts)
	}
	bg := m.Pools["background"]
	if bg.AcquireP99Ms != nil || bg.AcquireP99AboveMs == nil || *bg.AcquireP99AboveMs != 30000 || bg.WaitingMax != nil {
		t.Errorf("background pool: p99 %v above %v waiting %v; want past 30,000 ms and no waiting series", bg.AcquireP99Ms, bg.AcquireP99AboveMs, bg.WaitingMax)
	}
	if m.CheckoutsPerRequest == nil || *m.CheckoutsPerRequest != 2 {
		t.Errorf("checkouts per request %v, want 1000 / 500 = 2", m.CheckoutsPerRequest)
	}
	if m.HTTPQueuedMax["read"] != 1 || m.HTTPQueuedMax["write"] != 9 || m.HTTPInFlightMax["read"] != 7 || m.HTTPInFlightMax["write"] != 1 {
		t.Errorf("HTTP queued %v in flight %v", m.HTTPQueuedMax, m.HTTPInFlightMax)
	}
	if m.HTTPRefused["queue_full"] != 10 || m.HTTPRefused["wait_exceeded"] != 2 {
		t.Errorf("HTTP refused %v, want queue_full 10 (both classes) and wait_exceeded 2", m.HTTPRefused)
	}
	if *m.DocWriterQueuedMax != 4 || *m.SettlementsRunningMax != 2 || *m.SettlementsWaitingMax != 3 || *m.LogDropped != 5 {
		t.Errorf("doc writer %v settlements %v / %v log dropped %v; want 4, 2 / 3, 5",
			*m.DocWriterQueuedMax, *m.SettlementsRunningMax, *m.SettlementsWaitingMax, *m.LogDropped)
	}

	// A build without the endpoint: every sample skipped, nothing measured.
	none := summarizeMetrics([]sample{failed, failed}, 500)
	if none.Scraped != 0 || none.Skipped != 2 || none.Pools != nil || none.CheckoutsPerRequest != nil || none.LogDropped != nil {
		t.Errorf("no scrape worked: %+v", none)
	}
	// One sample: maxima, but no delta.
	one := summarizeMetrics([]sample{metricsSample(0, first)}, 500)
	if one.Pools["requests"].WaitingMax == nil || one.Pools["requests"].Checkouts != nil || one.CheckoutsPerRequest != nil || one.LogDropped != nil {
		t.Errorf("one sample: %+v", one)
	}
}
