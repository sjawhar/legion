// Package wake is the broker's one best-effort side channel: a POST to Envoy's
// notifications.agent.<sessionID> subject via /v1/messages/send so a session already watching
// for wake events knows a secret decision landed. /v1/messages/publish rejects any
// notifications.agent.* topic outright (cmd/listener/api.go's publishHandler:
// "cannot publish to agent topics; use /v1/messages/send for direct agent messages"), so this
// targets the session directly with target_session instead of publishing to that topic. The
// envelope's source is "envoy" — the platform's own enum (contracts.Envelope.Validate) has no
// "agent-secrets" entry, and the broker is a backend service speaking on the platform's behalf
// here, not an agent session, so "agent" would misrepresent it. Notify never reports failure to
// its caller and never blocks the decision it announces on Envoy being reachable — the client's
// own status poll is always the authority, this is only a nudge to check sooner.
package wake

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
)

// Envoy sends a "secret request <state>" notification directly to the target session.
type Envoy struct {
	URL   string
	Token string
	HTTP  *http.Client
}

type sendBody struct {
	TargetSession string `json:"target_session"`
	Message       string `json:"message"`
	Payload       string `json:"payload"`
	Source        string `json:"source"`
}

type secretRequestPayload struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	State     string `json:"state"`
}

// Notify tells sessionID that requestID reached state. Any failure — building the request,
// reaching Envoy, or a non-2xx response (including a 404 for a session that is no longer live) —
// is logged with slog.Warn and otherwise ignored: the caller has already applied the decision
// and this is only a best-effort nudge.
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
	body, err := json.Marshal(sendBody{
		TargetSession: sessionID,
		Message:       "secret request " + state,
		Payload:       string(payload),
		Source:        "envoy",
	})
	if err != nil {
		slog.Warn("wake envoy: marshal body", "session", sessionID, "request", requestID, "error", err)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.URL+"/v1/messages/send", bytes.NewReader(body))
	if err != nil {
		slog.Warn("wake envoy: build request", "session", sessionID, "request", requestID, "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.Token)
	resp, err := e.HTTP.Do(req)
	if err != nil {
		slog.Warn("wake envoy: send", "session", sessionID, "request", requestID, "error", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Warn("wake envoy: send", "session", sessionID, "request", requestID, "status", resp.StatusCode, "error", fmt.Sprintf("unexpected status %d", resp.StatusCode))
	}
}
