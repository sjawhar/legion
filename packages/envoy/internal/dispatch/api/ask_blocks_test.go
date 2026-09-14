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

func TestDocumentEditRejectsInvalidAskBlockAtomically(t *testing.T) {
	var documentService *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Reject malformed ask edit", "Context\n")
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"actor": sessionActor(),
		"ops": []map[string]string{{
			"after":    "end",
			"markdown": ":::ask{#agent-ask urgency=\"med\" multiple=\"false\" state=\"open\"}\nShould we ship?\n\n- Ship: Release it\n- Hold: Wait for review\n:::\n",
			"op":       "insert",
		}},
	})
	if created.Code != http.StatusOK {
		t.Fatalf("create typed ask block: status=%d body=%s", created.Code, created.Body.String())
	}

	var askID, question string
	var optionsJSON []byte
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		err := database.Pool.QueryRow(context.Background(), `
			select id::text, question, options from asks
			where block_artifact_id = $1 and block_id = 'agent-ask'
		`, issue.PrimaryArtifactID).Scan(&askID, &question, &optionsJSON)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if askID == "" {
		t.Fatal("typed ask block did not settle")
	}
	var versionsBefore int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from artifact_versions where artifact_id = $1
	`, issue.PrimaryArtifactID).Scan(&versionsBefore); err != nil {
		t.Fatalf("count versions before rejected edit: %v", err)
	}

	rejected := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"actor": sessionActor(),
		"ops":   []map[string]string{{"op": "replace", "find": "Ship", "with": ""}},
	})
	const reason = `ask block "agent-ask" has an option without a label`
	errorBody := decodeBody[struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}](t, rejected)
	if rejected.Code != http.StatusBadRequest || errorBody.Code != "INVALID_ASK_BLOCK" || errorBody.Error != reason {
		t.Fatalf("reject malformed ask edit: status=%d body=%s", rejected.Code, rejected.Body.String())
	}
	var versionsAfter int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from artifact_versions where artifact_id = $1
	`, issue.PrimaryArtifactID).Scan(&versionsAfter); err != nil {
		t.Fatalf("count versions after rejected edit: %v", err)
	}
	if versionsAfter != versionsBefore {
		t.Fatalf("versions after rejected edit = %d, want %d", versionsAfter, versionsBefore)
	}
	var questionAfter string
	var optionsAfter []byte
	if err := database.Pool.QueryRow(context.Background(), `
		select question, options from asks where id = $1
	`, askID).Scan(&questionAfter, &optionsAfter); err != nil {
		t.Fatalf("load ask after rejected edit: %v", err)
	}
	if questionAfter != question || string(optionsAfter) != string(optionsJSON) {
		t.Fatalf("ask changed after rejected edit: question=%q options=%s", questionAfter, optionsAfter)
	}
}

func TestDocumentEditExplainsRenderedQuoteMiss(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Rendered quote miss", "Use `config` with care.\n")
	response := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"actor": sessionActor(),
		"ops": []map[string]string{{
			"op":   "replace",
			"find": "Use `missing`",
			"with": "updated",
		}},
	})
	errorBody := decodeBody[struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}](t, response)
	const want = `operation 0: quote not found; quotes match the block text as rendered (no markdown markers); nearest block: "Use config with care."`
	if response.Code != http.StatusNotFound || errorBody.Code != "TARGET_NOT_FOUND" || errorBody.Error != want {
		t.Fatalf("rendered quote miss: status=%d code=%q error=%q", response.Code, errorBody.Code, errorBody.Error)
	}
}

// awaitIndexedAskBlock polls the inbox until the ask block with blockID on artifactID is
// indexed, failing the test when settlement never reaches it.
func awaitIndexedAskBlock(t *testing.T, handler http.Handler, artifactID, blockID, question string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		inbox := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, "alice")
		if inbox.Code != http.StatusOK {
			t.Fatalf("read inbox: status=%d body=%s", inbox.Code, inbox.Body.String())
		}
		for _, ask := range decodeBody[[]model.Ask](t, inbox) {
			if ask.BlockID == nil || *ask.BlockID != blockID {
				continue
			}
			if ask.Question != question || ask.State != "open" || ask.BlockArtifact == nil || ask.BlockArtifact.ID != artifactID {
				t.Fatalf("indexed ask = %#v", ask)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("ask block %q on %s was never indexed", blockID, artifactID)
}

// A spec written at issue creation never passes through a live edit; its ask blocks must still
// become asks, or an agent that opens an issue with decisions in the spec gets no inbox rows.
func TestAskBlocksInCreatedSpecAreIndexed(t *testing.T) {
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Seeded decisions",
		"Context\n\n:::ask{#seeded-ask urgency=\"med\" multiple=\"false\"}\nShip the seeded decision?\n\n- Ship: Now.\n- Hold: Later.\n:::\n")
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "seeded-ask", "Ship the seeded decision?")
}

// Uploading a new document version through the artifacts API replaces the text inside the
// upload's own transaction, which suppresses live settlement; the closer must still run.
func TestAskBlocksInUploadedSpecVersionAreIndexed(t *testing.T) {
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Uploaded decisions", "Before\n")
	uploaded := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"actor":   sessionActor(),
		"name":    "spec.md",
		"content": "Before\n\n:::ask{#uploaded-ask urgency=\"high\" multiple=\"false\"}\nShip the uploaded decision?\n\n- Ship: Now.\n- Hold: Later.\n:::\n",
		"summary": "decisions as ask blocks",
	})
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload spec version: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "uploaded-ask", "Ship the uploaded decision?")
}
