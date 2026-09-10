package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func TestDocumentAsksAndCommentsAreOwnerScoped(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	artifact := createProjectDocument(t, handler, "CORE", "Design notes", "# Design notes\n")

	askResponse := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifact.ID+"/asks", map[string]any{
		"question": "Is this ready?", "anchor": map[string]string{"artifact": "design-notes", "quote": "Design"}, "actor": sessionActor(),
	})
	if askResponse.Code != http.StatusCreated {
		t.Fatalf("create document ask: status=%d body=%s", askResponse.Code, askResponse.Body.String())
	}
	ask := decodeBody[model.Ask](t, askResponse)
	if ask.IssueKey != nil || ask.ArtifactID == nil || *ask.ArtifactID != artifact.ID || ask.Anchor == nil {
		t.Fatalf("document ask = %#v", ask)
	}
	listedAsks := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifact.ID+"/asks", nil, "alice")
	if listedAsks.Code != http.StatusOK {
		t.Fatalf("list document asks: status=%d body=%s", listedAsks.Code, listedAsks.Body.String())
	}
	if values := decodeBody[[]model.Ask](t, listedAsks); len(values) != 1 || values[0].ID != ask.ID {
		t.Fatalf("document asks = %#v", values)
	}
	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]string{"text": "Yes"}, "alice")
	if answered.Code != http.StatusOK || !strings.Contains(answered.Body.String(), `"state":"answered"`) {
		t.Fatalf("answer document ask: status=%d body=%s", answered.Code, answered.Body.String())
	}

	commentResponse := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifact.ID+"/comments", map[string]any{
		"body":       "Use Plan.",
		"anchor":     map[string]string{"artifact": "design-notes", "quote": "notes"},
		"suggestion": map[string]string{"replace_with": "plan"},
		"actor":      sessionActor(),
	})
	if commentResponse.Code != http.StatusCreated {
		t.Fatalf("create document suggestion: status=%d body=%s", commentResponse.Code, commentResponse.Body.String())
	}
	comment := decodeBody[model.Comment](t, commentResponse)
	if comment.IssueKey != nil || comment.ArtifactID == nil || *comment.ArtifactID != artifact.ID || comment.Anchor == nil {
		t.Fatalf("document comment = %#v", comment)
	}
	listedComments := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifact.ID+"/comments", nil, "alice")
	if listedComments.Code != http.StatusOK {
		t.Fatalf("list document comments: status=%d body=%s", listedComments.Code, listedComments.Body.String())
	}
	if values := decodeBody[[]model.Comment](t, listedComments); len(values) != 1 || values[0].ID != comment.ID {
		t.Fatalf("document comments = %#v", values)
	}
	accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice")
	if accepted.Code != http.StatusOK || !strings.Contains(accepted.Body.String(), `"accepted":true`) {
		t.Fatalf("accept document suggestion: status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	text := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifact.ID+"/text", nil, "alice")
	if text.Code != http.StatusOK || !strings.Contains(text.Body.String(), "Design plan") {
		t.Fatalf("read accepted document suggestion: status=%d body=%s", text.Code, text.Body.String())
	}

	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifact.ID+"/events", nil, "alice")
	if events.Code != http.StatusOK {
		t.Fatalf("list document events: status=%d body=%s", events.Code, events.Body.String())
	}
	documentEvents := decodeBody[[]model.Event](t, events)
	if len(documentEvents) == 0 {
		t.Fatal("document event list is empty")
	}
	for index, event := range documentEvents {
		if event.Seq != index+1 || event.IssueKey != nil || event.ArtifactID == nil || *event.ArtifactID != artifact.ID || event.Project != "CORE" {
			t.Fatalf("document event %d = %#v", index, event)
		}
	}

	issue := createArtifactIssue(t, handler)
	issueComment := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{"body": "Issue comment", "actor": sessionActor()})
	if issueComment.Code != http.StatusCreated {
		t.Fatalf("create issue comment: status=%d body=%s", issueComment.Code, issueComment.Body.String())
	}
	otherComment := decodeBody[model.Comment](t, issueComment)
	invalidReply := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifact.ID+"/comments", map[string]string{
		"body": "Wrong owner", "reply_to": otherComment.ID,
	}, "alice")
	if invalidReply.Code != http.StatusBadRequest || !strings.Contains(invalidReply.Body.String(), `"code":"INVALID_COMMENT"`) {
		t.Fatalf("cross-owner comment reply: status=%d body=%s", invalidReply.Code, invalidReply.Body.String())
	}
}

func TestOwnerRoutesRejectLinkedArtifact(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)
	base := "/api/v1/artifacts/" + issue.PrimaryArtifactID
	for _, route := range []struct {
		method string
		suffix string
		body   any
	}{
		{method: http.MethodGet, suffix: "/asks"},
		{method: http.MethodPost, suffix: "/asks", body: map[string]string{"question": "Nope"}},
		{method: http.MethodGet, suffix: "/comments"},
		{method: http.MethodPost, suffix: "/comments", body: map[string]string{"body": "Nope"}},
		{method: http.MethodGet, suffix: "/events"},
	} {
		response := dispatchRequest(t, handler, route.method, base+route.suffix, route.body, "alice")
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"ARTIFACT_LINKED"`) || !strings.Contains(response.Body.String(), issue.Key) {
			t.Fatalf("linked artifact %s %s: status=%d body=%s", route.method, route.suffix, response.Code, response.Body.String())
		}
	}
}

func TestDocumentAnchorMustTargetTheDocument(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	first := createProjectDocument(t, handler, "CORE", "Design notes", "# Design notes\n")
	second := createProjectDocument(t, handler, "CORE", "Other notes", "# Other notes\n")

	response := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+first.ID+"/asks", map[string]any{
		"question": "Can I anchor elsewhere?", "anchor": map[string]string{"artifact": second.Slug, "quote": "Other"}, "actor": sessionActor(),
	})
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_ANCHOR"`) || !strings.Contains(response.Body.String(), "anchor must target this document") {
		t.Fatalf("cross-document anchor: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestArtifactOwnerWritesAuthenticateBeforeResolvingOwner(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)
	targets := []string{
		issue.PrimaryArtifactID,
		"00000000-0000-0000-0000-000000000001",
	}
	writes := []struct {
		suffix string
		body   map[string]string
	}{
		{suffix: "/asks", body: map[string]string{"question": "No identity"}},
		{suffix: "/comments", body: map[string]string{"body": "No identity"}},
	}
	for _, target := range targets {
		for _, write := range writes {
			response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+target+write.suffix, write.body, "")
			if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), `"code":"NO_IDENTITY"`) {
				t.Fatalf("unauthenticated artifact write %s%s: status=%d body=%s", target, write.suffix, response.Code, response.Body.String())
			}
		}
	}
}
