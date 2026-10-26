package claude

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
	"github.com/sjawhar/legion/daemon/internal/testbin"
)

// standInModel is the model the real-binary tests run the pinned Claude Code on: an id Claude Code
// does not know, which it calls through ANTHROPIC_BASE_URL like any other.
const standInModel = "claude-stand-in"

// standIn is a local Anthropic Messages endpoint: it records every request and answers each
// streaming one with the next canned server-sent-event stream, or, once they run out, a short
// signed answer. A request that does not stream gets a one-text message.
type standIn struct {
	server   *httptest.Server
	mu       sync.Mutex
	requests [][]byte
	replies  []string
}

func newStandIn(t *testing.T) *standIn {
	t.Helper()
	fallback := anthropicStream("msg_default", "end_turn", sseThinking("Resumed.", "cmVzdW1lZA=="), sseText("Resumed."))
	s := &standIn{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &request)
		if !strings.HasSuffix(r.URL.Path, "/v1/messages") || !request.Stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"msg_plain","type":"message","role":"assistant","model":"`+standInModel+
				`","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`)
			return
		}
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

// queue adds replies for the next streaming requests, in order. The rig's working directory names
// the files a reply's tool calls read, so they are queued once the rig exists.
func (s *standIn) queue(replies ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replies = append(s.replies, replies...)
}

// last is the last streaming request, decoded.
func (s *standIn) last(t *testing.T) anthropicRequest {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		t.Fatal("the stand-in model received no request")
	}
	body := s.requests[len(s.requests)-1]
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

// claudeRig is a fresh HOME and Claude Code configuration directory whose model endpoint is the
// stand-in, and a working directory holding note.txt and pear.txt.
type claudeRig struct {
	claude, config, work string
	env                  []string
}

// sessionID is the one session each test's rig runs.
const sessionID = "5e551011-0000-4000-8000-000000000001"

func newClaudeRig(t *testing.T, model *standIn) *claudeRig {
	t.Helper()
	claude := testbin.Claude(t)
	out, err := exec.Command(claude, "--version").CombinedOutput()
	if err != nil || !strings.HasPrefix(string(out), PinnedVersion+" ") {
		t.Fatalf("%s --version = %q, %v; want the pinned Claude Code %s", claude, out, err, PinnedVersion)
	}
	dir := t.TempDir()
	rig := &claudeRig{claude: claude, config: filepath.Join(dir, "config"), work: filepath.Join(dir, "work")}
	home := filepath.Join(dir, "home")
	for _, d := range []string{rig.config, rig.work, home} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range map[string]string{"note.txt": "plum\n", "pear.txt": "pear\n"} {
		if err := os.WriteFile(filepath.Join(rig.work, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Telemetry, error reports, the auto-updater and every other request Claude Code makes beside
	// the model's are off (its documented switches), so the stand-in is the only endpoint it
	// reaches.
	rig.env = []string{
		"HOME=" + home, "PATH=/usr/bin:/bin", "CLAUDE_CONFIG_DIR=" + rig.config,
		"ANTHROPIC_BASE_URL=" + model.server.URL, "ANTHROPIC_API_KEY=sk-ant-stand-in",
		"DISABLE_TELEMETRY=1", "DISABLE_ERROR_REPORTING=1", "DISABLE_AUTOUPDATER=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	}
	return rig
}

// run runs `claude -p` with prompt in the rig's session: a fresh one, or resumed.
func (r *claudeRig) run(t *testing.T, resume bool, prompt string) {
	t.Helper()
	r.exec(t, nil, r.args(resume, prompt)...)
}

// runStream runs `claude -p` reading stream-json input, as Legion's bridge drives it, sending each
// of prompts as a user message.
func (r *claudeRig) runStream(t *testing.T, resume bool, prompts ...string) {
	t.Helper()
	var input bytes.Buffer
	for _, prompt := range prompts {
		line, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []map[string]string{{"type": "text", "text": prompt}}}})
		input.Write(append(line, '\n'))
	}
	r.exec(t, &input, append(r.args(resume, ""), "--input-format", "stream-json")...)
}

func (r *claudeRig) args(resume bool, prompt string) []string {
	flag := "--session-id"
	if resume {
		flag = "--resume"
	}
	args := []string{"-p", "--output-format", "stream-json", "--verbose", "--model", standInModel, "--permission-mode", "bypassPermissions", flag, sessionID}
	if prompt != "" {
		args = append(args, prompt)
	}
	return args
}

func (r *claudeRig) exec(t *testing.T, stdin io.Reader, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.claude, args...)
	cmd.Dir, cmd.Env, cmd.Stdin = r.work, r.env, stdin
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("claude %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// transcript is the session's transcript: <CLAUDE_CONFIG_DIR>/projects/<cwd>/<session>.jsonl.
func (r *claudeRig) transcript(t *testing.T) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(r.config, "projects", "*", sessionID+".jsonl"))
	if err != nil || len(files) != 1 {
		t.Fatalf("transcripts of %s under %s = %v, %v; want one", sessionID, r.config, files, err)
	}
	return files[0]
}

// The first run's two model calls: a turn that thinks (signed and redacted), says it reads the
// note and calls Read, then an answer.
func firstRunReplies(work string) []string {
	return []string{
		anthropicStream("msg_1", "tool_use", sseThinking("I should read the note.", "c2lnLW9uZQ=="), sseRedacted("cmVkYWN0ZWQtb25l"),
			sseText("Reading the note."), sseToolUse("toolu_01ABC", "Read", `{"file_path":`+quote(filepath.Join(work, "note.txt"))+`}`)),
		anthropicStream("msg_2", "end_turn", sseThinking("The note says plum.", "c2lnLXR3bw=="), sseText("The note says plum.")),
	}
}

// anthropicRequest is the part of an Anthropic Messages request the tests compare.
type anthropicRequest struct {
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	OutputConfig struct {
		Effort string `json:"effort"`
	} `json:"output_config"`
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

// systemReminder opens the text of what Claude Code renders from a user-role attachment.
const systemReminder = "<system-reminder>"

// turns reduces the request's messages, cutting what Claude Code puts in a request beside the
// conversation, which the reader keeps out (the package comment): every system-role message (its
// context attachments rendered as system messages: the environment, the model's identity, the
// deferred tool, agent and skill listings, the date, a queued prompt's notice) and every user text
// block that is a system reminder (its user-role attachments: the commit and pull request
// attribution guidance), dropping a user message left empty.
func (request anthropicRequest) turns(t *testing.T) []wireTurn {
	t.Helper()
	var turns []wireTurn
	for _, message := range request.Messages {
		if message.Role == "system" {
			continue
		}
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
				if message.Role == "user" && strings.HasPrefix(b.Text, systemReminder) {
					continue
				}
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
		if len(turn.Blocks) > 0 {
			turns = append(turns, turn)
		}
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

// readerTurns reduces the reader's messages the same way, grouped as the request groups them: a
// message's tool results and the user message after them one user turn.
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
		role := string(m.Role)
		if m.Role == conversation.Tool {
			role = "user"
		}
		if role == "user" && len(turns) > 0 && turns[len(turns)-1].Role == "user" {
			turns[len(turns)-1].Blocks = append(turns[len(turns)-1].Blocks, blocks...)
			continue
		}
		turns = append(turns, wireTurn{Role: role, Blocks: blocks})
	}
	return turns
}

// assertResumeMatches checks the request the pinned Claude Code sent on resuming a session
// against what the reader rebuilt from the transcript before the resume: the request is the
// rebuilt conversation, then the follow-up; and it runs at the effort the reader read as the
// conversation's thinking level.
func assertResumeMatches(t *testing.T, before *Session, request anthropicRequest, followUp string) {
	t.Helper()
	got := request.turns(t)
	want := readerTurns(t, before.Messages)
	if n := len(want); n > 0 && want[n-1].Role == "user" {
		// The follow-up joins a last user message no answer was asked for (a local command).
		want[n-1].Blocks[len(want[n-1].Blocks)-1].Text += "\n"
		want[n-1].Blocks = append(want[n-1].Blocks, wireBlock{Type: "text", Text: followUp})
	} else {
		want = append(want, wireTurn{Role: "user", Blocks: []wireBlock{{Type: "text", Text: followUp}}})
	}
	observed, _ := json.Marshal(got)
	t.Logf("Claude Code's resume request, reduced: %s", observed)
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.MarshalIndent(got, "", "  ")
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("Claude Code's resume request:\n%s\nthe reader's conversation and the follow-up:\n%s", gotJSON, wantJSON)
	}
	if before.Model != "anthropic/"+standInModel || before.API != anthropicMessages {
		t.Errorf("the reader's model %q, API %q", before.Model, before.API)
	}
	if before.ThinkingLevel != request.OutputConfig.Effort {
		t.Errorf("the reader's thinking level %q, the request's effort %q", before.ThinkingLevel, request.OutputConfig.Effort)
	}
}

// The pinned Claude Code, resuming a session it wrote with a tool call and signed and redacted
// thinking in it, sends the stand-in model the conversation the reader rebuilds from the
// transcript: the same turns, roles, block order, text, tool ids, thinking signatures and
// redacted data, the assistant lines of one message merged into one.
func TestThePinnedClaudeCodeResumesTheConversationTheReaderRebuilds(t *testing.T) {
	model := newStandIn(t)
	rig := newClaudeRig(t, model)
	model.queue(firstRunReplies(rig.work)...)
	rig.run(t, false, "Read note.txt and tell me what it says.")
	before, err := ReadFile(rig.transcript(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Messages) != 4 {
		t.Fatalf("the reader rebuilt %d messages from the first run, want the request, the tool turn, its result and the answer", len(before.Messages))
	}
	rig.run(t, true, "What colour is the fruit?")
	assertResumeMatches(t, before, model.last(t), "What colour is the fruit?")
}

// Parallel tool calls leave each result on a branch of its own (each a child of the line holding
// its call); the pinned Claude Code puts the branches back on resume, as the reader does.
func TestThePinnedClaudeCodeResumesParallelToolCallsAsTheReaderRebuildsThem(t *testing.T) {
	model := newStandIn(t)
	rig := newClaudeRig(t, model)
	model.queue(
		anthropicStream("msg_1", "tool_use", sseText("Reading both."),
			sseToolUse("toolu_A", "Read", `{"file_path":`+quote(filepath.Join(rig.work, "note.txt"))+`}`),
			sseToolUse("toolu_B", "Read", `{"file_path":`+quote(filepath.Join(rig.work, "pear.txt"))+`}`)),
		anthropicStream("msg_2", "end_turn", sseText("Plum and pear.")),
	)
	rig.run(t, false, "Read note.txt and pear.txt.")
	before, err := ReadFile(rig.transcript(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Messages) != 5 {
		t.Fatalf("the reader rebuilt %d messages, want the request, the turn, two results and the answer", len(before.Messages))
	}
	rig.run(t, true, "Which came first?")
	assertResumeMatches(t, before, model.last(t), "Which came first?")
}

// A `/compact` the pinned Claude Code runs writes a compaction boundary, its summary and the
// messages it keeps verbatim; resumed, it sends the summary, the kept messages and what followed,
// as the reader rebuilds them.
func TestThePinnedClaudeCodeResumesACompactionAsTheReaderRebuildsIt(t *testing.T) {
	model := newStandIn(t)
	rig := newClaudeRig(t, model)
	model.queue(append(firstRunReplies(rig.work), anthropicStream("msg_summary", "end_turn", sseText("The user asked what note.txt says; it says plum.")))...)
	rig.run(t, false, "Read note.txt and tell me what it says.")
	rig.run(t, true, "/compact")
	file := rig.transcript(t)
	content, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(content, []byte(`"subtype":"compact_boundary"`)) || !bytes.Contains(content, []byte(`"isCompactSummary":true`)) {
		t.Fatalf("the transcript after /compact holds no compaction boundary and summary:\n%s", content)
	}
	before, err := Read(content)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Messages) == 0 || !strings.Contains(before.Messages[0].Content[0].Text, "it says plum.") {
		t.Fatalf("the reader's conversation after the compaction does not open with its summary: %+v", before.Messages)
	}
	rig.run(t, true, "What colour is the fruit?")
	assertResumeMatches(t, before, model.last(t), "What colour is the fruit?")
}

// A session the bridge seeds with `/rename` records its title in a custom-title line, which
// Describe and the reader read; resumed, the pinned Claude Code sends the local command's caveat,
// command and output as user text, merged with the next prompt, as the reader rebuilds them.
func TestThePinnedClaudeCodeRecordsTheTitleDescribeReads(t *testing.T) {
	model := newStandIn(t)
	rig := newClaudeRig(t, model)
	rig.runStream(t, false, "/rename LEGION-1-implementer", "Hello there.")
	file := rig.transcript(t)
	described, err := DescribeFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if described.Title != "LEGION-1-implementer" || described.SessionID != sessionID || described.Cwd != rig.work {
		t.Errorf("Describe = %+v, want the title LEGION-1-implementer, session %s, cwd %s", described, sessionID, rig.work)
	}
	before, err := ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if before.Title != "LEGION-1-implementer" || before.ID != sessionID {
		t.Errorf("the reader's title %q, session %q", before.Title, before.ID)
	}
	rig.run(t, true, "Thanks.")
	assertResumeMatches(t, before, model.last(t), "Thanks.")
}

// handLine is a conversation line written by hand after the first run, in the shape Claude Code
// writes: fields names the line's own fields, parent its parentUuid ("" for null).
func handLine(rig *claudeRig, uuid, parent, fields string) string {
	parentJSON := "null"
	if parent != "" {
		parentJSON = quote(parent)
	}
	return `{"parentUuid":` + parentJSON + `,"isSidechain":false,` + fields + `,"uuid":` + quote(uuid) + `,"timestamp":` + quote(isoTime(time.Now())) +
		`,"userType":"external","entrypoint":"sdk-cli","cwd":` + quote(rig.work) + `,"sessionId":"` + sessionID + `","version":"` + PinnedVersion + `"}` + "\n"
}

func lastPrompt(leaf string) string {
	return `{"type":"last-prompt","lastPrompt":"x","leafUuid":` + quote(leaf) + `,"sessionId":"` + sessionID + `"}` + "\n"
}

func handAssistant(id, content string) string {
	return `"message":{"id":` + quote(id) + `,"type":"message","role":"assistant","model":"` + standInModel + `","content":` + content +
		`,"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}},"type":"assistant"`
}

func handUser(content string) string {
	return `"type":"user","message":{"role":"user","content":` + content + `}`
}

// The pinned Claude Code and the reader rebuild alike a transcript that, after a real first run,
// holds lines written by hand: a tool call interrupted before its result (Claude Code answers it
// as interrupted and closes the turn), a tool call the next prompt abandoned (Claude Code drops
// its message), a prompt never answered, an API error record (dropped, its prompt joining the
// next), a compaction boundary with no preserved messages and its summary (closed like a prompt
// never answered), a subagent's sidechain line and a torn last line. Each resumed request is the
// reader's conversation, then the follow-up.
func TestThePinnedClaudeCodeResumesHandWrittenRecordsAsTheReaderRebuildsThem(t *testing.T) {
	const (
		u1 = "4a4d0000-0000-4000-8000-000000000001"
		u2 = "4a4d0000-0000-4000-8000-000000000002"
		u3 = "4a4d0000-0000-4000-8000-000000000003"
	)
	for _, testCase := range []struct {
		name    string
		records func(rig *claudeRig, leaf string) string
	}{
		{"an interrupted tool call", func(rig *claudeRig, leaf string) string {
			return handLine(rig, u1, leaf, handAssistant("msg_interrupted", `[{"type":"thinking","thinking":"Read it again.","signature":"aW50ZXJydXB0ZWQ="}]`)) +
				handLine(rig, u2, u1, handAssistant("msg_interrupted", `[{"type":"text","text":"Reading again."}]`)) +
				handLine(rig, u3, u2, handAssistant("msg_interrupted", `[{"type":"tool_use","id":"toolu_interrupted","name":"Read","input":{"file_path":`+quote(filepath.Join(rig.work, "note.txt"))+`}}]`)) +
				lastPrompt(u3)
		}},
		{"a tool call the next prompt abandoned", func(rig *claudeRig, leaf string) string {
			return handLine(rig, u1, leaf, handAssistant("msg_abandoned", `[{"type":"text","text":"Checking."},{"type":"tool_use","id":"toolu_abandoned","name":"Read","input":{"file_path":"note.txt"}}]`)) +
				handLine(rig, u2, u1, handUser(`"Never mind."`)) +
				handLine(rig, u3, u2, `"message":{"id":"msg_ok","type":"message","role":"assistant","model":"`+standInModel+`","content":[{"type":"text","text":"OK."}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}},"type":"assistant"`) +
				lastPrompt(u3)
		}},
		{"an unanswered prompt", func(rig *claudeRig, leaf string) string {
			return handLine(rig, u1, leaf, handUser(`[{"type":"text","text":"Is it ripe?"}]`)) + lastPrompt(u1)
		}},
		{"an API error record", func(rig *claudeRig, leaf string) string {
			return handLine(rig, u1, leaf, handUser(`"Is it ripe?"`)) +
				handLine(rig, u2, u1, `"type":"assistant","message":{"id":"e1a2b3c4-0000-4000-8000-000000000009","model":"<synthetic>","role":"assistant","stop_reason":"stop_sequence","stop_sequence":"","type":"message","usage":{"input_tokens":0,"output_tokens":0},"content":[{"type":"text","text":"API Error: 500 Internal server error"}]},"isApiErrorMessage":true,"error":"unknown"`) +
				lastPrompt(u2)
		}},
		{"a compaction boundary and its summary", func(rig *claudeRig, leaf string) string {
			return handLine(rig, u1, "", `"logicalParentUuid":`+quote(leaf)+`,"type":"system","subtype":"compact_boundary","content":"Conversation compacted","isMeta":false,"level":"info","compactMetadata":{"trigger":"manual","preTokens":100}`) +
				handLine(rig, u2, u1, handUser(`"This session is being continued from a previous conversation. Summary: the note says plum."`)+`,"isVisibleInTranscriptOnly":true,"isCompactSummary":true`) +
				lastPrompt(u2)
		}},
		{"a sidechain line", func(rig *claudeRig, leaf string) string {
			return strings.Replace(handLine(rig, u1, leaf, handUser(`"A subagent's prompt."`)), `"isSidechain":false`, `"isSidechain":true,"agentId":"a1"`, 1)
		}},
		{"a torn last line", func(rig *claudeRig, leaf string) string {
			return strings.TrimSuffix(handLine(rig, u1, leaf, handUser(`"cut off"`)), `"}`+"\n")
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			model := newStandIn(t)
			rig := newClaudeRig(t, model)
			model.queue(firstRunReplies(rig.work)...)
			rig.run(t, false, "Read note.txt and tell me what it says.")
			file := rig.transcript(t)
			first, err := ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			appendRaw(t, file, testCase.records(rig, first.Leaf))
			before, err := ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			rig.run(t, true, "What colour is the fruit?")
			assertResumeMatches(t, before, model.last(t), "What colour is the fruit?")
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

// The answer an append writes reaches the model when the pinned Claude Code resumes the session:
// its next request carries the question, the tool call, its result and the answer once each, in
// that order, with the appended thinking signed as it was written, and is the conversation the
// reader rebuilds from the appended transcript.
func TestThePinnedClaudeCodeResumesAnAppendedAnswer(t *testing.T) {
	model := newStandIn(t)
	rig := newClaudeRig(t, model)
	model.queue(firstRunReplies(rig.work)...)
	answer := conversation.Append{
		QuestionID: "q-1", Question: "Which fruit does the note name?",
		Provider: "anthropic", API: anthropicMessages, Model: standInModel,
		Turns: []conversation.Turn{
			{
				Content: []conversation.Block{
					thinking("I will read the note again.", "YXBwZW5kLW9uZQ=="),
					text("Reading the note again."),
					call("toolu_append_01", "Read", `{"file_path":`+quote(filepath.Join(rig.work, "note.txt"))+`}`),
				},
				Results: []conversation.Block{{Type: conversation.ToolResult, ToolCallID: "toolu_append_01", ToolName: "Read", Content: []conversation.Block{text("plum")}}},
				Usage:   conversation.Usage{InputTokens: 12, OutputTokens: 8},
			},
			{
				Content: []conversation.Block{thinking("It names plum.", "YXBwZW5kLXR3bw=="), text("The note names plum.")},
				Usage:   conversation.Usage{InputTokens: 20, OutputTokens: 5},
			},
		},
	}
	rig.run(t, false, "Read note.txt and tell me what it says.")
	file := rig.transcript(t)
	before, err := ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := AppendFile(file, before, answer, time.Now()); err != nil {
		t.Fatal(err)
	}
	appended, err := ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	rig.run(t, true, "Thanks.")
	request := model.last(t)
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
