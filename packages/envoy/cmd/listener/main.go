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
	"slices"
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
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/webhook"
)

// listenerDeps holds NATS-dependent resources published atomically after
// initialization completes. HTTP handlers read these via atomic.Pointer to
// avoid data races during the startup window.
type listenerDeps struct {
	client     *bus.Client
	registry   *store.Registry
	sessions   *session.SessionRegistry
	ciStore    *cistore.Store
	streamName string
	streamInfo streamInfoLookup
}

func newCIRecorder(deps *atomic.Pointer[listenerDeps]) webhook.CIRecorderFuncs {
	return webhook.CIRecorderFuncs{
		RecordFunc: func(observation contracts.CIObservation) error {
			return deps.Load().ciStore.Record(observation)
		},
		RecordSuiteFunc: func(observation contracts.CIObservation) error {
			return deps.Load().ciStore.RecordSuite(observation)
		},
		RecordHeadFunc: func(owner, repo, number, sha, updatedAt string) error {
			return deps.Load().ciStore.RecordHead(owner, repo, number, sha, updatedAt)
		},
	}
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
// exactly one definition.
func applyListenerConsumerPolicy(config *nats.ConsumerConfig, subjects []string) {
	config.FilterSubject = ""
	config.FilterSubjects = subjects
	config.AckPolicy = nats.AckExplicitPolicy
	config.AckWait = consumerAckWait
	config.MaxAckPending = consumerMaxAckPending
	config.MaxDeliver = consumerMaxDeliver
	config.InactiveThreshold = consumerInactiveThreshold
}

// listenerConsumerPolicyDrifted reports whether a consumer's server-side
// config diverges from the canonical policy.
func listenerConsumerPolicyDrifted(config nats.ConsumerConfig, subjects []string) bool {
	return config.FilterSubject != "" ||
		!slices.Equal(config.FilterSubjects, subjects) ||
		config.AckPolicy != nats.AckExplicitPolicy ||
		config.AckWait != consumerAckWait ||
		config.MaxAckPending != consumerMaxAckPending ||
		config.MaxDeliver != consumerMaxDeliver ||
		config.InactiveThreshold != consumerInactiveThreshold
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
	info, err := client.JS().ConsumerInfo(bus.Stream, consumer)
	switch {
	case errors.Is(err, nats.ErrConsumerNotFound):
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
	case err != nil:
		return nil, err
	case listenerConsumerPolicyDrifted(info.Config, subjects):
		config := info.Config
		applyListenerConsumerPolicy(&config, subjects)
		if _, err := client.JS().UpdateConsumer(bus.Stream, &config); err != nil {
			return nil, err
		}
	}
	return client.Subscribe(
		"",
		handler,
		nats.Bind(bus.Stream, consumer),
		nats.ManualAck(),
	)
}

func isSessionLive(sessions *session.SessionRegistry, sessionID string) bool {
	if sessions == nil {
		return false
	}
	_, err := sessions.Get(sessionID)
	return err == nil
}

// runSelfHealthWatchdog periodically pings the KV registries and the durable
// consumer, self-terminating after `threshold` consecutive failures. Designed
// for wedged states the bus recovery path cannot repair: a KV-bound JetStream
// context that stays broken after reconnect (observed on sami after a long
// Tailscale write-timeout), or a durable consumer the server garbage-collected
// via consumerInactiveThreshold while this process was alive but unbound —
// recovery replays nats.Bind, which cannot recreate a missing consumer, so
// only a restart re-runs the ensure-then-bind boot path.
//
// Termination uses SIGTERM so the existing graceful shutdown path runs. Docker
// `restart: unless-stopped` (on-prem) and ECS task respawn (Fargate) then bring
// the listener back with a fresh conn.
func runSelfHealthWatchdog(logger *logging.Logger, registry *store.Registry, sessions *session.SessionRegistry, ci *cistore.Store, durable func() error, interval time.Duration, threshold int) {
	terminate := func() { _ = syscall.Kill(os.Getpid(), syscall.SIGTERM) }
	probe := func() error { return checkSelfHealth(registry, sessions, ci, durable) }
	runSelfHealthLoop(logger, probe, terminate, interval, threshold)
}

// checkSelfHealth pings the KV-backed registries and the optional durable
// consumer probe. Returns the first error encountered, or nil when all are
// healthy. Extracted from the watchdog loop so callers (and tests) can probe
// the same view of health that the loop uses.
func checkSelfHealth(registry *store.Registry, sessions *session.SessionRegistry, ci *cistore.Store, durable func() error) error {
	if registry != nil {
		if err := registry.Ping(); err != nil {
			return fmt.Errorf("interest kv: %w", err)
		}
	}
	if sessions != nil {
		if err := sessions.Ping(); err != nil {
			return fmt.Errorf("session kv: %w", err)
		}
	}
	if ci != nil {
		if err := ci.Ping(); err != nil {
			return fmt.Errorf("ci kv: %w", err)
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
// just in-memory reads) and unit-testable. Returns nil when sessions is nil.
func sessionHealthFields(sessions *session.SessionRegistry) map[string]interface{} {
	if sessions == nil {
		return nil
	}
	return map[string]interface{}{
		"session_cache_ready": sessions.CacheReady(),
		"session_cache_size":  sessions.CacheSize(),
		"session_watch_error": sessions.WatchError(),
	}
}

// runSelfHealthLoop ticks `probe`, tracking consecutive failures. After
// `threshold` consecutive failures it invokes `terminate` once and returns.
// Split from runSelfHealthWatchdog so tests can inject a fake terminate.
func runSelfHealthLoop(logger *logging.Logger, probe func() error, terminate func(), interval time.Duration, threshold int) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	failures := 0
	for range ticker.C {
		err := probe()
		if err == nil {
			if failures > 0 {
				logger.Info("self-health recovered", slog.Int("prior_consecutive_failures", failures))
			}
			failures = 0
			continue
		}
		failures++
		logger.Warn("self-health probe failed", slog.Int("consecutive", failures), slog.Int("threshold", threshold), slog.String("error", err.Error()))
		if failures >= threshold {
			logger.Error("self-health threshold exceeded — terminating to let restart policy recover", slog.String("error", err.Error()))
			terminate()
			return
		}
	}
}

// readinessGate returns 503 until ready returns true, providing a single
// gate for all /v1/* endpoints during NATS initialization.
func readinessGate(ready func() bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ready() {
			writeJSONError(w, http.StatusServiceUnavailable, "service starting")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func main() {
	// Phase 1: Load config (synchronous, fast).
	cfg, err := config.Load(9020)
	if err != nil {
		log.Fatal(err)
	}
	logger := logging.New(cfg.MachineID)
	apiToken := os.Getenv("ENVOY_API_TOKEN")
	if err := validateListenerAPIAuth(cfg.ListenHost, apiToken, os.Getenv("ENVOY_API_ALLOW_UNAUTHENTICATED")); err != nil {
		log.Fatal(err)
	}
	if apiToken == "" {
		logger.Info("listener API auth: disabled (ENVOY_API_TOKEN)")
	} else {
		logger.Info("listener API auth: enabled (ENVOY_API_TOKEN)")
	}

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
	met.NewGaugeFunc("envoy_active_sessions", "Number of active sessions", func() int64 {
		d := deps.Load()
		if d == nil || d.sessions == nil {
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
		if d == nil || d.registry == nil {
			return 0
		}
		return int64(len(d.registry.List()))
	})

	// Phase 3: Build HTTP mux.
	mux := http.NewServeMux()

	// /metrics is always reachable — NOT gated by readiness.
	mux.Handle("/metrics", met.Handler())

	var healthzConsumer string
	// /healthz is always reachable — returns 200 "starting" before NATS init,
	// 200 "healthy" after init with live NATS, 503 "unhealthy" if NATS drops.
	// Consumer lag metrics are included when available (after subscription setup).
	// 200 "healthy" after init with live NATS, 503 "unhealthy" if NATS drops.
	// Consumer lag metrics are included when available (after subscription setup).
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
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
		// Ping JetStream KV buckets. The bus recovery path only restores the main
		// subject subscription on reconnect; the KV-backed registries hold handles
		// to the original closed *nats.Conn and never get re-opened. Catching this
		// here lets the self-health watchdog terminate the listener so the restart
		// policy can bring it back fresh.
		if d.registry != nil {
			if err := d.registry.Ping(); err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "unhealthy", "error": "interest kv unavailable: " + err.Error()})
				return
			}
		}
		if d.sessions != nil {
			if err := d.sessions.Ping(); err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "unhealthy", "error": "session kv unavailable: " + err.Error()})
				return
			}
		}
		if d.ciStore != nil {
			if err := d.ciStore.Ping(); err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "unhealthy", "error": "ci kv unavailable: " + err.Error()})
				return
			}
		}
		response := map[string]interface{}{"status": "healthy"}
		for k, v := range sessionHealthFields(d.sessions) {
			response[k] = v
		}
		if healthzConsumer != "" {
			consumerInfo, err := d.client.JS().ConsumerInfo(bus.Stream, healthzConsumer)
			if err == nil && consumerInfo != nil {
				response["num_pending"] = consumerInfo.NumPending
				response["num_ack_pending"] = consumerInfo.NumAckPending
			}
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(response)
	})

	// GaugeFunc for consumer pending — queries NATS at scrape time

	// Webhook routes — on public mux, gated by readiness.
	// Publisher delegates to deps.client behind readinessGate.
	webhookPublisher := webhook.PublisherFunc(func(item contracts.Envelope) error {
		return deps.Load().client.Publish(item)
	})
	// CI recorder folds check_run events into cistore behind the same readiness
	// gate (deps is non-nil once init completes, so ciStore is set).
	ciRecorder := newCIRecorder(&deps)
	if webhookCfg.GitHub != nil {
		mux.Handle("/webhook/github", readinessGate(
			func() bool { return deps.Load() != nil },
			webhook.GitHubHandler(webhookCfg.GitHub.Secret, webhookCfg.GitHub.MentionTrigger, webhookCfg.GitHub.ReviewerAppID, webhookPublisher, ciRecorder),
		))
	}
	if webhookCfg.Slack != nil {
		mux.Handle("/webhook/slack", readinessGate(
			func() bool { return deps.Load() != nil },
			webhook.SlackHandler(webhookCfg.Slack.Secret, webhookPublisher),
		))
	}
	if webhookCfg.GhostWispr != nil {
		mux.Handle("/webhook/ghostwispr", readinessGate(
			func() bool { return deps.Load() != nil },
			webhook.GhostWisprHandler(webhookCfg.GhostWispr.Secret, webhookPublisher),
		))
	}

	// /v1/* routes on a sub-mux, gated by a single readiness middleware.
	v1 := http.NewServeMux()
	registerV1Routes(v1, &deps, cfg.MachineID, logger)

	// Serve /v1/* on the listener port for local plugin registration.
	v1Handler := apiAuth(apiToken, readinessGate(func() bool { return deps.Load() != nil }, v1))
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
	logger.Info("envoy-listener listening", slog.String("addr", addr))

	// Phase 5: Connect to NATS (main goroutine — log.Fatal is safe here).
	client, err := bus.Connect(cfg.NATSURLs, bus.WithReplicas(cfg.NATSReplicas))
	if err != nil {
		log.Fatal(err)
	}
	registry, err := store.Open(client.Conn, store.WithReplicas(cfg.NATSReplicas))
	if err != nil {
		log.Fatal(err)
	}

	// Wait for the interest cache to finish its initial scan of existing KV
	// entries before we start accepting NATS messages. Without this gate, real
	// events that arrive in the warm-up window get "no matching interests"
	// even when the durable KV entry has subscribers — the second half of the
	// Atlas dropout investigation (PR #610 fixed the silent-fallback half).
	//
	// Bounded at 30s: a healthy NATS cluster completes the scan in milliseconds.
	// If the watcher fails to start, signalReady() is called from the error
	// path so we fail open and let the self-health watchdog catch a persistently
	// broken registry via its KV pings.
	cacheReadyCtx, cacheReadyCancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := registry.WaitForCacheReady(cacheReadyCtx); err != nil {
		logger.Warn("interest cache warm-up timed out; serving with possibly empty cache",
			slog.String("error", err.Error()))
	}
	cacheReadyCancel()

	sessions, err := session.OpenSessionRegistry(
		client.Conn,
		session.WithSessionReplicas(cfg.NATSReplicas),
	)
	if err != nil {
		log.Fatal(err)
	}

	// Wait for the session cache to finish its initial scan before we accept
	// traffic, mirroring the interest-cache gate above. Bounded at 30s: a healthy
	// cluster completes in milliseconds. On timeout we fail open — Put
	// write-through keeps locally-registered sessions visible, and the self-health
	// watchdog catches a persistently broken registry via its KV pings.
	sessionCacheReadyCtx, sessionCacheReadyCancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := sessions.WaitForCacheReady(sessionCacheReadyCtx); err != nil {
		logger.Warn("session cache warm-up timed out; serving with possibly empty cache",
			slog.String("error", err.Error()))
	}
	sessionCacheReadyCancel()

	// CI-summary aggregation state. Its WatchAll cache warms asynchronously like
	// the registries above; the summary loop tolerates an empty cache until it fills.
	ciStore, err := cistore.Open(client.Conn, cistore.WithReplicas(cfg.NATSReplicas), cistore.WithTTL(7*24*time.Hour))
	if err != nil {
		log.Fatal(err)
	}

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

	consumer := "listener-" + strings.ReplaceAll(cfg.MachineID, " ", "-")
	healthzConsumer = consumer
	roleQueue := "envoy-listener-" + strings.ReplaceAll(cfg.MachineID, " ", "-")

	// /healthz handler was registered early (before NATS) — no re-registration needed.

	// Subscribe with retry — during rolling deployments, the old container may
	// still hold the durable consumer binding. nats.Bind rejects a second
	// binding ("consumer is already bound"), so retry with backoff until the
	// old listener's delivery interest clears; never delete the consumer to
	// steal the binding — that resets the durable cursor and replays the full
	// retention window to every subscriber.
	deliveryConfig := listenerDeliveryHandlerConfig{
		client:            client,
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

	var sub *nats.Subscription
	for attempt := 1; attempt <= 10; attempt++ {
		sub, err = startListenerSubscription(client, consumer, jetStreamDeliveryHandler(deliveryConfig))
		if err == nil {
			break
		}
		if attempt == 10 {
			logger.Error("subscribe failed after max attempts, shutting down",
				slog.String("error", err.Error()),
				slog.Int("attempts", attempt),
			)
			// Close NATS before exiting — releases the consumer binding so the
			// next container can claim it. log.Fatal/os.Exit skips cleanup.
			client.Conn.Close()
			os.Exit(1)
		}
		// Check if auto-resubscribe (bus.Client.onReconnect) already succeeded
		// while we were sleeping. If so, the consumer is bound by our own client
		// and retrying would hit "consumer is already bound" from ourselves.
		if client.SubOK() {
			logger.Info("subscribe succeeded via auto-resubscribe during retry backoff")
			break
		}
		backoff := time.Duration(attempt) * 3 * time.Second
		logger.Warn("subscribe failed, retrying",
			slog.String("error", err.Error()),
			slog.Int("attempt", attempt),
			slog.String("retry_in", backoff.String()),
		)
		time.Sleep(backoff)
	}
	_ = sub // used by NATS internally

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

	// Phase 6: Publish initialized state — readiness gate opens for /v1/*.
	deps.Store(&listenerDeps{
		client:     client,
		registry:   registry,
		sessions:   sessions,
		ciStore:    ciStore,
		streamName: bus.Stream,
	})
	logger.Info("envoy-listener ready (NATS connected)")

	// Phase 6b: Start interest reaper for stale KV cleanup.
	// Cross-references envoy_sessions (5-min TTL) with envoy_interests (permanent)
	// to prune orphaned interests from dead sessions.
	registry.StartReaper(func(sessionID string) bool { return isSessionLive(sessions, sessionID) }, 5*time.Minute, 10*time.Minute)

	// Phase 6b2: Start the CI summary loop. It emits one pr.<n>.checks event
	// once the head commit's checks settle; new runs re-arm settlement. The
	// debounce window is ENVOY_CI_DEBOUNCE (default 5s).
	ciDebounce := 5 * time.Second
	if v := os.Getenv("ENVOY_CI_DEBOUNCE"); v != "" {
		if d, perr := time.ParseDuration(v); perr == nil {
			ciDebounce = d
		} else {
			logger.Warn("invalid ENVOY_CI_DEBOUNCE; using default", slog.String("value", v), slog.String("error", perr.Error()))
		}
	}
	summaryCtx, summaryCancel := context.WithCancel(context.Background())
	cistore.StartSummaryLoop(summaryCtx, ciStore, client, ciDebounce, 1*time.Second, logger)

	// Phase 6c: Self-health watchdog. Periodically pings the JetStream KV
	// buckets; if they are unreachable for several consecutive checks the
	// listener self-terminates so the container restart policy (Docker
	// `restart: unless-stopped` on-prem, ECS task respawn on Fargate) can
	// bring it back with a fresh NATS conn and registries.
	//
	// This is the failure mode observed on the on-prem `sami` listener after a
	// long Tailscale write-timeout: bus recovery re-established the subject
	// subscription on a fresh *nats.Conn, but store.Registry and
	// session.SessionRegistry kept their KV handles bound to the original
	// closed conn. Restart is cheap and lossless — KV state is durable, NATS
	// retries undelivered messages, and agent sessions re-register.
	// The durable probe only treats a definitively missing consumer as
	// unhealthy — transient lookup failures are already covered by the KV
	// pings — so a consumer the server garbage-collected out from under a
	// live-but-unbound process forces the restart that recreates it.
	durableProbe := func() error {
		if _, err := client.JS().ConsumerInfo(bus.Stream, consumer); errors.Is(err, nats.ErrConsumerNotFound) {
			return err
		}
		return nil
	}
	go runSelfHealthWatchdog(logger, registry, sessions, ciStore, durableProbe, 30*time.Second, 3)
	// Phase 7: Block until SIGTERM/SIGINT or fatal error.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)

	select {
	case err := <-fatal:
		logger.Error("fatal error", slog.String("error", err.Error()))
	case s := <-sig:
		logger.Info("received signal, shutting down", slog.String("signal", s.String()))
	}

	// Stop the CI summary loop first so it doesn't hit the KV on a draining conn.
	summaryCancel()

	// Ordered shutdown:
	// 2. HTTP server — stop accepting new requests, drain in-flight.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warn("http shutdown error", slog.String("error", err.Error()))
	}

	// 3. NATS — drain subscription (finishes in-flight deliveries), then close.
	if err := client.Conn.Drain(); err != nil {
		logger.Warn("nats drain error", slog.String("error", err.Error()))
	}
	client.Conn.Close()

	logger.Info("envoy-listener shutdown complete")
}
