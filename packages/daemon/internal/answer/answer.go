// Package answer is the daemon's model loop: it answers one question to an agent that is not
// running, as that agent, by resuming the agent's own conversation on the model the conversation
// last ran on, with the loop's tools (the slice in tools.go, holding gh), and returns the agent's
// reply in its structured form, the turns to append to the conversation, and what the answer
// cost.
//
// The model calls themselves are agentloop's (github.com/sjawhar/envoy/agentloop), the one model
// loop the daemon and Dispatch share and the only import of Grafana's SDK: the providers, the
// retry, the step bound with no tool on the last allowed call, the thinking and reasoning
// options, the cache marks and the check of the reply against its schema are all its. This
// package is its caller: it builds the request (the conversation as agentloop's messages, the
// system prompt, the reply form as the request's Final schema, the gh tool, the bounds and the
// endpoint `answers.providers` names, whose key is read again for every call), decides the faults
// that need no model call (no_provider, no_price, unknown_format), maps agentloop's faults onto
// the daemon's own reasons, and reads the run back as the agent's reply, the turns to append and
// what the answer cost.
package answer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/sjawhar/envoy/agentloop"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/conversation"
	"github.com/sjawhar/legion/daemon/internal/ghtool"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// Kind is what the agent's reply is.
type Kind string

const (
	// Answered is an answer to the question.
	Answered Kind = "answered"
	// NotAnswered is a reply that the agent could not answer from its conversation and what its
	// tools read, with what it would need. It is also the kind of every reply with a Fault, which
	// carries no text of the agent's.
	NotAnswered Kind = "not_answered"
	// NeedsWork is a reply that the message asks for work the agent's tools cannot do, such as work
	// needing its working copy or a run of its phase; nothing starts.
	NeedsWork Kind = "needs_work"
	// None is a reply that the message asks nothing of the agent. Only a decision message and an
	// agent's Envoy message may get it (Request.MayAskNothing).
	None Kind = "none"
)

// Fault is why a question got no answer from the model: a bound it passed, or what was wrong.
// The first six are agentloop's own faults under the daemon's names (faultOf); the last three the
// daemon decides itself, before any model call.
type Fault string

const (
	// AnswerSeconds is the answer_seconds bound: the answer ran past its time.
	AnswerSeconds Fault = "answer_seconds"
	// ModelCalls is the model_calls bound: the last allowed model call still called a tool.
	ModelCalls Fault = "model_calls"
	// Length is a last reply cut off at the output_tokens bound.
	Length Fault = "length"
	// Refusal is a last reply the provider refused or filtered.
	Refusal Fault = "refusal"
	// Malformed is a last reply that is not exactly one JSON object of the reply form, or one the
	// form takes but the daemon's own rules refuse (a kind the message does not offer, a reply
	// naming no need), or turns no appender could write.
	Malformed Fault = "malformed"
	// ProviderError is a model call that failed: an error status after agentloop's one retry, a
	// stream that broke, or a key that could not be read; and a request agentloop refused to run
	// at all, which is the daemon's own bug and is logged with its detail.
	ProviderError Fault = "provider_error"
	// NoProvider is a conversation whose model's provider `answers.providers` does not name, or
	// whose provider API the loop does not call; no model call is made.
	NoProvider Fault = "no_provider"
	// NoPrice is a conversation whose model `answers.prices` does not price; no model call is
	// made.
	NoPrice Fault = "no_price"
	// UnknownFormat is a conversation holding history the loop cannot send as its harness would
	// (a provider-side compaction, a native item of a kind it does not know); no model call is
	// made.
	UnknownFormat Fault = "unknown_format"
)

// faultOf is agentloop's fault under the daemon's own name, which is what the controller reads as
// an agent-question's reason and what the daemon's reply to the asker names.
func faultOf(fault agentloop.Fault) Fault {
	switch fault {
	case agentloop.FaultTime:
		return AnswerSeconds
	case agentloop.FaultCalls:
		return ModelCalls
	case agentloop.FaultLength:
		return Length
	case agentloop.FaultRefusal:
		return Refusal
	case agentloop.FaultMalformed:
		return Malformed
	default:
		return ProviderError
	}
}

// GH runs one gh call the model makes: its seq among the answer's calls, from 1, and its
// arguments, a JSON array of strings, gh's arguments without `gh`. The daemon's runs ghtool.Run
// with the answer's ghtool.Answer and bounds, which refuses a call past the gh_calls bound.
type GH interface {
	Run(ctx context.Context, seq int, args string) ghtool.Result
}

// Agent is the stopped agent that answers: its role, its issue, its session and the phase the
// daemon has it in.
type Agent struct {
	Role    claim.Role
	Issue   string
	Session string
	Phase   string
}

// Question is what the agent is asked: its id (which the log line names and the append records),
// its text, who asked and where.
type Question struct {
	ID    string
	Text  string
	Asker string
	Where string
}

// Request is one question to answer.
type Request struct {
	// Conversation is the agent's conversation as its harness rebuilds it on resume; the answer
	// runs on its model, at its thinking level.
	Conversation conversation.Conversation
	Agent        Agent
	// ChangeNote is what has changed on the agent's issue since it stopped (its Dispatch status,
	// its phase, its pull request's state and head), as the daemon writes it.
	ChangeNote string
	Question   Question
	// MayAskNothing is a decision message or an agent's Envoy message, which may carry a question
	// or not: only then may the agent reply None.
	MayAskNothing bool
	// Earlier are the gh calls an earlier run of this question already made, in seq order, each
	// with its result; a call with no Result is of unknown outcome. New calls are numbered after
	// them.
	Earlier []store.AnswerToolCall
	// Providers, Prices and Bounds are `answers`' (config.Answers).
	Providers map[config.AnswerProvider]config.AnswerEndpoint
	Prices    map[string]config.AnswerPrice
	Bounds    config.AnswerBounds
	// GH runs the gh tool's calls.
	GH GH
	// Log takes the answer's `answers: answered` line; slog.Default() when nil.
	Log *slog.Logger
}

// Reply is the agent's reply to a question and what it cost.
type Reply struct {
	// Kind, Text and Needs are the agent's reply in its form: Needs is what the agent would need,
	// for NotAnswered and NeedsWork. A reply with a Fault has Kind NotAnswered and no text.
	Kind  Kind
	Text  string
	Needs string
	// Fault is why the model gave no answer, "" when it replied in the form; Detail says more
	// (the provider's error, the text that was not in the form), for the daemon's log.
	Fault  Fault
	Detail string
	// Append is the question and the model's turns (thinking with its signatures or OpenAI
	// reasoning items, each tool call with its result, the reply), for the conversation's
	// appender; set when the model replied in the form.
	Append conversation.Append
	// Calls is how many model calls the answer made; Usage their tokens and costs summed, cache
	// writes and reads included, reasoning counted as output; CostUSD the sum of its costs.
	Calls   int
	Usage   conversation.Usage
	CostUSD float64
}

// The provider APIs the loop calls, as a conversation names them; each is the protocol agentloop
// speaks to that API.
const (
	anthropicMessages = string(agentloop.AnthropicMessages)
	openaiResponses   = string(agentloop.OpenAIResponses)
)

// Answer answers req's question. Every outcome the question has is a Reply: an answer, the
// agent's reply that it cannot answer, or no answer with its Fault. The error is for a request
// the loop cannot run at all (no question, no gh tool, no bounds) and for ctx ending, when the
// daemon itself stops; the Reply then still carries what the calls made so far cost.
func Answer(ctx context.Context, req Request) (Reply, error) {
	if err := req.validate(); err != nil {
		return Reply{}, err
	}
	log := req.Log
	if log == nil {
		log = slog.Default()
	}
	reply, err := answer(ctx, req)
	if reply.Fault != "" {
		reply.Kind = NotAnswered
	}
	attrs := []any{
		"question", req.Question.ID, "issue", req.Agent.Issue, "role", req.Agent.Role, "session", req.Agent.Session,
		"model", req.Conversation.Model, "kind", reply.Kind, "fault", reply.Fault, "calls", reply.Calls,
		"inputTokens", reply.Usage.InputTokens, "outputTokens", reply.Usage.OutputTokens,
		"cacheWriteTokens", reply.Usage.CacheWriteTokens, "cacheReadTokens", reply.Usage.CacheReadTokens,
		"reasoningTokens", reply.Usage.ReasoningTokens, "costUSD", reply.CostUSD,
	}
	if reply.Detail != "" {
		attrs = append(attrs, "detail", reply.Detail)
	}
	log.Info("answers: answered", attrs...)
	return reply, err
}

func (req Request) validate() error {
	switch {
	case req.Question.ID == "" || strings.TrimSpace(req.Question.Text) == "":
		return errors.New("answer: the request names no question")
	case req.GH == nil:
		return errors.New("answer: the request has no gh tool")
	case req.Bounds.Answer <= 0 || req.Bounds.ModelCalls <= 0 || req.Bounds.OutputTokens <= 0:
		return errors.New("answer: the request has no answer_seconds, model_calls or output_tokens bound")
	}
	return nil
}

// route is the provider a conversation's model is called through.
type route struct {
	provider config.AnswerProvider
	api      string
	// kind is the protocol agentloop speaks to that API.
	kind agentloop.Kind
	// model is the model's id at its provider ("claude-opus-5-5").
	model    string
	endpoint config.AnswerEndpoint
	price    config.AnswerPrice
}

// routeOf is the provider, endpoint and price of the conversation's model: `anthropic/…` on
// Anthropic Messages, `openai/…` on OpenAI Responses, each only where `answers.providers` names
// the provider and `answers.prices` prices the model.
func routeOf(req Request) (route, Fault, string) {
	conv := req.Conversation
	name, model, ok := strings.Cut(conv.Model, "/")
	if !ok || model == "" {
		return route{}, NoProvider, fmt.Sprintf("the conversation's model %q names no provider", conv.Model)
	}
	r := route{provider: config.AnswerProvider(name), api: conv.API, model: model}
	switch {
	case r.provider == config.AnswerAnthropic && r.api == anthropicMessages:
		r.kind = agentloop.AnthropicMessages
	case r.provider == config.AnswerOpenAI && r.api == openaiResponses:
		r.kind = agentloop.OpenAIResponses
	default:
		return route{}, NoProvider, fmt.Sprintf("the loop calls no provider %s on API %q", name, conv.API)
	}
	endpoint, ok := req.Providers[r.provider]
	if !ok {
		return route{}, NoProvider, fmt.Sprintf("answers.providers names no %s", name)
	}
	price, ok := req.Prices[conv.Model]
	if !ok {
		return route{}, NoPrice, fmt.Sprintf("answers.prices prices no %s", conv.Model)
	}
	r.endpoint, r.price = endpoint, price
	return r, "", ""
}

// agentModel is the route as agentloop's model: its protocol and base URL from
// `answers.providers`, its token the provider's key read again for every HTTP attempt
// (config.AnswerCredential.Read, so a rotated key or a minted token is the one each call
// carries), its effort the level the conversation last ran at, and its price `answers.prices`'.
func (r route) agentModel(level string) agentloop.Model {
	return agentloop.Model{
		Endpoint: agentloop.Endpoint{Kind: r.kind, BaseURL: r.endpoint.BaseURL, Token: r.endpoint.Credential.Read},
		ID:       r.model,
		Effort:   level,
		Price: agentloop.Price{
			Input: r.price.Input, Output: r.price.Output,
			CacheWrite: r.price.CacheWrite, CacheRead: r.price.CacheRead,
		},
	}
}

func answer(ctx context.Context, req Request) (Reply, error) {
	r, fault, detail := routeOf(req)
	if fault != "" {
		return Reply{Fault: fault, Detail: detail}, nil
	}
	history, err := toAgentloop(req.Conversation.Messages, r)
	if err != nil {
		return Reply{Fault: UnknownFormat, Detail: err.Error()}, nil
	}
	question := userMessage(req)
	result, runErr := agentloop.Run(ctx, agentloop.Request{
		Model:   r.agentModel(req.Conversation.ThinkingLevel),
		System:  systemPrompt(req),
		History: history,
		Input:   []agentloop.Message{{Role: agentloop.User, Blocks: []agentloop.Block{{Kind: agentloop.TextBlock, Text: question}}}},
		Tools:   toolSet(req),
		Bounds: agentloop.Bounds{
			Time: req.Bounds.Answer, Calls: req.Bounds.ModelCalls, OutputTokens: req.Bounds.OutputTokens,
		},
		FirstToolIndex: firstToolIndex(req.Earlier),
		Final:          finalSchema(req.MayAskNothing),
	})
	reply := Reply{Calls: len(result.Steps), Usage: usageOf(result.Usage, r.price), CostUSD: result.Usage.CostUSD}
	switch {
	case runErr != nil && ctx.Err() != nil:
		return reply, fmt.Errorf("answer: %w", ctx.Err())
	case runErr != nil:
		reply.Fault, reply.Detail = ProviderError, runErr.Error()
		return reply, nil
	case result.Fault != "":
		reply.Fault, reply.Detail = faultOf(result.Fault), result.Detail
		return reply, nil
	}
	finish(&reply, req, r, result, question)
	return reply, nil
}

// firstToolIndex is how many gh calls an earlier run of the question already made, which
// agentloop numbers this run's calls on from, so a re-run's first call takes the seq after the
// recorded ones.
func firstToolIndex(earlier []store.AnswerToolCall) int {
	first := 0
	for _, call := range earlier {
		first = max(first, call.Seq)
	}
	return first
}

// finish reads the run's last reply into reply: the agent's reply in the form, or no answer when
// the form's own rules refuse it; and, with it, the turns to append.
func finish(reply *Reply, req Request, r route, result agentloop.Result, question string) {
	form, err := parseReply(result.Value, req.MayAskNothing)
	if err != nil {
		reply.Fault, reply.Detail = Malformed, fmt.Sprintf("%v: %.500q", err, result.Text)
		return
	}
	reply.Kind, reply.Text, reply.Needs = form.kind, form.text, form.needs
	reply.Append = conversation.Append{
		QuestionID: req.Question.ID, Question: question,
		Provider: string(r.provider), API: r.api, Model: r.model,
	}
	for _, step := range result.Steps {
		reply.Append.Turns = append(reply.Append.Turns, turnOf(step, r.price))
	}
	if err := reply.Append.Validate(); err != nil {
		*reply = Reply{Calls: reply.Calls, Usage: reply.Usage, CostUSD: reply.CostUSD, Fault: Malformed, Detail: err.Error()}
	}
}

// turnOf is one model call as a turn of the append: its thinking (with its signature), redacted
// thinking, text and tool calls in order, each call's result, the provider's own output items for
// a turn that has them (an OpenAI Responses turn, which the appender writes from them), and what
// the call used and cost.
func turnOf(step agentloop.Step, price config.AnswerPrice) conversation.Turn {
	turn := conversation.Turn{Native: step.Native, Usage: usageOf(step.Usage, price)}
	for _, block := range step.Blocks {
		switch block.Kind {
		case agentloop.ThinkingBlock:
			if block.Redacted != "" {
				turn.Content = append(turn.Content, conversation.Block{Type: conversation.RedactedThinking, Data: block.Redacted})
				continue
			}
			turn.Content = append(turn.Content, conversation.Block{Type: conversation.Thinking, Text: block.Text, Signature: block.Signature})
		case agentloop.ReasoningBlock:
			// An OpenAI reasoning item's id and encrypted content are in the turn's native
			// items, which the appender writes the turn from; the block keeps its summary.
			turn.Content = append(turn.Content, conversation.Block{Type: conversation.Thinking, Text: block.Text})
		case agentloop.TextBlock:
			if block.Text != "" {
				turn.Content = append(turn.Content, conversation.Block{Type: conversation.Text, Text: block.Text})
			}
		case agentloop.ToolCallBlock:
			turn.Content = append(turn.Content, conversation.Block{
				Type: conversation.ToolCall, ToolCallID: block.ToolCallID, ToolName: block.Name, Arguments: block.Input,
			})
		case agentloop.ToolResultBlock:
			turn.Results = append(turn.Results, conversation.Block{
				Type: conversation.ToolResult, ToolCallID: block.ToolCallID, ToolName: block.Name,
				Content: []conversation.Block{{Type: conversation.Text, Text: resultText(block)}}, IsError: block.IsError,
			})
		}
	}
	return turn
}

// resultText is a tool result's output as one text, which is how the loop's tools answer.
func resultText(block agentloop.Block) string {
	if len(block.Content) == 0 {
		return block.Text
	}
	texts := make([]string, 0, len(block.Content))
	for _, part := range block.Content {
		texts = append(texts, part.Text)
	}
	return strings.Join(texts, "\n")
}

// usageOf is agentloop's usage as the conversation's: its token counts, and its cost split by
// what each count cost at the model's price (USD per million tokens), which is the breakdown the
// appender writes into the agent's own session beside the turn.
func usageOf(usage agentloop.Usage, price config.AnswerPrice) conversation.Usage {
	const perToken = 1e-6
	return conversation.Usage{
		InputTokens: int64(usage.Input), OutputTokens: int64(usage.Output),
		CacheReadTokens: int64(usage.CacheRead), CacheWriteTokens: int64(usage.CacheWrite),
		ReasoningTokens: int64(usage.Reasoning),
		InputCost:       float64(usage.Input) * price.Input * perToken,
		OutputCost:      float64(usage.Output) * price.Output * perToken,
		CacheReadCost:   float64(usage.CacheRead) * price.CacheRead * perToken,
		CacheWriteCost:  float64(usage.CacheWrite) * price.CacheWrite * perToken,
	}
}
