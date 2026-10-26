package omp

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/conversation"
)

// responsesItem is one output item of a canned OpenAI Responses stream: the item as
// response.output_item.added announces it and as response.output_item.done completes it.
type responsesItem struct {
	added, done string
}

func reasoningItem(id, encrypted, summary string) responsesItem {
	return responsesItem{
		added: `{"id":"` + id + `","type":"reasoning","summary":[]}`,
		done:  `{"id":"` + id + `","type":"reasoning","encrypted_content":` + quote(encrypted) + `,"summary":[{"type":"summary_text","text":` + quote(summary) + `}]}`,
	}
}

func functionCallItem(id, callID, name, arguments string) responsesItem {
	return responsesItem{
		added: `{"id":"` + id + `","type":"function_call","status":"in_progress","call_id":"` + callID + `","name":"` + name + `","arguments":""}`,
		done:  `{"id":"` + id + `","type":"function_call","status":"completed","call_id":"` + callID + `","name":"` + name + `","arguments":` + quote(arguments) + `}`,
	}
}

func messageItem(id, text string) responsesItem {
	return responsesItem{
		added: `{"id":"` + id + `","type":"message","role":"assistant","status":"in_progress","content":[]}`,
		done:  `{"id":"` + id + `","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":` + quote(text) + `,"annotations":[]}]}`,
	}
}

// responsesStream is an OpenAI Responses streaming response of items.
func responsesStream(id string, items ...responsesItem) string {
	var b strings.Builder
	sequence := 0
	event := func(name, data string) {
		fmt.Fprintf(&b, "event: %s\ndata: {\"type\":%q,\"sequence_number\":%d,%s}\n\n", name, name, sequence, data)
		sequence++
	}
	response := func(status string) string {
		return `"response":{"id":"` + id + `","object":"response","created_at":1,"status":"` + status + `","model":"` + standInGPT +
			`","output":[],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":2}}}`
	}
	event("response.created", response("in_progress"))
	for i, item := range items {
		event("response.output_item.added", fmt.Sprintf(`"output_index":%d,"item":%s`, i, item.added))
		event("response.output_item.done", fmt.Sprintf(`"output_index":%d,"item":%s`, i, item.done))
	}
	event("response.completed", response("completed"))
	return b.String()
}

func newOpenAIStandIn(t *testing.T, replies ...string) *standIn {
	t.Helper()
	return newStandIn(t, responsesStream("resp_default", reasoningItem("rs_default", "gAAAAAresumed==", "Resuming."), messageItem("msg_default", "Resumed.")), replies...)
}

// The first run's two model calls on OpenAI Responses: reasoning and a read call, then reasoning
// and the answer.
func firstOpenAIRunReplies() []string {
	return []string{
		responsesStream("resp_1", reasoningItem("rs_1", "gAAAAENC+/one==", "Need the note."), functionCallItem("fc_1", "call_1", "read", `{"path":"note.txt"}`)),
		responsesStream("resp_2", reasoningItem("rs_2", "gAAAAENC+/two==", "It says plum."), messageItem("msg_2", "The note says plum.")),
	}
}

// requestInput is an OpenAI Responses request's input items, with the date and working directory
// reminder Oh My Pi puts on the first user item at request time cut from it (date-cwd-reminder.ts).
func requestInput(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var request struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatalf("decode the request: %v\n%s", err, body)
	}
	for _, item := range request.Input {
		if item["role"] != "user" {
			continue
		}
		var kept []any
		for _, part := range item["content"].([]any) {
			if text, _ := part.(map[string]any)["text"].(string); !strings.HasPrefix(text, "<system-reminder>\n") {
				kept = append(kept, part)
			}
		}
		item["content"] = kept
		break
	}
	return request.Input
}

// readerInput is the reader's conversation as the input items of a Responses request: a turn's
// native output items as Oh My Pi replays them, without their item id and status (its replay
// sanitizer drops both; every other field, encrypted_content included, goes as stored).
func readerInput(t *testing.T, messages []conversation.Message) []map[string]any {
	t.Helper()
	var input []map[string]any
	for _, m := range messages {
		switch m.Role {
		case conversation.User:
			var content []any
			for _, b := range m.Content {
				content = append(content, map[string]any{"type": "input_text", "text": b.Text})
			}
			input = append(input, map[string]any{"role": "user", "content": content})
		case conversation.Assistant:
			if m.Native == nil {
				t.Fatalf("an assistant turn of the model with no native items: %+v", m)
			}
			for _, raw := range m.Native.Items {
				var item map[string]any
				if err := json.Unmarshal(raw, &item); err != nil {
					t.Fatal(err)
				}
				delete(item, "id")
				delete(item, "status")
				input = append(input, item)
			}
		case conversation.Tool:
			result := m.Content[0]
			var texts []string
			for _, part := range result.Content {
				texts = append(texts, part.Text)
			}
			input = append(input, map[string]any{"type": "function_call_output", "call_id": result.ToolCallID, "output": strings.Join(texts, "\n")})
		}
	}
	return input
}

// assertResponsesResumeMatches checks the Responses request the pinned Oh My Pi sent on resuming
// a session against the reader's conversation from before the resume, then the follow-up.
func assertResponsesResumeMatches(t *testing.T, before *Session, body []byte, followUp string) []map[string]any {
	t.Helper()
	got := requestInput(t, body)
	want := append(readerInput(t, before.Messages), map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": followUp}}})
	observed, _ := json.Marshal(got)
	t.Logf("Oh My Pi's resume request input: %s", observed)
	wantJSON, _ := json.Marshal(want)
	var wantDecoded []map[string]any
	if err := json.Unmarshal(wantJSON, &wantDecoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, wantDecoded) {
		t.Errorf("Oh My Pi's resume request input:\n%s\nthe reader's conversation and the follow-up:\n%s", observed, wantJSON)
	}
	if before.Model != standInProvider+"/"+standInGPT || before.API != openaiResponses {
		t.Errorf("the reader's model %q, API %q", before.Model, before.API)
	}
	return got
}

// The pinned Oh My Pi on OpenAI Responses, resuming a session of reasoning and a tool call, sends
// each turn's output items as the session holds them, the reasoning items' encrypted content byte
// for byte, which is what the reader carries as the turns' native items.
func TestThePinnedOhMyPiReplaysTheOpenAIItemsTheReaderCarries(t *testing.T) {
	model := newOpenAIStandIn(t, firstOpenAIRunReplies()...)
	rig := newOMPRig(t, openaiResponses, model)
	rig.run(t, "", "Read note.txt and tell me what it says.")
	file := rig.sessionFile(t)
	before, err := ReadFile(file, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var encrypted []string
	for _, m := range before.Messages {
		if m.Native == nil {
			continue
		}
		for _, item := range m.Native.Items {
			var reasoning struct {
				Type             string `json:"type"`
				EncryptedContent string `json:"encrypted_content"`
			}
			if json.Unmarshal(item, &reasoning) == nil && reasoning.Type == "reasoning" {
				encrypted = append(encrypted, reasoning.EncryptedContent)
			}
		}
	}
	if strings.Join(encrypted, " ") != "gAAAAENC+/one== gAAAAENC+/two==" {
		t.Errorf("the reader's reasoning items carry %q, want the model's encrypted content as it came", encrypted)
	}

	rig.run(t, file, "What colour is the fruit?")
	assertResponsesResumeMatches(t, before, model.last(t), "What colour is the fruit?")
}

// An answer appended on OpenAI Responses reaches the model when the pinned Oh My Pi resumes: its
// next request carries the question, the reasoning items with their encrypted content as the
// append wrote them, the call, its output and the answer, once each and in order.
func TestThePinnedOhMyPiResumesAnAppendedOpenAIAnswer(t *testing.T) {
	model := newOpenAIStandIn(t, firstOpenAIRunReplies()...)
	rig := newOMPRig(t, openaiResponses, model)
	rig.run(t, "", "Read note.txt and tell me what it says.")
	file := rig.sessionFile(t)
	before, err := ReadFile(file, Options{})
	if err != nil {
		t.Fatal(err)
	}
	item := func(raw string) json.RawMessage { return json.RawMessage(raw) }
	answer := conversation.Append{
		QuestionID: "q-1", Question: "Which fruit does the note name?",
		Provider: standInProvider, API: openaiResponses, Model: standInGPT,
		Turns: []conversation.Turn{
			{
				Content: []conversation.Block{thinking("Check the note.", ""), call("call_append_1", "read", `{"path":"note.txt"}`)},
				Native: []json.RawMessage{
					item(reasoningItem("rs_append_1", "gAAAAAPPEND+/1==", "Check the note.").done),
					item(functionCallItem("fc_append_1", "call_append_1", "read", `{"path":"note.txt"}`).done),
				},
				Results: []conversation.Block{{Type: conversation.ToolResult, ToolCallID: "call_append_1", ToolName: "read", Content: []conversation.Block{text("plum")}}},
				Usage:   conversation.Usage{InputTokens: 12, OutputTokens: 8, ReasoningTokens: 3},
			},
			{
				Content: []conversation.Block{thinking("It names plum.", ""), text("The note names plum.")},
				Native: []json.RawMessage{
					item(reasoningItem("rs_append_2", "gAAAAAPPEND+/2==", "It names plum.").done),
					item(messageItem("msg_append_2", "The note names plum.").done),
				},
				Usage: conversation.Usage{InputTokens: 20, OutputTokens: 5},
			},
		},
	}
	if err := AppendFile(file, before, answer, time.Now()); err != nil {
		t.Fatal(err)
	}
	appended, err := ReadFile(file, Options{})
	if err != nil {
		t.Fatal(err)
	}
	rig.run(t, file, "Thanks.")
	input := assertResponsesResumeMatches(t, appended, model.last(t), "Thanks.")

	find := func(match func(map[string]any) bool, name string) int {
		t.Helper()
		index, count := -1, 0
		for i, item := range input {
			if match(item) {
				index = i
				count++
			}
		}
		if count != 1 {
			t.Errorf("the request carries %s %d times, want once", name, count)
		}
		return index
	}
	userText := func(item map[string]any) string {
		parts, _ := item["content"].([]any)
		if item["role"] != "user" || len(parts) != 1 {
			return ""
		}
		text, _ := parts[0].(map[string]any)["text"].(string)
		return text
	}
	question := find(func(item map[string]any) bool { return userText(item) == answer.Question }, "the question")
	firstReasoning := find(func(item map[string]any) bool { return item["encrypted_content"] == "gAAAAAPPEND+/1==" }, "the first reasoning item")
	toolCall := find(func(item map[string]any) bool {
		return item["type"] == "function_call" && item["call_id"] == "call_append_1"
	}, "the call")
	output := find(func(item map[string]any) bool {
		return item["type"] == "function_call_output" && item["call_id"] == "call_append_1" && item["output"] == "plum"
	}, "the call's output")
	secondReasoning := find(func(item map[string]any) bool { return item["encrypted_content"] == "gAAAAAPPEND+/2==" }, "the second reasoning item")
	finalAnswer := find(func(item map[string]any) bool {
		content, _ := item["content"].([]any)
		return item["type"] == "message" && len(content) == 1 && content[0].(map[string]any)["text"] == "The note names plum."
	}, "the answer")
	order := []int{question, firstReasoning, toolCall, output, secondReasoning, finalAnswer, len(input) - 1}
	for i := 1; i < len(order); i++ {
		if order[i-1] >= order[i] {
			t.Errorf("the question, reasoning, call, output, reasoning, answer and follow-up at %v of %d items; want them in that order", order, len(input))
			break
		}
	}
}
