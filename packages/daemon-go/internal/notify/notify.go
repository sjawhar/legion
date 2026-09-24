// Package notify publishes persisted workflow notices through the Envoy listener.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const requestTimeout = 10 * time.Second

// refusalBodyLimit bounds how much of a refused publish's body the error carries: the listener
// answers `{"error": ...}` (packages/envoy/cmd/listener/api.go writeJSONError).
const refusalBodyLimit = 4096

// Topic is the persisted issue topic workers and architects subscribe to. project is the project
// token panes are told as LEGION_PROJECT (packages/pi-envoy/src/legion/go-bootstrap.ts:154-159),
// never the Dispatch project key.
func Topic(project, issue string) string {
	return "notifications.legion." + project + "." + issue
}

// Publisher delivers a notice to one listener topic.
type Publisher interface {
	Publish(ctx context.Context, topic, message string, payload any, dedupeKey string) error
}

// HTTPPublisher calls the Envoy listener's publish route.
type HTTPPublisher struct {
	baseURL string
	token   string
	client  *http.Client
}

// New creates a publisher for one Envoy listener. token is the listener's bearer
// (`envoy_token_file`), sent on every publish; "" sends none, for a listener that requires none.
func New(baseURL, token string) *HTTPPublisher {
	return &HTTPPublisher{baseURL: strings.TrimRight(baseURL, "/"), token: token, client: &http.Client{Timeout: requestTimeout}}
}

// Publish posts the listener envelope that preserves the outbox row's idempotent delivery key.
func (p *HTTPPublisher) Publish(ctx context.Context, topic, message string, payload any, dedupeKey string) error {
	// The listener carries a payload as a JSON document in a string (its messageBody,
	// packages/envoy/cmd/listener/api.go:68-80); an object there is refused as invalid JSON.
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode notice payload for %s: %w", topic, err)
	}
	body, err := json.Marshal(struct {
		Topic     string `json:"topic"`
		Message   string `json:"message"`
		Payload   string `json:"payload"`
		DedupeKey string `json:"dedupe_key"`
	}{Topic: topic, Message: message, Payload: string(encoded), DedupeKey: dedupeKey})
	if err != nil {
		return fmt.Errorf("encode notice for %s: %w", topic, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/messages/publish", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build notice publish request for %s: %w", topic, err)
	}
	request.Header.Set("Content-Type", "application/json")
	if p.token != "" {
		request.Header.Set("Authorization", "Bearer "+p.token)
	}
	response, err := p.client.Do(request)
	if err != nil {
		return fmt.Errorf("publish notice to %s: %w", topic, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, refusalBodyLimit))
		return fmt.Errorf("publish notice to %s: listener returned %s: %s", topic, response.Status, strings.TrimSpace(string(body)))
	}
	return nil
}
