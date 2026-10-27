package docs

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
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
// referenceExcerptSkipFor how long the reconciliation passes over a document holding an element
// this build does not know, at the version it read (excerptSkips).
const (
	referenceExcerptBatch             = 20
	referenceExcerptStep              = 2 * time.Second
	ReferenceExcerptReconcileInterval = 5 * time.Minute
	referenceExcerptSkipFor           = 10 * time.Minute
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
// ReferenceExcerptReconcileInterval once none does, until ctx ends. Each tick that follows a full
// interval - the first one too - first marks owed the excerpts a version written by a build that
// stores none left behind (refs.MarkStaleExcerpts): two builds serve one database during every
// release, however long this process has run. Until a tick marks one, GET /api/v1/references
// serves that older excerpt. The two-second steps do not mark: the marking reads every mention
// holding an excerpt, and a step only continues the batches the last marking left owed. A mention
// owed an excerpt is answered with the document's name until this stores one.
func (s *Service) RunReferenceExcerptReconciliation(ctx context.Context, leases duty.Leases) {
	ctx = store.WithTransactionTracking(ctx)
	// period is this loop's own interval at this moment, which is the wait the last tick asked
	// for: referenceExcerptStep while drift remains, the full interval once none does. A tick
	// inside it is a tick some process has already made, so it is skipped; passing the full
	// interval instead would make the converging 2 s steps - this loop's whole point when a
	// rebuild leaves thousands of mentions owed an excerpt - one batch per five minutes.
	period := ReferenceExcerptReconcileInterval
	for {
		// A tick whose lease another process holds, or whose period that process has already
		// served, waits the full interval: that process is doing this work, and the drift it
		// leaves is what the next tick to take the lease finds. leases is required, as it is of
		// every leased loop.
		wait := ReferenceExcerptReconcileInterval
		markStale := period == ReferenceExcerptReconcileInterval
		duty.Exclusive(ctx, leases, referenceExcerptLease, period, func(ctx context.Context) {
			wait = s.reconcileExcerptTick(ctx, markStale)
		})
		period = wait
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// reconcileExcerptTick is one tick of the reconciliation - the stale marking when markStale says
// the tick follows a full interval, and one batch - and returns how long to wait before the next:
// referenceExcerptStep while a full batch says drift remains, the interval once none does.
func (s *Service) reconcileExcerptTick(ctx context.Context, markStale bool) time.Duration {
	if markStale {
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
// none and keep the document's name until its next version. A document holding an element this
// build does not know (ErrDocNewerContent) is one a newer build wrote while two builds serve one
// database: its mentions stay owed, for a build that reads it, and this process passes over it at
// that version for referenceExcerptSkipFor (excerptSkips), so the batches after it are read. A
// document it cannot read or record for any other reason is logged and skipped for this tick, its
// mentions left owed, so one document never holds back the rest of its batch; documents counts the
// ones recorded, and full reports a batch that was full.
func (s *Service) ReconcileReferenceExcerpts(ctx context.Context) (documents int, full bool, err error) {
	drifted, err := refs.DriftedExcerpts(ctx, s.store.Pool, referenceExcerptBatch, s.excerptSkips.current(s.now()))
	if err != nil {
		return 0, false, err
	}
	for _, document := range drifted {
		var excerpts map[refs.Target]model.GraphExcerpt
		markdown, blocks, err := s.TextWithBlocks(ctx, document.FromID)
		switch {
		case ctx.Err() != nil:
			return documents, false, ctx.Err()
		case errors.Is(err, ErrDocNewerContent):
			slog.Warn("dispatch: reference excerpt reconciliation passed over a document holding an element this build cannot read; its mentions stay owed for a build that reads it",
				"artifact", document.FromID, "version", document.Version, "error", err)
			s.excerptSkips.skip(document, s.now())
			continue
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
		// The drift lists a skipped document again only at another version, which this build
		// reads: the skip is over.
		s.excerptSkips.forget(document.FromID)
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

// excerptSkips is the documents this process's reconciliation passes over, each at the version it
// found holding an element this build does not know, and since when. A newer build reads such a
// document, and while two builds serve one database the newer one's reconciliation stores its
// excerpts; without the skips a batch of them would stand at the front of the drift, which lists
// documents in id order, and this process would read the same batch every tick. A skip ends when
// the document has another version, which the drift then lists, or after referenceExcerptSkipFor,
// so a release that has finished meets the document again. It is this process's own: a process
// started since reads the document once and skips it too.
type excerptSkips struct {
	mu      sync.Mutex
	entries map[string]excerptSkip
}

// excerptSkip is one skipped document's version and when the reconciliation skipped it.
type excerptSkip struct {
	version int
	since   time.Time
}

// current is every skip younger than referenceExcerptSkipFor at now, each as the drift it passes
// over; it drops the older ones.
func (k *excerptSkips) current(now time.Time) []refs.ExcerptDrift {
	k.mu.Lock()
	defer k.mu.Unlock()
	skipped := make([]refs.ExcerptDrift, 0, len(k.entries))
	for fromID, entry := range k.entries {
		if now.Sub(entry.since) >= referenceExcerptSkipFor {
			delete(k.entries, fromID)
			continue
		}
		skipped = append(skipped, refs.ExcerptDrift{FromID: fromID, Version: entry.version})
	}
	return skipped
}

// skip passes over document at its version from now on.
func (k *excerptSkips) skip(document refs.ExcerptDrift, now time.Time) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.entries == nil {
		k.entries = make(map[string]excerptSkip)
	}
	k.entries[document.FromID] = excerptSkip{version: document.Version, since: now}
}

// forget ends the skip of the document fromID, if there is one.
func (k *excerptSkips) forget(fromID string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.entries, fromID)
}
