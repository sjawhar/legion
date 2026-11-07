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

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// Data grants: which data files one page file may read (dispatch://INFRA-682, "Data grants").
// A view block names a page file and the data files it draws, but a block is markdown any
// author writes, an agent included, so the block never decides what the page is given. The
// grants do, they hang from the page file rather than from the block, and only a person writes
// one: both writes here hold the caller to the person themselves before they read a body
// (grantingPerson), so no token writes a grant and none changes anything.
//
// A grant is access and not content, so a person may change the grants of a page file on a
// closed issue: blocks elsewhere still show that page. A grant covers every version of the page
// file, so a new upload of the page's code keeps its grants.

// codeHumanOnly refuses a credential that is no person at a browser. It is these two routes'
// own code and no shared mechanism: every other route a person alone may call is decided by
// the scopes its row names (routeScopes), and these two need more than any scope can say,
// since a personal token holding every scope its person holds is still not that person. The
// rows name no scope for that reason — a scope the gate refused would answer a token
// OAUTH_SCOPE_REQUIRED, which reads as "hold this scope and you may", and none may.
const codeHumanOnly = "HUMAN_ONLY"

// grantingPerson is the person at their own browser, the one caller that may write a grant
// (dispatch://INFRA-682, R9). The test is identity and not a property: personCredential is the
// one credential optionalActor's cookie branch returns, the same value for every such request
// (server.go), so a caller carrying it came through that branch and no other. Every other
// branch builds a credential of its own — a personal token's names its id, a broker-issued
// token's the session it proved, the deployment's shared token and a pod's service-account
// token each hold a scopeless one — and none of them is this value, so each is refused 403
// HUMAN_ONLY whatever scopes it holds. A caller with no credential at all is refused with
// them, since nil is not this value either.
//
// What the gate is not: it reads no scope and no empty field. A credential a later release
// gives `person` to, or one that happens to name no token and no session, is still not the
// value the cookie branch returns, so widening the gate takes a second value named here.
//
// optionalActor's second answer, the `human` one this discards, is the same verdict today: it
// is true on the cookie branch and on no other. The test is written against the credential all
// the same, and deliberately: what "human" means is being loosened as this release migrates to
// OAuth scopes, and a gate that read it would widen with it, silently, the day something else
// is allowed to call itself human. The `_` is the discard that says so, not an answer nobody
// had.
func (s *server) grantingPerson(w http.ResponseWriter, r *http.Request) (model.Actor, bool) {
	actor, _, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return model.Actor{}, false
	}
	if actor.Credential != personCredential {
		writeError(w, codeHumanOnly, http.StatusForbidden,
			"only a person at their own browser may change a page file's data grants; no token may, whatever scopes it holds")
		return model.Actor{}, false
	}
	return actor.Recorded(), true
}

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

// grantedFileColumns reads a data file in the grant list's join, over artifacts aliased a.
// It is artifactColumns, in artifactColumns' order, each column named over the alias, because
// the join's other table carries columns of these names too. The reads that join nothing read
// artifactColumns itself.
const grantedFileColumns = `a.id::text, a.issue_key, a.project_key, a.session_id, a.ref_key,
	a.slug, a.name, a.kind, a.is_primary, coalesce(a.role, ''), a.created_by, a.created_at`

// pageFileOfRequest is the page file {id} names, for all three routes: a grant list belongs to a
// page file and to nothing else, so an artifact that is not one is refused here rather than
// answered an empty list it could never fill.
//
// The read selects the latest version's stored type beside the artifact, since that with the
// name is the page test (isPageFile), and it is the only read of these routes that pays for
// that probe: a data file need not be a page, and the list's rows are not asked.
func (s *server) pageFileOfRequest(ctx context.Context, q queryer, r *http.Request) (model.Artifact, error) {
	id, err := parseArtifactID(r.PathValue("id"))
	if err != nil {
		return model.Artifact{}, err
	}
	var storedType string
	artifact, err := scanArtifact(q.QueryRow(ctx, `
		select `+artifactColumns+`, `+latestStoredTypeColumn+` from artifacts where id = $1
	`, id), &storedType)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Artifact{}, errorf(http.StatusNotFound, "ARTIFACT_NOT_FOUND", "artifact not found")
	}
	if err != nil {
		return model.Artifact{}, err
	}
	if !isPageFile(artifact, storedType) {
		return model.Artifact{}, errorf(http.StatusUnsupportedMediaType, "NOT_A_PAGE", "this artifact is not a page file")
	}
	return artifact, nil
}

// readArtifactGrants lists one page file's grants, oldest first, as artifact_grants_page holds
// them.
func (s *server) readArtifactGrants(ctx context.Context, q queryer, pageID string) ([]artifactGrant, error) {
	rows, err := q.Query(ctx, `
		select `+grantedFileColumns+`, g.id::text, g.version, g.granted_by, g.created_at
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
		data, err := scanArtifact(rows, &grant.ID, &grant.Version, &grant.GrantedBy, &grant.CreatedAt)
		if err != nil {
			return nil, err
		}
		grant.ArtifactID, grant.Ref, grant.Name = data.ID, artifactReference(data), data.Name
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
//   - VERSION_NOT_FOUND: a @vN the file does not have, since such a grant could cover nothing,
//     and a @vN past what a version number can be, which no file could ever have.
//
// The file is resolved to its id and the id is what the grant stores, so a later file taking the
// same name is not covered by it.
func (s *server) resolveGrantedFile(ctx context.Context, q queryer, reference string) (model.Artifact, *int, error) {
	reference = strings.TrimSpace(reference)
	var version *int
	if pinned := grantReferencePin.FindStringSubmatch(reference); pinned != nil {
		// A version number is an `integer` column, so a pin past that range is a version no
		// file can have. It is refused here, as a version the file does not have is: sent on,
		// it would reach Postgres as a number the column cannot hold, and the request a person
		// can correct would be answered 500.
		number, err := strconv.ParseInt(pinned[1], 10, 32)
		if err != nil {
			return model.Artifact{}, nil, errorf(http.StatusBadRequest, "VERSION_NOT_FOUND",
				"no file has a version %s, so a grant of it could cover nothing", pinned[1])
		}
		pin := int(number)
		version = &pin
		reference = strings.TrimSuffix(reference, pinned[0])
	}
	ref, ok := text.ParseRef(reference, s.deps.ServerURL)
	if !ok || ref.Kind != "artifact" {
		return model.Artifact{}, nil, errorf(http.StatusBadRequest, "NOT_A_FILE_REFERENCE",
			"a grant names one file, as dispatch://<issue or project>/artifact/<slug>[@v<N>]; %q names none", reference)
	}
	data, err := scanArtifact(q.QueryRow(ctx, `
		select `+artifactColumns+` from artifacts where ref_key = $1
	`, text.TargetID(ref)))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Artifact{}, nil, errorf(http.StatusBadRequest, "DATA_FILE_NOT_FOUND",
			"%s is no file of this workspace", reference)
	}
	if err != nil {
		return model.Artifact{}, nil, err
	}
	switch {
	case data.Kind == "doc":
		return model.Artifact{}, nil, errorf(http.StatusBadRequest, "DATA_FILE_IS_A_DOCUMENT",
			"%s is a document: its text is markdown Dispatch rewrites, not the bytes an upload sent", reference)
	case ownedByAnAgentConversation(data):
		return model.Artifact{}, nil, errorf(http.StatusBadRequest, "AGENT_CONVERSATION_FILE",
			"%s belongs to an agent's conversation, whose uploads announce nothing a page could redraw from", reference)
	}
	if version != nil {
		var exists bool
		if err := q.QueryRow(ctx, `
			select true from artifact_versions where artifact_id = $1 and number = $2
		`, data.ID, *version).Scan(&exists); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return model.Artifact{}, nil, errorf(http.StatusBadRequest, "VERSION_NOT_FOUND",
					"%s has no version %d, so a grant of it could cover nothing", artifactReference(data), *version)
			}
			return model.Artifact{}, nil, err
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
	actor, ok := s.grantingPerson(w, r)
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
		ArtifactID: data.ID, Ref: artifactReference(data), Name: data.Name, Version: version, GrantedBy: actor.ID,
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
		existing.ArtifactID, existing.Ref, existing.Name = data.ID, artifactReference(data), data.Name
		WriteJSON(w, http.StatusOK, existing)
		return
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, ownerForArtifact(page).event(
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
	actor, ok := s.grantingPerson(w, r)
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
	event, err := s.appendEvent(r.Context(), tx, ownerForArtifact(page).event(
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
