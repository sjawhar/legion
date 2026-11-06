package api

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// R12: an ask or comment written on a document without an anchor belongs to the version current
// when it was written, and an approval request to the version it named when it opened. A later
// version of the document moves neither, though it moves the approval request's own
// approval.version: the graph reads the version recorded at the write, never the document's
// latest version or the request's current one.
func TestALaterVersionMovesNoUnanchoredItemsVersion(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "CORE")

	issue := createSpecIssue(t, handler, "CORE", "Context paragraph.\n")
	specRef := "dispatch://" + issue.Key + "/spec"
	approval := decodeBody[struct {
		Ask model.Ask `json:"ask"`
	}](t, created(t, "request approval of the spec at version 1", sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests", map[string]any{
		"actor": sessionActor(),
	}))).Ask

	design := createProjectDocument(t, handler, "CORE", "Design notes", "# Design\n")
	designRef := "dispatch://CORE/artifact/" + design.Slug
	comment := decodeBody[model.Comment](t, created(t, "comment on version 1 of the project document", dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+design.ID+"/comments", map[string]any{
		"body": "Looks right.",
	}, "alice")))
	ask := decodeBody[model.Ask](t, created(t, "ask on version 1 of the project document", dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+design.ID+"/asks", map[string]any{
		"question": "Ship the design?",
	}, "alice")))

	later := editSpec(t, handler, issue.Key, []map[string]any{{"op": "insert", "after": "end", "markdown": "Later paragraph."}})
	created(t, "write version 2 of the project document", dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/CORE/artifacts", map[string]string{
		"name": "Design notes", "content": "# Design\n\nRevised.\n",
	}, "alice"))
	var followed int
	if err := database.Pool.QueryRow(ctx, `select (approval->>'version')::int from asks where id = $1`, approval.ID).Scan(&followed); err != nil {
		t.Fatalf("read the approval request's version: %v", err)
	}
	if followed != later.Number || later.Number != 2 {
		t.Fatalf("the approval request names version %d after the spec's version %d, want both 2", followed, later.Number)
	}

	for _, test := range []struct {
		what, ref string
		parent    *model.NodeParent
	}{
		{"the approval request", "dispatch://" + issue.Key + "/ask/" + approval.ID, &model.NodeParent{Kind: "artifact", ID: issue.PrimaryArtifactID, Ref: specRef}},
		{"the project document's comment", designRef + "/comment/" + comment.ID, &model.NodeParent{Kind: "artifact", ID: design.ID, Ref: designRef}},
		{"the project document's ask", designRef + "/ask/" + ask.ID, &model.NodeParent{Kind: "artifact", ID: design.ID, Ref: designRef}},
	} {
		node := readNode(t, handler, test.ref)
		if !reflect.DeepEqual(node.Parent, test.parent) || node.ParentVersion == nil || *node.ParentVersion != 1 {
			t.Fatalf("%s after the document's version 2: %s, want part of %+v at version 1", test.what, factsOf(node), *test.parent)
		}
	}
}
