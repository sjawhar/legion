package claude

import (
	"bytes"
	"encoding/json"

	"github.com/sjawhar/legion/daemon/internal/conversation"
)

// transcriptTypes are the line types Claude Code keeps in its message map, each with a uuid and a
// parentUuid (sessionStorage's transcript messages). A progress line is not kept: its children are
// re-parented to its own parent.
var transcriptTypes = map[string]bool{"user": true, "assistant": true, "attachment": true, "system": true, "progress": true}

// metadataTypes are the other line types the pinned Claude Code writes (its transcript writer's
// routing table, every entry of it but the transcript types): titles, tags, the leaf checkpoint,
// file history, cost and request records. None is part of the conversation the model sees.
// last-prompt says which leaf a resume continues and custom-title is the session's title; a
// content-replacement of the main session's tool results refuses the transcript, since Claude
// Code sends the replacement in place of the result only while a setting it does not record is
// on. Any type outside both sets refuses the transcript.
var metadataTypes = map[string]bool{
	"summary": true, "custom-title": true, "ended-by-model": true, "continued-in": true, "ai-title": true,
	"last-prompt": true, "tag": true, "relocated": true, "agent-name": true, "agent-color": true,
	"agent-setting": true, "pr-link": true, "frame-link": true, "artifact-comment-monitor": true,
	"artifact-autoreact-ledger": true, "bridge-session": true, "history-suppression": true,
	"file-history-snapshot": true, "file-history-delta": true, "attribution-snapshot": true, "mode": true,
	"permission-mode": true, "isolation-latch": true, "dev-mods": true, "memory-mode": true, "atis-latch": true,
	"worktree-state": true, "cost-state": true, "queue-operation": true, "content-replacement": true,
	"api-request-shape": true, "api-request-blob": true, "api-request": true, "fork-context-ref": true,
	"observer-ref": true,
}

// System line subtypes. A local command's output is sent to the model as a user turn and a
// compaction boundary roots the conversation after it; the noticeSubtypes are notices to the
// person (the system subtypes the pinned Claude Code renders, less these two, the microcompact
// boundary and the model consent fallback), which it keeps out of the request. Any other subtype
// refuses the transcript: among them a microcompact boundary, which clears old tool results in
// the request, a clearing the reader does not rebuild.
const (
	localCommand    = "local_command"
	compactBoundary = "compact_boundary"
)

var noticeSubtypes = map[string]bool{
	"api_error": true, "informational": true, "permission_retry": true, "stop_hook_summary": true,
	"turn_duration": true, "memory_saved": true, "agents_killed": true, "away_summary": true, "thinking": true,
	"file_snapshot": true, "bridge_status": true, "scheduled_task_fire": true, "read_divider": true,
	"model_fallback": true,
}

// entry is one transcript line, with the fields of every type the reader acts on.
type entry struct {
	Type        string  `json:"type"`
	Subtype     string  `json:"subtype"`
	UUID        string  `json:"uuid"`
	ParentUUID  *string `json:"parentUuid"`
	IsSidechain bool    `json:"isSidechain"`
	Timestamp   string  `json:"timestamp"`
	SessionID   string  `json:"sessionId"`
	Cwd         string  `json:"cwd"`
	Version     string  `json:"version"`
	GitBranch   string  `json:"gitBranch"`
	// user, assistant
	Message           *message `json:"message"`
	IsMeta            bool     `json:"isMeta"`
	IsCompactSummary  bool     `json:"isCompactSummary"`
	IsAPIErrorMessage bool     `json:"isApiErrorMessage"`
	Effort            string   `json:"effort"`
	// system
	Content         json.RawMessage  `json:"content"`
	CompactMetadata *compactMetadata `json:"compactMetadata"`
	// attachment
	Attachment *attachment `json:"attachment"`
	// last-prompt: leafUuid absent, null or a uuid
	LeafUUID json.RawMessage `json:"leafUuid"`
	Explicit bool            `json:"explicit"`
	// custom-title
	CustomTitle string `json:"customTitle"`
	// content-replacement: a subagent's (agentId) or the main session's
	AgentID      string            `json:"agentId"`
	Replacements []json.RawMessage `json:"replacements"`
}

// parent is the entry's parentUuid, "" for none.
func (e *entry) parent() string {
	if e.ParentUUID == nil {
		return ""
	}
	return *e.ParentUUID
}

func (e *entry) isBoundary() bool { return e.Type == "system" && e.Subtype == compactBoundary }

// isMessage is a user or assistant line: what a leaf walk stops at.
func (e *entry) isMessage() bool { return e.Type == "user" || e.Type == "assistant" }

// message is a user or assistant line's message.
type message struct {
	ID      string          `json:"id"`
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
}

// block is a content block of a message or of a tool result, with the fields of every type.
type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Signature string          `json:"signature"`
	Data      string          `json:"data"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
	Source    *imageSource    `json:"source"`
}

type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// compactMetadata is a compaction boundary's record of the messages it kept verbatim after its
// summary: a list of uuids, or a segment from head to tail, each re-linked after the anchor (the
// summary) when the transcript loads.
type compactMetadata struct {
	PreservedMessages *struct {
		AnchorUUID string   `json:"anchorUuid"`
		UUIDs      []string `json:"uuids"`
	} `json:"preservedMessages"`
	PreservedSegment *struct {
		HeadUUID   string `json:"headUuid"`
		AnchorUUID string `json:"anchorUuid"`
		TailUUID   string `json:"tailUuid"`
	} `json:"preservedSegment"`
}

// attachment is an attachment line's attachment: context Claude Code puts beside the
// conversation. Only a queued command, a prompt the person sent while a turn ran, carries the
// conversation's own content.
type attachment struct {
	Type   string          `json:"type"`
	Prompt json.RawMessage `json:"prompt"`
}

const queuedCommand = "queued_command"

// parseLines reads content as Claude Code's transcript loader does: one JSON object per line, a
// blank line or one that is not a JSON object (a torn last line) skipped. Unlike the loader, a
// well-formed line of a type the pinned Claude Code does not write refuses the transcript.
func parseLines(content []byte) ([]*entry, error) {
	var entries []*entry
	for line := range bytes.SplitSeq(content, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] != '{' || !json.Valid(line) {
			continue
		}
		var e entry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, conversation.Unknown("a transcript line of an unknown shape: %v", err)
		}
		switch {
		case transcriptTypes[e.Type]:
			if e.UUID == "" {
				return nil, conversation.Unknown("a %s line with no uuid", e.Type)
			}
		case metadataTypes[e.Type]:
		default:
			return nil, conversation.Unknown("a transcript line of type %q", e.Type)
		}
		entries = append(entries, &e)
	}
	return entries, nil
}
