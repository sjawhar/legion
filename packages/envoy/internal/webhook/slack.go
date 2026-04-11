package webhook

import (
	"encoding/json"
	"io"
	"log"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/verify"
)

func typeName(value any) string {
	text, _ := value.(string)
	return text
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

// SlackHandler returns the HTTP handler for Slack webhook events.
func SlackHandler(secret string, publisher Publisher) http.HandlerFunc {
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
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		// URL verification must happen BEFORE signature check.
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
				if err := publisher.Publish(item); err != nil {
					log.Printf("slack publish failed: %v", err)
					http.Error(w, "service unavailable", http.StatusServiceUnavailable)
					return
				}
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}
