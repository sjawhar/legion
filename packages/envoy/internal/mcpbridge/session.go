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
// Transports provide send/receive functions; session handles protocol logic.
type session struct {
	serverName string
	nextID     atomic.Int32
	mu         sync.Mutex
	pending    map[int]chan jsonrpcResponse
	sendFn     func([]byte) error
	onNotify   func(uri string)
	stopCh     chan struct{}
}

func newSession(serverName string, sendFn func([]byte) error, onNotify func(uri string), stopCh chan struct{}) *session {
	return &session{
		serverName: serverName,
		pending:    make(map[int]chan jsonrpcResponse),
		sendFn:     sendFn,
		onNotify:   onNotify,
		stopCh:     stopCh,
	}
}

func (s *session) nextRequestID() int {
	return int(s.nextID.Add(1))
}

func (s *session) send(req jsonrpcRequest) error {
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	return s.sendFn(data)
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

// handleMessage processes an incoming JSON-RPC message (response or notification).
func (s *session) handleMessage(data []byte) {
	var resp jsonrpcResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		log.Printf("mcp-bridge: %s: invalid JSON from server: %v", s.serverName, err)
		return
	}

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
		log.Printf("mcp-bridge: %s: invalid notification params: %v", s.serverName, err)
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

	// Send initialized notification (no ID — true JSON-RPC notification).
	return s.sendNotification("notifications/initialized")
}

func (s *session) subscribe(uri string) error {
	_, err := s.call("resources/subscribe", subscribeParams{URI: uri})
	return err
}

// readResource calls resources/read and returns the content.
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
