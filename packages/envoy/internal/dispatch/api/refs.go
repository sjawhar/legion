package api

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

func (s *server) replaceRefs(ctx context.Context, tx pgx.Tx, fromKind, fromID, body string) error {
	if _, err := tx.Exec(ctx, `delete from refs where from_kind = $1 and from_id = $2`, fromKind, fromID); err != nil {
		return fmt.Errorf("clear references: %w", err)
	}
	for _, ref := range text.Extract(body, s.deps.ServerURL) {
		toID := ref.ID
		if ref.Kind == "artifact" {
			toID = ref.IssueKey + "/" + ref.ID
		}
		if _, err := tx.Exec(ctx, `
			insert into refs (from_kind, from_id, to_kind, to_id)
			values ($1, $2, $3, $4)
			on conflict do nothing
		`, fromKind, fromID, ref.Kind, toID); err != nil {
			return fmt.Errorf("write reference: %w", err)
		}
	}
	return nil
}

func (s *server) loadReferencedBy(ctx context.Context, artifact model.Artifact) ([]model.ReferencedBy, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select sources.kind, sources.id, sources.issue_key, sources.excerpt
		from refs
		join (
			select 'ask'::text as kind, id::text as id, issue_key, left(question, 240) as excerpt from asks
			union all
			select 'comment'::text, id::text, issue_key, left(body, 240) from comments
			union all
			select 'message'::text, id::text, issue_key, left(body, 240) from messages
		) as sources on sources.kind = refs.from_kind and sources.id = refs.from_id
		where refs.to_kind = 'artifact' and refs.to_id = $1
		order by sources.kind, sources.id
	`, artifact.IssueKey+"/"+artifact.Slug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	references := []model.ReferencedBy{}
	for rows.Next() {
		var reference model.ReferencedBy
		if err := rows.Scan(&reference.Kind, &reference.ID, &reference.IssueKey, &reference.Excerpt); err != nil {
			return nil, err
		}
		references = append(references, reference)
	}
	return references, rows.Err()
}
