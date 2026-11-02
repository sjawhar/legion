package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// An ask's question and its options' labels and descriptions share one bound, held wherever the
// ask is written: on create, on an edit that sends options alone and is measured against the
// question the row already holds, and in a document's decision block, where the refusal comes
// back as the one every unreadable ask block gets. They are bounded together because an
// ask.answered event carries them all at once.
func TestAnAsksWordingIsBoundedWhereverItIsWritten(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "ASKB", "Ask wording bounds", "A spec")

	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": strings.Repeat("a", 1000) + "\n\n" + strings.Repeat("b", 1000),
		"options":  []map[string]string{{"label": "Ship"}, {"label": "Hold"}},
		"actor":    sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("a question of two 1,000-unit paragraphs with options: status=%d body=%s", created.Code, created.Body.String())
	}
	ask := decodeBody[struct {
		ID       string `json:"id"`
		Question string `json:"question"`
	}](t, created)

	// The edit sends options alone, so what it is held to is the question already stored plus
	// the options it sends: one unit past the bound between them.
	room := contracts.DispatchEventBodyMax + 1 - len16(ask.Question)
	edited := sessionRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+ask.ID, map[string]any{
		"options": []map[string]string{{"label": "Ship", "description": strings.Repeat("c", room-len("Ship"))}},
		"actor":   sessionActor(),
	})
	assertCapExceeded(t, "an edit sending options alone", edited, "question and options",
		contracts.DispatchEventBodyMax+1, contracts.DispatchEventBodyMax, "")

	// A decision block is an ask the document writes, so the same bound refuses the document's
	// save and nothing of it is stored.
	block := ":::ask{#big urgency=\"med\" multiple=\"false\" state=\"open\"}\n" +
		strings.Repeat("q", contracts.DispatchEventBodyMax) + "\n\n- Ship: Release it\n:::\n"
	refused := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"actor": sessionActor(),
		"ops":   []map[string]string{{"op": "insert", "after": "end", "markdown": block}},
	})
	// The wording is the 32,000-unit question plus "Ship" and "Release it": 32,014 units.
	body := refused.Body.String()
	if refused.Code != http.StatusBadRequest || !strings.Contains(body, `"code":"INVALID_ASK_BLOCK"`) ||
		!strings.Contains(body, `ask block \"big\" is 14 characters over the 32000-character limit on its question and options (32014/32000)`) {
		t.Fatalf("a decision block past the bound: status=%d body=%.400s", refused.Code, body)
	}
	text := decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice"))
	if strings.Contains(text.Markdown, "#big") {
		t.Fatalf("the document holds the refused decision block: %.200s", text.Markdown)
	}
}

// An answer's text, a resolution's reason and a review's reason each ride an event whole, so
// each takes contracts.DispatchEventBodyMax on its own and is refused naming its own field. A
// review's reason is held before the open approval request is looked for, so the refusal is the
// same whether one is open or not, and nothing is written either way.
func TestAnAnswerAResolutionAndAReviewAreBoundedByTheirField(t *testing.T) {
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "EVT", "Event bounds", "A spec")
	over := strings.Repeat("a", contracts.DispatchEventBodyMax+1)
	openAsk := func(question string) string {
		created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
			"question": question, "actor": sessionActor(),
		})
		if created.Code != http.StatusCreated {
			t.Fatalf("open %q: status=%d body=%s", question, created.Code, created.Body.String())
		}
		return decodeBody[struct {
			ID string `json:"id"`
		}](t, created).ID
	}

	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+openAsk("Answer?")+"/answer", map[string]any{
		"text": over, "expected_edited_at": nil,
	}, "alice")
	assertCapExceeded(t, "an answer's text", answered, "text",
		contracts.DispatchEventBodyMax+1, contracts.DispatchEventBodyMax, "")

	resolved := sessionRequest(t, handler, http.MethodPost, "/api/v1/asks/"+openAsk("Resolve?")+"/resolve", map[string]any{
		"kind": "resolved", "reason": over, "actor": sessionActor(),
	})
	assertCapExceeded(t, "a resolution's reason", resolved, "reason",
		contracts.DispatchEventBodyMax+1, contracts.DispatchEventBodyMax, "")

	review := func(step string) {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/reviews", map[string]any{
			"state": "changes_requested", "reason": over,
		}, "alice")
		assertCapExceeded(t, step, response, "reason",
			contracts.DispatchEventBodyMax+1, contracts.DispatchEventBodyMax, "")
	}
	review("a review's reason with no approval request open")
	if requested := sessionRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests",
		map[string]any{"actor": sessionActor()}); requested.Code != http.StatusCreated {
		t.Fatalf("open an approval request: status=%d body=%s", requested.Code, requested.Body.String())
	}
	review("a review's reason with an approval request open")
	if reviews := dispatchRequest(t, handler, http.MethodGet,
		"/api/v1/artifacts/"+issue.PrimaryArtifactID+"/reviews", nil, "alice"); strings.TrimSpace(reviews.Body.String()) != "[]" {
		t.Fatalf("reviews after the refusals = %s, want none written", reviews.Body.String())
	}
}
