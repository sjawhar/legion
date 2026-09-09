package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func TestBrowserAnchorAcceptsMatchingQuoteAtSelectedRange(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Exact browser anchor", "The quick brown fox")

	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"anchor":   map[string]any{"artifact": "spec", "from": 10, "to": 15, "quote": "brown"},
		"question": "Why brown?",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create exact browser anchor: status=%d body=%s", created.Code, created.Body.String())
	}
	ask := decodeBody[struct {
		Anchor model.Anchor `json:"anchor"`
	}](t, created)
	if ask.Anchor.Quote != "brown" || ask.Anchor.From != 10 || ask.Anchor.To != 15 {
		t.Fatalf("exact browser anchor = %#v, want brown at [10,15)", ask.Anchor)
	}
}

func TestBrowserAnchorResolvesShiftedQuoteNearOriginalRange(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Shifted browser anchor", "The quick brown fox")

	shifted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "insert", "markdown": "Note: ", "before": "start"}},
	}, "alice")
	if shifted.Code != http.StatusOK {
		t.Fatalf("shift live document: status=%d body=%s", shifted.Code, shifted.Body.String())
	}

	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"anchor":   map[string]any{"artifact": "spec", "from": 10, "to": 15, "quote": "brown"},
		"question": "Why brown?",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create shifted browser anchor: status=%d body=%s", created.Code, created.Body.String())
	}
	ask := decodeBody[struct {
		Anchor model.Anchor `json:"anchor"`
	}](t, created)
	if ask.Anchor.Quote != "brown" || ask.Anchor.From != 16 || ask.Anchor.To != 21 {
		t.Fatalf("shifted browser anchor = %#v, want brown at [16,21)", ask.Anchor)
	}
}

func TestBrowserAnchorRejectsRemovedQuote(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Stale browser anchor", "The quick brown fox")

	changed := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "replace", "find": "brown", "with": "red"}},
	}, "alice")
	if changed.Code != http.StatusOK {
		t.Fatalf("remove selected quote: status=%d body=%s", changed.Code, changed.Body.String())
	}

	stale := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"anchor":   map[string]any{"artifact": "spec", "from": 10, "to": 15, "quote": "brown"},
		"question": "Why brown?",
	}, "alice")
	if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), `"code":"ANCHOR_STALE"`) || !strings.Contains(stale.Body.String(), "brown") {
		t.Fatalf("create stale browser anchor: status=%d body=%s", stale.Code, stale.Body.String())
	}
}
