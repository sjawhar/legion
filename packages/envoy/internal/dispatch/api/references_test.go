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
	"time"

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
		"unknown kind":        {url.Values{"to": {askRef}, "kind": {"mentions,linked"}}, http.StatusBadRequest, "INVALID_KIND"},
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

// blockIndexHandler is a test server whose documents never settle during the test, so every
// version a test reads is one its own requests wrote.
func blockIndexHandler(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	return handler, database
}

func createSpecIssue(t *testing.T, handler http.Handler, project, spec string) model.Issue {
	t.Helper()
	response := createIssueRequest(t, handler, map[string]any{"project": project, "title": "Block index", "spec": spec})
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue with spec: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[model.Issue](t, response)
}

// blockIDsByText is, for each block GET …/blocks lists, its id keyed by its text: the document's
// markdown over the block's range.
func blockIDsByText(t *testing.T, handler http.Handler, artifactID string) map[string]string {
	t.Helper()
	markdown := decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/text", nil, "alice")).Markdown
	ids := map[string]string{}
	for _, block := range decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/blocks", nil, "alice")) {
		ids[markdown[block.From:block.To]] = block.ID
	}
	return ids
}

// editSpec applies ops to an issue's spec as a named version and returns the version it wrote.
func editSpec(t *testing.T, handler http.Handler, key string, ops []map[string]any) model.Version {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/artifacts/spec/edits", map[string]any{
		"ops": ops, "summary": "edit",
	}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("edit the spec: status=%d body=%s", response.Code, response.Body.String())
	}
	version := decodeBody[struct {
		Version *model.Version `json:"version"`
	}](t, response).Version
	if version == nil {
		t.Fatalf("the edit wrote no version: %s", response.Body.String())
	}
	return *version
}

type indexedBlock struct {
	Type           string
	Excerpt        string
	ChangedVersion int
}

// readIndexedBlocks is the document's block nodes by block id, and each one's changed_at.
func readIndexedBlocks(t *testing.T, database *store.Store, artifactID string) (map[string]indexedBlock, map[string]time.Time) {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select d.block_id, d.node_id, a.ref_key, d.type, d.excerpt, d.changed_version, d.changed_at
		from document_blocks d join artifacts a on a.id = d.artifact_id where d.artifact_id = $1
	`, artifactID)
	if err != nil {
		t.Fatalf("read document_blocks: %v", err)
	}
	defer rows.Close()
	blocks, changedAt := map[string]indexedBlock{}, map[string]time.Time{}
	for rows.Next() {
		var id, node, refKey string
		var block indexedBlock
		var at time.Time
		if err := rows.Scan(&id, &node, &refKey, &block.Type, &block.Excerpt, &block.ChangedVersion, &at); err != nil {
			t.Fatalf("scan document_blocks: %v", err)
		}
		if node != refKey+"#"+id {
			t.Fatalf("block %q node id = %q, want %q", id, node, refKey+"#"+id)
		}
		blocks[id], changedAt[id] = block, at
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate document_blocks: %v", err)
	}
	return blocks, changedAt
}

// readBlockTexts is the text each block's row at version records: nil for a block it deleted.
func readBlockTexts(t *testing.T, database *store.Store, artifactID string, version int) map[string]*string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select block_id, text from document_block_texts where artifact_id = $1 and version = $2
	`, artifactID, version)
	if err != nil {
		t.Fatalf("read document_block_texts: %v", err)
	}
	defer rows.Close()
	texts := map[string]*string{}
	for rows.Next() {
		var id string
		var body *string
		if err := rows.Scan(&id, &body); err != nil {
			t.Fatalf("scan document_block_texts: %v", err)
		}
		texts[id] = body
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate document_block_texts: %v", err)
	}
	return texts
}

type blockRefRow struct {
	BlockID, FromNode, ToKind, ToID, Kind string
	CreatedVersion                        int
	CreatedAt                             time.Time
	SourceSeq                             *int64
}

func readBlockRefs(t *testing.T, database *store.Store, artifactID string) []blockRefRow {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select block_id, from_node, to_kind, to_id, kind, created_version, created_at, source_seq
		from block_refs where artifact_id = $1 order by block_id, to_kind, to_id, kind
	`, artifactID)
	if err != nil {
		t.Fatalf("read block_refs: %v", err)
	}
	defer rows.Close()
	var result []blockRefRow
	for rows.Next() {
		var row blockRefRow
		if err := rows.Scan(&row.BlockID, &row.FromNode, &row.ToKind, &row.ToID, &row.Kind, &row.CreatedVersion, &row.CreatedAt, &row.SourceSeq); err != nil {
			t.Fatalf("scan block_refs: %v", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate block_refs: %v", err)
	}
	return result
}

// requireIndexedAt fails unless the document's blocks are indexed at version: its
// document_block_versions row names it, and document_indexed_versions records it.
func requireIndexedAt(t *testing.T, database *store.Store, artifactID string, version int) {
	t.Helper()
	var indexed *int
	var recorded bool
	if err := database.Pool.QueryRow(context.Background(), `
		select (select version from document_block_versions where artifact_id = $1),
		       exists (select 1 from document_indexed_versions where artifact_id = $1 and version = $2)
	`, artifactID, version).Scan(&indexed, &recorded); err != nil {
		t.Fatalf("read the block index version: %v", err)
	}
	if indexed == nil || *indexed != version || !recorded {
		t.Fatalf("document %s blocks indexed at %v (version %d recorded indexed: %v), want version %d", artifactID, indexed, version, recorded, version)
	}
}

func latestVersion(t *testing.T, database *store.Store, artifactID string) model.Version {
	t.Helper()
	var version model.Version
	if err := database.Pool.QueryRow(context.Background(), `
		select number, created_at from artifact_versions where artifact_id = $1 order by number desc limit 1
	`, artifactID).Scan(&version.Number, &version.CreatedAt); err != nil {
		t.Fatalf("read the latest version of %s: %v", artifactID, err)
	}
	return version
}

// A version records each block GET …/blocks lists as a node with its text, and a mention written
// inside a block as an edge out of the innermost block holding it, dated by the version that wrote
// it and the event that recorded that version.
func TestAVersionIndexesItsBlocksAndTheMentionsInThem(t *testing.T) {
	handler, database := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	target := createReferenceAPIIssue(t, handler, "CORE")
	callout := ":::callout{#ship_it kind=\"note\" title=\"\"}\nShip it.\n:::"
	issue := createSpecIssue(t, handler, "CORE", callout+"\n\nOther paragraph.\n")
	spec := issue.PrimaryArtifactID

	ids := blockIDsByText(t, handler, spec)
	inner, other := ids["Ship it."], ids["Other paragraph."]
	if len(ids) != 3 || ids[callout] != "ship_it" || inner == "" || other == "" {
		t.Fatalf("GET /blocks = %#v, want the callout ship_it, its paragraph and the other paragraph", ids)
	}
	blocks, _ := readIndexedBlocks(t, database, spec)
	want := map[string]indexedBlock{
		"ship_it": {Type: "callout", Excerpt: callout, ChangedVersion: 1},
		inner:     {Type: "paragraph", Excerpt: "Ship it.", ChangedVersion: 1},
		other:     {Type: "paragraph", Excerpt: "Other paragraph.", ChangedVersion: 1},
	}
	if !reflect.DeepEqual(blocks, want) {
		t.Fatalf("document_blocks after version 1 = %#v, want %#v", blocks, want)
	}
	requireIndexedAt(t, database, spec, 1)
	if texts := readBlockTexts(t, database, spec, 1); len(texts) != 3 || *texts["ship_it"] != callout || *texts[inner] != "Ship it." || *texts[other] != "Other paragraph." {
		t.Fatalf("block texts of version 1 = %#v, want each block's text", texts)
	}
	if pairs := readBlockRefs(t, database, spec); len(pairs) != 0 {
		t.Fatalf("block_refs of a spec citing nothing = %#v", pairs)
	}

	version := editSpec(t, handler, issue.Key, []map[string]any{
		{"op": "replace", "find": "Other paragraph.", "with": "Other paragraph. See dispatch://" + target.Key + "."},
		{"op": "replace", "find": "Ship it.", "with": "Ship it after dispatch://" + target.Key + "."},
	})
	var eventID int64
	if err := database.Pool.QueryRow(context.Background(), `
		select id from events
		where type = 'artifact.version' and payload->>'artifact_id' = $1 and (payload->'version'->>'number')::int = $2
	`, spec, version.Number).Scan(&eventID); err != nil {
		t.Fatalf("read the event of version %d: %v", version.Number, err)
	}
	pairs := readBlockRefs(t, database, spec)
	// The callout holds the paragraph that cites the issue, but the paragraph is the innermost
	// block holding the mention: the callout itself writes no edge.
	wantPairs := []blockRefRow{
		{BlockID: inner, FromNode: issue.Key + "/spec#" + inner, ToKind: "issue", ToID: target.Key, Kind: "mentions"},
		{BlockID: other, FromNode: issue.Key + "/spec#" + other, ToKind: "issue", ToID: target.Key, Kind: "mentions"},
	}
	if inner > other {
		wantPairs[0], wantPairs[1] = wantPairs[1], wantPairs[0]
	}
	for index := range wantPairs {
		wantPairs[index].CreatedVersion, wantPairs[index].CreatedAt, wantPairs[index].SourceSeq = version.Number, version.CreatedAt, &eventID
	}
	if len(pairs) != len(wantPairs) {
		t.Fatalf("block_refs after the citing edit = %#v, want %#v", pairs, wantPairs)
	}
	for index, pair := range pairs {
		wanted := wantPairs[index]
		if pair.SourceSeq == nil || *pair.SourceSeq != eventID || !pair.CreatedAt.Equal(wanted.CreatedAt) {
			t.Fatalf("block_refs[%d] = %#v (source_seq %v), want created at %s by event %d", index, pair, pair.SourceSeq, wanted.CreatedAt, eventID)
		}
		pair.SourceSeq, pair.CreatedAt = wanted.SourceSeq, wanted.CreatedAt
		if pair != wanted {
			t.Fatalf("block_refs[%d] = %#v, want %#v", index, pair, wanted)
		}
	}
	requireIndexedAt(t, database, spec, version.Number)
}

// A5: a block's changed version moves only when its own text (its markdown, children included)
// changes. Editing the paragraph inside the callout moves the paragraph and the callout; editing
// another paragraph moves only that one. Deleting the callout records both texts as gone, and
// restoring the callout's id records it anew.
func TestABlocksChangedVersionMovesOnlyWithItsText(t *testing.T) {
	handler, database := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	callout := func(body string) string { return ":::callout{#ship_it kind=\"note\" title=\"\"}\n" + body + "\n:::" }
	issue := createSpecIssue(t, handler, "CORE", callout("Ship it.")+"\n\nQ holds steady.\n\nR holds steady.\n")
	spec := issue.PrimaryArtifactID
	ids := blockIDsByText(t, handler, spec)
	p, q, r := ids["Ship it."], ids["Q holds steady."], ids["R holds steady."]
	if len(ids) != 4 || ids[callout("Ship it.")] != "ship_it" || p == "" || q == "" || r == "" {
		t.Fatalf("GET /blocks = %#v, want the callout, its paragraph P, and Q and R", ids)
	}
	versionAt := map[int]time.Time{1: latestVersion(t, database, spec).CreatedAt}
	requireBlocks := func(stage string, want map[string]int) {
		t.Helper()
		blocks, changedAt := readIndexedBlocks(t, database, spec)
		got := make(map[string]int, len(blocks))
		for id, block := range blocks {
			got[id] = block.ChangedVersion
			if !changedAt[id].Equal(versionAt[block.ChangedVersion]) {
				t.Fatalf("%s: block %q changed_at = %s, want version %d's %s", stage, id, changedAt[id], block.ChangedVersion, versionAt[block.ChangedVersion])
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: changed versions = %#v, want %#v", stage, got, want)
		}
	}
	requireTexts := func(stage string, version int, want map[string]*string) {
		t.Helper()
		if got := readBlockTexts(t, database, spec, version); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: block texts of version %d = %#v, want %#v", stage, version, got, want)
		}
	}
	requireBlocks("version 1", map[string]int{"ship_it": 1, p: 1, q: 1, r: 1})

	edited := editSpec(t, handler, issue.Key, []map[string]any{{"op": "replace", "find": "Ship it.", "with": "Ship it now."}})
	versionAt[edited.Number] = edited.CreatedAt
	requireBlocks("P edited", map[string]int{"ship_it": edited.Number, p: edited.Number, q: 1, r: 1})
	requireTexts("P edited", edited.Number, map[string]*string{"ship_it": new(callout("Ship it now.")), p: new("Ship it now.")})

	movedQ := editSpec(t, handler, issue.Key, []map[string]any{{"op": "replace", "find": "Q holds steady.", "with": "Q moves."}})
	versionAt[movedQ.Number] = movedQ.CreatedAt
	requireBlocks("Q edited", map[string]int{"ship_it": edited.Number, p: edited.Number, q: movedQ.Number, r: 1})
	requireTexts("Q edited", movedQ.Number, map[string]*string{q: new("Q moves.")})

	deleted := editSpec(t, handler, issue.Key, []map[string]any{{"op": "delete", "block": "ship_it"}})
	versionAt[deleted.Number] = deleted.CreatedAt
	requireBlocks("callout deleted", map[string]int{q: movedQ.Number, r: 1})
	requireTexts("callout deleted", deleted.Number, map[string]*string{"ship_it": nil, p: nil})

	restored := editSpec(t, handler, issue.Key, []map[string]any{{"op": "insert", "after": "end", "markdown": callout("Ship it now.")}})
	versionAt[restored.Number] = restored.CreatedAt
	restoredP := blockIDsByText(t, handler, spec)["Ship it now."]
	requireBlocks("callout restored", map[string]int{"ship_it": restored.Number, restoredP: restored.Number, q: movedQ.Number, r: 1})
	requireTexts("callout restored", restored.Number, map[string]*string{"ship_it": new(callout("Ship it now.")), restoredP: new("Ship it now.")})
	requireIndexedAt(t, database, spec, restored.Number)
}

// A node one document cites from two blocks is cited twice, so a version that moves a citation
// from one citing block to two, or back, moves the node's count even though the document cites it
// throughout: each such version's event names it.
func TestAVersionWriteNamesATargetWhoseBlockCountChanged(t *testing.T) {
	handler, database := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	createReferenceAPIProject(t, handler, "OPS")
	target := createReferenceAPIIssue(t, handler, "CORE")
	cite := "dispatch://" + target.Key
	source := createSpecIssue(t, handler, "OPS", "First cites "+cite+".\n\nSecond paragraph.\n")
	events := "/api/v1/issues/" + source.Key + "/events"
	named := []model.ChangedReference{{Kind: "issue", ID: target.Key}}
	requireBlockCitations := func(stage string, want int) {
		t.Helper()
		var got int
		if err := database.Pool.QueryRow(context.Background(), `
			select count(*) from graph_edges where from_kind = 'block' and to_kind = 'issue' and to_id = $1
		`, target.Key).Scan(&got); err != nil {
			t.Fatalf("count block citations: %v", err)
		}
		if got != want {
			t.Fatalf("%s: blocks citing %s = %d, want %d", stage, target.Key, got, want)
		}
	}
	requireBlockCitations("one citing block", 1)

	editSpec(t, handler, source.Key, []map[string]any{{"op": "replace", "find": "Second paragraph.", "with": "Second cites " + cite + "."}})
	requireArtifactEventNames(t, handler, events, source.PrimaryArtifactID, named)
	requireBlockCitations("two citing blocks", 2)

	editSpec(t, handler, source.Key, []map[string]any{{"op": "replace", "find": "Second cites " + cite + ".", "with": "Second paragraph."}})
	requireArtifactEventNames(t, handler, events, source.PrimaryArtifactID, named)
	requireBlockCitations("one citing block again", 1)
}

// Migration 0118's trigger marks a document owed on every version insert; each path a block-aware
// build writes a version through indexes that version's blocks in the same transaction, so none
// leaves its document owed: issue creation, the edit route, the upload route (a new document and
// a replacement) and an accepted suggestion.
func TestEveryVersionWriteLeavesItsDocumentIndexed(t *testing.T) {
	handler, database := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	requireWritten := func(path, artifactID, text string) {
		t.Helper()
		version := latestVersion(t, database, artifactID)
		requireIndexedAt(t, database, artifactID, version.Number)
		blocks, _ := readIndexedBlocks(t, database, artifactID)
		for _, block := range blocks {
			if block.Excerpt == text {
				return
			}
		}
		t.Fatalf("%s: version %d of %s indexed blocks %#v, none of them %q", path, version.Number, artifactID, blocks, text)
	}

	issue := createSpecIssue(t, handler, "CORE", "Created paragraph.\n")
	spec := issue.PrimaryArtifactID
	requireWritten("issue creation", spec, "Created paragraph.")

	editSpec(t, handler, issue.Key, []map[string]any{{"op": "replace", "find": "Created paragraph.", "with": "Edited paragraph."}})
	requireWritten("edit route", spec, "Edited paragraph.")

	uploaded := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{"name": "notes.md"}, "notes.md", "text/markdown", []byte("Uploaded paragraph.\n"), "alice")
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload a document: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	notes := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact.ID
	requireWritten("upload of a new document", notes, "Uploaded paragraph.")
	if replaced := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{"name": "notes.md"}, "notes.md", "text/markdown", []byte("Replaced paragraph.\n"), "alice"); replaced.Code != http.StatusCreated {
		t.Fatalf("replace the document: status=%d body=%s", replaced.Code, replaced.Body.String())
	}
	requireWritten("upload replacing a document", notes, "Replaced paragraph.")

	suggestion := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"anchor":     map[string]any{"artifact": "spec", "quote": "Edited paragraph."},
		"body":       "Reword it.",
		"suggestion": map[string]any{"replace_with": "Suggested paragraph."},
	}, "alice")
	if suggestion.Code != http.StatusCreated {
		t.Fatalf("suggest a rewording: status=%d body=%s", suggestion.Code, suggestion.Body.String())
	}
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+decodeBody[writtenRecord](t, suggestion).ID+"/accept", nil, "alice"); response.Code != http.StatusOK {
		t.Fatalf("accept the suggestion: status=%d body=%s", response.Code, response.Body.String())
	}
	requireWritten("accepted suggestion", spec, "Suggested paragraph.")
}
