package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/heavybodies"
)

// A document write of each heavy body gets the verdict the reader gives it: the eight shapes of
// heavybodies.Shapes are stored or refused 400 INVALID_MARKDOWN, and both deeply nested
// bare-directive bodies are refused the same way. The mapping from the reader's refusal to that
// code lives in one place (writeDocumentError, server.go), and until this test no API test
// reached it for a body refused for how deeply it nests.
//
// The browser suite used to make this claim, writing the same bodies through this same route and
// holding each to a second of wall clock as well (heavy-document-writes.e2e.ts,
// nested-directive.e2e.ts). Nothing here reads a clock: two wall times a shared CI runner took
// cannot separate a regression from the runner's own noise (INFRA-1013, INFRA-985). What the
// bodies cost is measured in pmdoc's own CPU time
// (TestHeavyBodiesAtTheBlockCapAreStoredOrRefusedInTime) and by BenchmarkHeavyDocumentWrite
// beside this file; what a writer is answered is gated here.
func TestHeavyDocumentWriteVerdicts(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)
	write := func(t *testing.T, name, body string, stored bool) {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
			"name": name, "content": body,
		}, "alice")
		if stored {
			if response.Code != http.StatusCreated {
				t.Fatalf("%s (%d characters): status=%d body=%s, want 201", name, len(body), response.Code, response.Body.String())
			}
			return
		}
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_MARKDOWN"`) {
			t.Fatalf("%s (%d characters): status=%d body=%s, want 400 INVALID_MARKDOWN", name, len(body), response.Code, response.Body.String())
		}
	}
	for index, shape := range heavybodies.Shapes() {
		t.Run(shape.Name, func(t *testing.T) {
			write(t, fmt.Sprintf("heavy-%d.md", index), shape.Grow(), shape.Refusal == "")
		})
	}
	for index, body := range heavybodies.NestedDirectives() {
		t.Run(body.Name, func(t *testing.T) {
			write(t, fmt.Sprintf("nested-directive-%d.md", index), body.Markdown, false)
		})
	}
}
