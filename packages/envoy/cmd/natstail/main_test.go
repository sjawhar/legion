package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/testcontainers/testcontainers-go"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
)

func TestTailPrintsEnvelopesOnTheSubject(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	container, err := tcnats.Run(ctx, "nats:2.10", testcontainers.WithCmd("-DV", "-js", "-m", "8222"))
	if err != nil {
		t.Fatalf("start NATS: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })
	url, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("get NATS connection string: %v", err)
	}

	publisher, err := bus.Connect([]string{url})
	if err != nil {
		t.Fatalf("connect publisher: %v", err)
	}
	t.Cleanup(publisher.Close)

	var output bytes.Buffer
	result := make(chan error, 1)
	go func() {
		result <- tail(ctx, []string{url}, "notifications.dispatch.document.T.d.>", 2, &output)
	}()
	waitForNATSSubscription(t, ctx, container, "notifications.dispatch.document.T.d.>")
	for _, envelope := range []contracts.Envelope{
		{EventID: "artifact-1", Topic: "notifications.dispatch.document.T.d.artifact.version"},
		{EventID: "artifact-2", Topic: "notifications.dispatch.document.T.d.artifact.version"},
		{EventID: "issue-1", Topic: "notifications.dispatch.issue.T-1.ask.opened"},
	} {
		if err := publisher.Publish(envelope); err != nil {
			t.Fatalf("publish %q: %v", envelope.EventID, err)
		}
	}

	if err := <-result; err != nil {
		t.Fatalf("tail: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("tail output lines = %d, want 2: %q", len(lines), output.String())
	}
	for _, line := range lines {
		var envelope contracts.Envelope
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			t.Fatalf("decode output envelope %q: %v", line, err)
		}
		if !strings.HasSuffix(envelope.Topic, ".artifact.version") {
			t.Fatalf("tail topic = %q, want document artifact version", envelope.Topic)
		}
	}
}

func waitForNATSSubscription(t *testing.T, ctx context.Context, container *tcnats.NATSContainer, subject string) {
	t.Helper()
	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("get NATS host: %v", err)
	}
	port, err := container.MappedPort(ctx, "8222/tcp")
	if err != nil {
		t.Fatalf("get NATS monitoring port: %v", err)
	}
	endpoint := "http://" + net.JoinHostPort(host, port.Port()) + "/subsz?subs=1"
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			t.Fatalf("create NATS subscription request: %v", err)
		}
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			var status struct {
				Subscriptions []struct {
					Subject string `json:"subject"`
				} `json:"subscriptions_list"`
			}
			err = json.NewDecoder(response.Body).Decode(&status)
			_ = response.Body.Close()
			if err == nil {
				for _, subscription := range status.Subscriptions {
					if subscription.Subject == subject {
						return
					}
				}
			}
		}
		lastErr = err
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("NATS never registered subscription %q at %s: %v", subject, endpoint, lastErr)
}
