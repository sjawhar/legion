package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// IndexDocumentReferences indexes the mentions of version's markdown, which the calling
// transaction has just stored as the document's version (refs.ReplaceCounted), and stores each
// mention's excerpt from the tree that transaction wrote: an upload's replacement, or a seeded
// spec. It refuses a context that was not joined.
func (s *Service) IndexDocumentReferences(ctx context.Context, artifactID string, version int, markdown string) (model.ReferenceChanges, error) {
	tx, joined := txFromContext(ctx)
	if !joined {
		return model.ReferenceChanges{}, errUnjoined
	}
	changes, err := refs.ReplaceCounted(ctx, tx, "artifact", artifactID, markdown, s.serverURL)
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	return changes, s.recordTreeExcerpts(ctx, tx, artifactID, version, markdown, ledgerFrom(ctx).writtenTree(artifactID))
}

// recordTreeExcerpts stores the excerpt of every mention the document's version writes, cut from
// tree, the tree the version's markdown was rendered from, so the block ids are the ones the
// document carries. A tree that does not render as markdown is left to the reconciliation, which
// reads the document itself.
func (s *Service) recordTreeExcerpts(ctx context.Context, tx pgx.Tx, artifactID string, version int, markdown string, tree *pmdoc.Node) error {
	if tree == nil {
		slog.Warn("dispatch: a document version left its reference excerpts to the reconciliation: no tree",
			"artifact", artifactID, "version", version)
		return nil
	}
	rendered, offsets, err := pmdoc.RenderWithBlockOffsets(tree)
	if err != nil || rendered != markdown {
		slog.Warn("dispatch: a document version left its reference excerpts to the reconciliation: its tree does not render as its markdown",
			"artifact", artifactID, "version", version, "error", err)
		return nil
	}
	blocks := make([]model.ArtifactBlock, len(offsets))
	for index, offset := range offsets {
		blocks[index] = model.ArtifactBlock{ID: offset.ID, Type: offset.Type, From: offset.From, To: offset.To}
	}
	return refs.RecordExcerpts(ctx, tx, artifactID, version, refs.MentionExcerpts(rendered, blocks, s.serverURL))
}

// referenceExcerptBatch is how many documents one reconciliation tick reads; referenceExcerptStep
// is the wait before the next tick while a full batch says drift remains, and
// ReferenceExcerptReconcileInterval the wait once a tick finds less than a batch.
const (
	referenceExcerptBatch             = 20
	referenceExcerptStep              = 2 * time.Second
	ReferenceExcerptReconcileInterval = 5 * time.Minute
)

// RunReferenceExcerptReconciliation stores the excerpts of every document whose mentions hold
// none of its latest version (refs.DriftedExcerpts): every mention indexed before migration 0090,
// each one rebuild-refs inserts, and each a version write could not fill. One batch per tick, at
// start, then every referenceExcerptStep while a full batch says drift remains and every
// ReferenceExcerptReconcileInterval once none does, until ctx ends. Until a mention's excerpt is
// stored, GET /api/v1/references answers it with the document's name.
func (s *Service) RunReferenceExcerptReconciliation(ctx context.Context) {
	ctx = store.WithTransactionTracking(ctx)
	for {
		wait := ReferenceExcerptReconcileInterval
		documents, full, err := s.ReconcileReferenceExcerpts(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			slog.Error("dispatch: reconcile reference excerpts", "error", err)
		case full:
			wait = referenceExcerptStep
		}
		if documents > 0 {
			slog.Info("dispatch: reconciled reference excerpts", "documents", documents)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// ReconcileReferenceExcerpts runs one tick: it reads each document of one batch of drift as the
// caller sees it (TextWithBlocks), with no transaction or connection held, and stores its
// mentions' excerpts at the version the drift named. A document whose stored history does not
// load or whose tree is outside the schema has no block to show, so its mentions are stored with
// none and keep the document's name until its next version. full reports a batch that was full.
func (s *Service) ReconcileReferenceExcerpts(ctx context.Context) (documents int, full bool, err error) {
	drifted, err := refs.DriftedExcerpts(ctx, s.store.Pool, referenceExcerptBatch)
	if err != nil {
		return 0, false, err
	}
	for _, document := range drifted {
		var excerpts map[refs.Target]model.GraphExcerpt
		markdown, blocks, err := s.TextWithBlocks(ctx, document.FromID)
		switch {
		case ctx.Err() != nil:
			return documents, false, ctx.Err()
		case errors.Is(err, ErrDocumentUnloadable), errors.Is(err, ErrDocOutsideSchema):
			slog.Warn("dispatch: reference excerpt reconciliation cannot read the document; its mentions keep the document's name",
				"artifact", document.FromID, "error", err)
		case err != nil:
			return documents, false, fmt.Errorf("read document %s for its reference excerpts: %w", document.FromID, err)
		default:
			excerpts = refs.MentionExcerpts(markdown, blocks, s.serverURL)
		}
		if err := refs.RecordExcerpts(ctx, s.store.Pool, document.FromID, document.Version, excerpts); err != nil {
			return documents, false, err
		}
		documents++
	}
	return documents, len(drifted) == referenceExcerptBatch, nil
}
