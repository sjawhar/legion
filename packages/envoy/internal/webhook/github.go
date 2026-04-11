package webhook

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/verify"
)

// githubEvent returns true for event types where sender logging and bot filtering apply.
func githubEvent(event string) bool {
	switch event {
	case "issue_comment", "pull_request_review_comment", "pull_request_review":
		return true
	}
	return false
}

// githubSkip returns true for event types that should NOT be published.
// Currently all events are published.
func githubSkip(_ string) bool {
	return false
}

// githubSenderField extracts a string field from the sender map.
func githubSenderField(payload map[string]any, field string) string {
	sender, ok := payload["sender"].(map[string]any)
	if !ok {
		return fmt.Sprintf("<no sender map: %T>", payload["sender"])
	}
	v, ok := sender[field]
	if !ok {
		return fmt.Sprintf("<missing %s>", field)
	}
	s, ok := v.(string)
	if !ok {
		return fmt.Sprintf("<non-string %s: %T=%v>", field, v, v)
	}
	return s
}

// GitHubHandler returns the HTTP handler for GitHub webhook events.
func GitHubHandler(secret, mentionTrigger string, publisher Publisher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
		if githubEvent(event) {
			log.Printf("github sender login=%s type=%s delivery=%s event=%s",
				githubSenderField(payload, "login"),
				githubSenderField(payload, "type"),
				delivery, event)
			if contracts.GithubIsBotSender(payload) {
				log.Printf("github skipped bot sender delivery=%s event=%s", delivery, event)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("ok"))
				return
			}
		}
		if githubSkip(event) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
			return
		}
		trigger := mentionTrigger
		if trigger == "" {
			trigger = "@legion"
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
			if err := publisher.Publish(item); err != nil {
				log.Printf("github publish failed: %v", err)
				http.Error(w, "service unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		if len(items) > 1 {
			log.Printf("github mention detected delivery=%s trigger=%s", delivery, strings.TrimSpace(trigger))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}
