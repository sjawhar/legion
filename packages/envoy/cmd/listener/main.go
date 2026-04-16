package main

import (
	"github.com/sjawhar/envoy/internal/logging"
	"log/slog"
	"encoding/json"
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"os/signal"
	"syscall"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/config"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dedupe"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/metrics"
	envoytsnet "github.com/sjawhar/envoy/internal/tsnet"
	"github.com/sjawhar/envoy/internal/webhook"
)

// listenerDeps holds NATS-dependent resources published atomically after
// initialization completes. HTTP handlers read these via atomic.Pointer to
// avoid data races during the startup window.
type listenerDeps struct {
	client   *bus.Client
	registry *store.Registry
	sessions *session.SessionRegistry
}

const rolePatternString = `^[a-z0-9][a-z0-9_-]*$`

var rolePattern = regexp.MustCompile(rolePatternString)

func isValidRole(role string) bool {
	return rolePattern.MatchString(role)
}

// startListenerSubscription preserves the durable consumer so restarts resume
// from the last ACKed message instead of skipping pending work.
func startListenerSubscription(client *bus.Client, consumer string, handler nats.MsgHandler) (*nats.Subscription, error) {
	return client.Subscribe(
		"notifications.>",
		handler,
		nats.Durable(consumer),
		nats.AckExplicit(),
		nats.ManualAck(),
		nats.AckWait(60*time.Second),
		nats.MaxAckPending(256),
		nats.MaxDeliver(20),
	)
}

func isSessionLive(sessions *session.SessionRegistry, sessionID string) bool {
	_, err := sessions.Get(sessionID)
	return err == nil
}

func shouldNAKFanoutDelivery(sessions *session.SessionRegistry, sessionID string, err error) bool {
	if err == nil {
		return false
	}
	return isSessionLive(sessions, sessionID)
}

// readinessGate returns 503 until ready returns true, providing a single
// gate for all /v1/* endpoints during NATS initialization.
func readinessGate(ready func() bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ready() {
			http.Error(w, "service starting", http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// publishHandler rejects agent-targeted topics (must use /v1/messages/send
// instead) and publishes the envelope to NATS.
func publishHandler(state *atomic.Pointer[listenerDeps]) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Source         string `json:"source"`
			SourceSession  string `json:"source_session"`
			Topic          string `json:"topic"`
			Message        string `json:"message"`
			IdempotencyKey string `json:"idempotency_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if body.Topic == "" || body.Message == "" {
			http.Error(w, "topic and message are required", http.StatusBadRequest)
			return
		}
		if strings.HasPrefix(body.Topic, contracts.AgentTopicPrefix) {
			http.Error(w, "cannot publish to agent topics; use /v1/messages/send for direct agent messages", http.StatusBadRequest)
			return
		}
		if body.Source == "" {
			body.Source = "agent"
		}
		dedupeKey := "publish." + id.New()
		if body.IdempotencyKey != "" {
			dedupeKey = "publish." + body.IdempotencyKey
		}
		item := contracts.Envelope{
			EventID:        id.New(),
			Source:         body.Source,
			SourceSession:  body.SourceSession,
			SourceEventID:  id.New(),
			Topic:          body.Topic,
			DedupeKey:      dedupeKey,
			IssuedAt:       contracts.NowMillis(),
			PayloadSummary: body.Message,
			TraceID:        id.New(),
		}
		if err := item.Validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		d := state.Load()
		if err := d.client.Publish(item); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(item)
	}
}

// sessionInfo is the joined view of an Interest (topics, dir, machine) and a
// SessionEntry (port). Returned by GET /v1/sessions.
type sessionInfo struct {
	SessionID string   `json:"session_id"`
	MachineID string   `json:"machine_id"`
	Dir       string   `json:"dir"`
	Port      int      `json:"port"`
	Title     string   `json:"title"`
	Topics    []string `json:"topics"`
	UpdatedAt int64    `json:"updated_at"`
}

// sessionsHandler returns all live sessions by iterating the session registry
// (envoy_sessions, 5-min TTL — only live sessions) and enriching each with
// topic data from the interests registry (envoy_interests).
func sessionsHandler(registry *store.Registry, sessions *session.SessionRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if sessions == nil {
			http.Error(w, "session registry unavailable", http.StatusServiceUnavailable)
			return
		}
		entries, err := sessions.List()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		result := make([]sessionInfo, 0, len(entries))
		for _, entry := range entries {
			info := sessionInfo{
				SessionID: entry.SessionID,
				MachineID: entry.MachineID,
				Dir:       entry.Dir,
				Port:      entry.Port,
				Title:     entry.Title,
				UpdatedAt: entry.UpdatedAt,
			}
			if registry != nil {
				if interest, err := registry.Get(entry.SessionID); err == nil {
					info.Topics = interest.Topics
				}
			}
			result = append(result, info)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	}
}

func adminInterestsHandler(registry *store.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID := strings.TrimPrefix(r.URL.Path, "/v1/interests/")

		if sessionID == "" {
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			items := registry.List()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(items)
			return
		}

		switch r.Method {
		case http.MethodGet:
			item, err := registry.Get(sessionID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(item)
		case http.MethodDelete:
			if err := registry.Remove(sessionID, nil); err != nil {
				if !errors.Is(err, nats.ErrKeyNotFound) {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

func roleSetHandler(state *atomic.Pointer[listenerDeps], machineID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			SessionID string `json:"session_id"`
			Role      string `json:"role"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		body.SessionID = strings.TrimSpace(body.SessionID)
		body.Role = strings.TrimSpace(body.Role)
		if body.SessionID == "" {
			http.Error(w, "session_id is required", http.StatusBadRequest)
			return
		}
		if body.Role == "" {
			http.Error(w, "role is required", http.StatusBadRequest)
			return
		}
		if !isValidRole(body.Role) {
			http.Error(w, "role must match "+rolePatternString, http.StatusBadRequest)
			return
		}
		d := state.Load()
		if d == nil || d.registry == nil {
			http.Error(w, "service starting", http.StatusServiceUnavailable)
			return
		}
		item, err := d.registry.SetRole(body.SessionID, machineID, body.Role)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(item)
	}
}

func main() {
	// Phase 1: Load config (synchronous, fast).
	cfg, err := config.Load(9020)
	if err != nil {
		log.Fatal(err)
	}
	logger := logging.New(cfg.MachineID)

	// Load tsnet config (fast — env var reads only).
	tsCfg, err := envoytsnet.LoadConfig()
	if err != nil {
		log.Fatal(err)
	}

	// Load webhook config (fast — env var reads only).
	webhookCfg, err := webhook.LoadWebhookConfig()
	if err != nil {
		log.Fatal(err)
	}

	// Phase 2: Bind HTTP port deterministically in main goroutine before
	// any NATS work begins. This guarantees /healthz is reachable as soon
	// as Serve starts, regardless of NATS connection latency.
	addr := ":" + strconv.Itoa(cfg.Port)
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
		response := map[string]interface{}{"status": "healthy"}
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
	if webhookCfg.GitHub != nil {
		mux.Handle("/webhook/github", readinessGate(
			func() bool { return deps.Load() != nil },
			webhook.GitHubHandler(webhookCfg.GitHub.Secret, webhookCfg.GitHub.MentionTrigger, webhookPublisher),
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

	v1.HandleFunc("/v1/interests/subscribe", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			SessionID string   `json:"session_id"`
			Dir       string   `json:"dir"`
			Topics    []string `json:"topics"`
			Port      int      `json:"port"`
			Title     string   `json:"title"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		logger.Info("listener subscribe", slog.String("session_id", body.SessionID), slog.Any("topics", body.Topics), slog.Int("port", body.Port), slog.String("dir", body.Dir))
		d := deps.Load()
		item, err := d.registry.Upsert(store.Interest{
			SessionID: body.SessionID,
			MachineID: cfg.MachineID,
			Dir:       body.Dir,
		}, append(body.Topics, contracts.AgentSubject(body.SessionID)))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if body.Port > 0 {
			if err := d.sessions.Put(body.SessionID, session.SessionEntry{
				Port:      body.Port,
				MachineID: cfg.MachineID,
				Dir:       body.Dir,
				Title:     body.Title,
			}); err != nil {
				logger.Error("listener session registry put failed", slog.String("session_id", body.SessionID), slog.String("error", err.Error()))
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(item)
	})
	v1.HandleFunc("/v1/interests/unsubscribe", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			SessionID string   `json:"session_id"`
			Topics    []string `json:"topics"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		logger.Info("listener unsubscribe", slog.String("session_id", body.SessionID), slog.Any("topics", body.Topics))
		d := deps.Load()
		if err := d.registry.Remove(body.SessionID, body.Topics); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	v1.HandleFunc("/v1/roles/set", roleSetHandler(&deps, cfg.MachineID))
	v1.HandleFunc("/v1/registry/", func(w http.ResponseWriter, r *http.Request) {
		sessionID := strings.TrimPrefix(r.URL.Path, "/v1/registry/")
		if sessionID == "" {
			http.Error(w, "session_id required", http.StatusBadRequest)
			return
		}
		d := deps.Load()
		entry, err := d.sessions.Get(sessionID)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(entry)
	})

	v1.HandleFunc("/v1/interests/", func(w http.ResponseWriter, r *http.Request) {
		d := deps.Load()
		adminInterestsHandler(d.registry).ServeHTTP(w, r)
	})
	v1.HandleFunc("/v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		d := deps.Load()
		sessionsHandler(d.registry, d.sessions).ServeHTTP(w, r)
	})
	v1.HandleFunc("/v1/messages/send", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			SourceSession  string `json:"source_session"`
			TargetSession  string `json:"target_session"`
			Message        string `json:"message"`
			IdempotencyKey string `json:"idempotency_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		dedupeKey := "agent." + body.TargetSession + "." + id.New()
		if body.IdempotencyKey != "" {
			dedupeKey = "agent." + body.TargetSession + "." + body.IdempotencyKey
		}
		item := contracts.Envelope{
			EventID:        id.New(),
			Source:         "agent",
			SourceSession:  body.SourceSession,
			SourceEventID:  id.New(),
			Topic:          contracts.AgentSubject(body.TargetSession),
			DedupeKey:      dedupeKey,
			IssuedAt:       contracts.NowMillis(),
			PayloadSummary: body.Message,
			TraceID:        id.New(),
		}
		if err := item.Validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		d := deps.Load()
		if err := d.client.Publish(item); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(item)
	})
	v1.HandleFunc("/v1/messages/publish", publishHandler(&deps))

	// Always serve /v1/* on the legacy port for local plugin registration.
	// The tsnet TLS listener (if enabled) also serves /v1/* on :443 for cross-machine access.
	mux.Handle("/v1/", readinessGate(func() bool { return deps.Load() != nil }, v1))

	// Phase 4: Start HTTP server (port already bound via net.Listen).
	server := &http.Server{
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	// Buffer 2: one for legacy HTTP, one for tsnet HTTP (if enabled).
	fatal := make(chan error, 2)
	go func() {
		if err := server.Serve(ln); err != http.ErrServerClosed {
			fatal <- err
		}
	}()
	logger.Info("envoy-listener listening", slog.String("addr", addr))

	// Phase 4.5: Start tsnet server (if enabled). The tsnet node connects
	// to the Tailscale network, which is a slow operation like NATS connect.
	// /v1/* routes on the tsnet listener are gated by readiness (deps nil
	// until NATS init completes).
	// NOTE: No defer Close() here — tsnet shutdown is handled explicitly in
	// the signal handler below to ensure ordered deregistration before NATS drain.
	var tsServer *envoytsnet.Server
	if tsCfg.Enabled {
		tsServer = envoytsnet.New(tsCfg)
		tsMux := http.NewServeMux()
		tsMux.Handle("/v1/", readinessGate(func() bool { return deps.Load() != nil }, v1))
		if _, err := tsServer.Serve(tsMux, fatal); err != nil {
			logger.Warn("tsnet failed to start (non-fatal, continuing without Tailscale)", slog.String("error", err.Error()))
			tsServer.Close()
			tsServer = nil
		} else {
			logger.Info("envoy-listener tsnet serving /v1/* on :443", slog.String("hostname", tsServer.Hostname()))
		}
	}
	// Phase 5: Connect to NATS (main goroutine — log.Fatal is safe here).
	client, err := bus.Connect(cfg.NATSURLs, bus.WithReplicas(cfg.NATSReplicas))
	if err != nil {
		log.Fatal(err)
	}
	registry, err := store.Open(client.Conn, store.WithReplicas(cfg.NATSReplicas))
	if err != nil {
		log.Fatal(err)
	}

	sessions, err := session.OpenSessionRegistry(
		client.Conn,
		session.WithSessionReplicas(cfg.NATSReplicas),
	)
	if err != nil {
		log.Fatal(err)
	}

	deliver := session.Deliverer{
		MachineID:    cfg.MachineID,
		HostBridge:   os.Getenv("ENVOY_HOST_BRIDGE"),
		RequestLimit: 30 * time.Second,
		Sessions:     sessions,
	}
	if tsServer != nil {
		deliver.HTTPClient = tsServer.HTTPClient()
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

	// /healthz handler was registered early (before NATS) — no re-registration needed.

	// Subscribe with retry — during rolling deployments, the old container may still
	// hold the durable consumer binding. Before subscribing, check if the consumer
	// is push-bound by a stale connection and force-delete it if so.
	var sub *nats.Subscription
	for attempt := 1; attempt <= 10; attempt++ {
		// If consumer exists and is push-bound by a dead connection, delete it.
		info, infoErr := client.JS().ConsumerInfo(bus.Stream, consumer)
		if infoErr == nil && info.PushBound {
			logger.Warn("consumer is push-bound by stale connection, deleting",
				slog.String("consumer", consumer),
				slog.Int("attempt", attempt),
			)
			if delErr := client.JS().DeleteConsumer(bus.Stream, consumer); delErr != nil {
				logger.Warn("failed to delete stale consumer",
					slog.String("error", delErr.Error()),
				)
			}
		}
		sub, err = startListenerSubscription(client, consumer, func(msg *nats.Msg) {
		var item contracts.Envelope
		if err := json.Unmarshal(msg.Data, &item); err != nil {
			logger.Error("listener decode failed", slog.String("error", err.Error()))
			_ = msg.Ack()
			return
		}
		if err := item.Validate(); err != nil {
			logger.Error("listener invalid envelope", slog.String("error", err.Error()))
			_ = msg.Ack()
			return
		}
		messagesReceived.Inc([2]string{"source", item.Source}, [2]string{"topic_prefix", metrics.TopicPrefix(item.Topic)})
		logger.Info("listener received", slog.String("source", item.Source), slog.String("source_session", item.SourceSession), slog.String("topic", item.Topic), slog.String("event_id", item.EventID), slog.String("payload_summary", item.PayloadSummary))
		if strings.HasPrefix(item.Topic, contracts.AgentTopicPrefix) {
			sessionID := strings.TrimPrefix(item.Topic, contracts.AgentTopicPrefix)
			if dedupeCache.Seen(item.DedupeKey, sessionID) {
				messagesDelivered.Inc([2]string{"delivery_status", "dedupe"})
				logger.DeliveryLog(slog.LevelInfo, "listener dedupe skip", sessionID, item.Topic, item.EventID, "dedupe", slog.String("dedupe_key", item.DedupeKey))
				_ = msg.Ack()
				return
			}
			if attemptCache.Seen(item.DedupeKey, sessionID) {
				messagesDelivered.Inc([2]string{"delivery_status", "dedupe"})
				logger.DeliveryLog(slog.LevelInfo, "listener attempt-dedupe skip", sessionID, item.Topic, item.EventID, "skipped", slog.String("dedupe_key", item.DedupeKey))
				_ = msg.Ack()
				return
			}
			interest, err := registry.Get(sessionID)
			var interestPtr *store.Interest
			if err == nil {
				interestPtr = &interest
			}
			attemptCache.Record(item.DedupeKey, sessionID)
			deliveryTimer := metrics.NewTimer()
			result := session.HandleAgentMessage(item, sessionID, cfg.MachineID, interestPtr, &deliver)
			if result.Err != nil {
				logger.DeliveryLog(slog.LevelError, "listener agent delivery failed", sessionID, item.Topic, item.EventID, "failed", slog.String("error", result.Err.Error()))
			}
			if !result.Delivered && result.Err != nil {
				deliveryTimer.ObserveDuration(deliveryDuration, [2]string{"delivery_status", "failed"})
				messagesDelivered.Inc([2]string{"delivery_status", "failed"})
			}
			if result.Delivered {
				deliveryTimer.ObserveDuration(deliveryDuration, [2]string{"delivery_status", "delivered"})
				messagesDelivered.Inc([2]string{"delivery_status", "delivered"})
				dedupeCache.Record(item.DedupeKey, sessionID)
				logger.DeliveryLog(slog.LevelInfo, "listener agent delivered", sessionID, item.Topic, item.EventID, "delivered")
			} else if result.Err == nil {
				messagesDelivered.Inc([2]string{"delivery_status", "skipped"})
				logger.DeliveryLog(slog.LevelWarn, "listener agent session not found anywhere", sessionID, item.Topic, item.EventID, "skipped")
			}
			if result.ShouldNAK {
				messagesNAKed.Inc()
				_ = msg.NakWithDelay(30 * time.Second)
			} else {
				_ = msg.Ack()
			}
			return
		}
		items := registry.Match(cfg.MachineID, item.Topic)
		if len(items) == 0 {
			logger.Info("listener no matching interests", slog.String("topic", item.Topic))
			_ = msg.Ack()
			return
		}
		var failed bool
		var deadDeliveries int
		for _, interest := range items {
			if dedupeCache.Seen(item.DedupeKey, interest.SessionID) {
				messagesDelivered.Inc([2]string{"delivery_status", "dedupe"})
				logger.DeliveryLog(slog.LevelInfo, "listener dedupe skip", interest.SessionID, item.Topic, item.EventID, "dedupe", slog.String("dedupe_key", item.DedupeKey))
				continue
			}
			if attemptCache.Seen(item.DedupeKey, interest.SessionID) {
				messagesDelivered.Inc([2]string{"delivery_status", "dedupe"})
				logger.DeliveryLog(slog.LevelInfo, "listener attempt-dedupe skip", interest.SessionID, item.Topic, item.EventID, "skipped", slog.String("dedupe_key", item.DedupeKey))
				continue
			}
			if item.SourceSession != "" && item.SourceSession == interest.SessionID {
				messagesDelivered.Inc([2]string{"delivery_status", "skipped"})
				logger.DeliveryLog(slog.LevelInfo, "listener skip echo", interest.SessionID, item.Topic, item.EventID, "skipped")
				continue
			}
			attemptCache.Record(item.DedupeKey, interest.SessionID)
			deliveryTimer := metrics.NewTimer()
			if err := deliver.Deliver(item, interest); err != nil {
				deliveryTimer.ObserveDuration(deliveryDuration, [2]string{"delivery_status", "failed"})
				messagesDelivered.Inc([2]string{"delivery_status", "failed"})
				logger.DeliveryLog(slog.LevelError, "listener delivery failed", interest.SessionID, item.Topic, item.EventID, "failed", slog.String("error", err.Error()))
				if shouldNAKFanoutDelivery(sessions, interest.SessionID, err) {
					failed = true
				} else {
					deadDeliveries++
				}
			} else {
				deliveryTimer.ObserveDuration(deliveryDuration, [2]string{"delivery_status", "delivered"})
				messagesDelivered.Inc([2]string{"delivery_status", "delivered"})
				dedupeCache.Record(item.DedupeKey, interest.SessionID)
			}
		}
		if deadDeliveries > 0 {
			logger.Info("listener skipped dead session deliveries", slog.String("topic", item.Topic), slog.Int("count", deadDeliveries))
		}
		if failed {
			messagesNAKed.Inc()
			_ = msg.NakWithDelay(30 * time.Second)
		} else {
			_ = msg.Ack()
		}
	})
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

	// Phase 6: Publish initialized state — readiness gate opens for /v1/*.
	deps.Store(&listenerDeps{
		client:   client,
		registry: registry,
		sessions: sessions,
	})
	logger.Info("envoy-listener ready (NATS connected)")

	// Phase 6b: Start interest reaper for stale KV cleanup.
	// Cross-references envoy_sessions (5-min TTL) with envoy_interests (permanent)
	// to prune orphaned interests from dead sessions.
	registry.StartReaper(func(sessionID string) bool { return isSessionLive(sessions, sessionID) }, 5*time.Minute, 10*time.Minute)

	// Phase 7: Block until SIGTERM/SIGINT or fatal error.
	// Ordered shutdown ensures tsnet deregisters its node key before the
	// process exits — critical for ECS rolling deployments where old and new
	// tasks briefly overlap on the same EFS-backed tsnet state directory.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)

	select {
	case err := <-fatal:
		logger.Error("fatal error", slog.String("error", err.Error()))
	case s := <-sig:
		logger.Info("received signal, shutting down", slog.String("signal", s.String()))
	}

	// Ordered shutdown:
	// 1. tsnet first — deregisters node key from Tailscale coordination server.
	//    This must happen before the new container starts and claims the same key.
	if tsServer != nil {
		logger.Info("closing tsnet")
		if err := tsServer.Close(); err != nil {
			logger.Warn("tsnet close error", slog.String("error", err.Error()))
		}
	}

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
