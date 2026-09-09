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
	"strconv"
	"strings"
	"unicode"

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

func (s *server) uploadArtifact(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxArtifactBlobSize+(1<<20))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		writeError(w, "CAP_EXCEEDED", http.StatusRequestEntityTooLarge, "artifact blob exceeds 25 MB")
		return
	}
	var supplied *model.Actor
	if raw := r.FormValue("actor"); raw != "" {
		var actor model.Actor
		if err := json.Unmarshal([]byte(raw), &actor); err != nil {
			writeError(w, "INVALID_JSON", http.StatusBadRequest, "invalid multipart actor")
			return
		}
		supplied = &actor
	}
	actor, ok := s.requireActor(w, r, supplied)
	if !ok {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		writeError(w, "INVALID_ARTIFACT", http.StatusBadRequest, "artifact name is required")
		return
	}
	primary, err := strconv.ParseBool(defaultString(r.FormValue("primary"), "false"))
	if err != nil {
		writeError(w, "INVALID_ARTIFACT", http.StatusBadRequest, "primary must be true or false")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, "INVALID_ARTIFACT", http.StatusBadRequest, "artifact file is required")
		return
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maxArtifactBlobSize+1))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if len(content) > maxArtifactBlobSize {
		writeError(w, "CAP_EXCEEDED", http.StatusRequestEntityTooLarge, "artifact blob exceeds 25 MB")
		return
	}
	contentType := header.Header.Get("Content-Type")
	if contentType == "" {
		contentType = mime.TypeByExtension(extension(name))
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		writeError(w, "INVALID_ARTIFACT", http.StatusBadRequest, "invalid artifact content type")
		return
	}
	kind := artifactKind(mediaType)
	if primary && kind != "doc" {
		writeError(w, "PRIMARY_NOT_DOC", http.StatusBadRequest, "only documents can be primary")
		return
	}
	summary := strings.TrimSpace(r.FormValue("summary"))

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	issueKey := r.PathValue("key")
	if err := s.requireOpenIssue(r.Context(), tx, issueKey); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var artifact model.Artifact
	var created bool
	artifact, err = s.findArtifactByName(r.Context(), tx, issueKey, name)
	if errors.Is(err, pgx.ErrNoRows) {
		created = true
		slug, err := s.nextArtifactSlug(r.Context(), tx, issueKey, name)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if primary {
			if _, err := tx.Exec(r.Context(), `update artifacts set is_primary = false where issue_key = $1 and is_primary`, issueKey); err != nil {
				s.writeHandlerError(w, err)
				return
			}
		}
		actorJSON, err := jsonActor(actor)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if err := tx.QueryRow(r.Context(), `
			insert into artifacts (issue_key, slug, name, kind, is_primary, created_by)
			values ($1, $2, $3, $4, $5, $6)
			returning id::text, issue_key, slug, name, kind, is_primary, created_by, created_at
		`, issueKey, slug, name, kind, primary, actorJSON).Scan(
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
		if primary && !artifact.Primary {
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
	checksum := sha256.Sum256(content)
	sha := hex.EncodeToString(checksum[:])
	var summaryValue any
	if summary != "" {
		summaryValue = summary
	}
	if kind == "doc" {
		if err := tx.QueryRow(r.Context(), `
			insert into artifact_versions (artifact_id, number, markdown, authors, summary)
			values ($1, $2, $3, $4, $5)
			returning number, named, summary, authors, created_at
		`, artifact.ID, nextNumber, string(content), authors, summaryValue).Scan(
			&version.Number, &version.Named, &version.Summary, &versionAuthors, &version.CreatedAt,
		); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		ctx := docs.WithTx(r.Context(), tx)
		if created {
			err = s.deps.Docs.SeedText(ctx, tx, artifact.ID, string(content))
		} else {
			err = s.deps.Docs.ReplaceText(ctx, artifact.ID, string(content), actor)
		}
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if err := s.replaceRefs(r.Context(), tx, "artifact", artifact.ID, string(content)); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	} else {
		size := len(content)
		if err := tx.QueryRow(r.Context(), `
			insert into artifact_versions (artifact_id, number, content, mime, size, sha256, authors, summary)
			values ($1, $2, $3, $4, $5, $6, $7, $8)
			returning number, named, summary, authors, created_at, size, mime, sha256
		`, artifact.ID, nextNumber, content, contentType, size, sha, authors, summaryValue).Scan(
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
	eventType := "artifact.version"
	payload := map[string]any{"artifact_id": artifact.ID, "name": artifact.Name, "version": version}
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
	s.publish(event)
	writeJSON(w, http.StatusCreated, map[string]any{"artifact": artifact, "version": version})
}

func (s *server) getArtifact(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	artifact, err := s.loadArtifact(r.Context(), s.deps.Store.Pool, r.PathValue("id"))
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
	artifact, err := s.loadArtifact(r.Context(), s.deps.Store.Pool, r.PathValue("id"))
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
	artifact, err := s.loadArtifact(r.Context(), s.deps.Store.Pool, r.PathValue("id"))
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
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	artifact, err := s.loadArtifact(r.Context(), tx, r.PathValue("id"))
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
	version, err := s.deps.Docs.NamedVersion(docs.WithTx(r.Context(), tx), artifact.ID, input.Summary, actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, model.Event{
		IssueKey: artifact.IssueKey,
		Type:     "artifact.version",
		Actor:    actor,
		Payload:  map[string]any{"artifact_id": artifact.ID, "name": artifact.Name, "version": version},
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
	artifact, err := s.loadArtifact(r.Context(), s.deps.Store.Pool, r.PathValue("id"))
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
	defer tx.Rollback(r.Context())
	if err := s.requireOpenIssue(r.Context(), tx, artifact.IssueKey); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	applied, err := s.deps.Docs.ApplyOps(r.Context(), artifact.ID, input.Ops, actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var version *model.Version
	var published []model.Event
	if summary := strings.TrimSpace(input.Summary); applied > 0 && summary != "" {
		namedVersion, err := s.deps.Docs.NamedVersion(docs.WithTx(r.Context(), tx), artifact.ID, summary, actor)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		version = &namedVersion
		event, err := s.appendEvent(r.Context(), tx, model.Event{
			IssueKey: artifact.IssueKey,
			Type:     "artifact.version",
			Actor:    actor,
			Payload:  map[string]any{"artifact_id": artifact.ID, "name": artifact.Name, "version": namedVersion},
		})
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		published = append(published, event)
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
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
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	artifact, err := s.loadArtifact(r.Context(), tx, r.PathValue("id"))
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

func (s *server) loadArtifacts(ctx context.Context, q issueQueryer, issueKey string) ([]model.Artifact, error) {
	rows, err := q.Query(ctx, `
		select id::text, issue_key, slug, name, kind, is_primary, created_by, created_at
		from artifacts where issue_key = $1 order by created_at, id
	`, issueKey)
	if err != nil {
		return nil, err
	}
	artifacts := []model.Artifact{}
	for rows.Next() {
		var artifact model.Artifact
		var createdBy []byte
		if err := rows.Scan(&artifact.ID, &artifact.IssueKey, &artifact.Slug, &artifact.Name, &artifact.Kind, &artifact.Primary, &createdBy, &artifact.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		if err := json.Unmarshal(createdBy, &artifact.CreatedBy); err != nil {
			rows.Close()
			return nil, fmt.Errorf("decode artifact author: %w", err)
		}
		artifacts = append(artifacts, artifact)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for index := range artifacts {
		versions, err := s.loadVersions(ctx, q, artifacts[index].ID)
		if err != nil {
			return nil, err
		}
		artifacts[index].Versions = versions
	}
	return artifacts, nil
}

func (s *server) loadArtifact(ctx context.Context, q issueQueryer, id string) (model.Artifact, error) {
	var artifact model.Artifact
	var createdBy []byte
	if err := q.QueryRow(ctx, `
		select id::text, issue_key, slug, name, kind, is_primary, created_by, created_at
		from artifacts where id = $1
	`, id).Scan(&artifact.ID, &artifact.IssueKey, &artifact.Slug, &artifact.Name, &artifact.Kind, &artifact.Primary, &createdBy, &artifact.CreatedAt); err != nil {
		return model.Artifact{}, err
	}
	if err := json.Unmarshal(createdBy, &artifact.CreatedBy); err != nil {
		return model.Artifact{}, fmt.Errorf("decode artifact author: %w", err)
	}
	versions, err := s.loadVersions(ctx, q, artifact.ID)
	if err != nil {
		return model.Artifact{}, err
	}
	artifact.Versions = versions
	return artifact, nil
}

func (s *server) findArtifactByName(ctx context.Context, q issueQueryer, issueKey, name string) (model.Artifact, error) {
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

func (s *server) loadVersions(ctx context.Context, q issueQueryer, artifactID string) ([]model.Version, error) {
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

func (s *server) loadVersion(ctx context.Context, q issueQueryer, artifactID string, number int, markdown *string) (model.Version, error) {
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

func (s *server) nextArtifactSlug(ctx context.Context, q issueQueryer, issueKey, name string) (string, error) {
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

func extension(name string) string {
	if index := strings.LastIndexByte(name, '.'); index >= 0 {
		return name[index:]
	}
	return ""
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
