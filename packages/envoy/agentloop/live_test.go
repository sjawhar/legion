//go:build modellive

// This file is the package's live proof: one real model call on each protocol Run serves, and one
// real decision, against a model gateway an operator names. It sits behind the modellive build
// tag, so no ordinary test run and no CI job makes a model call; CI vets this file and never runs
// it.
//
// The operator names the gateway in the environment, and every test here skips unless each of its
// own inputs is set, since nothing is compiled in: a base URL is a deployment's address and a
// model id is a deployment's name, and neither belongs in this repository.
//
//	AGENTLOOP_LIVE_BASE_URL             the gateway's base URL, the root its API paths hang off
//	AGENTLOOP_LIVE_KEY_FILE             a file holding the gateway's key, read on every attempt
//	AGENTLOOP_LIVE_MODEL                the model id the gateway knows the model by
//	AGENTLOOP_LIVE_SYSTEMONE_BASE_URL   the System One API's base URL, for the decision
//	AGENTLOOP_LIVE_SYSTEMONE_KEY_FILE   a file holding that API's key
//	AGENTLOOP_LIVE_SYSTEMONE_MODEL      the model id that API knows its model by
//
// The key is never in an environment variable of the test's own: the endpoint's Token reads the
// named file on every attempt, as a caller holding a rotated key does.
//
// Run the proof with:
//
//	go test -count=1 -tags modellive ./agentloop/ -run TestLive -v
//
// What the run prints is the evidence a person reads, so each test logs the reply it got, how many
// model calls it took, what it consumed and what it cost. None of them logs the key, the gateway's
// address or the model id.

package agentloop

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// The environment the operator names the gateway and the System One API in.
const (
	liveBaseURLEnv = "AGENTLOOP_LIVE_BASE_URL"
	liveKeyFileEnv = "AGENTLOOP_LIVE_KEY_FILE"
	liveModelEnv   = "AGENTLOOP_LIVE_MODEL"

	liveDecideBaseURLEnv = "AGENTLOOP_LIVE_SYSTEMONE_BASE_URL"
	liveDecideKeyFileEnv = "AGENTLOOP_LIVE_SYSTEMONE_KEY_FILE"
	liveDecideModelEnv   = "AGENTLOOP_LIVE_SYSTEMONE_MODEL"
)

// The item the live proof asks about and the two facts only its tool holds. No model knows this
// item's stock or its aisle, so an answer carrying both is an answer the model called the tool
// for, and an answer that did not call the tool is wrong.
const (
	liveItem  = "left-handed brass grommet"
	liveStock = 1374
	liveAisle = "C7"
)

// liveFinal is the object the live reply must be: the item asked about, what the tool said is in
// stock and where the tool said it is.
const liveFinal = `{"type":"object","properties":{"item":{"type":"string"},"count":{"type":"integer"},"aisle":{"type":"string"}},"required":["item","count","aisle"],"additionalProperties":false}`

// liveAnswer is that object read back.
type liveAnswer struct {
	Item  string `json:"item"`
	Count int    `json:"count"`
	Aisle string `json:"aisle"`
}

// livePrice is a price of the caller's own, in USD per million tokens, so that the cost the proof
// reports is computed rather than zero. It is no gateway's tariff and is not meant to be one.
var livePrice = Price{Input: 3, Output: 15, CacheWrite: 3.75, CacheRead: 0.3}

// A live anthropic-messages run calls the tool it is given and answers with the object it is
// asked for, carrying the fact only that tool holds.
func TestLiveAnthropicMessages(t *testing.T) { liveProof(t, AnthropicMessages) }

// A live openai-responses run does the same on the Responses API.
func TestLiveOpenAIResponses(t *testing.T) { liveProof(t, OpenAIResponses) }

// A live openai-chat run does the same on the Chat Completions API.
func TestLiveOpenAIChat(t *testing.T) { liveProof(t, OpenAIChat) }

// liveProof is one real run of the loop on kind: the model is given one tool holding a fact it
// cannot know and is asked for an object carrying that fact, so the run proves the whole path,
// the tool call and the final object included, rather than that a request was accepted.
func liveProof(t *testing.T, kind Kind) {
	t.Helper()
	base, keyFile, id := liveInputs(t, liveBaseURLEnv, liveKeyFileEnv, liveModelEnv)
	tool := &liveInventory{}
	req := Request{
		Model: Model{
			Endpoint: Endpoint{Kind: kind, BaseURL: liveBaseURL(base, kind), Token: fileToken(keyFile)},
			ID:       id, Price: livePrice,
		},
		System: "You answer questions about what a warehouse holds. Call the inventory tool for the item the question names, and then answer with the object you are asked for, carrying the count and the aisle the tool gave you. Never guess either.",
		Input:  []Message{{Role: User, Blocks: []Block{{Kind: TextBlock, Text: "How many " + liveItem + "s are in stock, and in which aisle are they?"}}}},
		Tools:  []Tool{tool.register()},
		Bounds: Bounds{Time: 2 * time.Minute, Calls: 3, OutputTokens: 2000},
		Final:  json.RawMessage(liveFinal),
	}

	result, err := Run(context.Background(), req)
	if err != nil {
		t.Fatalf("the run: %v", err)
	}
	if result.Fault != "" {
		t.Fatalf("the run ended on the fault %s: %s", result.Fault, result.Detail)
	}
	calls := tool.made()
	if len(calls) == 0 {
		t.Fatal("the model answered without calling the tool, so it cannot hold the fact the answer needs")
	}
	if calls[0].Index != 1 {
		t.Errorf("the first tool call is numbered %d, want 1, the first index of a fresh run", calls[0].Index)
	}
	var answer liveAnswer
	reply := json.NewDecoder(bytes.NewReader(result.Value))
	reply.DisallowUnknownFields()
	if err := reply.Decode(&answer); err != nil {
		t.Fatalf("the reply is no object of the form the request asked for: %v", err)
	}
	if answer.Item == "" || answer.Count != liveStock || answer.Aisle != liveAisle {
		t.Errorf("the answer is %+v, want it to name the item and to carry the tool's own %d and %s", answer, liveStock, liveAisle)
	}
	if len(result.Steps) < 2 {
		t.Errorf("the run took %d model call(s), want more than one: one to call the tool and one to answer", len(result.Steps))
	}
	if !result.Usage.Metered {
		t.Error("the run's usage is not metered, so the provider reported no token counts at all")
	}
	if result.Usage.CostUSD <= 0 {
		t.Errorf("the run cost %v, want a cost computed from the request's own price", result.Usage.CostUSD)
	}

	t.Logf("the reply: %s", result.Value)
	t.Logf("the run took %d model calls and %d tool calls", len(result.Steps), len(calls))
	t.Logf("usage: %d input, %d output, %d cache write, %d cache read, %d reasoning, metered %t",
		result.Usage.Input, result.Usage.Output, result.Usage.CacheWrite, result.Usage.CacheRead,
		result.Usage.Reasoning, result.Usage.Metered)
	t.Logf("cost: $%.6f, at the price the request carried", result.Usage.CostUSD)
}

// A live decision puts one noul question about a short state to System One and reads the answer
// back, priced at the request's own price.
func TestLiveDecide(t *testing.T) {
	base, keyFile, id := liveInputs(t, liveDecideBaseURLEnv, liveDecideKeyFileEnv, liveDecideModelEnv)
	req := DecideRequest{
		Model: Model{
			Endpoint: Endpoint{Kind: SystemOne, BaseURL: base, Token: fileToken(keyFile)},
			ID:       id, Price: livePrice,
		},
		State: json.RawMessage(fmt.Sprintf(`{"label":"%s, aisle %s, %d in stock"}`, liveItem, liveAisle, liveStock)),
		Questions: map[string]Question{
			"stocked": {
				Type:         "noul",
				Instructions: "The label says the item is in stock.",
				Criteria: json.RawMessage(
					`{"true":"the label gives a count above zero","false":"the label gives no count, or a count of zero"}`),
			},
		},
		Bounds: Bounds{Time: 2 * time.Minute},
	}

	result, err := Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("the decision: %v", err)
	}
	if result.Fault != "" {
		t.Fatalf("the decision ended on the fault %s: %s", result.Fault, result.Detail)
	}
	answer, answered := result.Answers["stocked"]
	if !answered {
		t.Fatalf("the decision answered %v, want one answer under the name the question was asked under", result.Answers)
	}
	if answer.Type != "noul" {
		t.Errorf("the answer's type is %q, want noul, the question's own", answer.Type)
	}
	if !result.Usage.Metered || result.Usage.CostUSD <= 0 {
		t.Errorf("usage %+v, want it metered and priced at the request's own price", result.Usage)
	}

	t.Logf("the answer: the label says the item is in stock, with probability %.3f", answer.Noul)
	t.Logf("usage: %d input, %d output; cost: $%.6f, at the price the request carried",
		result.Usage.Input, result.Usage.Output, result.Usage.CostUSD)
}

// liveInputs are the base URL, the key file and the model id the named variables hold, or a skip
// naming the ones that are unset, so a run with no gateway reports why it made no call. It names
// the variables and never prints what they hold.
func liveInputs(t *testing.T, baseEnv, keyFileEnv, modelEnv string) (base, keyFile, id string) {
	t.Helper()
	var unset []string
	for _, name := range []string{baseEnv, keyFileEnv, modelEnv} {
		if os.Getenv(name) == "" {
			unset = append(unset, name)
		}
	}
	if len(unset) > 0 {
		t.Skipf("no live call: %s unset", strings.Join(unset, ", "))
	}
	return os.Getenv(baseEnv), os.Getenv(keyFileEnv), os.Getenv(modelEnv)
}

// liveBaseURL is the endpoint base one protocol's own paths hang off, from the gateway's base URL:
// the Anthropic provider posts to <base>/v1/messages, while the two OpenAI providers post to
// <base>/responses and <base>/chat/completions, so those two are given the gateway's /v1. A base
// URL written with that /v1 already on it, or with a trailing slash, names the same gateway.
func liveBaseURL(base string, kind Kind) string {
	root := strings.TrimSuffix(strings.TrimRight(base, "/"), "/v1")
	if kind == AnthropicMessages {
		return root
	}
	return root + "/v1"
}

// liveInventory is the one tool the live run registers. It answers how many of the item are in
// stock and where, which is the fact the model cannot hold without calling it, and records every
// call it is given.
type liveInventory struct {
	mu    sync.Mutex
	calls []ToolCall
}

func (l *liveInventory) register() Tool {
	return Tool{
		Name:        "inventory",
		Description: "How many of an item the warehouse holds, and the aisle it is in.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"item":{"type":"string"}},"required":["item"],"additionalProperties":false}`),
		Run: func(_ context.Context, call ToolCall) (string, bool) {
			l.mu.Lock()
			l.calls = append(l.calls, call)
			l.mu.Unlock()
			return fmt.Sprintf("%d in stock, in aisle %s.", liveStock, liveAisle), false
		},
	}
}

// made are the calls the tool was given, in order.
func (l *liveInventory) made() []ToolCall {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]ToolCall(nil), l.calls...)
}
