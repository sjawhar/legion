package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/logtest"
)

// benchmarkFeedComment is the text every comment a feed benchmark seeds carries: a few hundred
// bytes of the markdown an issue's activity actually holds, so the tree each event gains is the
// size a reader of that feed is sent rather than a word's.
const benchmarkFeedComment = `The deploy is **green** on staging.

I read the ` + "`listIssueEvents`" + ` path again and the warm read spends its time finding the
texts rather than reading them:

- the payload is decoded whole, as a map of raw members;
- every member is then walked a second time by the encoder.

Next step is a typed decode of the few fields that can hold a stored text.`

// BenchmarkTheActivityFeedsWarmRead measures GET /api/v1/issues/{key}/events as the 1.2x bound
// states it: the same feed read by a caller holding `Dispatch-Trees: 1`, which is sent every
// stored text's ProseMirror tree, and by the base reader, which sends no header and is sent none.
// Both arms go through the mux Register mounts - the access log, the compression layer and the
// handler - and take gzip, which is what a browser's warm read does, so the ratio between them is
// the ratio the bound is about.
//
// Two feeds are read. The first is the bound's own: 200 comment.created events, one shape. The
// second is mixed - comments, an ask opened and answered, messages, and issue updates, which hold
// no stored text at all - so the type-driven decode is exercised on every shape the feed carries
// and not only on a comment's.
func BenchmarkTheActivityFeedsWarmRead(b *testing.B) {
	logtest.SetDefault(b, slog.NewTextHandler(io.Discard, nil))

	handler, database, _, _ := newRegisteredTestServer(b, testServerOptions{})
	benchmarkFeedProject(b, database, "FEED")
	for _, feed := range []struct {
		name   string
		target string
	}{
		{"200 comment.created", benchmarkCommentFeed(b, handler, 200)},
		{"a mixed feed", benchmarkMixedFeed(b, handler)},
	} {
		for _, read := range []struct {
			name  string
			trees bool
		}{
			{"with trees", true},
			{"without trees", false},
		} {
			b.Run(feed.name+"/"+read.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					response := benchmarkFeedRead(b, handler, feed.target, read.trees)
					if response.Code != http.StatusOK {
						b.Fatalf("GET %s = %d, want 200", feed.target, response.Code)
					}
				}
			})
		}
	}
}

// benchmarkFeedRead is one warm read of a feed, with the trees header or without it, taking gzip
// as a browser does.
func benchmarkFeedRead(b *testing.B, handler http.Handler, target string, trees bool) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.Header.Set("X-Dispatch-User", "alice")
	request.Header.Set("Accept-Encoding", "gzip")
	if trees {
		request.Header.Set(displayTreesHeader, "1")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// benchmarkFeedProject records the project the benchmark's issues are opened in.
func benchmarkFeedProject(b *testing.B, database *store.Store, key string) {
	b.Helper()
	if _, err := database.Pool.Exec(b.Context(),
		`insert into projects (key, name) values ($1, $2)`, key, key+" project"); err != nil {
		b.Fatalf("seed project %s: %v", key, err)
	}
}

// benchmarkCommentFeed opens an issue, posts count comments on it, and answers the feed that
// reads them: the one shape the bound was stated over.
func benchmarkCommentFeed(b *testing.B, handler http.Handler, count int) string {
	b.Helper()
	key := benchmarkFeedIssue(b, handler, "A feed of comments")
	for index := range count {
		benchmarkFeedWrite(b, handler, http.MethodPost, "/api/v1/issues/"+key+"/comments", map[string]any{
			"body": fmt.Sprintf("%s\n\nComment %d.", benchmarkFeedComment, index),
		}, http.StatusCreated)
	}
	return benchmarkFeedTarget(b, handler, key, map[string]int{"comment.created": count})
}

// benchmarkMixedFeed opens a second issue whose feed carries every shape a type-driven decode has
// to tell apart: a comment's body, an ask's question, options, answer text and resolution reason,
// a message's body, and the issue updates, which hold no stored text and are read not at all.
func benchmarkMixedFeed(b *testing.B, handler http.Handler) string {
	b.Helper()
	key := benchmarkFeedIssue(b, handler, "A mixed feed")
	for index := range 20 {
		benchmarkFeedWrite(b, handler, http.MethodPost, "/api/v1/issues/"+key+"/comments", map[string]any{
			"body": fmt.Sprintf("%s\n\nComment %d.", benchmarkFeedComment, index),
		}, http.StatusCreated)
		benchmarkFeedWrite(b, handler, http.MethodPost, "/api/v1/issues/"+key+"/messages", map[string]any{
			"body": fmt.Sprintf("%s\n\nMessage %d.", benchmarkFeedComment, index),
		}, http.StatusCreated)
		benchmarkFeedWrite(b, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{
			"title": fmt.Sprintf("A mixed feed, %d", index),
		}, http.StatusOK)
		ask := benchmarkFeedWrite(b, handler, http.MethodPost, "/api/v1/issues/"+key+"/asks", map[string]any{
			"question": fmt.Sprintf("%s\n\nShip %d?", benchmarkFeedComment, index),
			"options": []map[string]string{
				{"label": "Ship it", "description": "The staging run is **green**."},
				{"label": "Hold", "description": "Wait for the next deploy."},
			},
		}, http.StatusCreated)
		var opened struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(ask.Body.Bytes(), &opened); err != nil {
			b.Fatalf("decode the opened ask: %v", err)
		}
		benchmarkFeedWrite(b, handler, http.MethodPost, "/api/v1/asks/"+opened.ID+"/answer", map[string]any{
			"selected":           []string{"Ship it"},
			"text":               benchmarkFeedComment,
			"expected_edited_at": nil,
		}, http.StatusOK)
	}
	return benchmarkFeedTarget(b, handler, key, map[string]int{
		"comment.created": 20,
		"message.created": 20,
		"ask.opened":      20,
		"ask.answered":    20,
		"issue.updated":   20,
	})
}

// benchmarkFeedIssue opens one issue of the benchmark's project.
func benchmarkFeedIssue(b *testing.B, handler http.Handler, title string) string {
	b.Helper()
	response := benchmarkFeedWrite(b, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "FEED", "title": title, "spec": "A benchmark's issue.",
	}, http.StatusCreated)
	var created struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		b.Fatalf("decode the created issue: %v", err)
	}
	return created.Key
}

// benchmarkFeedWrite makes one write through the server under benchmark and fails the benchmark
// where it was not answered want.
func benchmarkFeedWrite(b *testing.B, handler http.Handler, method, target string, body any, want int) *httptest.ResponseRecorder {
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

// benchmarkFeedTarget is the issue's feed as the page reads it - its newest page, newest first -
// checked before it is measured: the read answers one page holding at least the events seeded for
// each type, so a benchmark can never report the cost of a feed that lost them.
func benchmarkFeedTarget(b *testing.B, handler http.Handler, key string, want map[string]int) string {
	b.Helper()
	target := "/api/v1/issues/" + key + "/events?order=desc&limit=200"
	response := benchmarkFeedRead(b, handler, target, true)
	if response.Code != http.StatusOK {
		b.Fatalf("GET %s = %d, want 200: %s", target, response.Code, response.Body.String())
	}
	var events []struct {
		Type  string                     `json:"type"`
		Trees map[string]json.RawMessage `json:"trees"`
	}
	if err := json.Unmarshal(gunzipBenchmarkFeed(b, response), &events); err != nil {
		b.Fatalf("decode the feed of %s: %v", key, err)
	}
	counted := map[string]int{}
	for _, event := range events {
		counted[event.Type]++
	}
	for kind, least := range want {
		if counted[kind] < least {
			b.Fatalf("the feed of %s holds %d %s events, want %d of them in its page of %d",
				key, counted[kind], kind, least, len(events))
		}
	}
	return target
}

// gunzipBenchmarkFeed is the feed's bytes, decompressed where the answer took the gzip the read
// offered.
func gunzipBenchmarkFeed(b *testing.B, response *httptest.ResponseRecorder) []byte {
	b.Helper()
	if response.Header().Get("Content-Encoding") != "gzip" {
		return response.Body.Bytes()
	}
	return gunzip(b, response.Body.Bytes())
}
