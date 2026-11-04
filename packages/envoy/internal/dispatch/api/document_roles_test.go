package api

import (
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A document an issue holds under one of the two reserved names plays that role from the moment
// it is created, the issue read names both documents by it, and nothing else on the issue takes a
// role. The role is the document's for life: a second upload of the same name is its next
// version, under the row that already holds the role.
func TestIssueDocumentsTakeTheirRoleFromTheirName(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "ROLE", "Roles", "A spec\n")
	owner := "/api/v1/issues/" + issue.Key

	verification := uploadDocument(t, handler, owner, "verification.md", "# Verification\n\nOne.\n")
	plan := uploadDocument(t, handler, owner, "plan.md", "# Plan\n\nOne.\n")
	notes := uploadDocument(t, handler, owner, "notes.md", "# Notes\n\nOne.\n")
	if verification.Artifact.Role != "verification" || plan.Artifact.Role != "plan" || notes.Artifact.Role != "" {
		t.Fatalf("roles = %q, %q, %q; want verification, plan and none",
			verification.Artifact.Role, plan.Artifact.Role, notes.Artifact.Role)
	}
	if again := uploadDocument(t, handler, owner, "plan.md", "# Plan\n\nTwo.\n"); again.Artifact.ID != plan.Artifact.ID || again.Artifact.Role != "plan" || again.Version.Number != 2 {
		t.Fatalf("second upload of plan.md = %#v version %d, want version 2 of the plan", again.Artifact, again.Version.Number)
	}

	// The spec keeps is_primary and takes no role.
	spec := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID, nil, "alice")
	if spec.Code != http.StatusOK {
		t.Fatalf("read the spec: status=%d body=%s", spec.Code, spec.Body.String())
	}
	if read := decodeBody[model.Artifact](t, spec); !read.Primary || read.Role != "" {
		t.Fatalf("spec = primary %v role %q, want the primary document with no role", read.Primary, read.Role)
	}

	read := dispatchRequest(t, handler, http.MethodGet, owner, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read the issue: status=%d body=%s", read.Code, read.Body.String())
	}
	detail := decodeBody[struct {
		PrimaryArtifactID      string `json:"primary_artifact_id"`
		VerificationArtifactID string `json:"verification_artifact_id"`
		PlanArtifactID         string `json:"plan_artifact_id"`
	}](t, read)
	if detail.PrimaryArtifactID != issue.PrimaryArtifactID ||
		detail.VerificationArtifactID != verification.Artifact.ID ||
		detail.PlanArtifactID != plan.Artifact.ID {
		t.Fatalf("issue read = %#v, want the spec, the Verification document and the plan", detail)
	}

	// Another issue holds its own pair: the role is unique within an issue, not across the project.
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "ROLE", "title": "A second issue", "spec": "Another spec\n",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create a second issue: status=%d body=%s", created.Code, created.Body.String())
	}
	other := decodeBody[struct {
		Key string `json:"key"`
	}](t, created)
	if second := uploadDocument(t, handler, "/api/v1/issues/"+other.Key, "plan.md", "# Plan\n\nOne.\n"); second.Artifact.Role != "plan" {
		t.Fatalf("the second issue's plan = %#v, want its own plan role", second.Artifact)
	}

	// A project document takes no role: a role belongs to an issue.
	if project := uploadDocument(t, handler, "/api/v1/projects/ROLE", "plan.md", "# Plan\n\nOne.\n"); project.Artifact.Role != "" {
		t.Fatalf("project document = %#v, want no role", project.Artifact)
	}
}
