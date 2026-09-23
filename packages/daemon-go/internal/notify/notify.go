// Package notify publishes persisted workflow notices through the Envoy listener.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const requestTimeout = 10 * time.Second

// Topic is the persisted issue topic workers and architects subscribe to.
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
	client  *http.Client
}

// New creates a publisher for one Envoy listener.
func New(baseURL string) *HTTPPublisher {
	return &HTTPPublisher{baseURL: strings.TrimRight(baseURL, "/"), client: &http.Client{Timeout: requestTimeout}}
}

// Publish posts the listener envelope that preserves the outbox row's idempotent delivery key.
func (p *HTTPPublisher) Publish(ctx context.Context, topic, message string, payload any, dedupeKey string) error {
	body, err := json.Marshal(struct {
		Topic     string `json:"topic"`
		Message   string `json:"message"`
		Payload   any    `json:"payload"`
		DedupeKey string `json:"dedupe_key"`
	}{Topic: topic, Message: message, Payload: payload, DedupeKey: dedupeKey})
	if err != nil {
		return fmt.Errorf("encode notice for %s: %w", topic, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/messages/publish", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build notice publish request for %s: %w", topic, err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return fmt.Errorf("publish notice to %s: %w", topic, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("publish notice to %s: listener returned %s", topic, response.Status)
	}
	return nil
}
