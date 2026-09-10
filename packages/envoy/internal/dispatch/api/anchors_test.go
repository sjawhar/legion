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

func TestQuoteAnchorWritesServerMark(t *testing.T) {
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Quote anchor", "The quick brown fox")

	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Why brown?",
		"anchor":   map[string]any{"artifact": "spec", "quote": "brown"},
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create quote anchor: status=%d body=%s", created.Code, created.Body.String())
	}
	ask := decodeBody[struct {
		ID     string `json:"id"`
		Anchor struct {
			ArtifactID string `json:"artifact_id"`
			MarkID     string `json:"mark_id"`
			Version    int    `json:"version"`
			Quote      string `json:"quote"`
			Orphaned   bool   `json:"orphaned"`
		} `json:"anchor"`
	}](t, created)
	if ask.Anchor.ArtifactID != issue.PrimaryArtifactID || ask.Anchor.MarkID != ask.ID || ask.Anchor.Version != 1 || ask.Anchor.Quote != "brown" || ask.Anchor.Orphaned {
		t.Fatalf("quote anchor = %#v, want mark-backed brown at version 1", ask.Anchor)
	}
	if quote, err := documentService.VerifyMark(context.Background(), issue.PrimaryArtifactID, docs.MarkAsk, ask.ID); err != nil || quote != "brown" {
		t.Fatalf("server-written ask mark = %q, %v", quote, err)
	}
	text := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice")
	if text.Code != http.StatusOK || strings.Contains(text.Body.String(), "<span") {
		t.Fatalf("marked document text: status=%d body=%s", text.Code, text.Body.String())
	}
}

func TestMarkAnchorVerifiesBrowserMark(t *testing.T) {
	var documentService *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Browser mark", "The quick brown fox")
	if _, err := documentService.MarkQuote(context.Background(), issue.PrimaryArtifactID, docs.MarkSpec{
		Kind: docs.MarkComment,
		ID:   "m-1",
		By:   model.Actor{Kind: "user", ID: "alice"},
	}, "quick", nil); err != nil {
		t.Fatalf("write browser mark: %v", err)
	}

	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body":   "why",
		"anchor": map[string]any{"artifact": "spec", "mark_id": "m-1"},
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("record browser mark: status=%d body=%s", created.Code, created.Body.String())
	}
	comment := decodeBody[struct {
		ID     string `json:"id"`
		Anchor struct {
			MarkID string `json:"mark_id"`
			Quote  string `json:"quote"`
		} `json:"anchor"`
	}](t, created)
	if comment.Anchor.MarkID != "m-1" || comment.Anchor.Quote != "quick" {
		t.Fatalf("browser-mark anchor = %#v, want m-1 / quick", comment.Anchor)
	}
	if projection, found := findMarkProjection(t, database, issue.PrimaryArtifactID, "m-1"); !found || projection["text"] != "why" {
		t.Fatalf("browser-mark projection = %#v found=%t, want m-1 record", projection, found)
	}
	if _, found := findMarkProjection(t, database, issue.PrimaryArtifactID, comment.ID); found {
		t.Fatalf("browser-mark projection unexpectedly uses row id %q", comment.ID)
	}
}

func TestMarkAnchorMissingAfterWaitIs409(t *testing.T) {
	var documentService *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour, MarkWait: 50 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Missing browser mark", "The quick brown fox")

	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body":   "why",
		"anchor": map[string]any{"artifact": "spec", "mark_id": "ghost"},
	}, "alice")
	if created.Code != http.StatusConflict || !strings.Contains(created.Body.String(), `"code":"ANCHOR_MISSING"`) {
		t.Fatalf("record missing browser mark: status=%d body=%s", created.Code, created.Body.String())
	}
	var comments int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from comments where issue_key = $1`, issue.Key).Scan(&comments); err != nil {
		t.Fatalf("count comments: %v", err)
	}
	if comments != 0 {
		t.Fatalf("missing browser mark recorded %d comments", comments)
	}
}

func TestAnchorInputRequiresQuoteXorMarkID(t *testing.T) {
	tests := []struct {
		name   string
		anchor map[string]any
		code   string
	}{
		{name: "artifact only", anchor: map[string]any{"artifact": "spec"}, code: "INVALID_ANCHOR"},
		{name: "quote and mark id", anchor: map[string]any{"artifact": "spec", "quote": "brown", "mark_id": "m-1"}, code: "INVALID_ANCHOR"},
		{name: "empty quote", anchor: map[string]any{"artifact": "spec", "quote": ""}, code: "INVALID_ANCHOR"},
		{name: "legacy range", anchor: map[string]any{"artifact": "spec", "from": 0, "to": 3}, code: "INVALID_JSON"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := newTestHandler(t)
			issue := createInteractionIssue(t, handler, "TEST", "Invalid anchor", "The quick brown fox")
			created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
				"question": "Why?",
				"anchor":   test.anchor,
			}, "alice")
			if created.Code != http.StatusBadRequest || !strings.Contains(created.Body.String(), `"code":"`+test.code+`"`) {
				t.Fatalf("invalid anchor: status=%d body=%s", created.Code, created.Body.String())
			}
		})
	}
}

func TestAnchorQuoteNotFoundIs404(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Missing quote", "The quick brown fox")
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Why purple?",
		"anchor":   map[string]any{"artifact": "spec", "quote": "purple"},
	}, "alice")
	if created.Code != http.StatusNotFound || !strings.Contains(created.Body.String(), `"code":"TARGET_NOT_FOUND"`) {
		t.Fatalf("create missing quote anchor: status=%d body=%s", created.Code, created.Body.String())
	}
}

func TestReplyMayNotCarryAnAnchor(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Anchored reply", "The quick brown fox")
	root := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{"body": "root"}, "alice")
	if root.Code != http.StatusCreated {
		t.Fatalf("create root comment: status=%d body=%s", root.Code, root.Body.String())
	}
	comment := decodeBody[struct {
		ID string `json:"id"`
	}](t, root)
	ask := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{"question": "Why?"}, "alice")
	if ask.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", ask.Code, ask.Body.String())
	}
	askID := decodeBody[struct {
		ID string `json:"id"`
	}](t, ask).ID

	for _, body := range []map[string]any{
		{"body": "reply", "reply_to": comment.ID, "anchor": map[string]any{"artifact": "spec", "quote": "quick"}},
		{"body": "answer", "ask_id": askID, "anchor": map[string]any{"artifact": "spec", "quote": "quick"}},
	} {
		created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", body, "alice")
		if created.Code != http.StatusBadRequest || !strings.Contains(created.Body.String(), `"code":"INVALID_COMMENT"`) {
			t.Fatalf("anchored reply: status=%d body=%s", created.Code, created.Body.String())
		}
	}
}
