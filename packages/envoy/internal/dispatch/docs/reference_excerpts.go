package docs

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// IndexDocumentReferences indexes version's markdown, which the calling transaction has just
// stored as the document's version (indexDocument): its references, each mention's excerpt and
// its blocks, all from the rendering of the tree that transaction wrote: an upload's replacement,
// or a seeded spec. A seed renders its tree once. An upload that changes the document renders it
// twice - ReplaceText's render of the replacement, which is the version's markdown, and
// applyLive's render of the fork, which the ledger records - so the excerpts and blocks are cut
// from applyLive's render only when the two agree. It refuses a context that was not joined.
//
// The version's time, which dates its new references and blocks, is the transaction's now()
// (Ledger.Now): every writer of a version in a joined transaction dates it so, by the column's
// default or explicitly (NextVersion), and its row may not be written yet - its insert can be
// queued for the write phase like the index's own writes.
func (s *Service) IndexDocumentReferences(ctx context.Context, artifactID string, version int, markdown string) (model.ReferenceChanges, error) {
	tx, joined := txFromContext(ctx)
	if !joined {
		return model.ReferenceChanges{}, errUnjoined
	}
	ledger := ledgerFrom(ctx)
	versionAt, err := ledger.Now(ctx)
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	rendered := ledger.writtenRendering(artifactID)
	if rendered.tree != nil && rendered.markdown != markdown {
		slog.Warn("dispatch: a document version left its reference excerpts and blocks to the reconciliation: the tree its transaction wrote does not render as the version's markdown",
			"artifact", artifactID, "version", version)
		rendered = renderedTree{}
	}
	return s.indexDocument(ctx, tx, artifactID, version, versionAt, markdown, rendered)
}

// indexDocument indexes one document version, written at versionAt, in its writer's transaction,
// which holds the document owner's lock, from the render the version's markdown came from:
// indexRenderedVersion with rendered's blocks. The blocks are rendered.offsets; GET …/blocks lists
// one block per offset of the same renderer (renderedBlocks), so both list one set of blocks. A
// version with no rendering - an upload whose replacement changed nothing, which writes no tree -
// is indexUnrenderedVersion, which indexes no blocks: migration 0122's trigger left the document
// owed when the version's row was inserted, for the block-index reconciliation, which reads the
// document itself.
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
// holding the document owner's lock: its references, the excerpt of each mention, and its blocks
// with the mentions each holds (refs.IndexBlocks), all from one extraction of markdown. blocks
// index into markdown, the render they came from. The event the caller appends names the counted
// targets the document's references and their excerpts, its blocks and its blocks' mentions moved
// (replaceVersionRefs), since a node is counted once for each block citing it. The extraction,
// the index's plans and the counted targets are worked out where it is called; its writes go
// through write, in the order the index makes them.
func (s *Service) indexRenderedVersion(ctx context.Context, tx pgx.Tx, artifactID string, version int, versionAt time.Time, markdown string, blocks []model.ArtifactBlock) (model.ReferenceChanges, error) {
	located := text.ExtractAt(markdown, s.serverURL)
	// The blocks are indexed before the document's own references are reconciled: a first
	// indexing dates a block's mention from the document's refs row for its target, which a
	// surviving target already holds and a new one gets with this same time.
	moved, indexBlocks, err := planBlockIndex(ctx, tx, artifactID, version, versionAt, markdown, blocks, located)
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	if err := write(ctx, tx, indexBlocks); err != nil {
		return model.ReferenceChanges{}, err
	}
	return replaceVersionRefs(ctx, tx, artifactID, version, versionAt, located, moved, refs.MentionExcerpts(markdown, blocks, located))
}

// indexUnrenderedVersion indexes one document version with no rendering, written at versionAt, in
// a transaction holding the document owner's lock: its references alone. It indexes no blocks and
// marks the document's mentions owed an excerpt (refs.ClearExcerpts) for the excerpt
// reconciliation, which reads the document itself.
func (s *Service) indexUnrenderedVersion(ctx context.Context, tx pgx.Tx, artifactID string, version int, versionAt time.Time, markdown string) (model.ReferenceChanges, error) {
	return replaceVersionRefs(ctx, tx, artifactID, version, versionAt, text.ExtractAt(markdown, s.serverURL), nil, nil)
}

// replaceVersionRefs reconciles the document's references, dated versionAt, to the mentions
// located in its version, stores excerpts on them (nil: marks them owed, refs.ClearExcerpts), and
// counts the targets moved, which its blocks and their mentions moved, as well. A document mention
// shows the block holding it as its excerpt, so a target whose kept edge now stores another block
// is counted beside the ones whose edges the version inserted or deleted (refs.ExcerptsMoved,
// worked out here, before the reconciliation and the excerpts are written, in that order, by one
// write).
func replaceVersionRefs(ctx context.Context, tx pgx.Tx, artifactID string, version int, versionAt time.Time, located []text.Located, moved []refs.Target, excerpts map[refs.Target]model.GraphExcerpt) (model.ReferenceChanges, error) {
	replacement, err := planReplacement(ctx, tx, "artifact", artifactID, text.Refs(located), &versionAt)
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	counted := slices.Concat(replacement.Changed(), moved)
	writeExcerpts := func(b *store.WriteBatch) { refs.ClearExcerpts(b, artifactID) }
	if excerpts != nil {
		recorded, err := refs.ExcerptsMoved(ctx, tx, artifactID, version, replacement.Held(), excerpts)
		if err != nil {
			return model.ReferenceChanges{}, err
		}
		counted = append(counted, recorded...)
		writeExcerpts = func(b *store.WriteBatch) { refs.QueueRecordExcerpts(b, artifactID, version, excerpts) }
	}
	changes, err := refs.CountedChanges(ctx, tx, counted)
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	return changes, write(ctx, tx, func(b *store.WriteBatch) error {
		replacement.Queue(b)
		writeExcerpts(b)
		return nil
	})
}

// planBlockIndex plans version's block index of artifactID (refs.PlanBlocks) and returns the
// targets it moves, the cited blocks it changes included (refs.BlockIndexing.Cited), and the step
// that writes it. It plans against what the index holds: as the transaction's own queued indexing
// of the document leaves it, when there is one, else as read in tx; and it records what this one
// leaves for a later indexing of the same document.
func planBlockIndex(
	ctx context.Context,
	tx pgx.Tx,
	artifactID string,
	version int,
	versionAt time.Time,
	markdown string,
	blocks []model.ArtifactBlock,
	located []text.Located,
) ([]refs.Target, WriteStep, error) {
	ledger := queueFor(ctx, tx)
	index, held := refs.BlockIndex{}, false
	if ledger != nil {
		index, held = ledger.blockIndexes[artifactID]
	}
	if !held {
		var err error
		if index, err = refs.ReadBlockIndex(ctx, tx, artifactID); err != nil {
			return nil, nil, err
		}
	}
	indexing := refs.PlanBlocks(index, artifactID, version, versionAt, markdown, blocks, located)
	cited, err := indexing.Cited(ctx, tx)
	if err != nil {
		return nil, nil, err
	}
	if ledger != nil {
		if ledger.blockIndexes == nil {
			ledger.blockIndexes = make(map[string]refs.BlockIndex)
		}
		ledger.blockIndexes[artifactID] = indexing.Held()
	}
	return slices.Concat(indexing.Moved(), cited), func(b *store.WriteBatch) error {
		indexing.Queue(b)
		return nil
	}, nil
}

// planReplacement plans the reconciliation of fromKind/fromID's references with found, a new edge
// dated insertedAt (nil: the transaction's time), against the source's edges as the transaction's
// own queued reconciliation of it leaves them, when there is one, else as read in tx, and records
// what this one leaves.
func planReplacement(ctx context.Context, tx pgx.Tx, fromKind, fromID string, found []text.Ref, insertedAt *time.Time) (refs.Replacement, error) {
	ledger := queueFor(ctx, tx)
	source := refs.Target{Kind: fromKind, ID: fromID}
	var replacement refs.Replacement
	held, known := []refs.Target(nil), false
	if ledger != nil {
		held, known = ledger.referenced[source]
	}
	if known {
		replacement = refs.PlanReplaceFrom(held, fromKind, fromID, found, insertedAt)
	} else {
		var err error
		if replacement, err = refs.PlanReplace(ctx, tx, fromKind, fromID, found, insertedAt); err != nil {
			return refs.Replacement{}, err
		}
	}
	if ledger != nil {
		if ledger.referenced == nil {
			ledger.referenced = make(map[refs.Target][]refs.Target)
		}
		ledger.referenced[source] = replacement.Held()
	}
	return replacement, nil
}

// planReplaceCounted is refs.ReplaceCounted split in two: it plans the reconciliation of
// fromKind/fromID's references with found (planReplacement), resolves the counted targets it and
// moved move, and returns those with the step that writes it.
func planReplaceCounted(
	ctx context.Context,
	tx pgx.Tx,
	fromKind, fromID string,
	found []text.Ref,
	insertedAt *time.Time,
	moved []refs.Target,
) (model.ReferenceChanges, WriteStep, error) {
	replacement, err := planReplacement(ctx, tx, fromKind, fromID, found, insertedAt)
	if err != nil {
		return model.ReferenceChanges{}, nil, err
	}
	changes, err := refs.CountedChanges(ctx, tx, slices.Concat(replacement.Changed(), moved))
	if err != nil {
		return model.ReferenceChanges{}, nil, err
	}
	return changes, func(b *store.WriteBatch) error {
		replacement.Queue(b)
		return nil
	}, nil
}

// planAskReferences is planReplaceCounted for ask askID, whose references are the ones its
// question cites.
func planAskReferences(ctx context.Context, tx pgx.Tx, askID, question, serverURL string) (model.ReferenceChanges, WriteStep, error) {
	return planReplaceCounted(ctx, tx, "ask", askID, text.Extract(question, serverURL), nil, nil)
}

// planEditedAskReferences is planAskReferences for an edit of the ask's question from previous:
// it also counts every target the question still cites when the edit moves the head of its text,
// which each of the ask's edges shows (refs.EditedTargets).
func planEditedAskReferences(ctx context.Context, tx pgx.Tx, askID, previous, question, serverURL string) (model.ReferenceChanges, WriteStep, error) {
	found := text.Extract(question, serverURL)
	return planReplaceCounted(ctx, tx, "ask", askID, found, nil, refs.EditedTargets(previous, question, found))
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
