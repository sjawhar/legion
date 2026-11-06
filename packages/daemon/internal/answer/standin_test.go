package answer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// standIn is a provider endpoint that records every request it gets and answers each with the next
// of its replies.
type standIn struct {
	t       *testing.T
	server  *httptest.Server
	mu      sync.Mutex
	replies []reply
	got     []recorded
}

// reply is one response a stand-in gives: an SSE stream, or an error status with a body, after a
// delay.
type reply struct {
	status  int
	body    []byte
	headers map[string]string
	delay   time.Duration
}

// recorded is a request a stand-in got.
type recorded struct {
	path   string
	header http.Header
	body   []byte
}

func newStandIn(t *testing.T, replies ...reply) *standIn {
	t.Helper()
	s := &standIn{t: t, replies: replies}
	s.server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.server.Close)
	return s
}

func (s *standIn) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.got = append(s.got, recorded{path: r.URL.Path, header: r.Header.Clone(), body: body})
	if len(s.replies) == 0 {
		s.mu.Unlock()
		http.Error(w, `{"type":"error","error":{"type":"invalid_request_error","message":"the stand-in has no more replies"}}`, http.StatusBadRequest)
		return
	}
	next := s.replies[0]
	s.replies = s.replies[1:]
	s.mu.Unlock()
	if next.delay > 0 {
		select {
		case <-time.After(next.delay):
		case <-r.Context().Done():
			return
		}
	}
	for k, v := range next.headers {
		w.Header().Set(k, v)
	}
	if next.status != 0 && next.status != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(next.status)
		_, _ = w.Write(next.body)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write(next.body)
}

// requests are the requests the stand-in got, in order.
func (s *standIn) requests() []recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recorded(nil), s.got...)
}

// decoded is a recorded request's JSON body.
func (r recorded) decoded(t *testing.T) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(r.body, &body); err != nil {
		t.Fatalf("the request body is no JSON object: %v\n%s", err, r.body)
	}
	return body
}

type sseWriter struct{ bytes.Buffer }

func (w *sseWriter) event(name string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
}

// anthropicUsage is a Messages API usage object.
type anthropicUsage struct {
	Input, Output, CacheWrite, CacheRead int
}

func (u anthropicUsage) object() map[string]any {
	return map[string]any{
		"input_tokens": u.Input, "output_tokens": u.Output,
		"cache_creation_input_tokens": u.CacheWrite, "cache_read_input_tokens": u.CacheRead,
	}
}

// anthropicStream is the event stream the Messages API sends for a message of blocks (each a
// Messages API content block) that stops for stopReason.
func anthropicStream(stopReason string, usage anthropicUsage, blocks ...map[string]any) reply {
	var w sseWriter
	w.event("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_standin", "type": "message", "role": "assistant", "model": "claude-standin", "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": usage.object(),
	}})
	for i, b := range blocks {
		switch b["type"] {
		case "text":
			w.event("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "text", "text": ""}})
			w.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "text_delta", "text": b["text"]}})
		case "thinking":
			w.event("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""}})
			w.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "thinking_delta", "thinking": b["thinking"]}})
			w.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "signature_delta", "signature": b["signature"]}})
		case "tool_use":
			w.event("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "tool_use", "id": b["id"], "name": b["name"], "input": map[string]any{}}})
			input, _ := json.Marshal(b["input"])
			w.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)}})
		default:
			panic(fmt.Sprintf("anthropicStream: a %v block", b["type"]))
		}
		w.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
	}
	w.event("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil}, "usage": usage.object()})
	w.event("message_stop", map[string]any{"type": "message_stop"})
	return reply{body: w.Bytes()}
}

func anthropicText(text string) map[string]any {
	return map[string]any{"type": "text", "text": text}
}

func anthropicThinking(thinking, signature string) map[string]any {
	return map[string]any{"type": "thinking", "thinking": thinking, "signature": signature}
}

func anthropicGH(id string, args ...string) map[string]any {
	return map[string]any{"type": "tool_use", "id": id, "name": "gh", "input": map[string]any{"args": args}}
}

// openaiUsage is a Responses API usage object.
type openaiUsage struct {
	Input, Cached, CacheWrite, Output, Reasoning int
}

func (u openaiUsage) object() map[string]any {
	return map[string]any{
		"input_tokens":          u.Input,
		"input_tokens_details":  map[string]any{"cached_tokens": u.Cached, "cache_write_tokens": u.CacheWrite},
		"output_tokens":         u.Output,
		"output_tokens_details": map[string]any{"reasoning_tokens": u.Reasoning},
		"total_tokens":          u.Input + u.Output,
	}
}

// openaiStream is the event stream the Responses API sends for a response of output items, each
// raw JSON as the response.output_item.done event carries it; incomplete, when set, is the reason
// the response did not complete.
func openaiStream(usage openaiUsage, incomplete string, items ...string) reply {
	seq := 0
	var w sseWriter
	event := func(name string, fields map[string]any) {
		fields["type"] = name
		fields["sequence_number"] = seq
		seq++
		w.event(name, fields)
	}
	response := func(status string, output []any, usage any) map[string]any {
		r := map[string]any{"id": "resp_standin", "object": "response", "created_at": 1, "status": status, "model": "gpt-standin", "output": output, "usage": usage}
		if incomplete != "" && status != "in_progress" {
			r["incomplete_details"] = map[string]any{"reason": incomplete}
		}
		return r
	}
	event("response.created", map[string]any{"response": response("in_progress", []any{}, nil)})
	var output []any
	for i, raw := range items {
		var item map[string]any
		if err := json.Unmarshal([]byte(raw), &item); err != nil {
			panic(err)
		}
		output = append(output, json.RawMessage(raw))
		id := item["id"]
		added := map[string]any{}
		for k, v := range item {
			added[k] = v
		}
		switch item["type"] {
		case "message":
			added["content"], added["status"] = []any{}, "in_progress"
			event("response.output_item.added", map[string]any{"output_index": i, "item": added})
			for ci, rc := range item["content"].([]any) {
				part := rc.(map[string]any)
				if part["type"] != "output_text" {
					continue
				}
				event("response.content_part.added", map[string]any{"item_id": id, "output_index": i, "content_index": ci, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
				event("response.output_text.delta", map[string]any{"item_id": id, "output_index": i, "content_index": ci, "delta": part["text"], "logprobs": []any{}})
				event("response.output_text.done", map[string]any{"item_id": id, "output_index": i, "content_index": ci, "text": part["text"], "logprobs": []any{}})
				event("response.content_part.done", map[string]any{"item_id": id, "output_index": i, "content_index": ci, "part": part})
			}
		case "function_call":
			added["arguments"], added["status"] = "", "in_progress"
			event("response.output_item.added", map[string]any{"output_index": i, "item": added})
			event("response.function_call_arguments.delta", map[string]any{"item_id": id, "output_index": i, "delta": item["arguments"]})
			event("response.function_call_arguments.done", map[string]any{"item_id": id, "output_index": i, "arguments": item["arguments"]})
		case "reasoning":
			added["summary"] = []any{}
			delete(added, "encrypted_content")
			event("response.output_item.added", map[string]any{"output_index": i, "item": added})
			for si, rs := range item["summary"].([]any) {
				s := rs.(map[string]any)
				event("response.reasoning_summary_part.added", map[string]any{"item_id": id, "output_index": i, "summary_index": si, "part": map[string]any{"type": "summary_text", "text": ""}})
				event("response.reasoning_summary_text.delta", map[string]any{"item_id": id, "output_index": i, "summary_index": si, "delta": s["text"]})
				event("response.reasoning_summary_text.done", map[string]any{"item_id": id, "output_index": i, "summary_index": si, "text": s["text"]})
				event("response.reasoning_summary_part.done", map[string]any{"item_id": id, "output_index": i, "summary_index": si, "part": s})
			}
		default:
			panic(fmt.Sprintf("openaiStream: a %v item", item["type"]))
		}
		event("response.output_item.done", map[string]any{"output_index": i, "item": json.RawMessage(raw)})
	}
	final := "completed"
	if incomplete != "" {
		final = "incomplete"
	}
	event("response."+final, map[string]any{"response": response(final, output, usage.object())})
	return reply{body: w.Bytes()}
}

func openaiReasoning(id, encrypted, summary string) string {
	return `{"id":"` + id + `","type":"reasoning","encrypted_content":` + quote(encrypted) + `,"summary":[{"type":"summary_text","text":` + quote(summary) + `}]}`
}

func openaiGH(id, callID string, args ...string) string {
	arguments, _ := json.Marshal(map[string]any{"args": args})
	return `{"id":"` + id + `","type":"function_call","status":"completed","call_id":"` + callID + `","name":"gh","arguments":` + quote(string(arguments)) + `}`
}

func openaiMessage(id, text string) string {
	return `{"id":"` + id + `","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":` + quote(text) + `,"annotations":[]}]}`
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
