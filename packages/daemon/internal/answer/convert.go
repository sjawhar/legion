package answer

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/sjawhar/envoy/agentloop"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/conversation"
)

// toAgentloop is the conversation's messages as agentloop's, each block as the harness's own
// request carries it: Anthropic thinking with its signature and redacted thinking as they came;
// an OpenAI Responses turn from its native output items, each reasoning item with its id and
// encrypted content byte for byte, without the item ids and statuses Oh My Pi's replay drops from
// messages and calls; tool calls, results and images. A message that holds nothing the protocol
// takes is left out, as agentloop leaves it out. An error is a conversation the loop cannot send
// as its harness would, and the question is answered from none of it (UnknownFormat).
func toAgentloop(messages []conversation.Message, r route) ([]agentloop.Message, error) {
	out := make([]agentloop.Message, 0, len(messages))
	for i, m := range messages {
		var role agentloop.Role
		var blocks []agentloop.Block
		var err error
		switch m.Role {
		case conversation.User:
			role = agentloop.User
			blocks, err = userBlocks(m, r.provider)
		case conversation.Tool:
			role = agentloop.ToolRole
			blocks, err = toolBlocks(m)
		case conversation.Assistant:
			role = agentloop.Assistant
			blocks, err = assistantBlocks(m, r.kind)
		default:
			err = fmt.Errorf("a %q message", m.Role)
		}
		if err != nil {
			return nil, fmt.Errorf("message %d: %w", i+1, err)
		}
		if len(blocks) == 0 {
			continue
		}
		out = append(out, agentloop.Message{Role: role, Blocks: blocks})
	}
	return out, nil
}

// userBlocks is a user message's text and images. A provider-side compaction's replay for the
// target provider is refused: the harness would send the provider's own compaction state, which
// the loop does not.
func userBlocks(m conversation.Message, target config.AnswerProvider) ([]agentloop.Block, error) {
	if m.Native != nil && m.Native.Provider == string(target) {
		return nil, fmt.Errorf("a %s compaction's provider-native replay", m.Native.Kind)
	}
	var blocks []agentloop.Block
	for _, b := range m.Content {
		switch b.Type {
		case conversation.Text:
			blocks = append(blocks, agentloop.Block{Kind: agentloop.TextBlock, Text: b.Text})
		case conversation.Image:
			blocks = append(blocks, agentloop.Block{Kind: agentloop.ImageBlock, MediaType: b.MediaType, Data: b.ImageData})
		default:
			return nil, fmt.Errorf("a %s block in a user message", b.Type)
		}
	}
	return blocks, nil
}

// toolBlocks is a tool result message: each result with its parts, text and images, as the
// harness sent them back.
func toolBlocks(m conversation.Message) ([]agentloop.Block, error) {
	var blocks []agentloop.Block
	for _, b := range m.Content {
		if b.Type != conversation.ToolResult {
			return nil, fmt.Errorf("a %s block in a tool message", b.Type)
		}
		result := agentloop.Block{Kind: agentloop.ToolResultBlock, ToolCallID: b.ToolCallID, Name: b.ToolName, IsError: b.IsError}
		for _, part := range b.Content {
			switch part.Type {
			case conversation.Text:
				result.Content = append(result.Content, agentloop.Block{Kind: agentloop.TextBlock, Text: part.Text})
			case conversation.Image:
				result.Content = append(result.Content, agentloop.Block{Kind: agentloop.ImageBlock, MediaType: part.MediaType, Data: part.ImageData})
			default:
				return nil, fmt.Errorf("a %s block in a tool result", part.Type)
			}
		}
		blocks = append(blocks, result)
	}
	return blocks, nil
}

// assistantBlocks is a model turn: on OpenAI Responses from its native items where it carries
// them, otherwise from its blocks.
func assistantBlocks(m conversation.Message, kind agentloop.Kind) ([]agentloop.Block, error) {
	switch {
	case m.Native != nil && kind == agentloop.OpenAIResponses:
		return nativeBlocks(m)
	case kind == agentloop.OpenAIResponses:
		return openaiBlocks(m.Content)
	default:
		return anthropicBlocks(m.Content)
	}
}

// anthropicBlocks is an Anthropic turn's blocks: signed thinking with its signature, unsigned
// thinking as the reader kept it (an endpoint that replays it), redacted thinking's data, text
// and tool calls.
func anthropicBlocks(blocks []conversation.Block) ([]agentloop.Block, error) {
	var out []agentloop.Block
	for _, b := range blocks {
		switch b.Type {
		case conversation.Thinking:
			out = append(out, agentloop.Block{Kind: agentloop.ThinkingBlock, Text: b.Text, Signature: b.Signature})
		case conversation.RedactedThinking:
			out = append(out, agentloop.Block{Kind: agentloop.ThinkingBlock, Redacted: b.Data})
		case conversation.Text:
			out = append(out, agentloop.Block{Kind: agentloop.TextBlock, Text: b.Text})
		case conversation.ToolCall:
			out = append(out, agentloop.Block{Kind: agentloop.ToolCallBlock, ToolCallID: b.ToolCallID, Name: b.ToolName, Input: b.Arguments})
		default:
			return nil, fmt.Errorf("a %s block in a model turn", b.Type)
		}
	}
	return out, nil
}

// openaiBlocks is an OpenAI Responses turn that carries no native items: text and tool calls, and
// a reasoning item where its thinking's signature holds one (Oh My Pi's record of the item before
// it kept native history). Thinking with no item cannot go back to the API and is left out, as
// the harness leaves it out.
func openaiBlocks(blocks []conversation.Block) ([]agentloop.Block, error) {
	var out []agentloop.Block
	for _, b := range blocks {
		switch b.Type {
		case conversation.Thinking:
			if block, ok := signedReasoning(b.Signature); ok {
				out = append(out, block)
			}
		case conversation.Text:
			out = append(out, agentloop.Block{Kind: agentloop.TextBlock, Text: b.Text})
		case conversation.ToolCall:
			out = append(out, agentloop.Block{Kind: agentloop.ToolCallBlock, ToolCallID: b.ToolCallID, Name: b.ToolName, Input: b.Arguments})
		case conversation.RedactedThinking:
		default:
			return nil, fmt.Errorf("a %s block in a model turn", b.Type)
		}
	}
	return out, nil
}

// signedReasoning is the reasoning item a thinking signature holds, as a reasoning block.
func signedReasoning(signature string) (agentloop.Block, bool) {
	if signature == "" {
		return agentloop.Block{}, false
	}
	var item nativeItem
	if json.Unmarshal([]byte(signature), &item) != nil || item.Type != "reasoning" || item.ID == "" || item.EncryptedContent == nil {
		return agentloop.Block{}, false
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

// reasoning is a reasoning item as a block agentloop sends back as the item: its id and its
// encrypted content as they came, its summaries joined.
func (item nativeItem) reasoning() agentloop.Block {
	summaries := make([]string, len(item.Summary))
	for i, s := range item.Summary {
		summaries[i] = s.Text
	}
	block := agentloop.Block{Kind: agentloop.ReasoningBlock, Text: strings.Join(summaries, "\n\n"), ItemID: item.ID}
	if item.EncryptedContent != nil {
		block.Encrypted = *item.EncryptedContent
	}
	return block
}

// nativeBlocks is an OpenAI Responses turn from its output items, as Oh My Pi's replay sends them
// (sanitizeOpenAIResponsesAssistantHistoryItemsForReplay): a reasoning item with its id and
// encrypted content (one with none cannot go back to a stateless call and is left out), an
// assistant message's text and phase, a function call's call id, name and arguments; an item
// reference dropped; a whitespace-only message dropped with the reasoning that only introduced
// it; and a turn of reasoning and empty messages alone sent from its blocks' visible text instead.
// Any other item, a call whose arguments are not JSON, and a history that replaces the turns
// before it (a provider-side compaction) are refused.
func nativeBlocks(m conversation.Message) ([]agentloop.Block, error) {
	if m.Native.Kind != conversation.OpenAIResponsesHistory || !m.Native.Incremental {
		return nil, fmt.Errorf("a turn whose %s history replaces the turns before it", m.Native.Kind)
	}
	var blocks, pendingReasoning []agentloop.Block
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
			blocks = append(append(blocks, pendingReasoning...), agentloop.Block{Kind: agentloop.TextBlock, Text: text.String(), Phase: item.Phase})
			pendingReasoning, removedMessage, replayable = nil, false, true
		case "function_call":
			if !json.Valid([]byte(item.Arguments)) {
				return nil, fmt.Errorf("output item %d: function call %s's arguments are not JSON", i+1, item.CallID)
			}
			blocks = append(append(blocks, pendingReasoning...), agentloop.Block{
				Kind: agentloop.ToolCallBlock, ToolCallID: item.CallID, Name: item.Name, Input: json.RawMessage(item.Arguments),
			})
			pendingReasoning, removedMessage, replayable = nil, false, true
		case "item_reference":
		default:
			return nil, fmt.Errorf("output item %d: an item of type %q", i+1, item.Type)
		}
	}
	if !replayable {
		var visible []agentloop.Block
		for _, b := range m.Content {
			if b.Type == conversation.Text && strings.TrimFunc(b.Text, unicode.IsSpace) != "" {
				visible = append(visible, agentloop.Block{Kind: agentloop.TextBlock, Text: b.Text})
			}
		}
		return visible, nil
	}
	if anyRemoved {
		// Oh My Pi's replay keeps reasoning only ahead of the output it introduced once it has
		// dropped an empty message (dropEmptyAssistantMessagesForReplay).
		return blocks, nil
	}
	return append(blocks, pendingReasoning...), nil
}
