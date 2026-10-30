package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
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
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/webhook"
	"github.com/sjawhar/envoy/listenfd"
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
	// podHostname is this pod's host name (listenerPodHostname), which the subscribe handler stamps
	// on a port registration as session.SessionEntry.AcceptedHost; empty when it is unknown.
	podHostname string
	// delivery is the config main hands the delivery handlers (the role lane, the live lane and the
	// durable), wired once, so the publish route forwards through its forwardRole, records each
	// session it confirms in its delivery ledger (so no lane prompts or forwards that key to the
	// session again, on this pod or any other of the machine), and writes each send's delivery line
	// and delivery_status count through its logger and messagesDelivered, reading as the role lane's
	// do, beside the route's own request record (request_record.go). The route never reads that
	// ledger for its own dedupe answer: the ledger records what this machine delivered, a prompt
	// or a forward, and not the recipient's word that it has the message.
	delivery *listenerDeliveryHandlerConfig
	// confirmed is the publish route's own record of each (dedupe key, session) whose receipt arrived
	// through the route, which only the route writes, and which this pod alone holds (README,
	// "State the pods share"). A send skips a holder or recipient recorded
	// there, so envoy-client's retry of a send does not forward again to one the first attempt
	// confirmed, and no one is counted as confirming a message without having sent its receipt.
	confirmed *dedupe.Cache
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

// envDuration reads a duration an operator may override through the environment: an unset or
// empty value leaves fallback standing, and so does one time.ParseDuration refuses, after a
// single WARN naming the variable - a listener never refuses to start over a malformed duration.
// The WARN's message and fields are what an operator's log filter matches, so they name the
// variable exactly as it is written in the environment.
func envDuration(logger *logging.Logger, name string, fallback time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		logger.Warn("invalid "+name+"; using default", slog.String("value", v), slog.String("error", err.Error()))
		return fallback
	}
	return d
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
// missing bucket is terminal too: a rebuild cannot create a bucket, so its
// failed rebuilds end in a restart, whose store Open creates it again.
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

// healthzDeadline bounds one /healthz answer. A dependency that has not answered when it passes
// makes the answer 503, so a readiness probe whose own timeout is longer always gets an answer.
const healthzDeadline = 5 * time.Second

// healthAnswer is one /healthz answer: its HTTP status and its JSON body.
type healthAnswer struct {
	code int
	body map[string]interface{}
}

// healthzHandler is the listener's /healthz, the readiness answer of every pod of a machine id. It
// answers 503 "starting" until main publishes deps, which it does once NATS is connected, the
// webhook and /v1 gates are open over warm caches and the receive loop has started, and, on a
// listener configured with an OIDC issuer, until the issuer's discovery has succeeded (verifier).
// Then it answers 200 "healthy" once every dependency answers, naming this pod's "role": "active"
// while it receives the durable (it holds the lease and is subscribed, or it runs without a lease
// and receives), with the durable's lag (num_pending, num_ack_pending), or "standby" otherwise,
// naming the holder in "active_instance" when the lease record names one and, while it holds the
// lease but does not receive yet, what it waits for in "receive". A pod that is the machine's
// deliverer and does not receive yet - it holds the lease, or runs without one, or has read no
// holder at all - is 200 "degraded" rather than healthy, however well its dependencies answer:
// nothing of the durable reaches a session until it receives. It stays ready, since a readiness
// probe that takes any 200 keeps routing /v1 to it, as it does to a standby. A standby of another
// holder is ready and healthy: it serves /v1, the webhooks and the role lane, and takes over
// delivery when the lease comes free. A listener without the lease's grant says so in "lease"
// (listenerLeaseUnavailable). It answers 503 "unhealthy" when NATS is unavailable, the role
// lane's core subscription is not valid, a cache's watcher has stopped, a bus check did not
// answer within healthzBudget, or on the active pod when its subscription is inactive, the
// durable consumer is gone, or the durable consumer is "not bound to this listener's
// subscription" because it was recreated server-side and now delivers elsewhere; and 200
// "degraded" for a transient KV failure the self-health monitor and the NATS reconnect retry, or
// an active pod's durable lag that could not be read. A pod that holds the lease, or runs
// without one, and fails to read, create, correct or subscribe to the durable answers 200
// "degraded" with the failure in "error" and how long it has failed in
// "receive_failing_ms", and 503 "unhealthy" once that has lasted the alarm bound
// (durableTakeoverAlarmAfter): a holder gives the lease up then and is a standby again, so only a
// listener without a lease, which keeps retrying, stays there. A listener that mounts no GitHub
// webhook route keeps no CI cache, and its answer carries "ci_cache": "not_applicable".
// "live_topics" says whether the live topics lane is "subscribed" or, with
// ENVOY_SUBSCRIBE_ALL_TOPICS off, "not_applicable"; with it on, a live client that is not
// connected or holds no valid subscription is 503 "live subscription inactive", since every topic
// the stream does not store then reaches no session. The checks run under one healthzDeadline, and
// an answer they have not reached by then is 503 "unhealthy".
//
// The checks that go to the bus share one healthzBudget taken from that deadline: the flush, each
// cache's KV ping and the active pod's durable consumer info read. A ping or that read that has not
// answered when it ends (healthzPing) is 503 unhealthy naming the dependency and the context's
// deadline, so a stall in JetStream alone, which the flush's core PING does not see, fails the
// container health check inside the budget, as the 10 s JetStream wait it replaces failed it by
// running past the check's timeout. A check that answered with a failure keeps its answer.
func healthzHandler(deps *atomic.Pointer[listenerDeps], verifier *apiVerifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), healthzDeadline)
		defer cancel()
		answered := make(chan healthAnswer, 1)
		go func() { answered <- listenerHealth(ctx, deps.Load(), verifier) }()
		var answer healthAnswer
		select {
		case answer = <-answered:
		case <-ctx.Done():
			answer = healthAnswer{http.StatusServiceUnavailable, map[string]interface{}{
				"status": "unhealthy",
				"error":  "health checks did not finish within " + healthzDeadline.String(),
			}}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(answer.code)
		_ = json.NewEncoder(w).Encode(answer.body)
	}
}

// listenerHealth runs /healthz's checks over d, nil until main publishes it, within ctx, its bus
// checks within healthzBudget of it.
func listenerHealth(ctx context.Context, d *listenerDeps, verifier *apiVerifier) healthAnswer {
	if d == nil || !verifier.ready() {
		return healthAnswer{http.StatusServiceUnavailable, map[string]interface{}{"status": "starting"}}
	}
	unhealthy := func(body map[string]interface{}) healthAnswer {
		body["status"] = "unhealthy"
		return healthAnswer{http.StatusServiceUnavailable, body}
	}
	ctx, cancel := context.WithTimeout(ctx, healthzBudget)
	defer cancel()
	if err := d.client.Conn.FlushWithContext(ctx); err != nil {
		return unhealthy(map[string]interface{}{"error": "nats unavailable"})
	}
	if !d.client.CoreSubOK() {
		return unhealthy(map[string]interface{}{"error": "role subscription inactive"})
	}
	if d.live != nil && (!d.live.Connected() || !d.live.CoreSubOK()) {
		return unhealthy(map[string]interface{}{"error": "live subscription inactive"})
	}
	for _, c := range d.caches {
		if err := c.cache.WatchErr(); err != nil {
			return dependencyHealth(c.name+" KV watcher", err, true)
		}
	}
	for _, c := range d.caches {
		if answered, err := healthzPing(ctx, c.cache); err != nil {
			return dependencyHealth(c.name+" KV", err, !answered || c.cache.WatchErr() != nil || errors.Is(err, nats.ErrConnectionClosed))
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
		if report.Deliverer {
			// This pod is the machine's deliverer and does not receive yet (receiveReport.Deliverer):
			// nothing of the durable reaches a session until that changes, which no answer of this
			// pod may call healthy. It stays 200, as a standby does, so a readiness probe keeps
			// sending it /v1.
			response["status"] = "degraded"
		}
		if report.Failing > 0 {
			response["error"] = "durable consumer unavailable: " + report.FailingReason
			response["receive_failing_ms"] = report.Failing.Milliseconds()
			if report.Stuck {
				return unhealthy(response)
			}
			response["status"] = "degraded"
		}
		return healthAnswer{http.StatusOK, response}
	}
	response["role"] = "active"
	if !report.SubscriptionValid {
		return unhealthy(map[string]interface{}{"role": "active", "error": "subscription inactive"})
	}
	consumerInfo, err := d.client.JS().ConsumerInfo(bus.Stream, d.receiver.consumer, nats.Context(ctx))
	switch {
	case errors.Is(err, nats.ErrConsumerNotFound):
		return dependencyHealth("durable consumer", err, true)
	case err != nil && ctx.Err() != nil:
		// A read ctx ended did not answer, the info read's own form of healthzPing's unanswered.
		return dependencyHealth("durable consumer", err, true)
	case err != nil:
		response["status"] = "degraded"
		response["error"] = "durable consumer unavailable: " + err.Error()
	case consumerInfo.Config.DeliverSubject != report.Subject:
		// One read and no confirming second one, unlike the self-health probe's: the endpoint
		// reports the fault the moment a request finds it, and a request landing in a
		// resubscribe's own gap simply answers 503 until the next one.
		return dependencyHealth("durable consumer", errDurableUnbound, true)
	default:
		response["num_pending"] = consumerInfo.NumPending
		response["num_ack_pending"] = consumerInfo.NumAckPending
	}
	return healthAnswer{http.StatusOK, response}
}

// healthzBudget bounds the bus checks of one /healthz answer.
const healthzBudget = 3 * time.Second

// healthzPing is cache's Ping as a /healthz request holding ctx waits for it (bus.Bounded): nats.go's
// KV Status takes no context, so a ping that has not answered when ctx ends is answered false, with
// ctx's error, the "context deadline exceeded" a ping that waited out the JetStream MaxWait failed
// with. The abandoned ping runs on to its answer or that wait; what it can still do then - record a
// recreated bucket's error on its watcher, or prune the session cache's last-seen times - is what
// any later ping would do.
func healthzPing(ctx context.Context, cache kvCache) (answered bool, err error) {
	_, answered, err = bus.Bounded(ctx, func() (struct{}, error) { return struct{}{}, cache.Ping() })
	return answered, err
}

// dependencyHealth is /healthz's answer for a dependency that failed with err: 503 "unhealthy"
// when the failure is terminal, else 200 "degraded".
func dependencyHealth(dependency string, err error, terminal bool) healthAnswer {
	statusCode := http.StatusOK
	status := "degraded"
	if terminal {
		statusCode = http.StatusServiceUnavailable
		status = "unhealthy"
	}
	return healthAnswer{statusCode, map[string]interface{}{
		"status": status,
		"error":  dependency + " unavailable: " + err.Error(),
	}}
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

// listenerPodHostname is the host name of the pod this listener runs in, which the subscribe handler
// stamps on a port registration (session.SessionEntry.AcceptedHost) and the deliverer compares
// before it dials a pod-local address (session.Deliverer.PodHost). It is os.Hostname, the pod's name
// in Kubernetes and the shared host's name under host networking, where pods on one host reach one
// loopback; ENVOY_POD_HOSTNAME overrides it, so two listener processes on one host can stand for two
// pods (the tests). ENVOY_INSTANCE_ID does not: it names the process, not where its loopback is.
// An unreadable host name returns "" and an error, and the listener then neither stamps nor refuses.
func listenerPodHostname() (string, error) {
	if hostname := os.Getenv("ENVOY_POD_HOSTNAME"); hostname != "" {
		return hostname, nil
	}
	hostname, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("pod host name: ENVOY_POD_HOSTNAME is unset and the host name is unreadable: %w", err)
	}
	return hostname, nil
}

// startRetry is how long a start waits between attempts to reach NATS while it cannot answer: the
// connect (connectListener), each store's buckets (openStartStore) and the listener state bucket
// (openDeliveryLease). /healthz answers 503 "starting" meanwhile, and a SIGTERM or SIGINT ends the
// process at once, since the start has installed no handler for either yet.
const startRetry = 2 * time.Second

// listenerGrant names the grant that gives the listener's NATS user its KV buckets, for the line
// that reports a bucket the grant refuses.
const listenerGrant = "infra/core ListenerPermissions"

// natsConnectionError reports whether err is a failure to reach NATS, or to get an answer from it,
// that a later attempt can get past: no server answered the dial or closed it during the
// handshake, a request ran out of time or found no responder, the connection closed or is
// reconnecting, or JetStream answered that it is not available yet. Any other error, a credential
// or configuration the connect refuses before it dials among them, is not one.
func natsConnectionError(err error) bool {
	for _, transient := range []error{
		io.EOF, io.ErrUnexpectedEOF, syscall.ECONNREFUSED, syscall.ECONNRESET, context.DeadlineExceeded,
		nats.ErrNoServers, nats.ErrTimeout, nats.ErrNoResponders, nats.ErrNoStreamResponse,
		nats.ErrConnectionClosed, nats.ErrDisconnected, nats.ErrConnectionReconnecting, nats.ErrStaleConnection,
	} {
		if errors.Is(err, transient) {
			return true
		}
	}
	if netErr := net.Error(nil); errors.As(err, &netErr) {
		return true
	}
	var jsErr nats.JetStreamError
	if errors.As(err, &jsErr) {
		api := jsErr.APIError()
		return api != nil && api.Code == http.StatusServiceUnavailable
	}
	return false
}

// connectListener connects to NATS and reconciles the stream the listener owns
// (bus.ConnectOwningStream), as instance, at the deployment's replica count and byte limit and
// with the buckets it keeps (listenerStreamOptions). A NATS that cannot be reached
// (natsConnectionError) is tried again every startRetry for as long as it takes, so a listener
// started before its NATS waits for it rather than exiting. Any other failure ends the process
// after one ERROR naming it.
func connectListener(cfg config.Service, instance string, logger *logging.Logger) *bus.Client {
	for {
		options := append(listenerStreamOptions(cfg), bus.WithName("envoy-listener "+instance))
		client, err := bus.ConnectOwningStream(cfg.NATSURLs, options...)
		if err == nil {
			return client
		}
		if !natsConnectionError(err) {
			logger.Error("NATS connect failed", slog.String("error", err.Error()))
			os.Exit(1)
		}
		logger.Warn("NATS connect failed; retrying", slog.String("retry_in", startRetry.String()), slog.String("error", err.Error()))
		time.Sleep(startRetry)
	}
}

// openStartStore opens one of the listener's stores on client's connection with open, which opens
// buckets, until it succeeds. An open that fails for want of NATS (natsConnectionError) is retried
// every startRetry. An open the NATS user's grant refuses ends the process after one ERROR naming
// the bucket and the grant it lacks: nats-server drops a refused request and reports the refusal
// only as the connection's last error, so the open runs out of time, and a last error that changed
// across the open to a refusal of one of buckets' subjects is that refusal (bucketRefusal). Any
// other failure ends the process after one ERROR naming it.
func openStartStore[T any](client *bus.Client, logger *logging.Logger, what string, buckets []string, open func(*nats.Conn) (T, error)) T {
	for {
		conn := client.Conn
		before := conn.LastError()
		store, err := open(conn)
		if err == nil {
			return store
		}
		if bucket, violation := bucketRefusal(buckets, err, before, conn.LastError()); violation != nil {
			logger.Error("NATS refused KV bucket "+bucket+": the listener's NATS user lacks the grant for it",
				slog.String("bucket", bucket),
				slog.String("grant", listenerGrant),
				slog.String("error", violation.Error()),
			)
			conn.Close()
			os.Exit(1)
		}
		if !natsConnectionError(err) {
			logger.Error(what+" open failed", slog.String("buckets", strings.Join(buckets, ",")), slog.String("error", err.Error()))
			conn.Close()
			os.Exit(1)
		}
		logger.Warn(what+" open failed; retrying",
			slog.String("buckets", strings.Join(buckets, ",")),
			slog.String("retry_in", startRetry.String()),
			slog.String("error", err.Error()),
		)
		time.Sleep(startRetry)
	}
}

// bucketRefusal returns the bucket of buckets whose subjects the server refused a failed open, and
// the refusal: err when it is itself such a refusal, else after, the connection's last error once
// the open failed, when it differs from before, the last error before it. nats.go keeps one last
// error per connection, so a refusal another goroutine causes in between hides this one, and the
// open reads as a connection error that is retried and refused again.
func bucketRefusal(buckets []string, err, before, after error) (string, error) {
	for _, bucket := range buckets {
		if refusesBucket(err, bucket) {
			return bucket, err
		}
		if !sameLastError(before, after) && refusesBucket(after, bucket) {
			return bucket, after
		}
	}
	return "", nil
}

// refusesBucket reports whether err is the server's refusal of a publish to a subject of KV bucket
// bucket: a JetStream API call on its stream KV_<bucket>, or a write to one of its keys,
// $KV.<bucket>.<key>.
func refusesBucket(err error, bucket string) bool {
	if err == nil || !errors.Is(err, nats.ErrPermissionViolation) {
		return false
	}
	match := publishViolation.FindStringSubmatch(err.Error())
	if match == nil {
		return false
	}
	subject, stream := match[1], ".KV_"+bucket
	return strings.HasSuffix(subject, stream) || strings.Contains(subject, stream+".") ||
		strings.HasPrefix(subject, "$KV."+bucket+".")
}

// openDeliveryLease opens the listener state bucket and makes machineID's delivery lease over it
// for this process, instance, and returns it with the connection the bucket is on. The bucket is
// opened on a NATS connection of the lease's own (bus.Dial, as the user and through the proxy the
// environment names), which nats.go reconnects in place for the life of the process: the bus client
// replaces its connection after a close, and a handle on the replaced one would never renew the
// lease again. An open that fails for want of a connection is retried every startRetry, so a NATS
// that answers late does not end the start. An open the NATS user's grant refuses
// (errListenerStateRefused) returns no lease and no connection after one ERROR naming the bucket
// and the missing grant: the listener then delivers without a lease, as the machine's one
// deliverer receiving only while no other subscriber holds the durable, and says so in /healthz.
func openDeliveryLease(urls []string, replicas int, machineID, instance string, logger *logging.Logger) (*lease, *nats.Conn) {
	var conn *nats.Conn
	for {
		var err error
		if conn == nil {
			conn, err = bus.Dial("envoy-listener-lease "+instance, urls, os.LookupEnv)
		}
		if err == nil {
			var kv bus.KeyValue
			if kv, err = openListenerState(conn, replicas, listenerStateRequestWait); err == nil {
				return newLease(kv, machineID, instance, productionLeaseConfig(), time.Now, logger), conn
			}
		}
		if errors.Is(err, errListenerStateRefused) {
			logger.Error("listener lease unavailable: the NATS user lacks the grant for KV bucket "+bus.ListenerStateBucket+"; delivering without a lease",
				slog.String("bucket", bus.ListenerStateBucket),
				slog.String("grant", listenerGrant),
				slog.String("error", err.Error()),
			)
			conn.Close()
			return nil, nil
		}
		logger.Warn("listener state bucket open failed; retrying",
			slog.String("bucket", bus.ListenerStateBucket),
			slog.String("retry_in", startRetry.String()),
			slog.String("error", err.Error()),
		)
		time.Sleep(startRetry)
	}
}

// promptRequestLimit is the longest one prompt to a session may take (session.Deliverer's
// RequestLimit): the delivery ledger's take-over age and the unplaceable deferrals' expiry
// (unplaceableDeferralExpiry) count on it.
const promptRequestLimit = 30 * time.Second

// ledgerTakeoverMargin is how much longer than the longest prompt (session.Deliverer.RequestLimit)
// a pod waits on another pod's unresolved delivery attempt before it takes the attempt over: the
// other pod may still be prompting until then, and after it, that pod died without resolving it.
const ledgerTakeoverMargin = 5 * time.Second

// ledgerReconnectWait bounds how long the durable lane waits for the delivery ledger's connection
// to come back, to ask the ledger once more, before it NAKs the message for durableRetryDelay.
const ledgerReconnectWait = 5 * time.Second

// ledgerConnected is the delivery handler's ledgerReconnected over conn, the connection the
// listener state bucket is on: it waits, polling, until conn is connected, for at most
// ledgerReconnectWait and while ctx lasts. A listener without the bucket (conn nil) keeps its
// ledger in process and needs none.
func ledgerConnected(conn *nats.Conn) func(ctx context.Context) bool {
	if conn == nil {
		return nil
	}
	return func(ctx context.Context) bool {
		ctx, cancel := context.WithTimeout(ctx, ledgerReconnectWait)
		defer cancel()
		poll := time.NewTicker(50 * time.Millisecond)
		defer poll.Stop()
		for !conn.IsConnected() {
			select {
			case <-ctx.Done():
				return conn.IsConnected()
			case <-poll.C:
			}
		}
		return true
	}
}

// newDeliveryLedger makes the delivery ledger every lane asks before it prompts a session or
// forwards a role-lane message to it: the `delivery.` keys of the listener state bucket, shared by
// every pod of machineID, under the lease's boot, so a pod that takes the lease over does not prompt
// a session again for a message the pod before it delivered, and prompts at once one that a former
// lease holder (lease.FormerHolder) only attempted, so its settle hold delivers it in stream order.
// Another pod's young attempt is otherwise deferred until it is requestLimit and
// ledgerTakeoverMargin old. A listener with no lease (its NATS user lacks the grant for the
// bucket) is the machine's one deliverer and keeps the ledger in process (dedupe.Memory), which a
// restart forgets.
//
// A record is keyed by the envelope's dedupe key, not its event id: fan-out normalization can
// produce envelopes with one event id and distinct dedupe keys. Each prompt is recorded as
// attempted before it is sent and as delivered once it lands, so a slow server that accepts
// prompt_async but answers 204 after the HTTP client gave up is not prompted again (#389).
func newDeliveryLedger(deliveryLease *lease, machineID string, requestLimit time.Duration) dedupe.Ledger {
	takeoverAfter := requestLimit + ledgerTakeoverMargin
	if deliveryLease == nil {
		return dedupe.NewMemory(takeoverAfter)
	}
	return dedupe.NewKV(deliveryLease.kv, dedupe.KVConfig{
		Durable:       bus.ListenerDurable(machineID),
		Boot:          deliveryLease.boot,
		TakeoverAfter: takeoverAfter,
		GoneBoot:      deliveryLease.FormerHolder,
	})
}

// listenerStreamOptions are the options envoy-listener starts the stream with
// (bus.ConnectOwningStream): the deployment's replica count and byte limit, from cfg, and the
// buckets the listener opens at that count (bus.ListenerBuckets: store.Open's interest and role
// buckets, session.OpenSessionRegistry's and cistore.Open's, the list its NATS grant is built
// from), which the client's keeper raises back to it when a concurrent start or an operator
// lowered one (bus.WithKeptBuckets). The CI bucket is named even on a listener with no GitHub
// route, which opens none: the keeper skips a bucket that does not exist. The listener state
// bucket is not named here but added once the lease has opened it (bus.Client.KeepBucket, main),
// so a pod whose NATS user lacks the grant for it is not refused on it every period. main and the
// tests that start the listener's stream both take them from here.
func listenerStreamOptions(cfg config.Service) []bus.ConnectOption {
	return []bus.ConnectOption{
		bus.WithReplicas(cfg.NATSReplicas),
		bus.WithStreamMaxBytes(cfg.StreamMaxBytes),
		bus.WithKeptBuckets(bus.ListenerBuckets()...),
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

	// Phase 1: Load config (synchronous, fast). With ENVOY_LISTEN_FD, Load inspects the inherited
	// socket and takes its address as cfg's, so the API auth fence below judges the address that
	// serves; Phase 2 adopts it.
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
	// An issuer's discovery runs in the background until it succeeds (apiVerifier.discover): /v1
	// answers 503 "service starting" and /healthz "starting" meanwhile, so an issuer that does not
	// answer at start leaves the listener up and unready instead of ending it.
	apiVerifier := newAPIVerifier(oidcIssuer, oidcAudience)
	if err := validateListenerAPIAuth(cfg, apiToken, os.Getenv("ENVOY_API_ALLOW_UNAUTHENTICATED"), apiVerifier); err != nil {
		log.Fatal(err)
	}
	logger.Info("listener API auth: " + describeListenerAPIAuth(apiToken, oidcIssuer))
	logger.Info("listener API hosts: " + describeListenerAPIHosts(apiToken, apiVerifier, cfg.AllowedHosts))
	if apiVerifier != nil {
		go apiVerifier.discover(context.Background(), logger)
	}

	// Load webhook config (fast — env var reads only).
	webhookCfg, err := webhook.LoadWebhookConfig()
	if err != nil {
		log.Fatal(err)
	}

	// Phase 2: Serve on the socket the listener inherited (ENVOY_LISTEN_FD) or bind its address,
	// in the main goroutine before any NATS work begins, so /healthz is reachable as soon as Serve
	// starts, whatever the NATS connection's latency.
	var ln net.Listener
	if cfg.ListenSocket != nil {
		ln, err = listenfd.Adopt(*cfg.ListenSocket)
	} else {
		ln, err = net.Listen("tcp", cfg.ListenAddress())
	}
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
		ciLegacyHeld = met.NewGauge("envoy_ci_legacy_records_held", "CI records a head-gated listener left unsettled that the summary loop's last pass held back")
	}
	met.NewGaugeFunc("envoy_active_sessions", "Number of active sessions", func() int64 {
		d := deps.Load()
		if d == nil {
			return 0
		}
		return int64(d.sessions.LiveCount())
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

	// /healthz is always reachable, before NATS setup as well as after it, and answers 503
	// "starting" until the listener is ready.
	mux.HandleFunc("/healthz", healthzHandler(&deps, apiVerifier))

	// GaugeFunc for consumer pending — queries NATS at scrape time

	// The webhook and /v1 routes answer 503 "service starting" until their dependencies are open,
	// each behind its own gate: the webhooks once NATS and, for the GitHub route, the CI store are
	// (Phase 5), /v1 once the interest and session caches are too (Phase 6) and, with an OIDC
	// issuer configured, its discovery has succeeded (apiAuth answers that wait itself). Neither
	// waits for the durable consumer, which only /healthz reads. The webhook paths reach their gate
	// bare, /v1 through apiAuth.
	webhookGate, v1Gate := newStartingGate("webhook", logger), newStartingGate("v1", logger)
	hooks := webhookRoutes(webhookCfg)
	for _, hook := range hooks {
		mux.Handle(hook.path, webhookGate)
	}
	// Serve /v1/* on the listener port for local plugin registration.
	v1Handler := apiAuth(apiToken, apiVerifier, cfg.AllowedHosts, logger, v1Gate)
	mux.Handle("/v1", v1Handler)
	mux.Handle("/v1/", v1Handler)

	// Phase 4: Start HTTP server on the listener Phase 2 holds.
	server := newHTTPServer(mux)
	fatal := make(chan error, 1)
	go func() {
		if err := server.Serve(ln); err != http.ErrServerClosed {
			fatal <- err
		}
	}()
	listeningAt := time.Now()
	// The address ln is bound to, whether inherited or bound here, so the line names the port a
	// kernel chose (PORT=0 or a socket a harness opened on port 0).
	logger.Info("envoy-listener listening", slog.String("addr", ln.Addr().String()))

	// Phase 5: Connect to NATS, for as long as NATS takes to answer. The listener owns
	// ENVOY_NOTIFICATIONS: this start reconciles the stream (bus.ConnectOwningStream) at the
	// deployment's replica count and byte limit (listenerStreamOptions).
	client := connectListener(cfg, instance, logger)
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
	// and started once /v1 serves and the shutdown can release it. A pod that opened the bucket
	// has the grant for it, so its keeper raises that bucket to the deployment's copies every
	// period too (bus.Client.KeepBucket): naming it at the connect would have a pod whose grant
	// lacks it, which delivers without a lease, refused on it every period instead.
	deliveryLease, leaseConn := openDeliveryLease(cfg.NATSURLs, cfg.NATSReplicas, cfg.MachineID, instance, logger)
	if deliveryLease != nil {
		client.KeepBucket(bus.ListenerStateBucket)
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
		ciStore = openStartStore(client, logger, "CI store", []string{bus.CIStateBucket}, func(conn *nats.Conn) (*cistore.Store, error) {
			return cistore.Open(conn, logger, cistore.WithReplicas(cfg.NATSReplicas), cistore.WithTTL(7*24*time.Hour))
		})
	} else {
		logger.Info("CI store not opened: no GitHub webhook route, so this listener records and settles no checks")
	}
	openWebhooks(webhookGate, hooks, client, ciStore)
	logger.Info("envoy-listener webhooks open (NATS connected)")

	// The registry and its cache watcher log through the listener's own handler, so its warm-up
	// and its reaper cycles are JSON records with this machine's id. Never slog.SetDefault
	// instead: see store.WithLogger for the CloudWatch filters that breaks. A role claim's grace
	// counts from the machine's service start, the delivery lease record's service_since, which
	// every pod reads alike; a listener with no lease takes its own start (store.WithServiceSince).
	registryOptions := []store.OpenOption{store.WithReplicas(cfg.NATSReplicas), store.WithLogger(logger.Slog())}
	if deliveryLease != nil {
		registryOptions = append(registryOptions, store.WithServiceSince(func() time.Time { return deliveryLease.Status().ServiceSince }))
	}
	registry := openStartStore(client, logger, "interest registry", []string{bus.InterestBucket, bus.RoleClaimBucket}, func(conn *nats.Conn) (*store.Registry, error) {
		return store.Open(conn, registryOptions...)
	})

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
		logger.Warn("interest or role claim cache warm-up timed out; serving with possibly empty cache",
			slog.String("error", err.Error()))
	}
	cacheReadyCancel()

	// A session's last-seen time lives in the listener state bucket beside the lease, so every pod
	// answers it alike for a lapsed session; a listener with no lease keeps it in process
	// (session.WithLastSeenBucket). A dead session's last heartbeat is kept for as long as the
	// interest reaper can still hold its interest: the reaper deletes the row only
	// store.InterestReapAllowance after its last write, so /v1/interests/ can report the session's
	// real last heartbeat (the row's updated_at lags it by up to store.InterestRefreshAge) for
	// every row it still lists. The allowance is the reaper's own (store.Registry.Reap), so the
	// two cannot drift apart.
	sessionOptions := []session.SessionRegistryOption{
		session.WithSessionReplicas(cfg.NATSReplicas),
		session.WithSessionLogger(logger.Slog()),
		session.WithLastSeenRetention(store.InterestReapAllowance(store.InterestReapGrace)),
	}
	if deliveryLease != nil {
		sessionOptions = append(sessionOptions, session.WithLastSeenBucket(deliveryLease.kv))
	}
	sessions := openStartStore(client, logger, "session registry", []string{bus.SessionBucket}, func(conn *nats.Conn) (*session.SessionRegistry, error) {
		return session.OpenSessionRegistry(conn, sessionOptions...)
	})

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

	podHostname, err := listenerPodHostname()
	if err != nil {
		logger.Warn("listener pod host name unknown: port registrations are not stamped with the accepting pod, and pod-local delivery addresses are not checked against it", slog.String("error", err.Error()))
	}
	deliver := session.Deliverer{
		MachineID:    cfg.MachineID,
		HostBridge:   os.Getenv("ENVOY_HOST_BRIDGE"),
		PodHost:      podHostname,
		RequestLimit: promptRequestLimit,
	}

	ledger := newDeliveryLedger(deliveryLease, cfg.MachineID, deliver.RequestLimit)

	roleQueue := bus.ListenerQueue(cfg.MachineID)

	// The window an agent delivery this listener cannot place is retried within before it is
	// given up with a record (unplaceable_delivery.go).
	unplaceableRetryWindow := envDuration(logger, "ENVOY_UNPLACEABLE_RETRY_WINDOW", defaultUnplaceableRetryWindow)

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
		ledger:            ledger,
		ledgerReconnected: ledgerConnected(leaseConn),
		logger:            logger,
		messagesReceived:  messagesReceived,
		messagesDelivered: messagesDelivered,
		messagesNAKed:     messagesNAKed,
		deliveryDuration:  deliveryDuration,

		unplaceableRetryWindow: unplaceableRetryWindow,
		// One deferral set for the life of the process, shared by construction by every handler
		// built from this one value below - the role lane's, the live lane's and the durable's,
		// which the receive loop keeps across every resubscribe and every take-over of the lease -
		// so no handler opens a second budget beside the first (unplaceableDeferrals,
		// unplaceable_delivery.go).
		deferredUnplaceable: newUnplaceableDeferrals(),
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
	ciDebounce := envDuration(logger, "ENVOY_CI_DEBOUNCE", 5*time.Second)
	// summaryDone closes once the last term's CI summary loop has detached from the CI store. A
	// store feeds one summary loop at a time, and a loop's pass in flight can outlast the end of
	// its term's wait (holderLoopsStopWait), so each term's loop starts only after it.
	summaryDone := make(chan struct{})
	close(summaryDone)
	holderLoops := func(ctx context.Context) []<-chan struct{} {
		// The interest reaper cross-references envoy_sessions (5-min TTL) with envoy_interests
		// (permanent) to prune orphaned interests from dead sessions, each kept until more than
		// store.InterestReapGrace plus store.InterestRefreshAge (15 minutes) has passed since its
		// last write; the role-claim reaper ends the claims of holders whose sessions are gone.
		done := []<-chan struct{}{
			registry.StartReaper(ctx, func(sessionID string) bool { return isSessionLive(sessions, sessionID) }, 5*time.Minute, store.InterestReapGrace),
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
		// The loop runs a pass only on a one-second tick by which a cached record changed or fell
		// due.
		if ciStore != nil {
			previous, loopDone := summaryDone, make(chan struct{})
			summaryDone = loopDone
			go func() {
				defer close(loopDone)
				<-previous
				if ctx.Err() == nil {
					<-cistore.StartSummaryLoop(ctx, ciStore, client, ciDebounce, 1*time.Second, ciLegacyHeld, logger)
				}
			}()
			done = append(done, loopDone)
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
		client:      client,
		live:        live,
		registry:    registry,
		sessions:    sessions,
		ciStore:     ciStore,
		caches:      caches,
		receiver:    receiver,
		streamName:  bus.Stream,
		podHostname: podHostname,
		delivery:    &deliveryConfig,
		confirmed:   dedupe.New(10 * time.Minute),
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
	loops.stop()

	// 2. Stop receiving the durable, inside the 10 s budget the HTTP stop shares: the holder-only
	// loops stop, the handler takes no more messages, the message in hand gets inflightGrace (3 s)
	// before its prompt is cancelled and it is NAKed, with a second for the handler to return,
	// every message not handled is NAKed with a delay for the next holder, and the lease is marked
	// released, so a standby takes the lease at its next poll; the subscription then stays until
	// the standby receives, at least 1.25 s after the release (lingerUntaken and a poll, the floor
	// that holds the deliver subject while the standby subscribes) and up to lingerMax (3 s) after
	// the NAKs, then drains (2 s) and, when the whole hand-over went through, writes the lease's
	// clean-hand-back mark, one bucket call of 2 s at most, which ends the standby's settle hold
	// at 3 s (durableReceiver.handBack): 11 s at most, and 12.25 s when the flush (1 s) and the
	// release (2 s) both run to their bounds and leave that floor past the cap. This budget bounds
	// none of those waits: it is how long this shutdown waits for the receive stop. Past it the
	// shutdown goes on to the HTTP stop with no time left, then to the NATS drains below and
	// os.Exit(1), while the hand-back may still be draining or writing its mark. That exit, not the
	// runtime's kill, cuts the hand-back off, so a longer grace period in the runtime keeps no
	// mark. A stop cut off this way writes no mark, and the next holder then settles at its 7 s
	// cap, as it does after any stop that wrote none.
	// HTTP keeps serving meanwhile.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	loops.receiver.Stop(shutdownCtx)

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
	if awaitDone(shutdownCtx, loops.monitorDone) != nil {
		logger.Warn("self-health monitor still running at shutdown")
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
