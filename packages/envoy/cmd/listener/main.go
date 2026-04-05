package main

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
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
)

// listenerDeps holds NATS-dependent resources published atomically after
// initialization completes. HTTP handlers read these via atomic.Pointer to
// avoid data races during the startup window.
type listenerDeps struct {
	client   *bus.Client
	registry *store.Registry
	sessions *session.SessionRegistry
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
		item := contracts.Envelope{
			EventID:        id.New(),
			Source:         "agent",
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

func main() {
	// Phase 1: Load config (synchronous, fast).
	cfg, err := config.Load(9020)
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
		sessionID := strings.TrimPrefix(r.URL.Path, "/v1/interests/")
		d := deps.Load()
		item, err := d.registry.Get(sessionID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(item)
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

	mux.Handle("/v1/", readinessGate(func() bool { return deps.Load() != nil }, v1))

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
	log.Printf("envoy-listener listening on %s", addr)

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

	var sessions *session.SessionRegistry
	if reg, err := session.OpenSessionRegistry(client.Conn, session.WithSessionReplicas(cfg.NATSReplicas)); err != nil {
		log.Printf("WARN: KV registry unavailable, using file-only delivery: %v", err)
	} else {
		sessions = reg
	}

	deliver := session.Deliverer{
		MachineID:   cfg.MachineID,
		RegistryDir: os.Getenv("ENVOY_REGISTRY_DIR"),
		HostBridge:  os.Getenv("ENVOY_HOST_BRIDGE"),
		Sessions:    sessions,
	}

	dedupeCache := dedupe.New(10 * time.Minute)

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
			interest, err := registry.Get(sessionID)
			var interestPtr *store.Interest
			if err == nil {
				interestPtr = &interest
			}
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
			if item.SourceSession != "" && item.SourceSession == interest.SessionID {
				log.Printf("listener skip echo session=%s topic=%s", interest.SessionID, item.Topic)
				continue
			}
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
