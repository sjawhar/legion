package answer

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/grafana/ai-sdk/provider"
	"github.com/grafana/ai-sdk/providers/openai"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/conversation"
)

// toProvider is the conversation's messages as the SDK's provider messages for the target
// provider, each block as the harness's request carries it: Anthropic thinking with its signature
// and redacted thinking as they came; an OpenAI Responses turn from its native output items, each
// reasoning item with its id and encrypted content byte for byte, without the item ids and
// statuses Oh My Pi's replay drops from messages and calls; tool calls, results and images. An
// error is a conversation the loop cannot send as its harness would.
func toProvider(messages []conversation.Message, target config.AnswerProvider) ([]provider.Message, error) {
	out := make([]provider.Message, 0, len(messages))
	for i, m := range messages {
		var converted *provider.Message
		var err error
		switch m.Role {
		case conversation.User:
			converted, err = userOf(m, target)
		case conversation.Tool:
			converted, err = toolOf(m)
		case conversation.Assistant:
			converted, err = assistantOf(m, target)
		default:
			err = fmt.Errorf("a %q message", m.Role)
		}
		if err != nil {
			return nil, fmt.Errorf("message %d: %w", i+1, err)
		}
		if converted != nil {
			out = append(out, *converted)
		}
	}
	return out, nil
}

// userOf is a user message's text and images. A provider-side compaction's replay for the target
// provider is refused: the harness would send the provider's own compaction state, which the loop
// does not.
func userOf(m conversation.Message, target config.AnswerProvider) (*provider.Message, error) {
	if m.Native != nil && m.Native.Provider == string(target) {
		return nil, fmt.Errorf("a %s compaction's provider-native replay", m.Native.Kind)
	}
	var parts []provider.ContentPart
	for _, b := range m.Content {
		switch b.Type {
		case conversation.Text:
			parts = append(parts, provider.TextPart(b.Text))
		case conversation.Image:
			parts = append(parts, provider.FilePart(b.MediaType, provider.Base64DataContent(b.ImageData)))
		default:
			return nil, fmt.Errorf("a %s block in a user message", b.Type)
		}
	}
	if len(parts) == 0 {
		return nil, nil
	}
	message := provider.NewUserMessage(parts...)
	return &message, nil
}

// toolOf is a tool result message: its text joined by newlines, or its text and images.
func toolOf(m conversation.Message) (*provider.Message, error) {
	var parts []provider.ContentPart
	for _, b := range m.Content {
		if b.Type != conversation.ToolResult {
			return nil, fmt.Errorf("a %s block in a tool message", b.Type)
		}
		output, err := toolOutput(b)
		if err != nil {
			return nil, err
		}
		parts = append(parts, provider.ToolResultPart(b.ToolCallID, b.ToolName, output))
	}
	message := provider.NewToolMessage(parts...)
	return &message, nil
}

func toolOutput(b conversation.Block) (*provider.ToolResultOutput, error) {
	var texts []string
	images := false
	for _, part := range b.Content {
		switch part.Type {
		case conversation.Text:
			texts = append(texts, part.Text)
		case conversation.Image:
			images = true
		default:
			return nil, fmt.Errorf("a %s block in a tool result", part.Type)
		}
	}
	if !images {
		kind := provider.ToolOutputText
		if b.IsError {
			kind = provider.ToolOutputErrorText
		}
		return &provider.ToolResultOutput{Type: kind, Text: strings.Join(texts, "\n")}, nil
	}
	output := &provider.ToolResultOutput{Type: provider.ToolOutputContent}
	for _, part := range b.Content {
		if part.Type == conversation.Text {
			output.Content = append(output.Content, provider.ToolResultContentValue{Type: provider.ToolContentText, Text: part.Text})
			continue
		}
		data := provider.Base64DataContent(part.ImageData)
		output.Content = append(output.Content, provider.ToolResultContentValue{Type: provider.ToolContentFile, Data: &data, MediaType: part.MediaType})
	}
	return output, nil
}

// assistantOf is a model turn: on OpenAI Responses from its native items where it carries them,
// otherwise from its blocks.
func assistantOf(m conversation.Message, target config.AnswerProvider) (*provider.Message, error) {
	var parts []provider.ContentPart
	var err error
	switch {
	case m.Native != nil && target == config.AnswerOpenAI:
		parts, err = nativeParts(m)
	case target == config.AnswerOpenAI:
		parts, err = openaiParts(m.Content)
	default:
		parts, err = anthropicParts(m.Content)
	}
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return nil, nil
	}
	message := provider.NewAssistantMessage(parts...)
	return &message, nil
}

// anthropicParts is an Anthropic turn's blocks: signed thinking with its signature, unsigned
// thinking as the reader kept it (an endpoint that replays it), redacted thinking's data, text and
// tool calls. The SDK's Anthropic converter reads a signature and redacted data only from a raw
// "anthropic" provider option.
func anthropicParts(blocks []conversation.Block) ([]provider.ContentPart, error) {
	var parts []provider.ContentPart
	for _, b := range blocks {
		switch b.Type {
		case conversation.Thinking:
			part := provider.ReasoningPart(b.Text)
			if b.Signature != "" {
				part.ProviderOptions = anthropicRaw("signature", b.Signature)
			}
			parts = append(parts, part)
		case conversation.RedactedThinking:
			part := provider.ReasoningPart("")
			part.ProviderOptions = anthropicRaw("redactedData", b.Data)
			parts = append(parts, part)
		case conversation.Text:
			parts = append(parts, provider.TextPart(b.Text))
		case conversation.ToolCall:
			parts = append(parts, provider.ToolCallPart(b.ToolCallID, b.ToolName, b.Arguments))
		default:
			return nil, fmt.Errorf("a %s block in a model turn", b.Type)
		}
	}
	return parts, nil
}

func anthropicRaw(key, value string) provider.ProviderOptions {
	raw, _ := json.Marshal(map[string]string{key: value})
	return provider.ProviderOptions{"anthropic": provider.RawProviderOption{Key: "anthropic", Raw: raw}}
}

// openaiParts is an OpenAI Responses turn that carries no native items: text and tool calls, and
// a reasoning item where its thinking's signature holds one (Oh My Pi's record of the item before
// it kept native history). Thinking with no item cannot go back to the API and is left out, as
// the harness leaves it out.
func openaiParts(blocks []conversation.Block) ([]provider.ContentPart, error) {
	var parts []provider.ContentPart
	for _, b := range blocks {
		switch b.Type {
		case conversation.Thinking:
			if part, ok := signedReasoning(b.Signature); ok {
				parts = append(parts, part)
			}
		case conversation.Text:
			parts = append(parts, provider.TextPart(b.Text))
		case conversation.ToolCall:
			parts = append(parts, provider.ToolCallPart(b.ToolCallID, b.ToolName, b.Arguments))
		case conversation.RedactedThinking:
		default:
			return nil, fmt.Errorf("a %s block in a model turn", b.Type)
		}
	}
	return parts, nil
}

// signedReasoning is the reasoning item a thinking signature holds, as a reasoning part.
func signedReasoning(signature string) (provider.ContentPart, bool) {
	if signature == "" {
		return provider.ContentPart{}, false
	}
	var item nativeItem
	if json.Unmarshal([]byte(signature), &item) != nil || item.Type != "reasoning" || item.ID == "" || item.EncryptedContent == nil {
		return provider.ContentPart{}, false
	}
	return item.reasoning(), true
}

// nativeItem is the part of an OpenAI Responses output item the loop reads.
type nativeItem struct {
	Type             string  `json:"type"`
	ID               string  `json:"id"`
	Role             string  `json:"role"`
	Phase            string  `json:"phase"`
	EncryptedContent *string `json:"encrypted_content"`
	Summary          []struct {
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

// reasoning is a reasoning item as a part the SDK sends back as the item: its id and its
// encrypted content as they came, its summaries joined.
func (item nativeItem) reasoning() provider.ContentPart {
	summaries := make([]string, len(item.Summary))
	for i, s := range item.Summary {
		summaries[i] = s.Text
	}
	part := provider.ReasoningPart(strings.Join(summaries, "\n\n"))
	part.ProviderOptions = provider.ProviderOptions{"openai": openai.OpenAIPartOptions{ItemID: item.ID, ReasoningEncryptedContent: item.EncryptedContent}}
	return part
}

// nativeParts is an OpenAI Responses turn from its output items, as Oh My Pi's replay sends them
// (sanitizeOpenAIResponsesAssistantHistoryItemsForReplay): a reasoning item with its id and
// encrypted content (one with none cannot go back to a stateless call and is left out), an
// assistant message's text and phase, a function call's call id, name and arguments; an item
// reference dropped; a whitespace-only message dropped with the reasoning that only introduced
// it; and a turn of reasoning and empty messages alone sent from its blocks' visible text instead.
// Any other item, a call whose arguments are not JSON, and a history that replaces the turns
// before it (a provider-side compaction) are refused.
func nativeParts(m conversation.Message) ([]provider.ContentPart, error) {
	if m.Native.Kind != conversation.OpenAIResponsesHistory || !m.Native.Incremental {
		return nil, fmt.Errorf("a turn whose %s history replaces the turns before it", m.Native.Kind)
	}
	var parts, pendingReasoning []provider.ContentPart
	removedMessage, anyRemoved, replayable := false, false, false
	for i, raw := range m.Native.Items {
		var item nativeItem
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, fmt.Errorf("output item %d: %w", i+1, err)
		}
		switch item.Type {
		case "reasoning":
			if removedMessage {
				pendingReasoning = nil
			}
			removedMessage = false
			if item.EncryptedContent != nil && item.ID != "" {
				pendingReasoning = append(pendingReasoning, item.reasoning())
			}
		case "message":
			if item.Role != "assistant" {
				return nil, fmt.Errorf("output item %d: a %q message", i+1, item.Role)
			}
			var text strings.Builder
			for _, part := range item.Content {
				text.WriteString(part.Text)
				text.WriteString(part.Refusal)
			}
			if strings.TrimFunc(text.String(), unicode.IsSpace) == "" {
				removedMessage, anyRemoved = true, true
				continue
			}
			part := provider.TextPart(text.String())
			if item.Phase != "" {
				part.ProviderOptions = provider.ProviderOptions{"openai": openai.OpenAIPartOptions{Phase: item.Phase}}
			}
			parts = append(append(parts, pendingReasoning...), part)
			pendingReasoning, removedMessage, replayable = nil, false, true
		case "function_call":
			if !json.Valid([]byte(item.Arguments)) {
				return nil, fmt.Errorf("output item %d: function call %s's arguments are not JSON", i+1, item.CallID)
			}
			parts = append(append(parts, pendingReasoning...), provider.ToolCallPart(item.CallID, item.Name, json.RawMessage(item.Arguments)))
			pendingReasoning, removedMessage, replayable = nil, false, true
		case "item_reference":
		default:
			return nil, fmt.Errorf("output item %d: an item of type %q", i+1, item.Type)
		}
	}
	if !replayable {
		var visible []provider.ContentPart
		for _, b := range m.Content {
			if b.Type == conversation.Text && strings.TrimFunc(b.Text, unicode.IsSpace) != "" {
				visible = append(visible, provider.TextPart(b.Text))
			}
		}
		return visible, nil
	}
	if anyRemoved {
		// Oh My Pi's replay keeps reasoning only ahead of the output it introduced once it has
		// dropped an empty message (dropEmptyAssistantMessagesForReplay).
		return parts, nil
	}
	return append(parts, pendingReasoning...), nil
}

// markAnthropicHistory puts Anthropic's ephemeral cache mark on the history's last message, so
// the next model call, and the next question within the cache's life, reads the conversation
// from the cache.
func markAnthropicHistory(messages []provider.Message, history int) {
	if history > 0 {
		messages[history-1].ProviderOptions = withCacheMark(messages[history-1].ProviderOptions)
	}
}

// markOpenAIHistory puts an explicit prompt cache breakpoint at the end of the history, on the
// last part of the last history message that can carry one (a user message's text or file, a
// tool result): GPT-6's implicit breakpoint sits after the newest message only, so without it a
// later question would not read the conversation from the cache.
func markOpenAIHistory(messages []provider.Message, history int) {
	breakpoint := provider.ProviderOptions{"openai": openai.OpenAIPartOptions{PromptCacheBreakpoint: &openai.PromptCacheBreakpoint{Mode: "explicit"}}}
	for i := history - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role != provider.RoleUser && m.Role != provider.RoleTool || len(m.Content) == 0 {
			continue
		}
		last := len(m.Content) - 1
		switch m.Content[last].Type {
		case provider.ContentPartTypeText, provider.ContentPartTypeFile, provider.ContentPartTypeToolResult:
			content := append([]provider.ContentPart(nil), m.Content...)
			content[last].ProviderOptions = breakpoint
			messages[i].Content = content
			return
		}
	}
}
