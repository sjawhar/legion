package omp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/conversation"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
)

// AnswerEntryType is the customType of the custom entry an append ends with, naming the question
// it answered. Oh My Pi keeps a custom entry for its extensions and never puts one in the model's
// context (session-context.ts reads only custom_message entries), so it is a marker the next
// reader finds and the model never sees.
const AnswerEntryType = "legion-answer"

// Encode is what an append of a after the session writes: one message entry for the question, one
// for each model turn and one for each tool result, as Oh My Pi writes them, then the
// legion-answer custom entry; each with a fresh 8-hex id and the previous entry, the session's leaf
// first, as its parent. When the session's content does not end in a newline the bytes start with
// one. now stamps every entry.
func (s *Session) Encode(a conversation.Append, now time.Time) ([]byte, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	if a.API != anthropicMessages && a.API != openaiResponses {
		return nil, fmt.Errorf("append: Oh My Pi's %s turns are not written by this appender", a.API)
	}
	w := entryWriter{session: s, parent: s.Leaf, now: now, taken: map[string]bool{}}
	if s.Torn {
		w.buf.WriteByte('\n')
	}
	ms := now.UnixMilli()
	if err := w.message(userOut{Role: "user", Content: []any{textOut{Type: "text", Text: a.Question}}, Attribution: "user", Timestamp: ms}); err != nil {
		return nil, err
	}
	for i, turn := range a.Turns {
		content, callIDs, payload, err := turnContent(a, turn)
		if err != nil {
			return nil, fmt.Errorf("append: turn %d: %w", i+1, err)
		}
		stopReason := "stop"
		if len(turn.Results) > 0 {
			stopReason = "toolUse"
		}
		assistant := assistantOut{
			Role: "assistant", Content: content, API: a.API, Provider: a.Provider, Model: a.Model,
			Usage: usageOf(turn.Usage), StopReason: stopReason, Timestamp: ms, ProviderPayload: payload,
		}
		if err := w.message(assistant); err != nil {
			return nil, err
		}
		for _, result := range turn.Results {
			resultContent := []any{}
			for _, part := range result.Content {
				if part.Type == conversation.Image {
					resultContent = append(resultContent, imageOut{Type: "image", Data: part.ImageData, MimeType: part.MediaType})
				} else {
					resultContent = append(resultContent, textOut{Type: "text", Text: part.Text})
				}
			}
			if err := w.message(toolResultOut{
				Role: "toolResult", ToolCallID: callIDs[result.ToolCallID], ToolName: result.ToolName,
				Content: resultContent, IsError: result.IsError, Timestamp: ms,
			}); err != nil {
				return nil, err
			}
		}
	}
	if err := w.custom(AnswerEntryType, map[string]string{"questionId": a.QuestionID}); err != nil {
		return nil, err
	}
	return w.buf.Bytes(), nil
}

// AppendFile appends a to the session file at path, which s was read from, in one O_APPEND write.
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
		return fmt.Errorf("session %s: %w: read %d bytes, now %d", path, conversation.ErrMoved, s.Size, info.Size())
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("append to session %s: %w", path, err)
	}
	return file.Close()
}

// AppendSQL appends a to the session Oh My Pi's session table holds at path, which s was read
// from, with Oh My Pi's own append statement (ompsessions.Append): refused with an error that Is
// conversation.ErrMoved when the row's length is no longer s's.
func AppendSQL(ctx context.Context, conn *pgx.Conn, path string, s *Session, a conversation.Append, now time.Time) error {
	data, err := s.Encode(a, now)
	if err != nil {
		return err
	}
	return ompsessions.Append(ctx, conn, path, s.Size, data, now)
}

// entryWriter writes entries one per line, each chained to the one before.
type entryWriter struct {
	session *Session
	parent  string
	now     time.Time
	taken   map[string]bool
	buf     bytes.Buffer
}

// newID is Oh My Pi's generateId: 8 hex characters no entry of the session holds.
func (w *entryWriter) newID() (string, error) {
	for {
		var raw [4]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", err
		}
		id := hex.EncodeToString(raw[:])
		if !w.session.ids[id] && !w.taken[id] {
			w.taken[id] = true
			return id, nil
		}
	}
}

func (w *entryWriter) parentID() *string {
	if w.parent == "" {
		return nil
	}
	parent := w.parent
	return &parent
}

func (w *entryWriter) message(m any) error {
	id, err := w.newID()
	if err != nil {
		return err
	}
	return w.line(id, messageEntryOut{Type: "message", ID: id, ParentID: w.parentID(), Timestamp: isoTime(w.now), Message: m})
}

func (w *entryWriter) custom(customType string, data any) error {
	id, err := w.newID()
	if err != nil {
		return err
	}
	return w.line(id, customEntryOut{Type: "custom", CustomType: customType, Data: data, ID: id, ParentID: w.parentID(), Timestamp: isoTime(w.now)})
}

func (w *entryWriter) line(id string, entry any) error {
	encoded, err := marshal(entry)
	if err != nil {
		return err
	}
	w.buf.Write(encoded)
	w.buf.WriteByte('\n')
	w.parent = id
	return nil
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

// turnContent is a model turn's content as Oh My Pi records it for the API, the session id of each
// of its tool calls by the call's id, and the provider payload. An Anthropic turn's blocks are
// recorded as they are; an OpenAI Responses turn is recorded from its output items, as Oh My Pi's
// provider builds it from the stream (openai-shared.ts): a reasoning item a thinking block (its
// summary, the item's id, whether it is a summary), a message item a text block with its id in a
// text signature, a function_call item a tool call "call_id|item_id"; the items themselves the
// openaiResponsesHistory payload, byte for byte.
func turnContent(a conversation.Append, turn conversation.Turn) ([]any, map[string]string, *payloadOut, error) {
	callIDs := map[string]string{}
	if a.API == anthropicMessages {
		var content []any
		for _, b := range turn.Content {
			switch b.Type {
			case conversation.Thinking:
				content = append(content, thinkingOut{Type: "thinking", Thinking: b.Text, ThinkingSignature: b.Signature})
			case conversation.RedactedThinking:
				content = append(content, redactedOut{Type: "redactedThinking", Data: b.Data})
			case conversation.Text:
				content = append(content, textOut{Type: "text", Text: b.Text})
			case conversation.ToolCall:
				content = append(content, toolCallOut{Type: "toolCall", ID: b.ToolCallID, Name: b.ToolName, Arguments: arguments(b.Arguments)})
				callIDs[b.ToolCallID] = b.ToolCallID
			}
		}
		return content, callIDs, nil, nil
	}
	if len(turn.Native) == 0 {
		return nil, nil, nil, fmt.Errorf("an OpenAI Responses turn with no output items")
	}
	var wantCalls []string
	for _, b := range turn.Content {
		if b.Type == conversation.ToolCall {
			wantCalls = append(wantCalls, b.ToolCallID)
		}
	}
	var content []any
	var calls []string
	for _, raw := range turn.Native {
		var item struct {
			Type    string `json:"type"`
			ID      string `json:"id"`
			Phase   string `json:"phase"`
			Summary []struct {
				Text string `json:"text"`
			} `json:"summary"`
			Content []struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Refusal string `json:"refusal"`
			} `json:"content"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, nil, nil, fmt.Errorf("an output item: %w", err)
		}
		switch item.Type {
		case "reasoning":
			var summaries []string
			hasSummary := false
			for _, part := range item.Summary {
				summaries = append(summaries, part.Text)
				hasSummary = hasSummary || part.Text != ""
			}
			thinking := strings.Join(summaries, "\n\n")
			if thinking == "" && len(item.Content) > 0 && item.Content[0].Type == "reasoning_text" {
				thinking = item.Content[0].Text
			}
			content = append(content, thinkingOut{Type: "thinking", Thinking: thinking, ItemID: item.ID, Summary: &hasSummary})
		case "message":
			var text strings.Builder
			for _, part := range item.Content {
				if part.Type == "output_text" {
					text.WriteString(part.Text)
				} else {
					text.WriteString(part.Refusal)
				}
			}
			signature, err := marshal(textSignature{V: 1, ID: item.ID, Phase: item.Phase})
			if err != nil {
				return nil, nil, nil, err
			}
			content = append(content, textOut{Type: "text", Text: text.String(), TextSignature: string(signature)})
		case "function_call":
			id := item.CallID + "|" + item.ID
			content = append(content, toolCallOut{Type: "toolCall", ID: id, Name: item.Name, Arguments: arguments(json.RawMessage(item.Arguments))})
			callIDs[item.CallID] = id
			calls = append(calls, item.CallID)
		default:
			return nil, nil, nil, fmt.Errorf("an output item of type %q", item.Type)
		}
	}
	if strings.Join(calls, "\n") != strings.Join(wantCalls, "\n") {
		return nil, nil, nil, fmt.Errorf("the output items call %v, the turn %v", calls, wantCalls)
	}
	return content, callIDs, &payloadOut{Type: string(conversation.OpenAIResponsesHistory), Provider: a.Provider, DT: true, Items: turn.Native}, nil
}

// arguments is a tool call's arguments as the object Oh My Pi records; anything else is {}.
func arguments(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return json.RawMessage("{}")
	}
	return trimmed
}

func usageOf(u conversation.Usage) usageOut {
	return usageOut{
		Input: u.InputTokens, Output: u.OutputTokens, CacheRead: u.CacheReadTokens, CacheWrite: u.CacheWriteTokens,
		TotalTokens:     u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens,
		ReasoningTokens: u.ReasoningTokens,
		Cost: costOut{
			Input: u.InputCost, Output: u.OutputCost, CacheRead: u.CacheReadCost, CacheWrite: u.CacheWriteCost,
			Total: u.InputCost + u.OutputCost + u.CacheReadCost + u.CacheWriteCost,
		},
	}
}

// The shapes Oh My Pi writes, field for field in its order.
type (
	userOut struct {
		Role        string `json:"role"`
		Content     []any  `json:"content"`
		Attribution string `json:"attribution"`
		Timestamp   int64  `json:"timestamp"`
	}
	messageEntryOut struct {
		Type      string  `json:"type"`
		ID        string  `json:"id"`
		ParentID  *string `json:"parentId"`
		Timestamp string  `json:"timestamp"`
		Message   any     `json:"message"`
	}
	customEntryOut struct {
		Type       string  `json:"type"`
		CustomType string  `json:"customType"`
		Data       any     `json:"data"`
		ID         string  `json:"id"`
		ParentID   *string `json:"parentId"`
		Timestamp  string  `json:"timestamp"`
	}
	assistantOut struct {
		Role            string      `json:"role"`
		Content         []any       `json:"content"`
		API             string      `json:"api"`
		Provider        string      `json:"provider"`
		Model           string      `json:"model"`
		Usage           usageOut    `json:"usage"`
		StopReason      string      `json:"stopReason"`
		Timestamp       int64       `json:"timestamp"`
		ProviderPayload *payloadOut `json:"providerPayload,omitempty"`
	}
	toolResultOut struct {
		Role       string `json:"role"`
		ToolCallID string `json:"toolCallId"`
		ToolName   string `json:"toolName"`
		Content    []any  `json:"content"`
		IsError    bool   `json:"isError"`
		Timestamp  int64  `json:"timestamp"`
	}
	usageOut struct {
		Input           int64   `json:"input"`
		Output          int64   `json:"output"`
		CacheRead       int64   `json:"cacheRead"`
		CacheWrite      int64   `json:"cacheWrite"`
		TotalTokens     int64   `json:"totalTokens"`
		ReasoningTokens int64   `json:"reasoningTokens,omitempty"`
		Cost            costOut `json:"cost"`
	}
	costOut struct {
		Input      float64 `json:"input"`
		Output     float64 `json:"output"`
		CacheRead  float64 `json:"cacheRead"`
		CacheWrite float64 `json:"cacheWrite"`
		Total      float64 `json:"total"`
	}
	payloadOut struct {
		Type     string            `json:"type"`
		Provider string            `json:"provider"`
		DT       bool              `json:"dt"`
		Items    []json.RawMessage `json:"items"`
	}
	textOut struct {
		Type          string `json:"type"`
		Text          string `json:"text"`
		TextSignature string `json:"textSignature,omitempty"`
	}
	thinkingOut struct {
		Type              string `json:"type"`
		Thinking          string `json:"thinking"`
		ThinkingSignature string `json:"thinkingSignature,omitempty"`
		ItemID            string `json:"itemId,omitempty"`
		Summary           *bool  `json:"summary,omitempty"`
	}
	redactedOut struct {
		Type string `json:"type"`
		Data string `json:"data"`
	}
	toolCallOut struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	imageOut struct {
		Type     string `json:"type"`
		Data     string `json:"data"`
		MimeType string `json:"mimeType"`
	}
	textSignature struct {
		V     int    `json:"v"`
		ID    string `json:"id"`
		Phase string `json:"phase,omitempty"`
	}
)
