package shimwire

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
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
		{
			"launcher_hello",
			`{"type":"launcher_hello","token":"launcher-token","sandbox":"legion-legion-legion-208","role":"tester","podUid":"pod-1","launcherId":"launcher-1"}`,
			LauncherHello{Token: "launcher-token", Sandbox: "legion-legion-legion-208", Role: "tester", PodUID: "pod-1", LauncherID: "launcher-1"},
		},
		{"launcher_hello_ack", `{"type":"launcher_hello_ack"}`, LauncherHelloAck{}},
		{
			"launcher_state",
			`{"type":"launcher_state","child":{"generation":7,"pid":42},"lastExit":{"generation":6,"code":143,"signal":"terminated"}}`,
			LauncherState{Child: &LauncherChild{Generation: 7, PID: 42}, LastExit: &LauncherExit{Generation: 6, Code: 143, Signal: "terminated"}},
		},
		{
			"launcher_start",
			`{"type":"launcher_start","id":"start-1","generation":7,"argv":["omp","--mode","rpc"],"env":["A=B"],"files":{"LEGION_BOOT_TOKEN":"boot-7"},"resumeFile":"/sessions/test.jsonl"}`,
			LauncherStart{ID: "start-1", Generation: 7, Argv: []string{"omp", "--mode", "rpc"}, Env: []string{"A=B"}, Files: map[string]string{"LEGION_BOOT_TOKEN": "boot-7"}, ResumeFile: "/sessions/test.jsonl"},
		},
		{
			"launcher_start_result",
			`{"type":"launcher_start_result","id":"start-1","ok":true,"runningGeneration":7}`,
			LauncherStartResult{ID: "start-1", OK: true, RunningGeneration: 7},
		},
		{
			"launcher_stop",
			`{"type":"launcher_stop","id":"stop-1","generation":7,"graceMs":10000}`,
			LauncherStop{ID: "stop-1", Generation: 7, GraceMs: 10_000},
		},
		{"launcher_stop_result", `{"type":"launcher_stop_result","id":"stop-1","ok":true}`, LauncherStopResult{ID: "stop-1", OK: true}},
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

func TestHello2RoundTripsWithAndWithoutAnIdentity(t *testing.T) {
	identity := &AgentSecretsHello{Thumbprint: "NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs", PodToken: "eyJhbGciOiJSUzI1NiJ9.e30.sig"}
	for name, frame := range map[string]Hello2{
		"with identity":    {BootToken: "boot", AgentSecrets: identity},
		"without identity": {BootToken: "boot"},
	} {
		t.Run(name, func(t *testing.T) {
			line, err := json.Marshal(frame)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.HasPrefix(line, []byte(`{"type":"hello2"`)) {
				t.Fatalf("encoded as %s, want the hello2 type first", line)
			}
			if frame.AgentSecrets == nil && bytes.Contains(line, []byte("agentSecrets")) {
				t.Fatalf("an absent identity is encoded: %s", line)
			}
			decoded, err := Decode(line)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := decoded.(Hello2)
			if !ok || got.BootToken != "boot" || (got.AgentSecrets == nil) != (frame.AgentSecrets == nil) ||
				(got.AgentSecrets != nil && *got.AgentSecrets != *identity) {
				t.Fatalf("decoded %#v", decoded)
			}
			if err := got.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHello2ValidateNamesWhatIsMissing(t *testing.T) {
	for name, tc := range map[string]struct {
		frame Hello2
		want  string
	}{
		"no boot token":     {Hello2{}, "no bootToken"},
		"no thumbprint":     {Hello2{BootToken: "b", AgentSecrets: &AgentSecretsHello{PodToken: "a.b.c"}}, "no thumbprint"},
		"bad thumbprint":    {Hello2{BootToken: "b", AgentSecrets: &AgentSecretsHello{Thumbprint: "not base64url!", PodToken: "a.b.c"}}, "not a base64url SHA-256"},
		"no pod token":      {Hello2{BootToken: "b", AgentSecrets: &AgentSecretsHello{Thumbprint: "NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs"}}, "no podToken"},
		"pod token not jwt": {Hello2{BootToken: "b", AgentSecrets: &AgentSecretsHello{Thumbprint: "NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs", PodToken: "nodots"}}, "not a JWT"},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.frame.Validate()
			if err == nil || !errors.Is(err, ErrMalformedFrame) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want ErrMalformedFrame naming %q", err, tc.want)
			}
		})
	}
}

// A hello2 carrying a projected token the size EKS issues (about 1 KiB; a 2 KiB one here, for
// margin) fits the listener's pre-newline bound, so MaxHelloBytes stays as the shipped listener
// had it.
func TestAHello2WithAProjectedTokenFitsTheHelloBound(t *testing.T) {
	token := strings.Repeat("a", 700) + "." + strings.Repeat("b", 1000) + "." + strings.Repeat("c", 342)
	line, err := json.Marshal(Hello2{BootToken: strings.Repeat("t", 64), AgentSecrets: &AgentSecretsHello{
		Thumbprint: "NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs", PodToken: token}})
	if err != nil {
		t.Fatal(err)
	}
	if len(line)+1 > MaxHelloBytes {
		t.Fatalf("a hello2 with a 2 KiB token is %d bytes; MaxHelloBytes is %d", len(line)+1, MaxHelloBytes)
	}
}

func TestEnrollmentFramesRoundTrip(t *testing.T) {
	request := AgentSecretsEnrollment{ID: "req-1", EnrollmentID: "enr-1"}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (AgentSecretsEnrollment{ID: "req-1"}).Validate(); err == nil || !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("an enrollment frame with no enrollment id validated: %v", err)
	}
	for _, frame := range []Frame{request, AgentSecretsEnrollmentResult{ID: "req-1", OK: false, Error: "key dir is read-only"}} {
		line, err := json.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := Decode(line)
		if err != nil {
			t.Fatal(err)
		}
		if decoded.FrameType() != frame.FrameType() || !reflect.DeepEqual(decoded, frame) {
			t.Fatalf("decoded %#v from %s, want %#v", decoded, line, frame)
		}
	}
}
