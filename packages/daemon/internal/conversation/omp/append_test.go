package omp

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/conversation"
)

var appendedAt = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func claudeAnswer() conversation.Append {
	return conversation.Append{
		QuestionID: "q-7", Question: "What does the note say?",
		Provider: "anthropic", API: anthropicMessages, Model: "claude-opus-5-5",
		Turns: []conversation.Turn{
			{
				Content: []conversation.Block{
					thinking("Read it.", "c2lnLWE="),
					{Type: conversation.RedactedThinking, Data: "cmVk"},
					call("toolu_q7", "read", `{"path":"note.txt"}`),
				},
				Results: []conversation.Block{{Type: conversation.ToolResult, ToolCallID: "toolu_q7", ToolName: "read", Content: []conversation.Block{text("plum")}}},
				Usage:   conversation.Usage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 50, CacheWriteTokens: 10, InputCost: 0.25, OutputCost: 0.5},
			},
			{
				Content: []conversation.Block{thinking("It says plum.", "c2lnLWI="), text("It says plum.")},
				Usage:   conversation.Usage{InputTokens: 130, OutputTokens: 6},
			},
		},
	}
}

func baseSession() *fixture {
	f := newFixture()
	f.add(`{"type":"model_change","model":"anthropic/claude-opus-5-5"}`)
	f.add(userEntry("Read note.txt."))
	f.add(claudeEntry("stop", `[{"type":"text","text":"Done."}]`))
	return f
}

// An append after a torn record starts with a newline, which leaves the torn record a malformed
// line on its own; its entries chain from the session's leaf with fresh 8-hex ids, as Oh My Pi
// writes them (the question, the turn and its result, the answer), and end with the legion-answer
// entry naming the question, which the next read keeps out of the conversation.
func TestEncodeAppendsAfterATornTailAsOhMyPiWritesEntries(t *testing.T) {
	f := baseSession()
	torn := `{"type":"message","id":"torn0001","parentId":"` + f.last + `","message":{"role":"user","con`
	content := append(f.bytes(), []byte(torn)...)
	s := read(t, content)
	data, err := s.Encode(claudeAnswer(), appendedAt)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, []byte("\n")) || !bytes.HasSuffix(data, []byte("\n")) {
		t.Fatalf("appended bytes %q, want them to start and end with a newline", data)
	}
	appended := append(append([]byte{}, content...), data...)
	lines := strings.Split(strings.TrimSuffix(string(appended), "\n"), "\n")
	if lines[len(lines)-6] != torn {
		t.Errorf("the torn record's line is %q, want it unchanged on its own line", lines[len(lines)-6])
	}

	var entries []map[string]any
	for _, line := range lines[len(lines)-5:] {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("appended line %q: %v", line, err)
		}
		entries = append(entries, entry)
	}
	parent := f.last
	hex8 := regexp.MustCompile(`^[0-9a-f]{8}$`)
	for i, entry := range entries {
		if !hex8.MatchString(entry["id"].(string)) || entry["parentId"] != parent || entry["timestamp"] != "2026-10-26T12:00:00.000Z" {
			t.Errorf("entry %d: id %v, parent %v, timestamp %v; want 8 hex characters after %s", i, entry["id"], entry["parentId"], entry["timestamp"], parent)
		}
		parent = entry["id"].(string)
	}
	roles := []string{}
	for _, entry := range entries[:4] {
		roles = append(roles, entry["message"].(map[string]any)["role"].(string))
	}
	if strings.Join(roles, ",") != "user,assistant,toolResult,assistant" {
		t.Errorf("message roles %v", roles)
	}
	first := entries[1]["message"].(map[string]any)
	if first["api"] != anthropicMessages || first["provider"] != "anthropic" || first["model"] != "claude-opus-5-5" || first["stopReason"] != "toolUse" {
		t.Errorf("the tool turn = %v", first)
	}
	usage := first["usage"].(map[string]any)
	if usage["input"] != 100.0 || usage["cacheRead"] != 50.0 || usage["totalTokens"] != 180.0 || usage["cost"].(map[string]any)["total"] != 0.75 {
		t.Errorf("the tool turn's usage = %v", usage)
	}
	if got := entries[3]["message"].(map[string]any)["stopReason"]; got != "stop" {
		t.Errorf("the answer's stop reason = %v", got)
	}
	if entries[4]["type"] != "custom" || entries[4]["customType"] != AnswerEntryType || entries[4]["data"].(map[string]any)["questionId"] != "q-7" {
		t.Errorf("the last entry = %v, want the legion-answer entry naming q-7", entries[4])
	}

	again := read(t, appended)
	assertMessages(t, again.Messages[2:], []conversation.Message{
		user("What does the note say?"),
		claude(thinking("Read it.", "c2lnLWE="), conversation.Block{Type: conversation.RedactedThinking, Data: "cmVk"}, call("toolu_q7", "read", `{"path":"note.txt"}`)),
		result("toolu_q7", "read", "plum", false),
		claude(thinking("It says plum.", "c2lnLWI="), text("It says plum.")),
	})
	if again.Leaf != entries[4]["id"] || again.Torn {
		t.Errorf("leaf %q, torn %t; want the answer entry, not torn", again.Leaf, again.Torn)
	}
}

// An OpenAI Responses turn is written as Oh My Pi's provider records one: the output items as the
// openaiResponsesHistory payload byte for byte, a thinking block per reasoning item naming it, a
// text block per message item with its id in a text signature, a tool call per function_call item
// under "call_id|item_id", and the result under that id.
func TestEncodeWritesOpenAIResponsesTurnsFromTheirItems(t *testing.T) {
	reasoning := `{"id":"rs_1","type":"reasoning","encrypted_content":"gAAAA+/x==","summary":[{"type":"summary_text","text":"Weigh it."}]}`
	functionCall := `{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_1","name":"read","arguments":"{\"path\":\"note.txt\"}"}`
	message := `{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Plum.","annotations":[]}]}`
	a := conversation.Append{
		QuestionID: "q-8", Question: "What does it say?", Provider: "openai", API: openaiResponses, Model: "gpt-6.1",
		Turns: []conversation.Turn{
			{
				Content: []conversation.Block{thinking("Weigh it.", ""), call("call_1", "read", `{"path":"note.txt"}`)},
				Native:  []json.RawMessage{json.RawMessage(reasoning), json.RawMessage(functionCall)},
				Results: []conversation.Block{{Type: conversation.ToolResult, ToolCallID: "call_1", ToolName: "read", Content: []conversation.Block{text("plum")}}},
			},
			{Content: []conversation.Block{text("Plum.")}, Native: []json.RawMessage{json.RawMessage(message)}},
		},
	}
	f := newFixture()
	f.add(`{"type":"model_change","model":"openai/gpt-6.1"}`)
	f.add(userEntry("hello"))
	f.add(assistantEntry("openai", openaiResponses, "gpt-6.1", "stop", `[{"type":"text","text":"hi"}]`))
	s := read(t, f.bytes())
	data, err := s.Encode(a, appendedAt)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	var turn, toolResult struct {
		Message struct {
			Content         []map[string]any `json:"content"`
			ToolCallID      string           `json:"toolCallId"`
			ProviderPayload struct {
				Type     string            `json:"type"`
				Provider string            `json:"provider"`
				DT       bool              `json:"dt"`
				Items    []json.RawMessage `json:"items"`
			} `json:"providerPayload"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &turn); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[2]), &toolResult); err != nil {
		t.Fatal(err)
	}
	payload := turn.Message.ProviderPayload
	if payload.Type != "openaiResponsesHistory" || payload.Provider != "openai" || !payload.DT || len(payload.Items) != 2 ||
		string(payload.Items[0]) != reasoning || string(payload.Items[1]) != functionCall {
		t.Errorf("payload = %+v, want the two items byte for byte", payload)
	}
	content := turn.Message.Content
	if len(content) != 2 || content[0]["type"] != "thinking" || content[0]["thinking"] != "Weigh it." || content[0]["itemId"] != "rs_1" || content[0]["summary"] != true ||
		content[1]["type"] != "toolCall" || content[1]["id"] != "call_1|fc_1" || content[1]["arguments"].(map[string]any)["path"] != "note.txt" {
		t.Errorf("content = %v", content)
	}
	if toolResult.Message.ToolCallID != "call_1|fc_1" {
		t.Errorf("the result's tool call id = %q, want call_1|fc_1", toolResult.Message.ToolCallID)
	}
	if !strings.Contains(lines[3], `"textSignature":"{\"v\":1,\"id\":\"msg_1\"}"`) {
		t.Errorf("the answer = %s, want its text signed with the message item's id", lines[3])
	}

	again := read(t, append(f.bytes(), data...))
	last := again.Messages[len(again.Messages)-1]
	if last.Native == nil || string(last.Native.Items[0]) != message || last.Content[0].Text != "Plum." {
		t.Errorf("the answer read back = %+v", last)
	}
}

// A file append is one write after exactly the content the session was read at: a file that grew
// since is refused with ErrMoved and left as it is.
func TestAppendFileRefusesASessionThatMoved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	f := baseSession()
	if err := os.WriteFile(path, f.bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := ReadFile(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := AppendFile(path, s, claudeAnswer(), appendedAt); err != nil {
		t.Fatal(err)
	}
	after, err := ReadFile(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := after.Messages[len(after.Messages)-1].Content; len(got) != 2 || got[1].Text != "It says plum." {
		t.Errorf("the session's last message after the append = %+v", got)
	}

	grown, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	err = AppendFile(path, s, claudeAnswer(), appendedAt)
	if !errors.Is(err, conversation.ErrMoved) {
		t.Errorf("a second append from the first read = %v, want ErrMoved", err)
	}
	if now, _ := os.ReadFile(path); !bytes.Equal(now, grown) {
		t.Error("the refused append changed the file")
	}
}

// An append the session's format cannot hold is refused before anything is written.
func TestEncodeRefusesAnAppendItCannotWrite(t *testing.T) {
	s := read(t, baseSession().bytes())
	unanswered := claudeAnswer()
	unanswered.Turns = unanswered.Turns[:1]
	if _, err := s.Encode(unanswered, appendedAt); err == nil {
		t.Error("an append whose last turn calls a tool encoded")
	}
	otherAPI := claudeAnswer()
	otherAPI.API = "google-generative-ai"
	if _, err := s.Encode(otherAPI, appendedAt); err == nil {
		t.Error("an append on an API the appender does not write encoded")
	}
	noItems := claudeAnswer()
	noItems.API = openaiResponses
	if _, err := s.Encode(noItems, appendedAt); err == nil {
		t.Error("an OpenAI Responses append with no output items encoded")
	}
}
