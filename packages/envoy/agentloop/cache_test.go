package agentloop

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

// countKey counts the objects holding key anywhere in v.
func countKey(v any, key string) int {
	n := 0
	switch v := v.(type) {
	case map[string]any:
		if _, ok := v[key]; ok {
			n++
		}
		for _, child := range v {
			n += countKey(child, key)
		}
	case []any:
		for _, child := range v {
			n += countKey(child, key)
		}
	}
	return n
}

// marked are the indexes of the messages whose last block carries a cache mark.
func marked(t *testing.T, r recorded) []int {
	t.Helper()
	var out []int
	for i, m := range messages(t, r) {
		b := blocks(m)
		if len(b) > 0 && b[len(b)-1]["cache_control"] != nil {
			out = append(out, i)
		}
	}
	return out
}

// Across a three-call Anthropic run of a request that sets no cache field every request carries
// two cache marks, never above four: one on the end of the replayed history, kept on every
// call, and one on the newest message, moved there from the one before; the last call offers no
// tool; the model's effort and adaptive, summarized thinking reach every call; the history's
// signed thinking goes as the caller handed it over, the first call's thinking goes back with
// its signature in the second, and the tool is given both calls in the order the model asked
// for them. A request that sets no System prompt is marked the same way: the SDK then puts no
// system message before a step's own messages, which is what afterHistory counts.
func TestAnthropicMarksTheCacheMovesTheNewestMarkAndForcesTheReplyOnTheLastCall(t *testing.T) {
	t.Parallel()
	// The wire's messages: the history's request and model turn (0, 1), the new question
	// (2), then each call's turn and the tool result answering it. A system prompt rides
	// beside them rather than among them, so both cases expect the same indexes.
	for _, c := range []struct {
		name      string
		system    string
		wantMarks [][]int
	}{
		{"with a system prompt", "You answer the question with the tools you are given.", [][]int{{1, 2}, {1, 4}, {1, 6}}},
		{"with no system prompt", "", [][]int{{1, 2}, {1, 4}, {1, 6}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := newStandIn(t, threeCallAnthropic()...)
			f := newFixture(t, s, AnthropicMessages, 3)
			f.req.System = c.system
			f.req.History = anthropicHistory()
			result, err := Run(context.Background(), f.req)
			if err != nil {
				t.Fatal(err)
			}
			if result.Fault != "" || result.Text != "The note says plum." {
				t.Fatalf("result %+v, want the answer and no fault", result)
			}
			got := s.requests()
			if len(got) != 3 {
				t.Fatalf("%d model calls, want 3", len(got))
			}
			for i, r := range got {
				body := r.decoded(t)
				if n := countKey(body, "cache_control"); n > 4 || n != 2 {
					t.Errorf("call %d carries %d cache marks, want 2 (never above 4)", i+1, n)
				}
				if marks := marked(t, r); !reflect.DeepEqual(marks, c.wantMarks[i]) {
					t.Errorf("call %d marks messages %v, want %v (the history's end and the newest message)", i+1, marks, c.wantMarks[i])
				}
				choice, _ := body["tool_choice"].(map[string]any)
				if last := i == 2; (choice["type"] == "none") != last {
					t.Errorf("call %d's tool_choice %v; want none on the last call only", i+1, body["tool_choice"])
				}
				if thinking := body["thinking"]; !reflect.DeepEqual(thinking, map[string]any{"type": "adaptive", "display": "summarized"}) {
					t.Errorf("call %d's thinking %v, want adaptive and summarized", i+1, thinking)
				}
				if config, _ := body["output_config"].(map[string]any); config["effort"] != "high" {
					t.Errorf("call %d's output_config %v, want effort high, the model's", i+1, body["output_config"])
				}
				if !bytes.Contains(r.body, []byte(`"signature":"`+historySignature+`"`)) {
					t.Errorf("call %d does not carry the history's signature byte for byte", i+1)
				}
			}
			history := blocks(messages(t, got[0])[1])
			if history[0]["type"] != "thinking" || history[0]["signature"] != historySignature || history[0]["thinking"] != "The plan needs a migration." {
				t.Errorf("the history's thinking went as %v", history[0])
			}
			stepOne := blocks(messages(t, got[1])[3])
			if stepOne[0]["type"] != "thinking" || stepOne[0]["signature"] != "sig-step-1+/==" || stepOne[0]["thinking"] != "Read the note." {
				t.Errorf("the second call carries the first call's turn as %v, want its thinking with its signature", stepOne)
			}
			want := []seen{{1, "toolu_1", `{"path":"note.txt"}`}, {2, "toolu_2", `{"path":"other.txt"}`}}
			if calls := madeSeen(f.tool.made()); !reflect.DeepEqual(calls, want) {
				t.Errorf("the tool was given %+v, want %+v", calls, want)
			}
		})
	}
}

// anthropicCarriedOn is a conversation on Anthropic that already ends in the question to
// answer, as the history of a caller that carries every turn it has and asks for nothing new
// does.
func anthropicCarriedOn() []Message {
	return append(anthropicHistory(), Message{Role: User, Blocks: []Block{{Kind: TextBlock, Text: "What does note.txt say?"}}})
}

// A run whose Input is empty keeps the history's cache mark on every call: the newest message
// of the first call is the history's own last message, which carries the mark the run put at
// the history's end, and the moved newest-message mark lands on the turns the run itself adds.
// A later call takes no mark off a message of the history. A request that sets no System prompt
// is marked the same way: the SDK then puts no system message before a step's own messages,
// which is what afterHistory counts.
func TestAnthropicKeepsTheHistorysMarkWhenTheInputIsEmpty(t *testing.T) {
	t.Parallel()
	// The wire's messages: the history's request, model turn and question (0, 1, 2), the
	// last of which is the newest message of the first call, then each call's turn and the
	// tool result answering it. A system prompt rides beside them rather than among them,
	// so both cases expect the same indexes.
	for _, c := range []struct {
		name      string
		system    string
		wantMarks [][]int
	}{
		{"with a system prompt", "You answer the question with the tools you are given.", [][]int{{2}, {2, 4}, {2, 6}}},
		{"with no system prompt", "", [][]int{{2}, {2, 4}, {2, 6}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := newStandIn(t, threeCallAnthropic()...)
			f := newFixture(t, s, AnthropicMessages, 3)
			f.req.System = c.system
			f.req.History = anthropicCarriedOn()
			f.req.Input = nil
			result, err := Run(context.Background(), f.req)
			if err != nil {
				t.Fatal(err)
			}
			if result.Fault != "" || result.Text != "The note says plum." {
				t.Fatalf("result %+v, want the answer and no fault", result)
			}
			got := s.requests()
			if len(got) != 3 {
				t.Fatalf("%d model calls, want 3", len(got))
			}
			for i, r := range got {
				// The whole request carries exactly the marks the messages below
				// are expected to show, and never more than the four Anthropic
				// allows: marked reads each message's last block alone, so a mark
				// anywhere else is counted only here.
				if n := countKey(r.decoded(t), "cache_control"); n != len(c.wantMarks[i]) {
					t.Errorf("call %d carries %d cache marks, want %d", i+1, n, len(c.wantMarks[i]))
				}
				if marks := marked(t, r); !reflect.DeepEqual(marks, c.wantMarks[i]) {
					t.Errorf("call %d marks messages %v, want %v (the history's end and the newest message)", i+1, marks, c.wantMarks[i])
				}
			}
		})
	}
}

// anthropicCutOffHistory is a conversation on Anthropic whose model turn ends in thinking, as a
// turn the provider cut off at its limit does: nothing follows the thinking block.
func anthropicCutOffHistory() []Message {
	return []Message{
		{Role: User, Blocks: []Block{{Kind: TextBlock, Text: "Write the plan."}}},
		{Role: Assistant, Blocks: []Block{
			{Kind: TextBlock, Text: "Working on it."},
			{Kind: ThinkingBlock, Text: "The plan needs a migration.", Signature: historySignature},
		}},
	}
}

// A history whose last turn ends in thinking still has its end marked for the cache: Anthropic
// puts a message's mark on that message's last block alone and drops it on a thinking block, so
// the mark goes on the last message of the history that can carry one.
func TestAHistoryEndingInThinkingIsStillMarked(t *testing.T) {
	t.Parallel()
	s := newStandIn(t, anthropicStream("end_turn", anthropicUsage{Input: 10, Output: 5}, anthropicText("The note says plum.")))
	f := newFixture(t, s, AnthropicMessages, 1)
	f.req.History = anthropicCutOffHistory()
	result, err := Run(context.Background(), f.req)
	if err != nil || result.Fault != "" {
		t.Fatalf("result %+v, %v", result, err)
	}
	got := s.requests()
	if len(got) != 1 {
		t.Fatalf("%d model calls, want 1", len(got))
	}
	if n := countKey(got[0].decoded(t), "cache_control"); n != 2 {
		t.Errorf("the call carries %d cache marks, want 2", n)
	}
	// The wire's messages: the history's request (0) and its cut-off model turn (1), then
	// the new question (2), which is the newest message. The model turn ends in thinking and
	// cannot carry a mark, so the history's goes on the request before it.
	if marks := marked(t, got[0]); !reflect.DeepEqual(marks, []int{0, 2}) {
		t.Errorf("the call marks messages %v, want [0 2] (the history's last message that can carry a mark and the newest message)", marks)
	}
}

// A history whose last tool result holds an image still carries the explicit breakpoint: the
// openai provider reads a content output's breakpoint from each content value's own options and
// never from the tool result part's, so the mark goes on the last of the result's content
// values, which is where the provider reads it.
func TestABreakpointOnAToolResultHoldingAnImageReachesTheWire(t *testing.T) {
	t.Parallel()
	s := newStandIn(t, openaiStream(openaiUsage{Input: 1000, Cached: 900, Output: 20}, "", openaiMessage("msg_1", "The diagram says plum.")))
	f := newFixture(t, s, OpenAIResponses, 1)
	f.req.History = []Message{
		{Role: User, Blocks: []Block{{Kind: TextBlock, Text: "Read diagram.gif."}}},
		{Role: Assistant, Blocks: []Block{
			{Kind: ReasoningBlock, Text: "Reading the diagram", ItemID: "rs_h1", Encrypted: openaiHistoryOne},
			{Kind: ToolCallBlock, ToolCallID: "call_h1", Name: toolName, Input: json.RawMessage(`{"path":"diagram.gif"}`)},
		}},
		{Role: ToolRole, Blocks: []Block{{
			Kind: ToolResultBlock, ToolCallID: "call_h1", Name: toolName,
			Content: []Block{
				{Kind: TextBlock, Text: "the diagram"},
				{Kind: ImageBlock, MediaType: "image/gif", Data: diagramData},
			},
		}}},
	}
	result, err := Run(context.Background(), f.req)
	if err != nil || result.Fault != "" {
		t.Fatalf("result %+v, %v", result, err)
	}
	got := s.requests()
	if len(got) != 1 {
		t.Fatalf("%d model calls, want 1", len(got))
	}
	if n := countKey(got[0].decoded(t), "prompt_cache_breakpoint"); n != 1 {
		t.Errorf("the call carries %d prompt cache breakpoints, want 1", n)
	}
	var values []map[string]any
	for _, item := range inputItems(t, got[0]) {
		if item["type"] != "function_call_output" {
			continue
		}
		output, _ := item["output"].([]any)
		for _, value := range output {
			values = append(values, value.(map[string]any))
		}
	}
	if len(values) != 2 {
		t.Fatalf("the tool result's content values are %v, want the text and the image", values)
	}
	if last := values[1]; last["type"] != "input_image" || last["prompt_cache_breakpoint"] == nil {
		t.Errorf("the tool result's last content value is %v, want the image carrying the breakpoint", last)
	}
}

// cacheCall makes a one-call run on kind with that protocol's replayed history, opting out of
// the cache, and is the request the provider was given.
func cacheCall(t *testing.T, kind Kind) recorded {
	t.Helper()
	var s *standIn
	var history []Message
	if kind == OpenAIResponses {
		s = newStandIn(t, openaiStream(openaiUsage{Input: 1000, Cached: 900, Output: 20}, "", openaiMessage("msg_1", "It said plum.")))
		history = openaiHistory()
	} else {
		s = newStandIn(t, anthropicStream("end_turn", anthropicUsage{Input: 10, Output: 5}, anthropicText("The note says plum.")))
		history = anthropicHistory()
	}
	f := newFixture(t, s, kind, 1)
	f.req.History = history
	f.req.NoCache = true
	result, err := Run(context.Background(), f.req)
	if err != nil || result.Fault != "" {
		t.Fatalf("result %+v, %v", result, err)
	}
	got := s.requests()
	if len(got) != 1 {
		t.Fatalf("%d model calls, want 1", len(got))
	}
	return got[0]
}

// A request that sets NoCache carries none of the marks this package places: on the kinds
// Anthropic serves no cache_control, at the history's end or on the newest message, and on
// openai-responses no explicit prompt cache breakpoint. Each kind is asked for its own
// protocol's mark, the only one the package would ever put on it.
func TestOptingOutOfTheCacheSendsNoMark(t *testing.T) {
	t.Parallel()
	for _, kind := range []Kind{AnthropicMessages, AzureAnthropic, OpenAIResponses} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			key := "cache_control"
			if kind == OpenAIResponses {
				key = "prompt_cache_breakpoint"
			}
			if n := countKey(cacheCall(t, kind).decoded(t), key); n != 0 {
				t.Errorf("the call carries %d %s marks, want none", n, key)
			}
		})
	}
}
