package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
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

func TestUploadArtifactJSONCreatesPrimaryDocument(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)

	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"name": "architect-spec.md", "content": "# Spec\n", "primary": true, "summary": "Initial spec",
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create JSON artifact: status=%d body=%s", response.Code, response.Body.String())
	}
	upload := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
		Version  model.Version  `json:"version"`
	}](t, response)
	if upload.Artifact.Kind != "doc" || !upload.Artifact.Primary || upload.Version.Number != 1 {
		t.Fatalf("JSON artifact = %#v version=%#v, want primary document version 1", upload.Artifact, upload.Version)
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

func TestUploadArtifactJSONPreservesNonblankSurroundingWhitespace(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)
	content := " \n# Architect spec\n "
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "architect-spec.md", "content": content,
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
	if document.Markdown != content {
		t.Fatalf("stored Markdown = %q, want %q", document.Markdown, content)
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
		"name": " architect-spec.md ", "primary": "true",
	}, "architect-spec.md", "", []byte("# Architect spec\n"), "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("upload whitespace-named document: status=%d body=%s", response.Code, response.Body.String())
	}
	upload := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, response)
	if upload.Artifact.Name != "architect-spec.md" || upload.Artifact.Kind != "doc" || !upload.Artifact.Primary {
		t.Fatalf("whitespace-named document = %#v, want primary Markdown document", upload.Artifact)
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
