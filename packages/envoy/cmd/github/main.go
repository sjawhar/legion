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
	cfg, err := config.Load(9010)
	if err != nil {
		log.Fatal(err)
	}
	secret := getenv("ENVOY_GITHUB_WEBHOOK_SECRET")
	trigger := getenvDefault("ENVOY_GITHUB_MENTION_TRIGGER", "@legion")
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
	mux.HandleFunc("/webhook/github", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusOK)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
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
		if githubEvent(event) && contracts.GithubIsBotSender(payload) {
			log.Printf("github skipped bot sender delivery=%s event=%s", delivery, event)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
			return
		}
		if githubSkip(event) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
			return
		}
		items := contracts.GithubEnvelopes(contracts.GithubEnvelopeInput{
			Event:    event,
			Delivery: delivery,
			Body:     payload,
			EventID:  id.New(),
			TraceID:  id.New(),
		}, trigger)
		for _, item := range items {
			if err := item.Validate(); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if err := client.Publish(item); err != nil {
				log.Printf("github publish failed: %v", err)
				http.Error(w, "service unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		if len(items) > 1 {
			log.Printf("github mention detected delivery=%s trigger=%s", delivery, trigger)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	log.Printf("envoy-github listening on %d", cfg.Port)
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

func getenvDefault(key string, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value != "" {
		return value
	}
	return fallback
}

func githubEvent(event string) bool {
	switch event {
	case "issue_comment", "pull_request_review_comment", "pull_request_review":
		return true
	}
	return false
}

func githubSkip(event string) bool {
	switch event {
	case "check_run", "check_suite":
		return true
	}
	return false
}
