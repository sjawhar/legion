package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// storeNumberAnchorArtifactID rewrites the ask's anchor so its artifact_id is the JSON number 5,
// beside a block_artifact_id, the same shape storeUncastableAnchor writes: asks_anchor_artifact
// (migration 0032) casts an anchor only where block_artifact_id is null, so no index and no census
// has ever read this row's artifact_id. The guarded cast answers null for it, and the decode of
// the anchor column must answer an anchor naming no document rather than failing on a number where
// model.Anchor holds a string.
func storeNumberAnchorArtifactID(t *testing.T, database *store.Store, askID, blockDocument string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		update asks
		set anchor = jsonb_set(anchor, '{artifact_id}', '5'), block_artifact_id = $2
		where id = $1
	`, askID, blockDocument); err != nil {
		t.Fatalf("store the anchor whose artifact_id is a json number: %v", err)
	}
}

// Every ask route that serves an anchor answers over an ask whose stored artifact_id is a JSON
// number: the issue's ask list, the single-ask read and the issue's event log. That ask resolves
// to no anchor document - a number names none - and the canonically anchored ask beside it keeps
// the document it had.
func TestAskRoutesAnswerOverAnAnchorArtifactIDStoredAsANumber(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Numeric anchor", "The quick brown fox jumps over the lazy dog")
	blockDocument, _ := secondDocument(t, handler, issue.Key)
	canonical := openAnchoredAsk(t, handler, issue.Key, "spec", "brown", "Why brown?")
	number := openAnchoredAsk(t, handler, issue.Key, "spec", "lazy", "Why lazy?")
	storeNumberAnchorArtifactID(t, database, number, blockDocument)
	want := anchorDocument(t, database, issue.PrimaryArtifactID)

	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list the issue's asks: status=%d body=%s", listed.Code, listed.Body.String())
	}
	asks := map[string]model.Ask{}
	for _, ask := range decodeBody[[]model.Ask](t, listed) {
		asks[ask.ID] = ask
	}
	if len(asks) != 2 {
		t.Fatalf("the issue's ask list = %s, want both asks", listed.Body.String())
	}
	if got := asks[canonical].AnchorArtifact; got == nil || *got != want {
		t.Errorf("canonically anchored ask's anchor document = %#v, want %#v", got, want)
	}
	if got := asks[number].AnchorArtifact; got != nil {
		t.Errorf("numeric anchor ask's anchor document = %#v, want none: a number names no document", *got)
	}

	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+number, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read the numeric anchor ask: status=%d body=%s", read.Code, read.Body.String())
	}
	ask := decodeBody[struct {
		Ask model.Ask `json:"ask"`
	}](t, read).Ask
	if ask.AnchorArtifact != nil {
		t.Errorf("single ask read: the numeric anchor's document = %#v, want none", *ask.AnchorArtifact)
	}
	if ask.Anchor == nil || ask.Anchor.ArtifactID != "" {
		t.Errorf("single ask read: the numeric anchor = %#v, want one naming no document", ask.Anchor)
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
	if len(opened) != 2 {
		t.Fatalf("the issue's events = %s, want an ask.opened event for both asks", events.Body.String())
	}
	if got := opened[canonical]; got == nil || *got != want {
		t.Errorf("canonically anchored ask event's anchor document = %#v, want %#v", got, want)
	}
	if got := opened[number]; got != nil {
		t.Errorf("numeric anchor ask event's anchor document = %#v, want none: a number names no document", *got)
	}
}
