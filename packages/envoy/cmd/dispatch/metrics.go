package main

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	runtimemetrics "runtime/metrics"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/logging"
	"github.com/sjawhar/envoy/internal/dispatch/outbox"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/metrics"
)

// metricsAddress is where DISPATCH_METRICS_ADDR asks the metrics listener to bind, host:port with
// the host optional (every interface when empty, as ":9090"). Unset or empty, it is "", and
// nothing listens. A value that is not host:port with a port 1-65535 refuses startup naming the
// variable.
func metricsAddress(env settingValues) (string, error) {
	raw := strings.TrimSpace(env.get("DISPATCH_METRICS_ADDR"))
	if raw == "" {
		return "", nil
	}
	host, rawPort, err := net.SplitHostPort(raw)
	if err != nil {
		return "", fmt.Errorf("invalid DISPATCH_METRICS_ADDR %q: want host:port: %w", raw, err)
	}
	port, err := parsePort("DISPATCH_METRICS_ADDR", rawPort)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

// metricsSources is what the metrics endpoint's series are read from.
type metricsSources struct {
	// Pool is the store's pool, whose Stats report every pool the process has opened; Background
	// is the pool the outbox lag is sampled on (outboxLag.sample).
	Pool       *store.Pool
	Background *store.Pool
	Events     *events.Broker
	Admission  *admission.Limiter
	Docs       *docs.Service
	Logs       *logging.Handler
}

// outboxLagInterval is how often the outbox lag is sampled: one query on the background pool every
// five seconds, whatever the scrape rate, so a scraper (or several) never adds a statement per
// scrape to a loaded database. Five seconds is the load test's sampling period.
const outboxLagInterval = 5 * time.Second

// startMetrics binds addr and serves the metrics endpoint, GET /metrics in Prometheus's text
// format, on it until ctx ends, when the listener, every open connection and the outbox lag's
// sampler stop. The endpoint has no authentication: its series carry counts and durations only,
// never a name, a key or a person. An empty addr starts nothing, not even the sampler, and returns
// a nil address. A bind that fails is an error naming DISPATCH_METRICS_ADDR, so a port clash
// refuses the boot.
func startMetrics(ctx context.Context, addr string, sources metricsSources) (net.Addr, error) {
	if addr == "" {
		return nil, nil
	}
	lag := &outboxLag{}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", newDispatchMetrics(sources, lag).Handler())
	bound, err := serveSide(ctx, addr, "DISPATCH_METRICS_ADDR", "metrics", mux)
	if err != nil {
		return nil, err
	}
	go lag.sample(ctx, sources.Background, outboxLagInterval)
	return bound, nil
}

// serveSide binds addr and serves handler on it until ctx ends, when the listener and every open
// connection close at once, for a listener beside the API's (metrics, pprof) named name in its log
// lines. A bind that fails is an error naming variable, the setting addr came from, so a port
// clash refuses the boot.
func serveSide(ctx context.Context, addr, variable, name string, handler http.Handler) (net.Addr, error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("%s: listen on %s: %w", variable, addr, err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: readHeaderTimeout}
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			slog.Error("dispatch: "+name+" listener stopped", "addr", addr, "error", err)
		}
	}()
	slog.Info("dispatch: "+name+" listening", "addr", listener.Addr().String())
	return listener.Addr(), nil
}

// outboxLag holds the outbox lag's latest sample, in seconds, as float64 bits.
type outboxLag struct {
	seconds atomic.Uint64
}

func (l *outboxLag) value() float64 { return math.Float64frombits(l.seconds.Load()) }

// sample reads the outbox lag on pool (outbox.Lag) at once and then every interval until ctx ends.
// A read that fails keeps the last sample and is logged once at WARN, not again until a read has
// succeeded: a database that is down would otherwise log a line every interval.
func (l *outboxLag) sample(ctx context.Context, pool *store.Pool, interval time.Duration) {
	ctx = store.WithTransactionTracking(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	failing := false
	for {
		lag, err := outbox.Lag(ctx, pool)
		switch {
		case err == nil:
			l.seconds.Store(math.Float64bits(lag.Seconds()))
			failing = false
		case ctx.Err() == nil && !failing:
			slog.Warn("dispatch: sample the outbox lag", "error", err)
			failing = true
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// metricPrefix begins the name of every series the endpoint serves.
const metricPrefix = "dispatch_"

// gcPauseBuckets are the upper bounds, in seconds, of dispatch_go_gc_pause_seconds' buckets: from
// ten microseconds, a short stop-the-world phase, to a second, far past any pause a healthy heap
// of Dispatch's size takes.
var gcPauseBuckets = []float64{
	0.00001, 0.000025, 0.00005, 0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01,
	0.025, 0.05, 0.1, 0.25, 0.5, 1,
}

// newDispatchMetrics is the registry the metrics endpoint serves. Every series is read from
// sources when a scrape comes, the pools' from one reading of their Stats a scrape, but the outbox
// lag, which is lag's latest sample. A pool's series appear once the pool has opened: the ones
// opened on first use (rooms, health, the listener's, live, settlements) are absent until
// something uses them, and the outbox's on a server with no NATS, rather than reported as an
// empty pool.
func newDispatchMetrics(sources metricsSources, lag *outboxLag) *metrics.Registry {
	registry := metrics.New()
	// pools is the scrape's reading of every pool's Stats, taken once before any family is written
	// (OnScrape) and read by each pool family. Two scrapes at once each store their own; a family
	// reads whichever is latest, both of them whole.
	var pools atomic.Pointer[[]store.PoolStats]
	registry.OnScrape(func() {
		stats := sources.Pool.Stats()
		pools.Store(&stats)
	})
	poolStats := func() []store.PoolStats {
		if stats := pools.Load(); stats != nil {
			return *stats
		}
		return nil
	}
	poolGauge := func(name, help string, value func(store.PoolStats) float64) {
		registry.NewGaugeVecFunc(metricPrefix+name, help, func() []metrics.Sample {
			return poolSamples(poolStats(), value)
		})
	}
	poolGauge("db_pool_max_conns", "Connections the pool may open.", func(s store.PoolStats) float64 { return float64(s.Stat.MaxConns()) })
	poolGauge("db_pool_total_conns", "Connections the pool holds open, in use or idle, and being opened.", func(s store.PoolStats) float64 { return float64(s.Stat.TotalConns()) })
	poolGauge("db_pool_in_use_conns", "Connections of the pool its callers hold now.", func(s store.PoolStats) float64 { return float64(s.Stat.AcquiredConns()) })
	poolGauge("db_pool_idle_conns", "Open connections of the pool nobody holds.", func(s store.PoolStats) float64 { return float64(s.Stat.IdleConns()) })
	poolGauge("db_pool_waiting", "Acquires of the pool in progress now: callers waiting for a connection.", func(s store.PoolStats) float64 { return float64(s.Acquire.Waiting) })
	registry.NewHistogramVecFunc(metricPrefix+"db_pool_acquire_seconds", "How long each acquire of the pool waited for a connection; its count is the pool's checkouts.", func() []metrics.HistogramSample {
		bounds := store.AcquireBucketBounds()
		stats := poolStats()
		samples := make([]metrics.HistogramSample, 0, len(stats))
		for _, pool := range stats {
			samples = append(samples, metrics.HistogramSample{
				Labels:     poolLabel(pool),
				Bounds:     bounds[:],
				Cumulative: pool.Acquire.Buckets[:],
				Count:      pool.Acquire.Count,
				Sum:        pool.Acquire.Sum,
			})
		}
		return samples
	})
	registry.NewCounterVecFunc(metricPrefix+"db_pool_acquire_timeouts_total", "Acquires of the pool refused for running out its acquire bound (store.ErrPoolBusy).", func() []metrics.Sample {
		return poolSamples(poolStats(), func(s store.PoolStats) float64 { return float64(s.Acquire.Timeouts) })
	})

	registry.NewHistogramVecFunc(metricPrefix+"go_gc_pause_seconds", "Stop-the-world pauses of the garbage collector (/gc/pauses:seconds).", func() []metrics.HistogramSample {
		read := []runtimemetrics.Sample{{Name: "/gc/pauses:seconds"}}
		runtimemetrics.Read(read)
		if read[0].Value.Kind() != runtimemetrics.KindFloat64Histogram {
			return nil
		}
		return []metrics.HistogramSample{metrics.RuntimeHistogram(read[0].Value.Float64Histogram(), gcPauseBuckets)}
	})
	registry.NewGaugeFunc(metricPrefix+"go_heap_live_bytes", "Heap bytes the last garbage collection marked live (/gc/heap/live:bytes).", runtimeValue("/gc/heap/live:bytes"))
	registry.NewGaugeFunc(metricPrefix+"go_heap_objects_bytes", "Heap bytes held by objects, live and not yet swept (/memory/classes/heap/objects:bytes).", runtimeValue("/memory/classes/heap/objects:bytes"))
	registry.NewGaugeFunc(metricPrefix+"go_goroutines", "Goroutines that exist now (/sched/goroutines:goroutines).", runtimeValue("/sched/goroutines:goroutines"))

	registry.NewGaugeVecFunc(metricPrefix+"outbox_lag_seconds", "Age of the oldest event the outbox has not yet published, pending subscription commands aside; 0 when none. Sampled every 5 s.", func() []metrics.Sample {
		return []metrics.Sample{{Value: lag.value()}}
	})
	registry.NewCounterVecFunc(metricPrefix+"outbox_published_total", "Events this process's outbox has marked published.", func() []metrics.Sample {
		return []metrics.Sample{{Value: float64(outbox.Published())}}
	})
	registry.NewCounterVecFunc(metricPrefix+"events_committed_total", "Events this process has committed to the event log.", func() []metrics.Sample {
		return []metrics.Sample{{Value: float64(sources.Events.Committed())}}
	})

	classSamples := func(value func(admission.ClassStats) float64) []metrics.Sample {
		stats := sources.Admission.Stats()
		samples := make([]metrics.Sample, 0, len(stats))
		for _, class := range stats {
			samples = append(samples, metrics.Sample{Labels: [][2]string{{"class", class.Class.String()}}, Value: value(class)})
		}
		return samples
	}
	registry.NewGaugeVecFunc(metricPrefix+"http_in_flight", "Admission units the admitted requests of the class hold now (a route that reads side by side takes more than one).", func() []metrics.Sample {
		return classSamples(func(c admission.ClassStats) float64 { return float64(c.InFlight) })
	})
	registry.NewGaugeVecFunc(metricPrefix+"http_queued", "Requests of the class waiting for admission now.", func() []metrics.Sample {
		return classSamples(func(c admission.ClassStats) float64 { return float64(c.Queued) })
	})
	registry.NewCounterVecFunc(metricPrefix+"http_refused_total", "Requests admission refused, by class and reason.", func() []metrics.Sample {
		stats := sources.Admission.Stats()
		samples := make([]metrics.Sample, 0, 2*len(stats))
		for _, class := range stats {
			samples = append(samples,
				metrics.Sample{Labels: [][2]string{{"class", class.Class.String()}, {"reason", string(admission.QueueFull)}}, Value: float64(class.QueueFull)},
				metrics.Sample{Labels: [][2]string{{"class", class.Class.String()}, {"reason", string(admission.WaitExceeded)}}, Value: float64(class.WaitExceeded)},
			)
		}
		return samples
	})
	registry.NewGaugeFunc(metricPrefix+"http_admitted_units", "Admission units the admitted requests of both classes hold now, under the combined cap.", func() int64 {
		return int64(sources.Admission.Totals().InFlight)
	})
	registry.NewGaugeFunc(metricPrefix+"http_admitted_units_limit", "Admission units both classes may hold together: the combined cap, which is below the two class limits summed.", func() int64 {
		return int64(sources.Admission.Totals().Limit)
	})
	registry.NewGaugeFunc(metricPrefix+"http_admitted_reserved_read_units", "Admission units of the combined cap writes leave to the reads while a read waits for room.", func() int64 {
		return int64(sources.Admission.Totals().ReservedReads)
	})

	registry.NewGaugeFunc(metricPrefix+"doc_writer_queued", "Writers waiting for a document's writer gate now.", sources.Docs.QueuedDocumentWriters)
	registry.NewGaugeFunc(metricPrefix+"settlements_running", "Document settlements holding a settlement slot now.", func() int64 { return sources.Docs.SettlementLoad().Running })
	registry.NewGaugeFunc(metricPrefix+"settlements_waiting", "Document settlements waiting for a settlement slot now.", func() int64 { return sources.Docs.SettlementLoad().Waiting })

	registry.NewCounterVecFunc(metricPrefix+"log_dropped_total", "Log records dropped because the log writer was too far behind, by level.", func() []metrics.Sample {
		samples := make([]metrics.Sample, 0, len(logging.Levels))
		for _, level := range logging.Levels {
			samples = append(samples, metrics.Sample{Labels: [][2]string{{"level", strings.ToLower(level.String())}}, Value: float64(sources.Logs.Dropped(level))})
		}
		return samples
	})
	return registry
}

// poolLabel is a pool's series' label: pool, the pool's name, which is its application_name less
// "dispatch-".
func poolLabel(pool store.PoolStats) [][2]string { return [][2]string{{"pool", pool.Name}} }

// poolSamples is value of each pool in stats, labelled by pool.
func poolSamples(stats []store.PoolStats, value func(store.PoolStats) float64) []metrics.Sample {
	samples := make([]metrics.Sample, 0, len(stats))
	for _, pool := range stats {
		samples = append(samples, metrics.Sample{Labels: poolLabel(pool), Value: value(pool)})
	}
	return samples
}

// runtimeValue reads name, a runtime/metrics value of kind uint64, on each call. Each name it is
// given is one the runtime/metrics package documents as such; one it stopped reporting would panic
// the scrape (Value.Uint64), which the metrics endpoint's test catches.
func runtimeValue(name string) func() int64 {
	return func() int64 {
		read := []runtimemetrics.Sample{{Name: name}}
		runtimemetrics.Read(read)
		return int64(read[0].Value.Uint64())
	}
}
