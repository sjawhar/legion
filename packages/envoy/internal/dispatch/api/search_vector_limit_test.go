package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/store/searchtest"
)

// A document whose whole search vector would pass Postgres's limit on one tsvector is versioned by
// the edit that writes its text and by an upload of it, and is found by the words that open it
// (LEGION-505). Before 0071 each version write failed with `string is too long for tsvector` and
// answered 500. The text is 1,000 paragraphs, 2,000 elements, so the vector is what fails, not the
// document's own bounds. Settlement's own version write is the docs package's
// TestSettlementVersionsADocumentPastTheSearchVectorLimit.
func TestADocumentPastTheSearchVectorLimitVersionsAndIsFound(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)
	markdown := searchtest.DistinctWords(100_000, "\n\n")

	before := len(documentVersions(t, handler, issue.PrimaryArtifactID))
	edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "insert", "after": "end", "markdown": markdown}},
	}, "alice")
	if edited.Code != http.StatusOK {
		t.Fatalf("insert the words: status=%d body=%.300s", edited.Code, edited.Body.String())
	}
	if after := len(documentVersions(t, handler, issue.PrimaryArtifactID)); after != before+1 {
		t.Fatalf("the edit left %d versions, want %d", after, before+1)
	}

	uploaded := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "distinct-words.md",
	}, "body.md", "text/markdown", []byte(markdown), "alice")
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload the words: status=%d body=%.300s", uploaded.Code, uploaded.Body.String())
	}
	upload := decodeBody[struct {
		Artifact struct {
			ID string `json:"id"`
		} `json:"artifact"`
	}](t, uploaded)

	found := map[string]bool{}
	for _, result := range searchResponse(t, handler, "q=w000001").Results {
		if result.Kind == "document" {
			found[result.ID] = true
		}
	}
	for name, id := range map[string]string{"the edited spec": issue.PrimaryArtifactID, "the upload": upload.Artifact.ID} {
		if !found[id] {
			t.Errorf("search for the first word did not find %s (%s)", name, id)
		}
	}
}

// A title whose whole search vector would pass Postgres's limit on one tsvector is far past
// contracts.IssueTitleMax, so creating an issue with it, forced or not, and retitling one to it are
// refused before the duplicate check reads a title: the issue titled with its first word, which
// that check names as its near-duplicate, goes unnamed. Without the cap such a creation answered 500
// (`string is too long for tsvector`), and with the bounded vector alone it was created, and every
// later creation in its project read its 800 KB in the duplicate check. The store's
// TestTextPastTheSearchVectorLimitIsWrittenAndIndexedFromItsOpening writes such a title straight
// into issues, where the trigger still indexes its opening.
func TestAnIssueTitlePastTheSearchVectorLimitIsRefused(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "WORDS", "name": "Words"}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	first := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "WORDS", "title": "w000001"}, "alice")
	if first.Code != http.StatusCreated {
		t.Fatalf("create an issue titled w000001: status=%d body=%.300s", first.Code, first.Body.String())
	}
	word := decodeBody[struct {
		Key string `json:"key"`
	}](t, first).Key

	// The words end in a space, which the routes trim before they count.
	title := strings.TrimSpace(searchtest.DistinctWords(100_000, " "))
	want := fmt.Sprintf("title is %d characters over the %d-character limit (%d/%d)",
		len(title)-contracts.IssueTitleMax, contracts.IssueTitleMax, len(title), contracts.IssueTitleMax)
	refused := func(what string, response *httptest.ResponseRecorder) {
		t.Helper()
		body := response.Body.String()
		refusal := decodeBody[struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		}](t, response)
		if response.Code != http.StatusBadRequest || refusal.Code != "CAP_EXCEEDED" || refusal.Error != want {
			t.Fatalf("%s: status=%d body=%.300s, want 400 CAP_EXCEEDED %q", what, response.Code, body, want)
		}
	}
	for _, force := range []bool{false, true} {
		refused(fmt.Sprintf("create the long title, force %t", force), dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "WORDS", "title": title, "force": force,
		}, "alice"))
	}
	refused("retitle "+word+" to the long title", dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+word, map[string]any{"title": title}, "alice"))

	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=WORDS", nil, "alice")
	issues := decodeBody[[]struct {
		Key   string `json:"key"`
		Title string `json:"title"`
	}](t, listed)
	if len(issues) != 1 || issues[0].Key != word || issues[0].Title != "w000001" {
		t.Fatalf("WORDS holds %+v, want only %s titled w000001", issues, word)
	}
}
