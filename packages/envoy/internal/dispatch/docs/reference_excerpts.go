package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// deferBlockIndexBlocks is the number of blocks above which a document write leaves the block
// index to the settlement instead of paying for it in the request. Not every document defers: one
// of this many blocks or fewer is indexed by its own write, as every document was.
//
// The in-request index costs about 46 µs a block on a two-core pod: refs.IndexBlocks writes two
// rows a block, one in document_blocks and one in document_block_texts. At the bound itself a
// write of a list of 1,999 blocks took 234, 239 and 252 ms against 144, 145 and 157 ms for the
// same list one block longer, which defers - 92 ms for 2,000 blocks - and the same difference at
// 5,000 blocks is 239 ms, 48 µs a block. The heavy write measured for sjawhar/legion#2519 agrees:
// its 8,000-item list body is 8,001 blocks and spends 347 ms in IndexDocumentReferences, 43 µs a
// block, of a request total of 939-1,130 ms against the 1 s that write is held to. A bullet list
// is one block for the list and one for each item's paragraph; the list item itself is no block.
//
// The budget this holds the in-request index to is a tenth of that 1 s, 100 ms, which is well
// inside the ±150 ms by which this pod moves the same byte-identical write, so the block index of
// a document that stays inline is never what carries its write past the bound. 100 ms ÷ 48 µs a
// block is 2,083 blocks, and the bound is 2,000 of them, the 92 ms measured above. A document of
// 1,999 blocks is indexed by its own write and pays that there; one of 2,001 pays nothing in the
// request, and its blocks reach the index out of it, through the block-index reconciliation,
// which indexes every document owed one and runs every BlockIndexReconcileInterval, 30 s
// (RunBlockIndexReconciliation, block_index.go). The settlement the write queues indexes them
// only where it writes a version of its own, which it does only where the document changed after
// that write (contentChangedSinceVersion, mutation.go), so an upload nothing follows is the
// reconciliation's. The 8,000-item list pays nothing instead of 347 ms.
const deferBlockIndexBlocks = 2000

// IndexDocumentReferences indexes version's markdown, which the calling transaction has just
// stored as the document's version (indexDocument): its references, each mention's excerpt and
// its blocks, all from the rendering of the tree that transaction wrote: an upload's replacement,
// or a seeded spec. A seed renders its tree once. An upload that changes the document renders it
// twice - ReplaceText's render of the replacement, which is the version's markdown, and
// applyLive's render of the fork, which the ledger records - so the excerpts and blocks are cut
// from applyLive's render only when the two agree. A document of more blocks than a write indexes
// in its own request leaves them to the block-index reconciliation (deferBlockIndexBlocks). It
// refuses a context that was not joined.
func (s *Service) IndexDocumentReferences(ctx context.Context, artifactID string, version int, markdown string) (model.ReferenceChanges, error) {
	tx, joined := txFromContext(ctx)
	if !joined {
		return model.ReferenceChanges{}, errUnjoined
	}
	var versionAt time.Time
	if err := tx.QueryRow(ctx, `select created_at from artifact_versions where artifact_id = $1 and number = $2`,
		artifactID, version).Scan(&versionAt); err != nil {
		return model.ReferenceChanges{}, fmt.Errorf("read version %d of %s: %w", version, artifactID, err)
	}
	rendered := ledgerFrom(ctx).writtenRendering(artifactID)
	if rendered.tree != nil && rendered.markdown != markdown {
		slog.Warn("dispatch: a document version left its reference excerpts and blocks to the reconciliation: the tree its transaction wrote does not render as the version's markdown",
			"artifact", artifactID, "version", version)
		rendered = renderedTree{}
	}
	// Above deferBlockIndexBlocks the write indexes this version's references alone: migration
	// 0122's trigger marked the document owed a block index (document_block_versions.version
	// null) when the version's row was inserted, and nothing here settles that mark, so the
	// block-index reconciliation (RunBlockIndexReconciliation) indexes the blocks and the
	// mentions they hold out of the request.
	if blocks := len(rendered.offsets); rendered.tree != nil && blocks > deferBlockIndexBlocks {
		slog.Info("dispatch: a document version left its blocks to the settlement: it holds more blocks than a write indexes in its own request",
			"artifact", artifactID, "version", version, "blocks", blocks, "bound", deferBlockIndexBlocks)
		rendered = renderedTree{}
	}
	return s.indexDocument(ctx, tx, artifactID, version, versionAt, markdown, rendered)
}

// indexDocument indexes one document version, written at versionAt, in its writer's transaction,
// which holds the document's owner row, from the render the version's markdown came from:
// indexRenderedVersion with rendered's blocks. The blocks are rendered.offsets; GET …/blocks lists
// one block per offset of the same renderer (renderedBlocks), so both list one set of blocks. A
// version with no rendering - an upload whose replacement changed nothing, which writes no tree,
// or one whose blocks the write deferred (deferBlockIndexBlocks) - is indexUnrenderedVersion,
// which indexes no blocks: migration 0122's trigger left the document owed when the version's row
// was inserted, for the block-index reconciliation, which reads the document itself.
func (s *Service) indexDocument(ctx context.Context, tx pgx.Tx, artifactID string, version int, versionAt time.Time, markdown string, rendered renderedTree) (model.ReferenceChanges, error) {
	if rendered.tree == nil {
		return s.indexUnrenderedVersion(ctx, tx, artifactID, version, versionAt, markdown)
	}
	blocks := make([]model.ArtifactBlock, len(rendered.offsets))
	for index, offset := range rendered.offsets {
		blocks[index] = model.ArtifactBlock{ID: offset.ID, Type: offset.Type, From: offset.From, To: offset.To}
	}
	return s.indexRenderedVersion(ctx, tx, artifactID, version, versionAt, markdown, blocks)
}

// indexRenderedVersion indexes one document version, written at versionAt, in a transaction
// holding the document's owner row: its references, the excerpt of each mention, and its blocks
// with the mentions each holds (refs.IndexBlocks), all from one extraction of markdown. blocks
// index into markdown, the render they came from. The event the caller appends names the counted
// targets the document's references and their excerpts, its blocks and its blocks' mentions moved
// (refs.ReplaceDocumentCounted), since a node is counted once for each block citing it.
func (s *Service) indexRenderedVersion(ctx context.Context, tx pgx.Tx, artifactID string, version int, versionAt time.Time, markdown string, blocks []model.ArtifactBlock) (model.ReferenceChanges, error) {
	located := text.ExtractAt(markdown, s.serverURL)
	// The blocks are indexed before the document's own references are reconciled: a first
	// indexing dates a block's mention from the document's refs row for its target, which a
	// surviving target already holds and a new one gets with this same time.
	moved, err := refs.IndexBlocks(ctx, tx, artifactID, version, versionAt, markdown, blocks, located)
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	return replaceVersionRefs(ctx, tx, artifactID, version, versionAt, located, moved, refs.MentionExcerpts(markdown, blocks, located))
}

// indexUnrenderedVersion indexes one document version with no rendering, written at versionAt, in
// a transaction holding the document's owner row: its references alone. It indexes no blocks and
// marks the document's mentions owed an excerpt (refs.ClearExcerpts) for the excerpt
// reconciliation, which reads the document itself.
func (s *Service) indexUnrenderedVersion(ctx context.Context, tx pgx.Tx, artifactID string, version int, versionAt time.Time, markdown string) (model.ReferenceChanges, error) {
	return replaceVersionRefs(ctx, tx, artifactID, version, versionAt, text.ExtractAt(markdown, s.serverURL), nil, nil)
}

// replaceVersionRefs reconciles the document's references, dated versionAt, to the mentions
// located in its version, stores excerpts on them (nil: marks them owed), and counts the targets
// moved, which its blocks and their mentions moved, as well (refs.ReplaceDocumentCounted).
func replaceVersionRefs(ctx context.Context, tx pgx.Tx, artifactID string, version int, versionAt time.Time, located []text.Located, moved []refs.Target, excerpts map[refs.Target]model.GraphExcerpt) (model.ReferenceChanges, error) {
	return refs.ReplaceDocumentCounted(ctx, tx, artifactID, version, versionAt, text.Refs(located), moved, excerpts)
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

// referenceExcerptLease names the lease one reconciliation tick runs under. The excerpts it
// stores are the same whichever process cuts them, and a mention is owed one until some process
// does, so two processes reading the same batch of documents and writing the same rows is work
// done twice for one result - and, because each tick claims nothing, the same documents twice.
const referenceExcerptLease = "reference-excerpts"

// RunReferenceExcerptReconciliation stores the excerpts of every document some of whose mentions
// are owed one (refs.DriftedExcerpts): every mention indexed before migration 0092, each one
// rebuild-refs inserts, and each a version write could not cut. One batch per tick, at start, then
// every referenceExcerptStep while a full batch says drift remains and every
// ReferenceExcerptReconcileInterval once none does, until ctx ends. For the first
// referenceExcerptStaleWindow after it starts, each tick first marks owed the excerpts a version
// written by the binary a deploy replaces left behind, while both run; until a tick marks one,
// GET /api/v1/references serves that older excerpt. A mention owed an excerpt is answered with
// the document's name until this stores one.
//
// The ticks are duty.RunLeasedAfterEachPass's, each one the wait the pass before it asked for after
// that pass returned, and so is the period each runs under: that wait, referenceExcerptStep while
// drift remains and the full interval once none does. A tick inside it is a tick some process has
// already made, so it is skipped; passing the full interval instead would make the converging 2 s
// steps - this loop's whole point when a rebuild leaves thousands of mentions owed an excerpt - one
// batch per five minutes. A tick whose lease another process holds, or whose period that process
// has already served, waits the full interval: that process is doing this work, and the drift it
// leaves is what the next tick to take the lease finds. leases is required, as it is of every
// leased loop.
func (s *Service) RunReferenceExcerptReconciliation(ctx context.Context, leases duty.Leases) {
	ctx = store.WithTransactionTracking(ctx)
	started := time.Now()
	duty.RunLeasedAfterEachPass(ctx, leases, referenceExcerptLease, ReferenceExcerptReconcileInterval, func(ctx context.Context) time.Duration {
		return s.reconcileExcerptTick(ctx, started)
	})
}

// reconcileExcerptTick is one tick of the reconciliation - the stale marking its first
// referenceExcerptStaleWindow after started does, and one batch - and returns how long to wait
// before the next: referenceExcerptStep while a full batch says drift remains, the interval once
// none does.
func (s *Service) reconcileExcerptTick(ctx context.Context, started time.Time) time.Duration {
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
		return wait
	case err != nil:
		slog.Error("dispatch: reconcile reference excerpts", "error", err)
	case full:
		wait = referenceExcerptStep
	}
	if documents > 0 {
		slog.Info("dispatch: reconciled reference excerpts", "documents", documents)
	}
	return wait
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
			excerpts = refs.MentionExcerpts(markdown, blocks, text.ExtractAt(markdown, s.serverURL))
		}
		// The reconciliation appends no event, so what it moved is named nowhere: until it stores
		// an excerpt, the route answers a mention owed one with the document's name.
		if _, err := refs.RecordExcerpts(ctx, s.store.Pool, document.FromID, document.Version, excerpts); err != nil {
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
