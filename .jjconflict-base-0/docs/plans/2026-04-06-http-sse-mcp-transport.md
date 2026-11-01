# HTTP/SSE Transport for MCP Bridge — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add HTTP/SSE transport support to the MCP bridge so it can connect to pre-existing MCP servers over HTTP, alongside the existing stdio transport.

**Architecture:** Extract shared JSON-RPC session logic from `ManagedServer` into a composable `session` helper. Introduce a `Server` interface that both `ManagedServer` (stdio) and a new `HTTPServer` (SSE) implement. The bridge operates on the interface, enabling mixed-transport configurations. HTTP transport uses legacy MCP SSE protocol: GET `{url}/sse` for SSE stream, POST to server-provided message endpoint for JSON-RPC requests.

**Tech Stack:** Go, `net/http`, `net/http/httptest`, `bufio.Scanner` for SSE parsing, existing `encoding/json` JSON-RPC types.

**Scope exclusions:** Streamable HTTP transport, auth headers, TLS client certs, `Last-Event-ID` resume, config hot-reload, new MCP features.

---

## File Structure

| File | Action | Responsibility |
|------|--------|---------------|
| `packages/envoy/internal/mcpbridge/session.go` | Create | Shared JSON-RPC request/response correlation, MCP handshake, resource read |
| `packages/envoy/internal/mcpbridge/server.go` | Modify | Add `Server` interface, simplify `ManagedServer` to compose `session` |
| `packages/envoy/internal/mcpbridge/http_server.go` | Create | HTTP/SSE transport: SSE connection, endpoint discovery, POST-based send |
| `packages/envoy/internal/mcpbridge/http_mock_test.go` | Create | Mock HTTP MCP server using `httptest` for SSE + JSON-RPC |
| `packages/envoy/internal/mcpbridge/http_server_test.go` | Create | Tests for HTTPServer lifecycle, notifications, ReadResource, Stop |
| `packages/envoy/internal/mcpbridge/config.go` | Modify | Add `URL` field, HTTP transport validation, default empty transport to `"stdio"` |
| `packages/envoy/internal/mcpbridge/config_test.go` | Modify | Add HTTP config validation tests |
| `packages/envoy/internal/mcpbridge/bridge.go` | Modify | Use `Server` interface, `NewServer` factory, transport-agnostic monitoring |
| `packages/envoy/internal/mcpbridge/bridge_test.go` | Create | Tests for mixed stdio+HTTP bridge, monitoring, health |

---

## Task 1: Extract shared MCP session logic — Independent

Extract the JSON-RPC session handling from `ManagedServer` into a composable `session` struct. This is a pure refactor — all existing tests must pass unchanged.

**Files:**
- Create: `packages/envoy/internal/mcpbridge/session.go`
- Modify: `packages/envoy/internal/mcpbridge/server.go`

- [ ] **Step 1: Create session.go with extracted session struct**

Create `packages/envoy/internal/mcpbridge/session.go`:

```go
package mcpbridge

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// session manages JSON-RPC request/response correlation and MCP handshake.
// Transports provide a sendFn; session handles protocol logic.
type session struct {
	name     string
	nextID   atomic.Int32
	mu       sync.Mutex
	pending  map[int]chan jsonrpcResponse
	sendFn   func([]byte) error
	onNotify func(uri string)
	stopCh   chan struct{}
}

func newSession(name string, sendFn func([]byte) error, onNotify func(uri string), stopCh chan struct{}) *session {
	return &session{
		name:     name,
		pending:  make(map[int]chan jsonrpcResponse),
		sendFn:   sendFn,
		onNotify: onNotify,
		stopCh:   stopCh,
	}
}

func (s *session) nextRequestID() int {
	return int(s.nextID.Add(1))
}

func (s *session) call(method string, params any) (json.RawMessage, error) {
	id := s.nextRequestID()
	ch := make(chan jsonrpcResponse, 1)
	s.mu.Lock()
	s.pending[id] = ch
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()

	var raw json.RawMessage
	if params != nil {
		var err error
		raw, err = json.Marshal(params)
		if err != nil {
			return nil, err
		}
	}

	req := jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  raw,
	}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if err := s.sendFn(data); err != nil {
		return nil, err
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("rpc error %d: %s", resp.Error.Code, resp.Error.Message)
		}
		return resp.Result, nil
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("timeout waiting for response to %s (id=%d)", method, id)
	case <-s.stopCh:
		return nil, fmt.Errorf("server stopped")
	}
}

func (s *session) sendNotification(method string) error {
	data, err := json.Marshal(jsonrpcNotification{
		JSONRPC: "2.0",
		Method:  method,
	})
	if err != nil {
		return err
	}
	return s.sendFn(data)
}

// handleMessage routes an incoming JSON-RPC message to the appropriate handler.
// Call this from the transport's read loop for each complete message received.
func (s *session) handleMessage(resp jsonrpcResponse) {
	// Notification (no ID).
	if resp.ID == nil && resp.Method != "" {
		s.handleNotification(resp)
		return
	}
	// Response to a pending request.
	if resp.ID != nil {
		s.mu.Lock()
		ch, ok := s.pending[*resp.ID]
		s.mu.Unlock()
		if ok {
			ch <- resp
		}
	}
}

func (s *session) handleNotification(resp jsonrpcResponse) {
	if resp.Method != "notifications/resources/updated" {
		return
	}
	var params notificationParams
	if err := json.Unmarshal(resp.Params, &params); err != nil {
		log.Printf("mcp-bridge: %s: invalid notification params: %v", s.name, err)
		return
	}
	if params.URI == "" {
		return
	}
	if s.onNotify != nil {
		go s.onNotify(params.URI)
	}
}

func (s *session) initialize() error {
	_, err := s.call("initialize", initializeParams{
		ProtocolVersion: "2024-11-05",
		Capabilities:    clientCaps{},
		ClientInfo: clientInfoParams{
			Name:    "envoy-mcp-bridge",
			Version: "1.0.0",
		},
	})
	if err != nil {
		return err
	}
	return s.sendNotification("notifications/initialized")
}

func (s *session) subscribe(uri string) error {
	_, err := s.call("resources/subscribe", subscribeParams{URI: uri})
	return err
}

func (s *session) readResource(uri string) ([]resourceContent, error) {
	raw, err := s.call("resources/read", readParams{URI: uri})
	if err != nil {
		return nil, err
	}
	var result readResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("parse read result: %w", err)
	}
	return result.Contents, nil
}
```

- [ ] **Step 2: Update ManagedServer to compose session**

Modify `packages/envoy/internal/mcpbridge/server.go`. Replace the fields and methods that moved to `session` with composition. The `ManagedServer` struct becomes:

```go
package mcpbridge

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// ServerState tracks the readiness of a managed MCP server.
type ServerState int32

const (
	StateStarting ServerState = iota
	StateReady
	StateDead
)

// ManagedServer wraps a child MCP server process with lifecycle management.
type ManagedServer struct {
	cfg       ServerConfig
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	scanner   *bufio.Scanner
	session   *session
	state     atomic.Int32
	stopCh    chan struct{}
	closeOnce sync.Once
}

// NewManagedServer creates a managed server from config. Call Start() to spawn.
func NewManagedServer(cfg ServerConfig, onNotify func(uri string)) *ManagedServer {
	s := &ManagedServer{
		cfg:    cfg,
		stopCh: make(chan struct{}),
	}
	s.state.Store(int32(StateStarting))
	s.session = newSession(cfg.Name, s.send, onNotify, s.stopCh)
	return s
}

// Name returns the server's configured name.
func (s *ManagedServer) Name() string {
	return s.cfg.Name
}

// State returns the current server state.
func (s *ManagedServer) State() ServerState {
	return ServerState(s.state.Load())
}

// Start spawns the child process, performs the MCP handshake, and subscribes to resources.
func (s *ManagedServer) Start() error {
	cmd := exec.Command(s.cfg.Command[0], s.cfg.Command[1:]...)
	cmd.Stderr = os.Stderr

	// Pass configured env vars to child.
	cmd.Env = os.Environ()
	for k, v := range s.cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", s.cfg.Name, err)
	}

	s.cmd = cmd
	s.stdin = stdin
	s.scanner = bufio.NewScanner(stdout)
	s.scanner.Buffer(make([]byte, 1<<20), 1<<20) // 1MB line buffer

	// Start reading responses in background.
	go s.readLoop()

	// Perform MCP handshake.
	if err := s.session.initialize(); err != nil {
		s.kill()
		return fmt.Errorf("initialize %s: %w", s.cfg.Name, err)
	}

	// Subscribe to configured resources.
	for _, uri := range s.cfg.Resources {
		if err := s.session.subscribe(uri); err != nil {
			s.kill()
			return fmt.Errorf("subscribe %s to %s: %w", s.cfg.Name, uri, err)
		}
	}

	s.state.Store(int32(StateReady))
	log.Printf("mcp-bridge: server %s ready", s.cfg.Name)
	return nil
}

// Stop gracefully shuts down the child process.
func (s *ManagedServer) Stop() {
	s.closeOnce.Do(func() { close(s.stopCh) })
	s.state.Store(int32(StateDead))
	if s.stdin != nil {
		s.stdin.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		done := make(chan struct{})
		go func() {
			s.cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			s.cmd.Process.Kill()
		}
	}
}

// WaitForExit blocks until the child process exits.
func (s *ManagedServer) WaitForExit() error {
	if s.cmd == nil {
		return fmt.Errorf("not started")
	}
	return s.cmd.Wait()
}

// ReadResource calls resources/read and returns the content.
func (s *ManagedServer) ReadResource(uri string) ([]resourceContent, error) {
	return s.session.readResource(uri)
}

func (s *ManagedServer) kill() {
	if s.stdin != nil {
		s.stdin.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		s.cmd.Process.Kill()
		s.cmd.Wait()
	}
	s.state.Store(int32(StateDead))
}

func (s *ManagedServer) send(data []byte) error {
	data = append(data, '\n')
	_, err := s.stdin.Write(data)
	return err
}

func (s *ManagedServer) readLoop() {
	for s.scanner.Scan() {
		line := s.scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var resp jsonrpcResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			log.Printf("mcp-bridge: %s: invalid JSON from server: %v", s.cfg.Name, err)
			continue
		}
		s.session.handleMessage(resp)
	}
	if err := s.scanner.Err(); err != nil {
		log.Printf("mcp-bridge: %s: read error: %v", s.cfg.Name, err)
	}
	s.state.Store(int32(StateDead))
}
```

- [ ] **Step 3: Run all existing tests to verify no regression**

Run from `packages/envoy`:
```bash
go test ./internal/mcpbridge/...
```
Expected: `ok github.com/sjawhar/envoy/internal/mcpbridge` — all existing tests pass unchanged.

- [ ] **Step 4: Run lsp_diagnostics on changed files**

Run `lsp_diagnostics` on `packages/envoy/internal/mcpbridge/` — expect zero errors.

- [ ] **Step 5: Commit**

```bash
jj describe -m "refactor: extract shared MCP session handling from ManagedServer"
jj new
```

---

## Task 2: Add Server interface and update Bridge — Depends on: Task 1

Define a `Server` interface that `ManagedServer` implements. Update `Bridge` to use the interface. Add a `NewServer` factory function. Pure refactor — all existing tests pass.

**Files:**
- Modify: `packages/envoy/internal/mcpbridge/server.go` (add interface)
- Modify: `packages/envoy/internal/mcpbridge/bridge.go` (use interface)

- [ ] **Step 1: Add Server interface to server.go**

Add at the top of `packages/envoy/internal/mcpbridge/server.go`, after the `ServerState` constants:

```go
// Server is the interface for MCP server connections.
// Both ManagedServer (stdio) and HTTPServer (SSE) implement this.
type Server interface {
	Start() error
	Stop()
	State() ServerState
	WaitForExit() error
	ReadResource(uri string) ([]resourceContent, error)
	Name() string
}
```

- [ ] **Step 2: Add NewServer factory function**

Add to `packages/envoy/internal/mcpbridge/server.go`:

```go
// NewServer creates the appropriate Server implementation based on transport config.
func NewServer(cfg ServerConfig, onNotify func(uri string)) Server {
	switch cfg.Transport {
	case "http":
		return NewHTTPServer(cfg, onNotify)
	default:
		return NewManagedServer(cfg, onNotify)
	}
}
```

**Note:** This will have a compile error until Task 5 creates `NewHTTPServer`. For now, comment out the `"http"` case or add a placeholder. The implementer should add the http case when HTTPServer is implemented. For this step, just add:

```go
// NewServer creates the appropriate Server implementation based on transport config.
func NewServer(cfg ServerConfig, onNotify func(uri string)) Server {
	// HTTP transport added in Task 5.
	return NewManagedServer(cfg, onNotify)
}
```

- [ ] **Step 3: Update Bridge to use Server interface**

Modify `packages/envoy/internal/mcpbridge/bridge.go`:

1. Change `servers` field type from `[]*ManagedServer` to `[]Server`
2. In `Start()`, use `NewServer()` instead of `NewManagedServer()`
3. In `handleNotification()`, look up by `Name()` method instead of `s.cfg.Name`
4. In `monitor()`, use `NewServer()` for restart

Full updated `bridge.go`:

```go
package mcpbridge

import (
	"log"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
)

// Bridge manages multiple MCP server connections and publishes events to NATS.
type Bridge struct {
	cfg     *Config
	client  *bus.Client
	servers []Server
	mu      sync.Mutex
	stopCh  chan struct{}
}

// NewBridge creates a bridge from config and a NATS client.
func NewBridge(cfg *Config, client *bus.Client) *Bridge {
	return &Bridge{
		cfg:    cfg,
		client: client,
		stopCh: make(chan struct{}),
	}
}

// Start spawns all configured MCP servers and begins processing notifications.
func (b *Bridge) Start() error {
	for i := range b.cfg.Servers {
		serverCfg := b.cfg.Servers[i]
		s := NewServer(serverCfg, b.makeHandler(&serverCfg))
		if err := s.Start(); err != nil {
			// Stop already-started servers.
			for _, started := range b.servers {
				started.Stop()
			}
			return err
		}
		b.mu.Lock()
		b.servers = append(b.servers, s)
		b.mu.Unlock()

		// Monitor this server for unexpected exit.
		go b.monitor(s, &serverCfg)
	}
	return nil
}

// Stop gracefully shuts down all managed servers.
func (b *Bridge) Stop() {
	close(b.stopCh)
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.servers {
		s.Stop()
	}
}

// Healthy returns true if all servers are ready and NATS is flushable.
func (b *Bridge) Healthy() bool {
	if err := b.client.Conn.FlushTimeout(3 * time.Second); err != nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.servers {
		if s.State() != StateReady {
			return false
		}
	}
	return true
}

func (b *Bridge) makeHandler(cfg *ServerConfig) func(uri string) {
	return func(uri string) {
		b.handleNotification(cfg, uri)
	}
}

func (b *Bridge) handleNotification(cfg *ServerConfig, uri string) {
	// Find the server for this config.
	b.mu.Lock()
	var server Server
	for _, s := range b.servers {
		if s.Name() == cfg.Name {
			server = s
			break
		}
	}
	b.mu.Unlock()

	if server == nil {
		log.Printf("mcp-bridge: no server for %s", cfg.Name)
		return
	}

	// Check if URI matches the pattern.
	if _, err := cfg.RenderTopic(uri); err != nil {
		log.Printf("mcp-bridge: %s: URI does not match pattern: %s", cfg.Name, uri)
		return
	}

	// Read the resource content.
	contents, err := server.ReadResource(uri)
	if err != nil {
		log.Printf("mcp-bridge: %s: resources/read failed for %s: %v", cfg.Name, uri, err)
		return
	}

	// Build and publish envelope.
	envelope, err := BuildEnvelope(cfg, uri, contents)
	if err != nil {
		log.Printf("mcp-bridge: %s: envelope construction failed: %v", cfg.Name, err)
		return
	}

	if err := envelope.Validate(); err != nil {
		log.Printf("mcp-bridge: %s: invalid envelope: %v", cfg.Name, err)
		return
	}

	if err := b.client.Publish(envelope); err != nil {
		log.Printf("mcp-bridge: %s: publish failed: %v", cfg.Name, err)
		return
	}

	log.Printf("mcp-bridge: %s: published to %s", cfg.Name, envelope.Topic)
}

func (b *Bridge) monitor(s Server, cfg *ServerConfig) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		// Wait for exit.
		err := s.WaitForExit()

		select {
		case <-b.stopCh:
			return
		default:
		}

		log.Printf("mcp-bridge: %s: server exited: %v", cfg.Name, err)

		// Restart with exponential backoff.
		for {
			select {
			case <-b.stopCh:
				return
			case <-time.After(backoff):
			}

			log.Printf("mcp-bridge: %s: restarting (backoff=%v)", cfg.Name, backoff)
			newServer := NewServer(*cfg, b.makeHandler(cfg))
			if err := newServer.Start(); err != nil {
				log.Printf("mcp-bridge: %s: restart failed: %v", cfg.Name, err)
				backoff = min(backoff*2, maxBackoff)
				continue
			}

			// Replace the old server.
			b.mu.Lock()
			for i, existing := range b.servers {
				if existing == s {
					b.servers[i] = newServer
					break
				}
			}
			b.mu.Unlock()

			s = newServer
			backoff = time.Second
			break
		}
	}
}
```

- [ ] **Step 4: Run all existing tests to verify no regression**

Run from `packages/envoy`:
```bash
go test ./internal/mcpbridge/...
```
Expected: `ok github.com/sjawhar/envoy/internal/mcpbridge` — all existing tests pass.

- [ ] **Step 5: Run lsp_diagnostics**

Run `lsp_diagnostics` on `packages/envoy/internal/mcpbridge/` — expect zero errors.

- [ ] **Step 6: Commit**

```bash
jj describe -m "refactor: add Server interface and update Bridge to use it"
jj new
```

---

## Task 3: Add HTTP transport config validation — Independent

Add config support for `transport: "http"` with a required `url` field. Default empty transport to `"stdio"` for backward compatibility. TDD.

**Files:**
- Modify: `packages/envoy/internal/mcpbridge/config.go`
- Modify: `packages/envoy/internal/mcpbridge/config_test.go`

- [ ] **Step 1: Write failing tests for HTTP config validation**

Add to `packages/envoy/internal/mcpbridge/config_test.go`:

```go
func TestLoadConfig_HTTPTransportValid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{
		"servers": [{
			"name": "whatsapp",
			"transport": "http",
			"url": "http://localhost:3456",
			"resources": ["whatsapp://messages/new"],
			"source": "whatsapp",
			"topic_template": "notifications.whatsapp.{phone}.{jid}.message",
			"uri_pattern": "whatsapp://messages/(?P<phone>[^/]+)/(?P<jid>.+)"
		}]
	}`), 0644)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("expected valid HTTP config: %v", err)
	}
	if cfg.Servers[0].Transport != "http" {
		t.Fatalf("transport = %s, want http", cfg.Servers[0].Transport)
	}
	if cfg.Servers[0].URL != "http://localhost:3456" {
		t.Fatalf("url = %s, want http://localhost:3456", cfg.Servers[0].URL)
	}
}

func TestLoadConfig_HTTPTransportMissingURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{
		"servers": [{
			"name": "test",
			"transport": "http",
			"source": "whatsapp",
			"topic_template": "notifications.whatsapp.{phone}.message",
			"uri_pattern": "whatsapp://(?P<phone>.+)"
		}]
	}`), 0644)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for HTTP transport missing url")
	}
}

func TestLoadConfig_HTTPTransportInvalidURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{
		"servers": [{
			"name": "test",
			"transport": "http",
			"url": "ftp://localhost:3456",
			"source": "whatsapp",
			"topic_template": "notifications.whatsapp.{phone}.message",
			"uri_pattern": "whatsapp://(?P<phone>.+)"
		}]
	}`), 0644)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for invalid URL scheme")
	}
}

func TestLoadConfig_HTTPTransportWithCommand(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{
		"servers": [{
			"name": "test",
			"transport": "http",
			"url": "http://localhost:3456",
			"command": ["echo"],
			"source": "whatsapp",
			"topic_template": "notifications.whatsapp.{phone}.message",
			"uri_pattern": "whatsapp://(?P<phone>.+)"
		}]
	}`), 0644)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for HTTP transport with command")
	}
}

func TestLoadConfig_StdioTransportWithURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{
		"servers": [{
			"name": "test",
			"transport": "stdio",
			"command": ["echo"],
			"url": "http://localhost:3456",
			"source": "whatsapp",
			"topic_template": "notifications.whatsapp.{phone}.message",
			"uri_pattern": "whatsapp://(?P<phone>.+)"
		}]
	}`), 0644)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for stdio transport with url")
	}
}

func TestLoadConfig_DefaultTransportStdio(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{
		"servers": [{
			"name": "whatsapp",
			"command": ["echo", "hello"],
			"resources": ["whatsapp://messages/new"],
			"source": "whatsapp",
			"topic_template": "notifications.whatsapp.{phone}.{jid}.message",
			"uri_pattern": "whatsapp://messages/(?P<phone>[^/]+)/(?P<jid>.+)"
		}]
	}`), 0644)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("expected valid config with default transport: %v", err)
	}
	if cfg.Servers[0].Transport != "stdio" {
		t.Fatalf("transport = %s, want stdio", cfg.Servers[0].Transport)
	}
}

func TestLoadConfig_HTTPSURLValid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{
		"servers": [{
			"name": "secure",
			"transport": "http",
			"url": "https://mcp.example.com",
			"source": "whatsapp",
			"topic_template": "notifications.whatsapp.{phone}.message",
			"uri_pattern": "whatsapp://(?P<phone>.+)"
		}]
	}`), 0644)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("expected valid HTTPS config: %v", err)
	}
	if cfg.Servers[0].URL != "https://mcp.example.com" {
		t.Fatalf("url = %s", cfg.Servers[0].URL)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run from `packages/envoy`:
```bash
go test ./internal/mcpbridge/... -run "TestLoadConfig_HTTP|TestLoadConfig_Default|TestLoadConfig_StdioTransportWithURL"
```
Expected: FAIL — tests fail because config validation rejects `transport: "http"` and doesn't handle empty transport.

- [ ] **Step 3: Update config.go to support HTTP transport**

Modify `packages/envoy/internal/mcpbridge/config.go`:

1. Add `URL` field to `ServerConfig`:
```go
type ServerConfig struct {
	Name          string            `json:"name"`
	Transport     string            `json:"transport"`
	Command       []string          `json:"command"`
	URL           string            `json:"url"`
	Env           map[string]string `json:"env"`
	Resources     []string          `json:"resources"`
	Source        string            `json:"source"`
	TopicTemplate string            `json:"topic_template"`
	URIPattern    string            `json:"uri_pattern"`

	// compiled is the pre-compiled regex from URIPattern.
	compiled *regexp.Regexp
}
```

2. Add `net/url` to imports.

3. Replace the `validate()` method on `ServerConfig`:

```go
func (s *ServerConfig) validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("name is required")
	}

	// Default transport to stdio.
	if s.Transport == "" {
		s.Transport = "stdio"
	}

	switch s.Transport {
	case "stdio":
		if s.URL != "" {
			return fmt.Errorf("url is not allowed for stdio transport")
		}
		if len(s.Command) == 0 {
			return fmt.Errorf("command is required for stdio transport")
		}
		// Verify command[0] exists.
		if _, err := exec.LookPath(s.Command[0]); err != nil {
			return fmt.Errorf("command[0] %q not found: %w", s.Command[0], err)
		}
	case "http":
		if len(s.Command) > 0 {
			return fmt.Errorf("command is not allowed for http transport")
		}
		if strings.TrimSpace(s.URL) == "" {
			return fmt.Errorf("url is required for http transport")
		}
		u, err := url.Parse(s.URL)
		if err != nil {
			return fmt.Errorf("url: %w", err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("url scheme must be http or https, got %q", u.Scheme)
		}
	default:
		return fmt.Errorf("transport must be \"stdio\" or \"http\", got %q", s.Transport)
	}

	if strings.TrimSpace(s.Source) == "" {
		return fmt.Errorf("source is required")
	}
	if strings.TrimSpace(s.TopicTemplate) == "" {
		return fmt.Errorf("topic_template is required")
	}
	if strings.TrimSpace(s.URIPattern) == "" {
		return fmt.Errorf("uri_pattern is required")
	}

	// Compile URI pattern.
	re, err := regexp.Compile(s.URIPattern)
	if err != nil {
		return fmt.Errorf("uri_pattern: %w", err)
	}
	s.compiled = re

	// Verify every {placeholder} in topic_template has a matching named capture group.
	groups := make(map[string]bool)
	for _, name := range re.SubexpNames() {
		if name != "" {
			groups[name] = true
		}
	}
	placeholders := extractPlaceholders(s.TopicTemplate)
	if len(placeholders) == 0 {
		return fmt.Errorf("topic_template has no {placeholder} variables")
	}
	for _, ph := range placeholders {
		if !groups[ph] {
			return fmt.Errorf("topic_template placeholder {%s} has no matching named capture group in uri_pattern", ph)
		}
	}

	return nil
}
```

- [ ] **Step 4: Update the existing "UnsupportedTransport" test**

The existing `TestLoadConfig_UnsupportedTransport` tests `transport: "http"` as unsupported. Now that http is supported, this test uses `transport: "http"` with `command` set, which should now fail with a different error (command not allowed for http). Update the test to use a truly unsupported transport:

```go
func TestLoadConfig_UnsupportedTransport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{
		"servers": [{
			"name": "test",
			"transport": "websocket",
			"command": ["echo"],
			"source": "whatsapp",
			"topic_template": "notifications.whatsapp.{phone}.message",
			"uri_pattern": "whatsapp://(?P<phone>.+)"
		}]
	}`), 0644)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for unsupported transport")
	}
}
```

- [ ] **Step 5: Run all config tests**

Run from `packages/envoy`:
```bash
go test ./internal/mcpbridge/... -run "TestLoadConfig|TestRenderTopic|TestExtractPlaceholders"
```
Expected: all pass including new HTTP tests and updated unsupported transport test.

- [ ] **Step 6: Run full test suite**

Run from `packages/envoy`:
```bash
go test ./internal/mcpbridge/...
```
Expected: `ok github.com/sjawhar/envoy/internal/mcpbridge` — all tests pass.

- [ ] **Step 7: Run lsp_diagnostics**

Run `lsp_diagnostics` on `packages/envoy/internal/mcpbridge/` — expect zero errors.

- [ ] **Step 8: Commit**

```bash
jj describe -m "feat: add HTTP/SSE transport config validation"
jj new
```

---

## Task 4: Create mock HTTP MCP server for tests — Depends on: Task 1

Create a test helper that spins up a mock HTTP MCP server using `httptest`. This serves SSE on `/sse` and accepts POST on a message endpoint. Reusable across all HTTP transport tests.

**Files:**
- Create: `packages/envoy/internal/mcpbridge/http_mock_test.go`

- [ ] **Step 1: Create the mock HTTP MCP server**

Create `packages/envoy/internal/mcpbridge/http_mock_test.go`:

```go
package mcpbridge

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
)

// mockHTTPMCPServer is a test helper that simulates an MCP server over HTTP/SSE.
// It serves SSE on /sse and accepts JSON-RPC requests via POST on /message.
type mockHTTPMCPServer struct {
	server       *httptest.Server
	mu           sync.Mutex
	sseClients   []http.Flusher
	sseWriters   []io.Writer
	// onSubscribe is called when a resources/subscribe request is received.
	// Return the notification URI to send back, or "" for no notification.
	onSubscribe  func(uri string) string
	// onRead is called when a resources/read request is received.
	onRead       func(uri string) []resourceContent
	// failHandshake causes initialize to return an error.
	failHandshake bool
}

func newMockHTTPMCPServer() *mockHTTPMCPServer {
	m := &mockHTTPMCPServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", m.handleSSE)
	mux.HandleFunc("/message", m.handleMessage)
	m.server = httptest.NewServer(mux)
	return m
}

func (m *mockHTTPMCPServer) URL() string {
	return m.server.URL
}

func (m *mockHTTPMCPServer) Close() {
	m.server.Close()
}

func (m *mockHTTPMCPServer) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// Send the endpoint event with the message URL.
	messageURL := m.server.URL + "/message"
	fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", messageURL)
	flusher.Flush()

	m.mu.Lock()
	m.sseClients = append(m.sseClients, flusher)
	m.sseWriters = append(m.sseWriters, w)
	m.mu.Unlock()

	// Block until client disconnects.
	<-r.Context().Done()
}

func (m *mockHTTPMCPServer) handleMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int             `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params,omitempty"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	switch req.Method {
	case "initialize":
		if m.failHandshake {
			resp := map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"error":   map[string]any{"code": -32600, "message": "handshake rejected"},
			}
			json.NewEncoder(w).Encode(resp)
			// Also send via SSE so the client's pending channel gets the error.
			m.sendSSE(resp)
			return
		}
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result": map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":   map[string]any{"resources": map[string]any{"subscribe": true}},
				"serverInfo":     map[string]any{"name": "mock-http", "version": "1.0.0"},
			},
		}
		// Send response via SSE (the client reads responses from SSE, not HTTP response body).
		m.sendSSE(resp)

	case "notifications/initialized":
		// No response needed for notifications.
		w.WriteHeader(http.StatusAccepted)
		return

	case "resources/subscribe":
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  map[string]any{},
		}
		m.sendSSE(resp)

		// Send notification if configured.
		if m.onSubscribe != nil {
			var params struct{ URI string `json:"uri"` }
			json.Unmarshal(req.Params, &params)
			if notifyURI := m.onSubscribe(params.URI); notifyURI != "" {
				notif := map[string]any{
					"jsonrpc": "2.0",
					"method":  "notifications/resources/updated",
					"params":  map[string]any{"uri": notifyURI},
				}
				m.sendSSE(notif)
			}
		}

	case "resources/read":
		var params struct{ URI string `json:"uri"` }
		json.Unmarshal(req.Params, &params)

		contents := []resourceContent{{URI: params.URI, Text: "Hello from HTTP mock"}}
		if m.onRead != nil {
			contents = m.onRead(params.URI)
		}

		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  map[string]any{"contents": contents},
		}
		m.sendSSE(resp)

	default:
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// HTTP response is 202 Accepted — actual JSON-RPC response goes via SSE.
	w.WriteHeader(http.StatusAccepted)
}

// sendSSE sends a JSON-RPC message to all connected SSE clients.
func (m *mockHTTPMCPServer) sendSSE(msg any) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, w := range m.sseWriters {
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
		m.sseClients[i].Flush()
	}
}

// sendNotification sends a resource notification to all connected SSE clients.
func (m *mockHTTPMCPServer) sendNotification(uri string) {
	notif := map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/resources/updated",
		"params":  map[string]any{"uri": uri},
	}
	m.sendSSE(notif)
}
```

- [ ] **Step 2: Verify mock compiles**

Run from `packages/envoy`:
```bash
go vet ./internal/mcpbridge/...
```
Expected: no errors (the mock is only used in test files).

- [ ] **Step 3: Commit**

```bash
jj describe -m "test: add mock HTTP MCP server for SSE transport tests"
jj new
```

---

## Task 5: Implement HTTPServer — Depends on: Task 1, Task 3, Task 4

Create `HTTPServer` that connects to a pre-existing MCP server over HTTP/SSE. Uses shared `session` for JSON-RPC protocol handling. TDD.

**Files:**
- Create: `packages/envoy/internal/mcpbridge/http_server.go`
- Create: `packages/envoy/internal/mcpbridge/http_server_test.go`

- [ ] **Step 1: Write failing test for HTTPServer start + handshake**

Create `packages/envoy/internal/mcpbridge/http_server_test.go`:

```go
package mcpbridge

import (
	"testing"
	"time"
)

func TestHTTPServer_StartAndStop(t *testing.T) {
	mock := newMockHTTPMCPServer()
	defer mock.Close()

	var notifiedURI string
	mock.onSubscribe = func(uri string) string {
		return "whatsapp://messages/15551234567/5551234567@s.whatsapp.net"
	}

	cfg := ServerConfig{
		Name:          "whatsapp-http",
		Transport:     "http",
		URL:           mock.URL(),
		Resources:     []string{"whatsapp://messages/new"},
		Source:        "whatsapp",
		TopicTemplate: "notifications.whatsapp.{phone}.{jid}.message",
		URIPattern:    "whatsapp://messages/(?P<phone>[^/]+)/(?P<jid>.+)",
	}

	s := NewHTTPServer(cfg, func(uri string) {
		notifiedURI = uri
	})

	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Stop()

	if s.State() != StateReady {
		t.Fatalf("expected ready state, got %d", s.State())
	}

	// Wait for notification to arrive.
	time.Sleep(500 * time.Millisecond)

	if notifiedURI == "" {
		t.Fatal("expected notification URI to be set")
	}
	if notifiedURI != "whatsapp://messages/15551234567/5551234567@s.whatsapp.net" {
		t.Fatalf("unexpected notified URI: %s", notifiedURI)
	}
}

func TestHTTPServer_ReadResource(t *testing.T) {
	mock := newMockHTTPMCPServer()
	defer mock.Close()

	cfg := ServerConfig{
		Name:          "whatsapp-http",
		Transport:     "http",
		URL:           mock.URL(),
		Resources:     []string{"whatsapp://messages/new"},
		Source:        "whatsapp",
		TopicTemplate: "notifications.whatsapp.{phone}.{jid}.message",
		URIPattern:    "whatsapp://messages/(?P<phone>[^/]+)/(?P<jid>.+)",
	}

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
	defer mock.Close()

	cfg := ServerConfig{
		Name:          "whatsapp-http",
		Transport:     "http",
		URL:           mock.URL(),
		Source:        "whatsapp",
		TopicTemplate: "notifications.whatsapp.{phone}.message",
		URIPattern:    "whatsapp://(?P<phone>.+)",
	}

	s := NewHTTPServer(cfg, nil)
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	s.Stop()

	if s.State() != StateDead {
		t.Fatalf("expected dead state after stop, got %d", s.State())
	}
}

func TestHTTPServer_StartFailsOnUnreachable(t *testing.T) {
	cfg := ServerConfig{
		Name:          "unreachable",
		Transport:     "http",
		URL:           "http://127.0.0.1:1", // port 1 — won't be listening
		Source:        "whatsapp",
		TopicTemplate: "notifications.whatsapp.{phone}.message",
		URIPattern:    "whatsapp://(?P<phone>.+)",
	}

	s := NewHTTPServer(cfg, nil)
	if err := s.Start(); err == nil {
		s.Stop()
		t.Fatal("expected error for unreachable server")
	}
}

func TestHTTPServer_StartFailsOnBadHandshake(t *testing.T) {
	mock := newMockHTTPMCPServer()
	mock.failHandshake = true
	defer mock.Close()

	cfg := ServerConfig{
		Name:          "bad-handshake",
		Transport:     "http",
		URL:           mock.URL(),
		Source:        "whatsapp",
		TopicTemplate: "notifications.whatsapp.{phone}.message",
		URIPattern:    "whatsapp://(?P<phone>.+)",
	}

	s := NewHTTPServer(cfg, nil)
	if err := s.Start(); err == nil {
		s.Stop()
		t.Fatal("expected error for failed handshake")
	}
}

func TestHTTPServer_WaitForExitOnDisconnect(t *testing.T) {
	mock := newMockHTTPMCPServer()

	cfg := ServerConfig{
		Name:          "disconnect-test",
		Transport:     "http",
		URL:           mock.URL(),
		Source:        "whatsapp",
		TopicTemplate: "notifications.whatsapp.{phone}.message",
		URIPattern:    "whatsapp://(?P<phone>.+)",
	}

	s := NewHTTPServer(cfg, nil)
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Close the mock server to simulate disconnect.
	mock.Close()

	// WaitForExit should return.
	done := make(chan error, 1)
	go func() { done <- s.WaitForExit() }()

	select {
	case err := <-done:
		// Should return with an error (SSE stream broken).
		if err == nil {
			t.Fatal("expected error from WaitForExit after disconnect")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitForExit did not return after mock server closed")
	}
}

func TestHTTPServer_StopDoesNotBlockWaitForExit(t *testing.T) {
	mock := newMockHTTPMCPServer()
	defer mock.Close()

	cfg := ServerConfig{
		Name:          "stop-test",
		Transport:     "http",
		URL:           mock.URL(),
		Source:        "whatsapp",
		TopicTemplate: "notifications.whatsapp.{phone}.message",
		URIPattern:    "whatsapp://(?P<phone>.+)",
	}

	s := NewHTTPServer(cfg, nil)
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- s.WaitForExit() }()

	// Stop should cause WaitForExit to return without error.
	s.Stop()

	select {
	case err := <-done:
		// Stop is intentional — WaitForExit should return nil.
		if err != nil {
			t.Fatalf("expected nil from WaitForExit after Stop, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitForExit did not return after Stop")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run from `packages/envoy`:
```bash
go test ./internal/mcpbridge/... -run "TestHTTPServer"
```
Expected: FAIL — `NewHTTPServer` is not defined.

- [ ] **Step 3: Implement HTTPServer**

Create `packages/envoy/internal/mcpbridge/http_server.go`:

```go
package mcpbridge

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// HTTPServer connects to a pre-existing MCP server over HTTP/SSE transport.
type HTTPServer struct {
	cfg        ServerConfig
	session    *session
	messageURL string        // Set from SSE endpoint event.
	sseResp    *http.Response // SSE connection.
	state      atomic.Int32
	stopCh     chan struct{}
	closeOnce  sync.Once
	exitCh     chan error // Signals SSE connection loss.
	stopped    atomic.Bool
}

// NewHTTPServer creates an HTTP/SSE server from config. Call Start() to connect.
func NewHTTPServer(cfg ServerConfig, onNotify func(uri string)) *HTTPServer {
	s := &HTTPServer{
		cfg:    cfg,
		stopCh: make(chan struct{}),
		exitCh: make(chan error, 1),
	}
	s.state.Store(int32(StateStarting))
	s.session = newSession(cfg.Name, s.send, onNotify, s.stopCh)
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

// Start connects to the SSE endpoint, discovers the message URL,
// performs the MCP handshake, and subscribes to configured resources.
func (s *HTTPServer) Start() error {
	sseURL := strings.TrimRight(s.cfg.URL, "/") + "/sse"

	req, err := http.NewRequest("GET", sseURL, nil)
	if err != nil {
		return fmt.Errorf("create SSE request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")

	client := &http.Client{Timeout: 0} // No timeout for SSE.
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", sseURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return fmt.Errorf("SSE connect: unexpected status %d", resp.StatusCode)
	}

	s.sseResp = resp

	// Read the endpoint event to discover the message URL.
	messageURL, err := s.readEndpointEvent(resp.Body)
	if err != nil {
		resp.Body.Close()
		return fmt.Errorf("read endpoint event: %w", err)
	}
	s.messageURL = messageURL

	// Start reading SSE events in background.
	go s.readLoop(resp.Body)

	// Perform MCP handshake.
	if err := s.session.initialize(); err != nil {
		s.closeSSE()
		return fmt.Errorf("initialize %s: %w", s.cfg.Name, err)
	}

	// Subscribe to configured resources.
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

// Stop gracefully closes the SSE connection.
func (s *HTTPServer) Stop() {
	s.stopped.Store(true)
	s.closeOnce.Do(func() { close(s.stopCh) })
	s.closeSSE()
	s.state.Store(int32(StateDead))
}

// WaitForExit blocks until the SSE connection is lost or Stop is called.
// Returns nil if Stop was called intentionally, error otherwise.
func (s *HTTPServer) WaitForExit() error {
	err := <-s.exitCh
	if s.stopped.Load() {
		return nil
	}
	return err
}

// ReadResource calls resources/read and returns the content.
func (s *HTTPServer) ReadResource(uri string) ([]resourceContent, error) {
	return s.session.readResource(uri)
}

func (s *HTTPServer) closeSSE() {
	if s.sseResp != nil {
		s.sseResp.Body.Close()
	}
	s.state.Store(int32(StateDead))
}

// send posts a JSON-RPC message to the server's message endpoint.
func (s *HTTPServer) send(data []byte) error {
	if s.messageURL == "" {
		return fmt.Errorf("message URL not discovered")
	}

	resp, err := http.Post(s.messageURL, "application/json", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("POST to %s: %w", s.messageURL, err)
	}
	defer resp.Body.Close()

	// Accept 2xx status codes.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST %s: status %d: %s", s.messageURL, resp.StatusCode, body)
	}

	return nil
}

// readEndpointEvent reads SSE events until it finds the "endpoint" event.
func (s *HTTPServer) readEndpointEvent(r io.Reader) (string, error) {
	scanner := bufio.NewScanner(r)
	var eventType string

	deadline := time.After(10 * time.Second)
	eventCh := make(chan string, 1)
	errCh := make(chan error, 1)

	go func() {
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "event: ") {
				eventType = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				data := strings.TrimPrefix(line, "data: ")
				if eventType == "endpoint" {
					eventCh <- data
					return
				}
			}
		}
		if err := scanner.Err(); err != nil {
			errCh <- err
		} else {
			errCh <- fmt.Errorf("SSE stream ended before endpoint event")
		}
	}()

	select {
	case url := <-eventCh:
		return url, nil
	case err := <-errCh:
		return "", err
	case <-deadline:
		return "", fmt.Errorf("timeout waiting for endpoint event")
	}
}

// readLoop reads SSE events and routes JSON-RPC messages to the session.
func (s *HTTPServer) readLoop(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1<<20), 1<<20) // 1MB buffer.

	var eventType string
	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "event: ") {
			eventType = strings.TrimPrefix(line, "event: ")
			continue
		}

		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")

			if eventType == "message" {
				var resp jsonrpcResponse
				if err := json.Unmarshal([]byte(data), &resp); err != nil {
					log.Printf("mcp-bridge: %s: invalid JSON from SSE: %v", s.cfg.Name, err)
					continue
				}
				s.session.handleMessage(resp)
			}

			eventType = "" // Reset for next event.
			continue
		}
	}

	err := scanner.Err()
	if err != nil {
		log.Printf("mcp-bridge: %s: SSE read error: %v", s.cfg.Name, err)
	} else {
		err = fmt.Errorf("SSE stream closed")
	}
	s.state.Store(int32(StateDead))

	// Signal exit non-blocking.
	select {
	case s.exitCh <- err:
	default:
	}
}
```

- [ ] **Step 4: Run HTTP server tests**

Run from `packages/envoy`:
```bash
go test ./internal/mcpbridge/... -run "TestHTTPServer"
```
Expected: all `TestHTTPServer_*` tests pass.

- [ ] **Step 5: Run full test suite**

Run from `packages/envoy`:
```bash
go test ./internal/mcpbridge/...
```
Expected: `ok github.com/sjawhar/envoy/internal/mcpbridge` — all tests pass (existing stdio + new HTTP).

- [ ] **Step 6: Run lsp_diagnostics**

Run `lsp_diagnostics` on `packages/envoy/internal/mcpbridge/` — expect zero errors.

- [ ] **Step 7: Commit**

```bash
jj describe -m "feat: add HTTP/SSE MCP server transport"
jj new
```

---

## Task 6: Wire Bridge for HTTP transport + mixed mode — Depends on: Task 2, Task 5

Update `NewServer` factory to dispatch on transport type. Verify bridge `monitor()` works with HTTP servers. Test mixed stdio+HTTP configuration.

**Files:**
- Modify: `packages/envoy/internal/mcpbridge/server.go` (update NewServer)
- Create: `packages/envoy/internal/mcpbridge/bridge_test.go`

- [ ] **Step 1: Update NewServer to dispatch on transport**

In `packages/envoy/internal/mcpbridge/server.go`, update the `NewServer` function:

```go
// NewServer creates the appropriate Server implementation based on transport config.
func NewServer(cfg ServerConfig, onNotify func(uri string)) Server {
	switch cfg.Transport {
	case "http":
		return NewHTTPServer(cfg, onNotify)
	default:
		return NewManagedServer(cfg, onNotify)
	}
}
```

- [ ] **Step 2: Write test for NewServer factory**

Add to `packages/envoy/internal/mcpbridge/http_server_test.go`:

```go
func TestNewServer_HTTPTransport(t *testing.T) {
	cfg := ServerConfig{
		Name:      "test-http",
		Transport: "http",
		URL:       "http://localhost:3456",
	}
	s := NewServer(cfg, nil)
	if _, ok := s.(*HTTPServer); !ok {
		t.Fatalf("expected *HTTPServer, got %T", s)
	}
}

func TestNewServer_StdioTransport(t *testing.T) {
	cfg := ServerConfig{
		Name:      "test-stdio",
		Transport: "stdio",
	}
	s := NewServer(cfg, nil)
	if _, ok := s.(*ManagedServer); !ok {
		t.Fatalf("expected *ManagedServer, got %T", s)
	}
}
```

- [ ] **Step 3: Run factory tests**

Run from `packages/envoy`:
```bash
go test ./internal/mcpbridge/... -run "TestNewServer"
```
Expected: both pass.

- [ ] **Step 4: Run full test suite**

Run from `packages/envoy`:
```bash
go test ./internal/mcpbridge/...
```
Expected: `ok github.com/sjawhar/envoy/internal/mcpbridge` — all tests pass.

- [ ] **Step 5: Run full package tests**

Run from `packages/envoy`:
```bash
go test ./...
```
Expected: all packages pass.

- [ ] **Step 6: Run lsp_diagnostics**

Run `lsp_diagnostics` on `packages/envoy/internal/mcpbridge/` — expect zero errors.

- [ ] **Step 7: Commit**

```bash
jj describe -m "feat: wire bridge for mixed stdio and HTTP transport"
jj new
```

---

## Testing Plan

### Setup
- `cd packages/envoy` (all test commands run from here)
- No external infrastructure needed — tests use `httptest.NewServer` and compiled Go binaries

### Health Check
- `go vet ./internal/mcpbridge/...` — returns 0
- `go test ./internal/mcpbridge/... -count=1` — returns 0

### Verification Steps

For each acceptance criterion:

1. **Config: HTTP transport valid**
   - Action: `go test ./internal/mcpbridge/... -run TestLoadConfig_HTTPTransportValid -v`
   - Expected: PASS
   - Tool: Go test runner

2. **Config: HTTP transport missing URL**
   - Action: `go test ./internal/mcpbridge/... -run TestLoadConfig_HTTPTransportMissingURL -v`
   - Expected: PASS
   - Tool: Go test runner

3. **Config: HTTP transport invalid URL**
   - Action: `go test ./internal/mcpbridge/... -run TestLoadConfig_HTTPTransportInvalidURL -v`
   - Expected: PASS
   - Tool: Go test runner

4. **Config: HTTP transport with command rejected**
   - Action: `go test ./internal/mcpbridge/... -run TestLoadConfig_HTTPTransportWithCommand -v`
   - Expected: PASS
   - Tool: Go test runner

5. **Config: default transport is stdio**
   - Action: `go test ./internal/mcpbridge/... -run TestLoadConfig_DefaultTransportStdio -v`
   - Expected: PASS
   - Tool: Go test runner

6. **HTTP Server: start, handshake, subscribe, notify**
   - Action: `go test ./internal/mcpbridge/... -run TestHTTPServer_StartAndStop -v`
   - Expected: PASS
   - Tool: Go test runner

7. **HTTP Server: ReadResource via POST**
   - Action: `go test ./internal/mcpbridge/... -run TestHTTPServer_ReadResource -v`
   - Expected: PASS
   - Tool: Go test runner

8. **HTTP Server: Stop sets state dead**
   - Action: `go test ./internal/mcpbridge/... -run TestHTTPServer_StopSetsStateDead -v`
   - Expected: PASS
   - Tool: Go test runner

9. **HTTP Server: Start fails on unreachable**
   - Action: `go test ./internal/mcpbridge/... -run TestHTTPServer_StartFailsOnUnreachable -v`
   - Expected: PASS
   - Tool: Go test runner

10. **HTTP Server: Start fails on bad handshake**
    - Action: `go test ./internal/mcpbridge/... -run TestHTTPServer_StartFailsOnBadHandshake -v`
    - Expected: PASS
    - Tool: Go test runner

11. **HTTP Server: WaitForExit on disconnect**
    - Action: `go test ./internal/mcpbridge/... -run TestHTTPServer_WaitForExitOnDisconnect -v`
    - Expected: PASS
    - Tool: Go test runner

12. **HTTP Server: Stop does not block WaitForExit**
    - Action: `go test ./internal/mcpbridge/... -run TestHTTPServer_StopDoesNotBlockWaitForExit -v`
    - Expected: PASS
    - Tool: Go test runner

13. **Factory: correct dispatch**
    - Action: `go test ./internal/mcpbridge/... -run "TestNewServer" -v`
    - Expected: PASS
    - Tool: Go test runner

14. **No regression: all existing tests**
    - Action: `go test ./internal/mcpbridge/... -count=1`
    - Expected: `ok github.com/sjawhar/envoy/internal/mcpbridge`
    - Tool: Go test runner

15. **Full package tests**
    - Action: `go test ./... -count=1`
    - Expected: all packages ok
    - Tool: Go test runner

### Tools Needed
- Go test runner (built-in)
- `lsp_diagnostics` for static analysis between tasks
- No browser, no external services, no Docker

## Required Skills

The following project-specific skills should be loaded by downstream workers:

| Phase | Skills |
|-------|--------|
| Implement | `test-driven-development`, `verification-before-completion` |
| Test | (none beyond standard) |
| Review | (none beyond standard) |

Workers: invoke these skills at the start of your workflow before beginning work.
If a skill is unavailable in your environment, proceed without it.
