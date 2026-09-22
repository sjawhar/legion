package shimwire

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// Every frame the shim and the daemon exchange, as the shipped TypeScript writes it on the wire
// (worker-rpc.ts:420, 430, 452, 475, 490, 493-496; worker-shim.ts:332, 356-366;
// worker-stream-listener.ts:14; fake-omp-rpc.ts:24-79). A frame decodes to its Go type and
// encodes back to the same line, so a Go peer is indistinguishable from the shipped one.
func TestEveryExchangedFrameRoundTripsItsWireLine(t *testing.T) {
	streaming := true
	for _, tc := range []struct {
		name  string
		line  string
		frame Frame
	}{
		{"hello", `{"type":"hello","bootToken":"boot-1"}`, Hello{BootToken: "boot-1"}},
		{"hello_ack", `{"type":"hello_ack"}`, HelloAck{}},
		{
			"negotiate_protocol",
			`{"type":"negotiate_protocol","id":"r1","protocolVersion":2}`,
			NegotiateProtocol{ID: "r1", ProtocolVersion: 2},
		},
		{
			"prompt",
			`{"type":"prompt","id":"r2","deliveryId":"d1","message":"work"}`,
			Prompt{ID: "r2", DeliveryID: "d1", Message: "work"},
		},
		{
			"response",
			`{"type":"response","id":"r2","command":"prompt","success":true}`,
			Response{ID: "r2", Command: TypePrompt, Success: true},
		},
		{
			"refusal response",
			`{"type":"response","id":"r2","command":"prompt","success":false,"error":"busy"}`,
			Response{ID: "r2", Command: TypePrompt, Success: false, Error: "busy"},
		},
		{
			"get_state response",
			`{"type":"response","id":"r3","command":"get_state","success":true,"data":{"isStreaming":true}}`,
			Response{
				ID:      "r3",
				Command: TypeGetState,
				Success: true,
				Data:    json.RawMessage(`{"isStreaming":true}`),
			},
		},
		{"get_state", `{"type":"get_state","id":"r3"}`, GetState{ID: "r3"}},
		{"agent_start", `{"type":"agent_start"}`, AgentStart{}},
		{"replayed agent_start", `{"type":"agent_start","deliveryId":"d1"}`, AgentStart{DeliveryID: "d1"}},
		{"agent_end", `{"type":"agent_end"}`, AgentEnd{}},
		{"shutdown", `{"type":"shutdown"}`, Shutdown{}},
		{
			"adopt-working-copy",
			`{"type":"adopt-working-copy","id":"r4","jjUser":"legion","jjEmail":"legion@example.com","timeoutMs":30000}`,
			AdoptWorkingCopy{ID: "r4", JJUser: "legion", JJEmail: "legion@example.com", TimeoutMs: 30_000},
		},
		{
			"adopt-working-copy-result",
			`{"type":"adopt-working-copy-result","id":"r4","ok":true}`,
			AdoptWorkingCopyResult{ID: "r4", OK: true},
		},
		{
			"failed adopt-working-copy-result",
			`{"type":"adopt-working-copy-result","id":"r4","ok":false,"error":"no workspace"}`,
			AdoptWorkingCopyResult{ID: "r4", OK: false, Error: "no workspace"},
		},
		{
			"rpc_chunk",
			`{"type":"rpc_chunk","chunkId":"agent-end-1","index":0,"count":2,"byteLength":1048577,"data":"eA=="}`,
			RPCChunk{ChunkID: "agent-end-1", Index: 0, Count: 2, ByteLength: 1_048_577, Data: "eA=="},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decoded, err := Decode([]byte(tc.line))
			if err != nil {
				t.Fatalf("Decode(%s): %v", tc.line, err)
			}
			if !reflect.DeepEqual(decoded, tc.frame) {
				t.Fatalf("Decode(%s) = %#v, want %#v", tc.line, decoded, tc.frame)
			}
			encoded, err := json.Marshal(tc.frame)
			if err != nil {
				t.Fatalf("Marshal(%#v): %v", tc.frame, err)
			}
			if string(encoded) != tc.line {
				t.Fatalf("Marshal(%#v) = %s, want %s", tc.frame, encoded, tc.line)
			}
		})
	}
	// The get_state answer's one field the daemon reads (worker-rpc.ts:476-479).
	var state StateData
	if err := json.Unmarshal(json.RawMessage(`{"isStreaming":true}`), &state); err != nil {
		t.Fatalf("Unmarshal state data: %v", err)
	}
	if state.IsStreaming == nil || *state.IsStreaming != streaming {
		t.Fatalf("StateData.IsStreaming = %v, want %v", state.IsStreaming, streaming)
	}
	// Absent rather than false: the shipped client leaves the run state alone and logs
	// (worker-rpc.ts:480-485), which it can only do because it can tell the two apart.
	state = StateData{}
	if err := json.Unmarshal(json.RawMessage(`{}`), &state); err != nil {
		t.Fatalf("Unmarshal empty state data: %v", err)
	}
	if state.IsStreaming != nil {
		t.Fatalf("StateData.IsStreaming = %v, want nil for an answer that omits it", *state.IsStreaming)
	}
}

// The shim is a bridge: OMP emits frames this package models nothing of (tool_execution_start,
// error, and whatever a later OMP adds), and they must cross it unchanged rather than fail a
// decode (worker-shim.ts:141-142 leaves them unsummarized and :290 forwards them anyway).
func TestAnUnknownFrameTypeDecodesToItsRawJSON(t *testing.T) {
	line := `{"type":"tool_execution_start","toolName":"bash","args":{"command":"ls"}}`
	decoded, err := Decode([]byte(line))
	if err != nil {
		t.Fatalf("Decode(%s): %v", line, err)
	}
	raw, ok := decoded.(Raw)
	if !ok {
		t.Fatalf("Decode(%s) = %#v, want a Raw frame", line, decoded)
	}
	if raw.FrameType() != "tool_execution_start" {
		t.Fatalf("FrameType() = %q, want %q", raw.FrameType(), "tool_execution_start")
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("Marshal(%#v): %v", raw, err)
	}
	if string(encoded) != line {
		t.Fatalf("Marshal(raw) = %s, want the line unchanged %s", encoded, line)
	}
	// A frame with no type at all is still a frame to forward, not an error.
	decoded, err = Decode([]byte(`{"messages":[]}`))
	if err != nil {
		t.Fatalf("Decode of a frame without a type: %v", err)
	}
	if got := decoded.FrameType(); got != "" {
		t.Fatalf("FrameType() = %q, want the empty string", got)
	}
}

// A line that is not a JSON object is not a frame. The shipped shim reaches the same verdict by
// parsing to undefined and matching no branch (worker-shim.ts:146-152, 252), and the listener
// refuses the connection outright (worker-stream-listener.ts:129-138).
func TestALineThatIsNotAJSONObjectIsRefused(t *testing.T) {
	for _, line := range []string{"", "   ", "not json", "null", "[1,2]", `"a string"`, "42", "{"} {
		if _, err := Decode([]byte(line)); !errors.Is(err, ErrNotAFrame) {
			t.Fatalf("Decode(%q) error = %v, want ErrNotAFrame", line, err)
		}
	}
}

// The two frames the shipped code refuses when their fields are malformed, because in each case
// the sender is the only writer and a malformed one is a bug to name: the hello
// (worker-stream-listener.ts:140-148) and the daemon's adoption request (worker-shim.ts:171-180).
func TestTheFramesTheShippedCodeValidatesRefuseTheirMalformedForms(t *testing.T) {
	if err := (Hello{BootToken: "boot-1"}).Validate(); err != nil {
		t.Fatalf("Hello.Validate: %v", err)
	}
	if err := (Hello{}).Validate(); !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("Hello{}.Validate = %v, want ErrMalformedFrame", err)
	}
	good := AdoptWorkingCopy{ID: "r4", JJUser: "legion", JJEmail: "l@example.com", TimeoutMs: 1}
	if err := good.Validate(); err != nil {
		t.Fatalf("AdoptWorkingCopy.Validate: %v", err)
	}
	for _, tc := range []struct {
		name  string
		frame AdoptWorkingCopy
	}{
		{"no id", AdoptWorkingCopy{JJUser: "legion", JJEmail: "l@example.com", TimeoutMs: 1}},
		{"no jjUser", AdoptWorkingCopy{ID: "r4", JJEmail: "l@example.com", TimeoutMs: 1}},
		{"no jjEmail", AdoptWorkingCopy{ID: "r4", JJUser: "legion", TimeoutMs: 1}},
		{"no timeout", AdoptWorkingCopy{ID: "r4", JJUser: "legion", JJEmail: "l@example.com"}},
		{"negative timeout", AdoptWorkingCopy{ID: "r4", JJUser: "legion", JJEmail: "l@example.com", TimeoutMs: -1}},
	} {
		if err := tc.frame.Validate(); !errors.Is(err, ErrMalformedFrame) {
			t.Fatalf("%s: Validate = %v, want ErrMalformedFrame", tc.name, err)
		}
	}
}
