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
			if team, channel, bot, subtype, ok := botMention(payload); ok {
				log.Printf("slack app_mention from a bot dropped team=%s channel=%s bot_id=%s subtype=%s event_id=%s",
					team, channel, bot, subtype, stringValue(payload["event_id"]))
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("ok"))
				return
			}
			items := contracts.SlackEnvelopes(contracts.SlackEnvelopeInput{Body: payload, EventID: id.New(), TraceID: id.New()})
			for _, item := range items {
				if err := item.Validate(); err != nil {
					continue
				}
				if err := publisher.Publish(item); err != nil {
					deliveryFailed(w, "slack publish", err)
					return
				}
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}

// botMention reports an event_callback whose event is an app_mention a bot posted (it carries
// bot_id, or the subtype bot_message), with its team, channel, bot id and subtype. Such a mention is
// never published: the controller answers mentions, and answering a bot that answers it never ends.
func botMention(payload map[string]any) (team, channel, bot, subtype string, ok bool) {
	event, _ := payload["event"].(map[string]any)
	if typeName(event["type"]) != "app_mention" {
		return "", "", "", "", false
	}
	bot, subtype = stringValue(event["bot_id"]), stringValue(event["subtype"])
	if bot == "" && subtype != "bot_message" {
		return "", "", "", "", false
	}
	return stringValue(payload["team_id"]), stringValue(event["channel"]), bot, subtype, true
}
