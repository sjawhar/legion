package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Writes add up, so a write may not leave a document heavier than one document may hold and
// heavier than it was. Inserts each well within one write's element limit are refused with 413
// CAP_EXCEEDED, saying what to do, once the document would pass it - 42 KB of `)_` at a time, and a
// thousand headings at a time - and the document still reads, its text and its blocks. Without the
// bound, four to six `)_` inserts or eighteen of headings left a document the server could no
// longer load: the next insert answered 500, and its reads 503.
func TestRepeatedInsertsCannotGrowADocumentPastWhatItCanStoreAndRead(t *testing.T) {
	for _, test := range []struct {
		name, chunk string
		inserts     int
	}{
		{"42 KB of )_", strings.Repeat(")_", 21_000), 6},
		{"a thousand headings", strings.Repeat("# a\n", 1_000), 20},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := newTestHandler(t)
			issue := createInteractionIssue(t, handler, "TEST", "Repeated growth", "Before.\n")
			refused := 0
			for index := range test.inserts {
				response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
					"ops": []map[string]string{{"op": "insert", "after": "end", "markdown": test.chunk}},
				}, "alice")
				if response.Code == http.StatusOK {
					continue
				}
				if response.Code != http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), `"code":"CAP_EXCEEDED"`) ||
					!strings.Contains(response.Body.String(), "shorten the change, or split the document") {
					t.Fatalf("insert %d: status=%d body=%.500s, want 200 or 413 CAP_EXCEEDED saying to shorten the change", index+1, response.Code, response.Body.String())
				}
				refused = index + 1
				t.Logf("insert %d refused: %.300s", refused, response.Body.String())
				break
			}
			if refused == 0 {
				t.Fatalf("all %d inserts were taken, want the document's growth refused", test.inserts)
			}
			for _, read := range []string{"/text", "/blocks"} {
				if response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+read, nil, "alice"); response.Code != http.StatusOK {
					t.Fatalf("%s after insert %d was refused: status=%d body=%.300s, want 200", read, refused, response.Code, response.Body.String())
				}
			}
		})
	}
}

// An accepted suggestion adds the caller's text as an edit does: on a document that weighs what one
// document may hold, an accept that would make it heavier is refused with 413 CAP_EXCEEDED, saying
// to shorten the change, and the document keeps its text.
func TestAnAcceptCannotGrowADocumentPastWhatOneDocumentMayHold(t *testing.T) {
	handler := newTestHandler(t)
	// 16,384 headings of four elements each: the 65,536 one document may hold.
	issue := createInteractionIssue(t, handler, "TEST", "Accepted growth", strings.Repeat("# a\n", 16_383)+"# target\n")
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body":       "more",
		"anchor":     map[string]any{"artifact": "spec", "quote": "target"},
		"suggestion": map[string]string{"replace_with": "target **and more**"},
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("suggest: status=%d body=%.300s", created.Code, created.Body.String())
	}
	comment := decodeBody[model.Comment](t, created)
	accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice")
	if accepted.Code != http.StatusRequestEntityTooLarge || !strings.Contains(accepted.Body.String(), `"code":"CAP_EXCEEDED"`) ||
		!strings.Contains(accepted.Body.String(), "shorten the change, or split the document") {
		t.Fatalf("accept: status=%d body=%.500s, want 413 CAP_EXCEEDED saying to shorten the change", accepted.Code, accepted.Body.String())
	}
	if text := documentMarkdown(t, handler, issue.PrimaryArtifactID); !strings.HasSuffix(text, "# target\n") {
		t.Fatalf("the document after the refused accept ends %q, want its own last heading", text[max(0, len(text)-40):])
	}
}
