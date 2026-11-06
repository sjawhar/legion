package answer

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/conversation"
	"github.com/sjawhar/legion/daemon/internal/ghtool"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// The provider-level proofs of the loop — the cache marks, the effort and the thinking options on
// the wire, the retry, the tool choice of the last call, the reply read back against its schema —
// are agentloop's own (packages/envoy/agentloop). What is proved here is the daemon's half: the
// conversation and the prompt as the request agentloop is given, the gh tool and its numbering,
// the faults under the daemon's own names, the turns to append and the cost line.

// stubGH is the gh tool's stand-in: it records each call and answers each with output naming it.
type stubGH struct {
	mu     sync.Mutex
	calls  []ghCall
	onCall func(seq int)
}

type ghCall struct {
	seq  int
	args string
}

func (g *stubGH) Run(_ context.Context, seq int, args string) ghtool.Result {
	g.mu.Lock()
	g.calls = append(g.calls, ghCall{seq: seq, args: args})
	onCall := g.onCall
	g.mu.Unlock()
	if onCall != nil {
		onCall(seq)
	}
	return ghtool.Result{Output: "output of " + args, Exit: 0, Ran: true}
}

func (g *stubGH) made() []ghCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]ghCall(nil), g.calls...)
}

func testBounds(modelCalls int) config.AnswerBounds {
	return config.AnswerBounds{
		Answer: 30 * time.Second, ModelCalls: modelCalls, OutputTokens: 4000, GHCall: 30 * time.Second,
		GHOutputChars: 60000, GHCalls: 20, Concurrent: 4, DailyUSD: 50,
	}
}

// keyFile is a provider key file holding key, and its credential.
func keyFile(t *testing.T, key string) (string, config.AnswerCredential) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, config.AnswerCredential{File: path}
}

var (
	anthropicPrice = config.AnswerPrice{Input: 5, Output: 25, CacheWrite: 6.25, CacheRead: 0.5}
	openaiPrice    = config.AnswerPrice{Input: 2, Output: 10, CacheWrite: 2.5, CacheRead: 0.1}
)

// historySignature is the signature of the Anthropic conversation's thinking, with the characters
// a signature carries that JSON could escape.
const historySignature = "EqQBCkgIARABGAIiQL+/hist0ry==/sig"

// anthropicConversation is a conversation on Anthropic: a request, the agent's signed thinking
// and its answer.
func anthropicConversation() conversation.Conversation {
	return conversation.Conversation{
		Model: "anthropic/claude-standin", API: anthropicMessages, ThinkingLevel: "high",
		Messages: []conversation.Message{
			{Role: conversation.User, Content: []conversation.Block{{Type: conversation.Text, Text: "Write the plan for INFRA-1."}}},
			{
				Role: conversation.Assistant, Provider: "anthropic", API: anthropicMessages, Model: "claude-standin",
				Content: []conversation.Block{
					{Type: conversation.Thinking, Text: "The plan needs a migration.", Signature: historySignature},
					{Type: conversation.Text, Text: "The plan is written."},
				},
			},
		},
	}
}

// anthropicRequest is a question on the Anthropic conversation, against the stand-in.
func anthropicRequest(t *testing.T, s *standIn, gh GH, modelCalls int) Request {
	t.Helper()
	_, credential := keyFile(t, "sk-ant-standin")
	return Request{
		Conversation: anthropicConversation(),
		Agent:        Agent{Role: "planner", Issue: "INFRA-1", Session: "s-planner", Phase: "implementing"},
		ChangeNote:   "Dispatch status: in progress. PR #7: open, head abc123.",
		Question:     Question{ID: "dispatch:m-1", Text: "Is PR 7 green?", Asker: "a person", Where: "the INFRA-1 thread"},
		Providers:    map[config.AnswerProvider]config.AnswerEndpoint{config.AnswerAnthropic: {BaseURL: s.server.URL, Credential: credential}},
		Prices:       map[string]config.AnswerPrice{"anthropic/claude-standin": anthropicPrice},
		Bounds:       testBounds(modelCalls),
		GH:           gh,
		Log:          slog.New(slog.DiscardHandler),
	}
}

// threeStepAnthropic are the replies of a three-call answer: thinking and a gh call, another gh
// call, then the reply.
func threeStepAnthropic() []reply {
	return []reply{
		anthropicStream("tool_use", anthropicUsage{Input: 10, Output: 20, CacheWrite: 1000}, anthropicThinking("Check the PR.", "sig-step-1+/=="), anthropicGH("toolu_1", "pr", "view", "7")),
		anthropicStream("tool_use", anthropicUsage{Input: 3, Output: 15, CacheWrite: 50, CacheRead: 1000}, anthropicGH("toolu_2", "pr", "checks", "7")),
		anthropicStream("end_turn", anthropicUsage{Input: 2, Output: 30, CacheWrite: 40, CacheRead: 1050}, anthropicText(`{"kind":"answered","text":"PR 7 is green.","needs":""}`)),
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

// An Anthropic answer sends the conversation's signed thinking as the session stored it, sends
// each call's own turn back to the next call, and appends the model's turns with their
// signatures, their gh calls and each call's result, under the question the loop asked.
func TestAnAnthropicAnswerReplaysItsSignaturesAndAppendsEveryTurn(t *testing.T) {
	s := newStandIn(t, threeStepAnthropic()...)
	gh := &stubGH{}
	reply, err := Answer(context.Background(), anthropicRequest(t, s, gh, 3))
	if err != nil {
		t.Fatal(err)
	}
	if reply.Fault != "" || reply.Kind != Answered || reply.Text != "PR 7 is green." {
		t.Fatalf("reply %+v, want answered %q", reply, "PR 7 is green.")
	}
	got := s.requests()
	if len(got) != 3 {
		t.Fatalf("%d model calls, want 3", len(got))
	}
	for i, r := range got {
		if !bytes.Contains(r.body, []byte(`"signature":"`+historySignature+`"`)) {
			t.Errorf("call %d does not carry the history's signature byte for byte", i+1)
		}
	}
	// The wire's messages: the history's request and answer (0, 1), the question (2), then each
	// call's turn and its tool result.
	history := blocks(messages(t, got[0])[1])
	if history[0]["type"] != "thinking" || history[0]["signature"] != historySignature || history[0]["thinking"] != "The plan needs a migration." {
		t.Errorf("the history's thinking went as %v", history[0])
	}
	stepOne := blocks(messages(t, got[1])[3])
	if stepOne[0]["type"] != "thinking" || stepOne[0]["signature"] != "sig-step-1+/==" || stepOne[0]["thinking"] != "Check the PR." {
		t.Errorf("the second call carries the first call's turn as %v, want its thinking with its signature", stepOne)
	}
	if calls := gh.made(); !reflect.DeepEqual(calls, []ghCall{{1, `["pr","view","7"]`}, {2, `["pr","checks","7"]`}}) {
		t.Errorf("gh calls %+v", calls)
	}

	turns := reply.Append.Turns
	if len(turns) != 3 || reply.Append.QuestionID != "dispatch:m-1" || reply.Append.Model != "claude-standin" || reply.Append.Provider != "anthropic" {
		t.Fatalf("append %+v", reply.Append)
	}
	if first := turns[0].Content[0]; first.Type != conversation.Thinking || first.Signature != "sig-step-1+/==" || first.Text != "Check the PR." {
		t.Errorf("the first turn's thinking %+v", first)
	}
	if call := turns[0].Content[1]; call.Type != conversation.ToolCall || call.ToolCallID != "toolu_1" || call.ToolName != "gh" {
		t.Errorf("the first turn's call %+v", call)
	}
	if result := turns[1].Results[0]; result.ToolCallID != "toolu_2" || result.Content[0].Text != `output of ["pr","checks","7"]` || result.IsError {
		t.Errorf("the second turn's result %+v", result)
	}
	if !strings.Contains(reply.Append.Question, "Is PR 7 green?") || !strings.Contains(reply.Append.Question, "head abc123") {
		t.Errorf("the appended question %q holds no question or change note", reply.Append.Question)
	}
}

// The answer's cost is every call's tokens at the model's prices, cache writes and reads included,
// and one `answers: answered` line carries it with the tokens summed over every call.
func TestTheCostSumsEveryCallCacheWritesIncludedAndOneLineLogsIt(t *testing.T) {
	s := newStandIn(t, threeStepAnthropic()...)
	var logged bytes.Buffer
	req := anthropicRequest(t, s, &stubGH{}, 3)
	req.Log = slog.New(slog.NewJSONHandler(&logged, nil))
	reply, err := Answer(context.Background(), req)
	if err != nil || reply.Kind != Answered {
		t.Fatalf("reply %+v, %v", reply, err)
	}
	// By hand: input 10+3+2, output 20+15+30, cache writes 1000+50+40, cache reads 0+1000+1050,
	// at $5, $25, $6.25 and $0.50 per million tokens.
	want := (15*5 + 65*25 + 1090*6.25 + 2050*0.5) / 1e6
	if math.Abs(reply.CostUSD-want) > 1e-12 {
		t.Errorf("cost $%.10f, want $%.10f", reply.CostUSD, want)
	}
	if u := reply.Usage; u.InputTokens != 15 || u.OutputTokens != 65 || u.CacheWriteTokens != 1090 || u.CacheReadTokens != 2050 || reply.Calls != 3 {
		t.Errorf("usage %+v over %d calls", u, reply.Calls)
	}
	// Each turn carries its own call's cost, which the appender writes beside it.
	if first := reply.Append.Turns[0].Usage; first.InputTokens != 10 || math.Abs(first.CacheWriteCost-1000*6.25/1e6) > 1e-12 {
		t.Errorf("the first turn's usage %+v", first)
	}
	var lines []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(logged.Bytes()), []byte("\n")) {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		if record["msg"] == "answers: answered" {
			lines = append(lines, record)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("%d answers: answered lines, want 1: %s", len(lines), logged.Bytes())
	}
	line := lines[0]
	for key, value := range map[string]any{
		"question": "dispatch:m-1", "issue": "INFRA-1", "role": "planner", "session": "s-planner",
		"model": "anthropic/claude-standin", "calls": 3.0, "inputTokens": 15.0, "outputTokens": 65.0,
		"cacheWriteTokens": 1090.0, "cacheReadTokens": 2050.0, "reasoningTokens": 0.0,
	} {
		if line[key] != value {
			t.Errorf("the line's %s is %v, want %v", key, line[key], value)
		}
	}
	if cost, _ := line["costUSD"].(float64); math.Abs(cost-want) > 1e-12 {
		t.Errorf("the line's costUSD %v, want %v", line["costUSD"], want)
	}
}

// The loop's tool list holds gh, every model call offers it, and the system prompt names it.
func TestTheLoopsToolListHoldsGh(t *testing.T) {
	if !slices.ContainsFunc(tools, func(t tool) bool { return t.name == "gh" }) {
		t.Fatalf("the loop's tools %v hold no gh", tools)
	}
	s := newStandIn(t, anthropicStream("end_turn", anthropicUsage{Input: 1, Output: 1}, anthropicText(`{"kind":"answered","text":"Yes.","needs":""}`)))
	if _, err := Answer(context.Background(), anthropicRequest(t, s, &stubGH{}, 1)); err != nil {
		t.Fatal(err)
	}
	var body struct {
		System []struct {
			Text string `json:"text"`
		} `json:"system"`
		Tools []struct {
			Name        string `json:"name"`
			InputSchema struct {
				Properties map[string]struct {
					Type string `json:"type"`
				} `json:"properties"`
			} `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(s.requests()[0].body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Tools) != 1 || body.Tools[0].Name != "gh" || body.Tools[0].InputSchema.Properties["args"].Type != "array" {
		t.Errorf("the call offers tools %+v, want gh taking args, an array", body.Tools)
	}
	if len(body.System) == 0 || !strings.Contains(body.System[0].Text, "`gh`") {
		t.Errorf("the system prompt %+v does not name gh", body.System)
	}
}

const openaiHistoryOne = "gAAAAhist+/one=="
const openaiHistoryTwo = "gAAAAhist+/two=="

// openaiConversation is a conversation on OpenAI Responses: a request, a turn of reasoning and a
// tool call, its result, and a turn of reasoning and an answer, each turn's native items as the
// session holds them.
func openaiConversation() conversation.Conversation {
	read := `{"id":"fc_h1","type":"function_call","status":"completed","call_id":"call_h1","name":"read","arguments":"{\"path\":\"note.txt\"}"}`
	native := func(items ...string) *conversation.Native {
		raw := make([]json.RawMessage, len(items))
		for i, item := range items {
			raw[i] = json.RawMessage(item)
		}
		return &conversation.Native{Kind: conversation.OpenAIResponsesHistory, Provider: "openai", Incremental: true, Items: raw}
	}
	return conversation.Conversation{
		Model: "openai/gpt-6.1-sol", API: openaiResponses, ThinkingLevel: "high",
		Messages: []conversation.Message{
			{Role: conversation.User, Content: []conversation.Block{{Type: conversation.Text, Text: "Read note.txt."}}},
			{
				Role: conversation.Assistant, Provider: "openai", API: openaiResponses, Model: "gpt-6.1-sol",
				Native:  native(openaiReasoning("rs_h1", openaiHistoryOne, "Reading the note"), read),
				Content: []conversation.Block{{Type: conversation.ToolCall, ToolCallID: "call_h1", ToolName: "read", Arguments: json.RawMessage(`{"path":"note.txt"}`)}},
			},
			{Role: conversation.Tool, Content: []conversation.Block{{Type: conversation.ToolResult, ToolCallID: "call_h1", ToolName: "read", Content: []conversation.Block{{Type: conversation.Text, Text: "plum"}}}}},
			{
				Role: conversation.Assistant, Provider: "openai", API: openaiResponses, Model: "gpt-6.1-sol",
				Native:  native(openaiReasoning("rs_h2", openaiHistoryTwo, "Answering"), openaiMessage("msg_h2", "It says plum.")),
				Content: []conversation.Block{{Type: conversation.Text, Text: "It says plum."}},
			},
		},
	}
}

func openaiRequest(t *testing.T, s *standIn, gh GH, modelCalls int) Request {
	t.Helper()
	_, credential := keyFile(t, "sk-openai-standin")
	return Request{
		Conversation: openaiConversation(),
		Agent:        Agent{Role: "implementer", Issue: "INFRA-2", Session: "s-impl", Phase: "testing"},
		ChangeNote:   "Dispatch status: in review.",
		Question:     Question{ID: "envoy:k-1", Text: "What did the note say?", Asker: "the tester", Where: "an Envoy message"},
		Providers:    map[config.AnswerProvider]config.AnswerEndpoint{config.AnswerOpenAI: {BaseURL: s.server.URL + "/v1", Credential: credential}},
		Prices:       map[string]config.AnswerPrice{"openai/gpt-6.1-sol": openaiPrice},
		Bounds:       testBounds(modelCalls),
		GH:           gh,
		Log:          slog.New(slog.DiscardHandler),
	}
}

// input is an OpenAI Responses request's input items.
func input(t *testing.T, r recorded) []map[string]any {
	t.Helper()
	var body struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(r.body, &body); err != nil {
		t.Fatal(err)
	}
	return body.Input
}

// An OpenAI Responses answer sends every reasoning item of the conversation with its id and
// encrypted content as the session stored them, sends messages and calls without the item ids and
// statuses Oh My Pi's replay drops, and gives the append each turn's own output items byte for
// byte, which is what the appender writes the turn from.
func TestAnOpenAIAnswerReplaysTheSessionsNativeItemsAndAppendsTheCallsOwn(t *testing.T) {
	stepOne := []string{openaiReasoning("rs_s1", "gAAAAstep+/one==", "Checking the PR"), openaiGH("fc_s1", "call_s1", "pr", "view", "7")}
	stepTwo := []string{openaiMessage("msg_s2", `{"kind":"answered","text":"It said plum.","needs":""}`)}
	s := newStandIn(t,
		openaiStream(openaiUsage{Input: 1000, CacheWrite: 900, Output: 50, Reasoning: 20}, "", stepOne...),
		openaiStream(openaiUsage{Input: 1100, Cached: 900, Output: 30, Reasoning: 0}, "", stepTwo...),
	)
	gh := &stubGH{}
	reply, err := Answer(context.Background(), openaiRequest(t, s, gh, 2))
	if err != nil {
		t.Fatal(err)
	}
	if reply.Fault != "" || reply.Kind != Answered || reply.Text != "It said plum." {
		t.Fatalf("reply %+v", reply)
	}
	got := s.requests()
	if len(got) != 2 {
		t.Fatalf("%d model calls, want 2", len(got))
	}
	for i, r := range got {
		if r.path != "/v1/responses" {
			t.Errorf("call %d went to %s", i+1, r.path)
		}
		for _, encrypted := range []string{openaiHistoryOne, openaiHistoryTwo} {
			if !bytes.Contains(r.body, []byte(`"encrypted_content":`+quote(encrypted))) {
				t.Errorf("call %d does not carry %s byte for byte", i+1, encrypted)
			}
		}
		var ids []string
		for _, item := range input(t, r) {
			switch item["type"] {
			case "reasoning":
				ids = append(ids, item["id"].(string))
			case "function_call", "message":
				if _, ok := item["id"]; ok {
					t.Errorf("call %d sends %s with its item id: %v", i+1, item["type"], item)
				}
				if _, ok := item["status"]; ok {
					t.Errorf("call %d sends %s with its status: %v", i+1, item["type"], item)
				}
			}
		}
		wantIDs := []string{"rs_h1", "rs_h2"}
		if i == 1 {
			wantIDs = append(wantIDs, "rs_s1")
		}
		if !reflect.DeepEqual(ids, wantIDs) {
			t.Errorf("call %d's reasoning items %v, want %v", i+1, ids, wantIDs)
		}
	}

	turns := reply.Append.Turns
	if len(turns) != 2 {
		t.Fatalf("%d turns", len(turns))
	}
	for i, want := range [][]string{stepOne, stepTwo} {
		var native []string
		for _, item := range turns[i].Native {
			native = append(native, string(item))
		}
		if !reflect.DeepEqual(native, want) {
			t.Errorf("turn %d's output items\n%v\nwant them as the API sent them\n%v", i+1, native, want)
		}
	}
	if call := turns[0].Content[len(turns[0].Content)-1]; call.Type != conversation.ToolCall || call.ToolCallID != "call_s1" {
		t.Errorf("the first turn's call %+v", call)
	}
}

// Every call goes to the base URL `answers.providers` names, never one the environment names, and
// carries the key that provider's key file holds when the call is made.
func TestEveryCallCarriesTheConfiguredEndpointAndTheKeyFilesKey(t *testing.T) {
	elsewhere := newStandIn(t)
	t.Setenv("OPENAI_BASE_URL", elsewhere.server.URL+"/v1")
	t.Setenv("ANTHROPIC_BASE_URL", elsewhere.server.URL)
	t.Setenv("OPENAI_API_KEY", "sk-from-the-environment")

	openaiEndpoint := newStandIn(t, openaiStream(openaiUsage{Input: 1, Output: 1}, "", openaiMessage("m", `{"kind":"answered","text":"Plum.","needs":""}`)))
	if reply, err := Answer(context.Background(), openaiRequest(t, openaiEndpoint, &stubGH{}, 1)); err != nil || reply.Kind != Answered {
		t.Fatalf("openai reply %+v, %v", reply, err)
	}
	if got := openaiEndpoint.requests()[0].header.Get("Authorization"); got != "Bearer sk-openai-standin" {
		t.Errorf("the OpenAI call's Authorization %q, want the configured key's", got)
	}

	s := newStandIn(t,
		anthropicStream("tool_use", anthropicUsage{Input: 1, Output: 1}, anthropicGH("toolu_1", "pr", "view", "7")),
		anthropicStream("end_turn", anthropicUsage{Input: 1, Output: 1}, anthropicText(`{"kind":"answered","text":"Yes.","needs":""}`)),
	)
	gh := &stubGH{}
	req := anthropicRequest(t, s, gh, 2)
	path := req.Providers[config.AnswerAnthropic].Credential.File
	gh.onCall = func(int) {
		if err := os.WriteFile(path, []byte("sk-ant-rotated\n"), 0o600); err != nil {
			t.Error(err)
		}
	}
	if reply, err := Answer(context.Background(), req); err != nil || reply.Kind != Answered {
		t.Fatalf("anthropic reply %+v, %v", reply, err)
	}
	var keys []string
	for _, r := range s.requests() {
		keys = append(keys, r.header.Get("X-Api-Key"))
	}
	if !reflect.DeepEqual(keys, []string{"sk-ant-standin", "sk-ant-rotated"}) {
		t.Errorf("the calls carried keys %v, want the key file's at each call", keys)
	}
	if n := len(elsewhere.requests()); n != 0 {
		t.Errorf("the environment's base URL got %d requests", n)
	}
}

// Each way a run ends short of an answer is no answer under the daemon's own name for it, which
// is the reason the controller reads and the daemon's reply to the asker names, and nothing is
// appended.
func TestEveryAgentloopFaultIsNoAnswerUnderTheDaemonsOwnName(t *testing.T) {
	answered := `{"kind":"answered","text":"Yes.","needs":""}`
	for _, c := range []struct {
		name    string
		replies []reply
		openai  bool
		answer  time.Duration
		want    Fault
	}{
		{name: "cut off", replies: []reply{anthropicStream("max_tokens", anthropicUsage{Output: 4000}, anthropicText(`{"kind":"answered","text":"Ye`))}, want: Length},
		{name: "refused", replies: []reply{anthropicStream("refusal", anthropicUsage{Output: 1}, anthropicText(""))}, want: Refusal},
		{name: "OpenAI refusal", openai: true, replies: []reply{openaiStream(openaiUsage{Output: 1}, "",
			`{"id":"msg_r","type":"message","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"I can't help with that."}]}`)}, want: Refusal},
		{name: "OpenAI cut off", openai: true, replies: []reply{openaiStream(openaiUsage{Output: 4000}, "max_output_tokens", openaiMessage("msg_l", `{"kind":"answered","text":"Ye`))}, want: Length},
		{name: "prose", replies: []reply{anthropicStream("end_turn", anthropicUsage{Output: 1}, anthropicText("PR 7 is green."))}, want: Malformed},
		{name: "fenced", replies: []reply{anthropicStream("end_turn", anthropicUsage{Output: 1}, anthropicText("```json\n"+answered+"\n```"))}, want: Malformed},
		{name: "two objects", replies: []reply{anthropicStream("end_turn", anthropicUsage{Output: 1}, anthropicText(answered+answered))}, want: Malformed},
		{name: "a field the form does not hold", replies: []reply{anthropicStream("end_turn", anthropicUsage{Output: 1}, anthropicText(`{"kind":"answered","text":"Yes.","needs":"","aside":"and a pear"}`))}, want: Malformed},
		{name: "unknown kind", replies: []reply{anthropicStream("end_turn", anthropicUsage{Output: 1}, anthropicText(`{"kind":"maybe","text":"Yes.","needs":""}`))}, want: Malformed},
		{name: "none not offered", replies: []reply{anthropicStream("end_turn", anthropicUsage{Output: 1}, anthropicText(`{"kind":"none","text":"","needs":""}`))}, want: Malformed},
		{name: "not answered naming no need", replies: []reply{anthropicStream("end_turn", anthropicUsage{Output: 1}, anthropicText(`{"kind":"not_answered","text":"I cannot tell.","needs":""}`))}, want: Malformed},
		{name: "answered with no text", replies: []reply{anthropicStream("end_turn", anthropicUsage{Output: 1}, anthropicText(`{"kind":"answered","text":"  ","needs":""}`))}, want: Malformed},
		{name: "the provider failed", replies: []reply{{status: http.StatusBadRequest, body: []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`)}}, want: ProviderError},
		{name: "past the time bound", replies: []reply{{delay: 5 * time.Second, body: anthropicStream("end_turn", anthropicUsage{}, anthropicText(answered)).body}}, answer: time.Second, want: AnswerSeconds},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newStandIn(t, c.replies...)
			req := anthropicRequest(t, s, &stubGH{}, 1)
			if c.openai {
				req = openaiRequest(t, s, &stubGH{}, 1)
			}
			if c.answer > 0 {
				req.Bounds.Answer = c.answer
			}
			got, err := Answer(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if got.Fault != c.want || got.Kind != NotAnswered || got.Text != "" || len(got.Append.Turns) != 0 {
				t.Errorf("reply %+v, want no answer with fault %s", got, c.want)
			}
		})
	}
}

// The last allowed call still calling a tool is no answer past the model_calls bound.
func TestTheLastCallStillCallingAToolIsPastTheModelCallsBound(t *testing.T) {
	s := newStandIn(t, anthropicStream("tool_use", anthropicUsage{Output: 1}, anthropicGH("toolu_1", "pr", "view", "7")))
	got, err := Answer(context.Background(), anthropicRequest(t, s, &stubGH{}, 1))
	if err != nil || got.Fault != ModelCalls || got.Calls != 1 {
		t.Errorf("reply %+v, %v; want no answer past model_calls after 1 call", got, err)
	}
}

// A conversation on a provider the configuration does not name, on a model it does not price, or
// holding history the loop cannot send is no answer, and no model call is made.
func TestNoProviderNoPriceAndAnUnknownFormatMakeNoModelCall(t *testing.T) {
	s := newStandIn(t)
	for _, c := range []struct {
		name   string
		change func(*Request)
		want   Fault
	}{
		{name: "provider not configured", change: func(r *Request) { delete(r.Providers, config.AnswerAnthropic) }, want: NoProvider},
		{name: "API the loop does not call", change: func(r *Request) { r.Conversation.API = "openai-completions" }, want: NoProvider},
		{name: "model not priced", change: func(r *Request) { r.Conversation.Model = "anthropic/claude-unpriced" }, want: NoPrice},
		{name: "provider-side compaction", change: func(r *Request) {
			r.Conversation.Messages[0].Native = &conversation.Native{Kind: conversation.AnthropicCompaction, Provider: "anthropic"}
		}, want: UnknownFormat},
	} {
		t.Run(c.name, func(t *testing.T) {
			req := anthropicRequest(t, s, &stubGH{}, 1)
			c.change(&req)
			got, err := Answer(context.Background(), req)
			if err != nil || got.Fault != c.want || got.Kind != NotAnswered || got.Calls != 0 {
				t.Errorf("reply %+v, %v; want no answer with %s", got, err, c.want)
			}
		})
	}
	if n := len(s.requests()); n != 0 {
		t.Errorf("%d model calls were made", n)
	}
}

// None is offered only for a message that may ask nothing, in the prompt and in the reply form
// the model is held to, and is then a reply.
func TestNoneIsOfferedOnlyForAMessageThatMayAskNothing(t *testing.T) {
	s := newStandIn(t, anthropicStream("end_turn", anthropicUsage{Output: 1}, anthropicText(`{"kind":"none","text":"","needs":""}`)))
	req := anthropicRequest(t, s, &stubGH{}, 1)
	req.MayAskNothing = true
	got, err := Answer(context.Background(), req)
	if err != nil || got.Kind != None || got.Fault != "" {
		t.Fatalf("reply %+v, %v; want none", got, err)
	}
	var body struct {
		System []struct {
			Text string `json:"text"`
		} `json:"system"`
	}
	if err := json.Unmarshal(s.requests()[0].body, &body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body.System[0].Text, `"none"`) {
		t.Errorf("the system prompt offers no none: %s", body.System[0].Text)
	}
	if !bytes.Contains(s.requests()[0].body, []byte(`\"none\"`)) && !bytes.Contains(s.requests()[0].body, []byte(`"none"`)) {
		t.Error("the call carries no reply form offering none")
	}
	if strings.Contains(systemPrompt(anthropicRequest(t, s, &stubGH{}, 1)), `"none"`) {
		t.Error("the system prompt offers none to a message that asks something")
	}
	if strings.Contains(string(finalSchema(false)), `"none"`) {
		t.Errorf("the reply form of a message that asks something offers none: %s", finalSchema(false))
	}
}

// On a re-run the question carries the gh calls already made with their results, a call with no
// result of unknown outcome, and new calls are numbered after them.
func TestARerunCarriesTheEarlierCallsAndNumbersNewCallsAfterThem(t *testing.T) {
	s := newStandIn(t,
		anthropicStream("tool_use", anthropicUsage{Output: 1}, anthropicGH("toolu_1", "pr", "view", "7")),
		anthropicStream("end_turn", anthropicUsage{Output: 1}, anthropicText(`{"kind":"answered","text":"Done.","needs":""}`)),
	)
	gh := &stubGH{}
	req := anthropicRequest(t, s, gh, 2)
	output, exit := "comment posted", 0
	req.Earlier = []store.AnswerToolCall{
		{Seq: 1, Args: []string{"pr", "comment", "7", "--body", "on it"}, Result: &output, Exit: &exit},
		{Seq: 2, Args: []string{"pr", "review", "7", "--approve"}},
	}
	if got, err := Answer(context.Background(), req); err != nil || got.Kind != Answered {
		t.Fatalf("reply %+v, %v", got, err)
	}
	question := messages(t, s.requests()[0])[2]
	text := blocks(question)[0]["text"].(string)
	for _, want := range []string{`1. gh ["pr","comment","7","--body","on it"]`, "Exit 0:\ncomment posted", `2. gh ["pr","review","7","--approve"]`, "Outcome unknown"} {
		if !strings.Contains(text, want) {
			t.Errorf("the question does not hold %q:\n%s", want, text)
		}
	}
	if calls := gh.made(); len(calls) != 1 || calls[0].seq != 3 {
		t.Errorf("the new calls %+v, want one numbered 3", calls)
	}
}

// A gh call that failed or never ran goes back to the model as an error result, with gh's exit
// status, and the append records it as the call's result.
func TestAFailedGhCallGoesBackAsAnErrorResult(t *testing.T) {
	s := newStandIn(t,
		anthropicStream("tool_use", anthropicUsage{Output: 1}, anthropicGH("toolu_1", "pr", "view", "7")),
		anthropicStream("end_turn", anthropicUsage{Output: 1}, anthropicText(`{"kind":"answered","text":"Done.","needs":""}`)),
	)
	req := anthropicRequest(t, s, failingGH{}, 2)
	got, err := Answer(context.Background(), req)
	if err != nil || got.Kind != Answered {
		t.Fatalf("reply %+v, %v", got, err)
	}
	result := got.Append.Turns[0].Results[0]
	if !result.IsError || !strings.Contains(result.Content[0].Text, "gh exited 1") {
		t.Errorf("the appended result %+v, want an error result naming the exit status", result)
	}
	sent := blocks(messages(t, s.requests()[1])[4])
	if len(sent) != 1 || sent[0]["is_error"] != true {
		t.Fatalf("the result the model was given %v, want one error result", sent)
	}
	if text, _ := json.Marshal(sent[0]["content"]); !strings.Contains(string(text), "gh exited 1") {
		t.Errorf("the result the model was given %s, want it naming the exit status", text)
	}
}

// failingGH is a gh tool whose call runs and fails.
type failingGH struct{}

func (failingGH) Run(context.Context, int, string) ghtool.Result {
	return ghtool.Result{Output: "could not resolve to a PullRequest", Exit: 1, Ran: true}
}
