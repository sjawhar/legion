package omp

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf16"

	"github.com/sjawhar/legion/daemon/internal/conversation"
)

// The provider APIs the reader rebuilds a request for.
const (
	anthropicMessages = "anthropic-messages"
	openaiResponses   = "openai-responses"
)

// target is the model the conversation resumes on, which Oh My Pi's provider layer
// (pi-ai transform-messages.ts) shapes the history for, and the endpoint it is called through.
type target struct {
	provider, api, model string
	// signing is an Anthropic endpoint that verifies thinking signatures and replays no unsigned
	// thinking (Options).
	signing bool
}

// toRequest is the pinned Oh My Pi's transformMessages for the target, then each message in the
// neutral model: malformed tool calls dropped, each assistant turn's thinking kept, signed, demoted
// to text or dropped as the target replays it, tool call ids as the target's request carries them,
// every tool call followed by its result; and, on Anthropic, a "Continue." user turn between two
// assistant turns, as Oh My Pi's encoder puts one.
func toRequest(messages []llmMessage, t target) ([]conversation.Message, error) {
	if t.api != anthropicMessages && t.api != openaiResponses {
		return nil, conversation.Unknown("the conversation runs on provider API %q, which the reader does not rebuild a request for", t.api)
	}
	messages = dropMalformedToolCalls(messages)
	latest := -1
	for i, m := range messages {
		if m.role == "assistant" && !truncatedThinkingOnly(m) {
			latest = i
		}
	}
	// The ids an Anthropic request gives tool calls, by their own id and, for an OpenAI Responses
	// call, by its call_id, which pairs a result whose item half differs.
	idOf := map[string]string{}
	out := make([]conversation.Message, len(messages))
	for i, m := range messages {
		var err error
		switch m.role {
		case "user":
			out[i], err = userMessage(m)
		case "assistant":
			out[i], err = assistantMessage(m, t, i == latest, idOf)
		case "toolResult":
			out[i], err = toolResultMessage(m)
		}
		if err != nil {
			return nil, err
		}
	}
	for i, m := range messages {
		if m.role != "toolResult" {
			continue
		}
		result := &out[i].Content[0]
		if t.api == openaiResponses {
			result.ToolCallID = responsesCallID(m.toolCallID)
		} else if id, ok := idOf[m.toolCallID]; ok {
			result.ToolCallID = id
		} else if id, ok := idOf[responsesCallID(m.toolCallID)]; ok && strings.Contains(m.toolCallID, "|") {
			result.ToolCallID = id
		}
	}
	paired, err := pairToolResults(messages, out)
	if err != nil || t.api != anthropicMessages {
		return paired, err
	}
	var continued []conversation.Message
	for i, m := range paired {
		if i > 0 && m.Role == conversation.Assistant && paired[i-1].Role == conversation.Assistant {
			continued = append(continued, conversation.Message{Role: conversation.User, Content: []conversation.Block{{Type: conversation.Text, Text: "Continue."}}})
		}
		continued = append(continued, m)
	}
	return continued, nil
}

// userMessage is a user message's text and images; blank text is dropped, as both providers'
// encoders drop it.
func userMessage(m llmMessage) (conversation.Message, error) {
	msg := conversation.Message{Role: conversation.User, Native: m.native}
	for _, b := range m.blocks {
		converted, keep, err := inputBlock(b)
		if err != nil {
			return conversation.Message{}, err
		}
		if keep && (converted.Type != conversation.Text || strings.TrimFunc(converted.Text, isJSSpace) != "") {
			msg.Content = append(msg.Content, converted)
		}
	}
	return msg, nil
}

func toolResultMessage(m llmMessage) (conversation.Message, error) {
	result := conversation.Block{Type: conversation.ToolResult, ToolCallID: m.toolCallID, ToolName: m.toolName, IsError: m.isError}
	for _, b := range m.blocks {
		converted, keep, err := inputBlock(b)
		if err != nil {
			return conversation.Message{}, err
		}
		if keep {
			result.Content = append(result.Content, converted)
		}
	}
	return conversation.Message{Role: conversation.Tool, Content: []conversation.Block{result}}, nil
}

// inputBlock is a text or image block of a user message or tool result. An image Oh My Pi moved
// to its blob store (data "blob:sha256:…") refuses the session: the reader does not read the store.
func inputBlock(b block) (conversation.Block, bool, error) {
	switch b.Type {
	case "text":
		return conversation.Block{Type: conversation.Text, Text: b.Text}, true, nil
	case "image":
		if strings.HasPrefix(b.Data, "blob:") {
			return conversation.Block{}, false, conversation.Unknown("an image held in Oh My Pi's blob store")
		}
		return conversation.Block{Type: conversation.Image, MediaType: b.MimeType, ImageData: b.Data}, true, nil
	}
	return conversation.Block{}, false, nil
}

// assistantMessage is an assistant turn as the target's request replays it: transformMessages'
// first pass for the target, then the Anthropic encoder's rule for the turn's thinking.
func assistantMessage(m llmMessage, t target, latest bool, idOf map[string]string) (conversation.Message, error) {
	msg := conversation.Message{Role: conversation.Assistant, Provider: m.provider, API: m.api, Model: m.model}
	native, err := nativeHistory(m, t)
	if err != nil {
		return conversation.Message{}, err
	}
	msg.Native = native
	sameModel := m.provider == t.provider && m.api == t.api && m.model == t.model
	anthropicTarget := t.api == anthropicMessages
	anthropicReplay := anthropicTarget && m.api == anthropicMessages
	official := anthropicTarget && t.signing
	sameDeployment := anthropicReplay && m.provider == t.provider && official
	signingInvolved := (anthropicReplay && m.provider == "anthropic") || official
	replaysUnsigned := anthropicTarget && !t.signing
	abandonedToolUse := m.stopReason != "toolUse" && hasToolCall(m.blocks)
	crossProvider := m.provider != t.provider

	// Each source block's replay, nil when it is dropped; demoted marks thinking replayed as text.
	converted := make([]*conversation.Block, len(m.blocks))
	demoted := make([]bool, len(m.blocks))
	visibleThinking, survivingThinking := false, false
	for i, b := range m.blocks {
		switch b.Type {
		case "thinking":
			visibleThinking = true
			thinking := conversation.Block{Type: conversation.Thinking, Text: b.Thinking, Signature: b.ThinkingSignature}
			switch {
			case anthropicReplay && latest && abandonedToolUse && !crossProvider:
				// Anthropic's latest turn replays byte for byte.
			case anthropicReplay:
				if abandonedToolUse {
					thinking.Signature = ""
				}
				stale := !sameDeployment && ((latest && crossProvider) || (!latest && !sameModel))
				if stale && signingInvolved {
					thinking.Signature = ""
				}
				if blank(thinking.Signature) && (blank(b.Thinking) || (sameModel && t.signing)) {
					continue
				}
			case sameModel && b.ThinkingSignature != "":
			case blank(b.Thinking):
				continue
			case sameModel || replaysUnsigned:
				thinking.Signature = ""
			default:
				text, err := demotedThinking(t.model, b.Thinking)
				if err != nil {
					return conversation.Message{}, err
				}
				thinking = conversation.Block{Type: conversation.Text, Text: text}
				demoted[i] = true
			}
			survivingThinking = true
			converted[i] = &thinking
		case "text":
			converted[i] = &conversation.Block{Type: conversation.Text, Text: b.Text}
		case "toolCall":
			id := b.ID
			switch {
			case t.api == openaiResponses:
				id = responsesCallID(b.ID)
			case !sameModel || official:
				id = anthropicToolCallID(b.ID)
				idOf[b.ID] = id
				if m.api == openaiResponses {
					idOf[responsesCallID(b.ID)] = id
				}
			}
			arguments := b.Arguments
			if len(arguments) == 0 || string(arguments) == "null" {
				arguments = json.RawMessage("{}")
			}
			converted[i] = &conversation.Block{Type: conversation.ToolCall, ToolCallID: id, ToolName: b.Name, Arguments: arguments}
		}
	}
	// Redacted thinking replays only on Anthropic, beside the turn's visible thinking.
	dropsAllVisible := anthropicReplay && sameModel && t.signing && visibleThinking && !survivingThinking
	for i, b := range m.blocks {
		if b.Type == "redactedThinking" && anthropicReplay && !dropsAllVisible &&
			(sameModel || sameDeployment || (latest && !crossProvider) || replaysUnsigned) {
			converted[i] = &conversation.Block{Type: conversation.RedactedThinking, Data: b.Data}
		}
	}
	last := -1
	for i, block := range converted {
		if block != nil {
			last = i
		}
	}
	if last >= 0 && demoted[last] {
		converted[last].Text = strings.TrimRightFunc(converted[last].Text, isJSSpace)
	}
	signed := false
	for _, block := range converted {
		if block != nil && block.Type == conversation.Thinking && !blank(block.Signature) {
			signed = true
		}
	}
	for _, block := range converted {
		if block == nil {
			continue
		}
		switch {
		case block.Type == conversation.Text && blank(block.Text):
			// Both encoders drop blank text.
			continue
		case block.Type == conversation.RedactedThinking && blank(block.Data):
			continue
		case block.Type == conversation.Thinking && anthropicTarget && blank(block.Signature):
			// The Anthropic encoder sends unsigned thinking as thinking with an empty signature
			// where the endpoint replays it and the turn holds no signed thinking, else as text.
			if blank(block.Text) {
				continue
			}
			if signed || !replaysUnsigned {
				text, err := demotedThinking(t.model, block.Text)
				if err != nil {
					return conversation.Message{}, err
				}
				*block = conversation.Block{Type: conversation.Text, Text: text}
			} else {
				block.Signature = ""
			}
		}
		msg.Content = append(msg.Content, *block)
	}
	return msg, nil
}

// nativeHistory is the OpenAI Responses history a turn carries when the target replays it: an
// openaiResponsesHistory payload of a turn on the target's API and model, from the target's
// provider. A payload of a type Oh My Pi does not write refuses the session.
func nativeHistory(m llmMessage, t target) (*conversation.Native, error) {
	if len(m.payload) == 0 || string(m.payload) == "null" {
		return nil, nil
	}
	var payload struct {
		Type     string            `json:"type"`
		Provider *string           `json:"provider"`
		DT       bool              `json:"dt"`
		Items    []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(m.payload, &payload); err != nil {
		return nil, conversation.Unknown("an assistant providerPayload Oh My Pi's shape does not fit: %v", err)
	}
	switch payload.Type {
	case string(conversation.OpenAIResponsesHistory):
	case "cursorHistory":
		return nil, nil
	case string(conversation.AnthropicCompaction):
		return nil, conversation.Unknown("an assistant turn carrying an Anthropic compaction block")
	default:
		return nil, conversation.Unknown("providerPayload type %q", payload.Type)
	}
	provider := m.provider
	if payload.Provider != nil {
		provider = *payload.Provider
	}
	if m.api != t.api || m.model != t.model || provider != t.provider || payload.Items == nil {
		return nil, nil
	}
	return &conversation.Native{Kind: conversation.OpenAIResponsesHistory, Provider: provider, Incremental: payload.DT, Items: payload.Items, Payload: m.payload}, nil
}

var validAnthropicToolCallID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// anthropicToolCallID is a tool call id as an Anthropic request carries it: a valid id as it is,
// any other with each character outside [a-zA-Z0-9_-] replaced by '_' and cut to 64 (pi-ai
// utils.ts normalizeToolCallId), so an OpenAI "call_…|fc_…" id keeps both halves.
func anthropicToolCallID(id string) string {
	if validAnthropicToolCallID.MatchString(id) {
		return id
	}
	var b strings.Builder
	for _, r := range id {
		if r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
			if r > 0xFFFF {
				// JavaScript's replace sees a surrogate pair as two code units.
				b.WriteByte('_')
			}
		}
	}
	sanitized := b.String()
	if len(sanitized) > 64 {
		sanitized = sanitized[:64]
	}
	return sanitized
}

// responsesCallID is the Responses API's call_id of a "call_id|item_id" tool call id: the part
// before the '|' (pi-ai utils.ts normalizeResponsesToolCallId).
func responsesCallID(id string) string {
	if i := strings.IndexAny(id, "|\n"); i > 0 {
		return id[:i]
	}
	return id
}

// demotedThinking is renderDemotedThinking: reasoning another model produced, as text in the
// target model's own dialect. Claude models take it bare; OpenAI's harmony dialect in a <think>
// block. Another family's dialect the reader does not render, and refuses.
func demotedThinking(model, thinking string) (string, error) {
	lower := strings.ToLower(model)
	switch {
	case strings.Contains(lower, "claude"):
		return thinking, nil
	case strings.HasPrefix(lower, "gpt") || strings.Contains(lower, "/gpt") || openaiReasoningModel.MatchString(lower):
		return fmt.Sprintf("<think>\n%s\n</think>", thinking), nil
	}
	return "", conversation.Unknown("another model's reasoning demoted for model %q, whose dialect the reader does not render", model)
}

// openaiReasoningModel is an OpenAI o-series model id ("o3", "o4-mini").
var openaiReasoningModel = regexp.MustCompile(`^o\d`)

func blank(s string) bool { return strings.TrimFunc(s, isJSSpace) == "" }

func hasToolCall(blocks []block) bool {
	for _, b := range blocks {
		if b.Type == "toolCall" {
			return true
		}
	}
	return false
}

// truncatedThinkingOnly is shouldDropTruncatedThinkingOnlyAssistant: a turn the provider cut off
// (max tokens) with neither text nor a tool call is not replayed.
func truncatedThinkingOnly(m llmMessage) bool {
	if m.stopReason != "length" && m.stopReason != "error" && m.stopReason != "aborted" {
		return false
	}
	for _, b := range m.blocks {
		if b.Type == "toolCall" || b.Type == "text" {
			return false
		}
	}
	return true
}

// dropMalformedToolCalls is sanitizeMalformedToolCalls: a tool call with a blank id, or a name no
// provider could have declared, is dropped with the result answering it, and a turn left with no
// content with it.
func dropMalformedToolCalls(messages []llmMessage) []llmMessage {
	malformed := false
	for _, m := range messages {
		for _, b := range m.blocks {
			if m.role == "assistant" && b.Type == "toolCall" && malformedToolCall(b) {
				malformed = true
			}
		}
	}
	if !malformed {
		return messages
	}
	queues := map[string][]bool{}
	var out []llmMessage
	for _, m := range messages {
		switch m.role {
		case "assistant":
			clear(queues)
			var kept []block
			for _, b := range m.blocks {
				if b.Type == "toolCall" {
					bad := malformedToolCall(b)
					queues[b.ID] = append(queues[b.ID], bad)
					if bad {
						continue
					}
				}
				kept = append(kept, b)
			}
			if len(kept) == 0 {
				continue
			}
			m.blocks = kept
			out = append(out, m)
		case "toolResult":
			if queue := queues[m.toolCallID]; len(queue) > 0 {
				drop := queue[0]
				queues[m.toolCallID] = queue[1:]
				if drop {
					continue
				}
			}
			out = append(out, m)
		default:
			clear(queues)
			out = append(out, m)
		}
	}
	return out
}

func malformedToolCall(b block) bool {
	if blank(b.ID) || b.Name == "" || len(utf16.Encode([]rune(b.Name))) > 128 {
		return true
	}
	for _, r := range b.Name {
		if isJSSpace(r) || unicode.Is(unicode.Cc, r) {
			return true
		}
	}
	return false
}

// pairToolResults is transformMessages' second pass: each surviving tool call is followed at once
// by exactly one result, its first real result after the call, pulled forward past other messages,
// else a synthetic "No result provided" error. A result whose call is gone becomes a user note in
// <stale-tool-result> (or is dropped while a call still waits for its result); a second result of
// one call is dropped. Two tool calls sharing one id, which Oh My Pi renames apart, refuse the
// session.
func pairToolResults(source []llmMessage, messages []conversation.Message) ([]conversation.Message, error) {
	calls := map[string]bool{}
	for _, m := range messages {
		for _, b := range m.Content {
			if m.Role == conversation.Assistant && b.Type == conversation.ToolCall {
				if calls[b.ToolCallID] {
					return nil, conversation.Unknown("two tool calls with id %s", b.ToolCallID)
				}
				calls[b.ToolCallID] = true
			}
		}
	}
	type indexed struct {
		index    int
		consumed bool
	}
	results := map[string][]*indexed{}
	for i, m := range messages {
		if m.Role == conversation.Tool {
			id := m.Content[0].ToolCallID
			results[id] = append(results[id], &indexed{index: i})
		}
	}
	take := func(id string, after int) (conversation.Message, bool) {
		for _, r := range results[id] {
			if !r.consumed && r.index > after {
				r.consumed = true
				return messages[r.index], true
			}
		}
		return conversation.Message{}, false
	}
	var out []conversation.Message
	resolved := map[string]bool{}
	var pending []conversation.Block
	pendingStart := -1
	flush := func() {
		for _, call := range pending {
			if resolved[call.ToolCallID] {
				continue
			}
			resolved[call.ToolCallID] = true
			if result, ok := take(call.ToolCallID, pendingStart); ok {
				out = append(out, result)
				continue
			}
			out = append(out, conversation.Message{Role: conversation.Tool, Content: []conversation.Block{{
				Type: conversation.ToolResult, ToolCallID: call.ToolCallID, ToolName: call.ToolName, IsError: true,
				Content: []conversation.Block{{Type: conversation.Text, Text: "No result provided"}},
			}}})
		}
		pending = nil
	}
	for i, m := range messages {
		switch m.Role {
		case conversation.Assistant:
			flush()
			if truncatedThinkingOnly(source[i]) {
				continue
			}
			var turnCalls []conversation.Block
			for _, b := range m.Content {
				if b.Type == conversation.ToolCall {
					turnCalls = append(turnCalls, b)
				}
			}
			if len(turnCalls) > 0 {
				pending, pendingStart = turnCalls, i
			}
			if len(m.Content) > 0 || m.Native != nil {
				out = append(out, m)
			}
		case conversation.Tool:
			result := m.Content[0]
			if resolved[result.ToolCallID] {
				continue
			}
			if pendingHas(pending, result.ToolCallID) {
				resolved[result.ToolCallID] = true
				for _, r := range results[result.ToolCallID] {
					if r.index == i {
						r.consumed = true
					}
				}
				out = append(out, m)
				continue
			}
			if calls[result.ToolCallID] {
				continue
			}
			if pendingUnresolved(pending, resolved) {
				continue
			}
			var texts []string
			for _, part := range result.Content {
				if part.Type == conversation.Text && strings.TrimFunc(part.Text, isJSSpace) != "" {
					texts = append(texts, part.Text)
				}
			}
			if len(texts) > 0 {
				errorAttribute := ""
				if result.IsError {
					errorAttribute = ` is-error="true"`
				}
				out = append(out, conversation.Message{Role: conversation.User, Content: []conversation.Block{{
					Type: conversation.Text,
					Text: "<stale-tool-result tool=\"" + result.ToolName + "\" id=\"" + result.ToolCallID + "\"" + errorAttribute + ">\n" + strings.Join(texts, "\n") + "\n</stale-tool-result>",
				}}})
			}
		default:
			flush()
			if len(m.Content) > 0 || m.Native != nil {
				out = append(out, m)
			}
		}
	}
	flush()
	return out, nil
}

func pendingHas(pending []conversation.Block, id string) bool {
	for _, call := range pending {
		if call.ToolCallID == id {
			return true
		}
	}
	return false
}

func pendingUnresolved(pending []conversation.Block, resolved map[string]bool) bool {
	for _, call := range pending {
		if !resolved[call.ToolCallID] {
			return true
		}
	}
	return false
}
