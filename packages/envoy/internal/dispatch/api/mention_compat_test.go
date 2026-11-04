package api

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func TestIdentityMentionStorageKindsAreInvisibleToCompatibilityReaders(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	document := createProjectDocument(t, handler, "CORE", "Mention compatibility", "# Compatibility\n\nSee dispatch://"+issue.Key+".\n")
	comment := createReferenceAPIComment(t, handler, issue.Key, "Compatibility comment")
	for _, kind := range []string{"person_mention", "session_mention", "role_mention"} {
		insertUnknownKindEdges(t, database, [][4]string{
			{"artifact", document.ID, kind, "reader"},
			{"comment", comment.ID, kind, "reader"},
		})
	}
	for _, ref := range []string{"dispatch://CORE/artifact/" + document.Slug, "dispatch://" + issue.Key + "/comment/" + comment.ID} {
		response := graphRequest(t, handler, url.Values{"from": {ref}})
		if response.Code != http.StatusOK {
			t.Fatalf("references = %d %s", response.Code, response.Body.String())
		}
		for _, edge := range decodeBody[model.GraphReferences](t, response).Edges {
			if strings.HasSuffix(edge.Node.Kind, "_mention") || edge.Node.Kind == "session" {
				t.Fatalf("compatibility read leaked %+v", edge)
			}
		}
	}
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+document.ID+"/references", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("artifact references = %d %s", response.Code, response.Body.String())
	}
	refs := decodeBody[model.ArtifactReferences](t, response)
	if len(refs.Outgoing) != 1 || refs.Outgoing[0].Kind != "issue" || len(refs.ReferencedBy) != 0 {
		t.Fatalf("compatibility artifact refs = %+v", refs)
	}
	response = dispatchRequest(t, handler, http.MethodGet, "/api/v1/search?q=compatibility", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("search = %d %s", response.Code, response.Body.String())
	}
	result := decodeBody[model.SearchResponse](t, response)
	if len(result.Results) == 0 {
		t.Fatal("search control returned no source hits")
	}
	for _, hit := range result.Results {
		if hit.Href == "" || strings.Contains(hit.Href, "_mention") {
			t.Fatalf("search href = %+v", hit)
		}
	}
}
