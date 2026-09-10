// Package refs stores Dispatch links and reads artifact reference relationships.
package refs

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// Queryer is the query surface shared by a pool and a transaction.
type Queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// ToID returns the durable target identifier for a parsed Dispatch reference.
func ToID(ref text.Ref) string {
	if ref.Kind != "artifact" {
		return ref.ID
	}
	key := ref.IssueKey
	if key == "" {
		key = ref.Project
	}
	return key + "/" + ref.ID
}

// Replace rewrites all Dispatch references written by one source.
func Replace(ctx context.Context, tx pgx.Tx, fromKind, fromID, body, serverURL string) error {
	if _, err := tx.Exec(ctx, `delete from refs where from_kind = $1 and from_id = $2`, fromKind, fromID); err != nil {
		return fmt.Errorf("clear references: %w", err)
	}
	for _, ref := range text.Extract(body, serverURL) {
		if ref.Kind == "url" {
			continue
		}
		if _, err := tx.Exec(ctx, `
			insert into refs (from_kind, from_id, to_kind, to_id)
			values ($1, $2, $3, $4)
			on conflict do nothing
		`, fromKind, fromID, ref.Kind, ToID(ref)); err != nil {
			return fmt.Errorf("write reference: %w", err)
		}
	}
	return nil
}

// ReferencedBy lists Dispatch items and artifacts that link an artifact reference key.
func ReferencedBy(ctx context.Context, q Queryer, refKey string) ([]model.ReferencedBy, error) {
	rows, err := q.Query(ctx, `
		select sources.kind, sources.id, sources.issue_key, sources.project, sources.excerpt, sources.ref_key
		from refs
		join (
			select 'ask'::text as kind, a.id::text as id, a.issue_key,
			       coalesce(i.project_key, ar.project_key) as project, left(a.question, 240) as excerpt,
			       ''::text as ref_key
			from asks a
			left join issues i on i.key = a.issue_key
			left join artifacts ar on ar.id = a.artifact_id
			union all
			select 'comment'::text, c.id::text, c.issue_key,
			       coalesce(i.project_key, ar.project_key), left(c.body, 240), ''::text
			from comments c
			left join issues i on i.key = c.issue_key
			left join artifacts ar on ar.id = c.artifact_id
			union all
			select 'message'::text, m.id::text, m.issue_key, i.project_key, left(m.body, 240), ''::text
			from messages m
			join issues i on i.key = m.issue_key
			union all
			select 'artifact'::text, a.id::text, a.issue_key, a.project_key, a.name, a.ref_key
			from artifacts a
		) as sources on sources.kind = refs.from_kind and sources.id = refs.from_id
		where refs.to_kind = 'artifact' and refs.to_id = $1
		order by sources.kind, sources.id
	`, refKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	references := []model.ReferencedBy{}
	for rows.Next() {
		var reference model.ReferencedBy
		if err := rows.Scan(
			&reference.Kind,
			&reference.ID,
			&reference.IssueKey,
			&reference.Project,
			&reference.Excerpt,
			&reference.RefKey,
		); err != nil {
			return nil, err
		}
		references = append(references, reference)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return references, nil
}

// Outgoing lists Dispatch references written by an artifact.
func Outgoing(ctx context.Context, q Queryer, artifactID string) ([]model.OutgoingReference, error) {
	rows, err := q.Query(ctx, `
		select r.to_kind, r.to_id,
		       a.id::text, a.issue_key, coalesce(a.project_key, ''), coalesce(a.ref_key, ''), coalesce(a.slug, ''), coalesce(a.name, ''), coalesce(a.kind, ''), coalesce(a.is_primary, false),
		       coalesce(a.created_by, '{}'::jsonb), coalesce(a.created_at, timestamptz 'epoch'),
		       coalesce(v.number, 0), coalesce(v.named, false), v.summary, coalesce(v.authors, '[]'::jsonb), coalesce(v.created_at, timestamptz 'epoch'), v.size, v.mime, v.sha256
		from refs r
		left join artifacts a on r.to_kind = 'artifact' and a.ref_key = r.to_id
		left join artifact_versions v on v.artifact_id = a.id
		where r.from_kind = 'artifact' and r.from_id = $1
		order by r.to_kind, r.to_id, v.number
	`, artifactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	outgoing := []model.OutgoingReference{}
	positions := make(map[string]int)
	for rows.Next() {
		var reference model.OutgoingReference
		var artifactIDValue *string
		var artifact model.Artifact
		var createdBy, authors []byte
		var version model.Version
		if err := rows.Scan(
			&reference.Kind,
			&reference.ToID,
			&artifactIDValue,
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
		if artifactIDValue == nil {
			outgoing = append(outgoing, reference)
			continue
		}
		artifact.ID = *artifactIDValue
		if err := json.Unmarshal(createdBy, &artifact.CreatedBy); err != nil {
			return nil, fmt.Errorf("decode artifact author: %w", err)
		}
		if err := json.Unmarshal(authors, &version.Authors); err != nil {
			return nil, fmt.Errorf("decode artifact version authors: %w", err)
		}
		if position, found := positions[reference.ToID]; found {
			outgoing[position].Artifact.Versions = append(outgoing[position].Artifact.Versions, version)
			continue
		}
		artifact.Versions = []model.Version{version}
		reference.Artifact = &artifact
		positions[reference.ToID] = len(outgoing)
		outgoing = append(outgoing, reference)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return outgoing, nil
}
