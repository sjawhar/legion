package agentloop

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/rand/v2"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// An openai-chat run reaches the Chat Completions path under the endpoint's base URL, carries
// the endpoint's own query parameters and the endpoint's token as a bearer token, and reads the
// reply and the stream's usage chunk back.
func TestOpenAIChatSendsTheEndpointsQueryAndTheBearerToken(t *testing.T) {
	t.Parallel()
	s := newStandIn(t, chatStream("stop", chatUsage{Prompt: 120, Cached: 20, Completion: 30, Reasoning: 5}, "The note ", "says plum."))
	f := newFixture(t, s, OpenAIChat, 1)
	f.req.Model.Endpoint.Query = url.Values{"api-version": {"2024-10-21"}}
	writeToken(t, f.token, "the-chat-token")
	result, err := Run(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Fault != "" || result.Text != "The note says plum." {
		t.Fatalf("result %+v, want the answer and no fault", result)
	}
	got := s.requests()
	if len(got) != 1 {
		t.Fatalf("%d model calls, want 1", len(got))
	}
	if got[0].path != "/chat/completions" {
		t.Errorf("the call went to %s, want /chat/completions", got[0].path)
	}
	query, err := url.ParseQuery(got[0].query)
	if err != nil {
		t.Fatalf("the call's query %q: %v", got[0].query, err)
	}
	if want := []string{"2024-10-21"}; !reflect.DeepEqual(query["api-version"], want) {
		t.Errorf("the call's api-version %v, want the endpoint's %v", query["api-version"], want)
	}
	if auth := got[0].header.Get("Authorization"); auth != "Bearer the-chat-token" {
		t.Errorf("Authorization %q, want the request's token as a bearer token", auth)
	}
	// The stream's usage chunk, priced at the fixture's model: 120 prompt tokens of which 20
	// were served from the cache, 30 reply tokens of which 5 were reasoning.
	want := Usage{
		Input: 100, Output: 30, CacheRead: 20, Reasoning: 5,
		CostUSD: (100*anthropicPrice.Input + 30*anthropicPrice.Output + 20*anthropicPrice.CacheRead) / 1e6,
		Metered: true,
	}
	if got := result.Usage; got.Input != want.Input || got.Output != want.Output ||
		got.CacheRead != want.CacheRead || got.Reasoning != want.Reasoning || !got.Metered ||
		math.Abs(got.CostUSD-want.CostUSD) > 1e-12 {
		t.Errorf("usage %+v, want the stream's usage chunk metered as %+v", got, want)
	}
}

// An endpoint with no query parameters of its own sends no query string at all, rather than an
// empty one.
func TestOpenAIChatWithNoQuerySendsNoQueryString(t *testing.T) {
	t.Parallel()
	s := newStandIn(t, chatStream("stop", chatUsage{Prompt: 2, Completion: 3}, "The note says plum."))
	f := newFixture(t, s, OpenAIChat, 1)
	f.req.Model.Endpoint.Query = nil
	if result, err := Run(context.Background(), f.req); err != nil || result.Fault != "" {
		t.Fatalf("result %+v, %v", result, err)
	}
	got := s.requests()
	if len(got) != 1 {
		t.Fatalf("%d model calls, want 1", len(got))
	}
	if got[0].query != "" {
		t.Errorf("the call's query string is %q, want none", got[0].query)
	}
}

// A model configured for no reasoning at all reaches Chat Completions as the effort that API
// defines for it, as it does the Responses API: the loop's own "off" is sent as "none".
func TestOpenAIChatSendsEffortOffAsNone(t *testing.T) {
	t.Parallel()
	s := newStandIn(t, chatStream("stop", chatUsage{Prompt: 2, Completion: 3}, "The note says plum."))
	f := newFixture(t, s, OpenAIChat, 1)
	f.req.Model.Effort = "off"
	if result, err := Run(context.Background(), f.req); err != nil || result.Fault != "" {
		t.Fatalf("result %+v, %v", result, err)
	}
	got := s.requests()
	if len(got) != 1 {
		t.Fatalf("%d model calls, want 1", len(got))
	}
	if effort := got[0].decoded(t)["reasoning_effort"]; effort != "none" {
		t.Errorf("the call's reasoning_effort is %v, want none, the API's word for no reasoning", effort)
	}
}

// An openai-chat run asking for a Final object sends that schema as the Chat Completions API's
// json_schema response format, rather than the bare json_object the provider falls back to, and
// outside OpenAI's strict mode, which refuses schemas this package serves. A run that asks for no
// object sends no response_format at all.
func TestOpenAIChatSendsTheFinalSchemaAsAJSONSchemaResponseFormat(t *testing.T) {
	t.Parallel()
	s := newStandIn(t, chatStream("stop", chatUsage{Prompt: 2, Completion: 3}, finalAnswer))
	f := newFixture(t, s, OpenAIChat, 1)
	f.req.Final = json.RawMessage(finalSchema)
	result, err := Run(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Fault != "" || !sameJSON(t, result.Value, []byte(finalAnswer)) {
		t.Fatalf("result %+v, want %s and no fault", result, finalAnswer)
	}
	got := s.requests()
	if len(got) != 1 {
		t.Fatalf("%d model calls, want 1", len(got))
	}
	var body struct {
		Format *struct {
			Type   string `json:"type"`
			Schema *struct {
				Schema json.RawMessage `json:"schema"`
				Strict bool            `json:"strict"`
			} `json:"json_schema"`
		} `json:"response_format"`
	}
	if err := json.Unmarshal(got[0].body, &body); err != nil {
		t.Fatalf("the request body is no JSON object: %v\n%s", err, got[0].body)
	}
	if body.Format == nil || body.Format.Type != "json_schema" || body.Format.Schema == nil {
		t.Fatalf("the call's response_format is %s, want one of type json_schema carrying the request's schema",
			responseFormatOf(t, got[0]))
	}
	if !sameJSON(t, body.Format.Schema.Schema, []byte(finalSchema)) {
		t.Errorf("the call's response_format schema is %s, want the request's %s", body.Format.Schema.Schema, finalSchema)
	}
	if body.Format.Schema.Strict {
		t.Errorf("the call's response_format is %s, want strict false: the package checks the reply against the schema itself",
			responseFormatOf(t, got[0]))
	}

	// A run that asks for no object leaves the reply free text.
	plain := newStandIn(t, chatStream("stop", chatUsage{Prompt: 2, Completion: 3}, "The note says plum."))
	g := newFixture(t, plain, OpenAIChat, 1)
	if result, err := Run(context.Background(), g.req); err != nil || result.Fault != "" {
		t.Fatalf("the run with no Final: %+v, %v", result, err)
	}
	free := plain.requests()
	if len(free) != 1 {
		t.Fatalf("%d model calls with no Final, want 1", len(free))
	}
	if format, ok := free[0].decoded(t)["response_format"]; ok {
		t.Errorf("the call with no Final sends response_format %v, want none", format)
	}
}

// responseFormatOf is a recorded request's response_format as it was sent, for a failure to
// quote.
func responseFormatOf(t *testing.T, r recorded) []byte {
	t.Helper()
	format, err := json.Marshal(r.decoded(t)["response_format"])
	if err != nil {
		t.Fatal(err)
	}
	return format
}

// An azure-anthropic run reaches the Messages path under the endpoint's /anthropic service root,
// is authorized with the endpoint's token as a bearer token and with no API key header, and names
// the deployment as the model it calls, falling back to the model's own id where the endpoint
// names no deployment.
func TestAzureAnthropicSendsTheDeploymentNameAndTheBearerToken(t *testing.T) {
	t.Parallel()
	answered := func() reply {
		return anthropicStream("end_turn", anthropicUsage{Input: 1, Output: 1}, anthropicText("The note says plum."))
	}
	s := newStandIn(t, answered())
	f := newFixture(t, s, AzureAnthropic, 1)
	f.req.Model.Endpoint.Deployment = "claude-standin-deployment"
	writeToken(t, f.token, "the-azure-token")
	result, err := Run(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Fault != "" || result.Text != "The note says plum." {
		t.Fatalf("result %+v, want the answer and no fault", result)
	}
	got := s.requests()
	if len(got) != 1 {
		t.Fatalf("%d model calls, want 1", len(got))
	}
	if got[0].path != "/anthropic/v1/messages" {
		t.Errorf("the call went to %s, want the Messages path under /anthropic", got[0].path)
	}
	if auth := got[0].header.Get("Authorization"); auth != "Bearer the-azure-token" {
		t.Errorf("Authorization %q, want the request's token as a bearer token", auth)
	}
	if key := got[0].header.Get("X-Api-Key"); key != "" {
		t.Errorf("the call carries X-Api-Key %q, want none: the token goes in the bearer header", key)
	}
	if model := got[0].decoded(t)["model"]; model != "claude-standin-deployment" {
		t.Errorf("the call names the model %v, want the endpoint's deployment", model)
	}

	bare := newStandIn(t, answered())
	g := newFixture(t, bare, AzureAnthropic, 1)
	if result, err := Run(context.Background(), g.req); err != nil || result.Fault != "" {
		t.Fatalf("the run with no deployment: %+v, %v", result, err)
	}
	if model := bare.requests()[0].decoded(t)["model"]; model != g.req.Model.ID {
		t.Errorf("with no deployment the call names the model %v, want the model's own id %s", model, g.req.Model.ID)
	}
}

// An azure-anthropic run of a request that sets no cache field marks the cache as an
// anthropic-messages run does: one mark kept at the end of the replayed history and one on the
// newest message, moved there as the run goes on.
func TestAzureAnthropicMarksTheCacheLikeAnthropic(t *testing.T) {
	t.Parallel()
	s := newStandIn(t,
		anthropicStream("tool_use", anthropicUsage{Input: 10, Output: 5}, anthropicToolUse("toolu_1", toolName, map[string]any{"path": "note.txt"})),
		anthropicStream("end_turn", anthropicUsage{Input: 20, Output: 9}, anthropicText("The note says plum.")),
	)
	f := newFixture(t, s, AzureAnthropic, 2)
	f.req.History = anthropicHistory()
	if result, err := Run(context.Background(), f.req); err != nil || result.Fault != "" {
		t.Fatalf("result %+v, %v", result, err)
	}
	got := s.requests()
	if len(got) != 2 {
		t.Fatalf("%d model calls, want 2", len(got))
	}
	// The wire's messages: the history's request and model turn (0, 1), the new question (2),
	// then the first call's turn and the tool result answering it.
	wantMarks := [][]int{{1, 2}, {1, 4}}
	for i, r := range got {
		if n := countKey(r.decoded(t), "cache_control"); n != 2 {
			t.Errorf("call %d carries %d cache marks, want 2", i+1, n)
		}
		if marks := marked(t, r); !reflect.DeepEqual(marks, wantMarks[i]) {
			t.Errorf("call %d marks messages %v, want %v (the history's end and the newest message)", i+1, marks, wantMarks[i])
		}
	}
}

// proseSchema is the form of a reply that is one prose field, and proseDeltas the pieces one such
// reply arrives in.
const proseSchema = `{"type":"object","properties":{"prose":{"type":"string"}},"required":["prose"],"additionalProperties":false}`

var proseDeltas = []string{`{"prose":"The note `, `says plum`, ` and pear."}`}

// sameJSON is whether two JSON documents hold the same value, whatever they are written as.
func sameJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("%.200q is no JSON: %v", a, err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatalf("%.200q is no JSON: %v", b, err)
	}
	return reflect.DeepEqual(x, y)
}

// A run's callbacks see it as it happens: the reply's prose reaches OnText in the order it
// streams and in full, the Final object reaches OnPartial as it is built, the last snapshot being
// the value the run answers with and an earlier one already carrying part of the prose, and
// OnStep is given each model call as it finishes, with that call's own usage and finish reason.
func TestOnTextOnPartialAndOnStepSeeTheRunAsItHappens(t *testing.T) {
	t.Parallel()
	s := newStandIn(t,
		anthropicStream("tool_use", anthropicUsage{Input: 10, Output: 5}, anthropicToolUse("toolu_1", toolName, map[string]any{"path": "note.txt"})),
		anthropicStream("end_turn", anthropicUsage{Input: 20, Output: 9}, anthropicTexts(proseDeltas...)),
	)
	f := newFixture(t, s, AnthropicMessages, 2)
	f.req.Final = json.RawMessage(proseSchema)
	var mu sync.Mutex
	var texts, partials []string
	var steps []Step
	f.req.OnText = func(delta string) error {
		mu.Lock()
		defer mu.Unlock()
		texts = append(texts, delta)
		return nil
	}
	f.req.OnPartial = func(snapshot json.RawMessage) error {
		mu.Lock()
		defer mu.Unlock()
		partials = append(partials, string(snapshot))
		return nil
	}
	f.req.OnStep = func(step Step) error {
		mu.Lock()
		defer mu.Unlock()
		steps = append(steps, step)
		return nil
	}
	result, err := Run(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Fault != "" {
		t.Fatalf("result %+v, want no fault: %s", result, result.Detail)
	}

	if len(texts) < 2 {
		t.Errorf("OnText saw %v, want the reply as several deltas", texts)
	}
	if joined := strings.Join(texts, ""); joined != result.Text {
		t.Errorf("OnText's deltas join to %q, want the reply %q", joined, result.Text)
	}
	if len(partials) < 2 {
		t.Fatalf("OnPartial saw %d snapshots, want at least 2", len(partials))
	}
	for i, snapshot := range partials {
		if !json.Valid([]byte(snapshot)) {
			t.Errorf("snapshot %d is no JSON: %q", i+1, snapshot)
		}
	}
	if last := partials[len(partials)-1]; !sameJSON(t, []byte(last), result.Value) {
		t.Errorf("the last snapshot is %s, want the run's value %s", last, result.Value)
	}
	if first := partials[0]; !strings.Contains(first, "The note") || sameJSON(t, []byte(first), result.Value) {
		t.Errorf("the first snapshot is %s, want part of the prose before the object is whole", first)
	}

	if !reflect.DeepEqual(steps, result.Steps) {
		t.Fatalf("OnStep saw %+v, want one call each, as the result reports them: %+v", steps, result.Steps)
	}
	if len(steps) != 2 {
		t.Fatalf("OnStep was called %d times, want once per model call", len(steps))
	}
	wantUsage := []Usage{
		{Input: 10, Output: 5, CostUSD: (10*anthropicPrice.Input + 5*anthropicPrice.Output) / 1e6, Metered: true},
		{Input: 20, Output: 9, CostUSD: (20*anthropicPrice.Input + 9*anthropicPrice.Output) / 1e6, Metered: true},
	}
	for i, want := range []string{"tool-calls", "stop"} {
		if steps[i].Finish != want {
			t.Errorf("step %d's finish reason is %q, want %q", i+1, steps[i].Finish, want)
		}
		if steps[i].Usage != wantUsage[i] {
			t.Errorf("step %d's usage is %+v, want that call's own %+v", i+1, steps[i].Usage, wantUsage[i])
		}
	}
}

// A callback that returns an error ends the run there: Run returns that error, the result still
// carries the model calls that finished and what they spent, and no model call is made past the
// one the callback stopped.
func TestACallbacksErrorEndsTheRunAndIsReturnedWithTheResultSoFar(t *testing.T) {
	t.Parallel()
	stopped := errors.New("the caller stopped the run")
	// Three replies for a three-call run: a tool call, then the prose and a second tool call,
	// then the answer.
	replies := func() []reply {
		return []reply{
			anthropicStream("tool_use", anthropicUsage{Input: 10, Output: 5}, anthropicToolUse("toolu_1", toolName, map[string]any{"path": "note.txt"})),
			anthropicStream("tool_use", anthropicUsage{Input: 20, Output: 9}, anthropicTexts(proseDeltas...), anthropicToolUse("toolu_2", toolName, map[string]any{"path": "other.txt"})),
			anthropicStream("end_turn", anthropicUsage{Input: 30, Output: 7}, anthropicText(`{"prose":"The note says plum."}`)),
		}
	}
	// With no callback to stop it the run makes all three calls, which is the run each case
	// below cuts short.
	whole := newStandIn(t, replies()...)
	f := newFixture(t, whole, AnthropicMessages, 3)
	f.req.Final = json.RawMessage(proseSchema)
	if result, err := Run(context.Background(), f.req); err != nil || result.Fault != "" {
		t.Fatalf("the run no callback stops: %+v, %v", result, err)
	}
	if n := len(whole.requests()); n != 3 {
		t.Fatalf("the run no callback stops made %d model calls, want 3", n)
	}
	for _, c := range []struct {
		name string
		// set puts the failing callback on the request, and calls is the model calls the
		// run has made when the callback is first given something: the call the callback
		// ends the run on. OnText and OnPartial are first called on the prose of the
		// second call; OnStep when the first call finishes.
		set   func(*Request)
		calls int
	}{
		{name: "OnText", set: func(r *Request) { r.OnText = func(string) error { return stopped } }, calls: 2},
		{name: "OnPartial", set: func(r *Request) { r.OnPartial = func(json.RawMessage) error { return stopped } }, calls: 2},
		{name: "OnStep", set: func(r *Request) { r.OnStep = func(Step) error { return stopped } }, calls: 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := newStandIn(t, replies()...)
			f := newFixture(t, s, AnthropicMessages, 3)
			f.req.Final = json.RawMessage(proseSchema)
			c.set(&f.req)
			result, err := Run(context.Background(), f.req)
			if !errors.Is(err, stopped) {
				t.Fatalf("Run returned %v, want the callback's own error", err)
			}
			// The steps the result carries are the ones that finished: the calls
			// before the one in flight, and that call too where it finished before
			// the error landed, which it may, since OnPartial is given its
			// snapshots from the run's own second reader.
			if len(result.Steps) < 1 || len(result.Steps) > c.calls {
				t.Fatalf("the result carries %d steps, want between 1 and the %d calls made: %+v", len(result.Steps), c.calls, result.Steps)
			}
			if first := (Usage{Input: 10, Output: 5, CostUSD: (10*anthropicPrice.Input + 5*anthropicPrice.Output) / 1e6, Metered: true}); result.Steps[0].Usage != first {
				t.Errorf("the first step's usage is %+v, want the first call's own %+v", result.Steps[0].Usage, first)
			}
			var want Usage
			for _, step := range result.Steps {
				want = sum(want, step.Usage)
			}
			if result.Usage != want {
				t.Errorf("the result's usage is %+v, want the finished steps' %+v", result.Usage, want)
			}
			if !result.Usage.Metered || result.Usage.Input == 0 {
				t.Errorf("the result's usage is %+v, want what the calls that finished spent", result.Usage)
			}
			if n := len(s.requests()); n != c.calls {
				t.Errorf("%d model calls were made, want %d: the run stops at the one in flight", n, c.calls)
			}
		})
	}
}

// The tools of one step are numbered in the order the model asked for them, counting on from the
// request's FirstToolIndex, however long each tool takes to answer.
func TestToolCallsAreNumberedInTheModelsOrderFromFirstToolIndex(t *testing.T) {
	t.Parallel()
	const first = 7
	want := map[string]int{"toolu_a": first + 1, "toolu_b": first + 2, "toolu_c": first + 3}
	// Run the body often enough that a numbering that followed the order the tools finished
	// in, rather than the order the model asked in, would show.
	for round := range 20 {
		s := newStandIn(t,
			anthropicStream("tool_use", anthropicUsage{Output: 3},
				anthropicToolUse("toolu_a", toolName, map[string]any{"path": "a.txt"}),
				anthropicToolUse("toolu_b", toolName, map[string]any{"path": "b.txt"}),
				anthropicToolUse("toolu_c", toolName, map[string]any{"path": "c.txt"})),
			anthropicStream("end_turn", anthropicUsage{Output: 2}, anthropicText("All three read.")),
		)
		f := newFixture(t, s, AnthropicMessages, 2)
		f.req.FirstToolIndex = first
		f.tool.onCall = func(ToolCall) { time.Sleep(time.Duration(rand.IntN(1000)) * time.Microsecond) }
		if result, err := Run(context.Background(), f.req); err != nil || result.Fault != "" {
			t.Fatalf("round %d: result %+v, %v", round+1, result, err)
		}
		got := make(map[string]int, 3)
		for _, call := range f.tool.made() {
			got[call.ID] = call.Index
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("round %d numbered the calls %v, want the model's order from %d: %v", round+1, got, first, want)
		}
	}
}

// A tool that refuses and a tool the set does not hold both go back to the model as tool results,
// the refusal marked as an error, and the run goes on to an answer rather than failing.
func TestAToolErrorAndAnUnknownToolGoBackToTheModel(t *testing.T) {
	t.Parallel()
	s := newStandIn(t,
		anthropicStream("tool_use", anthropicUsage{Output: 4},
			anthropicToolUse("toolu_1", toolName, map[string]any{"path": "gone.txt"}),
			anthropicToolUse("toolu_2", "write", map[string]any{"path": "note.txt"})),
		anthropicStream("end_turn", anthropicUsage{Output: 6}, anthropicText("The note is gone.")),
	)
	f := newFixture(t, s, AnthropicMessages, 2)
	refused := f.req.Tools[0]
	refused.Run = func(context.Context, ToolCall) (string, bool) { return "gone.txt is not there", true }
	f.req.Tools = []Tool{refused}
	result, err := Run(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Fault != "" || result.Text != "The note is gone." {
		t.Fatalf("result %+v, want the answer and no fault: %s", result, result.Detail)
	}
	got := s.requests()
	if len(got) != 2 {
		t.Fatalf("%d model calls, want 2: both results go back to the model", len(got))
	}
	sent := messages(t, got[1])
	results := map[string]map[string]any{}
	for _, b := range blocks(sent[len(sent)-1]) {
		if b["type"] == "tool_result" {
			results[b["tool_use_id"].(string)] = b
		}
	}
	if len(results) != 2 {
		t.Fatalf("the second call carries %d tool results, want both: %v", len(results), sent[len(sent)-1])
	}
	if refusal := results["toolu_1"]; refusal["is_error"] != true || !strings.Contains(toolResultText(refusal), "gone.txt is not there") {
		t.Errorf("the refused call went back as %v, want the tool's reason marked as an error", refusal)
	}
	if unknown := results["toolu_2"]; unknown["is_error"] != true || !strings.Contains(toolResultText(unknown), "write") {
		t.Errorf("the unknown tool's call went back as %v, want an error naming the tool", unknown)
	}
}

// toolResultText is the text of one Messages API tool_result block, whichever shape its content
// takes.
func toolResultText(block map[string]any) string {
	switch content := block["content"].(type) {
	case string:
		return content
	case []any:
		var texts []string
		for _, part := range content {
			if part, ok := part.(map[string]any); ok && part["type"] == "text" {
				texts = append(texts, part["text"].(string))
			}
		}
		return strings.Join(texts, "\n")
	}
	return ""
}
