package docs

import (
	"context"
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// ReferenceExcerptBackfill reports one document whose existing mention edges received stored
// excerpts. A document that cannot load stays unprepared so the server does not serve a changed
// backlink panel.
type ReferenceExcerptBackfill struct {
	ArtifactID string
	References int
	Err        error
}

// BackfillReferenceExcerpts records stored excerpts for every document mention created before
// document indexing started doing so. It is safe to rerun: completed documents are omitted and
// each selected document updates every one of its existing reference rows in one transaction.
func (s *Service) BackfillReferenceExcerpts(ctx context.Context) ([]ReferenceExcerptBackfill, error) {
	ctx = store.WithTransactionTracking(ctx)
	rows, err := s.store.Pool.Query(ctx, `
		select distinct a.id::text
		from artifacts a
		join refs r on r.from_kind = 'artifact' and r.from_id = a.id::text
		where a.kind = 'doc' and not r.excerpt_ready
		order by a.id::text
	`)
	if err != nil {
		return nil, fmt.Errorf("list document reference excerpts: %w", err)
	}
	defer rows.Close()
	artifactIDs := []string{}
	for rows.Next() {
		var artifactID string
		if err := rows.Scan(&artifactID); err != nil {
			return nil, fmt.Errorf("scan document reference excerpt: %w", err)
		}
		artifactIDs = append(artifactIDs, artifactID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate document reference excerpts: %w", err)
	}

	reports := make([]ReferenceExcerptBackfill, 0, len(artifactIDs))
	for _, artifactID := range artifactIDs {
		reports = append(reports, s.backfillReferenceExcerpts(ctx, artifactID))
	}
	return reports, nil
}

func (s *Service) backfillReferenceExcerpts(ctx context.Context, artifactID string) ReferenceExcerptBackfill {
	report := ReferenceExcerptBackfill{ArtifactID: artifactID}
	markdown, blocks, err := s.TextWithBlocks(ctx, artifactID)
	if err != nil {
		report.Err = fmt.Errorf("read document: %w", err)
		return report
	}

	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		report.Err = fmt.Errorf("begin transaction: %w", err)
		return report
	}
	defer tx.Rollback(ctx)
	if err := tx.QueryRow(ctx, `
		select count(*)
		from refs
		where from_kind = 'artifact' and from_id = $1
	`, artifactID).Scan(&report.References); err != nil {
		report.Err = fmt.Errorf("count references: %w", err)
		return report
	}
	if err := refs.StoreDocumentExcerpts(ctx, tx, artifactID, markdown, blocks, s.serverURL); err != nil {
		report.Err = err
		return report
	}
	if err := tx.Commit(ctx); err != nil {
		report.Err = fmt.Errorf("commit transaction: %w", err)
	}
	return report
}
