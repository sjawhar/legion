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
	state     atomic.Int32
	nextID    atomic.Int32
	mu        sync.Mutex
	pending   map[int]chan jsonrpcResponse
	onNotify  func(uri string)
	stopCh    chan struct{}
	closeOnce sync.Once
}

// NewManagedServer creates a managed server from config. Call Start() to spawn.
func NewManagedServer(cfg ServerConfig, onNotify func(uri string)) *ManagedServer {
	s := &ManagedServer{
		cfg:      cfg,
		pending:  make(map[int]chan jsonrpcResponse),
		onNotify: onNotify,
		stopCh:   make(chan struct{}),
	}
	s.state.Store(int32(StateStarting))
	return s
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
	if err := s.initialize(); err != nil {
		s.kill()
		return fmt.Errorf("initialize %s: %w", s.cfg.Name, err)
	}

	// Subscribe to configured resources.
	for _, uri := range s.cfg.Resources {
		if err := s.subscribe(uri); err != nil {
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

func (s *ManagedServer) nextRequestID() int {
	return int(s.nextID.Add(1))
}

func (s *ManagedServer) send(req jsonrpcRequest) error {
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = s.stdin.Write(data)
	return err
}

func (s *ManagedServer) sendNotification(method string) error {
	data, err := json.Marshal(jsonrpcNotification{
		JSONRPC: "2.0",
		Method:  method,
	})
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = s.stdin.Write(data)
	return err
}

func (s *ManagedServer) call(method string, params any) (json.RawMessage, error) {
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

	if err := s.send(jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  raw,
	}); err != nil {
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

		// Notification (no ID).
		if resp.ID == nil && resp.Method != "" {
			s.handleNotification(resp)
			continue
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
	if err := s.scanner.Err(); err != nil {
		log.Printf("mcp-bridge: %s: read error: %v", s.cfg.Name, err)
	}
	s.state.Store(int32(StateDead))
}

func (s *ManagedServer) handleNotification(resp jsonrpcResponse) {
	if resp.Method != "notifications/resources/updated" {
		return
	}
	var params notificationParams
	if err := json.Unmarshal(resp.Params, &params); err != nil {
		log.Printf("mcp-bridge: %s: invalid notification params: %v", s.cfg.Name, err)
		return
	}
	if params.URI == "" {
		return
	}
	if s.onNotify != nil {
		go s.onNotify(params.URI)
	}
}

func (s *ManagedServer) initialize() error {
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

	// Send initialized notification (no ID — true JSON-RPC notification).
	return s.sendNotification("notifications/initialized")
}

func (s *ManagedServer) subscribe(uri string) error {
	_, err := s.call("resources/subscribe", subscribeParams{URI: uri})
	return err
}

// ReadResource calls resources/read and returns the content.
func (s *ManagedServer) ReadResource(uri string) ([]resourceContent, error) {
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
