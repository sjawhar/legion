package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
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
	client, err := bus.ConnectOwningStream([]string{sharedListenerTestNATSURI(t)}, bus.WithReplicas(1))
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

// A listener built before the KV key check stored role claims, interests, sessions and CI records
// under keys this one refuses to write (testnats.LegacyKeys), and every listener on that NATS reads
// those buckets as it starts. It must start healthy over them and serve the ordinary keys beside
// them: one stored role must not keep every listener from starting.
func TestTheListenerStartsOverKeysAnEarlierListenerStored(t *testing.T) {
	client, err := bus.ConnectOwningStream([]string{sharedListenerTestNATSURI(t)}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect bus: %v", err)
	}
	t.Cleanup(client.Close)
	resetListenerTestState(t, client.Conn)
	t.Cleanup(func() { resetListenerTestState(t, client.Conn); clearKVBucket(t, client.Conn, cistore.Bucket) })
	bucket := func(name string, ttl time.Duration) natsgo.KeyValue {
		t.Helper()
		kv, err := client.JS().KeyValue(name)
		if errors.Is(err, natsgo.ErrBucketNotFound) {
			kv, err = client.JS().CreateKeyValue(&natsgo.KeyValueConfig{Bucket: name, TTL: ttl, Storage: natsgo.FileStorage})
		}
		if err != nil {
			t.Fatalf("open bucket %s: %v", name, err)
		}
		return kv
	}
	put := func(kv natsgo.KeyValue, key string, value any) {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("encode %s value: %v", kv.Bucket(), err)
		}
		if _, err := kv.Put(key, data); err != nil {
			t.Fatalf("store the %d-byte %s key: %v", len(key), kv.Bucket(), err)
		}
	}
	now := time.Now().UnixMilli()
	roles, interests := bucket(store.RoleBucket, 0), bucket(store.Bucket, 0)
	sessions, records := bucket(session.SessionBucket, 5*time.Minute), bucket(cistore.Bucket, 0)
	put(roles, "reviewer", store.RoleClaim{HolderSessionID: "ses_live", ClaimedAt: now})
	put(interests, "ses_live", store.Interest{SessionID: "ses_live", MachineID: "startup-legacy", Topics: []string{contracts.AgentSubject("ses_live")}, UpdatedAt: now})
	put(sessions, "ses_live", session.SessionEntry{Port: 1, MachineID: "startup-legacy", UpdatedAt: now})
	for _, kv := range []natsgo.KeyValue{roles, interests, sessions, records} {
		readable, unreadable := testnats.LegacyKeys(kv.Bucket(), "k")
		for _, key := range []string{readable, unreadable} {
			switch kv {
			case roles:
				put(kv, key, store.RoleClaim{HolderSessionID: "ses_gone", ClaimedAt: now})
			case interests:
				put(kv, key, store.Interest{SessionID: key, MachineID: "earlier", UpdatedAt: now})
			case sessions:
				put(kv, key, session.SessionEntry{Port: 2, MachineID: "earlier", UpdatedAt: now})
			default:
				put(kv, key, cistore.State{Owner: "acme", Repo: "widgets", Number: "7", SHA: strings.Repeat("c", 40)})
			}
		}
	}

	listener := startListenerProcess(t, buildListener(t), client.Conn.ConnectedUrl(), "startup-legacy")
	listener.waitHealthy(t)
	request, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(listener.port)+"/v1/roles/reviewer", nil)
	if err != nil {
		t.Fatalf("role request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+listenerTestToken)
	if status, answer := do(t, request); status != http.StatusOK || !strings.Contains(answer, `"holder":"ses_live"`) {
		t.Fatalf("GET /v1/roles/reviewer beside the earlier listener's keys: status %d %q, want 200 naming ses_live\n%s", status, answer, listener.output.String())
	}
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
