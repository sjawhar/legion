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
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
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
	return s.indexRenderedVersion(ctx, tx, artifactID, version, versionAt, markdown, offsetBlocks(rendered.offsets), rendered.tree)
}

// offsetBlocks is the blocks a render's offsets name, as the block index takes them.
func offsetBlocks(offsets []pmdoc.BlockOffset) []model.ArtifactBlock {
	blocks := make([]model.ArtifactBlock, len(offsets))
	for index, offset := range offsets {
		blocks[index] = model.ArtifactBlock{ID: offset.ID, Type: offset.Type, From: offset.From, To: offset.To}
	}
	return blocks
}

// indexRenderedVersion indexes one document version, written at versionAt, in a transaction
<<<<<<< conflict 1 of 2
%%%%%%% diff from: rwloyrmo dbdf9318 "chore(claude-envoy): rebuild dist/ and legion/ from ff93d6c4df9e [skip ci]"
\\\\\\\        to: lntxprnt d3218a11 "review: record handoff"
-// holding the document's owner row: its references, the excerpt of each mention, and its blocks
-// with the mentions each holds (refs.IndexBlocks), all from one extraction of markdown. blocks
+// holding the document owner's lock: its references, the excerpt of each mention, and its blocks
+// with the mentions each holds (refs.PlanBlocks), all from one extraction of markdown. blocks
 // index into markdown, the render they came from. The event the caller appends names the counted
 // targets the document's references and their excerpts, its blocks and its blocks' mentions moved
-// (refs.ReplaceDocumentCounted), since a node is counted once for each block citing it.
+// (replaceVersionRefs), since a node is counted once for each block citing it. The extraction,
+// the index's plans and the counted targets are worked out where it is called; its writes go
+// through write, in the order the index makes them.
 func (s *Service) indexRenderedVersion(ctx context.Context, tx pgx.Tx, artifactID string, version int, versionAt time.Time, markdown string, blocks []model.ArtifactBlock) (model.ReferenceChanges, error) {
+++++++ orsoxnss f8f0425b "chore: fold changelog fragments [skip ci]"
// holding the document's owner row: its references, the excerpt of each mention, and its blocks
// with the links each holds (refs.IndexBlocks), all from one extraction of markdown. blocks
// index into markdown, the render they came from; tree, when the caller holds it, is the tree
// markdown was rendered from, and nil otherwise. The event the caller appends names the counted
// targets the document's references and their excerpts, its blocks and its blocks' links moved
// (refs.ReplaceDocumentCounted), since a node is counted once for each block citing it.
func (s *Service) indexRenderedVersion(ctx context.Context, tx pgx.Tx, artifactID string, version int, versionAt time.Time, markdown string, blocks []model.ArtifactBlock, tree *pmdoc.Node) (model.ReferenceChanges, error) {
>>>>>>> conflict 1 of 2 ends
	located := text.ExtractAt(markdown, s.serverURL)
	// The blocks are indexed before the document's own references are reconciled: a first
	// indexing dates a block's mention from the document's refs row for its target, which a
	// surviving target already holds and a new one gets with this same time.
<<<<<<< conflict 2 of 2
%%%%%%% diff from: rwloyrmo dbdf9318 "chore(claude-envoy): rebuild dist/ and legion/ from ff93d6c4df9e [skip ci]"
\\\\\\\        to: lntxprnt d3218a11 "review: record handoff"
-	moved, err := refs.IndexBlocks(ctx, tx, artifactID, version, versionAt, markdown, blocks, located)
+	moved, indexBlocks, err := planBlockIndex(ctx, tx, artifactID, version, versionAt, markdown, blocks, located)
+++++++ orsoxnss f8f0425b "chore: fold changelog fragments [skip ci]"
	moved, err := refs.IndexBlocks(ctx, tx, artifactID, version, versionAt, markdown, blocks, located, blockPointers(artifactID, markdown, blocks, tree))
>>>>>>> conflict 2 of 2 ends
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	if err := write(ctx, tx, indexBlocks); err != nil {
		return model.ReferenceChanges{}, err
	}
	return replaceVersionRefs(ctx, tx, artifactID, version, versionAt, located, moved, refs.MentionExcerpts(markdown, blocks, located))
}

// blockPointers is each `block[]` value of the version (pmdoc.BlockPointers) as the block index
// writes it: none when the schema declares no `block[]` attribute or the version has no blocks;
// otherwise those of tree, or, with no tree, of markdown read back. A version whose markdown does
// not read back is indexed with no pointers, as one the server cannot read is with no blocks.
func blockPointers(artifactID, markdown string, blocks []model.ArtifactBlock, tree *pmdoc.Node) []refs.Pointer {
	if len(blocks) == 0 || !pmdoc.HasLinkKinds() {
		return nil
	}
	if tree == nil {
		read, err := pmdoc.ParseRendering(markdown)
		if err != nil {
			slog.Warn("dispatch: a document version's block pointers are not indexed: its markdown does not read back",
				"artifact", artifactID, "error", err)
			return nil
		}
		tree = read
	}
	found := pmdoc.BlockPointers(tree)
	pointers := make([]refs.Pointer, len(found))
	for index, pointer := range found {
		pointers[index] = refs.Pointer{Block: pointer.Block, BlockType: pointer.BlockType, Kind: pointer.Attribute, Target: pointer.Target, Targets: pointer.Targets}
	}
	return pointers
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

// ExcerptsOwedChannel is the Postgres notification channel every artifact mention left owed an
// excerpt announces itself on at commit (refs_excerpts_owed_notify, migration 0153): the channel
// whose pglisten.Hub wake the server passes to RunReferenceExcerptReconciliation.
const ExcerptsOwedChannel = "dispatch_excerpts_owed"

// referenceExcerptBatch is how many documents one reconciliation pass reads, and
// referenceExcerptRetryCeiling the longest a document that keeps failing waits between attempts
// (refs.DeferExcerpts).
const (
	referenceExcerptBatch        = 20
	referenceExcerptRetryCeiling = 6 * time.Hour
)

// excerptSchedule is the reconciliation's pace. minGap is the least time from one pass's start to
// the next one's, whatever woke it (duty.WokenLoop.MinGap), and also how long a pass waits when
// another process holds the lease; retryBase is how long a document whose read or record failed
// waits before its first retry, doubling with each failure after (refs.DeferExcerpts), and how
// long a pass whose own read failed waits before the next. A test runs the loop faster.
type excerptSchedule struct {
	minGap    time.Duration
	retryBase time.Duration
}

// referenceExcerptSchedule is the server's pace: two seconds between passes, the step the timer
// this loop replaced took while a batch was full, so under load the loop runs no more often than
// that timer did; one minute before a failed document's first retry.
var referenceExcerptSchedule = excerptSchedule{minGap: 2 * time.Second, retryBase: time.Minute}

// referenceExcerptLease names the lease one reconciliation pass runs under. The excerpts it
// stores are the same whichever process cuts them, and a mention is owed one until some process
// does, so two processes reading the same batch of documents and writing the same rows is work
// done twice for one result - and, because each pass claims nothing, the same documents twice.
const referenceExcerptLease = "reference-excerpts"

// RunReferenceExcerptReconciliation stores the excerpts of every document some of whose mentions
// are owed one (refs.DriftedExcerpts): every mention indexed before migration 0092, each one
// rebuild-refs inserts, and each a version write could not cut. It is a woken loop
// (duty.RunWoken) under the reference-excerpts lease: one pass at start, then a pass when wake is
// signalled - in the server, the pglisten.Hub wake on ExcerptsOwedChannel, which every mention
// left owed announces through a trigger on refs, and which the hub also signals after each
// successful LISTEN, so what was written while the listener had no connection is caught up then -
// and when a pass asks for one: at once while a full batch leaves documents it can still record,
// and when the earliest document it deferred falls due (refs.NextExcerptRetry). Passes start at
// least two seconds apart. With nothing owed and nothing deferred it holds no timer and makes no
// statement. A nil wake leaves the loop with its start and its own due times.
//
// A document the pass cannot read or record is deferred, one minute after its first failure and
// doubling to six hours (refs.DeferExcerpts), so it neither holds the loop at two seconds nor
// fills every batch ahead of the documents that sort after it. The first pass each process runs
// also marks owed the excerpts a version written by a binary predating migration 0092 left behind
// (refs.MarkStaleExcerpts); every build since records its excerpts in each version write, so one
// marking per process is enough. A mention owed an excerpt is answered with the document's name
// until this stores one.
func (s *Service) RunReferenceExcerptReconciliation(ctx context.Context, leases duty.Leases, wake <-chan struct{}) {
	s.runReferenceExcerptReconciliation(ctx, leases, wake, referenceExcerptSchedule)
}

func (s *Service) runReferenceExcerptReconciliation(ctx context.Context, leases duty.Leases, wake <-chan struct{}, schedule excerptSchedule) {
	ctx = store.WithTransactionTracking(ctx)
	// marked is whether a pass of this process has marked the stale excerpts; a marking that
	// failed is tried again by the next pass that runs.
	marked := false
	duty.RunWoken(ctx, duty.WokenLoop{
		Lease:  referenceExcerptLease,
		Leases: leases,
		Wake:   wake,
		MinGap: schedule.minGap,
		Pass: func(ctx context.Context) time.Time {
			if !marked {
				marked = s.markStaleExcerpts(ctx)
			}
			return s.reconcileExcerptPass(ctx, schedule)
		},
	})
}

// markStaleExcerpts marks owed the excerpts older than their document's latest version
// (refs.MarkStaleExcerpts) and reports whether it did so without error.
func (s *Service) markStaleExcerpts(ctx context.Context) bool {
	marked, err := refs.MarkStaleExcerpts(ctx, s.store.Pool)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("dispatch: mark stale reference excerpts", "error", err)
		}
		return false
	}
	if marked > 0 {
		slog.Info("dispatch: marked stale reference excerpts owed", "mentions", marked)
	}
	return true
}

// reconcileExcerptPass is one pass of the reconciliation's batch, and returns when the loop next
// needs to run with no wake: now while a full batch leaves documents it can still record, else the
// earliest due time of a document it deferred, else never. A pass whose own read failed asks again
// after schedule.retryBase.
func (s *Service) reconcileExcerptPass(ctx context.Context, schedule excerptSchedule) time.Time {
	documents, full, err := s.reconcileReferenceExcerpts(ctx, schedule.retryBase)
	if documents > 0 {
		slog.Info("dispatch: reconciled reference excerpts", "documents", documents)
	}
	switch {
	case ctx.Err() != nil:
		return time.Time{}
	case err != nil:
		slog.Error("dispatch: reconcile reference excerpts", "error", err)
		return time.Now().Add(schedule.retryBase)
	case full:
		return time.Now()
	}
	next, err := refs.NextExcerptRetry(ctx, s.store.Pool)
	if err != nil {
		if ctx.Err() != nil {
			return time.Time{}
		}
		slog.Error("dispatch: reconcile reference excerpts", "error", err)
		return time.Now().Add(schedule.retryBase)
	}
	return next
}

// ReconcileReferenceExcerpts runs one batch of the reconciliation at the server's pace
// (reconcileReferenceExcerpts).
func (s *Service) ReconcileReferenceExcerpts(ctx context.Context) (documents int, full bool, err error) {
	return s.reconcileReferenceExcerpts(ctx, referenceExcerptSchedule.retryBase)
}

// reconcileReferenceExcerpts reads each document of one batch of drift as the caller sees it
// (TextWithBlocks), with no transaction or connection held, and stores its mentions' excerpts at
// the version the drift named. A document whose stored history does not load or whose tree is
// outside the schema has no block to show, so its mentions are stored with none and keep the
// document's name until its next version. A document it cannot read or record for any other
// reason is logged and deferred (refs.DeferExcerpts, retryBase after its first failure), its
// mentions left owed, so one document never holds back the rest of its batch nor the batches
// after it; a recorded document's backoff row is deleted. documents counts the ones recorded, and
// full reports a full batch at least one of whose documents left the drift a batch reads - recorded
// or deferred - so the next batch reads documents this one did not.
func (s *Service) reconcileReferenceExcerpts(ctx context.Context, retryBase time.Duration) (documents int, full bool, err error) {
	drifted, err := refs.DriftedExcerpts(ctx, s.store.Pool, referenceExcerptBatch)
	if err != nil {
		return 0, false, err
	}
	moved := 0
	deferDocument := func(document refs.ExcerptDrift) {
		if err := refs.DeferExcerpts(ctx, s.store.Pool, document.FromID, document.Version, retryBase, referenceExcerptRetryCeiling); err != nil {
			if ctx.Err() == nil {
				slog.Error("dispatch: reference excerpt reconciliation could not defer a document; the next batch reads it again",
					"artifact", document.FromID, "error", err)
			}
			return
		}
		moved++
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
			slog.Error("dispatch: reference excerpt reconciliation deferred a document it could not read",
				"artifact", document.FromID, "error", err)
			deferDocument(document)
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
			slog.Error("dispatch: reference excerpt reconciliation deferred a document it could not record",
				"artifact", document.FromID, "error", err)
			deferDocument(document)
			continue
		}
		documents++
		moved++
		if document.BackedOff {
			// A row left behind only costs the next batch its exclusion check: the version
			// recorded here owes nothing, and NextExcerptRetry drops rows of documents that owe
			// nothing.
			if err := refs.ClearExcerptBackoff(ctx, s.store.Pool, document.FromID); err != nil && ctx.Err() == nil {
				slog.Error("dispatch: reference excerpt reconciliation could not clear a document's backoff",
					"artifact", document.FromID, "error", err)
			}
		}
	}
	return documents, len(drifted) == referenceExcerptBatch && moved > 0, nil
}
