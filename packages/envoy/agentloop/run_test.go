package agentloop

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// The prices the fixture's models charge, in USD per million tokens.
var (
	anthropicPrice = Price{Input: 5, Output: 25, CacheWrite: 6.25, CacheRead: 0.5}
	openaiPrice    = Price{Input: 2, Output: 10, CacheWrite: 2.5, CacheRead: 0.1}
)

// toolName is the name of the one tool every fixture registers.
const toolName = "read"

// recordingTool is the fixture's tool: it records every call it is given and answers with the
// output naming it. onCall, where a test sets one, runs inside the call before it answers.
type recordingTool struct {
	mu     sync.Mutex
	calls  []ToolCall
	onCall func(ToolCall)
}

func (r *recordingTool) register() Tool {
	return Tool{
		Name:        toolName,
		Description: "Read a file.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`),
		Run: func(_ context.Context, call ToolCall) (string, bool) {
			r.mu.Lock()
			r.calls = append(r.calls, call)
			onCall := r.onCall
			r.mu.Unlock()
			if onCall != nil {
				onCall(call)
			}
			return "the contents of " + string(call.Input), false
		},
	}
}

// made are the calls the tool was given, in order.
func (r *recordingTool) made() []ToolCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ToolCall(nil), r.calls...)
}

// fixture is one run against a stand-in: the request, the tool it registers and the path of the
// file its endpoint reads its token from, which a test may rewrite between calls.
type fixture struct {
	req   Request
	tool  *recordingTool
	token string
}

// newFixture is a request against s on the protocol kind speaks, allowed calls model calls, with
// the one recording tool registered and a token read from a file on every attempt.
func newFixture(t *testing.T, s *standIn, kind Kind, calls int) *fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	token := "standin-anthropic-token"
	if kind == OpenAIResponses {
		token = "standin-openai-token"
	}
	writeToken(t, path, token)
	tool := &recordingTool{}
	model := Model{
		Endpoint: Endpoint{Kind: kind, BaseURL: s.server.URL, Token: fileToken(path)},
		ID:       "claude-standin", Effort: "high", Price: anthropicPrice,
	}
	if kind == OpenAIResponses {
		model.Endpoint.BaseURL = s.server.URL + "/v1"
		model.ID, model.Price = "gpt-6.1-standin", openaiPrice
	}
	return &fixture{
		req: Request{
			Model:  model,
			System: "You answer the question with the tools you are given.",
			Input:  []Message{{Role: User, Blocks: []Block{{Kind: TextBlock, Text: "What does note.txt say?"}}}},
			Tools:  []Tool{tool.register()},
			Bounds: Bounds{Time: 30 * time.Second, Calls: calls, OutputTokens: 4000},
		},
		tool: tool, token: path,
	}
}

// writeToken writes the endpoint's token to its file.
func writeToken(t *testing.T, path, token string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fileToken reads the endpoint's token from its file, as a caller holding a rotated key would.
func fileToken(path string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(raw)), nil
	}
}

// messages are an Anthropic request's messages.
func messages(t *testing.T, r recorded) []map[string]any {
	t.Helper()
	var body struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(r.body, &body); err != nil {
		t.Fatal(err)
	}
	return body.Messages
}

// blocks are a message's content blocks.
func blocks(m map[string]any) []map[string]any {
	var out []map[string]any
	content, _ := m["content"].([]any)
	for _, b := range content {
		out = append(out, b.(map[string]any))
	}
	return out
}

// seen is one call the recording tool was given, as a test compares it.
type seen struct {
	index int
	id    string
	input string
}

func madeSeen(calls []ToolCall) []seen {
	out := make([]seen, len(calls))
	for i, c := range calls {
		out[i] = seen{index: c.Index, id: c.ID, input: string(c.Input)}
	}
	return out
}

// historySignature is the signature over the replayed history's thinking, with the characters a
// signature carries that JSON could escape.
const historySignature = "EqQBCkgIARABGAIiQL+/hist0ry==/sig"

// anthropicHistory is a conversation on Anthropic: a request, and a model turn of signed thinking
// and text.
func anthropicHistory() []Message {
	return []Message{
		{Role: User, Blocks: []Block{{Kind: TextBlock, Text: "Write the plan."}}},
		{Role: Assistant, Blocks: []Block{
			{Kind: ThinkingBlock, Text: "The plan needs a migration.", Signature: historySignature},
			{Kind: TextBlock, Text: "The plan is written."},
		}},
	}
}

// threeCallAnthropic are the replies of a three-call run: thinking and a tool call, a second tool
// call, then the answer.
func threeCallAnthropic() []reply {
	return []reply{
		anthropicStream("tool_use", anthropicUsage{Input: 10, Output: 20, CacheWrite: 1000},
			anthropicThinking("Read the note.", "sig-step-1+/=="), anthropicToolUse("toolu_1", toolName, map[string]any{"path": "note.txt"})),
		anthropicStream("tool_use", anthropicUsage{Input: 3, Output: 15, CacheWrite: 50, CacheRead: 1000},
			anthropicToolUse("toolu_2", toolName, map[string]any{"path": "other.txt"})),
		anthropicStream("end_turn", anthropicUsage{Input: 2, Output: 30, CacheWrite: 40, CacheRead: 1050},
			anthropicText("The note says plum.")),
	}
}

// The encrypted reasoning of the OpenAI Responses history, as the caller kept it.
const (
	openaiHistoryOne = "gAAAAhist+/one=="
	openaiHistoryTwo = "gAAAAhist+/two=="
)

// openaiHistory is a conversation on OpenAI Responses: a request, a turn of reasoning and a tool
// call, that call's result, and a turn of reasoning and an answer.
func openaiHistory() []Message {
	return []Message{
		{Role: User, Blocks: []Block{{Kind: TextBlock, Text: "Read note.txt."}}},
		{Role: Assistant, Blocks: []Block{
			{Kind: ReasoningBlock, Text: "Reading the note", ItemID: "rs_h1", Encrypted: openaiHistoryOne},
			{Kind: ToolCallBlock, ToolCallID: "call_h1", Name: toolName, Input: json.RawMessage(`{"path":"note.txt"}`)},
		}},
		{Role: ToolRole, Blocks: []Block{
			{Kind: ToolResultBlock, ToolCallID: "call_h1", Name: toolName, Content: []Block{{Kind: TextBlock, Text: "plum"}}},
		}},
		{Role: Assistant, Blocks: []Block{
			{Kind: ReasoningBlock, Text: "Answering", ItemID: "rs_h2", Encrypted: openaiHistoryTwo},
			{Kind: TextBlock, Text: "It says plum."},
		}},
	}
}

// inputItems are an OpenAI Responses request's input items.
func inputItems(t *testing.T, r recorded) []map[string]any {
	t.Helper()
	var body struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(r.body, &body); err != nil {
		t.Fatal(err)
	}
	return body.Input
}

// An OpenAI Responses run of a request that sets no cache field replays every reasoning item of
// the history with its id and encrypted content byte for byte, runs stateless at the model's
// effort within the run's output token bound, marks the end of the history for the cache once,
// sends the first call's reasoning back in the second, offers no tool on the last call, and
// reports each call's output items as the API sent them.
func TestOpenAIResponsesReplaysReasoningItemsByteForByteAndSendsTheStepsReasoningBack(t *testing.T) {
	t.Parallel()
	stepOne := []string{
		openaiReasoning("rs_s1", "gAAAAstep+/one==", "Checking the note"),
		openaiToolCall("fc_s1", "call_s1", toolName, map[string]any{"path": "note.txt"}),
	}
	stepTwo := []string{openaiMessage("msg_s2", "It said plum.")}
	s := newStandIn(t,
		openaiStream(openaiUsage{Input: 1000, CacheWrite: 900, Output: 50, Reasoning: 20}, "", stepOne...),
		openaiStream(openaiUsage{Input: 1100, Cached: 900, Output: 30}, "", stepTwo...),
	)
	f := newFixture(t, s, OpenAIResponses, 2)
	f.req.History = openaiHistory()
	result, err := Run(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Fault != "" || result.Text != "It said plum." {
		t.Fatalf("result %+v, want the answer and no fault", result)
	}
	got := s.requests()
	if len(got) != 2 {
		t.Fatalf("%d model calls, want 2", len(got))
	}
	for i, r := range got {
		if r.path != "/v1/responses" {
			t.Errorf("call %d went to %s, want /v1/responses", i+1, r.path)
		}
		body := r.decoded(t)
		if body["store"] != false || !reflect.DeepEqual(body["include"], []any{"reasoning.encrypted_content"}) {
			t.Errorf("call %d: store %v, include %v; want a stateless call returning the encrypted reasoning", i+1, body["store"], body["include"])
		}
		if reasoning, _ := body["reasoning"].(map[string]any); reasoning["effort"] != "high" {
			t.Errorf("call %d's reasoning %v, want effort high, the model's", i+1, body["reasoning"])
		}
		if body["max_output_tokens"] != 4000.0 {
			t.Errorf("call %d's max_output_tokens %v, want the run's bound", i+1, body["max_output_tokens"])
		}
		if last := i == 1; (body["tool_choice"] == "none") != last {
			t.Errorf("call %d's tool_choice %v; want none on the last call only", i+1, body["tool_choice"])
		}
		for _, encrypted := range []string{openaiHistoryOne, openaiHistoryTwo} {
			if !bytes.Contains(r.body, []byte(`"encrypted_content":`+quote(encrypted))) {
				t.Errorf("call %d does not carry %s byte for byte", i+1, encrypted)
			}
		}
		var ids, breakpoints []string
		for _, item := range inputItems(t, r) {
			if item["type"] == "reasoning" {
				ids = append(ids, item["id"].(string))
			}
			if countKey(item, "prompt_cache_breakpoint") > 0 {
				breakpoints = append(breakpoints, item["type"].(string)+":"+item["call_id"].(string))
			}
		}
		wantIDs := []string{"rs_h1", "rs_h2"}
		if i == 1 {
			wantIDs = append(wantIDs, "rs_s1")
		}
		if !reflect.DeepEqual(ids, wantIDs) {
			t.Errorf("call %d's reasoning items %v, want %v", i+1, ids, wantIDs)
		}
		if !reflect.DeepEqual(breakpoints, []string{"function_call_output:call_h1"}) {
			t.Errorf("call %d's cache breakpoints %v, want one on the history's last tool result", i+1, breakpoints)
		}
	}
	if !bytes.Contains(got[1].body, []byte(`"encrypted_content":"gAAAAstep+/one=="`)) {
		t.Error("the second call does not carry the first call's reasoning")
	}
	if len(result.Steps) != 2 {
		t.Fatalf("%d steps, want 2", len(result.Steps))
	}
	for i, want := range [][]string{stepOne, stepTwo} {
		var native []string
		for _, item := range result.Steps[i].Native {
			native = append(native, string(item))
		}
		if !reflect.DeepEqual(native, want) {
			t.Errorf("step %d's output items\n%v\nwant them as the API sent them\n%v", i+1, native, want)
		}
	}
}

// finalSchema is the form a Final reply must take, and finalAnswer one reply of that form.
const (
	finalSchema = `{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`
	finalAnswer = `{"answer":"plum"}`
)

// A last reply the provider cut off at the output token limit is a length fault: no value comes
// back, and the step and the usage that call spent are still reported.
func TestALastReplyCutOffIsAFault(t *testing.T) {
	t.Parallel()
	s := newStandIn(t, anthropicStream("max_tokens", anthropicUsage{Input: 7, Output: 4000}, anthropicText(`{"answer":"pl`)))
	f := newFixture(t, s, AnthropicMessages, 1)
	f.req.Final = json.RawMessage(finalSchema)
	result, err := Run(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Fault != FaultLength {
		t.Errorf("fault %q, want %q: %s", result.Fault, FaultLength, result.Detail)
	}
	if result.Value != nil {
		t.Errorf("the cut-off reply gave the value %s", result.Value)
	}
	if len(result.Steps) != 1 || result.Steps[0].Finish != "length" {
		t.Errorf("steps %+v, want the one call that was made", result.Steps)
	}
	if u := result.Usage; u.Input != 7 || u.Output != 4000 || !u.Metered {
		t.Errorf("usage %+v, want the cut-off call's own", u)
	}
}

// More than one of a provider's own reasons reaches the loop as the same normalized length:
// Anthropic ends a reply that way both for the run's output token bound and for a prompt past
// the model's context window, which is no reply cut off at that bound. The detail names the
// provider's own reason, so a reader of the log can tell the two apart.
func TestALengthFaultsDetailNamesTheProvidersOwnReason(t *testing.T) {
	t.Parallel()
	for _, stopReason := range []string{"max_tokens", "model_context_window_exceeded"} {
		t.Run(stopReason, func(t *testing.T) {
			t.Parallel()
			s := newStandIn(t, anthropicStream(stopReason, anthropicUsage{Input: 7, Output: 11}, anthropicText("The note says")))
			result, err := Run(context.Background(), newFixture(t, s, AnthropicMessages, 1).req)
			if err != nil {
				t.Fatal(err)
			}
			if result.Fault != FaultLength {
				t.Fatalf("fault %q, want %q: %s", result.Fault, FaultLength, result.Detail)
			}
			if !strings.Contains(result.Detail, stopReason) {
				t.Errorf("the detail is %q, want the provider's own reason %q in it", result.Detail, stopReason)
			}
		})
	}
}

// A reply the model refused is a refusal fault, on each protocol's own way of saying so, and
// whether or not the request asked for a Final object: a refusal is why the run ended, not a
// reply of the wrong form.
func TestARefusedReplyIsAFault(t *testing.T) {
	t.Parallel()
	refusal := `{"id":"msg_r","type":"message","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"I will not do that."}]}`
	for _, c := range []struct {
		name  string
		kind  Kind
		reply reply
	}{
		{name: "an Anthropic refusal stop reason", kind: AnthropicMessages, reply: anthropicStream("refusal", anthropicUsage{Output: 1}, anthropicText(""))},
		{name: "an OpenAI Responses refusal part", kind: OpenAIResponses, reply: openaiStream(openaiUsage{Output: 1}, "", refusal)},
		{name: "a Chat Completions refusal delta", kind: OpenAIChat, reply: chatRefusalStream("I will not do that.", false)},
		{name: "a Chat Completions refusal message", kind: OpenAIChat, reply: chatRefusalStream("I will not do that.", true)},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			for _, final := range []struct {
				name  string
				value json.RawMessage
			}{
				{name: "with free text asked for"},
				{name: "with a final object asked for", value: json.RawMessage(finalSchema)},
			} {
				t.Run(final.name, func(t *testing.T) {
					t.Parallel()
					s := newStandIn(t, c.reply)
					f := newFixture(t, s, c.kind, 1)
					f.req.Final = final.value
					result, err := Run(context.Background(), f.req)
					if err != nil {
						t.Fatal(err)
					}
					if result.Fault != FaultRefusal {
						t.Errorf("fault %q, want %q: %s", result.Fault, FaultRefusal, result.Detail)
					}
				})
			}
		})
	}
}

// A last reply that is not exactly one object of the request's form is malformed, and one that is
// comes back as the value.
func TestALastReplyThatIsNotTheFinalObjectIsMalformed(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		text string
		want Fault
	}{
		{name: "prose", text: "The note says plum.", want: FaultMalformed},
		{name: "two objects", text: finalAnswer + finalAnswer, want: FaultMalformed},
		{name: "an object the form refuses", text: `{"answer":"plum","aside":"and a pear"}`, want: FaultMalformed},
		{name: "the object itself", text: finalAnswer},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := newStandIn(t, anthropicStream("end_turn", anthropicUsage{Output: 5}, anthropicText(c.text)))
			f := newFixture(t, s, AnthropicMessages, 1)
			f.req.Final = json.RawMessage(finalSchema)
			result, err := Run(context.Background(), f.req)
			if err != nil {
				t.Fatal(err)
			}
			if result.Fault != c.want {
				t.Fatalf("fault %q, want %q: %s", result.Fault, c.want, result.Detail)
			}
			switch {
			case c.want != "" && result.Value != nil:
				t.Errorf("the malformed reply gave the value %s", result.Value)
			case c.want == "" && string(result.Value) != finalAnswer:
				t.Errorf("the value is %s, want %s", result.Value, finalAnswer)
			}
		})
	}
}

// The last allowed model call still calling a tool is past the run's call bound, with the call it
// made reported.
func TestTheLastAllowedCallStillCallingAToolIsPastTheCallBound(t *testing.T) {
	t.Parallel()
	s := newStandIn(t, anthropicStream("tool_use", anthropicUsage{Output: 1}, anthropicToolUse("toolu_1", toolName, map[string]any{"path": "note.txt"})))
	result, err := Run(context.Background(), newFixture(t, s, AnthropicMessages, 1).req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Fault != FaultCalls || len(result.Steps) != 1 {
		t.Errorf("result %+v, want %q after the one call it was allowed: %s", result, FaultCalls, result.Detail)
	}
}

// A run whose model call does not answer within the run's time is a time fault.
func TestARunPastItsTimeBoundIsAFault(t *testing.T) {
	t.Parallel()
	answered := anthropicStream("end_turn", anthropicUsage{Output: 1}, anthropicText("The note says plum."))
	s := newStandIn(t, reply{delay: 30 * time.Second, body: answered.body})
	f := newFixture(t, s, AnthropicMessages, 1)
	f.req.Bounds.Time = 200 * time.Millisecond
	result, err := Run(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Fault != FaultTime {
		t.Errorf("fault %q, want %q: %s", result.Fault, FaultTime, result.Detail)
	}
}

// The cost of a run of a request that sets no cache field is every call's tokens at the model's
// prices, its cache writes and reads included, and every step carries its own call's.
func TestTheCostSumsEveryCallWithItsCacheWrites(t *testing.T) {
	t.Parallel()
	s := newStandIn(t, threeCallAnthropic()...)
	f := newFixture(t, s, AnthropicMessages, 3)
	f.req.History = anthropicHistory()
	result, err := Run(context.Background(), f.req)
	if err != nil || result.Fault != "" {
		t.Fatalf("result %+v, %v", result, err)
	}
	// By hand, from the stand-in's usage objects: input 10+3+2, output 20+15+30, cache writes
	// 1000+50+40 and cache reads 0+1000+1050, at $5, $25, $6.25 and $0.50 per million tokens.
	want := (15*5 + 65*25 + 1090*6.25 + 2050*0.5) / 1e6
	if u := result.Usage; u.Input != 15 || u.Output != 65 || u.CacheWrite != 1090 || u.CacheRead != 2050 || !u.Metered {
		t.Errorf("usage %+v, want every call's summed", u)
	}
	if math.Abs(result.Usage.CostUSD-want) > 1e-12 {
		t.Errorf("cost $%.10f, want $%.10f", result.Usage.CostUSD, want)
	}
	steps := []Usage{
		{Input: 10, Output: 20, CacheWrite: 1000, CostUSD: (10*5 + 20*25 + 1000*6.25) / 1e6, Metered: true},
		{Input: 3, Output: 15, CacheWrite: 50, CacheRead: 1000, CostUSD: (3*5 + 15*25 + 50*6.25 + 1000*0.5) / 1e6, Metered: true},
		{Input: 2, Output: 30, CacheWrite: 40, CacheRead: 1050, CostUSD: (2*5 + 30*25 + 40*6.25 + 1050*0.5) / 1e6, Metered: true},
	}
	if len(result.Steps) != len(steps) {
		t.Fatalf("%d steps, want %d", len(result.Steps), len(steps))
	}
	for i, want := range steps {
		if got := result.Steps[i].Usage; got != want {
			t.Errorf("step %d's usage %+v, want its own call's %+v", i+1, got, want)
		}
	}
}

// environmentHeader is the name of a header the environment asks a vendor client to put on every
// request it makes, whatever endpoint the caller configured.
const environmentHeader = "X-From-The-Environment"

// The endpoint the run calls is the request's, whatever the environment's own base URLs and key
// say, and the call carries the request's token and nothing else the environment holds: neither
// the organization and the project openai-go reads, nor the headers either vendor SDK reads.
func TestTheEndpointComesFromTheRequestAndNeverTheEnvironment(t *testing.T) {
	elsewhere := newStandIn(t)
	t.Setenv("OPENAI_BASE_URL", elsewhere.server.URL+"/v1")
	t.Setenv("ANTHROPIC_BASE_URL", elsewhere.server.URL)
	t.Setenv("OPENAI_API_KEY", "a-key-from-the-environment")
	t.Setenv("OPENAI_ORG_ID", "an-organization-from-the-environment")
	t.Setenv("OPENAI_PROJECT_ID", "a-project-from-the-environment")
	t.Setenv("OPENAI_CUSTOM_HEADERS", environmentHeader+": yes\n"+environmentHeader+"-Too: yes")
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", environmentHeader+": yes")

	openaiStandIn := newStandIn(t, openaiStream(openaiUsage{Input: 1, Output: 1}, "", openaiMessage("msg_1", "It says plum.")))
	if result, err := Run(context.Background(), newFixture(t, openaiStandIn, OpenAIResponses, 1).req); err != nil || result.Fault != "" {
		t.Fatalf("the OpenAI run: %+v, %v", result, err)
	}
	anthropicStandIn := newStandIn(t, anthropicStream("end_turn", anthropicUsage{Input: 1, Output: 1}, anthropicText("It says plum.")))
	if result, err := Run(context.Background(), newFixture(t, anthropicStandIn, AnthropicMessages, 1).req); err != nil || result.Fault != "" {
		t.Fatalf("the Anthropic run: %+v, %v", result, err)
	}
	chatStandIn := newStandIn(t, chatStream("stop", chatUsage{Prompt: 1, Completion: 1}, "It says plum."))
	if result, err := Run(context.Background(), newFixture(t, chatStandIn, OpenAIChat, 1).req); err != nil || result.Fault != "" {
		t.Fatalf("the Chat Completions run: %+v, %v", result, err)
	}
	azureStandIn := newStandIn(t, anthropicStream("end_turn", anthropicUsage{Input: 1, Output: 1}, anthropicText("It says plum.")))
	if result, err := Run(context.Background(), newFixture(t, azureStandIn, AzureAnthropic, 1).req); err != nil || result.Fault != "" {
		t.Fatalf("the Azure run: %+v, %v", result, err)
	}
	if n := len(elsewhere.requests()); n != 0 {
		t.Errorf("the environment's endpoint got %d requests, want none", n)
	}
	if got := openaiStandIn.requests()[0].header.Get("Authorization"); got != "Bearer standin-openai-token" {
		t.Errorf("the OpenAI call's Authorization %q, want the request's token", got)
	}
	for _, c := range []struct {
		kind Kind
		s    *standIn
	}{
		{OpenAIResponses, openaiStandIn}, {AnthropicMessages, anthropicStandIn},
		{OpenAIChat, chatStandIn}, {AzureAnthropic, azureStandIn},
	} {
		for _, name := range []string{"OpenAI-Organization", "OpenAI-Project", environmentHeader, environmentHeader + "-Too"} {
			if got := c.s.requests()[0].header.Get(name); got != "" {
				t.Errorf("the %s call carries %s %q, want none: the package reads nothing from the environment", c.kind, name, got)
			}
		}
	}
}

// A header the environment names is never put on the call, so a header the call needs is never
// taken off it either: OPENAI_CUSTOM_HEADERS naming Content-Type and Accept leaves the
// openai-responses request carrying both, because the package builds openai-go's Responses
// service itself rather than its client, and that service reads no environment at all.
func TestAnEnvironmentHeaderNamingTheCallsOwnHeadersLeavesThemOnTheCall(t *testing.T) {
	t.Setenv("OPENAI_CUSTOM_HEADERS", "Content-Type: application/json\nAccept: application/json")
	s := newStandIn(t, openaiStream(openaiUsage{Input: 1, Output: 1}, "", openaiMessage("msg_1", "It says plum.")))
	result, err := Run(context.Background(), newFixture(t, s, OpenAIResponses, 1).req)
	if err != nil || result.Fault != "" {
		t.Fatalf("the OpenAI run: %+v, %v", result, err)
	}
	got := s.requests()
	if len(got) != 1 {
		t.Fatalf("%d model calls, want 1", len(got))
	}
	for _, name := range []string{"Content-Type", "Accept"} {
		if value := got[0].header.Get(name); value != "application/json" {
			t.Errorf("the call's %s is %q, want application/json: the environment takes no header off a call", name, value)
		}
	}
}

// Every model call carries the token as the endpoint reads it at that call, so a token rotated
// between two calls reaches the second.
func TestTheTokenIsReadAgainForEveryModelCall(t *testing.T) {
	t.Parallel()
	s := newStandIn(t,
		anthropicStream("tool_use", anthropicUsage{Output: 1}, anthropicToolUse("toolu_1", toolName, map[string]any{"path": "note.txt"})),
		anthropicStream("end_turn", anthropicUsage{Output: 1}, anthropicText("The note says plum.")),
	)
	f := newFixture(t, s, AnthropicMessages, 2)
	writeToken(t, f.token, "the-first-token")
	f.tool.onCall = func(ToolCall) { writeToken(t, f.token, "the-rotated-token") }
	if result, err := Run(context.Background(), f.req); err != nil || result.Fault != "" {
		t.Fatalf("result %+v, %v", result, err)
	}
	var tokens []string
	for _, r := range s.requests() {
		tokens = append(tokens, r.header.Get("X-Api-Key"))
	}
	if want := []string{"the-first-token", "the-rotated-token"}; !reflect.DeepEqual(tokens, want) {
		t.Errorf("the calls carried %v, want the file's token at each call, %v", tokens, want)
	}
}

// A 500 is retried once and the retry's reply is the answer; a second 500 is a provider error
// rather than a second retry, and a 400 is a provider error on the first request.
func TestAServerErrorIsRetriedOnceAndARequestErrorIsNot(t *testing.T) {
	t.Parallel()
	overloaded := reply{
		status: http.StatusInternalServerError, headers: map[string]string{"Retry-After": "0"},
		body: []byte(`{"type":"error","error":{"type":"api_error","message":"overloaded"}}`),
	}
	answered := anthropicStream("end_turn", anthropicUsage{Input: 1, Output: 1}, anthropicText("The note says plum."))

	retried := newStandIn(t, overloaded, answered)
	result, err := Run(context.Background(), newFixture(t, retried, AnthropicMessages, 1).req)
	if err != nil || result.Fault != "" || result.Text != "The note says plum." {
		t.Fatalf("after a 500: result %+v, %v", result, err)
	}
	if n := len(retried.requests()); n != 2 {
		t.Errorf("a 500 then a reply took %d requests, want 2", n)
	}

	twice := newStandIn(t, overloaded, overloaded, answered)
	result, err = Run(context.Background(), newFixture(t, twice, AnthropicMessages, 1).req)
	if err != nil || result.Fault != FaultProviderError {
		t.Errorf("after two 500s: result %+v, %v; want %q", result, err, FaultProviderError)
	}
	if n := len(twice.requests()); n != 2 {
		t.Errorf("two 500s took %d requests, want 2: one retry only", n)
	}

	bad := newStandIn(t, reply{status: http.StatusBadRequest, body: []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`)})
	result, err = Run(context.Background(), newFixture(t, bad, AnthropicMessages, 1).req)
	if err != nil || result.Fault != FaultProviderError {
		t.Errorf("after a 400: result %+v, %v; want %q", result, err, FaultProviderError)
	}
	if n := len(bad.requests()); n != 1 {
		t.Errorf("a 400 took %d requests, want 1: a request error is not retried", n)
	}
}

// A redirect is never followed, in a run and in a decision alike: the transport puts the
// endpoint's credential on every request it carries, and Go's own client strips Authorization
// on a cross-domain redirect before a transport runs, so following one would hand the token to
// whatever server the Location names. The client's redirect check fails the call instead, with
// a detail naming the host the redirect points at, and the server the redirect points at hears
// nothing at all.
func TestARedirectIsNotFollowedAndItsTargetIsNeverCalled(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// answer is what the redirect's target would reply with, were it ever called,
		// and call makes the request through the stand-in that redirects, returning the
		// fault it ended on and what that fault says.
		answer reply
		call   func(*testing.T, *standIn) (Fault, string)
	}{
		{
			name:   "a run",
			answer: anthropicStream("end_turn", anthropicUsage{Input: 1, Output: 1}, anthropicText("The target answered.")),
			call: func(t *testing.T, s *standIn) (Fault, string) {
				result, err := Run(context.Background(), newFixture(t, s, AnthropicMessages, 1).req)
				if err != nil {
					t.Fatal(err)
				}
				if result.Text != "" {
					t.Errorf("the run answered %q, want no answer from the redirect's target", result.Text)
				}
				return result.Fault, result.Detail
			},
		},
		{
			name:   "a decision",
			answer: reply{body: []byte(decideAnswer)},
			call: func(t *testing.T, s *standIn) (Fault, string) {
				result, err := Decide(context.Background(), newDecideFixture(t, s))
				if err != nil {
					t.Fatal(err)
				}
				if len(result.Answers) != 0 {
					t.Errorf("the decision answered %+v, want no answer from the redirect's target", result.Answers)
				}
				return result.Fault, result.Detail
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			target := newStandIn(t, c.answer)
			redirecting := newStandIn(t, reply{
				status:  http.StatusTemporaryRedirect,
				headers: map[string]string{"Location": target.server.URL + "/elsewhere"},
			})
			fault, detail := c.call(t, redirecting)
			if fault != FaultProviderError {
				t.Errorf("fault %q, want %q: a redirect is the provider failing the call: %s", fault, FaultProviderError, detail)
			}
			// The host the redirect names, so a caller reading the detail can see
			// where the call was sent and why it went no further.
			if host := target.server.Listener.Addr().String(); !strings.Contains(detail, host) {
				t.Errorf("the detail is %q, want the redirect's target host, %s, named in it", detail, host)
			}
			got := target.requests()
			if len(got) != 0 {
				t.Errorf("the redirect's target got %d requests, want none", len(got))
			}
			for i, r := range got {
				for _, name := range []string{"Authorization", "X-Api-Key"} {
					if credential := r.header.Get(name); credential != "" {
						t.Errorf("request %d to the redirect's target carries %s %q, want no credential anywhere", i+1, name, credential)
					}
				}
			}
		})
	}
}

// A request the loop cannot run at all is an error rather than a fault: nothing comes back in the
// result and no model call is made.
func TestARequestTheLoopCannotRunIsAnError(t *testing.T) {
	t.Parallel()
	s := newStandIn(t)
	for _, c := range []struct {
		name   string
		change func(*Request)
	}{
		{name: "no time bound", change: func(r *Request) { r.Bounds.Time = 0 }},
		{name: "no call bound", change: func(r *Request) { r.Bounds.Calls = 0 }},
		{name: "no output token bound", change: func(r *Request) { r.Bounds.OutputTokens = 0 }},
		{name: "an endpoint that reads no token", change: func(r *Request) { r.Model.Endpoint.Token = nil }},
		{name: "no model", change: func(r *Request) { r.Model.ID = "" }},
		{name: "a tool with no name", change: func(r *Request) { r.Tools[0].Name = "" }},
		{name: "a tool that runs nothing", change: func(r *Request) { r.Tools[0].Run = nil }},
		{name: "a tool registered twice", change: func(r *Request) { r.Tools = append(r.Tools, r.Tools[0]) }},
		{name: "a protocol the loop makes no run on", change: func(r *Request) { r.Model.Endpoint.Kind = SystemOne }},
		{name: "a history block the protocol cannot carry", change: func(r *Request) {
			r.History = []Message{{Role: Assistant, Blocks: []Block{{Kind: ToolResultBlock, ToolCallID: "toolu_1", Name: toolName}}}}
		}},
		{name: "a final that is no JSON schema", change: func(r *Request) { r.Final = json.RawMessage(`{"type":"object","properties":5}`) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, s, AnthropicMessages, 1)
			c.change(&f.req)
			result, err := Run(context.Background(), f.req)
			if err == nil {
				t.Fatalf("the request ran, with result %+v", result)
			}
			if !reflect.DeepEqual(result, Result{}) {
				t.Errorf("the result is %+v, want nothing", result)
			}
		})
	}
	if n := len(s.requests()); n != 0 {
		t.Errorf("%d model calls were made, want none", n)
	}
}
