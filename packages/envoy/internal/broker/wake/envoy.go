// Package wake is the broker's one best-effort side channel: a POST to Envoy's own
// notifications.agent.<sessionID> topic so a session already watching for wake events knows a
// secret decision landed. Notify never reports failure to its caller and never blocks the
// decision it announces on Envoy being reachable — the client's own status poll is always the
// authority, this is only a nudge to check sooner.
package wake

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
)

// Envoy publishes a "secret request <state>" notification to the target session's wake topic.
type Envoy struct {
	URL   string
	Token string
	HTTP  *http.Client
}

type publishBody struct {
	Topic   string `json:"topic"`
	Message string `json:"message"`
	Payload string `json:"payload"`
	Source  string `json:"source"`
}

type secretRequestPayload struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	State     string `json:"state"`
}

// Notify tells sessionID's wake topic that requestID reached state. Any failure — building the
// request, reaching Envoy, or a non-2xx response — is logged with slog.Warn and otherwise
// ignored: the caller has already applied the decision and this is only a best-effort nudge.
func (e Envoy) Notify(ctx context.Context, sessionID, requestID, state string) {
	payload, err := json.Marshal(secretRequestPayload{
		Type:      "secret-request",
		RequestID: requestID,
		State:     state,
	})
	if err != nil {
		slog.Warn("wake envoy: marshal payload", "session", sessionID, "request", requestID, "error", err)
		return
	}
	body, err := json.Marshal(publishBody{
		Topic:   "notifications.agent." + sessionID,
		Message: "secret request " + state,
		Payload: string(payload),
		Source:  "agent-secrets",
	})
	if err != nil {
		slog.Warn("wake envoy: marshal body", "session", sessionID, "request", requestID, "error", err)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.URL+"/v1/messages/publish", bytes.NewReader(body))
	if err != nil {
		slog.Warn("wake envoy: build request", "session", sessionID, "request", requestID, "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.Token)
	resp, err := e.HTTP.Do(req)
	if err != nil {
		slog.Warn("wake envoy: publish", "session", sessionID, "request", requestID, "error", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Warn("wake envoy: publish", "session", sessionID, "request", requestID, "status", resp.StatusCode, "error", fmt.Sprintf("unexpected status %d", resp.StatusCode))
	}
}
