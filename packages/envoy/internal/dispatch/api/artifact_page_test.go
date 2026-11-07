package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/files/filestest"
)

// The Go half of dispatch://INFRA-682's A4: what a caller of
// GET /api/v1/artifacts/{id}/versions/{number}/page is answered. The spec's "The page's address"
// lists the headers a frame's request and any other request get, and the three refusals the
// route makes to GET and to HEAD alike.

// wantPagePolicy is the Content-Security-Policy of "The page's address", written out here rather
// than read from the server: a test that compared the header with the constant that wrote it
// would pass for any policy at all, including one that let the page reach the network.
const wantPagePolicy = "sandbox allow-scripts; default-src 'none'; script-src 'unsafe-inline' 'unsafe-eval' blob:; style-src 'unsafe-inline'; img-src data: blob:; font-src data:; media-src data: blob:; worker-src blob:; connect-src 'none'; frame-src 'none'; form-action 'none'; base-uri 'none'; frame-ancestors 'self'"

// pageTestIssue is the project and issue a page test's files are uploaded to.
func pageTestIssue(t *testing.T, handler http.Handler) string {
	t.Helper()
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "PAGE", "name": "Pages",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "PAGE", "title": "A view's page", "spec": "# Pages",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	return decodeBody[struct {
		Key string `json:"key"`
	}](t, created).Key
}

// uploadTestFile uploads one file under an owner's artifacts route - an issue's, a project's or
// an agent conversation's - and answers the artifact it made.
func uploadTestFile(t *testing.T, handler http.Handler, artifactsRoute, name, contentType string, content []byte) uploaded {
	t.Helper()
	response := multipartRequest(t, handler, artifactsRoute, map[string]string{"name": name}, name, contentType, content, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("upload %s as %s to %s: status=%d body=%s", name, contentType, artifactsRoute, response.Code, response.Body.String())
	}
	return decodeBody[uploaded](t, response)
}

// uploadPageTestFile uploads one file to an issue and answers the artifact it made.
func uploadPageTestFile(t *testing.T, handler http.Handler, issue, name, contentType string, content []byte) uploaded {
	t.Helper()
	return uploadTestFile(t, handler, "/api/v1/issues/"+issue+"/artifacts", name, contentType, content)
}

// pageRequest is one request to the page route as a signed-in person, carrying the
// Sec-Fetch-Dest a browser sends (empty for a request that sends none, such as a `curl`).
func pageRequest(t *testing.T, handler http.Handler, method, target, fetchDest string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, nil)
	request.Header.Set("X-Dispatch-User", "alice")
	if fetchDest != "" {
		request.Header.Set("Sec-Fetch-Dest", fetchDest)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// pageRefusal is the status and code a refused page request carries.
type pageRefusal struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

// requirePageRefusal reads one refusal of the page route, to GET and to HEAD alike: the block
// sends a HEAD before it inserts a frame and shows the cause, so a HEAD that answered otherwise
// would show the reader a frame the GET then refuses.
func requirePageRefusal(t *testing.T, handler http.Handler, what, target string, status int, code string) {
	t.Helper()
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		response := pageRequest(t, handler, method, target, "iframe")
		if response.Code != status || decodeBody[pageRefusal](t, response).Code != code {
			t.Errorf("%s %s (%s) = %d %s, want %d %s", method, target, what, response.Code, response.Body.String(), status, code)
		}
	}
}

// TestArtifactPageHeadersForAFrameAndForEveryOtherRequest: a frame's request is answered inline
// under the policy that gives the page an opaque origin and lets it reach nothing, and every
// other request - a reader opening the address in a tab - is answered the same bytes as an
// attachment of a type no browser renders, so no page runs under Dispatch's own address in any
// engine. Content-Type and Content-Disposition are what differ between the two, and nothing else.
func TestArtifactPageHeadersForAFrameAndForEveryOtherRequest(t *testing.T) {
	handler := newTestHandler(t)
	issue := pageTestIssue(t, handler)
	page := []byte("<!doctype html><title>Chart</title><script>parent.postMessage({type:'dispatch-view:ready'},'*')</script>")
	file := uploadPageTestFile(t, handler, issue, "chart.html", "text/html", page)
	digest := sha256.Sum256(page)
	// The quoted entity tag of RFC 9110 8.8.3, which is what the browser sends back in
	// If-None-Match; the bare hash it was would match nothing a browser asked with.
	etag := `"` + hex.EncodeToString(digest[:]) + `"`

	target := "/api/v1/artifacts/" + file.Artifact.ID + "/versions/1/page"
	answers := map[string]*httptest.ResponseRecorder{
		"inline":     pageRequest(t, handler, http.MethodGet, target, "iframe"),
		"attachment": pageRequest(t, handler, http.MethodGet, target, ""),
	}
	wantType := map[string]string{
		"inline":     "text/html; charset=utf-8",
		"attachment": "application/octet-stream",
	}
	for disposition, response := range answers {
		if response.Code != http.StatusOK {
			t.Fatalf("the %s answer: status=%d body=%s, want 200", disposition, response.Code, response.Body.String())
		}
		if got := response.Body.String(); got != string(page) {
			t.Errorf("the %s answer's body = %q, want the page's own bytes", got, page)
		}
		for header, want := range map[string]string{
			"Content-Type":            wantType[disposition],
			"Content-Disposition":     disposition,
			"X-Content-Type-Options":  "nosniff",
			"Referrer-Policy":         "no-referrer",
			"Cache-Control":           "private, no-cache",
			"ETag":                    etag,
			"Vary":                    "Sec-Fetch-Dest",
			"Content-Security-Policy": wantPagePolicy,
		} {
			if got := response.Header().Get(header); got != want {
				t.Errorf("the %s answer's %s = %q, want %q", disposition, header, got, want)
			}
		}
	}

	// Sec-Fetch-Dest decides the type and the disposition and nothing else, so a cache keyed on
	// Vary serves one of these answers to the other kind of request only in those two headers.
	frame, tab := answers["inline"].Header().Clone(), answers["attachment"].Header().Clone()
	for _, header := range []string{"Content-Disposition", "Content-Type"} {
		frame.Del(header)
		tab.Del(header)
	}
	if !maps.EqualFunc(frame, tab, slices.Equal) {
		t.Errorf("the two answers differ beyond Content-Disposition and Content-Type:\nframe %v\ntab   %v", frame, tab)
	}

	// A dest that is not a frame is every other request: a document load, a script load, a
	// fetch, each saved rather than run, and none of them served a type a browser draws.
	for _, dest := range []string{"document", "empty", "script", "frame"} {
		response := pageRequest(t, handler, http.MethodGet, target, dest)
		if response.Code != http.StatusOK ||
			response.Header().Get("Content-Disposition") != "attachment" ||
			response.Header().Get("Content-Type") != "application/octet-stream" {
			t.Errorf("Sec-Fetch-Dest: %s = %d %q %q, want 200 attachment application/octet-stream",
				dest, response.Code, response.Header().Get("Content-Disposition"),
				response.Header().Get("Content-Type"))
		}
	}
}

// TestArtifactPageServesAFileByItsStoredTypeAndByItsName: a page file is one whose stored type is
// text/html, whatever its name, or whose name ends .html or .htm under any other type. Both are
// served as HTML, since the route decides the type and never the stored one.
func TestArtifactPageServesAFileByItsStoredTypeAndByItsName(t *testing.T) {
	handler := newTestHandler(t)
	issue := pageTestIssue(t, handler)
	for _, file := range []struct {
		what        string
		name        string
		contentType string
	}{
		{"its stored type", "dashboard.txt", "text/html; charset=utf-8"},
		{"its .html name", "panel.html", "application/octet-stream"},
		{"its .htm name", "legacy.htm", "application/octet-stream"},
	} {
		t.Run(file.what, func(t *testing.T) {
			body := []byte("<!doctype html><title>" + file.name + "</title>")
			uploaded := uploadPageTestFile(t, handler, issue, file.name, file.contentType, body)
			response := pageRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+uploaded.Artifact.ID+"/versions/1/page", "iframe")
			if response.Code != http.StatusOK || response.Body.String() != string(body) {
				t.Fatalf("%s as a page: status=%d body=%s, want 200 with the page's bytes", file.name, response.Code, response.Body.String())
			}
			if got := response.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
				t.Errorf("%s Content-Type = %q, want text/html; charset=utf-8", file.name, got)
			}
		})
	}
}

// TestArtifactPageRefusesAnArtifactOrAVersionThatIsNotThere: both absences are 404
// ARTIFACT_NOT_FOUND, to GET and to HEAD.
func TestArtifactPageRefusesAnArtifactOrAVersionThatIsNotThere(t *testing.T) {
	handler := newTestHandler(t)
	issue := pageTestIssue(t, handler)
	file := uploadPageTestFile(t, handler, issue, "chart.html", "text/html", []byte("<!doctype html><title>One</title>"))

	requirePageRefusal(t, handler, "a version the page file does not have",
		"/api/v1/artifacts/"+file.Artifact.ID+"/versions/2/page", http.StatusNotFound, "ARTIFACT_NOT_FOUND")
	requirePageRefusal(t, handler, "an artifact that is not there",
		"/api/v1/artifacts/00000000-0000-0000-0000-000000000000/versions/1/page", http.StatusNotFound, "ARTIFACT_NOT_FOUND")
}

// TestArtifactPageRefusesEveryFileThatIsNotAPageFile: 415 NOT_A_PAGE for each, to GET and to
// HEAD. They are the four arms of the page test (isPageFile), which the grants routes make too,
// so each file refused here is one whose grants those routes refuse to list:
//
//   - a file that is HTML by neither name nor stored type;
//   - a document, though its markdown would render: its text is what Dispatch rewrites as edits
//     settle, not the bytes an author sent;
//   - an agent conversation's file, whose uploads write no event, so nothing could redraw a page
//     from it;
//   - a version of a file whose latest version is no longer HTML, since the test reads the
//     latest version's stored type and the file is a page file or is not, at every version of it.
func TestArtifactPageRefusesEveryFileThatIsNotAPageFile(t *testing.T) {
	handler := newTestHandler(t)
	issue := pageTestIssue(t, handler)
	notes := uploadPageTestFile(t, handler, issue, "notes.txt", "text/plain", []byte("plain notes\n"))
	document := uploadPageTestFile(t, handler, issue, "notes.md", "text/markdown", []byte("# Notes\n\nA paragraph.\n"))
	const session = "01a1058e-f14f-7684-87eb-3dc885955553"
	conversation := uploadTestFile(t, handler, "/api/v1/agents/"+session+"/artifacts",
		"session.html", "text/html", []byte("<!doctype html><title>Session</title>"))

	requirePageRefusal(t, handler, "a file that is not a page",
		"/api/v1/artifacts/"+notes.Artifact.ID+"/versions/1/page", http.StatusUnsupportedMediaType, "NOT_A_PAGE")
	requirePageRefusal(t, handler, "a document",
		"/api/v1/artifacts/"+document.Artifact.ID+"/versions/1/page", http.StatusUnsupportedMediaType, "NOT_A_PAGE")
	requirePageRefusal(t, handler, "an agent conversation's HTML file",
		"/api/v1/artifacts/"+conversation.Artifact.ID+"/versions/1/page", http.StatusUnsupportedMediaType, "NOT_A_PAGE")

	// A file stored as HTML at v1 and as something else at v2, under a name that is no page's:
	// the page test reads the latest version's type, so neither version is served and the
	// grants routes refuse the same file, which is the agreement the one predicate is for.
	stopped := uploadPageTestFile(t, handler, issue, "report", "text/html", []byte("<!doctype html><title>v1</title>"))
	if second := multipartRequest(t, handler, "/api/v1/issues/"+issue+"/artifacts",
		map[string]string{"name": "report"}, "report", "application/json", []byte(`{"at":"v2"}`), "alice"); second.Code != http.StatusCreated {
		t.Fatalf("second version of report: status=%d body=%s", second.Code, second.Body.String())
	}
	for _, number := range []string{"1", "2"} {
		requirePageRefusal(t, handler, "version "+number+" of a file whose latest version is not HTML",
			"/api/v1/artifacts/"+stopped.Artifact.ID+"/versions/"+number+"/page", http.StatusUnsupportedMediaType, "NOT_A_PAGE")
	}
	grants := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+stopped.Artifact.ID+"/grants", nil, "alice")
	if grants.Code != http.StatusUnsupportedMediaType || decodeBody[pageRefusal](t, grants).Code != "NOT_A_PAGE" {
		t.Errorf("the grants of that file = %d %s, want the 415 NOT_A_PAGE the page route makes", grants.Code, grants.Body.String())
	}
}

// TestArtifactPageRefusesAPageOverTheLimit: a version past 5 MiB is 413 PAGE_TOO_LARGE, to GET
// and to HEAD, and the page one byte under it is served.
func TestArtifactPageRefusesAPageOverTheLimit(t *testing.T) {
	handler := newTestHandler(t)
	issue := pageTestIssue(t, handler)
	const limit = 5 << 20
	prefix := "<!doctype html><title>Big</title><!--"
	suffix := "-->"
	filler := strings.Repeat("x", limit+1-len(prefix)-len(suffix))
	over := []byte(prefix + filler + suffix)
	if len(over) != limit+1 {
		t.Fatalf("the oversized page is %d bytes, want one past the %d-byte limit", len(over), limit)
	}
	big := uploadPageTestFile(t, handler, issue, "big.html", "text/html", over)
	requirePageRefusal(t, handler, "a page one byte over 5 MiB",
		"/api/v1/artifacts/"+big.Artifact.ID+"/versions/1/page", http.StatusRequestEntityTooLarge, "PAGE_TOO_LARGE")

	atLimit := uploadPageTestFile(t, handler, issue, "at-limit.html", "text/html", over[:limit])
	served := pageRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+atLimit.Artifact.ID+"/versions/1/page", "iframe")
	if served.Code != http.StatusOK || served.Body.Len() != limit {
		t.Fatalf("a page of exactly 5 MiB: status=%d, %d body bytes, want 200 with %d", served.Code, served.Body.Len(), limit)
	}
}

// The HEAD a view block sends before each frame is answered from the row, whether the bytes sit
// in the row or in a file store. A page file is the one a store is most likely to hold, since it
// carries a whole drawing library, and reading that object back for a body net/http discards is
// a megabyte off the bucket per frame the reader scrolls to.
func TestAHeadOfAStoreBackedPageAnswersItsLengthWithoutReadingTheObject(t *testing.T) {
	memory := filestest.NewMemory()
	handler, database, _ := newTestServer(t, testServerOptions{files: memory})
	issue := pageTestIssue(t, handler)
	body := []byte("<!doctype html><title>chart</title>" + strings.Repeat("<!-- draw -->", 2048))
	page := uploadPageTestFile(t, handler, issue, "chart.html", "text/html", body)
	if versionRow(t, database, page.Artifact.ID, 1) != nil {
		t.Fatal("the row kept its bytes, so this test would not reach the store path")
	}
	target := "/api/v1/artifacts/" + page.Artifact.ID + "/versions/1/page"

	memory.SetFailure(errors.New("the store must not be read to answer a HEAD"))
	head := pageRequest(t, handler, http.MethodHead, target, "iframe")
	if head.Code != http.StatusOK {
		t.Fatalf("HEAD of a store-backed page: status=%d body=%s, want 200 answered from the row", head.Code, head.Body.String())
	}
	if got := head.Header().Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Errorf("HEAD Content-Length = %q, want %d", got, len(body))
	}
	if got := head.Header().Get("Content-Security-Policy"); got != wantPagePolicy {
		t.Errorf("HEAD Content-Security-Policy = %q, want the page policy", got)
	}
	if got := head.Header().Get("Content-Disposition"); got != "inline" {
		t.Errorf("HEAD Content-Disposition = %q, want inline", got)
	}

	// The control: with the bucket reachable the same address still serves the page, so what the
	// HEAD avoided is a read it did not need rather than one the route has lost.
	memory.SetFailure(nil)
	served := pageRequest(t, handler, http.MethodGet, target, "iframe")
	if served.Code != http.StatusOK || served.Body.Len() != len(body) {
		t.Fatalf("GET after the HEAD: status=%d, %d body bytes, want 200 with %d", served.Code, served.Body.Len(), len(body))
	}
}

// TestArtifactPageAnswersNotModifiedToTheEntityTagItSent: the page route is `no-cache`, so a
// browser revalidates every time a frame starts; the second ask carries the tag the first
// answer sent and is answered 304 with no body, so a reader who scrolls back to a view block
// downloads the page once. The 304 carries the headers the 200 carried, because a cache
// replaces what it stored with what a 304 brings: one that left them out would keep the frame
// running under the policy stored the first time.
func TestArtifactPageAnswersNotModifiedToTheEntityTagItSent(t *testing.T) {
	handler := newTestHandler(t)
	issue := pageTestIssue(t, handler)
	body := []byte("<!doctype html><title>Chart</title><p>drawn once")
	file := uploadPageTestFile(t, handler, issue, "chart.html", "text/html", body)
	target := "/api/v1/artifacts/" + file.Artifact.ID + "/versions/1/page"

	first := pageRequest(t, handler, http.MethodGet, target, "iframe")
	if first.Code != http.StatusOK || first.Body.String() != string(body) {
		t.Fatalf("the first GET: status=%d body=%s, want 200 with the page", first.Code, first.Body.String())
	}
	etag := first.Header().Get("ETag")
	digest := sha256.Sum256(body)
	if etag != `"`+hex.EncodeToString(digest[:])+`"` {
		t.Fatalf("the ETag = %q, want the quoted hash of the page's bytes", etag)
	}

	again := httptest.NewRequest(http.MethodGet, target, nil)
	again.Header.Set("X-Dispatch-User", "alice")
	again.Header.Set("Sec-Fetch-Dest", "iframe")
	again.Header.Set("If-None-Match", etag)
	revalidated := httptest.NewRecorder()
	handler.ServeHTTP(revalidated, again)
	if revalidated.Code != http.StatusNotModified {
		t.Fatalf("the revalidation: status=%d body=%s, want 304", revalidated.Code, revalidated.Body.String())
	}
	if revalidated.Body.Len() != 0 {
		t.Errorf("the 304 carries %d body bytes, want none", revalidated.Body.Len())
	}
	if got := revalidated.Header().Get("Content-Length"); got != "" {
		t.Errorf("the 304's Content-Length = %q, want none", got)
	}
	for header, want := range map[string]string{
		"ETag":                    etag,
		"Cache-Control":           "private, no-cache",
		"Vary":                    "Sec-Fetch-Dest",
		"Content-Security-Policy": wantPagePolicy,
		"Content-Type":            "text/html; charset=utf-8",
		"Content-Disposition":     "inline",
	} {
		if got := revalidated.Header().Get(header); got != want {
			t.Errorf("the 304's %s = %q, want %q", header, got, want)
		}
	}

	// Only the frame's answer is revalidated. A browser that had fetched the page into a frame
	// was observed reusing that stored answer for a later navigation to the same address in a
	// tab, `Vary: Sec-Fetch-Dest` notwithstanding, so a 304 to a tab would hand it the frame's
	// `text/html` and `inline` and draw the page on Dispatch's own address - the one thing A4
	// forbids. Every request but the frame's gets the attachment in full, whatever tag it sends.
	for _, dest := range []string{"document", "empty", "script", ""} {
		tab := httptest.NewRequest(http.MethodGet, target, nil)
		tab.Header.Set("X-Dispatch-User", "alice")
		if dest != "" {
			tab.Header.Set("Sec-Fetch-Dest", dest)
		}
		tab.Header.Set("If-None-Match", etag)
		answered := httptest.NewRecorder()
		handler.ServeHTTP(answered, tab)
		if answered.Code != http.StatusOK || answered.Body.String() != string(body) {
			t.Errorf("Sec-Fetch-Dest %q revalidating: status=%d %d body bytes, want 200 with the page",
				dest, answered.Code, answered.Body.Len())
		}
		if got := answered.Header().Get("Content-Type"); got != "application/octet-stream" {
			t.Errorf("Sec-Fetch-Dest %q revalidating: Content-Type = %q, want application/octet-stream", dest, got)
		}
	}

	// A tag of another version is no match, and that ask is answered the page in full.
	stale := httptest.NewRequest(http.MethodGet, target, nil)
	stale.Header.Set("X-Dispatch-User", "alice")
	stale.Header.Set("Sec-Fetch-Dest", "iframe")
	stale.Header.Set("If-None-Match", `"0000000000000000000000000000000000000000000000000000000000000000"`)
	served := httptest.NewRecorder()
	handler.ServeHTTP(served, stale)
	if served.Code != http.StatusOK || served.Body.String() != string(body) {
		t.Fatalf("a request under another tag: status=%d body=%s, want 200 with the page", served.Code, served.Body.String())
	}
}

// TestAFileVersionDownloadDrawsNoDocumentOnDispatchsOwnOrigin: the plain download route, the
// one that serves any file version's bytes. An uploaded .html is answered
// application/octet-stream, so no engine draws it; every answer of the route carries the
// sandbox policy, so an engine that drew one anyway would draw it with an opaque origin,
// holding no cookie of Dispatch's and reaching nothing - and so could not POST a data grant as
// the reader (R9, dispatch://INFRA-682).
//
// The control that matters as much: a PNG is still answered image/png, so the pictures the web
// app draws with <img src> are untouched. A policy on a subresource is ignored by the engine
// that fetched it, so the header costs that path nothing.
func TestAFileVersionDownloadDrawsNoDocumentOnDispatchsOwnOrigin(t *testing.T) {
	handler := newTestHandler(t)
	issue := pageTestIssue(t, handler)
	for _, file := range []struct {
		what     string
		name     string
		declared string
		content  []byte
		download string
	}{
		{"an uploaded page", "chart.html", "text/html", []byte("<!doctype html><title>Chart</title><script>fetch('/api/v1/me')</script>"), "application/octet-stream"},
		{"an XHTML file", "page.xhtml", "application/xhtml+xml", []byte(`<html xmlns="http://www.w3.org/1999/xhtml"/>`), "application/octet-stream"},
		{"a drawing", "drawing.svg", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script/></svg>`), "application/octet-stream"},
		{"an XML document", "feed.xml", "text/xml", []byte(`<?xml version="1.0"?><feed/>`), "application/octet-stream"},
		{"an XML file of the other type", "data.xml", "application/xml", []byte(`<?xml version="1.0"?><data/>`), "application/octet-stream"},
		{"a picture", "shot.png", "image/png", pngBytes("pixels"), "image/png"},
		{"a file of no type a browser draws", "attempts.json", "application/json", []byte(`{"attempts":3}`), "application/json"},
	} {
		t.Run(file.what, func(t *testing.T) {
			uploaded := uploadPageTestFile(t, handler, issue, file.name, file.declared, file.content)
			target := "/api/v1/artifacts/" + uploaded.Artifact.ID + "/versions/1"
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				response := dispatchRequest(t, handler, method, target, nil, "alice")
				if response.Code != http.StatusOK {
					t.Fatalf("%s %s: status=%d body=%s, want 200", method, file.name, response.Code, response.Body.String())
				}
				for header, want := range map[string]string{
					"Content-Type":            file.download,
					"Content-Disposition":     "attachment",
					"X-Content-Type-Options":  "nosniff",
					"Content-Security-Policy": "sandbox; default-src 'none'",
				} {
					if got := response.Header().Get(header); got != want {
						t.Errorf("%s %s: %s = %q, want %q", method, file.name, header, got, want)
					}
				}
			}
			if served := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice"); !bytes.Equal(served.Body.Bytes(), file.content) {
				t.Errorf("%s's bytes = %q, want what was uploaded", file.name, served.Body.String())
			}
		})
	}
}
