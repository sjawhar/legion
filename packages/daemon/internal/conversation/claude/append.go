package claude

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/conversation"
)

// Encode is what an append of a after the transcript writes, line for line as Claude Code writes
// a turn: a user line with the question; for each model turn one assistant line per content block,
// the lines of a turn sharing one message id, then one user line per tool result; and the
// last-prompt line naming the last of them, the checkpoint Claude Code resumes from (without it,
// a transcript whose checkpoint is not the chain's last line resumes from the checkpoint and
// leaves the append off). Each conversation line has a fresh uuid and the line before it, the
// session's leaf first, as its parent, and the session's id, working directory, release and git
// branch; an assistant line records the session's effort (its thinking level) as Claude Code
// records the effort it ran at. When the transcript does not end in a newline the bytes start with
// one. now stamps every line.
func (s *Session) Encode(a conversation.Append, now time.Time) ([]byte, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	if a.API != anthropicMessages {
		return nil, fmt.Errorf("append: a Claude Code transcript holds %s turns, not %s", anthropicMessages, a.API)
	}
	if s.ID == "" {
		return nil, errors.New("append: the transcript names no session")
	}
	version := s.Version
	if version == "" {
		version = PinnedVersion
	}
	w := lineWriter{
		parent: s.Leaf,
		common: commonOut{UserType: "external", Entrypoint: "sdk-cli", Cwd: s.Cwd, SessionID: s.ID, Version: version, GitBranch: s.GitBranch},
		at:     isoTime(now),
		effort: s.ThinkingLevel,
	}
	if s.Torn {
		w.buf.WriteByte('\n')
	}
	promptID := newUUID()
	if err := w.user(promptID, a.Question, nil, ""); err != nil {
		return nil, err
	}
	for i, turn := range a.Turns {
		messageID := "msg_" + randomHex(12)
		stopReason := "end_turn"
		if len(turn.Results) > 0 {
			stopReason = "tool_use"
		}
		usage := usageOut{
			InputTokens: turn.Usage.InputTokens, CacheCreationInputTokens: turn.Usage.CacheWriteTokens,
			CacheReadInputTokens: turn.Usage.CacheReadTokens, OutputTokens: turn.Usage.OutputTokens,
		}
		callLines := map[string]string{}
		for _, b := range turn.Content {
			content, err := assistantBlockOut(b)
			if err != nil {
				return nil, fmt.Errorf("append: turn %d: %w", i+1, err)
			}
			uuid, err := w.assistant(assistantMessageOut{
				ID: messageID, Type: "message", Role: "assistant", Model: a.Model, Content: []any{content},
				StopReason: stopReason, Usage: usage,
			})
			if err != nil {
				return nil, err
			}
			if b.Type == conversation.ToolCall {
				callLines[b.ToolCallID] = uuid
			}
		}
		for _, r := range turn.Results {
			if err := w.user(promptID, nil, &r, callLines[r.ToolCallID]); err != nil {
				return nil, err
			}
		}
	}
	if err := w.write(lastPromptOut{Type: "last-prompt", LastPrompt: a.Question, LeafUUID: w.parent, SessionID: s.ID}); err != nil {
		return nil, err
	}
	return w.buf.Bytes(), nil
}

// AppendFile appends a to the transcript at path, which s was read from, in one O_APPEND write.
// It refuses with an error that Is conversation.ErrMoved when the file is no longer the size s was
// read at.
func AppendFile(path string, s *Session, a conversation.Append, now time.Time) error {
	data, err := s.Encode(a, now)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	if info.Size() != s.Size {
		_ = file.Close()
		return fmt.Errorf("transcript %s: %w: read %d bytes, now %d", path, conversation.ErrMoved, s.Size, info.Size())
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("append to transcript %s: %w", path, err)
	}
	return file.Close()
}

// lineWriter writes lines one per line, each conversation line chained to the one before.
type lineWriter struct {
	parent string
	common commonOut
	at     string
	effort string
	buf    bytes.Buffer
}

func (w *lineWriter) parentUUID() *string {
	if w.parent == "" {
		return nil
	}
	parent := w.parent
	return &parent
}

// user writes a user line: the question (text) or one tool result with the uuid of the assistant
// line holding its call. A tool result line carries toolUseResult, which is what tells Claude Code
// it is not a prompt the person typed; Claude Code puts the tool's own output there, the reader
// has its text.
func (w *lineWriter) user(promptID string, text any, result *conversation.Block, callLine string) error {
	line := userLineOut{ParentUUID: w.parentUUID(), PromptID: promptID, Type: "user", UUID: newUUID(), Timestamp: w.at, commonOut: w.common}
	if result == nil {
		line.Message = userMessageOut{Role: "user", Content: text}
	} else {
		var parts []any
		var texts []string
		for _, p := range result.Content {
			if p.Type == conversation.Image {
				parts = append(parts, imageOut{Type: "image", Source: imageSourceOut{Type: "base64", MediaType: p.MediaType, Data: p.ImageData}})
				continue
			}
			parts = append(parts, textOut{Type: "text", Text: p.Text})
			texts = append(texts, p.Text)
		}
		var content any = parts
		if len(parts) == 1 && len(texts) == 1 {
			content = texts[0]
		} else if len(parts) == 0 {
			content = ""
		}
		line.Message = userMessageOut{Role: "user", Content: []any{toolResultOut{ToolUseID: result.ToolCallID, Type: "tool_result", Content: content, IsError: result.IsError}}}
		line.ToolUseResult = strings.Join(texts, "\n")
		line.SourceToolAssistantUUID = callLine
	}
	if err := w.write(line); err != nil {
		return err
	}
	w.parent = line.UUID
	return nil
}

// assistant writes an assistant line and returns its uuid.
func (w *lineWriter) assistant(m assistantMessageOut) (string, error) {
	line := assistantLineOut{ParentUUID: w.parentUUID(), Message: m, Type: "assistant", UUID: newUUID(), Timestamp: w.at, Effort: w.effort, commonOut: w.common}
	if err := w.write(line); err != nil {
		return "", err
	}
	w.parent = line.UUID
	return line.UUID, nil
}

func (w *lineWriter) write(v any) error {
	encoded, err := marshal(v)
	if err != nil {
		return err
	}
	w.buf.Write(encoded)
	w.buf.WriteByte('\n')
	return nil
}

// assistantBlockOut is a model turn's block as Claude Code records it from the Messages API.
func assistantBlockOut(b conversation.Block) (any, error) {
	switch b.Type {
	case conversation.Text:
		return textOut{Type: "text", Text: b.Text}, nil
	case conversation.Thinking:
		return thinkingOut{Type: "thinking", Thinking: b.Text, Signature: b.Signature}, nil
	case conversation.RedactedThinking:
		return redactedOut{Type: "redacted_thinking", Data: b.Data}, nil
	case conversation.ToolCall:
		input := b.Arguments
		if !json.Valid(input) || !bytes.HasPrefix(bytes.TrimSpace(input), []byte("{")) {
			input = json.RawMessage("{}")
		}
		return toolUseOut{Type: "tool_use", ID: b.ToolCallID, Name: b.ToolName, Input: input}, nil
	}
	return nil, fmt.Errorf("a %s block in a model turn", b.Type)
}

// marshal is JSON as JavaScript's JSON.stringify writes it: no HTML escaping, no trailing newline.
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// isoTime is JavaScript's Date.prototype.toISOString.
func isoTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// newUUID is a random (version 4) UUID, the form of Claude Code's line uuids.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// The shapes Claude Code writes, field for field in its order.
type (
	// commonOut are the fields that end every conversation line.
	commonOut struct {
		UserType   string `json:"userType"`
		Entrypoint string `json:"entrypoint"`
		Cwd        string `json:"cwd"`
		SessionID  string `json:"sessionId"`
		Version    string `json:"version"`
		GitBranch  string `json:"gitBranch,omitempty"`
	}
	userLineOut struct {
		ParentUUID              *string        `json:"parentUuid"`
		IsSidechain             bool           `json:"isSidechain"`
		PromptID                string         `json:"promptId"`
		Type                    string         `json:"type"`
		Message                 userMessageOut `json:"message"`
		UUID                    string         `json:"uuid"`
		Timestamp               string         `json:"timestamp"`
		ToolUseResult           any            `json:"toolUseResult,omitempty"`
		SourceToolAssistantUUID string         `json:"sourceToolAssistantUUID,omitempty"`
		commonOut
	}
	userMessageOut struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	}
	assistantLineOut struct {
		ParentUUID  *string             `json:"parentUuid"`
		IsSidechain bool                `json:"isSidechain"`
		Message     assistantMessageOut `json:"message"`
		Type        string              `json:"type"`
		UUID        string              `json:"uuid"`
		Timestamp   string              `json:"timestamp"`
		Effort      string              `json:"effort,omitempty"`
		commonOut
	}
	assistantMessageOut struct {
		ID           string    `json:"id"`
		Type         string    `json:"type"`
		Role         string    `json:"role"`
		Model        string    `json:"model"`
		Content      []any     `json:"content"`
		StopReason   string    `json:"stop_reason"`
		StopSequence *string   `json:"stop_sequence"`
		Usage        usageOut  `json:"usage"`
		StopDetails  *struct{} `json:"stop_details"`
	}
	usageOut struct {
		InputTokens              int64 `json:"input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
	}
	lastPromptOut struct {
		Type       string `json:"type"`
		LastPrompt string `json:"lastPrompt"`
		LeafUUID   string `json:"leafUuid"`
		SessionID  string `json:"sessionId"`
	}
	textOut struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	thinkingOut struct {
		Type      string `json:"type"`
		Thinking  string `json:"thinking"`
		Signature string `json:"signature"`
	}
	redactedOut struct {
		Type string `json:"type"`
		Data string `json:"data"`
	}
	toolUseOut struct {
		Type  string          `json:"type"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	toolResultOut struct {
		ToolUseID string `json:"tool_use_id"`
		Type      string `json:"type"`
		Content   any    `json:"content"`
		IsError   bool   `json:"is_error,omitempty"`
	}
	imageOut struct {
		Type   string         `json:"type"`
		Source imageSourceOut `json:"source"`
	}
	imageSourceOut struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	}
)
