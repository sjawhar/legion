// Package answer is the daemon's model loop: it answers one question to an agent that is not
// running, as that agent, by resuming the agent's own conversation on the model the conversation
// last ran on, with the loop's tools (the slice in tools.go, holding gh), and returns the agent's
// reply in its structured form, the turns to append to the conversation, and what the answer
// cost. It is the only package that imports Grafana's SDK (github.com/grafana/ai-sdk) and the
// vendor SDKs under it.
//
// What the SDK leaves to its caller, the loop sets: the step bound (model_calls) with no tool on
// the last allowed step; no retry in the SDK or the vendor clients, and one of its own on a 429, a
// 5xx or a transport error; the provider's base URL from the configuration, never the
// environment; the provider's key read again for every call; the thinking the conversation ran at;
// stateless OpenAI reasoning with its encrypted content; the cache marks; and the time bound over
// the whole answer. A reply cut off, refused, past a bound or not in the form is no answer, with
// the fault named (Fault).
package answer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"

	anthropicopt "github.com/anthropics/anthropic-sdk-go/option"
	aisdk "github.com/grafana/ai-sdk"
	"github.com/grafana/ai-sdk/provider"
	"github.com/grafana/ai-sdk/providers/anthropic"
	"github.com/grafana/ai-sdk/providers/openai"
	openaiopt "github.com/openai/openai-go/v3/option"

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
	// Malformed is a last reply that is not exactly one JSON object of the reply form.
	Malformed Fault = "malformed"
	// ProviderError is a model call that failed: an error status after the loop's one retry, a
	// stream that broke, or a key that could not be read.
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

// GH runs one gh call the model makes: its seq among the answer's calls, from 1, and its
// arguments, a JSON array of strings, gh's arguments without `gh`. The daemon's runs ghtool.Run
// with the answer's ghtool.Answer and bounds.
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

// The provider APIs the loop calls.
const (
	anthropicMessages = "anthropic-messages"
	openaiResponses   = "openai-responses"
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
	case r.provider == config.AnswerOpenAI && r.api == openaiResponses:
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

// placeholderKey is the key each vendor client is built with; the loop's transport puts the key
// it reads for the call in its place on every request.
const placeholderKey = "read-per-call"

func answer(ctx context.Context, req Request) (Reply, error) {
	r, fault, detail := routeOf(req)
	if fault != "" {
		return Reply{Fault: fault, Detail: detail}, nil
	}
	history, err := toProvider(req.Conversation.Messages, r.provider)
	if err != nil {
		return Reply{Fault: UnknownFormat, Detail: err.Error()}, nil
	}
	question := userMessage(req)
	messages := append(history, provider.NewUserMessage(provider.TextPart(question)))

	transport := &modelTransport{base: http.DefaultTransport, credential: r.endpoint.Credential, provider: r.provider}
	client := &http.Client{Transport: transport}
	var model provider.LanguageModel
	var options provider.ProviderOption
	switch r.provider {
	case config.AnswerAnthropic:
		markAnthropicHistory(messages, len(history))
		model = anthropic.New(placeholderKey, r.model, anthropic.WithRequestOptions(
			anthropicopt.WithHTTPClient(client), anthropicopt.WithMaxRetries(0), anthropicopt.WithBaseURL(r.endpoint.BaseURL)))
		options = anthropicOptions(req.Conversation.ThinkingLevel)
	case config.AnswerOpenAI:
		markOpenAIHistory(messages, len(history))
		model = openai.NewResponses(placeholderKey, r.model, openai.WithRequestOptions(
			openaiopt.WithHTTPClient(client), openaiopt.WithMaxRetries(0), openaiopt.WithBaseURL(r.endpoint.BaseURL)))
		options = openaiOptions(req.Conversation.ThinkingLevel)
	}

	run := &loop{req: req, route: r, transport: transport, nextSeq: nextSeq(req.Earlier)}
	answerCtx, cancel := context.WithTimeout(ctx, req.Bounds.Answer)
	defer cancel()
	_, genErr := aisdk.GenerateText(answerCtx, model,
		aisdk.WithSystem(systemPrompt(req)),
		aisdk.WithModelMessages(messages...),
		aisdk.WithTools(run.toolSet()),
		aisdk.WithMaxOutputTokens(req.Bounds.OutputTokens),
		aisdk.WithStopWhen(aisdk.StepCountIs(req.Bounds.ModelCalls)),
		aisdk.WithProviderOptions(options),
		aisdk.WithMaxRetries(0),
		aisdk.WithPrepareStep(run.prepareStep),
		aisdk.OnStepFinish(run.stepFinished),
	)
	reply := run.reply()
	switch {
	case genErr == nil:
	case ctx.Err() != nil:
		return reply, fmt.Errorf("answer: %w", ctx.Err())
	case errors.Is(answerCtx.Err(), context.DeadlineExceeded):
		reply.Fault, reply.Detail = AnswerSeconds, fmt.Sprintf("the answer ran past %s", req.Bounds.Answer)
		return reply, nil
	default:
		reply.Fault, reply.Detail = ProviderError, genErr.Error()
		return reply, nil
	}
	run.finish(&reply, question)
	return reply, nil
}

// loop is one answer's state across its model calls.
type loop struct {
	req       Request
	route     route
	transport *modelTransport

	// question is the question message's index in the step's messages, set on the first step.
	question int

	mu sync.Mutex
	// steps are the model calls that finished, in order.
	steps []aisdk.StepResult

	// gh holds one gh call at a time; nextSeq is the seq the next one gets.
	gh      sync.Mutex
	nextSeq int
}

// nextSeq is the seq after the earlier run's last call.
func nextSeq(earlier []store.AnswerToolCall) int {
	next := 1
	for _, call := range earlier {
		next = max(next, call.Seq+1)
	}
	return next
}

// prepareStep offers no tool on the last allowed step, so the model must reply, and on Anthropic
// moves the newest message's cache mark: the history's end keeps the one Answer put there, the
// newest message gets one, and the one an earlier step put on its newest message is taken off
// (the SDK carries a step's message overrides into later steps; Anthropic allows four marks).
func (l *loop) prepareStep(state aisdk.PrepareStepState) (*aisdk.PrepareStepResult, error) {
	result := &aisdk.PrepareStepResult{}
	if state.StepNumber == l.req.Bounds.ModelCalls-1 {
		result.ToolChoice = &provider.ToolChoice{Type: provider.ToolChoiceNone}
	}
	if l.route.provider != config.AnswerAnthropic || len(state.Messages) == 0 {
		return result, nil
	}
	if state.StepNumber == 0 {
		l.question = len(state.Messages) - 1
	}
	messages := slices.Clone(state.Messages)
	for i := l.question; i < len(messages); i++ {
		if _, marked := messages[i].ProviderOptions["anthropic"].(anthropic.AnthropicCacheControl); marked {
			options := maps.Clone(messages[i].ProviderOptions)
			delete(options, "anthropic")
			messages[i].ProviderOptions = options
		}
	}
	last := len(messages) - 1
	messages[last].ProviderOptions = withCacheMark(messages[last].ProviderOptions)
	result.Messages = messages
	return result, nil
}

// withCacheMark is options with Anthropic's ephemeral cache mark added.
func withCacheMark(options provider.ProviderOptions) provider.ProviderOptions {
	marked := maps.Clone(options)
	if marked == nil {
		marked = provider.ProviderOptions{}
	}
	marked["anthropic"] = anthropic.CacheControl("ephemeral")
	return marked
}

func (l *loop) stepFinished(state aisdk.OnStepFinishState) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.steps = append(l.steps, state.StepResult)
}

// reply is the Reply's usage and cost over every model call that finished.
func (l *loop) reply() Reply {
	l.mu.Lock()
	defer l.mu.Unlock()
	reply := Reply{Calls: len(l.steps)}
	for _, step := range l.steps {
		reply.Usage = sum(reply.Usage, priced(step.Usage, l.route.price))
	}
	reply.CostUSD = reply.Usage.InputCost + reply.Usage.OutputCost + reply.Usage.CacheReadCost + reply.Usage.CacheWriteCost
	return reply
}

// finish reads the last model call's reply into reply: no answer when it was cut off, refused,
// still calling a tool or not in the form; otherwise the reply and the turns to append.
func (l *loop) finish(reply *Reply, question string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.steps) == 0 {
		reply.Fault, reply.Detail = ProviderError, "the model made no reply"
		return
	}
	last := l.steps[len(l.steps)-1]
	var natives [][]json.RawMessage
	if l.route.provider == config.AnswerOpenAI {
		var err error
		if natives, err = l.transport.outputItems(len(l.steps)); err != nil {
			reply.Fault, reply.Detail = ProviderError, err.Error()
			return
		}
		if refused(natives[len(natives)-1]) {
			reply.Fault, reply.Detail = Refusal, "the last reply is a refusal"
			return
		}
	}
	switch last.FinishReason.Unified {
	case provider.FinishReasonLength:
		reply.Fault, reply.Detail = Length, fmt.Sprintf("the last reply was cut off at %d output tokens", l.req.Bounds.OutputTokens)
		return
	case provider.FinishReasonContentFilter:
		reply.Fault, reply.Detail = Refusal, fmt.Sprintf("the provider stopped the last reply: %s", last.FinishReason.Raw)
		return
	case provider.FinishReasonError:
		reply.Fault, reply.Detail = ProviderError, fmt.Sprintf("the last reply ended in an error: %s", last.FinishReason.Raw)
		return
	case provider.FinishReasonToolCalls:
		reply.Fault, reply.Detail = ModelCalls, fmt.Sprintf("the last of %d model calls still called a tool", l.req.Bounds.ModelCalls)
		return
	}
	form, err := parseReply(last.Text, l.req.MayAskNothing)
	if err != nil {
		reply.Fault, reply.Detail = Malformed, fmt.Sprintf("%v: %.500q", err, last.Text)
		return
	}
	reply.Kind, reply.Text, reply.Needs = form.kind, form.text, form.needs
	reply.Append = conversation.Append{
		QuestionID: l.req.Question.ID, Question: question,
		Provider: string(l.route.provider), API: l.route.api, Model: l.route.model,
	}
	for i, step := range l.steps {
		turn := turnOf(step, l.route.price)
		if natives != nil {
			turn.Native = natives[i]
		}
		reply.Append.Turns = append(reply.Append.Turns, turn)
	}
	if err := reply.Append.Validate(); err != nil {
		*reply = Reply{Calls: reply.Calls, Usage: reply.Usage, CostUSD: reply.CostUSD, Fault: Malformed, Detail: err.Error()}
	}
}

// refused is whether a turn's output items hold a refusal.
func refused(items []json.RawMessage) bool {
	for _, raw := range items {
		var item struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		}
		if json.Unmarshal(raw, &item) != nil || item.Type != "message" {
			continue
		}
		for _, part := range item.Content {
			if part.Type == "refusal" {
				return true
			}
		}
	}
	return false
}

// turnOf is a model call as a turn of the append: its thinking (with its signature), redacted
// thinking, text and tool calls in order, each call's result, and what the call used and cost.
func turnOf(step aisdk.StepResult, price config.AnswerPrice) conversation.Turn {
	turn := conversation.Turn{Usage: priced(step.Usage, price)}
	for _, part := range step.Content {
		switch part := part.(type) {
		case aisdk.ReasoningContent:
			var meta struct {
				Signature    string  `json:"signature"`
				RedactedData *string `json:"redactedData"`
			}
			if raw, ok := part.ProviderMetadata["anthropic"]; ok {
				_ = json.Unmarshal(raw, &meta)
			}
			if meta.RedactedData != nil {
				turn.Content = append(turn.Content, conversation.Block{Type: conversation.RedactedThinking, Data: *meta.RedactedData})
				continue
			}
			turn.Content = append(turn.Content, conversation.Block{Type: conversation.Thinking, Text: part.Text, Signature: meta.Signature})
		case aisdk.TextContent:
			if part.Text != "" {
				turn.Content = append(turn.Content, conversation.Block{Type: conversation.Text, Text: part.Text})
			}
		case aisdk.ToolCallContent:
			turn.Content = append(turn.Content, conversation.Block{Type: conversation.ToolCall, ToolCallID: part.ToolCallID, ToolName: part.ToolName, Arguments: part.Input})
		case aisdk.ToolResultContent:
			output := modelOutput(part.Output)
			turn.Results = append(turn.Results, conversation.Block{
				Type: conversation.ToolResult, ToolCallID: part.ToolCallID, ToolName: part.ToolName,
				Content: []conversation.Block{{Type: conversation.Text, Text: output.Text}}, IsError: output.Type == provider.ToolOutputErrorText,
			})
		case aisdk.ToolErrorContent:
			turn.Results = append(turn.Results, conversation.Block{
				Type: conversation.ToolResult, ToolCallID: part.ToolCallID, ToolName: part.ToolName,
				Content: []conversation.Block{{Type: conversation.Text, Text: fmt.Sprint(part.Error)}}, IsError: true,
			})
		}
	}
	return turn
}

// priced is a model call's usage and its cost at price, USD per million tokens: input not read
// from the cache, output (reasoning included), cache writes and cache reads.
func priced(usage provider.Usage, price config.AnswerPrice) conversation.Usage {
	value := func(n *int) int64 {
		if n == nil {
			return 0
		}
		return int64(*n)
	}
	read, write := value(usage.InputTokens.CacheRead), value(usage.InputTokens.CacheWrite)
	input := value(usage.InputTokens.NoCache)
	if usage.InputTokens.NoCache == nil {
		input = max(value(usage.InputTokens.Total)-read-write, 0)
	}
	output := value(usage.OutputTokens.Total)
	const perToken = 1e-6
	return conversation.Usage{
		InputTokens: input, OutputTokens: output, CacheReadTokens: read, CacheWriteTokens: write,
		ReasoningTokens: value(usage.OutputTokens.Reasoning),
		InputCost:       float64(input) * price.Input * perToken,
		OutputCost:      float64(output) * price.Output * perToken,
		CacheReadCost:   float64(read) * price.CacheRead * perToken,
		CacheWriteCost:  float64(write) * price.CacheWrite * perToken,
	}
}

func sum(a, b conversation.Usage) conversation.Usage {
	return conversation.Usage{
		InputTokens: a.InputTokens + b.InputTokens, OutputTokens: a.OutputTokens + b.OutputTokens,
		CacheReadTokens: a.CacheReadTokens + b.CacheReadTokens, CacheWriteTokens: a.CacheWriteTokens + b.CacheWriteTokens,
		ReasoningTokens: a.ReasoningTokens + b.ReasoningTokens,
		InputCost:       a.InputCost + b.InputCost, OutputCost: a.OutputCost + b.OutputCost,
		CacheReadCost: a.CacheReadCost + b.CacheReadCost, CacheWriteCost: a.CacheWriteCost + b.CacheWriteCost,
	}
}

// anthropicOptions is Anthropic's adaptive thinking with summarized display, at the effort of the
// conversation's thinking level; thinking off where the conversation ran with it off.
func anthropicOptions(level string) anthropic.AnthropicOptions {
	switch level {
	case "off":
		return anthropic.AnthropicOptions{Thinking: &anthropic.ThinkingConfig{Type: anthropic.ThinkingDisabled}}
	case "minimal":
		level = "low"
	}
	return anthropic.AnthropicOptions{
		Effort:   level,
		Thinking: &anthropic.ThinkingConfig{Type: anthropic.ThinkingAdaptive, Display: anthropic.ThinkingDisplaySummarized},
	}
}

// openaiOptions is a stateless Responses call (store false, the reasoning's encrypted content
// returned so the next call can carry it) at the reasoning effort of the conversation's thinking
// level.
func openaiOptions(level string) openai.OpenAIResponsesOptions {
	if level == "off" {
		level = "none"
	}
	store := false
	return openai.OpenAIResponsesOptions{
		ReasoningEffort: level,
		Store:           &store,
		Include:         []string{"reasoning.encrypted_content"},
	}
}
