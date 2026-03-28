package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

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
		OpencodeBin: os.Getenv("ENVOY_OPENCODE_BIN"),
		XDGConfig:   os.Getenv("ENVOY_XDG_CONFIG_HOME"),
		XDGData:     os.Getenv("ENVOY_XDG_DATA_HOME"),
		XDGCache:    os.Getenv("ENVOY_XDG_CACHE_HOME"),
	}

	_, err = client.Conn.Subscribe("notifications.>", func(msg *nats.Msg) {
		var item contracts.Envelope
		if err := json.Unmarshal(msg.Data, &item); err != nil {
			log.Printf("listener decode failed: %v", err)
			return
		}
		if err := item.Validate(); err != nil {
			log.Printf("listener invalid envelope: %v", err)
			return
		}
		log.Printf("listener received machine=%s source=%s topic=%s event_id=%s", cfg.MachineID, item.Source, item.Topic, item.EventID)
		if item.Source == "agent" {
			sessionID := strings.TrimPrefix(item.Topic, "notifications.agent.")
			interest, err := registry.Get(sessionID)
			if err == nil && interest.MachineID == cfg.MachineID {
				if err := deliver.Deliver(item, interest); err != nil {
					log.Printf("listener agent delivery failed: %v", err)
				}
				return
			}
			entry, err := deliver.Find(sessionID)
			if err != nil || entry == nil {
				return
			}
			fallback := store.Interest{SessionID: sessionID, Dir: entry.Dir, MachineID: cfg.MachineID}
			if err := deliver.Deliver(item, fallback); err != nil {
				log.Printf("listener agent delivery failed: %v", err)
			}
			return
		}
		items, err := registry.Match(cfg.MachineID, item.Topic)
		if err != nil {
			log.Printf("listener match failed: %v", err)
			return
		}
		for _, interest := range items {
			if err := deliver.Deliver(item, interest); err != nil {
				log.Printf("listener delivery failed session=%s: %v", interest.SessionID, err)
			}
		}
	})
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
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

	log.Printf("envoy-listener listening on %d", cfg.Port)
	log.Fatal(http.ListenAndServe(":"+strconv.Itoa(cfg.Port), mux))
}
