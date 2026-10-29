package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// An anchor artifact_id that spells a real document id in upper case, or inside braces, casts to
// that document's uuid. The readers that resolve an ask's anchor document (askRowFrom, behind the
// issue's ask list and the single-ask read, and the event log's ask hydration) answer what base
// answered for every anchor that casts: the document it names. Only a value that cannot cast
// resolves to none. Both rows here carry block_artifact_id too, the shape asks_anchor_artifact
// (0032) never cast, which is how an earlier release could leave such a spelling stored.
func TestAskReadersResolveAnUpperCaseOrBracedAnchorToItsDocument(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Anchor spellings", "The quick brown fox jumps over the lazy dog")
	blockDocument, _ := secondDocument(t, handler, issue.Key)
	spellings := map[string]string{
		"upper case": openAnchoredAsk(t, handler, issue.Key, "spec", "brown", "Why brown?"),
		"braces":     openAnchoredAsk(t, handler, issue.Key, "spec", "lazy", "Why lazy?"),
	}
	if _, err := database.Pool.Exec(context.Background(), `
		update asks
		set anchor = jsonb_set(anchor, '{artifact_id}', to_jsonb(case when id = $1 then upper(anchor->>'artifact_id') else '{' || (anchor->>'artifact_id') || '}' end)),
			block_artifact_id = $3
		where id in ($1, $2)
	`, spellings["upper case"], spellings["braces"], blockDocument); err != nil {
		t.Fatalf("respell the asks' anchors: %v", err)
	}
	want := anchorDocument(t, database, issue.PrimaryArtifactID)

	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list the issue's asks: status=%d body=%s", listed.Code, listed.Body.String())
	}
	inList := map[string]*model.AskAnchorArtifact{}
	for _, ask := range decodeBody[[]model.Ask](t, listed) {
		inList[ask.ID] = ask.AnchorArtifact
	}

	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if events.Code != http.StatusOK {
		t.Fatalf("read the issue's events: status=%d body=%s", events.Code, events.Body.String())
	}
	type askEvent struct {
		Type    string `json:"type"`
		Payload struct {
			ID             string                   `json:"id"`
			AnchorArtifact *model.AskAnchorArtifact `json:"anchor_artifact"`
		} `json:"payload"`
	}
	opened := map[string]*model.AskAnchorArtifact{}
	for _, event := range decodeBody[[]askEvent](t, events) {
		if event.Type == "ask.opened" {
			opened[event.Payload.ID] = event.Payload.AnchorArtifact
		}
	}

	for spelling, askID := range spellings {
		if got := inList[askID]; got == nil || *got != want {
			t.Errorf("issue ask list: the %s anchor's document = %#v, want %#v", spelling, got, want)
		}
		read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+askID, nil, "alice")
		if read.Code != http.StatusOK {
			t.Fatalf("read the %s ask: status=%d body=%s", spelling, read.Code, read.Body.String())
		}
		if got := decodeBody[struct {
			Ask model.Ask `json:"ask"`
		}](t, read).Ask.AnchorArtifact; got == nil || *got != want {
			t.Errorf("single ask read: the %s anchor's document = %#v, want %#v", spelling, got, want)
		}
		if got := opened[askID]; got == nil || *got != want {
			t.Errorf("issue events: the %s anchor's ask.opened document = %#v, want %#v", spelling, got, want)
		}
	}
}
