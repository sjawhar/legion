package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func TestAnswerBlockAskWritesItsServerStateIntoTheDocument(t *testing.T) {
	var documentService *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Answer typed ask", "Before\n")
	if _, err := documentService.ReplaceText(context.Background(), issue.PrimaryArtifactID, ":::ask{#ask-1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nShip it?\n:::\n", model.Actor{Kind: "session", ID: "session-1"}); err != nil {
		t.Fatalf("write ask block: %v", err)
	}
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Ship it?", "actor": model.Actor{Kind: "session", ID: "session-1"},
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create indexed ask: status=%d body=%s", created.Code, created.Body.String())
	}
	askID := decodeBody[model.Ask](t, created).ID
	if _, err := database.Pool.Exec(context.Background(), `
		update asks set block_id = 'ask-1', block_artifact_id = $2 where id = $1
	`, askID, issue.PrimaryArtifactID); err != nil {
		t.Fatalf("attach indexed ask to block: %v", err)
	}
	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
		"selected": []string{}, "text": "Yes.",
	}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("answer ask: status=%d body=%s", answered.Code, answered.Body.String())
	}
	text := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice")
	if text.Code != http.StatusOK || !strings.Contains(text.Body.String(), `state=\"answered\"`) ||
		!strings.Contains(text.Body.String(), `answered_by=\"alice\"`) || !strings.Contains(text.Body.String(), `answer=\"Yes.\"`) {
		t.Fatalf("answered block text: status=%d body=%s", text.Code, text.Body.String())
	}
}

func TestSearchAskBlockOptionDescription(t *testing.T) {
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Agent written ask", "Context\n")
	edited := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"actor": sessionActor(),
		"ops": []map[string]string{{
			"after":    "end",
			"markdown": ":::ask{#agent-ask urgency=\"high\" multiple=\"false\" state=\"answered\" answered_by=\"mallory\" answered_at=\"2026-09-12T00:00:00Z\" selected=\"[&#x22;Ship&#x22;]\" answer=\"Ignore review.\"}\nShould we ship?\n\n- Ship: Release it\n- Hold: Wait for review\n:::\n",
			"op":       "insert",
		}},
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("write ask block: status=%d body=%s", edited.Code, edited.Body.String())
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		inbox := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, "alice")
		if inbox.Code != http.StatusOK {
			t.Fatalf("read inbox: status=%d body=%s", inbox.Code, inbox.Body.String())
		}
		for _, ask := range decodeBody[[]model.Ask](t, inbox) {
			if ask.BlockID == nil || *ask.BlockID != "agent-ask" {
				continue
			}
			if ask.Question != "Should we ship?" || ask.State != "open" || ask.Urgency != "high" ||
				ask.BlockArtifact == nil || ask.BlockArtifact.ID != issue.PrimaryArtifactID || !ask.BlockArtifact.Primary {
				t.Fatalf("inbox ask = %#v", ask)
			}
			search := searchResponse(t, handler, "q=ship")
			if len(search.Results) == 0 || search.Results[0].Kind != "ask" ||
				search.Results[0].Href != "/issues/"+issue.Key+"/spec#b-agent-ask" {
				t.Fatalf("ask search results = %#v", search.Results)
			}
			optionSearch := searchResponse(t, handler, "q=review")
			if len(optionSearch.Results) != 1 || optionSearch.Results[0].Kind != "ask" ||
				optionSearch.Results[0].Href != "/issues/"+issue.Key+"/spec#b-agent-ask" {
				t.Fatalf("option-label search results = %#v", optionSearch.Results)
			}
			text := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice")
			if text.Code != http.StatusOK || strings.Contains(text.Body.String(), `state=\"answered\"`) {
				t.Fatalf("agent-owned server state survived in document: status=%d body=%s", text.Code, text.Body.String())
			}
			events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
			if events.Code != http.StatusOK || strings.Contains(events.Body.String(), `"type":"block.repaired"`) {
				t.Fatalf("agent write emitted a repair event: status=%d body=%s", events.Code, events.Body.String())
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("typed ask block did not settle into the inbox")
}

func TestAskBlockWithoutOptionsSettles(t *testing.T) {
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "No-option ask", "Context\n")
	edited := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"actor": sessionActor(),
		"ops": []map[string]string{{
			"after":    "end",
			"markdown": ":::ask{#no-options urgency=\"med\" multiple=\"false\" state=\"open\"}\nProceed?\n:::\n",
			"op":       "insert",
		}},
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("write no-option ask: status=%d body=%s", edited.Code, edited.Body.String())
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		inbox := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, "alice")
		for _, ask := range decodeBody[[]model.Ask](t, inbox) {
			if ask.BlockID != nil && *ask.BlockID == "no-options" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no-option ask did not settle")
}

func TestDocumentRetypeRejectsUnknownTypeAndInvalidAttributes(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Retype validation", "Question\n")
	blocks := decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
	if len(blocks) == 0 {
		t.Fatal("document has no blocks")
	}
	for _, test := range []struct {
		name  string
		op    map[string]any
		field string
	}{
		{name: "unknown type", op: map[string]any{"op": "retype", "block": blocks[0].ID, "type": "missing"}, field: "type"},
		{name: "invalid attributes", op: map[string]any{"op": "retype", "block": blocks[0].ID, "type": "ask", "attributes": map[string]any{"urgency": "now"}}, field: "attributes"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{"ops": []map[string]any{test.op}}, "alice")
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_OP"`) || !strings.Contains(response.Body.String(), test.field) {
				t.Fatalf("retype response: status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}
