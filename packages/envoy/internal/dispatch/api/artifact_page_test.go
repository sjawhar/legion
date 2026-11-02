package api

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
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
// attachment, so no page runs under Dispatch's own address. Nothing but Content-Disposition
// differs between the two.
func TestArtifactPageHeadersForAFrameAndForEveryOtherRequest(t *testing.T) {
	handler := newTestHandler(t)
	issue := pageTestIssue(t, handler)
	page := []byte("<!doctype html><title>Chart</title><script>parent.postMessage({type:'dispatch-view:ready'},'*')</script>")
	file := uploadPageTestFile(t, handler, issue, "chart.html", "text/html", page)
	digest := sha256.Sum256(page)
	etag := hex.EncodeToString(digest[:])

	target := "/api/v1/artifacts/" + file.Artifact.ID + "/versions/1/page"
	answers := map[string]*httptest.ResponseRecorder{
		"inline":     pageRequest(t, handler, http.MethodGet, target, "iframe"),
		"attachment": pageRequest(t, handler, http.MethodGet, target, ""),
	}
	for disposition, response := range answers {
		if response.Code != http.StatusOK {
			t.Fatalf("the %s answer: status=%d body=%s, want 200", disposition, response.Code, response.Body.String())
		}
		if got := response.Body.String(); got != string(page) {
			t.Errorf("the %s answer's body = %q, want the page's own bytes", got, page)
		}
		for header, want := range map[string]string{
			"Content-Type":            "text/html; charset=utf-8",
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

	// Sec-Fetch-Dest decides the disposition and nothing else, so a cache keyed on Vary serves
	// one of these answers to the other kind of request only in that one header.
	frame, tab := answers["inline"].Header().Clone(), answers["attachment"].Header().Clone()
	frame.Del("Content-Disposition")
	tab.Del("Content-Disposition")
	if !maps.EqualFunc(frame, tab, slices.Equal) {
		t.Errorf("the two answers differ beyond Content-Disposition:\nframe %v\ntab   %v", frame, tab)
	}

	// A dest that is not a frame is every other request: a document load, a script load, a
	// fetch, each saved rather than run.
	for _, dest := range []string{"document", "empty", "script", "frame"} {
		response := pageRequest(t, handler, http.MethodGet, target, dest)
		if response.Code != http.StatusOK || response.Header().Get("Content-Disposition") != "attachment" {
			t.Errorf("Sec-Fetch-Dest: %s = %d %q, want 200 attachment", dest, response.Code, response.Header().Get("Content-Disposition"))
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

// TestArtifactPageRefusesAFileThatIsNotAPageAndADocument: 415 NOT_A_PAGE for both. A document is
// refused though its markdown would render: its text is what Dispatch rewrites as edits settle,
// not the bytes an author sent.
func TestArtifactPageRefusesAFileThatIsNotAPageAndADocument(t *testing.T) {
	handler := newTestHandler(t)
	issue := pageTestIssue(t, handler)
	notes := uploadPageTestFile(t, handler, issue, "notes.txt", "text/plain", []byte("plain notes\n"))
	document := uploadPageTestFile(t, handler, issue, "notes.md", "text/markdown", []byte("# Notes\n\nA paragraph.\n"))

	requirePageRefusal(t, handler, "a file that is not a page",
		"/api/v1/artifacts/"+notes.Artifact.ID+"/versions/1/page", http.StatusUnsupportedMediaType, "NOT_A_PAGE")
	requirePageRefusal(t, handler, "a document",
		"/api/v1/artifacts/"+document.Artifact.ID+"/versions/1/page", http.StatusUnsupportedMediaType, "NOT_A_PAGE")
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
