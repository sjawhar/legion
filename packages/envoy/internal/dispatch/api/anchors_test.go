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

func TestBrowserAnchorResolvesQuoteAfterSelectedRangeMovesOutOfBounds(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Moved browser anchor", "prefix target")

	shifted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "replace", "find": "prefix ", "with": ""}},
	}, "alice")
	if shifted.Code != http.StatusOK {
		t.Fatalf("remove anchor prefix: status=%d body=%s", shifted.Code, shifted.Body.String())
	}

	resolved := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"anchor":   map[string]any{"artifact": "spec", "from": 7, "to": 13, "quote": "target"},
		"question": "Why target?",
	}, "alice")
	if resolved.Code != http.StatusCreated {
		t.Fatalf("create moved browser anchor: status=%d body=%s", resolved.Code, resolved.Body.String())
	}
	ask := decodeBody[struct {
		Anchor model.Anchor `json:"anchor"`
	}](t, resolved)
	if ask.Anchor.From != 0 || ask.Anchor.To != 6 {
		t.Fatalf("moved browser anchor = %#v, want target at [0,6)", ask.Anchor)
	}

	removed := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "replace", "find": "target", "with": "gone"}},
	}, "alice")
	if removed.Code != http.StatusOK {
		t.Fatalf("remove anchor quote: status=%d body=%s", removed.Code, removed.Body.String())
	}
	stale := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"anchor":   map[string]any{"artifact": "spec", "from": 7, "to": 13, "quote": "target"},
		"question": "Why target?",
	}, "alice")
	if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), `"code":"ANCHOR_STALE"`) {
		t.Fatalf("create stale moved browser anchor: status=%d body=%s", stale.Code, stale.Body.String())
	}
}

func TestBrowserAnchorRequiresOccurrenceWhenShiftedQuoteRepeats(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Repeated browser anchor", "brown X brown")

	shifted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "insert", "markdown": "Note: ", "before": "start"}},
	}, "alice")
	if shifted.Code != http.StatusOK {
		t.Fatalf("shift repeated browser anchor: status=%d body=%s", shifted.Code, shifted.Body.String())
	}

	occurrence := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"anchor":   map[string]any{"artifact": "spec", "from": 8, "to": 13, "quote": "brown", "occurrence": 1},
		"question": "Why the second brown?",
	}, "alice")
	if occurrence.Code != http.StatusCreated {
		t.Fatalf("create repeated browser anchor with occurrence: status=%d body=%s", occurrence.Code, occurrence.Body.String())
	}
	ask := decodeBody[struct {
		Anchor model.Anchor `json:"anchor"`
	}](t, occurrence)
	if ask.Anchor.From != 14 || ask.Anchor.To != 19 {
		t.Fatalf("occurrence browser anchor = %#v, want second brown at [14,19)", ask.Anchor)
	}

	ambiguous := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"anchor":   map[string]any{"artifact": "spec", "from": 8, "to": 13, "quote": "brown"},
		"question": "Why the selected brown?",
	}, "alice")
	if ambiguous.Code != http.StatusConflict || !strings.Contains(ambiguous.Body.String(), `"code":"ANCHOR_STALE"`) || !strings.Contains(ambiguous.Body.String(), "occurs 2 times") {
		t.Fatalf("create ambiguous browser anchor: status=%d body=%s", ambiguous.Code, ambiguous.Body.String())
	}
}
