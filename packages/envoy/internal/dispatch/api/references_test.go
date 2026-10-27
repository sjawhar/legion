package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func createReferenceAPIProject(t *testing.T, handler http.Handler, key string) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": key, "name": key + " project",
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create project %q: status=%d body=%s", key, response.Code, response.Body.String())
	}
}

func createReferenceAPIIssue(t *testing.T, handler http.Handler, project string) model.Issue {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": project, "title": "Reference issue",
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create reference issue: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[model.Issue](t, response)
}

func createReferenceAPIComment(t *testing.T, handler http.Handler, issueKey, body string) model.Comment {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/comments", map[string]string{
		"body": body,
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create reference comment: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[model.Comment](t, response)
}

func issueReferencesRequest(t *testing.T, handler http.Handler, issueKey, etag string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/issues/"+issueKey+"/references", nil)
	request.Header.Set("X-Dispatch-User", "alice")
	if etag != "" {
		request.Header.Set("If-None-Match", etag)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestIssueReferencesClosureDepthViaAndETag(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "CORE")
	createReferenceAPIProject(t, handler, "OPS")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	diagram := createProjectDocument(t, handler, "OPS", "Diagram PNG", "# Diagram\n")
	design := createProjectDocument(t, handler, "CORE", "Design notes", "# Design\ndispatch://OPS/artifact/diagram-png\n")
	comment := createReferenceAPIComment(t, handler, issue.Key, "dispatch://CORE/artifact/design-notes")

	var crossProjectReferences int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from refs
		where from_kind = 'artifact' and from_id = $1 and to_kind = 'artifact' and to_id = $2
	`, design.ID, diagram.RefKey).Scan(&crossProjectReferences); err != nil {
		t.Fatalf("read cross-project reference: %v", err)
	}
	if crossProjectReferences != 1 {
		t.Fatalf("cross-project reference rows = %d; want 1", crossProjectReferences)
	}

	first := issueReferencesRequest(t, handler, issue.Key, "")
	if first.Code != http.StatusOK {
		t.Fatalf("get issue references: status=%d body=%s", first.Code, first.Body.String())
	}
	firstValue := decodeBody[model.IssueReferences](t, first)
	if firstValue.Truncated || len(firstValue.Members) != 2 {
		t.Fatalf("issue references = %#v; want two untruncated members", firstValue)
	}
	firstMember, secondMember := firstValue.Members[0], firstValue.Members[1]
	if firstMember.Artifact.ID != design.ID || firstMember.Depth != 1 || firstMember.Via != (model.ReferenceVia{Kind: "comment", ID: comment.ID}) {
		t.Fatalf("first reference member = %#v; want Design notes through comment %q", firstMember, comment.ID)
	}
	if secondMember.Artifact.ID != diagram.ID || secondMember.Artifact.Project != "OPS" || secondMember.Depth != 2 || secondMember.Via != (model.ReferenceVia{Kind: "artifact", ID: design.ID}) {
		t.Fatalf("second reference member = %#v; want cross-project Diagram PNG through Design notes", secondMember)
	}
	firstETag := first.Header().Get("ETag")
	if firstETag == "" {
		t.Fatal("issue references response has no ETag")
	}

	notModified := issueReferencesRequest(t, handler, issue.Key, firstETag)
	if notModified.Code != http.StatusNotModified || notModified.Body.Len() != 0 || notModified.Header().Get("ETag") != firstETag {
		t.Fatalf("conditional issue references: status=%d etag=%q body=%q", notModified.Code, notModified.Header().Get("ETag"), notModified.Body.String())
	}

	updated := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/CORE/artifacts", map[string]string{
		"name": "Design notes", "content": "# Design\nNo references\n",
	}, "alice")
	if updated.Code != http.StatusCreated {
		t.Fatalf("update Design notes: status=%d body=%s", updated.Code, updated.Body.String())
	}
	second := issueReferencesRequest(t, handler, issue.Key, firstETag)
	if second.Code != http.StatusOK {
		t.Fatalf("read changed issue references: status=%d body=%s", second.Code, second.Body.String())
	}
	secondValue := decodeBody[model.IssueReferences](t, second)
	if secondValue.Truncated || len(secondValue.Members) != 1 || secondValue.Members[0].Artifact.ID != design.ID {
		t.Fatalf("references after removing link = %#v; want only Design notes", secondValue)
	}
	if second.Header().Get("ETag") == firstETag {
		t.Fatal("reference ETag did not change after linked document version changed")
	}
}

func TestIssueReferencesExcludeOwnArtifactsAndDanglingTargets(t *testing.T) {
	handler := newTestHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	createReferenceAPIComment(t, handler, issue.Key, "dispatch://"+issue.Key+"/spec dispatch://CORE/artifact/ghost")

	response := issueReferencesRequest(t, handler, issue.Key, "")
	if response.Code != http.StatusOK {
		t.Fatalf("get issue references: status=%d body=%s", response.Code, response.Body.String())
	}
	value := decodeBody[model.IssueReferences](t, response)
	if value.Truncated || len(value.Members) != 0 {
		t.Fatalf("issue references = %#v; want no own or dangling artifacts", value)
	}
}

func TestIssueReferencesTruncateBeyondDepthEight(t *testing.T) {
	handler := newTestHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	for index := 1; index <= 10; index++ {
		name := "Document " + strconv.Itoa(index)
		content := "# " + name + "\n"
		if index < 10 {
			content += "dispatch://CORE/artifact/document-" + strconv.Itoa(index+1) + "\n"
		}
		createProjectDocument(t, handler, "CORE", name, content)
	}
	createReferenceAPIComment(t, handler, issue.Key, "dispatch://CORE/artifact/document-1")

	response := issueReferencesRequest(t, handler, issue.Key, "")
	if response.Code != http.StatusOK {
		t.Fatalf("get truncated issue references: status=%d body=%s", response.Code, response.Body.String())
	}
	value := decodeBody[model.IssueReferences](t, response)
	if !value.Truncated || len(value.Members) != 8 {
		t.Fatalf("truncated issue references = %#v; want eight members with truncation", value)
	}
	for index, member := range value.Members {
		if member.Depth != index+1 {
			t.Fatalf("member %d depth = %d; want %d", index, member.Depth, index+1)
		}
	}
}

func TestArtifactReferencesListOutgoingAndReferencedBy(t *testing.T) {
	handler := newTestHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	createReferenceAPIProject(t, handler, "OPS")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	diagram := createProjectDocument(t, handler, "OPS", "Diagram PNG", "# Diagram\n")
	design := createProjectDocument(t, handler, "CORE", "Design notes", "# Design\ndispatch://OPS/artifact/diagram-png\n")
	comment := createReferenceAPIComment(t, handler, issue.Key, "dispatch://CORE/artifact/design-notes")

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+design.ID+"/references", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("get artifact references: status=%d body=%s", response.Code, response.Body.String())
	}
	value := decodeBody[model.ArtifactReferences](t, response)
	if len(value.Outgoing) != 1 || value.Outgoing[0].Kind != "artifact" || value.Outgoing[0].ToID != diagram.RefKey || value.Outgoing[0].Artifact == nil || value.Outgoing[0].Artifact.ID != diagram.ID {
		t.Fatalf("outgoing references = %#v; want resolved Diagram PNG", value.Outgoing)
	}
	if len(value.ReferencedBy) != 1 || value.ReferencedBy[0].Kind != "comment" || value.ReferencedBy[0].ID != comment.ID || value.ReferencedBy[0].IssueKey == nil || *value.ReferencedBy[0].IssueKey != issue.Key {
		t.Fatalf("referenced by = %#v; want issue comment %q", value.ReferencedBy, comment.ID)
	}
}

func TestIssueReferencesRejectUnauthenticatedRequest(t *testing.T) {
	handler := newTestHandler(t)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/issues/CORE-1/references", nil))
	if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), `"code":"NO_IDENTITY"`) {
		t.Fatalf("unauthenticated issue references: status=%d body=%s", response.Code, response.Body.String())
	}
}

func graphRequest(t *testing.T, handler http.Handler, query url.Values) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodGet, "/api/v1/references?"+query.Encode(), nil, "alice")
}

func graphEdges(t *testing.T, handler http.Handler, query url.Values) model.GraphReferences {
	t.Helper()
	response := graphRequest(t, handler, query)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/references?%s: status=%d body=%s", query.Encode(), response.Code, response.Body.String())
	}
	return decodeBody[model.GraphReferences](t, response)
}

// A message on an ACME issue that cites a LEGION ask is a backlink on that ask: the edge names
// the message with its own issue, project, and dispatch:// address, carries the message body as
// excerpt, and is stamped with the events.id of the message.created write. Read from the
// message, the same edge points out at the ask. Filters narrow it and malformed input is 400.
func TestReferencesReadBacklinksAcrossProjectsWithProvenance(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "LEGION")
	createReferenceAPIProject(t, handler, "ACME")
	legion := createReferenceAPIIssue(t, handler, "LEGION")
	acme := createReferenceAPIIssue(t, handler, "ACME")
	askResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+legion.Key+"/asks", map[string]any{
		"question": "Ship the reference graph?", "options": []map[string]string{{"label": "Yes"}, {"label": "No"}},
	}, "alice")
	if askResponse.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", askResponse.Code, askResponse.Body.String())
	}
	ask := decodeBody[model.Ask](t, askResponse)
	askRef := "dispatch://" + legion.Key + "/ask/" + ask.ID
	messageResponse := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+acme.Key+"/messages", map[string]any{
		"body": "Decided in " + askRef + ", see also https://example.com/notes", "actor": sessionActor(),
	})
	if messageResponse.Code != http.StatusCreated {
		t.Fatalf("create message: status=%d body=%s", messageResponse.Code, messageResponse.Body.String())
	}
	message := decodeBody[model.Message](t, messageResponse)
	var messageEventID int64
	if err := database.Pool.QueryRow(context.Background(), `
		select id from events where issue_key = $1 and type = 'message.created'
	`, acme.Key).Scan(&messageEventID); err != nil {
		t.Fatalf("read message.created event: %v", err)
	}

	incoming := graphEdges(t, handler, url.Values{"to": {askRef}})
	legionKey := legion.Key
	if want := (model.GraphNode{Kind: "ask", ID: ask.ID, IssueKey: &legionKey, Project: "LEGION", Ref: askRef}); !reflect.DeepEqual(incoming.Node, want) {
		t.Fatalf("queried node = %#v; want %#v", incoming.Node, want)
	}
	if len(incoming.Edges) != 1 {
		t.Fatalf("incoming edges = %#v; want the ACME message only", incoming.Edges)
	}
	edge := incoming.Edges[0]
	acmeKey := acme.Key
	wantNode := model.GraphNode{Kind: "message", ID: message.ID, IssueKey: &acmeKey, Project: "ACME", Ref: "dispatch://" + acme.Key + "/message/" + message.ID}
	if edge.Kind != "mentions" || edge.Direction != "in" || !reflect.DeepEqual(edge.Node, wantNode) {
		t.Fatalf("backlink edge = %#v; want mentions in from %#v", edge, wantNode)
	}
	if edge.Excerpt == nil || edge.Excerpt.BlockID != "" || edge.Excerpt.Text != message.Body {
		t.Fatalf("backlink excerpt = %#v; want the message body", edge.Excerpt)
	}
	if edge.SourceSeq == nil || *edge.SourceSeq != messageEventID || !edge.CreatedAt.Equal(message.CreatedAt) {
		t.Fatalf("backlink provenance = seq %v at %v; want event %d at %v", edge.SourceSeq, edge.CreatedAt, messageEventID, message.CreatedAt)
	}

	outgoing := graphEdges(t, handler, url.Values{"from": {"dispatch://" + acme.Key + "/message/" + message.ID}})
	if len(outgoing.Edges) != 1 || outgoing.Edges[0].Direction != "out" || outgoing.Edges[0].Kind != "mentions" || outgoing.Edges[0].Node.Ref != askRef || outgoing.Edges[0].Node.Kind != "ask" {
		t.Fatalf("outgoing edges = %#v; want one mention of the ask", outgoing.Edges)
	}
	if outgoing.Edges[0].Excerpt == nil || outgoing.Edges[0].Excerpt.Text != ask.Question {
		t.Fatalf("outgoing excerpt = %#v; want the ask question", outgoing.Edges[0].Excerpt)
	}

	if filtered := graphEdges(t, handler, url.Values{"to": {askRef}, "kind": {"replies_to,followed_by"}}); len(filtered.Edges) != 0 {
		t.Fatalf("kind filter kept %#v; want no edges", filtered.Edges)
	}
	// A newer build may name an edge kind this build does not emit. Its edges are none here, and
	// the answer names the kinds it did not know, rather than refusing the read.
	type knownEdges struct {
		Edges        []model.GraphEdge `json:"edges"`
		UnknownKinds []string          `json:"unknown_kinds"`
	}
	for kinds, wantEdges := range map[string]int{"future": 0, "mentions,future": 1} {
		query := url.Values{"to": {askRef}, "kind": {kinds}}
		response := graphRequest(t, handler, query)
		if response.Code != http.StatusOK {
			t.Fatalf("GET /api/v1/references?%s: status=%d body=%s; want 200", query.Encode(), response.Code, response.Body.String())
		}
		if got := decodeBody[knownEdges](t, response); len(got.Edges) != wantEdges || !reflect.DeepEqual(got.UnknownKinds, []string{"future"}) {
			t.Fatalf("kind=%s answered %#v; want %d edges and unknown_kinds [future]", kinds, got, wantEdges)
		}
	}
	if response := graphRequest(t, handler, url.Values{"to": {askRef}}); strings.Contains(response.Body.String(), "unknown_kinds") {
		t.Fatalf("a read naming no kind answered %s; want no unknown_kinds", response.Body.String())
	}
	if since := graphEdges(t, handler, url.Values{"to": {askRef}, "since": {strconv.FormatInt(messageEventID, 10)}}); len(since.Edges) != 0 {
		t.Fatalf("since the introducing event kept %#v; want no edges", since.Edges)
	}
	if since := graphEdges(t, handler, url.Values{"to": {askRef}, "since": {strconv.FormatInt(messageEventID-1, 10)}}); len(since.Edges) != 1 {
		t.Fatalf("since the prior event = %#v; want the backlink", since.Edges)
	}

	for name, tc := range map[string]struct {
		query  url.Values
		status int
		code   string
	}{
		"neither to nor from": {url.Values{}, http.StatusBadRequest, "INVALID_REFERENCE"},
		"both to and from":    {url.Values{"to": {askRef}, "from": {askRef}}, http.StatusBadRequest, "INVALID_REFERENCE"},
		"not a reference":     {url.Values{"to": {"LEGION-1 spec"}}, http.StatusBadRequest, "INVALID_REFERENCE"},
		"since not an id":     {url.Values{"to": {askRef}, "since": {"yesterday"}}, http.StatusBadRequest, "INVALID_SINCE"},
		"missing node":        {url.Values{"to": {"dispatch://" + legion.Key + "/ask/00000000-0000-0000-0000-000000000000"}}, http.StatusNotFound, "NOT_FOUND"},
		"missing document":    {url.Values{"from": {"dispatch://LEGION/artifact/ghost"}}, http.StatusNotFound, "NOT_FOUND"},
	} {
		response := graphRequest(t, handler, tc.query)
		if response.Code != tc.status || !strings.Contains(response.Body.String(), `"code":"`+tc.code+`"`) {
			t.Errorf("%s: status=%d body=%s; want %d %s", name, response.Code, response.Body.String(), tc.status, tc.code)
		}
	}
}

// An issue's backlinks are its mentions and its structure in one list: the child issue
// (child_of), its own documents (attached_to), and a document elsewhere that cites it, whose
// excerpt is the block containing the mention. Read from the citing document the same mention
// points out at the issue; read from the issue's spec the structural edge points out at it.
func TestReferencesIncludeStructuralEdgesAndDocumentBlockExcerpts(t *testing.T) {
	handler := newTestHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	createReferenceAPIProject(t, handler, "OPS")
	parent := createReferenceAPIIssue(t, handler, "CORE")
	childResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "CORE", "title": "Child work", "parent": parent.Key,
	}, "alice")
	if childResponse.Code != http.StatusCreated {
		t.Fatalf("create child issue: status=%d body=%s", childResponse.Code, childResponse.Body.String())
	}
	child := decodeBody[model.Issue](t, childResponse)
	design := createProjectDocument(t, handler, "OPS", "Design notes", "# Design\n\nIntroduction paragraph.\n\nThe plan lives in dispatch://"+parent.Key+" and nowhere else.\n")

	incoming := graphEdges(t, handler, url.Values{"to": {"dispatch://" + parent.Key}})
	byKind := make(map[string]model.GraphEdge, len(incoming.Edges))
	for _, edge := range incoming.Edges {
		byKind[edge.Kind] = edge
	}
	if len(incoming.Edges) != 3 || len(byKind) != 3 {
		t.Fatalf("issue edges = %#v; want one mention, one child_of, one attached_to", incoming.Edges)
	}
	for index := 1; index < len(incoming.Edges); index++ {
		if incoming.Edges[index].CreatedAt.After(incoming.Edges[index-1].CreatedAt) {
			t.Fatalf("edges are not newest first: %#v", incoming.Edges)
		}
	}
	mention := byKind["mentions"]
	if mention.Node.Kind != "artifact" || mention.Node.ID != design.ID || mention.Node.Project != "OPS" || mention.Node.Ref != "dispatch://OPS/artifact/design-notes" {
		t.Fatalf("mention node = %#v; want the OPS design document", mention.Node)
	}
	if mention.Excerpt == nil || mention.Excerpt.BlockID == "" || mention.Excerpt.Text != "The plan lives in dispatch://"+parent.Key+" and nowhere else." {
		t.Fatalf("mention excerpt = %#v; want the containing paragraph block with its id", mention.Excerpt)
	}
	if mention.SourceSeq == nil {
		t.Fatalf("document mention has no source_seq: %#v", mention)
	}
	childOf := byKind["child_of"]
	if childOf.Node.Kind != "issue" || childOf.Node.ID != child.Key || childOf.Node.Ref != "dispatch://"+child.Key || childOf.Excerpt == nil || childOf.Excerpt.Text != "Child work" || childOf.SourceSeq != nil {
		t.Fatalf("child_of edge = %#v; want the child issue with its title and no source_seq", childOf)
	}
	attached := byKind["attached_to"]
	if attached.Node.Kind != "artifact" || attached.Node.ID != parent.PrimaryArtifactID || attached.Node.Ref != "dispatch://"+parent.Key+"/spec" || attached.Excerpt == nil || attached.Excerpt.Text != "spec.md" {
		t.Fatalf("attached_to edge = %#v; want the issue's spec", attached)
	}

	outgoing := graphEdges(t, handler, url.Values{"from": {"dispatch://OPS/artifact/design-notes"}})
	if outgoing.Node.Kind != "artifact" || outgoing.Node.ID != design.ID {
		t.Fatalf("document node = %#v; want %s resolved from its ref_key", outgoing.Node, design.ID)
	}
	if len(outgoing.Edges) != 1 || outgoing.Edges[0].Kind != "mentions" || outgoing.Edges[0].Node.Ref != "dispatch://"+parent.Key || outgoing.Edges[0].Excerpt == nil || outgoing.Edges[0].Excerpt.Text != "Reference issue" {
		t.Fatalf("document links = %#v; want one mention of the parent issue with its title", outgoing.Edges)
	}

	spec := graphEdges(t, handler, url.Values{"from": {"dispatch://" + parent.Key + "/spec"}})
	if len(spec.Edges) != 1 || spec.Edges[0].Kind != "attached_to" || spec.Edges[0].Direction != "out" || spec.Edges[0].Node.Ref != "dispatch://"+parent.Key {
		t.Fatalf("spec links = %#v; want attached_to the issue", spec.Edges)
	}
	specBacklinks := graphEdges(t, handler, url.Values{"to": {"dispatch://" + parent.Key + "/spec"}})
	if specBacklinks.Node.ID != parent.PrimaryArtifactID || len(specBacklinks.Edges) != 0 {
		t.Fatalf("spec backlinks = %#v; want the spec node with no edges", specBacklinks)
	}
}

// insertUnknownKindEdges plants refs rows whose other end is a node kind this build does not
// know, as a newer build sharing the database would write them.
func insertUnknownKindEdges(t *testing.T, database *store.Store, edges [][4]string) {
	t.Helper()
	for _, edge := range edges {
		if _, err := database.Pool.Exec(context.Background(), `
			insert into refs (from_kind, from_id, to_kind, to_id) values ($1, $2, $3, $4)
		`, edge[0], edge[1], edge[2], edge[3]); err != nil {
			t.Fatalf("insert reference %v: %v", edge, err)
		}
	}
}

// An edge whose other end is a node kind this build cannot address is one a newer build wrote:
// the graph read skips it in both directions instead of answering 500, and the issue header's
// count is the number of edges its panel lists, not one more.
func TestReferencesAnswerAroundAnEdgeOfAKindThisBuildCannotAddress(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "CORE")
	createReferenceAPIProject(t, handler, "OPS")
	target := createReferenceAPIIssue(t, handler, "CORE")
	source := createReferenceAPIIssue(t, handler, "OPS")
	comment := createReferenceAPIComment(t, handler, source.Key, "see dispatch://"+target.Key)
	commentRef := "dispatch://" + source.Key + "/comment/" + comment.ID
	insertUnknownKindEdges(t, database, [][4]string{
		{"future", "x#y", "issue", target.Key},
		{"comment", comment.ID, "future", "x#y"},
	})

	// The target's own spec is attached to it and the comment cites it: two edges this build
	// can address, and the header counts the same edges the panel lists.
	incoming := graphRequest(t, handler, url.Values{"to": {"dispatch://" + target.Key}})
	if incoming.Code != http.StatusOK {
		t.Errorf("GET references to %s: status=%d body=%s", target.Key, incoming.Code, incoming.Body.String())
	} else {
		edges := decodeBody[model.GraphReferences](t, incoming).Edges
		byKind := make(map[string]model.GraphEdge, len(edges))
		for _, edge := range edges {
			byKind[edge.Kind] = edge
		}
		mention, attached := byKind["mentions"], byKind["attached_to"]
		if len(edges) != 2 || mention.Node.Kind != "comment" || mention.Node.ID != comment.ID || attached.Node.Kind != "artifact" || attached.Node.ID != target.PrimaryArtifactID {
			t.Errorf("backlinks of %s = %#v; want the comment's mention and the attached spec, nothing of kind future", target.Key, edges)
		}
	}

	outgoing := graphRequest(t, handler, url.Values{"from": {commentRef}})
	if outgoing.Code != http.StatusOK {
		t.Errorf("GET references from %s: status=%d body=%s", commentRef, outgoing.Code, outgoing.Body.String())
	} else {
		edges := decodeBody[model.GraphReferences](t, outgoing).Edges
		if len(edges) != 1 || edges[0].Kind != "mentions" || edges[0].Node.Kind != "issue" || edges[0].Node.ID != target.Key {
			t.Errorf("links of %s = %#v; want only the mention of %s, nothing of kind future", commentRef, edges, target.Key)
		}
	}

	detail := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+target.Key, nil, "alice")
	if detail.Code != http.StatusOK {
		t.Fatalf("read issue: status=%d body=%s", detail.Code, detail.Body.String())
	}
	if count := decodeBody[struct {
		ReferencedByCount int `json:"referenced_by_count"`
	}](t, detail).ReferencedByCount; count != 2 {
		t.Errorf("issue %q referenced_by_count = %d; want 2, the edges its panel lists", target.Key, count)
	}
}

// A document's references read skips an edge of a node kind this build cannot address on both
// sides: its referenced-by list is the comment that cites it, and its outgoing list is what its
// text cites, the known issue and the missing document both unresolved as before.
func TestArtifactReferencesSkipANodeKindThisBuildCannotAddress(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	design := createProjectDocument(t, handler, "CORE", "Design notes", "# Design\n\nThe plan lives in dispatch://"+issue.Key+" and dispatch://CORE/artifact/missing.\n")
	comment := createReferenceAPIComment(t, handler, issue.Key, "dispatch://CORE/artifact/design-notes")
	insertUnknownKindEdges(t, database, [][4]string{
		{"artifact", design.ID, "future", "x#y"},
		{"future", "x#y", "artifact", design.RefKey},
	})

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+design.ID+"/references", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("get artifact references: status=%d body=%s", response.Code, response.Body.String())
	}
	value := decodeBody[model.ArtifactReferences](t, response)
	if want := []model.OutgoingReference{
		{Kind: "artifact", ToID: "CORE/missing"},
		{Kind: "issue", ToID: issue.Key},
	}; !reflect.DeepEqual(value.Outgoing, want) {
		t.Errorf("outgoing references = %#v; want the unresolved issue and missing document, nothing of kind future", value.Outgoing)
	}
	if len(value.ReferencedBy) != 1 || value.ReferencedBy[0].Kind != "comment" || value.ReferencedBy[0].ID != comment.ID {
		t.Errorf("referenced by = %#v; want only comment %q", value.ReferencedBy, comment.ID)
	}
}

// A spec seeded at issue creation that cites an issue in bold, another project's issue in a code
// span and a third project's issue as an autolink, which the spec stores as a link whose text is
// its target, mentions all three. The source of each mention is the spec document, not the issue
// that owns it: the issue node writes no mention, and each target's backlink names the spec.
func TestSeededSpecCitationsInMarkdownDelimitersAreDocumentMentions(t *testing.T) {
	handler := newTestHandler(t)
	createReferenceAPIProject(t, handler, "LEGION")
	createReferenceAPIProject(t, handler, "ACME")
	createReferenceAPIProject(t, handler, "CORE")
	sameProject := createReferenceAPIIssue(t, handler, "LEGION")
	otherProject := createReferenceAPIIssue(t, handler, "ACME")
	autolinked := createReferenceAPIIssue(t, handler, "CORE")
	response := createIssueRequest(t, handler, map[string]any{
		"project": "LEGION", "title": "Due dates",
		"spec": "Tied to **dispatch://" + sameProject.Key + "** and `dispatch://" + otherProject.Key + "`, " +
			"see <dispatch://" + autolinked.Key + ">.\n",
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue with spec: status=%d body=%s", response.Code, response.Body.String())
	}
	citing := decodeBody[model.Issue](t, response)
	specRef := "dispatch://" + citing.Key + "/spec"
	targets := []model.Issue{sameProject, otherProject, autolinked}

	mentioned := map[string]bool{}
	for _, edge := range graphEdges(t, handler, url.Values{"from": {specRef}, "kind": {"mentions"}}).Edges {
		mentioned[edge.Node.Ref] = true
	}
	want := map[string]bool{}
	for _, target := range targets {
		want["dispatch://"+target.Key] = true
	}
	if !reflect.DeepEqual(mentioned, want) {
		t.Fatalf("spec mentions = %v; want %v", mentioned, want)
	}
	if fromIssue := graphEdges(t, handler, url.Values{"from": {"dispatch://" + citing.Key}, "kind": {"mentions"}}); len(fromIssue.Edges) != 0 {
		t.Fatalf("issue node mentions = %#v; want none, the spec document writes them", fromIssue.Edges)
	}
	for _, target := range targets {
		backlinks := graphEdges(t, handler, url.Values{"to": {"dispatch://" + target.Key}, "kind": {"mentions"}}).Edges
		if len(backlinks) != 1 || backlinks[0].Node.Kind != "artifact" || backlinks[0].Node.ID != citing.PrimaryArtifactID || backlinks[0].Node.Ref != specRef {
			t.Fatalf("%s backlinks = %#v; want one mention from %s", target.Key, backlinks, specRef)
		}
	}
}

// An item id that decodes to a NUL names nothing. The index binds ids as `text[]`, and Postgres
// refuses a NUL there, so a reader that decoded one failed the write holding it: the comment, the
// seeded spec and the author's edit answered 500, and a rebuild stopped at the first stored body
// that held one.
func TestCitationsOfAnIDDecodingToNULDoNotFailTheWrite(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "CORE")
	target := createReferenceAPIIssue(t, handler, "CORE")
	body := "See dispatch://" + target.Key + "/comment/%00, https://dispatch.example/issues/" + target.Key +
		"/comments/%00 and https://dispatch.example/issues/" + target.Key + "/spec?comment=%2500.\n"

	comment := createReferenceAPIComment(t, handler, target.Key, body)
	commentRef := "dispatch://" + target.Key + "/comment/" + comment.ID
	if edges := graphEdges(t, handler, url.Values{"from": {commentRef}, "kind": {"mentions"}}).Edges; len(edges) != 0 {
		t.Fatalf("comment mentions = %#v; want none", edges)
	}
	if seeded := createIssueRequest(t, handler, map[string]any{"project": "CORE", "title": "Seeded", "spec": body}); seeded.Code != http.StatusCreated {
		t.Fatalf("create issue whose spec cites a NUL id: status=%d body=%s", seeded.Code, seeded.Body.String())
	}
	plain := createReferenceAPIComment(t, handler, target.Key, "Plain comment.")
	if edit := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+plain.ID, map[string]string{
		"body": body,
	}, "alice"); edit.Code != http.StatusOK {
		t.Fatalf("edit a comment to cite a NUL id: status=%d body=%s", edit.Code, edit.Body.String())
	}
	if _, err := refs.RebuildAll(context.Background(), database.Pool, "https://dispatch.example"); err != nil {
		t.Fatalf("rebuild refs over bodies citing a NUL id: %v", err)
	}
}

type refRow struct {
	FromKind, FromID, ToKind, ToID, Kind string
	SourceSeq                            *int64
	CreatedAt                            string
}

func readRefRows(t *testing.T, database *store.Store) []refRow {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select from_kind, from_id, to_kind, to_id, kind, source_seq, created_at::text
		from refs order by from_kind, from_id, to_kind, to_id
	`)
	if err != nil {
		t.Fatalf("read refs: %v", err)
	}
	defer rows.Close()
	var result []refRow
	for rows.Next() {
		var row refRow
		if err := rows.Scan(&row.FromKind, &row.FromID, &row.ToKind, &row.ToID, &row.Kind, &row.SourceSeq, &row.CreatedAt); err != nil {
			t.Fatalf("scan ref: %v", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate refs: %v", err)
	}
	return result
}

// The text is the truth and refs is its index: after the index is doctored (an edge deleted, a
// stale edge planted), a rebuild converges on exactly the edge set the live writes produced,
// keeping the provenance of every edge it did not have to recreate, and a second rebuild is a
// no-op.
func TestRebuildRefsConvergesOnTheLiveIndex(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "CORE")
	createReferenceAPIProject(t, handler, "OPS")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	createProjectDocument(t, handler, "OPS", "Diagram", "# Diagram\n")
	design := createProjectDocument(t, handler, "CORE", "Design notes", "# Design\n\nSee dispatch://OPS/artifact/diagram and https://dispatch.example/issues/"+issue.Key+".\n")
	comment := createReferenceAPIComment(t, handler, issue.Key, "dispatch://CORE/artifact/design-notes and dispatch://"+issue.Key+"/spec")
	askResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Merge dispatch://CORE/artifact/design-notes?", "options": []map[string]string{{"label": "Yes"}},
	}, "alice")
	if askResponse.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", askResponse.Code, askResponse.Body.String())
	}
	if response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "Read dispatch://" + issue.Key + "/comment/" + comment.ID, "actor": sessionActor(),
	}); response.Code != http.StatusCreated {
		t.Fatalf("create message: status=%d body=%s", response.Code, response.Body.String())
	}
	live := readRefRows(t, database)
	if len(live) != 6 {
		t.Fatalf("live index = %#v; want six edges", live)
	}
	for _, row := range live {
		if row.SourceSeq == nil {
			t.Fatalf("live edge %#v has no source_seq", row)
		}
	}

	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`delete from refs where from_kind = 'comment' and to_kind = 'artifact' and to_id = $1`, []any{issue.Key + "/spec"}},
		{`insert into refs (from_kind, from_id, to_kind, to_id, source_seq) values ('artifact', $1, 'issue', 'ZZZ-9', 1)`, []any{design.ID}},
		{`insert into refs (from_kind, from_id, to_kind, to_id) values ('message', '00000000-0000-0000-0000-000000000000', 'issue', $1)`, []any{issue.Key}},
	} {
		if _, err := database.Pool.Exec(context.Background(), statement.sql, statement.args...); err != nil {
			t.Fatalf("doctor refs: %v", err)
		}
	}
	if doctored := readRefRows(t, database); len(doctored) != 7 {
		t.Fatalf("doctored index = %#v; want seven rows", doctored)
	}

	report, err := refs.RebuildAll(context.Background(), database.Pool, "https://dispatch.example")
	if err != nil {
		t.Fatalf("rebuild refs: %v", err)
	}
	// Every document has a version (three, counting the two specs); the planted row from a message
	// that does not exist is the one orphan.
	if report != (refs.Rebuild{Documents: 3, Asks: 1, Comments: 1, Messages: 1, Orphans: 1, Edges: 6}) {
		t.Fatalf("rebuild report = %#v", report)
	}
	rebuilt := readRefRows(t, database)
	if len(rebuilt) != len(live) {
		t.Fatalf("rebuilt index = %#v; want the live index %#v", rebuilt, live)
	}
	for index, row := range live {
		got := rebuilt[index]
		if got.FromKind != row.FromKind || got.FromID != row.FromID || got.ToKind != row.ToKind || got.ToID != row.ToID || got.Kind != row.Kind {
			t.Fatalf("rebuilt edge %d = %#v; want %#v", index, got, row)
		}
		recreated := row.FromKind == "comment" && row.ToID == issue.Key+"/spec"
		if recreated {
			if got.SourceSeq != nil || got.CreatedAt == row.CreatedAt {
				t.Fatalf("recreated edge %#v kept provenance %#v", got, row)
			}
			continue
		}
		if got.SourceSeq == nil || *got.SourceSeq != *row.SourceSeq || got.CreatedAt != row.CreatedAt {
			t.Fatalf("surviving edge %#v lost provenance %#v", got, row)
		}
	}
	if _, err := refs.RebuildAll(context.Background(), database.Pool, "https://dispatch.example"); err != nil {
		t.Fatalf("second rebuild: %v", err)
	}
	if again := readRefRows(t, database); !reflect.DeepEqual(again, rebuilt) {
		t.Fatalf("second rebuild changed the index: %#v -> %#v", rebuilt, again)
	}
}
