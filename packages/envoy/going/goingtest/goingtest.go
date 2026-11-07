// Package goingtest holds the fake text model shared by this module's own tests and its callers'
// tests, kept out of the going package proper so no test-only type ships in the production
// binary. It serves the chat-completions shape over httptest, so a test drives the real HTTP
// client - its route, its sign-in and its reading of the stream - rather than a stub of it.
package goingtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/going"
)

// defaultChunk is how many runes a frame carries when a Reply names no size: small enough that
// any reply of a few words arrives in several frames, as a real completion does.
const defaultChunk = 8

// Reply is what the model answers one call with.
type Reply struct {
	// Text is streamed as the turn's content.
	Text string
	// ToolCalls is a turn that ends in tool calls instead of an answer.
	ToolCalls []going.ToolCall
	// Chunk is how many runes each frame carries; zero picks a small default.
	Chunk int
	// Pause is how long the model waits between frames, so a test can assert the first byte
	// arrives early.
	Pause time.Duration
	// Silent never answers: the response is held open until the caller's context ends.
	Silent bool
	// Status, when non-zero, is answered with an error body instead of a completion.
	Status int
}

// ReceivedRequest is one call as the model received it, decoded.
type ReceivedRequest struct {
	Model         string
	Messages      []going.Message
	Tools         []going.Tool
	Authorization string
	Path          string
	RawQuery      string
}

// Model is a chat-completions service a test configures a client against: it answers each call
// with the reply the test named, streams it frame by frame as a real completion does, and counts
// what it served. It calls nothing and holds no credential.
type Model struct {
	// URL is the base URL to configure a client's endpoint with; the client appends the route.
	URL string

	replies []Reply

	mu       sync.Mutex
	requests []ReceivedRequest
}

// NewModel starts a model that serves reply i to call i, the last reply serving every call after
// it, and closes with the test. A model with no replies answers every call with an empty
// completion.
func NewModel(t testing.TB, replies ...Reply) *Model {
	t.Helper()
	model := &Model{replies: replies}
	server := httptest.NewServer(http.HandlerFunc(model.serve))
	t.Cleanup(server.Close)
	model.URL = server.URL
	return model
}

// Calls reports how many calls the model has served.
func (m *Model) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

// Requests reports the decoded bodies the model received, in order.
func (m *Model) Requests() []ReceivedRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]ReceivedRequest(nil), m.requests...)
}

// serve records one call and answers it with the reply that call's turn names.
func (m *Model) serve(w http.ResponseWriter, r *http.Request) {
	var body wireRequest
	_ = json.NewDecoder(r.Body).Decode(&body)
	received := ReceivedRequest{
		Model:         body.Model,
		Messages:      body.messages(),
		Tools:         body.tools(),
		Authorization: r.Header.Get("Authorization"),
		Path:          r.URL.Path,
		RawQuery:      r.URL.RawQuery,
	}
	m.mu.Lock()
	m.requests = append(m.requests, received)
	call := len(m.requests)
	m.mu.Unlock()

	reply := Reply{}
	if len(m.replies) > 0 {
		reply = m.replies[min(call, len(m.replies))-1]
	}
	switch {
	case reply.Status != 0:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(reply.Status)
		_, _ = w.Write([]byte(`{"error":{"code":"simulated_failure","message":"the model service refused this call"}}`))
	case reply.Silent:
		<-r.Context().Done()
	default:
		stream(w, reply)
	}
}

// stream writes the reply as a real completion does: the content in frames of Chunk runes, then
// each tool call, its arguments spread over frames too, then the turn's finish_reason and [DONE].
func stream(w http.ResponseWriter, reply Reply) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	control := http.NewResponseController(w)
	_ = control.Flush()
	chunk := reply.Chunk
	if chunk <= 0 {
		chunk = defaultChunk
	}
	wrote := false
	write := func(delta wireDelta) {
		if wrote && reply.Pause > 0 {
			time.Sleep(reply.Pause)
		}
		wrote = true
		frame, _ := json.Marshal(wireFrame{Choices: []wireChoice{{Delta: delta}}})
		_, _ = w.Write([]byte("data: " + string(frame) + "\n\n"))
		_ = control.Flush()
	}
	for _, piece := range pieces(reply.Text, chunk) {
		write(wireDelta{Content: piece})
	}
	for index, call := range reply.ToolCalls {
		arguments := pieces(call.Arguments, chunk)
		head := wireToolCallDelta{Index: index, ID: call.ID, Type: "function"}
		head.Function.Name = call.Name
		if len(arguments) > 0 {
			head.Function.Arguments = arguments[0]
		}
		write(wireDelta{ToolCalls: []wireToolCallDelta{head}})
		for _, piece := range arguments[min(1, len(arguments)):] {
			tail := wireToolCallDelta{Index: index}
			tail.Function.Arguments = piece
			write(wireDelta{ToolCalls: []wireToolCallDelta{tail}})
		}
	}
	finish := "stop"
	if len(reply.ToolCalls) > 0 {
		finish = "tool_calls"
	}
	if wrote && reply.Pause > 0 {
		time.Sleep(reply.Pause)
	}
	final, _ := json.Marshal(wireFrame{Choices: []wireChoice{{FinishReason: finish}}})
	_, _ = w.Write([]byte("data: " + string(final) + "\n\ndata: [DONE]\n\n"))
	_ = control.Flush()
}

// pieces cuts text into runs of at most size runes, so a frame never splits a rune.
func pieces(text string, size int) []string {
	runes := []rune(text)
	var out []string
	for start := 0; start < len(runes); start += size {
		out = append(out, string(runes[start:min(start+size, len(runes))]))
	}
	return out
}

// The chat-completions shapes the model speaks. They are declared here rather than taken from
// going, which keeps its own unexported, because a fake that speaks the protocol is what proves
// the client speaks it too.
type wireRequest struct {
	Model    string        `json:"model"`
	Stream   bool          `json:"stream"`
	Messages []wireMessage `json:"messages"`
	Tools    []wireTool    `json:"tools"`
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls"`
	ToolCallID string         `json:"tool_call_id"`
}

type wireToolCall struct {
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireTool struct {
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

// messages is the request's conversation as a caller of going states it.
func (r wireRequest) messages() []going.Message {
	var messages []going.Message
	for _, message := range r.Messages {
		converted := going.Message{Role: message.Role, Content: message.Content, ToolCallID: message.ToolCallID}
		for _, call := range message.ToolCalls {
			converted.ToolCalls = append(converted.ToolCalls, going.ToolCall{
				ID:        call.ID,
				Name:      call.Function.Name,
				Arguments: call.Function.Arguments,
			})
		}
		messages = append(messages, converted)
	}
	return messages
}

// tools is the request's tools as a caller of going declares them.
func (r wireRequest) tools() []going.Tool {
	var tools []going.Tool
	for _, tool := range r.Tools {
		tools = append(tools, going.Tool{
			Name:        tool.Function.Name,
			Description: tool.Function.Description,
			Parameters:  tool.Function.Parameters,
		})
	}
	return tools
}

type wireFrame struct {
	Choices []wireChoice `json:"choices"`
}

type wireChoice struct {
	Index        int       `json:"index"`
	Delta        wireDelta `json:"delta"`
	FinishReason string    `json:"finish_reason,omitempty"`
}

type wireDelta struct {
	Content   string              `json:"content,omitempty"`
	ToolCalls []wireToolCallDelta `json:"tool_calls,omitempty"`
}

type wireToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}
