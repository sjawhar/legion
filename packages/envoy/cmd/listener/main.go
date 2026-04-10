package main

import (
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
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
	envoytsnet "github.com/sjawhar/envoy/internal/tsnet"
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
			Source        string `json:"source"`
			SourceSession string `json:"source_session"`
			Topic         string `json:"topic"`
			Message       string `json:"message"`
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
		item := contracts.Envelope{
			EventID:        id.New(),
			Source:         body.Source,
			SourceSession:  body.SourceSession,
			SourceEventID:  id.New(),
			Topic:          body.Topic,
			DedupeKey:      "publish." + id.New(),
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

	// Load tsnet config (fast — env var reads only).
	tsCfg, err := envoytsnet.LoadConfig()
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

	// Phase 3: Build HTTP mux.
	mux := http.NewServeMux()

	// /healthz is always reachable — returns 200 "starting" before NATS init,
	// 200 "healthy" after init with live NATS, 503 "unhealthy" if NATS drops.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		d := deps.Load()
		if d == nil {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "starting"})
			return
		}
		if err := d.client.Conn.FlushTimeout(3 * time.Second); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "unhealthy", "error": "nats unavailable"})
			return
		}
		if !d.client.SubOK() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "unhealthy", "error": "subscription inactive"})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "healthy"})
	})

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
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
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
			}); err != nil {
				log.Printf("listener session registry put failed session=%s: %v", body.SessionID, err)
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
			SourceSession string `json:"source_session"`
			TargetSession string `json:"target_session"`
			Message       string `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		item := contracts.Envelope{
			EventID:        id.New(),
			Source:         "agent",
			SourceSession:  body.SourceSession,
			SourceEventID:  id.New(),
			Topic:          contracts.AgentSubject(body.TargetSession),
			DedupeKey:      "agent." + body.TargetSession + "." + id.New(),
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

	// When tsnet is disabled, serve /v1/* on the legacy port (existing behavior).
	// When tsnet is enabled, /v1/* is served exclusively on the tsnet TLS listener
	// and the legacy port only has /healthz (security boundary).
	if !tsCfg.Enabled {
		mux.Handle("/v1/", readinessGate(func() bool { return deps.Load() != nil }, v1))
	}

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
	log.Printf("envoy-listener listening on %s", addr)

	// Phase 4.5: Start tsnet server (if enabled). The tsnet node connects
	// to the Tailscale network, which is a slow operation like NATS connect.
	// /v1/* routes on the tsnet listener are gated by readiness (deps nil
	// until NATS init completes).
	var tsServer *envoytsnet.Server
	if tsCfg.Enabled {
		tsServer = envoytsnet.New(tsCfg)
		defer tsServer.Close()
		tsMux := http.NewServeMux()
		tsMux.Handle("/v1/", readinessGate(func() bool { return deps.Load() != nil }, v1))
		if _, err := tsServer.Serve(tsMux, fatal); err != nil {
			log.Fatal(err)
		}
		log.Printf("envoy-listener tsnet serving /v1/* on %s:443", tsServer.Hostname())
	}
	// Phase 5: Connect to NATS (main goroutine — log.Fatal is safe here).
	client, err := bus.Connect(cfg.NATSURLs, bus.WithReplicas(cfg.NATSReplicas))
	if err != nil {
		log.Fatal(err)
	}
	defer client.Conn.Close()

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
	_ = client.JS().DeleteConsumer(bus.Stream, consumer)
	_, err = client.Subscribe("notifications.>", func(msg *nats.Msg) {
		var item contracts.Envelope
		if err := json.Unmarshal(msg.Data, &item); err != nil {
			log.Printf("listener decode failed: %v", err)
			_ = msg.Ack()
			return
		}
		if err := item.Validate(); err != nil {
			log.Printf("listener invalid envelope: %v", err)
			_ = msg.Ack()
			return
		}
		log.Printf("listener received machine=%s source=%s topic=%s event_id=%s", cfg.MachineID, item.Source, item.Topic, item.EventID)
		if strings.HasPrefix(item.Topic, contracts.AgentTopicPrefix) {
			sessionID := strings.TrimPrefix(item.Topic, contracts.AgentTopicPrefix)
			if dedupeCache.Seen(item.DedupeKey, sessionID) {
				log.Printf("listener dedupe skip session=%s dedupe_key=%s", sessionID, item.DedupeKey)
				_ = msg.Ack()
				return
			}
			if attemptCache.Seen(item.DedupeKey, sessionID) {
				log.Printf("listener attempt-dedupe skip session=%s dedupe_key=%s", sessionID, item.DedupeKey)
				_ = msg.Ack()
				return
			}
			interest, err := registry.Get(sessionID)
			var interestPtr *store.Interest
			if err == nil {
				interestPtr = &interest
			}
			attemptCache.Record(item.DedupeKey, sessionID)
			result := session.HandleAgentMessage(item, sessionID, cfg.MachineID, interestPtr, &deliver)
			if result.Err != nil {
				log.Printf("listener agent delivery failed session=%s: %v", sessionID, result.Err)
			}
			if result.Delivered {
				dedupeCache.Record(item.DedupeKey, sessionID)
				log.Printf("listener agent delivered session=%s event_id=%s", sessionID, item.EventID)
			} else if result.Err == nil {
				log.Printf("listener agent session not found anywhere session=%s", sessionID)
			}
			if result.ShouldNAK {
				_ = msg.NakWithDelay(30 * time.Second)
			} else {
				_ = msg.Ack()
			}
			return
		}
		items := registry.Match(cfg.MachineID, item.Topic)
		if len(items) == 0 {
			log.Printf("listener no matching interests for topic=%s", item.Topic)
			_ = msg.Ack()
			return
		}
		var failed bool
		for _, interest := range items {
			if dedupeCache.Seen(item.DedupeKey, interest.SessionID) {
				log.Printf("listener dedupe skip session=%s dedupe_key=%s", interest.SessionID, item.DedupeKey)
				continue
			}
			if attemptCache.Seen(item.DedupeKey, interest.SessionID) {
				log.Printf("listener attempt-dedupe skip session=%s dedupe_key=%s", interest.SessionID, item.DedupeKey)
				continue
			}
			if item.SourceSession != "" && item.SourceSession == interest.SessionID {
				log.Printf("listener skip echo session=%s topic=%s", interest.SessionID, item.Topic)
				continue
			}
			attemptCache.Record(item.DedupeKey, interest.SessionID)
			if err := deliver.Deliver(item, interest); err != nil {
				log.Printf("listener delivery failed session=%s: %v", interest.SessionID, err)
				failed = true
			} else {
				dedupeCache.Record(item.DedupeKey, interest.SessionID)
			}
		}
		if failed {
			_ = msg.NakWithDelay(30 * time.Second)
		} else {
			_ = msg.Ack()
		}
	}, nats.Durable(consumer), nats.DeliverNew(), nats.AckExplicit(), nats.ManualAck(), nats.AckWait(60*time.Second), nats.MaxAckPending(256), nats.MaxDeliver(20))
	if err != nil {
		log.Fatal(err)
	}

	// Phase 6: Publish initialized state — readiness gate opens for /v1/*.
	deps.Store(&listenerDeps{
		client:   client,
		registry: registry,
		sessions: sessions,
	})
	log.Printf("envoy-listener ready (NATS connected)")

	// Phase 7: Block until fatal error from HTTP server.
	log.Fatal(<-fatal)
}
