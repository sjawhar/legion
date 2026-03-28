package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/config"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/verify"
)

func main() {
	cfg, err := config.Load(9010)
	if err != nil {
		log.Fatal(err)
	}
	secret := getenv("ENVOY_GITHUB_WEBHOOK_SECRET")
	client, err := bus.Connect(cfg.NATSURLs)
	if err != nil {
		log.Fatal(err)
	}
	defer client.Conn.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/webhook/github", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusOK)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		delivery := r.Header.Get("X-GitHub-Delivery")
		event := r.Header.Get("X-GitHub-Event")
		signature := r.Header.Get("X-Hub-Signature-256")
		if delivery == "" || event == "" {
			http.Error(w, "missing github headers", http.StatusBadRequest)
			return
		}
		if !verify.Github(secret, body, signature) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		item := contracts.GithubEnvelope(contracts.GithubEnvelopeInput{
			Event:    event,
			Delivery: delivery,
			Body:     payload,
			EventID:  id.New(),
			TraceID:  id.New(),
		})
		if err := item.Validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := client.Publish(item); err != nil {
			log.Printf("github publish failed: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	log.Printf("envoy-github listening on %d", cfg.Port)
	log.Fatal(http.ListenAndServe(addr(cfg.Port), mux))
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
