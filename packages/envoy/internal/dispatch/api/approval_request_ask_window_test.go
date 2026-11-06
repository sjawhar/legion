package api

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// An approval request opens an ask, and meaning search ranks that ask by the windows of its
// parts. Those windows are written in the request's own transaction, stamped with the state the
// parts are in, so the moment the request answers the ask is windowed and current: the
// block-index tick's window arm owes it nothing (refs.ItemsOwedWindows), a window covers every
// part, and once the embed queue has run each of those windows holds a vector of the text it
// stands at. A request that left its windows to the tick would answer with the ask listed as
// owed and no window row to embed, so the ask would be unreachable by meaning until a tick
// caught up - up to docs.BlockIndexReconcileInterval (30 seconds) after the write.
func TestAnApprovalRequestLeavesItsAskWindowedAndCurrent(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "APPRW", "Window the request's ask", "A spec")

	requested := sessionRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests",
		map[string]any{"actor": sessionActor(), "summary": "Caps retries at three attempts."})
	if requested.Code != http.StatusCreated {
		t.Fatalf("request approval: status=%d body=%s", requested.Code, requested.Body.String())
	}
	opened := decodeBody[struct {
		Ask *model.Ask `json:"ask"`
	}](t, requested)
	if opened.Ask == nil {
		t.Fatalf("the request opened no ask: %s", requested.Body.String())
	}
	askID := opened.Ask.ID

	// The request is written: the document awaits the ask the answer named.
	approval := readApproval(t, handler, issue.PrimaryArtifactID)
	if approval.Approval == nil || approval.Approval.State != "awaiting" ||
		approval.Approval.AskID == nil || *approval.Approval.AskID != askID {
		t.Fatalf("the document's approval = %#v, want awaiting ask %s", approval.Approval, askID)
	}

	ctx := context.Background()
	owed, err := refs.ItemsOwedWindows(ctx, database.Pool, refs.ItemAsk, "", 50)
	if err != nil {
		t.Fatalf("list the asks owed their windows: %v", err)
	}
	if slices.Contains(owed, askID) {
		t.Fatalf("the window arm is owed ask %s the request has just written: %v", askID, owed)
	}

	parts := askPartBlockIDs(t, database, askID)
	if len(parts) == 0 {
		t.Fatalf("the ask %s the request opened holds no indexed part", askID)
	}
	ownerKey := refs.ItemAsk.WindowOwnerKey(askID)
	processAllPending(t, database, &fakeEmbedder{})

	var windows, current int
	if err := database.Pool.QueryRow(ctx, `
		select count(*), count(*) filter (where embedding is not null and embedded_hash = content_hash)
		from embeddings where kind = $1 and split_part(id, '#', 1) = $2
	`, refs.ItemAsk.WindowKind(), ownerKey).Scan(&windows, &current); err != nil {
		t.Fatalf("read the windows of ask %s: %v", askID, err)
	}
	if windows == 0 || current != windows {
		t.Fatalf("ask %s holds %d windows, %d of them carrying a vector of their current text; want every one of at least one",
			askID, windows, current)
	}

	states, err := refs.BlockEmbeddingStates(ctx, database.Pool, refs.ItemAsk.WindowKind(), ownerKey)
	if err != nil {
		t.Fatalf("read where meaning search stands on the parts of ask %s: %v", askID, err)
	}
	for _, part := range parts {
		if states[part] != model.BlockEmbedded {
			t.Fatalf("part %s of ask %s is %q, want %q", part, askID, states[part], model.BlockEmbedded)
		}
	}
}

// askPartBlockIDs is the block id of each part the index holds for ask askID, in part order: the
// question and each option, as indexAskParts wrote them.
func askPartBlockIDs(t *testing.T, database *store.Store, askID string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select block_id from ask_blocks where ask_id = $1::uuid and position is not null order by position
	`, askID)
	if err != nil {
		t.Fatalf("read the parts of ask %s: %v", askID, err)
	}
	defer rows.Close()
	var parts []string
	for rows.Next() {
		var part string
		if err := rows.Scan(&part); err != nil {
			t.Fatalf("read the parts of ask %s: %v", askID, err)
		}
		parts = append(parts, part)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the parts of ask %s: %v", askID, err)
	}
	return parts
}
