package agentloop

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"time"
)

// Kind is the wire protocol an endpoint speaks. It picks the provider a call is made through
// and, with it, the header the endpoint's token is sent in.
type Kind string

// The protocols this package speaks. The first four are Run's; SystemOne is Decide's.
const (
	// AnthropicMessages is Anthropic's Messages API, directly or through a gateway that
	// serves its shape.
	AnthropicMessages Kind = "anthropic-messages"
	// OpenAIResponses is OpenAI's Responses API.
	OpenAIResponses Kind = "openai-responses"
	// OpenAIChat is the Chat Completions shape, which serves both an OpenAI-compatible
	// gateway and the Azure endpoints.
	OpenAIChat Kind = "openai-chat"
	// AzureAnthropic is Claude on Azure Foundry, addressed by a canonical model id and a
	// deployment name.
	AzureAnthropic Kind = "azure-anthropic"
	// SystemOne is the System One decision API, which Decide calls and Run does not serve.
	SystemOne Kind = "systemone"
)

// anthropicKind is whether a protocol speaks Anthropic's Messages shape, directly or through
// Azure Foundry. The two are converted and cache-marked the same way, and a kind added to the
// family is added here.
func anthropicKind(kind Kind) bool {
	return kind == AnthropicMessages || kind == AzureAnthropic
}

// Endpoint is where a model is called and how that call is authorized. Every part of it comes
// from the caller; this package reads nothing of it from the environment.
type Endpoint struct {
	// Kind is the protocol the endpoint speaks, and so the header its token is sent in.
	Kind Kind
	// BaseURL is the root the provider's own paths hang off.
	BaseURL string
	// Query holds the query parameters added to every request to the endpoint, such as
	// Azure's api-version. An empty Query adds none, never an empty one.
	Query url.Values
	// Deployment is the Azure deployment name, for a kind that addresses a deployment and
	// a model separately and when the two differ. Other kinds leave it empty.
	Deployment string
	// Token returns the credential for one HTTP attempt. On every kind but
	// azure-anthropic it is called for each attempt, the one retry included, so a caller
	// may read a key that has been rotated or mint a token scoped to the call. On
	// azure-anthropic it is called once per model call: the Azure provider's credential
	// middleware sets the header above this package's HTTP client, and the retry below it
	// resends the first attempt's token.
	Token func(context.Context) (string, error)
}

// Price is what a model charges, in USD per million tokens, for prompt tokens, reply tokens, a
// cache write and a cache read. A zero Price leaves every CostUSD at zero.
type Price struct{ Input, Output, CacheWrite, CacheRead float64 }

// cost is what a call's token counts cost at the price, and is the one place this package
// prices anything: every Usage.CostUSD, a run's and a decision's alike, comes from here. It
// reads the counts by name off the usage they belong to, so no caller can hand them over in
// the wrong order.
func (p Price) cost(u Usage) float64 {
	return perMillion(u.Input, p.Input) + perMillion(u.Output, p.Output) +
		perMillion(u.CacheWrite, p.CacheWrite) + perMillion(u.CacheRead, p.CacheRead)
}

// perMillion is what a token count costs at a price in USD per million tokens.
func perMillion(tokens int, price float64) float64 {
	return float64(tokens) * price * 1e-6
}

// Model is one model at one endpoint, with what it charges.
type Model struct {
	// Endpoint is where the model is called and how the call is authorized.
	Endpoint Endpoint
	// ID is the model id the provider knows the model by, and Effort the reasoning effort
	// asked of it, in the provider's own words. An empty Effort leaves the provider's
	// default.
	ID, Effort string
	// Price is what the model charges, and is what every Usage.CostUSD is computed from.
	Price Price
}

// validate is whether the model is one this package can call at all: it names a model and its
// endpoint reads a token. Run and Decide make the same two checks, and say the same thing of a
// model that fails them.
func (m Model) validate() error {
	switch {
	case m.Endpoint.Token == nil:
		return errors.New("agentloop: the request's endpoint reads no token")
	case m.ID == "":
		return errors.New("agentloop: the request names no model")
	}
	return nil
}

// Bounds are the limits of one run. Reaching one ends the run with a Fault rather than an error,
// so what the run already produced and already spent is still reported.
type Bounds struct {
	// Time bounds the whole run: every model call, every retry and every tool call
	// together.
	Time time.Duration
	// Calls is the most model calls the run may make, and OutputTokens the most tokens one
	// call may produce. The last allowed call is sent with the tool choice set to none, so
	// that call must answer rather than ask for another tool.
	Calls, OutputTokens int
}

// Role is whose turn a message is.
type Role string

// The roles a message may be.
const (
	// User is a turn from the caller's side of the conversation.
	User Role = "user"
	// Assistant is a turn the model produced.
	Assistant Role = "assistant"
	// ToolRole is a turn carrying the results of the tools the model asked for. It is named
	// ToolRole because Tool is already the type of a registered tool.
	ToolRole Role = "tool"
)

// BlockKind is what one block of a message holds.
type BlockKind string

// The kinds of block a message may hold.
const (
	// TextBlock is prose.
	TextBlock BlockKind = "text"
	// ThinkingBlock is Anthropic's thinking, summarized for display, with the signature or
	// the redacted payload that must be replayed with it.
	ThinkingBlock BlockKind = "thinking"
	// ReasoningBlock is an OpenAI reasoning item, with the id and encrypted content that
	// must be replayed with it.
	ReasoningBlock BlockKind = "reasoning"
	// ToolCallBlock is the model asking for one tool call.
	ToolCallBlock BlockKind = "tool_call"
	// ToolResultBlock is what a tool answered that call with.
	ToolResultBlock BlockKind = "tool_result"
	// ImageBlock is an image, in a user message or inside a tool result.
	ImageBlock BlockKind = "image"
)

// Block is one part of a message. Kind says which of the fields below carry meaning; the rest
// are zero. A block holds everything a provider needs to be given the turn back, so a
// conversation replayed through this package reaches the provider as it left it.
type Block struct {
	// Kind is what the block holds.
	Kind BlockKind
	// Text is the block's own text: a text block's prose, a thinking block's summarized
	// thinking, or a tool result's output. Signature is Anthropic's signature over a
	// thinking block and Redacted its redacted thinking; both are replayed byte for byte.
	Text, Signature, Redacted string
	// ToolCallID is the provider's id for a tool call, which ties a tool result back to the
	// call it answers, and Name is the tool's name.
	ToolCallID, Name string
	// Input is a tool call's arguments, as the model wrote them.
	Input json.RawMessage
	// IsError marks a tool result the tool refused rather than answered.
	IsError bool
	// ItemID and Encrypted are an OpenAI reasoning item's id and its encrypted content,
	// replayed byte for byte.
	ItemID, Encrypted string
	// MediaType and Data are an image block's media type and its base64 data.
	MediaType, Data string
	// Content holds a tool result's parts, text and image, for a result that is more than
	// one text.
	Content []Block
	// Phase is an openai-responses assistant text part's phase, kept so the part goes back
	// as it came.
	Phase string
}

// Message is one turn of a conversation.
type Message struct {
	// Role is whose turn it is, and Blocks what the turn holds, in the order it holds it.
	Role   Role
	Blocks []Block
}

// ToolCall is one call of one tool, as the tool's Run is given it.
type ToolCall struct {
	// Index numbers the call across the whole of the work rather than the run: it is
	// one-based and counts on from Request.FirstToolIndex, so a caller can key an
	// idempotent write on it and have a re-run write the same thing once.
	Index int
	// ID is the provider's id for the call, and Input the arguments the model wrote.
	ID    string
	Input json.RawMessage
}

// Tool is a tool the model may call. How much work the tool will do before it refuses is its
// own Run's business; Bounds.Calls bounds model calls, not tool calls.
type Tool struct {
	// Name is what the model calls the tool by, and Description what the model is told the
	// tool is for.
	Name, Description string
	// Schema is the JSON schema of the tool's arguments.
	Schema json.RawMessage
	// Run does the tool's work and answers with what the model is told. A refusal answers
	// isError true, with the reason as the output; a refusal goes back to the model as a
	// tool result rather than ending the run.
	Run func(ctx context.Context, call ToolCall) (output string, isError bool)
}

// Request is one run of the loop: what to call, what to send it, what it may do and who hears
// about it as it happens.
type Request struct {
	// Model is the model to call and the endpoint to call it at.
	Model Model
	// System is the system prompt.
	System string
	// History is the conversation before this turn and Input the new messages of it, so the
	// stable part of a conversation the caller re-sends is the part marked for caching.
	History []Message
	Input   []Message
	// NoCache turns this package's cache marks off. With it unset, anthropic-messages and
	// azure-anthropic take one ephemeral mark at the end of the history and one on the
	// newest message, which lands on the last of Input, or on a tool result the run itself
	// adds, even where History is empty; openai-responses takes an explicit prompt cache
	// breakpoint at the end of the history; openai-chat takes none, so setting the field
	// changes nothing there. The marks are not free: a provider prices a cache write above
	// plain input, so a single call that is never re-sent pays the write premium and reads
	// nothing back. They pay off on the next call of the run, and on a later run that
	// re-sends the marked prefix within the cache's life, which reads it at the cache's
	// price instead of the input price.
	NoCache bool
	// Tools are the tools the model may call.
	Tools []Tool
	// Bounds are the limits of the run.
	Bounds Bounds
	// FirstToolIndex is the number of tool calls an earlier run of the same work already
	// made, so this run's first call is FirstToolIndex + 1. A fresh run leaves it zero.
	FirstToolIndex int
	// Final is the JSON schema the last reply must be exactly one object of, which is what
	// Result.Value holds. A nil Final asks for free text.
	Final json.RawMessage
	// OnText is given the reply's prose as it streams, OnPartial the Final object as it is
	// built, and OnStep every step as it finishes. An error from any of them ends the run
	// at that point and Run returns it with the Result so far. A nil callback is not
	// called.
	OnText    func(delta string) error
	OnPartial func(snapshot json.RawMessage) error
	OnStep    func(Step) error
}

// Usage is what one model call, or a whole run, consumed and what it cost.
type Usage struct {
	// Input, Output, CacheWrite, CacheRead and Reasoning are the token counts the provider
	// reported: the prompt, the reply, the prompt tokens written to and read from the
	// provider's cache, and the reasoning tokens.
	Input, Output, CacheWrite, CacheRead, Reasoning int
	// CostUSD is those counts priced with Model.Price.
	CostUSD float64
	// Metered is false when the provider reported no counts at all, so that a zero Usage
	// reads as the provider's silence rather than as a call that cost nothing.
	Metered bool
}

// Step is one model call and what came back from it.
type Step struct {
	// Blocks is the reply, in the order the model produced it.
	Blocks []Block
	// Native is the provider's own output items for the call, byte for byte, for a caller
	// that must hand them back exactly as they came.
	Native []json.RawMessage
	// Usage is this call's own, priced.
	Usage Usage
	// Finish is the SDK's normalized finish reason for the call, not the provider's own
	// word for it: stop, length, content-filter, tool-calls, error or other. A reply the
	// provider cut at its token limit reads length, whatever the provider called it.
	Finish string
}

// Result is the outcome of a run, and every outcome is one: a run that ends on a fault, on its
// context ending or on a callback's error still reports every step that finished and what those
// steps spent. A call cut off in flight reports none of its own tokens, since a call's usage is
// read only when the call finishes.
type Result struct {
	// Text is the last reply's prose, and Value the object that reply was required to be
	// when Request.Final asked for one.
	Text  string
	Value json.RawMessage
	// Steps are the run's model calls, in order, and Usage is their total.
	Steps []Step
	Usage Usage
	// Fault is why the run ended short of an answer, and is empty on a run that answered.
	// Detail says what happened, for a log rather than for a wire.
	Fault  Fault
	Detail string
}

// Fault is why a run ended short of an answer. A caller that publishes a reason of its own maps
// these at its own boundary.
type Fault string

// The ways a run ends short of an answer.
const (
	// FaultTime is Bounds.Time spent.
	FaultTime Fault = "time"
	// FaultCalls is Bounds.Calls spent without an answer.
	FaultCalls Fault = "calls"
	// FaultLength is a reply the provider cut at the output token limit.
	FaultLength Fault = "length"
	// FaultRefusal is the model refusing to answer.
	FaultRefusal Fault = "refusal"
	// FaultMalformed is a last reply that is not exactly one object of Request.Final.
	FaultMalformed Fault = "malformed"
	// FaultProviderError is the provider failing: an error status that the one retry did
	// not clear, or a stream that broke after its first byte.
	FaultProviderError Fault = "provider_error"
)
