package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// During a rolling deploy the replacement listener cannot bind its durable consumer until the task
// it replaces lets go of it, which takes the old task's deregistration and shutdown; the load
// balancer already sends the replacement webhooks by then. A webhook needs NATS and the CI store,
// not the durable, so the replacement serves it: GitHub does not redeliver a refused delivery on
// its own. /v1 still waits for every dependency.
func TestAWebhookIsServedWhileAnotherTaskHoldsTheDurable(t *testing.T) {
	const (
		machineID = "webhook-bind-window"
		secret    = "bind-window-secret"
		delivery  = "delivery-bind-window"
	)
	client, err := bus.Connect([]string{sharedListenerTestNATSURI(t)}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect bus: %v", err)
	}
	t.Cleanup(client.Close)
	consumer := "listener-" + machineID
	_ = client.JS().DeleteConsumer(bus.Stream, consumer)
	t.Cleanup(func() { _ = client.JS().DeleteConsumer(bus.Stream, consumer) })
	config := natsgo.ConsumerConfig{Durable: consumer, DeliverSubject: natsgo.NewInbox()}
	applyListenerConsumerPolicy(&config, bus.StreamSubjects())
	if _, err := client.JS().AddConsumer(bus.Stream, &config); err != nil {
		t.Fatalf("add the durable: %v", err)
	}
	// The task being replaced: it holds the durable's push binding until it stops.
	holder, err := client.JS().Subscribe("", func(msg *natsgo.Msg) { _ = msg.Ack() }, natsgo.Bind(bus.Stream, consumer), natsgo.ManualAck())
	if err != nil {
		t.Fatalf("bind the durable as the old task: %v", err)
	}

	listener := startListenerProcess(t, buildListener(t), client.Conn.ConnectedUrl(), machineID,
		"ENVOY_WEBHOOKS=github", "ENVOY_GITHUB_WEBHOOK_SECRET="+secret, "ENVOY_REVIEWER_APP_ID=1")
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(listener.output.String(), "subscribe failed, retrying") {
		select {
		case <-listener.exited:
			t.Fatalf("the listener exited before it met the bound durable:\n%s", listener.output.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the listener never met the bound durable:\n%s", listener.output.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	body := []byte(`{"ref":"refs/heads/main","after":"` + strings.Repeat("b", 40) + `","before":"` + strings.Repeat("a", 40) + `",` +
		`"pusher":{"name":"example-author"},"sender":{"login":"example-author","type":"User"},` +
		`"repository":{"name":"bind-window","owner":{"login":"acme"},"full_name":"acme/bind-window"}}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	request, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(listener.port)+"/webhook/github", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("webhook request: %v", err)
	}
	request.Header.Set("X-GitHub-Delivery", delivery)
	request.Header.Set("X-GitHub-Event", "push")
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	status, answer := do(t, request)
	if status != http.StatusOK {
		t.Fatalf("webhook while another task holds the durable: status %d %q, want 200\n%s", status, answer, listener.output.String())
	}
	message, err := client.JS().GetLastMsg(bus.Stream, "notifications.github.acme.bind-window.push.branch.main")
	if err != nil {
		t.Fatalf("the accepted push is not on the stream: %v", err)
	}
	var envelope contracts.Envelope
	if err := json.Unmarshal(message.Data, &envelope); err != nil {
		t.Fatalf("decode the published push: %v", err)
	}
	if envelope.SourceEventID != delivery {
		t.Fatalf("the stream's last push is delivery %q, want %q", envelope.SourceEventID, delivery)
	}

	sessions, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(listener.port)+"/v1/sessions", nil)
	if err != nil {
		t.Fatalf("sessions request: %v", err)
	}
	sessions.Header.Set("Authorization", "Bearer "+listenerTestToken)
	if status, answer := do(t, sessions); status != http.StatusServiceUnavailable || !strings.Contains(answer, "service starting") {
		t.Fatalf("/v1 before the durable binds: status %d %q, want 503 service starting", status, answer)
	}

	if err := holder.Unsubscribe(); err != nil {
		t.Fatalf("release the durable as the old task: %v", err)
	}
	listener.waitHealthy(t)
}

// do sends request and returns its status and body.
func do(t *testing.T, request *http.Request) (int, string) {
	t.Helper()
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", request.Method, request.URL.Path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("%s %s: read the answer: %v", request.Method, request.URL.Path, err)
	}
	return response.StatusCode, string(answer)
}
