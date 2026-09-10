package api

import (
	"net/http"
	"strings"
	"testing"

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

func TestArtifactRoutesResolveUUIDsAndIssueScopedSlugs(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)
	otherIssueResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Other artifact issue",
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
		{name: "primary", method: http.MethodPost, suffix: "/primary"},
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
		{name: "primary", method: http.MethodPost, suffix: "/primary"},
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

func TestArtifactIDRoutesValidateBeforeDatabaseUse(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux, Deps{
		Store: &store.Store{},
		Identity: identity.HeaderIdentity{
			Header:        "X-Dispatch-User",
			AllowedLogins: map[string]struct{}{"alice": {}},
		},
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
		{name: "primary", method: http.MethodPost, suffix: "/primary"},
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
