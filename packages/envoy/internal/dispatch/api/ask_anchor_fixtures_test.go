package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// openAnchoredAsk opens an ask anchored on the issue document named by slug, at quote.
func openAnchoredAsk(t *testing.T, handler http.Handler, issueKey, slug, quote, question string) string {
	t.Helper()
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/asks", map[string]any{
		"question": question,
		"options":  []map[string]string{{"label": "Yes"}, {"label": "No"}},
		"anchor":   map[string]any{"artifact": slug, "quote": quote},
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create the ask anchored on %q: status=%d body=%s", quote, created.Code, created.Body.String())
	}
	return decodeBody[model.Ask](t, created).ID
}

// secondDocument uploads a second document to the issue, the one a seeded ask names as its ask
// block so that asks_anchor_artifact (0032) never cast its anchor, and answers its id and slug.
func secondDocument(t *testing.T, handler http.Handler, issueKey string) (string, string) {
	t.Helper()
	upload := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/artifacts", map[string]any{
		"content": "Block document.\n", "name": "blocks.md",
	}, "alice")
	if upload.Code != http.StatusCreated {
		t.Fatalf("create the block document: status=%d body=%s", upload.Code, upload.Body.String())
	}
	artifact := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, upload).Artifact
	return artifact.ID, artifact.Slug
}

// storeAnchorArtifactID rewrites the ask's anchor so its artifact_id is the JSON value rawJSON -
// a string no uuid cast accepts, a number, a spelling only the cast reads - beside a
// block_artifact_id. That is the shape only a release before asks_anchor_artifact_id (migration
// 0148) could store: asks_anchor_artifact (0032) casts an anchor only where block_artifact_id is
// null, so no index and no census has ever read this row's artifact_id, and an unguarded cast in
// a read answers SQLSTATE 22P02 for the whole statement.
func storeAnchorArtifactID(t *testing.T, database *store.Store, askID, blockDocument, rawJSON string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		update asks
		set anchor = jsonb_set(anchor, '{artifact_id}', $3::jsonb), block_artifact_id = $2
		where id = $1
	`, askID, blockDocument, rawJSON); err != nil {
		t.Fatalf("store the anchor whose artifact_id is %s: %v", rawJSON, err)
	}
}

// anchorDocument is the document an ask read resolves an anchor to, read straight from artifacts,
// so a test compares the resolved fields against the row they are resolved from.
func anchorDocument(t *testing.T, database *store.Store, artifactID string) model.AskAnchorArtifact {
	t.Helper()
	var document model.AskAnchorArtifact
	if err := database.Pool.QueryRow(context.Background(), `
		select project_key, slug, name, is_primary from artifacts where id = $1
	`, artifactID).Scan(&document.Project, &document.Slug, &document.Name, &document.Primary); err != nil {
		t.Fatalf("read document %q: %v", artifactID, err)
	}
	return document
}

// issueAsks is the issue's ask list, by ask id: what the list route resolves each ask's anchor
// and block documents to.
func issueAsks(t *testing.T, handler http.Handler, issueKey string) map[string]model.Ask {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issueKey+"/asks", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("list the issue's asks: status=%d body=%s", response.Code, response.Body.String())
	}
	asks := map[string]model.Ask{}
	for _, ask := range decodeBody[[]model.Ask](t, response) {
		asks[ask.ID] = ask
	}
	return asks
}

// singleAsk is the ask the single-ask read answers, with the anchor it serves and the document it
// resolved that anchor to.
func singleAsk(t *testing.T, handler http.Handler, askID string) model.Ask {
	t.Helper()
	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+askID, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read ask %s: status=%d body=%s", askID, read.Code, read.Body.String())
	}
	return decodeBody[struct {
		Ask model.Ask `json:"ask"`
	}](t, read).Ask
}

// openedAskAnchors is the anchor document the issue's event log hydrates each ask.opened payload
// with (attachAskDocumentArtifacts), by ask id.
func openedAskAnchors(t *testing.T, handler http.Handler, issueKey string) map[string]*model.AskAnchorArtifact {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issueKey+"/events", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the issue's events: status=%d body=%s", response.Code, response.Body.String())
	}
	type askEvent struct {
		Type    string `json:"type"`
		Payload struct {
			ID             string                   `json:"id"`
			AnchorArtifact *model.AskAnchorArtifact `json:"anchor_artifact"`
		} `json:"payload"`
	}
	opened := map[string]*model.AskAnchorArtifact{}
	for _, event := range decodeBody[[]askEvent](t, response) {
		if event.Type == "ask.opened" {
			opened[event.Payload.ID] = event.Payload.AnchorArtifact
		}
	}
	return opened
}
