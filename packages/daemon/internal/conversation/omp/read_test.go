package omp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/conversation"
)

const fixtureHeader = `{"type":"session","version":3,"id":"0001","timestamp":"2026-10-26T00:00:00.000Z","cwd":"/work"}`

// fixture builds a session's content an entry at a time, each entry chained to the one before
// unless it names its own parent.
type fixture struct {
	lines []string
	last  string
	n     int
}

func newFixture() *fixture { return &fixture{lines: []string{fixtureHeader}} }

// add appends entry, a JSON object with neither id nor parentId, as the next entry, and returns
// its id.
func (f *fixture) add(entry string) string {
	return f.addAfter(f.last, entry)
}

// addAfter appends entry with parent as its parent ("" for none).
func (f *fixture) addAfter(parent, entry string) string {
	f.n++
	id := fmt.Sprintf("e%07d", f.n)
	parentJSON := "null"
	if parent != "" {
		parentJSON = `"` + parent + `"`
	}
	f.lines = append(f.lines, fmt.Sprintf(`{"id":%q,"parentId":%s,"timestamp":"2026-10-26T00:00:00.000Z",%s`, id, parentJSON, entry[1:]))
	f.last = id
	return id
}

func (f *fixture) bytes() []byte { return []byte(strings.Join(f.lines, "\n") + "\n") }

func userEntry(text string) string {
	return `{"type":"message","message":{"role":"user","content":[{"type":"text","text":` + quote(text) + `}],"attribution":"user","timestamp":1}}`
}

func assistantEntry(provider, api, model, stopReason, content string) string {
	return `{"type":"message","message":{"role":"assistant","content":` + content + `,"api":"` + api + `","provider":"` + provider +
		`","model":"` + model + `","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"` +
		stopReason + `","timestamp":2}}`
}

func claudeEntry(stopReason, content string) string {
	return assistantEntry("anthropic", anthropicMessages, "claude-opus-5-5", stopReason, content)
}

func toolResultEntry(id, name, text string, isError bool) string {
	return fmt.Sprintf(`{"type":"message","message":{"role":"toolResult","toolCallId":%s,"toolName":%s,"content":[{"type":"text","text":%s}],"isError":%t,"timestamp":3}}`,
		quote(id), quote(name), quote(text), isError)
}

func quote(s string) string {
	encoded, _ := json.Marshal(s)
	return string(encoded)
}

func read(t *testing.T, content []byte) *Session {
	t.Helper()
	s, err := Read(content, Options{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return s
}

func text(s string) conversation.Block { return conversation.Block{Type: conversation.Text, Text: s} }

func user(texts ...string) conversation.Message {
	m := conversation.Message{Role: conversation.User}
	for _, s := range texts {
		m.Content = append(m.Content, text(s))
	}
	return m
}

func claude(blocks ...conversation.Block) conversation.Message {
	return conversation.Message{Role: conversation.Assistant, Provider: "anthropic", API: anthropicMessages, Model: "claude-opus-5-5", Content: blocks}
}

func result(id, name, output string, isError bool) conversation.Message {
	return conversation.Message{Role: conversation.Tool, Content: []conversation.Block{{
		Type: conversation.ToolResult, ToolCallID: id, ToolName: name, IsError: isError, Content: []conversation.Block{text(output)},
	}}}
}

func call(id, name, arguments string) conversation.Block {
	return conversation.Block{Type: conversation.ToolCall, ToolCallID: id, ToolName: name, Arguments: json.RawMessage(arguments)}
}

func thinking(s, signature string) conversation.Block {
	return conversation.Block{Type: conversation.Thinking, Text: s, Signature: signature}
}

func assertMessages(t *testing.T, got, want []conversation.Message) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.MarshalIndent(got, "", "  ")
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("messages:\n%s\nwant:\n%s", gotJSON, wantJSON)
	}
}

// A session reads as the request Oh My Pi sends on resume: the user turn, the signed thinking with
// its signature, the redacted thinking's data, the text and the tool call with its arguments byte
// for byte, the result, the answer; and the model, API and thinking level the conversation last
// ran at, from its model and thinking-level changes.
func TestReadRebuildsTheConversationOhMyPiResumes(t *testing.T) {
	f := newFixture()
	f.add(`{"type":"model_change","model":"anthropic/claude-opus-5-5"}`)
	f.add(`{"type":"thinking_level_change","thinkingLevel":"high","configured":null}`)
	f.add(userEntry("Read note.txt."))
	f.add(`{"type":"custom","customType":"system-prompt-digest","data":"284852018acfe3b4"}`)
	f.add(claudeEntry("toolUse", `[{"type":"thinking","thinking":"I should read it.","thinkingSignature":"c2ln"},{"type":"redactedThinking","data":"cmVk"},{"type":"text","text":"Reading."},{"type":"toolCall","id":"toolu_01","name":"read","arguments":{"path": "note.txt"}}]`))
	f.add(`{"type":"custom","customType":"tool_execution_start","data":{"toolCallId":"toolu_01"}}`)
	f.add(toolResultEntry("toolu_01", "read", "plum", false))
	f.add(claudeEntry("stop", `[{"type":"thinking","thinking":"It says plum.","thinkingSignature":"c2lnMg=="},{"type":"text","text":"Plum."}]`))
	f.add(`{"type":"custom","customType":"session_exit","data":{"reason":"dispose"}}`)
	s := read(t, f.bytes())

	assertMessages(t, s.Messages, []conversation.Message{
		user("Read note.txt."),
		claude(thinking("I should read it.", "c2ln"), conversation.Block{Type: conversation.RedactedThinking, Data: "cmVk"}, text("Reading."), call("toolu_01", "read", `{"path": "note.txt"}`)),
		result("toolu_01", "read", "plum", false),
		claude(thinking("It says plum.", "c2lnMg=="), text("Plum.")),
	})
	if s.Model != "anthropic/claude-opus-5-5" || s.API != anthropicMessages || s.ThinkingLevel != "high" {
		t.Errorf("model %q, API %q, thinking level %q", s.Model, s.API, s.ThinkingLevel)
	}
	if s.ID != "0001" || s.Leaf != f.last || s.Torn || s.Size != int64(len(f.bytes())) {
		t.Errorf("session %q, leaf %q, torn %t, size %d", s.ID, s.Leaf, s.Torn, s.Size)
	}
}

// A compaction replaces everything before the entry it keeps from with its summary, in Oh My Pi's
// template, ahead of the kept entries and everything after it; a handoff compaction's summary is in
// the handoff template; an Anthropic compaction's native block rides on its summary; a /clear
// boundary after the compaction drops it and all before.
func TestReadReplacesCompactedHistoryWithItsSummary(t *testing.T) {
	build := func(method, preserve string, reset bool) []byte {
		f := newFixture()
		f.add(userEntry("first request"))
		f.add(claudeEntry("stop", `[{"type":"text","text":"first answer"}]`))
		kept := f.add(userEntry("second request"))
		f.add(claudeEntry("stop", `[{"type":"text","text":"second answer"}]`))
		f.add(`{"type":"compaction","summary":"Did the first thing.\n\n\n\nThen more.  ","firstKeptEntryId":"` + kept + `","tokensBefore":100` + method + preserve + `}`)
		if reset {
			f.add(`{"type":"reset_boundary"}`)
		}
		f.add(userEntry("third request"))
		f.add(claudeEntry("stop", `[{"type":"text","text":"third answer"}]`))
		return f.bytes()
	}
	summary := "Prior model work/tool state available.\nMUST build on prior work; NEVER duplicate prior work.\n\n<summary>\nDid the first thing.\nThen more.\n</summary>"
	tail := []conversation.Message{user("second request"), claude(text("second answer")), user("third request"), claude(text("third answer"))}

	s := read(t, build("", "", false))
	assertMessages(t, s.Messages, append([]conversation.Message{user(summary)}, tail...))

	s = read(t, build(`,"method":"handoff"`, "", false))
	if got := s.Messages[0].Content[0].Text; !strings.HasPrefix(got, "Context replaced. The <handoff> below") || !strings.HasSuffix(got, "<handoff>\nDid the first thing.\nThen more.\n</handoff>") {
		t.Errorf("handoff summary = %q", got)
	}

	payload := `{"provider":"anthropic","content":"API summary","signature":"c2ln"}`
	s = read(t, build("", `,"preserveData":{"anthropicCompaction":`+payload+`}`, false))
	if native := s.Messages[0].Native; native == nil || native.Kind != conversation.AnthropicCompaction || native.Provider != "anthropic" || string(native.Payload) != payload {
		t.Errorf("summary native = %+v, want the Anthropic compaction payload", native)
	}

	s = read(t, build("", "", true))
	assertMessages(t, s.Messages, []conversation.Message{user("third request"), claude(text("third answer"))})
}

// The path runs from the last entry: a branch left behind is not in the context, a branch summary
// is, in its template.
func TestReadFollowsTheActiveBranch(t *testing.T) {
	f := newFixture()
	root := f.add(userEntry("request"))
	f.add(claudeEntry("stop", `[{"type":"text","text":"abandoned answer"}]`))
	f.addAfter(root, `{"type":"branch_summary","fromId":"x","summary":"Tried another way."}`)
	f.add(claudeEntry("stop", `[{"type":"text","text":"kept answer"}]`))
	s := read(t, f.bytes())
	assertMessages(t, s.Messages, []conversation.Message{
		user("request"),
		user("Branch-return summary:\n\n<summary>\nTried another way.\n</summary>"),
		claude(text("kept answer")),
	})
}

// A tool call interrupted before its result is not replayed: the call is dropped from its turn and
// the turn's signatures with it, so its now-unsigned thinking is dropped on Anthropic, as its
// redacted thinking is. A turn that failed or was aborted is dropped with its tool results, a turn
// that failed empty and a retry-recovery turn are skipped, and a cut-off thinking-only turn is not
// replayed.
func TestReadDropsInterruptedAndFailedTurns(t *testing.T) {
	f := newFixture()
	f.add(userEntry("request"))
	f.add(claudeEntry("toolUse", `[{"type":"thinking","thinking":"try","thinkingSignature":"c2ln"},{"type":"toolCall","id":"toolu_failed","name":"bash","arguments":{}}]`))
	f.add(toolResultEntry("toolu_failed", "bash", "aborted", true))
	f.add(claudeEntry("error", `[{"type":"text","text":"half an answer"},{"type":"toolCall","id":"toolu_err","name":"bash","arguments":{}}]`))
	f.add(toolResultEntry("toolu_err", "bash", "aborted", true))
	f.add(claudeEntry("error", `[{"type":"text","text":"  "}]`))
	f.add(`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"recovering"}],"api":"anthropic-messages","provider":"anthropic","model":"claude-opus-5-5","stopReason":"stop","retryRecovery":true,"timestamp":2}}`)
	f.add(claudeEntry("length", `[{"type":"thinking","thinking":"cut off","thinkingSignature":"c2ln"}]`))
	f.add(userEntry("again"))
	f.add(claudeEntry("toolUse", `[{"type":"thinking","thinking":"I will look.","thinkingSignature":"c2lnMw=="},{"type":"redactedThinking","data":"cmVk"},{"type":"text","text":"Looking."},{"type":"toolCall","id":"toolu_dangling","name":"read","arguments":{"path":"x"}}]`))
	s := read(t, f.bytes())
	assertMessages(t, s.Messages, []conversation.Message{
		user("request"),
		claude(thinking("try", "c2ln"), call("toolu_failed", "bash", `{}`)),
		result("toolu_failed", "bash", "aborted", true),
		user("again"),
		claude(text("Looking.")),
	})
}

// Tool call ids reach Anthropic as Oh My Pi's normalizeToolCallId leaves them: an OpenAI
// "call_…|fc_…" id with every character outside [a-zA-Z0-9_-] replaced, cut to 64; the Responses
// API's call_id is the id's part before the '|'. Each result follows its call, pulled forward past
// a message between them.
func TestReadCarriesToolCallIDsAsTheTargetRequestDoes(t *testing.T) {
	build := func(final string) []byte {
		f := newFixture()
		f.add(userEntry("request"))
		f.add(assistantEntry("openai", openaiResponses, "gpt-6.1", "toolUse", `[{"type":"toolCall","id":"call_A|fc_0123456789012345678901234567890123456789012345678901234567890123","name":"read","arguments":{}}]`))
		f.add(`{"type":"custom_message","customType":"mid-run-todo-nudge","content":"Keep going.","display":false}`)
		f.add(toolResultEntry("call_A|fc_0123456789012345678901234567890123456789012345678901234567890123", "read", "out", false))
		f.add(final)
		return f.bytes()
	}
	s := read(t, build(claudeEntry("stop", `[{"type":"text","text":"done"}]`)))
	anthropicID := ("call_A_fc_" + "0123456789012345678901234567890123456789012345678901234567890123")[:64]
	if got := s.Messages[1].Content[0].ToolCallID; got != anthropicID {
		t.Errorf("Anthropic tool call id = %q, want %q", got, anthropicID)
	}
	if got := s.Messages[2]; got.Role != conversation.Tool || got.Content[0].ToolCallID != anthropicID {
		t.Errorf("message after the call = %+v, want its result under %q", got, anthropicID)
	}
	if got := s.Messages[3]; got.Role != conversation.User || got.Content[0].Text != "Keep going." {
		t.Errorf("message after the result = %+v, want the custom message", got)
	}

	s = read(t, build(assistantEntry("openai", openaiResponses, "gpt-6.1", "stop", `[{"type":"text","text":"done"}]`)))
	if s.Messages[1].Content[0].ToolCallID != "call_A" || s.Messages[2].Content[0].ToolCallID != "call_A" {
		t.Errorf("Responses ids = %q and %q, want call_A", s.Messages[1].Content[0].ToolCallID, s.Messages[2].Content[0].ToolCallID)
	}
}

// An OpenAI Responses turn on the model the conversation resumes on carries its output items byte
// for byte, reasoning items' ids and encrypted content included; its thinking block is the
// summary. A reasoning summary from that model reaches Claude as plain text, and Claude's thinking
// reaches an OpenAI model in a <think> block, each without its signature or redacted data.
func TestReadCarriesOpenAIReasoningNativeAndForeignReasoningAsText(t *testing.T) {
	items := `[{"id":"rs_01","type":"reasoning","content":[],"encrypted_content":"gAAAAABq3krn+/=","summary":[{"type":"summary_text","text":"Weighing it."}]},` +
		`{"id":"fc_01","type":"function_call","status":"completed","arguments":"{\"path\":\"x\"}","call_id":"call_01","name":"read"}]`
	openaiTurn := `{"type":"message","message":{"role":"assistant","content":[{"type":"thinking","thinking":"Weighing it.","itemId":"rs_01","summary":true},{"type":"toolCall","id":"call_01|fc_01","name":"read","arguments":{"path":"x"}}],` +
		`"api":"openai-responses","provider":"openai","model":"gpt-6.1","stopReason":"toolUse","timestamp":2,"providerPayload":{"type":"openaiResponsesHistory","provider":"openai","dt":true,"items":` + items + `}}}`

	f := newFixture()
	f.add(userEntry("request"))
	f.add(openaiTurn)
	f.add(toolResultEntry("call_01|fc_01", "read", "out", false))
	s := read(t, f.bytes())
	native := s.Messages[1].Native
	if native == nil || native.Kind != conversation.OpenAIResponsesHistory || !native.Incremental || len(native.Items) != 2 {
		t.Fatalf("native = %+v, want the turn's two output items", native)
	}
	var want []json.RawMessage
	if err := json.Unmarshal([]byte(items), &want); err != nil {
		t.Fatal(err)
	}
	for i := range want {
		if !bytes.Equal(native.Items[i], want[i]) {
			t.Errorf("item %d = %s, want byte for byte %s", i, native.Items[i], want[i])
		}
	}
	if got := s.Messages[1].Content[0]; got.Type != conversation.Thinking || got.Text != "Weighing it." {
		t.Errorf("thinking = %+v", got)
	}
	if s.API != openaiResponses || s.Model != "openai/gpt-6.1" {
		t.Errorf("API %q, model %q", s.API, s.Model)
	}

	f.add(`{"type":"model_change","model":"anthropic/claude-opus-5-5"}`)
	f.add(claudeEntry("stop", `[{"type":"thinking","thinking":"Claude thinks.","thinkingSignature":"c2ln"},{"type":"redactedThinking","data":"cmVk"},{"type":"text","text":"Claude answers."}]`))
	s = read(t, f.bytes())
	assertMessages(t, s.Messages[1:2], []conversation.Message{{
		Role: conversation.Assistant, Provider: "openai", API: openaiResponses, Model: "gpt-6.1",
		Content: []conversation.Block{text("Weighing it."), call("call_01_fc_01", "read", `{"path":"x"}`)},
	}})

	f.add(`{"type":"model_change","model":"openai/gpt-6.1"}`)
	s = read(t, f.bytes())
	assertMessages(t, s.Messages[3:4], []conversation.Message{claude(text("<think>\nClaude thinks.\n</think>"), text("Claude answers."))})
	if s.Messages[1].Native == nil {
		t.Error("back on gpt-6.1 the OpenAI turn carries no native items")
	}
}

// Images reach the request as their media type and data; an image Oh My Pi moved to its blob
// store refuses the session.
func TestReadCarriesImages(t *testing.T) {
	f := newFixture()
	f.add(`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"look"},{"type":"image","data":"iVBORw0K","mimeType":"image/png"}],"timestamp":1}}`)
	f.add(claudeEntry("stop", `[{"type":"text","text":"seen"}]`))
	s := read(t, f.bytes())
	if got := s.Messages[0].Content; len(got) != 2 || !reflect.DeepEqual(got[1], conversation.Block{Type: conversation.Image, MediaType: "image/png", ImageData: "iVBORw0K"}) {
		t.Errorf("user content = %+v", got)
	}

	f = newFixture()
	f.add(`{"type":"message","message":{"role":"user","content":[{"type":"image","data":"blob:sha256:abc","mimeType":"image/png"}],"timestamp":1}}`)
	f.add(claudeEntry("stop", `[{"type":"text","text":"seen"}]`))
	if _, err := Read(f.bytes(), Options{}); !errors.Is(err, conversation.ErrUnknownFormat) {
		t.Errorf("a blob-store image: Read = %v, want ErrUnknownFormat", err)
	}
}

// A record that is not JSON, as a torn last record is, is skipped as Oh My Pi skips it, and the
// session is marked torn when its content does not end in a newline.
func TestReadSkipsATornRecord(t *testing.T) {
	f := newFixture()
	f.add(userEntry("request"))
	f.add(claudeEntry("stop", `[{"type":"text","text":"answer"}]`))
	content := append(f.bytes(), []byte(`{"type":"message","id":"torn","parentId":"`+f.last+`","message":{"role":"user","content":[{"type":"te`)...)
	s := read(t, content)
	assertMessages(t, s.Messages, []conversation.Message{user("request"), claude(text("answer"))})
	if !s.Torn || s.Leaf != f.last {
		t.Errorf("torn %t, leaf %q; want torn, leaf %q", s.Torn, s.Leaf, f.last)
	}
}

// A well-formed record the pinned Oh My Pi does not write refuses the session: an entry type, a
// message role, a content block type, a header version, a provider payload type; so does content a
// reader cannot rebuild as Oh My Pi would.
func TestReadRefusesWhatItDoesNotKnow(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		content string
	}{
		{"an entry type", fixtureHeader + "\n" + `{"type":"bookmark","id":"a","parentId":null}` + "\n"},
		{"a header version", `{"type":"session","version":4,"id":"0001","cwd":"/work"}` + "\n"},
		{"an older header version", `{"type":"session","version":2,"id":"0001","cwd":"/work"}` + "\n"},
		{"no header", `{"type":"message","id":"a","parentId":null,"message":{"role":"user","content":"hi"}}` + "\n"},
		{"a message role", fixtureHeader + "\n" + `{"type":"message","id":"a","parentId":null,"message":{"role":"narrator","content":"hi"}}` + "\n"},
		{"a content block type", fixtureHeader + "\n" + `{"type":"message","id":"a","parentId":null,"message":{"role":"user","content":[{"type":"audio","data":"x"}]}}` + "\n"},
		{"an assistant block type", fixtureHeader + "\n" + `{"type":"message","id":"a","parentId":null,"message":{"role":"assistant","api":"anthropic-messages","provider":"anthropic","model":"claude-x","stopReason":"stop","content":[{"type":"fallback","from":{"model":"a"},"to":{"model":"b"}}]}}` + "\n"},
		{"a provider payload type", fixtureHeader + "\n" + `{"type":"message","id":"a","parentId":null,"message":{"role":"assistant","api":"anthropic-messages","provider":"anthropic","model":"claude-x","stopReason":"stop","content":[{"type":"text","text":"hi"}],"providerPayload":{"type":"geminiHistory"}}}` + "\n"},
		{"a provider API", fixtureHeader + "\n" + `{"type":"message","id":"a","parentId":null,"message":{"role":"assistant","api":"google-generative-ai","provider":"google","model":"gemini-3","stopReason":"stop","content":[{"type":"text","text":"hi"}]}}` + "\n"},
		{"a mistyped field", fixtureHeader + "\n" + `{"type":"compaction","id":"a","parentId":null,"summary":7,"firstKeptEntryId":"x"}` + "\n"},
		{"a snapcompact archive", fixtureHeader + "\n" + `{"type":"compaction","id":"a","parentId":null,"summary":"s","firstKeptEntryId":"","preserveData":{"snapcompact":{}}}` + "\n"},
		{"a bash execution", fixtureHeader + "\n" + `{"type":"message","id":"a","parentId":null,"message":{"role":"bashExecution","command":"ls","output":""}}` + "\n"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := Read([]byte(testCase.content), Options{})
			if !errors.Is(err, conversation.ErrUnknownFormat) {
				t.Fatalf("Read = %v, want ErrUnknownFormat", err)
			}
			var formatError *conversation.FormatError
			if !errors.As(err, &formatError) || formatError.What == "" {
				t.Errorf("Read = %v, want a FormatError naming what it did not know", err)
			}
		})
	}
}

// Custom messages reach the model as Oh My Pi converts them: an extension's message as a user
// turn, its images in a user turn of their own; a prewalk plan not at all; a collaborator's prompt
// in the user-interjection envelope. A custom entry, such as the answer marker an append writes,
// never reaches it, nor does a tool result a prune record blanked keep its output.
func TestReadConvertsCustomMessagesAndPrunes(t *testing.T) {
	f := newFixture()
	f.add(userEntry("request"))
	f.add(`{"type":"custom_message","customType":"envoy-message","content":"A message arrived.","display":true}`)
	f.add(`{"type":"custom_message","customType":"prewalk-plan","content":"plan","display":false}`)
	f.add(`{"type":"custom_message","customType":"screenshot","content":[{"type":"text","text":"Screen:"},{"type":"image","data":"iVBO","mimeType":"image/png"}],"display":true}`)
	f.add(`{"type":"custom_message","customType":"collab-prompt","content":"Stop   \n\n\n\nnow","display":true,"attribution":"user"}`)
	f.add(claudeEntry("toolUse", `[{"type":"toolCall","id":"toolu_01","name":"xd://codegraph","arguments":{}}]`))
	result := f.add(`{"type":"message","message":{"role":"toolResult","toolCallId":"toolu_01","toolName":"xd://codegraph","content":[{"type":"text","text":"a long output"},{"type":"text","text":" continued"}],"isError":false,"timestamp":3}}`)
	f.add(`{"type":"custom","customType":"tool_result_prune","data":{"results":[{"entryId":"` + result + `","notice":"[Uneventful result elided]","prunedAt":5}]}}`)
	f.add(claudeEntry("stop", `[{"type":"text","text":"done"}]`))
	f.add(`{"type":"custom","customType":"legion-answer","data":{"questionId":"q1"}}`)
	s := read(t, f.bytes())
	assertMessages(t, s.Messages, []conversation.Message{
		user("request"),
		user("A message arrived."),
		user("Screen:"),
		{Role: conversation.User, Content: []conversation.Block{text("Images attached to screenshot."), {Type: conversation.Image, MediaType: "image/png", ImageData: "iVBO"}}},
		user("<system-notice>\nUser interjection during work: priority; supersedes conflicting prior instructions. Re-read; ensure current work reflects user intent.\n</system-notice>\nStop\nnow"),
		claude(call("toolu_01", "codegraph", `{}`)),
		{Role: conversation.Tool, Content: []conversation.Block{{Type: conversation.ToolResult, ToolCallID: "toolu_01", ToolName: "codegraph", Content: []conversation.Block{text("[Uneventful result elided]")}}}},
		claude(text("done")),
	})
}

// Oh My Pi's post-render format: trailing white space trimmed, a run of blank lines removed whole,
// one blank line kept between text, table rows compacted outside code fences, blank lines dropped
// before a closing tag and at either end.
func TestFormatPromptIsOhMyPisPostRenderFormat(t *testing.T) {
	for _, testCase := range []struct{ in, want string }{
		{"a  \nb\t\n", "a\nb"},
		{"a\n\nb", "a\n\nb"},
		{"a\n\n\nb", "a\nb"},
		{"\n\na\n", "a"},
		{"<summary>\nx\n\n</summary>\n", "<summary>\nx\n</summary>"},
		{"| a |  b |\n|:--|--:|", "|a|b|\n|:---|---:|"},
		{"```\n| a |\n\n\n```", "```\n| a |\n\n\n```"},
	} {
		if got := formatPrompt(testCase.in); got != testCase.want {
			t.Errorf("formatPrompt(%q) = %q, want %q", testCase.in, got, testCase.want)
		}
	}
}
