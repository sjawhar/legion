package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	if _, err := replaceDocumentText(database, documentService, issue.PrimaryArtifactID, ":::ask{#ask-1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nShip it?\n:::\n", model.Actor{Kind: "session", ID: "session-1"}); err != nil {
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

// Markdown that becomes a document is held to an ask block's content rule, paragraph+
// bullet_list? - one or more paragraphs, then at most one bullet list, last - wherever it enters: a
// spec at issue creation, a new document, a new version of one. The browser editor's parser
// refuses to build such a block, and stored, it would refuse every later edit to the document. What
// the rule allows is taken, including an option without a label or a question that is only an
// image, which settlement flags `invalid` and the dashboard shows as a malformed decision.
func TestUploadsHoldAskBlocksToTheirContentRule(t *testing.T) {
	handler := newTestHandler(t)
	ask := func(body string) string {
		return "Intro.\n\n:::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\n" + body + "\n:::\n\nAfter.\n"
	}
	issue := createInteractionIssue(t, handler, "UP", "Uploads", "Intro.\n")
	uploaded := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"name": "notes.md", "content": "Notes.\n",
	}, "alice")
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload notes: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	notes := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact
	routes := func(index int, content string) map[string]func() *httptest.ResponseRecorder {
		return map[string]func() *httptest.ResponseRecorder{
			"create an issue": func() *httptest.ResponseRecorder {
				return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "UP", "title": fmt.Sprintf("Decision %d: %s", index, strings.Repeat("x", index+1)), "spec": content, "force": true}, "alice")
			},
			"upload a document": func() *httptest.ResponseRecorder {
				return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{"name": fmt.Sprintf("doc-%d.md", index), "content": content}, "alice")
			},
		}
	}
	for index, body := range []string{
		"Which?\n\n```\ncode\n```",
		"Which?\n\n- A\n- B\n\nAn afterthought.",
		"Which?\n\n- A\n\n1. B",
		"- A\n- B",
	} {
		for route, send := range routes(index, ask(body)) {
			if response := send(); response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_ASK_BLOCK"`) || !strings.Contains(response.Body.String(), "paragraph+ bullet_list?") {
				t.Fatalf("%s with %q: status=%d body=%s", route, body, response.Code, response.Body.String())
			}
		}
		version := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{"name": "notes.md", "content": ask(body)}, "alice")
		if version.Code != http.StatusBadRequest || !strings.Contains(version.Body.String(), `"code":"INVALID_ASK_BLOCK"`) {
			t.Fatalf("upload a version with %q: status=%d body=%s", body, version.Code, version.Body.String())
		}
	}
	text := decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+notes.ID+"/text", nil, "alice"))
	if text.Markdown != "Notes.\n" {
		t.Fatalf("notes after refused versions = %q, want them unchanged", text.Markdown)
	}
	for index, body := range []string{
		"Which path?\n\n- Ship: Release it\n- : No label",
		"![a diagram](https://x.test/d.png)\n\n- A\n- B",
	} {
		for route, send := range routes(10+index, ask(body)) {
			if response := send(); response.Code != http.StatusCreated {
				t.Fatalf("%s with %q: status=%d body=%s", route, body, response.Code, response.Body.String())
			}
		}
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
	if response.Code != http.StatusNotFound || errorBody.Code != "TARGET_NOT_FOUND" {
		t.Fatalf("rendered quote miss: status=%d code=%q error=%q", response.Code, errorBody.Code, errorBody.Error)
	}
	for _, want := range []string{
		"operation 0",
		"quote \"Use `missing`\"",
		"the block text as rendered",
		`nearest blocks: "Use config with care."`,
	} {
		if !strings.Contains(errorBody.Error, want) {
			t.Fatalf("rendered quote miss = %q, want it to name %q", errorBody.Error, want)
		}
	}
}

// awaitIndexedAskBlock polls the inbox until the ask block with blockID on artifactID is
// indexed, failing the test when settlement never reaches it.
func awaitIndexedAskBlock(t *testing.T, handler http.Handler, artifactID, blockID, question string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
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

// A record copy of a spec uploaded as a second document of the issue carries the spec's ask
// blocks under their ids. It opens no ask in anyone's Inbox: each copied block shows the state and
// answer of the ask it was copied from (LEGION-651).
func TestAnUploadedCopyOfASpecOpensNoAskForItsCopiedBlocks(t *testing.T) {
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	spec := "Context\n\n" +
		":::ask{#rec-answered urgency=\"med\" multiple=\"false\"}\nShip on Monday?\n\n- Yes: Monday.\n- No: Later.\n:::\n\n" +
		":::ask{#rec-resolved urgency=\"med\" multiple=\"false\"}\nWrite the changelog?\n:::\n\n" +
		":::ask{#rec-open urgency=\"med\" multiple=\"false\"}\nWhich region?\n:::\n"
	issue := createInteractionIssue(t, handler, "TEST", "Record copy", spec)
	for _, block := range []struct{ id, question string }{
		{"rec-answered", "Ship on Monday?"}, {"rec-resolved", "Write the changelog?"}, {"rec-open", "Which region?"},
	} {
		awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, block.id, block.question)
	}
	asks := map[string]string{}
	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks", nil, "alice")
	for _, ask := range decodeBody[[]model.Ask](t, listed) {
		if ask.BlockID != nil {
			asks[*ask.BlockID] = ask.ID
		}
	}
	if answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+asks["rec-answered"]+"/answer", map[string]any{
		"selected": []string{"Yes"},
	}, "alice"); answered.Code != http.StatusOK {
		t.Fatalf("answer: status=%d body=%s", answered.Code, answered.Body.String())
	}
	if resolved := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+asks["rec-resolved"]+"/resolve", map[string]any{
		"kind": "resolved", "reason": "Waived.",
	}, "alice"); resolved.Code != http.StatusOK {
		t.Fatalf("resolve: status=%d body=%s", resolved.Code, resolved.Body.String())
	}
	current := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice")
	record := decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, current).Markdown
	if !strings.Contains(record, `state="answered"`) || !strings.Contains(record, `state="resolved"`) {
		t.Fatalf("spec before the copy = %s", record)
	}

	uploaded := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"actor": sessionActor(), "name": "spec-record.md", "content": record,
	})
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload the record copy: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	copied := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact.ID
	deadline := time.Now().Add(5 * time.Second)
	for {
		text := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+copied+"/text", nil, "alice").Body.String()
		if strings.Contains(text, `#rec-answered urgency=\"med\" multiple=\"false\" state=\"answered\" answered_by=\"alice\"`) &&
			strings.Contains(text, `copied_from=\"`+asks["rec-answered"]+`\"`) &&
			strings.Contains(text, `#rec-resolved urgency=\"med\" multiple=\"false\" state=\"resolved\" copied_from=\"`+asks["rec-resolved"]+`\"`) &&
			strings.Contains(text, `#rec-open urgency=\"med\" multiple=\"false\" state=\"open\" copied_from=\"`+asks["rec-open"]+`\"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the copy never showed its sources' states: %s", text)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Settlement of the copy has run; a few more passes would have opened its asks by now.
	time.Sleep(200 * time.Millisecond)
	var onCopy []string
	all := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks", nil, "alice")
	for _, ask := range decodeBody[[]model.Ask](t, all) {
		if ask.BlockArtifact != nil && ask.BlockArtifact.ID == copied {
			onCopy = append(onCopy, *ask.BlockID+" "+ask.State)
		}
	}
	if len(onCopy) != 0 {
		t.Fatalf("the record copy opened asks %v, want none", onCopy)
	}
}

// An answer to a copy's source, or its resolution, made after the copy settled reaches the copy a
// settlement later, in its live text and in its latest version, which is what the decision card and
// dispatch request-approval's refusal read: the routes settle the copies of the ask they close.
func TestAnsweringOrResolvingACopysSourceSettlesTheCopy(t *testing.T) {
	var documentService *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	spec := "Context\n\n" +
		":::ask{#later-answered urgency=\"med\" multiple=\"false\"}\nShip on Monday?\n\n- Yes: Monday.\n- No: Later.\n:::\n\n" +
		":::ask{#later-resolved urgency=\"med\" multiple=\"false\"}\nWrite the changelog?\n:::\n"
	issue := createInteractionIssue(t, handler, "TEST", "Source answered after its copy", spec)
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "later-answered", "Ship on Monday?")
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "later-resolved", "Write the changelog?")
	asks := map[string]string{}
	for _, ask := range decodeBody[[]model.Ask](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks", nil, "alice")) {
		if ask.BlockID != nil {
			asks[*ask.BlockID] = ask.ID
		}
	}
	uploaded := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"actor": sessionActor(), "name": "spec-record.md", "content": spec,
	})
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload the copy: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	copied := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact.ID
	answeredOpener := `:::ask{#later-answered urgency="med" multiple="false" state="answered" answered_by="alice" `
	resolvedOpener := `:::ask{#later-resolved urgency="med" multiple="false" state="resolved" copied_from="` + asks["later-resolved"] + `"`
	// latest is the copy's live text and the markdown of its latest version.
	latest := func() (string, string) {
		read := decodeBody[struct {
			Markdown string `json:"markdown"`
			Version  *int   `json:"version"`
		}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+copied+"/text", nil, "alice"))
		if read.Version == nil {
			return read.Markdown, ""
		}
		version := decodeBody[struct {
			Markdown string `json:"markdown"`
		}](t, dispatchRequest(t, handler, http.MethodGet, fmt.Sprintf("/api/v1/artifacts/%s/versions/%d", copied, *read.Version), nil, "alice"))
		return read.Markdown, version.Markdown
	}
	awaitCopy := func(what string, holds func(string) bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			live, version := latest()
			if holds(live) && holds(version) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("the copy never showed %s; live:\n%s\nlatest version:\n%s", what, live, version)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	awaitCopy("its open sources", func(text string) bool {
		return strings.Contains(text, `#later-answered urgency="med" multiple="false" state="open" copied_from="`+asks["later-answered"]+`"`) &&
			strings.Contains(text, `#later-resolved urgency="med" multiple="false" state="open" copied_from="`+asks["later-resolved"]+`"`)
	})

	if answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+asks["later-answered"]+"/answer", map[string]any{
		"selected": []string{"Yes"}, "text": "From the record.",
	}, "alice"); answered.Code != http.StatusOK {
		t.Fatalf("answer the source: status=%d body=%s", answered.Code, answered.Body.String())
	}
	if resolved := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+asks["later-resolved"]+"/resolve", map[string]any{
		"kind": "resolved", "reason": "Waived.",
	}, "alice"); resolved.Code != http.StatusOK {
		t.Fatalf("resolve the source: status=%d body=%s", resolved.Code, resolved.Body.String())
	}
	awaitCopy("its sources' answer and resolution", func(text string) bool {
		return strings.Contains(text, answeredOpener) &&
			strings.Contains(text, `selected="[&#x22;Yes&#x22;]" answer="From the record." copied_from="`+asks["later-answered"]+`"`) &&
			strings.Contains(text, resolvedOpener)
	})
	var onCopy, pending int
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from asks where block_artifact_id = $1),
			(select count(*) from doc_settlements_pending where artifact_id = $1)
	`, copied).Scan(&onCopy, &pending); err != nil {
		t.Fatalf("read the copy's asks and pending settlement: %v", err)
	}
	if onCopy != 0 || pending != 0 {
		t.Fatalf("the copy holds %d asks and %d pending settlements, want none of either", onCopy, pending)
	}
}

// A record copy taken while an ask was open asks the ask's wording of then. Once the ask is
// reworded with other options (PATCH) and the new wording is answered, the answer route settles the
// copy (SettleCopiesOf), and that settlement opens the copy's own ask for its block rather than
// writing the answer to a question the copy does not ask into the record.
func TestAnAnswerToACopysSourceAfterItsRewordingOpensTheCopysOwnAsk(t *testing.T) {
	var documentService *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	spec := "Context\n\n:::ask{#ship urgency=\"med\" multiple=\"false\"}\nShip on Monday?\n\n- Yes: Monday.\n- No: Later.\n:::\n"
	issue := createInteractionIssue(t, handler, "TEST", "Source reworded after its copy", spec)
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "ship", "Ship on Monday?")
	var source string
	for _, ask := range decodeBody[[]model.Ask](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks", nil, "alice")) {
		if ask.BlockID != nil && *ask.BlockID == "ship" {
			source = ask.ID
		}
	}
	uploaded := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"actor": sessionActor(), "name": "spec-record.md", "content": spec,
	})
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload the copy: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	copied := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact.ID
	// copyText is the copy's live text and the markdown of its latest version.
	copyText := func() (string, string) {
		read := decodeBody[struct {
			Markdown string `json:"markdown"`
			Version  *int   `json:"version"`
		}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+copied+"/text", nil, "alice"))
		if read.Version == nil {
			return read.Markdown, ""
		}
		version := decodeBody[struct {
			Markdown string `json:"markdown"`
		}](t, dispatchRequest(t, handler, http.MethodGet, fmt.Sprintf("/api/v1/artifacts/%s/versions/%d", copied, *read.Version), nil, "alice"))
		return read.Markdown, version.Markdown
	}
	shown := `#ship urgency="med" multiple="false" state="open" copied_from="` + source + `"`
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		live, version := copyText()
		if strings.Contains(live, shown) && strings.Contains(version, shown) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the copy never showed its open source; live:\n%s\nlatest version:\n%s", live, version)
		}
	}

	reworded := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+source, map[string]any{
		"question": "Ship on Tuesday?",
		"options":  []model.AskOption{{Label: "Tuesday", Description: "Ship it."}, {Label: "No", Description: "Later."}},
	}, "alice")
	if reworded.Code != http.StatusOK {
		t.Fatalf("reword the source: status=%d body=%s", reworded.Code, reworded.Body.String())
	}
	editedAt := decodeBody[model.Ask](t, reworded).EditedAt
	if answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+source+"/answer", map[string]any{
		"selected": []string{"Tuesday"}, "expected_edited_at": editedAt,
	}, "alice"); answered.Code != http.StatusOK {
		t.Fatalf("answer the reworded source: status=%d body=%s", answered.Code, answered.Body.String())
	}
	own := `:::ask{#ship urgency="med" multiple="false" state="open"}` + "\nShip on Monday?"
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		live, version := copyText()
		if strings.Contains(live, `state="answered"`) || strings.Contains(version, `state="answered"`) {
			t.Fatalf("the copy of the earlier wording shows the answer to the reworded question; live:\n%s\nlatest version:\n%s", live, version)
		}
		if strings.Contains(live, own) && strings.Contains(version, own) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the copy never opened its own ask; live:\n%s\nlatest version:\n%s", live, version)
		}
	}
	var onCopy []string
	for _, ask := range decodeBody[[]model.Ask](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks", nil, "alice")) {
		if ask.BlockArtifact != nil && ask.BlockArtifact.ID == copied {
			onCopy = append(onCopy, *ask.BlockID+" "+ask.State+" "+ask.Question)
		}
	}
	if len(onCopy) != 1 || onCopy[0] != "ship open Ship on Monday?" {
		t.Fatalf("the copy's asks = %v, want its own open ship asking the earlier wording", onCopy)
	}
	var pending int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from doc_settlements_pending where artifact_id = $1`, copied).Scan(&pending); err != nil {
		t.Fatalf("read the copy's pending settlement: %v", err)
	}
	if pending != 0 {
		t.Fatalf("the copy owes %d settlements, want none", pending)
	}
}

// A free-text ask block (no bullet list) must put `"options": []` on the wire - in the ask.opened
// event and on the ask row - never JSON null: the SPA's Conversation tab reads options.length
// and a null there takes the page down.
func TestOptionlessAskBlockCarriesEmptyOptionsNotNull(t *testing.T) {
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Free-text decision", "Context\n")
	edited := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"actor": sessionActor(),
		"ops": []map[string]string{{
			"after":    "end",
			"markdown": ":::ask{#free-ask urgency=\"med\" multiple=\"false\"}\nWhich name do we ship under?\n:::\n",
			"op":       "insert",
		}},
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("write ask block: status=%d body=%s", edited.Code, edited.Body.String())
	}
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "free-ask", "Which name do we ship under?")
	if options := awaitAskBlockEvent(t, handler, issue.Key, "ask.opened", "free-ask").Options; string(options) != "[]" {
		t.Fatalf("ask.opened options = %s, want []", options)
	}
	asks := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks", nil, "alice")
	for _, ask := range decodeBody[[]model.Ask](t, asks) {
		if ask.BlockID != nil && *ask.BlockID == "free-ask" && ask.Options == nil {
			t.Fatalf("ask row options are nil; want an empty list")
		}
	}
}

type askBlockEventPayload struct {
	BlockID *string         `json:"block_id"`
	Options json.RawMessage `json:"options"`
}

// awaitAskBlockEvent polls the issue's events until one of eventType names the ask block
// blockID, failing the test when settlement never emits it.
func awaitAskBlockEvent(t *testing.T, handler http.Handler, issueKey, eventType, blockID string) askBlockEventPayload {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issueKey+"/events?limit=50", nil, "alice")
		if events.Code != http.StatusOK {
			t.Fatalf("read events: status=%d body=%s", events.Code, events.Body.String())
		}
		for _, event := range decodeBody[[]struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}](t, events) {
			if event.Type != eventType {
				continue
			}
			var payload askBlockEventPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatalf("decode %s payload: %v", eventType, err)
			}
			if payload.BlockID != nil && *payload.BlockID == blockID {
				return payload
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no %s event for ask block %q", eventType, blockID)
	return askBlockEventPayload{}
}

// Settlement runs after the edit's own version has consumed the room's pending authors; the ask
// it indexes must still be attributed to the session that wrote the block, or the asker can
// never edit its own ask and nobody can tell who is asking.
func TestBlockAskIndexedAfterEditKeepsTheWritingActor(t *testing.T) {
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Attributed decision",
		"Context\n\n:::ask{#seeded urgency=\"med\" multiple=\"false\"}\nSeeded question?\n:::\n")
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "seeded", "Seeded question?")
	edited := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"actor": sessionActor(),
		"ops": []map[string]string{{
			"after":    "end",
			"markdown": ":::ask{#edited urgency=\"high\" multiple=\"false\"}\nEdited question?\n:::\n",
			"op":       "insert",
		}},
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("write ask block: status=%d body=%s", edited.Code, edited.Body.String())
	}
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "edited", "Edited question?")
	asks := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks", nil, "alice")
	want := map[string]model.Actor{
		"seeded": {Kind: "user", ID: "alice"},
		"edited": {Kind: "session", ID: sessionActor()["id"].(string)},
	}
	seen := 0
	for _, ask := range decodeBody[[]model.Ask](t, asks) {
		if ask.BlockID == nil {
			continue
		}
		expected, ok := want[*ask.BlockID]
		if !ok {
			continue
		}
		seen++
		if ask.Author.Kind != expected.Kind || ask.Author.ID != expected.ID {
			t.Fatalf("block %s author = %#v, want %s %s", *ask.BlockID, ask.Author, expected.Kind, expected.ID)
		}
	}
	if seen != 2 {
		t.Fatalf("saw %d block asks, want 2", seen)
	}
}

func TestChangingBlockAskReplacesAnswerAttributes(t *testing.T) {
	handler, _ := blockAskHandler(t)
	issue, askID := seedBlockAsk(t, handler, "Change typed ask", "decision", transportAsk, "Which transport?")
	first := readBlockAsk(t, handler, askID)
	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
		"selected":           []string{"REST"},
		"text":               "REST first.",
		"expected_edited_at": first.EditedAt,
	}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("answer block ask: status=%d body=%s", answered.Code, answered.Body.String())
	}
	original := decodeBody[model.Ask](t, answered)
	if original.Answer == nil {
		t.Fatal("initial block answer is missing")
	}
	originalAt := timestampValue(original.Answer.At)

	changed := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
		"selected":           []string{"gRPC"},
		"expected_answer_at": originalAt,
	}, "alice")
	if changed.Code != http.StatusOK {
		t.Fatalf("change block answer: status=%d body=%s", changed.Code, changed.Body.String())
	}
	updated := decodeBody[model.Ask](t, changed)
	if updated.Answer == nil || len(updated.Answer.Selected) != 1 || updated.Answer.Selected[0] != "gRPC" || updated.Answer.Text != nil || !updated.Answer.At.After(original.Answer.At) {
		t.Fatalf("changed block answer = %#v", updated)
	}
	markdown := documentMarkdown(t, handler, issue.PrimaryArtifactID)
	if !strings.Contains(markdown, `selected="[&#x22;gRPC&#x22;]"`) || strings.Contains(markdown, `answer="REST first."`) || strings.Contains(markdown, `answer=""`) {
		t.Fatalf("changed block markdown = %q, want gRPC selected and no answer attribute", markdown)
	}
}
