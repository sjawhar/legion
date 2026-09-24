package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

const maxArtifactBlobSize = 25 << 20

func (s *server) listArtifacts(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	artifacts, err := s.loadArtifacts(r.Context(), s.deps.Store.Pool, r.PathValue("key"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, artifacts)
}

type artifactUploadInput struct {
	name        string
	content     []byte
	contentType string
	summary     string
	supplied    *model.Actor
	inline      bool
	blank       bool
}

type artifactTarget struct {
	IssueKey *string
	Project  string
}

func (target artifactTarget) refPrefix() string {
	if target.IssueKey != nil {
		return *target.IssueKey
	}
	return target.Project
}

type jsonArtifactUpload struct {
	Name    string          `json:"name"`
	Content *string         `json:"content"`
	Primary json.RawMessage `json:"primary"`
	Summary string          `json:"summary"`
	Actor   *model.Actor    `json:"actor"`
}

func (s *server) uploadArtifact(w http.ResponseWriter, r *http.Request) {
	issueKey := r.PathValue("key")
	s.uploadArtifactFor(w, r, artifactTarget{IssueKey: &issueKey})
}

func (s *server) uploadArtifactFor(w http.ResponseWriter, r *http.Request, target artifactTarget) {
	r.Body = http.MaxBytesReader(w, r.Body, maxArtifactBlobSize+(1<<20))
	requestType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		writeError(w, "ARTIFACT_INPUT", http.StatusBadRequest, "invalid artifact input type")
		return
	}

	var input artifactUploadInput
	var ok bool
	switch requestType {
	case "application/json":
		input, ok = s.jsonArtifactUpload(w, r)
	case "multipart/form-data":
		input, ok = s.multipartArtifactUpload(w, r)
	default:
		writeError(w, "ARTIFACT_INPUT", http.StatusBadRequest, "artifact input must be JSON or multipart")
		return
	}
	if !ok {
		return
	}
	actor, ok := s.requireActor(w, r, input.supplied)
	if !ok {
		return
	}
	if input.name == "" {
		writeError(w, "INVALID_ARTIFACT", http.StatusBadRequest, "artifact name is required")
		return
	}
	if input.inline && input.blank {
		writeError(w, "ARTIFACT_INPUT", http.StatusBadRequest, "inline artifact content is required")
		return
	}
	if len(input.content) > maxArtifactBlobSize {
		writeError(w, "CAP_EXCEEDED", http.StatusRequestEntityTooLarge, "artifact blob exceeds 25 MB")
		return
	}
	mediaType, _, err := mime.ParseMediaType(input.contentType)
	if err != nil {
		writeError(w, "INVALID_ARTIFACT", http.StatusBadRequest, "invalid artifact content type")
		return
	}
	s.storeArtifact(w, r, input, actor, artifactKind(mediaType), target)
}

func (s *server) jsonArtifactUpload(w http.ResponseWriter, r *http.Request) (artifactUploadInput, bool) {
	var body jsonArtifactUpload
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeError(w, "CAP_EXCEEDED", http.StatusRequestEntityTooLarge, "artifact blob exceeds 25 MB")
		} else {
			writeError(w, "INVALID_JSON", http.StatusBadRequest, "invalid JSON body")
		}
		return artifactUploadInput{}, false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(w, "INVALID_JSON", http.StatusBadRequest, "request body must contain one JSON value")
		return artifactUploadInput{}, false
	}
	if body.Primary != nil {
		writeError(w, "ARTIFACT_INPUT", http.StatusBadRequest, "primary is fixed at issue creation")
		return artifactUploadInput{}, false
	}
	blank := body.Content == nil || strings.TrimSpace(*body.Content) == ""
	var content []byte
	if body.Content != nil {
		content = []byte(*body.Content)
	}
	return artifactUploadInput{
		name:        strings.TrimSpace(body.Name),
		content:     content,
		contentType: "text/markdown; charset=utf-8",
		summary:     strings.TrimSpace(body.Summary),
		supplied:    body.Actor,
		inline:      true,
		blank:       blank,
	}, true
}

func (s *server) multipartArtifactUpload(w http.ResponseWriter, r *http.Request) (artifactUploadInput, bool) {
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		writeError(w, "CAP_EXCEEDED", http.StatusRequestEntityTooLarge, "artifact blob exceeds 25 MB")
		return artifactUploadInput{}, false
	}
	var supplied *model.Actor
	if raw := r.FormValue("actor"); raw != "" {
		var actor model.Actor
		if err := json.Unmarshal([]byte(raw), &actor); err != nil {
			writeError(w, "INVALID_JSON", http.StatusBadRequest, "invalid multipart actor")
			return artifactUploadInput{}, false
		}
		supplied = &actor
	}
	if _, exists := r.MultipartForm.Value["primary"]; exists {
		writeError(w, "ARTIFACT_INPUT", http.StatusBadRequest, "primary is fixed at issue creation")
		return artifactUploadInput{}, false
	}
	name := strings.TrimSpace(r.FormValue("name"))
	file, header, err := r.FormFile("file")
	if len(r.MultipartForm.Value["content"]) > 0 {
		if err == nil {
			_ = file.Close()
		}
		writeError(w, "ARTIFACT_INPUT", http.StatusBadRequest, "provide either a file or inline JSON content")
		return artifactUploadInput{}, false
	}
	if err != nil {
		writeError(w, "INVALID_ARTIFACT", http.StatusBadRequest, "artifact file is required")
		return artifactUploadInput{}, false
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maxArtifactBlobSize+1))
	if err != nil {
		s.writeHandlerError(w, err)
		return artifactUploadInput{}, false
	}
	contentType := header.Header.Get("Content-Type")
	if contentType == "" {
		contentType = mime.TypeByExtension(path.Ext(name))
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return artifactUploadInput{
		name:        name,
		content:     content,
		contentType: contentType,
		summary:     strings.TrimSpace(r.FormValue("summary")),
		supplied:    supplied,
	}, true
}

func (s *server) storeArtifact(
	w http.ResponseWriter,
	r *http.Request,
	input artifactUploadInput,
	actor model.Actor,
	kind string,
	target artifactTarget,
) {
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// A CRDT replacement cannot be rolled back in memory, so failures after an
	// existing document replacement evict the room after tx.Rollback and reload
	// durable state on the next access (R30).
	evictOnFailure := false
	evictArtifactID := ""
	defer func() {
		if evictOnFailure {
			_ = s.deps.Docs.Evict(r.Context(), evictArtifactID)
		}
	}()
	defer tx.Rollback(r.Context())
	var issueStatus *string
	var project string
	if target.IssueKey != nil {
		issueStatus, err = s.requireOpenOwnerStatus(r.Context(), tx, issueOwner(*target.IssueKey))
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if err := tx.QueryRow(r.Context(), `select project_key from issues where key = $1`, *target.IssueKey).Scan(&project); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	} else {
		loaded, err := s.loadProject(r.Context(), tx, target.Project)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		project = loaded.Key
	}
	var artifact model.Artifact
	var created bool
	artifact, err = s.findArtifactByName(r.Context(), tx, target, input.name)
	if errors.Is(err, pgx.ErrNoRows) {
		created = true
		slug, err := s.nextArtifactSlug(r.Context(), tx, target, input.name)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		actorJSON, err := encodeJSON(actor)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if err := tx.QueryRow(r.Context(), `
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			values ($1, $2, $3, $4, $5, false, $6)
			returning id::text, issue_key, project_key, ref_key, slug, name, kind, is_primary, created_by, created_at
		`, target.IssueKey, project, slug, input.name, kind, actorJSON).Scan(
			&artifact.ID, &artifact.IssueKey, &artifact.Project, &artifact.RefKey, &artifact.Slug, &artifact.Name,
			&artifact.Kind, &artifact.Primary, &actorJSON, &artifact.CreatedAt,
		); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if err := json.Unmarshal(actorJSON, &artifact.CreatedBy); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	} else if err != nil {
		s.writeHandlerError(w, err)
		return
	} else if artifact.Kind != kind {
		writeError(w, "ARTIFACT_KIND_MISMATCH", http.StatusBadRequest, "uploaded content type does not match existing artifact")
		return
	}
	// A project document's owner row is the artifact itself, and until here this transaction
	// has not locked it: the issue branch above locks its issue, but this one only read its
	// project. Every writer that takes the owner row at all takes it before the room lock -
	// the durable writers never take it - and the event this upload appends takes it after the
	// document write has taken the room. Without this line the upload ran room -> owner against
	// a settlement's owner -> room, and Postgres broke the cycle with a 500.
	if target.IssueKey == nil && !created {
		if err := s.requireOpenOwner(r.Context(), tx, ownerForArtifact(artifact)); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}

	var nextNumber int
	if err := tx.QueryRow(r.Context(), `select coalesce(max(number), 0) + 1 from artifact_versions where artifact_id = $1`, artifact.ID).Scan(&nextNumber); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	authors, err := encodeJSON([]model.Actor{actor})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var version model.Version
	var versionAuthors []byte
	checksum := sha256.Sum256(input.content)
	sha := hex.EncodeToString(checksum[:])
	var summaryValue any
	if input.summary != "" {
		summaryValue = input.summary
	}
	var documentEvents *docs.EventCollector
	var documentMarkdown string
	var documentChanges model.ReferenceChanges
	if kind == "doc" {
		ctx, collector := documentMutationContext(r.Context(), tx)
		documentEvents = collector
		if created {
			documentMarkdown, err = s.deps.Docs.SeedText(ctx, tx, artifact.ID, string(input.content), actor)
		} else {
			evictArtifactID = artifact.ID
			evictOnFailure = true
			documentMarkdown, err = s.deps.Docs.ReplaceText(ctx, artifact.ID, string(input.content), actor)
		}
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if err := tx.QueryRow(r.Context(), `
			insert into artifact_versions (artifact_id, number, markdown, authors, named, summary, doc_update_version)
			values ($1, $2, $3, $4, $5, $6, coalesce((select max(version) from doc_updates where artifact_id = $1), 0))
			returning number, named, summary, authors, created_at
		`, artifact.ID, nextNumber, documentMarkdown, authors, input.summary != "", summaryValue).Scan(
			&version.Number, &version.Named, &version.Summary, &versionAuthors, &version.CreatedAt,
		); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		documentChanges, err = s.replaceReferences(r.Context(), tx, "artifact", artifact.ID, documentMarkdown)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
	} else {
		size := len(input.content)
		if err := tx.QueryRow(r.Context(), `
			insert into artifact_versions (artifact_id, number, content, mime, size, sha256, authors, named, summary)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			returning number, named, summary, authors, created_at, size, mime, sha256
		`, artifact.ID, nextNumber, input.content, input.contentType, size, sha, authors, input.summary != "", summaryValue).Scan(
			&version.Number, &version.Named, &version.Summary, &versionAuthors, &version.CreatedAt,
			&version.Size, &version.MIME, &version.SHA256,
		); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if err := json.Unmarshal(versionAuthors, &version.Authors); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var diff *string
	if !created && kind == "doc" {
		diff, err = s.namedVersionDiff(r.Context(), tx, artifact.ID, version)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	// An uploaded document's body cites nodes whose rows carry a backlink count, exactly as a
	// message or comment body does, so both payload shapes name what the upload moved.
	eventType := "artifact.version"
	payload := docs.ArtifactVersionEventPayload(artifact.ID, artifact.Name, version, diff, documentChanges)
	if created {
		eventType = "artifact.created"
		artifact.Versions = []model.Version{version}
		payload = artifactCreatedEventPayload(artifact, documentChanges)
	}
	event, err := s.appendEvent(r.Context(), tx, ownerForArtifact(artifact).event(eventType, actor, payload))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if kind == "doc" {
		if err := refs.Stamp(r.Context(), tx, "artifact", artifact.ID, event.ID); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	var advice *writeAdvice
	if target.IssueKey != nil && issueStatus != nil {
		advice = s.writeAdvice(
			r.Context(), tx, "POST /api/v1/issues/{key}/artifacts", *target.IssueKey, actor, "", *issueStatus,
		)
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	evictOnFailure = false
	if kind == "doc" {
		// The seeded or replaced text was written inside this transaction, which suppresses
		// the live settlement the edits path relies on; queue the closer now so the
		// document's ask blocks are indexed and its block ids repaired.
		s.deps.Docs.ScheduleSettlement(artifact.ID)
	}
	s.publishDocumentEvents(documentEvents, event)
	var decisionBlocks *int
	if kind == "doc" {
		decisionBlocks = countAskBlocks(documentMarkdown)
	}
	if advice != nil {
		advice.DecisionBlocks = decisionBlocks
	}
	responsePayload := map[string]any{"artifact": artifact, "version": version}
	if target.IssueKey != nil {
		WriteJSON(w, http.StatusCreated, withAdvice(responsePayload, advice))
	} else if decisionBlocks != nil {
		WriteJSON(w, http.StatusCreated, withDecisionBlockAdvice(responsePayload, *decisionBlocks))
	} else {
		WriteJSON(w, http.StatusCreated, responsePayload)
	}
}

func (s *server) getArtifact(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	artifact, err := s.loadArtifactForRequest(r.Context(), s.deps.Store.Pool, r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The artifact detail is the artifact. Its inbound edges are the graph's to read
	// (`GET /api/v1/references?to=`, and `GET /api/v1/artifacts/{id}/references` for agents),
	// so this read does not pay for them.
	WriteJSON(w, http.StatusOK, artifact)
}

func (s *server) getArtifactText(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	artifact, err := s.loadArtifactForRequest(r.Context(), s.deps.Store.Pool, r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if artifact.Kind != "doc" {
		writeError(w, "NOT_DOCUMENT", http.StatusBadRequest, "artifact is not a document")
		return
	}
	markdown, token, err := s.deps.Docs.TextWithToken(r.Context(), artifact.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"markdown": markdown, "version": nil, "token": token})
}

func (s *server) getArtifactBlocks(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	artifact, err := s.loadArtifactForRequest(r.Context(), s.deps.Store.Pool, r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if artifact.Kind != "doc" {
		writeError(w, "NOT_DOCUMENT", http.StatusBadRequest, "artifact is not a document")
		return
	}
	blocks, err := s.deps.Docs.Blocks(r.Context(), artifact.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	references, err := s.blockReferences(r.Context(), artifact.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	for index := range blocks {
		counts := references[blocks[index].ID]
		for _, descendantID := range blocks[index].DescendantIDs {
			descendant := references[descendantID]
			counts.Comments += descendant.Comments
			counts.Asks += descendant.Asks
		}
		blocks[index].References = counts
	}
	WriteJSON(w, http.StatusOK, blocks)
}

// blockReferences counts the comments and asks anchored directly to each block.
// getArtifactBlocks adds a table's descendant-cell counts before it serves the
// table block. The predicate is two conjuncts (is not null, then not the empty
// string) rather than nullif(...) is not null so the planner can use the partial
// comments_/asks_anchor_block_id_idx indexes.
func (s *server) blockReferences(ctx context.Context, artifactID string) (map[string]model.BlockReferences, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select anchor_block_id,
		       count(*) filter (where kind = 'comment'),
		       count(*) filter (where kind = 'ask')
		from (
			select anchor->>'block_id' as anchor_block_id, 'comment' as kind
			from comments
			where anchor->>'artifact_id' = $1 and anchor->>'block_id' is not null and anchor->>'block_id' <> ''
			union all
			select anchor->>'block_id' as anchor_block_id, 'ask' as kind
			from asks
			where anchor->>'artifact_id' = $1 and anchor->>'block_id' is not null and anchor->>'block_id' <> ''
		) anchored
		group by anchor_block_id
	`, artifactID)
	if err != nil {
		return nil, fmt.Errorf("list block references: %w", err)
	}
	defer rows.Close()

	references := make(map[string]model.BlockReferences)
	for rows.Next() {
		var blockID string
		var comments, asks int
		if err := rows.Scan(&blockID, &comments, &asks); err != nil {
			return nil, fmt.Errorf("scan block references: %w", err)
		}
		references[blockID] = model.BlockReferences{Comments: comments, Asks: asks}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate block references: %w", err)
	}
	return references, nil
}

func (s *server) getArtifactVersion(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || number < 1 {
		writeError(w, "INVALID_VERSION", http.StatusBadRequest, "version number must be positive")
		return
	}
	artifact, err := s.loadArtifactForRequest(r.Context(), s.deps.Store.Pool, r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if artifact.Kind == "doc" {
		var markdown string
		version, err := s.loadVersion(r.Context(), s.deps.Store.Pool, artifact.ID, number, &markdown)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, struct {
			Markdown string `json:"markdown"`
			model.Version
		}{Markdown: markdown, Version: version})
		return
	}
	var content []byte
	var contentType string
	var sha string
	if err := s.deps.Store.Pool.QueryRow(r.Context(), `
		select content, mime, sha256 from artifact_versions where artifact_id = $1 and number = $2
	`, artifact.ID, number).Scan(&content, &contentType, &sha); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("ETag", sha)
	w.Header().Set("Content-Disposition", "attachment")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

func (s *server) createNamedVersion(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	var input struct {
		Summary string       `json:"summary"`
		Actor   *model.Actor `json:"actor"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	summary := strings.TrimSpace(input.Summary)
	if summary == "" {
		writeError(w, "INVALID_VERSION", http.StatusBadRequest, "named versions require a summary")
		return
	}
	if err := validateArtifactRequestRef(r); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	artifact, err := s.loadArtifactForRequest(r.Context(), tx, r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	eventOwner := ownerForArtifact(artifact)
	if err := s.requireOpenOwner(r.Context(), tx, eventOwner); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if artifact.Kind != "doc" {
		writeError(w, "NOT_DOCUMENT", http.StatusBadRequest, "artifact is not a document")
		return
	}
	documentCtx, documentEvents := documentMutationContext(r.Context(), tx)
	named, err := s.deps.Docs.NamedVersion(documentCtx, artifact.ID, summary, actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	version := named.Version
	diff, err := s.namedVersionDiff(r.Context(), tx, artifact.ID, version)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, eventOwner.event(
		"artifact.version",
		actor,
		docs.ArtifactVersionEventPayload(artifact.ID, artifact.Name, version, diff, named.Changes),
	))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := refs.Stamp(r.Context(), tx, "artifact", artifact.ID, event.ID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.deps.Docs.CommitVersion(artifact.ID, version)
	s.publishDocumentEvents(documentEvents, event)
	WriteJSON(w, http.StatusCreated, version)
}

func (s *server) editArtifact(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	var input struct {
		Ops          []model.EditOp          `json:"ops"`
		Summary      string                  `json:"summary"`
		Precondition *model.EditPrecondition `json:"precondition"`
		Actor        *model.Actor            `json:"actor"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	artifact, err := s.loadArtifactForRequest(r.Context(), s.deps.Store.Pool, r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if artifact.Kind != "doc" {
		writeError(w, "NOT_DOCUMENT", http.StatusBadRequest, "artifact is not a document")
		return
	}
	if input.Precondition != nil {
		release, err := s.deps.Docs.AcquireConditionalEdit(r.Context(), artifact.ID)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		defer release()
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// A CRDT edit cannot be rolled back in memory, so any failure after ApplyOps evicts the
	// room and the next access reloads durable state (R30). Deferred BEFORE tx.Rollback so
	// LIFO order rolls the transaction back first: eviction compacts the room and would
	// otherwise block on the document rows this transaction still locks.
	evictOnFailure := false
	defer func() {
		if evictOnFailure {
			_ = s.deps.Docs.Evict(r.Context(), artifact.ID)
		}
	}()
	defer tx.Rollback(r.Context())
	eventOwner := ownerForArtifact(artifact)
	status, err := s.requireOpenOwnerStatus(r.Context(), tx, eventOwner)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	evictOnFailure = true
	documentCtx, documentEvents := documentMutationContext(r.Context(), tx)
	applied, err := s.deps.Docs.ApplyOps(documentCtx, artifact.ID, input.Ops, actor, input.Precondition)
	if err != nil {
		var preconditionFailed *docs.ErrPreconditionFailed
		var invalidPrecondition *docs.ErrInvalidPrecondition
		if errors.As(err, &preconditionFailed) || errors.As(err, &invalidPrecondition) {
			evictOnFailure = false
		}
		s.writeHandlerError(w, err)
		return
	}
	var written *docs.VersionResult
	var published []model.Event
	if applied > 0 {
		summary := strings.TrimSpace(input.Summary)
		if summary != "" {
			namedVersion, err := s.deps.Docs.NamedVersion(documentCtx, artifact.ID, summary, actor)
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
			written = &namedVersion
		} else {
			snapshot, err := s.deps.Docs.SnapshotVersion(documentCtx, tx, artifact.ID, actor)
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
			if snapshot.Wrote {
				written = &snapshot
			}
		}
		if written != nil {
			var diff *string
			if written.Version.Named {
				diff, err = s.namedVersionDiff(r.Context(), tx, artifact.ID, written.Version)
				if err != nil {
					s.writeHandlerError(w, err)
					return
				}
			}
			event, err := s.appendEvent(r.Context(), tx, eventOwner.event(
				"artifact.version",
				actor,
				docs.ArtifactVersionEventPayload(artifact.ID, artifact.Name, written.Version, diff, written.Changes),
			))
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
			if err := refs.Stamp(r.Context(), tx, "artifact", artifact.ID, event.ID); err != nil {
				s.writeHandlerError(w, err)
				return
			}
			published = append(published, event)
		}
	}
	var advice *writeAdvice
	if artifact.IssueKey != nil && status != nil {
		advice = s.writeAdvice(
			r.Context(), tx, "POST /api/v1/artifacts/{id}/edits", *artifact.IssueKey, actor, "", *status,
		)
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	evictOnFailure = false
	var version *model.Version
	if written != nil {
		version = &written.Version
		s.deps.Docs.CommitVersion(artifact.ID, written.Version)
	}
	if applied > 0 {
		s.deps.Docs.ScheduleSettlement(artifact.ID)
	}
	s.publishDocumentEvents(documentEvents, published...)
	WriteJSON(w, http.StatusOK, withAdvice(map[string]any{"applied": applied, "version": version}, advice))
}

func (s *server) loadArtifacts(ctx context.Context, q queryer, issueKey string) ([]model.Artifact, error) {
	rows, err := q.Query(ctx, `
		select id::text, issue_key, project_key, ref_key, slug, name, kind, is_primary, created_by, created_at
		from artifacts where issue_key = $1 order by created_at, id
	`, issueKey)
	if err != nil {
		return nil, err
	}
	artifacts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.Artifact, error) {
		return scanArtifact(row)
	})
	if err != nil {
		return nil, err
	}
	if artifacts == nil {
		artifacts = []model.Artifact{}
	}
	pointers := make([]*model.Artifact, len(artifacts))
	for index := range artifacts {
		versions, err := s.loadVersions(ctx, q, artifacts[index].ID)
		if err != nil {
			return nil, err
		}
		artifacts[index].Versions = versions
		pointers[index] = &artifacts[index]
	}
	if err := s.attachApprovals(ctx, q, pointers); err != nil {
		return nil, err
	}
	return artifacts, nil
}

func validateArtifactRequestRef(r *http.Request) error {
	if r.PathValue("key") != "" {
		return nil
	}
	_, err := parseArtifactID(r.PathValue("id"))
	return err
}

func parseArtifactID(id string) (string, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return "", errorf(http.StatusNotFound, "ARTIFACT_NOT_FOUND", "artifact not found")
	}
	return parsed.String(), nil
}

func (s *server) loadArtifactForRequest(ctx context.Context, q queryer, r *http.Request) (model.Artifact, error) {
	if key := r.PathValue("key"); key != "" {
		target := artifactTarget{Project: key}
		if strings.HasPrefix(r.URL.Path, "/api/v1/issues/") {
			target = artifactTarget{IssueKey: &key}
		}
		return s.loadArtifactByDocumentReference(ctx, q, target, r.PathValue("slug"))
	}
	return s.loadArtifact(ctx, q, r.PathValue("id"))
}

const documentHintLimit = 8

func (s *server) loadArtifactByDocumentReference(ctx context.Context, q queryer, target artifactTarget, reference string) (model.Artifact, error) {
	artifact, err := s.loadArtifactByRefKey(ctx, q, target.refPrefix()+"/"+reference)
	if err == nil || !isArtifactNotFound(err) {
		return artifact, err
	}
	documents, listErr := s.loadDocumentReferences(ctx, q, target)
	if listErr != nil {
		return model.Artifact{}, listErr
	}
	matches := make([]model.Artifact, 0, 1)
	for index := range documents {
		if documents[index].Name == reference {
			matches = append(matches, documents[index])
		}
	}
	switch len(matches) {
	case 0:
		return model.Artifact{}, documentReferenceError(target, reference, documents)
	case 1:
		return s.loadArtifact(ctx, q, matches[0].ID)
	default:
		return model.Artifact{}, ambiguousDocumentReferenceError(target, reference, matches)
	}
}

func isArtifactNotFound(err error) bool {
	var apiErr *apiError
	return errors.As(err, &apiErr) && apiErr.code == "ARTIFACT_NOT_FOUND"
}

func (s *server) loadDocumentReferences(ctx context.Context, q queryer, target artifactTarget) ([]model.Artifact, error) {
	query := `
		select id::text, issue_key, project_key, ref_key, slug, name, kind, is_primary, created_by, created_at
		from artifacts where issue_key = $1 and kind = 'doc' order by created_at, id`
	value := target.Project
	if target.IssueKey != nil {
		value = *target.IssueKey
	} else {
		query = `
			select id::text, issue_key, project_key, ref_key, slug, name, kind, is_primary, created_by, created_at
			from artifacts where project_key = $1 and issue_key is null and kind = 'doc' order by created_at, id`
	}
	rows, err := q.Query(ctx, query, value)
	if err != nil {
		return nil, err
	}
	documents, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.Artifact, error) {
		return scanArtifact(row)
	})
	if err != nil {
		return nil, err
	}
	return documents, nil
}

func documentReferenceError(target artifactTarget, reference string, documents []model.Artifact) error {
	scope := "this project's"
	if target.IssueKey != nil {
		scope = "this issue's"
	}
	return errorf(
		http.StatusNotFound,
		"ARTIFACT_NOT_FOUND",
		`document %q not found by slug; %s documents: %s`,
		reference,
		scope,
		documentReferenceLabels(documents),
	)
}

func ambiguousDocumentReferenceError(target artifactTarget, reference string, documents []model.Artifact) error {
	scope := "project"
	if target.IssueKey != nil {
		scope = "issue"
	}
	return errorf(
		http.StatusBadRequest,
		"ARTIFACT_REFERENCE_AMBIGUOUS",
		`%q names %d documents on this %s; use a slug: %s`,
		reference,
		len(documents),
		scope,
		documentReferenceLabels(documents),
	)
}

func documentReferenceLabels(documents []model.Artifact) string {
	labels := make([]string, 0, min(len(documents), documentHintLimit))
	for index, artifact := range documents {
		if index == documentHintLimit {
			break
		}
		labels = append(labels, fmt.Sprintf("%s (%s)", artifact.Slug, artifact.Name))
	}
	if len(labels) == 0 {
		return "none"
	}
	return strings.Join(labels, ", ")
}

func (s *server) loadArtifact(ctx context.Context, q queryer, id string) (model.Artifact, error) {
	parsed, err := parseArtifactID(id)
	if err != nil {
		return model.Artifact{}, err
	}
	return s.loadArtifactRow(ctx, q, q.QueryRow(ctx, `
		select id::text, issue_key, project_key, ref_key, slug, name, kind, is_primary, created_by, created_at
		from artifacts where id = $1
	`, parsed))
}

func (s *server) loadArtifactByRefKey(ctx context.Context, q queryer, refKey string) (model.Artifact, error) {
	return s.loadArtifactRow(ctx, q, q.QueryRow(ctx, `
		select id::text, issue_key, project_key, ref_key, slug, name, kind, is_primary, created_by, created_at
		from artifacts where ref_key = $1
	`, refKey))
}

func (s *server) loadArtifactRow(ctx context.Context, q queryer, row pgx.Row) (model.Artifact, error) {
	artifact, err := scanArtifact(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Artifact{}, errorf(http.StatusNotFound, "ARTIFACT_NOT_FOUND", "artifact not found")
	}
	if err != nil {
		return model.Artifact{}, err
	}
	versions, err := s.loadVersions(ctx, q, artifact.ID)
	if err != nil {
		return model.Artifact{}, err
	}
	artifact.Versions = versions
	if err := s.attachApproval(ctx, q, &artifact); err != nil {
		return model.Artifact{}, err
	}
	return artifact, nil
}

// scanArtifact reads the canonical artifact column list:
// id::text, issue_key, project_key, ref_key, slug, name, kind, is_primary, created_by, created_at.
func scanArtifact(row pgx.Row) (model.Artifact, error) {
	var artifact model.Artifact
	var createdBy []byte
	if err := row.Scan(
		&artifact.ID, &artifact.IssueKey, &artifact.Project, &artifact.RefKey, &artifact.Slug, &artifact.Name,
		&artifact.Kind, &artifact.Primary, &createdBy, &artifact.CreatedAt,
	); err != nil {
		return model.Artifact{}, err
	}
	if err := json.Unmarshal(createdBy, &artifact.CreatedBy); err != nil {
		return model.Artifact{}, fmt.Errorf("decode artifact author: %w", err)
	}
	return artifact, nil
}

func (s *server) findArtifactByName(ctx context.Context, q queryer, target artifactTarget, name string) (model.Artifact, error) {
	artifact, err := scanArtifact(q.QueryRow(ctx, `
		select id::text, issue_key, project_key, ref_key, slug, name, kind, is_primary, created_by, created_at
		from artifacts where coalesce(issue_key, project_key) = $1 and name = $2
	`, target.refPrefix(), name))
	if err != nil {
		return model.Artifact{}, err
	}
	versions, err := s.loadVersions(ctx, q, artifact.ID)
	if err != nil {
		return model.Artifact{}, err
	}
	artifact.Versions = versions
	return artifact, nil
}

func (s *server) loadVersions(ctx context.Context, q queryer, artifactID string) ([]model.Version, error) {
	rows, err := q.Query(ctx, `
		select number, named, summary, authors, created_at, size, mime, sha256
		from artifact_versions where artifact_id = $1 order by number
	`, artifactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	versions := []model.Version{}
	for rows.Next() {
		var version model.Version
		var authors []byte
		if err := rows.Scan(&version.Number, &version.Named, &version.Summary, &authors, &version.CreatedAt, &version.Size, &version.MIME, &version.SHA256); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(authors, &version.Authors); err != nil {
			return nil, fmt.Errorf("decode version authors: %w", err)
		}
		versions = append(versions, version)
	}
	return versions, rows.Err()
}

func (s *server) loadVersion(ctx context.Context, q queryer, artifactID string, number int, markdown *string) (model.Version, error) {
	var version model.Version
	var authors []byte
	if err := q.QueryRow(ctx, `
		select number, named, summary, authors, created_at, size, mime, sha256, markdown
		from artifact_versions where artifact_id = $1 and number = $2
	`, artifactID, number).Scan(
		&version.Number, &version.Named, &version.Summary, &authors, &version.CreatedAt,
		&version.Size, &version.MIME, &version.SHA256, markdown,
	); err != nil {
		return model.Version{}, err
	}
	if err := json.Unmarshal(authors, &version.Authors); err != nil {
		return model.Version{}, fmt.Errorf("decode version authors: %w", err)
	}
	return version, nil
}

func artifactKind(contentType string) string {
	if contentType == "text/markdown" {
		return "doc"
	}
	if strings.HasPrefix(contentType, "image/") {
		return "image"
	}
	return "file"
}

func artifactSlug(name string) string {
	var slug strings.Builder
	previousDash := false
	for _, r := range strings.ToLower(name) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			slug.WriteRune(r)
			previousDash = false
			continue
		}
		if slug.Len() > 0 && !previousDash {
			slug.WriteByte('-')
			previousDash = true
		}
	}
	result := strings.Trim(slug.String(), "-")
	if result == "" {
		return "artifact"
	}
	return result
}

func (s *server) nextArtifactSlug(ctx context.Context, q queryer, target artifactTarget, name string) (string, error) {
	base := artifactSlug(name)
	for suffix := 1; ; suffix++ {
		candidate := base
		if suffix > 1 {
			candidate = fmt.Sprintf("%s-%d", base, suffix)
		}
		var inUse bool
		if err := q.QueryRow(ctx, `select exists(select 1 from artifacts where coalesce(issue_key, project_key) = $1 and slug = $2)`, target.refPrefix(), candidate).Scan(&inUse); err != nil {
			return "", err
		}
		if !inUse {
			return candidate, nil
		}
	}
}
