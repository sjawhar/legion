package omp

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/sjawhar/legion/daemon/internal/conversation"
)

// sessionVersion is the one session header version this reader knows: the pinned Oh My Pi's
// CURRENT_SESSION_VERSION. Older files Oh My Pi migrates when it loads them, and a newer one it
// would read as current; the reader refuses both rather than guess.
const sessionVersion = 3

// entryTypes are the pinned Oh My Pi's session entry types (session-entries.ts SessionEntry). Any
// other type refuses the session.
var entryTypes = map[string]bool{
	"message": true, "model_usage": true, "thinking_level_change": true, "model_change": true,
	"service_tier_change": true, "compaction": true, "branch_summary": true, "custom": true,
	"custom_message": true, "label": true, "title_change": true, "ttsr_injection": true,
	"session_init": true, "mode_change": true, "credential_pin": true, "reset_boundary": true,
}

// header is a session file's header record: its first record, after the title slot.
type header struct {
	Type          string `json:"type"`
	Version       *int   `json:"version"`
	ID            string `json:"id"`
	Timestamp     string `json:"timestamp"`
	Cwd           string `json:"cwd"`
	ParentSession string `json:"parentSession"`
}

// entry is one session entry, with the fields any type the reader acts on carries.
type entry struct {
	Type      string  `json:"type"`
	ID        string  `json:"id"`
	ParentID  *string `json:"parentId"`
	Timestamp string  `json:"timestamp"`
	// message
	Message *message `json:"message"`
	// custom, custom_message
	CustomType  string          `json:"customType"`
	Data        json.RawMessage `json:"data"`
	Content     json.RawMessage `json:"content"`
	Attribution *string         `json:"attribution"`
	// compaction, branch_summary
	Summary                      string                     `json:"summary"`
	FirstKeptEntryID             string                     `json:"firstKeptEntryId"`
	ProviderReplayThroughEntryID string                     `json:"providerReplayThroughEntryId"`
	Method                       string                     `json:"method"`
	Details                      json.RawMessage            `json:"details"`
	PreserveData                 map[string]json.RawMessage `json:"preserveData"`
	// thinking_level_change
	ThinkingLevel *string `json:"thinkingLevel"`
	// model_change
	Model string  `json:"model"`
	Role  *string `json:"role"`
}

// message is a message entry's message, with the fields of every role the reader acts on.
type message struct {
	Role        string          `json:"role"`
	Content     json.RawMessage `json:"content"`
	Attribution string          `json:"attribution"`
	Steering    bool            `json:"steering"`
	// assistant
	API             string          `json:"api"`
	Provider        string          `json:"provider"`
	Model           string          `json:"model"`
	StopReason      string          `json:"stopReason"`
	RetryRecovery   bool            `json:"retryRecovery"`
	ProviderPayload json.RawMessage `json:"providerPayload"`
	// toolResult
	ToolCallID string   `json:"toolCallId"`
	ToolName   string   `json:"toolName"`
	IsError    bool     `json:"isError"`
	PrunedAt   *float64 `json:"prunedAt"`
	// custom
	CustomType string `json:"customType"`
	// bashExecution, pythonExecution
	ExcludeFromContext bool `json:"excludeFromContext"`
}

// block is a content block of a message, with the fields of every block type.
type block struct {
	Type              string          `json:"type"`
	Text              string          `json:"text"`
	Thinking          string          `json:"thinking"`
	ThinkingSignature string          `json:"thinkingSignature"`
	Data              string          `json:"data"`
	MimeType          string          `json:"mimeType"`
	ID                string          `json:"id"`
	Name              string          `json:"name"`
	Arguments         json.RawMessage `json:"arguments"`
}

// titleSlot is the fixed-width first line Oh My Pi keeps the session's title in
// (session-title-slot.ts), when the file has one.
type titleSlot struct {
	Type      string  `json:"type"`
	V         int     `json:"v"`
	Title     *string `json:"title"`
	Source    *string `json:"source"`
	UpdatedAt *string `json:"updatedAt"`
	Pad       *string `json:"pad"`
}

// parsed is a session's records as Oh My Pi's loader reads them.
type parsed struct {
	slot    *titleSlot
	header  header
	entries []*entry
}

// parseSession reads content as Oh My Pi's lenient JSONL loader does (session-loader.ts): a
// first line that is a title slot is set aside, a blank line or a line that is not JSON (a torn
// last record) is skipped, and the first record must be the session header. Unlike the loader,
// a well-formed record of a type, or a header version, the pinned Oh My Pi does not write refuses
// the session.
func parseSession(content []byte) (*parsed, error) {
	var p parsed
	first := true
	sawHeader := false
	for len(content) > 0 {
		line := content
		if i := bytes.IndexByte(content, '\n'); i >= 0 {
			line, content = content[:i], content[i+1:]
		} else {
			content = nil
		}
		line = bytes.TrimSpace(line)
		wasFirst := first
		first = false
		if len(line) == 0 || !json.Valid(line) {
			continue
		}
		if wasFirst {
			if slot, ok := parseTitleSlot(line); ok {
				p.slot = slot
				continue
			}
		}
		if !sawHeader {
			if err := json.Unmarshal(line, &p.header); err != nil || p.header.Type != "session" || p.header.ID == "" {
				return nil, conversation.Unknown("the session's first record is not an Oh My Pi session header")
			}
			if p.header.Version == nil || *p.header.Version != sessionVersion {
				version := "none"
				if p.header.Version != nil {
					version = fmt.Sprint(*p.header.Version)
				}
				return nil, conversation.Unknown("session header version %s", version)
			}
			sawHeader = true
			continue
		}
		var e entry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, conversation.Unknown("a session record Oh My Pi's entry shape does not fit: %v", err)
		}
		if e.Type == "session" {
			// The loader keeps every later header record out of the entries.
			continue
		}
		if !entryTypes[e.Type] {
			return nil, conversation.Unknown("session entry type %q", e.Type)
		}
		if e.ID == "" {
			return nil, conversation.Unknown("a %s entry with no id", e.Type)
		}
		if e.Type == "message" && e.Message == nil {
			return nil, conversation.Unknown("message entry %s with no message", e.ID)
		}
		p.entries = append(p.entries, &e)
	}
	if !sawHeader {
		return nil, conversation.Unknown("no Oh My Pi session header")
	}
	return &p, nil
}

// parseTitleSlot is Oh My Pi's parseTitleSlotLine: a title slot, or false for a line that is not
// one (which is then the header).
func parseTitleSlot(line []byte) (*titleSlot, bool) {
	var slot titleSlot
	if json.Unmarshal(line, &slot) != nil || slot.Type != "title" || slot.V != 1 ||
		slot.Title == nil || slot.UpdatedAt == nil || slot.Pad == nil {
		return nil, false
	}
	if slot.Source != nil && *slot.Source != "auto" && *slot.Source != "user" {
		return nil, false
	}
	return &slot, true
}

// parseBlocks reads a message's content: a string is one text block, an array its blocks, each of
// a type allowed lists. A content of any other shape, or a block of another type, refuses.
func parseBlocks(raw json.RawMessage, allowed map[string]bool, where string) ([]block, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, conversation.Unknown("%s has no content", where)
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []block{{Type: "text", Text: text}}, nil
	}
	var blocks []block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, conversation.Unknown("%s content Oh My Pi's block shape does not fit: %v", where, err)
	}
	for _, b := range blocks {
		if !allowed[b.Type] {
			return nil, conversation.Unknown("%s content block type %q", where, b.Type)
		}
	}
	return blocks, nil
}

// The block types each kind of content holds (pi-ai types.ts): a user, developer, custom or tool
// result message holds text and images; an assistant message also thinking, redacted thinking
// and tool calls. anthropicServerTool and fallback blocks are Oh My Pi's too, but replayed only
// under conditions the reader does not know, so they refuse the session.
var (
	inputBlocks     = map[string]bool{"text": true, "image": true}
	assistantBlocks = map[string]bool{"text": true, "image": true, "thinking": true, "redactedThinking": true, "toolCall": true}
)
