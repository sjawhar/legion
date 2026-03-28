package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/config"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/verify"
)

func main() {
	cfg, err := config.Load(9011)
	if err != nil {
		log.Fatal(err)
	}
	secret := getenv("ENVOY_SLACK_SIGNING_SECRET")
	client, err := bus.Connect(cfg.NATSURLs)
	if err != nil {
		log.Fatal(err)
	}
	defer client.Conn.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := client.Conn.FlushTimeout(3 * time.Second); err != nil {
			http.Error(w, "nats unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/webhook/slack", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusOK)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if typeName(payload["type"]) == "url_verification" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"challenge": payload["challenge"]})
			return
		}
		if !verify.Slack(secret, body, r.Header.Get("X-Slack-Request-Timestamp"), r.Header.Get("X-Slack-Signature")) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		if typeName(payload["type"]) == "event_callback" && stringValue(payload["event_id"]) != "" {
			items := contracts.SlackEnvelopes(contracts.SlackEnvelopeInput{Body: payload, EventID: id.New(), TraceID: id.New()})
			for _, item := range items {
				if err := item.Validate(); err != nil {
					continue
				}
				if err := client.Publish(item); err != nil {
					log.Printf("slack publish failed: %v", err)
					http.Error(w, "service unavailable", http.StatusServiceUnavailable)
					return
				}
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	log.Printf("envoy-slack listening on %d", cfg.Port)
	server := &http.Server{Addr: addr(cfg.Port), Handler: mux, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	log.Fatal(server.ListenAndServe())
}

func addr(port int) string {
	return ":" + strconv.Itoa(port)
}

func getenv(key string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		log.Fatalf("%s is required", key)
	}
	return value
}

func typeName(value any) string {
	text, _ := value.(string)
	return text
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}
