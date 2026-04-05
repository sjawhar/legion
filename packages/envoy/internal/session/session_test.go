package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func writeRegistryEntry(t *testing.T, dir string, pid, port int, sessionID string) {
	t.Helper()
	raw := fmt.Sprintf(`{"pid":%d,"port":%d,"dir":"/test","session":{"id":"%s","title":"test"}}`, pid, port, sessionID)
	if err := os.WriteFile(filepath.Join(dir, sessionID+".json"), []byte(raw), 0644); err != nil {
		t.Fatal(err)
	}
}

func newDeliverer(dir string) Deliverer {
	return Deliverer{
		RegistryDir:  dir,
		HostBridge:   "127.0.0.1",
		RequestLimit: 5 * time.Second,
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

	dir := t.TempDir()
	writeRegistryEntry(t, dir, 12345, port, "ses_target")

	deliverer := newDeliverer(dir)
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
	dir := t.TempDir()
	deliverer := newDeliverer(dir)
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
	dir := t.TempDir()
	writeRegistryEntry(t, dir, 1, port, "ses_target")

	deliverer := newDeliverer(dir)
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
	dir := t.TempDir()
	writeRegistryEntry(t, dir, 1, port, "ses_target")

	deliverer := newDeliverer(dir)
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
	dir := t.TempDir()
	writeRegistryEntry(t, dir, 1, port, "ses_target")

	deliverer := newDeliverer(dir)
	item := newTestEnvelope("agent", "notifications.agent.ses_target", "test")

	err := deliverer.Deliver(item, store.Interest{SessionID: "ses_target", Dir: "/test", MachineID: "m"})
	if err == nil {
		t.Fatal("expected error for 500 response, got nil")
	}
}

// TestFind_SessionIDKeyedFile verifies direct O(1) lookup by session ID.
func TestFind_SessionIDKeyedFile(t *testing.T) {
	dir := t.TempDir()
	writeRegistryEntry(t, dir, 999, 12345, "ses_direct")

	deliverer := newDeliverer(dir)
	entry, err := deliverer.Find("ses_direct")
	if err != nil {
		t.Fatalf("expected to find session, got: %v", err)
	}
	if entry.Port != 12345 {
		t.Fatalf("expected port 12345, got %d", entry.Port)
	}
}

func TestFind_NotFound(t *testing.T) {
	dir := t.TempDir()
	deliverer := newDeliverer(dir)
	_, err := deliverer.Find("ses_nonexistent")
	if err == nil {
		t.Fatal("expected error for missing session")
	}
}

func TestDeliver_KVFirstOverFile(t *testing.T) {
	// Set up a mock server for the "correct" port (from KV)
	var kvDeliveries atomic.Int32
	kvMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kvDeliveries.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer kvMock.Close()
	kvPort := mockPort(kvMock.URL)

	// Set up a mock server for the "stale" port (from file)
	var fileDeliveries atomic.Int32
	fileMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fileDeliveries.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer fileMock.Close()
	filePort := mockPort(fileMock.URL)

	// Write file registry with stale port
	dir := t.TempDir()
	writeRegistryEntry(t, dir, 1, filePort, "ses_target")

	// Set up KV registry with correct port (setupNATS is in registry_test.go, same package)
	client := setupNATS(t)
	sessions, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(10*time.Second))
	if err != nil {
		t.Fatalf("failed to open session registry: %v", err)
	}
	sessions.Put("ses_target", SessionEntry{Port: kvPort, MachineID: "test", Dir: "/test"})

	deliverer := Deliverer{
		MachineID:    "test",
		RegistryDir:  dir,
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

	// Verify KV port was used, NOT file port
	if kvDeliveries.Load() != 1 {
		t.Fatalf("expected 1 delivery to KV port, got %d", kvDeliveries.Load())
	}
	if fileDeliveries.Load() != 0 {
		t.Fatalf("expected 0 deliveries to file port, got %d", fileDeliveries.Load())
	}
}

func TestDeliver_NilSessionsFallsBackToFile(t *testing.T) {
	var deliveryCount atomic.Int32

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deliveryCount.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer mock.Close()

	port := mockPort(mock.URL)
	dir := t.TempDir()
	writeRegistryEntry(t, dir, 1, port, "ses_target")

	deliverer := Deliverer{
		RegistryDir:  dir,
		HostBridge:   "127.0.0.1",
		RequestLimit: 5 * time.Second,
		Sessions:     nil, // KV unavailable
	}
	item := newTestEnvelope("agent", "notifications.agent.ses_target", "test message")
	interest := store.Interest{SessionID: "ses_target", Dir: "/test", MachineID: "m"}

	err := deliverer.Deliver(item, interest)
	if err != nil {
		t.Fatalf("expected file-only delivery to succeed, got: %v", err)
	}
	if count := deliveryCount.Load(); count != 1 {
		t.Fatalf("expected exactly 1 delivery via file fallback, got %d", count)
	}
}

func TestDeliver_WrongMachineReturnsError(t *testing.T) {
	var deliveryCount atomic.Int32

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deliveryCount.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer mock.Close()

	port := mockPort(mock.URL)
	dir := t.TempDir()
	writeRegistryEntry(t, dir, 1, port, "ses_target")

	client := setupNATS(t)
	sessions, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(10*time.Second))
	if err != nil {
		t.Fatalf("failed to open session registry: %v", err)
	}
	// Register session on machine-B
	sessions.Put("ses_target", SessionEntry{Port: port, MachineID: "machine-B", Dir: "/test"})

	// Deliverer is on machine-A
	deliverer := Deliverer{
		MachineID:    "machine-A",
		RegistryDir:  dir,
		HostBridge:   "127.0.0.1",
		RequestLimit: 5 * time.Second,
		Sessions:     sessions,
	}
	item := newTestEnvelope("agent", "notifications.agent.ses_target", "test message")
	interest := store.Interest{SessionID: "ses_target", Dir: "/test", MachineID: "machine-A"}

	err = deliverer.Deliver(item, interest)
	if !errors.Is(err, ErrWrongMachine) {
		t.Fatalf("expected ErrWrongMachine, got: %v", err)
	}
	if count := deliveryCount.Load(); count != 0 {
		t.Fatalf("expected 0 deliveries (wrong machine), got %d", count)
	}
}

func TestDeliver_WrongMachineSkipsFileFallback(t *testing.T) {
	var fileDeliveries atomic.Int32

	fileMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fileDeliveries.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer fileMock.Close()

	filePort := mockPort(fileMock.URL)
	dir := t.TempDir()
	writeRegistryEntry(t, dir, 1, filePort, "ses_target")

	client := setupNATS(t)
	sessions, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(10*time.Second))
	if err != nil {
		t.Fatalf("failed to open session registry: %v", err)
	}
	// KV says session is on machine-B
	sessions.Put("ses_target", SessionEntry{Port: 9999, MachineID: "machine-B", Dir: "/test"})

	deliverer := Deliverer{
		MachineID:    "machine-A",
		RegistryDir:  dir,
		HostBridge:   "127.0.0.1",
		RequestLimit: 5 * time.Second,
		Sessions:     sessions,
	}
	item := newTestEnvelope("agent", "notifications.agent.ses_target", "test message")
	interest := store.Interest{SessionID: "ses_target", Dir: "/test", MachineID: "machine-A"}

	err = deliverer.Deliver(item, interest)
	if !errors.Is(err, ErrWrongMachine) {
		t.Fatalf("expected ErrWrongMachine, got: %v", err)
	}
	// File fallback must NOT have been tried
	if count := fileDeliveries.Load(); count != 0 {
		t.Fatalf("expected 0 file deliveries (KV says wrong machine), got %d", count)
	}
}
