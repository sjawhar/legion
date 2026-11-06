package agentloop

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// systemOnePrice is what the fixture's decision model charges, in USD per million tokens.
var systemOnePrice = Price{Input: 3, Output: 15}

// newDecideFixture is a decision put to s, with the one System One model and a token read from a
// file on every attempt, as the run fixture's endpoint reads one.
func newDecideFixture(t *testing.T, s *standIn) DecideRequest {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	writeToken(t, path, "the-decision-token")
	return DecideRequest{
		Model: Model{
			Endpoint: Endpoint{Kind: SystemOne, BaseURL: s.server.URL, Token: fileToken(path)},
			ID:       "systemone-standin", Price: systemOnePrice,
		},
		State:     json.RawMessage(`{"note":"The note says plum."}`),
		Questions: decideQuestions(),
		Bounds:    Bounds{Time: 30 * time.Second},
	}
}

// decideQuestions are one question of each shape the API answers.
func decideQuestions() map[string]Question {
	return map[string]Question{
		"answered":  {Type: "noul", Instructions: "The note answers the question."},
		"fruit":     {Type: "choice", Instructions: "Which fruit does the note name?", Criteria: json.RawMessage(`{"plum":"the note names a plum","pear":"the note names a pear"}`)},
		"clarity":   {Type: "score", Instructions: "How clear is the note?", Criteria: json.RawMessage(`["unreadable","muddled","clear"]`)},
		"signature": {Type: "box", Instructions: "Where is the signature?"},
	}
}

// decideAnswer is the body the API sends for the four questions, one answer of each shape.
const decideAnswer = `{
  "model": "systemone-standin",
  "answers": {
    "answered": {"type":"noul","noul":0.92},
    "fruit": {"type":"choice","choice":"plum","confidence":0.81,"probabilities":{"plum":0.81,"pear":0.19}},
    "clarity": {"type":"score","score":2.4,"confidence":0.7,"legend":{"0":"unreadable","1":"muddled","2":"clear"},"probabilities":{"0":0.05,"1":0.5,"2":0.45}},
    "signature": {"type":"box","box":{"top":0.1,"bottom":0.42,"left":0.05,"right":0.6},"confidence":0.64}
  },
  "usage": {"input_tokens": 400, "output_tokens": 60}
}`

// A decision posts the model, the state and the questions as the caller wrote them to the
// System One path, authorized with the endpoint's token as a bearer token, and reads every shape
// of answer back with its own fields, priced at the model's prices.
func TestDecideSendsTheQuestionsAndReadsEveryAnswerShape(t *testing.T) {
	t.Parallel()
	s := newStandIn(t, reply{body: []byte(decideAnswer)})
	req := newDecideFixture(t, s)
	result, err := Decide(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Fault != "" {
		t.Fatalf("result %+v, want no fault: %s", result, result.Detail)
	}
	got := s.requests()
	if len(got) != 1 {
		t.Fatalf("%d requests, want 1", len(got))
	}
	if got[0].path != "/v1/systemone" {
		t.Errorf("the decision went to %s, want /v1/systemone", got[0].path)
	}
	if auth := got[0].header.Get("Authorization"); auth != "Bearer the-decision-token" {
		t.Errorf("Authorization %q, want the request's token as a bearer token", auth)
	}
	body := got[0].decoded(t)
	if body["model"] != req.Model.ID {
		t.Errorf("the body names the model %v, want %s", body["model"], req.Model.ID)
	}
	var state, questions any
	if err := json.Unmarshal(req.State, &state); err != nil {
		t.Fatal(err)
	}
	asJSON(t, decideQuestions(), &questions)
	if !reflect.DeepEqual(body["state"], state) {
		t.Errorf("the body's state is %v, want the request's %v", body["state"], state)
	}
	if !reflect.DeepEqual(body["questions"], questions) {
		t.Errorf("the body's questions are %v, want the request's %v", body["questions"], questions)
	}

	want := map[string]Decision{
		"answered": {Type: "noul", Noul: 0.92},
		"fruit":    {Type: "choice", Choice: "plum", Confidence: 0.81, Probabilities: map[string]float64{"plum": 0.81, "pear": 0.19}},
		"clarity": {
			Type: "score", Score: json.RawMessage("2.4"), Confidence: 0.7,
			Legend:        json.RawMessage(`{"0":"unreadable","1":"muddled","2":"clear"}`),
			Probabilities: map[string]float64{"0": 0.05, "1": 0.5, "2": 0.45},
		},
		"signature": {Type: "box", Box: json.RawMessage(`{"top":0.1,"bottom":0.42,"left":0.05,"right":0.6}`), Confidence: 0.64},
	}
	if len(result.Answers) != len(want) {
		t.Fatalf("%d answers, want %d: %+v", len(result.Answers), len(want), result.Answers)
	}
	for name, want := range want {
		got := result.Answers[name]
		if got.Type != want.Type || got.Choice != want.Choice || got.Noul != want.Noul ||
			got.Confidence != want.Confidence || !reflect.DeepEqual(got.Probabilities, want.Probabilities) {
			t.Errorf("the %s answer is %+v, want %+v", name, got, want)
		}
		for _, raw := range []struct {
			field     string
			got, want json.RawMessage
		}{{"score", got.Score, want.Score}, {"legend", got.Legend, want.Legend}, {"box", got.Box, want.Box}} {
			if (len(raw.want) == 0) != (len(raw.got) == 0) || (len(raw.want) > 0 && !sameJSON(t, raw.got, raw.want)) {
				t.Errorf("the %s answer's %s is %s, want %s", name, raw.field, raw.got, raw.want)
			}
		}
	}

	// The response's own token counts, at $3 and $15 per million tokens.
	wantCost := (400*systemOnePrice.Input + 60*systemOnePrice.Output) / 1e6
	if u := result.Usage; u.Input != 400 || u.Output != 60 || !u.Metered || math.Abs(u.CostUSD-wantCost) > 1e-12 {
		t.Errorf("usage %+v, want the response's counts priced at $%.10f", u, wantCost)
	}
}

// asJSON reads v back through JSON, so a test compares what the wire would carry rather than the
// Go value.
func asJSON(t *testing.T, v any, into any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatal(err)
	}
}

// A decision retries a server error once: a second failure is a provider error after two
// requests, and an answer to the retry is the decision's answer, also after two requests.
func TestDecideReportsAProviderErrorAfterOneRetry(t *testing.T) {
	t.Parallel()
	failed := reply{
		status: http.StatusInternalServerError, headers: map[string]string{"Retry-After": "0"},
		body: []byte(`{"error":"the decider is overloaded"}`),
	}

	twice := newStandIn(t, failed, failed, reply{body: []byte(decideAnswer)})
	result, err := Decide(context.Background(), newDecideFixture(t, twice))
	if err != nil {
		t.Fatal(err)
	}
	if result.Fault != FaultProviderError || len(result.Answers) != 0 {
		t.Errorf("after two 500s: result %+v, want %q and no answers: %s", result, FaultProviderError, result.Detail)
	}
	if n := len(twice.requests()); n != 2 {
		t.Errorf("two 500s took %d requests, want 2: one retry only", n)
	}

	retried := newStandIn(t, failed, reply{body: []byte(decideAnswer)})
	result, err = Decide(context.Background(), newDecideFixture(t, retried))
	if err != nil {
		t.Fatal(err)
	}
	if result.Fault != "" || len(result.Answers) != 4 {
		t.Errorf("after a 500 then an answer: result %+v, want the four answers: %s", result, result.Detail)
	}
	if n := len(retried.requests()); n != 2 {
		t.Errorf("a 500 then an answer took %d requests, want 2", n)
	}
}

// A request a decision cannot be made from at all is an error rather than a fault: nothing comes
// back in the result and nothing reaches the wire.
func TestDecideRefusesARequestItCannotRun(t *testing.T) {
	t.Parallel()
	s := newStandIn(t)
	for _, c := range []struct {
		name   string
		change func(*DecideRequest)
	}{
		{name: "no time bound", change: func(r *DecideRequest) { r.Bounds.Time = 0 }},
		{name: "a protocol that answers no decision", change: func(r *DecideRequest) { r.Model.Endpoint.Kind = AnthropicMessages }},
		{name: "an endpoint that reads no token", change: func(r *DecideRequest) { r.Model.Endpoint.Token = nil }},
		{name: "no model", change: func(r *DecideRequest) { r.Model.ID = "" }},
		{name: "no question", change: func(r *DecideRequest) { r.Questions = nil }},
		{name: "a question with no type", change: func(r *DecideRequest) { r.Questions["fruit"] = Question{Instructions: "Which fruit?"} }},
	} {
		t.Run(c.name, func(t *testing.T) {
			req := newDecideFixture(t, s)
			c.change(&req)
			result, err := Decide(context.Background(), req)
			if err == nil {
				t.Fatalf("the decision was made, with result %+v", result)
			}
			if !reflect.DeepEqual(result, DecideResult{}) {
				t.Errorf("the result is %+v, want nothing", result)
			}
		})
	}
	if n := len(s.requests()); n != 0 {
		t.Errorf("%d requests were made, want none", n)
	}
}

// An answer that is not the shape the API answers in is malformed, whether it is no JSON at all
// or a body that holds no answers and no usage.
func TestDecideReportsAMalformedBody(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		body string
	}{
		{name: "no JSON at all", body: "<html>the gateway ate it</html>"},
		{name: "no answers and no usage", body: `{"model":"systemone-standin"}`},
		{name: "answers but no usage", body: `{"model":"systemone-standin","answers":{"answered":{"type":"noul","noul":0.5}}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := newStandIn(t, reply{body: []byte(c.body)})
			result, err := Decide(context.Background(), newDecideFixture(t, s))
			if err != nil {
				t.Fatal(err)
			}
			if result.Fault != FaultMalformed || len(result.Answers) != 0 {
				t.Errorf("result %+v, want %q and no answers: %s", result, FaultMalformed, result.Detail)
			}
		})
	}
}

// A decision reads a bounded answer: an endpoint that answers with more than the cap is
// malformed, the detail names the cap, and the caller is not made to hold the body. The same
// answer within the cap is read as the answer it is, so it is the length and nothing else that
// the fault reports. An oversized body that came with a failing status is that status, since
// what the caller must be told of an error page is the error.
func TestDecideRefusesAnAnswerPastTheCap(t *testing.T) {
	t.Parallel()
	// A whole, valid answer with a padding field, so a body that is refused is refused for
	// its length rather than for its shape.
	padded := func(padding int) []byte {
		answer := map[string]any{
			"model":   "systemone-standin",
			"answers": map[string]any{"answered": map[string]any{"type": "noul", "noul": 0.5}},
			"usage":   map[string]any{"input_tokens": 1, "output_tokens": 1},
			"padding": strings.Repeat("a", padding),
		}
		body, err := json.Marshal(answer)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}

	oversized := newStandIn(t, reply{body: padded(answerLimit)})
	result, err := Decide(context.Background(), newDecideFixture(t, oversized))
	if err != nil {
		t.Fatal(err)
	}
	if result.Fault != FaultMalformed || len(result.Answers) != 0 {
		t.Errorf("result %+v, want %q and no answers: %s", result, FaultMalformed, result.Detail)
	}
	if want := fmt.Sprintf("%d MiB", answerLimit>>20); !strings.Contains(result.Detail, want) {
		t.Errorf("the detail is %q, want the cap, %s, named in it", result.Detail, want)
	}

	within := newStandIn(t, reply{body: padded(1024)})
	result, err = Decide(context.Background(), newDecideFixture(t, within))
	if err != nil {
		t.Fatal(err)
	}
	if result.Fault != "" || len(result.Answers) != 1 {
		t.Errorf("the answer within the cap: %+v, want the one answer it holds: %s", result, result.Detail)
	}

	// The status is read before the length, so an error page past the cap is the
	// provider's error rather than a malformed answer. A 5xx is retried once, which
	// Retry-After keeps to no wait, so the stand-in answers twice.
	page := []byte("<html><body>" + strings.Repeat("a", answerLimit) + "</body></html>")
	failing := make([]reply, 2)
	for i := range failing {
		failing[i] = reply{status: http.StatusBadGateway, body: page, headers: map[string]string{"Retry-After": "0"}}
	}
	result, err = Decide(context.Background(), newDecideFixture(t, newStandIn(t, failing...)))
	if err != nil {
		t.Fatal(err)
	}
	if result.Fault != FaultProviderError || !strings.Contains(result.Detail, "502") {
		t.Errorf("the oversized error page: %+v, want %q naming the status: %s", result, FaultProviderError, result.Detail)
	}
}
