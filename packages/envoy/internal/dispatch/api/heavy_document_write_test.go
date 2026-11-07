package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/heavybodies"
)

// heavyWrite is one heavy body as this package writes it: the name its row carries, the bytes
// that go out, and whether the write is answered 201 rather than refused.
type heavyWrite struct {
	name, body string
	stored     bool
}

// heavyWrites is every heavy body of pmdoc/heavybodies flattened into the writes this package
// makes of them, the shapes grown to full size (Grow) first and the nested bare-directive bodies
// after: the order the verdict test's subtests and BenchmarkHeavyDocumentWrite's sub-benchmarks
// are named in, which is why both take their rows from here rather than flattening the two lists
// apart.
func heavyWrites() []heavyWrite {
	shapes, nested := heavybodies.Shapes(), heavybodies.NestedDirectives()
	writes := make([]heavyWrite, 0, len(shapes)+len(nested))
	for _, shape := range shapes {
		writes = append(writes, heavyWrite{name: shape.Name, body: shape.Grow(), stored: shape.Refusal == ""})
	}
	for _, directive := range nested {
		writes = append(writes, heavyWrite{name: directive.Name, body: directive.Markdown})
	}
	return writes
}

// A document write of each heavy body gets the verdict the reader gives it: the nine shapes of
// heavybodies.Shapes are stored or refused 400 INVALID_MARKDOWN, and both deeply nested
// bare-directive bodies are refused the same way. The mapping from the reader's refusal to that
// code lives in one place (writeHandlerError, server.go), and until this test no API test
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
	for index, heavy := range heavyWrites() {
		t.Run(heavy.name, func(t *testing.T) {
			write(t, fmt.Sprintf("heavy-%d.md", index), heavy.body, heavy.stored)
		})
	}
}
