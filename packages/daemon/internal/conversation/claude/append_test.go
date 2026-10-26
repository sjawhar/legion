package claude

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

func toolAnswer() conversation.Append {
	return conversation.Append{
		QuestionID: "q-7", Question: "What does the note say?",
		Provider: "anthropic", API: anthropicMessages, Model: fixtureModel,
		Turns: []conversation.Turn{
			{
				Content: []conversation.Block{
					thinking("Read it.", "c2lnLWE="),
					{Type: conversation.RedactedThinking, Data: "cmVk"},
					call("toolu_q7", "Read", `{"file_path":"note.txt"}`),
				},
				Results: []conversation.Block{{Type: conversation.ToolResult, ToolCallID: "toolu_q7", ToolName: "Read", Content: []conversation.Block{text("plum")}}},
				Usage:   conversation.Usage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 50, CacheWriteTokens: 10},
			},
			{
				Content: []conversation.Block{thinking("It says plum.", "c2lnLWI="), text("It says plum.")},
				Usage:   conversation.Usage{InputTokens: 130, OutputTokens: 6},
			},
		},
	}
}

func baseTranscript() *fixture {
	f := &fixture{}
	f.add(prompt("Read note.txt."))
	leaf := f.add(assistantLine("msg_1", `[{"type":"text","text":"Done."}]`))
	f.checkpoint(leaf)
	return f
}

// An append after a torn line starts with a newline, which leaves the torn line a malformed one
// of its own; its lines are Claude Code's: the question as a prompt line, one assistant line per
// block of a turn sharing a message id, a user line per tool result naming the line of its call,
// the answer, each with a fresh uuid chained from the session's leaf and the session's id,
// working directory, release and branch; then the last-prompt checkpoint naming the last of them.
// Read back, the session's conversation ends in the question, the turn, its result and the
// answer.
func TestEncodeAppendsAfterATornTailAsClaudeCodeWritesLines(t *testing.T) {
	f := baseTranscript()
	torn := `{"parentUuid":"x","isSidechain":false,"type":"user","message":{"role":"user","con`
	content := append(f.bytes(), []byte(torn)...)
	s := read(t, content)
	data, err := s.Encode(toolAnswer(), appendedAt)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, []byte("\n")) || !bytes.HasSuffix(data, []byte("\n")) {
		t.Fatalf("appended bytes %q, want them to start and end with a newline", data)
	}
	appended := append(append([]byte{}, content...), data...)
	lines := strings.Split(strings.TrimSuffix(string(appended), "\n"), "\n")
	added := lines[len(lines)-8:]
	if lines[len(lines)-9] != torn {
		t.Errorf("the torn line is %q, want it unchanged on its own line", lines[len(lines)-9])
	}

	var records []map[string]any
	for _, line := range added {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("appended line %q: %v", line, err)
		}
		records = append(records, record)
	}
	uuidPattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	parent := s.Leaf
	for i, record := range records[:7] {
		if !uuidPattern.MatchString(record["uuid"].(string)) || record["parentUuid"] != parent || record["timestamp"] != "2026-10-26T12:00:00.000Z" ||
			record["sessionId"] != "s-1" || record["cwd"] != "/work" || record["version"] != "2.1.310" || record["gitBranch"] != "main" || record["isSidechain"] != false {
			t.Errorf("line %d: %v; want a fresh uuid after %s, the session's id, cwd, version and branch", i, record, parent)
		}
		parent = record["uuid"].(string)
	}
	types := []string{"user", "assistant", "assistant", "assistant", "user", "assistant", "assistant", "last-prompt"}
	for i, record := range records {
		if record["type"] != types[i] {
			t.Errorf("line %d is a %v line, want %s", i, record["type"], types[i])
		}
	}
	message := func(i int) map[string]any { return records[i]["message"].(map[string]any) }
	if q := message(0); q["role"] != "user" || q["content"] != "What does the note say?" || records[0]["promptId"] == "" {
		t.Errorf("the question line's message %v", q)
	}
	firstTurn, secondTurn := message(1)["id"], message(5)["id"]
	for i := 1; i <= 3; i++ {
		if m := message(i); m["id"] != firstTurn || m["model"] != fixtureModel || m["stop_reason"] != "tool_use" || len(m["content"].([]any)) != 1 || records[i]["effort"] != "high" {
			t.Errorf("first turn line %d: %v, effort %v (want the session's, high)", i, m, records[i]["effort"])
		}
	}
	for i := 5; i <= 6; i++ {
		if m := message(i); m["id"] != secondTurn || m["stop_reason"] != "end_turn" {
			t.Errorf("second turn line %d: %v", i, m)
		}
	}
	if firstTurn == secondTurn || !strings.HasPrefix(firstTurn.(string), "msg_") {
		t.Errorf("turn message ids %v and %v, want two distinct msg_ ids", firstTurn, secondTurn)
	}
	toolResult := message(4)["content"].([]any)[0].(map[string]any)
	if toolResult["type"] != "tool_result" || toolResult["tool_use_id"] != "toolu_q7" || toolResult["content"] != "plum" ||
		records[4]["sourceToolAssistantUUID"] != records[3]["uuid"] || records[4]["toolUseResult"] != "plum" || records[4]["promptId"] != records[0]["promptId"] {
		t.Errorf("the tool result line %v", records[4])
	}
	if last := records[7]; last["leafUuid"] != records[6]["uuid"] || last["sessionId"] != "s-1" || last["lastPrompt"] != "What does the note say?" {
		t.Errorf("the last-prompt line %v, want it to name the answer's last line", last)
	}

	back := read(t, appended)
	assertMessages(t, back.Messages[2:], []conversation.Message{
		user(text("What does the note say?")),
		claude(thinking("Read it.", "c2lnLWE="), conversation.Block{Type: conversation.RedactedThinking, Data: "cmVk"}, call("toolu_q7", "Read", `{"file_path":"note.txt"}`)),
		result("toolu_q7", "Read", false, text("plum")),
		claude(thinking("It says plum.", "c2lnLWI="), text("It says plum.")),
	})
	if back.Leaf != records[6]["uuid"] {
		t.Errorf("the appended transcript's leaf %q, want the answer's last line", back.Leaf)
	}
}

// AppendFile writes the append in one write after exactly the content the session was read from,
// and refuses with ErrMoved once the file has grown since.
func TestAppendFileRefusesATranscriptThatMoved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s-1.jsonl")
	content := baseTranscript().bytes()
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	s := read(t, content)
	if err := AppendFile(path, s, toolAnswer(), appendedAt); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(written, content) || len(read(t, written).Messages) != 6 {
		t.Fatalf("the transcript after the append:\n%s", written)
	}
	if err := AppendFile(path, s, toolAnswer(), appendedAt); !errors.Is(err, conversation.ErrMoved) {
		t.Fatalf("a second append from the same read = %v, want ErrMoved", err)
	}
}

// A transcript holds Anthropic Messages turns only, so an answer from another provider API is
// refused, as is an Append Validate refuses.
func TestEncodeRefusesWhatClaudeCodeCannotHold(t *testing.T) {
	s := read(t, baseTranscript().bytes())
	other := toolAnswer()
	other.API = "openai-responses"
	if _, err := s.Encode(other, appendedAt); err == nil {
		t.Error("an OpenAI Responses answer was encoded")
	}
	invalid := toolAnswer()
	invalid.Question = ""
	if _, err := s.Encode(invalid, appendedAt); err == nil {
		t.Error("an answer to no question was encoded")
	}
}
