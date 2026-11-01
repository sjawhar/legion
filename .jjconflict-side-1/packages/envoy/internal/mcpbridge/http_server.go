package mcpbridge

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

// HTTPServer connects to a pre-existing MCP server over HTTP/SSE transport.
type HTTPServer struct {
	cfg        ServerConfig
	session    *session
	messageURL string
	sseResp    *http.Response
	state      atomic.Int32
	onNotify   func(uri string)
	stopCh     chan struct{}
	closeOnce  sync.Once
	exitCh     chan error
	stopped    atomic.Bool
}

// NewHTTPServer creates an HTTP/SSE server from config. Call Start() to connect.
func NewHTTPServer(cfg ServerConfig, onNotify func(uri string)) *HTTPServer {
	s := &HTTPServer{
		cfg:      cfg,
		onNotify: onNotify,
		stopCh:   make(chan struct{}),
		exitCh:   make(chan error, 1),
	}
	s.state.Store(int32(StateStarting))
	return s
}

// Name returns the server's configured name.
func (s *HTTPServer) Name() string {
	return s.cfg.Name
}

// State returns the current server state.
func (s *HTTPServer) State() ServerState {
	return ServerState(s.state.Load())
}

// Stopped returns true if Stop() was called intentionally (as opposed to SSE failure).
func (s *HTTPServer) Stopped() bool {
	return s.stopped.Load()
}

// Start connects to the MCP server's SSE endpoint, performs the handshake, and subscribes.
func (s *HTTPServer) Start() error {
	// 1. Connect to {url}/sse.
	sseURL := strings.TrimRight(s.cfg.URL, "/") + "/sse"
	req, err := http.NewRequest("GET", sseURL, nil)
	if err != nil {
		return fmt.Errorf("create SSE request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("SSE connect to %s: %w", sseURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return fmt.Errorf("SSE connect to %s: status %d", sseURL, resp.StatusCode)
	}
	s.sseResp = resp

	// 2. Read the first SSE event (type "endpoint") to get the message POST URL.
	scanner := bufio.NewScanner(resp.Body)
	messageURL, err := readEndpointEvent(scanner)
	if err != nil {
		resp.Body.Close()
		return fmt.Errorf("read endpoint event: %w", err)
	}
	s.messageURL = messageURL

	// 3. Create session with HTTP POST send function.
	s.session = newSession(s.cfg.Name, func(data []byte) error {
		postResp, postErr := http.Post(s.messageURL, "application/json", bytes.NewReader(data))
		if postErr != nil {
			return fmt.Errorf("POST to %s: %w", s.messageURL, postErr)
		}
		defer postResp.Body.Close()
		if postResp.StatusCode >= 400 {
			body, _ := io.ReadAll(postResp.Body)
			return fmt.Errorf("POST to %s: status %d: %s", s.messageURL, postResp.StatusCode, body)
		}
		return nil
	}, s.onNotify, s.stopCh)

	// 4. Start read loop for SSE events.
	go s.readLoop(scanner)

	// 5. Perform MCP handshake.
	if err := s.session.initialize(); err != nil {
		s.closeSSE()
		return fmt.Errorf("initialize %s: %w", s.cfg.Name, err)
	}

	// 6. Subscribe to configured resources.
	for _, uri := range s.cfg.Resources {
		if err := s.session.subscribe(uri); err != nil {
			s.closeSSE()
			return fmt.Errorf("subscribe %s to %s: %w", s.cfg.Name, uri, err)
		}
	}

	s.state.Store(int32(StateReady))
	log.Printf("mcp-bridge: server %s ready (http)", s.cfg.Name)
	return nil
}

// Stop gracefully shuts down the SSE connection.
func (s *HTTPServer) Stop() {
	s.stopped.Store(true)
	s.closeOnce.Do(func() { close(s.stopCh) })
	s.state.Store(int32(StateDead))
	s.closeSSE()
}

// WaitForExit blocks until the SSE connection is lost or intentionally closed.
func (s *HTTPServer) WaitForExit() error {
	return <-s.exitCh
}

// ReadResource calls resources/read via HTTP POST and returns the content.
func (s *HTTPServer) ReadResource(uri string) ([]resourceContent, error) {
	return s.session.readResource(uri)
}

func (s *HTTPServer) closeSSE() {
	if s.sseResp != nil {
		s.sseResp.Body.Close()
	}
}

func (s *HTTPServer) readLoop(scanner *bufio.Scanner) {
	for {
		eventType, data, err := readSSEEvent(scanner)
		if err != nil {
			s.state.Store(int32(StateDead))
			if s.stopped.Load() {
				s.exitCh <- nil
			} else {
				s.exitCh <- err
			}
			return
		}
		if eventType == "message" && len(data) > 0 {
			s.session.handleMessage(data)
		}
	}
}

// readEndpointEvent reads SSE events until it finds one with type "endpoint".
func readEndpointEvent(scanner *bufio.Scanner) (string, error) {
	for {
		eventType, data, err := readSSEEvent(scanner)
		if err != nil {
			return "", fmt.Errorf("reading SSE: %w", err)
		}
		if eventType == "endpoint" {
			u := strings.TrimSpace(string(data))
			if u == "" {
				return "", fmt.Errorf("empty endpoint URL in SSE event")
			}
			return u, nil
		}
	}
}

// readSSEEvent reads a single SSE event from the scanner.
// Returns event type (defaults to "message") and data payload.
func readSSEEvent(scanner *bufio.Scanner) (string, []byte, error) {
	var eventType string
	var dataLines [][]byte

	for scanner.Scan() {
		line := scanner.Text()

		// Empty line marks end of event.
		if line == "" {
			if len(dataLines) > 0 || eventType != "" {
				data := bytes.Join(dataLines, []byte("\n"))
				if eventType == "" {
					eventType = "message"
				}
				return eventType, data, nil
			}
			continue
		}

		if strings.HasPrefix(line, "event: ") {
			eventType = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			dataLines = append(dataLines, []byte(strings.TrimPrefix(line, "data: ")))
		} else if line == "data:" {
			dataLines = append(dataLines, []byte{})
		} else if strings.HasPrefix(line, ":") {
			// Comment, ignore.
		}
	}

	if err := scanner.Err(); err != nil {
		return "", nil, err
	}
	return "", nil, io.EOF
}
