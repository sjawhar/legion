package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
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

// An ask's text and its answer are written into its block, so they are caller text in the
// document as an edit's is, and editing the asks of one document or answering them cannot grow it
// past what one upload may hold either: through these routes thirty-two option descriptions of
// 900 KB grew one document to 28.8 MB, on which a one-word edit then took the server past the
// production task's 1,024 MiB. The write that would pass the bound is refused with 413
// CAP_EXCEEDED, saying what to do; the ask keeps its text and stays open, and the document still
// reads.
func TestAskEditsAndAnswersCannotGrowADocumentPastWhatOneUploadMayHold(t *testing.T) {
	prose := strings.Repeat("word ", 180_000)
	for _, route := range []struct {
		name  string
		write func(handler http.Handler, askID string) *httptest.ResponseRecorder
		kept  func(ask model.Ask) bool
	}{
		{"PATCH /api/v1/asks/{id}", func(handler http.Handler, askID string) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+askID, map[string]any{
				"options": []map[string]string{{"label": "A", "description": prose}},
			}, "alice")
		}, func(ask model.Ask) bool { return ask.State == "open" && len(ask.Options) == 0 }},
		{"POST /api/v1/asks/{id}/answer", func(handler http.Handler, askID string) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{"text": prose}, "alice")
		}, func(ask model.Ask) bool { return ask.State == "open" && ask.Answer == nil }},
	} {
		t.Run(route.name, func(t *testing.T) {
			handler, _ := blockAskHandler(t)
			const asks = 3
			var spec strings.Builder
			spec.WriteString("Context\n")
			for index := range asks {
				fmt.Fprintf(&spec, "\n:::ask{#ask-%d urgency=\"med\" multiple=\"false\" state=\"open\"}\nQuestion %d?\n:::\n", index, index)
			}
			issue := createInteractionIssue(t, handler, "TEST", "Ask growth", spec.String())
			refused := 0
			for index := range asks {
				blockID := fmt.Sprintf("ask-%d", index)
				awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, blockID, fmt.Sprintf("Question %d?", index))
				askID := blockAskID(t, handler, issue.Key, blockID)
				response := route.write(handler, askID)
				if response.Code == http.StatusOK && refused == 0 {
					continue
				}
				if response.Code != http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), `"code":"CAP_EXCEEDED"`) ||
					!strings.Contains(response.Body.String(), "shorten the change, or split the document") {
					t.Fatalf("write %d: status=%d body=%.500s, want 200 until one is refused with 413 CAP_EXCEEDED saying to shorten the change, and 413 after it", index+1, response.Code, response.Body.String())
				}
				if ask := readBlockAsk(t, handler, askID); !route.kept(ask) {
					t.Fatalf("the ask after its refused write = %#v, want it as it was", ask)
				}
				if refused == 0 {
					refused = index + 1
					t.Logf("write %d refused: %.300s", refused, response.Body.String())
				}
			}
			if refused == 0 {
				t.Fatalf("all %d writes were taken, want the document's growth refused", asks)
			}
			for _, read := range []string{"/text", "/blocks"} {
				if response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+read, nil, "alice"); response.Code != http.StatusOK {
					t.Fatalf("%s after write %d was refused: status=%d body=%.300s, want 200", read, refused, response.Code, response.Body.String())
				}
			}
			if text := documentMarkdown(t, handler, issue.PrimaryArtifactID); len(text) > 1<<20 {
				t.Fatalf("the document's markdown is %d bytes, past the 1 MiB one upload may hold", len(text))
			}
		})
	}
}

// A suggestion's replacement and a comment's text live in the document's margin, which every load
// of the document builds, so they are weighed as the document is: a comment's record holds at
// most 256 KiB of text and the margin 1 MiB, and a comment that would pass either is refused with
// 413 CAP_EXCEEDED naming the limit and both sizes, with the document as it was. Ordinary use -
// dozens of comments, suggestions and replies of everyday length - is taken whole. Without the
// bound, thirty-two suggestions of 900 KB were taken and one cold websocket load of their document
// took 296 MiB.
func TestSuggestionsCannotGrowADocumentsMarginPastWhatItMayHold(t *testing.T) {
	handler := newTestHandler(t)
	var words strings.Builder
	for index := range 100 {
		fmt.Fprintf(&words, "q%02dx ", index)
	}
	spec := words.String() + "\n"
	suggest := func(issue string, quote int, replacement string) *httptest.ResponseRecorder {
		return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/comments", map[string]any{
			"body":       "a suggestion",
			"anchor":     map[string]any{"artifact": "spec", "quote": fmt.Sprintf("q%02dx", quote)},
			"suggestion": map[string]string{"replace_with": replacement},
		}, "alice")
	}
	refused := func(response *httptest.ResponseRecorder, want string) {
		t.Helper()
		if response.Code != http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), `"code":"CAP_EXCEEDED"`) || !strings.Contains(response.Body.String(), want) {
			t.Fatalf("status=%d body=%.500s, want 413 CAP_EXCEEDED saying %q", response.Code, response.Body.String(), want)
		}
	}

	issue := createInteractionIssue(t, handler, "TEST", "Margin", spec)
	stored := documentMarkdown(t, handler, issue.PrimaryArtifactID)
	refused(suggest(issue.Key, 0, strings.Repeat("a", 300_000)), "holds at most 256 KiB (262144 bytes) of text")
	for index := range 4 {
		if response := suggest(issue.Key, index+1, strings.Repeat("a", 230_000)); response.Code != http.StatusCreated {
			t.Fatalf("suggestion %d of four that fit the margin: status=%d body=%.300s", index+1, response.Code, response.Body.String())
		}
	}
	refused(suggest(issue.Key, 5, strings.Repeat("a", 230_000)), "a document's margin holds at most 1 MiB (1048576 bytes)")
	for _, read := range []string{"/text", "/blocks"} {
		if response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+read, nil, "alice"); response.Code != http.StatusOK {
			t.Fatalf("%s after the refused suggestion: status=%d body=%.300s", read, response.Code, response.Body.String())
		}
	}
	if text := documentMarkdown(t, handler, issue.PrimaryArtifactID); text != stored {
		t.Fatalf("the document after the suggestions reads %q, want it as it was, %q", text, stored)
	}

	ordinary := createInteractionIssue(t, handler, "ORD", "Ordinary margin", spec)
	sentence := strings.Repeat("A sentence of ordinary length. ", 16)
	for index := range 60 {
		var response *httptest.ResponseRecorder
		if index%3 == 0 {
			response = suggest(ordinary.Key, index, sentence)
		} else {
			response = dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+ordinary.Key+"/comments", map[string]any{
				"body":   sentence,
				"anchor": map[string]any{"artifact": "spec", "quote": fmt.Sprintf("q%02dx", index)},
			}, "alice")
		}
		if response.Code != http.StatusCreated {
			t.Fatalf("ordinary comment %d: status=%d body=%.300s", index+1, response.Code, response.Body.String())
		}
		if index%5 == 0 {
			root := decodeBody[model.Comment](t, response)
			reply := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+ordinary.Key+"/comments", map[string]any{"body": sentence, "reply_to": root.ID}, "alice")
			if reply.Code != http.StatusCreated {
				t.Fatalf("a reply to comment %d: status=%d body=%.300s", index+1, reply.Code, reply.Body.String())
			}
		}
	}
}
