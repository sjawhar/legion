package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
)

// uploadVersionHistory is how many versions the benchmark spec has before its timed uploads.
const uploadVersionHistory = 28

// uploadVersionComments is how many anchored comments the benchmark spec carries: the production
// median per spec.
const uploadVersionComments = 25

// BenchmarkUploadVersion measures POST /api/v1/issues/{key}/artifacts uploading the next version
// of a ~98 KB spec of 400 paragraphs, 28 versions and 25 anchored comments in, each version
// rewriting 5% of the paragraphs. Before each upload, untimed, the spec's stored updates are made
// an hour old and its room is evicted, so every upload loads the document cold from its compacted
// history, as an upload to a spec nobody has open, minutes after its last version, does. The
// document service settles after an hour, keeping settlement out of the timed uploads.
func BenchmarkUploadVersion(b *testing.B) {
	// The access log's line for every request would split the benchmark's result line; warnings
	// and errors still reach stderr.
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	b.Cleanup(func() { slog.SetDefault(previous) })
	handler, database, deps := newTestServer(b, testServerOptions{settle: time.Hour})
	documents, ok := deps.Docs.(*docs.Service)
	if !ok {
		b.Fatalf("document service = %T, want *docs.Service", deps.Docs)
	}
	issueKey, artifactID := newUploadVersionSpec(b, handler)
	next := uploadVersionHistory + 1
	// One sub-benchmark keeps the setup above to a single run: the framework calls a benchmark
	// with no sub-benchmarks again for the requested iteration count.
	b.Run(fmt.Sprintf("versions=%d", uploadVersionHistory), func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			b.StopTimer()
			// The room's compaction as it closes folds only updates older than its tail window;
			// an hour old, every one of them is.
			if _, err := database.Pool.Exec(context.Background(), `update doc_updates set created_at = created_at - interval '1 hour' where artifact_id = $1`, artifactID); err != nil {
				b.Fatalf("backdate spec updates: %v", err)
			}
			if err := documents.Evict(context.Background(), artifactID); err != nil {
				b.Fatalf("evict spec room: %v", err)
			}
			b.StartTimer()
			uploadSpecVersion(b, handler, issueKey, next)
			next++
		}
	})
}

// newUploadVersionSpec creates an issue whose spec has uploadVersionHistory uploaded versions,
// with uploadVersionComments anchored comments spread over its history and its paragraphs.
func newUploadVersionSpec(tb testing.TB, handler http.Handler) (issueKey, artifactID string) {
	tb.Helper()
	if response := uploadVersionRequest(tb, handler, "/api/v1/projects", map[string]string{
		"key": "BENCH", "name": "Benchmark project",
	}); response.Code != http.StatusCreated {
		tb.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	response := uploadVersionRequest(tb, handler, "/api/v1/issues", map[string]string{
		"project": "BENCH", "title": "Upload benchmark",
	})
	if response.Code != http.StatusCreated {
		tb.Fatalf("create issue: status=%d body=%s", response.Code, response.Body.String())
	}
	var issue struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &issue); err != nil {
		tb.Fatalf("decode issue: %v", err)
	}
	comment := 0
	for version := 1; version <= uploadVersionHistory; version++ {
		uploaded := uploadSpecVersion(tb, handler, issue.Key, version)
		if version == 1 {
			var upload struct {
				Artifact struct {
					ID string `json:"id"`
				} `json:"artifact"`
			}
			if err := json.Unmarshal(uploaded.Body.Bytes(), &upload); err != nil {
				tb.Fatalf("decode upload: %v", err)
			}
			artifactID = upload.Artifact.ID
		}
		// Comment i follows version 1 + i*(history-1)/comments.
		for comment < uploadVersionComments && 1+comment*(uploadVersionHistory-1)/uploadVersionComments == version {
			quote := fmt.Sprintf("Paragraph %d revision", 8+16*comment)
			if response := uploadVersionRequest(tb, handler, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
				"body":   fmt.Sprintf("Comment %d on this paragraph.", comment),
				"anchor": map[string]any{"artifact": "spec", "quote": quote},
			}); response.Code != http.StatusCreated {
				tb.Fatalf("create comment %d: status=%d body=%s", comment, response.Code, response.Body.String())
			}
			comment++
		}
	}
	if comment != uploadVersionComments {
		tb.Fatalf("created %d comments, want %d", comment, uploadVersionComments)
	}
	if artifactID == "" {
		tb.Fatal("the first upload named no artifact")
	}
	return issue.Key, artifactID
}

// uploadSpecVersion uploads version of the spec and fails unless the upload succeeds.
func uploadSpecVersion(tb testing.TB, handler http.Handler, issueKey string, version int) *httptest.ResponseRecorder {
	tb.Helper()
	response := uploadVersionRequest(tb, handler, "/api/v1/issues/"+issueKey+"/artifacts", map[string]string{
		"name": "spec.md", "content": uploadVersionMarkdown(version),
	})
	if response.Code < 200 || response.Code > 299 {
		tb.Fatalf("upload version %d: status=%d body=%s", version, response.Code, response.Body.String())
	}
	return response
}

// uploadVersionRequest POSTs body as JSON, signed in as alice.
func uploadVersionRequest(tb testing.TB, handler http.Handler, target string, body any) *httptest.ResponseRecorder {
	tb.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		tb.Fatalf("encode request body: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "alice")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// uploadVersionMarkdown is version of a ~98 KB spec: 400 paragraphs in 20 sections, of which
// version rewrites the 5% where (paragraph+version)%20 == 0.
func uploadVersionMarkdown(version int) string {
	var b strings.Builder
	b.WriteString("# Spec\n\n")
	for p := range 400 {
		if p%20 == 0 {
			fmt.Fprintf(&b, "## Section %d\n\n", p/20)
		}
		rev := 0
		if version > 0 && (p+version)%20 == 0 {
			rev = version
		}
		fmt.Fprintf(&b, "Paragraph %d revision %d: %s\n\n", p, rev, strings.Repeat("lorem ipsum dolor sit amet ", 8))
	}
	return b.String()
}
