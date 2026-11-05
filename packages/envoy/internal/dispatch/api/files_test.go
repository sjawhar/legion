package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/files"
	"github.com/sjawhar/envoy/internal/dispatch/files/filestest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// fileIssue creates the project and issue a file upload lands on and returns the issue key.
func fileIssue(t *testing.T, handler http.Handler) string {
	t.Helper()
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "FILES", "name": "Files",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "FILES", "title": "Issue", "spec": "# Files",
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[struct {
		Key string `json:"key"`
	}](t, response).Key
}

type uploaded struct {
	Artifact struct {
		ID string `json:"id"`
	} `json:"artifact"`
	Version struct {
		Number int    `json:"number"`
		SHA256 string `json:"sha256"`
		Size   int    `json:"size"`
	} `json:"version"`
}

// versionRow is what a version's row holds of its bytes: nil when they live in the store.
func versionRow(t *testing.T, database *store.Store, artifactID string, number int) []byte {
	t.Helper()
	var content []byte
	if err := database.Pool.QueryRow(context.Background(), `select content from artifact_versions where artifact_id = $1 and number = $2`, artifactID, number).Scan(&content); err != nil {
		t.Fatalf("read version row: %v", err)
	}
	return content
}

func TestUploadsGoToTheFileStoreAndAreServedFromIt(t *testing.T) {
	memory := filestest.NewMemory()
	handler, database, _ := newTestServer(t, testServerOptions{files: memory})
	issue := fileIssue(t, handler)
	image := pngBytes(strings.Repeat("pixels", 4096))
	sha := files.SHA256(image)

	first := multipartRequest(t, handler, "/api/v1/issues/"+issue+"/artifacts", map[string]string{"name": "shot.png"}, "shot.png", "image/png", image, "alice")
	if first.Code != http.StatusCreated {
		t.Fatalf("upload: status=%d body=%s", first.Code, first.Body.String())
	}
	version := decodeBody[uploaded](t, first)
	if version.Version.SHA256 != sha || version.Version.Size != len(image) {
		t.Fatalf("version = %+v, want sha %s and size %d", version.Version, sha, len(image))
	}
	if row := versionRow(t, database, version.Artifact.ID, version.Version.Number); row != nil {
		t.Fatalf("the row holds %d bytes; with a store it should hold none", len(row))
	}
	if memory.Len() != 1 || memory.MIME(sha) != "image/png" {
		t.Fatalf("store holds %d objects (mime %q), want the one image as image/png", memory.Len(), memory.MIME(sha))
	}

	// The same bytes again, as a second version of the same name: one object, two rows.
	second := multipartRequest(t, handler, "/api/v1/issues/"+issue+"/artifacts", map[string]string{"name": "shot.png"}, "shot.png", "image/png", image, "alice")
	if second.Code != http.StatusCreated {
		t.Fatalf("second upload: status=%d body=%s", second.Code, second.Body.String())
	}
	duplicate := decodeBody[uploaded](t, second)
	if duplicate.Version.Number != 2 || duplicate.Version.SHA256 != sha {
		t.Fatalf("second version = %+v, want version 2 under sha %s", duplicate.Version, sha)
	}
	if row := versionRow(t, database, version.Artifact.ID, 2); row != nil {
		t.Fatalf("the second row holds %d bytes; with a store it should hold none", len(row))
	}

	// The version route answers from the store with the picture's headers: a picture is drawn
	// where the reader is rather than downloaded, under the artifact's own name, and its bytes
	// are named by a quoted entity tag.
	served := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+version.Artifact.ID+"/versions/1", nil, "alice")
	if served.Code != http.StatusOK || !bytes.Equal(served.Body.Bytes(), image) {
		t.Fatalf("serve version: status=%d, %d body bytes, want 200 with the image", served.Code, served.Body.Len())
	}
	headers := served.Header()
	if headers.Get("Content-Type") != "image/png" || headers.Get("ETag") != `"`+sha+`"` ||
		headers.Get("Content-Disposition") != `inline; filename="shot.png"; filename*=UTF-8''shot.png` ||
		headers.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("serve version headers = %v", headers)
	}
	bySlug := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue+"/artifacts/shot-png/versions/2", nil, "alice")
	if bySlug.Code != http.StatusOK || !bytes.Equal(bySlug.Body.Bytes(), image) {
		t.Fatalf("serve version by slug: status=%d, %d body bytes", bySlug.Code, bySlug.Body.Len())
	}
}

// A file version's bytes never change, so its download says a browser may keep it for a year
// without asking again, privately since the route is authenticated; a document version is the
// document's JSON read and says nothing of the kind.
func TestAFileVersionIsCachedForeverAndADocumentVersionIsNot(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{files: filestest.NewMemory()})
	issue := fileIssue(t, handler)
	if upload := multipartRequest(t, handler, "/api/v1/issues/"+issue+"/artifacts", map[string]string{"name": "shot.png"}, "shot.png", "image/png", []byte("png bytes"), "alice"); upload.Code != http.StatusCreated {
		t.Fatalf("upload: status=%d body=%s", upload.Code, upload.Body.String())
	}
	for target, want := range map[string]string{
		"/api/v1/issues/" + issue + "/artifacts/shot-png/versions/1": "private, max-age=31536000, immutable",
		"/api/v1/issues/" + issue + "/artifacts/spec/versions/1":     "",
	} {
		response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s: status=%d body=%s", target, response.Code, response.Body.String())
		}
		if got := response.Header().Get("Cache-Control"); got != want {
			t.Errorf("GET %s: Cache-Control %q, want %q", target, got, want)
		}
	}
}

// fileVersionRequest is a GET of a version's download carrying headers of its own: the Range and
// If-Range of a reader who wants part of a file, or the rest of one.
func fileVersionRequest(t *testing.T, handler http.Handler, target string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.Header.Set("X-Dispatch-User", "alice")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// The types a browser draws where the reader is are answered inline, so a picture's own address
// shows the picture and a video's a player; every other type is still handed over as a download,
// an SVG (a page of its own, which a browser would run) and a type a client declared freely
// among them. The stored type is read without its parameters. No answer carries a
// Content-Security-Policy.
func TestAFileVersionIsInlineOnlyForTheTypesABrowserDraws(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{files: filestest.NewMemory()})
	issue := fileIssue(t, handler)
	for _, row := range []struct {
		name        string
		declared    string
		disposition string
	}{
		{"shot.png", "image/png", "inline"},
		{"shot.jpg", "image/jpeg", "inline"},
		{"shot.gif", "image/gif", "inline"},
		{"shot.webp", "image/webp", "inline"},
		{"clip.mp4", "video/mp4", "inline"},
		{"clip.webm", "video/webm", "inline"},
		{"filmed.m4v", `video/mp4; codecs="avc1.42E01E"`, "inline"},
		{"rows.csv", "text/csv", "attachment"},
		{"drawing.svg", "image/svg+xml", "attachment"},
		{"notes.bin", "application/x-dispatch-test", "attachment"},
	} {
		t.Run(row.name, func(t *testing.T) {
			upload := multipartRequest(t, handler, "/api/v1/issues/"+issue+"/artifacts", map[string]string{"name": row.name}, row.name, row.declared, []byte("the file's own bytes"), "alice")
			if upload.Code != http.StatusCreated {
				t.Fatalf("upload %s: status=%d body=%s", row.declared, upload.Code, upload.Body.String())
			}
			version := decodeBody[uploaded](t, upload)
			served := fileVersionRequest(t, handler, "/api/v1/artifacts/"+version.Artifact.ID+"/versions/1", nil)
			if served.Code != http.StatusOK {
				t.Fatalf("serve %s: status=%d body=%s", row.declared, served.Code, served.Body.String())
			}
			if got := served.Header().Get("Content-Type"); got != row.declared {
				t.Errorf("%s: Content-Type %q, want %q", row.declared, got, row.declared)
			}
			want := row.disposition + `; filename="` + row.name + `"; filename*=UTF-8''` + row.name
			if got := served.Header().Get("Content-Disposition"); got != want {
				t.Errorf("%s: Content-Disposition %q, want %q", row.declared, got, want)
			}
			// A page's policy is the page's; a file the reader opened is drawn by the browser
			// itself, and a policy here would only stop it.
			if got := served.Header().Get("Content-Security-Policy"); got != "" {
				t.Errorf("%s: Content-Security-Policy %q, want none", row.declared, got)
			}
		})
	}
}

// The artifact's own name goes out twice, as a download's name must: the plain `filename`, which
// holds printable ASCII alone and escapes a quote and a backslash, and `filename*`, which holds
// the name whole, so a reader saves what Dispatch shows rather than an id.
func TestAFileVersionNamesItsFileInBothFilenameParameters(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{files: filestest.NewMemory()})
	issue := fileIssue(t, handler)
	for _, row := range []struct {
		name        string
		disposition string
	}{
		{"shot.png", `inline; filename="shot.png"; filename*=UTF-8''shot.png`},
		{`a "quoted" náme\.png`, `inline; filename="a \"quoted\" n_me\\.png"; filename*=UTF-8''a%20%22quoted%22%20n%C3%A1me%5C.png`},
	} {
		// The upload's own file part carries a plain name: the artifact's name is the field,
		// and it is the field the download is named after.
		upload := multipartRequest(t, handler, "/api/v1/issues/"+issue+"/artifacts", map[string]string{"name": row.name}, "upload.png", "image/png", []byte("the picture's bytes"), "alice")
		if upload.Code != http.StatusCreated {
			t.Fatalf("upload %q: status=%d body=%s", row.name, upload.Code, upload.Body.String())
		}
		version := decodeBody[uploaded](t, upload)
		served := fileVersionRequest(t, handler, "/api/v1/artifacts/"+version.Artifact.ID+"/versions/1", nil)
		if served.Code != http.StatusOK {
			t.Fatalf("serve %q: status=%d body=%s", row.name, served.Code, served.Body.String())
		}
		if got := served.Header().Get("Content-Disposition"); got != row.disposition {
			t.Errorf("%q: Content-Disposition %q, want %q", row.name, got, row.disposition)
		}
	}
}

// assertFileVersionRanges drives the reads a player makes of a file it may seek in, and is run
// against both paths of the route, so the two answer alike: content is the file's bytes and sha
// the hash its row records.
func assertFileVersionRanges(t *testing.T, handler http.Handler, route string, content []byte, sha string) {
	t.Helper()
	whole := fileVersionRequest(t, handler, route, nil)
	if whole.Code != http.StatusOK || !bytes.Equal(whole.Body.Bytes(), content) {
		t.Fatalf("GET %s: status=%d, %d body bytes, want 200 with the whole file", route, whole.Code, whole.Body.Len())
	}
	// A player decides from the first answer it gets whether it may seek, and that answer
	// carries no range of its own.
	if got := whole.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("GET %s with no Range: Accept-Ranges %q, want bytes", route, got)
	}

	part := fileVersionRequest(t, handler, route, map[string]string{"Range": "bytes=4-11"})
	if part.Code != http.StatusPartialContent || !bytes.Equal(part.Body.Bytes(), content[4:12]) {
		t.Fatalf("GET %s bytes=4-11: status=%d body=%q, want 206 with %q", route, part.Code, part.Body.String(), content[4:12])
	}
	if got, want := part.Header().Get("Content-Range"), fmt.Sprintf("bytes 4-11/%d", len(content)); got != want {
		t.Errorf("GET %s bytes=4-11: Content-Range %q, want %q", route, got, want)
	}
	if got := part.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("GET %s bytes=4-11: Accept-Ranges %q, want bytes", route, got)
	}

	outside := fileVersionRequest(t, handler, route, map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", len(content)+1, len(content)+9)})
	if outside.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("GET %s for a range past the end: status=%d, want 416", route, outside.Code)
	}
	if got, want := outside.Header().Get("Content-Range"), fmt.Sprintf("bytes */%d", len(content)); got != want {
		t.Errorf("GET %s for a range past the end: Content-Range %q, want %q", route, got, want)
	}

	// A Range header naming no range at all is no range to answer, and the whole file goes.
	malformed := fileVersionRequest(t, handler, route, map[string]string{"Range": "bytes="})
	if malformed.Code != http.StatusOK || !bytes.Equal(malformed.Body.Bytes(), content) {
		t.Errorf("GET %s with a malformed Range: status=%d, %d body bytes, want 200 with the whole file", route, malformed.Code, malformed.Body.Len())
	}

	// A resume takes the rest of the file only while the bytes are the ones it already holds,
	// which the entity tag says; net/http reads a tag in quotes and nothing else, so a bare one
	// matches nothing and the whole file goes instead.
	resumed := fileVersionRequest(t, handler, route, map[string]string{"Range": "bytes=4-11", "If-Range": `"` + sha + `"`})
	if resumed.Code != http.StatusPartialContent || !bytes.Equal(resumed.Body.Bytes(), content[4:12]) {
		t.Errorf("GET %s with If-Range on the quoted tag: status=%d body=%q, want 206 with %q", route, resumed.Code, resumed.Body.String(), content[4:12])
	}
	bare := fileVersionRequest(t, handler, route, map[string]string{"Range": "bytes=4-11", "If-Range": sha})
	if bare.Code != http.StatusOK || !bytes.Equal(bare.Body.Bytes(), content) {
		t.Errorf("GET %s with If-Range on a bare tag: status=%d, %d body bytes, want 200 with the whole file", route, bare.Code, bare.Body.Len())
	}
}

// A version whose row still holds its bytes answers a range from the row.
func TestAFileVersionInItsRowAnswersRangeRequests(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := fileIssue(t, handler)
	clip := []byte("a video's bytes, start to end")
	upload := multipartRequest(t, handler, "/api/v1/issues/"+issue+"/artifacts", map[string]string{"name": "clip.mp4"}, "clip.mp4", "video/mp4", clip, "alice")
	if upload.Code != http.StatusCreated {
		t.Fatalf("upload: status=%d body=%s", upload.Code, upload.Body.String())
	}
	version := decodeBody[uploaded](t, upload)
	if versionRow(t, database, version.Artifact.ID, 1) == nil {
		t.Fatal("with no store configured the row should hold the bytes")
	}
	assertFileVersionRanges(t, handler, "/api/v1/artifacts/"+version.Artifact.ID+"/versions/1", clip, version.Version.SHA256)
}

// A version whose bytes are in the store answers the same ranges, read back through the store.
func TestAFileVersionInTheStoreAnswersRangeRequests(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{files: filestest.NewMemory()})
	issue := fileIssue(t, handler)
	clip := []byte("a video's bytes, start to end")
	upload := multipartRequest(t, handler, "/api/v1/issues/"+issue+"/artifacts", map[string]string{"name": "clip.mp4"}, "clip.mp4", "video/mp4", clip, "alice")
	if upload.Code != http.StatusCreated {
		t.Fatalf("upload: status=%d body=%s", upload.Code, upload.Body.String())
	}
	version := decodeBody[uploaded](t, upload)
	if row := versionRow(t, database, version.Artifact.ID, 1); row != nil {
		t.Fatalf("the row holds %d bytes; with a store it should hold none", len(row))
	}
	assertFileVersionRanges(t, handler, "/api/v1/artifacts/"+version.Artifact.ID+"/versions/1", clip, version.Version.SHA256)
}

func TestAnUploadTheStoreRefusesLeavesNoVersionBehind(t *testing.T) {
	memory := filestest.NewMemory()
	memory.SetFailure(errors.New("bucket unreachable"))
	handler, database, _ := newTestServer(t, testServerOptions{files: memory})
	issue := fileIssue(t, handler)

	response := multipartRequest(t, handler, "/api/v1/issues/"+issue+"/artifacts", map[string]string{"name": "notes.bin"}, "notes.bin", "application/octet-stream", []byte("payload"), "alice")
	if response.Code != http.StatusBadGateway {
		t.Fatalf("upload against a failing store: status=%d body=%s", response.Code, response.Body.String())
	}
	if code := responseCode(t, response); code != "FILE_STORE_UNAVAILABLE" {
		t.Fatalf("error code = %q, want FILE_STORE_UNAVAILABLE", code)
	}
	var artifacts int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from artifacts where issue_key = $1 and kind <> 'doc'`, issue).Scan(&artifacts); err != nil {
		t.Fatalf("count artifacts: %v", err)
	}
	if artifacts != 0 {
		t.Fatalf("%d file artifacts exist after a refused upload, want none", artifacts)
	}
}

func TestARowStillHoldingItsBytesIsServedFromTheRow(t *testing.T) {
	// An upload made before the store existed keeps its bytes in the row; the store configured
	// later holds nothing for it, and the route serves the row.
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := fileIssue(t, handler)
	legacy := multipartRequest(t, handler, "/api/v1/issues/"+issue+"/artifacts", map[string]string{"name": "old.bin"}, "old.bin", "application/octet-stream", []byte("legacy bytes"), "alice")
	if legacy.Code != http.StatusCreated {
		t.Fatalf("legacy upload: status=%d body=%s", legacy.Code, legacy.Body.String())
	}
	version := decodeBody[uploaded](t, legacy)
	if versionRow(t, database, version.Artifact.ID, 1) == nil {
		t.Fatal("an upload with no store left no bytes in the row")
	}

	memory := filestest.NewMemory()
	deps, err := NewDeps(DepsInput{
		Store: database, Identity: headerIdentity(database), AgentTokens: sharedAgentTokens(t, "agent-token"),
		ServerURL: "https://dispatch.example", Files: memory,
	})
	if err != nil {
		t.Fatalf("new deps with a store: %v", err)
	}
	withStore := http.NewServeMux()
	t.Cleanup(Register(withStore, t.Context(), deps).Wait)
	served := dispatchRequest(t, withStore, http.MethodGet, "/api/v1/artifacts/"+version.Artifact.ID+"/versions/1", nil, "alice")
	if served.Code != http.StatusOK || served.Body.String() != "legacy bytes" {
		t.Fatalf("serve a row-held version with a store configured: status=%d body=%q", served.Code, served.Body.String())
	}
	if memory.Len() != 0 {
		t.Fatalf("serving a row-held version wrote %d objects to the store", memory.Len())
	}
}

func TestAClearedRowIsAnsweredByWhatTheStoreSays(t *testing.T) {
	memory := filestest.NewMemory()
	handler, database, _ := newTestServer(t, testServerOptions{files: memory})
	issue := fileIssue(t, handler)
	response := multipartRequest(t, handler, "/api/v1/issues/"+issue+"/artifacts", map[string]string{"name": "gone.bin"}, "gone.bin", "application/octet-stream", []byte("gone bytes"), "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("upload: status=%d body=%s", response.Code, response.Body.String())
	}
	version := decodeBody[uploaded](t, response)
	route := "/api/v1/artifacts/" + version.Artifact.ID + "/versions/1"

	// The store cannot be reached: an outage a retry may mend, 502.
	memory.SetFailure(errors.New("bucket unreachable"))
	served := dispatchRequest(t, handler, http.MethodGet, route, nil, "alice")
	if served.Code != http.StatusBadGateway || responseCode(t, served) != "FILE_STORE_UNAVAILABLE" {
		t.Fatalf("serve a version while the store is down: status=%d body=%s, want 502 FILE_STORE_UNAVAILABLE", served.Code, served.Body.String())
	}
	// Nor the version's year-long cache header, which would keep the failure in place of the bytes.
	if served.Header().Get("Content-Disposition") != "" || served.Header().Get("ETag") != "" || served.Header().Get("Cache-Control") != "" {
		t.Fatalf("a failed read sent the success headers: %v", served.Header())
	}

	// The store answers but holds no object under the row's hash: data a retry never brings
	// back, named as such, 500.
	memory.SetFailure(nil)
	memory.Delete(version.Version.SHA256)
	missing := dispatchRequest(t, handler, http.MethodGet, route, nil, "alice")
	if missing.Code != http.StatusInternalServerError || responseCode(t, missing) != "FILE_MISSING" {
		t.Fatalf("serve a version whose object is gone: status=%d body=%s, want 500 FILE_MISSING", missing.Code, missing.Body.String())
	}
	if got := missing.Header().Get("Cache-Control"); got != "" {
		t.Fatalf("a version whose object is gone: Cache-Control %q, want none", got)
	}

	// Without a store at all, a cleared row is a configuration the server names: 503.
	noStore, err := NewDeps(DepsInput{
		Store: database, Identity: headerIdentity(database), AgentTokens: sharedAgentTokens(t, "agent-token"),
		ServerURL: "https://dispatch.example",
	})
	if err != nil {
		t.Fatalf("new deps without a store: %v", err)
	}
	withoutStore := http.NewServeMux()
	t.Cleanup(Register(withoutStore, t.Context(), noStore).Wait)
	unconfigured := dispatchRequest(t, withoutStore, http.MethodGet, "/api/v1/artifacts/"+version.Artifact.ID+"/versions/1", nil, "alice")
	if unconfigured.Code != http.StatusServiceUnavailable || responseCode(t, unconfigured) != "FILE_STORE_UNAVAILABLE" {
		t.Fatalf("serve a cleared row with no store: status=%d body=%s, want 503 FILE_STORE_UNAVAILABLE", unconfigured.Code, unconfigured.Body.String())
	}
}

// heldPut is a store whose Put waits until the test releases it, so a test can look at the
// database while an upload's bytes are on their way to the store.
type heldPut struct {
	*filestest.Memory
	entered chan struct{}
	release chan struct{}
}

func (h heldPut) Put(ctx context.Context, sha, mime string, body []byte) error {
	close(h.entered)
	<-h.release
	return h.Memory.Put(ctx, sha, mime, body)
}

// An upload writes its object before its transaction opens, so while the store takes the bytes
// the issue's row is free: the transaction locks it for no key update, and every other writer
// of the issue (a comment, an ask, a status change) would queue behind a store write nothing
// bounds.
func TestAnUploadHoldsNoIssueLockWhileTheStoreWrites(t *testing.T) {
	held := heldPut{Memory: filestest.NewMemory(), entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(held.release) }) }
	handler, database, _ := newTestServer(t, testServerOptions{files: held})
	issue := fileIssue(t, handler)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("name", "held.bin"); err != nil {
		t.Fatalf("write name field: %v", err)
	}
	part, err := writer.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="file"; filename="held.bin"`},
		"Content-Type":        {"application/octet-stream"},
	})
	if err != nil {
		t.Fatalf("create file part: %v", err)
	}
	if _, err := part.Write([]byte("held bytes")); err != nil {
		t.Fatalf("write file part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("finish multipart body: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("X-Dispatch-User", "alice")
	uploaded := make(chan *httptest.ResponseRecorder, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		uploaded <- response
	}()
	// Registered after the server's own cleanups, so it runs before them: a failure below must
	// not leave the upload parked in Put while the server's shutdown waits for it.
	t.Cleanup(func() {
		release()
		<-finished
	})

	select {
	case <-held.entered:
	case response := <-uploaded:
		t.Fatalf("the upload answered without writing to the store: status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := database.Pool.Exec(context.Background(), `select key from issues where key = $1 for no key update nowait`, issue); err != nil {
		t.Fatalf("lock the issue while its upload's bytes are in flight: %v", err)
	}
	release()
	if response := <-uploaded; response.Code != http.StatusCreated {
		t.Fatalf("upload: status=%d body=%s", response.Code, response.Body.String())
	}
}

// tampered is a store whose object under one hash reads back as other bytes of the same length,
// as a replaced or corrupted object in the bucket would, and passes every other call on.
type tampered struct {
	files.Store
	sha   string
	bytes []byte
}

func (s tampered) Get(ctx context.Context, sha string) (*files.Object, error) {
	if sha != s.sha {
		return s.Store.Get(ctx, sha)
	}
	size := int64(len(s.bytes))
	return &files.Object{Body: files.NewVerifyingReader(io.NopCloser(bytes.NewReader(s.bytes)), sha, size), Size: size}, nil
}

// A version whose object no longer holds the row's bytes is never delivered as a whole file: the
// response is cut short of its Content-Length, which a client reports. Over a real connection,
// since the cut is the connection's.
func TestATamperedObjectIsNotDeliveredWhole(t *testing.T) {
	original := bytes.Repeat([]byte("o"), 300_000)
	replaced := append(bytes.Repeat([]byte("o"), 299_999), 'x')
	store := tampered{Store: filestest.NewMemory(), sha: files.SHA256(original), bytes: replaced}
	handler, _, _ := newTestServer(t, testServerOptions{files: store})
	issue := fileIssue(t, handler)
	response := multipartRequest(t, handler, "/api/v1/issues/"+issue+"/artifacts", map[string]string{"name": "shot.bin"}, "shot.bin", "application/octet-stream", original, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("upload: status=%d body=%s", response.Code, response.Body.String())
	}
	version := decodeBody[uploaded](t, response)

	server := httptest.NewServer(handler)
	defer server.Close()
	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/artifacts/"+version.Artifact.ID+"/versions/1", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("X-Dispatch-User", "alice")
	served, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer served.Body.Close()
	if served.StatusCode != http.StatusOK || served.ContentLength != int64(len(original)) {
		t.Fatalf("status %d, Content-Length %d, want 200 and %d: the object opened, so the headers went out", served.StatusCode, served.ContentLength, len(original))
	}
	got, err := io.ReadAll(served.Body)
	if err == nil || int64(len(got)) >= int64(len(original)) {
		t.Fatalf("the client read %d of %d bytes with error %v; want fewer bytes and a read error", len(got), len(original), err)
	}
}
