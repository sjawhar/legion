package main

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// seedRun is one run: the database it writes, the plan it writes, and the text source it draws
// from. The text source is separate from the plan's, so a change to how the rows are laid out
// does not move every word of every document.
type seedRun struct {
	database *store.Store
	sizes    sizes
	out      io.Writer
	started  time.Time

	plan   *seedPlan
	random *rand.Rand

	// progressAt is when the last progress line was written, so a phase reports often enough to
	// be watched and seldom enough to be read.
	progressAt time.Time

	// bands counts the versions written in each markdown size band, for the report.
	bands map[string]int

	// blockCounter numbers the blocks of the document being rendered (blockIDs), and tableRow is
	// the block id of the first table's first body row, which every latest version shares and
	// which a table-row anchor names (renderVersion).
	blockCounter int
	tableRow     string

	// subjects are the named rows the measurements take: filled as the run writes them.
	subject subjects
}

// subjects are the named rows a measurement binds that the run alone knows, which the command
// prints: they come out of this run's own draw, so no measurement elsewhere can name them by
// reading the code. Every other subject reportSubjects prints it reads from the plan or from
// the database as it prints it, so it is not held here.
type subjects struct {
	Model          string
	QueryVectorRow string
	RareWord       string
	RareHits       int
}

// batchRows is how many rows of a plain table one transaction writes, and batchDocuments how many
// documents, whose versions carry the markdown. A failure rolls back one batch, not the whole
// run, and a run can be watched as its batches land.
const (
	batchRows      = 2_000
	batchDocuments = 50
	batchItems     = 250
)

// do writes the whole data set and reports it. The order is the order the foreign keys demand:
// projects, then their architecture and issues, then the documents the issues own, then the asks,
// comments and messages over both, then the events, and last the state a server would otherwise
// have to settle for itself.
func (r *seedRun) do(ctx context.Context) error {
	r.bands = map[string]int{}
	r.random = rand.New(rand.NewPCG(randomSeed+1, randomSeed^0x5851f42d4c957f2d))
	// Every block id of every document comes from this counter, reset for each document, so a
	// run writes the same block ids as the run before it. pmdoc mints a UUID v4 otherwise.
	pmdoc.SetBlockIDGenerator(r.blockIDs())
	defer pmdoc.SetBlockIDGenerator(nil)

	planned := time.Now()
	r.plan = newPlan(r.sizes)
	fmt.Fprintf(r.out, "planned %d issues, %d documents, %d asks, %d comments and %d messages in %s\n",
		len(r.plan.issues), len(r.plan.documents), len(r.plan.asks), len(r.plan.comments),
		len(r.plan.messages), time.Since(planned).Round(time.Millisecond))

	steps := []struct {
		name string
		run  func(context.Context) error
	}{
		{"projects", r.writeProjects},
		{"architecture", r.writeArchitecture},
		{"issues", r.writeIssues},
		{"issue links", r.writeIssueLinks},
		{"issue components", r.writeIssueComponents},
		{"documents", r.writeDocuments},
		{"asks", r.writeAsks},
		{"comments", r.writeComments},
		{"messages", r.writeMessages},
		{"events", r.writeEvents},
		{"event counters", r.settleEventCounters},
		{"owed work", r.settleOwedWork},
		{"embeddings", r.writeEmbeddings},
		{"term statistics", r.buildTermStatistics},
		{"vacuum", r.vacuum},
	}
	for _, step := range steps {
		started := time.Now()
		if err := step.run(ctx); err != nil {
			return fmt.Errorf("%s: %w", step.name, err)
		}
		fmt.Fprintf(r.out, "%s: done in %s\n", step.name, time.Since(started).Round(time.Millisecond))
	}
	return r.report(ctx)
}

// blockIDs is the block id generator every document render uses: b1, b2, … in document order,
// restarted for each document (startDocument). pmdoc stamps every block that carries no id, so
// the ids of one document are the same in every run, and a mention naming `#b-b2` names the same
// block each time.
func (r *seedRun) blockIDs() func() string {
	return func() string {
		r.blockCounter++
		return fmt.Sprintf("b%d", r.blockCounter)
	}
}

// startDocument restarts the block id counter, so the next document's blocks are numbered from
// one. Every parse between two calls of it - the task counter's, itemblocks.Split's - draws from
// the same counter and throws its ids away, which is why the counter is restarted for the
// document rather than counted on.
func (r *seedRun) startDocument() {
	r.blockCounter = 0
}

// inBatches runs write over [0, total) in transactions of at most size rows, reporting progress
// while it goes. A batch commits on its own, so a failure costs one batch and a long run can be
// watched.
func (r *seedRun) inBatches(ctx context.Context, label string, total, size int, write func(context.Context, pgx.Tx, int, int) error) error {
	if total == 0 {
		return nil
	}
	started := time.Now()
	for from := 0; from < total; from += size {
		to := min(from+size, total)
		if err := r.inTransaction(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return write(ctx, tx, from, to)
		}); err != nil {
			return fmt.Errorf("rows %d to %d: %w", from, to, err)
		}
		r.progress(label, to, total, started)
	}
	fmt.Fprintf(r.out, "  %s %d in %s\n", label, total, time.Since(started).Round(time.Millisecond))
	return nil
}

// inTransaction runs write in one transaction and commits it.
func (r *seedRun) inTransaction(ctx context.Context, write func(context.Context, pgx.Tx) error) error {
	tx, err := r.database.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := write(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// progressInterval is how often a phase writes a progress line.
const progressInterval = 5 * time.Second

func (r *seedRun) progress(label string, done, total int, started time.Time) {
	now := time.Now()
	if now.Sub(r.progressAt) < progressInterval || done == total {
		return
	}
	r.progressAt = now
	fmt.Fprintf(r.out, "  %s %d/%d (%.0f%%) in %s\n", label, done, total,
		100*float64(done)/float64(total), now.Sub(started).Round(time.Second))
}
