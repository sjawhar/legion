package main

import (
	"context"
	"fmt"
)

// settleOwedWork leaves the database in the state a settled system is in: nothing a Dispatch
// server starts up and drains. The indexers the seed calls settle most of it as they run -
// refs.IndexBlocks records the document's blocks at its latest version and refs.IndexItem clears
// each item's owed_at and stores its body_sha256 - and this settles what is left, then proves the
// invariant rather than trusting it.
//
// A server that drained owed work would rewrite the seeded rows: RunBlockIndexReconciliation
// would reindex every document, RunTaskProgressReconciliation would count every issue again and
// RunReferenceExcerptReconciliation would rewrite every mention's excerpt. A before-and-after
// measurement taken across that churn would be two readings of two data sets.
func (r *seedRun) settleOwedWork(ctx context.Context) error {
	// The block-grammar queue holds one row for every body the seed wrote that names a block
	// (`#b-`), which the seed has already indexed: the grammar sweep would find nothing to
	// change in them.
	if _, err := r.database.Pool.Exec(ctx, `delete from block_grammar_owed`); err != nil {
		return fmt.Errorf("empty the block grammar queue: %w", err)
	}
	// The related-terms refresh queue the plan's seam 5 adds does not exist yet. When it does,
	// its truncation goes here, before the term statistics are built: the seed's own inserts
	// fire its triggers, so a head-seeded database would otherwise start with a queue row for
	// every item rather than the empty queue a steady system has.
	return r.checkNoOwedWork(ctx)
}

// owedWorkCounts is the check the review's finding 1 names, with the fifth count the plan's
// answer 7 adds: a mention whose excerpt_version is below its document's highest version number
// is one refs.MarkStaleExcerpts re-owes, and the documents with 28 versions make that the common
// case for a seed that wrote 1.
const owedWorkCounts = `
	select (select count(*) from document_block_versions where version is null),
	       (select count(*) from block_grammar_owed),
	       (select count(*) from comment_block_state where owed_at is not null)
	         + (select count(*) from message_block_state where owed_at is not null),
	       (select count(*) from refs where from_kind = 'artifact' and excerpt_version is null),
	       (select count(*) from refs r
	         where r.from_kind = 'artifact'
	           and r.excerpt_version < (select coalesce(max(v.number), 0) from artifact_versions v
	                                     where v.artifact_id = r.from_id::uuid))`

func (r *seedRun) checkNoOwedWork(ctx context.Context) error {
	var blocks, grammar, items, excerpts, stale int
	if err := r.database.Pool.QueryRow(ctx, owedWorkCounts).Scan(&blocks, &grammar, &items, &excerpts, &stale); err != nil {
		return fmt.Errorf("count the owed work: %w", err)
	}
	if blocks|grammar|items|excerpts|stale != 0 {
		return fmt.Errorf("the seed left owed work: %d documents owed a block index, %d bodies owed a grammar pass, %d items owed a block index, %d mentions owed an excerpt and %d mentions holding an excerpt older than their document's latest version",
			blocks, grammar, items, excerpts, stale)
	}
	return nil
}

// vacuum ends the run with `vacuum (analyze)`, not `analyze`. A freshly loaded table has no
// all-visible pages, so the planner avoids an index-only scan on it however good the statistics
// are, and a measurement taken there would read a plan production never chooses.
func (r *seedRun) vacuum(ctx context.Context) error {
	connection, err := r.database.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer connection.Release()
	_, err = connection.Exec(ctx, `vacuum (analyze)`)
	return err
}
