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
	"reflect"
	"strings"
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
// it through an atomic.Pointer that stays nil until the durable consumer binds.
type listenerDeps struct {
	client   *bus.Client
	registry *store.Registry
	sessions *session.SessionRegistry
	// ciStore is nil on a listener that mounts no GitHub webhook route, the one route that uses it.
	ciStore *cistore.Store
	// caches lists the stores above that are open, for the sites that treat every cache alike.
	caches []listenerCache
	// consumer names the durable whose lag /healthz reports.
	consumer   string
	streamName string
	streamInfo streamInfoLookup
}

// kvCache is what the listener asks of each KV-backed cache it keeps.
type kvCache interface {
	Rewatch(*nats.Conn) error
	WatchErr() error
	Ping() error
	StopWatch()
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

// Canonical policy for the listener's durable consumer. DeliverSubject is
// deliberately not part of the policy: it is fixed at creation and preserved
// for the consumer's lifetime (nats.Bind attaches to whatever subject the
// server has persisted).
//
// consumerInactiveThreshold lets the server delete a machine's durable
// consumer once its listener has been gone this long. The notification
// stream only retains 72h of messages, so a consumer inactive longer than
// that has already lost data and its durability protects nothing; without
// a threshold, every decommissioned machine leaves a consumer behind
// forever, silently accumulating pending state.
const (
	consumerAckWait           = 60 * time.Second
	consumerMaxAckPending     = 256
	consumerMaxDeliver        = 20
	consumerInactiveThreshold = 7 * 24 * time.Hour
)

// applyListenerConsumerPolicy stamps the canonical consumer policy onto
// config. Shared by the create and drift-correction paths so the policy has
// exactly one definition: the drift correction applies it to a copy of the
// durable's config and updates the durable when the copy differs. The policy
// has no idle heartbeat: the create path starts from a zero config, so a
// durable the listener creates has none. listenerDurableRefusal refuses an
// existing durable whose heartbeat or ack policy differs, since NATS cannot
// change either in place.
func applyListenerConsumerPolicy(config *nats.ConsumerConfig, subjects []string) {
	config.FilterSubject = ""
	config.FilterSubjects = subjects
	config.AckPolicy = nats.AckExplicitPolicy
	config.AckWait = consumerAckWait
	config.MaxAckPending = consumerMaxAckPending
	config.MaxDeliver = consumerMaxDeliver
	config.InactiveThreshold = consumerInactiveThreshold
}

// errListenerDurableRefused marks a durable startListenerSubscription will not bind however often
// it is asked: no retry can succeed, so the listener's startup exits at once.
var errListenerDurableRefused = errors.New("listener durable refused")

// listenerDurableRefusal refuses an existing durable carrying a setting the listener's consumer
// policy fixes and NATS cannot change in place, so the drift correction could never apply it:
//   - An idle heartbeat. The bus logs nats.ErrConsumerNotActive at WARN because only KV watchers'
//     ordered consumers report it, and only while disconnected; a heartbeat here would make a
//     stalled durable report that same WARN.
//   - An ack policy other than explicit. The delivery handler acks each message or NAKs it for a
//     delayed retry, one at a time; under ack all, a later message's ack would also ack an earlier
//     one still waiting for its retry.
//
// Recreating the durable would drop its cursor, so the listener leaves it to an operator, and the
// refusal names every such setting the durable carries and says how to recreate it without
// replaying the stream.
func listenerDurableRefusal(consumer string, config nats.ConsumerConfig) error {
	var settings []string
	if config.Heartbeat != 0 {
		settings = append(settings, fmt.Sprintf("an idle heartbeat of %s", config.Heartbeat))
	}
	if config.AckPolicy != nats.AckExplicitPolicy {
		settings = append(settings, fmt.Sprintf("ack policy %s", config.AckPolicy))
	}
	if len(settings) == 0 {
		return nil
	}
	without := "that setting"
	if len(settings) > 1 {
		without = "those settings"
	}
	return fmt.Errorf("%w: durable consumer %s has %s, which the listener's consumer policy forbids and NATS cannot change in place; "+
		"deleting it lets the listener recreate it at deliver policy all, which replays every retained message, "+
		"so to keep its cursor recreate it from its own config without %s, at deliver policy by_start_sequence "+
		"with opt_start_seq one past its ack_floor.stream_seq (packages/envoy/AGENTS.md, Operational notes)",
		errListenerDurableRefused, consumer, strings.Join(settings, " and "), without)
}

// listenerDurable reads the machine's durable. It returns a nil info when there is none,
// listenerDurableRefusal's error when the listener's policy cannot use the one there is, and the
// lookup's error when NATS cannot say.
func listenerDurable(client *bus.Client, consumer string) (*nats.ConsumerInfo, error) {
	info, err := client.JS().ConsumerInfo(bus.Stream, consumer)
	if errors.Is(err, nats.ErrConsumerNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := listenerDurableRefusal(consumer, info.Config); err != nil {
		return nil, err
	}
	return info, nil
}

// startListenerSubscription preserves the durable consumer so restarts resume
// from the last ACKed non-role message instead of skipping pending work.
//
// The consumer is always created server-side and then bound, never created by
// js.Subscribe: the client library deletes consumers it created itself on
// Unsubscribe()/Drain(), and both the bus reconnect recovery and the SIGTERM
// drain paths trigger exactly that — silently resetting the durable cursor.
// The config drift-correction also stamps consumerInactiveThreshold onto
// consumers created before the threshold existed.
func startListenerSubscription(client *bus.Client, consumer string, handler nats.MsgHandler) (*nats.Subscription, error) {
	subjects := bus.StreamSubjects()
	info, err := listenerDurable(client, consumer)
	switch {
	case err != nil:
		return nil, err
	case info == nil:
		// The deliver subject is a random inbox, not a derivable name: on a
		// shared NATS account a predictable subject would let any client
		// hold interest on it — shadow-reading deliveries and making the
		// consumer look push-bound so the listener could never attach.
		config := nats.ConsumerConfig{
			Durable:        consumer,
			DeliverSubject: nats.NewInbox(),
		}
		applyListenerConsumerPolicy(&config, subjects)
		if _, err := client.JS().AddConsumer(bus.Stream, &config); err != nil {
			return nil, err
		}
	default:
		// Only a field the policy writes can differ: the copy shares every other one, the
		// server-set metadata included, and listenerDurable has already refused any ack policy
		// but explicit.
		corrected := info.Config
		applyListenerConsumerPolicy(&corrected, subjects)
		if !reflect.DeepEqual(corrected, info.Config) {
			if _, err := client.JS().UpdateConsumer(bus.Stream, &corrected); err != nil {
				return nil, err
			}
		}
	}
	return client.Subscribe(
		"",
		handler,
		nats.Bind(bus.Stream, consumer),
		nats.ManualAck(),
	)
}

// errListenerDurableBindExhausted marks a bind still refused at its deadline: something other than
// a rolling deploy's old task holds the durable, or JetStream keeps failing the lookup, so the start
// ends and the runtime starts the listener again on a fresh connection.
var errListenerDurableBindExhausted = errors.New("listener durable bind exhausted")

// During a rolling deploy the durable's push binding is the old task's until that task stops, which
// takes its deregistration from the load balancer and its own shutdown: 35 to 71 s in production.
// The bind is polled every durableBindInterval, so it lands within one interval of the old task's
// exit, and given up at durableBindDeadline, the 135 s the backoff it replaced took to exhaust, so
// the subscribe-exhausted page and the runtime's relaunch come no later than they did.
const (
	durableBindInterval = 2 * time.Second
	durableBindDeadline = 135 * time.Second
	durableBindLogEvery = 30 * time.Second
)

// durableBind is how many attempts a bind took and how long it waited.
type durableBind struct {
	attempts int
	waited   time.Duration
}

// bindListenerDurable binds consumer through startListenerSubscription every interval until it
// binds, the bus's auto-resubscribe after a reconnect binds it, the durable is refused
// (errListenerDurableRefused, returned as is), deadline passes (errListenerDurableBindExhausted,
// wrapping the last refusal), or ctx ends (ctx.Err()). It logs the first refusal and then one every
// durableBindLogEvery, and the bind itself.
func bindListenerDurable(ctx context.Context, client *bus.Client, consumer string, handler nats.MsgHandler, logger *logging.Logger, interval, deadline time.Duration) (durableBind, error) {
	start := time.Now()
	var bind durableBind
	bound := func(autoResubscribe bool) (durableBind, error) {
		logger.Info("durable bound",
			slog.Int("attempts", bind.attempts),
			slog.Int64("waited_ms", bind.waited.Milliseconds()),
			slog.Bool("auto_resubscribe", autoResubscribe),
		)
		return bind, nil
	}
	var logged time.Time
	for {
		if err := ctx.Err(); err != nil {
			bind.waited = time.Since(start)
			return bind, err
		}
		bind.attempts++
		_, err := startListenerSubscription(client, consumer, handler)
		bind.waited = time.Since(start)
		if err == nil {
			return bound(false)
		}
		if errors.Is(err, errListenerDurableRefused) {
			return bind, err
		}
		if bind.waited >= deadline {
			return bind, fmt.Errorf("%w after %d attempts in %s: %w", errListenerDurableBindExhausted, bind.attempts, bind.waited.Round(time.Millisecond), err)
		}
		wait := min(interval, deadline-bind.waited)
		if logged.IsZero() || time.Since(logged) >= durableBindLogEvery {
			logged = time.Now()
			logger.Warn("subscribe failed, retrying",
				slog.String("error", err.Error()),
				slog.Int("attempt", bind.attempts),
				slog.String("retry_in", wait.String()),
				slog.String("deadline", deadline.String()),
			)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			bind.waited = time.Since(start)
			return bind, ctx.Err()
		case <-timer.C:
		}
		// A reconnect during the wait re-subscribes the subscription the failed attempt registered
		// (bus.Client.onReconnect), which binds it once the durable is free. Another attempt would
		// replace that handle with a bind racing the server's release of it, which can be refused
		// with "consumer is already bound to a subscription".
		if client.SubOK() {
			bind.waited = time.Since(start)
			return bound(true)
		}
	}
}

func isSessionLive(sessions *session.SessionRegistry, sessionID string) bool {
	_, err := sessions.Get(sessionID)
	return err == nil
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
	if errors.Is(err, nats.ErrConsumerNotFound) || errors.Is(err, nats.ErrConnectionClosed) ||
		errors.Is(err, nats.ErrStreamNotFound) {
		return true
	}
	for _, c := range caches {
		if c.cache.WatchErr() != nil {
			return true
		}
	}
	return false
}

func rebuildListenerDependencies(
	client *bus.Client,
	caches []listenerCache,
	durableProbe func() error,
	consumer string,
	handler nats.MsgHandler,
) error {
	if !client.Connected() {
		return nats.ErrConnectionClosed
	}
	err := rewatchListenerKVWatchers(client.Conn, caches)
	if durableProbe == nil || !errors.Is(durableProbe(), nats.ErrConsumerNotFound) {
		return err
	}
	if _, durableErr := startListenerSubscription(client, consumer, handler); durableErr != nil {
		return errors.Join(err, fmt.Errorf("recreate durable consumer: %w", durableErr))
	}
	return err
}

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
// the probe. Either is one WARN, then "self-health recovered" from the next.
func runSelfHealthMonitor(
	ctx context.Context,
	logger *logging.Logger,
	probe func() error,
	isUnrecoverable func(error) bool,
	rebuild func() error,
	terminate func(),
	interval time.Duration,
	threshold int,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	failures := 0
	terminalFailures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		err := probe()
		if err != nil {
			failures++
			logger.Warn("self-health probe failed", slog.Int("consecutive", failures), slog.Int("threshold", threshold), slog.String("error", err.Error()))
			if isUnrecoverable == nil || !isUnrecoverable(err) {
				terminalFailures = 0
				continue
			}
			terminalFailures++
			if rebuild != nil {
				if rebuildErr := rebuild(); rebuildErr != nil {
					logger.Error("self-health rebuild failed", slog.Int("consecutive", terminalFailures), slog.String("error", rebuildErr.Error()))
				} else {
					logger.Info("self-health rebuild started", slog.Int("consecutive", terminalFailures))
					// Whatever this probe reads, transient or terminal, the count keeps its
					// increment: only a healthy read is a recovery.
					if err = probe(); err != nil {
						logger.Warn("self-health probe after rebuild failed", slog.Int("consecutive", terminalFailures), slog.String("error", err.Error()))
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
// once the durable consumer binds, which during a rolling deploy waits out the task being replaced
// while /v1 already serves; 200 "healthy" with the durable consumer's lag once every dependency
// answers; 503 "unhealthy" when NATS is unavailable, the durable subscription is inactive, a cache's
// watcher has stopped or the durable consumer is gone; and 200 "degraded" for a transient KV failure
// the self-health monitor and the NATS reconnect retry. A listener that mounts no GitHub webhook
// route keeps no CI cache, and its answer carries "ci_cache": "not_applicable".
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
		if !d.client.SubOK() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "unhealthy", "error": "subscription inactive"})
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
		consumerInfo, err := d.client.JS().ConsumerInfo(bus.Stream, d.consumer)
		if err != nil {
			writeDependencyHealth(w, "durable consumer", err, errors.Is(err, nats.ErrConsumerNotFound))
			return
		}
		response["num_pending"] = consumerInfo.NumPending
		response["num_ack_pending"] = consumerInfo.NumAckPending
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
// handler reads the durable consumer, so /v1 opens before the durable binds: during a rolling deploy
// the bind waits out the task being replaced, and callers that resolve the listener's name reach
// both tasks meanwhile. Publishing the dependencies, which turns /healthz healthy, waits for the
// bind.
func openV1Routes(gate *startingGate, ready *listenerDeps, machineID string, logger *logging.Logger, listeningAt time.Time) {
	routes := http.NewServeMux()
	registerV1Routes(routes, ready, machineID, logger)
	gate.open(routes)
	logger.Info("envoy-listener /v1 open", slog.Int64("since_listening_ms", time.Since(listeningAt).Milliseconds()))
}

func main() {
	// Phase 1: Load config (synchronous, fast).
	cfg, err := config.Load(9020)
	if err != nil {
		log.Fatal(err)
	}
	logger := logging.New(cfg.MachineID)
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
	v1Handler := apiAuth(apiToken, apiVerifier, logger, v1Gate)
	mux.Handle("/v1", v1Handler)
	mux.Handle("/v1/", v1Handler)

	// Phase 4: Start HTTP server (port already bound via net.Listen).
	server := &http.Server{
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
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
	client, err := bus.ConnectOwningStream(cfg.NATSURLs, bus.WithReplicas(cfg.NATSReplicas))
	if err != nil {
		log.Fatal(err)
	}
	consumer := "listener-" + strings.ReplaceAll(cfg.MachineID, " ", "-")
	// exitRefused ends a start that met a refused durable. No retry can bind it, and while this
	// listener starts, /healthz answers 200 "starting", so a rolling deploy waiting on it would take
	// the task for healthy and stop the one it replaces.
	exitRefused := func(err error) {
		logger.Error("subscribe refused, shutting down", slog.String("error", err.Error()))
		client.Conn.Close()
		os.Exit(1)
	}
	// The durable is checked here, before the cache warm-ups below (up to 30 s each), so a refused
	// one ends the start within the NATS connect. A lookup that fails is left to the subscribe
	// loop, which retries it.
	if _, err := listenerDurable(client, consumer); errors.Is(err, errListenerDurableRefused) {
		exitRefused(err)
	} else if err != nil {
		logger.Warn("durable consumer check failed; the subscribe retries it", slog.String("consumer", consumer), slog.String("error", err.Error()))
	}
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
	// recovered NATS connection.
	caches := listenerCaches(registry, sessions, ciStore)
	client.AddReconnectHook(func(conn *nats.Conn) error {
		return rewatchListenerKVWatchers(conn, caches)
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

	roleQueue := "envoy-listener-" + strings.ReplaceAll(cfg.MachineID, " ", "-")

	// /healthz handler was registered early (before NATS) — no re-registration needed.

	// Phase 6: Open the role lane and /v1 onto the stores, then bind the durable consumer.
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

	// The role lane is a core-NATS queue subscription in this machine's group. During a rolling
	// deploy the task being replaced is a member too, and NATS hands each role message to one member
	// of a group, so opening the lane before the durable binds forwards no message twice.
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

	// From here a SIGTERM or SIGINT is an ordered shutdown: /v1 serves from the next step, and the
	// bind can wait out a rolling deploy's old task for most of a minute. Before here, during the
	// NATS connect and the cache warm-ups, a signal ends the process at once, as it always has.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	stopping, stop := context.WithCancel(context.Background())
	var received os.Signal
	go func() {
		received = <-sig
		stop()
	}()

	ready := &listenerDeps{
		client:     client,
		registry:   registry,
		sessions:   sessions,
		ciStore:    ciStore,
		caches:     caches,
		consumer:   consumer,
		streamName: bus.Stream,
	}
	openV1Routes(v1Gate, ready, cfg.MachineID, logger, listeningAt)

	// During a rolling deploy the old task still holds the durable's push binding, and nats.Bind
	// rejects a second one ("consumer is already bound"), so the bind is polled until the old task's
	// delivery interest clears. It never deletes the consumer to take the binding: that resets the
	// durable cursor and replays the full retention window to every subscriber. A durable refused
	// here is not retried either. The bind is the only refusal when the check after the connect
	// could not read the durable (logged at WARN above), and it also catches a durable made
	// refusable since that check.
	bind, err := bindListenerDurable(stopping, client, consumer, jetStreamDeliveryHandler(deliveryConfig), logger, durableBindInterval, durableBindDeadline)
	switch {
	case err == nil:
	case errors.Is(err, errListenerDurableRefused):
		exitRefused(err)
	case errors.Is(err, errListenerDurableBindExhausted):
		logger.Error("subscribe failed after max attempts, shutting down",
			slog.String("error", err.Error()),
			slog.Int("attempts", bind.attempts),
			slog.Int64("waited_ms", bind.waited.Milliseconds()),
		)
		// Close NATS before exiting — releases the consumer binding so the
		// next container can claim it. log.Fatal/os.Exit skips cleanup.
		client.Conn.Close()
		os.Exit(1)
	default:
		// The bind's only other error is its context's, which ends at a signal. No background
		// loop has started yet, so the shutdown has none to stop.
		<-stopping.Done()
		logger.Info("received signal, shutting down", slog.String("signal", received.String()), slog.String("phase", "durable bind"))
		noMonitor := make(chan struct{})
		close(noMonitor)
		shutdownListener(logger, server, client, caches, func() {}, noMonitor)
	}
	deps.Store(ready)
	logger.Info("envoy-listener ready (NATS connected)",
		slog.Int("attempts", bind.attempts),
		slog.Int64("waited_ms", bind.waited.Milliseconds()),
	)

	// Phase 6b: Start interest reaper for stale KV cleanup.
	// Cross-references envoy_sessions (5-min TTL) with envoy_interests (permanent)
	// to prune orphaned interests from dead sessions.
	registry.StartReaper(func(sessionID string) bool { return isSessionLive(sessions, sessionID) }, 5*time.Minute, 10*time.Minute)
	registry.StartRoleClaimReaper(func(sessionID string) bool { return isSessionLive(sessions, sessionID) }, 5*time.Minute, sessions.TTL())

	// Phase 6b1: Collect the interest bucket's delete markers, which the reaper above leaves one of
	// per dead session on a key that is never reused and which nothing else removes, so every
	// restart's cache warm-up replays them before it serves (LEGION-374). The first pass runs now,
	// after the warm-up gate above, so it is off the readiness path; then on the reapers' cadence.
	// Every listener runs it and the purge is idempotent, so it needs no leader. The JetStream
	// context comes from the client at each pass, since a replaced connection reassigns it.
	registry.StartInterestMarkerCollector(client.JS, 5*time.Minute)

	// Phase 6b2: Start the CI summary loop, on a listener that opened the CI store. It emits one
	// pr.<n>.checks event once a commit's checks settle, for every commit of a pull request, its
	// head or not; new runs re-arm settlement. The debounce window is ENVOY_CI_DEBOUNCE (default
	// 5s).
	summaryCtx, summaryCancel := context.WithCancel(context.Background())
	if ciStore != nil {
		ciDebounce := 5 * time.Second
		if v := os.Getenv("ENVOY_CI_DEBOUNCE"); v != "" {
			if d, perr := time.ParseDuration(v); perr == nil {
				ciDebounce = d
			} else {
				logger.Warn("invalid ENVOY_CI_DEBOUNCE; using default", slog.String("value", v), slog.String("error", perr.Error()))
			}
		}
		cistore.StartSummaryLoop(summaryCtx, ciStore, client, ciDebounce, 1*time.Second, ciLegacyHeld, logger)
	}

	// Phase 6c: Keep transient JetStream timeouts observable without restarting
	// the listener. Terminal watcher/consumer failures are rebuilt immediately;
	// after three failed recovery intervals, SIGTERM enters the bounded shutdown
	// path and exits non-zero for Docker to replace the listener.
	durableProbe := func() error {
		if _, err := client.JS().ConsumerInfo(bus.Stream, consumer); errors.Is(err, nats.ErrConsumerNotFound) {
			return err
		}
		return nil
	}
	monitorCtx, monitorCancel := context.WithCancel(context.Background())
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		runSelfHealthMonitor(
			monitorCtx,
			logger,
			func() error {
				return checkSelfHealth(caches, durableProbe)
			},
			func(err error) bool {
				return isUnrecoverableSelfHealthFailure(err, client, caches)
			},
			func() error {
				return rebuildListenerDependencies(
					client,
					caches,
					durableProbe,
					consumer,
					jetStreamDeliveryHandler(deliveryConfig),
				)
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
		logger.Info("received signal, shutting down", slog.String("signal", received.String()))
	}
	shutdownListener(logger, server, client, caches, func() {
		monitorCancel()
		summaryCancel()
	}, monitorDone)
}

// shutdownListener is the ordered shutdown a signal and a fatal error both end in, and exits
// non-zero so the runtime's restart policy restores the listener. stopLoops stops the background
// loops main started, and monitorDone is closed once the self-health monitor has returned.
func shutdownListener(logger *logging.Logger, server *http.Server, client *bus.Client, caches []listenerCache, stopLoops func(), monitorDone <-chan struct{}) {
	// Stop background loops before draining NATS.
	stopLoops()

	// Ordered shutdown:
	// 2. HTTP server — stop accepting new requests, drain in-flight.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warn("http shutdown error", slog.String("error", err.Error()))
	}

	// 3. NATS — wait (within the HTTP deadline) for a self-health rebuild the monitor may still be
	// running, so nothing re-subscribes or re-watches during the drain. Retire every cache's KV
	// watchers, so the drain ending their subscriptions reads as the shutdown it is rather than a
	// watcher failure, and a reconnect hook still running cannot arm a new one; then drain in-flight
	// deliveries without reconnecting, but never let a blocked NATS request pin the process after
	// its HTTP listener is gone.
	select {
	case <-monitorDone:
	case <-shutdownCtx.Done():
		logger.Warn("self-health monitor still running at shutdown")
	}
	for _, c := range caches {
		c.cache.StopWatch()
	}
	if err := client.Drain(10 * time.Second); err != nil {
		logger.Warn("nats drain error", slog.String("error", err.Error()))
	}

	logger.Info("envoy-listener shutdown complete")
	os.Exit(1)
}
