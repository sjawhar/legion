package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Writes add up, so a write may not leave a document's markdown past what one upload may hold, by
// an upload's own measures - 1 MiB, and 65,536 elements as an upload's parse counts them - and
// bigger than it was. Inserts each well within both are refused with 413 CAP_EXCEEDED, saying what
// to do, once the document would pass either, whatever makes it grow: prose and a code block's text
// by their bytes, `)_`, headings, hard breaks, list items and table rows by their elements. The
// document still reads, its text and its blocks, and whatever the inserts left uploads back from its
// own text. Without the bound, thirty-two 900 KB inserts of prose grew one document to 29.5 MB, and
// four to six `)_` inserts or eighteen of headings left one the server could no longer load: the
// next insert answered 500, and its reads 503.
func TestRepeatedInsertsCannotGrowADocumentPastWhatOneUploadMayHold(t *testing.T) {
	tableSpec := "| a | b |\n| - | - |\n| A10 | b |\n"
	for _, test := range []struct {
		name, spec, after, chunk string
		inserts                  int
	}{
		{"900 KB of prose", "Before.\n", "end", strings.Repeat("word ", 180_000), 3},
		{"900 KB of a code block's text", "Before.\n", "end", "```\n" + strings.Repeat("a line of code, forty bytes long; more\n", 22_500) + "```\n", 3},
		{"42 KB of )_", "Before.\n", "end", strings.Repeat(")_", 21_000), 6},
		{"a thousand headings", "Before.\n", "end", strings.Repeat("# a\n", 1_000), 20},
		{"two thousand hard breaks", "Before.\n", "end", strings.Repeat("a  \n", 2_000) + "a\n", 12},
		{"three thousand list items", "Before.\n", "end", strings.Repeat("- a\n", 3_000), 8},
		{"a thousand table rows", tableSpec, "A10", strings.Repeat("| x | y |\n", 1_000), 10},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := newTestHandler(t)
			issue := createInteractionIssue(t, handler, "TEST", "Repeated growth", test.spec)
			refused := 0
			for index := range test.inserts {
				response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
					"ops": []map[string]string{{"op": "insert", "after": test.after, "markdown": test.chunk}},
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
			text := documentMarkdown(t, handler, issue.PrimaryArtifactID)
			uploaded := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{"name": "spec.md"}, "spec.md", "text/markdown", []byte(text), "alice")
			if uploaded.Code != http.StatusCreated {
				t.Fatalf("upload of the document's own %d-byte text: status=%d body=%.500s, want 201", len(text), uploaded.Code, uploaded.Body.String())
			}
		})
	}
}

// A new document is measured as an upload is: front matter, which an upload's parse does not
// count, beside 16,384 headings, which weigh the 65,536 elements one document may hold, is a spec
// the server takes.
func TestASpecOfFrontMatterAndTheMostHeadingsIsTaken(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Front matter", "---\ntitle: a spec\n---\n\n"+strings.Repeat("# a\n", 16_384))
	if text := documentMarkdown(t, handler, issue.PrimaryArtifactID); !strings.HasPrefix(text, "---\ntitle: a spec\n---\n") || strings.Count(text, "# a\n") != 16_384 {
		t.Fatalf("the spec reads back %d bytes opening %q, want its front matter and 16,384 headings", len(text), text[:min(len(text), 40)])
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
