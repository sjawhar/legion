package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/api/apitest"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// What the page a reader opens a document at carries, and what it does not (dispatch://INFRA-341
// R11, acceptance A11). A reader with a session that may read the document is sent its opening
// blocks in the page itself, before the page's own scripts; a reader with no session, or one
// whose session has been revoked, or one asking for a document that is not theirs to read, is
// sent the build's own shell with no word of the document in it. A page carrying a document is
// nobody's to keep: no store, no validator, and no 304 for a browser that asks with one.

// firstScreenPage is the dashboard page these rows serve: the shell's own shape - the build's
// inline bootstrap and its entry chunk in the head, as Vite writes them, and the marker the
// server replaces inside the element that holds a document's opening blocks, after `#root`
// because `createRoot` clears what it mounts into.
const firstScreenPage = `<!doctype html>
<html lang="en">
  <head>
    <title>Dispatch</title>
    <script>window.__dispatchEarlyDocumentRead = undefined;</script>
    <script type="module" crossorigin src="/assets/index-abc.js"></script>
    <link rel="stylesheet" crossorigin href="/assets/index-abc.css">
  </head>
  <body>
    <div id="root"></div>
    <div id="first-screen" class="dispatch-markdown prose"><!--first-screen--></div>
  </body>
</html>
`

// firstScreenRig is a router on a real database and a cookie identity, serving a dashboard build
// of one page, so a row can revoke the reader's session and ask again.
type firstScreenRig struct {
	cookie    *http.Cookie
	database  *store.Store
	documents *docs.Service
	handler   http.Handler
	sessions  *memorySessionStore
}

func newFirstScreenRig(t *testing.T) *firstScreenRig {
	t.Helper()
	database := storetest.Open(t)
	webDist := t.TempDir()
	if err := os.WriteFile(filepath.Join(webDist, "index.html"), []byte(firstScreenPage), 0o600); err != nil {
		t.Fatalf("write dashboard index: %v", err)
	}
	people := newMemoryPeople()
	if err := people.SignIn(context.Background(), "sami@d.example", "refresh-token"); err != nil {
		t.Fatalf("record sign-in: %v", err)
	}
	sessions := &memorySessionStore{generations: map[string]int64{"sami@d.example": 0}}
	broker := events.NewBroker()
	documents := docs.New(docs.Deps{Store: database, Events: broker})
	t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
	ctx, err := BuildAppContext(AppContextOptions{
		SigningKey: testSigningKey,
		People:     people,
		Sessions:   sessions,
		Identity:   identity.CookieIdentity{SigningKey: testSigningKey, Sessions: sessions},
		Store:      database,
		Docs:       documents,
		Events:     broker,
		WebDistDir: webDist,
		Lifetime:   t.Context(),
	})
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	cookie, err := http.ParseSetCookie(auth.IssueSessionCookie("sami@d.example", 0, testSigningKey, true))
	if err != nil {
		t.Fatalf("parse session cookie: %v", err)
	}
	handler, background := New(ctx)
	t.Cleanup(background.Wait)
	return &firstScreenRig{cookie: cookie, database: database, documents: documents, handler: handler, sessions: sessions}
}

// call makes one request as the signed-in reader, unless signedIn is false.
func (r *firstScreenRig) call(t *testing.T, method, path string, signedIn bool, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, "http://dispatch.test"+path, nil)
	if signedIn {
		request.AddCookie(r.cookie)
	}
	for name, value := range header {
		request.Header.Set(name, value)
	}
	response := httptest.NewRecorder()
	r.handler.ServeHTTP(response, request)
	return response
}

// write posts body as the signed-in reader and answers the response's JSON.
func (r *firstScreenRig) write(t *testing.T, path, body string) map[string]any {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "http://dispatch.test"+path, strings.NewReader(body))
	request.AddCookie(r.cookie)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://dispatch.test")
	response := httptest.NewRecorder()
	r.handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated && response.Code != http.StatusOK {
		t.Fatalf("POST %s: status=%d body=%s", path, response.Code, response.Body.String())
	}
	var answer map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode POST %s %s: %v", path, response.Body.String(), err)
	}
	return answer
}

// seedDocument writes an issue holding one document and answers the issue's key and the
// document's slug.
func (r *firstScreenRig) seedDocument(t *testing.T, markdown string) (string, string) {
	t.Helper()
	r.write(t, "/api/v1/projects", `{"key":"BIG","name":"Big documents"}`)
	issue := r.write(t, "/api/v1/issues", `{"project":"BIG","title":"A document"}`)
	key, _ := issue["key"].(string)
	if key == "" {
		key, _ = issue["issue"].(map[string]any)["key"].(string)
	}
	if key == "" {
		t.Fatalf("the issue answered no key: %v", issue)
	}
	body, err := json.Marshal(map[string]string{"content": markdown, "name": "notes.md"})
	if err != nil {
		t.Fatalf("encode the document: %v", err)
	}
	created := r.write(t, "/api/v1/issues/"+key+"/artifacts", string(body))
	artifact, _ := created["artifact"].(map[string]any)
	slug, _ := artifact["slug"].(string)
	if slug == "" {
		t.Fatalf("the document answered no slug: %v", created)
	}
	return key, slug
}

// documentID is the id of the issue document at slug, read through the API the page reads it by.
func (r *firstScreenRig) documentID(t *testing.T, key, slug string) string {
	t.Helper()
	response := r.call(t, http.MethodGet, "/api/v1/issues/"+key+"/artifacts/"+slug, true, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("read the document: status=%d body=%s", response.Code, response.Body.String())
	}
	var artifact struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &artifact); err != nil {
		t.Fatalf("decode the document %s: %v", response.Body.String(), err)
	}
	if artifact.ID == "" {
		t.Fatalf("the document answered no id: %s", response.Body.String())
	}
	return artifact.ID
}

// theDocument is the markdown these rows seed: a heading and two paragraphs, each a block of its
// own, so a page carrying the document carries three block ids and their text.
const theDocument = "# The lease window\n\nThe window opens on the first lease.\n\nIt closes on the last.\n"

// blockIDsIn is every `data-block-id` value in page, in the order the page carries them.
func blockIDsIn(page string) []string {
	ids := []string{}
	rest := page
	for {
		at := strings.Index(rest, `data-block-id="`)
		if at < 0 {
			return ids
		}
		rest = rest[at+len(`data-block-id="`):]
		end := strings.IndexByte(rest, '"')
		if end < 0 {
			return ids
		}
		ids = append(ids, rest[:end])
		rest = rest[end:]
	}
}

// scriptTagsIn is every script start tag in page, in the order the page carries them.
func scriptTagsIn(page string) []string {
	tags := []string{}
	rest := page
	for {
		at := strings.Index(rest, "<script")
		if at < 0 {
			return tags
		}
		rest = rest[at:]
		end := strings.IndexByte(rest, '>')
		if end < 0 {
			return append(tags, rest)
		}
		tags = append(tags, rest[:end+1])
		rest = rest[end+1:]
	}
}

func TestDocumentPageCarriesItsOpeningBlocks(t *testing.T) {
	rig := newFirstScreenRig(t)
	key, slug := rig.seedDocument(t, theDocument)

	response := rig.call(t, http.MethodGet, "/issues/"+key+"/artifacts/"+slug, true, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("the document page: status=%d body=%s", response.Code, response.Body.String())
	}
	page := response.Body.String()
	for _, text := range []string{"The lease window", "The window opens on the first lease.", "It closes on the last."} {
		if !strings.Contains(page, text) {
			t.Fatalf("the page carries no %q:\n%s", text, page)
		}
	}
	ids := blockIDsIn(page)
	if len(ids) != 3 {
		t.Fatalf("the page carries %d block ids, want the document's three: %v", len(ids), ids)
	}
	for _, id := range ids {
		if id == "" {
			t.Fatalf("the page carries an empty block id: %v", ids)
		}
	}
	// Nothing the browser has to fetch and run stands between the page's first byte and the
	// reader's first block. Every script the page carries before the blocks is the build's own
	// bootstrap: inline, so it costs no request, or `type="module"`, which is deferred until the
	// document has been parsed. So the blocks are painted from the page's own bytes, with no app
	// code downloaded, evaluated or run - which is what the browser row with the app's code
	// blocked proves from the other side.
	blocks := strings.Index(page, `data-block-id="`)
	if blocks < 0 {
		t.Fatalf("the page carries no block at all:\n%s", page)
	}
	for _, tag := range scriptTagsIn(page[:blocks]) {
		if strings.Contains(tag, " src=") && !strings.Contains(tag, `type="module"`) {
			t.Fatalf("a script the browser must fetch and run stands before the document: %s", tag)
		}
	}
	if label := strings.Index(page, `aria-label="Document text"`); label < 0 || label > blocks {
		t.Fatalf("the document is not named as the page's own preview names it:\n%s", page)
	}
	if root := strings.Index(page, `id="root"`); root < 0 || root > blocks {
		t.Fatalf("the document's blocks do not sit after the app's own root:\n%s", page)
	}
	// Nobody's to keep, and no validator for a browser to revalidate with.
	if got := response.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("Cache-Control = %q, want %q", got, "private, no-store")
	}
	if got := response.Header().Get("ETag"); got != "" {
		t.Fatalf("the document page carries an ETag %q", got)
	}
}

func TestDocumentPageCarriesTheIssuesSpec(t *testing.T) {
	rig := newFirstScreenRig(t)
	rig.write(t, "/api/v1/projects", `{"key":"BIG","name":"Big documents"}`)
	issue := rig.write(t, "/api/v1/issues", `{"project":"BIG","title":"A document","spec":`+
		`"# The lease window\n\nThe window opens on the first lease.\n"}`)
	key, _ := issue["key"].(string)
	if key == "" {
		t.Fatalf("the issue answered no key: %v", issue)
	}

	page := rig.call(t, http.MethodGet, "/issues/"+key, true, nil).Body.String()
	if !strings.Contains(page, "The window opens on the first lease.") {
		t.Fatalf("the Spec tab's page carries no spec:\n%s", page)
	}
	if len(blockIDsIn(page)) == 0 {
		t.Fatalf("the Spec tab's page carries no block ids:\n%s", page)
	}
}

func TestDocumentPageWithoutASessionCarriesNoDocument(t *testing.T) {
	rig := newFirstScreenRig(t)
	key, slug := rig.seedDocument(t, theDocument)

	response := rig.call(t, http.MethodGet, "/issues/"+key+"/artifacts/"+slug, false, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("the page without a session: status=%d body=%s", response.Code, response.Body.String())
	}
	page := response.Body.String()
	if page != firstScreenPage {
		t.Fatalf("the page without a session is not the build's own shell:\n%s", page)
	}
	for _, text := range []string{"The lease window", "data-block-id"} {
		if strings.Contains(page, text) {
			t.Fatalf("the page without a session carries %q:\n%s", text, page)
		}
	}
	if got := response.Header().Get("Cache-Control"); got != pageCacheControl {
		t.Fatalf("the shell's Cache-Control = %q, want %q", got, pageCacheControl)
	}
}

func TestDocumentPageWithARevokedSessionCarriesNoDocument(t *testing.T) {
	rig := newFirstScreenRig(t)
	key, slug := rig.seedDocument(t, theDocument)
	address := "/issues/" + key + "/artifacts/" + slug
	if !strings.Contains(rig.call(t, http.MethodGet, address, true, nil).Body.String(), "The lease window") {
		t.Fatal("the reader's own session read no document to begin with")
	}

	if err := rig.sessions.RevokeSessions(context.Background(), "sami@d.example"); err != nil {
		t.Fatalf("revoke the session: %v", err)
	}

	page := rig.call(t, http.MethodGet, address, true, nil).Body.String()
	if page != firstScreenPage {
		t.Fatalf("a revoked session was sent a document:\n%s", page)
	}
}

func TestDocumentPageOfAnAddressNamingNoDocumentCarriesNothing(t *testing.T) {
	rig := newFirstScreenRig(t)
	key, _ := rig.seedDocument(t, theDocument)

	for _, address := range []string{
		"/issues/" + key + "/artifacts/no-such-document",
		"/issues/NOSUCH-9/artifacts/notes-md",
		"/issues/NOSUCH-9",
	} {
		page := rig.call(t, http.MethodGet, address, true, nil).Body.String()
		if page != firstScreenPage {
			t.Fatalf("%s was sent a document:\n%s", address, page)
		}
	}
}

// A file is not a document, and `/text` answers NOT_DOCUMENT for it. The page draws nothing
// rather than inventing a reading of bytes nobody asked it to read.
func TestPageOfAnAddressNamingAFileCarriesNoDocument(t *testing.T) {
	rig := newFirstScreenRig(t)
	key, _ := rig.seedDocument(t, theDocument)
	body, bodyType, err := apitest.MultipartUpload(map[string]string{"name": "a.png"}, "a.png", "image/png", []byte("an image"))
	if err != nil {
		t.Fatalf("build the upload: %v", err)
	}
	upload := httptest.NewRequest(http.MethodPost, "http://dispatch.test/api/v1/issues/"+key+"/artifacts", body)
	upload.AddCookie(rig.cookie)
	upload.Header.Set("Content-Type", bodyType)
	upload.Header.Set("Origin", "http://dispatch.test")
	uploaded := httptest.NewRecorder()
	rig.handler.ServeHTTP(uploaded, upload)
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload a file: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	var created struct {
		Artifact struct {
			Slug string `json:"slug"`
		} `json:"artifact"`
	}
	if err := json.Unmarshal(uploaded.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode the upload %s: %v", uploaded.Body.String(), err)
	}

	page := rig.call(t, http.MethodGet, "/issues/"+key+"/artifacts/"+created.Artifact.Slug, true, nil).Body.String()
	if page != firstScreenPage {
		t.Fatalf("a file's address was sent a document:\n%s", page)
	}
}

// A document whose tree this server cannot read is one the page cannot draw, and the reader's
// own editor meets that refusal with its reason a moment later. The page is the shell.
func TestPageOfADocumentThatWillNotLoadCarriesNoDocument(t *testing.T) {
	rig := newFirstScreenRig(t)
	key, slug := rig.seedDocument(t, theDocument)
	address := "/issues/" + key + "/artifacts/" + slug
	if !strings.Contains(rig.call(t, http.MethodGet, address, true, nil).Body.String(), "The lease window") {
		t.Fatal("the document did not draw to begin with")
	}
	artifact := rig.documentID(t, key, slug)
	if err := rig.documents.InjectSchemaInvalidForTest(context.Background(), artifact); err != nil {
		t.Fatalf("make the document unreadable: %v", err)
	}

	page := rig.call(t, http.MethodGet, address, true, nil).Body.String()
	if page != firstScreenPage {
		t.Fatalf("a document that will not load was drawn:\n%s", page)
	}
}

// A pinned version is a different document than the one this draws, and every other address
// opens no document at all, so each is the build's own shell with its own caching.
func TestOnlyADocumentAddressCarriesADocument(t *testing.T) {
	rig := newFirstScreenRig(t)
	key, slug := rig.seedDocument(t, theDocument)

	for _, address := range []string{
		"/issues/" + key + "/artifacts/" + slug + "?v=1",
		"/issues/" + key + "/activity",
		"/issues/" + key + "/artifacts/" + slug + "/extra",
		"/inbox",
		"/",
	} {
		response := rig.call(t, http.MethodGet, address, true, nil)
		if response.Body.String() != firstScreenPage {
			t.Fatalf("%s was sent a document:\n%s", address, response.Body.String())
		}
		if got := response.Header().Get("ETag"); got == "" {
			t.Fatalf("%s is a shell and carries no ETag", address)
		}
	}
}

// A browser that kept a page it was sent before - another reader's document, or one this reader
// may no longer read - must never be told to use it again. The document page carries no
// validator, so there is nothing for an If-None-Match to match, and the whole page is answered.
func TestDocumentPageNeverAnswers304(t *testing.T) {
	rig := newFirstScreenRig(t)
	key, slug := rig.seedDocument(t, theDocument)
	address := "/issues/" + key + "/artifacts/" + slug

	first := rig.call(t, http.MethodGet, address, true, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("the first read: status=%d", first.Code)
	}
	shell := rig.call(t, http.MethodGet, "/inbox", true, nil)
	tag := shell.Header().Get("ETag")
	if tag == "" {
		t.Fatal("the shell carries no ETag to ask with")
	}

	for _, condition := range []string{tag, "*", `"anything"`} {
		again := rig.call(t, http.MethodGet, address, true, map[string]string{"If-None-Match": condition})
		if again.Code != http.StatusOK {
			t.Fatalf("If-None-Match %s: status=%d, want 200", condition, again.Code)
		}
		if !strings.Contains(again.Body.String(), "The lease window") {
			t.Fatalf("If-None-Match %s answered no document:\n%s", condition, again.Body.String())
		}
	}
}

// All four addresses a document opens at carry it: the issue's own spec at `/issues/{key}` and
// at `/issues/{key}/spec`, which a link to a block of the spec names, and the issue document
// under both spellings of its artifact path, which are both written.
func TestEveryDocumentAddressCarriesItsDocument(t *testing.T) {
	rig := newFirstScreenRig(t)
	rig.write(t, "/api/v1/projects", `{"key":"BIG","name":"Big documents"}`)
	issue := rig.write(t, "/api/v1/issues", `{"project":"BIG","title":"A document","spec":`+
		`"# The lease window\n\nThe window opens on the first lease.\n"}`)
	key, _ := issue["key"].(string)
	if key == "" {
		t.Fatalf("the issue answered no key: %v", issue)
	}
	body, err := json.Marshal(map[string]string{"content": theDocument, "name": "notes.md"})
	if err != nil {
		t.Fatalf("encode the document: %v", err)
	}
	created := rig.write(t, "/api/v1/issues/"+key+"/artifacts", string(body))
	slug, _ := created["artifact"].(map[string]any)["slug"].(string)
	if slug == "" {
		t.Fatalf("the document answered no slug: %v", created)
	}

	for _, address := range []string{
		"/issues/" + key,
		"/issues/" + key + "/spec",
		"/issues/" + key + "/artifacts/" + slug,
		"/issues/" + key + "/artifact/" + slug,
	} {
		page := rig.call(t, http.MethodGet, address, true, nil).Body.String()
		if !strings.Contains(page, "The window opens on the first lease.") {
			t.Fatalf("%s carries no document:\n%s", address, page)
		}
		if len(blockIDsIn(page)) == 0 {
			t.Fatalf("%s carries no block ids:\n%s", address, page)
		}
	}
}

// The page is what the reader is waiting for. A document the server cannot draw inside its
// budget is one the page's own read draws for them a moment later, so the page goes without it
// rather than holding every reader of that document for the wait.
func TestADocumentReadPastItsBudgetSendsTheShell(t *testing.T) {
	rig := newFirstScreenRig(t)
	key, slug := rig.seedDocument(t, theDocument)
	address := "/issues/" + key + "/artifacts/" + slug
	if !strings.Contains(rig.call(t, http.MethodGet, address, true, nil).Body.String(), "The lease window") {
		t.Fatal("the document did not draw to begin with")
	}

	budget := firstScreenBudget
	firstScreenBudget = time.Nanosecond
	t.Cleanup(func() { firstScreenBudget = budget })

	response := rig.call(t, http.MethodGet, address, true, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("the page past its budget: status=%d", response.Code)
	}
	if response.Body.String() != firstScreenPage {
		t.Fatalf("a read past its budget still drew:\n%s", response.Body.String())
	}
	if got := response.Header().Get("Cache-Control"); got != pageCacheControl {
		t.Fatalf("the shell's Cache-Control = %q, want %q", got, pageCacheControl)
	}
}

// The bytes are one reader's document: no shared cache may hold them, no browser may keep them,
// and there is no validator for either to revalidate with.
func TestADocumentPageIsNobodysToKeep(t *testing.T) {
	rig := newFirstScreenRig(t)
	key, slug := rig.seedDocument(t, theDocument)

	response := rig.call(t, http.MethodGet, "/issues/"+key+"/artifacts/"+slug, true, nil)
	if got := response.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("Cache-Control = %q, want %q", got, "private, no-store")
	}
	if got := response.Header().Get("ETag"); got != "" {
		t.Fatalf("the document page carries an ETag %q", got)
	}
	if got := response.Header().Get("Last-Modified"); got != "" {
		t.Fatalf("the document page carries a Last-Modified %q", got)
	}
}
