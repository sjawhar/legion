package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// Data grants: which data files one page file may read (dispatch://INFRA-682, "Data grants").
// A view block names a page file and the data files it draws, but a block is markdown any
// author writes, an agent included, so the block never decides what the page is given. The
// grants do, they hang from the page file rather than from the block, and only a person writes
// one: both writes here are authHuman and call requireHuman before they read a body, so every
// token — shared, personal, service account and broker — gets 403 HUMAN_ONLY and changes
// nothing.
//
// A grant is access and not content, so a person may change the grants of a page file on a
// closed issue: blocks elsewhere still show that page. A grant covers every version of the page
// file, so a new upload of the page's code keeps its grants.

// grantReferencePin is the `@v<N>` a grant's reference may end in. text.ParseRef reads a
// document reference's version and does not carry it (parseArtifactSlug drops it), so the pin is
// cut here and read here, and what is left is parsed as the plain file reference it is.
var grantReferencePin = regexp.MustCompile(`@v([1-9][0-9]*)$`)

// artifactGrant is one grant as the panel on a page file's view lists it: the grant's own id, for
// the delete, and the data file it covers, by id, address and name, with the version it is pinned
// to (null for every version of that file), who granted it and when.
type artifactGrant struct {
	ID         string    `json:"id"`
	ArtifactID string    `json:"artifact_id"`
	Ref        string    `json:"ref"`
	Name       string    `json:"name"`
	Version    *int      `json:"version"`
	GrantedBy  string    `json:"granted_by"`
	CreatedAt  time.Time `json:"created_at"`
}

// artifactGrantsChangedPayload is the wire payload of artifact.grants_changed: the page file
// whose grants changed. It names no grant, because every reader of it re-reads the list — a
// block applies the whole list to the files its attributes name, and the panel shows the whole
// list — so a payload naming one grant would be read by nobody and would have to stay true of a
// row a later change removed.
type artifactGrantsChangedPayload struct {
	ArtifactID string `json:"artifact_id"`
}

// grantArtifact is an artifact as the grants routes read it: enough to decide whether it is a
// page file, to own the event, and to write its address. MIME is the stored type of its latest
// version, "" for an artifact with no version, and the page test reads it with the name
// (isPageFile).
type grantArtifact struct {
	ID        string
	IssueKey  *string
	Project   string
	SessionID *string
	Slug      string
	Name      string
	Kind      string
	Primary   bool
	MIME      string
}

// owner is the collaboration owner the page file's event hangs from: its issue for an issue page
// file, so the change shows on that issue's log, and the file itself for a project page file,
// whose event carries the project with no issue key.
func (a grantArtifact) owner() owner {
	if a.IssueKey != nil {
		return issueOwner(*a.IssueKey)
	}
	return documentOwner(a.ID)
}

// ref is the address a reference to this artifact writes.
func (a grantArtifact) ref() string {
	return refs.ArtifactRef(a.IssueKey, a.Project, a.SessionID, a.Slug, a.Kind, a.Primary)
}

// ownedByAnAgentConversation reports whether the artifact belongs to an agent's conversation
// rather than to an issue or a project: it carries a session and neither key, which is the third
// arm of artifacts_one_owner (0071_agent_artifacts.up.sql). Such a file's uploads write no event,
// so nothing could redraw it, and it is neither a page a view block shows nor data it is given.
func (a grantArtifact) ownedByAnAgentConversation() bool {
	return a.SessionID != nil && a.IssueKey == nil && a.Project == ""
}

// isPage reports whether the artifact is a page file a view block can show: an issue's or a
// project's file whose stored type is text/html or whose name ends .html or .htm. A document is
// never one — its text is markdown Dispatch rewrites, not the bytes an author sent — and nor is
// an agent conversation's file.
func (a grantArtifact) isPage() bool {
	if a.Kind == "doc" || a.ownedByAnAgentConversation() {
		return false
	}
	return isPageFile(a.Name, a.MIME)
}

// grantArtifactColumns reads an artifact for these routes, over artifacts aliased a: the latest
// version's stored type comes from one backwards probe of artifact_versions' (artifact_id,
// number) key, as the summary reads its size.
const grantArtifactColumns = `a.id::text, a.issue_key, coalesce(a.project_key, ''), a.session_id,
	a.slug, a.name, a.kind, a.is_primary,
	coalesce((select v.mime from artifact_versions v where v.artifact_id = a.id order by v.number desc limit 1), '')`

func scanGrantArtifact(row pgx.Row) (grantArtifact, error) {
	var artifact grantArtifact
	err := row.Scan(&artifact.ID, &artifact.IssueKey, &artifact.Project, &artifact.SessionID,
		&artifact.Slug, &artifact.Name, &artifact.Kind, &artifact.Primary, &artifact.MIME)
	if errors.Is(err, pgx.ErrNoRows) {
		return grantArtifact{}, errorf(http.StatusNotFound, "ARTIFACT_NOT_FOUND", "artifact not found")
	}
	if err != nil {
		return grantArtifact{}, err
	}
	return artifact, nil
}

// pageFileOfRequest is the page file {id} names, for all three routes: a grant list belongs to a
// page file and to nothing else, so an artifact that is not one is refused here rather than
// answered an empty list it could never fill.
func (s *server) pageFileOfRequest(ctx context.Context, q queryer, r *http.Request) (grantArtifact, error) {
	id, err := parseArtifactID(r.PathValue("id"))
	if err != nil {
		return grantArtifact{}, err
	}
	artifact, err := scanGrantArtifact(q.QueryRow(ctx, `
		select `+grantArtifactColumns+` from artifacts a where a.id = $1
	`, id))
	if err != nil {
		return grantArtifact{}, err
	}
	if !artifact.isPage() {
		return grantArtifact{}, errorf(http.StatusUnsupportedMediaType, "NOT_A_PAGE", "this artifact is not a page file")
	}
	return artifact, nil
}

// readArtifactGrants lists one page file's grants, oldest first, as artifact_grants_page holds
// them.
func (s *server) readArtifactGrants(ctx context.Context, q queryer, pageID string) ([]artifactGrant, error) {
	rows, err := q.Query(ctx, `
		select g.id::text, `+grantArtifactColumns+`, g.version, g.granted_by, g.created_at
		from artifact_grants g join artifacts a on a.id = g.data_artifact_id
		where g.page_artifact_id = $1
		order by g.created_at, g.id
	`, pageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	grants := []artifactGrant{}
	for rows.Next() {
		var grant artifactGrant
		var data grantArtifact
		if err := rows.Scan(&grant.ID, &data.ID, &data.IssueKey, &data.Project, &data.SessionID,
			&data.Slug, &data.Name, &data.Kind, &data.Primary, &data.MIME,
			&grant.Version, &grant.GrantedBy, &grant.CreatedAt); err != nil {
			return nil, err
		}
		grant.ArtifactID, grant.Ref, grant.Name = data.ID, data.ref(), data.Name
		grants = append(grants, grant)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return grants, nil
}

// listArtifactGrants is GET /api/v1/artifacts/{id}/grants: the data files a person has granted
// this page file, which is what decides what Dispatch gives the page. Open to every signed-in
// caller, as every artifact read is: the list is not the data.
func (s *server) listArtifactGrants(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	page, err := s.pageFileOfRequest(r.Context(), s.deps.Store.Pool, r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	grants, err := s.readArtifactGrants(r.Context(), s.deps.Store.Pool, page.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, struct {
		Grants []artifactGrant `json:"grants"`
	}{Grants: grants})
}

// resolveGrantedFile reads the file a grant's reference names, with the version it pins, and
// makes every refusal a grant can earn before anything is written. Each is a 400 naming its
// cause, because each is a reference a person can correct:
//
//   - NOT_A_FILE_REFERENCE: a reference to anything but an issue's or a project's file — an
//     issue, an ask, a comment, a message, a component, a block, or no reference at all.
//   - DATA_FILE_NOT_FOUND: no file at that address.
//   - DATA_FILE_IS_A_DOCUMENT: a document, whose text is markdown Dispatch rewrites as edits
//     settle rather than the bytes an agent sent, so it is no data file.
//   - AGENT_CONVERSATION_FILE: a file an agent's conversation owns, whose uploads write no
//     event, so nothing could ever redraw the page from it.
//   - VERSION_NOT_FOUND: a @vN the file does not have, since such a grant could cover nothing.
//
// The file is resolved to its id and the id is what the grant stores, so a later file taking the
// same name is not covered by it.
func (s *server) resolveGrantedFile(ctx context.Context, q queryer, reference string) (grantArtifact, *int, error) {
	reference = strings.TrimSpace(reference)
	var version *int
	if pinned := grantReferencePin.FindStringSubmatch(reference); pinned != nil {
		number, err := strconv.Atoi(pinned[1])
		if err != nil {
			return grantArtifact{}, nil, errorf(http.StatusBadRequest, "NOT_A_FILE_REFERENCE",
				"%q names no version this file could have", reference)
		}
		version = &number
		reference = strings.TrimSuffix(reference, pinned[0])
	}
	ref, ok := text.ParseRef(reference, s.deps.ServerURL)
	if !ok || ref.Kind != "artifact" {
		return grantArtifact{}, nil, errorf(http.StatusBadRequest, "NOT_A_FILE_REFERENCE",
			"a grant names one file, as dispatch://<issue or project>/artifact/<slug>[@v<N>]; %q names none", reference)
	}
	data, err := scanGrantArtifact(q.QueryRow(ctx, `
		select `+grantArtifactColumns+` from artifacts a where a.ref_key = $1
	`, text.TargetID(ref)))
	if err != nil {
		var refusal *apiError
		if errors.As(err, &refusal) && refusal.code == "ARTIFACT_NOT_FOUND" {
			return grantArtifact{}, nil, errorf(http.StatusBadRequest, "DATA_FILE_NOT_FOUND",
				"%s is no file of this workspace", reference)
		}
		return grantArtifact{}, nil, err
	}
	switch {
	case data.Kind == "doc":
		return grantArtifact{}, nil, errorf(http.StatusBadRequest, "DATA_FILE_IS_A_DOCUMENT",
			"%s is a document: its text is markdown Dispatch rewrites, not the bytes an upload sent", reference)
	case data.ownedByAnAgentConversation():
		return grantArtifact{}, nil, errorf(http.StatusBadRequest, "AGENT_CONVERSATION_FILE",
			"%s belongs to an agent's conversation, whose uploads announce nothing a page could redraw from", reference)
	}
	if version != nil {
		var exists bool
		if err := q.QueryRow(ctx, `
			select true from artifact_versions where artifact_id = $1 and number = $2
		`, data.ID, *version).Scan(&exists); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return grantArtifact{}, nil, errorf(http.StatusBadRequest, "VERSION_NOT_FOUND",
					"%s has no version %d, so a grant of it could cover nothing", data.ref(), *version)
			}
			return grantArtifact{}, nil, err
		}
	}
	return data, version, nil
}

// createArtifactGrant is POST /api/v1/artifacts/{id}/grants: a person grants this page file one
// data file, named by a dispatch:// reference. A data file equal to the page file is allowed: it
// costs no code and refuses nothing a person might mean.
//
// A grant this page file already holds is answered 200 with the grant that is there, and writes
// neither a row nor an event: two people adding one file at once each get that one grant, and
// nothing counts as a change twice.
func (s *server) createArtifactGrant(w http.ResponseWriter, r *http.Request) {
	// Before the body, so no token's request is read any further than its credential.
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	page, err := s.pageFileOfRequest(r.Context(), s.deps.Store.Pool, r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var input struct {
		Ref string `json:"ref"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	data, version, err := s.resolveGrantedFile(r.Context(), s.deps.Store.Pool, input.Ref)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	grant := artifactGrant{
		ArtifactID: data.ID, Ref: data.ref(), Name: data.Name, Version: version, GrantedBy: actor.ID,
	}
	err = tx.QueryRow(r.Context(), `
		insert into artifact_grants (page_artifact_id, data_artifact_id, version, granted_by)
		values ($1, $2, $3, $4)
		on conflict on constraint artifact_grants_pair do nothing
		returning id::text, created_at
	`, page.ID, data.ID, version, actor.ID).Scan(&grant.ID, &grant.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// The pair is granted already (the unique key reads two null versions as one), so this
		// request changed nothing and announces nothing.
		existing, err := s.grantOfPair(r.Context(), tx, page.ID, data.ID, version)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		existing.ArtifactID, existing.Ref, existing.Name = data.ID, data.ref(), data.Name
		WriteJSON(w, http.StatusOK, existing)
		return
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, page.owner().event(
		"artifact.grants_changed", actor, artifactGrantsChangedPayload{ArtifactID: page.ID},
	))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(event)
	WriteJSON(w, http.StatusCreated, grant)
}

// grantOfPair reads the grant a repeated add met: the one row the unique key holds for this
// page, file and version.
func (s *server) grantOfPair(ctx context.Context, q queryer, pageID, dataID string, version *int) (artifactGrant, error) {
	var grant artifactGrant
	err := q.QueryRow(ctx, `
		select id::text, version, granted_by, created_at
		from artifact_grants
		where page_artifact_id = $1 and data_artifact_id = $2 and version is not distinct from $3
	`, pageID, dataID, version).Scan(&grant.ID, &grant.Version, &grant.GrantedBy, &grant.CreatedAt)
	return grant, err
}

// deleteArtifactGrant is DELETE /api/v1/artifacts/{id}/grants/{grant_id}: a person takes one data
// file back from this page file. The page keeps running and gets the files the remaining grants
// cover, with nothing for this one.
func (s *server) deleteArtifactGrant(w http.ResponseWriter, r *http.Request) {
	// Before the body, as the add does: a token changes nothing here either.
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	grantID, err := uuid.Parse(r.PathValue("grant_id"))
	if err != nil {
		writeError(w, "GRANT_NOT_FOUND", http.StatusNotFound, "grant not found")
		return
	}
	page, err := s.pageFileOfRequest(r.Context(), s.deps.Store.Pool, r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	removed, err := tx.Exec(r.Context(), `
		delete from artifact_grants where id = $1 and page_artifact_id = $2
	`, grantID, page.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if removed.RowsAffected() == 0 {
		writeError(w, "GRANT_NOT_FOUND", http.StatusNotFound, "grant not found")
		return
	}
	event, err := s.appendEvent(r.Context(), tx, page.owner().event(
		"artifact.grants_changed", actor, artifactGrantsChangedPayload{ArtifactID: page.ID},
	))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(event)
	w.WriteHeader(http.StatusNoContent)
}
