// Package conversation is a provider-neutral model of an agent's conversation as its harness
// rebuilds it for the model when the session resumes: the messages of the provider request the
// harness would send, in order, with each block as that request carries it, and what the
// conversation last ran on (its model, the provider API, its thinking level). A reader for each
// harness's own format (conversation/omp for Oh My Pi) builds a Conversation, and refuses, with an
// error that Is ErrUnknownFormat, a session holding a record it does not know, so its caller
// answers nothing from it.
//
// Append is the other direction: a question, the model's turns answering it (each with its tool
// calls and their results, and the thinking it produced) and the answer, which an appender for the
// harness writes into the session in the harness's own format, so the agent sees them when it
// resumes.
//
// No system message is modelled: neither harness keeps one in its session (each builds its system
// prompt when it starts), so a caller supplies its own.
package conversation

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Role is who a message is from, as the provider request carries it.
type Role string

const (
	// User is a user turn: what a person typed, and what the harness sends as one (its own notes,
	// summaries of compacted history).
	User Role = "user"
	// Assistant is a model turn.
	Assistant Role = "assistant"
	// Tool is one tool call's result.
	Tool Role = "tool"
)

// BlockType is a content block's kind.
type BlockType string

const (
	// Text is plain text.
	Text BlockType = "text"
	// Thinking is the model's visible reasoning: Text, and the Signature the provider minted for
	// it (Anthropic), empty when the request carries it unsigned.
	Thinking BlockType = "thinking"
	// RedactedThinking is reasoning the provider returned encrypted: Data, opaque, sent back as it
	// came.
	RedactedThinking BlockType = "redacted_thinking"
	// ToolCall is a call the model made: ToolCallID, ToolName and Arguments, a JSON object.
	ToolCall BlockType = "tool_call"
	// ToolResult is a call's result: ToolCallID, ToolName, Content (Text and Image blocks) and
	// IsError.
	ToolResult BlockType = "tool_result"
	// Image is an image: MediaType and ImageData, base64.
	Image BlockType = "image"
)

// Block is one content block. Which fields it uses depends on its Type.
type Block struct {
	Type BlockType
	// Text is a Text block's text and a Thinking block's reasoning.
	Text string
	// Signature is a Thinking block's signature, empty when unsigned.
	Signature string
	// Data is a RedactedThinking block's opaque payload.
	Data string
	// ToolCallID names the call of a ToolCall or ToolResult block.
	ToolCallID string
	// ToolName is the tool of a ToolCall or ToolResult block.
	ToolName string
	// Arguments is a ToolCall block's arguments, a JSON object byte for byte as the session holds
	// it.
	Arguments json.RawMessage
	// Content is a ToolResult block's output: Text and Image blocks.
	Content []Block
	// IsError marks a ToolResult block whose call failed.
	IsError bool
	// MediaType and ImageData are an Image block's media type ("image/png") and base64 data.
	MediaType string
	ImageData string
}

// NativeKind is the kind of provider-native history a message carries.
type NativeKind string

const (
	// OpenAIResponsesHistory is the OpenAI Responses API's own output items for a turn (reasoning
	// items with their id and encrypted_content, function_call items, message items), which the
	// harness sends back as they came when the conversation runs on that API and model.
	OpenAIResponsesHistory NativeKind = "openaiResponsesHistory"
	// AnthropicCompaction is the Anthropic API's own compaction block for a summary it wrote,
	// which the harness can send back in place of the summary's text.
	AnthropicCompaction NativeKind = "anthropicCompaction"
)

// Native is provider-native history a message carries beside its blocks, byte for byte as the
// session holds it.
type Native struct {
	Kind NativeKind
	// Provider is the provider the history came from: only that provider's requests carry it.
	Provider string
	// Incremental is true for OpenAIResponsesHistory items that are this turn's own output, false
	// for items that replace every earlier one (the history of a provider-side compaction).
	Incremental bool
	// Items are the OpenAIResponsesHistory items, each byte for byte.
	Items []json.RawMessage
	// Payload is the whole payload object the session holds.
	Payload json.RawMessage
}

// Message is one message of the rebuilt conversation.
type Message struct {
	Role    Role
	Content []Block
	// Native is the provider-native history the harness sends for this message in place of its
	// blocks, nil when it sends the blocks.
	Native *Native
	// Provider, API and Model are where an Assistant message's turn ran.
	Provider string
	API      string
	Model    string
}

// Conversation is a session's conversation as its harness rebuilds it on resume.
type Conversation struct {
	Messages []Message
	// Model is the model the conversation resumes on, "<provider>/<model>".
	Model string
	// API is the provider API that model is called through ("anthropic-messages",
	// "openai-responses").
	API string
	// ThinkingLevel is the reasoning level the conversation last ran at ("off", "low", "high", …).
	ThinkingLevel string
}

// ErrUnknownFormat is what a reader's error Is when the session holds a record it does not know: a
// type, content block, header version or provider payload, or one it knows but cannot rebuild as
// the harness would. Nothing is answered from such a session.
var ErrUnknownFormat = errors.New("a conversation format the reader does not know")

// FormatError is a reader's refusal, naming what it did not know.
type FormatError struct {
	What string
}

func (e *FormatError) Error() string { return fmt.Sprintf("%s: %s", ErrUnknownFormat, e.What) }

// Is makes a FormatError match ErrUnknownFormat.
func (e *FormatError) Is(target error) bool { return target == ErrUnknownFormat }

// Unknown is a FormatError naming what was not known, formatted as fmt.Sprintf formats.
func Unknown(format string, args ...any) error {
	return &FormatError{What: fmt.Sprintf(format, args...)}
}

// ErrMoved is what an appender's error Is when the session changed since it was read: an append is
// written only after the very content it was built from.
var ErrMoved = errors.New("the session changed since it was read")

// Append is a question and the model's answer to it, which an appender writes into the session.
type Append struct {
	// QuestionID names the question; the appender records it with the answer.
	QuestionID string
	// Question is the question's text, written as a user turn.
	Question string
	// Provider, API and Model are the model that answered: its provider id, provider API and
	// model id, as the harness names them.
	Provider string
	API      string
	Model    string
	// Turns are the model's turns, in order. Every turn but the last made tool calls and carries
	// their results; the last made none and its Text blocks are the answer.
	Turns []Turn
}

// Turn is one model turn of an Append.
type Turn struct {
	// Content is what the model returned, in order: Thinking (with its Signature),
	// RedactedThinking, Text and ToolCall blocks.
	Content []Block
	// Native is the turn's OpenAI Responses output items, each byte for byte as the API returned
	// it, for a turn on that API; the appender writes the turn from them.
	Native []json.RawMessage
	// Results are the turn's ToolResult blocks, one per ToolCall, in the calls' order.
	Results []Block
	// Usage is what the turn's model call used.
	Usage Usage
}

// Usage is one model call's token counts and their cost in US dollars.
type Usage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
	InputCost        float64
	OutputCost       float64
	CacheReadCost    float64
	CacheWriteCost   float64
}

// Validate refuses an Append an appender could not write as a well-formed exchange.
func (a Append) Validate() error {
	switch {
	case a.QuestionID == "":
		return errors.New("append: no question id")
	case a.Question == "":
		return errors.New("append: no question")
	case a.Provider == "" || a.API == "" || a.Model == "":
		return errors.New("append: no provider, API or model")
	case len(a.Turns) == 0:
		return errors.New("append: no turns")
	}
	for i, turn := range a.Turns {
		var calls []Block
		for _, block := range turn.Content {
			switch block.Type {
			case Thinking, RedactedThinking, Text:
			case ToolCall:
				if block.ToolCallID == "" || block.ToolName == "" {
					return fmt.Errorf("append: turn %d: a tool call with no id or name", i+1)
				}
				calls = append(calls, block)
			default:
				return fmt.Errorf("append: turn %d: a %s block in a model turn", i+1, block.Type)
			}
		}
		last := i == len(a.Turns)-1
		if last {
			if len(calls) > 0 {
				return fmt.Errorf("append: the last turn makes %d tool calls; it must answer", len(calls))
			}
			if !hasText(turn.Content) {
				return errors.New("append: the last turn holds no answer text")
			}
		} else if len(calls) == 0 {
			return fmt.Errorf("append: turn %d makes no tool call but is not the last", i+1)
		}
		if len(turn.Results) != len(calls) {
			return fmt.Errorf("append: turn %d: %d tool calls and %d results", i+1, len(calls), len(turn.Results))
		}
		for j, result := range turn.Results {
			if result.Type != ToolResult || result.ToolCallID != calls[j].ToolCallID {
				return fmt.Errorf("append: turn %d: result %d is not the result of call %s", i+1, j+1, calls[j].ToolCallID)
			}
			for _, part := range result.Content {
				if part.Type != Text && part.Type != Image {
					return fmt.Errorf("append: turn %d: result %d holds a %s block", i+1, j+1, part.Type)
				}
			}
		}
	}
	return nil
}

func hasText(blocks []Block) bool {
	for _, block := range blocks {
		if block.Type == Text && block.Text != "" {
			return true
		}
	}
	return false
}
