package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/conversation"
)

const fixtureModel = "claude-opus-5-5"

// fixture builds a transcript's content a line at a time, each conversation line chained to the
// one before unless it names its own parent.
type fixture struct {
	lines []string
	last  string
	n     int
}

// add appends a conversation line of fields (its own fields, without parentUuid, uuid or the
// common trailer) after the last one, and returns its uuid.
func (f *fixture) add(fields string) string { return f.addAfter(f.last, fields) }

// addAfter appends a conversation line with parent as its parent ("" for null).
func (f *fixture) addAfter(parent, fields string) string {
	f.n++
	uuid := fmt.Sprintf("00000000-0000-4000-8000-%012d", f.n)
	parentJSON := "null"
	if parent != "" {
		parentJSON = quote(parent)
	}
	f.lines = append(f.lines, fmt.Sprintf(`{"parentUuid":%s,"isSidechain":false,%s,"uuid":%q,"timestamp":"2026-10-26T00:00:%02d.000Z","userType":"external","entrypoint":"sdk-cli","cwd":"/work","sessionId":"s-1","version":"2.1.310","gitBranch":"main"}`,
		parentJSON, fields, uuid, f.n%60))
	f.last = uuid
	return uuid
}

// raw appends line as it is.
func (f *fixture) raw(line string) { f.lines = append(f.lines, line) }

// checkpoint appends a last-prompt line naming leaf.
func (f *fixture) checkpoint(leaf string) {
	f.raw(`{"type":"last-prompt","lastPrompt":"x","leafUuid":` + quote(leaf) + `,"sessionId":"s-1"}`)
}

func (f *fixture) bytes() []byte { return []byte(strings.Join(f.lines, "\n") + "\n") }

func prompt(text string) string {
	return `"type":"user","message":{"role":"user","content":` + quote(text) + `}`
}

func userBlocksLine(blocks string) string {
	return `"type":"user","message":{"role":"user","content":` + blocks + `}`
}

// assistantLine is one assistant line of message id holding blocks.
func assistantLine(id, blocks string) string {
	return `"message":{"id":` + quote(id) + `,"type":"message","role":"assistant","model":"` + fixtureModel + `","content":` + blocks +
		`,"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}},"type":"assistant","effort":"high"`
}

func resultLine(id, content string) string {
	return `"type":"user","message":{"role":"user","content":[{"tool_use_id":` + quote(id) + `,"type":"tool_result","content":` + content + `}]},"toolUseResult":"x"`
}

func quote(s string) string {
	encoded, _ := json.Marshal(s)
	return string(encoded)
}

func read(t *testing.T, content []byte) *Session {
	t.Helper()
	s, err := Read(content)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return s
}

func text(s string) conversation.Block { return conversation.Block{Type: conversation.Text, Text: s} }

func user(blocks ...conversation.Block) conversation.Message {
	return conversation.Message{Role: conversation.User, Content: blocks}
}

func claude(blocks ...conversation.Block) conversation.Message {
	return conversation.Message{Role: conversation.Assistant, Provider: "anthropic", API: anthropicMessages, Model: fixtureModel, Content: blocks}
}

func synthetic() conversation.Message {
	return conversation.Message{Role: conversation.Assistant, Provider: "anthropic", API: anthropicMessages, Model: syntheticModel, Content: []conversation.Block{text(noResponse)}}
}

func result(id, name string, isError bool, content ...conversation.Block) conversation.Message {
	return conversation.Message{Role: conversation.Tool, Content: []conversation.Block{{
		Type: conversation.ToolResult, ToolCallID: id, ToolName: name, IsError: isError, Content: content,
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

// A transcript reads as the request Claude Code sends on resume: the prompt; the assistant lines
// of one message (signed thinking, redacted thinking, text, the tool call with its input byte for
// byte) merged into one message; the tool result, named for its call; the answer. Claude Code's
// context attachments and metadata lines are not in it. The session's model, effort, id, working
// directory, release and leaf come with it.
func TestReadRebuildsTheConversationClaudeCodeResumes(t *testing.T) {
	f := &fixture{}
	f.raw(`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-10-26T00:00:00.000Z","sessionId":"s-1","content":"Read note.txt."}`)
	f.add(prompt("Read note.txt."))
	f.add(`"attachment":{"type":"date","date":"2026-10-26"},"type":"attachment","rendered":[{"content":"<system-reminder>\nToday's date is 2026-10-26.\n</system-reminder>"}],"renderedRole":"system"`)
	f.add(assistantLine("msg_1", `[{"type":"thinking","thinking":"Read it.","signature":"c2ln"}]`))
	f.add(assistantLine("msg_1", `[{"type":"redacted_thinking","data":"cmVk"}]`))
	f.add(assistantLine("msg_1", `[{"type":"text","text":"Reading."}]`))
	f.add(assistantLine("msg_1", `[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path": "note.txt"}}]`))
	f.raw(`{"type":"api-request","sessionId":"s-1","id":"r-1","timestamp":"2026-10-26T00:00:00.000Z","version":"2.1.310","params":{}}`)
	f.add(resultLine("toolu_1", `"1\tplum"`))
	f.add(assistantLine("msg_2", `[{"type":"text","text":"It says plum."}]`))
	leaf := f.add(`"attachment":{"type":"prompt_snapshot"},"type":"attachment"`)
	f.checkpoint(leaf)
	f.raw(`{"type":"cost-state","sessionId":"s-1","totalCostUSD":0}`)

	s := read(t, f.bytes())
	assertMessages(t, s.Messages, []conversation.Message{
		user(text("Read note.txt.")),
		claude(thinking("Read it.", "c2ln"), conversation.Block{Type: conversation.RedactedThinking, Data: "cmVk"}, text("Reading."),
			call("toolu_1", "Read", `{"file_path": "note.txt"}`)),
		result("toolu_1", "Read", false, text("1\tplum")),
		claude(text("It says plum.")),
	})
	if s.Model != "anthropic/"+fixtureModel || s.API != anthropicMessages || s.ThinkingLevel != "high" {
		t.Errorf("model %q, API %q, thinking level %q", s.Model, s.API, s.ThinkingLevel)
	}
	if s.ID != "s-1" || s.Cwd != "/work" || s.Version != "2.1.310" || s.GitBranch != "main" || s.Leaf != leaf || s.Torn {
		t.Errorf("session %q, cwd %q, version %q, branch %q, leaf %q (want %q), torn %v", s.ID, s.Cwd, s.Version, s.GitBranch, s.Leaf, leaf, s.Torn)
	}
}

// The leaf is Claude Code's: the last row when it descends from the last checkpoint, else the
// checkpoint; a sidechain line and a dead branch are never on the chain; a progress line's child
// follows the progress line's parent.
func TestReadFollowsTheChainClaudeCodeResumes(t *testing.T) {
	t.Run("the last row after the checkpoint", func(t *testing.T) {
		f := &fixture{}
		f.add(prompt("One."))
		answer := f.add(assistantLine("msg_1", `[{"type":"text","text":"Two."}]`))
		f.checkpoint(answer)
		f.raw(`{"parentUuid":` + quote(answer) + `,"isSidechain":false,"type":"progress","data":{"type":"bash_progress"},"uuid":"progress-1","timestamp":"2026-10-26T00:01:00.000Z","sessionId":"s-1"}`)
		f.addAfter("progress-1", prompt("Three."))
		f.add(assistantLine("msg_2", `[{"type":"text","text":"Four."}]`))
		s := read(t, f.bytes())
		assertMessages(t, s.Messages, []conversation.Message{user(text("One.")), claude(text("Two.")), user(text("Three.")), claude(text("Four."))})
	})
	t.Run("a last row on another branch", func(t *testing.T) {
		f := &fixture{}
		first := f.add(prompt("One."))
		answer := f.add(assistantLine("msg_1", `[{"type":"text","text":"Two."}]`))
		f.checkpoint(answer)
		f.addAfter(first, prompt("A rewound prompt."))
		s := read(t, f.bytes())
		assertMessages(t, s.Messages, []conversation.Message{user(text("One.")), claude(text("Two."))})
		if s.Leaf != answer {
			t.Errorf("leaf %q, want the checkpoint %q", s.Leaf, answer)
		}
	})
	t.Run("a sidechain line", func(t *testing.T) {
		f := &fixture{}
		f.add(prompt("One."))
		answer := f.add(assistantLine("msg_1", `[{"type":"text","text":"Two."}]`))
		f.raw(strings.Replace(f.lines[len(f.lines)-1], `"isSidechain":false`, `"isSidechain":true`, 1))
		f.lines[len(f.lines)-1] = strings.Replace(f.lines[len(f.lines)-1], answer, "00000000-0000-4000-8000-0000000000aa", 1)
		s := read(t, f.bytes())
		assertMessages(t, s.Messages, []conversation.Message{user(text("One.")), claude(text("Two."))})
	})
}

// Parallel tool calls leave each result on a branch of its own: the reader puts back the call
// lines and results the chain from the leaf runs past, as Claude Code does, the results merged
// into one user turn in their lines' order.
func TestReadPutsBackParallelToolResults(t *testing.T) {
	f := &fixture{}
	f.add(prompt("Read both."))
	callA := f.add(assistantLine("msg_1", `[{"type":"tool_use","id":"toolu_A","name":"Read","input":{"file_path":"a"}}]`))
	callB := f.add(assistantLine("msg_1", `[{"type":"tool_use","id":"toolu_B","name":"Read","input":{"file_path":"b"}}]`))
	f.addAfter(callA, resultLine("toolu_A", `"a"`))
	f.addAfter(callB, resultLine("toolu_B", `"b"`))
	f.add(assistantLine("msg_2", `[{"type":"text","text":"Both read."}]`))
	s := read(t, f.bytes())
	assertMessages(t, s.Messages, []conversation.Message{
		user(text("Read both.")),
		claude(call("toolu_A", "Read", `{"file_path":"a"}`), call("toolu_B", "Read", `{"file_path":"b"}`)),
		result("toolu_A", "Read", false, text("a")),
		result("toolu_B", "Read", false, text("b")),
		claude(text("Both read.")),
	})
}

// After a compaction that kept messages verbatim, the conversation is the summary, the kept
// messages re-linked after it and what followed; nothing before the boundary but the kept
// messages is in it. A boundary that kept none roots the conversation at its summary, which, last,
// gets Claude Code's synthetic reply like any prompt never answered.
func TestReadAppliesACompactionAsClaudeCodeLoadsIt(t *testing.T) {
	t.Run("kept messages", func(t *testing.T) {
		f := &fixture{}
		f.add(prompt("Old prompt."))
		kept := f.add(assistantLine("msg_1", `[{"type":"text","text":"Kept answer."}]`))
		boundary := f.addAfter("", `"logicalParentUuid":`+quote(kept)+`,"type":"system","subtype":"compact_boundary","content":"Conversation compacted","level":"info","compactMetadata":{"trigger":"manual","preTokens":30,"preservedSegment":{"headUuid":`+quote(kept)+`,"anchorUuid":"00000000-0000-4000-8000-000000000004","tailUuid":`+quote(kept)+`}}`)
		summary := f.addAfter(boundary, prompt("Summary of the old prompt.")+`,"isVisibleInTranscriptOnly":true,"isCompactSummary":true`)
		f.addAfter(summary, prompt("New prompt."))
		f.add(assistantLine("msg_2", `[{"type":"text","text":"New answer."}]`))
		s := read(t, f.bytes())
		assertMessages(t, s.Messages, []conversation.Message{
			user(text("Summary of the old prompt.")), claude(text("Kept answer.")), user(text("New prompt.")), claude(text("New answer.")),
		})
	})
	t.Run("no kept messages", func(t *testing.T) {
		f := &fixture{}
		f.add(prompt("Old prompt."))
		f.add(assistantLine("msg_1", `[{"type":"text","text":"Old answer."}]`))
		boundary := f.addAfter("", `"type":"system","subtype":"compact_boundary","content":"Conversation compacted","compactMetadata":{"trigger":"auto","preTokens":30}`)
		f.addAfter(boundary, prompt("Summary.")+`,"isCompactSummary":true`)
		s := read(t, f.bytes())
		assertMessages(t, s.Messages, []conversation.Message{user(text("Summary.")), synthetic()})
	})
}

// A resumed turn that did not end is closed as Claude Code records it on resume: tool calls with
// no result answered as interrupted, then the synthetic reply; a prompt never answered gets the
// synthetic reply; a local command at the tail, which asked for no answer, gets none. A tool call
// the next prompt abandoned drops its message, an API error record is dropped and the prompts
// either side of it merge, a newline at the seam.
func TestReadClosesTheTurnAsClaudeCodeResumesIt(t *testing.T) {
	interrupted := func(id string) conversation.Message {
		return result(id, "Read", true, text(interruptedResult))
	}
	for _, testCase := range []struct {
		name  string
		lines func(f *fixture)
		want  []conversation.Message
	}{
		{"an interrupted tool call", func(f *fixture) {
			f.add(assistantLine("msg_2", `[{"type":"text","text":"Reading."}]`))
			f.add(assistantLine("msg_2", `[{"type":"tool_use","id":"toolu_x","name":"Read","input":{}}]`))
		}, []conversation.Message{claude(text("Reading."), call("toolu_x", "Read", `{}`)), interrupted("toolu_x"), synthetic()}},
		{"an unanswered prompt", func(f *fixture) {
			f.add(prompt("Still there?"))
		}, []conversation.Message{user(text("Still there?")), synthetic()}},
		{"a local command at the tail", func(f *fixture) {
			f.add(prompt("<local-command-caveat>Caveat.</local-command-caveat>") + `,"isMeta":true`)
			f.add(prompt("<command-name>/rename</command-name>"))
			f.add(`"type":"system","subtype":"local_command","content":"<local-command-stdout>Renamed.</local-command-stdout>","level":"info"`)
		}, []conversation.Message{user(text("<local-command-caveat>Caveat.</local-command-caveat>\n"), text("<command-name>/rename</command-name>\n"), text("<local-command-stdout>Renamed.</local-command-stdout>"))}},
		{"an abandoned tool call", func(f *fixture) {
			f.add(assistantLine("msg_2", `[{"type":"text","text":"Checking."},{"type":"tool_use","id":"toolu_y","name":"Read","input":{}}]`))
			f.add(prompt("Never mind."))
			f.add(assistantLine("msg_3", `[{"type":"text","text":"OK."}]`))
		}, []conversation.Message{user(text("Never mind.")), claude(text("OK."))}},
		{"an API error record", func(f *fixture) {
			f.add(prompt("Again?"))
			f.add(`"type":"assistant","message":{"id":"e-1","model":"<synthetic>","role":"assistant","content":[{"type":"text","text":"API Error: 500"}]},"isApiErrorMessage":true`)
			f.add(prompt("Hello?"))
			f.add(assistantLine("msg_3", `[{"type":"text","text":"Here."}]`))
		}, []conversation.Message{user(text("Again?\n"), text("Hello?")), claude(text("Here."))}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			f := &fixture{}
			f.add(prompt("Start."))
			f.add(assistantLine("msg_1", `[{"type":"text","text":"Started."}]`))
			testCase.lines(f)
			s := read(t, f.bytes())
			assertMessages(t, s.Messages, append([]conversation.Message{user(text("Start.")), claude(text("Started."))}, testCase.want...))
		})
	}
}

// Images in a prompt and in a tool result read as images; a prompt queued during a turn reads as a
// user message of its own words.
func TestReadKeepsImagesAndQueuedPrompts(t *testing.T) {
	f := &fixture{}
	f.add(userBlocksLine(`[{"type":"text","text":"Look."},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1n"}}]`))
	f.add(assistantLine("msg_1", `[{"type":"tool_use","id":"toolu_1","name":"Read","input":{}},{"type":"tool_use","id":"toolu_2","name":"Read","input":{}}]`))
	f.add(resultLine("toolu_1", `[{"type":"text","text":"a picture"},{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"anBn"}}]`))
	f.add(resultLine("toolu_2", `"text"`))
	f.add(`"attachment":{"type":"queued_command","prompt":[{"type":"text","text":"Also this."}],"commandMode":"prompt"},"type":"attachment","rendered":[],"renderedRole":"system"`)
	f.add(assistantLine("msg_2", `[{"type":"text","text":"Seen."}]`))
	f.raw(`{"type":"content-replacement","sessionId":"s-1","agentId":"a-1","replacements":[{"kind":"tool-result","toolUseId":"toolu_2","replacement":"a subagent's"}]}`)
	s := read(t, f.bytes())
	assertMessages(t, s.Messages, []conversation.Message{
		user(text("Look."), conversation.Block{Type: conversation.Image, MediaType: "image/png", ImageData: "aW1n"}),
		claude(call("toolu_1", "Read", `{}`), call("toolu_2", "Read", `{}`)),
		result("toolu_1", "Read", false, text("a picture"), conversation.Block{Type: conversation.Image, MediaType: "image/jpeg", ImageData: "anBn"}),
		result("toolu_2", "Read", false, text("text")),
		user(text("Also this.")),
		claude(text("Seen.")),
	})
}

// A torn last line is skipped, as Claude Code skips it, and marks the session torn.
func TestReadSkipsATornLastLine(t *testing.T) {
	f := &fixture{}
	f.add(prompt("One."))
	f.add(assistantLine("msg_1", `[{"type":"text","text":"Two."}]`))
	content := append(f.bytes(), []byte(`{"parentUuid":"x","type":"user","message":{"role":"user","content":"cut o`)...)
	s := read(t, content)
	assertMessages(t, s.Messages, []conversation.Message{user(text("One.")), claude(text("Two."))})
	if !s.Torn || s.Size != int64(len(content)) {
		t.Errorf("torn %v, size %d; want torn at %d bytes", s.Torn, s.Size, len(content))
	}
}

// A well-formed line of a type, system subtype or content block the pinned Claude Code does not
// write refuses the transcript with ErrUnknownFormat, and so does what the reader cannot rebuild as
// Claude Code would: a microcompact boundary, which clears old tool results in the request, and a
// content replacement of the main session's tool results, which Claude Code sends only while a
// setting the transcript does not record is on.
func TestReadRefusesWhatItDoesNotKnow(t *testing.T) {
	for name, line := range map[string]string{
		"a line type":            `"type":"hologram"`,
		"a system subtype":       `"type":"system","subtype":"teleport","content":"x"`,
		"a microcompact":         `"type":"system","subtype":"microcompact_boundary","content":"x"`,
		"an assistant block":     assistantLine("msg_9", `[{"type":"server_tool_use","id":"s","name":"web_search","input":{}}]`),
		"a user block":           userBlocksLine(`[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"x"}}]`),
		"a tool result block":    resultLine("toolu_9", `[{"type":"tool_reference","tool_name":"WebFetch"}]`),
		"an image that is a URL": userBlocksLine(`[{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}]`),
	} {
		t.Run(name, func(t *testing.T) {
			f := &fixture{}
			f.add(prompt("One."))
			f.add(assistantLine("msg_1", `[{"type":"tool_use","id":"toolu_9","name":"Read","input":{}}]`))
			f.add(line)
			if _, err := Read(f.bytes()); !errors.Is(err, conversation.ErrUnknownFormat) {
				t.Fatalf("Read = %v, want ErrUnknownFormat", err)
			}
		})
	}
	t.Run("a content replacement", func(t *testing.T) {
		f := &fixture{}
		f.add(prompt("One."))
		f.raw(`{"type":"content-replacement","sessionId":"s-1","replacements":[{"kind":"tool-result","toolUseId":"toolu_1","replacement":"saved"}]}`)
		if _, err := Read(f.bytes()); !errors.Is(err, conversation.ErrUnknownFormat) {
			t.Fatalf("Read = %v, want ErrUnknownFormat", err)
		}
	})
}
