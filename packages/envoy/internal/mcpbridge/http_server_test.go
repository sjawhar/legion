package mcpbridge

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type mockHTTPMCPServer struct {
	server               *httptest.Server
	mu                   sync.Mutex
	sseWriter            http.Flusher
	sseConn              io.Writer
	closeCh              chan struct{}
	notifyAfterSubscribe bool
	failInitialize       bool
}

func newMockHTTPMCPServer(opts ...func(*mockHTTPMCPServer)) *mockHTTPMCPServer {
	m := &mockHTTPMCPServer{
		notifyAfterSubscribe: true,
		closeCh:              make(chan struct{}),
	}
	for _, opt := range opts {
		opt(m)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)

		messageURL := "http://" + r.Host + "/message"
		fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", messageURL)
		flusher.Flush()

		m.mu.Lock()
		m.sseWriter = flusher
		m.sseConn = w
		m.mu.Unlock()

		select {
		case <-r.Context().Done():
		case <-m.closeCh:
		}
	})

	mux.HandleFunc("/message", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}
		var req jsonrpcRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}

		switch req.Method {
		case "initialize":
			if m.failInitialize {
				m.sendSSE("message", fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32600,"message":"initialization rejected"}}`, req.ID))
			} else {
				m.sendSSE("message", fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2024-11-05","capabilities":{"resources":{"subscribe":true}},"serverInfo":{"name":"mock-http","version":"1.0.0"}}}`, req.ID))
			}
		case "notifications/initialized":
			// No response needed.
		case "resources/subscribe":
			m.sendSSE("message", fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{}}`, req.ID))
			if m.notifyAfterSubscribe {
				time.Sleep(50 * time.Millisecond)
				m.sendSSE("message", `{"jsonrpc":"2.0","method":"notifications/resources/updated","params":{"uri":"whatsapp://messages/15551234567/5551234567@s.whatsapp.net"}}`)
			}
		case "resources/read":
			m.sendSSE("message", fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"contents":[{"uri":"whatsapp://messages/15551234567/5551234567@s.whatsapp.net","text":"Hello from HTTP mock"}]}}`, req.ID))
		}
		w.WriteHeader(http.StatusAccepted)
	})

	m.server = httptest.NewServer(mux)
	return m
}

func (m *mockHTTPMCPServer) sendSSE(eventType, data string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sseConn == nil || m.sseWriter == nil {
		return
	}
	fmt.Fprintf(m.sseConn, "event: %s\ndata: %s\n\n", eventType, data)
	m.sseWriter.Flush()
}

func (m *mockHTTPMCPServer) disconnectSSE() {
	select {
	case <-m.closeCh:
	default:
		close(m.closeCh)
	}
}

func (m *mockHTTPMCPServer) close() {
	m.disconnectSSE()
	m.server.Close()
}

func (m *mockHTTPMCPServer) url() string { return m.server.URL }

func httpServerConfig(serverURL string) ServerConfig {
	return ServerConfig{
		Name:          "whatsapp",
		Transport:     "http",
		URL:           serverURL,
		Resources:     []string{"whatsapp://messages/new"},
		Source:        "whatsapp",
		TopicTemplate: "notifications.whatsapp.{phone}.{jid}.message",
		URIPattern:    "whatsapp://messages/(?P<phone>[^/]+)/(?P<jid>.+)",
	}
}

func TestHTTPServer_StartAndStop(t *testing.T) {
	mock := newMockHTTPMCPServer()
	defer mock.close()
	cfg := httpServerConfig(mock.url())
	cfg.validate()

	var notifiedURI string
	var notifyMu sync.Mutex
	s := NewHTTPServer(cfg, func(uri string) {
		notifyMu.Lock()
		notifiedURI = uri
		notifyMu.Unlock()
	})
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Stop()
	if s.State() != StateReady {
		t.Fatalf("expected ready state, got %d", s.State())
	}
	time.Sleep(500 * time.Millisecond)
	notifyMu.Lock()
	uri := notifiedURI
	notifyMu.Unlock()
	if uri == "" {
		t.Fatal("expected notification URI to be set")
	}
	if uri != "whatsapp://messages/15551234567/5551234567@s.whatsapp.net" {
		t.Fatalf("unexpected notified URI: %s", uri)
	}
}

func TestHTTPServer_ReadResource(t *testing.T) {
	mock := newMockHTTPMCPServer()
	defer mock.close()
	cfg := httpServerConfig(mock.url())
	cfg.validate()
	s := NewHTTPServer(cfg, nil)
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Stop()
	contents, err := s.ReadResource("whatsapp://messages/15551234567/5551234567@s.whatsapp.net")
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if len(contents) != 1 {
		t.Fatalf("expected 1 content, got %d", len(contents))
	}
	if contents[0].Text != "Hello from HTTP mock" {
		t.Fatalf("unexpected text: %s", contents[0].Text)
	}
}

func TestHTTPServer_StopSetsStateDead(t *testing.T) {
	mock := newMockHTTPMCPServer()
	defer mock.close()
	cfg := httpServerConfig(mock.url())
	cfg.validate()
	s := NewHTTPServer(cfg, nil)
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	s.Stop()
	if s.State() != StateDead {
		t.Fatalf("expected dead state after stop, got %d", s.State())
	}
	if !s.Stopped() {
		t.Fatal("expected Stopped() to return true after Stop()")
	}
}

func TestHTTPServer_StopDoesNotTriggerReconnect(t *testing.T) {
	mock := newMockHTTPMCPServer()
	defer mock.close()
	cfg := httpServerConfig(mock.url())
	cfg.validate()
	s := NewHTTPServer(cfg, nil)
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	s.Stop()
	err := s.WaitForExit()
	if err != nil {
		t.Fatalf("expected nil error from WaitForExit after Stop, got: %v", err)
	}
}

func TestHTTPServer_SSEDisconnectReturnsError(t *testing.T) {
	mock := newMockHTTPMCPServer()
	cfg := httpServerConfig(mock.url())
	cfg.validate()
	s := NewHTTPServer(cfg, nil)
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	mock.close()
	done := make(chan struct{})
	go func() {
		s.WaitForExit()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("WaitForExit did not return within timeout after SSE disconnect")
	}
	if s.State() != StateDead {
		t.Fatalf("expected dead state after SSE disconnect, got %d", s.State())
	}
	if s.Stopped() {
		t.Fatal("Stopped() should be false — unexpected disconnect, not intentional Stop()")
	}
}

func TestHTTPServer_StartFailsWhenUnreachable(t *testing.T) {
	cfg := httpServerConfig("http://127.0.0.1:1")
	cfg.validate()
	s := NewHTTPServer(cfg, nil)
	err := s.Start()
	if err == nil {
		s.Stop()
		t.Fatal("expected error when connecting to unreachable server")
	}
}

func TestHTTPServer_StartFailsOnHandshakeReject(t *testing.T) {
	mock := newMockHTTPMCPServer(func(m *mockHTTPMCPServer) { m.failInitialize = true })
	defer mock.close()
	cfg := httpServerConfig(mock.url())
	cfg.validate()
	s := NewHTTPServer(cfg, nil)
	err := s.Start()
	if err == nil {
		s.Stop()
		t.Fatal("expected error when handshake is rejected")
	}
	if !strings.Contains(err.Error(), "initialize") {
		t.Fatalf("error should mention initialize: %v", err)
	}
}

func TestHTTPServer_Name(t *testing.T) {
	cfg := httpServerConfig("http://localhost:3456")
	s := NewHTTPServer(cfg, nil)
	if s.Name() != "whatsapp" {
		t.Fatalf("unexpected name: %s", s.Name())
	}
}

func TestNewServer_DispatchesHTTP(t *testing.T) {
	cfg := ServerConfig{Name: "test", Transport: "http", URL: "http://localhost:1234", Source: "test", TopicTemplate: "test.{id}", URIPattern: "test://(?P<id>.+)"}
	s := NewServer(cfg, nil)
	if _, ok := s.(*HTTPServer); !ok {
		t.Fatal("expected NewServer to return *HTTPServer for http transport")
	}
}

func TestNewServer_DispatchesStdio(t *testing.T) {
	cfg := ServerConfig{Name: "test", Transport: "stdio", Command: []string{"echo"}, Source: "test", TopicTemplate: "test.{id}", URIPattern: "test://(?P<id>.+)"}
	s := NewServer(cfg, nil)
	if _, ok := s.(*ManagedServer); !ok {
		t.Fatal("expected NewServer to return *ManagedServer for stdio transport")
	}
}

// SSE parsing tests.

func TestReadSSEEvent_Basic(t *testing.T) {
	scanner := bufio.NewScanner(strings.NewReader("event: endpoint\ndata: http://localhost/message\n\n"))
	eventType, data, err := readSSEEvent(scanner)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if eventType != "endpoint" {
		t.Fatalf("expected 'endpoint', got %q", eventType)
	}
	if string(data) != "http://localhost/message" {
		t.Fatalf("unexpected data: %q", string(data))
	}
}

func TestReadSSEEvent_MessageDefault(t *testing.T) {
	scanner := bufio.NewScanner(strings.NewReader("data: {\"jsonrpc\":\"2.0\"}\n\n"))
	eventType, data, err := readSSEEvent(scanner)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if eventType != "message" {
		t.Fatalf("expected 'message', got %q", eventType)
	}
	if string(data) != `{"jsonrpc":"2.0"}` {
		t.Fatalf("unexpected data: %q", string(data))
	}
}

func TestReadSSEEvent_MultipleDataLines(t *testing.T) {
	scanner := bufio.NewScanner(strings.NewReader("event: message\ndata: line1\ndata: line2\n\n"))
	_, data, err := readSSEEvent(scanner)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(data) != "line1\nline2" {
		t.Fatalf("unexpected data: %q", string(data))
	}
}

func TestReadSSEEvent_Comment(t *testing.T) {
	scanner := bufio.NewScanner(strings.NewReader(": keep-alive\ndata: payload\n\n"))
	_, data, err := readSSEEvent(scanner)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(data) != "payload" {
		t.Fatalf("unexpected data: %q", string(data))
	}
}

func TestReadSSEEvent_EOF(t *testing.T) {
	scanner := bufio.NewScanner(strings.NewReader(""))
	_, _, err := readSSEEvent(scanner)
	if err != io.EOF {
		t.Fatalf("expected EOF, got: %v", err)
	}
}

func TestReadEndpointEvent(t *testing.T) {
	scanner := bufio.NewScanner(strings.NewReader("event: endpoint\ndata: http://localhost:3456/message?sessionId=abc\n\n"))
	u, err := readEndpointEvent(scanner)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u != "http://localhost:3456/message?sessionId=abc" {
		t.Fatalf("unexpected url: %s", u)
	}
}

func TestReadEndpointEvent_SkipsOtherEvents(t *testing.T) {
	scanner := bufio.NewScanner(strings.NewReader("event: other\ndata: something\n\nevent: endpoint\ndata: http://localhost/msg\n\n"))
	u, err := readEndpointEvent(scanner)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u != "http://localhost/msg" {
		t.Fatalf("unexpected url: %s", u)
	}
}
