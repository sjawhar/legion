package api

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// conversationSession is the Envoy session the simulated conversation-owned artifact belongs to.
const conversationSession = "session-0123456789abcdef"

// allowArtifactsWithNoProject gives the test's own database (storetest.Open clones one per test and
// drops it after) the two changes LEGION-541's migration makes to the artifacts columns main reads:
// project_key takes null, and ref_key stops being generated from coalesce(issue_key, project_key),
// since that migration fills it `agent/<session id>/<slug>` for an artifact an agent's conversation
// owns, whose issue_key and project_key are both null.
func allowArtifactsWithNoProject(t *testing.T, database *store.Store) {
	t.Helper()
	for _, statement := range []string{
		`alter table artifacts alter column project_key drop not null`,
		`alter table artifacts alter column ref_key drop expression`,
	} {
		if _, err := database.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("simulate artifacts with no project (%s): %v", statement, err)
		}
	}
}

// A Dispatch reverted past LEGION-541 still reads the rows that change stored. This test gives its
// database that schema, stores a conversation's file the way that change does (issue_key and
// project_key null, ref_key agent/<session id>/<slug>), and nulls the project of an issue's document
// and of a project document as well: no write path on main produces either, but each is a row with
// no project on an owner main knows, and the project document keeps the comment written on it
// before, the shape a comment on such a row would take (LEGION-541 refuses one on its own rows). Every
// read of artifacts.project_key, and every route over these rows, answers an empty project and no
// error.
func TestArtifactReadsServeARowWithNoProject(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	s := directServer(deps)
	ctx := context.Background()

	owner := createTestIssue(t, handler, "CORE", "Owner")
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "CORE", "title": "Citing",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create citing issue: status=%d body=%s", created.Code, created.Body.String())
	}
	citing := decodeBody[model.Issue](t, created).Key
	uploaded := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+owner+"/artifacts", map[string]string{
		"name": "notes.md", "content": "# Notes\n\nThe plan stands.\n",
	}, "alice")
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload issue document: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	notes := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact
	shared := createProjectDocument(t, handler, "CORE", "Shared notes", "# Shared notes\n")
	cited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+citing+"/comments", map[string]string{
		"body": "See dispatch://" + owner + "/artifact/" + notes.Slug + " for the plan.",
	}, "alice")
	if cited.Code != http.StatusCreated {
		t.Fatalf("comment citing the document: status=%d body=%s", cited.Code, cited.Body.String())
	}
	citation := decodeBody[model.Comment](t, cited)
	commented := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+shared.ID+"/comments", map[string]string{
		"body": "Zebraquill wording here.",
	}, "alice")
	if commented.Code != http.StatusCreated {
		t.Fatalf("comment on the project document: status=%d body=%s", commented.Code, commented.Body.String())
	}
	sharedComment := decodeBody[model.Comment](t, commented)
	// No settlement may run on these documents once their project is gone: each would read the
	// owner through docs' lockArtifactOwner, which TestDocumentOwnerReadsServeARowWithNoProject holds.
	if err := deps.Docs.Quiesce(ctx); err != nil {
		t.Fatalf("quiesce documents: %v", err)
	}

	allowArtifactsWithNoProject(t, database)
	photoRefKey := "agent/" + conversationSession + "/photo-png"
	var photoID string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (slug, name, kind, created_by, ref_key)
		values ('photo-png', 'photo.png', 'image', jsonb_build_object('kind', 'session', 'id', $1::text), $2)
		returning id::text
	`, conversationSession, photoRefKey).Scan(&photoID); err != nil {
		t.Fatalf("store a conversation's file: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, content, mime, size, sha256, authors)
		values ($1, 1, '\x89504e47'::bytea, 'image/png', 4, repeat('0', 64), jsonb_build_array(jsonb_build_object('kind', 'session', 'id', $2::text)))
	`, photoID, conversationSession); err != nil {
		t.Fatalf("store the file's version: %v", err)
	}
	// The citing comment names the conversation's file as well, as a message linking a picture would.
	if _, err := database.Pool.Exec(ctx, `
		insert into refs (from_kind, from_id, to_kind, to_id) values ('comment', $1, 'artifact', $2)
	`, citation.ID, photoRefKey); err != nil {
		t.Fatalf("cite the conversation's file: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `update artifacts set project_key = null where id = any($1::uuid[])`,
		[]string{notes.ID, shared.ID}); err != nil {
		t.Fatalf("null the documents' project: %v", err)
	}

	wantNoProject := func(read string, artifact model.Artifact, err error, wantID string) {
		t.Helper()
		if err != nil {
			t.Errorf("%s: %v", read, err)
			return
		}
		if artifact.ID != wantID || artifact.Project != "" {
			t.Errorf("%s = id %q project %q, want id %q and no project", read, artifact.ID, artifact.Project, wantID)
		}
	}
	read, err := s.loadArtifact(ctx, database.Pool, notes.ID)
	wantNoProject("loadArtifact of the issue document", read, err, notes.ID)
	read, err = s.loadArtifact(ctx, database.Pool, photoID)
	wantNoProject("loadArtifact of the conversation's file", read, err, photoID)
	read, err = s.loadArtifactByRefKey(ctx, database.Pool, notes.RefKey)
	wantNoProject("loadArtifactByRefKey of the issue document", read, err, notes.ID)
	read, err = s.loadArtifactByRefKey(ctx, database.Pool, photoRefKey)
	wantNoProject("loadArtifactByRefKey of the conversation's file", read, err, photoID)
	read, err = s.findArtifactByName(ctx, database.Pool, artifactTarget{IssueKey: &owner}, notes.Name)
	wantNoProject("findArtifactByName of the issue document", read, err, notes.ID)
	listed, err := s.loadArtifacts(ctx, database.Pool, owner)
	if err != nil {
		t.Errorf("loadArtifacts of the owner issue: %v", err)
	}
	documents, err := s.loadDocumentReferences(ctx, database.Pool, artifactTarget{IssueKey: &owner})
	if err != nil {
		t.Errorf("loadDocumentReferences of the owner issue: %v", err)
	}
	for _, group := range [][]model.Artifact{listed, documents} {
		for _, artifact := range group {
			if artifact.ID == notes.ID {
				wantNoProject("the owner issue's artifact list", artifact, nil, notes.ID)
			}
		}
	}

	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	read, err = s.lockAnchorArtifact(ctx, tx, issueOwner(owner), notes.Slug)
	wantNoProject("lockAnchorArtifact on the issue", read, err, notes.ID)
	read, err = s.lockAnchorArtifact(ctx, tx, documentOwner(photoID), photoID)
	wantNoProject("lockAnchorArtifact on the artifact", read, err, photoID)
	for _, id := range []string{notes.ID, photoID} {
		anchor, err := s.loadAskAnchorArtifact(ctx, tx, id)
		if err != nil {
			t.Errorf("loadAskAnchorArtifact(%s): %v", id, err)
		} else if anchor.Project != "" {
			t.Errorf("loadAskAnchorArtifact(%s) project = %q, want none", id, anchor.Project)
		}
	}
	for _, id := range []string{shared.ID, photoID} {
		payload, err := s.commentEventPayload(ctx, tx, model.Comment{ArtifactID: &id}, "", commentEventThread{}, model.ReferenceChanges{})
		if err != nil {
			t.Errorf("commentEventPayload on %s: %v", id, err)
		} else if payload.ProjectKey != "" || payload.ArtifactSlug == "" {
			t.Errorf("commentEventPayload on %s = project %q slug %q, want no project and the slug", id, payload.ProjectKey, payload.ArtifactSlug)
		}
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if project, err := s.resolveSuggestionProject(ctx, suggestionSource{kind: "ask", artifactID: shared.ID}); err != nil || project != "" {
		t.Errorf("resolveSuggestionProject = %q, %v; want no project and no error", project, err)
	}
	// The search reads a document-owned comment's project through the document's row.
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Errorf("search over a comment on a document with no project panicked: %v", recovered)
			}
		}()
		found, err := s.runFusedSearch(ctx, "zebraquill", "", 10, 0)
		if err != nil {
			t.Errorf("search over a comment on a document with no project: %v", err)
			return
		}
		if len(found.Results) != 1 || found.Results[0].ID != sharedComment.ID || found.Results[0].Owner.Project != "" {
			t.Errorf("search results = %#v, want the comment on the document with no project", found.Results)
		}
	}()

	for _, path := range []string{
		"/api/v1/artifacts/" + notes.ID,
		"/api/v1/artifacts/" + photoID,
		"/api/v1/artifacts/" + notes.ID + "/references",
		"/api/v1/artifacts/" + photoID + "/references",
		"/api/v1/issues/" + owner,
		"/api/v1/issues/" + owner + "/artifacts",
	} {
		if response := dispatchRequest(t, handler, http.MethodGet, path, nil, "alice"); response.Code != http.StatusOK {
			t.Errorf("GET %s: status=%d body=%s", path, response.Code, response.Body.String())
		}
	}

	references := func(parameter, ref string) model.GraphReferences {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/references?"+url.Values{parameter: {ref}}.Encode(), nil, "alice")
		if response.Code != http.StatusOK {
			t.Errorf("GET references %s=%s: status=%d body=%s", parameter, ref, response.Code, response.Body.String())
			return model.GraphReferences{}
		}
		return decodeBody[model.GraphReferences](t, response)
	}
	notesRef := "dispatch://" + owner + "/artifact/" + notes.Slug
	if node := references("to", notesRef).Node; node.ID != notes.ID || node.Project != "" || node.Ref != notesRef {
		t.Errorf("node at %s = %#v, want the issue document with no project", notesRef, node)
	}
	nodes := map[string]model.GraphNode{}
	for _, edge := range references("from", "dispatch://"+citing+"/comment/"+citation.ID).Edges {
		nodes[edge.Node.ID] = edge.Node
	}
	if node, found := nodes[notes.ID]; !found || node.Project != "" || node.Ref != notesRef {
		t.Errorf("cited issue document = %#v (found %t), want no project and its address", node, found)
	}
	// Main names no address for an artifact whose owner is neither an issue nor a project.
	if node, found := nodes[photoID]; !found || node.Project != "" || node.Ref != "" {
		t.Errorf("cited conversation file = %#v (found %t), want no project and no address", node, found)
	}
	sharedCommentRef := "dispatch://CORE/artifact/" + shared.Slug + "/comment/" + sharedComment.ID
	if node := references("from", sharedCommentRef).Node; node.ID != sharedComment.ID || node.Project != "" || node.Ref != "" {
		t.Errorf("comment on the document with no project = %#v, want no project and no address", node)
	}

	closure := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+citing+"/references", nil, "alice")
	if closure.Code != http.StatusOK {
		t.Fatalf("GET issue references: status=%d body=%s", closure.Code, closure.Body.String())
	}
	members := map[string]model.Artifact{}
	for _, member := range decodeBody[model.IssueReferences](t, closure).Members {
		members[member.Artifact.ID] = member.Artifact
	}
	for _, id := range []string{notes.ID, photoID} {
		member, found := members[id]
		if !found {
			t.Errorf("issue references of %s lack %s: %#v", citing, id, members)
			continue
		}
		wantNoProject("issue references member", member, nil, id)
	}
}
