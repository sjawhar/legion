package docs

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// referenceReindexBatch is how many owed rows one re-index tick reads; referenceReindexStep is the
// wait before the next tick while a full batch says more are owed, ReferenceReindexInterval the
// wait once a tick finds less than a batch.
const (
	referenceReindexBatch    = 200
	referenceReindexStep     = 2 * time.Second
	ReferenceReindexInterval = 30 * time.Second
)

// referenceReindexLease names the lease one re-index tick runs under. Every row is claimed in the
// transaction that re-indexes it, so two processes would never index one source twice at once, but
// both would read the same batch and fight over its rows: one process drains the queue.
const referenceReindexLease = "reference-reindex"

// RunReferenceReindex re-indexes every reference source owed one (refs_owed, migration 0118): each
// one stored before the release that reads bare issue keys and `#id`s, each one a server of an
// earlier release writes while a rolling update runs (its writes fire the triggers, and it never
// clears the row a write of this build clears, refs.ReplaceCounted), and every source in the scope
// of a document whose question map changed. One batch per tick, at start, then every
// referenceReindexStep while a full batch says more are owed and every ReferenceReindexInterval
// once none does, until ctx ends. It keeps running for as long as the triggers fire, so the index
// matches the stored text once no older server is running.
func (s *Service) RunReferenceReindex(ctx context.Context, leases duty.Leases) {
	ctx = store.WithTransactionTracking(ctx)
	// period is the wait the last tick asked for, as RunReferenceExcerptReconciliation's is: the
	// 2 s steps while rows remain, the full interval once none do.
	period := ReferenceReindexInterval
	for {
		wait := ReferenceReindexInterval
		duty.Exclusive(ctx, leases, referenceReindexLease, period, func(ctx context.Context) {
			wait = s.reindexTick(ctx)
		})
		period = wait
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// reindexTick is one tick of the re-index and returns how long to wait before the next:
// referenceReindexStep while a full batch says more rows are owed, the interval once none are.
func (s *Service) reindexTick(ctx context.Context) time.Duration {
	wait := ReferenceReindexInterval
	reindexed, full, err := s.ReindexReferences(ctx)
	switch {
	case ctx.Err() != nil:
		return wait
	case err != nil:
		slog.Error("dispatch: re-index owed references", "error", err)
	case full:
		wait = referenceReindexStep
	}
	if reindexed > 0 {
		slog.Info("dispatch: re-indexed owed references", "sources", reindexed)
	}
	return wait
}

// ReindexReferences runs one tick: it reads one batch of refs_owed, oldest first, and re-indexes
// each source it names in a transaction of its own (reindexOwed). A scope row names a document
// whose question map may have changed: every source in that scope whose text may cite a question
// by `#id` (refs.ScopeCitations) is owed in its place and re-indexed in this tick. A source it
// cannot re-index is logged and left owed for a later tick, so one never holds back the rest of
// its batch; reindexed counts the sources re-indexed, and full reports a batch that was full.
func (s *Service) ReindexReferences(ctx context.Context) (reindexed int, full bool, err error) {
	owed, err := refs.OwedSources(ctx, s.store.Pool, referenceReindexBatch)
	if err != nil {
		return 0, false, err
	}
	for _, item := range owed {
		var done int
		if item.Kind == "scope" {
			done, err = s.reindexScope(ctx, item)
		} else {
			var claimed bool
			if claimed, err = s.reindexOwed(ctx, item.Kind, item.ID, nil); claimed {
				done = 1
			}
		}
		reindexed += done
		if ctx.Err() != nil {
			return reindexed, false, ctx.Err()
		}
		if err != nil {
			slog.Error("dispatch: reference re-index left a source owed for a later tick",
				"kind", item.Kind, "id", item.ID, "error", err)
		}
	}
	return reindexed, len(owed) == referenceReindexBatch, nil
}

// scopeQuestions is the question map of the document a scope row names (refs.QuestionMap), read
// once for every source of that scope reindexScope re-indexes.
type scopeQuestions struct {
	doc       string
	questions map[string]text.Ref
}

// reindexScope owes every source in the scope of the document item names that may cite a question
// by `#id`, settles the scope row once each is owed (refs.SettleScope keeps a row the question map
// re-dated meanwhile), and re-indexes those sources, each resolving `#id` against the document's
// question map read once here (refs.NewScopedResolver) rather than once per source. The map is read
// before the row is settled, so a change to it after the read - a version changing its question
// blocks, an ask made for one - re-dates or remakes the row, which stays owed and is read again at
// a later tick. Each statement runs on its own, holding no transaction: a write landing in that
// scope may hold one of the rows it waits on.
func (s *Service) reindexScope(ctx context.Context, item refs.Owed) (int, error) {
	citing, err := refs.ScopeCitations(ctx, s.store.Pool, item.ID)
	if err != nil {
		return 0, err
	}
	scope := &scopeQuestions{doc: item.ID}
	if len(citing) > 0 {
		if scope.questions, err = refs.QuestionMap(ctx, s.store.Pool, item.ID); err != nil {
			return 0, err
		}
	}
	for _, source := range citing {
		if err := refs.OweSource(ctx, s.store.Pool, source.Kind, source.ID); err != nil {
			return 0, err
		}
	}
	if _, err := refs.SettleScope(ctx, s.store.Pool, item); err != nil {
		return 0, err
	}
	reindexed := 0
	for _, source := range citing {
		claimed, err := s.reindexOwed(ctx, source.Kind, source.ID, scope)
		switch {
		case ctx.Err() != nil:
			return reindexed, ctx.Err()
		case err != nil:
			slog.Error("dispatch: reference re-index left a source of a document's scope owed for a later tick",
				"document", item.ID, "kind", source.Kind, "id", source.ID, "error", err)
		case claimed:
			reindexed++
		}
	}
	return reindexed, nil
}

// excerptRead is a document as RunReferenceReindex cuts its mentions' excerpts from it: the
// rendering the caller sees (TextWithBlocks), read before the re-index's transaction opens, since a
// document read takes a connection of its own. unreadable marks a document whose stored history
// does not load or whose tree is outside the schema, which has no block to show.
type excerptRead struct {
	markdown   string
	blocks     []model.ArtifactBlock
	unreadable bool
}

// reindexOwed re-indexes the source (kind, id) in one transaction and reports whether it claimed
// its row: the claim (refs.ClaimOwed) comes first, then the stored text (refs.LoadSource) and the
// index (refs.Reindex); a source that is gone loses its edges. A source of a scope reindexScope
// drains resolves `#id` against scope's question map while it is still in that scope; any other
// (scope nil) as its live write does (refs.NewResolver). A document also stores its mentions'
// excerpts in that transaction, so it leaves none owed for an earlier release's reconciliation,
// which reads mentions by the grammar before bare keys and `#id`s, to fill: cut from the rendering
// read beforehand when it is the version's markdown, as IndexDocumentReferences cuts them only
// from the render its version came from; else marked owed (refs.ClearExcerpts) for this release's
// reconciliation, which reads the document itself. A row another transaction holds is left owed.
func (s *Service) reindexOwed(ctx context.Context, kind, id string, scope *scopeQuestions) (bool, error) {
	var read excerptRead
	if kind == "artifact" {
		markdown, blocks, err := s.TextWithBlocks(ctx, id)
		switch {
		case errors.Is(err, ErrDocumentUnloadable), errors.Is(err, ErrDocOutsideSchema):
			slog.Warn("dispatch: reference re-index cannot read the document; its mentions keep the document's name",
				"artifact", id, "error", err)
			read.unreadable = true
		case err != nil:
			return false, err
		default:
			read.markdown, read.blocks = markdown, blocks
		}
	}
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	claimed, err := refs.ClaimOwed(ctx, tx, kind, id)
	if err != nil || !claimed {
		return false, err
	}
	source, err := refs.LoadSource(ctx, tx, kind, id)
	if err != nil {
		return false, err
	}
	resolver := refs.NewResolver(ctx, tx, kind, id)
	if scope != nil {
		resolver = refs.NewScopedResolver(ctx, tx, kind, id, scope.doc, scope.questions)
	}
	if err := refs.Reindex(ctx, tx, source, s.serverURL, resolver); err != nil {
		return false, err
	}
	if kind == "artifact" && source.Exists {
		if err := s.recordReindexedExcerpts(ctx, tx, source, read, resolver); err != nil {
			return false, err
		}
	}
	return true, tx.Commit(ctx)
}

// recordReindexedExcerpts stores the excerpts of the mentions the document source writes, at its
// version, each resolved through resolver as its index was: none for a document it could not read
// (as ReconcileReferenceExcerpts records one), cut from read when read renders as the version's
// markdown, else owed to the reconciliation.
func (s *Service) recordReindexedExcerpts(ctx context.Context, tx pgx.Tx, source refs.Source, read excerptRead, resolver text.Resolver) error {
	switch {
	case read.unreadable:
		return refs.RecordExcerpts(ctx, tx, source.ID, source.Version, nil)
	case read.markdown != source.Body:
		return refs.ClearExcerpts(ctx, tx, source.ID)
	}
	excerpts, err := refs.MentionExcerpts(read.markdown, read.blocks, s.serverURL, resolver)
	if err != nil {
		return err
	}
	return refs.RecordExcerpts(ctx, tx, source.ID, source.Version, excerpts)
}
