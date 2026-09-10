package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

func (s *server) getIssueReferences(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	issue, err := s.loadIssue(r.Context(), s.deps.Store.Pool, r.PathValue("key"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	closure, truncated, err := refs.Closure(r.Context(), s.deps.Store.Pool, issue.Key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	refKeys := make([]string, len(closure))
	for index, member := range closure {
		refKeys[index] = member.RefKey
	}
	artifacts, err := s.loadArtifactsByRefKey(r.Context(), s.deps.Store.Pool, refKeys)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	members := make([]model.ReferenceMember, 0, len(closure))
	for _, closureMember := range closure {
		artifact, ok := artifacts[closureMember.RefKey]
		if !ok {
			s.writeHandlerError(w, fmt.Errorf("reference target %q disappeared while loading closure", closureMember.RefKey))
			return
		}
		members = append(members, model.ReferenceMember{
			Artifact: artifact,
			Depth:    closureMember.Depth,
			Via:      closureMember.Via,
		})
	}
	etag := referencesETag(issue.LastSeq, members, truncated)
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, model.IssueReferences{Members: members, Truncated: truncated})
}

func (s *server) getArtifactReferences(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	artifact, err := s.loadArtifact(r.Context(), s.deps.Store.Pool, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	outgoing, err := refs.Outgoing(r.Context(), s.deps.Store.Pool, artifact.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	referencedBy, err := refs.ReferencedBy(r.Context(), s.deps.Store.Pool, artifact.RefKey)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, model.ArtifactReferences{Outgoing: outgoing, ReferencedBy: referencedBy})
}

func referencesETag(lastSeq int, members []model.ReferenceMember, truncated bool) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "%d\n%t\n", lastSeq, truncated)
	for _, member := range members {
		_, _ = fmt.Fprintf(hash, "%s@%d\n", member.Artifact.ID, member.Artifact.Versions[len(member.Artifact.Versions)-1].Number)
	}
	return `"` + hex.EncodeToString(hash.Sum(nil)) + `"`
}

func (s *server) loadArtifactsByRefKey(ctx context.Context, q queryer, refKeys []string) (map[string]model.Artifact, error) {
	artifacts := make(map[string]model.Artifact, len(refKeys))
	if len(refKeys) == 0 {
		return artifacts, nil
	}
	rows, err := q.Query(ctx, `
		select a.id::text, a.issue_key, a.project_key, a.ref_key, a.slug, a.name, a.kind, a.is_primary,
		       a.created_by, a.created_at,
		       v.number, v.named, v.summary, v.authors, v.created_at, v.size, v.mime, v.sha256
		from artifacts a
		join artifact_versions v on v.artifact_id = a.id
		where a.ref_key = any($1)
		order by a.ref_key, v.number
	`, refKeys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var artifact model.Artifact
		var createdBy, authors []byte
		var version model.Version
		if err := rows.Scan(
			&artifact.ID,
			&artifact.IssueKey,
			&artifact.Project,
			&artifact.RefKey,
			&artifact.Slug,
			&artifact.Name,
			&artifact.Kind,
			&artifact.Primary,
			&createdBy,
			&artifact.CreatedAt,
			&version.Number,
			&version.Named,
			&version.Summary,
			&authors,
			&version.CreatedAt,
			&version.Size,
			&version.MIME,
			&version.SHA256,
		); err != nil {
			return nil, err
		}
		if err := decodeArtifactReference(&artifact, createdBy, &version, authors); err != nil {
			return nil, err
		}
		if current, found := artifacts[artifact.RefKey]; found {
			current.Versions = append(current.Versions, version)
			artifacts[artifact.RefKey] = current
			continue
		}
		artifact.Versions = []model.Version{version}
		artifacts[artifact.RefKey] = artifact
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return artifacts, nil
}

func decodeArtifactReference(artifact *model.Artifact, createdBy []byte, version *model.Version, authors []byte) error {
	if err := json.Unmarshal(createdBy, &artifact.CreatedBy); err != nil {
		return fmt.Errorf("decode artifact author: %w", err)
	}
	if err := json.Unmarshal(authors, &version.Authors); err != nil {
		return fmt.Errorf("decode artifact version authors: %w", err)
	}
	return nil
}
