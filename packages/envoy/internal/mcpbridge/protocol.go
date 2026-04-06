package mcpbridge

import "encoding/json"

// JSON-RPC 2.0 types for the MCP protocol.

type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// jsonrpcNotification is a JSON-RPC 2.0 notification (no id field).
type jsonrpcNotification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// MCP-specific types.

type initializeParams struct {
	ProtocolVersion string           `json:"protocolVersion"`
	Capabilities    clientCaps       `json:"capabilities"`
	ClientInfo      clientInfoParams `json:"clientInfo"`
}

type clientCaps struct{}

type clientInfoParams struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type subscribeParams struct {
	URI string `json:"uri"`
}

type readParams struct {
	URI string `json:"uri"`
}

type notificationParams struct {
	URI string `json:"uri"`
}

type resourceContent struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
}

type readResult struct {
	Contents []resourceContent `json:"contents"`
}
