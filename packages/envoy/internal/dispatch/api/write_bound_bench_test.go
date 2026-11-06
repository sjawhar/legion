package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/logtest"
)

// writeBoundProjectKey is the project every issue these benchmarks write to is opened in.
const writeBoundProjectKey = "BOUND"

// writeBoundParagraphs is how many paragraphs the document a version write and an upload rewrite
// holds: a ~20 KB spec, the size of a document people upload a new version of, so the markdown
// the write renders and the tree its commit stores are the size a real write carries.
const writeBoundParagraphs = 80

// writeBoundBody is the markdown a benchmarked comment and a benchmarked ask question carry: a
// few hundred bytes of the markdown an issue's activity actually holds, so the tree the write
// stores is the size a reader of that text is sent rather than a word's.
const writeBoundBody = `The deploy is **green** on staging.

I read the write path again and the cost is where the text is rendered, not where it is
stored:

- the markdown is parsed once by the write itself;
- the tree is stored after the transaction commits, on the request's own connection.

Next step is a benchmark of each write path at the base and at the head.`

// BenchmarkTheWritePathsThatStoreADisplayTree measures the four write paths A6 bounds - "each
// write path's added cost is at most 1.5 times the base by benchmark", over the document version
// write, an upload, a comment write and the event append - each as one request through the mux
// Register mounts, so what is timed is the whole write a caller waits for, the display tree its
// commit stores included.
//
// The four sub-benchmarks are those four paths:
//
//   - "the document version write" is POST /api/v1/artifacts/{id}/edits, the route that writes a
//     version through the document service's one version path (docs' writeVersionTx), whose
//     ledger stores that version's markdown tree when the transaction commits
//     (docs.Ledger.storeVersionTrees).
//   - "an upload" is POST /api/v1/issues/{key}/artifacts, the next version of a document that is
//     already there, which renders the uploaded markdown and writes the version row itself.
//   - "a comment write" is POST /api/v1/issues/{key}/comments, whose commit stores the body's
//     tree (api's storeDisplayTrees).
//   - "the event append" is PATCH /api/v1/asks/{id}. An append's trees are stored in the publish
//     path (storeEventDisplayTrees), and an ask edit is the write whose event payload carries
//     texts no row holds any more - the question and the option labels it replaced - so the
//     publish path is the only thing that stores them.
//
// Each path writes to an issue of its own, so one path's growing document or comment list never
// moves another's cost, and each iteration writes text the one before it did not: a run that
// wrote the same text twice would measure a tree store that finds its row already there, which
// is not what a write costs.
//
// The base arm is this same file at the branch's fork point, where no write stores a tree; the
// head arm is this file here. Both run the same requests, so the ratio between them is the added
// cost the bound is about.
func BenchmarkTheWritePathsThatStoreADisplayTree(b *testing.B) {
	logtest.SetDefault(b, slog.NewTextHandler(io.Discard, nil))
	// An hour's settlement delay keeps the document closer out of the timed writes: a settlement
	// firing mid-run would be timed as part of whichever write it landed in.
	handler, _, _ := newTestServer(b, testServerOptions{settle: time.Hour})
	writeBoundProject(b, handler)

	b.Run("the document version write", func(b *testing.B) {
		issue := writeBoundIssue(b, handler, "A document its version write edits")
		target := "/api/v1/artifacts/" + issue.PrimaryArtifactID + "/edits"
		revision := 0
		b.ReportAllocs()
		for b.Loop() {
			writeBoundRequest(b, handler, http.MethodPost, target, map[string]any{"ops": []map[string]any{{
				"op":   "replace",
				"find": fmt.Sprintf("Paragraph 7 revision %d", revision),
				"with": fmt.Sprintf("Paragraph 7 revision %d", revision+1),
			}}}, http.StatusOK)
			revision++
		}
	})

	b.Run("an upload", func(b *testing.B) {
		issue := writeBoundIssue(b, handler, "A document uploaded again")
		target := "/api/v1/issues/" + issue.Key + "/artifacts"
		version := 1
		b.ReportAllocs()
		for b.Loop() {
			writeBoundRequest(b, handler, http.MethodPost, target, map[string]any{
				"name": "spec.md", "content": writeBoundMarkdown(version),
			}, http.StatusCreated)
			version++
		}
	})

	b.Run("a comment write", func(b *testing.B) {
		issue := writeBoundIssue(b, handler, "An issue people comment on")
		target := "/api/v1/issues/" + issue.Key + "/comments"
		comment := 0
		b.ReportAllocs()
		for b.Loop() {
			writeBoundRequest(b, handler, http.MethodPost, target, map[string]any{
				"body": fmt.Sprintf("%s\n\nComment %d.", writeBoundBody, comment),
			}, http.StatusCreated)
			comment++
		}
	})

	b.Run("the event append", func(b *testing.B) {
		issue := writeBoundIssue(b, handler, "An issue whose ask is reworded")
		target := "/api/v1/asks/" + writeBoundAsk(b, handler, issue.Key)
		edit := 0
		b.ReportAllocs()
		for b.Loop() {
			writeBoundRequest(b, handler, http.MethodPatch, target, map[string]any{
				"question": fmt.Sprintf("%s\n\nWording %d.", writeBoundBody, edit),
				"options": []map[string]string{
					{"label": fmt.Sprintf("Ship it, wording %d", edit)},
					{"label": fmt.Sprintf("Hold it, wording %d", edit)},
				},
			}, http.StatusOK)
			edit++
		}
	})
}

// writeBoundProject records the project the benchmarks' issues are opened in.
func writeBoundProject(b *testing.B, handler http.Handler) {
	b.Helper()
	writeBoundRequest(b, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": writeBoundProjectKey, "name": "Write bound project",
	}, http.StatusCreated)
}

// writeBoundIssue opens one issue, whose spec is the document a version write and an upload
// rewrite, and answers its key and the id of that document.
func writeBoundIssue(b *testing.B, handler http.Handler, title string) struct {
	Key               string `json:"key"`
	PrimaryArtifactID string `json:"primary_artifact_id"`
} {
	b.Helper()
	response := writeBoundRequest(b, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": writeBoundProjectKey, "title": title, "spec": writeBoundMarkdown(0),
	}, http.StatusCreated)
	var issue struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &issue); err != nil {
		b.Fatalf("decode the created issue: %v", err)
	}
	if issue.Key == "" || issue.PrimaryArtifactID == "" {
		b.Fatalf("the created issue = %#v, want a key and a document", issue)
	}
	return issue
}

// writeBoundAsk opens the ask the event-append benchmark rewords, and answers its id.
func writeBoundAsk(b *testing.B, handler http.Handler, issueKey string) string {
	b.Helper()
	response := writeBoundRequest(b, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/asks", map[string]any{
		"question": writeBoundBody,
		"options":  []map[string]string{{"label": "Ship it"}, {"label": "Hold it"}},
		"urgency":  "low",
	}, http.StatusCreated)
	var ask struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &ask); err != nil {
		b.Fatalf("decode the created ask: %v", err)
	}
	if ask.ID == "" {
		b.Fatal("the created ask has no id")
	}
	return ask.ID
}

// writeBoundRequest makes one write through the server under benchmark, signed in as alice, and
// fails the benchmark where it was not answered want: a benchmark may never report the cost of a
// write the server refused.
func writeBoundRequest(b *testing.B, handler http.Handler, method, target string, body any, want int) *httptest.ResponseRecorder {
	b.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		b.Fatalf("encode the %s %s body: %v", method, target, err)
	}
	request := httptest.NewRequest(method, target, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "alice")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != want {
		b.Fatalf("%s %s = %d, want %d: %s", method, target, response.Code, want, response.Body.String())
	}
	return response
}

// writeBoundMarkdown is revision revision of the benchmarks' document: writeBoundParagraphs
// paragraphs in sections of twenty, of which paragraph 7 carries the revision number a version
// write moves.
func writeBoundMarkdown(revision int) string {
	var markdown strings.Builder
	markdown.WriteString("# Spec\n\n")
	for paragraph := range writeBoundParagraphs {
		if paragraph%20 == 0 {
			fmt.Fprintf(&markdown, "## Section %d\n\n", paragraph/20)
		}
		fmt.Fprintf(&markdown, "Paragraph %d revision %d: %s\n\n",
			paragraph, revision, strings.Repeat("lorem ipsum dolor sit amet ", 8))
	}
	return markdown.String()
}
