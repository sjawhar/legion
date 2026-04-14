package session

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/store"
)

func newTestEnvelope(source, topic, summary string) contracts.Envelope {
	return contracts.Envelope{
		EventID:        "test-event-1",
		Source:         source,
		SourceSession:  "ses_sender",
		SourceEventID:  "test-source-1",
		Topic:          topic,
		DedupeKey:      "test-dedupe-1",
		IssuedAt:       time.Now().UnixMilli(),
		PayloadSummary: summary,
		TraceID:        "test-trace-1",
	}
}

func mockPort(url string) int {
	port := 0
	fmt.Sscanf(url, "http://127.0.0.1:%d", &port)
	if port == 0 {
		fmt.Sscanf(url, "http://[::1]:%d", &port)
	}
	return port
}

func newKVDeliverer(t *testing.T) (*SessionRegistry, Deliverer) {
	t.Helper()
	client := setupNATS(t)
	sessions, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(10*time.Second))
	if err != nil {
		t.Fatalf("failed to open session registry: %v", err)
	}
	return sessions, Deliverer{
		HostBridge:   "127.0.0.1",
		RequestLimit: 5 * time.Second,
		Sessions:     sessions,
	}
}

func TestDeliver_ExactlyOnce(t *testing.T) {
	var deliveryCount atomic.Int32

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deliveryCount.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer mock.Close()

	port := mockPort(mock.URL)
	if port == 0 {
		t.Fatalf("could not parse mock port from %s", mock.URL)
	}

	sessions, deliverer := newKVDeliverer(t)
	if err := sessions.Put("ses_target", SessionEntry{Port: port, Dir: "/test"}); err != nil {
		t.Fatalf("failed to register session: %v", err)
	}
	interest := store.Interest{SessionID: "ses_target", Dir: "/test", MachineID: "m"}
	item := newTestEnvelope("agent", "notifications.agent.ses_target", "test message")

	err := deliverer.Deliver(item, interest)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if count := deliveryCount.Load(); count != 1 {
		t.Fatalf("expected exactly 1 delivery, got %d", count)
	}
}

func TestDeliver_NoRegistryEntry(t *testing.T) {
	_, deliverer := newKVDeliverer(t)
	interest := store.Interest{SessionID: "ses_ghost", Dir: "/test", MachineID: "m"}
	item := newTestEnvelope("agent", "notifications.agent.ses_ghost", "test message")

	err := deliverer.Deliver(item, interest)
	if err == nil {
		t.Fatal("expected error for missing session, got nil")
	}
}

func TestDeliver_PromptAsyncBody(t *testing.T) {
	var receivedBody []byte

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/session/ses_target/prompt_async" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
		}
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		receivedBody = buf[:n]
		w.WriteHeader(http.StatusNoContent)
	}))
	defer mock.Close()

	port := mockPort(mock.URL)
	sessions, deliverer := newKVDeliverer(t)
	if err := sessions.Put("ses_target", SessionEntry{Port: port, Dir: "/test"}); err != nil {
		t.Fatalf("failed to register session: %v", err)
	}
	item := newTestEnvelope("slack", "notifications.slack.T123.C456.mention", "test payload")
	item.SourceSession = "ses_sender_123"

	err := deliverer.Deliver(item, store.Interest{SessionID: "ses_target", Dir: "/test", MachineID: "m"})
	if err != nil {
		t.Fatalf("delivery failed: %v", err)
	}

	var body struct {
		Parts []map[string]string `json:"parts"`
	}
	if err := json.Unmarshal(receivedBody, &body); err != nil {
		t.Fatalf("failed to parse body: %v (raw: %s)", err, string(receivedBody))
	}

	if len(body.Parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(body.Parts))
	}

	text := body.Parts[0]["text"]
	if text == "" {
		t.Fatal("notification text is empty")
	}
	for _, want := range []string{"slack", "ses_sender_123", "test payload", "notifications.slack.T123.C456.mention"} {
		if !strings.Contains(text, want) {
			t.Errorf("notification text missing %q: %s", want, text)
		}
	}
	replyInstruction := `Use envoy_send(target_session="ses_sender_123", message="...") to reply to this message.`
	if !strings.Contains(text, replyInstruction) {
		t.Errorf("notification text missing reply instruction: %s", text)
	}
}

func TestText_WithSourceSession(t *testing.T) {
	deliverer := Deliverer{}
	item := contracts.Envelope{
		EventID:        "evt-1",
		Source:         "agent",
		SourceSession:  "ses_abc",
		Topic:          "notifications.agent.ses_target",
		PayloadSummary: "hello world",
	}
	got := deliverer.Text(item)
	want := "[NOTIFICATION from agent (reply-to: ses_abc)]\nhello world\n\nTopic: notifications.agent.ses_target\nEvent ID: evt-1\nUse envoy_send(target_session=\"ses_abc\", message=\"...\") to reply to this message."
	if got != want {
		t.Errorf("Text() mismatch\ngot:  %q\nwant: %q", got, want)
	}
}

func TestText_WithoutSourceSession(t *testing.T) {
	deliverer := Deliverer{}
	item := contracts.Envelope{
		EventID:        "evt-2",
		Source:         "slack",
		SourceSession:  "",
		Topic:          "notifications.slack.T1.C1.mention",
		PayloadSummary: "no sender",
	}
	got := deliverer.Text(item)
	want := "[NOTIFICATION from slack]\nno sender\n\nTopic: notifications.slack.T1.C1.mention\nEvent ID: evt-2"
	if got != want {
		t.Errorf("Text() mismatch\ngot:  %q\nwant: %q", got, want)
	}
	if strings.Contains(got, "envoy_send") {
		t.Error("Text() should NOT contain reply instruction when SourceSession is empty")
	}
}

func TestText_PrefersPayloadOverSummary(t *testing.T) {
	deliverer := Deliverer{}
	item := contracts.Envelope{
		EventID:        "evt-3",
		Source:         "github",
		SourceSession:  "",
		Topic:          "notifications.github.sjawhar.legion.issue.42.comment",
		PayloadSummary: "truncated summary",
		Payload:        "full payload content that is much longer",
	}
	got := deliverer.Text(item)
	if !strings.Contains(got, "full payload content that is much longer") {
		t.Errorf("Text() should use Payload when set, got: %s", got)
	}
	if strings.Contains(got, "truncated summary") {
		t.Errorf("Text() should NOT use PayloadSummary when Payload is set, got: %s", got)
	}
}

func TestText_FallsBackToSummaryWhenPayloadEmpty(t *testing.T) {
	deliverer := Deliverer{}
	item := contracts.Envelope{
		EventID:        "evt-4",
		Source:         "github",
		SourceSession:  "",
		Topic:          "notifications.github.sjawhar.legion.push",
		PayloadSummary: "summary only",
		Payload:        "",
	}
	got := deliverer.Text(item)
	if !strings.Contains(got, "summary only") {
		t.Errorf("Text() should fall back to PayloadSummary when Payload is empty, got: %s", got)
	}
}

func TestDeliver_NoAgentField(t *testing.T) {
	var receivedBody []byte

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		receivedBody = buf[:n]
		w.WriteHeader(http.StatusNoContent)
	}))
	defer mock.Close()

	port := mockPort(mock.URL)
	sessions, deliverer := newKVDeliverer(t)
	if err := sessions.Put("ses_target", SessionEntry{Port: port, Dir: "/test"}); err != nil {
		t.Fatalf("failed to register session: %v", err)
	}
	item := newTestEnvelope("agent", "notifications.agent.ses_target", "test")

	deliverer.Deliver(item, store.Interest{SessionID: "ses_target", Dir: "/test", MachineID: "m"})

	// The prompt body must NOT contain an "agent" field — that would override the session's agent
	if strings.Contains(string(receivedBody), `"agent"`) {
		t.Errorf("prompt body must not contain agent field: %s", string(receivedBody))
	}
}

func TestDeliver_PromptAsyncFailure(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer mock.Close()

	port := mockPort(mock.URL)
	sessions, deliverer := newKVDeliverer(t)
	if err := sessions.Put("ses_target", SessionEntry{Port: port, Dir: "/test"}); err != nil {
		t.Fatalf("failed to register session: %v", err)
	}
	item := newTestEnvelope("agent", "notifications.agent.ses_target", "test")

	err := deliverer.Deliver(item, store.Interest{SessionID: "ses_target", Dir: "/test", MachineID: "m"})
	if err == nil {
		t.Fatal("expected error for 500 response, got nil")
	}
}

func TestDeliver_KVRegistryUsed(t *testing.T) {
	var kvDeliveries atomic.Int32
	kvMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kvDeliveries.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer kvMock.Close()
	kvPort := mockPort(kvMock.URL)

	client := setupNATS(t)
	sessions, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(10*time.Second))
	if err != nil {
		t.Fatalf("failed to open session registry: %v", err)
	}
	sessions.Put("ses_target", SessionEntry{Port: kvPort, MachineID: "test", Dir: "/test"})

	deliverer := Deliverer{
		MachineID:    "test",
		HostBridge:   "127.0.0.1",
		RequestLimit: 5 * time.Second,
		Sessions:     sessions,
	}
	item := newTestEnvelope("agent", "notifications.agent.ses_target", "test message")
	interest := store.Interest{SessionID: "ses_target", Dir: "/test", MachineID: "m"}

	err = deliverer.Deliver(item, interest)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if kvDeliveries.Load() != 1 {
		t.Fatalf("expected 1 delivery to KV port, got %d", kvDeliveries.Load())
	}
}

func TestDeliver_NilSessionsReturnsError(t *testing.T) {
	deliverer := Deliverer{
		HostBridge:   "127.0.0.1",
		RequestLimit: 5 * time.Second,
		Sessions:     nil, // no registry configured
	}
	item := newTestEnvelope("agent", "notifications.agent.ses_target", "test message")
	interest := store.Interest{SessionID: "ses_target", Dir: "/test", MachineID: "m"}

	err := deliverer.Deliver(item, interest)
	if err == nil {
		t.Fatal("expected error when Sessions is nil, got nil")
	}
	if !strings.Contains(err.Error(), "no session registry configured") {
		t.Fatalf("expected 'no session registry configured' error, got: %v", err)
	}
}

func TestDeliver_CrossMachineUsesRemoteHost(t *testing.T) {
	var deliveryCount atomic.Int32
	var capturedHost string

	// Set up a test server that will receive the remote delivery
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deliveryCount.Add(1)
		capturedHost = r.Host
		w.WriteHeader(http.StatusNoContent)
	}))
	defer remote.Close()

	remotePort := mockPort(remote.URL)
	// Use localhost as the "remote" machine hostname
	remoteHost := "localhost"

	client := setupNATS(t)
	sessions, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(10*time.Second))
	if err != nil {
		t.Fatalf("failed to open session registry: %v", err)
	}
	// Register session on remote machine with localhost hostname
	sessions.Put("ses_target", SessionEntry{Port: remotePort, MachineID: remoteHost, Dir: "/test"})

	// Deliverer is on local machine
	deliverer := Deliverer{
		MachineID:    "local-machine",
		HostBridge:   "127.0.0.1",
		RequestLimit: 5 * time.Second,
		Sessions:     sessions,
	}
	item := newTestEnvelope("agent", "notifications.agent.ses_target", "test message")
	interest := store.Interest{SessionID: "ses_target", Dir: "/test", MachineID: "local-machine"}

	err = deliverer.Deliver(item, interest)
	if err != nil {
		t.Fatalf("expected successful remote delivery, got: %v", err)
	}
	if count := deliveryCount.Load(); count != 1 {
		t.Fatalf("expected 1 delivery to remote host, got %d", count)
	}
	// Verify the request went to the remote machine hostname (localhost)
	if !strings.Contains(capturedHost, remoteHost) {
		t.Fatalf("expected request to remote host %s, got %s", remoteHost, capturedHost)
	}
}

func TestDeliver_DefaultTimeout30s(t *testing.T) {
	d := Deliverer{}
	if got := d.timeout(); got != 30*time.Second {
		t.Fatalf("expected default timeout 30s, got %v", got)
	}
}

func TestDeliver_CustomTimeoutRespected(t *testing.T) {
	d := Deliverer{RequestLimit: 5 * time.Second}
	if got := d.timeout(); got != 5*time.Second {
		t.Fatalf("expected custom timeout 5s, got %v", got)
	}
}
