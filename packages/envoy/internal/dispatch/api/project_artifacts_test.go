package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func createProjectDocument(t *testing.T, handler http.Handler, project, name, content string) model.Artifact {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/"+project+"/artifacts", map[string]string{
		"name": name, "content": content,
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create project document: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, response).Artifact
}

func TestCreateProjectDocumentJSONAndMultipart(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}

	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/CORE/artifacts", map[string]string{
		"name": "Design notes", "content": "# Design notes\n",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create JSON project document: status=%d body=%s", created.Code, created.Body.String())
	}
	first := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
		Version  model.Version  `json:"version"`
	}](t, created)
	if first.Artifact.IssueKey != nil || first.Artifact.Project != "CORE" || first.Artifact.Primary || first.Artifact.Slug != "design-notes" || first.Artifact.RefKey != "CORE/design-notes" || first.Version.Number != 1 {
		t.Fatalf("project document = %#v version=%#v", first.Artifact, first.Version)
	}

	image := multipartRequest(t, handler, "/api/v1/projects/CORE/artifacts", map[string]string{"name": "diagram.png"}, "diagram.png", "image/png", []byte("PNG"), "alice")
	if image.Code != http.StatusCreated {
		t.Fatalf("create project image: status=%d body=%s", image.Code, image.Body.String())
	}
	imageUpload := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, image)
	if imageUpload.Artifact.Kind != "image" || imageUpload.Artifact.IssueKey != nil || imageUpload.Artifact.Project != "CORE" {
		t.Fatalf("project image = %#v", imageUpload.Artifact)
	}

	updated := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/CORE/artifacts", map[string]string{
		"name": "Design notes", "content": "# Revised notes\n",
	}, "alice")
	if updated.Code != http.StatusCreated {
		t.Fatalf("update JSON project document: status=%d body=%s", updated.Code, updated.Body.String())
	}
	second := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
		Version  model.Version  `json:"version"`
	}](t, updated)
	if second.Artifact.ID != first.Artifact.ID || second.Version.Number != 2 {
		t.Fatalf("project document update = %#v version=%#v, want artifact %q version 2", second.Artifact, second.Version, first.Artifact.ID)
	}
}

func TestListProjectArtifactsIncludesIssueArtifactsExceptPrimaries(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	issue := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "CORE", "title": "Issue",
	}, "alice"))
	if uploaded := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "notes.md", "content": "# Notes\n",
	}, "alice"); uploaded.Code != http.StatusCreated {
		t.Fatalf("upload issue artifact: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	projectDocument := createProjectDocument(t, handler, "CORE", "Design notes", "# Design notes\n")

	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/CORE/artifacts", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list project artifacts: status=%d body=%s", listed.Code, listed.Body.String())
	}
	artifacts := decodeBody[[]model.Artifact](t, listed)
	if len(artifacts) != 2 || artifacts[0].Slug != "notes-md" || artifacts[0].IssueKey == nil || *artifacts[0].IssueKey != issue.Key || artifacts[1].ID != projectDocument.ID || artifacts[1].IssueKey != nil {
		t.Fatalf("project artifacts = %#v", artifacts)
	}

	unlinked := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/CORE/artifacts?unlinked=true", nil, "alice")
	if unlinked.Code != http.StatusOK {
		t.Fatalf("list unlinked project artifacts: status=%d body=%s", unlinked.Code, unlinked.Body.String())
	}
	if values := decodeBody[[]model.Artifact](t, unlinked); len(values) != 1 || values[0].ID != projectDocument.ID {
		t.Fatalf("unlinked project artifacts = %#v", values)
	}

	missing := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/NOPE/artifacts", nil, "alice")
	if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), `"code":"PROJECT_NOT_FOUND"`) {
		t.Fatalf("list unknown project artifacts: status=%d body=%s", missing.Code, missing.Body.String())
	}
}

func TestProjectArtifactSlugRoutesMirrorIssueRoutes(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	artifact := createProjectDocument(t, handler, "CORE", "Design notes", "# Design notes\n")
	base := "/api/v1/projects/CORE/artifacts/" + artifact.Slug

	details := dispatchRequest(t, handler, http.MethodGet, base, nil, "alice")
	if details.Code != http.StatusOK || !strings.Contains(details.Body.String(), `"referenced_by":[]`) {
		t.Fatalf("read project document: status=%d body=%s", details.Code, details.Body.String())
	}
	text := dispatchRequest(t, handler, http.MethodGet, base+"/text", nil, "alice")
	if text.Code != http.StatusOK || !strings.Contains(text.Body.String(), `"markdown":"# Design notes\n"`) {
		t.Fatalf("read project document text: status=%d body=%s", text.Code, text.Body.String())
	}
	version := dispatchRequest(t, handler, http.MethodGet, base+"/versions/1", nil, "alice")
	if version.Code != http.StatusOK || !strings.Contains(version.Body.String(), `"number":1`) {
		t.Fatalf("read project document version: status=%d body=%s", version.Code, version.Body.String())
	}
	named := dispatchRequest(t, handler, http.MethodPost, base+"/versions", map[string]string{"summary": "Initial review"}, "alice")
	if named.Code != http.StatusCreated || !strings.Contains(named.Body.String(), `"named":true`) {
		t.Fatalf("name project document version: status=%d body=%s", named.Code, named.Body.String())
	}
	edited := dispatchRequest(t, handler, http.MethodPost, base+"/edits", map[string]any{
		"ops": []model.EditOp{{Op: "replace", Find: "notes", With: "plan"}},
	}, "alice")
	if edited.Code != http.StatusOK || !strings.Contains(edited.Body.String(), `"applied":1`) {
		t.Fatalf("edit project document: status=%d body=%s", edited.Code, edited.Body.String())
	}
	text = dispatchRequest(t, handler, http.MethodGet, base+"/text", nil, "alice")
	if text.Code != http.StatusOK || !strings.Contains(text.Body.String(), "Design plan") {
		t.Fatalf("read edited project document: status=%d body=%s", text.Code, text.Body.String())
	}
	missing := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/CORE/artifacts/missing", nil, "alice")
	if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), `"code":"ARTIFACT_NOT_FOUND"`) {
		t.Fatalf("read missing project artifact: status=%d body=%s", missing.Code, missing.Body.String())
	}
}

func TestProjectUploadRejectsUnknownProjectAndPrimary(t *testing.T) {
	handler := newTestHandler(t)
	unknown := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/NOPE/artifacts", map[string]string{
		"name": "Design notes", "content": "# Design notes\n",
	}, "alice")
	if unknown.Code != http.StatusNotFound || !strings.Contains(unknown.Body.String(), `"code":"PROJECT_NOT_FOUND"`) {
		t.Fatalf("upload to unknown project: status=%d body=%s", unknown.Code, unknown.Body.String())
	}
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	primary := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/CORE/artifacts", map[string]any{
		"name": "Design notes", "content": "# Design notes\n", "primary": true,
	}, "alice")
	if primary.Code != http.StatusBadRequest || !strings.Contains(primary.Body.String(), `"code":"ARTIFACT_INPUT"`) {
		t.Fatalf("upload primary project artifact: status=%d body=%s", primary.Code, primary.Body.String())
	}
}
