package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// conversationSession is the Envoy session the conversation-owned artifact belongs to.
const conversationSession = "session-0123456789abcdef"

// A Dispatch from before migration 0071 reads the rows it stores: an artifact an agent's conversation
// owns has issue_key and project_key both null and ref_key agent/<session id>/<slug>, and every read
// of artifacts.project_key answers an empty project for it rather than failing the scan (the fix
// that landed on main first, sjawhar/legion#1802, so a revert reads such rows). This holds every one
// of those reads, and every route over the row, to that on the row as 0071 stores it; the only other
// no-project shape, a document whose project is nulled, is one artifacts_one_owner refuses.
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
	if err := deps.Docs.Quiesce(ctx); err != nil {
		t.Fatalf("quiesce documents: %v", err)
	}
	// An issue's document keeps its project: the one-owner check refuses the row with none.
	var pgErr *pgconn.PgError
	if _, err := database.Pool.Exec(ctx, `update artifacts set project_key = null where id = $1`, notes.ID); !errors.As(err, &pgErr) || pgErr.ConstraintName != "artifacts_one_owner" {
		t.Fatalf("nulling an issue document's project = %v, want artifacts_one_owner to refuse it", err)
	}

	var photoID, photoRefKey string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (session_id, slug, name, kind, created_by)
		values ($1, 'photo-png', 'photo.png', 'image', jsonb_build_object('kind', 'session', 'id', $1::text))
		returning id::text, ref_key
	`, conversationSession).Scan(&photoID, &photoRefKey); err != nil {
		t.Fatalf("store a conversation's file: %v", err)
	}
	if want := "agent/" + conversationSession + "/photo-png"; photoRefKey != want {
		t.Fatalf("conversation file ref_key = %q, want %q", photoRefKey, want)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, content, mime, size, sha256, authors)
		values ($1, 1, '\x89504e47'::bytea, 'image/png', 4, repeat('0', 64), jsonb_build_array(jsonb_build_object('kind', 'session', 'id', $2::text)))
	`, photoID, conversationSession); err != nil {
		t.Fatalf("store the file's version: %v", err)
	}
	photoRef := "dispatch://agent/" + conversationSession + "/artifact/photo-png"
	cited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+citing+"/comments", map[string]string{
		"body": "See dispatch://" + owner + "/artifact/" + notes.Slug + " and " + photoRef + " for the plan.",
	}, "alice")
	if cited.Code != http.StatusCreated {
		t.Fatalf("comment citing the document and the file: status=%d body=%s", cited.Code, cited.Body.String())
	}
	citation := decodeBody[model.Comment](t, cited)
	// No route writes a comment on a conversation's file (ARTIFACT_AGENT_OWNED); a row one older
	// binary could leave is what the comment, suggestion and search reads are held to.
	const fileComment = "00000000-0000-4000-8000-000000000541"
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (id, artifact_id, author, body)
		values ($1, $2, '{"kind":"user","id":"alice"}', 'Zebraquill wording here.')
	`, fileComment, photoID); err != nil {
		t.Fatalf("insert a comment on the conversation's file: %v", err)
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
	read, err := s.loadArtifact(ctx, database.Pool, photoID)
	wantNoProject("loadArtifact of the conversation's file", read, err, photoID)
	read, err = s.loadArtifactByRefKey(ctx, database.Pool, photoRefKey)
	wantNoProject("loadArtifactByRefKey of the conversation's file", read, err, photoID)
	read, err = s.findArtifactByName(ctx, database.Pool, artifactTarget{Session: conversationSession}, "photo.png")
	wantNoProject("findArtifactByName in the conversation", read, err, photoID)

	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	read, err = s.lockAnchorArtifact(ctx, tx, documentOwner(photoID), photoID)
	wantNoProject("lockAnchorArtifact on the file", read, err, photoID)
	if anchor, err := s.loadAskAnchorArtifact(ctx, tx, photoID); err != nil {
		t.Errorf("loadAskAnchorArtifact: %v", err)
	} else if anchor.Project != "" {
		t.Errorf("loadAskAnchorArtifact project = %q, want none", anchor.Project)
	}
	if payload, err := s.commentEventPayload(ctx, tx, model.Comment{ArtifactID: &photoID}, "", commentEventThread{}, model.ReferenceChanges{}); err != nil {
		t.Errorf("commentEventPayload on the file: %v", err)
	} else if payload.ProjectKey != "" || payload.ArtifactSlug != "photo-png" {
		t.Errorf("commentEventPayload on the file = project %q slug %q, want no project and the slug", payload.ProjectKey, payload.ArtifactSlug)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if project, err := s.resolveSuggestionProject(ctx, suggestionSource{kind: "ask", artifactID: photoID}); err != nil || project != "" {
		t.Errorf("resolveSuggestionProject = %q, %v; want no project and no error", project, err)
	}
	// The search reads a document-owned comment's project through the artifact's row.
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Errorf("search over a comment on the conversation's file panicked: %v", recovered)
			}
		}()
		found, err := s.runFusedSearch(ctx, "zebraquill", "", 10, 0)
		if err != nil {
			t.Errorf("search over a comment on the conversation's file: %v", err)
			return
		}
		if len(found.Results) != 1 || found.Results[0].ID != fileComment || found.Results[0].Owner.Project != "" {
			t.Errorf("search results = %#v, want the comment on the conversation's file with no project", found.Results)
		}
	}()

	for _, path := range []string{
		"/api/v1/artifacts/" + photoID,
		"/api/v1/artifacts/" + photoID + "/references",
		"/api/v1/agents/" + conversationSession + "/artifacts/photo-png",
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

	if node := references("to", photoRef).Node; node.ID != photoID || node.Project != "" || node.Ref != photoRef {
		t.Errorf("node at %s = %#v, want the conversation's file with no project and its address", photoRef, node)
	}
	nodes := map[string]model.GraphNode{}
	for _, edge := range references("from", "dispatch://"+citing+"/comment/"+citation.ID).Edges {
		nodes[edge.Node.ID] = edge.Node
	}
	if node, found := nodes[photoID]; !found || node.Project != "" || node.Ref != photoRef {
		t.Errorf("cited conversation file = %#v (found %t), want no project and its address", node, found)
	}

	closure := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+citing+"/references", nil, "alice")
	if closure.Code != http.StatusOK {
		t.Fatalf("GET issue references: status=%d body=%s", closure.Code, closure.Body.String())
	}
	members := map[string]model.Artifact{}
	for _, member := range decodeBody[model.IssueReferences](t, closure).Members {
		members[member.Artifact.ID] = member.Artifact
	}
	member, found := members[photoID]
	if !found {
		t.Fatalf("issue references of %s lack the conversation's file: %#v", citing, members)
	}
	wantNoProject("issue references member", member, nil, photoID)
}
