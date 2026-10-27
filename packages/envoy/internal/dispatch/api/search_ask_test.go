package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// rawSearchResults decodes a search response's results as raw JSON objects, so a test can tell
// a key that is absent from one that is null or empty.
func rawSearchResults(t *testing.T, response *httptest.ResponseRecorder) []map[string]json.RawMessage {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("search: status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Results []map[string]json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode search response: %v; body=%s", err, response.Body.String())
	}
	return body.Results
}

// rawSearchHit is the one raw result of kind and id, failing the test when the page lacks it.
func rawSearchHit(t *testing.T, results []map[string]json.RawMessage, kind, id string) map[string]json.RawMessage {
	t.Helper()
	for _, result := range results {
		var resultKind, resultID string
		if err := json.Unmarshal(result["kind"], &resultKind); err != nil {
			t.Fatalf("decode result kind: %v", err)
		}
		if err := json.Unmarshal(result["id"], &resultID); err != nil {
			t.Fatalf("decode result id: %v", err)
		}
		if resultKind == kind && resultID == id {
			return result
		}
	}
	t.Fatalf("results = %v, want a %s hit %q", results, kind, id)
	return nil
}

// rawHitAsk is a hit's ask object as raw fields, failing the test when the hit carries none.
func rawHitAsk(t *testing.T, hit map[string]json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	raw, ok := hit["ask"]
	if !ok {
		t.Fatalf("hit %v carries no ask", hit)
	}
	var ask map[string]json.RawMessage
	if err := json.Unmarshal(raw, &ask); err != nil {
		t.Fatalf("decode hit ask %s: %v", raw, err)
	}
	return ask
}

func rawString(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("decode %s as a string: %v", raw, err)
	}
	return value
}

func rawAnswer(t *testing.T, ask map[string]json.RawMessage) model.AskAnswer {
	t.Helper()
	raw, ok := ask["answer"]
	if !ok {
		t.Fatalf("ask %v carries no answer", ask)
	}
	var answer model.AskAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		t.Fatalf("decode answer %s: %v", raw, err)
	}
	return answer
}

func rawResolution(t *testing.T, ask map[string]json.RawMessage) model.AskResolution {
	t.Helper()
	raw, ok := ask["resolution"]
	if !ok {
		t.Fatalf("ask %v carries no resolution", ask)
	}
	var resolution model.AskResolution
	if err := json.Unmarshal(raw, &resolution); err != nil {
		t.Fatalf("decode resolution %s: %v", raw, err)
	}
	return resolution
}

func createSearchAsk(t *testing.T, handler http.Handler, issueKey, question string, labels ...string) string {
	t.Helper()
	options := make([]map[string]string, 0, len(labels))
	for _, label := range labels {
		options = append(options, map[string]string{"label": label})
	}
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/asks", map[string]any{
		"question": question,
		"options":  options,
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create ask %q: status=%d body=%s", question, response.Code, response.Body.String())
	}
	return decodeBody[model.Ask](t, response).ID
}

func answerSearchAsk(t *testing.T, handler http.Handler, askID string, body map[string]any) model.AskAnswer {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", body, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("answer ask %s: status=%d body=%s", askID, response.Code, response.Body.String())
	}
	answered := decodeBody[model.Ask](t, response)
	if answered.Answer == nil {
		t.Fatalf("answered ask %s carries no answer", askID)
	}
	return *answered.Answer
}

func TestSearchAskHitsCarryTheirAnswerState(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "SRCH", "Orrery restoration", "Gear notes.")

	comment := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "The orrery needs oiling.",
	}, "alice")
	if comment.Code != http.StatusCreated {
		t.Fatalf("create comment: status=%d body=%s", comment.Code, comment.Body.String())
	}
	commentID := decodeBody[model.Comment](t, comment).ID

	// (a) answered, then changed by its answerer to no selected option and other text. The
	// change sends no selected at all, so the row stores it as null.
	changed := createSearchAsk(t, handler, issue.Key, "Which orrery gear metal?", "Brass", "Copper")
	first := answerSearchAsk(t, handler, changed, map[string]any{
		"selected": []string{"Brass"}, "text": "Brass resists wear.",
	})
	answerSearchAsk(t, handler, changed, map[string]any{
		"text": "Neither metal: use steel.", "expected_answer_at": timestampValue(first.At),
	})
	history := decodeBody[answerHistoryRead](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+changed, nil, "alice"))
	if len(history.Answers) != 2 {
		t.Fatalf("changed ask history = %#v, want two answers", history.Answers)
	}
	latest := history.Answers[len(history.Answers)-1]

	// (b) answered with an option and no text.
	optionOnly := createSearchAsk(t, handler, issue.Key, "Gild the orrery moons?", "Gold", "Leave bare")
	answerSearchAsk(t, handler, optionOnly, map[string]any{"selected": []string{"Gold"}})

	// (c) resolved without an answer.
	resolved := createSearchAsk(t, handler, issue.Key, "Repaint the orrery base?", "Yes", "No")
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+resolved+"/resolve", map[string]any{
		"kind": "resolved", "reason": "The base stays original.",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("resolve ask: status=%d body=%s", response.Code, response.Body.String())
	}

	// (d) a legacy settlement retraction: answered, then rewritten to resolved with its answer
	// left only in its ask.answered event.
	legacy := createSearchAsk(t, handler, issue.Key, "Plate the orrery arms?", "Silver", "Nickel")
	legacyAnswer := answerSearchAsk(t, handler, legacy, map[string]any{
		"selected": []string{"Silver"}, "text": "Silver, thinly.",
	})
	if _, err := database.Pool.Exec(context.Background(), `
		update asks
		set state = 'resolved', answer = null,
		    resolution = jsonb_build_object(
		      'kind', 'retracted',
		      'reason', 'removed from the document in version 3',
		      'actor', jsonb_build_object('kind', 'system', 'id', 'document-settlement'),
		      'at', now()
		    )
		where id = $1
	`, legacy); err != nil {
		t.Fatalf("seed legacy settlement retraction: %v", err)
	}

	// (e) open.
	open := createSearchAsk(t, handler, issue.Key, "Wind the orrery weekly?", "Yes", "No")

	results := rawSearchResults(t, searchRequest(t, handler, "q=orrery&limit=50"))

	t.Run("changed answer", func(t *testing.T) {
		hit := rawSearchHit(t, results, "ask", changed)
		ask := rawHitAsk(t, hit)
		if state := rawString(t, ask["state"]); state != "answered" {
			t.Fatalf("state = %q, want answered", state)
		}
		answer := rawAnswer(t, ask)
		if answer.User != "alice" || !answer.At.Equal(latest.At) || answer.Text == nil || *answer.Text != "Neither metal: use steel." {
			t.Fatalf("answer = %#v, want alice's latest answer %#v", answer, latest)
		}
		if !strings.Contains(string(ask["answer"]), `"selected":[]`) {
			t.Fatalf("answer = %s, want an empty selected list, never null", ask["answer"])
		}
		if snippet := rawString(t, hit["snippet"]); strings.Contains(snippet, "label") {
			t.Fatalf("snippet = %q, want the options' labels without their JSON keys", snippet)
		}
	})

	t.Run("option answer", func(t *testing.T) {
		ask := rawHitAsk(t, rawSearchHit(t, results, "ask", optionOnly))
		if state := rawString(t, ask["state"]); state != "answered" {
			t.Fatalf("state = %q, want answered", state)
		}
		answer := rawAnswer(t, ask)
		if !slices.Equal(answer.Selected, []string{"Gold"}) || answer.Text != nil {
			t.Fatalf("answer = %#v, want Gold selected and no text", answer)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(ask["answer"], &fields); err != nil {
			t.Fatalf("decode answer fields: %v", err)
		}
		if text, ok := fields["text"]; !ok || string(text) != "null" {
			t.Fatalf("answer = %s, want text null", ask["answer"])
		}
	})

	t.Run("resolved", func(t *testing.T) {
		ask := rawHitAsk(t, rawSearchHit(t, results, "ask", resolved))
		if state := rawString(t, ask["state"]); state != "resolved" {
			t.Fatalf("state = %q, want resolved", state)
		}
		resolution := rawResolution(t, ask)
		if resolution.Kind != "resolved" || resolution.Reason != "The base stays original." ||
			resolution.Actor.Kind != "user" || resolution.Actor.ID != "alice" || resolution.At.IsZero() {
			t.Fatalf("resolution = %#v, want alice's resolved reason", resolution)
		}
		if _, ok := ask["answer"]; ok {
			t.Fatalf("ask = %v, want no answer for an ask nobody answered", ask)
		}
	})

	t.Run("legacy retraction", func(t *testing.T) {
		ask := rawHitAsk(t, rawSearchHit(t, results, "ask", legacy))
		if state := rawString(t, ask["state"]); state != "resolved" {
			t.Fatalf("state = %q, want resolved", state)
		}
		resolution := rawResolution(t, ask)
		if resolution.Kind != "retracted" || resolution.Reason != "removed from the document in version 3" ||
			resolution.Actor.Kind != "system" || resolution.Actor.ID != "document-settlement" {
			t.Fatalf("resolution = %#v, want the settlement retraction", resolution)
		}
		answer := rawAnswer(t, ask)
		if answer.User != legacyAnswer.User || !answer.At.Equal(legacyAnswer.At) || !slices.Equal(answer.Selected, legacyAnswer.Selected) ||
			answer.Text == nil || legacyAnswer.Text == nil || *answer.Text != *legacyAnswer.Text {
			t.Fatalf("answer = %#v, want the answer alice gave %#v", answer, legacyAnswer)
		}
	})

	t.Run("open", func(t *testing.T) {
		ask := rawHitAsk(t, rawSearchHit(t, results, "ask", open))
		if state := rawString(t, ask["state"]); state != "open" {
			t.Fatalf("state = %q, want open", state)
		}
		if _, ok := ask["answer"]; ok {
			t.Fatalf("ask = %v, want no answer", ask)
		}
		if _, ok := ask["resolution"]; ok {
			t.Fatalf("ask = %v, want no resolution", ask)
		}
	})

	t.Run("other kinds", func(t *testing.T) {
		for _, hit := range []map[string]json.RawMessage{
			rawSearchHit(t, results, "issue", issue.Key),
			rawSearchHit(t, results, "comment", commentID),
		} {
			if raw, ok := hit["ask"]; ok {
				t.Fatalf("hit %v carries ask %s, want no ask key", hit, raw)
			}
		}
	})
}

// TestSuggestionsSearchCarriesAskStates reads the fused search the write-time suggestions run
// (runFusedSearch), which has no HTTP route of its own, and finds the same ask state on it.
func TestSuggestionsSearchCarriesAskStates(t *testing.T) {
	handler, _, deps := newTestServer(t, testServerOptions{})
	s := directServer(deps)
	issue := createInteractionIssue(t, handler, "SUGG", "Tellurion repair", "Notes.")
	askID := createSearchAsk(t, handler, issue.Key, "Which tellurion lamp?", "Oil", "Electric")
	given := answerSearchAsk(t, handler, askID, map[string]any{
		"selected": []string{"Oil"}, "text": "Oil, for period accuracy.",
	})

	found, err := s.runFusedSearch(context.Background(), "tellurion", "", 10, 0)
	if err != nil {
		t.Fatalf("runFusedSearch: %v", err)
	}
	var hit *model.SearchResult
	for index := range found.Results {
		if found.Results[index].Kind == "ask" && found.Results[index].ID == askID {
			hit = &found.Results[index]
		}
	}
	if hit == nil {
		t.Fatalf("results = %#v, want the ask %s", found.Results, askID)
	}
	encoded, err := json.Marshal(hit)
	if err != nil {
		t.Fatalf("encode hit: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("decode hit: %v", err)
	}
	ask := rawHitAsk(t, fields)
	if state := rawString(t, ask["state"]); state != "answered" {
		t.Fatalf("state = %q, want answered", state)
	}
	answer := rawAnswer(t, ask)
	if answer.User != "alice" || !answer.At.Equal(given.At) || !slices.Equal(answer.Selected, []string{"Oil"}) ||
		answer.Text == nil || *answer.Text != "Oil, for period accuracy." {
		t.Fatalf("answer = %#v, want alice's answer %#v", answer, given)
	}
}
