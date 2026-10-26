package claude

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/conversation"
)

const (
	// interruptedResult is the result Claude Code records, when a session resumes, for each tool
	// call its last turn made and never recorded a result for.
	interruptedResult = "[Tool call interrupted: the session ended before this call's result was recorded, so its outcome is unknown. Check whether it took effect before relying on it or running it again.]"
	// noResponse is the text of the assistant message Claude Code records after a turn the model
	// never answered, with the model syntheticModel.
	noResponse     = "No response requested."
	syntheticModel = "<synthetic>"
)

// item is one message of the request as it is assembled from the chain.
type item struct {
	role   string
	blocks []block
	// assistant: the API's message id the lines of one message share, its model, and whether it
	// is Claude Code's record of an API error.
	id       string
	model    string
	apiError bool
	// quiet marks a user message no answer was asked for: Claude Code's own notes, a local
	// command and its output.
	quiet bool
}

// localCommandTags open the text of the user lines a local command (`/rename`, `/compact`)
// records: the command and its output.
var localCommandTags = []string{"<command-name>", "<local-command-stdout>", "<local-command-stderr>", "<local-command-caveat>"}

// buildRequest turns the chain into the messages Claude Code sends: user, assistant and local
// command lines as messages (attachments are context Claude Code renders beside the conversation,
// kept out but for a prompt queued during a turn); a resumed turn's dangling tool calls answered
// as interrupted and a turn the model never answered closed with Claude Code's synthetic reply, as
// it records them on resume; then API error records dropped, the lines of one assistant message
// merged, a message whose tool calls the next prompt abandoned dropped, and consecutive user
// messages merged into one.
func buildRequest(chain []*entry) ([]conversation.Message, error) {
	var items []item
	for _, e := range chain {
		it, ok, err := itemOf(e)
		if err != nil {
			return nil, err
		}
		if ok {
			items = append(items, it)
		}
	}
	messages, err := normalize(closeTail(items))
	if err != nil {
		return nil, err
	}
	return toConversation(messages), nil
}

// itemOf is the request message a chain line becomes, false for one that becomes none.
func itemOf(e *entry) (item, bool, error) {
	switch e.Type {
	case "user":
		if e.Message == nil {
			return item{}, false, conversation.Unknown("user line %s holds no message", e.UUID)
		}
		blocks, err := userBlocks(e.Message.Content)
		if err != nil {
			return item{}, false, err
		}
		quiet := e.IsMeta
		if len(blocks) > 0 && blocks[0].Type == "text" {
			for _, tag := range localCommandTags {
				quiet = quiet || strings.HasPrefix(blocks[0].Text, tag)
			}
		}
		return item{role: "user", blocks: blocks, quiet: quiet}, true, nil
	case "assistant":
		if e.Message == nil {
			return item{}, false, conversation.Unknown("assistant line %s holds no message", e.UUID)
		}
		blocks, err := assistantBlocks(e.Message.Content)
		if err != nil {
			return item{}, false, err
		}
		return item{role: "assistant", blocks: blocks, id: e.Message.ID, model: e.Message.Model, apiError: e.IsAPIErrorMessage}, true, nil
	case "system":
		switch {
		case e.Subtype == localCommand:
			var text string
			if err := json.Unmarshal(e.Content, &text); err != nil {
				return item{}, false, conversation.Unknown("a local command line whose content is not text")
			}
			return item{role: "user", blocks: []block{{Type: "text", Text: text}}, quiet: true}, true, nil
		case e.Subtype == compactBoundary || noticeSubtypes[e.Subtype]:
			return item{}, false, nil
		}
		return item{}, false, conversation.Unknown("a system line of subtype %q", e.Subtype)
	case "attachment":
		if e.Attachment == nil || e.Attachment.Type != queuedCommand {
			return item{}, false, nil
		}
		blocks, err := userBlocks(e.Attachment.Prompt)
		if err != nil {
			return item{}, false, err
		}
		return item{role: "user", blocks: blocks}, true, nil
	}
	return item{}, false, nil
}

// userBlocks reads a user message's content: text, or text, image and tool_result blocks.
func userBlocks(raw json.RawMessage) ([]block, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []block{{Type: "text", Text: text}}, nil
	}
	var blocks []block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, conversation.Unknown("a user message's content is neither text nor blocks")
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
		case "image":
			if err := checkImage(b); err != nil {
				return nil, err
			}
		case "tool_result":
			if _, err := resultParts(b.Content); err != nil {
				return nil, err
			}
		default:
			return nil, conversation.Unknown("a user content block of type %q", b.Type)
		}
	}
	return blocks, nil
}

func checkImage(b block) error {
	if b.Source == nil || b.Source.Type != "base64" {
		return conversation.Unknown("an image that is not base64 data")
	}
	return nil
}

// resultParts reads a tool result's content: text, or text and image blocks.
func resultParts(raw json.RawMessage) ([]block, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []block{{Type: "text", Text: text}}, nil
	}
	var parts []block
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, conversation.Unknown("a tool result's content is neither text nor blocks")
	}
	for _, p := range parts {
		switch p.Type {
		case "text":
		case "image":
			if err := checkImage(p); err != nil {
				return nil, err
			}
		default:
			return nil, conversation.Unknown("a tool result block of type %q", p.Type)
		}
	}
	return parts, nil
}

// assistantBlocks reads an assistant line's content: text, thinking, redacted_thinking and
// tool_use blocks.
func assistantBlocks(raw json.RawMessage) ([]block, error) {
	var blocks []block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, conversation.Unknown("an assistant message's content is not blocks")
	}
	for _, b := range blocks {
		switch b.Type {
		case "text", "thinking", "redacted_thinking", "tool_use":
		default:
			return nil, conversation.Unknown("an assistant content block of type %q", b.Type)
		}
	}
	return blocks, nil
}

func hasToolResult(e *entry) bool {
	if e.Message == nil {
		return false
	}
	var blocks []block
	if json.Unmarshal(e.Message.Content, &blocks) != nil {
		return false
	}
	return len(toolResultIDs(blocks)) > 0
}

func toolUseIDs(blocks []block) []string {
	var ids []string
	for _, b := range blocks {
		if b.Type == "tool_use" {
			ids = append(ids, b.ID)
		}
	}
	return ids
}

func toolResultIDs(blocks []block) []string {
	var ids []string
	for _, b := range blocks {
		if b.Type == "tool_result" {
			ids = append(ids, b.ToolUseID)
		}
	}
	return ids
}

// closeTail is what Claude Code records when it resumes a session whose last turn did not end: a
// last assistant message with tool calls no result answers gets an interrupted result for each,
// then the synthetic reply; a last user message that asked for an answer gets the synthetic reply.
// A last assistant message that is an API error record, or a last user message no answer was asked
// for, is left as it is.
func closeTail(items []item) []item {
	if len(items) == 0 {
		return items
	}
	last := items[len(items)-1]
	synthetic := item{role: "assistant", blocks: []block{{Type: "text", Text: noResponse}}, model: syntheticModel}
	switch {
	case last.role == "user" && !last.quiet:
		return append(items, synthetic)
	case last.role == "assistant" && !last.apiError:
		answered := map[string]bool{}
		for _, it := range items {
			for _, id := range toolResultIDs(it.blocks) {
				answered[id] = true
			}
		}
		start := len(items) - 1
		for start > 0 && items[start-1].role == "assistant" && items[start-1].id == last.id {
			start--
		}
		var missing []block
		for _, it := range items[start:] {
			for _, id := range toolUseIDs(it.blocks) {
				if !answered[id] {
					encoded, _ := json.Marshal(interruptedResult)
					missing = append(missing, block{Type: "tool_result", ToolUseID: id, Content: encoded, IsError: true})
				}
			}
		}
		if len(missing) == 0 {
			return items
		}
		return append(items, item{role: "user", blocks: missing}, synthetic)
	}
	return items
}

// normalize shapes the items as Claude Code shapes the messages it sends.
func normalize(items []item) ([]item, error) {
	var merged []item
	for _, it := range items {
		if it.role == "assistant" {
			if it.apiError {
				continue
			}
			if i := sameMessage(merged, it.id); i >= 0 {
				merged[i].blocks = append(merged[i].blocks, it.blocks...)
				continue
			}
		}
		merged = append(merged, it)
	}
	merged = mergeUsers(merged)
	var kept []item
	for i, it := range merged {
		if results := toolResultIDs(it.blocks); it.role == "user" && len(results) > 0 {
			if len(kept) == 0 || kept[len(kept)-1].role != "assistant" || len(toolUseIDs(kept[len(kept)-1].blocks)) == 0 {
				return nil, conversation.Unknown("tool results %v no tool call precedes", results)
			}
		}
		if calls := toolUseIDs(it.blocks); it.role == "assistant" && len(calls) > 0 {
			if i+1 >= len(merged) || merged[i+1].role != "user" {
				return nil, conversation.Unknown("tool calls %v no user message follows", calls)
			}
			answers := toolResultIDs(merged[i+1].blocks)
			if len(answers) == 0 {
				// The next prompt abandoned the calls: Claude Code drops the whole message.
				continue
			}
			if !sameSet(calls, answers) {
				return nil, conversation.Unknown("tool results %v that do not answer tool calls %v", answers, calls)
			}
		}
		kept = append(kept, it)
	}
	return mergeUsers(kept), nil
}

// mergeUsers merges each run of consecutive user messages into one (joinAtSeam).
func mergeUsers(items []item) []item {
	var out []item
	for _, it := range items {
		if it.role == "user" && len(out) > 0 && out[len(out)-1].role == "user" {
			prev := &out[len(out)-1]
			prev.blocks = joinAtSeam(prev.blocks, it.blocks)
			continue
		}
		out = append(out, it)
	}
	return out
}

// sameMessage is the index of the assistant message id names that the new line continues: the
// last assistant message, past user messages that hold only tool results, when it is id's; -1
// otherwise.
func sameMessage(merged []item, id string) int {
	if id == "" {
		return -1
	}
	for i := len(merged) - 1; i >= 0; i-- {
		switch m := merged[i]; {
		case m.role == "assistant":
			if m.id == id {
				return i
			}
			return -1
		case len(toolResultIDs(m.blocks)) != len(m.blocks):
			return -1
		}
	}
	return -1
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := map[string]bool{}
	for _, s := range a {
		set[s] = true
	}
	for _, s := range b {
		if !set[s] {
			return false
		}
	}
	return true
}

// joinAtSeam merges two user messages' blocks as Claude Code does: where a text block meets a text
// block, the first ends in a newline; then every tool result moves to the front, in order.
func joinAtSeam(a, b []block) []block {
	joined := slices.Clone(a)
	if len(joined) > 0 && len(b) > 0 && joined[len(joined)-1].Type == "text" && b[0].Type == "text" {
		joined[len(joined)-1].Text += "\n"
	}
	joined = append(joined, b...)
	var results, rest []block
	for _, blk := range joined {
		if blk.Type == "tool_result" {
			results = append(results, blk)
		} else {
			rest = append(rest, blk)
		}
	}
	return append(results, rest...)
}

// toConversation is the request's messages in the neutral model: each tool result of a user
// message a Tool message of its own, its other blocks a User message after them.
func toConversation(items []item) []conversation.Message {
	names := map[string]string{}
	for _, it := range items {
		for _, b := range it.blocks {
			if b.Type == "tool_use" {
				names[b.ID] = b.Name
			}
		}
	}
	var messages []conversation.Message
	for _, it := range items {
		if it.role == "assistant" {
			m := conversation.Message{Role: conversation.Assistant, Provider: "anthropic", API: anthropicMessages, Model: it.model}
			for _, b := range it.blocks {
				switch b.Type {
				case "text":
					m.Content = append(m.Content, conversation.Block{Type: conversation.Text, Text: b.Text})
				case "thinking":
					m.Content = append(m.Content, conversation.Block{Type: conversation.Thinking, Text: b.Thinking, Signature: b.Signature})
				case "redacted_thinking":
					m.Content = append(m.Content, conversation.Block{Type: conversation.RedactedThinking, Data: b.Data})
				case "tool_use":
					m.Content = append(m.Content, conversation.Block{Type: conversation.ToolCall, ToolCallID: b.ID, ToolName: b.Name, Arguments: b.Input})
				}
			}
			messages = append(messages, m)
			continue
		}
		user := conversation.Message{Role: conversation.User}
		for _, b := range it.blocks {
			switch b.Type {
			case "tool_result":
				parts, _ := resultParts(b.Content)
				result := conversation.Block{Type: conversation.ToolResult, ToolCallID: b.ToolUseID, ToolName: names[b.ToolUseID], IsError: b.IsError}
				for _, p := range parts {
					result.Content = append(result.Content, neutralPart(p))
				}
				messages = append(messages, conversation.Message{Role: conversation.Tool, Content: []conversation.Block{result}})
			default:
				user.Content = append(user.Content, neutralPart(b))
			}
		}
		if len(user.Content) > 0 {
			messages = append(messages, user)
		}
	}
	return messages
}

// neutralPart is a text or image block in the neutral model.
func neutralPart(b block) conversation.Block {
	if b.Type == "image" {
		return conversation.Block{Type: conversation.Image, MediaType: b.Source.MediaType, ImageData: b.Source.Data}
	}
	return conversation.Block{Type: conversation.Text, Text: b.Text}
}
