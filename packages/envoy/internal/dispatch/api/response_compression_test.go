package api

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
)

// answerUnder is a GET of target as alice, sending encoding as the request's Accept-Encoding, and
// sending no Accept-Encoding at all when encoding is empty - which is what the command-line tool
// and every agent client send today.
func answerUnder(tb testing.TB, handler http.Handler, target, encoding string) *httptest.ResponseRecorder {
	tb.Helper()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.Header.Set("X-Dispatch-User", "alice")
	if encoding != "" {
		request.Header.Set("Accept-Encoding", encoding)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// gunzip is the bytes a gzipped answer decodes to.
func gunzip(tb testing.TB, compressed []byte) []byte {
	tb.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		tb.Fatalf("read the gzip header of %d bytes: %v", len(compressed), err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		tb.Fatalf("decompress the answer: %v", err)
	}
	if err := reader.Close(); err != nil {
		tb.Fatalf("close the gzip reader: %v", err)
	}
	return decoded
}

// commentSentences are the lines a comment is built from, rotated by the comment's number so no
// two comments are the same text: a list of identical comments would compress far better than
// anything Dispatch holds.
var commentSentences = []string{
	"The deploy waits on the migration, which waits on the backfill.",
	"Staging is green since the rollout began; production is not.",
	"I read the plan again and the second step reverses the first.",
	"Nothing in the log says which replica answered that request.",
	"The reviewer asked for the numbers before the branch merges.",
	"We counted 412 rows, of which nine carry no project at all.",
	"That path is only reached from the importer, never the page.",
}

// issueWithComments creates one issue in its own project and posts comments comments on it, each
// of a page's own length, so its comment list is the compressible JSON this layer exists for.
func issueWithComments(tb testing.TB, handler http.Handler, project string, comments int) string {
	tb.Helper()
	writeAs(tb, handler, http.MethodPost, "/api/v1/projects",
		map[string]string{"key": project, "name": project + " project"}, http.StatusCreated)
	created := writeAs(tb, handler, http.MethodPost, "/api/v1/issues",
		map[string]string{"project": project, "title": "Compression", "spec": "A spec"}, http.StatusCreated)
	var issue struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &issue); err != nil {
		tb.Fatalf("decode the created issue %s: %v", created.Body.String(), err)
	}
	var body strings.Builder
	for comment := range comments {
		body.Reset()
		for line := range 8 {
			fmt.Fprintf(&body, "%s (%d)\n", commentSentences[(comment+line)%len(commentSentences)], comment)
		}
		writeAs(tb, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
			map[string]any{"body": body.String()}, http.StatusCreated)
	}
	return issue.Key
}

// writeAs posts one JSON write as alice and fails unless it is answered with want.
func writeAs(tb testing.TB, handler http.Handler, method, target string, body any, want int) *httptest.ResponseRecorder {
	tb.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		tb.Fatalf("encode the body of %s %s: %v", method, target, err)
	}
	request := httptest.NewRequest(method, target, bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "alice")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != want {
		tb.Fatalf("%s %s: status=%d body=%s, want %d", method, target, response.Code, response.Body.String(), want)
	}
	return response
}

// An answer is gzipped when the request says the caller takes gzip, and the bytes it decodes to
// are exactly the bytes the same request is answered without the header. Nothing else about the
// answer changes: same status, same headers but Content-Encoding.
func TestAnAnswerIsGzippedForACallerThatTakesGzip(t *testing.T) {
	handler := newTestHandler(t)
	target := "/api/v1/issues/" + issueWithComments(t, handler, "GZIP", 4) + "/comments"

	plain := answerUnder(t, handler, target, "")
	if plain.Code != http.StatusOK {
		t.Fatalf("GET %s: status=%d body=%s", target, plain.Code, plain.Body.String())
	}
	if got := plain.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("GET %s with no Accept-Encoding: Content-Encoding = %q, want none", target, got)
	}
	if plain.Body.Len() <= compressibleMinimum {
		t.Fatalf("GET %s answers %d bytes, which is under the %d this test must be above", target, plain.Body.Len(), compressibleMinimum)
	}

	compressed := answerUnder(t, handler, target, "gzip")
	if compressed.Code != plain.Code {
		t.Errorf("GET %s under gzip: status=%d, want %d", target, compressed.Code, plain.Code)
	}
	if got := compressed.Header().Get("Content-Encoding"); got != "gzip" {
		t.Errorf("GET %s under gzip: Content-Encoding = %q, want gzip", target, got)
	}
	if decoded := gunzip(t, compressed.Body.Bytes()); !bytes.Equal(decoded, plain.Body.Bytes()) {
		t.Errorf("GET %s under gzip decodes to %d bytes, want the %d bytes the same request is answered without the header", target, len(decoded), plain.Body.Len())
	}
	if compressed.Body.Len() >= plain.Body.Len() {
		t.Errorf("GET %s under gzip is %d bytes, want fewer than the %d it answers uncompressed", target, compressed.Body.Len(), plain.Body.Len())
	}
	// Every other header is the answer's own, and Content-Length never stands beside an encoding.
	for name, want := range plain.Header() {
		if name == "Content-Encoding" {
			continue
		}
		if got := compressed.Header().Values(name); strings.Join(got, ", ") != strings.Join(want, ", ") {
			t.Errorf("GET %s under gzip: %s = %v, want %v", target, name, got, want)
		}
	}
	for name := range compressed.Header() {
		if name != "Content-Encoding" && plain.Header().Values(name) == nil {
			t.Errorf("GET %s under gzip carries %s, which the uncompressed answer does not", target, name)
		}
	}
	if got := compressed.Header().Get("Content-Length"); got != "" {
		t.Errorf("GET %s under gzip: Content-Length = %q, want none on a compressed answer", target, got)
	}
}

// Only an Accept-Encoding that takes gzip is answered with gzip: a caller that sends none, one
// that names other encodings, and one that refuses gzip outright are each written the bytes they
// were written before this layer existed.
func TestOnlyAnAcceptEncodingThatTakesGzipIsAnsweredWithIt(t *testing.T) {
	handler := newTestHandler(t)
	target := "/api/v1/issues/" + issueWithComments(t, handler, "ENCO", 4) + "/comments"
	plain := answerUnder(t, handler, target, "").Body.Bytes()

	for _, test := range []struct {
		encoding string
		gzipped  bool
	}{
		{"", false},
		{"identity", false},
		{"deflate", false},
		{"br", false},
		{"gzip;q=0", false},
		{"gzip;q=0, *", false},
		{"*;q=0", false},
		{"gzip", true},
		{"GZIP", true},
		{"gzip, deflate, br", true},
		{"deflate;q=0.5, gzip;q=0.9", true},
		{"br;q=1.0, *;q=0.1", true},
	} {
		t.Run(test.encoding, func(t *testing.T) {
			answer := answerUnder(t, handler, target, test.encoding)
			if answer.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", answer.Code, answer.Body.String())
			}
			switch encoding := answer.Header().Get("Content-Encoding"); {
			case test.gzipped && encoding != "gzip":
				t.Fatalf("Content-Encoding = %q, want gzip", encoding)
			case !test.gzipped && encoding != "":
				t.Fatalf("Content-Encoding = %q, want none", encoding)
			}
			got := answer.Body.Bytes()
			if test.gzipped {
				got = gunzip(t, got)
			}
			if !bytes.Equal(got, plain) {
				t.Fatalf("the answer under %q is not the answer without it: %d bytes against %d", test.encoding, len(got), len(plain))
			}
		})
	}
}

// An answer too small to be worth a compressor - a refusal, a 404, the small writes a page makes
// while a person types - goes out as it is however the caller asks.
func TestAnAnswerUnderTheThresholdIsNotCompressed(t *testing.T) {
	handler := newTestHandler(t)
	const target = "/api/v1/issues/GONE-1"

	answer := answerUnder(t, handler, target, "gzip")
	if answer.Code != http.StatusNotFound {
		t.Fatalf("GET %s: status=%d body=%s, want 404", target, answer.Code, answer.Body.String())
	}
	if answer.Body.Len() >= compressibleMinimum {
		t.Fatalf("GET %s answers %d bytes, which is over the %d threshold this test must be under", target, answer.Body.Len(), compressibleMinimum)
	}
	if got := answer.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("GET %s under gzip: Content-Encoding = %q, want none under the threshold", target, got)
	}
	if !bytes.Equal(answer.Body.Bytes(), answerUnder(t, handler, target, "").Body.Bytes()) {
		t.Errorf("GET %s under gzip is not the answer the same request gets without the header", target)
	}
}

// A download is the stored bytes themselves, under the type they were stored as - an image, an
// archive, a PDF - and is never compressed, whatever the caller takes.
func TestADownloadIsNotCompressed(t *testing.T) {
	handler := newTestHandler(t)
	issue := issueWithComments(t, handler, "DOWN", 0)
	pixels := strings.Repeat("pixels, and plenty of them, all alike. ", 100)
	uploaded := uploadImage(t, handler, issue, "picture.png", pixels)
	target := "/api/v1/issues/" + issue + "/artifacts/" + uploaded.Artifact.Slug + "/versions/1"

	answer := answerUnder(t, handler, target, "gzip")
	if answer.Code != http.StatusOK {
		t.Fatalf("download: status=%d body=%s", answer.Code, answer.Body.String())
	}
	if got := answer.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("download under gzip: Content-Encoding = %q, want none", got)
	}
	if !bytes.Equal(answer.Body.Bytes(), pngBytes(pixels)) {
		t.Errorf("download under gzip is %d bytes, want the %d stored", answer.Body.Len(), len(pngBytes(pixels)))
	}
	if got := answer.Header().Get("Content-Length"); got == "" {
		t.Error("download under gzip carries no Content-Length, which an uncompressed answer keeps")
	}
}

// The live socket still upgrades for a caller that takes gzip, and carries its frames: the
// upgrade hijacks the connection, which a wrapper that swallowed http.Hijacker would break.
func TestTheLiveSocketUpgradesForACallerThatTakesGzip(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	seedHubSettings(t, rig.database)
	header := asUser("alice")
	header.Set("Accept-Encoding", "gzip")
	client := rig.dial(t, header)
	client.send(t, deliverySubscribe("live", presetView(deliveryLiveTimeline, 7, "")))
	client.snapshot(t, 10*time.Second, "live")
}

// A handler that takes the connection reaches the connection through this layer: the upgrade's
// own answer goes out, and the frames after it are the socket's.
func TestAHandlerThatHijacksReachesTheConnection(t *testing.T) {
	upgrader := gws.Upgrader{}
	upgrades := make(chan error, 1)
	server := httptest.NewServer(compressAnswers(func(w http.ResponseWriter, r *http.Request) {
		socket, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			upgrades <- err
			return
		}
		defer socket.Close()
		upgrades <- socket.WriteMessage(gws.TextMessage, []byte("a frame"))
	}))
	defer server.Close()

	conn, response, err := gws.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), http.Header{"Accept-Encoding": {"gzip"}})
	if err != nil {
		t.Fatalf("upgrade through the compression layer: %v", err)
	}
	defer conn.Close()
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: status=%d, want 101", response.StatusCode)
	}
	if got := response.Header.Get("Content-Encoding"); got != "" {
		t.Errorf("upgrade: Content-Encoding = %q, want none on a socket", got)
	}
	kind, frame, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read the socket's frame: %v", err)
	}
	if kind != gws.TextMessage || string(frame) != "a frame" {
		t.Errorf("the socket carried kind %d %q, want a text frame of %q", kind, frame, "a frame")
	}
	if err := <-upgrades; err != nil {
		t.Fatalf("the handler's own write: %v", err)
	}
}

// BenchmarkCompressingAnAnswer measures what this layer costs on one answer of about 100 KB - an
// issue's comment list, the largest thing the dashboard reads often - and reports the bytes a
// caller is written with the header and without it. It drives the real route once to hold a real
// answer, then times the compression of exactly those bytes, so the figure is this layer's own
// and not Postgres's.
func BenchmarkCompressingAnAnswer(b *testing.B) {
	handler, _, _ := newTestServer(b, testServerOptions{})
	target := "/api/v1/issues/" + issueWithComments(b, handler, "BENCH", 113) + "/comments"
	answer := answerUnder(b, handler, target, "").Body.Bytes()
	if len(answer) < 90_000 || len(answer) > 130_000 {
		b.Fatalf("the comment list is %d bytes, want about 100 KB; change the comment count", len(answer))
	}
	written := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(answer)
	}
	compressing := compressAnswers(written)
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.Header.Set("Accept-Encoding", "gzip")

	measured := &countingWriter{header: http.Header{}}
	compressing(measured, request)

	b.ResetTimer()
	for range b.N {
		compressing(&countingWriter{header: http.Header{}}, request)
	}
	b.StopTimer()
	// Reported after the loop: ResetTimer drops the metrics reported before it.
	b.ReportMetric(float64(len(answer)), "bytes-plain")
	b.ReportMetric(float64(measured.written), "bytes-gzip")
}

// countingWriter is a response writer that counts the bytes written to it and keeps none, so a
// benchmark measures the compression rather than a buffer growing.
type countingWriter struct {
	header  http.Header
	written int
}

func (w *countingWriter) Header() http.Header { return w.header }

func (w *countingWriter) Write(body []byte) (int, error) {
	w.written += len(body)
	return len(body), nil
}

func (w *countingWriter) WriteHeader(int) {}
