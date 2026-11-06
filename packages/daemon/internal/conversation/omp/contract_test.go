package omp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/conversation"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/testbin"
	"github.com/sjawhar/legion/daemon/internal/testpg"
)

// The stand-in model the real-binary tests point the pinned Oh My Pi at: a models.yml provider on
// the Anthropic Messages API whose base URL is a local server that records each request and
// answers a canned stream.
const (
	standInProvider = "standin"
	standInModel    = "claude-test"
	standInGPT      = "gpt-test"
)

// standInEndpoint is how the stand-in's endpoint treats replayed thinking: a reasoning model on an
// Anthropic-compatible endpoint that is not Anthropic's own API, for which Oh My Pi's compat
// replays unsigned thinking.
var standInEndpoint = Options{ReplayUnsignedThinking: true}

// standIn is a local model endpoint: it records every request body and answers each with the next
// canned server-sent-event stream, or, once they run out, its fallback stream.
type standIn struct {
	server   *httptest.Server
	mu       sync.Mutex
	requests [][]byte
	replies  []string
}

func newStandIn(t *testing.T, fallback string, replies ...string) *standIn {
	t.Helper()
	s := &standIn{replies: replies}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.requests = append(s.requests, body)
		reply := fallback
		if len(s.replies) > 0 {
			reply, s.replies = s.replies[0], s.replies[1:]
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(s.server.Close)
	return s
}

// newAnthropicStandIn is a stand-in Anthropic Messages endpoint whose fallback is a short signed
// answer.
func newAnthropicStandIn(t *testing.T, replies ...string) *standIn {
	t.Helper()
	return newStandIn(t, anthropicStream("msg_default", "end_turn", sseThinking("Resumed.", "cmVzdW1lZA=="), sseText("Resumed.")), replies...)
}

// last is the body of the last request.
func (s *standIn) last(t *testing.T) []byte {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		t.Fatal("the stand-in model received no request")
	}
	return s.requests[len(s.requests)-1]
}

// lastAnthropic is the last request, decoded as an Anthropic Messages request.
func (s *standIn) lastAnthropic(t *testing.T) anthropicRequest {
	t.Helper()
	body := s.last(t)
	var request anthropicRequest
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatalf("decode the request: %v\n%s", err, body)
	}
	return request
}

// sseBlock is one content block of a canned stream.
type sseBlock struct {
	start  string
	deltas []string
}

func sseThinking(thinking, signature string) sseBlock {
	return sseBlock{
		start: `{"type":"thinking","thinking":"","signature":""}`,
		deltas: []string{
			`{"type":"thinking_delta","thinking":` + quote(thinking) + `}`,
			`{"type":"signature_delta","signature":` + quote(signature) + `}`,
		},
	}
}

func sseRedacted(data string) sseBlock {
	return sseBlock{start: `{"type":"redacted_thinking","data":` + quote(data) + `}`}
}

func sseText(text string) sseBlock {
	return sseBlock{start: `{"type":"text","text":""}`, deltas: []string{`{"type":"text_delta","text":` + quote(text) + `}`}}
}

func sseToolUse(id, name, input string) sseBlock {
	return sseBlock{
		start:  `{"type":"tool_use","id":` + quote(id) + `,"name":` + quote(name) + `,"input":{}}`,
		deltas: []string{`{"type":"input_json_delta","partial_json":` + quote(input) + `}`},
	}
}

// anthropicStream is an Anthropic Messages streaming response of blocks.
func anthropicStream(id, stopReason string, blocks ...sseBlock) string {
	var b strings.Builder
	event := func(name, data string) { fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", name, data) }
	event("message_start", `{"type":"message_start","message":{"id":"`+id+`","type":"message","role":"assistant","model":"`+standInModel+
		`","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1}}}`)
	for i, block := range blocks {
		event("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":%s}`, i, block.start))
		for _, delta := range block.deltas {
			event("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":%s}`, i, delta))
		}
		event("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, i))
	}
	event("message_delta", `{"type":"message_delta","delta":{"stop_reason":"`+stopReason+`","stop_sequence":null},"usage":{"output_tokens":20}}`)
	event("message_stop", `{"type":"message_stop"}`)
	return b.String()
}

// ompRig is a fresh HOME whose one model is the stand-in on api, a working directory holding
// note.txt, and the directory sessions go to.
type ompRig struct {
	omp, home, work, sessions string
	env                       []string
}

// standInModelOf is the stand-in's model id on api: a Claude id on Anthropic Messages, a GPT id on
// OpenAI Responses, so Oh My Pi's dialect for each is its family's.
func standInModelOf(api string) string {
	if api == openaiResponses {
		return standInGPT
	}
	return standInModel
}

func newOMPRig(t *testing.T, api string, model *standIn, extraEnv ...string) *ompRig {
	t.Helper()
	omp := testbin.OMP(t)
	dir := t.TempDir()
	rig := &ompRig{omp: omp, home: filepath.Join(dir, "home"), work: filepath.Join(dir, "work"), sessions: filepath.Join(dir, "sessions")}
	testbin.OMPHome(t, omp, rig.home)
	agent := filepath.Join(rig.home, ".omp", "agent")
	baseURL := model.server.URL
	if api == openaiResponses {
		baseURL += "/v1"
	}
	for path, content := range map[string]string{
		filepath.Join(agent, "models.yml"): "providers:\n  " + standInProvider + ":\n    baseUrl: " + baseURL +
			"\n    auth: apiKey\n    api: " + api + "\n    apiKey: unused\n    models:\n      - id: " + standInModelOf(api) +
			"\n        name: Stand-in\n        reasoning: true\n",
		filepath.Join(agent, "config.yml"):  "modelRoles:\n  default: " + standInProvider + "/" + standInModelOf(api) + "\n",
		filepath.Join(rig.work, "note.txt"): "plum\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rig.env = append([]string{"HOME=" + rig.home, "PATH=/usr/bin:/bin", "AWS_EC2_METADATA_DISABLED=true"}, extraEnv...)
	return rig
}

// run runs `omp -p` with prompt, resuming the session at resume when it is not empty.
func (r *ompRig) run(t *testing.T, resume, prompt string) {
	t.Helper()
	args := []string{"-p", "--session-dir", r.sessions, "--no-title", "--no-extensions", "--no-skills", "--no-rules", "--tools", "read"}
	if resume != "" {
		args = append(args, "--resume", resume)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.omp, append(args, prompt)...)
	cmd.Dir, cmd.Env = r.work, r.env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("omp %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// sessionFile is the one session file the rig's runs wrote.
func (r *ompRig) sessionFile(t *testing.T) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(r.sessions, "*.jsonl"))
	if err != nil || len(files) != 1 {
		t.Fatalf("session files under %s = %v, %v; want one", r.sessions, files, err)
	}
	return files[0]
}

// The first run's two model calls: a turn that thinks (signed and redacted), says it reads the
// note and calls read, then an answer.
func firstRunReplies() []string {
	return []string{
		anthropicStream("msg_1", "tool_use", sseThinking("I should read the note.", "c2lnLW9uZQ=="), sseRedacted("cmVkYWN0ZWQtb25l"),
			sseText("Reading the note."), sseToolUse("toolu_01ABC", "read", `{"path":"note.txt"}`)),
		anthropicStream("msg_2", "end_turn", sseThinking("The note says plum.", "c2lnLXR3bw=="), sseText("The note says plum.")),
	}
}

// anthropicRequest is the part of an Anthropic Messages request the tests compare.
type anthropicRequest struct {
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

// wireTurn is one Anthropic message reduced to what the comparison checks: its role and each
// block's type, text, signature or data, tool id, name and input, and result.
type wireTurn struct {
	Role   string      `json:"role"`
	Blocks []wireBlock `json:"blocks"`
}

type wireBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Input     any    `json:"input,omitempty"`
	Result    string `json:"result,omitempty"`
	IsError   bool   `json:"isError,omitempty"`
}

// turns reduces the request's messages. The date and working directory reminder Oh My Pi puts on
// the first user turn of every request is not part of the session (date-cwd-reminder.ts: "built
// at request time (never stored in the session)"), so it is cut from that turn.
func (request anthropicRequest) turns(t *testing.T) []wireTurn {
	t.Helper()
	var turns []wireTurn
	for _, message := range request.Messages {
		turn := wireTurn{Role: message.Role}
		var blocks []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			Thinking  string          `json:"thinking"`
			Signature string          `json:"signature"`
			Data      string          `json:"data"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     any             `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
			IsError   bool            `json:"is_error"`
		}
		var text string
		if json.Unmarshal(message.Content, &text) == nil {
			turn.Blocks = []wireBlock{{Type: "text", Text: text}}
		} else if err := json.Unmarshal(message.Content, &blocks); err != nil {
			t.Fatalf("decode a request message: %v\n%s", err, message.Content)
		}
		for _, b := range blocks {
			switch b.Type {
			case "text":
				turn.Blocks = append(turn.Blocks, wireBlock{Type: "text", Text: b.Text})
			case "thinking":
				turn.Blocks = append(turn.Blocks, wireBlock{Type: "thinking", Text: b.Thinking, Signature: b.Signature})
			case "redacted_thinking":
				turn.Blocks = append(turn.Blocks, wireBlock{Type: "redacted_thinking", Data: b.Data})
			case "tool_use":
				turn.Blocks = append(turn.Blocks, wireBlock{Type: "tool_use", ID: b.ID, Name: b.Name, Input: b.Input})
			case "tool_result":
				turn.Blocks = append(turn.Blocks, wireBlock{Type: "tool_result", ID: b.ToolUseID, Result: resultText(t, b.Content), IsError: b.IsError})
			default:
				t.Fatalf("a request block of type %q", b.Type)
			}
		}
		turns = append(turns, turn)
	}
	for i := range turns {
		if turns[i].Role != "user" {
			continue
		}
		first := &turns[i].Blocks[0]
		if strings.HasPrefix(first.Text, "<system-reminder>\n") {
			if end := strings.Index(first.Text, "\n</system-reminder>"); end >= 0 {
				first.Text = strings.TrimLeft(first.Text[end+len("\n</system-reminder>"):], "\n")
			}
			if first.Text == "" {
				turns[i].Blocks = turns[i].Blocks[1:]
			}
		}
		break
	}
	return turns
}

func resultText(t *testing.T, content json.RawMessage) string {
	t.Helper()
	var text string
	if json.Unmarshal(content, &text) == nil {
		return text
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &parts); err != nil {
		t.Fatalf("decode a tool result's content: %v", err)
	}
	var texts []string
	for _, part := range parts {
		texts = append(texts, part.Text)
	}
	return strings.Join(texts, "\n")
}

// readerTurns reduces the reader's messages the same way, grouped as an Anthropic request groups
// them: each run of tool results one user turn.
func readerTurns(t *testing.T, messages []conversation.Message) []wireTurn {
	t.Helper()
	var turns []wireTurn
	for _, m := range messages {
		var blocks []wireBlock
		for _, b := range m.Content {
			switch b.Type {
			case conversation.Text:
				blocks = append(blocks, wireBlock{Type: "text", Text: b.Text})
			case conversation.Thinking:
				blocks = append(blocks, wireBlock{Type: "thinking", Text: b.Text, Signature: b.Signature})
			case conversation.RedactedThinking:
				blocks = append(blocks, wireBlock{Type: "redacted_thinking", Data: b.Data})
			case conversation.ToolCall:
				var input any
				if err := json.Unmarshal(b.Arguments, &input); err != nil {
					t.Fatal(err)
				}
				blocks = append(blocks, wireBlock{Type: "tool_use", ID: b.ToolCallID, Name: b.ToolName, Input: input})
			case conversation.ToolResult:
				var texts []string
				for _, part := range b.Content {
					texts = append(texts, part.Text)
				}
				blocks = append(blocks, wireBlock{Type: "tool_result", ID: b.ToolCallID, Result: strings.Join(texts, "\n"), IsError: b.IsError})
			default:
				t.Fatalf("a reader block of type %q", b.Type)
			}
		}
		if m.Role == conversation.Tool && len(turns) > 0 && turns[len(turns)-1].Role == "user" &&
			turns[len(turns)-1].Blocks[0].Type == "tool_result" {
			turns[len(turns)-1].Blocks = append(turns[len(turns)-1].Blocks, blocks...)
			continue
		}
		role := string(m.Role)
		if m.Role == conversation.Tool {
			role = "user"
		}
		turns = append(turns, wireTurn{Role: role, Blocks: blocks})
	}
	return turns
}

// assertResumeMatches checks the request the pinned Oh My Pi sent on resuming a session against
// what the reader rebuilt from the session before the resume: the request is the rebuilt
// conversation, then the follow-up.
func assertResumeMatches(t *testing.T, before *Session, request anthropicRequest, followUp string) {
	t.Helper()
	got := request.turns(t)
	want := append(readerTurns(t, before.Messages), wireTurn{Role: "user", Blocks: []wireBlock{{Type: "text", Text: followUp}}})
	observed, _ := json.Marshal(got)
	t.Logf("Oh My Pi's resume request, reduced: %s", observed)
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.MarshalIndent(got, "", "  ")
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("Oh My Pi's resume request:\n%s\nthe reader's conversation and the follow-up:\n%s", gotJSON, wantJSON)
	}
	if before.Model != standInProvider+"/"+standInModel || before.API != anthropicMessages {
		t.Errorf("the reader's model %q, API %q", before.Model, before.API)
	}
}

// The pinned Oh My Pi, resuming a session it wrote with a tool call and signed and redacted
// thinking in it, sends the stand-in model the conversation the reader rebuilds from the session:
// the same turns, roles, block order, text, tool ids, thinking signatures and redacted data.
func TestThePinnedOhMyPiResumesTheConversationTheReaderRebuilds(t *testing.T) {
	model := newAnthropicStandIn(t, firstRunReplies()...)
	rig := newOMPRig(t, anthropicMessages, model)
	rig.run(t, "", "Read note.txt and tell me what it says.")
	file := rig.sessionFile(t)
	before, err := ReadFile(file, standInEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Messages) != 4 {
		t.Fatalf("the reader rebuilt %d messages from the first run, want the request, the tool turn, its result and the answer", len(before.Messages))
	}

	rig.run(t, file, "What colour is the fruit?")
	assertResumeMatches(t, before, model.lastAnthropic(t), "What colour is the fruit?")
}

// The pinned Oh My Pi and the reader rebuild alike a session that, after a real first run, holds
// records written by hand: a compaction (its summary in Oh My Pi's template, the entries it keeps
// after it), a tool call interrupted before its result, an aborted turn and its result, a torn last
// record, a branch summary, an extension's custom message. Each resumed request is the reader's
// conversation, then the follow-up.
func TestThePinnedOhMyPiResumesHandWrittenRecordsAsTheReaderRebuildsThem(t *testing.T) {
	stamp := func() string { return isoTime(time.Now()) }
	for _, testCase := range []struct {
		name string
		// records is what is written after the first run: entries is the first run's entry ids
		// by kind, leaf its last entry's id.
		records func(entries map[string]string, leaf string) string
	}{
		{"a compaction", func(entries map[string]string, leaf string) string {
			return `{"type":"compaction","id":"c0c0c0c0","parentId":"` + leaf + `","timestamp":"` + stamp() +
				`","summary":"The user asked what note.txt says.\n\n\n\n| It | said |\n|--|--|\n| plum |  |  ","firstKeptEntryId":"` + entries["toolResult"] + `","tokensBefore":1000}` + "\n"
		}},
		{"an interrupted tool call", func(_ map[string]string, leaf string) string {
			return `{"type":"message","id":"1a1a1a1a","parentId":"` + leaf + `","timestamp":"` + stamp() + `","message":{"role":"assistant","content":[` +
				`{"type":"thinking","thinking":"Read it again.","thinkingSignature":"aW50ZXJydXB0ZWQ="},{"type":"redactedThinking","data":"cmVk"},` +
				`{"type":"text","text":"Reading again."},{"type":"toolCall","id":"toolu_interrupted","name":"read","arguments":{"path":"note.txt"}}],` +
				`"api":"anthropic-messages","provider":"` + standInProvider + `","model":"` + standInModel + `","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"toolUse","timestamp":` +
				fmt.Sprint(time.Now().UnixMilli()) + `}}` + "\n"
		}},
		{"an aborted turn and its result", func(_ map[string]string, leaf string) string {
			ms := fmt.Sprint(time.Now().UnixMilli())
			return `{"type":"message","id":"2b2b2b2b","parentId":"` + leaf + `","timestamp":"` + stamp() + `","message":{"role":"assistant","content":[` +
				`{"type":"text","text":"Reading once more."},{"type":"toolCall","id":"toolu_aborted","name":"read","arguments":{"path":"note.txt"}}],` +
				`"api":"anthropic-messages","provider":"` + standInProvider + `","model":"` + standInModel + `","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"aborted","timestamp":` + ms + `}}` + "\n" +
				`{"type":"message","id":"3c3c3c3c","parentId":"2b2b2b2b","timestamp":"` + stamp() + `","message":{"role":"toolResult","toolCallId":"toolu_aborted","toolName":"read","content":[{"type":"text","text":"aborted"}],"isError":true,"timestamp":` + ms + `}}` + "\n"
		}},
		{"a torn last record", func(_ map[string]string, leaf string) string {
			return `{"type":"message","id":"4d4d4d4d","parentId":"` + leaf + `","timestamp":"` + stamp() + `","message":{"role":"user","content":[{"type":"text","text":"cut o`
		}},
		{"a branch summary", func(entries map[string]string, _ string) string {
			return `{"type":"branch_summary","id":"5e5e5e5e","parentId":"` + entries["user"] + `","timestamp":"` + stamp() + `","fromId":"` + entries["assistant"] + `","summary":"Looked at note.txt  \n\n\nand stopped."}` + "\n"
		}},
		{"a custom message", func(_ map[string]string, leaf string) string {
			return `{"type":"custom_message","customType":"envoy-message","content":"A message from a peer arrived.","display":true,"id":"6f6f6f6f","parentId":"` + leaf + `","timestamp":"` + stamp() + `"}` + "\n"
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			model := newAnthropicStandIn(t, firstRunReplies()...)
			rig := newOMPRig(t, anthropicMessages, model)
			rig.run(t, "", "Read note.txt and tell me what it says.")
			file := rig.sessionFile(t)
			content, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			first, err := Read(content, standInEndpoint)
			if err != nil {
				t.Fatal(err)
			}
			entries := map[string]string{}
			for _, line := range bytes.Split(content, []byte("\n")) {
				var e struct {
					Type    string `json:"type"`
					ID      string `json:"id"`
					Message struct {
						Role string `json:"role"`
					} `json:"message"`
				}
				if json.Unmarshal(line, &e) == nil && e.Type == "message" && entries[e.Message.Role] == "" {
					entries[e.Message.Role] = e.ID
				}
			}
			records := testCase.records(entries, first.Leaf)
			appendRaw(t, file, records)
			before, err := ReadFile(file, standInEndpoint)
			if err != nil {
				t.Fatal(err)
			}
			rig.run(t, file, "What colour is the fruit?")
			assertResumeMatches(t, before, model.lastAnthropic(t), "What colour is the fruit?")
		})
	}
}

func appendRaw(t *testing.T, path, records string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(records); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

// The answer an append writes reaches the model when the pinned Oh My Pi resumes the session,
// under file storage and under SQL storage: its next request carries the question, the tool call,
// its result and the answer once each, in that order, with the appended thinking signed as it was
// written, and is the conversation the reader rebuilds from the appended session.
func TestThePinnedOhMyPiResumesAnAppendedAnswer(t *testing.T) {
	answer := conversation.Append{
		QuestionID: "q-1", Question: "Which fruit does the note name?",
		Provider: standInProvider, API: anthropicMessages, Model: standInModel,
		Turns: []conversation.Turn{
			{
				Content: []conversation.Block{
					thinking("I will read the note again.", "YXBwZW5kLW9uZQ=="),
					text("Reading the note again."),
					call("toolu_append_01", "read", `{"path":"note.txt"}`),
				},
				Results: []conversation.Block{{Type: conversation.ToolResult, ToolCallID: "toolu_append_01", ToolName: "read", Content: []conversation.Block{text("plum")}}},
				Usage:   conversation.Usage{InputTokens: 12, OutputTokens: 8},
			},
			{
				Content: []conversation.Block{thinking("It names plum.", "YXBwZW5kLXR3bw=="), text("The note names plum.")},
				Usage:   conversation.Usage{InputTokens: 20, OutputTokens: 5},
			},
		},
	}
	assertAppended := func(t *testing.T, model *standIn, appended *Session) {
		t.Helper()
		request := model.lastAnthropic(t)
		turns := request.turns(t)
		assertResumeMatches(t, appended, request, "Thanks.")
		find := func(match func(wireTurn, wireBlock) bool, name string) int {
			t.Helper()
			index, count := -1, 0
			for i, turn := range turns {
				for _, b := range turn.Blocks {
					if match(turn, b) {
						index = i
						count++
					}
				}
			}
			if count != 1 {
				t.Errorf("the request carries %s %d times, want once", name, count)
			}
			return index
		}
		question := find(func(turn wireTurn, b wireBlock) bool {
			return turn.Role == "user" && b.Type == "text" && b.Text == answer.Question
		}, "the question")
		toolCall := find(func(turn wireTurn, b wireBlock) bool {
			return turn.Role == "assistant" && b.Type == "tool_use" && b.ID == "toolu_append_01"
		}, "the tool call")
		toolResult := find(func(turn wireTurn, b wireBlock) bool {
			return turn.Role == "user" && b.Type == "tool_result" && b.ID == "toolu_append_01" && b.Result == "plum"
		}, "the tool result")
		finalAnswer := find(func(turn wireTurn, b wireBlock) bool {
			return turn.Role == "assistant" && b.Type == "text" && b.Text == "The note names plum."
		}, "the answer")
		find(func(turn wireTurn, b wireBlock) bool {
			return b.Type == "thinking" && b.Signature == "YXBwZW5kLXR3bw==" && b.Text == "It names plum."
		}, "the answer's signed thinking")
		if !(question < toolCall && toolCall < toolResult && toolResult < finalAnswer && finalAnswer == len(turns)-2) {
			t.Errorf("the question at %d, the call at %d, its result at %d, the answer at %d of %d turns; want them in order, then the follow-up",
				question, toolCall, toolResult, finalAnswer, len(turns))
		}
	}

	t.Run("file storage", func(t *testing.T) {
		model := newAnthropicStandIn(t, firstRunReplies()...)
		rig := newOMPRig(t, anthropicMessages, model)
		rig.run(t, "", "Read note.txt and tell me what it says.")
		file := rig.sessionFile(t)
		before, err := ReadFile(file, standInEndpoint)
		if err != nil {
			t.Fatal(err)
		}
		if err := AppendFile(file, before, answer, time.Now()); err != nil {
			t.Fatal(err)
		}
		appended, err := ReadFile(file, standInEndpoint)
		if err != nil {
			t.Fatal(err)
		}
		rig.run(t, file, "Thanks.")
		assertAppended(t, model, appended)
	})

	t.Run("SQL storage", func(t *testing.T) {
		dsn, dsnFile := testpg.DSNFile(t, "legion_conversation_omp_test")
		model := newAnthropicStandIn(t, firstRunReplies()...)
		rig := newOMPRig(t, anthropicMessages, model, ompsessions.StorageVariable+"="+ompsessions.SQLStorage, ompsessions.DSNFileVariable+"="+dsnFile)
		rig.run(t, "", "Read note.txt and tell me what it says.")
		ctx := context.Background()
		conn, err := ompsessions.ConnectURL(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close(context.Background()) })
		var path string
		if err := conn.QueryRow(ctx, "SELECT path FROM omp_session_files").Scan(&path); err != nil {
			t.Fatalf("the session Oh My Pi wrote to the table: %v", err)
		}
		if files, _ := filepath.Glob(filepath.Join(rig.sessions, "*.jsonl")); len(files) != 0 {
			t.Fatalf("Oh My Pi under SQL storage wrote session files %v", files)
		}
		before, err := ReadSQL(ctx, conn, path, standInEndpoint)
		if err != nil {
			t.Fatal(err)
		}
		if err := AppendSQL(ctx, conn, path, before, answer, time.Now()); err != nil {
			t.Fatal(err)
		}
		appended, err := ReadSQL(ctx, conn, path, standInEndpoint)
		if err != nil {
			t.Fatal(err)
		}
		rig.run(t, path, "Thanks.")
		assertAppended(t, model, appended)
		content, _, err := ompsessions.Read(ctx, conn, path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(content, []byte(`"Thanks."`)) {
			t.Error("the resumed run's follow-up is not in the session table")
		}
	})
}
