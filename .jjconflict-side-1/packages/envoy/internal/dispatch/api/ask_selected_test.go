package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// requireEmptySelected fails unless the raw answer object sends `"selected":[]`: an answer that
// chose no option is an empty list on the wire, never null, so a client can read its length.
func requireEmptySelected(t *testing.T, where string, raw json.RawMessage) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("%s: decode answer %s: %v", where, raw, err)
	}
	if selected, ok := fields["selected"]; !ok || string(selected) != "[]" {
		t.Fatalf("%s: answer = %s, want \"selected\":[]", where, raw)
	}
}

// requireAskReadsEmptySelected reads askID, answered once by alice with text and no selected
// option, through every route that sends its answer, and requires each to send an empty
// selected list.
func requireAskReadsEmptySelected(t *testing.T, handler http.Handler, issueKey, askID string) {
	t.Helper()

	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+askID, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read ask: status=%d body=%s", read.Code, read.Body.String())
	}
	var askRead struct {
		Ask struct {
			Answer json.RawMessage `json:"answer"`
		} `json:"ask"`
		Answers []json.RawMessage `json:"answers"`
	}
	if err := json.Unmarshal(read.Body.Bytes(), &askRead); err != nil {
		t.Fatalf("decode ask read %s: %v", read.Body.String(), err)
	}
	requireEmptySelected(t, "ask read answer", askRead.Ask.Answer)
	if len(askRead.Answers) != 1 {
		t.Fatalf("ask read answers = %s, want one", read.Body.String())
	}
	requireEmptySelected(t, "ask read answer history", askRead.Answers[0])

	list := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issueKey+"/asks?thread=1", nil, "alice")
	if list.Code != http.StatusOK {
		t.Fatalf("list issue asks: status=%d body=%s", list.Code, list.Body.String())
	}
	var rows []struct {
		ID     string          `json:"id"`
		Answer json.RawMessage `json:"answer"`
		Thread struct {
			Answers []json.RawMessage `json:"answers"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode issue ask list %s: %v", list.Body.String(), err)
	}
	if len(rows) != 1 || rows[0].ID != askID || len(rows[0].Thread.Answers) != 1 {
		t.Fatalf("issue ask list = %s, want the one ask with one thread answer", list.Body.String())
	}
	requireEmptySelected(t, "issue ask list answer", rows[0].Answer)
	requireEmptySelected(t, "issue ask list thread answer", rows[0].Thread.Answers[0])

	mine := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/answers", nil, "alice")
	if mine.Code != http.StatusOK {
		t.Fatalf("list my answers: status=%d body=%s", mine.Code, mine.Body.String())
	}
	var answers struct {
		Rows []struct {
			Kind   string          `json:"kind"`
			AskID  string          `json:"ask_id"`
			Answer json.RawMessage `json:"answer"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(mine.Body.Bytes(), &answers); err != nil {
		t.Fatalf("decode my answers %s: %v", mine.Body.String(), err)
	}
	if len(answers.Rows) != 1 || answers.Rows[0].Kind != "answer" || answers.Rows[0].AskID != askID {
		t.Fatalf("my answers = %s, want the one answer", mine.Body.String())
	}
	requireEmptySelected(t, "my answers row", answers.Rows[0].Answer)
}

func TestAskAnswerWithoutSelectedReadsAsEmptyList(t *testing.T) {
	t.Run("answered with text only", func(t *testing.T) {
		handler := newTestHandler(t)
		issue := createInteractionIssue(t, handler, "TEST", "Text-only answer", "A spec")
		askID := createSearchAsk(t, handler, issue.Key, "Which bell metal?", "Bronze", "Brass")
		answerSearchAsk(t, handler, askID, map[string]any{"text": "Neither: cast iron."})

		requireAskReadsEmptySelected(t, handler, issue.Key, askID)
	})

	// A row and an event stored before answers wrote an empty list hold null; the reads fix it.
	t.Run("stored null", func(t *testing.T) {
		handler, database := newTestHandlerWithStore(t)
		issue := createInteractionIssue(t, handler, "TEST", "Stored null answer", "A spec")
		askID := createSearchAsk(t, handler, issue.Key, "Which clapper wood?", "Oak", "Ash")
		answerSearchAsk(t, handler, askID, map[string]any{"text": "Neither: hornbeam."})

		ctx := context.Background()
		if _, err := database.Pool.Exec(ctx, `
			update asks set answer = jsonb_set(answer, '{selected}', 'null') where id = $1
		`, askID); err != nil {
			t.Fatalf("store null selected on the ask row: %v", err)
		}
		if _, err := database.Pool.Exec(ctx, `
			update events set payload = jsonb_set(payload, '{answer,selected}', 'null')
			where type = 'ask.answered' and payload->>'id' = $1
		`, askID); err != nil {
			t.Fatalf("store null selected on the ask.answered event: %v", err)
		}
		var rowNull, eventNull bool
		if err := database.Pool.QueryRow(ctx, `
			select
			  (select answer->'selected' = 'null'::jsonb from asks where id = $1),
			  (select bool_and(payload->'answer'->'selected' = 'null'::jsonb) from events
			   where type = 'ask.answered' and payload->>'id' = $1::text)
		`, askID).Scan(&rowNull, &eventNull); err != nil {
			t.Fatalf("check stored null selected: %v", err)
		}
		if !rowNull || !eventNull {
			t.Fatalf("stored selected null: row=%v event=%v, want both", rowNull, eventNull)
		}

		requireAskReadsEmptySelected(t, handler, issue.Key, askID)
	})
}
