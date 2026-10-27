package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api/apitest"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/files"
	"github.com/sjawhar/envoy/internal/dispatch/files/filestest"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func createArtifactIssue(t *testing.T, handler http.Handler) model.Issue {
	t.Helper()
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Artifact issue",
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[model.Issue](t, response)
}

func TestUploadArtifactJSONCreatesDocumentVersionOne(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)

	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"name": "architect-spec.md", "content": "# Spec\n", "summary": "Initial spec",
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create JSON artifact: status=%d body=%s", response.Code, response.Body.String())
	}
	upload := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
		Version  model.Version  `json:"version"`
	}](t, response)
	if upload.Artifact.Kind != "doc" || upload.Artifact.Primary || upload.Version.Number != 1 {
		t.Fatalf("JSON artifact = %#v version=%#v, want non-primary document version 1", upload.Artifact, upload.Version)
	}
	text := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+upload.Artifact.ID+"/text", nil, "alice")
	if text.Code != http.StatusOK || !strings.Contains(text.Body.String(), `"markdown":"# Spec\n"`) {
		t.Fatalf("read JSON artifact text: status=%d body=%s", text.Code, text.Body.String())
	}
	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if events.Code != http.StatusOK || !strings.Contains(events.Body.String(), `"type":"artifact.created"`) {
		t.Fatalf("JSON artifact events: status=%d body=%s", events.Code, events.Body.String())
	}
}

func TestUploadArtifactRejectsPrimaryFlag(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)
	path := "/api/v1/issues/" + issue.Key + "/artifacts"

	assertRejected := func(t *testing.T, name string, responseCode int, body string, artifactName string) {
		t.Helper()
		if responseCode != http.StatusBadRequest || !strings.Contains(body, `"code":"ARTIFACT_INPUT"`) || !strings.Contains(body, "fixed at issue creation") {
			t.Fatalf("%s primary flag: status=%d body=%s", name, responseCode, body)
		}
		list := dispatchRequest(t, handler, http.MethodGet, path, nil, "alice")
		if list.Code != http.StatusOK {
			t.Fatalf("list artifacts after %s primary flag: status=%d body=%s", name, list.Code, list.Body.String())
		}
		for _, artifact := range decodeBody[[]model.Artifact](t, list) {
			if artifact.Name == artifactName {
				t.Fatalf("%s primary flag created artifact %#v", name, artifact)
			}
		}
	}

	for _, test := range []struct {
		name    string
		primary any
	}{
		{name: "true", primary: true},
		{name: "false", primary: false},
		{name: "null", primary: nil},
	} {
		t.Run("JSON "+test.name, func(t *testing.T) {
			artifactName := "json-primary-" + test.name + ".md"
			response := dispatchRequest(t, handler, http.MethodPost, path, map[string]any{
				"name": artifactName, "content": "# JSON primary\n", "primary": test.primary,
			}, "alice")
			assertRejected(t, "JSON "+test.name, response.Code, response.Body.String(), artifactName)
		})
	}

	multipartResponse := multipartRequest(t, handler, path, map[string]string{
		"name": "multipart-primary.md", "primary": "true",
	}, "multipart-primary.md", "text/markdown", []byte("# Multipart primary\n"), "alice")
	assertRejected(t, "multipart", multipartResponse.Code, multipartResponse.Body.String(), "multipart-primary.md")
}

func TestPrimaryRoutesAreGone(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)

	for _, target := range []string{
		"/api/v1/artifacts/" + issue.PrimaryArtifactID + "/primary",
		"/api/v1/issues/" + issue.Key + "/artifacts/spec/primary",
	} {
		response := dispatchRequest(t, handler, http.MethodPost, target, map[string]any{}, "alice")
		if response.Code != http.StatusNotFound {
			t.Fatalf("primary route %s: status=%d body=%s, want 404", target, response.Code, response.Body.String())
		}
	}
}
func TestUploadArtifactJSONCreatesNextVersion(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)
	path := "/api/v1/issues/" + issue.Key + "/artifacts"

	first := dispatchRequest(t, handler, http.MethodPost, path, map[string]string{
		"name": "architect-spec.md", "content": "# Initial\n",
	}, "alice")
	if first.Code != http.StatusCreated {
		t.Fatalf("create JSON artifact: status=%d body=%s", first.Code, first.Body.String())
	}
	second := dispatchRequest(t, handler, http.MethodPost, path, map[string]string{
		"name": "architect-spec.md", "content": "# Revised\n", "summary": "Revision",
	}, "alice")
	if second.Code != http.StatusCreated {
		t.Fatalf("create JSON artifact version: status=%d body=%s", second.Code, second.Body.String())
	}
	upload := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
		Version  model.Version  `json:"version"`
	}](t, second)
	if upload.Version.Number != 2 || !upload.Version.Named || upload.Version.Summary == nil || *upload.Version.Summary != "Revision" {
		t.Fatalf("JSON artifact version = %#v, want named version 2", upload.Version)
	}
	text := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+upload.Artifact.ID+"/text", nil, "alice")
	if text.Code != http.StatusOK || !strings.Contains(text.Body.String(), `"markdown":"# Revised\n"`) {
		t.Fatalf("read revised JSON artifact: status=%d body=%s", text.Code, text.Body.String())
	}
	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if events.Code != http.StatusOK || !strings.Contains(events.Body.String(), `"type":"artifact.version"`) {
		t.Fatalf("JSON artifact version events: status=%d body=%s", events.Code, events.Body.String())
	}
}

func TestUploadArtifactJSONRejectsBlankContent(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)
	for _, content := range []string{"", " \n\t "} {
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
			"name": "architect-spec.md", "content": content,
		}, "alice")
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"ARTIFACT_INPUT"`) {
			t.Fatalf("blank JSON content %q: status=%d body=%s", content, response.Code, response.Body.String())
		}
	}
}

func TestUploadArtifactJSONReturnsCanonicalMarkdown(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "architect-spec.md", "content": " \n# Architect spec\n ",
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("upload nonblank JSON content: status=%d body=%s", response.Code, response.Body.String())
	}
	upload := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, response)
	text := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+upload.Artifact.ID+"/text", nil, "alice")
	if text.Code != http.StatusOK {
		t.Fatalf("read nonblank JSON content: status=%d body=%s", text.Code, text.Body.String())
	}
	document := decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, text)
	if document.Markdown != "# Architect spec\n" {
		t.Fatalf("stored Markdown = %q, want canonical heading", document.Markdown)
	}
}

func TestUploadRejectsMarkdownOutsideProofSchema(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "architect-spec.md", "content": "<details>x</details>",
	}, "alice")
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_MARKDOWN"`) {
		t.Fatalf("invalid JSON Markdown: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestUploadArtifactRejectsFileAndContent(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)
	response := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "architect-spec.md", "content": "# Inline\n",
	}, "architect-spec.md", "text/markdown", []byte("# File\n"), "alice")
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"ARTIFACT_INPUT"`) {
		t.Fatalf("file and content: status=%d body=%s", response.Code, response.Body.String())
	}
}
func TestUploadArtifactMultipartTrimsNameBeforeMIMEInference(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)
	response := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": " architect-spec.md ",
	}, "architect-spec.md", "", []byte("# Architect spec\n"), "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("upload whitespace-named document: status=%d body=%s", response.Code, response.Body.String())
	}
	upload := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, response)
	if upload.Artifact.Name != "architect-spec.md" || upload.Artifact.Kind != "doc" || upload.Artifact.Primary {
		t.Fatalf("whitespace-named document = %#v, want non-primary Markdown document", upload.Artifact)
	}
}

// pngBytes is a PNG's eight-byte signature and then body: what http.DetectContentType reads as
// image/png, all an upload needs to be stored as a picture. Each call is a slice of its own.
func pngBytes(body string) []byte {
	return append([]byte("\x89PNG\r\n\x1a\n"), body...)
}

// An upload's kind is read from its bytes, never from the type its client declares, which any
// client sets at will. It is an image only when its bytes are one of the pictures a model is
// shown (PNG, JPEG, GIF, WebP), stored, kept in the file store and served under the type its
// bytes are; anything else, an SVG for one, is a file under the type its client declared. The
// rule is the same on an issue and in an agent's conversation.
func TestAnUploadIsAPictureOnlyWhenItsBytesAreOne(t *testing.T) {
	memory := filestest.NewMemory()
	handler, _, _ := newTestServer(t, testServerOptions{files: memory})
	issue := fileIssue(t, handler)
	for _, owner := range []string{"/api/v1/issues/" + issue, "/api/v1/agents/" + agentSession} {
		for _, test := range []struct {
			name, declared string
			content        []byte
			kind, mime     string
		}{
			{"not-a-picture.png", "image/png", []byte("these bytes are text, whatever the type says"), "file", "image/png"},
			{"undeclared.png", "application/octet-stream", pngBytes("pixels"), "image", "image/png"},
			{"jpeg-declared-png.png", "image/png", []byte("\xff\xd8\xff\xe0\x00\x10JFIF"), "image", "image/jpeg"},
			{"animation.gif", "image/gif", []byte("GIF89a\x01\x00\x01\x00"), "image", "image/gif"},
			{"photo.webp", "application/octet-stream", []byte("RIFF\x24\x00\x00\x00WEBPVP8 frame"), "image", "image/webp"},
			{"drawing.svg", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), "file", "image/svg+xml"},
		} {
			t.Run(owner+"/"+test.name, func(t *testing.T) {
				response := multipartRequest(t, handler, owner+"/artifacts", map[string]string{"name": test.name}, test.name, test.declared, test.content, "alice")
				if response.Code != http.StatusCreated {
					t.Fatalf("upload: status=%d body=%s", response.Code, response.Body.String())
				}
				created := decodeBody[struct {
					Artifact model.Artifact `json:"artifact"`
					Version  model.Version  `json:"version"`
				}](t, response)
				if created.Artifact.Kind != test.kind || created.Version.MIME == nil || *created.Version.MIME != test.mime {
					t.Fatalf("declared %s: kind %q, version %+v; want kind %q stored as %s", test.declared, created.Artifact.Kind, created.Version, test.kind, test.mime)
				}
				if stored := memory.MIME(files.SHA256(test.content)); stored != test.mime {
					t.Errorf("the file store holds it as %q, want %s", stored, test.mime)
				}
				served := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+created.Artifact.ID+"/versions/1", nil, "alice")
				if served.Code != http.StatusOK || served.Header().Get("Content-Type") != test.mime || !bytes.Equal(served.Body.Bytes(), test.content) {
					t.Fatalf("download: status=%d Content-Type %q, want 200 %s with the bytes", served.Code, served.Header().Get("Content-Type"), test.mime)
				}
			})
		}
	}
}

func TestUploadArtifactJSONRejectsOversizedContent(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "architect-spec.md", "content": strings.Repeat("x", maxArtifactBlobSize+1),
	}, "alice")
	if response.Code != http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), `"code":"CAP_EXCEEDED"`) {
		t.Fatalf("oversized JSON content: status=%d body=%s", response.Code, response.Body.String())
	}
}

// A markdown document is bounded at 1 MiB, the bound an issue's spec and every edit already have
// (maxJSONRequestBytes), and three times the largest document ever stored; a binary artifact keeps
// 25 MiB (LEGION-465, D6).
func TestUploadRefusesAMarkdownDocumentOverOneMiB(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)
	over := []byte(strings.Repeat("word ", (maxDocumentMarkdownBytes/5)+1))
	response := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{"name": "big.md"}, "big.md", "text/markdown", over, "alice")
	if response.Code != http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), `"code":"CAP_EXCEEDED"`) || !strings.Contains(response.Body.String(), "1 MiB") {
		t.Fatalf("markdown over 1 MiB: status=%d body=%s, want 413 CAP_EXCEEDED naming 1 MiB", response.Code, response.Body.String())
	}
	blob := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{"name": "big.bin"}, "big.bin", "application/octet-stream", over, "alice")
	if blob.Code != http.StatusCreated {
		t.Fatalf("binary of the same size: status=%d body=%s, want 201", blob.Code, blob.Body.String())
	}
}

// A multipart body the parser cannot read, such as one whose part header holds a control
// character, is the caller's malformed input: 400 with the parser's reason. Only a body past the
// upload's size limit is 413.
func TestUploadRefusesAMalformedMultipartBodyAsBadInput(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)
	send := func(contentType string, body []byte) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", bytes.NewReader(body))
		request.Header.Set("Content-Type", contentType)
		request.Header.Set("X-Dispatch-User", "alice")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	malformed := send("multipart/form-data; boundary=b", []byte("--b\r\n"+
		"Content-Disposition: form-data; name=\"file\"; filename=\"notes.md\"\r\n"+
		"Content-Type: text/markdown; x=\"a\x00b\"\r\n\r\n# Notes\r\n--b--\r\n"))
	if malformed.Code != http.StatusBadRequest || !strings.Contains(malformed.Body.String(), `"code":"ARTIFACT_INPUT"`) || !strings.Contains(malformed.Body.String(), `"error":"invalid multipart body: `) {
		t.Fatalf("part header holding a control character: status=%d body=%s, want 400 ARTIFACT_INPUT with the parser's reason", malformed.Code, malformed.Body.String())
	}
	over := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{"name": "big.bin"}, "big.bin", "application/octet-stream", make([]byte, maxArtifactBlobSize+(1<<20)+1), "alice")
	if over.Code != http.StatusRequestEntityTooLarge || !strings.Contains(over.Body.String(), `"code":"CAP_EXCEEDED"`) {
		t.Fatalf("body past the upload limit: status=%d body=%s, want 413 CAP_EXCEEDED", over.Code, over.Body.String())
	}
}

// The parser spools a file part past a megabyte to a temporary file. When the server cannot write
// that file, the failure is the server's, not the caller's: 500, logged, never a 400 that tells the
// caller its upload was malformed.
func TestAnUploadTheServerCannotSpoolIsItsOwnFailure(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	response := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{"name": "big.bin"}, "big.bin", "application/octet-stream", make([]byte, 2<<20), "alice")
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), `"code":"INTERNAL"`) {
		t.Fatalf("a part the server cannot spool: status=%d body=%s, want 500 INTERNAL", response.Code, response.Body.String())
	}
}

func TestArtifactRoutesResolveUUIDsAndIssueScopedSlugs(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)
	otherIssueResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Separate document record",
	}, "alice")
	if otherIssueResponse.Code != http.StatusCreated {
		t.Fatalf("create other issue: status=%d body=%s", otherIssueResponse.Code, otherIssueResponse.Body.String())
	}
	otherIssue := decodeBody[model.Issue](t, otherIssueResponse)

	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "architect-spec.md", "content": "# Artifact routes\n",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create artifact: status=%d body=%s", created.Code, created.Body.String())
	}
	upload := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, created)

	for _, route := range []struct {
		name   string
		method string
		suffix string
		body   any
	}{
		{name: "details", method: http.MethodGet},
		{name: "text", method: http.MethodGet, suffix: "/text"},
		{name: "version", method: http.MethodGet, suffix: "/versions/1"},
		{name: "named version", method: http.MethodPost, suffix: "/versions", body: map[string]string{"summary": "Named version"}},
		{name: "edit", method: http.MethodPost, suffix: "/edits", body: map[string]any{"ops": []any{}}},
	} {
		t.Run("invalid UUID "+route.name, func(t *testing.T) {
			response := dispatchRequest(t, handler, route.method, "/api/v1/artifacts/"+upload.Artifact.Slug+route.suffix, route.body, "alice")
			if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"code":"ARTIFACT_NOT_FOUND"`) {
				t.Fatalf("invalid UUID %s: status=%d body=%s", route.name, response.Code, response.Body.String())
			}
		})
	}

	for _, route := range []struct {
		name   string
		suffix string
	}{
		{name: "details", suffix: ""},
		{name: "text", suffix: "/text"},
		{name: "version", suffix: "/versions/1"},
	} {
		t.Run(route.name, func(t *testing.T) {
			byID := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+upload.Artifact.ID+route.suffix, nil, "alice")
			bySlug := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/artifacts/"+upload.Artifact.Slug+route.suffix, nil, "alice")
			if byID.Code != http.StatusOK || bySlug.Code != http.StatusOK {
				t.Fatalf("read by ID=%d body=%s; by slug=%d body=%s", byID.Code, byID.Body.String(), bySlug.Code, bySlug.Body.String())
			}
			if byID.Body.String() != bySlug.Body.String() {
				t.Fatalf("read response by ID=%s, by slug=%s", byID.Body.String(), bySlug.Body.String())
			}
		})
	}

	for _, route := range []struct {
		name   string
		method string
		suffix string
		body   any
	}{
		{name: "named version", method: http.MethodPost, suffix: "/versions", body: map[string]string{"summary": "Named version"}},
		{name: "edit", method: http.MethodPost, suffix: "/edits", body: map[string]any{"ops": []any{}}},
	} {
		t.Run(route.name, func(t *testing.T) {
			for _, target := range []string{
				"/api/v1/artifacts/" + upload.Artifact.ID + route.suffix,
				"/api/v1/issues/" + issue.Key + "/artifacts/" + upload.Artifact.Slug + route.suffix,
			} {
				response := dispatchRequest(t, handler, route.method, target, route.body, "alice")
				if response.Code != http.StatusOK && response.Code != http.StatusCreated {
					t.Fatalf("%s %s: status=%d body=%s", route.method, target, response.Code, response.Body.String())
				}
			}
		})
	}

	for _, target := range []string{
		"/api/v1/issues/" + issue.Key + "/artifacts/missing",
		"/api/v1/issues/" + otherIssue.Key + "/artifacts/" + upload.Artifact.Slug,
	} {
		response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
		if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"code":"ARTIFACT_NOT_FOUND"`) {
			t.Fatalf("missing scoped artifact %s: status=%d body=%s", target, response.Code, response.Body.String())
		}
	}
}

func TestDocumentRoutesResolveUnambiguousFilenameAliases(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)
	projectDocument := createProjectDocument(t, handler, "TEST", "runbook.md", "# Runbook\n")

	for _, target := range []string{
		"/api/v1/issues/" + issue.Key + "/artifacts/spec.md",
		"/api/v1/projects/TEST/artifacts/" + projectDocument.Name,
	} {
		response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("read document by filename %s: status=%d body=%s", target, response.Code, response.Body.String())
		}
	}
}

func TestMissingIssueDocumentRouteListsItsSlugsAndNames(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "plan.md", "content": "# Plan\n",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create plan document: status=%d body=%s", created.Code, created.Body.String())
	}

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/artifacts/missing.md", nil, "alice")
	want := `document "missing.md" not found by slug; this issue's documents: spec (spec.md), plan-md (plan.md)`
	raw := response.Body.String()
	body := decodeBody[struct {
		Error string `json:"error"`
	}](t, response)
	if response.Code != http.StatusNotFound || !strings.Contains(raw, `"code":"ARTIFACT_NOT_FOUND"`) || body.Error != want {
		t.Fatalf("missing document route: status=%d body=%s, want 404 %q", response.Code, raw, want)
	}
}

func TestDocumentRoutesRejectAmbiguousFilenameWithSlugChoices(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createArtifactIssue(t, handler)
	issueDocument := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "duplicate.md", "content": "# Duplicate\n",
	}, "alice")
	if issueDocument.Code != http.StatusCreated {
		t.Fatalf("create issue document: status=%d body=%s", issueDocument.Code, issueDocument.Body.String())
	}
	projectDocument := createProjectDocument(t, handler, "TEST", "duplicate.md", "# Duplicate\n")
	creator := []byte(`{"kind":"user","id":"alice"}`)
	for _, issueKey := range []any{issue.Key, nil} {
		if _, err := database.Pool.Exec(context.Background(), `
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			values ($1, $2, $3, $4, $5, false, $6)
		`, issueKey, "TEST", "duplicate-md-2", "duplicate.md", "doc", creator); err != nil {
			t.Fatalf("insert duplicate document: %v", err)
		}
	}

	for _, test := range []struct {
		name   string
		target string
		scope  string
	}{
		{name: "issue", target: "/api/v1/issues/" + issue.Key + "/artifacts/duplicate.md", scope: "issue"},
		{name: "project", target: "/api/v1/projects/TEST/artifacts/" + projectDocument.Name, scope: "project"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := dispatchRequest(t, handler, http.MethodGet, test.target, nil, "alice")
			raw := response.Body.String()
			body := decodeBody[struct {
				Code  string `json:"code"`
				Error string `json:"error"`
			}](t, response)
			want := `"duplicate.md" names 2 documents on this ` + test.scope + "; use a slug: duplicate-md (duplicate.md), duplicate-md-2 (duplicate.md)"
			if response.Code != http.StatusBadRequest || body.Code != "ARTIFACT_REFERENCE_AMBIGUOUS" || body.Error != want {
				t.Fatalf("ambiguous document route: status=%d body=%s, want 400 ARTIFACT_REFERENCE_AMBIGUOUS %q", response.Code, raw, want)
			}
		})
	}
}

// unrecordedPeople is a people store for a test with no database: it records nobody.
type unrecordedPeople struct{ auth.PeopleStore }

func (unrecordedPeople) Record(context.Context, string) error { return nil }

func TestArtifactIDRoutesValidateBeforeDatabaseUse(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux, Deps{
		Store:    &store.Store{},
		Identity: identity.HeaderIdentity{Header: "X-Dispatch-User", People: unrecordedPeople{}},
	})

	for _, route := range []struct {
		name   string
		method string
		suffix string
		body   any
	}{
		{name: "details", method: http.MethodGet},
		{name: "text", method: http.MethodGet, suffix: "/text"},
		{name: "version", method: http.MethodGet, suffix: "/versions/1"},
		{name: "named version", method: http.MethodPost, suffix: "/versions", body: map[string]string{"summary": "Named version"}},
		{name: "edit", method: http.MethodPost, suffix: "/edits", body: map[string]any{"ops": []any{}}},
	} {
		t.Run(route.name, func(t *testing.T) {
			response := dispatchRequest(t, mux, route.method, "/api/v1/artifacts/not-a-uuid"+route.suffix, route.body, "alice")
			if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"code":"ARTIFACT_NOT_FOUND"`) {
				t.Fatalf("invalid UUID %s: status=%d body=%s", route.name, response.Code, response.Body.String())
			}
		})
	}
}

// sentUpload is what a test upload's sender reads back: its answer, and whether the server read
// its body to the end, as drainBeforeAnswer reads one before an early answer.
type sentUpload struct {
	*httptest.ResponseRecorder
	readWhole bool
}

// sendBigUpload posts a 4 MiB file to the issue as alice, under key when key is not empty, in a
// multipart body of its own - a boundary of its own, as every attempt of a client's has.
func sendBigUpload(t *testing.T, handler http.Handler, issueKey, key string) sentUpload {
	t.Helper()
	return sendUpload(t, handler, issueKey, key, bytes.Repeat([]byte("a"), 4<<20))
}

// sendUpload posts file to the issue as big.bin, as sendBigUpload posts its 4 MiB.
func sendUpload(t *testing.T, handler http.Handler, issueKey, key string, file []byte) sentUpload {
	t.Helper()
	body, bodyType, err := apitest.MultipartUpload(map[string]string{"name": "big.bin"}, "big.bin",
		"application/octet-stream", file)
	if err != nil {
		t.Fatal(err)
	}
	size := int64(body.Len())
	sent := &countingBody{Reader: body}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/issues/"+issueKey+"/artifacts", sent)
	request.Header.Set("Content-Type", bodyType)
	request.Header.Set("X-Dispatch-User", "alice")
	if key != "" {
		request.Header.Set(contracts.IdempotencyKeyHeader, key)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return sentUpload{ResponseRecorder: response, readWhole: sent.read.Load() == size}
}

// unspooled runs send with the temporary directory gone, where a request that spooled a body past
// a megabyte - the keyed layer's spool, the multipart parser's - is answered 500
// (TestAnUploadTheServerCannotSpoolIsItsOwnFailure), and puts it back after.
func unspooled(t *testing.T, send func()) {
	t.Helper()
	previous, had := os.LookupEnv("TMPDIR")
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	send()
	if had {
		t.Setenv("TMPDIR", previous)
	} else {
		os.Unsetenv("TMPDIR")
	}
}

// closeArtifactIssue closes the issue as alice.
func closeArtifactIssue(t *testing.T, handler http.Handler, issueKey string) {
	t.Helper()
	if closing := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issueKey,
		map[string]string{"status": "done"}, "alice"); closing.Code != http.StatusOK {
		t.Fatalf("close the issue: status=%d body=%s", closing.Code, closing.Body.String())
	}
}

// issueArtifactCount counts the issue's artifacts.
func issueArtifactCount(t *testing.T, database *store.Store, issueKey string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from artifacts where issue_key = $1`, issueKey).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// INFRA-258. An upload to a closed issue is refused 409 ISSUE_CLOSED before any of
// its body is spooled, keyed or not: the keyed layer spools a body whole before its handler runs,
// and the multipart parser spools one whole before the handler reaches its issue, so a closed
// issue's 20 MB upload was written out in full only to be refused. The body is read off the
// connection and discarded before the refusal (drainBeforeAnswer). Nothing is stored and no key is
// claimed.
func TestAnUploadToAClosedIssueIsRefusedBeforeItsBodyIsSpooled(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{files: filestest.NewMemory()})
	issue := createArtifactIssue(t, handler)
	closeArtifactIssue(t, handler, issue.Key)
	before := issueArtifactCount(t, database, issue.Key)
	for name, key := range map[string]string{"an unkeyed upload": "", "a keyed upload": "closed-upload"} {
		unspooled(t, func() {
			response := sendBigUpload(t, handler, issue.Key, key)
			if code, _ := refusalOf(t, response.ResponseRecorder); response.Code != http.StatusConflict || code != "ISSUE_CLOSED" {
				t.Errorf("%s to a closed issue: status=%d body=%s, want 409 ISSUE_CLOSED, unspooled", name, response.Code, response.Body.String())
			}
			if !response.readWhole {
				t.Errorf("%s to a closed issue was refused before its body was read to the end", name)
			}
		})
	}
	if after, keys := issueArtifactCount(t, database, issue.Key), countRequestKeys(t, database); after != before || keys != 0 {
		t.Fatalf("the refused uploads left %d artifacts (was %d) and %d keys, want nothing written", after, before, keys)
	}
}

// INFRA-258. A repeat of an upload that landed is answered what that upload was,
// whatever its issue's state now. Its answer was lost - a dropped connection, a reload that resumed
// the write - and the issue closed before the repeat, as Legion's merge closes one: the upload is
// stored, so the closed-issue refusal must not answer it. The key holds the answer, and the keyed
// layer replays it; the file is stored once.
func TestARepeatOfAnUploadThatLandedReplaysItsAnswerAfterTheIssueCloses(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{files: filestest.NewMemory()})
	issue := createArtifactIssue(t, handler)
	before := issueArtifactCount(t, database, issue.Key)
	landed := sendBigUpload(t, handler, issue.Key, "landed-upload")
	if landed.Code != http.StatusCreated {
		t.Fatalf("the first upload: status=%d body=%s", landed.Code, landed.Body.String())
	}
	// The test drops nothing but its own reading of the answer: the client never saw it.
	closeArtifactIssue(t, handler, issue.Key)
	repeat := sendBigUpload(t, handler, issue.Key, "landed-upload")
	if repeat.Code != http.StatusCreated || !replayed(repeat.ResponseRecorder) || repeat.Body.String() != landed.Body.String() {
		t.Fatalf("the repeat once the issue closed: status=%d replayed=%v body=%s, want the first upload's 201 replayed",
			repeat.Code, replayed(repeat.ResponseRecorder), repeat.Body.String())
	}
	if count := issueArtifactCount(t, database, issue.Key); count != before+1 {
		t.Fatalf("the issue holds %d artifacts (%d before the upload), want the one upload added", count, before)
	}
}

// heldFileStore is a file store whose Put says it was reached and then waits for release. An upload
// held there has claimed its key, and has not yet begun the transaction that locks its issue, so the
// issue can close while the upload is in flight.
type heldFileStore struct {
	*filestest.Memory
	reached chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (s *heldFileStore) Put(ctx context.Context, sha, mime string, body []byte) error {
	s.once.Do(func() { close(s.reached) })
	<-s.release
	return s.Memory.Put(ctx, sha, mime, body)
}

// INFRA-258. A repeat of an upload whose first request is still running when its
// issue closes is told 409 KEY_IN_FLIGHT, unspooled, and not ISSUE_CLOSED: the first request
// decides, under the issue's lock, whether the upload lands, and once it has answered the repeat is
// given that answer. Here the first request reaches the issue after the close, so its answer, and
// the repeat's, is its own 409 ISSUE_CLOSED.
func TestARepeatOfAnUploadStillRunningWaitsForItsAnswerAfterTheIssueCloses(t *testing.T) {
	held := &heldFileStore{Memory: filestest.NewMemory(), reached: make(chan struct{})}
	handler, database, _ := newTestServer(t, testServerOptions{files: held})
	release, releaseOnce := heldUntilReleased(t)
	held.release = release
	issue := createArtifactIssue(t, handler)
	before := issueArtifactCount(t, database, issue.Key)
	answered := make(chan sentUpload, 1)
	go func() { answered <- sendBigUpload(t, handler, issue.Key, "running-upload") }()
	select {
	case <-held.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the first upload never reached the file store")
	}
	closeArtifactIssue(t, handler, issue.Key)

	// The first upload has parsed its spooled body already: it reaches nothing more of the
	// temporary directory.
	unspooled(t, func() {
		repeat := sendBigUpload(t, handler, issue.Key, "running-upload")
		if code, _ := refusalOf(t, repeat.ResponseRecorder); repeat.Code != http.StatusConflict || code != contracts.IdempotencyKeyInFlight ||
			repeat.Header().Get("Retry-After") != "1" || !repeat.readWhole {
			t.Fatalf("a repeat while the first upload runs on a closed issue: status=%d body=%s read whole=%v, want 409 %s with Retry-After: 1, unspooled, its body read to the end",
				repeat.Code, repeat.Body.String(), repeat.readWhole, contracts.IdempotencyKeyInFlight)
		}
	})

	releaseOnce()
	var first sentUpload
	select {
	case first = <-answered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first upload never answered once released")
	}
	if code, _ := refusalOf(t, first.ResponseRecorder); first.Code != http.StatusConflict || code != "ISSUE_CLOSED" {
		t.Fatalf("the first upload, which reached its issue after the close: status=%d body=%s, want 409 ISSUE_CLOSED",
			first.Code, first.Body.String())
	}
	after := sendBigUpload(t, handler, issue.Key, "running-upload")
	if after.Code != first.Code || !replayed(after.ResponseRecorder) || after.Body.String() != first.Body.String() {
		t.Fatalf("a repeat once the first upload answered: status=%d replayed=%v body=%s, want its answer %s replayed",
			after.Code, replayed(after.ResponseRecorder), after.Body.String(), first.Body.String())
	}
	if count := issueArtifactCount(t, database, issue.Key); count != before {
		t.Fatalf("the issue holds %d artifacts (%d before the upload), want none added", count, before)
	}
}

// INFRA-258. The closed-issue refusal runs before the body is spooled only where
// the upload's handler would run: a key no row holds
// (TestAnUploadToAClosedIssueIsRefusedBeforeItsBodyIsSpooled), one past its window, and one whose
// holder died with nothing committed, which the next attempt takes over. Every key the keyed layer
// answers itself is answered by it, whatever the issue's state: a first request that committed its
// upload and is still running is KEY_IN_FLIGHT, unspooled, and one that committed and died before
// its answer was kept is KEY_COMMITTED, once the layer has read the repeat's digest. Each case
// lands an upload, closes the issue, and rewrites the key's row into the state it names; the stored
// artifact is beside the point, since a repeat reads only the row.
func TestAClosedIssueRefusesAnUploadOnlyWhereItsHandlerWouldRun(t *testing.T) {
	unanswered := `status = null, headers = null, response = null`
	for _, state := range []struct {
		name      string
		set       string
		args      []any
		code      string
		unspooled bool
	}{
		{name: "in flight, its upload committed", set: unanswered + `, leased_at = now()`, code: contracts.IdempotencyKeyInFlight, unspooled: true},
		{name: "lapsed after committing its upload", set: unanswered + `, leased_at = null`, code: contracts.IdempotencyKeyCommitted},
		{name: "abandoned with nothing committed", set: unanswered + `, leased_at = null, committed_at = null`, code: "ISSUE_CLOSED", unspooled: true},
		{
			name: "past its window", set: `created_at = now() - make_interval(secs => $1) - interval '1 minute'`,
			args: []any{contracts.IdempotencyKeyTTL.Seconds()}, code: "ISSUE_CLOSED", unspooled: true,
		},
	} {
		t.Run(state.name, func(t *testing.T) {
			handler, database, _ := newTestServer(t, testServerOptions{files: filestest.NewMemory()})
			issue := createArtifactIssue(t, handler)
			if landed := sendBigUpload(t, handler, issue.Key, "an-upload"); landed.Code != http.StatusCreated {
				t.Fatalf("the first upload: status=%d body=%s", landed.Code, landed.Body.String())
			}
			closeArtifactIssue(t, handler, issue.Key)
			// The test's only key is the upload's.
			if tag, err := database.Pool.Exec(context.Background(), `update request_keys set `+state.set, state.args...); err != nil || tag.RowsAffected() != 1 {
				t.Fatalf("rewrite the key's row: rows=%d err=%v", tag.RowsAffected(), err)
			}
			send := func() {
				repeat := sendBigUpload(t, handler, issue.Key, "an-upload")
				if code, _ := refusalOf(t, repeat.ResponseRecorder); repeat.Code != http.StatusConflict || code != state.code || !repeat.readWhole {
					t.Fatalf("the repeat: status=%d body=%s read whole=%v, want 409 %s, its body read to the end",
						repeat.Code, repeat.Body.String(), repeat.readWhole, state.code)
				}
			}
			if state.unspooled {
				unspooled(t, send)
			} else {
				send()
			}
		})
	}
}
