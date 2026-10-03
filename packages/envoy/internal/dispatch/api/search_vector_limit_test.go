package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// distinctWords is n words no two alike, `w000001 w000002 …`, joined by separator, with paragraph
// after every hundredth. Each word is a lexeme of its own, which a search vector holds as its seven
// bytes and five more, so 100,000 of them - 800 KB of text, inside every write's 1 MiB bound -
// make 1.2 MB of lexemes and positions: past the 1,048,575 bytes Postgres holds in one tsvector.
func distinctWords(n int, paragraph string) string {
	var text strings.Builder
	for word := 1; word <= n; word++ {
		fmt.Fprintf(&text, "w%06d", word)
		if word%100 == 0 {
			text.WriteString(paragraph)
		} else {
			text.WriteString(" ")
		}
	}
	return text.String()
}

// A document whose whole search vector would pass Postgres's limit on one tsvector is versioned by
// the edit that writes its text and by an upload of it, and is found by the words that open it
// (LEGION-505). Before 0068 each version write failed with `string is too long for tsvector` and
// answered 500. The text is 1,000 paragraphs, 2,000 elements, so the vector is what fails, not the
// document's own bounds. Settlement's own version write is the docs package's
// TestSettlementVersionsADocumentPastTheSearchVectorLimit.
func TestADocumentPastTheSearchVectorLimitVersionsAndIsFound(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)
	markdown := distinctWords(100_000, "\n\n")

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

// An issue whose title's whole search vector would pass the limit is created, is found by its key
// and the words that open its title, and is checked against its project's titles and they against
// it. The duplicate check reads every title through the same bound, and marks a candidate's
// headline with the words the two titles share, of which two titles of distinct words share tens
// of thousands. Before 0068 its creation answered 500 (`string is too long for tsvector`), and a
// headline query of every word the new title held overflowed Postgres's stack (`stack depth limit
// exceeded`), which a title whose vector fit reached as well.
func TestAnIssueTitlePastTheSearchVectorLimitIsCreatedAndChecked(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "WORDS", "name": "Words"}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	create := func(title string, force bool) *httptest.ResponseRecorder {
		return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "WORDS", "title": title, "force": force,
		}, "alice")
	}
	created := func(what string, response *httptest.ResponseRecorder) string {
		t.Helper()
		if response.Code != http.StatusCreated {
			t.Fatalf("create %s: status=%d body=%.300s", what, response.Code, response.Body.String())
		}
		return decodeBody[struct {
			Key string `json:"key"`
		}](t, response).Key
	}
	refused := func(what string, response *httptest.ResponseRecorder, candidate string) {
		t.Helper()
		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"POSSIBLE_DUPLICATE"`) || !strings.Contains(response.Body.String(), `"key":"`+candidate+`"`) {
			t.Fatalf("%s: status=%d body=%.300s, want 409 POSSIBLE_DUPLICATE naming %s", what, response.Code, response.Body.String(), candidate)
		}
	}

	word := created("an issue titled w000001", create("w000001", false))
	title := distinctWords(100_000, " ")
	// The long title holds w000001, all of the first issue's title, so that issue is its
	// near-duplicate, and its headline marks the one word they share.
	refused("the long title beside an issue titled with its first word", create(title, false), word)
	long := created("the long title, forced", create(title, true))
	// The same title again shares every word its vector holds with the stored one.
	refused("the long title again", create(title, false), long)

	for _, query := range []string{long, "w000001"} {
		found := false
		for _, result := range searchResponse(t, handler, "q="+query).Results {
			if result.Kind == "issue" && result.ID == long {
				found = true
			}
		}
		if !found {
			t.Errorf("search %q did not find %s", query, long)
		}
	}
}
