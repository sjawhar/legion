package agentloop

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"testing"
)

// The base64 payloads a replayed conversation carries, with the characters base64 holds that JSON
// could escape or a converter could re-encode.
const (
	screenshotData = "iVBORw0KGgo+/A=="
	diagramData    = "R0lGODlhAQABAIA+/A=="
	redactedData   = "EvAFCkYIBRgCKkCp+/redacted==ThInKiNg"
)

// blocksOfKind are every JSON object anywhere in v whose "type" is kind, in the order they appear.
func blocksOfKind(v any, kind string) []map[string]any {
	var found []map[string]any
	switch v := v.(type) {
	case map[string]any:
		if v["type"] == kind {
			found = append(found, v)
		}
		for _, name := range slices.Sorted(maps.Keys(v)) {
			found = append(found, blocksOfKind(v[name], kind)...)
		}
	case []any:
		for _, child := range v {
			found = append(found, blocksOfKind(child, kind)...)
		}
	}
	return found
}

// An image in a replayed user message and an image inside a replayed tool result both reach the
// Anthropic wire as base64 image blocks, with their media types and their data byte for byte.
func TestAnImageReplaysToTheWire(t *testing.T) {
	t.Parallel()
	s := newStandIn(t, anthropicStream("end_turn", anthropicUsage{Input: 3, Output: 4}, anthropicText("Both pictures say plum.")))
	f := newFixture(t, s, AnthropicMessages, 1)
	f.req.History = []Message{
		{Role: User, Blocks: []Block{
			{Kind: TextBlock, Text: "What does this say?"},
			{Kind: ImageBlock, MediaType: "image/png", Data: screenshotData},
		}},
		{Role: Assistant, Blocks: []Block{
			{Kind: ToolCallBlock, ToolCallID: "toolu_h1", Name: toolName, Input: json.RawMessage(`{"path":"diagram.gif"}`)},
		}},
		{Role: ToolRole, Blocks: []Block{{
			Kind: ToolResultBlock, ToolCallID: "toolu_h1", Name: toolName,
			Content: []Block{
				{Kind: TextBlock, Text: "the diagram"},
				{Kind: ImageBlock, MediaType: "image/gif", Data: diagramData},
			},
		}}},
	}
	result, err := Run(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Fault != "" {
		t.Fatalf("result %+v, want no fault: %s", result, result.Detail)
	}
	got := s.requests()
	if len(got) != 1 {
		t.Fatalf("%d model calls, want 1", len(got))
	}
	images := blocksOfKind(got[0].decoded(t), "image")
	want := []map[string]any{
		{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": screenshotData}},
		{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/gif", "data": diagramData}},
	}
	if !reflect.DeepEqual(images, want) {
		t.Errorf("the wire carries the images as\n%v\nwant the user message's and the tool result's\n%v", images, want)
	}
	for _, data := range []string{screenshotData, diagramData} {
		if !bytes.Contains(got[0].body, []byte(`"data":"`+data+`"`)) {
			t.Errorf("the call does not carry %s byte for byte", data)
		}
	}
}

// A replayed assistant text block's phase reaches the OpenAI Responses wire with the text it was
// produced with.
func TestAnAssistantTextPartsPhaseReachesTheWire(t *testing.T) {
	t.Parallel()
	s := newStandIn(t, openaiStream(openaiUsage{Input: 5, Output: 2}, "", openaiMessage("msg_1", "It still says plum.")))
	f := newFixture(t, s, OpenAIResponses, 1)
	f.req.History = []Message{
		{Role: User, Blocks: []Block{{Kind: TextBlock, Text: "Read note.txt."}}},
		{Role: Assistant, Blocks: []Block{{Kind: TextBlock, Text: "Reading it now.", Phase: "commentary"}}},
	}
	result, err := Run(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Fault != "" {
		t.Fatalf("result %+v, want no fault: %s", result, result.Detail)
	}
	got := s.requests()
	if len(got) != 1 {
		t.Fatalf("%d model calls, want 1", len(got))
	}
	var assistant []map[string]any
	for _, item := range inputItems(t, got[0]) {
		if item["role"] == "assistant" {
			assistant = append(assistant, item)
		}
	}
	want := []map[string]any{{"role": "assistant", "content": "Reading it now.", "phase": "commentary"}}
	if !reflect.DeepEqual(assistant, want) {
		t.Errorf("the wire's assistant items are %v, want the text with its phase %v", assistant, want)
	}
}

// The phase the provider gave a reply's text comes back on the step's text block, and reaches
// the wire again when the caller replays that step as the next run's history, so a turn this
// package carries goes back as it came. TestAnAssistantTextPartsPhaseReachesTheWire covers the
// phase a caller writes by hand; this one is the phase only the provider knew.
func TestAStepsTextKeepsTheProvidersPhaseAndReplaysIt(t *testing.T) {
	t.Parallel()
	s := newStandIn(t, openaiStream(openaiUsage{Input: 4, Output: 2}, "", openaiPhasedMessage("msg_p", "final_answer", "It says plum.")))
	result, err := Run(context.Background(), newFixture(t, s, OpenAIResponses, 1).req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Fault != "" {
		t.Fatalf("result %+v, want no fault: %s", result, result.Detail)
	}
	if len(result.Steps) != 1 {
		t.Fatalf("%d steps, want 1", len(result.Steps))
	}
	blocks := []Block{{Kind: TextBlock, Text: "It says plum.", Phase: "final_answer"}}
	if !reflect.DeepEqual(result.Steps[0].Blocks, blocks) {
		t.Fatalf("the step's blocks are %+v, want the text with the phase the provider gave it %+v", result.Steps[0].Blocks, blocks)
	}

	second := newStandIn(t, openaiStream(openaiUsage{Input: 6, Output: 2}, "", openaiMessage("msg_2", "It still says plum.")))
	g := newFixture(t, second, OpenAIResponses, 1)
	g.req.History = []Message{
		{Role: User, Blocks: []Block{{Kind: TextBlock, Text: "Read note.txt."}}},
		{Role: Assistant, Blocks: result.Steps[0].Blocks},
	}
	if result, err := Run(context.Background(), g.req); err != nil || result.Fault != "" {
		t.Fatalf("the run replaying the step: %+v, %v", result, err)
	}
	got := second.requests()
	if len(got) != 1 {
		t.Fatalf("%d model calls, want 1", len(got))
	}
	var assistant []map[string]any
	for _, item := range inputItems(t, got[0]) {
		if item["role"] == "assistant" {
			assistant = append(assistant, item)
		}
	}
	want := []map[string]any{{"role": "assistant", "content": "It says plum.", "phase": "final_answer"}}
	if !reflect.DeepEqual(assistant, want) {
		t.Errorf("the wire's assistant items are %v, want the step's text with its phase %v", assistant, want)
	}
}

// Redacted thinking a conversation kept reaches the Anthropic wire as a redacted thinking block,
// with its payload byte for byte, so the model is given its own turn back as it came.
func TestRedactedThinkingReplaysToTheWire(t *testing.T) {
	t.Parallel()
	s := newStandIn(t, anthropicStream("end_turn", anthropicUsage{Input: 2, Output: 3}, anthropicText("The note says plum.")))
	f := newFixture(t, s, AnthropicMessages, 1)
	f.req.History = []Message{
		{Role: User, Blocks: []Block{{Kind: TextBlock, Text: "Write the plan."}}},
		{Role: Assistant, Blocks: []Block{
			{Kind: ThinkingBlock, Redacted: redactedData},
			{Kind: TextBlock, Text: "The plan is written."},
		}},
	}
	result, err := Run(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Fault != "" {
		t.Fatalf("result %+v, want no fault: %s", result, result.Detail)
	}
	got := s.requests()
	if len(got) != 1 {
		t.Fatalf("%d model calls, want 1", len(got))
	}
	redacted := blocksOfKind(got[0].decoded(t), "redacted_thinking")
	want := []map[string]any{{"type": "redacted_thinking", "data": redactedData}}
	if !reflect.DeepEqual(redacted, want) {
		t.Errorf("the wire carries %v, want the redacted thinking %v", redacted, want)
	}
	if !bytes.Contains(got[0].body, []byte(`"data":"`+redactedData+`"`)) {
		t.Error("the call does not carry the redacted payload byte for byte")
	}
}
