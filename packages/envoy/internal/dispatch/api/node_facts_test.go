package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// nodeFacts is a node's six facts as a test compares them: each actor as "kind:id", each time as
// an instant.
type nodeFacts struct {
	MadeBy        []string
	MadeAt        *time.Time
	ChangedBy     []string
	ChangedAt     *time.Time
	Parent        *model.NodeParent
	ParentVersion *int
}

func (f nodeFacts) String() string {
	instant := func(at *time.Time) string {
		if at == nil {
			return "null"
		}
		return at.UTC().Format(time.RFC3339Nano)
	}
	parent, version := "null", "null"
	if f.Parent != nil {
		parent = fmt.Sprintf("%+v", *f.Parent)
	}
	if f.ParentVersion != nil {
		version = strconv.Itoa(*f.ParentVersion)
	}
	return fmt.Sprintf("{made_by %v at %s, changed_by %v at %s, parent %s, parent_version %s}",
		f.MadeBy, instant(f.MadeAt), f.ChangedBy, instant(f.ChangedAt), parent, version)
}

// factsOf is node's six facts.
func factsOf(node model.GraphNode) nodeFacts {
	return nodeFacts{
		MadeBy: actorNames(node.MadeBy), MadeAt: node.MadeAt, ChangedBy: actorNames(node.ChangedBy), ChangedAt: node.ChangedAt,
		Parent: node.Parent, ParentVersion: node.ParentVersion,
	}
}

func actorNames(actors []model.Actor) []string {
	names := make([]string, len(actors))
	for index, actor := range actors {
		names[index] = docs.ActorRef(actor)
	}
	return names
}

func sameInstant(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// requireFacts fails unless node carries exactly want's facts.
func requireFacts(t *testing.T, what string, node model.GraphNode, want nodeFacts) {
	t.Helper()
	got := factsOf(node)
	if !slices.Equal(got.MadeBy, want.MadeBy) || !sameInstant(got.MadeAt, want.MadeAt) ||
		!slices.Equal(got.ChangedBy, want.ChangedBy) || !sameInstant(got.ChangedAt, want.ChangedAt) ||
		!reflect.DeepEqual(got.Parent, want.Parent) || !reflect.DeepEqual(got.ParentVersion, want.ParentVersion) {
		t.Fatalf("%s facts = %s, want %s", what, got, want)
	}
}

// requireSameNode fails unless got is the node want is, carrying want's six facts.
func requireSameNode(t *testing.T, what string, got, want model.GraphNode) {
	t.Helper()
	if got.Kind != want.Kind || got.ID != want.ID {
		t.Fatalf("%s = %s %s, want %s %s", what, got.Kind, got.ID, want.Kind, want.ID)
	}
	requireFacts(t, what, got, factsOf(want))
}

// normalizeAt fails unless *at is the instant want (or both are null), then points *at at want, so
// a whole-node comparison that follows compares that instant once and not its encoding.
func normalizeAt(t *testing.T, what string, at **time.Time, want *time.Time) {
	t.Helper()
	if !sameInstant(*at, want) {
		t.Fatalf("%s = %v, want %v", what, *at, want)
	}
	*at = want
}

// requireFactKeys fails unless the node JSON node carries all six facts, made_by and changed_by
// as arrays even when empty.
func requireFactKeys(t *testing.T, what string, node map[string]json.RawMessage) {
	t.Helper()
	for _, key := range []string{"made_by", "made_at", "changed_by", "changed_at", "parent", "parent_version"} {
		value, present := node[key]
		if !present {
			t.Fatalf("%s node has no %s: %v", what, key, node)
		}
		if (key == "made_by" || key == "changed_by") && !strings.HasPrefix(string(value), "[") {
			t.Fatalf("%s node's %s = %s, want an array", what, key, value)
		}
	}
}

// readGraph is GET /api/v1/references?query, failing unless its node and every edge's node and
// block carry all six facts on the wire.
func readGraph(t *testing.T, handler http.Handler, query url.Values) model.GraphReferences {
	t.Helper()
	response := graphRequest(t, handler, query)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/references?%s: status=%d body=%s", query.Encode(), response.Code, response.Body.String())
	}
	var raw struct {
		Node  map[string]json.RawMessage `json:"node"`
		Edges []struct {
			Node  map[string]json.RawMessage `json:"node"`
			Block map[string]json.RawMessage `json:"block"`
		} `json:"edges"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode %s: %v", response.Body.String(), err)
	}
	requireFactKeys(t, query.Encode(), raw.Node)
	for _, edge := range raw.Edges {
		requireFactKeys(t, query.Encode()+" edge", edge.Node)
		if edge.Block != nil {
			requireFactKeys(t, query.Encode()+" edge block", edge.Block)
		}
	}
	return decodeBody[model.GraphReferences](t, response)
}

// readNode is the node ?to=ref names.
func readNode(t *testing.T, handler http.Handler, ref string) model.GraphNode {
	t.Helper()
	return readGraph(t, handler, url.Values{"to": {ref}}).Node
}

// created fails unless response is 201 Created, and returns it.
func created(t *testing.T, what string, response *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	if response.Code != http.StatusCreated {
		t.Fatalf("%s: status=%d body=%s", what, response.Code, response.Body.String())
	}
	return response
}

// succeeded fails unless response is 200 OK.
func succeeded(t *testing.T, what string, response *httptest.ResponseRecorder) {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("%s: status=%d body=%s", what, response.Code, response.Body.String())
	}
}

// versionOf is version number of document artifactID as stored: its number, authors and time.
func versionOf(t *testing.T, database *store.Store, artifactID string, number int) model.Version {
	t.Helper()
	var version model.Version
	var authors []byte
	if err := database.Pool.QueryRow(context.Background(), `
		select number, authors, created_at from artifact_versions where artifact_id = $1 and number = $2
	`, artifactID, number).Scan(&version.Number, &authors, &version.CreatedAt); err != nil {
		t.Fatalf("read version %d of %s: %v", number, artifactID, err)
	}
	if err := json.Unmarshal(authors, &version.Authors); err != nil {
		t.Fatalf("decode the authors of version %d of %s: %v", number, artifactID, err)
	}
	return version
}

// latestIssueEventAt is when issue key's latest event of type eventType was written.
func latestIssueEventAt(t *testing.T, database *store.Store, key, eventType string) time.Time {
	t.Helper()
	var at time.Time
	if err := database.Pool.QueryRow(context.Background(), `
		select created_at from events where issue_key = $1 and type = $2 order by seq desc limit 1
	`, key, eventType).Scan(&at); err != nil {
		t.Fatalf("read %s's latest %s event: %v", key, eventType, err)
	}
	return at
}

// artifactCreatedAt is when artifact id was made.
func artifactCreatedAt(t *testing.T, database *store.Store, id string) time.Time {
	t.Helper()
	var at time.Time
	if err := database.Pool.QueryRow(context.Background(), `select created_at from artifacts where id = $1`, id).Scan(&at); err != nil {
		t.Fatalf("read when artifact %s was made: %v", id, err)
	}
	return at
}

// createChildIssue creates, as alice, an issue in project under parent with spec, and renames it
// as bob, so its last change is bob's.
func createChildIssue(t *testing.T, handler http.Handler, project, parent, spec string) model.Issue {
	t.Helper()
	child := decodeBody[model.Issue](t, created(t, "create the child issue", createIssueRequest(t, handler, map[string]any{
		"project": project, "title": "Child", "spec": spec, "parent": parent,
	})))
	succeeded(t, "rename the child issue as bob", dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child.Key, map[string]any{"title": "Renamed child"}, "bob"))
	return child
}

// R12, A11: every node kind the graph holds carries who made it and when, who last changed it and
// when, its parent and the version of that parent it belongs to, exactly as each kind records
// them; a fact Dispatch never recorded is empty. The facts are on the wire for every node, made_by
// and changed_by as arrays even when empty.
func TestEveryNodeKindCarriesItsFacts(t *testing.T) {
	handler, database := blockIndexHandler(t)
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "CORE")
	sessionID := sessionActor()["id"].(string)
	alice, bob, session, nobody := []string{"user:alice"}, []string{"user:bob"}, []string{"session:" + sessionID}, []string{}

	parentIssue := createSpecIssue(t, handler, "CORE", "Parent spec.\n")
	child := createChildIssue(t, handler, "CORE", parentIssue.Key, "Child spec paragraph.\n\nSecond paragraph.\n")
	parentRef, childRef, specRef := "dispatch://"+parentIssue.Key, "dispatch://"+child.Key, "dispatch://"+child.Key+"/spec"
	ofChild := &model.NodeParent{Kind: "issue", ID: child.Key, Ref: childRef}
	ofSpec := &model.NodeParent{Kind: "artifact", ID: child.PrimaryArtifactID, Ref: specRef}

	issueRequest := func(path string, body map[string]any) *httptest.ResponseRecorder {
		return created(t, "POST "+path, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+child.Key+path, body, "alice"))
	}
	anchored := decodeBody[model.Comment](t, issueRequest("/comments", map[string]any{
		"anchor": map[string]string{"artifact": "spec", "quote": "Child spec paragraph"}, "body": "Why this paragraph?",
	}))
	reply := decodeBody[model.Comment](t, issueRequest("/comments", map[string]any{"body": "Because.", "reply_to": anchored.ID}))
	replyToReply := decodeBody[model.Comment](t, issueRequest("/comments", map[string]any{"body": "Agreed.", "reply_to": reply.ID}))
	ask := decodeBody[model.Ask](t, created(t, "a session's ask", sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+child.Key+"/asks", map[string]any{
		"question": "Ship it?", "actor": sessionActor(),
	})))
	inThread := decodeBody[model.Comment](t, issueRequest("/comments", map[string]any{"body": "Context for the ask.", "ask_id": ask.ID}))
	anchoredAsk := decodeBody[model.Ask](t, issueRequest("/asks", map[string]any{
		"anchor": map[string]string{"artifact": "spec", "quote": "Second paragraph"}, "question": "Is this right?",
	}))
	message := decodeBody[model.Message](t, issueRequest("/messages", map[string]any{"body": "Status update."}))
	replyMessage := decodeBody[model.Message](t, issueRequest("/messages", map[string]any{"body": "Thanks.", "in_reply_to": message.ID}))
	upload := func(name, mime string, content []byte) model.Artifact {
		return decodeBody[struct {
			Artifact model.Artifact `json:"artifact"`
		}](t, created(t, "upload "+name, multipartRequest(t, handler, "/api/v1/issues/"+child.Key+"/artifacts", map[string]string{"name": name}, name, mime, content, "alice"))).Artifact
	}
	file, image := upload("notes.txt", "text/plain", []byte("plain notes\n")), upload("shot.png", "image/png", pngBytes("pixels"))

	specV2 := editSpec(t, handler, child.Key, []map[string]any{{"op": "insert", "after": "end", "markdown": "Third paragraph."}})
	approval := decodeBody[struct {
		Ask model.Ask `json:"ask"`
	}](t, created(t, "request approval of the spec", sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+child.PrimaryArtifactID+"/approval-requests", map[string]any{
		"actor": sessionActor(),
	}))).Ask

	design := createProjectDocument(t, handler, "CORE", "Design notes", "# Design\n")
	created(t, "write version 2 of the project document", dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/CORE/artifacts", map[string]string{
		"name": "Design notes", "content": "# Design\n\nRevised.\n",
	}, "alice"))
	designRef := "dispatch://CORE/artifact/" + design.Slug
	designComment := decodeBody[model.Comment](t, created(t, "comment on the project document", dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+design.ID+"/comments", map[string]any{
		"body": "Looks right.",
	}, "alice")))
	designAsk := decodeBody[model.Ask](t, created(t, "ask on the project document", dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+design.ID+"/asks", map[string]any{
		"question": "Ship the design?",
	}, "alice")))

	// An agent's conversation's file, stored as artifact_no_project_test stores one.
	const conversation = "conversation-session"
	var photoID string
	var photoMadeAt time.Time
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (session_id, slug, name, kind, created_by)
		values ($1, 'photo-png', 'photo.png', 'image', jsonb_build_object('kind', 'session', 'id', $1::text))
		returning id::text, created_at
	`, conversation).Scan(&photoID, &photoMadeAt); err != nil {
		t.Fatalf("store a conversation's file: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, content, mime, size, sha256, authors)
		values ($1, 1, '\x89504e47'::bytea, 'image/png', 4, repeat('0', 64), jsonb_build_array(jsonb_build_object('kind', 'session', 'id', $2::text)))
	`, photoID, conversation); err != nil {
		t.Fatalf("store the file's version: %v", err)
	}

	// web is part of platform; platform's times were never recorded; orphan names a parent the
	// model no longer holds and has no recorded times at all.
	webMade, webChanged := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	for _, statement := range []string{
		`insert into architecture_sources (project_key, id, repo, branch, installation_id, created_by)
			values ('CORE', 1, 'acme/core', 'main', 1, '{"kind":"system","id":"test"}')`,
		`insert into architecture_snapshots (id, project_key, commit, files) values (901, 'CORE', 'c1', '{}')`,
		`insert into components (project_key, id, title, prose, parent, snapshot_id, source_id) values
			('CORE', 'platform', 'Platform', '', null, 901, 1), ('CORE', 'web', 'Web', '', 'platform', 901, 1),
			('CORE', 'orphan', 'Orphan', '', 'retired', 901, 1)`,
		`insert into component_facts (project_key, id, made_at, changed_at) values
			('CORE', 'platform', null, null), ('CORE', 'web', '2026-10-01T09:00:00Z', '2026-10-02T09:00:00Z')`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed components: %v\n%s", err, statement)
		}
	}

	renamedAt := latestIssueEventAt(t, database, child.Key, "issue.updated")
	if !renamedAt.After(child.CreatedAt) {
		t.Fatalf("the rename at %s is not after the child's creation at %s", renamedAt, child.CreatedAt)
	}
	specMadeAt := artifactCreatedAt(t, database, child.PrimaryArtifactID)
	specChangedAt := versionOf(t, database, child.PrimaryArtifactID, specV2.Number).CreatedAt
	designChangedAt := versionOf(t, database, design.ID, 2).CreatedAt
	ofHead := &model.NodeParent{Kind: "comment", ID: anchored.ID, Ref: childRef + "/comment/" + anchored.ID}
	ofDesign := &model.NodeParent{Kind: "artifact", ID: design.ID, Ref: designRef}
	itemRef := func(kind, id string) string { return childRef + "/" + kind + "/" + id }
	unchanged := func(by []string, at time.Time, parent *model.NodeParent, version *int) nodeFacts {
		return nodeFacts{MadeBy: by, MadeAt: &at, ChangedBy: by, ChangedAt: &at, Parent: parent, ParentVersion: version}
	}
	if specV2.Number != 2 {
		t.Fatalf("the spec edit wrote version %d, want 2", specV2.Number)
	}

	for _, test := range []struct {
		what, ref string
		want      nodeFacts
	}{
		{"issue P", parentRef, unchanged(alice, parentIssue.CreatedAt, nil, nil)},
		{"issue C", childRef, nodeFacts{MadeBy: alice, MadeAt: &child.CreatedAt, ChangedBy: bob, ChangedAt: &renamedAt, Parent: &model.NodeParent{Kind: "issue", ID: parentIssue.Key, Ref: parentRef}}},
		{"C's spec", specRef, nodeFacts{MadeBy: alice, MadeAt: &specMadeAt, ChangedBy: alice, ChangedAt: &specChangedAt, Parent: ofChild}},
		{"the anchored comment", itemRef("comment", anchored.ID), unchanged(alice, anchored.CreatedAt, ofSpec, new(1))},
		{"the reply", itemRef("comment", reply.ID), unchanged(alice, reply.CreatedAt, ofHead, nil)},
		{"the reply to the reply", itemRef("comment", replyToReply.ID), unchanged(alice, replyToReply.CreatedAt, ofHead, nil)},
		{"the ask on C", itemRef("ask", ask.ID), unchanged(session, ask.CreatedAt, ofChild, nil)},
		{"the comment in the ask's thread", itemRef("comment", inThread.ID), unchanged(alice, inThread.CreatedAt, &model.NodeParent{Kind: "ask", ID: ask.ID, Ref: itemRef("ask", ask.ID)}, nil)},
		{"the anchored ask", itemRef("ask", anchoredAsk.ID), unchanged(alice, anchoredAsk.CreatedAt, ofSpec, new(1))},
		{"the message", itemRef("message", message.ID), unchanged(alice, message.CreatedAt, ofChild, nil)},
		{"the reply message", itemRef("message", replyMessage.ID), unchanged(alice, replyMessage.CreatedAt, &model.NodeParent{Kind: "message", ID: message.ID, Ref: itemRef("message", message.ID)}, nil)},
		{"the file", childRef + "/artifact/" + file.Slug, nodeFacts{MadeBy: alice, MadeAt: &file.CreatedAt, ChangedBy: alice, ChangedAt: new(versionOf(t, database, file.ID, 1).CreatedAt), Parent: ofChild}},
		{"the image", childRef + "/artifact/" + image.Slug, nodeFacts{MadeBy: alice, MadeAt: &image.CreatedAt, ChangedBy: alice, ChangedAt: new(versionOf(t, database, image.ID, 1).CreatedAt), Parent: ofChild}},
		{"the approval request", itemRef("ask", approval.ID), unchanged(session, approval.CreatedAt, ofSpec, new(2))},
		{"the project document", designRef, nodeFacts{MadeBy: alice, MadeAt: &design.CreatedAt, ChangedBy: alice, ChangedAt: &designChangedAt}},
		{"the project document's comment", designRef + "/comment/" + designComment.ID, unchanged(alice, designComment.CreatedAt, ofDesign, new(2))},
		{"the project document's ask", designRef + "/ask/" + designAsk.ID, unchanged(alice, designAsk.CreatedAt, ofDesign, new(2))},
		{"the conversation's file", "dispatch://agent/" + conversation + "/artifact/photo-png", nodeFacts{
			MadeBy: []string{"session:" + conversation}, MadeAt: &photoMadeAt, ChangedBy: []string{"session:" + conversation},
			ChangedAt: new(versionOf(t, database, photoID, 1).CreatedAt), Parent: &model.NodeParent{Kind: "session", ID: conversation},
		}},
		{"component web", "dispatch://CORE/component/web", nodeFacts{
			MadeBy: nobody, MadeAt: &webMade, ChangedBy: nobody, ChangedAt: &webChanged,
			Parent: &model.NodeParent{Kind: "component", ID: "CORE/platform", Ref: "dispatch://CORE/component/platform"},
		}},
		{"component platform", "dispatch://CORE/component/platform", nodeFacts{MadeBy: nobody, ChangedBy: nobody}},
		{"component orphan", "dispatch://CORE/component/orphan", nodeFacts{MadeBy: nobody, ChangedBy: nobody}},
	} {
		requireFacts(t, test.what, readNode(t, handler, test.ref), test.want)
	}

	followers := readGraph(t, handler, url.Values{"from": {itemRef("ask", ask.ID)}, "kind": {"followed_by"}}).Edges
	if len(followers) != 1 || followers[0].Node.Kind != "session" || followers[0].Node.ID != sessionID {
		t.Fatalf("the ask's followers = %#v, want session %s", followers, sessionID)
	}
	requireFacts(t, "the session following the ask", followers[0].Node, nodeFacts{MadeBy: nobody, ChangedBy: nobody})
}

// A11: the other end of an edge, a document read's block at an edge, and every node of a
// block-links read carry the same facts as the node read by its own reference.
func TestTheFactsOfAnEdgesOtherEndAreTheNodesOwn(t *testing.T) {
	handler, _ := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	parentIssue := createSpecIssue(t, handler, "CORE", "Parent spec.\n")
	child := createChildIssue(t, handler, "CORE", parentIssue.Key, shipIt("Ship it.")+"\n\nOther paragraph.\n")
	parentRef, childRef, specRef := "dispatch://"+parentIssue.Key, "dispatch://"+child.Key, "dispatch://"+child.Key+"/spec"
	blockRef := specRef + "#b-ship%5Fit"
	// bob changes the block alice made, so its maker and its last change differ.
	editIssueDocumentAs(t, handler, child.Key, "spec", "bob", []map[string]any{{"op": "replace", "find": "Ship it.", "with": "Ship it now."}})

	head := createReferenceAPIComment(t, handler, child.Key, "A head comment.")
	reply := decodeBody[model.Comment](t, created(t, "reply", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+child.Key+"/comments", map[string]any{
		"body": "A reply.", "reply_to": head.ID,
	}, "bob")))
	repliesTo := readGraph(t, handler, url.Values{"from": {childRef + "/comment/" + reply.ID}, "kind": {"replies_to"}}).Edges
	if len(repliesTo) != 1 {
		t.Fatalf("the reply's replies_to edges = %#v, want one", repliesTo)
	}
	requireSameNode(t, "the reply's replies_to end", repliesTo[0].Node, readNode(t, handler, childRef+"/comment/"+head.ID))

	up := readGraph(t, handler, url.Values{"from": {childRef}, "kind": {"child_of"}}).Edges
	if len(up) != 1 {
		t.Fatalf("C's child_of edges = %#v, want one", up)
	}
	requireSameNode(t, "C's child_of end", up[0].Node, readNode(t, handler, parentRef))
	down := readGraph(t, handler, url.Values{"to": {parentRef}, "kind": {"child_of"}}).Edges
	if len(down) != 1 {
		t.Fatalf("child_of edges into P = %#v, want one", down)
	}
	requireSameNode(t, "the child_of edge into P's other end", down[0].Node, readNode(t, handler, childRef))

	createReferenceAPIComment(t, handler, child.Key, "Decided in "+blockRef+".")
	var atBlock *model.GraphNode
	for _, edge := range readGraph(t, handler, url.Values{"to": {specRef}}).Edges {
		if edge.Block != nil {
			atBlock = edge.Block
		}
	}
	if atBlock == nil {
		t.Fatal("the spec's read names no block at any edge")
	}
	requireSameNode(t, "the spec read's edges[].block", *atBlock, readNode(t, handler, blockRef))

	decisions := uploadIssueDocument(t, handler, child.Key, "decisions.md", "Ship per "+blockRef+".\n")
	decisionsRef := "dispatch://" + child.Key + "/artifact/" + decisions.Slug
	response := blockLinksRequest(t, handler, url.Values{"from": {decisionsRef}, "to": {specRef}})
	succeeded(t, "GET /api/v1/block-links", response)
	var raw struct {
		From  map[string]json.RawMessage `json:"from"`
		To    map[string]json.RawMessage `json:"to"`
		Links []struct {
			From map[string]json.RawMessage `json:"from"`
			To   map[string]json.RawMessage `json:"to"`
		} `json:"links"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode block links %s: %v", response.Body.String(), err)
	}
	requireFactKeys(t, "block-links from", raw.From)
	requireFactKeys(t, "block-links to", raw.To)
	for _, link := range raw.Links {
		requireFactKeys(t, "block-links links[].from", link.From)
		requireFactKeys(t, "block-links links[].to", link.To)
	}
	links := decodeBody[model.BlockLinks](t, response)
	requireSameNode(t, "block-links from", links.From, readNode(t, handler, decisionsRef))
	requireSameNode(t, "block-links to", links.To, readNode(t, handler, specRef))
	if len(links.Links) != 1 {
		t.Fatalf("links from decisions.md to the spec = %#v, want one", links.Links)
	}
	for _, link := range links.Links {
		requireSameNode(t, "block-links links[].from", link.From, readNode(t, handler, link.From.Ref))
		requireSameNode(t, "block-links links[].to", link.To, readNode(t, handler, link.To.Ref))
	}
}

// R12: a node's last change is a change of its own text or fields. An ask's edit by a person who
// did not write it names that person; its answer and its resolution change nothing of it. A
// comment's edit by its author moves its last change; another person resolving it does not. A
// claim is no change of the issue. A spec edit is the spec's last change and the edited block's,
// and leaves the other blocks' and every block's maker.
func TestAChangeMovesOnlyTheChangedNode(t *testing.T) {
	handler, database := blockIndexHandler(t)
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "CORE")
	session := []string{"session:" + sessionActor()["id"].(string)}
	alice, bob := []string{"user:alice"}, []string{"user:bob"}
	issue, blockRef := createShipItSpec(t, handler, "CORE")
	issueRef, specRef := "dispatch://"+issue.Key, "dispatch://"+issue.Key+"/spec"
	otherRef := specRef + "#b-" + blockIDsByText(t, handler, issue.PrimaryArtifactID)["Other paragraph."]
	ofIssue := &model.NodeParent{Kind: "issue", ID: issue.Key, Ref: issueRef}
	ofSpec := &model.NodeParent{Kind: "artifact", ID: issue.PrimaryArtifactID, Ref: specRef}

	// editedAsk opens an ask as the session and rewords it as alice, checking its facts before and
	// after; it returns the ask's id, the edit's edited_at and the ask's facts after the edit.
	editedAsk := func(question string) (string, string, nodeFacts) {
		t.Helper()
		ask := decodeBody[model.Ask](t, created(t, "a session's ask", sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
			"question": question, "options": []map[string]string{{"label": "HTTP"}, {"label": "MCP"}}, "actor": sessionActor(),
		})))
		askRef := issueRef + "/ask/" + ask.ID
		requireFacts(t, "the ask as written", readNode(t, handler, askRef), nodeFacts{MadeBy: session, MadeAt: &ask.CreatedAt, ChangedBy: session, ChangedAt: &ask.CreatedAt, Parent: ofIssue})
		edit := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+ask.ID, map[string]any{"question": question + " Reworded."}, "alice")
		succeeded(t, "edit the ask as alice", edit)
		edited := decodeBody[struct {
			EditedAt *string `json:"edited_at"`
		}](t, edit)
		var editedAt time.Time
		if err := database.Pool.QueryRow(ctx, `select edited_at from asks where id = $1`, ask.ID).Scan(&editedAt); err != nil || edited.EditedAt == nil {
			t.Fatalf("read the ask's edit: %v (edited_at %v)", err, edited.EditedAt)
		}
		if !editedAt.After(ask.CreatedAt) {
			t.Fatalf("the edit at %s is not after the ask was made at %s", editedAt, ask.CreatedAt)
		}
		afterEdit := nodeFacts{MadeBy: session, MadeAt: &ask.CreatedAt, ChangedBy: alice, ChangedAt: &editedAt, Parent: ofIssue}
		requireFacts(t, "the ask alice edited", readNode(t, handler, askRef), afterEdit)
		return ask.ID, *edited.EditedAt, afterEdit
	}
	askRef := func(id string) string { return issueRef + "/ask/" + id }
	answered, editedAt, afterEdit := editedAsk("Which implementation?")
	succeeded(t, "answer the ask", dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+answered+"/answer", map[string]any{
		"selected": []string{"HTTP"}, "expected_edited_at": editedAt,
	}, "bob"))
	requireFacts(t, "the answered ask", readNode(t, handler, askRef(answered)), afterEdit)
	resolved, _, afterEdit := editedAsk("Which transport?")
	succeeded(t, "resolve the ask", dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+resolved+"/resolve", map[string]any{
		"kind": "resolved", "reason": "Decided.",
	}, "bob"))
	requireFacts(t, "the resolved ask", readNode(t, handler, askRef(resolved)), afterEdit)

	comment := createReferenceAPIComment(t, handler, issue.Key, "A comment.")
	commentRef := issueRef + "/comment/" + comment.ID
	succeeded(t, "edit the comment", dispatchRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+comment.ID, map[string]any{"body": "An edited comment."}, "alice"))
	var commentEditedAt time.Time
	if err := database.Pool.QueryRow(ctx, `select edited_at from comments where id = $1`, comment.ID).Scan(&commentEditedAt); err != nil {
		t.Fatalf("read the comment's edit: %v", err)
	}
	commentFacts := nodeFacts{MadeBy: alice, MadeAt: &comment.CreatedAt, ChangedBy: alice, ChangedAt: &commentEditedAt, Parent: ofIssue}
	requireFacts(t, "the comment alice edited", readNode(t, handler, commentRef), commentFacts)
	if !commentEditedAt.After(comment.CreatedAt) {
		t.Fatalf("the comment's edit at %s is not after it was made at %s", commentEditedAt, comment.CreatedAt)
	}
	succeeded(t, "resolve the comment as bob", dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/resolve", nil, "bob"))
	requireFacts(t, "the comment bob resolved", readNode(t, handler, commentRef), commentFacts)

	unclaimed := readNode(t, handler, issueRef)
	succeeded(t, "claim the issue", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/claim", map[string]any{}, "bob"))
	requireSameNode(t, "the claimed issue", readNode(t, handler, issueRef), unclaimed)

	other := readNode(t, handler, otherRef)
	v1At, specMadeAt := versionOf(t, database, issue.PrimaryArtifactID, 1).CreatedAt, artifactCreatedAt(t, database, issue.PrimaryArtifactID)
	requireFacts(t, "the block bob will not edit", other, nodeFacts{
		MadeBy: alice, MadeAt: &v1At, ChangedBy: alice, ChangedAt: &v1At, Parent: ofSpec, ParentVersion: new(1),
	})
	v2 := editIssueDocumentAs(t, handler, issue.Key, "spec", "bob", []map[string]any{{"op": "replace", "find": "Ship it.", "with": "Ship it now."}})
	v2At := versionOf(t, database, issue.PrimaryArtifactID, v2.Number).CreatedAt
	requireFacts(t, "the edited spec", readNode(t, handler, specRef), nodeFacts{MadeBy: alice, MadeAt: &specMadeAt, ChangedBy: bob, ChangedAt: &v2At, Parent: ofIssue})
	requireFacts(t, "the edited block", readNode(t, handler, blockRef), nodeFacts{
		MadeBy: alice, MadeAt: &v1At, ChangedBy: bob, ChangedAt: &v2At, Parent: ofSpec, ParentVersion: &v2.Number,
	})
	requireSameNode(t, "the block bob did not edit", readNode(t, handler, otherRef), other)
}

// R12: a block Dispatch first saw when it indexed its document, as the owed pass indexes a
// document written before blocks shipped, has no maker on record: version 3, where it first
// appears, need not be where it was made. Its last change is that version. A block a later
// version adds names that version's author as its maker.
func TestABlockSeenFirstAtItsDocumentsFirstIndexingHasNoMaker(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "First paragraph.\n\nSecond paragraph.\n")
	spec := issue.PrimaryArtifactID
	specRef := "dispatch://" + issue.Key + "/spec"
	editSpec(t, handler, issue.Key, []map[string]any{{"op": "replace", "find": "First paragraph.", "with": "First paragraph, revised."}})
	v3 := editIssueDocumentAs(t, handler, issue.Key, "spec", "bob", []map[string]any{{"op": "replace", "find": "Second paragraph.", "with": "Second paragraph, revised."}})
	if v3.Number != 3 {
		t.Fatalf("the third write is version %d, want 3", v3.Number)
	}

	// The state 0121 left every older document in: no block on record, owed an index.
	for _, statement := range []string{
		`delete from block_refs where artifact_id = $1`,
		`delete from document_blocks where artifact_id = $1`,
		`delete from document_block_texts where artifact_id = $1`,
		`delete from document_indexed_versions where artifact_id = $1`,
		`update document_block_versions set version = null where artifact_id = $1`,
	} {
		if _, err := database.Pool.Exec(ctx, statement, spec); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if _, err := deps.Docs.(*docs.Service).ReconcileBlockIndex(ctx, "", nil); err != nil {
		t.Fatalf("reconcile the block index: %v", err)
	}
	requireIndexedAt(t, database, spec, 3)

	v3At := versionOf(t, database, spec, 3).CreatedAt
	ofSpec := &model.NodeParent{Kind: "artifact", ID: spec, Ref: specRef}
	ids := blockIDsByText(t, handler, spec)
	for _, paragraph := range []string{"First paragraph, revised.", "Second paragraph, revised."} {
		node := readNode(t, handler, specRef+"#b-"+ids[paragraph])
		if node.ChangedVersion != 3 {
			t.Fatalf("%q changed_version = %d, want 3", paragraph, node.ChangedVersion)
		}
		requireFacts(t, paragraph, node, nodeFacts{MadeBy: []string{}, ChangedBy: []string{"user:bob"}, ChangedAt: &v3At, Parent: ofSpec, ParentVersion: new(3)})
	}

	v4 := editSpec(t, handler, issue.Key, []map[string]any{{"op": "insert", "after": "end", "markdown": "Third paragraph."}})
	v4At := versionOf(t, database, spec, v4.Number).CreatedAt
	added := readNode(t, handler, specRef+"#b-"+blockIDsByText(t, handler, spec)["Third paragraph."])
	requireFacts(t, "the block version 4 added", added, nodeFacts{
		MadeBy: []string{"user:alice"}, MadeAt: &v4At, ChangedBy: []string{"user:alice"}, ChangedAt: &v4At, Parent: ofSpec, ParentVersion: &v4.Number,
	})
}

// R12 for a pinned block reference: a pin to a current block carries that block's facts; a pin to
// a block no current version holds is made as the block was, last changed by the version that
// removed it, and part of the latest version indexed before that, the last known to hold it.
func TestAPinnedBlockCarriesItsFacts(t *testing.T) {
	handler, database := blockIndexHandler(t)
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "CORE")
	issue, blockRef := createShipItSpec(t, handler, "CORE")
	spec := issue.PrimaryArtifactID
	ofSpec := &model.NodeParent{Kind: "artifact", ID: spec, Ref: "dispatch://" + issue.Key + "/spec"}
	v2 := editIssueDocumentAs(t, handler, issue.Key, "spec", "bob", []map[string]any{{"op": "replace", "find": "Ship it.", "with": "Ship it now."}})
	v1At, v2At := versionOf(t, database, spec, 1).CreatedAt, versionOf(t, database, spec, v2.Number).CreatedAt

	current := readNode(t, handler, blockRef)
	pinned := readNode(t, handler, blockRef+"@v1")
	if pinned.Pin == nil || pinned.Pin.State != "changed" {
		t.Fatalf("the pin to version 1 = %#v, want changed", pinned.Pin)
	}
	requireSameNode(t, "the pin to the current block", pinned, current)
	requireFacts(t, "the current block", current, nodeFacts{
		MadeBy: []string{"user:alice"}, MadeAt: &v1At, ChangedBy: []string{"user:bob"}, ChangedAt: &v2At, Parent: ofSpec, ParentVersion: &v2.Number,
	})

	// Version 3 is one an older build wrote and no block-aware build indexed.
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		select artifact_id, 3, markdown, authors from artifact_versions where artifact_id = $1 and number = $2
	`, spec, v2.Number); err != nil {
		t.Fatalf("write a version as an older build would: %v", err)
	}
	removed := editIssueDocumentAs(t, handler, issue.Key, "spec", "bob", []map[string]any{{"op": "delete", "block": "ship_it"}})
	if removed.Number != 4 {
		t.Fatalf("the deletion is version %d, want 4", removed.Number)
	}
	removedAt := versionOf(t, database, spec, removed.Number).CreatedAt
	pin := readNode(t, handler, blockRef+"@v2")
	if pin.Pin == nil || pin.Pin.State != "removed" || pin.Pin.RemovedVersion != removed.Number {
		t.Fatalf("the pin to version 2 = %#v, want removed in version %d", pin.Pin, removed.Number)
	}
	requireFacts(t, "the pin to the removed block", pin, nodeFacts{
		MadeBy: []string{"user:alice"}, MadeAt: &v1At, ChangedBy: []string{"user:bob"}, ChangedAt: &removedAt, Parent: ofSpec, ParentVersion: &v2.Number,
	})
}

// R12: a block ask belongs to its document at the version that added its block, the version
// current when it was written. A later edit of the document and the ask's answer, which each move
// the block's changed version, leave it there.
func TestABlockAskBelongsToTheVersionThatAddedIt(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "Context paragraph.\n")
	spec := issue.PrimaryArtifactID
	specRef := "dispatch://" + issue.Key + "/spec"
	added := editSpec(t, handler, issue.Key, []map[string]any{{
		"op": "insert", "after": "end",
		"markdown": ":::ask{#decide urgency=\"med\" multiple=\"false\"}\nShip the decision?\n\n- Ship: Now.\n- Hold: Later.\n:::\n",
	}})
	awaitIndexedAskBlock(t, handler, spec, "decide", "Ship the decision?")
	var askID string
	if err := database.Pool.QueryRow(ctx, `select id::text from asks where block_artifact_id = $1 and block_id = 'decide'`, spec).Scan(&askID); err != nil {
		t.Fatalf("read the block ask: %v", err)
	}
	askRef := "dispatch://" + issue.Key + "/ask/" + askID
	blockRef := specRef + "#b-decide"
	want := func(what string) {
		t.Helper()
		node := readNode(t, handler, askRef)
		if !reflect.DeepEqual(node.Parent, &model.NodeParent{Kind: "artifact", ID: spec, Ref: specRef}) || node.ParentVersion == nil || *node.ParentVersion != added.Number {
			t.Fatalf("%s: the block ask's parent = %s, want the spec at version %d", what, factsOf(node), added.Number)
		}
	}
	want("as written")

	before := readNode(t, handler, blockRef).ChangedVersion
	editSpec(t, handler, issue.Key, []map[string]any{{"op": "insert", "after": "end", "markdown": "Closing paragraph."}})
	want("after a later edit")
	ask := decodeBody[struct {
		Ask model.Ask `json:"ask"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+askID, nil, "alice")).Ask
	succeeded(t, "answer the block ask", dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
		"selected": []string{"Ship"}, "expected_edited_at": ask.EditedAt,
	}, "alice"))
	deadline := time.Now().Add(10 * time.Second)
	for readNode(t, handler, blockRef).ChangedVersion <= before {
		if time.Now().After(deadline) {
			t.Fatalf("the answer never moved the block's changed version past %d", before)
		}
		time.Sleep(20 * time.Millisecond)
	}
	want("after its answer")
}

// R12: an actor column holding JSON null is nobody recorded, and one holding any other value that
// does not decode as its column's shape (one actor, or a list of them) fails the read with a 500,
// never a node made by nobody recorded.
func TestAnActorThatDoesNotDecodeFailsTheRead(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "Spec paragraph.\n")
	issueRef, specRef := "dispatch://"+issue.Key, "dispatch://"+issue.Key+"/spec"
	comment := decodeBody[model.Comment](t, created(t, "comment on the issue", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "A comment.",
	}, "alice")))
	commentRef := issueRef + "/comment/" + comment.ID
	exec := func(what, statement string, args ...any) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, statement, args...); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	fails := func(what, ref string) {
		t.Helper()
		query := url.Values{"to": {ref}}
		if response := graphRequest(t, handler, query); response.Code != http.StatusInternalServerError {
			t.Fatalf("%s: GET /api/v1/references?%s: status=%d body=%s, want 500", what, query.Encode(), response.Code, response.Body.String())
		}
	}

	exec("store a JSON null creator", `update issues set created_by = 'null'::jsonb where key = $1`, issue.Key)
	node := readNode(t, handler, issueRef)
	if node.MadeBy == nil || len(node.MadeBy) != 0 {
		t.Fatalf("the issue whose creator is JSON null is made by %#v, want []", node.MadeBy)
	}
	if got := actorNames(node.ChangedBy); !slices.Equal(got, []string{"user:alice"}) {
		t.Fatalf("the issue whose creator is JSON null was last changed by %v, want [user:alice]", got)
	}

	for _, author := range []string{`"not an actor"`, `[1]`, `[{"kind": "user", "id": "alice"}]`} {
		exec("store comment author "+author, `update comments set author = $1::jsonb where id = $2`, author, comment.ID)
		fails("a comment whose author is "+author, commentRef)
	}
	for _, authors := range []string{`{"kind": "user", "id": "alice"}`, `[1]`} {
		exec("store spec authors "+authors, `update artifact_versions set authors = $1::jsonb where artifact_id = $2`, authors, issue.PrimaryArtifactID)
		fails("a spec whose latest version's authors are "+authors, specRef)
	}
}
