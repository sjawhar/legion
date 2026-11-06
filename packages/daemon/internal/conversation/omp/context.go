package omp

import (
	"encoding/json"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/conversation"
)

// llmMessage is one message as Oh My Pi's convertToLlm hands it to the provider layer: a user,
// assistant or tool result message. A developer message is a user message here: both provider
// APIs the reader rebuilds for send it as one.
type llmMessage struct {
	role   string // "user", "assistant" or "toolResult"
	blocks []block
	// assistant
	api, provider, model, stopReason string
	payload                          json.RawMessage
	// toolResult
	toolCallID, toolName string
	isError              bool
	// user: a compaction summary's provider-native replay
	native *conversation.Native
}

// Custom message types the pinned Oh My Pi treats apart (session-context.ts, messages.ts,
// pi-tui chat/messages.ts, pi-wire).
const (
	prewalkPlanType         = "prewalk-plan"
	vibeModeContextType     = "vibe-mode-context"
	interruptedThinkingType = "interrupted-thinking"
	skillPromptType         = "skill-prompt"
	collabPromptType        = "collab-prompt"
	defaultCustomType       = "custom"
	toolResultPruneType     = "tool_result_prune"
	contextNotesType        = "experimental_context_notes"
	snapcompactPreserveKey  = "snapcompact"
	anthropicCompactionKey  = "anthropicCompaction"
	openaiRemoteCompaction  = "openaiRemoteCompaction"
	contextRolloverKind     = "experimental-context-rollover"
	handoffCompactionMethod = "handoff"
	outputTruncatedNotice   = "[Output truncated]"
)

// sessionContext is what buildSessionContext reports besides the messages.
type sessionContext struct {
	messages      []llmMessage
	thinkingLevel string
	model         string
}

// buildContext is the pinned Oh My Pi's buildSessionContext for the model context (not the display
// transcript) along the path from the last entry, with each message as convertToLlm turns it into
// provider messages. The tool result prunes are applied first, as the loader does.
func buildContext(entries []*entry) (*sessionContext, error) {
	if err := applyToolResultPrunes(entries); err != nil {
		return nil, err
	}
	ctx := &sessionContext{thinkingLevel: "off"}
	if len(entries) == 0 {
		return ctx, nil
	}
	path := activePath(entries)

	models := map[string]string{}
	explicitDefault := false
	lastModelRole := ""
	var compaction *entry
	compactionIdx, resetIdx := -1, -1
	for i, e := range path {
		switch e.Type {
		case "thinking_level_change":
			ctx.thinkingLevel = "off"
			if e.ThinkingLevel != nil {
				ctx.thinkingLevel = *e.ThinkingLevel
			}
		case "model_change":
			if e.Model != "" {
				role := "default"
				if e.Role != nil {
					role = *e.Role
				}
				models[role] = e.Model
				lastModelRole = role
				if role == "default" {
					explicitDefault = true
				}
			}
		case "message":
			if e.Message.Role == "assistant" && !explicitDefault {
				models["default"] = e.Message.Provider + "/" + e.Message.Model
			}
		case "compaction":
			compaction, compactionIdx = e, i
		case "reset_boundary":
			resetIdx = i
		}
	}
	if err := refuseContextNotes(path); err != nil {
		return nil, err
	}
	ctx.model = restoredModel(models, lastModelRole)

	var messages []llmMessage
	emit := func(e *entry) error {
		converted, err := entryMessages(e)
		if err != nil {
			return err
		}
		messages = append(messages, converted...)
		return nil
	}
	emitRange := func(from, to int) error {
		for _, e := range path[from:to] {
			if err := emit(e); err != nil {
				return err
			}
		}
		return nil
	}

	switch {
	case resetIdx >= 0 && resetIdx > compactionIdx:
		if err := emitRange(resetIdx+1, len(path)); err != nil {
			return nil, err
		}
	case compaction != nil:
		summary, remote, err := compactionSummary(compaction)
		if err != nil {
			return nil, err
		}
		messages = append(messages, summary)
		if remote == nil {
			retained := -1
			for i, e := range path[:compactionIdx] {
				if e.ID == compaction.FirstKeptEntryID {
					retained = i
					break
				}
			}
			if retained < 0 && summary.native != nil && summary.native.Kind == conversation.AnthropicCompaction &&
				compaction.FirstKeptEntryID == "" && compaction.ProviderReplayThroughEntryID != "" {
				if i := indexOf(path, compaction.ProviderReplayThroughEntryID); i >= 0 {
					retained = i + 1
				}
			}
			if retained >= 0 && retained < compactionIdx {
				if err := emitRange(retained, compactionIdx); err != nil {
					return nil, err
				}
			}
		} else if compaction.ProviderReplayThroughEntryID != "" {
			if i := indexOf(path, compaction.ProviderReplayThroughEntryID); i >= 0 && i < compactionIdx {
				if err := emitRange(i+1, compactionIdx); err != nil {
					return nil, err
				}
			}
		}
		if err := emitRange(compactionIdx+1, len(path)); err != nil {
			return nil, err
		}
	default:
		if err := emitRange(0, len(path)); err != nil {
			return nil, err
		}
	}

	messages = stripDanglingToolCalls(messages)
	messages = dropFailedTurns(messages)
	ctx.messages = messages
	return ctx, nil
}

// refuseContextNotes refuses a path whose latest context notebook, since the last reset, holds
// text: Oh My Pi renders it into the context (context-notes.ts), and the reader does not.
func refuseContextNotes(path []*entry) error {
	for i := len(path) - 1; i >= 0; i-- {
		e := path[i]
		if e.Type == "reset_boundary" {
			return nil
		}
		if e.Type != "custom" || e.CustomType != contextNotesType {
			continue
		}
		var data map[string]json.RawMessage
		if json.Unmarshal(e.Data, &data) != nil || len(data) != 2 || string(data["version"]) != "1" {
			continue
		}
		var text string
		if json.Unmarshal(data["text"], &text) != nil {
			continue
		}
		if text == "" {
			return nil
		}
		return conversation.Unknown("experimental context notes (%s)", e.ID)
	}
	return nil
}

// activePath is the path from the last entry, Oh My Pi's leaf after a load, to the root by
// parentId, root first. A parent cycle ends the walk at its first repeat, as Oh My Pi's does.
func activePath(entries []*entry) []*entry {
	byID := make(map[string]*entry, len(entries))
	for _, e := range entries {
		byID[e.ID] = e
	}
	var path []*entry
	seen := map[string]bool{}
	for current := entries[len(entries)-1]; current != nil && !seen[current.ID]; {
		seen[current.ID] = true
		path = append(path, current)
		if current.ParentID == nil {
			break
		}
		current = byID[*current.ParentID]
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path
}

// restoredModel is the model Oh My Pi restores the session on (getRestorableSessionModels' first
// choice): the model of the role the last model change named, unless that is the default or a
// retry fallback, else the default.
func restoredModel(models map[string]string, lastRole string) string {
	if lastRole != "" && lastRole != "default" && lastRole != "fallback" {
		if model, ok := models[lastRole]; ok {
			return model
		}
	}
	return models["default"]
}

func indexOf(path []*entry, id string) int {
	for i, e := range path {
		if e.ID == id {
			return i
		}
	}
	return -1
}

// applyToolResultPrunes blanks every tool result a tool_result_prune record names, as Oh My Pi's
// loader does (tool-result-prunes.ts): its content becomes the record's notice and it is marked
// pruned. A malformed record, or one naming no tool result, is skipped as Oh My Pi skips it.
func applyToolResultPrunes(entries []*entry) error {
	byID := map[string]*entry{}
	for _, e := range entries {
		byID[e.ID] = e
	}
	for _, e := range entries {
		if e.Type != "custom" || e.CustomType != toolResultPruneType {
			continue
		}
		var data struct {
			Results []struct {
				EntryID  *string  `json:"entryId"`
				Notice   *string  `json:"notice"`
				PrunedAt *float64 `json:"prunedAt"`
			} `json:"results"`
		}
		if json.Unmarshal(e.Data, &data) != nil || data.Results == nil {
			continue
		}
		valid := true
		for _, result := range data.Results {
			if result.EntryID == nil || result.Notice == nil || result.PrunedAt == nil {
				valid = false
			}
		}
		if !valid {
			continue
		}
		for _, result := range data.Results {
			target := byID[*result.EntryID]
			if target == nil || target.Type != "message" || target.Message.Role != "toolResult" {
				continue
			}
			notice, err := json.Marshal([]block{{Type: "text", Text: *result.Notice}})
			if err != nil {
				return err
			}
			target.Message.Content = notice
			target.Message.PrunedAt = result.PrunedAt
		}
	}
	return nil
}

// compactionSummary is the user message a compaction contributes (createCompactionSummaryMessage
// through convertMessageToLlm): its summary in Oh My Pi's template, carrying the provider-native
// replay the compaction preserved. remote is that replay when it is OpenAI's replacement history,
// which stands in for the kept messages.
func compactionSummary(c *entry) (summary llmMessage, remote *conversation.Native, err error) {
	if _, ok := c.PreserveData[snapcompactPreserveKey]; ok {
		return llmMessage{}, nil, conversation.Unknown("snapcompact compaction %s", c.ID)
	}
	var details struct {
		Kind string `json:"kind"`
	}
	if len(c.Details) > 0 && json.Unmarshal(c.Details, &details) == nil && details.Kind == contextRolloverKind {
		return llmMessage{}, nil, conversation.Unknown("context-rollover compaction %s", c.ID)
	}
	template := compactionSummaryTemplate
	if c.Method == handoffCompactionMethod {
		template = handoffSummaryTemplate
	}
	summary = llmMessage{role: "user", blocks: []block{{Type: "text", Text: render(template, "{{summary}}", c.Summary)}}}
	if raw, ok := c.PreserveData[openaiRemoteCompaction]; ok {
		var candidate struct {
			Provider           string            `json:"provider"`
			ReplacementHistory []json.RawMessage `json:"replacementHistory"`
		}
		if json.Unmarshal(raw, &candidate) == nil && candidate.Provider != "" && candidate.ReplacementHistory != nil && allObjects(candidate.ReplacementHistory) {
			payload, err := json.Marshal(map[string]any{"type": string(conversation.OpenAIResponsesHistory), "provider": candidate.Provider, "items": candidate.ReplacementHistory})
			if err != nil {
				return llmMessage{}, nil, err
			}
			summary.native = &conversation.Native{Kind: conversation.OpenAIResponsesHistory, Provider: candidate.Provider, Items: candidate.ReplacementHistory, Payload: payload}
			return summary, summary.native, nil
		}
	}
	if raw, ok := c.PreserveData[anthropicCompactionKey]; ok {
		var candidate struct {
			Provider string `json:"provider"`
			Content  string `json:"content"`
		}
		if json.Unmarshal(raw, &candidate) == nil && candidate.Provider != "" && candidate.Content != "" {
			summary.native = &conversation.Native{Kind: conversation.AnthropicCompaction, Provider: candidate.Provider, Payload: raw}
		}
	}
	return summary, nil, nil
}

func allObjects(items []json.RawMessage) bool {
	for _, item := range items {
		trimmed := strings.TrimSpace(string(item))
		if !strings.HasPrefix(trimmed, "{") {
			return false
		}
	}
	return true
}

// entryMessages is what one path entry contributes to the model context (appendMessage, then
// convertToLlm): a message entry's message, a custom message, a branch summary; nothing for any
// other entry.
func entryMessages(e *entry) ([]llmMessage, error) {
	switch e.Type {
	case "message":
		return messageEntryMessages(e)
	case "custom_message":
		if e.CustomType == prewalkPlanType || e.CustomType == vibeModeContextType {
			return nil, nil
		}
		customType := e.CustomType
		if customType == "" {
			customType = defaultCustomType
		}
		attribution := ""
		if e.Attribution != nil {
			attribution = *e.Attribution
		}
		return customMessages(customType, e.Content, attribution, "custom message "+e.ID)
	case "branch_summary":
		if e.Summary == "" {
			return nil, nil
		}
		return []llmMessage{{role: "user", blocks: []block{{Type: "text", Text: render(branchSummaryTemplate, "{{summary}}", e.Summary)}}}}, nil
	}
	return nil, nil
}

func messageEntryMessages(e *entry) ([]llmMessage, error) {
	m := e.Message
	where := "message " + e.ID
	switch m.Role {
	case "user", "developer":
		blocks, err := parseBlocks(m.Content, inputBlocks, where)
		if err != nil {
			return nil, err
		}
		if m.Role == "user" && m.Steering {
			blocks = steeringBlocks(blocks)
		}
		return []llmMessage{{role: "user", blocks: blocks}}, nil
	case "assistant":
		if m.RetryRecovery {
			return nil, nil
		}
		blocks, err := parseBlocks(m.Content, assistantBlocks, where)
		if err != nil {
			return nil, err
		}
		if m.StopReason == "error" && emptyErrorTurn(blocks) {
			return nil, nil
		}
		for i := range blocks {
			if blocks[i].Type == "toolCall" {
				blocks[i].Name = stripXdPrefix(blocks[i].Name)
			}
		}
		return []llmMessage{{role: "assistant", blocks: blocks, api: m.API, provider: m.Provider, model: m.Model, stopReason: m.StopReason, payload: m.ProviderPayload}}, nil
	case "toolResult":
		blocks, err := parseBlocks(m.Content, inputBlocks, where)
		if err != nil {
			return nil, err
		}
		if m.PrunedAt != nil {
			blocks = prunedContent(blocks)
		}
		return []llmMessage{{role: "toolResult", blocks: blocks, toolCallID: m.ToolCallID, toolName: stripXdPrefix(m.ToolName), isError: m.IsError}}, nil
	case "custom":
		return customMessages(m.CustomType, m.Content, m.Attribution, where)
	case "bashExecution", "pythonExecution":
		if m.ExcludeFromContext {
			return nil, nil
		}
		return nil, conversation.Unknown("%s message %s in the context", m.Role, e.ID)
	}
	return nil, conversation.Unknown("message role %q", m.Role)
}

// customMessages is convertToLlm's custom message: a collaborator's prompt in the steering
// envelope and a skill the user invoked as user turns, images split into a user turn of their own,
// anything else a developer message.
func customMessages(customType string, content json.RawMessage, attribution, where string) ([]llmMessage, error) {
	if customType == interruptedThinkingType {
		return nil, conversation.Unknown("an interrupted-thinking turn (%s)", where)
	}
	trimmed := strings.TrimSpace(string(content))
	if !strings.HasPrefix(trimmed, "\"") && !strings.HasPrefix(trimmed, "[") {
		// Not custom message content: Oh My Pi restores no message from it.
		return nil, nil
	}
	blocks, err := parseBlocks(content, inputBlocks, where)
	if err != nil {
		return nil, err
	}
	switch {
	case customType == collabPromptType && attribution == "user":
		return []llmMessage{{role: "user", blocks: steeringBlocks(blocks)}}, nil
	case customType == skillPromptType && attribution == "user":
		return []llmMessage{{role: "user", blocks: blocks}}, nil
	}
	var texts, images []block
	for _, b := range blocks {
		if b.Type == "image" {
			images = append(images, b)
		} else {
			texts = append(texts, b)
		}
	}
	if len(images) == 0 || strings.HasPrefix(trimmed, "\"") {
		return []llmMessage{{role: "user", blocks: blocks}}, nil
	}
	var out []llmMessage
	if len(texts) > 0 {
		out = append(out, llmMessage{role: "user", blocks: texts})
	}
	note := block{Type: "text", Text: "Images attached to " + customType + "."}
	return append(out, llmMessage{role: "user", blocks: append([]block{note}, images...)}), nil
}

// steeringBlocks is wrapSteeringUserMessage: the message's text in the user-interjection envelope,
// its images after it; a message with no text is left as it is.
func steeringBlocks(blocks []block) []block {
	var texts []string
	var images []block
	for _, b := range blocks {
		if b.Type == "text" {
			texts = append(texts, b.Text)
		} else {
			images = append(images, b)
		}
	}
	text := strings.Join(texts, "\n")
	if text == "" {
		return blocks
	}
	return append([]block{{Type: "text", Text: render(steeringTemplate, "{{message}}", text)}}, images...)
}

// prunedContent is getPrunedToolResultContent: a pruned result's text blocks joined into its first
// text block ("[Output truncated]" when they are empty), its images kept.
func prunedContent(blocks []block) []block {
	var text strings.Builder
	first := -1
	for i, b := range blocks {
		if b.Type == "text" {
			text.WriteString(b.Text)
			if first < 0 {
				first = i
			}
		}
	}
	joined := text.String()
	if joined == "" {
		joined = outputTruncatedNotice
	}
	if first < 0 {
		return append([]block{{Type: "text", Text: joined}}, blocks...)
	}
	var out []block
	for i, b := range blocks {
		switch {
		case b.Type != "text":
			out = append(out, b)
		case i == first:
			out = append(out, block{Type: "text", Text: joined})
		}
	}
	return out
}

// emptyErrorTurn is isEmptyErrorTurn's content test: no text, thinking or tool call worth keeping.
func emptyErrorTurn(blocks []block) bool {
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if strings.TrimFunc(b.Text, isJSSpace) != "" {
				return false
			}
		case "thinking":
			if strings.TrimFunc(b.Thinking, isJSSpace) != "" || strings.TrimFunc(b.ThinkingSignature, isJSSpace) != "" {
				return false
			}
		case "redactedThinking":
			if strings.TrimFunc(b.Data, isJSSpace) != "" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// stripXdPrefix is stripXdUrlPrefix: a tool name saved as an xd:// alias replays as the device.
func stripXdPrefix(name string) string {
	const prefix = "xd://"
	if len(name) >= len(prefix) && strings.EqualFold(name[:len(prefix)], prefix) {
		return name[len(prefix):]
	}
	return name
}

// stripDanglingToolCalls is buildSessionContext's dangling tool call pass: a tool call with no
// result in the context is dropped from its assistant turn, and that turn's redacted thinking with
// it and its thinking signatures cleared; a turn left empty is dropped.
func stripDanglingToolCalls(messages []llmMessage) []llmMessage {
	paired := map[string]bool{}
	for _, m := range messages {
		if m.role == "toolResult" {
			paired[m.toolCallID] = true
		}
	}
	out := messages[:0:0]
	for _, m := range messages {
		if m.role != "assistant" || !hasDangling(m.blocks, paired) {
			out = append(out, m)
			continue
		}
		var kept []block
		for _, b := range m.blocks {
			if (b.Type == "toolCall" && !paired[b.ID]) || b.Type == "redactedThinking" {
				continue
			}
			if b.Type == "thinking" {
				b.ThinkingSignature = ""
			}
			kept = append(kept, b)
		}
		if len(kept) == 0 {
			continue
		}
		m.blocks = kept
		out = append(out, m)
	}
	return out
}

func hasDangling(blocks []block, paired map[string]bool) bool {
	for _, b := range blocks {
		if b.Type == "toolCall" && !paired[b.ID] {
			return true
		}
	}
	return false
}

// dropFailedTurns is buildSessionContext's last pass: an aborted or failed assistant turn is not
// replayed, and the tool results answering its calls go with it.
func dropFailedTurns(messages []llmMessage) []llmMessage {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.role != "assistant" || (m.stopReason != "aborted" && m.stopReason != "error") {
			continue
		}
		dropped := map[string]bool{}
		for _, b := range m.blocks {
			if b.Type == "toolCall" {
				dropped[b.ID] = true
			}
		}
		rest := append([]llmMessage{}, messages[:i]...)
		for _, later := range messages[i+1:] {
			if later.role == "toolResult" && dropped[later.toolCallID] {
				continue
			}
			rest = append(rest, later)
		}
		messages = rest
	}
	return messages
}
