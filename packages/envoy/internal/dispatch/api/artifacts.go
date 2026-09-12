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
	writeJSON(w, http.StatusOK, artifacts)
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
	var project string
	if target.IssueKey != nil {
		if err := s.requireOpenOwner(r.Context(), tx, issueOwner(*target.IssueKey)); err != nil {
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
		ctx := docs.WithTx(r.Context(), tx)
		var markdown string
		if created {
			markdown, err = s.deps.Docs.SeedText(ctx, tx, artifact.ID, string(input.content))
		} else {
			evictArtifactID = artifact.ID
			evictOnFailure = true
			markdown, err = s.deps.Docs.ReplaceText(ctx, artifact.ID, string(input.content), actor)
		}
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if err := tx.QueryRow(r.Context(), `
			insert into artifact_versions (artifact_id, number, markdown, authors, named, summary)
			values ($1, $2, $3, $4, $5, $6)
			returning number, named, summary, authors, created_at
		`, artifact.ID, nextNumber, markdown, authors, input.summary != "", summaryValue).Scan(
			&version.Number, &version.Named, &version.Summary, &versionAuthors, &version.CreatedAt,
		); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if err := refs.Replace(r.Context(), tx, "artifact", artifact.ID, markdown, s.deps.ServerURL); err != nil {
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
	event, err := s.appendEvent(r.Context(), tx, ownerForArtifact(artifact).event(eventType, actor, payload))
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
	referencedBy, err := refs.ReferencedBy(r.Context(), s.deps.Store.Pool, artifact.RefKey)
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
	eventOwner := ownerForArtifact(artifact)
	if err := s.requireOpenOwner(r.Context(), tx, eventOwner); err != nil {
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
	event, err := s.appendEvent(r.Context(), tx, eventOwner.event(
		"artifact.version",
		actor,
		versionEventPayload(artifact.ID, artifact.Name, version, diff),
	))
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
	eventOwner := ownerForArtifact(artifact)
	if err := s.requireOpenOwner(r.Context(), tx, eventOwner); err != nil {
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
			event, err := s.appendEvent(r.Context(), tx, eventOwner.event(
				"artifact.version",
				actor,
				versionEventPayload(artifact.ID, artifact.Name, *version, diff),
			))
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
		return s.loadArtifactByRefKey(ctx, q, key+"/"+r.PathValue("slug"))
	}
	return s.loadArtifact(ctx, q, r.PathValue("id"))
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
