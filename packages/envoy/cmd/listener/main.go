package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/config"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dedupe"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/webhook"
)

// listenerDeps holds the NATS-dependent resources, all open. main builds the /v1 handlers over it
// once the stores are open; /healthz and the metrics gauges, which answer during startup too, read
// it through an atomic.Pointer that stays nil until main has started the receive loop.
type listenerDeps struct {
	client   *bus.Client
	registry *store.Registry
	sessions *session.SessionRegistry
	// ciStore is nil on a listener that mounts no GitHub webhook route, the one route that uses it.
	ciStore *cistore.Store
	// live is the live topics lane's own client (openLiveTopics), nil when ENVOY_SUBSCRIBE_ALL_TOPICS
	// leaves the lane off.
	live *bus.Client
	// caches lists the stores above that are open, for the sites that treat every cache alike.
	caches []listenerCache
	// receiver is the durable's receive loop, whose role and lag /healthz reports.
	receiver   *durableReceiver
	streamName string
	streamInfo streamInfoLookup
}

// kvCache is what the listener asks of each KV-backed cache it keeps.
type kvCache interface {
	Rewatch(*nats.Conn) error
	WatchErr() error
	Ping() error
	StopWatch()
	EndWatch(context.Context)
}

// listenerCache is one cache under the name /healthz and self-health report it by.
type listenerCache struct {
	name  string
	cache kvCache
}

// listenerCaches lists the caches the listener keeps. It is the one place that names them, so
// rewatch, self-health, /healthz and shutdown each reach every cache. ciStore is nil on a listener
// that mounts no GitHub webhook route, and then no site reaches a CI cache.
func listenerCaches(registry *store.Registry, sessions *session.SessionRegistry, ciStore *cistore.Store) []listenerCache {
	caches := []listenerCache{
		{name: "interest", cache: registry},
		{name: "session", cache: sessions},
	}
	if ciStore != nil {
		caches = append(caches, listenerCache{name: "CI", cache: ciStore})
	}
	return caches
}

func isSessionLive(sessions *session.SessionRegistry, sessionID string) bool {
	_, err := sessions.Get(sessionID)
	return err == nil
}

// roleHolderMayBeLive is the role reaper's liveness check: a claim's holder is gone only when the
// session bucket itself says so (roleHolderSession), so the reaper never ends a claim another listener
// accepted a moment ago. A read that did not answer keeps the claim for the next cycle.
func roleHolderMayBeLive(logger *logging.Logger, sessions *session.SessionRegistry, sessionID string) bool {
	_, err := roleHolderSession(sessions, sessionID, registered)
	if err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
		logger.Warn("role claim reaper kept a claim whose holder's session it could not read", slog.String("session_id", sessionID), slog.String("error", err.Error()))
	}
	return !errors.Is(err, nats.ErrKeyNotFound)
}

// rewatchListenerKVWatchers recreates cache watchers that are not represented
// by bus.Client subscriptions. It attempts every one so a failed rebuild of one
// cannot leave the other caches permanently stale too.
func rewatchListenerKVWatchers(conn *nats.Conn, caches []listenerCache) error {
	var errs []error
	for _, c := range caches {
		if err := c.cache.Rewatch(conn); err != nil {
			errs = append(errs, fmt.Errorf("rewatch %s cache: %w", c.name, err))
		}
	}
	return errors.Join(errs...)
}

// endListenerKVWatchers ends every cache's watchers at shutdown, all at once and within ctx, while
// the connection still carries their consumer deletes (kvwatch.Watcher.End); the bus's Drain runs
// it as a drain hook before it drains the connection.
func endListenerKVWatchers(ctx context.Context, caches []listenerCache) {
	var ended sync.WaitGroup
	for _, c := range caches {
		ended.Go(func() { c.cache.EndWatch(ctx) })
	}
	ended.Wait()
}

// isUnrecoverableSelfHealthFailure distinguishes state that must be rebuilt
// from transient JetStream deadlines. Rebuild only while the NATS client is
// connected; a disconnected client owns its own infinite reconnect loop. A
// missing bucket is terminal too: no watcher notices envoy_roles going, and a
// rebuild cannot create a bucket, so its failed rebuilds end in a restart,
// whose store Open creates it again.
func isUnrecoverableSelfHealthFailure(err error, client *bus.Client, caches []listenerCache) bool {
	if err == nil || !client.Connected() {
		return false
	}
	if errors.Is(err, nats.ErrConsumerNotFound) || errors.Is(err, errDurableUnbound) ||
		errors.Is(err, nats.ErrConnectionClosed) || errors.Is(err, nats.ErrStreamNotFound) {
		return true
	}
	for _, c := range caches {
		if c.cache.WatchErr() != nil {
			return true
		}
	}
	return false
}

// rebuildListenerDependencies rewatches the KV caches and, when the durable probe reports the
// consumer gone or not delivering to this listener's subscription, subscribes the durable again
// (resubscribe), creating it when it is gone. Only the receiving pod's probe reports either; a
// standby's never does, so a standby's rebuild touches nothing but the caches.
func rebuildListenerDependencies(
	ctx context.Context,
	client *bus.Client,
	caches []listenerCache,
	durableProbe func() error,
	resubscribe func(context.Context) error,
) error {
	if !client.Connected() {
		return nats.ErrConnectionClosed
	}
	err := rewatchListenerKVWatchers(client.Conn, caches)
	if durableProbe == nil {
		return err
	}
	probeErr := durableProbe()
	if !errors.Is(probeErr, nats.ErrConsumerNotFound) && !errors.Is(probeErr, errDurableUnbound) {
		return err
	}
	if durableErr := resubscribe(ctx); durableErr != nil {
		return errors.Join(err, fmt.Errorf("resubscribe the durable consumer: %w", durableErr))
	}
	return err
}

// disconnectedFailureThreshold bounds how many consecutive probe failures the self-health monitor
// tolerates while the NATS client reads as not connected (RECONNECTING or CLOSED, mid-recovery).
// nats.go retries a dropped connection every second (ReconnectWait) on its own, so a client still
// disconnected after this many intervals is not inside a brief reconnect gap: the transport itself
// has not come back (a NATS restart or deploy taking longer than expected, or a local forward that
// keeps accepting connections without actually reaching the server), and nats.go's in-place
// reconnect has no way to notice that and give up on its own. At the production interval (30s)
// this is 10 minutes of reading as disconnected. A transport that stalls the open connection
// rather than closing it reads as connected until nats.go's ping check marks it stale (its
// defaults: a ping every 2 minutes, 2 outstanding), so there the exit comes about 15 minutes
// after the stall.
const disconnectedFailureThreshold = 20

// runSelfHealthMonitor leaves transient dependency timeouts degraded while
// NATS reconnects. A terminal watcher, closed KV handle, or missing durable
// consumer is rebuilt immediately, and a rebuild that reports success is
// probed at once: a healthy probe is a recovery and resets the count, so
// separate faults that each rebuild repairs never add up. A failed probe
// there, transient or terminal, keeps the count and becomes the error the
// terminal line names. Only a terminal failure still there after threshold
// consecutive rebuilds enters the bounded shutdown path so Docker can replace
// an unrecoverable listener.
// A probe that lands in a reconnect gap is that transient case: it fails at
// once with "outbound buffer limit exceeded" while the bus is disconnected (its
// reconnect buffer is off), or at its deadline when the reconnect lands during
// the probe. Either is one WARN, then "self-health recovered" from the next -
// unless connected reads false for disconnectedFailureThreshold consecutive
// probes, in which case the bounded shutdown path runs for this too. That
// count is its own, advanced only while connected reads false and reset the
// moment it reads true again, so it never mixes with a transient failure that
// happens while the client is connected (an unclassified JetStream timeout,
// say), which keeps retrying under the unbounded path above.
func runSelfHealthMonitor(
	ctx context.Context,
	logger *logging.Logger,
	probe func() error,
	isUnrecoverable func(error) bool,
	connected func() bool,
	rebuild func() error,
	terminate func(),
	interval time.Duration,
	threshold int,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	failures := 0
	terminalFailures := 0
	disconnectedFailures := 0
	// readsDisconnected classifies every probe result, the one right after a rebuild included, so
	// the disconnected count is the same run of probes wherever in the loop they fall.
	readsDisconnected := func() bool { return connected != nil && !connected() }
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		err := probe()
		if err != nil {
			failures++
			disconnected := readsDisconnected()
			bound := threshold
			if disconnected {
				bound = disconnectedFailureThreshold
			}
			logger.Warn("self-health probe failed", slog.Int("consecutive", failures), slog.Int("threshold", bound), slog.String("error", err.Error()))
			if disconnected {
				disconnectedFailures++
				terminalFailures = 0
				if disconnectedFailures >= disconnectedFailureThreshold {
					logger.Error("self-health disconnected failure threshold exceeded; terminating for restart",
						slog.Int("consecutive", disconnectedFailures),
						slog.String("error", err.Error()),
					)
					if terminate != nil {
						terminate()
					}
					return
				}
				continue
			}
			disconnectedFailures = 0
			if isUnrecoverable == nil || !isUnrecoverable(err) {
				terminalFailures = 0
				continue
			}
			terminalFailures++
			if rebuild != nil {
				if rebuildErr := rebuild(); rebuildErr != nil {
					// The rebuild runs under ctx, so a shutdown that cancels ctx mid-rebuild cuts it
					// with context.Canceled. That error is the shutdown, not a failed rebuild, and an
					// ordered shutdown writes no ERROR, so the monitor stops here without logging it.
					if ctx.Err() != nil {
						return
					}
					logger.Error("self-health rebuild failed", slog.Int("consecutive", terminalFailures), slog.String("error", rebuildErr.Error()))
				} else {
					logger.Info("self-health rebuild started", slog.Int("consecutive", terminalFailures))
					// Whatever this probe reads, transient or terminal, the count keeps its
					// increment: only a healthy read is a recovery.
					if err = probe(); err != nil {
						logger.Warn("self-health probe after rebuild failed", slog.Int("consecutive", terminalFailures), slog.String("error", err.Error()))
						if readsDisconnected() {
							// The connected branch above reset the count, so this probe starts it.
							disconnectedFailures = 1
						}
					}
				}
			}
		}
		if err == nil {
			if failures > 0 {
				logger.Info("self-health recovered", slog.Int("prior_consecutive_failures", failures))
			}
			failures = 0
			terminalFailures = 0
			disconnectedFailures = 0
			continue
		}
		if terminalFailures < threshold {
			continue
		}
		logger.Error("self-health terminal failure threshold exceeded; terminating for restart",
			slog.String("error", err.Error()),
		)
		if terminate != nil {
			terminate()
		}
		return
	}
}

// checkSelfHealth pings the KV-backed caches and the optional durable
// consumer probe. Returns the first error encountered, or nil when all are
// healthy.
func checkSelfHealth(caches []listenerCache, durable func() error) error {
	for _, c := range caches {
		if err := c.cache.Ping(); err != nil {
			return fmt.Errorf("%s kv: %w", c.name, err)
		}
	}
	if durable != nil {
		if err := durable(); err != nil {
			return fmt.Errorf("durable consumer: %w", err)
		}
	}
	return nil
}

// sessionHealthFields returns observability fields about the session cache for
// /healthz. Kept separate from the handler so it stays cheap (no Keys()/List(),
// just in-memory reads) and unit-testable.
func sessionHealthFields(sessions *session.SessionRegistry) map[string]interface{} {
	watchError := ""
	if err := sessions.WatchErr(); err != nil {
		watchError = err.Error()
	}
	return map[string]interface{}{
		"session_cache_ready": sessions.CacheReady(),
		"session_cache_size":  sessions.CacheSize(),
		"session_watch_error": watchError,
	}
}

// healthzHandler is the listener's /healthz. It answers 200 "starting" until main publishes deps
// once the stores are open and the receive loop has started; then 200 "healthy" once every
// dependency answers, naming this pod's "role": "active" while it receives the durable (it holds
// the lease and is subscribed, or it runs without a lease and receives), with the durable's lag
// (num_pending, num_ack_pending), or "standby" otherwise, naming the holder in "active_instance"
// when the lease record names one and, while it holds the lease but does not receive yet, what it
// waits for in "receive". A standby is healthy: it serves /v1, the webhooks and the role lane, and
// takes over delivery when the lease comes free. A listener without the lease's grant says so in
// "lease" (listenerLeaseUnavailable). It answers 503 "unhealthy" when NATS is unavailable, a
// cache's watcher has stopped, or on the active pod when its subscription is inactive, the durable
// consumer is gone, or the durable consumer is "not bound to this listener's subscription"
// because it was recreated server-side and now delivers elsewhere; and 200 "degraded" for a
// transient KV failure the self-health monitor and the NATS reconnect retry, or an active pod's
// durable lag that could not be read. A listener that mounts no GitHub webhook route keeps no CI
// cache, and its answer carries "ci_cache": "not_applicable". "live_topics" says whether the live
// topics lane is "subscribed" or, with ENVOY_SUBSCRIBE_ALL_TOPICS off, "not_applicable"; with it
// on, a live client that is not connected or holds no valid subscription is 503 "live
// subscription inactive", since every topic the stream does not store then reaches no session.
func healthzHandler(deps *atomic.Pointer[listenerDeps]) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		d := deps.Load()
		if d == nil {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "starting"})
			return
		}
		if err := d.client.Conn.FlushTimeout(3 * time.Second); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "unhealthy", "error": "nats unavailable"})
			return
		}
		if d.live != nil && (!d.live.Connected() || !d.live.CoreSubOK()) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "unhealthy", "error": "live subscription inactive"})
			return
		}
		for _, c := range d.caches {
			if err := c.cache.WatchErr(); err != nil {
				writeDependencyHealth(w, c.name+" KV watcher", err, true)
				return
			}
		}
		for _, c := range d.caches {
			if err := c.cache.Ping(); err != nil {
				writeDependencyHealth(w, c.name+" KV", err, c.cache.WatchErr() != nil || errors.Is(err, nats.ErrConnectionClosed))
				return
			}
		}
		response := map[string]interface{}{"status": "healthy"}
		for k, v := range sessionHealthFields(d.sessions) {
			response[k] = v
		}
		if d.ciStore == nil {
			response["ci_cache"] = "not_applicable"
		}
		response["live_topics"] = "not_applicable"
		if d.live != nil {
			response["live_topics"] = "subscribed"
		}
		report := d.receiver.report()
		if report.NoLease {
			response["lease"] = listenerLeaseUnavailable
		}
		if report.State != receiveActive {
			response["role"] = "standby"
			if report.Lease.ActiveInstance != "" {
				response["active_instance"] = report.Lease.ActiveInstance
			}
			if report.State != receiveStandby {
				response["receive"] = report.State.String()
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(response)
			return
		}
		response["role"] = "active"
		if !report.SubscriptionValid {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "unhealthy", "role": "active", "error": "subscription inactive"})
			return
		}
		consumerInfo, err := d.client.JS().ConsumerInfo(bus.Stream, d.receiver.consumer, nats.Context(r.Context()))
		switch {
		case errors.Is(err, nats.ErrConsumerNotFound):
			writeDependencyHealth(w, "durable consumer", err, true)
			return
		case err != nil:
			response["status"] = "degraded"
			response["error"] = "durable consumer unavailable: " + err.Error()
		case consumerInfo.Config.DeliverSubject != report.Subject:
			// One read and no confirming second one, unlike the self-health probe's: the endpoint
			// reports the fault the moment a request finds it, and a request landing in a
			// resubscribe's own gap simply answers 503 until the next one.
			writeDependencyHealth(w, "durable consumer", errDurableUnbound, true)
			return
		default:
			response["num_pending"] = consumerInfo.NumPending
			response["num_ack_pending"] = consumerInfo.NumAckPending
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(response)
	}
}

func writeDependencyHealth(w http.ResponseWriter, dependency string, err error, terminal bool) {
	statusCode := http.StatusOK
	status := "degraded"
	if terminal {
		statusCode = http.StatusServiceUnavailable
		status = "unhealthy"
	}
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status": status,
		"error":  dependency + " unavailable: " + err.Error(),
	})
}

// webhookRoute is one configured webhook path and the handler it serves over the only
// dependencies a webhook uses: the NATS client it publishes through and the CI store the GitHub
// route records checks in, which main opens only when that route is configured.
type webhookRoute struct {
	path    string
	handler func(*bus.Client, *cistore.Store) http.Handler
}

// webhookRoutes lists the webhook routes the configuration enables. main registers the paths
// before NATS is up, so they answer 503 while it connects, and builds the handlers once NATS and,
// for the GitHub route, the CI store are open.
func webhookRoutes(cfg *webhook.WebhookConfig) []webhookRoute {
	var routes []webhookRoute
	if github := cfg.GitHub; github != nil {
		routes = append(routes, webhookRoute{"/webhook/github", func(client *bus.Client, ciStore *cistore.Store) http.Handler {
			return webhook.GitHubHandler(github.Secret, github.MentionTrigger, github.ReviewerAppID, client, ciStore)
		}})
	}
	if slack := cfg.Slack; slack != nil {
		routes = append(routes, webhookRoute{"/webhook/slack", func(client *bus.Client, _ *cistore.Store) http.Handler {
			return webhook.SlackHandler(slack.Secret, client)
		}})
	}
	if ghostWispr := cfg.GhostWispr; ghostWispr != nil {
		routes = append(routes, webhookRoute{"/webhook/ghostwispr", func(client *bus.Client, _ *cistore.Store) http.Handler {
			return webhook.GhostWisprHandler(ghostWispr.Secret, client)
		}})
	}
	return routes
}

// newHTTPServer is the listener's HTTP server over handler. Every route reads its header and body
// within ReadTimeout, 10 s from the request's start, and writes its answer within WriteTimeout,
// 10 s from the end of the header. The GitHub webhook route alone moves its own connection's
// deadlines (webhook.GitHubHandler), since a large delivery's body can take longer than that to
// arrive; every other route keeps these.
func newHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
}

// startingGate answers 503 "service starting" until open hands it the mux to serve, and logs each
// request it refuses: that line is the listener's only record of a caller it turned away while it
// started. main builds each gate's mux only once every dependency of its handlers is open, so a
// request never reaches a handler over a dependency that is not there.
type startingGate struct {
	name   string
	logger *logging.Logger
	mux    atomic.Pointer[http.ServeMux]
}

// newStartingGate returns a closed gate whose refusals log under name.
func newStartingGate(name string, logger *logging.Logger) *startingGate {
	return &startingGate{name: name, logger: logger}
}

func (g *startingGate) open(mux *http.ServeMux) { g.mux.Store(mux) }

func (g *startingGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	mux := g.mux.Load()
	if mux == nil {
		g.logger.Warn("request refused while starting",
			slog.String("gate", g.name),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
		)
		writeJSONError(w, http.StatusServiceUnavailable, "service starting")
		return
	}
	mux.ServeHTTP(w, r)
}

// openWebhooks builds the webhook routes over NATS and the CI store (nil without the GitHub route)
// and opens the webhook gate onto them. A webhook waits on nothing else, not even the interest and
// session caches /v1 waits on, and GitHub does not redeliver a delivery refused while they warm.
func openWebhooks(gate *startingGate, hooks []webhookRoute, client *bus.Client, ciStore *cistore.Store) {
	routes := http.NewServeMux()
	for _, hook := range hooks {
		routes.Handle(hook.path, hook.handler(client, ciStore))
	}
	gate.open(routes)
}

// openV1Routes builds the /v1 routes over the stores and opens the /v1 gate onto them. No /v1
// handler reads the durable consumer, so /v1 serves on every pod of a machine, the lease holder
// and each standby alike, and callers that resolve the listener's name reach any of them.
func openV1Routes(gate *startingGate, ready *listenerDeps, machineID string, logger *logging.Logger, listeningAt time.Time) {
	routes := http.NewServeMux()
	registerV1Routes(routes, ready, machineID, logger)
	gate.open(routes)
	logger.Info("envoy-listener /v1 open", slog.Int64("since_listening_ms", time.Since(listeningAt).Milliseconds()))
}

// listenerInstance names this process among the listeners that run for one machine id: the
// ENVOY_INSTANCE_ID environment variable when it is set, else the host name, which in Kubernetes is
// the pod's name.
func listenerInstance() (string, error) {
	if instance := os.Getenv("ENVOY_INSTANCE_ID"); instance != "" {
		return instance, nil
	}
	hostname, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("instance: ENVOY_INSTANCE_ID is unset and the host name is unreadable: %w", err)
	}
	if hostname == "" {
		return "", errors.New("instance: ENVOY_INSTANCE_ID is unset and the host name is empty")
	}
	return hostname, nil
}

// listenerStateRetry is how long a start waits between attempts to open the listener state bucket
// while NATS cannot answer.
const listenerStateRetry = 2 * time.Second

// openDeliveryLease opens the listener state bucket and makes machineID's delivery lease over it
// for this process, instance. The bucket is opened on a NATS connection of the lease's own
// (bus.Dial, as the user and through the proxy the environment names), which nats.go reconnects in
// place for the life of the process: the bus client replaces its connection after a close, and a
// handle on the replaced one would never renew the lease again. An open that fails for want of a
// connection is retried every listenerStateRetry, so a NATS that answers late does not end the
// start. An open the NATS user's grant refuses (errListenerStateRefused) returns nil after one
// ERROR naming the bucket and the missing grant: the listener then delivers without a lease, as
// the machine's one deliverer receiving only while no other subscriber holds the durable, and says
// so in /healthz.
func openDeliveryLease(urls []string, replicas int, machineID, instance string, logger *logging.Logger) *lease {
	var conn *nats.Conn
	for {
		var err error
		if conn == nil {
			conn, err = bus.Dial("envoy-listener-lease "+instance, urls, os.LookupEnv)
		}
		if err == nil {
			var kv bus.KeyValue
			if kv, err = openListenerState(conn, replicas, listenerStateRequestWait); err == nil {
				return newLease(kv, machineID, instance, productionLeaseConfig(), time.Now, logger)
			}
		}
		if errors.Is(err, errListenerStateRefused) {
			logger.Error("listener lease unavailable: the NATS user lacks the grant for KV bucket "+bus.ListenerStateBucket+"; delivering without a lease",
				slog.String("bucket", bus.ListenerStateBucket),
				slog.String("grant", "infra/core ListenerPermissions"),
				slog.String("error", err.Error()),
			)
			conn.Close()
			return nil
		}
		logger.Warn("listener state bucket open failed; retrying",
			slog.String("bucket", bus.ListenerStateBucket),
			slog.String("retry_in", listenerStateRetry.String()),
			slog.String("error", err.Error()),
		)
		time.Sleep(listenerStateRetry)
	}
}

func main() {
	instance, err := listenerInstance()
	if err != nil {
		log.Fatal(err)
	}
	// Go's default logger, which carries internal/bus's lines and the webhooks' plain ones, gets no
	// instance prefix: the deployed CloudWatch text filters (infra/aws/rendering/metricfilters.go)
	// match fields by position after the date and time, and a word there would stop three alarms.
	// The bus's lifecycle lines name the instance in their client name (`envoy-listener <instance>`).

	// Phase 1: Load config (synchronous, fast).
	cfg, err := config.Load(9020)
	if err != nil {
		log.Fatal(err)
	}
	logger := logging.New(cfg.MachineID, logging.WithInstance(instance))
	apiToken := os.Getenv("ENVOY_API_TOKEN")
	oidcIssuer, oidcAudience, err := resolveListenerOIDCConfig(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	apiVerifier, err := oidc.Discover(context.Background(), oidcIssuer, oidcAudience, oidc.DiscoveryTimeout)
	if err != nil {
		log.Fatal(err)
	}
	if err := validateListenerAPIAuth(cfg.ListenHost, apiToken, os.Getenv("ENVOY_API_ALLOW_UNAUTHENTICATED"), apiVerifier); err != nil {
		log.Fatal(err)
	}
	logger.Info("listener API auth: " + describeListenerAPIAuth(apiToken, oidcIssuer))
	logger.Info("listener API hosts: " + describeListenerAPIHosts(apiToken, apiVerifier, cfg.AllowedHosts))

	// Load webhook config (fast — env var reads only).
	webhookCfg, err := webhook.LoadWebhookConfig()
	if err != nil {
		log.Fatal(err)
	}

	// Phase 2: Bind HTTP port deterministically in main goroutine before
	// any NATS work begins. This guarantees /healthz is reachable as soon
	// as Serve starts, regardless of NATS connection latency.
	addr := cfg.ListenAddress()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}

	// Shared state: nil until NATS initialization completes.
	var deps atomic.Pointer[listenerDeps]

	// Metrics registry — /metrics available during startup (like /healthz).
	met := metrics.New()
	messagesReceived := met.NewCounter("envoy_messages_received_total", "Total messages received by the listener")
	messagesDelivered := met.NewCounter("envoy_messages_delivered_total", "Total message delivery attempts")
	messagesNAKed := met.NewCounter("envoy_messages_naked_total", "Total messages NAK'd for retry")
	deliveryDuration := met.NewHistogram("envoy_delivery_duration_seconds", "Duration of message delivery attempts", metrics.DefaultBuckets)
	// The CI summary loop runs only beside the GitHub webhook route (Phase 6b2), so only a listener
	// that mounts it carries the gauge the loop sets.
	var ciLegacyHeld *metrics.Gauge
	if webhookCfg.GitHub != nil {
		ciLegacyHeld = met.NewGauge("envoy_ci_legacy_records_held", "CI records a head-gated listener left unsettled that the last summary tick held back")
	}
	met.NewGaugeFunc("envoy_active_sessions", "Number of active sessions", func() int64 {
		d := deps.Load()
		if d == nil {
			return 0
		}
		entries, err := d.sessions.List()
		if err != nil {
			return 0
		}
		return int64(len(entries))
	})
	met.NewGaugeFunc("envoy_active_interests", "Number of active interest subscriptions", func() int64 {
		d := deps.Load()
		if d == nil {
			return 0
		}
		return int64(len(d.registry.List()))
	})

	// Phase 3: Build HTTP mux.
	mux := http.NewServeMux()

	// /metrics is always reachable — NOT gated by readiness.
	mux.Handle("/metrics", met.Handler())

	// /healthz is always reachable, before NATS setup as well as after it.
	mux.HandleFunc("/healthz", healthzHandler(&deps))

	// GaugeFunc for consumer pending — queries NATS at scrape time

	// The webhook and /v1 routes answer 503 "service starting" until their dependencies are open,
	// each behind its own gate: the webhooks once NATS and, for the GitHub route, the CI store are
	// (Phase 5), /v1 once the interest and session caches are too (Phase 6). Neither waits for the
	// durable consumer, which only /healthz reads. The webhook paths reach their gate bare, /v1
	// through apiAuth.
	webhookGate, v1Gate := newStartingGate("webhook", logger), newStartingGate("v1", logger)
	hooks := webhookRoutes(webhookCfg)
	for _, hook := range hooks {
		mux.Handle(hook.path, webhookGate)
	}
	// Serve /v1/* on the listener port for local plugin registration.
	v1Handler := apiAuth(apiToken, apiVerifier, cfg.AllowedHosts, logger, v1Gate)
	mux.Handle("/v1", v1Handler)
	mux.Handle("/v1/", v1Handler)

	// Phase 4: Start HTTP server (port already bound via net.Listen).
	server := newHTTPServer(mux)
	fatal := make(chan error, 1)
	go func() {
		if err := server.Serve(ln); err != http.ErrServerClosed {
			fatal <- err
		}
	}()
	listeningAt := time.Now()
	logger.Info("envoy-listener listening", slog.String("addr", addr))

	// Phase 5: Connect to NATS (main goroutine — log.Fatal is safe here). The listener owns
	// ENVOY_NOTIFICATIONS: this start reconciles the stream (bus.ConnectOwningStream).
	client, err := bus.ConnectOwningStream(cfg.NATSURLs, bus.WithReplicas(cfg.NATSReplicas), bus.WithName("envoy-listener "+instance))
	if err != nil {
		log.Fatal(err)
	}
	consumer := bus.ListenerDurable(cfg.MachineID)
	// The durable is checked here, before the cache warm-ups below (up to 30 s each), so a refused
	// one ends the start within the NATS connect: no pod could receive from it, and nothing serves
	// yet, so the start ends at once. A lookup that fails is left to the receive loop, which retries
	// it.
	if _, err := listenerDurable(context.Background(), client, consumer); errors.Is(err, errListenerDurableRefused) {
		logger.Error(durableRefusedLine, slog.String("error", err.Error()))
		client.Conn.Close()
		os.Exit(1)
	} else if err != nil {
		logger.Warn("durable consumer check failed; the receive loop retries it", slog.String("consumer", consumer), slog.String("error", err.Error()))
	}
	// The delivery lease makes one pod of this machine id its deliverer and every other a standby
	// (lease.go). It is created here, after the durable check so a refused start opens no bucket,
	// and started once /v1 serves and the shutdown can release it.
	deliveryLease := openDeliveryLease(cfg.NATSURLs, cfg.NATSReplicas, cfg.MachineID, instance, logger)
	// CI-summary aggregation state. Its WatchAll cache warms asynchronously like the registries
	// below; the summary loop tolerates an empty cache until it fills. It opens ahead of them, as the
	// one store the webhooks need, and after the durable check, so a refused start opens no bucket.
	//
	// Only the GitHub webhook route records checks in it, and the listeners that receive GitHub
	// webhooks publish every CI settlement, so a listener without the route opens no CI store and
	// runs no summary loop. It must not scan the bucket either: the watch delivers every record as
	// it starts (62 MB in production), and over a relayed link that burst holds up the replies the
	// stores below wait on past their 10 s deadline.
	var ciStore *cistore.Store
	if webhookCfg.GitHub != nil {
		ciStore, err = cistore.Open(client.Conn, logger, cistore.WithReplicas(cfg.NATSReplicas), cistore.WithTTL(7*24*time.Hour))
		if err != nil {
			log.Fatal(err)
		}
	} else {
		logger.Info("CI store not opened: no GitHub webhook route, so this listener records and settles no checks")
	}
	openWebhooks(webhookGate, hooks, client, ciStore)
	logger.Info("envoy-listener webhooks open (NATS connected)")

	// The registry and its cache watcher log through the listener's own handler, so its role-restore
	// count, its warm-up and its reaper cycles are JSON records with this machine's id. Never
	// slog.SetDefault instead: see store.WithLogger for the CloudWatch filters that breaks.
	registry, err := store.Open(client.Conn, store.WithReplicas(cfg.NATSReplicas), store.WithLogger(logger.Slog()))
	if err != nil {
		log.Fatal(err)
	}

	// Wait for the interest cache to finish its initial scan of existing KV
	// entries before we start accepting NATS messages. Without this gate, real
	// events that arrive in the warm-up window get "no matching interests"
	// even when the durable KV entry has subscribers.
	//
	// Bounded at 30s: a healthy NATS cluster completes the scan in milliseconds.
	// If the watcher fails to start, signalReady() unblocks startup and the
	// health endpoint exposes the unavailable registry while NATS recovers.
	cacheReadyCtx, cacheReadyCancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := registry.WaitForCacheReady(cacheReadyCtx); err != nil {
		logger.Warn("interest cache warm-up timed out; serving with possibly empty cache",
			slog.String("error", err.Error()))
	}
	cacheReadyCancel()

	sessions, err := session.OpenSessionRegistry(
		client.Conn,
		session.WithSessionReplicas(cfg.NATSReplicas),
		session.WithSessionLogger(logger.Slog()),
	)
	if err != nil {
		log.Fatal(err)
	}

	// The margin between a client's registration refresh and this lifetime can only be checked
	// against the defaults in the source, while the live value is whatever MaxAge the existing
	// bucket was created with. Say it at boot so an operator can read the value in force.
	logger.Info("session record lifetime resolved; a session record is dropped this long after its last write",
		slog.String("ttl", sessions.TTL().String()))

	// Wait for the session cache to finish its initial scan before we accept
	// traffic, mirroring the interest-cache gate above. Bounded at 30s: a healthy
	// cluster completes in milliseconds. On timeout we fail open — Put
	// write-through keeps locally-registered sessions visible, while /healthz
	// reports a persistently unavailable registry.
	sessionCacheReadyCtx, sessionCacheReadyCancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := sessions.WaitForCacheReady(sessionCacheReadyCtx); err != nil {
		logger.Warn("session cache warm-up timed out; serving with possibly empty cache",
			slog.String("error", err.Error()))
	}
	sessionCacheReadyCancel()

	// KV watchers are not bus subscriptions, so recreate them after every
	// recovered NATS connection, and end them in the drain before the connection's own.
	caches := listenerCaches(registry, sessions, ciStore)
	client.AddReconnectHook(func(conn *nats.Conn) error {
		return rewatchListenerKVWatchers(conn, caches)
	})
	client.AddDrainHook(func(ctx context.Context) {
		endListenerKVWatchers(ctx, caches)
	})

	deliver := session.Deliverer{
		MachineID:    cfg.MachineID,
		HostBridge:   os.Getenv("ENVOY_HOST_BRIDGE"),
		RequestLimit: 30 * time.Second,
		Sessions:     sessions,
	}

	dedupeCache := dedupe.New(10 * time.Minute)

	// attemptCache tracks (dedupe_key, session_id) pairs BEFORE delivery to
	// prevent phantom duplicates when slow serves accept prompt_async but
	// the HTTP client times out before receiving 204. Keyed by DedupeKey
	// (not EventID) because fan-out normalization can produce multiple
	// envelopes with the same EventID but distinct DedupeKeys. The existing
	// dedupeCache records only successful deliveries (post-delivery) and
	// cannot catch this slow-204 case. See #389.
	attemptCache := dedupe.New(5 * time.Minute)

	roleQueue := bus.ListenerQueue(cfg.MachineID)

	// /healthz handler was registered early (before NATS) — no re-registration needed.

	// Phase 6: Open the role lane and /v1 onto the stores, then start the lease and the durable's
	// receive loop.
	deliveryConfig := listenerDeliveryHandlerConfig{
		client:            client,
		forwardRole:       client.RequestCoreTo,
		registry:          registry,
		sessions:          sessions,
		machineID:         cfg.MachineID,
		deliverer:         &deliver,
		dedupeCache:       dedupeCache,
		attemptCache:      attemptCache,
		logger:            logger,
		messagesReceived:  messagesReceived,
		messagesDelivered: messagesDelivered,
		messagesNAKed:     messagesNAKed,
		deliveryDuration:  deliveryDuration,
	}

	// The role lane is a core-NATS queue subscription in this machine's group. Every pod of this
	// machine id, the lease holder and each standby, is a member, and NATS hands each role message
	// to one member of a group, so no message is forwarded twice whichever pods run.
	roleSub, err := client.SubscribeCore(contracts.RoleTopicPrefix+">", coreNATSDeliveryHandler(deliveryConfig), roleQueue)
	if err != nil {
		logger.Error("core role subscription failed", slog.String("error", err.Error()))
		client.Conn.Close()
		os.Exit(1)
	}
	if err := client.Conn.Flush(); err != nil {
		logger.Error("core role subscription flush failed", slog.String("error", err.Error()))
		client.Conn.Close()
		os.Exit(1)
	}
	_ = roleSub

	// The live topics lane, when ENVOY_SUBSCRIBE_ALL_TOPICS asks for it, opens beside the role lane
	// and before /v1, for the same reason: it is a queue subscription in this machine's group, so
	// NATS hands each message to one of the machine's pods. A grant that refuses it ends the start
	// here, where an operator who opted in learns of it at once, rather than in a listener that
	// delivers none of those topics.
	var live *bus.Client
	if cfg.SubscribeAllTopics {
		live, err = openLiveTopics(cfg.NATSURLs, cfg.MachineID, instance, liveDeliveryHandler(deliveryConfig))
		if err != nil {
			logger.Error("live topics subscription failed: ENVOY_SUBSCRIBE_ALL_TOPICS=1 needs a NATS user that may subscribe to every notification topic",
				slog.String("subject", liveTopicsSubject),
				slog.String("queue", bus.ListenerLiveQueue(cfg.MachineID)),
				slog.String("error", err.Error()))
			client.Conn.Close()
			os.Exit(1)
		}
		logger.Info("live topics subscribed", slog.String("subject", liveTopicsSubject), slog.String("queue", bus.ListenerLiveQueue(cfg.MachineID)))
	}

	// From here a SIGTERM or SIGINT is an ordered shutdown: /v1 serves from the next step, and the
	// lease is released on the way out. Before here, during the NATS connect and the cache
	// warm-ups, a signal ends the process at once, as it always has.
	stopping, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// The loops only the deliverer runs start when this pod starts receiving the durable and stop
	// when it stops: it lost or released the lease. A standby runs none of them. Each loop that
	// lists a bucket does so through watchers whose consumers nats.go creates and deletes, so the
	// end of receiving waits for a pass in flight, and the shutdown ends receiving before it
	// drains NATS.
	ciDebounce := 5 * time.Second
	if v := os.Getenv("ENVOY_CI_DEBOUNCE"); v != "" {
		if d, perr := time.ParseDuration(v); perr == nil {
			ciDebounce = d
		} else {
			logger.Warn("invalid ENVOY_CI_DEBOUNCE; using default", slog.String("value", v), slog.String("error", perr.Error()))
		}
	}
	holderLoops := func(ctx context.Context) []<-chan struct{} {
		// The interest reaper cross-references envoy_sessions (5-min TTL) with envoy_interests
		// (permanent) to prune orphaned interests from dead sessions; the role-claim reaper ends
		// the claims of holders whose sessions are gone.
		done := []<-chan struct{}{
			registry.StartReaper(ctx, func(sessionID string) bool { return isSessionLive(sessions, sessionID) }, 5*time.Minute, 10*time.Minute),
			registry.StartRoleClaimReaper(ctx, func(sessionID string) bool { return roleHolderMayBeLive(logger, sessions, sessionID) }, 5*time.Minute, sessions.TTL()),
			// The collector removes the interest bucket's delete markers, which the reaper leaves
			// one of per dead session on a key that is never reused and which nothing else removes,
			// so every restart's cache warm-up replays them before it serves (LEGION-374). Its first
			// pass runs as receiving starts, off the readiness path; then on the reapers' cadence.
			// The purge is idempotent. The JetStream context comes from the client at each pass,
			// since a replaced connection reassigns it.
			registry.StartInterestMarkerCollector(ctx, client.JS, 5*time.Minute),
		}
		// The CI summary loop, on a listener that opened the CI store, emits one pr.<n>.checks
		// event once a commit's checks settle, for every commit of a pull request, its head or
		// not; new runs re-arm settlement. The debounce window is ENVOY_CI_DEBOUNCE (default 5s).
		if ciStore != nil {
			cistore.StartSummaryLoop(ctx, ciStore, client, ciDebounce, 1*time.Second, ciLegacyHeld, logger)
		}
		return done
	}
	receiver := newDurableReceiver(durableReceiverOptions{
		client:   client,
		consumer: consumer,
		lease:    deliveryLease,
		handle:   jetStreamDeliveryHandler(deliveryConfig),
		loops:    holderLoops,
		// A durable refused after the check above (an operator changed it since) can never be
		// received from, so the listener stops, as that check stops a start.
		refused: func(err error) {
			logger.Error(durableRefusedLine, slog.String("error", err.Error()))
			_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		},
		logger: logger,
		timing: productionReceiveConfig(),
	})
	// A replaced connection invalidates the subscription; the receive loop subscribes again on the
	// connection the client now holds.
	client.AddReconnectHook(receiver.reconnected)

	ready := &listenerDeps{
		client:     client,
		live:       live,
		registry:   registry,
		sessions:   sessions,
		ciStore:    ciStore,
		caches:     caches,
		receiver:   receiver,
		streamName: bus.Stream,
	}
	openV1Routes(v1Gate, ready, cfg.MachineID, logger, listeningAt)

	// The lease runs until the shutdown releases it (durableReceiver.Stop); leaseCtx is cancelled
	// after that, which ends a lease the release could not stop in time.
	leaseCtx, leaseCancel := context.WithCancel(context.Background())
	defer leaseCancel()
	if deliveryLease != nil {
		deliveryLease.Start(leaseCtx)
	}
	receiver.Start()
	deps.Store(ready)
	logger.Info("envoy-listener ready (NATS connected)")

	// Phase 6c: Keep transient JetStream timeouts observable without restarting
	// the listener. Terminal watcher/consumer failures are rebuilt immediately;
	// after three failed recovery intervals, SIGTERM enters the bounded shutdown
	// path and exits non-zero for Docker to replace the listener. Only the pod receiving the
	// durable checks it; a standby's probe checks the caches alone, so a standby runs on for as
	// long as its caches answer. The durable probe and a rebuild's resubscribe run under
	// monitorCtx, each JetStream read included, so the shutdown's monitorCancel cuts a read the
	// server has not answered and the 10 s shutdown budget is held by neither.
	monitorCtx, monitorCancel := context.WithCancel(context.Background())
	probeDurable := func() error { return receiver.probe(monitorCtx) }
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		runSelfHealthMonitor(
			monitorCtx,
			logger,
			func() error {
				return checkSelfHealth(caches, probeDurable)
			},
			func(err error) bool {
				return isUnrecoverableSelfHealthFailure(err, client, caches)
			},
			client.Connected,
			func() error {
				return rebuildListenerDependencies(monitorCtx, client, caches, probeDurable, receiver.resubscribe)
			},
			func() { _ = syscall.Kill(os.Getpid(), syscall.SIGTERM) },
			30*time.Second,
			3,
		)
	}()

	// Phase 7: Block until SIGTERM/SIGINT or fatal error.
	select {
	case err := <-fatal:
		logger.Error("fatal error", slog.String("error", err.Error()))
	case <-stopping.Done():
		logger.Info("received signal, shutting down", slog.String("signal", context.Cause(stopping).Error()))
	}
	shutdownListener(logger, server, client, live, caches, listenerLoops{
		stop:        monitorCancel,
		monitorDone: monitorDone,
		receiver:    receiver,
	})
}

// listenerLoops are what main started that the shutdown ends before it drains NATS: stop ends the
// self-health monitor, monitorDone is closed once it has returned, and receiver is the durable's
// receive loop, which stops the holder-only loops itself.
type listenerLoops struct {
	stop        func()
	monitorDone <-chan struct{}
	receiver    *durableReceiver
}

// shutdownListener is the ordered shutdown every end of a start that serves /v1 comes to, a
// signal or a fatal error, and exits non-zero so the runtime's restart policy restores the
// listener. live is the live topics lane's client, nil when the lane is off.
func shutdownListener(logger *logging.Logger, server *http.Server, client *bus.Client, live *bus.Client, caches []listenerCache, loops listenerLoops) {
	// 1. Stop the self-health monitor, so nothing re-subscribes or re-watches from here.
	if loops.stop != nil {
		loops.stop()
	}

	// 2. Stop receiving the durable, inside the 10 s budget the HTTP stop shares: the holder-only
	// loops stop, the subscription ends, the message in hand gets inflightGrace (3 s) before its
	// prompt is cancelled and it is NAKed, every queued message is NAKed, and the lease is marked
	// released, so a standby takes the lease at its next poll and receives those messages first.
	// HTTP keeps serving meanwhile.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if loops.receiver != nil {
		loops.receiver.Stop(shutdownCtx)
	}

	// 3. HTTP server — stop accepting new requests, drain in-flight, within what is left of the
	// budget.
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warn("http shutdown error", slog.String("error", err.Error()))
	}

	// 4. NATS — wait (within the same deadline) for a self-health rebuild the monitor may still be
	// running, so nothing re-subscribes or re-watches during the drain. Retire every cache's KV
	// watchers, so their end reads as the shutdown it is rather than a watcher failure, and a
	// reconnect hook still running cannot arm a new one; then drain in-flight deliveries without
	// reconnecting, end the retired watchers and delete their consumers (endListenerKVWatchers, the
	// drain hook main adds), and drain the connection, but never let a blocked NATS request pin the
	// process after its HTTP listener is gone.
	if loops.monitorDone != nil {
		select {
		case <-loops.monitorDone:
		case <-shutdownCtx.Done():
			logger.Warn("self-health monitor still running at shutdown")
		}
	}
	// The live lane drains before the main client, within 3 s: a live delivery still in its handler
	// publishes its exception through the main client, and reads the caches stopped below. Core
	// NATS retries nothing, so a delivery still blocked at the deadline is lost, as one sent while
	// no listener runs is.
	if live != nil {
		if err := live.Drain(3 * time.Second); err != nil {
			logger.Warn("live topics drain error", slog.String("error", err.Error()))
		}
	}
	for _, c := range caches {
		c.cache.StopWatch()
	}
	// 10 s for the role-lane deliveries in their handlers, as before the drain hooks, and the
	// hooks' 3 s on top (bus's drainHookReserve): with the 10 s the receive stop and the HTTP stop
	// share and the live lane's 3 s, 26 s, inside every runtime's 30 s.
	if err := client.Drain(13 * time.Second); err != nil {
		logger.Warn("nats drain error", slog.String("error", err.Error()))
	}

	logger.Info("envoy-listener shutdown complete")
	os.Exit(1)
}
