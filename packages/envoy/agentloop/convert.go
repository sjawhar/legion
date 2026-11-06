package agentloop

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/grafana/ai-sdk/provider"
	"github.com/grafana/ai-sdk/providers/anthropic"
	"github.com/grafana/ai-sdk/providers/openai"
)

// toProvider is a run's messages as the SDK's provider messages for the endpoint's protocol,
// each block as the caller handed it over: Anthropic thinking with its signature and its
// redacted payload as they came, an OpenAI reasoning item with its id and encrypted content byte
// for byte, text, tool calls, tool results and images. A message that converts to no part at all
// is left out, as is a block the protocol cannot be given back. An error names the message by
// its one-based position and what it could not convert, and is a run the caller cannot make.
func toProvider(messages []Message, kind Kind) ([]provider.Message, error) {
	out := make([]provider.Message, 0, len(messages))
	for i, m := range messages {
		var parts []provider.ContentPart
		var err error
		var build func(...provider.ContentPart) provider.Message
		switch m.Role {
		case User:
			parts, err = userParts(m.Blocks)
			build = provider.NewUserMessage
		case ToolRole:
			parts, err = toolParts(m.Blocks)
			build = provider.NewToolMessage
		case Assistant:
			parts, err = assistantParts(m.Blocks, kind)
			build = provider.NewAssistantMessage
		default:
			err = fmt.Errorf("a %q message", m.Role)
		}
		if err != nil {
			return nil, fmt.Errorf("message %d: %w", i+1, err)
		}
		if len(parts) == 0 {
			continue
		}
		out = append(out, build(parts...))
	}
	return out, nil
}

// userParts is a user turn's blocks: its text and its images. Every protocol takes them the same
// way.
func userParts(blocks []Block) ([]provider.ContentPart, error) {
	parts := make([]provider.ContentPart, 0, len(blocks))
	for _, b := range blocks {
		switch b.Kind {
		case TextBlock:
			parts = append(parts, provider.TextPart(b.Text))
		case ImageBlock:
			parts = append(parts, provider.FilePart(b.MediaType, provider.Base64DataContent(b.Data)))
		default:
			return nil, fmt.Errorf("a %s block in a user message", b.Kind)
		}
	}
	return parts, nil
}

// toolParts is a turn of tool results, one part per result. Every protocol takes them the same
// way, and a block of any other kind is refused.
func toolParts(blocks []Block) ([]provider.ContentPart, error) {
	parts := make([]provider.ContentPart, 0, len(blocks))
	for _, b := range blocks {
		if b.Kind != ToolResultBlock {
			return nil, fmt.Errorf("a %s block in a tool message", b.Kind)
		}
		output, err := toolOutput(b)
		if err != nil {
			return nil, err
		}
		parts = append(parts, provider.ToolResultPart(b.ToolCallID, b.Name, output))
	}
	return parts, nil
}

// toolOutput is one tool result's output: its text parts joined by newlines, or, where it holds
// an image, its text and images as content values. A result the tool refused goes back as error
// text. A result that holds no Content carries its output in Text instead, so a caller may fill
// either.
func toolOutput(b Block) (*provider.ToolResultOutput, error) {
	content := b.Content
	if len(content) == 0 && b.Text != "" {
		content = []Block{{Kind: TextBlock, Text: b.Text}}
	}
	var texts []string
	images := false
	for _, part := range content {
		switch part.Kind {
		case TextBlock:
			texts = append(texts, part.Text)
		case ImageBlock:
			images = true
		default:
			return nil, fmt.Errorf("a %s block in a tool result", part.Kind)
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
	for _, part := range content {
		if part.Kind == TextBlock {
			output.Content = append(output.Content, provider.ToolResultContentValue{Type: provider.ToolContentText, Text: part.Text})
			continue
		}
		data := provider.Base64DataContent(part.Data)
		output.Content = append(output.Content, provider.ToolResultContentValue{Type: provider.ToolContentFile, Data: &data, MediaType: part.MediaType})
	}
	return output, nil
}

// assistantParts is a model turn's blocks for the protocol the turn goes back to, which is the
// only part of the conversion the protocol decides.
func assistantParts(blocks []Block, kind Kind) ([]provider.ContentPart, error) {
	switch {
	case anthropicKind(kind):
		return anthropicParts(blocks)
	case kind == OpenAIResponses:
		return openaiResponsesParts(blocks)
	case kind == OpenAIChat:
		return openaiChatParts(blocks)
	default:
		return nil, fmt.Errorf("a model turn for the %s protocol", kind)
	}
}

// anthropicParts is an Anthropic turn's blocks: signed thinking with its signature, unsigned
// thinking as the caller kept it, redacted thinking's payload, text and tool calls. An OpenAI
// reasoning item cannot go to Anthropic and is left out. The SDK's Anthropic converter reads a
// signature and redacted data only from a raw "anthropic" provider option.
func anthropicParts(blocks []Block) ([]provider.ContentPart, error) {
	parts := make([]provider.ContentPart, 0, len(blocks))
	for _, b := range blocks {
		switch b.Kind {
		case ThinkingBlock:
			var part provider.ContentPart
			switch {
			case b.Redacted != "":
				part = provider.ReasoningPart("")
				part.ProviderOptions = anthropicRaw("redactedData", b.Redacted)
			default:
				part = provider.ReasoningPart(b.Text)
				if b.Signature != "" {
					part.ProviderOptions = anthropicRaw("signature", b.Signature)
				}
			}
			parts = append(parts, part)
		case TextBlock:
			parts = append(parts, provider.TextPart(b.Text))
		case ToolCallBlock:
			parts = append(parts, provider.ToolCallPart(b.ToolCallID, b.Name, b.Input))
		case ReasoningBlock:
		default:
			return nil, fmt.Errorf("a %s block in a model turn", b.Kind)
		}
	}
	return parts, nil
}

// anthropicRaw is one raw "anthropic" provider option, as the single key and value of its JSON
// object.
func anthropicRaw(key, value string) provider.ProviderOptions {
	raw, _ := json.Marshal(map[string]string{key: value})
	return provider.BuildProviderOptions(provider.RawProviderOption{Key: "anthropic", Raw: raw})
}

// openaiResponsesParts is an OpenAI Responses turn's blocks: a reasoning item with its id and
// encrypted content as they came, text with the phase it was produced in, and tool calls.
// Reasoning that holds neither an item id nor encrypted content cannot go back to a stateless
// call and is left out, as is Anthropic thinking.
func openaiResponsesParts(blocks []Block) ([]provider.ContentPart, error) {
	parts := make([]provider.ContentPart, 0, len(blocks))
	for _, b := range blocks {
		switch b.Kind {
		case ReasoningBlock:
			if b.ItemID == "" || b.Encrypted == "" {
				continue
			}
			encrypted := b.Encrypted
			part := provider.ReasoningPart(b.Text)
			part.ProviderOptions = provider.BuildProviderOptions(openai.OpenAIPartOptions{ItemID: b.ItemID, ReasoningEncryptedContent: &encrypted})
			parts = append(parts, part)
		case TextBlock:
			part := provider.TextPart(b.Text)
			if b.Phase != "" {
				part.ProviderOptions = provider.BuildProviderOptions(openai.OpenAIPartOptions{Phase: b.Phase})
			}
			parts = append(parts, part)
		case ToolCallBlock:
			parts = append(parts, provider.ToolCallPart(b.ToolCallID, b.Name, b.Input))
		case ThinkingBlock:
		default:
			return nil, fmt.Errorf("a %s block in a model turn", b.Kind)
		}
	}
	return parts, nil
}

// openaiChatParts is a Chat Completions turn's blocks: text, which the shape carries no phase
// for, and tool calls. Neither Anthropic thinking nor an OpenAI reasoning item can go back to
// Chat Completions, and both are left out.
func openaiChatParts(blocks []Block) ([]provider.ContentPart, error) {
	parts := make([]provider.ContentPart, 0, len(blocks))
	for _, b := range blocks {
		switch b.Kind {
		case TextBlock:
			parts = append(parts, provider.TextPart(b.Text))
		case ToolCallBlock:
			parts = append(parts, provider.ToolCallPart(b.ToolCallID, b.Name, b.Input))
		case ThinkingBlock, ReasoningBlock:
		default:
			return nil, fmt.Errorf("a %s block in a model turn", b.Kind)
		}
	}
	return parts, nil
}

// withOption is options with option added under the provider key the option itself names, which
// is how the SDK's own provider.BuildProviderOptions keys a value.
func withOption(options provider.ProviderOptions, option provider.ProviderOption) provider.ProviderOptions {
	marked := make(provider.ProviderOptions, len(options)+1)
	maps.Copy(marked, options)
	marked[option.ProviderKey()] = option
	return marked
}

// markAnthropicHistory puts Anthropic's ephemeral cache mark at the end of the run's first
// history messages, so the next model call of the run, and the next run within the cache's
// life, reads the conversation from the cache. The provider gives a message's mark to that
// message's last block alone and drops it on a thinking block, so the mark goes on the last
// message of the history whose last part is not reasoning: a turn the model cut off at its
// limit, which ends in thinking, would otherwise take the mark and lose it. A history every
// message of which ends in reasoning goes unmarked, and where Input is not empty the newest
// message's mark, which the run places itself, is then the call's only one. Where Input is
// empty the newest message is that same history message, whose mark the provider drops as
// well, and the call carries no mark at all. The run places the marks unless the caller turns
// them off with Request.NoCache.
func markAnthropicHistory(messages []provider.Message, history int) {
	for i := history - 1; i >= 0; i-- {
		m := messages[i]
		if m.Content[len(m.Content)-1].Type == provider.ContentPartTypeReasoning {
			continue
		}
		messages[i].ProviderOptions = withOption(m.ProviderOptions, anthropic.CacheControl("ephemeral"))
		return
	}
}

// markOpenAIHistory puts an explicit prompt cache breakpoint at the end of the run's first
// history messages, on the last part of the last of them that can carry one (a user message's
// text or file, a tool result): the implicit breakpoint sits after the newest message only, so
// without it a later run would not read the conversation from the cache. A tool result whose
// output is content, which is how a result holding an image goes over, takes the breakpoint on
// the last of those content values: the provider reads a content output's breakpoint from each
// value's own options and never from the part's. The messages are the ones toProvider built
// for this run, which leaves out a message that converts to no part and gives every tool
// result an output of its own, so the mark goes on the part as it stands. The run places it
// unless the caller turns the marks off with Request.NoCache.
func markOpenAIHistory(messages []provider.Message, history int) {
	breakpoint := openai.OpenAIPartOptions{PromptCacheBreakpoint: &openai.PromptCacheBreakpoint{Mode: "explicit"}}
	for i := history - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role != provider.RoleUser && m.Role != provider.RoleTool {
			continue
		}
		part := &m.Content[len(m.Content)-1]
		switch {
		case part.Type == provider.ContentPartTypeToolResult && part.Output.Type == provider.ToolOutputContent:
			value := &part.Output.Content[len(part.Output.Content)-1]
			value.ProviderOptions = withOption(value.ProviderOptions, breakpoint)
		case part.Type == provider.ContentPartTypeText, part.Type == provider.ContentPartTypeFile,
			part.Type == provider.ContentPartTypeToolResult:
			part.ProviderOptions = withOption(part.ProviderOptions, breakpoint)
		default:
			continue
		}
		return
	}
}
