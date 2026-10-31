package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/logging"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// openEveryPool opens each pool database's Pool opens on demand, so each has its series.
func openEveryPool(t *testing.T, database *store.Store) (background *store.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Rooms(); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Healthy(ctx); err != nil {
		t.Fatal(err)
	}
	listen, err := database.Pool.Listener(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listen.Close(context.Background()) })
	if _, err := database.Pool.Leases(); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Outbox(); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Live(); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Settlements(); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Horizon(); err != nil {
		t.Fatal(err)
	}
	background, err = database.Pool.Background()
	if err != nil {
		t.Fatal(err)
	}
	return background
}

// scrapeMetrics answers GET /metrics on addr.
func scrapeMetrics(t *testing.T, addr net.Addr) string {
	t.Helper()
	response, err := http.Get("http://" + addr.String() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics = %d %s", response.StatusCode, body)
	}
	return string(body)
}

// The metrics endpoint main starts, scraped with every pool of the process open, lists each
// per-pool series for every pool by its name, the acquire histogram whole, and every process-wide
// series the load test samples; and it stops listening when the process's context ends.
func TestTheMetricsEndpointListsEverySeriesForEveryOpenPool(t *testing.T) {
	database := storetest.Open(t)
	background := openEveryPool(t, database)
	broker := events.NewBroker()
	documents := docs.New(docs.Deps{Store: database, Events: broker})
	t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bound, err := startMetrics(ctx, "127.0.0.1:0", metricsSources{
		Pool:       database.Pool,
		Background: background,
		Events:     broker,
		Admission:  admission.New(admission.DefaultLimits()),
		Docs:       documents,
		Logs:       logging.New(io.Discard, nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	body := scrapeMetrics(t, bound)

	stats := database.Pool.Stats()
	pools := []string{
		store.RequestsPool, store.RoomsPool, store.HealthPool, store.ListenPool, store.LeasesPool,
		store.OutboxPool, store.BackgroundPool, store.LivePool, store.SettlementsPool, store.HorizonPool,
	}
	if len(stats) != len(pools) {
		t.Fatalf("Stats reports %d pools, want the %d this test opened", len(stats), len(pools))
	}
	for _, pool := range pools {
		label := `{pool="` + pool + `"}`
		for _, series := range []string{
			"dispatch_db_pool_max_conns", "dispatch_db_pool_total_conns", "dispatch_db_pool_in_use_conns",
			"dispatch_db_pool_idle_conns", "dispatch_db_pool_waiting", "dispatch_db_pool_acquire_timeouts_total",
			"dispatch_db_pool_acquire_seconds_sum", "dispatch_db_pool_acquire_seconds_count",
		} {
			requireLine(t, body, series+label+" ")
		}
		requireLine(t, body, `dispatch_db_pool_acquire_seconds_bucket{pool="`+pool+`",le="0.001"} `)
		requireLine(t, body, `dispatch_db_pool_acquire_seconds_bucket{pool="`+pool+`",le="30"} `)
		requireLine(t, body, `dispatch_db_pool_acquire_seconds_bucket{pool="`+pool+`",le="+Inf"} `)
	}
	for _, pool := range stats {
		requireLine(t, body, `dispatch_db_pool_max_conns{pool="`+pool.Name+`"} `+strconv.Itoa(int(pool.Stat.MaxConns()))+"\n")
	}
	for _, line := range []string{
		`dispatch_go_gc_pause_seconds_bucket{le="1e-05"} `, `dispatch_go_gc_pause_seconds_bucket{le="+Inf"} `,
		"dispatch_go_gc_pause_seconds_sum ", "dispatch_go_gc_pause_seconds_count ",
		"dispatch_go_heap_live_bytes ", "dispatch_go_heap_objects_bytes ", "dispatch_go_goroutines ",
		"dispatch_outbox_lag_seconds 0\n", "dispatch_outbox_published_total ", "dispatch_events_committed_total 0\n",
		`dispatch_http_in_flight{class="read"} 0`, `dispatch_http_in_flight{class="write"} 0`,
		`dispatch_http_queued{class="read"} 0`, `dispatch_http_queued{class="write"} 0`,
		`dispatch_http_refused_total{class="read",reason="queue_full"} 0`, `dispatch_http_refused_total{class="write",reason="wait_exceeded"} 0`,
		"dispatch_http_admitted_units 0\n",
		"dispatch_http_admitted_units_limit " + strconv.Itoa(admission.MaxAdmittedUnits) + "\n",
		"dispatch_doc_writer_queued 0\n", "dispatch_settlements_running 0\n", "dispatch_settlements_waiting 0\n",
		`dispatch_log_dropped_total{level="debug"} 0`, `dispatch_log_dropped_total{level="error"} 0`,
	} {
		requireLine(t, body, line)
	}
	if strings.Contains(body, "dispatch_go_goroutines 0\n") {
		t.Error("dispatch_go_goroutines is 0, in a process running this test")
	}

	cancel()
	requireListenerClosed(t, bound)
}

// requireLine fails t unless body has a line that begins with prefix.
func requireLine(t *testing.T, body, prefix string) {
	t.Helper()
	if !strings.HasPrefix(body, prefix) && !strings.Contains(body, "\n"+prefix) {
		t.Errorf("the scrape has no line beginning %q", prefix)
	}
}

// requireListenerClosed waits up to two seconds for addr to stop accepting connections.
func requireListenerClosed(t *testing.T, addr net.Addr) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr.String(), 100*time.Millisecond)
		if err != nil {
			return
		}
		conn.Close()
		if time.Now().After(deadline) {
			t.Fatalf("%s still accepts connections after the shutdown context ended", addr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Unset, DISPATCH_METRICS_ADDR starts no listener.
func TestMetricsUnsetStartsNoListener(t *testing.T) {
	bound, err := startMetrics(context.Background(), "", metricsSources{})
	if err != nil || bound != nil {
		t.Fatalf("startMetrics with no address = %v, %v; want no listener", bound, err)
	}
}

// An address another process holds is a startup error naming the variable, never a silent skip.
func TestMetricsAddressInUseIsAnError(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if _, err := startMetrics(context.Background(), held.Addr().String(), metricsSources{}); err == nil || !strings.Contains(err.Error(), "DISPATCH_METRICS_ADDR") {
		t.Fatalf("startMetrics on a held address: err = %v; want an error naming DISPATCH_METRICS_ADDR", err)
	}
}

// mutexSample is a sample line of a mutex profile in pprof's text form: its contentions, its delay,
// and its stack.
var mutexSample = regexp.MustCompile(`(?m)^\d+ \d+ @ 0x`)

// With the profiler on, the mutex profile records contention: one taken after goroutines fight
// over a lock holds a sample, where a process that set no mutex rate records none.
func TestAMutexProfileTakenWithPprofOnIsNotEmpty(t *testing.T) {
	t.Cleanup(func() {
		runtime.SetMutexProfileFraction(0)
		runtime.SetBlockProfileRate(0)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bound, err := startPprof(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		contend()
		response, err := http.Get("http://" + bound.String() + "/debug/pprof/mutex?debug=1")
		if err != nil {
			t.Fatal(err)
		}
		profile, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("GET /debug/pprof/mutex = %d %s", response.StatusCode, profile)
		}
		if mutexSample.Match(profile) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the mutex profile holds no sample after 10 s of contention:\n%s", profile)
		}
	}
}

// contend has eight goroutines take one mutex a thousand times each, yielding while they hold it,
// so the others block on it.
func contend() {
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 1000 {
				mu.Lock()
				runtime.Gosched()
				mu.Unlock()
			}
		})
	}
	wg.Wait()
}

// A serving process's scrape lists every pool of its connection budget (store.ConnectionsPerProcess)
// by name, the registry's included: processes.Register opens that one-connection pool beside the
// store's, as dispatch-registry, and the heartbeat it carries is what keeps a serving process live,
// so a heartbeat waiting for its connection, or timing out, is a wait an operator must read here
// as for any other pool.
func TestAServingProcessScrapeListsTheRegistryPool(t *testing.T) {
	database := storetest.Open(t)
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := held.Addr().String()
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{
		Env: map[string]string{"DISPATCH_METRICS_ADDR": addr},
	})
	process.waitHealthy(t)
	response, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	scraped, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics = %d %s", response.StatusCode, scraped)
	}
	body := string(scraped)
	label := `{pool="` + store.RegistryPool + `"}`
	for _, series := range []string{
		"dispatch_db_pool_total_conns", "dispatch_db_pool_in_use_conns", "dispatch_db_pool_idle_conns",
		"dispatch_db_pool_waiting", "dispatch_db_pool_acquire_timeouts_total",
		"dispatch_db_pool_acquire_seconds_sum", "dispatch_db_pool_acquire_seconds_count",
	} {
		requireLine(t, body, series+label+" ")
	}
	requireLine(t, body, "dispatch_db_pool_max_conns"+label+" 1\n")
}
