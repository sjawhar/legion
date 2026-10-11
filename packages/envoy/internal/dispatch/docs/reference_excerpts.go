package docs

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// IndexDocumentReferences indexes the mentions of version's markdown, which the calling
// transaction has just stored as the document's version (refs.ReplaceCounted), and stores each
// mention's excerpt from the rendering of the tree that transaction wrote: an upload's
// replacement, or a seeded spec. A seed renders its tree once. An upload that changes the
// document renders it twice - ReplaceText's render of the replacement, which is the version's
// markdown, and applyLive's render of the fork, which the ledger records - so the excerpts are cut
// from applyLive's render only when the two agree. It refuses a context that was not joined.
func (s *Service) IndexDocumentReferences(ctx context.Context, artifactID string, version int, markdown string) (model.ReferenceChanges, error) {
	tx, joined := txFromContext(ctx)
	if !joined {
		return model.ReferenceChanges{}, errUnjoined
	}
	changes, err := refs.ReplaceCounted(ctx, tx, "artifact", artifactID, markdown, s.serverURL)
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	rendered := ledgerFrom(ctx).writtenRendering(artifactID)
	if rendered.tree != nil && rendered.markdown != markdown {
		slog.Warn("dispatch: a document version left its reference excerpts to the reconciliation: the tree its transaction wrote does not render as the version's markdown",
			"artifact", artifactID, "version", version)
		rendered = renderedTree{}
	}
	return changes, s.recordTreeExcerpts(ctx, tx, artifactID, version, rendered)
}

// recordTreeExcerpts stores the excerpt of every mention the document's version writes, cut from
// rendered, the render the version's markdown came from, so the block ids are the ones the
// document carries. A version with no rendering - an upload whose replacement changed nothing,
// which writes no tree - marks its mentions owed (refs.ClearExcerpts) for the reconciliation,
// which reads the document itself.
func (s *Service) recordTreeExcerpts(ctx context.Context, tx pgx.Tx, artifactID string, version int, rendered renderedTree) error {
	if rendered.tree == nil {
		return refs.ClearExcerpts(ctx, tx, artifactID)
	}
	blocks := make([]model.ArtifactBlock, len(rendered.offsets))
	for index, offset := range rendered.offsets {
		blocks[index] = model.ArtifactBlock{ID: offset.ID, Type: offset.Type, From: offset.From, To: offset.To}
	}
	return refs.RecordExcerpts(ctx, tx, artifactID, version, refs.MentionExcerpts(rendered.markdown, blocks, s.serverURL))
}

// referenceExcerptBatch is how many documents one reconciliation tick reads; referenceExcerptStep
// is the wait before the next tick while a full batch says drift remains,
// ReferenceExcerptReconcileInterval the wait once a tick finds less than a batch, and
// referenceExcerptStaleWindow how long after its start the reconciliation also looks for excerpts
// a version older than this binary left behind (refs.MarkStaleExcerpts).
const (
	referenceExcerptBatch             = 20
	referenceExcerptStep              = 2 * time.Second
	ReferenceExcerptReconcileInterval = 5 * time.Minute
	referenceExcerptStaleWindow       = time.Hour
)

// RunReferenceExcerptReconciliation stores the excerpts of every document some of whose mentions
// are owed one (refs.DriftedExcerpts): every mention indexed before migration 0092, each one
// rebuild-refs inserts, and each a version write could not cut. One batch per tick, at start, then
// every referenceExcerptStep while a full batch says drift remains and every
// ReferenceExcerptReconcileInterval once none does, until ctx ends. For the first
// referenceExcerptStaleWindow after it starts, each tick first marks owed the excerpts a version
// written by the binary a deploy replaces left behind, while both run; until a tick marks one,
// GET /api/v1/references serves that older excerpt. A mention owed an excerpt is answered with
// the document's name until this stores one.
func (s *Service) RunReferenceExcerptReconciliation(ctx context.Context) {
	ctx = store.WithTransactionTracking(ctx)
	started := time.Now()
	for {
		if time.Since(started) < referenceExcerptStaleWindow {
			if marked, err := refs.MarkStaleExcerpts(ctx, s.store.Pool); err != nil && ctx.Err() == nil {
				slog.Error("dispatch: mark stale reference excerpts", "error", err)
			} else if marked > 0 {
				slog.Info("dispatch: marked stale reference excerpts owed", "mentions", marked)
			}
		}
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
// none and keep the document's name until its next version. A document it cannot read or record
// for any other reason is logged and skipped for this tick, its mentions left owed, so one
// document never holds back the rest of its batch; documents counts the ones recorded, and full
// reports a batch that was full.
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
			slog.Error("dispatch: reference excerpt reconciliation skipped a document it could not read",
				"artifact", document.FromID, "error", err)
			continue
		default:
			excerpts = refs.MentionExcerpts(markdown, blocks, s.serverURL)
		}
		if err := refs.RecordExcerpts(ctx, s.store.Pool, document.FromID, document.Version, excerpts); err != nil {
			if ctx.Err() != nil {
				return documents, false, ctx.Err()
			}
			slog.Error("dispatch: reference excerpt reconciliation skipped a document it could not record",
				"artifact", document.FromID, "error", err)
			continue
		}
		documents++
	}
	return documents, len(drifted) == referenceExcerptBatch, nil
}
