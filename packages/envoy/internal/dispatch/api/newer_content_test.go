package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A document holding an element this build does not know was written by a newer build while two
// builds serve one database. Every route that starts from it - the reads, an edit (what `dispatch
// doc-edit` calls) and a named version - answers 409 DOC_NEWER_CONTENT with one message, whether a
// room holds the document or only the store does. The message says to try again in one minute and
// not to replace or rebuild the document, and an upload over the document is refused, so the
// element stays.
func TestEveryRouteAnswersADocumentHoldingNewerContentAlike(t *testing.T) {
	for _, resident := range []bool{true, false} {
		name := "stored"
		if resident {
			name = "resident"
		}
		t.Run(name, func(t *testing.T) {
			handler, _, deps := newTestServer(t, testServerOptions{})
			issue := createArtifactIssue(t, handler)
			created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
				"name": "newer.md", "content": "before\n",
			}, "alice")
			if created.Code != http.StatusCreated {
				t.Fatalf("create document: status=%d body=%s", created.Code, created.Body.String())
			}
			artifact := decodeBody[struct {
				Artifact model.Artifact `json:"artifact"`
			}](t, created).Artifact
			service := deps.Docs.(*docs.Service)
			if err := service.InjectUnknownElementForTest(context.Background(), artifact.ID); err != nil {
				t.Fatalf("write an element this build does not know: %v", err)
			}
			if !resident {
				if err := service.Evict(context.Background(), artifact.ID); err != nil {
					t.Fatalf("evict the document: %v", err)
				}
			}
			base := "/api/v1/artifacts/" + artifact.ID
			messages := map[string]string{}
			for _, route := range []struct {
				name, method, path string
				body               any
			}{
				{"text", http.MethodGet, base + "/text", nil},
				{"blocks", http.MethodGet, base + "/blocks", nil},
				{"edits", http.MethodPost, base + "/edits", map[string]any{
					"ops": []map[string]string{{"op": "insert", "after": "end", "markdown": "More.\n"}},
				}},
				{"versions", http.MethodPost, base + "/versions", map[string]string{"summary": "checkpoint"}},
			} {
				response := dispatchRequest(t, handler, route.method, route.path, route.body, "alice")
				answer := decodeBody[struct {
					Code  string `json:"code"`
					Error string `json:"error"`
				}](t, response)
				if response.Code != http.StatusConflict || answer.Code != "DOC_NEWER_CONTENT" || !strings.Contains(answer.Error, "Try again in one minute. Do not replace or rebuild the document") || strings.Contains(answer.Error, "If you replace") || !strings.Contains(answer.Error, docs.UnknownElementForTest) {
					t.Fatalf("%s %s: status=%d body=%s, want 409 DOC_NEWER_CONTENT naming the element, the retry and that no replacement or rebuild may follow", route.method, route.name, response.Code, response.Body.String())
				}
				messages[route.name] = answer.Error
			}
			for name, message := range messages {
				if message != messages["text"] {
					t.Fatalf("%s answered %q, /text %q: want one message on every route", name, message, messages["text"])
				}
			}

			upload := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{"name": "newer.md"}, "newer.md", "text/markdown", []byte("replaced\n"), "alice")
			if upload.Code != http.StatusConflict || !strings.Contains(upload.Body.String(), `"code":"DOC_NEWER_CONTENT"`) {
				t.Fatalf("upload over newer content: status=%d body=%s, want 409 DOC_NEWER_CONTENT", upload.Code, upload.Body.String())
			}
			text := dispatchRequest(t, handler, http.MethodGet, base+"/text", nil, "alice")
			if text.Code != http.StatusConflict || !strings.Contains(text.Body.String(), `"code":"DOC_NEWER_CONTENT"`) {
				t.Fatalf("read after the refused upload: status=%d body=%s, want the element still there (409 DOC_NEWER_CONTENT)", text.Code, text.Body.String())
			}
		})
	}
}

// The test hook POST /api/v1/artifacts/{id}/_test/newer-content gives a browser test a document
// holding newer content: after it, the document answers 409 DOC_NEWER_CONTENT naming the element.
// Without DISPATCH_TEST_HOOKS the route is not mounted, and the document is unchanged.
func TestTheNewerContentHookWritesAnElementThisBuildDoesNotKnow(t *testing.T) {
	for _, hooks := range []bool{true, false} {
		handler, _, _ := newTestServer(t, testServerOptions{testHooks: hooks})
		issue := createArtifactIssue(t, handler)
		created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
			"name": "newer.md", "content": "before\n",
		}, "alice")
		if created.Code != http.StatusCreated {
			t.Fatalf("create document: status=%d body=%s", created.Code, created.Body.String())
		}
		base := "/api/v1/artifacts/" + decodeBody[struct {
			Artifact model.Artifact `json:"artifact"`
		}](t, created).Artifact.ID
		hook := dispatchRequest(t, handler, http.MethodPost, base+"/_test/newer-content", nil, "alice")
		text := dispatchRequest(t, handler, http.MethodGet, base+"/text", nil, "alice")
		if !hooks {
			if hook.Code != http.StatusNotFound || text.Code != http.StatusOK {
				t.Fatalf("without the test hooks: the hook answered %d %s and /text %d %s, want 404 and the document unchanged", hook.Code, hook.Body.String(), text.Code, text.Body.String())
			}
			continue
		}
		if hook.Code != http.StatusNoContent {
			t.Fatalf("the hook: status=%d body=%s, want 204", hook.Code, hook.Body.String())
		}
		if text.Code != http.StatusConflict || !strings.Contains(text.Body.String(), `"code":"DOC_NEWER_CONTENT"`) || !strings.Contains(text.Body.String(), docs.UnknownElementForTest) {
			t.Fatalf("/text after the hook: status=%d body=%s, want 409 DOC_NEWER_CONTENT naming %s", text.Code, text.Body.String(), docs.UnknownElementForTest)
		}
	}
}
