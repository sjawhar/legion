package agentloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sync"

	anthropicopt "github.com/anthropics/anthropic-sdk-go/option"
	aisdk "github.com/grafana/ai-sdk"
	"github.com/grafana/ai-sdk/output"
	"github.com/grafana/ai-sdk/provider"
	"github.com/grafana/ai-sdk/providers/anthropic"
	"github.com/grafana/ai-sdk/providers/azure"
	"github.com/grafana/ai-sdk/providers/azure/foundry"
	"github.com/grafana/ai-sdk/providers/openai"
	openaicompatible "github.com/grafana/ai-sdk/providers/openai-compatible"
	"github.com/grafana/ai-sdk/schema"
	openaiopt "github.com/openai/openai-go/v3/option"
)

// placeholderKey is the key each vendor client is built with; the run's transport puts the
// token it reads for the attempt in its place on every request.
const placeholderKey = "read-per-call"

// Run makes one run of the loop. Every outcome of the request is a Result: the model's reply,
// or a Fault saying why the run ended short of one, each with every step that finished and
// everything the run spent. The error is for a request the loop cannot run at all, for the
// caller's context ending, and for a callback that returned an error; the Result then still
// carries what the calls made so far produced and cost.
func Run(ctx context.Context, req Request) (Result, error) {
	if err := req.validate(); err != nil {
		return Result{}, err
	}
	kind := req.Model.Endpoint.Kind
	history, err := toProvider(req.History, kind)
	if err != nil {
		return Result{}, fmt.Errorf("agentloop: the request's history: %w", err)
	}
	input, err := toProvider(req.Input, kind)
	if err != nil {
		return Result{}, fmt.Errorf("agentloop: the request's input: %w", err)
	}
	messages := append(history, input...)

	transport := &modelTransport{
		base: http.DefaultTransport, kind: kind,
		token: req.Model.Endpoint.Token, query: req.Model.Endpoint.Query,
	}
	model, options, err := languageModel(req, &http.Client{Transport: transport})
	if err != nil {
		return Result{}, err
	}
	if req.CacheHistory {
		switch kind {
		case AnthropicMessages, AzureAnthropic:
			markAnthropicHistory(messages, len(history))
		case OpenAIResponses:
			markOpenAIHistory(messages, len(history))
		}
	}

	r := &run{
		req: req, kind: kind, transport: transport,
		indexes: make(map[string]int, len(req.Tools)), next: req.FirstToolIndex + 1,
	}
	opts := []aisdk.StreamOption{
		aisdk.WithModelMessages(messages...),
		aisdk.WithMaxOutputTokens(req.Bounds.OutputTokens),
		aisdk.WithStopWhen(aisdk.StepCountIs(req.Bounds.Calls)),
		aisdk.WithProviderOptions(options),
		aisdk.WithMaxRetries(0),
		aisdk.WithPrepareStep(r.prepareStep),
		aisdk.OnStepFinish(r.stepFinished),
	}
	if req.System != "" {
		opts = append(opts, aisdk.WithSystem(req.System))
	}
	if req.Final != nil {
		final, err := finalOutput(req.Final)
		if err != nil {
			return Result{}, err
		}
		opts = append(opts, aisdk.WithOutput(final))
	}
	set, err := r.toolSet()
	if err != nil {
		return Result{}, err
	}
	if len(set) > 0 {
		opts = append(opts, aisdk.WithTools(set))
	}

	runCtx, cancel := context.WithTimeout(ctx, req.Bounds.Time)
	defer cancel()
	r.cancel = cancel
	stream := aisdk.StreamText(runCtx, model, opts...)
	r.drain(stream)

	result := r.result()
	switch failure := r.failure(); {
	case failure != nil:
		return result, failure
	case ctx.Err() != nil:
		return result, fmt.Errorf("agentloop: %w", ctx.Err())
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		result.Fault, result.Detail = FaultTime, fmt.Sprintf("the run went past %s", req.Bounds.Time)
	case stream.Err() != nil:
		result.Fault, result.Detail = FaultProviderError, stream.Err().Error()
	case len(result.Steps) == 0:
		result.Fault, result.Detail = FaultProviderError, "the model made no reply"
	default:
		r.finish(&result, stream)
	}
	return result, nil
}

// validate is whether the request is one the loop can run at all: it has its bounds, an
// endpoint that reads a token, a model, and tools that are each named, run something and are
// named once.
func (req Request) validate() error {
	switch {
	case req.Bounds.Time <= 0 || req.Bounds.Calls <= 0 || req.Bounds.OutputTokens <= 0:
		return errors.New("agentloop: the request has no time, call or output token bound")
	case req.Model.Endpoint.Token == nil:
		return errors.New("agentloop: the request's endpoint reads no token")
	case req.Model.ID == "":
		return errors.New("agentloop: the request names no model")
	}
	named := make(map[string]bool, len(req.Tools))
	for _, t := range req.Tools {
		switch {
		case t.Name == "":
			return errors.New("agentloop: a tool has no name")
		case t.Run == nil:
			return fmt.Errorf("agentloop: tool %s runs nothing", t.Name)
		case named[t.Name]:
			return fmt.Errorf("agentloop: tool %s is registered twice", t.Name)
		}
		named[t.Name] = true
	}
	return nil
}

// languageModel is the model the run calls and the provider options that call carries, for the
// protocol the run's endpoint speaks: the endpoint's base URL, the run's own HTTP client, which
// puts the token on every request, and the vendor client's own retries off, so the only retry is
// the transport's. A protocol Run does not serve is a request it cannot make.
func languageModel(req Request, client *http.Client) (provider.LanguageModel, provider.ProviderOption, error) {
	endpoint := req.Model.Endpoint
	switch endpoint.Kind {
	case AnthropicMessages:
		model := anthropic.New(placeholderKey, req.Model.ID, anthropic.WithRequestOptions(
			anthropicopt.WithHTTPClient(client), anthropicopt.WithMaxRetries(0), anthropicopt.WithBaseURL(endpoint.BaseURL)))
		return model, anthropicOptions(req.Model.Effort), nil
	case OpenAIResponses:
		model := openai.NewResponses(placeholderKey, req.Model.ID, openai.WithRequestOptions(
			openaiopt.WithHTTPClient(client), openaiopt.WithMaxRetries(0), openaiopt.WithBaseURL(endpoint.BaseURL)))
		return model, openaiOptions(req.Model.Effort), nil
	case OpenAIChat:
		// The run's own transport puts the token on every request and adds the
		// endpoint's query parameters, so the client is given neither headers nor query
		// parameters of its own: one mechanism, in the transport, for every kind.
		// WithIncludeUsage asks the stream for its token counts, and
		// WithStructuredOutputs lets a Final reply's schema ride as
		// response_format.json_schema rather than being downgraded to a bare json_object.
		model := openaicompatible.New(req.Model.ID,
			openaicompatible.WithBaseURL(endpoint.BaseURL), openaicompatible.WithAPIKey(placeholderKey),
			openaicompatible.WithHTTPClient(client), openaicompatible.WithIncludeUsage(true),
			openaicompatible.WithStructuredOutputs(true))
		return model, chatOptions(req.Model.Effort), nil
	case AzureAnthropic:
		// The deployment name is given only where it differs from the model id, and the
		// foundry config owns the base URL, which NewAnthropic applies itself.
		deployment := endpoint.Deployment
		if deployment == "" {
			deployment = req.Model.ID
		}
		config := foundry.Config{BaseURL: endpoint.BaseURL, Credential: func(ctx context.Context) (foundry.Credential, error) {
			token, err := endpoint.Token(ctx)
			if err != nil {
				return foundry.Credential{}, err
			}
			return foundry.Credential{AuthToken: token}, nil
		}}
		model, err := azure.NewAnthropic(config, req.Model.ID, deployment, anthropic.WithRequestOptions(
			anthropicopt.WithHTTPClient(client), anthropicopt.WithMaxRetries(0)))
		if err != nil {
			return nil, nil, fmt.Errorf("agentloop: the %s endpoint: %w", endpoint.Kind, err)
		}
		return model, anthropicOptions(req.Model.Effort), nil
	default:
		return nil, nil, fmt.Errorf("agentloop: the loop makes no run on the %s protocol", endpoint.Kind)
	}
}

// finalOutput is Request.Final as the SDK's output: the last reply is validated against the
// schema and read back as the object itself. A schema the SDK refuses is a request the loop
// cannot run.
func finalOutput(final json.RawMessage) (aisdk.Output, error) {
	s, err := schema.SchemaFromJSON(final)
	if err != nil {
		return nil, fmt.Errorf("agentloop: the request's final schema: %w", err)
	}
	out, err := output.Object[json.RawMessage](s)
	if err != nil {
		return nil, fmt.Errorf("agentloop: the request's final schema: %w", err)
	}
	return out, nil
}

// run is one Run's state across its model calls.
type run struct {
	req       Request
	kind      Kind
	transport *modelTransport
	// cancel ends the run's own context, which is how a callback's error stops the run.
	cancel context.CancelFunc

	// newest is the newest message's index in a step's messages, set on the first step.
	newest int

	mu sync.Mutex
	// steps are the model calls that finished, in order; text and reason are the last of
	// them's reply and finish reason; failed is the first error a callback returned.
	steps  []Step
	text   string
	reason provider.FinishReason
	failed error

	// tool holds one tool call at a time, so a step's calls run one after another.
	tool sync.Mutex

	// numbering guards the tool call indexes: indexes is the index each call's id took
	// when the model asked for it, and next is the index the next call takes.
	numbering sync.Mutex
	indexes   map[string]int
	next      int
}

// drain reads both of the run's streams to their end, which the SDK needs for the run to finish
// at all: the Final object's snapshots in a goroutine, the stream's own parts here. Each is read
// to the end whether or not the caller takes it, and after a callback has failed.
func (r *run) drain(stream *aisdk.StreamTextResult) {
	partials := make(chan struct{})
	go func() {
		defer close(partials)
		for snapshot := range stream.PartialOutputStream() {
			if r.req.OnPartial == nil || r.failure() != nil {
				continue
			}
			if err := r.req.OnPartial(snapshot); err != nil {
				r.fail(err)
			}
		}
	}()
	for part := range stream.FullStream() {
		delta, ok := part.(aisdk.StreamTextDelta)
		if !ok || r.req.OnText == nil || r.failure() != nil {
			continue
		}
		if err := r.req.OnText(delta.Text); err != nil {
			r.fail(err)
		}
	}
	<-partials
}

// fail keeps the first error a callback returned and ends the run; failure is that error.
func (r *run) fail(err error) {
	r.mu.Lock()
	if r.failed == nil {
		r.failed = err
	}
	r.mu.Unlock()
	r.cancel()
}

func (r *run) failure() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failed
}

// prepareStep offers no tool on the last allowed call, so that call must answer, and on the
// kinds Anthropic serves, where the run asks for cache marks, moves the newest message's mark:
// the history's end keeps the one Run put there, the newest message gets one, and the one an
// earlier step put on its newest message is taken off (the SDK carries a step's message
// overrides into later steps; Anthropic allows four marks).
func (r *run) prepareStep(state aisdk.PrepareStepState) (*aisdk.PrepareStepResult, error) {
	result := &aisdk.PrepareStepResult{}
	if state.StepNumber == r.req.Bounds.Calls-1 {
		result.ToolChoice = &provider.ToolChoice{Type: provider.ToolChoiceNone}
	}
	if (r.kind != AnthropicMessages && r.kind != AzureAnthropic) || !r.req.CacheHistory || len(state.Messages) == 0 {
		return result, nil
	}
	if state.StepNumber == 0 {
		r.newest = len(state.Messages) - 1
	}
	messages := slices.Clone(state.Messages)
	for i := r.newest; i < len(messages); i++ {
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

// stepFinished keeps one model call that finished and gives it to the caller's OnStep. The
// call's own output items are read once the run is over, so the step the callback is given
// carries no Native.
func (r *run) stepFinished(state aisdk.OnStepFinishState) {
	step := Step{
		Blocks: blocksOf(state.Content, r.kind),
		Usage:  priced(state.Usage, r.req.Model.Price),
		Finish: string(state.FinishReason.Unified),
	}
	r.mu.Lock()
	r.steps = append(r.steps, step)
	r.text, r.reason = state.Text, state.FinishReason
	r.mu.Unlock()
	if r.req.OnStep == nil || r.failure() != nil {
		return
	}
	if err := r.req.OnStep(step); err != nil {
		r.fail(err)
	}
}

// result is the run's steps, their usage summed and the last reply's text, as they stand.
func (r *run) result() Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := Result{Text: r.text, Steps: slices.Clone(r.steps)}
	for _, step := range r.steps {
		result.Usage = sum(result.Usage, step.Usage)
	}
	return result
}

// finish reads the last model call into result: its output items, the fault the call ended on,
// and the object the reply was required to be when the request asked for one.
func (r *run) finish(result *Result, stream *aisdk.StreamTextResult) {
	r.mu.Lock()
	reason := r.reason
	r.mu.Unlock()
	if r.kind == OpenAIResponses {
		items, err := r.transport.outputItems(len(result.Steps))
		if err != nil {
			result.Fault, result.Detail = FaultProviderError, err.Error()
			return
		}
		for i := range result.Steps {
			result.Steps[i].Native = items[i]
		}
		if refused(items[len(items)-1]) {
			result.Fault, result.Detail = FaultRefusal, "the last reply is a refusal"
			return
		}
	}
	switch reason.Unified {
	case provider.FinishReasonLength:
		result.Fault, result.Detail = FaultLength, fmt.Sprintf("the last reply was cut off at %d output tokens", r.req.Bounds.OutputTokens)
		return
	case provider.FinishReasonContentFilter:
		result.Fault, result.Detail = FaultRefusal, fmt.Sprintf("the provider stopped the last reply: %s", reason.Raw)
		return
	case provider.FinishReasonError:
		result.Fault, result.Detail = FaultProviderError, fmt.Sprintf("the last reply ended in an error: %s", reason.Raw)
		return
	case provider.FinishReasonToolCalls:
		result.Fault, result.Detail = FaultCalls, fmt.Sprintf("the last of %d model calls still called a tool", r.req.Bounds.Calls)
		return
	}
	if r.req.Final == nil {
		return
	}
	if err := stream.OutputError(); err != nil {
		result.Fault, result.Detail = FaultMalformed, fmt.Sprintf("%v: %.500q", err, result.Text)
		return
	}
	value, ok := stream.OutputValue().(json.RawMessage)
	if !ok {
		result.Fault, result.Detail = FaultMalformed, fmt.Sprintf("the last reply is no object of the form: %.500q", result.Text)
		return
	}
	result.Value = value
}

// refused is whether a call's output items hold a refusal.
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

// blocksOf is one model call's content as this package's blocks, in the order the model
// produced it: its thinking or its reasoning, its text, its tool calls and their results.
func blocksOf(content []aisdk.ContentPart, kind Kind) []Block {
	var blocks []Block
	for _, part := range content {
		switch part := part.(type) {
		case aisdk.ReasoningContent:
			blocks = append(blocks, reasoningBlock(part, kind))
		case aisdk.TextContent:
			if part.Text != "" {
				blocks = append(blocks, Block{Kind: TextBlock, Text: part.Text})
			}
		case aisdk.ToolCallContent:
			blocks = append(blocks, Block{Kind: ToolCallBlock, ToolCallID: part.ToolCallID, Name: part.ToolName, Input: part.Input})
		case aisdk.ToolResultContent:
			output := modelOutput(part.Output)
			blocks = append(blocks, Block{
				Kind: ToolResultBlock, ToolCallID: part.ToolCallID, Name: part.ToolName,
				Content: []Block{{Kind: TextBlock, Text: output.Text}}, IsError: output.Type == provider.ToolOutputErrorText,
			})
		case aisdk.ToolErrorContent:
			blocks = append(blocks, Block{
				Kind: ToolResultBlock, ToolCallID: part.ToolCallID, Name: part.ToolName,
				Content: []Block{{Kind: TextBlock, Text: fmt.Sprint(part.Error)}}, IsError: true,
			})
		}
	}
	return blocks
}

// reasoningBlock is one reasoning part as the protocol's own block: an OpenAI reasoning item
// with the id and the encrypted content its provider metadata carries, or Anthropic thinking
// with its signature, or redacted thinking's payload. Step.Native stays the authoritative copy.
func reasoningBlock(part aisdk.ReasoningContent, kind Kind) Block {
	if kind == OpenAIResponses {
		var meta struct {
			ItemID    string  `json:"itemId"`
			Encrypted *string `json:"reasoningEncryptedContent"`
		}
		if raw, ok := part.ProviderMetadata["openai"]; ok {
			_ = json.Unmarshal(raw, &meta)
		}
		block := Block{Kind: ReasoningBlock, Text: part.Text, ItemID: meta.ItemID}
		if meta.Encrypted != nil {
			block.Encrypted = *meta.Encrypted
		}
		return block
	}
	var meta struct {
		Signature    string  `json:"signature"`
		RedactedData *string `json:"redactedData"`
	}
	if raw, ok := part.ProviderMetadata["anthropic"]; ok {
		_ = json.Unmarshal(raw, &meta)
	}
	if meta.RedactedData != nil {
		return Block{Kind: ThinkingBlock, Redacted: *meta.RedactedData}
	}
	return Block{Kind: ThinkingBlock, Text: part.Text, Signature: meta.Signature}
}

// priced is one model call's usage and its cost at price, USD per million tokens: input not
// read from the cache, output with its reasoning counted in it, cache writes and cache reads.
// A call the provider reported no count at all for is not metered.
func priced(usage provider.Usage, price Price) Usage {
	value := func(n *int) int {
		if n == nil {
			return 0
		}
		return *n
	}
	read, write := value(usage.InputTokens.CacheRead), value(usage.InputTokens.CacheWrite)
	input := value(usage.InputTokens.NoCache)
	if usage.InputTokens.NoCache == nil {
		input = max(value(usage.InputTokens.Total)-read-write, 0)
	}
	output := value(usage.OutputTokens.Total)
	counted := []*int{
		usage.InputTokens.Total, usage.InputTokens.NoCache, usage.InputTokens.CacheRead, usage.InputTokens.CacheWrite,
		usage.OutputTokens.Total, usage.OutputTokens.Text, usage.OutputTokens.Reasoning,
	}
	return Usage{
		Input: input, Output: output, CacheWrite: write, CacheRead: read,
		Reasoning: value(usage.OutputTokens.Reasoning),
		CostUSD: perMillion(input, price.Input) + perMillion(output, price.Output) +
			perMillion(write, price.CacheWrite) + perMillion(read, price.CacheRead),
		Metered: slices.ContainsFunc(counted, func(n *int) bool { return n != nil }),
	}
}

// sum is two usages added, their costs included; the sum is metered where either side is.
func sum(a, b Usage) Usage {
	return Usage{
		Input: a.Input + b.Input, Output: a.Output + b.Output,
		CacheWrite: a.CacheWrite + b.CacheWrite, CacheRead: a.CacheRead + b.CacheRead,
		Reasoning: a.Reasoning + b.Reasoning,
		CostUSD:   a.CostUSD + b.CostUSD,
		Metered:   a.Metered || b.Metered,
	}
}

// anthropicOptions is Anthropic's adaptive thinking with summarized display, at the model's
// effort, and thinking off where the run asks for none. A Final reply's schema rides in
// output_config.format rather than in a forced json tool, which would override the tool choice
// the last call is made with.
func anthropicOptions(effort string) anthropic.AnthropicOptions {
	options := anthropic.AnthropicOptions{StructuredOutputMode: anthropic.StructuredOutputFormat}
	if effort == "off" {
		options.Thinking = &anthropic.ThinkingConfig{Type: anthropic.ThinkingDisabled}
		return options
	}
	if effort == "minimal" {
		effort = "low"
	}
	options.Effort = effort
	options.Thinking = &anthropic.ThinkingConfig{Type: anthropic.ThinkingAdaptive, Display: anthropic.ThinkingDisplaySummarized}
	return options
}

// openaiEffort is the loop's own effort words in OpenAI's: a model asked for no reasoning at all
// is configured "off", which both of OpenAI's protocols name "none". Every other word is the
// provider's own and rides as it is.
func openaiEffort(effort string) string {
	if effort == "off" {
		return "none"
	}
	return effort
}

// openaiOptions is a stateless Responses call (store false, the reasoning's encrypted content
// returned so the next call can carry it) at the model's reasoning effort.
func openaiOptions(effort string) openai.OpenAIResponsesOptions {
	store := false
	return openai.OpenAIResponsesOptions{
		ReasoningEffort: openaiEffort(effort),
		Store:           &store,
		Include:         []string{"reasoning.encrypted_content"},
	}
}

// chatOptions is a Chat Completions call at the model's reasoning effort, with a Final reply's
// schema sent as it is rather than in OpenAI's strict mode: strict mode refuses schemas that are
// valid JSON Schema, a oneOf at the root or a property left out of required among them, and this
// package reads the reply back against the schema itself, raising FaultMalformed where it does
// not fit, so strict mode would turn requests the model can serve into requests it refuses and
// buy no check the run does not already make.
func chatOptions(effort string) openaicompatible.OpenAIOptions {
	strict := false
	return openaicompatible.OpenAIOptions{
		ReasoningEffort:  openaiEffort(effort),
		StrictJSONSchema: &strict,
	}
}
