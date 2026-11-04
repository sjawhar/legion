package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestPersonNodesIncludesIdentityMentionsFromEverySurfaceWithoutStorageKinds(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	for _, surface := range []struct{ path, body string }{
		{"/api/v1/issues/" + issue.Key + "/comments", "Comment dispatch://people/ALICE"},
		{"/api/v1/issues/" + issue.Key + "/messages", "Message dispatch://people/alice"},
	} {
		response := dispatchRequest(t, handler, http.MethodPost, surface.path, map[string]string{"body": surface.body}, "bob")
		if response.Code != http.StatusCreated {
			t.Fatalf("create %s = %d %s", surface.path, response.Code, response.Body.String())
		}
	}
	createProjectDocument(t, handler, "CORE", "Mentioned document", "Document dispatch://people/alice")
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/people/alice/nodes", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("person nodes = %d %s", response.Code, response.Body.String())
	}
	page := decodeBody[personNodes](t, response)
	found := map[string]int{}
	for _, row := range page.Rows {
		for _, relation := range row.Relations {
			if relation == "mentions" {
				found[row.Kind]++
				if row.Ref == nil || row.Href == nil {
					t.Fatalf("mention has no link: %+v", row)
				}
			}
		}
		if row.Kind != "comment" && row.Kind != "message" && row.Kind != "block" && row.Kind != "ask" {
			t.Fatalf("raw kind in answer: %+v", row)
		}
	}
	if found["comment"] != 1 || found["message"] != 1 || found["block"] != 1 {
		t.Fatalf("mentioned sources = %v; body=%s", found, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "_mention") {
		t.Fatal("storage kind escaped")
	}
}
