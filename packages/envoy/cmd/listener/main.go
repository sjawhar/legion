package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/config"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

func main() {
	cfg, err := config.Load(9020)
	if err != nil {
		log.Fatal(err)
	}
	client, err := bus.Connect(cfg.NATSURLs)
	if err != nil {
		log.Fatal(err)
	}
	defer client.Conn.Close()
	registry, err := store.Open(client.Conn)
	if err != nil {
		log.Fatal(err)
	}
	deliver := session.Deliverer{
		RegistryDir: os.Getenv("ENVOY_REGISTRY_DIR"),
		HostBridge:  os.Getenv("ENVOY_HOST_BRIDGE"),
	}

	dedupe := session.NewDedupe(5 * time.Minute)

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
		if dedupe.Check(item.DedupeKey) {
			log.Printf("listener dedupe skip event_id=%s dedupe_key=%s", item.EventID, item.DedupeKey)
			_ = msg.Ack()
			return
		}
		if item.Source == "agent" {
			sessionID := strings.TrimPrefix(item.Topic, "notifications.agent.")
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
		// NAK on any failure retries ALL recipients, causing duplicates for already-delivered ones.
		// Acceptable: notifications are advisory and idempotent.
		var failed bool
		for _, interest := range items {
			if err := deliver.Deliver(item, interest); err != nil {
				log.Printf("listener delivery failed session=%s: %v", interest.SessionID, err)
				failed = true
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

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := client.Conn.FlushTimeout(3 * time.Second); err != nil {
			http.Error(w, "nats unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/v1/interests/subscribe", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			SessionID string   `json:"session_id"`
			Dir       string   `json:"dir"`
			Topics    []string `json:"topics"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		item, err := registry.Upsert(store.Interest{
			SessionID: body.SessionID,
			MachineID: cfg.MachineID,
			Dir:       body.Dir,
		}, append(body.Topics, contracts.AgentSubject(body.SessionID)))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(item)
	})
	mux.HandleFunc("/v1/interests/unsubscribe", func(w http.ResponseWriter, r *http.Request) {
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
		if err := registry.Remove(body.SessionID, body.Topics); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/v1/interests/", func(w http.ResponseWriter, r *http.Request) {
		sessionID := strings.TrimPrefix(r.URL.Path, "/v1/interests/")
		item, err := registry.Get(sessionID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(item)
	})
	mux.HandleFunc("/v1/messages/send", func(w http.ResponseWriter, r *http.Request) {
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
		if err := client.Publish(item); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(item)
	})
	mux.HandleFunc("/v1/messages/publish", func(w http.ResponseWriter, r *http.Request) {
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
		if err := client.Publish(item); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(item)
	})

	log.Printf("envoy-listener listening on 127.0.0.1:%d", cfg.Port)
	server := &http.Server{Addr: "127.0.0.1:" + strconv.Itoa(cfg.Port), Handler: mux, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	log.Fatal(server.ListenAndServe())
}
