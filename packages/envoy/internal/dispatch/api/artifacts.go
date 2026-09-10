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
	writeJSON(w, http.StatusOK, artifacts)
}

type artifactUploadInput struct {
	name        string
	content     []byte
	contentType string
	primary     bool
	summary     string
	supplied    *model.Actor
	inline      bool
	blank       bool
}

type jsonArtifactUpload struct {
	Name    string       `json:"name"`
	Content *string      `json:"content"`
	Primary bool         `json:"primary"`
	Summary string       `json:"summary"`
	Actor   *model.Actor `json:"actor"`
}

func (s *server) uploadArtifact(w http.ResponseWriter, r *http.Request) {
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
	kind := artifactKind(mediaType)
	if input.primary && kind != "doc" {
		writeError(w, "PRIMARY_NOT_DOC", http.StatusBadRequest, "only documents can be primary")
		return
	}
	s.storeArtifact(w, r, input, actor, kind)
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
	blank := body.Content == nil || strings.TrimSpace(*body.Content) == ""
	var content []byte
	if body.Content != nil {
		content = []byte(*body.Content)
	}
	return artifactUploadInput{
		name:        strings.TrimSpace(body.Name),
		content:     content,
		contentType: "text/markdown; charset=utf-8",
		primary:     body.Primary,
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
	primary := false
	if raw := r.FormValue("primary"); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			writeError(w, "INVALID_ARTIFACT", http.StatusBadRequest, "primary must be true or false")
			return artifactUploadInput{}, false
		}
		primary = parsed
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
		primary:     primary,
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
	issueKey := r.PathValue("key")
	if err := s.requireOpenIssue(r.Context(), tx, issueKey); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var artifact model.Artifact
	var created bool
	artifact, err = s.findArtifactByName(r.Context(), tx, issueKey, input.name)
	if errors.Is(err, pgx.ErrNoRows) {
		created = true
		slug, err := s.nextArtifactSlug(r.Context(), tx, issueKey, input.name)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if input.primary {
			if _, err := tx.Exec(r.Context(), `update artifacts set is_primary = false where issue_key = $1 and is_primary`, issueKey); err != nil {
				s.writeHandlerError(w, err)
				return
			}
		}
		actorJSON, err := encodeJSON(actor)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if err := tx.QueryRow(r.Context(), `
			insert into artifacts (issue_key, slug, name, kind, is_primary, created_by)
			values ($1, $2, $3, $4, $5, $6)
			returning id::text, issue_key, slug, name, kind, is_primary, created_by, created_at
		`, issueKey, slug, input.name, kind, input.primary, actorJSON).Scan(
			&artifact.ID, &artifact.IssueKey, &artifact.Slug, &artifact.Name, &artifact.Kind, &artifact.Primary, &actorJSON, &artifact.CreatedAt,
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
	} else {
		if artifact.Kind != kind {
			writeError(w, "ARTIFACT_KIND_MISMATCH", http.StatusBadRequest, "uploaded content type does not match existing artifact")
			return
		}
		if input.primary && !artifact.Primary {
			if _, err := tx.Exec(r.Context(), `update artifacts set is_primary = false where issue_key = $1 and is_primary`, issueKey); err != nil {
				s.writeHandlerError(w, err)
				return
			}
			if _, err := tx.Exec(r.Context(), `update artifacts set is_primary = true where id = $1`, artifact.ID); err != nil {
				s.writeHandlerError(w, err)
				return
			}
			artifact.Primary = true
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
	if kind == "doc" {
		if err := tx.QueryRow(r.Context(), `
			insert into artifact_versions (artifact_id, number, markdown, authors, named, summary)
			values ($1, $2, $3, $4, $5, $6)
			returning number, named, summary, authors, created_at
		`, artifact.ID, nextNumber, string(input.content), authors, input.summary != "", summaryValue).Scan(
			&version.Number, &version.Named, &version.Summary, &versionAuthors, &version.CreatedAt,
		); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		ctx := docs.WithTx(r.Context(), tx)
		if created {
			err = s.deps.Docs.SeedText(ctx, tx, artifact.ID, string(input.content))
		} else {
			evictArtifactID = artifact.ID
			evictOnFailure = true
			err = s.deps.Docs.ReplaceText(ctx, artifact.ID, string(input.content), actor)
		}
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if err := s.replaceRefs(r.Context(), tx, "artifact", artifact.ID, string(input.content)); err != nil {
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
	eventType := "artifact.version"
	payload := versionEventPayload(artifact.ID, artifact.Name, version, diff)
	if created {
		eventType = "artifact.created"
		artifact.Versions = []model.Version{version}
		payload = map[string]any{"artifact": artifact}
	}
	event, err := s.appendEvent(r.Context(), tx, model.Event{IssueKey: issueKey, Type: eventType, Actor: actor, Payload: payload})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	evictOnFailure = false
	s.publish(event)
	writeJSON(w, http.StatusCreated, map[string]any{"artifact": artifact, "version": version})
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
	referencedBy, err := s.loadReferencedBy(r.Context(), artifact)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		model.Artifact
		ReferencedBy []model.ReferencedBy `json:"referenced_by"`
	}{Artifact: artifact, ReferencedBy: referencedBy})
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
	markdown, err := s.deps.Docs.Text(r.Context(), artifact.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"markdown": markdown, "version": nil})
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
		writeJSON(w, http.StatusOK, struct {
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
	if err := s.requireOpenIssue(r.Context(), tx, artifact.IssueKey); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if artifact.Kind != "doc" {
		writeError(w, "NOT_DOCUMENT", http.StatusBadRequest, "artifact is not a document")
		return
	}
	version, err := s.deps.Docs.NamedVersion(docs.WithTx(r.Context(), tx), artifact.ID, summary, actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	diff, err := s.namedVersionDiff(r.Context(), tx, artifact.ID, version)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, model.Event{
		IssueKey: artifact.IssueKey,
		Type:     "artifact.version",
		Actor:    actor,
		Payload:  versionEventPayload(artifact.ID, artifact.Name, version, diff),
	})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.deps.Docs.CommitVersion(artifact.ID, version)
	s.publish(event)
	writeJSON(w, http.StatusCreated, version)
}

func (s *server) editArtifact(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Ops     []model.EditOp `json:"ops"`
		Summary string         `json:"summary"`
		Actor   *model.Actor   `json:"actor"`
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
	if err := s.requireOpenIssue(r.Context(), tx, artifact.IssueKey); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	evictOnFailure = true
	applied, err := s.deps.Docs.ApplyOps(docs.WithTx(r.Context(), tx), artifact.ID, input.Ops, actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var version *model.Version
	var published []model.Event
	if applied > 0 {
		summary := strings.TrimSpace(input.Summary)
		if summary != "" {
			namedVersion, err := s.deps.Docs.NamedVersion(docs.WithTx(r.Context(), tx), artifact.ID, summary, actor)
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
			version = &namedVersion
		} else {
			unnamedVersion, wrote, err := s.deps.Docs.SnapshotVersion(r.Context(), tx, artifact.ID, actor)
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
			if wrote {
				version = &unnamedVersion
			}
		}
		if version != nil {
			var diff *string
			if version.Named {
				diff, err = s.namedVersionDiff(r.Context(), tx, artifact.ID, *version)
				if err != nil {
					s.writeHandlerError(w, err)
					return
				}
			}
			event, err := s.appendEvent(r.Context(), tx, model.Event{
				IssueKey: artifact.IssueKey,
				Type:     "artifact.version",
				Actor:    actor,
				Payload:  versionEventPayload(artifact.ID, artifact.Name, *version, diff),
			})
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
			published = append(published, event)
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	evictOnFailure = false
	if version != nil {
		s.deps.Docs.CommitVersion(artifact.ID, *version)
	}
	s.publish(published...)
	writeJSON(w, http.StatusOK, map[string]any{"applied": applied, "version": version})
}

func (s *server) setPrimaryArtifact(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Actor *model.Actor `json:"actor"`
	}
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &input); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	if actor.Kind != "user" {
		writeError(w, "HUMAN_ONLY", http.StatusForbidden, "only users can select the primary document")
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
	if artifact.Kind != "doc" {
		writeError(w, "PRIMARY_NOT_DOC", http.StatusBadRequest, "only documents can be primary")
		return
	}
	if err := s.requireOpenIssue(r.Context(), tx, artifact.IssueKey); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if _, err := tx.Exec(r.Context(), `update artifacts set is_primary = false where issue_key = $1 and is_primary`, artifact.IssueKey); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if _, err := tx.Exec(r.Context(), `update artifacts set is_primary = true where id = $1`, artifact.ID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	issue, err := s.loadIssue(r.Context(), tx, artifact.IssueKey)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	issue.LastSeq++
	event, err := s.appendEvent(r.Context(), tx, model.Event{IssueKey: issue.Key, Type: "issue.updated", Actor: actor, Payload: issue})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(event)
	writeJSON(w, http.StatusOK, issue)
}

func (s *server) loadArtifacts(ctx context.Context, q queryer, issueKey string) ([]model.Artifact, error) {
	rows, err := q.Query(ctx, `
		select id::text, issue_key, slug, name, kind, is_primary, created_by, created_at
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
	for index := range artifacts {
		versions, err := s.loadVersions(ctx, q, artifacts[index].ID)
		if err != nil {
			return nil, err
		}
		artifacts[index].Versions = versions
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
	if issueKey := r.PathValue("key"); issueKey != "" {
		return s.loadArtifactBySlug(ctx, q, issueKey, r.PathValue("slug"))
	}
	return s.loadArtifact(ctx, q, r.PathValue("id"))
}

func (s *server) loadArtifact(ctx context.Context, q queryer, id string) (model.Artifact, error) {
	parsed, err := parseArtifactID(id)
	if err != nil {
		return model.Artifact{}, err
	}
	return s.loadArtifactRow(ctx, q, q.QueryRow(ctx, `
		select id::text, issue_key, slug, name, kind, is_primary, created_by, created_at
		from artifacts where id = $1
	`, parsed))
}

func (s *server) loadArtifactBySlug(ctx context.Context, q queryer, issueKey, slug string) (model.Artifact, error) {
	return s.loadArtifactRow(ctx, q, q.QueryRow(ctx, `
		select id::text, issue_key, slug, name, kind, is_primary, created_by, created_at
		from artifacts where issue_key = $1 and slug = $2
	`, issueKey, slug))
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
	return artifact, nil
}

// scanArtifact reads the canonical artifact column list:
// id::text, issue_key, slug, name, kind, is_primary, created_by, created_at.
func scanArtifact(row pgx.Row) (model.Artifact, error) {
	var artifact model.Artifact
	var createdBy []byte
	if err := row.Scan(&artifact.ID, &artifact.IssueKey, &artifact.Slug, &artifact.Name, &artifact.Kind, &artifact.Primary, &createdBy, &artifact.CreatedAt); err != nil {
		return model.Artifact{}, err
	}
	if err := json.Unmarshal(createdBy, &artifact.CreatedBy); err != nil {
		return model.Artifact{}, fmt.Errorf("decode artifact author: %w", err)
	}
	return artifact, nil
}

func (s *server) findArtifactByName(ctx context.Context, q queryer, issueKey, name string) (model.Artifact, error) {
	rows, err := s.loadArtifacts(ctx, q, issueKey)
	if err != nil {
		return model.Artifact{}, err
	}
	for _, artifact := range rows {
		if artifact.Name == name {
			return artifact, nil
		}
	}
	return model.Artifact{}, pgx.ErrNoRows
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

func (s *server) nextArtifactSlug(ctx context.Context, q queryer, issueKey, name string) (string, error) {
	base := artifactSlug(name)
	for suffix := 1; ; suffix++ {
		candidate := base
		if suffix > 1 {
			candidate = fmt.Sprintf("%s-%d", base, suffix)
		}
		var inUse bool
		if err := q.QueryRow(ctx, `select exists(select 1 from artifacts where issue_key = $1 and slug = $2)`, issueKey, candidate).Scan(&inUse); err != nil {
			return "", err
		}
		if !inUse {
			return candidate, nil
		}
	}
}
