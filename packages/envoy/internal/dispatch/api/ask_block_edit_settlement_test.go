package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// blockAskID is the id of the ask indexed from blockID on the issue, failing the test when the
// document's ask blocks never produced one.
func blockAskID(t *testing.T, handler http.Handler, issueKey, blockID string) string {
	t.Helper()
	asks := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issueKey+"/asks", nil, "alice")
	if asks.Code != http.StatusOK {
		t.Fatalf("list asks: status=%d body=%s", asks.Code, asks.Body.String())
	}
	for _, ask := range decodeBody[[]model.Ask](t, asks) {
		if ask.BlockID != nil && *ask.BlockID == blockID {
			return ask.ID
		}
	}
	t.Fatalf("no ask indexed from block %q on %s", blockID, issueKey)
	return ""
}

// askQuestion is one ask's current question, as the dashboard and the tools read it.
func askQuestion(t *testing.T, handler http.Handler, askID string) string {
	t.Helper()
	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+askID, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read ask: status=%d body=%s", read.Code, read.Body.String())
	}
	return decodeBody[struct {
		Ask model.Ask `json:"ask"`
	}](t, read).Ask.Question
}

// A block ask keeps its question in two places: the ask row and the `:::ask` block. `PATCH
// /api/v1/asks/{id}` writes the row only, so the next settlement finds the row disagreeing with
// the unchanged block and writes the block's text back - undoing an agent's edit of its own ask
// the next time anyone touches the document, and publishing the revert as an `ask.edited`
// naming that person (LEGION-265).
//
// The human's edit adds a second ask block, so the settlement that indexes it is the same
// reconciliation pass that reads the edited ask: its `ask.opened` is the signal that settlement
// has run, with no sleep standing in for one.
func TestEditedBlockAskSurvivesTheNextDocumentSettlement(t *testing.T) {
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Block ask edit", "Context\n")
	written := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"actor": sessionActor(),
		"ops": []map[string]string{{
			"op":       "insert",
			"after":    "end",
			"markdown": ":::ask{#decision urgency=\"high\" multiple=\"false\"}\nWhich transport?\n\n- REST: Matches the platform\n- gRPC: Adds streaming\n:::\n",
		}},
	})
	if written.Code != http.StatusOK {
		t.Fatalf("write ask block: status=%d body=%s", written.Code, written.Body.String())
	}
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "decision", "Which transport?")
	askID := blockAskID(t, handler, issue.Key, "decision")

	edited := sessionRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+askID, map[string]any{
		"question": "Which transport ships first?",
		"actor":    sessionActor(),
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("edit block ask: status=%d body=%s", edited.Code, edited.Body.String())
	}
	if question := askQuestion(t, handler, askID); question != "Which transport ships first?" {
		t.Fatalf("ask question right after the edit = %q", question)
	}

	// A human edits the same document, as the browser and dispatch_doc_edit both do.
	// Settlement follows; it must not touch the ask the session just reworded.
	settling := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]string{{
			"op":       "insert",
			"after":    "end",
			"markdown": ":::ask{#unrelated urgency=\"low\" multiple=\"false\"}\nAnything else?\n:::\n",
		}},
	}, "alice")
	if settling.Code != http.StatusOK {
		t.Fatalf("edit the document: status=%d body=%s", settling.Code, settling.Body.String())
	}
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "unrelated", "Anything else?")

	if question := askQuestion(t, handler, askID); question != "Which transport ships first?" {
		t.Fatalf("settlement reverted the edited ask: question = %q", question)
	}
	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events?limit=50", nil, "alice")
	if events.Code != http.StatusOK {
		t.Fatalf("read events: status=%d body=%s", events.Code, events.Body.String())
	}
	for _, event := range decodeBody[[]struct {
		Type    string      `json:"type"`
		Actor   model.Actor `json:"actor"`
		Payload struct {
			ID       string `json:"id"`
			Question string `json:"question"`
		} `json:"payload"`
	}](t, events) {
		if event.Type != "ask.edited" || event.Payload.ID != askID {
			continue
		}
		if event.Actor.Kind != "session" || event.Payload.Question != "Which transport ships first?" {
			t.Fatalf("ask.edited by %s %q carrying %q; only the editing session's own edit may be published",
				event.Actor.Kind, event.Actor.ID, event.Payload.Question)
		}
	}
}
