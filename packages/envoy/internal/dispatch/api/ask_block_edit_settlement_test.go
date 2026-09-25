package api

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// blockAskHandler is a handler whose documents settle fast enough for a test to observe one.
func blockAskHandler(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	var documentService *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	return handler, database
}

// seedBlockAsk writes an ask block into the issue's spec as sessionActor and returns the issue
// and the id of the ask settlement indexes from it.
func seedBlockAsk(t *testing.T, handler http.Handler, title, blockID, markdown, question string) (struct {
	Key               string `json:"key"`
	PrimaryArtifactID string `json:"primary_artifact_id"`
}, string) {
	t.Helper()
	issue := createInteractionIssue(t, handler, "TEST", title, "Context\n")
	written := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"actor": sessionActor(),
		"ops":   []map[string]string{{"op": "insert", "after": "end", "markdown": markdown}},
	})
	if written.Code != http.StatusOK {
		t.Fatalf("write ask block: status=%d body=%s", written.Code, written.Body.String())
	}
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, blockID, question)
	return issue, blockAskID(t, handler, issue.Key, blockID)
}

// blockAskID is the id of the ask indexed from blockID on the issue.
func blockAskID(t *testing.T, handler http.Handler, issueKey, blockID string) string {
	t.Helper()
	asks := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issueKey+"/asks?state=all", nil, "alice")
	if asks.Code != http.StatusOK {
		t.Fatalf("list asks: status=%d body=%s", asks.Code, asks.Body.String())
	}
	found := ""
	for _, ask := range decodeBody[[]model.Ask](t, asks) {
		if ask.BlockID == nil || *ask.BlockID != blockID {
			continue
		}
		if found != "" {
			t.Fatalf("block %q has more than one ask: %s and %s", blockID, found, ask.ID)
		}
		found = ask.ID
	}
	if found == "" {
		t.Fatalf("no ask indexed from block %q on %s", blockID, issueKey)
	}
	return found
}

// readBlockAsk is one ask as the dashboard and the tools read it.
func readBlockAsk(t *testing.T, handler http.Handler, askID string) model.Ask {
	t.Helper()
	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+askID, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read ask: status=%d body=%s", read.Code, read.Body.String())
	}
	return decodeBody[struct {
		Ask model.Ask `json:"ask"`
	}](t, read).Ask
}

func documentMarkdown(t *testing.T, handler http.Handler, artifactID string) string {
	t.Helper()
	text := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/text", nil, "alice")
	if text.Code != http.StatusOK {
		t.Fatalf("read document text: status=%d body=%s", text.Code, text.Body.String())
	}
	return decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, text).Markdown
}

// documentVersions is the document's settled versions, newest last.
func documentVersions(t *testing.T, handler http.Handler, artifactID string) []model.Version {
	t.Helper()
	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read artifact: status=%d body=%s", read.Code, read.Body.String())
	}
	return decodeBody[model.Artifact](t, read).Versions
}

// awaitDocumentVersions blocks until the document has at least count settled versions, so a
// test reads a settlement's version rather than racing it.
func awaitDocumentVersions(t *testing.T, handler http.Handler, artifactID string, count int) []model.Version {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		versions := documentVersions(t, handler, artifactID)
		if len(versions) >= count {
			return versions
		}
		if time.Now().After(deadline) {
			t.Fatalf("document %s settled %d versions, want %d", artifactID, len(versions), count)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// settleDocument triggers one settlement of the document by a human and blocks until it has
// run. The trigger inserts a marker ask block, whose indexing is settlement's own signal, so no
// test sleeps for a settlement it cannot see. Each call needs a marker id of its own.
func settleDocument(t *testing.T, handler http.Handler, artifactID, issueKey, marker string) {
	t.Helper()
	edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{
		"ops": []map[string]string{{
			"op":       "insert",
			"after":    "end",
			"markdown": ":::ask{#" + marker + " urgency=\"low\" multiple=\"false\"}\nMarker " + marker + "?\n:::\n",
		}},
	}, "alice")
	if edited.Code != http.StatusOK {
		t.Fatalf("edit the document to trigger settlement: status=%d body=%s", edited.Code, edited.Body.String())
	}
	awaitIndexedAskBlock(t, handler, artifactID, marker, "Marker "+marker+"?")
}

// askEvents are the issue's ask.* and block.* events, oldest first.
type askEvent struct {
	Type    string      `json:"type"`
	Actor   model.Actor `json:"actor"`
	Payload struct {
		ID         string               `json:"id"`
		BlockID    *string              `json:"block_id"`
		Question   string               `json:"question"`
		State      string               `json:"state"`
		Resolution *model.AskResolution `json:"resolution"`
	} `json:"payload"`
}

func issueAskEvents(t *testing.T, handler http.Handler, issueKey string) []askEvent {
	t.Helper()
	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issueKey+"/events?limit=100", nil, "alice")
	if events.Code != http.StatusOK {
		t.Fatalf("read events: status=%d body=%s", events.Code, events.Body.String())
	}
	return decodeBody[[]askEvent](t, events)
}

const transportAsk = ":::ask{#decision urgency=\"high\" multiple=\"false\"}\nWhich transport?\n\n" +
	"- REST: Matches the platform\n- gRPC: Adds streaming\n:::\n"

// A block ask keeps its text in two places: the ask row and the `:::ask` block. An edit writes
// both, so the next settlement agrees with the row and leaves it alone (LEGION-265).
func TestEditedBlockAskSurvivesTheNextDocumentSettlement(t *testing.T) {
	handler, _ := blockAskHandler(t)
	issue, askID := seedBlockAsk(t, handler, "Block ask edit", "decision", transportAsk, "Which transport?")

	edited := sessionRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+askID, map[string]any{
		"question": "Which transport ships first?",
		"options":  []map[string]string{{"label": "REST", "description": "Ship now"}, {"label": "gRPC", "description": "Later"}},
		"multiple": true,
		"urgency":  "blocking",
		"actor":    sessionActor(),
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("edit block ask: status=%d body=%s", edited.Code, edited.Body.String())
	}

	settleDocument(t, handler, issue.PrimaryArtifactID, issue.Key, "after-edit")

	ask := readBlockAsk(t, handler, askID)
	if ask.Question != "Which transport ships first?" || !ask.Multiple || ask.Urgency != "blocking" {
		t.Fatalf("settlement reverted the edited ask: question=%q multiple=%v urgency=%q", ask.Question, ask.Multiple, ask.Urgency)
	}
	if len(ask.Options) != 2 || ask.Options[0].Description != "Ship now" || ask.Options[1].Description != "Later" {
		t.Fatalf("settlement reverted the edited options: %#v", ask.Options)
	}
	if ask.BlockID == nil || *ask.BlockID != "decision" {
		t.Fatalf("edit moved the ask off its block: block_id=%v", ask.BlockID)
	}
	// The same id, so settlement neither opened a duplicate nor retracted the original.
	if again := blockAskID(t, handler, issue.Key, "decision"); again != askID {
		t.Fatalf("block decision now indexes ask %s, was %s", again, askID)
	}

	markdown := documentMarkdown(t, handler, issue.PrimaryArtifactID)
	for _, want := range []string{
		"Which transport ships first?", "- REST: Ship now", "- gRPC: Later",
		"#decision", `urgency="blocking"`, `multiple="true"`,
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("document does not carry %q after the edit:\n%s", want, markdown)
		}
	}
	for _, event := range issueAskEvents(t, handler, issue.Key) {
		if event.Type != "ask.edited" || event.Payload.ID != askID {
			continue
		}
		if event.Actor.Kind != "session" || event.Payload.Question != "Which transport ships first?" {
			t.Fatalf("ask.edited by %s %q carrying %q; only the editing session's own edit may be published",
				event.Actor.Kind, event.Actor.ID, event.Payload.Question)
		}
	}
}

// Acceptance 8: the ask's wording is part of the document, so an edit versions it once, crediting
// the session that edited - never the room's last actor.
func TestEditedBlockAskWritesOneVersionCreditingTheEditor(t *testing.T) {
	handler, _ := blockAskHandler(t)
	issue, askID := seedBlockAsk(t, handler, "Block ask version", "decision", transportAsk, "Which transport?")
	before := len(documentVersions(t, handler, issue.PrimaryArtifactID))

	edited := sessionRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+askID, map[string]any{
		"question": "Which transport ships first?", "actor": sessionActor(),
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("edit block ask: status=%d body=%s", edited.Code, edited.Body.String())
	}
	// The edit's own settlement writes its version; waiting for it is what makes the authors
	// the edit's, rather than whoever else happens to touch the document within the settle
	// window.
	versions := awaitDocumentVersions(t, handler, issue.PrimaryArtifactID, before+1)
	edit := versions[before]
	if len(edit.Authors) != 1 || edit.Authors[0].Kind != "session" || edit.Authors[0].ID != sessionActor()["id"] {
		t.Fatalf("the edit's version authors = %#v, want the editing session alone", edit.Authors)
	}

	// One version, and only one: a later settlement of the same document writes the versions
	// its own edit earns and none on the edited ask's account.
	settleDocument(t, handler, issue.PrimaryArtifactID, issue.Key, "after-version")
	credited := 0
	for _, version := range documentVersions(t, handler, issue.PrimaryArtifactID)[before:] {
		for _, author := range version.Authors {
			if author.Kind == "session" && author.ID == sessionActor()["id"] {
				credited++
				break
			}
		}
	}
	if credited != 1 {
		t.Fatalf("%d versions credit the editing session, want exactly the edit's one", credited)
	}
}

// Acceptance 7, refused half: an option label carrying ": " cannot round-trip through the
// block's `Label: Description` line, so the edit is refused whole - no row, block or version.
func TestEditBlockAskRefusesAnOptionLabelTheBlockCannotCarry(t *testing.T) {
	handler, _ := blockAskHandler(t)
	issue, askID := seedBlockAsk(t, handler, "Block ask refusal", "decision", transportAsk, "Which transport?")
	beforeVersions := len(documentVersions(t, handler, issue.PrimaryArtifactID))
	beforeMarkdown := documentMarkdown(t, handler, issue.PrimaryArtifactID)

	refused := sessionRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+askID, map[string]any{
		"options": []map[string]string{{"label": "REST: the one", "description": "Ship now"}},
		"actor":   sessionActor(),
	})
	if refused.Code != http.StatusBadRequest {
		t.Fatalf("edit with an unrepresentable option label: status=%d body=%s", refused.Code, refused.Body.String())
	}
	if body := refused.Body.String(); !strings.Contains(body, "options") {
		t.Fatalf("the refusal must name the field: %s", body)
	}

	if ask := readBlockAsk(t, handler, askID); ask.Question != "Which transport?" ||
		len(ask.Options) != 2 || ask.Options[0].Label != "REST" || ask.EditedAt != nil {
		t.Fatalf("the refused edit wrote the row: %#v", ask)
	}
	if got := documentMarkdown(t, handler, issue.PrimaryArtifactID); got != beforeMarkdown {
		t.Fatalf("the refused edit wrote the document:\n%s", got)
	}
	if got := len(documentVersions(t, handler, issue.PrimaryArtifactID)); got != beforeVersions {
		t.Fatalf("the refused edit wrote %d versions", got-beforeVersions)
	}
}

// The other half of acceptance 7's refusal: a question whose paragraph the block cannot
// reproduce. A line beginning `:::` closes the directive when the document is rendered, so the
// canonical markdown a version records and an upload re-parses would no longer read back.
func TestEditBlockAskRefusesAQuestionTheDocumentCannotCarry(t *testing.T) {
	handler, _ := blockAskHandler(t)
	issue, askID := seedBlockAsk(t, handler, "Block ask directive", "decision", transportAsk, "Which transport?")
	beforeMarkdown := documentMarkdown(t, handler, issue.PrimaryArtifactID)

	refused := sessionRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+askID, map[string]any{
		"question": "Which transport?\n\n::: not a block",
		"actor":    sessionActor(),
	})
	if refused.Code != http.StatusBadRequest {
		t.Fatalf("edit with a directive line: status=%d body=%s", refused.Code, refused.Body.String())
	}
	if body := refused.Body.String(); !strings.Contains(body, "question") {
		t.Fatalf("the refusal must name the field: %s", body)
	}
	if ask := readBlockAsk(t, handler, askID); ask.Question != "Which transport?" || ask.EditedAt != nil {
		t.Fatalf("the refused edit wrote the row: %#v", ask)
	}
	if got := documentMarkdown(t, handler, issue.PrimaryArtifactID); got != beforeMarkdown {
		t.Fatalf("the refused edit wrote the document:\n%s", got)
	}
}

// Acceptance 7, accepted half: everything the block can carry is accepted, normalised exactly as
// the block parses it back - a description-less option renders as a bare label, a single newline
// survives as a line break, and surrounding whitespace is trimmed rather than refused.
func TestEditBlockAskAcceptsWhatTheBlockCanCarry(t *testing.T) {
	handler, _ := blockAskHandler(t)
	issue, askID := seedBlockAsk(t, handler, "Block ask normalisation", "decision", transportAsk, "Which transport?")

	edited := sessionRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+askID, map[string]any{
		"question": "  Which *transport* ships `first`?\nSay which.\n\nSay why.  ",
		"options": []map[string]any{
			{"label": "REST"},
			{"label": "gRPC", "description": "  streaming: yes  "},
		},
		"actor": sessionActor(),
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("edit block ask: status=%d body=%s", edited.Code, edited.Body.String())
	}
	settleDocument(t, handler, issue.PrimaryArtifactID, issue.Key, "after-normalise")

	ask := readBlockAsk(t, handler, askID)
	if ask.Question != "Which *transport* ships `first`?\nSay which.\n\nSay why." {
		t.Fatalf("question = %q", ask.Question)
	}
	if len(ask.Options) != 2 || ask.Options[0].Label != "REST" || ask.Options[0].Description != "" ||
		ask.Options[1].Label != "gRPC" || ask.Options[1].Description != "streaming: yes" {
		t.Fatalf("options = %#v", ask.Options)
	}
	markdown := documentMarkdown(t, handler, issue.PrimaryArtifactID)
	if !strings.Contains(markdown, "- REST\\n") && !strings.Contains(markdown, "- REST\n") {
		t.Fatalf("a description-less option must render as a bare label:\n%s", markdown)
	}
	// The line break is a hard break in the document, not a literal newline: a literal one
	// renders to markdown that re-parses as a space, which would put the row and block out of
	// step at the next upload.
	if !strings.Contains(markdown, "\\\nSay which.") {
		t.Fatalf("a single newline must survive as a hard break:\n%s", markdown)
	}
}

// Acceptance 5: a retract by a session is a decision, not a settlement artefact, so no later
// settlement reopens it.
func TestRetractedBlockAskSurvivesTheNextDocumentSettlement(t *testing.T) {
	handler, _ := blockAskHandler(t)
	issue, askID := seedBlockAsk(t, handler, "Block ask retract", "decision", transportAsk, "Which transport?")

	resolved := sessionRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/resolve", map[string]any{
		"kind": "retracted", "reason": "answered in the thread", "actor": sessionActor(),
	})
	if resolved.Code != http.StatusOK {
		t.Fatalf("retract block ask: status=%d body=%s", resolved.Code, resolved.Body.String())
	}
	if markdown := documentMarkdown(t, handler, issue.PrimaryArtifactID); !strings.Contains(markdown, `state="resolved"`) {
		t.Fatalf("the retract did not write the block's state:\n%s", markdown)
	}

	settleDocument(t, handler, issue.PrimaryArtifactID, issue.Key, "after-retract")

	ask := readBlockAsk(t, handler, askID)
	if ask.State != "resolved" || ask.Resolution == nil || ask.Resolution.Kind != "retracted" {
		t.Fatalf("settlement reopened the retracted ask: state=%q resolution=%#v", ask.State, ask.Resolution)
	}
	for _, event := range issueAskEvents(t, handler, issue.Key) {
		if event.Type == "block.repaired" && event.Payload.BlockID != nil && *event.Payload.BlockID == "decision" {
			t.Fatalf("settlement repaired the retracted ask's block, crediting %s %q", event.Actor.Kind, event.Actor.ID)
		}
	}
}

// The legacy marker for a settlement-written retraction is its reason, so a caller may not write
// one that reads like it and be restored the next time the document settles.
func TestResolveBlockAskRefusesTheSettlementReason(t *testing.T) {
	handler, _ := blockAskHandler(t)
	_, askID := seedBlockAsk(t, handler, "Block ask spoof", "decision", transportAsk, "Which transport?")

	refused := sessionRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/resolve", map[string]any{
		"kind": "retracted", "reason": "removed from the document in version 9", "actor": sessionActor(),
	})
	if refused.Code != http.StatusBadRequest {
		t.Fatalf("resolve with the settlement's own reason: status=%d body=%s", refused.Code, refused.Body.String())
	}
	if body := refused.Body.String(); !strings.Contains(body, "reason") {
		t.Fatalf("the refusal must name the field: %s", body)
	}
	if ask := readBlockAsk(t, handler, askID); ask.State != "open" {
		t.Fatalf("the refused resolve closed the ask: state=%q", ask.State)
	}
}

// Acceptance 5, second half: when the block leaves the document, settlement retracts its ask in
// its own name, and undoing the deletion brings the ask back.
func TestBlockRemovedFromTheDocumentRetractsAsTheSystemActorAndUndoRestores(t *testing.T) {
	handler, _ := blockAskHandler(t)
	issue, askID := seedBlockAsk(t, handler, "Block ask deletion", "decision", transportAsk, "Which transport?")

	deleted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "delete", "block": "decision"}},
	}, "alice")
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete the ask block: status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	settleDocument(t, handler, issue.PrimaryArtifactID, issue.Key, "after-delete")

	ask := readBlockAsk(t, handler, askID)
	if ask.State != "resolved" || ask.Resolution == nil || ask.Resolution.Kind != "retracted" {
		t.Fatalf("the removed block's ask was not retracted: state=%q resolution=%#v", ask.State, ask.Resolution)
	}
	if ask.Resolution.Actor.Kind != "system" || ask.Resolution.Actor.ID != "document-settlement" {
		t.Fatalf("settlement credited its own retraction to %s %q", ask.Resolution.Actor.Kind, ask.Resolution.Actor.ID)
	}

	restored := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "insert", "after": "end", "markdown": transportAsk}},
	}, "alice")
	if restored.Code != http.StatusOK {
		t.Fatalf("restore the ask block: status=%d body=%s", restored.Code, restored.Body.String())
	}
	settleDocument(t, handler, issue.PrimaryArtifactID, issue.Key, "after-restore")

	if back := readBlockAsk(t, handler, askID); back.State != "open" {
		t.Fatalf("undoing the deletion did not bring the ask back: state=%q", back.State)
	}
}

// Acceptance 6: the revision a human reads is the revision they answer, whatever the document
// does in between.
func TestAnsweringAfterAnEditAndSettlementSucceeds(t *testing.T) {
	handler, _ := blockAskHandler(t)
	issue, askID := seedBlockAsk(t, handler, "Block ask answer", "decision", transportAsk, "Which transport?")

	edited := sessionRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+askID, map[string]any{
		"question": "Which transport ships first?", "actor": sessionActor(),
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("edit block ask: status=%d body=%s", edited.Code, edited.Body.String())
	}
	read := readBlockAsk(t, handler, askID)
	settleDocument(t, handler, issue.PrimaryArtifactID, issue.Key, "after-answer-edit")

	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
		"selected": []string{"REST"}, "text": "REST first.", "expected_edited_at": read.EditedAt,
	}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("answer with the revision the human read: status=%d body=%s", answered.Code, answered.Body.String())
	}
}

// Acceptance 3: the document is still the way a human or dispatch_doc_edit rewords a block ask,
// and the row follows it, credited to that editor.
func TestDocumentEditOfAnAskBlockStillUpdatesTheRow(t *testing.T) {
	handler, _ := blockAskHandler(t)
	issue, askID := seedBlockAsk(t, handler, "Block ask document edit", "decision", transportAsk, "Which transport?")

	reworded := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]string{
			{"op": "replace", "find": "Which transport?", "with": "Which transport, really?"},
			{"op": "replace", "find": "REST: Matches the platform", "with": "REST: Matches everything"},
		},
	}, "alice")
	if reworded.Code != http.StatusOK {
		t.Fatalf("reword the ask block through the document: status=%d body=%s", reworded.Code, reworded.Body.String())
	}
	settleDocument(t, handler, issue.PrimaryArtifactID, issue.Key, "after-document-reword")

	ask := readBlockAsk(t, handler, askID)
	if ask.Question != "Which transport, really?" {
		t.Fatalf("the row did not follow the document: question=%q", ask.Question)
	}
	if len(ask.Options) != 2 || ask.Options[0].Description != "Matches everything" {
		t.Fatalf("the row did not follow the document's options: %#v", ask.Options)
	}
	edits := 0
	for _, event := range issueAskEvents(t, handler, issue.Key) {
		if event.Type != "ask.edited" || event.Payload.ID != askID {
			continue
		}
		edits++
		if event.Actor.Kind != "user" || event.Actor.ID != "alice" {
			t.Fatalf("the document's reword was credited to %s %q", event.Actor.Kind, event.Actor.ID)
		}
	}
	if edits != 1 {
		t.Fatalf("the document's reword produced %d ask.edited events, want 1", edits)
	}
}

// Editing takes the document's locks after the ask row while settlement takes them the other way
// round. They are ordered by the owner row both hold first; if that ever stops being true this
// deadlocks rather than fails, so it runs under its own deadline.
func TestConcurrentBlockAskEditsAndSettlementsDoNotDeadlock(t *testing.T) {
	handler, _ := blockAskHandler(t)
	issue, askID := seedBlockAsk(t, handler, "Block ask concurrency", "decision", transportAsk, "Which transport?")

	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for round := range 6 {
			wg.Add(2)
			go func(round int) {
				defer wg.Done()
				edited := sessionRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+askID, map[string]any{
					"question": fmt.Sprintf("Which transport, round %d?", round), "actor": sessionActor(),
				})
				if edited.Code != http.StatusOK {
					t.Errorf("round %d edit: status=%d body=%s", round, edited.Code, edited.Body.String())
				}
			}(round)
			go func(round int) {
				defer wg.Done()
				settling := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
					"ops": []map[string]string{{
						"op": "insert", "after": "end",
						"markdown": fmt.Sprintf("Paragraph %d.\n", round),
					}},
				}, "alice")
				if settling.Code != http.StatusOK {
					t.Errorf("round %d document edit: status=%d body=%s", round, settling.Code, settling.Body.String())
				}
			}(round)
			wg.Wait()
		}
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("concurrent block-ask edits and document settlements did not finish in 60s")
	}

	settleDocument(t, handler, issue.PrimaryArtifactID, issue.Key, "after-concurrency")
	ask := readBlockAsk(t, handler, askID)
	if !strings.HasPrefix(ask.Question, "Which transport, round ") {
		t.Fatalf("the surviving question is neither edit: %q", ask.Question)
	}
	markdown := documentMarkdown(t, handler, issue.PrimaryArtifactID)
	if !strings.Contains(markdown, ask.Question) {
		t.Fatalf("the row and the block disagree after concurrent writes: row=%q\n%s", ask.Question, markdown)
	}
}

// An ask block written through the document carries formatting the ask row cannot hold: the row
// is `nodeText`, plain. An edit that names only the urgency must leave every one of those
// characters, and the links among them, exactly where they were - rebuilding the body from the
// row would flatten them (LEGION-265 review, PROBE B).
func TestUrgencyOnlyEditLeavesTheQuestionsFormattingAlone(t *testing.T) {
	handler, _ := blockAskHandler(t)
	const formatted = ":::ask{#decision urgency=\"med\" multiple=\"false\"}\nWhich **transport** ships [first](https://example.test/rfc)?\n\n" +
		"- REST: Matches the platform\n:::\n"
	issue, askID := seedBlockAsk(t, handler, "Block ask formatting", "decision", formatted,
		"Which transport ships first?")
	before := documentMarkdown(t, handler, issue.PrimaryArtifactID)

	edited := sessionRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+askID, map[string]any{
		"urgency": "high", "actor": sessionActor(),
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("urgency-only edit: status=%d body=%s", edited.Code, edited.Body.String())
	}
	settleDocument(t, handler, issue.PrimaryArtifactID, issue.Key, "after-urgency")

	markdown := documentMarkdown(t, handler, issue.PrimaryArtifactID)
	for _, want := range []string{"**transport**", "[first](https://example.test/rfc)", `urgency="high"`} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("urgency-only edit lost %q:\nbefore:\n%s\nafter:\n%s", want, before, markdown)
		}
	}
	if ask := readBlockAsk(t, handler, askID); ask.Urgency != "high" || ask.Question != "Which transport ships first?" {
		t.Fatalf("urgency-only edit changed the question: urgency=%q question=%q", ask.Urgency, ask.Question)
	}
}

// The inner paragraph a human's comment is anchored to keeps its block id through an edit that
// does not rewrite it, so the anchor stays attached (LEGION-265 review, PROBE D).
func TestUrgencyOnlyEditLeavesAnAnchoredCommentAttached(t *testing.T) {
	handler, _ := blockAskHandler(t)
	issue, askID := seedBlockAsk(t, handler, "Block ask anchor", "decision", transportAsk, "Which transport?")

	commented := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body":   "Why not both?",
		"anchor": map[string]any{"artifact": "spec", "quote": "Which transport?"},
	}, "alice")
	if commented.Code != http.StatusCreated {
		t.Fatalf("anchor a comment inside the ask block: status=%d body=%s", commented.Code, commented.Body.String())
	}
	comment := decodeBody[model.Comment](t, commented)
	if comment.Anchor == nil || comment.Anchor.Orphaned {
		t.Fatalf("the seeded comment is not anchored: %#v", comment.Anchor)
	}

	edited := sessionRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+askID, map[string]any{
		"urgency": "blocking", "actor": sessionActor(),
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("urgency-only edit: status=%d body=%s", edited.Code, edited.Body.String())
	}
	settleDocument(t, handler, issue.PrimaryArtifactID, issue.Key, "after-anchor")

	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+comment.ID, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read the anchored comment: status=%d body=%s", read.Code, read.Body.String())
	}
	after := decodeBody[struct {
		Comment model.Comment `json:"comment"`
	}](t, read).Comment
	if after.Anchor == nil || after.Anchor.Orphaned {
		t.Fatalf("the urgency-only edit orphaned the comment's anchor: %#v", after.Anchor)
	}
}

// Re-sending the values the block already holds writes nothing, which the live path reports as
// ErrNoChanges. That is the ask the caller asked for, not a server fault (LEGION-265 review,
// PROBE A2).
func TestRepeatingABlockAskEditSucceeds(t *testing.T) {
	handler, _ := blockAskHandler(t)
	_, askID := seedBlockAsk(t, handler, "Block ask repeat", "decision", transportAsk, "Which transport?")

	for round := range 2 {
		edited := sessionRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+askID, map[string]any{
			"question": "Which transport ships first?", "actor": sessionActor(),
		})
		if edited.Code != http.StatusOK {
			t.Fatalf("round %d edit: status=%d body=%s", round, edited.Code, edited.Body.String())
		}
	}
	if ask := readBlockAsk(t, handler, askID); ask.Question != "Which transport ships first?" {
		t.Fatalf("question after two identical edits = %q", ask.Question)
	}
}

// A rewritten body is the children the markdown pipeline produces, so the bullet list carries the
// list attributes every other document write gives it rather than a hand-built node's defaults.
func TestBlockAskOptionsEditWritesAParsedList(t *testing.T) {
	handler, database := blockAskHandler(t)
	issue, askID := seedBlockAsk(t, handler, "Block ask list attrs", "decision", transportAsk, "Which transport?")
	seeded := askBlockListAttrs(t, database, issue.PrimaryArtifactID)

	edited := sessionRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+askID, map[string]any{
		"options": []map[string]string{{"label": "REST", "description": "Now"}, {"label": "Queue"}},
		"actor":   sessionActor(),
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("options edit: status=%d body=%s", edited.Code, edited.Body.String())
	}
	settleDocument(t, handler, issue.PrimaryArtifactID, issue.Key, "after-options")

	ask := readBlockAsk(t, handler, askID)
	if len(ask.Options) != 2 || ask.Options[0].Description != "Now" || ask.Options[1].Label != "Queue" ||
		ask.Options[1].Description != "" {
		t.Fatalf("options = %#v", ask.Options)
	}
	if written := askBlockListAttrs(t, database, issue.PrimaryArtifactID); written != seeded {
		t.Fatalf("the rewritten list attributes = %s, want the parser's own %s", written, seeded)
	}
}

// askBlockListAttrs is the ask block's bullet-list attributes as the stored document holds them.
func askBlockListAttrs(t *testing.T, database *store.Store, artifactID string) string {
	t.Helper()
	var markdown string
	if err := database.Pool.QueryRow(context.Background(),
		`select markdown from artifact_versions where artifact_id = $1 order by number desc limit 1`,
		artifactID).Scan(&markdown); err != nil {
		t.Fatalf("read the settled document: %v", err)
	}
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatalf("parse the settled document: %v", err)
	}
	attrs := "(no list)"
	var walk func(*pmdoc.Node)
	walk = func(node *pmdoc.Node) {
		if node.Type == "bullet_list" {
			keys := make([]string, 0, len(node.Attrs))
			for name := range node.Attrs {
				if name == pmdoc.BlockIDAttr {
					continue
				}
				keys = append(keys, fmt.Sprintf("%s=%v", name, node.Attrs[name]))
			}
			sort.Strings(keys)
			attrs = strings.Join(keys, " ")
			return
		}
		for _, child := range node.Children {
			walk(child)
		}
	}
	walk(tree)
	return attrs
}

// An idempotent retry re-sends the whole ask. A field it names but does not change is not
// rewritten: rebuilding nodes that already say the same thing would mint fresh inner block ids
// and orphan the anchors inside them for no change at all (LEGION-265 review round 3, P3-1).
func TestIdempotentBlockAskEditRewritesNothing(t *testing.T) {
	handler, _ := blockAskHandler(t)
	const formatted = ":::ask{#decision urgency=\"med\" multiple=\"false\"}\nWhich **transport** ships [first](https://example.test/rfc)?\n\n" +
		"- REST: Matches the platform\n- gRPC: Adds streaming\n:::\n"
	issue, askID := seedBlockAsk(t, handler, "Block ask idempotent", "decision", formatted,
		"Which transport ships first?")

	commented := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body":   "Still open?",
		"anchor": map[string]any{"artifact": "spec", "quote": "Which transport ships first?"},
	}, "alice")
	if commented.Code != http.StatusCreated {
		t.Fatalf("anchor a comment inside the ask block: status=%d body=%s", commented.Code, commented.Body.String())
	}
	comment := decodeBody[model.Comment](t, commented)

	settleDocument(t, handler, issue.PrimaryArtifactID, issue.Key, "before-idempotent")
	beforeMarkdown := documentMarkdown(t, handler, issue.PrimaryArtifactID)
	beforeVersions := len(documentVersions(t, handler, issue.PrimaryArtifactID))
	current := readBlockAsk(t, handler, askID)

	// The whole ask, exactly as it reads now - what a retry sends.
	options := make([]map[string]string, 0, len(current.Options))
	for _, option := range current.Options {
		options = append(options, map[string]string{"label": option.Label, "description": option.Description})
	}
	retried := sessionRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+askID, map[string]any{
		"question": current.Question,
		"options":  options,
		"multiple": current.Multiple,
		"urgency":  current.Urgency,
		"actor":    sessionActor(),
	})
	if retried.Code != http.StatusOK {
		t.Fatalf("idempotent retry: status=%d body=%s", retried.Code, retried.Body.String())
	}

	if got := documentMarkdown(t, handler, issue.PrimaryArtifactID); got != beforeMarkdown {
		t.Fatalf("the idempotent retry rewrote the document:\nbefore:\n%s\nafter:\n%s", beforeMarkdown, got)
	}
	if got := len(documentVersions(t, handler, issue.PrimaryArtifactID)); got != beforeVersions {
		t.Fatalf("the idempotent retry wrote %d versions", got-beforeVersions)
	}
	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+comment.ID, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read the anchored comment: status=%d body=%s", read.Code, read.Body.String())
	}
	after := decodeBody[struct {
		Comment model.Comment `json:"comment"`
	}](t, read).Comment
	if after.Anchor == nil || after.Anchor.Orphaned {
		t.Fatalf("the idempotent retry orphaned the comment's anchor: %#v", after.Anchor)
	}
}
